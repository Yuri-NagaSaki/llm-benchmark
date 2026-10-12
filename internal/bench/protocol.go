package bench

import (
	"errors"
	"fmt"
	"strings"
)

// BuildBody retains the workload across protocol conversions. Structures whose
// semantics cannot be represented in the target protocol are rejected explicitly.
func BuildBody(ch Channel, w Workload, t ThinkingConfig) (map[string]any, error) {
	if ch.Model == "" || len(w.Messages) == 0 || w.MaxOutputTokens < 1 {
		return nil, errors.New("model, nonempty messages and positive max_output_tokens are required")
	}
	if _, exists := ch.UsagePaths["uncached_input_tokens"]; exists {
		if _, duplicate := ch.UsagePaths["cache_miss_input_tokens"]; duplicate {
			return nil, errors.New("usage_paths must use only one of uncached_input_tokens or cache_miss_input_tokens")
		}
	}
	for name, path := range ch.UsagePaths {
		switch name {
		case "input_tokens", "output_tokens", "reasoning_tokens", "text_output_tokens", "cached_input_tokens", "cache_miss_input_tokens", "uncached_input_tokens", "cache_write_tokens":
		default:
			return nil, fmt.Errorf("unknown normalized usage field %q", name)
		}
		for _, part := range strings.Split(path, ".") {
			if part == "" {
				return nil, fmt.Errorf("usage path for %s contains an empty component", name)
			}
		}
	}
	body := map[string]any{"model": ch.Model, "stream": true}
	if w.Temperature != nil {
		body["temperature"] = *w.Temperature
	}
	switch ch.Protocol {
	case "chat":
		body["messages"] = cloneJSON(w.Messages)
		limitField := ch.TokenLimitField
		if limitField == "" {
			limitField = "max_tokens"
		}
		if limitField != "max_tokens" && limitField != "max_completion_tokens" {
			return nil, errors.New("chat token_limit_field must be max_tokens or max_completion_tokens")
		}
		body[limitField] = w.MaxOutputTokens
		body["stream_options"] = map[string]any{"include_usage": true}
	case "responses":
		input, err := responsesInput(w.Messages)
		if err != nil {
			return nil, err
		}
		body["input"], body["max_output_tokens"] = input, w.MaxOutputTokens
	case "anthropic":
		messages, system, err := anthropicInput(w.Messages)
		if err != nil {
			return nil, err
		}
		body["messages"], body["max_tokens"] = messages, w.MaxOutputTokens
		if len(system) != 0 {
			body["system"] = system
		}
	default:
		return nil, fmt.Errorf("unsupported protocol %q", ch.Protocol)
	}
	if err := mergeRoot(body, ch.ExtraBody); err != nil {
		return nil, fmt.Errorf("extra_body: %w", err)
	}
	if t.Mode == "" {
		t.Mode = "default"
	}
	if err := validateThinking(t); err != nil {
		return nil, err
	}
	if t.Mode != "default" {
		mapping, configured := ch.ThinkingBodies[t.Mode]
		if !configured {
			mapping = profileThinking(ch, t)
		}
		if mapping == nil || len(mapping) == 0 {
			return nil, fmt.Errorf("thinking mode %q requires a supported profile or thinking_bodies mapping", t.Mode)
		}
		resolved, err := replaceEffort(mapping, t.Effort)
		if err != nil {
			return nil, err
		}
		if err := mergeRoot(body, resolved.(map[string]any)); err != nil {
			return nil, fmt.Errorf("thinking_bodies: %w", err)
		}
	}
	return body, nil
}

func mergeRoot(dst, src map[string]any) error {
	for key, value := range src {
		switch key {
		case "audio", "prediction":
			return fmt.Errorf("%s output is not supported by the text/reasoning measurement contract", key)
		case "modalities":
			items := array(value)
			if len(items) != 1 || items[0] != "text" {
				return errors.New("only text output modality is supported")
			}
		case "model", "stream", "messages", "input", "system", "instructions", "previous_response_id", "conversation", "prompt", "max_tokens", "max_completion_tokens", "max_output_tokens":
			return fmt.Errorf("%s is owned by the benchmark workload", key)
		case "n":
			if n := integer(value); n == nil || *n != 1 {
				return errors.New("n must be 1; multiple choices mix independent generations")
			}
		case "stream_options":
			options, ok := value.(map[string]any)
			if !ok || options["include_usage"] != true {
				return errors.New("stream_options must retain include_usage=true")
			}
		}
		dst[key] = cloneJSON(value)
	}
	return nil
}

