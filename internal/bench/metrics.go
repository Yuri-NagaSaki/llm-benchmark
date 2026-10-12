package bench

import (
	"fmt"
	"math"
	"sort"
	"strconv"
	"strings"
)

// FinalizeSample derives only metrics supported by the captured timings and usage.
// Unknown token counts stay unknown; summaries are never raw reasoning events.
func FinalizeSample(s *Sample, ch Channel, thinking ThinkingConfig) {
	s.Metrics = map[string]any{}
	for _, key := range []string{
		"first_text_ms", "first_reasoning_ms", "first_reasoning_summary_ms", "total_ms",
		"text_decode_tps", "text_decode_tps_estimate", "reasoning_decode_tps", "reasoning_decode_tps_estimate",
		"output_tps_e2e", "single_stream_tokens_per_minute", "account_tpm", "cache_hit_ratio", "cache_partial_hit",
		"cache_block_aligned", "cost", "currency", "dns_ms", "tcp_ms", "tls_ms", "post_connect_first_event_ms",
	} {
		s.Metrics[key] = nil
	}
	s.Metrics["definitions"] = map[string]string{
		"text_decode_tps":                 "(text tokens - first chunk tokens) / (last text - first text); only when both token counts are known",
		"text_decode_tps_estimate":        "(text tokens - 1) / (last text - first text); first chunk token count is unknown",
		"reasoning_decode_tps":            "(raw reasoning tokens - first chunk tokens) / (last raw reasoning - first raw reasoning)",
		"reasoning_decode_tps_estimate":   "(raw reasoning tokens - 1) / (last raw reasoning - first raw reasoning); first chunk token count is unknown",
		"output_tps_e2e":                  "all output tokens, including reasoning, / full request time",
		"single_stream_tokens_per_minute": "output_tps_e2e * 60; not an account quota",
		"post_connect_first_event_ms":     "connection ready to first SSE event; includes network, gateway, queuing and prefill",
		"account_tpm":                     "numeric quota from the explicitly configured response header only",
	}

	hasTool, hasReasoning := false, false
	for _, e := range s.Events {
		if e.Kind == "tool" {
			hasTool = true
		}
		if e.Kind == "reasoning" || e.Kind == "reasoning_summary" {
			hasReasoning = true
		}
	}
	if _, ok := s.TimingsMS["first_reasoning"]; ok {
		hasReasoning = true
	}
	if _, ok := s.TimingsMS["first_reasoning_summary"]; ok {
		hasReasoning = true
	}
	valid := validateUsage(s)
	if valid && s.Usage.CacheMissInputTokens == nil && s.Usage.InputTokens != nil && s.Usage.CachedInputTokens != nil {
		n := *s.Usage.InputTokens - *s.Usage.CachedInputTokens
		s.Usage.CacheMissInputTokens = &n
	}
	if valid && s.Usage.TextOutputTokens == nil && s.Usage.OutputTokens != nil && s.Usage.ReasoningTokens != nil && !hasTool && !nonTextAllocation(s.RawUsage) {
		n := *s.Usage.OutputTokens - *s.Usage.ReasoningTokens
		s.Usage.TextOutputTokens = &n
		s.Metrics["text_token_source"] = "output_tokens_minus_reasoning_tokens"
	} else if s.Usage.TextOutputTokens != nil {
		s.Metrics["text_token_source"] = "explicit_text_output_tokens"
	} else {
		s.Metrics["text_token_source"] = nil
	}
	if !valid {
		s.Outcome = "failed"
		if s.Error == nil {
			s.Error = &RequestError{Kind: "invalid_usage", Message: "usage counters are negative or inconsistent"}
		}
	}

	s.ReasoningCheck = "not_required"
	if thinking.Mode == "off" {
		switch {
		case hasReasoning || s.Usage.ReasoningTokens != nil && *s.Usage.ReasoningTokens > 0:
			s.ReasoningCheck = "failed"
			s.Outcome = "failed"
			metricWarning(s, "thinking=off failed: reasoning or reasoning summary was observed")
		case valid && s.Usage.ReasoningTokens != nil && *s.Usage.ReasoningTokens == 0:
			s.ReasoningCheck = "passed"
		default:
			s.ReasoningCheck = "unverifiable"
			s.Outcome = "failed"
			metricWarning(s, "thinking=off is unverifiable: explicit zero reasoning token usage is unavailable")
		}
		if s.ReasoningCheck != "passed" && s.Error == nil {
			s.Error = &RequestError{Kind: "reasoning_check", Message: "thinking=off verification " + s.ReasoningCheck}
		}
	}

	for _, kind := range []string{"text", "reasoning", "reasoning_summary"} {
		if t, ok := metricTime(s, "first_"+kind); ok {
			s.Metrics["first_"+kind+"_ms"] = t
		}
	}
	for _, kind := range []string{"dns", "tcp", "tls"} {
		if d, ok := networkInterval(s, kind); ok {
			s.Metrics[kind+"_ms"] = d
		}
	}
	if d, ok := metricInterval(s, "connection_ready", "first_sse"); ok {
		s.Metrics["post_connect_first_event_ms"] = d
	}
	total, hasTotal := metricTime(s, "total")
	if !hasTotal {
		total, hasTotal = metricTime(s, "stream_end")
	}
	if !hasTotal && !s.StartedAt.IsZero() && !s.EndedAt.IsZero() {
		total = float64(s.EndedAt.Sub(s.StartedAt)) / 1e6
		hasTotal = total >= 0
	}
	if hasTotal {
		s.Metrics["total_ms"] = total
	}
	if valid {
		setDecodeMetrics(s, "text", s.Usage.TextOutputTokens)
		setDecodeMetrics(s, "reasoning", s.Usage.ReasoningTokens)
		if s.Usage.OutputTokens != nil && hasTotal && total > 0 {
			rate := float64(*s.Usage.OutputTokens) * 1000 / total
			s.Metrics["output_tps_e2e"] = rate
			s.Metrics["single_stream_tokens_per_minute"] = rate * 60
		}
		if s.Usage.InputTokens != nil && s.Usage.CachedInputTokens != nil && *s.Usage.InputTokens > 0 {
			hit, input := *s.Usage.CachedInputTokens, *s.Usage.InputTokens
			s.Metrics["cache_hit_ratio"] = float64(hit) / float64(input)
			s.Metrics["cache_partial_hit"] = hit > 0 && hit < input
		}
		if ch.CacheBlockSize > 0 && s.Usage.CachedInputTokens != nil {
			s.Metrics["cache_block_aligned"] = *s.Usage.CachedInputTokens%ch.CacheBlockSize == 0
		}
	}
	if ch.TPMHeader != "" {
		for k, v := range s.ResponseHeaders {
			if strings.EqualFold(k, ch.TPMHeader) {
				n, err := strconv.ParseFloat(strings.TrimSpace(v), 64)
				if err == nil && n >= 0 && !math.IsNaN(n) && !math.IsInf(n, 0) {
					s.Metrics["account_tpm"] = n
				} else {
					metricWarning(s, "configured account TPM header is not a nonnegative number")
				}
				break
			}
		}
	}
	setCost(s, ch.Pricing, valid)
}

