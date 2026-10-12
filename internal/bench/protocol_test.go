package bench

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"
)

func protocolWorkload() Workload {
	return Workload{Messages: []map[string]any{{"role": "user", "content": "hello"}}, MaxOutputTokens: 128}
}

func TestBuildBodyPreservesChatWorkloadAndExtraFields(t *testing.T) {
	w := protocolWorkload()
	w.Messages = append(w.Messages, map[string]any{"role": "assistant", "content": "hi", "reasoning_content": "prior reasoning"}, map[string]any{"role": "user", "content": []any{
		map[string]any{"type": "image_url", "image_url": map[string]any{"url": "data:image/png;base64,aGVsbG8=", "detail": "high"}},
		map[string]any{"type": "file", "file": map[string]any{"file_id": "file_123"}},
	}})
	ch := Channel{Model: "exact-model", Protocol: "chat", ExtraBody: map[string]any{"top_k": 20, "response_format": map[string]any{"type": "json_object"}, "n": 1}}
	body, err := BuildBody(ch, w, ThinkingConfig{Mode: "default"})
	if err != nil {
		t.Fatal(err)
	}
	if body["max_tokens"] != 128 || body["model"] != "exact-model" || body["top_k"] != 20 || body["stream"] != true {
		t.Fatalf("body = %#v", body)
	}
	want, _ := json.Marshal(w.Messages)
	got, _ := json.Marshal(body["messages"])
	if string(got) != string(want) {
		t.Fatalf("messages changed: %s", got)
	}
	body["response_format"].(map[string]any)["type"] = "mutated"
	if ch.ExtraBody["response_format"].(map[string]any)["type"] != "json_object" {
		t.Fatal("BuildBody aliased mutable config")
	}
}

func TestBuildBodyResponsesMultiTurnImagesFilesAndTools(t *testing.T) {
	w := protocolWorkload()
	w.Messages = []map[string]any{
		{"role": "system", "content": "Be exact."},
		{"role": "developer", "content": "Use tools."},
		{"role": "user", "content": []any{
			map[string]any{"type": "text", "text": "inspect"},
			map[string]any{"type": "image_url", "image_url": map[string]any{"url": "https://example.com/a.png", "detail": "high"}},
			map[string]any{"type": "file", "file": map[string]any{"file_data": "data:application/pdf;base64,JVBERg==", "filename": "a.pdf"}},
		}},
		{"role": "assistant", "content": nil, "tool_calls": []any{map[string]any{"id": "call1", "type": "function", "function": map[string]any{"name": "lookup", "arguments": "{\"x\":1}"}}}},
		{"role": "tool", "tool_call_id": "call1", "content": "found"},
		{"role": "assistant", "content": []any{map[string]any{"type": "text", "text": "I found it."}}},
		{"role": "user", "content": "Continue."},
	}
	body, err := BuildBody(Channel{Protocol: "responses", Model: "exact"}, w, ThinkingConfig{Mode: "default"})
	if err != nil {
		t.Fatal(err)
	}
	input := body["input"].([]any)
	if len(input) != 7 || body["max_output_tokens"] != 128 {
		t.Fatalf("body = %#v", body)
	}
	parts := input[2].(map[string]any)["content"].([]any)
	if parts[1].(map[string]any)["image_url"] != "https://example.com/a.png" || parts[2].(map[string]any)["filename"] != "a.pdf" {
		t.Fatalf("multimodal contents changed: %#v", parts)
	}
	if input[3].(map[string]any)["type"] != "function_call" || input[4].(map[string]any)["call_id"] != "call1" {
		t.Fatalf("tool sequence changed: %#v", input)
	}
	if input[5].(map[string]any)["content"].([]any)[0].(map[string]any)["type"] != "output_text" {
		t.Fatal("assistant text did not become output_text")
	}
}

