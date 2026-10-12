package bench

import (
	"context"
	"crypto/x509"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func integrationConfig(base string) Config {
	c := DefaultConfig()
	c.Run.Rounds = 1
	c.Run.TimeoutSeconds = 2
	c.Run.ConnectTimeoutSeconds = 1
	c.Channels = []Channel{{Name: "local", BaseURL: base, Model: "fixture-model", Protocol: "chat", Anonymous: true}}
	return c
}

func integrationSSE(w http.ResponseWriter, frames ...string) {
	w.Header().Set("Content-Type", "text/event-stream")
	for _, frame := range frames {
		fmt.Fprintf(w, "data: %s\n\n", frame)
	}
	if f, ok := w.(http.Flusher); ok {
		f.Flush()
	}
}

func integrationChat(w http.ResponseWriter) {
	integrationSSE(w,
		`{"id":"fixture-response","model":"fixture-model","choices":[{"index":0,"delta":{"content":"hello"}}]}`,
		`{"choices":[{"index":0,"delta":{},"finish_reason":"stop"}],"usage":{"prompt_tokens":10,"completion_tokens":1,"completion_tokens_details":{"reasoning_tokens":0},"prompt_tokens_details":{"cached_tokens":0}}}`,
		`[DONE]`)
}

func integrationMeasure(t *testing.T, c Config, client *http.Client) Sample {
	t.Helper()
	return Measure(context.Background(), client, c, c.Channels[0], c.Workload, Sample{Channel: c.Channels[0].Name, Protocol: c.Channels[0].Protocol, Phase: "measure"})
}

func TestHTTPReasoningOnlyIncompletePreservesUsage(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		integrationSSE(w,
			`{"choices":[{"index":0,"delta":{"reasoning_content":"thinking"}}]}`,
			`{"choices":[{"index":0,"delta":{"reasoning_content":"still thinking"}}]}`,
			`{"choices":[{"index":0,"delta":{},"finish_reason":"length"}],"usage":{"prompt_tokens":20,"completion_tokens":12,"completion_tokens_details":{"reasoning_tokens":12}}}`,
			`[DONE]`)
	}))
	defer srv.Close()
	c := integrationConfig(srv.URL)
	client := newClient(c.Run)
	defer client.CloseIdleConnections()
	s := integrationMeasure(t, c, client)
	if s.Outcome != "incomplete" || s.FinishReason != "length" || s.Error != nil {
		t.Fatalf("unexpected outcome: %+v", s)
	}
	if s.Usage.OutputTokens == nil || *s.Usage.OutputTokens != 12 || s.Usage.ReasoningTokens == nil || *s.Usage.ReasoningTokens != 12 || len(s.RawUsage) != 1 {
		t.Fatalf("usage lost: %+v", s.Usage)
	}
	if s.Metrics["first_reasoning_ms"] == nil || s.Metrics["first_text_ms"] != nil || s.Metrics["text_decode_tps"] != nil || s.Metrics["text_decode_tps_estimate"] != nil || s.Metrics["output_tps_e2e"] == nil {
		t.Fatalf("reasoning-only metrics: %v", s.Metrics)
	}
	if len(s.Events) < 2 || s.Events[0].Kind != "reasoning" || s.Events[1].Kind != "reasoning" {
		t.Fatalf("reasoning events lost: %v", s.Events)
	}
}

func TestHTTPTextAndReasoningRemainSeparate(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		integrationSSE(w, `{"choices":[{"delta":{"reasoning_content":"thought"}}]}`)
		time.Sleep(2 * time.Millisecond)
		integrationSSE(w, `{"choices":[{"delta":{"content":"first"}}]}`)
		time.Sleep(2 * time.Millisecond)
		integrationSSE(w, `{"choices":[{"delta":{"content":"second"}}]}`,
			`{"choices":[{"delta":{},"finish_reason":"stop"}],"usage":{"prompt_tokens":20,"completion_tokens":110,"completion_tokens_details":{"reasoning_tokens":100}}}`,
			`[DONE]`)
	}))
	defer srv.Close()
	c := integrationConfig(srv.URL)
	client := newClient(c.Run)
	defer client.CloseIdleConnections()
	s := integrationMeasure(t, c, client)
	if s.Outcome != "completed" || s.Usage.TextOutputTokens == nil || *s.Usage.TextOutputTokens != 10 {
		t.Fatalf("split failed: %+v", s)
	}
	if s.TimingsMS["first_reasoning"] >= s.TimingsMS["first_text"] {
		t.Fatalf("phase timings not distinct: %v", s.TimingsMS)
	}
	want := 9000 / (s.TimingsMS["last_text"] - s.TimingsMS["first_text"])
	if s.Metrics["text_decode_tps_estimate"] != want || s.Metrics["text_decode_tps"] != nil {
		t.Fatalf("text speed contaminated: got %v want %v", s.Metrics, want)
	}
}