func networkInterval(s *Sample, kind string) (float64, bool) {
	if len(s.Trace) == 0 {
		return metricInterval(s, kind+"_start", kind+"_end")
	}
	connection := ""
	for _, e := range s.Trace {
		if e.Name == "got_connection" {
			connection = e.Attempt
		}
	}
	starts := map[string]float64{}
	for _, e := range s.Trace {
		if e.Name == kind+"_start" {
			starts[e.Attempt] = e.OffsetMS
		}
		if e.Name == kind+"_done" && e.Succeeded != nil && *e.Succeeded {
			if kind == "tcp" && connection != "" && e.Attempt != connection {
				continue
			}
			if start, ok := starts[e.Attempt]; ok && e.OffsetMS >= start {
				return e.OffsetMS - start, true
			}
		}
	}
	return 0, false
}

// Some completion counts include audio/image or rejected prediction tokens.
// Their absence from the visible text stream makes subtraction unsafe.
func nonTextAllocation(raw []map[string]any) bool {
	var check func(any) bool
	check = func(value any) bool {
		switch v := value.(type) {
		case map[string]any:
			for key, item := range v {
				if key == "audio_tokens" || key == "image_tokens" || key == "rejected_prediction_tokens" {
					n := integer(item)
					if n == nil || *n != 0 {
						return true
					}
				}
				if check(item) {
					return true
				}
			}
		case []any:
			for _, item := range v {
				if check(item) {
					return true
				}
			}
		}
		return false
	}
	for _, v := range raw {
		if check(v["completion_tokens_details"]) || check(v["output_tokens_details"]) {
			return true
		}
	}
	return false
}