func TestBuildBodyRejectsLossyConversions(t *testing.T) {
	for name, message := range map[string]map[string]any{
		"name":              {"role": "user", "content": "hi", "name": "person"},
		"unknown role":      {"role": "function", "content": "hi"},
		"reasoning history": {"role": "assistant", "content": "hi", "reasoning_content": "thought"},
		"audio":             {"role": "user", "content": []any{map[string]any{"type": "input_audio", "input_audio": map[string]any{"data": "x"}}}},
		"invalid image":     {"role": "user", "content": []any{map[string]any{"type": "image_url", "image_url": map[string]any{"url": "x", "custom": true}}}},
		"tool without ID":   {"role": "tool", "content": "result"},
		"tool on user":      {"role": "user", "content": "result", "tool_calls": []any{}},
		"nil user":          {"role": "user", "content": nil},
		"primitive content": {"role": "user", "content": 1},
	} {
		t.Run(name, func(t *testing.T) {
			w := protocolWorkload()
			w.Messages = []map[string]any{message}
			if _, err := BuildBody(Channel{Model: "m", Protocol: "responses"}, w, ThinkingConfig{}); err == nil {
				t.Fatal("lossy request accepted")
			}
		})
	}
}

func TestBuildBodyProtectedFieldsAndChoices(t *testing.T) {
	for _, key := range []string{"model", "stream", "messages", "input", "max_tokens", "max_completion_tokens", "max_output_tokens", "n", "stream_options"} {
		t.Run(key, func(t *testing.T) {
			ch := Channel{Model: "m", Protocol: "chat", ExtraBody: map[string]any{key: 2}}
			if _, err := BuildBody(ch, protocolWorkload(), ThinkingConfig{}); err == nil {
				t.Fatalf("protected field %s accepted", key)
			}
		})
	}
}

func TestBuildBodyThinkingModes(t *testing.T) {
	for _, profile := range []string{"deepseek", "tencent", "aliyun"} {
		for _, protocol := range []string{"chat", "responses"} {
			t.Run(profile+"/"+protocol, func(t *testing.T) {
				ch := Channel{Model: "m", Protocol: protocol, Profile: profile}
				body, err := BuildBody(ch, protocolWorkload(), ThinkingConfig{Mode: "off"})
				if err != nil {
					t.Fatal(err)
				}
				if protocol == "responses" && body["reasoning"].(map[string]any)["effort"] != "none" {
					t.Fatalf("off = %#v", body)
				}
				if protocol == "chat" && profile == "aliyun" && body["enable_thinking"] != false {
					t.Fatalf("off = %#v", body)
				}
				if protocol == "chat" && profile != "aliyun" && body["thinking"].(map[string]any)["type"] != "disabled" {
					t.Fatalf("off = %#v", body)
				}
				body, err = BuildBody(ch, protocolWorkload(), ThinkingConfig{Mode: "default"})
				if err != nil {
					t.Fatal(err)
				}
				for _, key := range []string{"thinking", "reasoning", "enable_thinking"} {
					if _, exists := body[key]; exists {
						t.Fatalf("default injected %s", key)
					}
				}
			})
		}
	}
	ch := Channel{Model: "m", Protocol: "responses", ThinkingBodies: map[string]map[string]any{
		"on": {"reasoning": map[string]any{"effort": "$effort", "summary": "detailed"}, "vendor_config": map[string]any{"levels": []any{"$effort"}}},
	}}
	body, err := BuildBody(ch, protocolWorkload(), ThinkingConfig{Mode: "on", Effort: "max"})
	if err != nil || body["reasoning"].(map[string]any)["effort"] != "max" {
		t.Fatalf("custom on: body=%#v err=%v", body, err)
	}
	if ch.ThinkingBodies["on"]["reasoning"].(map[string]any)["effort"] != "$effort" {
		t.Fatal("thinking config mutated")
	}
	for _, config := range []ThinkingConfig{{Mode: "on"}, {Mode: "off"}, {Mode: "default", Effort: "high"}} {
		if _, err := BuildBody(ch, protocolWorkload(), config); err == nil {
			t.Fatalf("unsupported thinking config accepted: %#v", config)
		}
	}
}