func profileThinking(ch Channel, t ThinkingConfig) map[string]any {
	if ch.Protocol == "chat" {
		switch ch.Profile {
		case "deepseek", "tencent":
			mode := "enabled"
			if t.Mode == "off" {
				mode = "disabled"
			}
			result := map[string]any{"thinking": map[string]any{"type": mode}}
			if t.Effort != "" {
				result["reasoning_effort"] = t.Effort
			}
			if ch.Profile == "deepseek" && t.Mode == "off" {
				result["reasoning_effort"] = "none"
			}
			return result
		case "aliyun":
			// Aliyun models use enable_thinking and model-specific budgets; an
			// effort string has no universal equivalent.
			if t.Effort == "" {
				return map[string]any{"enable_thinking": t.Mode == "on"}
			}
		}
	}
	if ch.Protocol == "responses" && (ch.Profile == "deepseek" || ch.Profile == "tencent" || ch.Profile == "aliyun") {
		effort := t.Effort
		if t.Mode == "off" {
			effort = "none"
		}
		if effort != "" {
			return map[string]any{"reasoning": map[string]any{"effort": effort}}
		}
	}
	return nil
}

func replaceEffort(value any, effort string) (any, error) {
	switch v := value.(type) {
	case string:
		if v == "$effort" {
			if effort == "" {
				return nil, errors.New("thinking mapping uses $effort but no effort was configured")
			}
			return effort, nil
		}
	case map[string]any:
		out := make(map[string]any, len(v))
		for key, item := range v {
			resolved, err := replaceEffort(item, effort)
			if err != nil {
				return nil, err
			}
			out[key] = resolved
		}
		return out, nil
	case []any:
		out := make([]any, len(v))
		for i, item := range v {
			resolved, err := replaceEffort(item, effort)
			if err != nil {
				return nil, err
			}
			out[i] = resolved
		}
		return out, nil
	}
	return value, nil
}

func cloneJSON(v any) any {
	switch value := v.(type) {
	case map[string]any:
		out := make(map[string]any, len(value))
		for k, item := range value {
			out[k] = cloneJSON(item)
		}
		return out
	case []map[string]any:
		out := make([]any, len(value))
		for i, item := range value {
			out[i] = cloneJSON(item)
		}
		return out
	case []any:
		out := make([]any, len(value))
		for i, item := range value {
			out[i] = cloneJSON(item)
		}
		return out
	default:
		return v
	}
}

func rejectKeys(m map[string]any, allowed ...string) error {
	for key := range m {
		found := false
		for _, allow := range allowed {
			if key == allow {
				found = true
				break
			}
		}
		if !found {
			return fmt.Errorf("cannot translate field %q without losing its semantics", key)
		}
	}
	return nil
}

func object(v any) map[string]any {
	m, _ := v.(map[string]any)
	return m
}

func array(v any) []any {
	switch value := v.(type) {
	case []any:
		return value
	case []map[string]any:
		out := make([]any, len(value))
		for i, m := range value {
			out[i] = m
		}
		return out
	default:
		return nil
	}
}

func str(v any) string {
	s, _ := v.(string)
	return s
}

