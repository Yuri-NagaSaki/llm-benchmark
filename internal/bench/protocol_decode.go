package bench

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"strconv"
	"strings"
)

// Decoder translates protocol events without accumulating response text. Usage
// events carry only counts actually supplied by the provider; nil stays unknown.
type Decoder struct {
	protocol       string
	usagePaths     map[string]string
	anthropicInput *int
	anthropicRead  *int
	anthropicWrite *int
}

func NewDecoder(protocol string, usagePaths map[string]string) *Decoder {
	paths := make(map[string]string, len(usagePaths))
	for key, path := range usagePaths {
		paths[key] = path
	}
	return &Decoder{protocol: protocol, usagePaths: paths}
}

func (d *Decoder) Feed(frame SSEFrame) ([]StreamEvent, error) {
	if strings.TrimSpace(frame.Data) == "[DONE]" {
		if d.protocol != "chat" {
			return nil, errors.New("unexpected [DONE] outside the Chat protocol")
		}
		return []StreamEvent{{Kind: "terminal"}}, nil
	}
	if frame.Data == "" {
		return nil, nil
	}
	parser := json.NewDecoder(strings.NewReader(frame.Data))
	parser.UseNumber()
	var payload map[string]any
	if err := parser.Decode(&payload); err != nil || payload == nil {
		return nil, errors.New("stream event is not a valid JSON object")
	}
	if err := parser.Decode(new(any)); err != io.EOF {
		return nil, errors.New("stream event contains trailing JSON or non-JSON data")
	}
	kind := str(payload["type"])
	if kind == "" {
		kind = frame.Event
	} else if frame.Event != "" && frame.Event != "message" && frame.Event != kind {
		return nil, errors.New("SSE event name conflicts with JSON event type")
	}
	var events []StreamEvent
	var providerError *StreamEvent
	if errPayload, exists := payload["error"]; exists && errPayload != nil {
		event := errorEvent(errPayload)
		providerError = &event
	} else if kind == "error" {
		event := errorEvent(payload)
		providerError = &event
	}
	switch d.protocol {
	case "chat":
		decoded, err := d.chat(payload)
		if err != nil {
			return nil, err
		}
		events = append(events, decoded...)
	case "responses":
		events = append(events, d.responses(payload, kind)...)
	case "anthropic":
		events = append(events, d.anthropic(payload, kind)...)
	default:
		return nil, fmt.Errorf("unsupported stream protocol %q", d.protocol)
	}
	if usage := d.usage(payload); usage != nil {
		events = append(events, *usage)
	}
	if providerError != nil {
		events = append(events, *providerError)
	}
	return events, nil
}

func errorEvent(value any) StreamEvent {
	info := object(value)
	kind, message := str(info["type"]), str(info["message"])
	if code := str(info["code"]); code != "" {
		if kind != "" && code != kind {
			kind += "/"
		}
		if !strings.HasSuffix(kind, code) {
			kind += code
		}
	}
	if kind == "" {
		kind = "api_error"
	}
	if message == "" {
		message = str(value)
	}
	if message == "" {
		message = "provider reported an error without a message"
	}
	return StreamEvent{Kind: "error", Error: &RequestError{Kind: kind, Message: message}}
}

func metadata(payload map[string]any) []StreamEvent {
	model, id := str(payload["model"]), str(payload["id"])
	if model == "" && id == "" {
		return nil
	}
	return []StreamEvent{{Kind: "metadata", Model: model, ResponseID: id}}
}

func textEvent(kind string, value any) []StreamEvent {
	if text := str(value); text != "" {
		return []StreamEvent{{Kind: kind, Text: text}}
	}
	return nil
}

func (d *Decoder) chat(payload map[string]any) ([]StreamEvent, error) {
	events := metadata(payload)
	choices := array(payload["choices"])
	if len(choices) > 1 {
		return nil, errors.New("received multiple choices; benchmark supports only n=1")
	}
	for _, raw := range choices {
		choice := object(raw)
		if idx := integer(choice["index"]); idx != nil && *idx != 0 {
			return nil, errors.New("received a nonzero choice index; benchmark supports only n=1")
		}
		delta := object(choice["delta"])
		events = append(events, textEvent("reasoning", delta["reasoning_content"])...)
		if str(delta["reasoning_content"]) == "" {
			events = append(events, textEvent("reasoning", delta["reasoning"])...)
		}
		for _, key := range []string{"content", "reasoning_content", "reasoning", "refusal"} {
			if value := delta[key]; value != nil {
				if _, ok := value.(string); !ok {
					return nil, fmt.Errorf("unsupported non-text Chat delta field %s", key)
				}
			}
		}
		texts := textEvent("text", delta["content"])
		if len(texts) > 0 {
			if logs := array(object(choice["logprobs"])["content"]); len(logs) > 0 {
				n := len(logs)
				texts[0].TokenCount = &n
			}
		}
		events = append(events, texts...)
		events = append(events, textEvent("text", delta["refusal"])...)
		if len(array(delta["tool_calls"])) > 0 || delta["function_call"] != nil {
			events = append(events, StreamEvent{Kind: "tool"})
		}
		if reason := str(choice["finish_reason"]); reason != "" {
			outcome := "completed"
			if reason != "stop" && reason != "tool_calls" && reason != "function_call" {
				outcome = "incomplete"
			}
			events = append(events, StreamEvent{Kind: "finish", Outcome: outcome, FinishReason: reason})
		}
	}
	return events, nil
}

