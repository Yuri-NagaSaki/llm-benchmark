package bench

import (
	"math"
	"strings"
	"testing"
)

func metricInt(n int) *int           { return &n }
func metricFloat(n float64) *float64 { return &n }
func metricFixture() Sample {
	return Sample{
		Channel: "a", Protocol: "chat", Phase: "measure", Outcome: "completed", FinishReason: "stop",
		Usage:     Usage{InputTokens: metricInt(20), OutputTokens: metricInt(10), ReasoningTokens: metricInt(0)},
		TimingsMS: map[string]float64{"first_text": 100, "last_text": 1100, "stream_end": 1200},
		Events:    []Observation{{Kind: "text", OffsetMS: 100, Characters: 1}, {Kind: "text", OffsetMS: 1100, Characters: 9}},
	}
}

func TestTextRateExcludesReasoning(t *testing.T) {
	plain, thought := metricFixture(), metricFixture()
	thought.Usage.OutputTokens = metricInt(110)
	thought.Usage.ReasoningTokens = metricInt(100)
	FinalizeSample(&plain, Channel{}, ThinkingConfig{Mode: "default"})
	FinalizeSample(&thought, Channel{}, ThinkingConfig{Mode: "default"})
	if plain.Metrics["text_decode_tps_estimate"] != float64(9) || thought.Metrics["text_decode_tps_estimate"] != plain.Metrics["text_decode_tps_estimate"] {
		t.Fatalf("reasoning changed text rate: %v vs %v", plain.Metrics, thought.Metrics)
	}
	if thought.Metrics["text_decode_tps"] != nil {
		t.Fatal("unknown first chunk count must not produce an exact rate")
	}
	if thought.Metrics["output_tps_e2e"] != float64(110)*1000/1200 {
		t.Fatal("end-to-end must include reasoning tokens")
	}
}

func TestDecodeRequiresKnownCountsAndRealWindow(t *testing.T) {
	cases := []struct {
		name            string
		edit            func(*Sample)
		exact, estimate any
	}{
		{"unknown reasoning", func(s *Sample) { s.Usage.ReasoningTokens = nil }, nil, nil},
		{"unknown output", func(s *Sample) { s.Usage.OutputTokens = nil }, nil, nil},
		{"no usage", func(s *Sample) { s.Usage = Usage{} }, nil, nil},
		{"one chunk", func(s *Sample) { s.Events = s.Events[:1]; s.TimingsMS["last_text"] = 100 }, nil, nil},
		{"one token", func(s *Sample) { s.Usage.OutputTokens = metricInt(1) }, nil, nil},
		{"known first chunk", func(s *Sample) { s.Events[0].TokenCount = metricInt(3) }, float64(7), nil},
		{"explicit text count", func(s *Sample) { s.Usage.ReasoningTokens = nil; s.Usage.TextOutputTokens = metricInt(10) }, nil, float64(9)},
		{"tool ambiguous", func(s *Sample) { s.Events = append(s.Events, Observation{Kind: "tool"}) }, nil, nil},
		{"tool explicit text count", func(s *Sample) {
			s.Events = append(s.Events, Observation{Kind: "tool"})
			s.Usage.TextOutputTokens = metricInt(8)
		}, nil, float64(7)},
	}
	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			s := metricFixture()
			tt.edit(&s)
			FinalizeSample(&s, Channel{}, ThinkingConfig{Mode: "default"})
			if s.Metrics["text_decode_tps"] != tt.exact || s.Metrics["text_decode_tps_estimate"] != tt.estimate {
				t.Fatalf("got exact %v estimate %v", s.Metrics["text_decode_tps"], s.Metrics["text_decode_tps_estimate"])
			}
		})
	}
}

func TestSummaryNeverProvidesRawReasoningSpeed(t *testing.T) {
	s := metricFixture()
	s.Usage.OutputTokens = metricInt(110)
	s.Usage.ReasoningTokens = metricInt(100)
	s.Events = append(s.Events, Observation{Kind: "reasoning_summary", OffsetMS: 1}, Observation{Kind: "reasoning_summary", OffsetMS: 50})
	s.TimingsMS["first_reasoning_summary"] = 1
	s.TimingsMS["last_reasoning_summary"] = 50
	FinalizeSample(&s, Channel{}, ThinkingConfig{Mode: "default"})
	if s.Metrics["reasoning_decode_tps"] != nil || s.Metrics["reasoning_decode_tps_estimate"] != nil || s.Metrics["first_reasoning_ms"] != nil {
		t.Fatal("summary misrepresented as raw reasoning")
	}
	if s.Metrics["first_reasoning_summary_ms"] != float64(1) {
		t.Fatal("summary timing lost")
	}
}