func TestBuildBodyAnthropicSystemAndMultimodal(t *testing.T) {
	w := protocolWorkload()
	w.Messages = []map[string]any{
		{"role": "system", "content": "System"},
		{"role": "user", "content": []any{
			map[string]any{"type": "image_url", "image_url": map[string]any{"url": "data:image/png;base64,aGVsbG8="}},
			map[string]any{"type": "file", "file": map[string]any{"filename": "a.pdf", "file_data": "data:application/pdf;base64,JVBERg=="}},
		}},
	}
	ch := Channel{Model: "m", Protocol: "anthropic"}
	body, err := BuildBody(ch, w, ThinkingConfig{})
	if err != nil {
		t.Fatal(err)
	}
	if len(body["system"].([]any)) != 1 || len(body["messages"].([]any)) != 1 {
		t.Fatalf("body = %#v", body)
	}
	w.Messages = append(w.Messages, map[string]any{"role": "system", "content": "Later system"})
	if _, err := BuildBody(ch, w, ThinkingConfig{}); err == nil {
		t.Fatal("interleaved system message silently moved")
	}
}

func decodeFixture(t *testing.T, d *Decoder, payload string) []StreamEvent {
	t.Helper()
	events, err := d.Feed(SSEFrame{Data: payload})
	if err != nil {
		t.Fatal(err)
	}
	return events
}

func kindEvents(events []StreamEvent, kind string) []StreamEvent {
	var result []StreamEvent
	for _, event := range events {
		if event.Kind == kind {
			result = append(result, event)
		}
	}
	return result
}

func requireCount(t *testing.T, count *int, want int) {
	t.Helper()
	if count == nil || *count != want {
		t.Fatalf("count = %v, want %d", count, want)
	}
}

func TestDecoderChatReasoningUsageAndLength(t *testing.T) {
	d := NewDecoder("chat", nil)
	events := decodeFixture(t, d, `{"id":"chat1","model":"exact-model","choices":[{"index":0,"delta":{"reasoning_content":"thinking"},"finish_reason":null}]}`)
	if len(kindEvents(events, "reasoning")) != 1 || len(kindEvents(events, "text")) != 0 || events[0].Model != "exact-model" {
		t.Fatalf("events = %#v", events)
	}
	events = decodeFixture(t, d, `{"choices":[{"index":0,"delta":{},"finish_reason":"length"}],"usage":{"prompt_tokens":100,"completion_tokens":9,"prompt_cache_hit_tokens":64,"prompt_cache_miss_tokens":36}}`)
	finish := kindEvents(events, "finish")[0]
	usage := kindEvents(events, "usage")[0]
	if finish.Outcome != "incomplete" || finish.FinishReason != "length" {
		t.Fatalf("finish = %#v", finish)
	}
	requireCount(t, usage.Usage.CachedInputTokens, 64)
	requireCount(t, usage.Usage.CacheMissInputTokens, 36)
	if usage.Usage.ReasoningTokens != nil || usage.Usage.TextOutputTokens != nil {
		t.Fatal("missing output breakdown must remain unknown")
	}
	if len(kindEvents(events, "terminal")) != 0 || decodeFixture(t, d, "[DONE]")[0].Kind != "terminal" {
		t.Fatal("finish and transport terminal were conflated")
	}
}

func TestDecoderChatRefusalToolsAndExplicitLogprobs(t *testing.T) {
	d := NewDecoder("chat", nil)
	events := decodeFixture(t, d, `{"choices":[{"delta":{"content":"a b","refusal":"no"},"logprobs":{"content":[{"token":"a"},{"token":" b"}]}}]}`)
	texts := kindEvents(events, "text")
	if len(texts) != 2 || texts[1].Text != "no" {
		t.Fatalf("text events = %#v", texts)
	}
	requireCount(t, texts[0].TokenCount, 2)
	events = decodeFixture(t, d, `{"choices":[{"delta":{"tool_calls":[{"index":0,"function":{"arguments":"{}"}}]},"finish_reason":"tool_calls"}]}`)
	if len(kindEvents(events, "tool")) != 1 || kindEvents(events, "finish")[0].Outcome != "completed" {
		t.Fatalf("tool events = %#v", events)
	}
	if _, err := d.Feed(SSEFrame{Data: `{"choices":[{},{}]}`}); err == nil {
		t.Fatal("multiple choices accepted")
	}
}

