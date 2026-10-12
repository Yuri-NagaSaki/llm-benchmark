package bench

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func integrationProtocolReply(w http.ResponseWriter, protocol string) {
	if protocol == "responses" {
		integrationSSE(w,
			`{"type":"response.output_text.delta","delta":"hello"}`,
			`{"type":"response.completed","response":{"id":"resp-fixture","model":"fixture-model","usage":{"input_tokens":10,"output_tokens":1,"output_tokens_details":{"reasoning_tokens":0},"input_tokens_details":{"cached_tokens":0}}}}`)
		return
	}
	integrationChat(w)
}

func TestRunMixedProtocolParallelBarrierAndSerialRotation(t *testing.T) {
	for _, schedule := range []string{"parallel", "serial"} {
		t.Run(schedule, func(t *testing.T) {
			var mu sync.Mutex
			var order []string
			arrived := 0
			gates := []chan struct{}{make(chan struct{}), make(chan struct{})}
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				var body map[string]any
				if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
					t.Error(err)
					http.Error(w, "invalid", 400)
					return
				}
				protocol := "chat"
				switch r.URL.Path {
				case "/chat/completions":
					if body["messages"] == nil || body["max_tokens"] != float64(17) {
						t.Error("chat workload missing")
					}
				case "/responses":
					protocol = "responses"
					if body["input"] == nil || body["max_output_tokens"] != float64(17) {
						t.Error("responses workload missing")
					}
				default:
					t.Errorf("wrong path %s", r.URL.Path)
				}
				mu.Lock()
				order = append(order, protocol)
				index := arrived
				arrived++
				if schedule == "parallel" && arrived%2 == 0 {
					close(gates[index/2])
				}
				mu.Unlock()
				if schedule == "parallel" {
					select {
					case <-gates[index/2]:
					case <-r.Context().Done():
						return
					}
				}
				integrationProtocolReply(w, protocol)
			}))
			defer srv.Close()
			c := integrationConfig(srv.URL)
			c.Run.Schedule = schedule
			c.Run.Rounds = 2
			c.Run.Concurrency = 2
			c.Workload.MaxOutputTokens = 17
			c.Channels = append(c.Channels, Channel{Name: "responses", BaseURL: srv.URL, Protocol: "responses", Model: "fixture-model", Anonymous: true})
			report, err := Run(context.Background(), c)
			if err != nil {
				t.Fatal(err)
			}
			if len(report.Samples) != 4 {
				t.Fatalf("samples=%d", len(report.Samples))
			}
			ids := map[string]bool{}
			for i, s := range report.Samples {
				if s.Outcome != "completed" || s.Phase != "measure" || s.Round != i/2 || s.Channel != c.Channels[i%2].Name || s.Protocol != c.Channels[i%2].Protocol {
					t.Fatalf("bad sample: %+v", s)
				}
				if ids[s.ID] {
					t.Fatal("duplicate sample ID")
				}
				ids[s.ID] = true
				if !s.RoundStartedAt.Equal(report.Samples[(i/2)*2].RoundStartedAt) {
					t.Fatal("same-round anchor differs across channels")
				}
			}
			if schedule == "serial" && !reflect.DeepEqual(order, []string{"chat", "responses", "responses", "chat"}) {
				t.Fatalf("serial rotation=%v", order)
			}
			if schedule == "parallel" {
				for i := 0; i < 4; i += 2 {
					a, b := report.Samples[i], report.Samples[i+1]
					if !a.StartedAt.Before(b.EndedAt) || !b.StartedAt.Before(a.EndedAt) {
						t.Fatal("barrier did not overlap same-round requests")
					}
				}
			}
			t.Logf("schedule=%s received=%v archived=%d", schedule, order, len(report.Samples))
		})
	}
}