func TestThinkingOffEvidence(t *testing.T) {
	cases := []struct {
		name           string
		edit           func(*Sample)
		check, outcome string
	}{
		{"explicit zero", func(*Sample) {}, "passed", "completed"},
		{"missing count", func(s *Sample) { s.Usage.ReasoningTokens = nil }, "unverifiable", "failed"},
		{"positive count", func(s *Sample) { s.Usage.ReasoningTokens = metricInt(1) }, "failed", "failed"},
		{"raw reasoning", func(s *Sample) { s.Events = append(s.Events, Observation{Kind: "reasoning"}) }, "failed", "failed"},
		{"summary", func(s *Sample) { s.Events = append(s.Events, Observation{Kind: "reasoning_summary"}) }, "failed", "failed"},
	}
	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			s := metricFixture()
			tt.edit(&s)
			FinalizeSample(&s, Channel{}, ThinkingConfig{Mode: "off"})
			if s.ReasoningCheck != tt.check || s.Outcome != tt.outcome || s.FinishReason != "stop" {
				t.Fatalf("check=%s outcome=%s finish=%s", s.ReasoningCheck, s.Outcome, s.FinishReason)
			}
		})
	}
}

func TestMissingTimingsAreNullAndQuotaExplicit(t *testing.T) {
	s := Sample{Outcome: "completed", ResponseHeaders: map[string]string{"x-ratelimit-limit-tokens": "5000", "quota": " 6000 "}}
	FinalizeSample(&s, Channel{}, ThinkingConfig{Mode: "default"})
	for _, key := range []string{"dns_ms", "tcp_ms", "tls_ms", "total_ms", "account_tpm", "cost", "first_text_ms", "output_tps_e2e", "single_stream_tokens_per_minute"} {
		if v, ok := s.Metrics[key]; !ok || v != nil {
			t.Errorf("%s must exist and be null, got %v", key, v)
		}
	}
	FinalizeSample(&s, Channel{TPMHeader: "Quota"}, ThinkingConfig{Mode: "default"})
	if s.Metrics["account_tpm"] != float64(6000) {
		t.Fatalf("explicit quota missing: %v", s.Metrics["account_tpm"])
	}
}

func TestNativeTraceTimingKeysAndRequestTotal(t *testing.T) {
	s := metricFixture()
	s.TimingsMS["dns_start"], s.TimingsMS["dns_done"] = 1, 5
	s.TimingsMS["tcp_start"], s.TimingsMS["tcp_done"] = 5, 9
	s.TimingsMS["tls_start"], s.TimingsMS["tls_done"] = 9, 19
	s.TimingsMS["got_connection"], s.TimingsMS["first_sse"] = 19, 39
	s.TimingsMS["total"] = 1300
	FinalizeSample(&s, Channel{}, ThinkingConfig{Mode: "default"})
	for key, want := range map[string]float64{"dns_ms": 4, "tcp_ms": 4, "tls_ms": 10, "post_connect_first_event_ms": 20, "total_ms": 1300} {
		if s.Metrics[key] != want {
			t.Errorf("%s = %v, want %v", key, s.Metrics[key], want)
		}
	}
}

func TestInvalidUsageFailsWithoutNegativeDerivedCounts(t *testing.T) {
	for _, edit := range []func(*Usage){func(u *Usage) { u.ReasoningTokens = metricInt(11) }, func(u *Usage) { u.OutputTokens = metricInt(-1) }, func(u *Usage) { u.CachedInputTokens = metricInt(21) }, func(u *Usage) { u.CachedInputTokens = metricInt(10); u.CacheMissInputTokens = metricInt(11) }} {
		s := metricFixture()
		edit(&s.Usage)
		FinalizeSample(&s, Channel{}, ThinkingConfig{Mode: "default"})
		if s.Outcome != "failed" || s.Usage.TextOutputTokens != nil || s.Metrics["text_decode_tps_estimate"] != nil || s.Metrics["cache_hit_ratio"] != nil {
			t.Fatalf("invalid usage accepted: %+v", s)
		}
	}
}

