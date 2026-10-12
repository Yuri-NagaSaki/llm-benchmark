package bench

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestProtocolTerminalDoesNotWaitForBodyEOF(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{\"content\":\"ok\"},\"finish_reason\":\"stop\"}]}\n\ndata: [DONE]\n\n")
		w.(http.Flusher).Flush()
		<-r.Context().Done()
	}))
	defer srv.Close()
	c := DefaultConfig()
	c.Run.Rounds = 1
	c.Run.TimeoutSeconds = 0.5
	c.Channels = []Channel{{Name: "local", BaseURL: srv.URL, Protocol: "chat", Model: "test", Anonymous: true}}
	start := time.Now()
	r, err := Run(context.Background(), c)
	elapsed := time.Since(start)
	if err != nil {
		t.Fatal(err)
	}
	s := r.Samples[0]
	if s.Outcome != "completed" || s.Error != nil {
		t.Fatalf("completed protocol incorrectly failed: %+v", s)
	}
	if elapsed > 400*time.Millisecond {
		t.Fatal("waited for model timeout after protocol termination")
	}
	if s.TimingsMS["total"] != s.TimingsMS["protocol_terminal"] || s.TimingsMS["stream_end"] != s.TimingsMS["protocol_terminal"] {
		t.Fatal("EOF cleanup polluted latency")
	}
}

func TestStickyStreamErrorsAndEmptyCompletedOutput(t *testing.T) {
	for name, payload := range map[string]string{
		"error-before-success":    "data: {\"error\":{\"message\":\"failed\"}}\n\ndata: {\"choices\":[{\"delta\":{\"content\":\"ok\"},\"finish_reason\":\"stop\"}]}\n\ndata: [DONE]\n\n",
		"empty-success":           "data: {\"choices\":[{\"delta\":{},\"finish_reason\":\"stop\"}]}\n\ndata: [DONE]\n\n",
		"terminal-without-finish": "data: {\"choices\":[{\"delta\":{\"content\":\"ok\"}}]}\n\ndata: [DONE]\n\n",
	} {
		t.Run(name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "text/event-stream")
				fmt.Fprint(w, payload)
			}))
			defer srv.Close()
			c := DefaultConfig()
			c.Run.Rounds = 1
			c.Channels = []Channel{{Name: "test", BaseURL: srv.URL, Model: "fixture", Protocol: "chat", Anonymous: true}}
			r, err := Run(context.Background(), c)
			if err != nil {
				t.Fatal(err)
			}
			if r.Samples[0].Outcome != "failed" || r.Samples[0].Error == nil {
				t.Fatal("false success")
			}
		})
	}
}

func TestPrefixIsolationWithNullAssistantToolHistory(t *testing.T) {
	w := Workload{MaxOutputTokens: 8, Messages: []map[string]any{{"role": "assistant", "content": nil, "tool_calls": []any{map[string]any{"id": "x", "type": "function", "function": map[string]any{"name": "lookup", "arguments": "{}"}}}}, {"role": "tool", "tool_call_id": "x", "content": "done"}}}
	prefixed := prefixedWorkload(w, "UNIQUE")
	if len(prefixed.Messages) != 3 || prefixed.Messages[0]["role"] != "system" || prefixed.Messages[0]["content"] != "UNIQUE" {
		t.Fatal("cache prefix silently omitted")
	}
	if len(w.Messages) != 2 || w.Messages[0]["content"] != nil {
		t.Fatal("original workload mutated")
	}
	for _, protocol := range []string{"chat", "responses", "anthropic"} {
		for _, field := range []string{"system", "instructions", "previous_response_id"} {
			ch := Channel{Model: "fixture", Protocol: protocol, ExtraBody: map[string]any{field: "override"}}
			if _, err := BuildBody(ch, DefaultConfig().Workload, ThinkingConfig{Mode: "default"}); err == nil {
				t.Fatalf("accepted workload override %s/%s", protocol, field)
			}
		}
	}
}

func TestRedactsCredentialsInJSONKeys(t *testing.T) {
	t.Setenv("LLM_BENCH_KEY_FIELD_TEST", "synthetic-test-only-secret")
	c := DefaultConfig()
	c.Channels = []Channel{{APIKeyEnv: "LLM_BENCH_KEY_FIELD_TEST"}}
	b, err := NewRedactor(c).JSON(map[string]any{"raw_usage": map[string]any{"synthetic-test-only-secret": 1, "n": 2}})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(b), "synthetic-test-only-secret") {
		t.Fatal("secret in JSON key leaked")
	}
	if !json.Valid(b) {
		t.Fatal("redaction corrupted JSON")
	}
}

func TestChatCompletionTokenLimitField(t *testing.T) {
	ch := Channel{Model: "reasoning-model", Protocol: "chat", TokenLimitField: "max_completion_tokens"}
	b, err := BuildBody(ch, DefaultConfig().Workload, ThinkingConfig{Mode: "default"})
	if err != nil {
		t.Fatal(err)
	}
	if b["max_completion_tokens"] != 256 || b["max_tokens"] != nil {
		t.Fatal("wrong budget field")
	}
}