func metricWarning(s *Sample, warning string) {
	for _, old := range s.Warnings {
		if old == warning {
			return
		}
	}
	s.Warnings = append(s.Warnings, warning)
}

func validateUsage(s *Sample) bool {
	u := s.Usage
	valid := true
	for _, entry := range []struct {
		name string
		n    *int
	}{
		{"input_tokens", u.InputTokens}, {"output_tokens", u.OutputTokens}, {"reasoning_tokens", u.ReasoningTokens},
		{"text_output_tokens", u.TextOutputTokens}, {"cached_input_tokens", u.CachedInputTokens},
		{"cache_miss_input_tokens", u.CacheMissInputTokens}, {"cache_write_tokens", u.CacheWriteTokens},
	} {
		if entry.n != nil && *entry.n < 0 {
			metricWarning(s, "invalid usage: "+entry.name+" is negative")
			valid = false
		}
	}
	for _, entry := range []struct {
		name          string
		child, parent *int
	}{
		{"reasoning_tokens exceeds output_tokens", u.ReasoningTokens, u.OutputTokens},
		{"text_output_tokens exceeds output_tokens", u.TextOutputTokens, u.OutputTokens},
		{"cached_input_tokens exceeds input_tokens", u.CachedInputTokens, u.InputTokens},
		{"cache_miss_input_tokens exceeds input_tokens", u.CacheMissInputTokens, u.InputTokens},
		{"cache_write_tokens exceeds input_tokens", u.CacheWriteTokens, u.InputTokens},
	} {
		if entry.child != nil && entry.parent != nil && *entry.child > *entry.parent {
			metricWarning(s, "invalid usage: "+entry.name)
			valid = false
		}
	}
	if u.InputTokens != nil && u.CachedInputTokens != nil && u.CacheMissInputTokens != nil && *u.CacheMissInputTokens != *u.InputTokens-*u.CachedInputTokens {
		metricWarning(s, "invalid usage: cache hit and miss counts do not sum to input_tokens")
		valid = false
	}
	if u.InputTokens != nil && u.CachedInputTokens != nil && u.CacheWriteTokens != nil && *u.CacheWriteTokens > *u.InputTokens-*u.CachedInputTokens {
		metricWarning(s, "invalid usage: cache read and write counts exceed input_tokens")
		valid = false
	}
	if u.OutputTokens != nil && u.TextOutputTokens != nil && u.ReasoningTokens != nil && *u.TextOutputTokens > *u.OutputTokens-*u.ReasoningTokens {
		metricWarning(s, "invalid usage: text and reasoning counts exceed output_tokens")
		valid = false
	}
	for _, e := range s.Events {
		if e.TokenCount != nil && *e.TokenCount < 0 {
			metricWarning(s, "invalid usage: event token count is negative")
			valid = false
		}
	}
	return valid
}