func responsesInput(messages []map[string]any) ([]any, error) {
	var result []any
	for i, message := range messages {
		if err := rejectKeys(message, "role", "content", "tool_calls", "tool_call_id"); err != nil {
			return nil, fmt.Errorf("message %d: %w", i, err)
		}
		role := str(message["role"])
		switch role {
		case "tool":
			if str(message["tool_call_id"]) == "" || message["tool_calls"] != nil {
				return nil, fmt.Errorf("message %d: tool output requires tool_call_id", i)
			}
			content, err := responsesContent(message["content"], "user")
			if err != nil {
				return nil, fmt.Errorf("message %d: %w", i, err)
			}
			result = append(result, map[string]any{"type": "function_call_output", "call_id": message["tool_call_id"], "output": content})
		case "system", "developer", "user", "assistant":
			if message["tool_call_id"] != nil || (message["tool_calls"] != nil && role != "assistant") {
				return nil, fmt.Errorf("message %d: tool fields have an incompatible role", i)
			}
			if message["content"] != nil {
				content, err := responsesContent(message["content"], role)
				if err != nil {
					return nil, fmt.Errorf("message %d: %w", i, err)
				}
				result = append(result, map[string]any{"role": role, "content": content})
			} else if role != "assistant" || len(array(message["tool_calls"])) == 0 {
				return nil, fmt.Errorf("message %d: missing content", i)
			}
			if calls, exists := message["tool_calls"]; exists {
				if array(calls) == nil {
					return nil, fmt.Errorf("message %d: tool_calls must be an array", i)
				}
				for _, raw := range array(calls) {
					call := object(raw)
					fn := object(call["function"])
					if err := rejectKeys(call, "id", "type", "function"); err != nil {
						return nil, err
					}
					if err := rejectKeys(fn, "name", "arguments"); err != nil {
						return nil, err
					}
					_, argumentsOK := fn["arguments"].(string)
					if str(call["type"]) != "function" || str(call["id"]) == "" || str(fn["name"]) == "" || !argumentsOK {
						return nil, fmt.Errorf("message %d: unsupported or invalid tool call", i)
					}
					result = append(result, map[string]any{"type": "function_call", "call_id": call["id"], "name": fn["name"], "arguments": fn["arguments"]})
				}
			}
		default:
			return nil, fmt.Errorf("message %d: unsupported role %q", i, role)
		}
	}
	return result, nil
}

func responsesContent(value any, role string) (any, error) {
	if text, ok := value.(string); ok {
		return text, nil
	}
	parts := array(value)
	if parts == nil {
		return nil, errors.New("content must be a string or an array of content parts")
	}
	out := make([]any, 0, len(parts))
	for _, raw := range parts {
		part := object(raw)
		switch str(part["type"]) {
		case "text":
			if err := rejectKeys(part, "type", "text"); err != nil {
				return nil, err
			}
			if _, ok := part["text"].(string); !ok {
				return nil, errors.New("text content requires a text string")
			}
			typeName := "input_text"
			if role == "assistant" {
				typeName = "output_text"
			}
			out = append(out, map[string]any{"type": typeName, "text": part["text"]})
		case "image_url":
			if role != "user" {
				return nil, errors.New("image content conversion requires user/tool role")
			}
			if err := rejectKeys(part, "type", "image_url"); err != nil {
				return nil, err
			}
			image := object(part["image_url"])
			if err := rejectKeys(image, "url", "detail"); err != nil {
				return nil, err
			}
			if str(image["url"]) == "" {
				return nil, errors.New("image_url requires a url string")
			}
			converted := map[string]any{"type": "input_image", "image_url": image["url"]}
			if detail, ok := image["detail"]; ok {
				converted["detail"] = detail
			}
			out = append(out, converted)
		case "file":
			if role != "user" {
				return nil, errors.New("file content conversion requires user/tool role")
			}
			if err := rejectKeys(part, "type", "file"); err != nil {
				return nil, err
			}
			file := object(part["file"])
			if err := rejectKeys(file, "file_id", "file_data", "filename"); err != nil {
				return nil, err
			}
			if (str(file["file_id"]) == "") == (str(file["file_data"]) == "") {
				return nil, errors.New("file requires exactly one of file_id or file_data")
			}
			converted := cloneJSON(file).(map[string]any)
			converted["type"] = "input_file"
			out = append(out, converted)
		case "refusal":
			if role != "assistant" {
				return nil, errors.New("refusal content requires assistant role")
			}
			if err := rejectKeys(part, "type", "refusal"); err != nil {
				return nil, err
			}
			if _, ok := part["refusal"].(string); !ok {
				return nil, errors.New("refusal requires a string")
			}
			out = append(out, cloneJSON(part))
		default:
			return nil, fmt.Errorf("unsupported content type %q", str(part["type"]))
		}
	}
	return out, nil
}