func TestMissingAndNullPricesAreNotFree(t *testing.T) {
	for _, rates := range []string{"", `,"input_per_million":null,"output_per_million":1`, `,"input_per_million":1`} {
		raw := strings.Replace(minimalConfig, `"anonymous":true`, `"anonymous":true,"pricing":{"currency":"USD","source":"fixture","as_of":"2026-10-12"`+rates+`}`, 1)
		if _, err := LoadConfig(strings.NewReader(raw)); err == nil {
			t.Fatal("missing price accepted as zero")
		}
	}
	raw := strings.Replace(minimalConfig, `"anonymous":true`, `"anonymous":true,"pricing":{"currency":"USD","source":"fixture","as_of":"2026-10-12","input_per_million":0,"output_per_million":0}`, 1)
	if _, err := LoadConfig(strings.NewReader(raw)); err != nil {
		t.Fatal("explicit zero price rejected", err)
	}
}

func TestNonTextUsageDoesNotInflateTextRate(t *testing.T) {
	total, zero := 110, 0
	for _, field := range []string{"audio_tokens", "image_tokens", "rejected_prediction_tokens"} {
		s := Sample{Outcome: "completed", TimingsMS: map[string]float64{"first_text": 10, "last_text": 1010, "total": 1050}, Usage: Usage{OutputTokens: &total, ReasoningTokens: &zero}, Events: []Observation{{Kind: "text", OffsetMS: 10}, {Kind: "text", OffsetMS: 1010}}, RawUsage: []map[string]any{{"completion_tokens_details": map[string]any{field: 100}}}}
		FinalizeSample(&s, Channel{}, ThinkingConfig{Mode: "default"})
		if s.Usage.TextOutputTokens != nil || s.Metrics["text_decode_tps_estimate"] != nil {
			t.Fatalf("%s contaminated text rate", field)
		}
	}
}

func TestMultimodalInputDoesNotHideTextOutputRate(t *testing.T) {
	total, zero := 10, 0
	s := Sample{Outcome: "completed", TimingsMS: map[string]float64{"first_text": 10, "last_text": 1010, "total": 1050}, Usage: Usage{OutputTokens: &total, ReasoningTokens: &zero}, Events: []Observation{{Kind: "text", OffsetMS: 10}, {Kind: "text", OffsetMS: 1010}}, RawUsage: []map[string]any{{"prompt_tokens_details": map[string]any{"audio_tokens": 50, "image_tokens": 100}}}}
	FinalizeSample(&s, Channel{}, ThinkingConfig{Mode: "default"})
	if s.Usage.TextOutputTokens == nil || *s.Usage.TextOutputTokens != 10 || s.Metrics["text_decode_tps_estimate"] != float64(9) {
		t.Fatal("input modality polluted output classification")
	}
}

func TestCustomAuthenticationRequiresHTTPS(t *testing.T) {
	c, _ := LoadConfig(strings.NewReader(minimalConfig))
	c.Channels[0].BaseURL = "http://example.test/v1"
	c.Channels[0].Headers = map[string]HeaderValue{"X-Gateway-Token": {Env: "GATEWAY_TOKEN"}}
	if c.Validate() == nil {
		t.Fatal("env authentication allowed over external plaintext HTTP")
	}
}

func TestThreeChannelCachePredecessorsAreSequential(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { integrationChat(w) }))
	defer srv.Close()
	c := integrationConfig(srv.URL)
	c.Run.Mode = "cache"
	for _, name := range []string{"b", "c"} {
		ch := c.Channels[0]
		ch.Name = name
		c.Channels = append(c.Channels, ch)
	}
	r, err := Run(context.Background(), c)
	if err != nil {
		t.Fatal(err)
	}
	if len(r.Samples) != 12 {
		t.Fatal("wrong request count")
	}
	for i, s := range r.Samples {
		if i%4 == 0 {
			if s.CachePredecessor != "" {
				t.Fatal("seed has predecessor")
			}
		} else if s.CachePredecessor != r.Samples[i-1].ID {
			t.Fatal("predecessor omits preceding probe")
		}
	}
}

func TestSuccessfulNetworkAttemptOwnsTiming(t *testing.T) {
	yes, no := true, false
	s := Sample{Trace: []TraceEvent{{Name: "tcp_start", Attempt: "v6", OffsetMS: 1}, {Name: "tcp_done", Attempt: "v6", OffsetMS: 3, Succeeded: &no}, {Name: "tcp_start", Attempt: "v4", OffsetMS: 2}, {Name: "tcp_done", Attempt: "v4", OffsetMS: 12, Succeeded: &yes}, {Name: "got_connection", Attempt: "v4", OffsetMS: 13}}}
	d, ok := networkInterval(&s, "tcp")
	if !ok || d != 10 {
		t.Fatalf("failed connection attempt polluted duration: %v %v", d, ok)
	}
}
