package bench

import (
	"encoding/json"
	"os"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

func sensitiveName(s string) bool {
	s = strings.ToLower(strings.NewReplacer("-", "", "_", "", ".", "").Replace(s))
	for _, name := range []string{"authorization", "apikey", "accesstoken", "refreshtoken", "secret", "password", "credential", "cookie"} {
		if strings.Contains(s, name) {
			return true
		}
	}
	return false
}

var keyPattern = regexp.MustCompile(`(?i)(bearer\s+)[^\s"\\<>]+|\bsk-[A-Za-z0-9_-]{8,}`)

// Redactor carries only explicitly configured credentials; it never dumps the environment.
type Redactor struct{ values []string }

func NewRedactor(c Config) Redactor {
	r := Redactor{}
	for _, ch := range c.Channels {
		if v := os.Getenv(ch.APIKeyEnv); v != "" {
			r.values = append(r.values, v)
		}
		for _, h := range ch.Headers {
			if v := os.Getenv(h.Env); v != "" {
				r.values = append(r.values, v)
			}
		}
	}
	return r
}

func (r Redactor) Text(s string) string {
	for _, v := range r.values {
		s = strings.ReplaceAll(s, v, "[REDACTED]")
	}
	return keyPattern.ReplaceAllString(s, "[REDACTED]")
}

func (r Redactor) Value(v any) any {
	switch x := v.(type) {
	case string:
		return r.Text(x)
	case map[string]any:
		m := make(map[string]any, len(x))
		keys := make([]string, 0, len(x))
		for k := range x {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			v := x[k]
			safeKey := r.Text(k)
			baseKey := safeKey
			for n := 2; ; n++ {
				if _, exists := m[safeKey]; !exists {
					break
				}
				safeKey = baseKey + "#" + strconv.Itoa(n)
			}
			if sensitiveName(k) && k != "api_key_env" {
				m[safeKey] = "[REDACTED]"
			} else {
				m[safeKey] = r.Value(v)
			}
		}
		return m
	case []any:
		a := make([]any, len(x))
		for i := range x {
			a[i] = r.Value(x[i])
		}
		return a
	default:
		return v
	}
}

func (r Redactor) JSON(v any) ([]byte, error) {
	b, err := json.Marshal(v)
	if err != nil {
		return nil, err
	}
	var value any
	if err := json.Unmarshal(b, &value); err != nil {
		return nil, err
	}
	return json.MarshalIndent(r.Value(value), "", "  ")
}