func metricTime(s *Sample, key string) (float64, bool) {
	v, ok := s.TimingsMS[key]
	// Accept the native httptrace names as well as the normalized timing keys.
	if !ok {
		alias := map[string]string{"dns_end": "dns_done", "tcp_end": "tcp_done", "tls_end": "tls_done", "connection_ready": "got_connection"}[key]
		if alias != "" {
			v, ok = s.TimingsMS[alias]
		}
	}
	return v, ok && v >= 0 && !math.IsNaN(v) && !math.IsInf(v, 0)
}

func metricInterval(s *Sample, start, end string) (float64, bool) {
	a, aok := metricTime(s, start)
	b, bok := metricTime(s, end)
	return b - a, aok && bok && b >= a
}

func setDecodeMetrics(s *Sample, kind string, tokens *int) {
	if tokens == nil || *tokens <= 1 {
		return
	}
	duration, ok := metricInterval(s, "first_"+kind, "last_"+kind)
	if !ok || duration <= 0 {
		return
	}
	var first *int
	chunks := 0
	for _, e := range s.Events {
		if e.Kind == kind {
			if chunks == 0 {
				first = e.TokenCount
			}
			chunks++
		}
	}
	if chunks < 2 {
		return
	}
	if first != nil {
		if *first > *tokens {
			metricWarning(s, "invalid usage: first "+kind+" chunk exceeds its total token count")
			s.Outcome = "failed"
			if s.Error == nil {
				s.Error = &RequestError{Kind: "invalid_usage", Message: "first chunk token count exceeds total " + kind + " tokens"}
			}
			return
		}
		if *first == *tokens {
			return
		}
		s.Metrics[kind+"_decode_tps"] = float64(*tokens-*first) * 1000 / duration
	} else {
		s.Metrics[kind+"_decode_tps_estimate"] = float64(*tokens-1) * 1000 / duration
	}
}

func setCost(s *Sample, p *Price, valid bool) {
	if p == nil {
		metricWarning(s, "cost unavailable: channel pricing is not configured")
		return
	}
	s.Metrics["currency"] = p.Currency
	s.Metrics["price_source"] = p.Source
	s.Metrics["price_as_of"] = p.AsOf
	u := s.Usage
	if !valid || u.InputTokens == nil || u.OutputTokens == nil {
		metricWarning(s, "cost unavailable: valid input and output token usage is required")
		return
	}
	input := *u.InputTokens
	cost := float64(*u.OutputTokens) * p.OutputPerMillion
	if p.CachedInputPerMillion != nil {
		if u.CachedInputTokens == nil {
			metricWarning(s, "cost unavailable: cached input token count is required by cache-discount pricing")
			return
		}
		input -= *u.CachedInputTokens
		cost += float64(*u.CachedInputTokens) * *p.CachedInputPerMillion
	}
	if p.CacheWritePerMillion != nil && u.CacheWriteTokens == nil {
		metricWarning(s, "cost unavailable: cache write token count is required by cache-write pricing")
		return
	}
	if u.CacheWriteTokens != nil && *u.CacheWriteTokens > 0 {
		if p.CacheWritePerMillion == nil {
			metricWarning(s, "cost unavailable: cache writes were reported without a cache-write price")
			return
		}
		input -= *u.CacheWriteTokens
		cost += float64(*u.CacheWriteTokens) * *p.CacheWritePerMillion
	}
	cost += float64(input) * p.InputPerMillion
	cost /= 1e6
	if input < 0 || cost < 0 || math.IsInf(cost, 0) || math.IsNaN(cost) {
		metricWarning(s, "cost unavailable: invalid token allocation or pricing")
		return
	}
	s.Metrics["cost"] = cost
}