func TestDecoderResponsesSummaryAndTerminalSnapshots(t *testing.T) {
	d := NewDecoder("responses", nil)
	for eventType, wantKind := range map[string]string{
		"response.reasoning_summary_text.delta": "reasoning_summary",
		"response.reasoning_text.delta":         "reasoning",
		"response.output_text.delta":            "text",
		"response.refusal.delta":                "text",
	} {
		events := decodeFixture(t, d, `{"type":"`+eventType+`","delta":"some output"}`)
		if len(events) != 1 || events[0].Kind != wantKind || events[0].TokenCount != nil {
			t.Fatalf("%s events = %#v", eventType, events)
		}
	}
	events := decodeFixture(t, d, `{"type":"response.completed","response":{"id":"resp1","model":"actual","output":[{"type":"message","content":[{"type":"output_text","text":"some output"}]}],"usage":{"input_tokens":8,"output_tokens":7,"input_tokens_details":{"cached_tokens":0},"output_tokens_details":{"reasoning_tokens":4}}}}`)
	if len(kindEvents(events, "text")) != 0 || len(kindEvents(events, "reasoning")) != 0 {
		t.Fatal("terminal snapshot was counted as streamed output")
	}
	if kindEvents(events, "metadata")[0].ResponseID != "resp1" || kindEvents(events, "finish")[0].Outcome != "completed" || len(kindEvents(events, "terminal")) != 1 {
		t.Fatalf("terminal events = %#v", events)
	}
	u := kindEvents(events, "usage")[0].Usage
	requireCount(t, u.ReasoningTokens, 4)
	requireCount(t, u.CachedInputTokens, 0)
	if u.TextOutputTokens != nil {
		t.Fatal("text tokens inferred without a declared breakdown")
	}
}

func TestDecoderFailuresIncompleteAndRootErrors(t *testing.T) {
	d := NewDecoder("responses", nil)
	events := decodeFixture(t, d, `{"type":"response.incomplete","response":{"incomplete_details":{"reason":"max_output_tokens"},"usage":{"output_tokens":11}}}`)
	if kindEvents(events, "finish")[0].Outcome != "incomplete" || kindEvents(events, "finish")[0].FinishReason != "max_output_tokens" {
		t.Fatalf("incomplete = %#v", events)
	}
	events = decodeFixture(t, d, `{"type":"response.failed","response":{"error":{"code":"server_error","message":"failed upstream"},"usage":{"output_tokens":2}}}`)
	if kindEvents(events, "error")[0].Error.Message != "failed upstream" || kindEvents(events, "finish")[0].Outcome != "failed" || len(kindEvents(events, "usage")) != 1 {
		t.Fatalf("failed = %#v", events)
	}
	for _, protocol := range []string{"chat", "responses", "anthropic"} {
		for _, payload := range []string{`{"error":{"type":"rate_limit","code":"quota","message":"try later"}}`, `{"type":"error","code":"quota","message":"try later"}`} {
			events := decodeFixture(t, NewDecoder(protocol, nil), payload)
			if len(kindEvents(events, "error")) != 1 || !strings.Contains(kindEvents(events, "error")[0].Error.Kind, "quota") {
				t.Fatalf("lost %s error: %#v", protocol, events)
			}
		}
	}
}