func (d *Decoder) responses(payload map[string]any, kind string) []StreamEvent {
	response := object(payload["response"])
	events := metadata(response)
	switch kind {
	case "response.output_text.delta", "response.refusal.delta":
		texts := textEvent("text", payload["delta"])
		if logs := array(payload["logprobs"]); len(texts) > 0 && len(logs) > 0 {
			n := len(logs)
			texts[0].TokenCount = &n
		}
		events = append(events, texts...)
	case "response.reasoning_text.delta":
		events = append(events, textEvent("reasoning", payload["delta"])...)
	case "response.reasoning_summary_text.delta", "response.reasoning_summary.delta":
		events = append(events, textEvent("reasoning_summary", payload["delta"])...)
	case "response.function_call_arguments.delta", "response.custom_tool_call_input.delta":
		events = append(events, StreamEvent{Kind: "tool", Text: str(payload["delta"])})
	case "response.output_item.added", "response.output_item.done":
		itemType := str(object(payload["item"])["type"])
		if isResponseTool(itemType) {
			events = append(events, StreamEvent{Kind: "tool"})
		}
	case "response.completed", "response.incomplete", "response.failed", "response.cancelled":
		outcome := strings.TrimPrefix(kind, "response.")
		reason := outcome
		if outcome == "incomplete" {
			if r := str(object(response["incomplete_details"])["reason"]); r != "" {
				reason = r
			}
		}
		if outcome == "failed" {
			events = append(events, errorEvent(response["error"]))
		}
		if outcome == "cancelled" {
			outcome = "incomplete"
		}
		// The terminal response repeats output snapshots; never count them as
		// deltas. Tool-only results still constitute valid output.
		for _, raw := range array(response["output"]) {
			if isResponseTool(str(object(raw)["type"])) {
				events = append(events, StreamEvent{Kind: "tool"})
				break
			}
		}
		events = append(events, StreamEvent{Kind: "finish", Outcome: outcome, FinishReason: reason}, StreamEvent{Kind: "terminal"})
	default:
		if strings.HasPrefix(kind, "response.") && strings.Contains(kind, "_call.") {
			events = append(events, StreamEvent{Kind: "tool"})
		}
	}
	return events
}

func isResponseTool(itemType string) bool {
	return strings.HasSuffix(itemType, "_call") || strings.HasSuffix(itemType, "_call_output") || itemType == "function_call" || itemType == "mcp_list_tools" || itemType == "mcp_approval_request"
}

func (d *Decoder) anthropic(payload map[string]any, kind string) []StreamEvent {
	var events []StreamEvent
	switch kind {
	case "message_start":
		events = append(events, metadata(object(payload["message"]))...)
	case "content_block_start":
		block := object(payload["content_block"])
		switch str(block["type"]) {
		case "text":
			events = append(events, textEvent("text", block["text"])...)
		case "thinking":
			events = append(events, textEvent("reasoning", block["thinking"])...)
		case "tool_use", "server_tool_use":
			events = append(events, StreamEvent{Kind: "tool"})
		}
	case "content_block_delta":
		delta := object(payload["delta"])
		switch str(delta["type"]) {
		case "text_delta":
			events = append(events, textEvent("text", delta["text"])...)
		case "thinking_delta":
			events = append(events, textEvent("reasoning", delta["thinking"])...)
		case "input_json_delta":
			events = append(events, StreamEvent{Kind: "tool", Text: str(delta["partial_json"])})
		}
	case "message_delta":
		if reason := str(object(payload["delta"])["stop_reason"]); reason != "" {
			outcome := "completed"
			if reason != "end_turn" && reason != "stop_sequence" && reason != "tool_use" && reason != "refusal" {
				outcome = "incomplete"
			}
			events = append(events, StreamEvent{Kind: "finish", Outcome: outcome, FinishReason: reason})
		}
	case "message_stop":
		events = append(events, StreamEvent{Kind: "terminal"})
	}
	return events
}

