package bench

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/url"
	"os"
	"regexp"
	"strings"
)

type HeaderValue struct {
	Value string `json:"value,omitempty"`
	Env   string `json:"env,omitempty"`
}

type ThinkingConfig struct {
	Mode   string `json:"mode"`
	Effort string `json:"effort,omitempty"`
}

type Price struct {
	Currency              string   `json:"currency"`
	InputPerMillion       float64  `json:"input_per_million"`
	OutputPerMillion      float64  `json:"output_per_million"`
	CachedInputPerMillion *float64 `json:"cached_input_per_million,omitempty"`
	CacheWritePerMillion  *float64 `json:"cache_write_per_million,omitempty"`
	Source                string   `json:"source"`
	AsOf                  string   `json:"as_of"`
}

func (p *Price) UnmarshalJSON(data []byte) error {
	type wirePrice Price
	var value wirePrice
	d := json.NewDecoder(bytes.NewReader(data))
	d.DisallowUnknownFields()
	if err := d.Decode(&value); err != nil {
		return err
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(data, &fields); err != nil {
		return err
	}
	for _, name := range []string{"input_per_million", "output_per_million"} {
		v, ok := fields[name]
		if !ok || bytes.Equal(bytes.TrimSpace(v), []byte("null")) {
			return fmt.Errorf("pricing requires explicit %s; absent is not zero", name)
		}
	}
	*p = Price(value)
	return nil
}

type Channel struct {
	Name            string                    `json:"name"`
	BaseURL         string                    `json:"base_url"`
	Model           string                    `json:"model"`
	APIKeyEnv       string                    `json:"api_key_env,omitempty"`
	Anonymous       bool                      `json:"anonymous,omitempty"`
	Protocol        string                    `json:"protocol"`
	TokenLimitField string                    `json:"token_limit_field,omitempty"`
	Path            string                    `json:"path,omitempty"`
	Profile         string                    `json:"profile,omitempty"`
	ExtraBody       map[string]any            `json:"extra_body,omitempty"`
	Headers         map[string]HeaderValue    `json:"headers,omitempty"`
	Thinking        *ThinkingConfig           `json:"thinking,omitempty"`
	ThinkingBodies  map[string]map[string]any `json:"thinking_bodies,omitempty"`
	UsagePaths      map[string]string         `json:"usage_paths,omitempty"`
	CacheBlockSize  int                       `json:"cache_block_size,omitempty"`
	Pricing         *Price                    `json:"pricing,omitempty"`
	TPMHeader       string                    `json:"tpm_header,omitempty"`
}

type Workload struct {
	Messages        []map[string]any `json:"messages"`
	MaxOutputTokens int              `json:"max_output_tokens"`
	Temperature     *float64         `json:"temperature,omitempty"`
}

type RunConfig struct {
	Rounds                int     `json:"rounds"`
	Warmup                int     `json:"warmup"`
	Schedule              string  `json:"schedule"`
	Connection            string  `json:"connection"`
	TimeoutSeconds        float64 `json:"timeout_seconds"`
	ConnectTimeoutSeconds float64 `json:"connect_timeout_seconds"`
	Concurrency           int     `json:"concurrency"`
	IntervalMS            int     `json:"interval_ms"`
	Mode                  string  `json:"mode"`
}

type Config struct {
	Run      RunConfig      `json:"run"`
	Thinking ThinkingConfig `json:"thinking"`
	Workload Workload       `json:"workload"`
	Channels []Channel      `json:"channels"`
}

func DefaultConfig() Config {
	return Config{
		Run:      RunConfig{Rounds: 5, Schedule: "parallel", Connection: "reuse", TimeoutSeconds: 120, ConnectTimeoutSeconds: 10, Concurrency: 8, Mode: "standard"},
		Thinking: ThinkingConfig{Mode: "default"},
		Workload: Workload{Messages: []map[string]any{{"role": "user", "content": "Reply with a short greeting."}}, MaxOutputTokens: 256},
	}
}

func LoadConfig(r io.Reader) (Config, error) {
	c := DefaultConfig()
	d := json.NewDecoder(io.LimitReader(r, 16<<20))
	d.DisallowUnknownFields()
	if err := d.Decode(&c); err != nil {
		return c, fmt.Errorf("invalid config JSON: %w", err)
	}
	if err := d.Decode(new(any)); err != io.EOF {
		return c, errors.New("config must contain exactly one JSON object")
	}
	return c, c.Validate()
}

func (c Config) Validate() error {
	if len(c.Channels) == 0 {
		return errors.New("at least one channel is required")
	}
	if c.Run.Rounds < 1 || c.Run.Warmup < 0 || c.Run.Concurrency < 1 || c.Run.IntervalMS < 0 {
		return errors.New("rounds and concurrency must be positive; warmup and interval_ms cannot be negative")
	}
	if !validTimeout(c.Run.TimeoutSeconds) || !validTimeout(c.Run.ConnectTimeoutSeconds) {
		return errors.New("timeouts must be finite, positive and at most 86400 seconds")
	}
	if c.Run.Schedule != "parallel" && c.Run.Schedule != "serial" {
		return errors.New("schedule must be parallel or serial")
	}
	if c.Run.Connection != "reuse" && c.Run.Connection != "fresh" {
		return errors.New("connection must be reuse or fresh")
	}
	if c.Run.Mode != "standard" && c.Run.Mode != "cache" {
		return errors.New("mode must be standard or cache")
	}
	if c.Workload.MaxOutputTokens < 1 || len(c.Workload.Messages) == 0 {
		return errors.New("workload requires messages and positive max_output_tokens")
	}
	for _, m := range c.Workload.Messages {
		if _, ok := m["role"].(string); !ok {
			return errors.New("each message needs a role")
		}
		if _, ok := m["content"]; !ok {
			return errors.New("each message needs content")
		}
	}
	if err := validateThinking(c.Thinking); err != nil {
		return err
	}
	names := map[string]bool{}
	for _, ch := range c.Channels {
		if containsCredential(ch.ExtraBody) || containsCredential(ch.ThinkingBodies) {
			return errors.New("credentials in request bodies are not allowed; use named environment variables for authentication")
		}
		if ch.Name == "" || names[ch.Name] {
			return errors.New("channel names must be nonempty and unique")
		}
		names[ch.Name] = true
		if ch.Model == "" {
			return fmt.Errorf("channel %s requires an explicit model ID", ch.Name)
		}
		u, err := url.Parse(ch.BaseURL)
		if err != nil || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https") || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
			return fmt.Errorf("channel %s requires an http(s) base_url without credentials, query or fragment", ch.Name)
		}
		if ch.Protocol != "chat" && ch.Protocol != "responses" && ch.Protocol != "anthropic" {
			return fmt.Errorf("channel %s has unsupported protocol", ch.Name)
		}
		if ch.TokenLimitField != "" && ch.Protocol != "chat" {
			return errors.New("token_limit_field is only configurable for chat")
		}
		if ch.Profile != "" && ch.Profile != "generic" && ch.Profile != "deepseek" && ch.Profile != "tencent" && ch.Profile != "aliyun" {
			return fmt.Errorf("channel %s has unsupported profile", ch.Name)
		}
		if sensitiveName(ch.TPMHeader) || strings.ContainsAny(ch.TPMHeader, "\r\n: ") {
			return errors.New("invalid or sensitive tpm_header")
		}
		if ch.Path != "" && (!strings.HasPrefix(ch.Path, "/") || strings.ContainsAny(ch.Path, "?#") || strings.Contains(ch.Path, "..")) {
			return fmt.Errorf("channel %s has invalid path", ch.Name)
		}
		if ch.Anonymous == (ch.APIKeyEnv != "") {
			return fmt.Errorf("channel %s requires exactly one of api_key_env or anonymous=true", ch.Name)
		}
		if ch.APIKeyEnv != "" && !envName.MatchString(ch.APIKeyEnv) {
			return fmt.Errorf("channel %s has an invalid api_key_env name", ch.Name)
		}
		if u.Scheme == "http" && !ch.Anonymous && u.Hostname() != "localhost" && u.Hostname() != "127.0.0.1" && u.Hostname() != "::1" {
			return fmt.Errorf("channel %s requires HTTPS for credentials", ch.Name)
		}
		for name, h := range ch.Headers {
			if h.Env != "" && u.Scheme == "http" && u.Hostname() != "localhost" && u.Hostname() != "127.0.0.1" && u.Hostname() != "::1" {
				return errors.New("environment-backed headers require HTTPS outside loopback")
			}
			if strings.ContainsAny(name, "\r\n: ") || strings.ContainsAny(h.Value, "\r\n") {
				return errors.New("invalid HTTP header")
			}
			if keyPattern.MatchString(h.Value) {
				return errors.New("credential-like header values must use env")
			}
			if h.Env == "" && h.Value == "" || h.Env != "" && h.Value != "" {
				return fmt.Errorf("header %s requires exactly one of env or value", name)
			}
			if h.Env != "" && !envName.MatchString(h.Env) {
				return fmt.Errorf("header %s has invalid env name", name)
			}
			if (sensitiveName(name) || strings.Contains(strings.ToLower(name), "token") || strings.Contains(strings.ToLower(name), "auth")) && h.Value != "" {
				return fmt.Errorf("header %s must use env", name)
			}
			if strings.EqualFold(name, "Authorization") || strings.EqualFold(name, "X-Api-Key") {
				return fmt.Errorf("header %s is owned by api_key_env", name)
			}
		}
		if ch.Thinking != nil {
			if err := validateThinking(*ch.Thinking); err != nil {
				return err
			}
		}
		if ch.CacheBlockSize < 0 {
			return errors.New("cache_block_size cannot be negative")
		}
		if p := ch.Pricing; p != nil {
			if p.Currency == "" || p.Source == "" || p.AsOf == "" || p.InputPerMillion < 0 || p.OutputPerMillion < 0 || p.CachedInputPerMillion != nil && *p.CachedInputPerMillion < 0 || p.CacheWritePerMillion != nil && *p.CacheWritePerMillion < 0 {
				return errors.New("pricing requires currency, source, as_of and nonnegative rates")
			}
		}
		if _, err := BuildBody(ch, c.Workload, c.ThinkingFor(ch)); err != nil {
			return fmt.Errorf("channel %s: %w", ch.Name, err)
		}
	}
	return nil
}

