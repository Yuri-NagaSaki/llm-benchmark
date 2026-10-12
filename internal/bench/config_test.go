package bench

import (
	"strings"
	"testing"
)

const minimalConfig = `{"channels":[{"name":"local","base_url":"http://localhost:8080/v1","model":"fixture","protocol":"chat","anonymous":true}]}`

func TestConfigDefaultsAndStrictJSON(t *testing.T) {
	c, err := LoadConfig(strings.NewReader(minimalConfig))
	if err != nil {
		t.Fatal(err)
	}
	if c.Run.Warmup != 0 || c.Run.TimeoutSeconds != 120 || c.Run.Rounds != 5 || c.Channels[0].URL() != "http://localhost:8080/v1/chat/completions" {
		t.Fatalf("defaults changed: %+v", c)
	}
	for _, raw := range []string{minimalConfig + ` {}`, strings.Replace(minimalConfig, `"channels":`, `"typo": 1, "channels":`, 1), strings.Replace(minimalConfig, `"anonymous":true`, `"api_key":"literal-secret"`, 1)} {
		if _, err := LoadConfig(strings.NewReader(raw)); err == nil {
			t.Error("accepted malformed or unsafe config")
		}
	}
}

func TestConfigRejectsUnsafeOrAmbiguousChannels(t *testing.T) {
	for name, change := range map[string]func(*Config){
		"both-auth-modes":  func(c *Config) { c.Channels[0].APIKeyEnv = "KEY" },
		"missing-auth":     func(c *Config) { c.Channels[0].Anonymous = false },
		"URL-secret":       func(c *Config) { c.Channels[0].BaseURL = "https://user:password@example.test/v1" },
		"query-secret":     func(c *Config) { c.Channels[0].BaseURL = "https://example.test/v1?key=secret" },
		"literal-header":   func(c *Config) { c.Channels[0].Headers = map[string]HeaderValue{"X-API-Key": {Value: "literal"}} },
		"body-key":         func(c *Config) { c.Channels[0].ExtraBody = map[string]any{"api_key": "literal"} },
		"bad-profile":      func(c *Config) { c.Channels[0].Profile = "typo" },
		"body-stream":      func(c *Config) { c.Channels[0].ExtraBody = map[string]any{"stream": false} },
		"generic-off":      func(c *Config) { c.Thinking.Mode = "off" },
		"duplicate":        func(c *Config) { c.Channels = append(c.Channels, c.Channels[0]) },
		"timeout-overflow": func(c *Config) { c.Run.TimeoutSeconds = 1e50 },
		"sensitive-quota":  func(c *Config) { c.Channels[0].TPMHeader = "Set-Cookie" },
	} {
		t.Run(name, func(t *testing.T) {
			c, err := LoadConfig(strings.NewReader(minimalConfig))
			if err != nil {
				t.Fatal(err)
			}
			change(&c)
			if c.Validate() == nil {
				t.Fatal("invalid configuration accepted")
			}
		})
	}
}

func TestConfigCustomFieldsAndCredentials(t *testing.T) {
	c, err := LoadConfig(strings.NewReader(minimalConfig))
	if err != nil {
		t.Fatal(err)
	}
	c.Channels[0].Anonymous = false
	c.Channels[0].APIKeyEnv = "LLM_BENCH_FAKE_TEST_KEY"
	c.Channels[0].Headers = map[string]HeaderValue{"X-Project": {Env: "LLM_BENCH_FAKE_PROJECT"}}
	t.Setenv("LLM_BENCH_FAKE_TEST_KEY", "")
	t.Setenv("LLM_BENCH_FAKE_PROJECT", "")
	if c.CheckCredentials() == nil {
		t.Fatal("missing key accepted")
	}
	t.Setenv("LLM_BENCH_FAKE_TEST_KEY", "test-only-credential")
	if c.CheckCredentials() == nil {
		t.Fatal("missing header environment accepted")
	}
	t.Setenv("LLM_BENCH_FAKE_PROJECT", "test-project")
	if c.CheckCredentials() != nil {
		t.Fatal("configured credentials rejected")
	}
	c.Channels[0].ExtraBody = map[string]any{"enable_thinking": false}
	if c.Validate() != nil {
		t.Fatal("custom body rejected")
	}
}