func TestRunConcurrencyLimitAndConnectionPolicies(t *testing.T) {
	for _, policy := range []string{"reuse", "fresh"} {
		t.Run(policy, func(t *testing.T) {
			var active, maximum atomic.Int32
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				n := active.Add(1)
				defer active.Add(-1)
				for {
					old := maximum.Load()
					if n <= old || maximum.CompareAndSwap(old, n) {
						break
					}
				}
				time.Sleep(time.Millisecond)
				integrationChat(w)
			}))
			defer srv.Close()
			c := integrationConfig(srv.URL)
			c.Run.Rounds = 2
			c.Run.Connection = policy
			c.Run.Concurrency = 1
			second := c.Channels[0]
			second.Name = "second"
			c.Channels = append(c.Channels, second)
			report, err := Run(context.Background(), c)
			if err != nil {
				t.Fatal(err)
			}
			if maximum.Load() != 1 || len(report.Samples) != 4 {
				t.Fatalf("maximum=%d samples=%d", maximum.Load(), len(report.Samples))
			}
			for _, s := range report.Samples {
				want := policy == "reuse" && s.Round > 0
				if s.Outcome != "completed" || s.ConnectionReused == nil || *s.ConnectionReused != want || s.ConnectionPolicy != policy {
					t.Fatalf("connection policy not applied: %+v", s)
				}
			}
		})
	}
}

func TestRunWarmupPrefixIsolationFailureArchive(t *testing.T) {
	for _, content := range []any{"shared system prefix", []any{map[string]any{"type": "text", "text": "shared system prefix"}}} {
		t.Run(fmt.Sprintf("%T", content), func(t *testing.T) {
			var mu sync.Mutex
			var bodies []map[string]any
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				var body map[string]any
				if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
					t.Error(err)
					return
				}
				mu.Lock()
				bodies = append(bodies, body)
				n := len(bodies)
				mu.Unlock()
				if n == 1 {
					http.Error(w, "synthetic warmup failure", 503)
					return
				}
				integrationChat(w)
			}))
			defer srv.Close()
			c := integrationConfig(srv.URL)
			c.Run.Warmup = 2
			c.Run.Rounds = 1
			c.Workload.Messages = []map[string]any{{"role": "system", "content": content}, {"role": "user", "content": "measured question"}}
			before, err := json.Marshal(c.Workload)
			if err != nil {
				t.Fatal(err)
			}
			report, err := Run(context.Background(), c)
			if err != nil {
				t.Fatal(err)
			}
			after, err := json.Marshal(c.Workload)
			if err != nil {
				t.Fatal(err)
			}
			if string(before) != string(after) {
				t.Fatal("warmup mutated configured workload")
			}
			if len(report.Samples) != 3 || len(bodies) != 3 {
				t.Fatalf("archive=%d requests=%d", len(report.Samples), len(bodies))
			}
			prefixes := make([]string, 3)
			for i, body := range bodies {
				messages := body["messages"].([]any)
				first := messages[0].(map[string]any)["content"]
				switch v := first.(type) {
				case string:
					prefixes[i] = v
				case []any:
					prefixes[i] = v[0].(map[string]any)["text"].(string)
				}
				if messages[1].(map[string]any)["content"] != "measured question" {
					t.Fatal("user workload changed")
				}
				if i < 2 {
					if !strings.HasPrefix(prefixes[i], "[connection-warmup:") || report.Samples[i].Phase != "warmup" {
						t.Fatal("warmup did not isolate complete prefix")
					}
				} else if strings.Contains(prefixes[i], "connection-warmup") || report.Samples[i].Phase != "measure" {
					t.Fatal("warmup marker leaked to measured input")
				}
			}
			if prefixes[0] == prefixes[1] {
				t.Fatal("warmup requests share a cache prefix")
			}
			if report.Samples[0].Outcome != "failed" || report.Samples[1].Outcome != "completed" || report.Samples[2].Outcome != "completed" {
				t.Fatal("warmup outcome not retained independently")
			}
			if report.Samples[2].StartedAt.Before(report.Samples[1].EndedAt) {
				t.Fatal("warmup overlaps measured request")
			}
			path := filepath.Join(t.TempDir(), "warmup.json")
			if err := WriteReport(path, report, NewRedactor(c)); err != nil {
				t.Fatal(err)
			}
			restored, err := ReadReport(path)
			if err != nil {
				t.Fatal(err)
			}
			if len(restored.Samples) != 3 || restored.Samples[0].Error == nil || restored.Samples[0].HTTPStatus != 503 || restored.Samples[2].Phase != "measure" {
				t.Fatal("warmup evidence missing from report roundtrip")
			}
			groups := restored.Summary["groups"].([]any)
			counts := map[string]map[string]any{}
			for _, entry := range groups {
				g := entry.(map[string]any)
				counts[g["phase"].(string)] = g
			}
			if counts["measure"]["sample_count"] != float64(1) || counts["measure"]["failed"] != float64(0) || counts["warmup"]["sample_count"] != float64(2) || counts["warmup"]["failed"] != float64(1) {
				t.Fatalf("warmup contaminated summary: %v", counts)
			}
		})
	}
}