func TestDecoderUsageMappingsAndMissingIncrementalFields(t *testing.T) {
	d := NewDecoder("responses", map[string]string{
		"cached_input_tokens": "response.billing.cache_hit",
		"reasoning_tokens":    "response.billing.thought_tokens",
		"cache_write_tokens":  "response.billing.write_tokens",
	})
	events := decodeFixture(t, d, `{"type":"response.completed","response":{"usage":{"input_tokens":100,"output_tokens":15,"input_tokens_details":{"cached_tokens":99}},"billing":{"cache_hit":50,"thought_tokens":0,"write_tokens":10}}}`)
	u := kindEvents(events, "usage")[0].Usage
	requireCount(t, u.InputTokens, 100)
	requireCount(t, u.CachedInputTokens, 50)
	requireCount(t, u.ReasoningTokens, 0)
	requireCount(t, u.CacheWriteTokens, 10)
	events = decodeFixture(t, d, `{"type":"response.in_progress","response":{"usage":{"output_tokens":2}}}`)
	u = kindEvents(events, "usage")[0].Usage
	if u.InputTokens != nil || u.CachedInputTokens != nil || u.ReasoningTokens != nil {
		t.Fatalf("incremental absence replaced earlier values with zero: %#v", u)
	}
	for _, invalid := range []string{"-1", "1.5", `"10"`, "null", "true", "9223372036854775808"} {
		events = decodeFixture(t, NewDecoder("chat", nil), `{"usage":{"prompt_tokens":`+invalid+`}}`)
		if kindEvents(events, "usage")[0].Usage.InputTokens != nil {
			t.Fatalf("invalid count accepted: %s", invalid)
		}
	}
}

func TestDecoderAnthropicLifecycle(t *testing.T) {
	d := NewDecoder("anthropic", nil)
	events := decodeFixture(t, d, `{"type":"message_start","message":{"id":"msg1","model":"claude","usage":{"input_tokens":20,"cache_read_input_tokens":10,"cache_creation_input_tokens":5,"output_tokens":0}}}`)
	u := kindEvents(events, "usage")[0].Usage
	requireCount(t, u.InputTokens, 35)
	requireCount(t, u.CacheMissInputTokens, 25)
	requireCount(t, u.CachedInputTokens, 10)
	requireCount(t, u.CacheWriteTokens, 5)
	if events[0].ResponseID != "msg1" {
		t.Fatalf("metadata = %#v", events)
	}
	events = decodeFixture(t, d, `{"type":"content_block_delta","delta":{"type":"thinking_delta","thinking":"thinking"}}`)
	if len(events) != 1 || events[0].Kind != "reasoning" {
		t.Fatalf("thinking = %#v", events)
	}
	events = decodeFixture(t, d, `{"type":"content_block_delta","delta":{"type":"input_json_delta","partial_json":"{\"x\":1}"}}`)
	if len(events) != 1 || events[0].Kind != "tool" {
		t.Fatalf("tool = %#v", events)
	}
	events = decodeFixture(t, d, `{"type":"message_delta","delta":{"stop_reason":"max_tokens"},"usage":{"output_tokens":15}}`)
	if kindEvents(events, "finish")[0].Outcome != "incomplete" {
		t.Fatalf("message_delta = %#v", events)
	}
	requireCount(t, kindEvents(events, "usage")[0].Usage.InputTokens, 35)
	events = decodeFixture(t, d, `{"type":"message_stop"}`)
	if len(events) != 1 || events[0].Kind != "terminal" {
		t.Fatalf("message_stop = %#v", events)
	}
}

func TestDecoderRejectsMalformedPayloadsAndConflictingEvents(t *testing.T) {
	d := NewDecoder("responses", nil)
	for _, payload := range []string{"not JSON", `[]`, `null`, `1`, `{"type":`, `{} {}`, "[DONE]"} {
		if _, err := d.Feed(SSEFrame{Data: payload}); err == nil {
			t.Fatalf("accepted invalid payload: %s", payload)
		}
	}
	if _, err := d.Feed(SSEFrame{Event: "response.failed", Data: `{"type":"response.completed"}`}); err == nil {
		t.Fatal("event type mismatch accepted")
	}
	events, err := d.Feed(SSEFrame{Event: "response.output_text.delta", Data: `{"delta":"hello"}`})
	if err != nil || !reflect.DeepEqual(events, []StreamEvent{{Kind: "text", Text: "hello"}}) {
		t.Fatalf("event-name fallback: events=%#v err=%v", events, err)
	}
}