func TestCacheAndCost(t *testing.T) {
	s := metricFixture()
	s.Usage.InputTokens = metricInt(100)
	s.Usage.CachedInputTokens = metricInt(64)
	s.Usage.CacheWriteTokens = metricInt(10)
	s.Usage.OutputTokens = metricInt(110)
	s.Usage.ReasoningTokens = metricInt(100)
	p := &Price{Currency: "USD", InputPerMillion: 2, OutputPerMillion: 4, CachedInputPerMillion: metricFloat(1), CacheWritePerMillion: metricFloat(3), Source: "local", AsOf: "2026-10-12"}
	FinalizeSample(&s, Channel{CacheBlockSize: 32, Pricing: p}, ThinkingConfig{Mode: "default"})
	if *s.Usage.CacheMissInputTokens != 36 || s.Metrics["cache_hit_ratio"] != float64(.64) || s.Metrics["cache_partial_hit"] != true || s.Metrics["cache_block_aligned"] != true {
		t.Fatalf("bad cache metrics: %v", s.Metrics)
	}
	want := float64(26*2+64+10*3+110*4) / 1e6
	got := s.Metrics["cost"].(float64)
	if math.Abs(got-want) > 1e-12 {
		t.Fatalf("cost=%g want=%g", got, want)
	}
	if s.Metrics["currency"] != "USD" {
		t.Fatal("missing currency")
	}
	cases := []struct {
		name string
		edit func(*Sample, *Price)
	}{
		{"missing discounted cache count", func(s *Sample, p *Price) { s.Usage.CachedInputTokens = nil; s.Usage.CacheMissInputTokens = nil }},
		{"missing cache write price", func(s *Sample, p *Price) { p.CacheWritePerMillion = nil }},
		{"missing cache write count", func(s *Sample, p *Price) { s.Usage.CacheWriteTokens = nil }},
		{"missing output count", func(s *Sample, p *Price) { s.Usage.OutputTokens = nil }},
	}
	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			ss := s
			pp := *p
			tt.edit(&ss, &pp)
			FinalizeSample(&ss, Channel{Pricing: &pp}, ThinkingConfig{Mode: "default"})
			if ss.Metrics["cost"] != nil || !strings.Contains(strings.Join(ss.Warnings, " "), "cost unavailable") {
				t.Fatalf("cost must be unknown: %v", ss.Metrics["cost"])
			}
		})
	}
}

func TestSummarySeparatesWarmupFailuresAndCurrencies(t *testing.T) {
	var samples []Sample
	for i := 1; i <= 99; i++ {
		samples = append(samples, Sample{Channel: "a", Protocol: "chat", Phase: "measure", Outcome: "completed", Metrics: map[string]any{"total_ms": float64(i), "cost": float64(1), "currency": "USD"}})
	}
	samples = append(samples, Sample{Channel: "a", Protocol: "chat", Phase: "measure", Outcome: "failed", Metrics: map[string]any{"total_ms": float64(9999), "cost": float64(2), "currency": "JPY"}})
	samples = append(samples, Sample{Channel: "a", Protocol: "chat", Phase: "warmup", Outcome: "completed", Metrics: map[string]any{"total_ms": float64(9999)}})
	summary := Summarize(samples)
	groups := summary["groups"].([]any)
	if len(groups) != 2 {
		t.Fatalf("groups=%d", len(groups))
	}
	g := groups[0].(map[string]any)
	d := g["metrics"].(map[string]any)["total_ms"].(map[string]any)
	if d["n"] != 99 || d["p99"] != nil || d["max"] != float64(99) || g["failed"] != 1 || g["sample_count"] != 100 {
		t.Fatalf("bad summary %v", g)
	}
	cost := g["cost_by_currency"].(map[string]any)
	if cost["USD"].(map[string]any)["total"] != float64(99) || cost["JPY"].(map[string]any)["total"] != float64(2) {
		t.Fatalf("currency totals %v", cost)
	}
	samples = append(samples, Sample{Channel: "a", Protocol: "chat", Phase: "measure", Outcome: "completed", Metrics: map[string]any{"total_ms": float64(100)}})
	g = Summarize(samples)["groups"].([]any)[0].(map[string]any)
	d = g["metrics"].(map[string]any)["total_ms"].(map[string]any)
	if d["n"] != 100 || d["p99"] != float64(99) || g["tail_label"] != "p99（经验分位数）" {
		t.Fatalf("p99 threshold %v", g)
	}
}