func TestRunCacheExperimentOrdersEachSeedAndPreservesPrefix(t *testing.T) {
	var mu sync.Mutex
	var paths []string
	var requests []map[string]any
	var active, maximum atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := active.Add(1)
		defer active.Add(-1)
		for {
			old := maximum.Load()
			if n <= old || maximum.CompareAndSwap(old, n) {
				break
			}
		}
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Error(err)
			return
		}
		mu.Lock()
		paths = append(paths, r.URL.Path)
		requests = append(requests, body)
		mu.Unlock()
		integrationChat(w)
	}))
	defer srv.Close()
	c := integrationConfig(srv.URL)
	c.Run.Mode = "cache"
	c.Run.Schedule = "parallel"
	c.Run.Concurrency = 2
	c.Channels[0].Name = "a"
	c.Channels[0].Path = "/a"
	b := c.Channels[0]
	b.Name = "b"
	b.Path = "/b"
	c.Channels = append(c.Channels, b)
	report, err := Run(context.Background(), c)
	if err != nil {
		t.Fatal(err)
	}
	if maximum.Load() != 1 {
		t.Fatal("cache predecessor chain ran concurrently")
	}
	if len(report.Samples) != 6 || !reflect.DeepEqual(paths, []string{"/a", "/a", "/b", "/b", "/b", "/a"}) {
		t.Fatalf("samples=%d paths=%v", len(report.Samples), paths)
	}
	ids := map[string]bool{}
	for seed := 0; seed < 2; seed++ {
		base := seed * 3
		samples := report.Samples[base : base+3]
		phases := []string{"cache_cold_candidate", "cache_hot", "cache_probe"}
		for i, s := range samples {
			if s.Outcome != "completed" || s.Phase != phases[i] || s.CacheGroup == "" || s.CacheGroup != samples[0].CacheGroup || s.RequestFingerprint != samples[0].RequestFingerprint {
				t.Fatalf("cache group mismatch: %+v", s)
			}
			if ids[s.ID] {
				t.Fatal("duplicate cache sample ID")
			}
			ids[s.ID] = true
			if i == 0 {
				if s.CachePredecessor != "" {
					t.Fatal("cold has predecessor")
				}
			} else if s.CachePredecessor != samples[i-1].ID || s.StartedAt.Before(samples[i-1].EndedAt) {
				t.Fatal("cache order does not follow recorded predecessor")
			}
			if !reflect.DeepEqual(requests[base], requests[base+i]) {
				t.Fatal("cold/hot/probe request prefixes differ")
			}
		}
		first := requests[base]["messages"].([]any)[0].(map[string]any)["content"].(string)
		if !strings.HasPrefix(first, "[cache-experiment:") {
			t.Fatal("cache seed lacks independent prefix")
		}
	}
	if report.Samples[0].CacheGroup == report.Samples[3].CacheGroup || report.Samples[0].RequestFingerprint == report.Samples[3].RequestFingerprint {
		t.Fatal("opposite seed direction reused the previous experiment prefix")
	}
	t.Logf("cache order=%v phases=%s,%s,%s", paths, report.Samples[0].Phase, report.Samples[1].Phase, report.Samples[2].Phase)
}