func TestBuildBodyValidatesUsageMapNamesAndPaths(t *testing.T) {
	for _, paths := range []map[string]string{
		{"made_up_tokens": "usage.foo"},
		{"input_tokens": ""},
		{"input_tokens": "usage..input_tokens"},
		{"uncached_input_tokens": "usage.a", "cache_miss_input_tokens": "usage.b"},
	} {
		_, err := BuildBody(Channel{Model: "m", Protocol: "chat", UsagePaths: paths}, protocolWorkload(), ThinkingConfig{})
		if err == nil {
			t.Fatalf("accepted invalid usage paths: %#v", paths)
		}
	}
}

func TestDecoderChatRejectsUnsupportedContentAndKeepsRootErrorLast(t *testing.T) {
	d := NewDecoder("chat", nil)
	if _, err := d.Feed(SSEFrame{Data: `{"choices":[{"delta":{"content":[{"type":"text","text":"ignored"}]}}]}`}); err == nil {
		t.Fatal("unsupported content silently discarded")
	}
	events := decodeFixture(t, d, `{"error":{"code":"failed","message":"upstream failed"},"choices":[{"delta":{},"finish_reason":"stop"}]}`)
	if events[len(events)-1].Kind != "error" {
		t.Fatalf("finish could overwrite root error in the same frame: %#v", events)
	}
}

func TestDecoderResponseToolOnlyOutputIsValidWithoutText(t *testing.T) {
	d := NewDecoder("responses", nil)
	for _, payload := range []string{
		`{"type":"response.output_item.added","item":{"type":"function_call","call_id":"x"}}`,
		`{"type":"response.function_call_arguments.delta","delta":"{}"}`,
		`{"type":"response.web_search_call.completed"}`,
		`{"type":"response.completed","response":{"output":[{"type":"function_call","call_id":"x","arguments":"{}"}]}}`,
	} {
		events := decodeFixture(t, d, payload)
		if len(kindEvents(events, "tool")) == 0 || len(kindEvents(events, "text")) != 0 {
			t.Fatalf("tool-only response lost: %#v", events)
		}
	}
}

func TestDecoderCustomUsageOutsideStandardUsageObject(t *testing.T) {
	d := NewDecoder("chat", map[string]string{"input_tokens": "billing.0.tokens", "cached_input_tokens": "billing.0.cached"})
	events := decodeFixture(t, d, `{"billing":[{"tokens":9,"cached":0}]}`)
	if len(events) != 1 || events[0].RawUsage == nil {
		t.Fatalf("custom-only usage was lost: %#v", events)
	}
	requireCount(t, events[0].Usage.InputTokens, 9)
	requireCount(t, events[0].Usage.CachedInputTokens, 0)
}

func TestDecoderAnthropicMissingCacheCountsRemainUnknown(t *testing.T) {
	d := NewDecoder("anthropic", nil)
	events := decodeFixture(t, d, `{"type":"message_start","message":{"usage":{"input_tokens":20,"output_tokens":0}}}`)
	u := kindEvents(events, "usage")[0].Usage
	if u.InputTokens != nil || u.CacheMissInputTokens != nil || u.CachedInputTokens != nil {
		t.Fatalf("unknown cache components treated as zero: %#v", u)
	}
	events = decodeFixture(t, d, `{"type":"message_delta","usage":{"cache_creation_input_tokens":2,"cache_read_input_tokens":10,"output_tokens":5}}`)
	u = kindEvents(events, "usage")[0].Usage
	requireCount(t, u.InputTokens, 32)
	requireCount(t, u.CacheMissInputTokens, 22)
	events = decodeFixture(t, d, `{"type":"message_delta","usage":{"input_tokens":23,"output_tokens":6}}`)
	u = kindEvents(events, "usage")[0].Usage
	requireCount(t, u.InputTokens, 35)
	requireCount(t, u.CacheMissInputTokens, 25)
}