func TestHTTPThinkingOffRequiresExplicitZeroUsage(t *testing.T) {
	for _, tt := range []struct{ name, details, check, outcome string }{
		{"missing", "", "unverifiable", "failed"},
		{"positive", `,"completion_tokens_details":{"reasoning_tokens":1}`, "failed", "failed"},
		{"zero", `,"completion_tokens_details":{"reasoning_tokens":0}`, "passed", "completed"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				var body map[string]any
				if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
					t.Error(err)
					return
				}
				if body["enable_thinking"] != false {
					t.Errorf("off not sent: %v", body)
				}
				integrationSSE(w, `{"choices":[{"delta":{"content":"answer"}}]}`,
					`{"choices":[{"delta":{},"finish_reason":"stop"}],"usage":{"prompt_tokens":10,"completion_tokens":2`+tt.details+`}}`, `[DONE]`)
			}))
			defer srv.Close()
			c := integrationConfig(srv.URL)
			c.Thinking.Mode = "off"
			c.Channels[0].Profile = "aliyun"
			client := newClient(c.Run)
			defer client.CloseIdleConnections()
			s := integrationMeasure(t, c, client)
			if s.ReasoningCheck != tt.check || s.Outcome != tt.outcome {
				t.Fatalf("check=%s outcome=%s want %s/%s", s.ReasoningCheck, s.Outcome, tt.check, tt.outcome)
			}
			if s.FinishReason != "stop" || s.Usage.OutputTokens == nil {
				t.Fatal("failed verification lost source evidence")
			}
		})
	}
}

func TestHTTP429PreservesEvidenceAndRedactsOnlySyntheticCredential(t *testing.T) {
	const key = "integration-test-key-DO-NOT-USE"
	const env = "LLM_BENCH_TEST_SYNTHETIC_KEY"
	t.Setenv(env, key)
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		if r.Header.Get("Authorization") != "Bearer "+key {
			t.Error("synthetic auth was not sent")
		}
		w.Header().Set("X-Request-Id", "request-fixture")
		w.Header().Set("Retry-After", "2")
		w.WriteHeader(http.StatusTooManyRequests)
		fmt.Fprintf(w, `{"error":"retry later; echoed key %s"}`, key)
	}))
	defer srv.Close()
	c := integrationConfig(srv.URL)
	c.Channels[0].Anonymous = false
	c.Channels[0].APIKeyEnv = env
	client := newClient(c.Run)
	defer client.CloseIdleConnections()
	s := integrationMeasure(t, c, client)
	if s.Outcome != "failed" || s.HTTPStatus != 429 || s.RequestID != "request-fixture" || s.ResponseHeaders["retry-after"] != "2" || s.Error == nil || s.Error.Kind != "http" {
		t.Fatalf("lost 429 evidence: %+v", s)
	}
	encoded, err := json.Marshal(s)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(encoded), key) || !strings.Contains(s.Error.BodyExcerpt, "[REDACTED]") {
		t.Fatal("synthetic credential was not redacted")
	}
	if calls.Load() != 1 {
		t.Fatalf("unexpected retry count: %d", calls.Load())
	}
}

func TestHTTPMalformedAndTruncatedStreamsFail(t *testing.T) {
	for _, tt := range []struct{ name, body, kind string }{
		{"bad JSON", "data: {broken}\n\n", "stream"},
		{"missing terminal", "data: {\"choices\":[{\"delta\":{\"content\":\"partial\"},\"finish_reason\":\"stop\"}]}\n\n", "truncated_stream"},
		{"missing finish", "data: [DONE]\n\n", "truncated_stream"},
		{"wrong content type", `{"message":"not SSE"}`, "protocol"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if tt.name != "wrong content type" {
					w.Header().Set("Content-Type", "text/event-stream")
				}
				io.WriteString(w, tt.body)
			}))
			defer srv.Close()
			c := integrationConfig(srv.URL)
			client := newClient(c.Run)
			defer client.CloseIdleConnections()
			s := integrationMeasure(t, c, client)
			if s.Outcome != "failed" || s.Error == nil || s.Error.Kind != tt.kind {
				t.Fatalf("unexpected stream outcome: %+v", s)
			}
		})
	}
}