var envName = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

func validTimeout(v float64) bool { return v > 0 && v <= 86400 && !math.IsInf(v, 0) && !math.IsNaN(v) }

func containsCredential(value any) bool {
	b, err := json.Marshal(value)
	if err != nil {
		return true
	}
	var v any
	if json.Unmarshal(b, &v) != nil {
		return true
	}
	var check func(any) bool
	check = func(x any) bool {
		switch item := x.(type) {
		case map[string]any:
			for k, v := range item {
				if sensitiveName(k) || check(v) {
					return true
				}
			}
		case []any:
			for _, v := range item {
				if check(v) {
					return true
				}
			}
		case string:
			return keyPattern.MatchString(item)
		}
		return false
	}
	return check(v)
}

func validateThinking(t ThinkingConfig) error {
	if t.Mode != "default" && t.Mode != "off" && t.Mode != "on" {
		return errors.New("thinking mode must be default, off or on")
	}
	if t.Mode != "on" && t.Effort != "" {
		return errors.New("thinking effort requires mode=on")
	}
	return nil
}

func (c Config) ThinkingFor(ch Channel) ThinkingConfig {
	if ch.Thinking != nil {
		return *ch.Thinking
	}
	return c.Thinking
}

func (ch Channel) URL() string {
	p := ch.Path
	if p == "" {
		p = map[string]string{"chat": "/chat/completions", "responses": "/responses", "anthropic": "/messages"}[ch.Protocol]
	}
	return strings.TrimRight(ch.BaseURL, "/") + p
}

func (c Config) CheckCredentials() error {
	for _, ch := range c.Channels {
		if ch.APIKeyEnv != "" && os.Getenv(ch.APIKeyEnv) == "" {
			return fmt.Errorf("channel %s: environment variable %s is empty", ch.Name, ch.APIKeyEnv)
		}
		for _, h := range ch.Headers {
			if h.Env != "" && os.Getenv(h.Env) == "" {
				return fmt.Errorf("channel %s: header environment variable %s is empty", ch.Name, h.Env)
			}
		}
	}
	return nil
}