// The basic Anthropic adapter accepts text, URL/data-URL images and PDF
// documents. OpenAI file IDs and tool history require an explicit native mapping.
func anthropicInput(messages []map[string]any) ([]any, []any, error) {
	var result, system []any
	seenConversation := false
	for i, message := range messages {
		if err := rejectKeys(message, "role", "content"); err != nil {
			return nil, nil, fmt.Errorf("message %d: Anthropic conversion: %w", i, err)
		}
		role := str(message["role"])
		if role != "system" && role != "user" && role != "assistant" {
			return nil, nil, fmt.Errorf("message %d: unsupported Anthropic role %q", i, role)
		}
		parts, err := anthropicContent(message["content"], role)
		if err != nil {
			return nil, nil, fmt.Errorf("message %d: %w", i, err)
		}
		if role == "system" {
			if seenConversation {
				return nil, nil, errors.New("Anthropic cannot preserve a system message inside the conversation")
			}
			system = append(system, parts...)
		} else {
			seenConversation = true
			result = append(result, map[string]any{"role": role, "content": parts})
		}
	}
	if len(result) == 0 {
		return nil, nil, errors.New("Anthropic requires at least one conversation message")
	}
	return result, system, nil
}

func anthropicContent(value any, role string) ([]any, error) {
	if text, ok := value.(string); ok {
		return []any{map[string]any{"type": "text", "text": text}}, nil
	}
	if array(value) == nil {
		return nil, errors.New("content must be text or a content array")
	}
	var out []any
	for _, raw := range array(value) {
		part := object(raw)
		switch str(part["type"]) {
		case "text":
			if err := rejectKeys(part, "type", "text", "cache_control"); err != nil {
				return nil, err
			}
			if _, ok := part["text"].(string); !ok {
				return nil, errors.New("text content requires a string")
			}
			out = append(out, cloneJSON(part))
		case "image_url":
			if role != "user" {
				return nil, errors.New("Anthropic image conversion requires user role")
			}
			if err := rejectKeys(part, "type", "image_url"); err != nil {
				return nil, err
			}
			image := object(part["image_url"])
			if err := rejectKeys(image, "url"); err != nil {
				return nil, err
			}
			source, err := anthropicSource(str(image["url"]), true)
			if err != nil {
				return nil, err
			}
			out = append(out, map[string]any{"type": "image", "source": source})
		case "file":
			if role != "user" {
				return nil, errors.New("Anthropic file conversion requires user role")
			}
			if err := rejectKeys(part, "type", "file"); err != nil {
				return nil, err
			}
			file := object(part["file"])
			if err := rejectKeys(file, "file_data", "filename"); err != nil {
				return nil, err
			}
			source, err := anthropicSource(str(file["file_data"]), false)
			if err != nil {
				return nil, err
			}
			document := map[string]any{"type": "document", "source": source}
			if title := str(file["filename"]); title != "" {
				document["title"] = title
			}
			out = append(out, document)
		default:
			return nil, fmt.Errorf("unsupported Anthropic content type %q", str(part["type"]))
		}
	}
	return out, nil
}

func anthropicSource(value string, image bool) (map[string]any, error) {
	if strings.HasPrefix(value, "data:") {
		header, data, ok := strings.Cut(strings.TrimPrefix(value, "data:"), ",")
		mediaType, base64 := strings.CutSuffix(header, ";base64")
		validMedia := mediaType == "application/pdf"
		if image {
			validMedia = mediaType == "image/png" || mediaType == "image/jpeg" || mediaType == "image/gif" || mediaType == "image/webp"
		}
		if !ok || !base64 || data == "" || !validMedia {
			return nil, errors.New("unsupported Anthropic data URL; requires a base64 image or PDF")
		}
		return map[string]any{"type": "base64", "media_type": mediaType, "data": data}, nil
	}
	if strings.HasPrefix(value, "https://") || strings.HasPrefix(value, "http://") {
		return map[string]any{"type": "url", "url": value}, nil
	}
	return nil, errors.New("Anthropic content requires an HTTP URL or supported base64 data URL")
}