// Summarize groups samples by channel, protocol and phase. Only completed samples
// contribute latency and throughput percentiles; every outcome remains counted.
func Summarize(samples []Sample) map[string]any {
	type groupKey struct{ channel, protocol, phase string }
	groups := map[groupKey][]Sample{}
	for _, s := range samples {
		k := groupKey{s.Channel, s.Protocol, s.Phase}
		groups[k] = append(groups[k], s)
	}
	keys := make([]groupKey, 0, len(groups))
	for k := range groups {
		keys = append(keys, k)
	}
	sort.Slice(keys, func(i, j int) bool {
		a, b := keys[i], keys[j]
		if a.channel != b.channel {
			return a.channel < b.channel
		}
		if a.protocol != b.protocol {
			return a.protocol < b.protocol
		}
		return a.phase < b.phase
	})
	result := make([]any, 0, len(keys))
	for _, k := range keys {
		ss := groups[k]
		completed, incomplete, failed := 0, 0, 0
		values := map[string][]float64{}
		costs := map[string]float64{}
		costCounts := map[string]int{}
		for _, s := range ss {
			switch s.Outcome {
			case "completed":
				completed++
			case "incomplete":
				incomplete++
			default:
				failed++
			}
			if cost, ok := metricNumber(s.Metrics["cost"]); ok {
				if currency, ok := s.Metrics["currency"].(string); ok && currency != "" {
					costs[currency] += cost
					costCounts[currency]++
				}
			}
			if s.Outcome != "completed" {
				continue
			}
			for name, value := range s.Metrics {
				if name == "cost" {
					continue
				}
				if n, ok := metricNumber(value); ok {
					values[name] = append(values[name], n)
				}
			}
		}
		metrics := map[string]any{}
		for _, name := range []string{"first_text_ms", "first_reasoning_ms", "first_reasoning_summary_ms", "total_ms", "text_decode_tps", "text_decode_tps_estimate", "reasoning_decode_tps", "reasoning_decode_tps_estimate", "output_tps_e2e", "single_stream_tokens_per_minute", "account_tpm", "cache_hit_ratio", "dns_ms", "tcp_ms", "tls_ms", "post_connect_first_event_ms"} {
			metrics[name] = distribution(values[name])
		}
		costByCurrency := map[string]any{}
		for currency, amount := range costs {
			costByCurrency[currency] = map[string]any{"total": amount, "known_samples": costCounts[currency], "scope": "all_outcomes"}
		}
		tailLabel := "最慢一次 / max"
		if len(values["total_ms"]) >= 100 {
			tailLabel = "p99（经验分位数）"
		}
		result = append(result, map[string]any{
			"channel": k.channel, "protocol": k.protocol, "phase": k.phase,
			"sample_count": len(ss), "completed": completed, "incomplete": incomplete, "failed": failed,
			"failure_rate": float64(failed+incomplete) / float64(len(ss)), "metrics": metrics,
			"tail_label": tailLabel, "cost_by_currency": costByCurrency,
		})
	}
	return map[string]any{"sample_count": len(samples), "groups": result, "percentile_scope": "completed samples only; warmup remains in separate groups", "cost_scope": "all outcomes with known cost, separated by currency"}
}

func metricNumber(v any) (float64, bool) {
	var n float64
	switch x := v.(type) {
	case float64:
		n = x
	case int:
		n = float64(x)
	case int64:
		n = float64(x)
	default:
		return 0, false
	}
	return n, n >= 0 && !math.IsNaN(n) && !math.IsInf(n, 0)
}

func distribution(values []float64) map[string]any {
	result := map[string]any{"n": len(values), "p50": nil, "p95": nil, "max": nil, "p99": nil, "tail_label": "最慢一次 / max"}
	if len(values) == 0 {
		return result
	}
	sort.Float64s(values)
	percentile := func(p float64) float64 { return values[int(math.Ceil(p*float64(len(values))))-1] }
	result["p50"], result["p95"], result["max"] = percentile(.5), percentile(.95), values[len(values)-1]
	if len(values) >= 100 {
		result["p99"] = percentile(.99)
		result["tail_label"] = "p99（经验分位数）"
	}
	return result
}

func metricString(v any) string {
	if v == nil {
		return "—"
	}
	return fmt.Sprint(v)
}