func dotted(value any, path string) any {
	if path == "" {
		return nil
	}
	for _, key := range strings.Split(path, ".") {
		switch node := value.(type) {
		case map[string]any:
			value = node[key]
		case []any:
			index, err := strconv.Atoi(key)
			if err != nil || index < 0 || index >= len(node) {
				return nil
			}
			value = node[index]
		default:
			return nil
		}
	}
	return value
}

func integer(value any) *int {
	var n int64
	switch v := value.(type) {
	case json.Number:
		parsed, err := v.Int64()
		if err != nil {
			return nil
		}
		n = parsed
	case int:
		n = int64(v)
	case int64:
		n = v
	case float64:
		if math.IsNaN(v) || math.IsInf(v, 0) || v < 0 || v >= float64(math.MaxInt64) || math.Trunc(v) != v {
			return nil
		}
		n = int64(v)
	default:
		return nil
	}
	if n < 0 || int64(int(n)) != n {
		return nil
	}
	result := int(n)
	return &result
}

func firstCount(raw map[string]any, paths ...string) *int {
	for _, path := range paths {
		if n := integer(dotted(raw, path)); n != nil {
			return n
		}
	}
	return nil
}

func (d *Decoder) usage(payload map[string]any) *StreamEvent {
	raw := object(payload["usage"])
	if raw == nil {
		raw = object(dotted(payload, "response.usage"))
	}
	if raw == nil {
		raw = object(dotted(payload, "message.usage"))
	}
	u := Usage{
		InputTokens:          firstCount(raw, "prompt_tokens", "input_tokens"),
		OutputTokens:         firstCount(raw, "completion_tokens", "output_tokens"),
		ReasoningTokens:      firstCount(raw, "completion_tokens_details.reasoning_tokens", "output_tokens_details.reasoning_tokens", "reasoning_tokens"),
		TextOutputTokens:     firstCount(raw, "completion_tokens_details.text_tokens", "output_tokens_details.text_tokens", "text_output_tokens"),
		CachedInputTokens:    firstCount(raw, "prompt_tokens_details.cached_tokens", "input_tokens_details.cached_tokens", "prompt_cache_hit_tokens", "cache_read_input_tokens", "cached_input_tokens"),
		CacheMissInputTokens: firstCount(raw, "prompt_cache_miss_tokens", "uncached_input_tokens", "cache_miss_input_tokens"),
		CacheWriteTokens:     firstCount(raw, "cache_creation_input_tokens", "input_tokens_details.cache_write_tokens", "prompt_tokens_details.cache_write_tokens", "cache_write_tokens"),
	}
	if d.protocol == "anthropic" && raw != nil {
		// Anthropic reports uncached input separately from cache read and
		// creation. Keep cumulative components across partial usage events.
		if n := integer(raw["input_tokens"]); n != nil {
			d.anthropicInput = n
		}
		if n := integer(raw["cache_read_input_tokens"]); n != nil {
			d.anthropicRead = n
		}
		if n := integer(raw["cache_creation_input_tokens"]); n != nil {
			d.anthropicWrite = n
		}
		u.InputTokens = sumCounts(d.anthropicInput, d.anthropicRead, d.anthropicWrite)
		u.CacheMissInputTokens = sumCounts(d.anthropicInput, d.anthropicWrite)
	}
	fields := map[string]**int{
		"input_tokens":            &u.InputTokens,
		"output_tokens":           &u.OutputTokens,
		"reasoning_tokens":        &u.ReasoningTokens,
		"text_output_tokens":      &u.TextOutputTokens,
		"cached_input_tokens":     &u.CachedInputTokens,
		"uncached_input_tokens":   &u.CacheMissInputTokens,
		"cache_miss_input_tokens": &u.CacheMissInputTokens,
		"cache_write_tokens":      &u.CacheWriteTokens,
	}
	mapped := map[string]any{}
	for name, path := range d.usagePaths {
		if target, ok := fields[name]; ok {
			value := dotted(payload, path)
			*target = integer(value)
			if value != nil {
				mapped[path] = value
			}
		}
	}
	if raw == nil && len(mapped) == 0 {
		return nil
	}
	if raw == nil {
		raw = map[string]any{"mapped_fields": mapped}
	}
	return &StreamEvent{Kind: "usage", Usage: &u, RawUsage: raw}
}

// sumCounts computes exact protocol arithmetic, never estimates an absent count.
func sumCounts(counts ...*int) *int {
	total := 0
	for _, n := range counts {
		if n == nil || *n > int(^uint(0)>>1)-total {
			return nil
		}
		total += *n
	}
	return &total
}