func TestHTTPWholeRequestTimeoutAndCancellationRetainPartialEvents(t *testing.T) {
	for _, cancelRequest := range []bool{false, true} {
		t.Run(fmt.Sprint("cancel=", cancelRequest), func(t *testing.T) {
			sent := make(chan struct{})
			release := make(chan struct{})
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				integrationSSE(w, `{"choices":[{"delta":{"reasoning_content":"partial"}}]}`)
				close(sent)
				select {
				case <-r.Context().Done():
				case <-release:
				}
			}))
			defer srv.Close()
			defer close(release)
			c := integrationConfig(srv.URL)
			if !cancelRequest {
				c.Run.TimeoutSeconds = .05
			}
			client := newClient(c.Run)
			defer client.CloseIdleConnections()
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			done := make(chan Sample, 1)
			go func() { done <- Measure(ctx, client, c, c.Channels[0], c.Workload, Sample{}) }()
			select {
			case <-sent:
			case <-time.After(time.Second):
				t.Fatal("handler never started")
			}
			if cancelRequest {
				cancel()
			}
			var s Sample
			select {
			case s = <-done:
			case <-time.After(time.Second):
				t.Fatal("request deadline was not enforced")
			}
			want := "timeout"
			if cancelRequest {
				want = "canceled"
			}
			if s.Outcome != "failed" || s.Error == nil || s.Error.Kind != want {
				t.Fatalf("lost timeout/cancel evidence: %+v", s)
			}
			// Cancellation immediately after a server flush can precede client parsing.
			// The deadline case observes the first frame before its streaming timeout.
			if !cancelRequest && (len(s.Events) == 0 || s.Metrics["first_reasoning_ms"] == nil) {
				t.Fatalf("stream timeout discarded already observed reasoning: %+v", s)
			}
		})
	}
}

func TestHTTPTLSConnectionReuseAndFreshTraces(t *testing.T) {
	for _, tlsServer := range []bool{false, true} {
		for _, policy := range []string{"reuse", "fresh"} {
			t.Run(fmt.Sprintf("tls=%v/%s", tlsServer, policy), func(t *testing.T) {
				srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { integrationChat(w) }))
				if tlsServer {
					srv.StartTLS()
				} else {
					srv.Start()
				}
				defer srv.Close()
				c := integrationConfig(srv.URL)
				c.Run.Connection = policy
				makeClient := func() *http.Client {
					client := newClient(c.Run)
					if tlsServer {
						pool := x509.NewCertPool()
						pool.AddCert(srv.Certificate())
						client.Transport.(*http.Transport).TLSClientConfig.RootCAs = pool
					}
					return client
				}
				client := makeClient()
				defer client.CloseIdleConnections()
				for i := 0; i < 2; i++ {
					if i > 0 && policy == "fresh" {
						client.CloseIdleConnections()
						client = makeClient()
						defer client.CloseIdleConnections()
					}
					s := integrationMeasure(t, c, client)
					if s.Outcome != "completed" {
						t.Fatalf("request failed: %+v", s.Error)
					}
					reused := i > 0 && policy == "reuse"
					if s.ConnectionReused == nil || *s.ConnectionReused != reused {
						t.Fatalf("request %d reused=%v want=%v", i, s.ConnectionReused, reused)
					}
					tcp, tls := false, false
					for _, e := range s.Trace {
						tcp = tcp || e.Name == "tcp_start"
						tls = tls || e.Name == "tls_start"
					}
					if tcp == reused || tls != (tlsServer && !reused) {
						t.Fatalf("request %d trace mismatch: %v", i, s.Trace)
					}
					if reused {
						if s.Metrics["tcp_ms"] != nil || s.Metrics["tls_ms"] != nil {
							t.Fatal("reused connection invented handshake cost")
						}
					} else {
						if s.Metrics["tcp_ms"] == nil {
							t.Errorf("new connection has TCP trace but null tcp_ms: %v", s.TimingsMS)
						}
						if tlsServer && s.Metrics["tls_ms"] == nil {
							t.Errorf("TLS handshake has trace but null tls_ms: %v", s.TimingsMS)
						}
					}
					if s.Metrics["post_connect_first_event_ms"] == nil {
						t.Errorf("connection ready and first SSE observed but derived metric is null: %v", s.TimingsMS)
					}
				}
			})
		}
	}
}
