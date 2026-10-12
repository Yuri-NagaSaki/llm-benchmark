package bench

import "time"

const Version = "1.0.0"
const SchemaVersion = "1.0"

// Pointer counts distinguish a measured zero from unavailable information.
type Usage struct {
	InputTokens          *int `json:"input_tokens"`
	OutputTokens         *int `json:"output_tokens"`
	ReasoningTokens      *int `json:"reasoning_tokens"`
	TextOutputTokens     *int `json:"text_output_tokens"`
	CachedInputTokens    *int `json:"cached_input_tokens"`
	CacheMissInputTokens *int `json:"cache_miss_input_tokens"`
	CacheWriteTokens     *int `json:"cache_write_tokens"`
}

type RequestError struct {
	Kind        string `json:"kind"`
	Message     string `json:"message"`
	BodyExcerpt string `json:"body_excerpt,omitempty"`
}

type StreamEvent struct {
	Kind         string
	Text         string
	TokenCount   *int
	Usage        *Usage
	RawUsage     map[string]any
	Outcome      string
	FinishReason string
	Model        string
	ResponseID   string
	Error        *RequestError
}

type Observation struct {
	Kind       string  `json:"kind"`
	OffsetMS   float64 `json:"offset_ms"`
	Characters int     `json:"characters,omitempty"`
	TokenCount *int    `json:"token_count,omitempty"`
}

type TraceEvent struct {
	Name      string  `json:"name"`
	OffsetMS  float64 `json:"offset_ms"`
	Attempt   string  `json:"attempt,omitempty"`
	Succeeded *bool   `json:"succeeded,omitempty"`
}

type Sample struct {
	ID                 string             `json:"id"`
	Channel            string             `json:"channel"`
	Protocol           string             `json:"protocol"`
	Phase              string             `json:"phase"`
	Round              int                `json:"round"`
	RoundStartedAt     time.Time          `json:"round_started_at"`
	ConnectionPolicy   string             `json:"connection_policy"`
	StartedAt          time.Time          `json:"started_at"`
	EndedAt            time.Time          `json:"ended_at"`
	Outcome            string             `json:"outcome"`
	FinishReason       string             `json:"finish_reason,omitempty"`
	Model              string             `json:"model,omitempty"`
	RequestID          string             `json:"request_id,omitempty"`
	ResponseID         string             `json:"response_id,omitempty"`
	HTTPStatus         int                `json:"http_status,omitempty"`
	HTTPVersion        string             `json:"http_version,omitempty"`
	ConnectionReused   *bool              `json:"connection_reused"`
	ReasoningCheck     string             `json:"reasoning_check"`
	TimingsMS          map[string]float64 `json:"timings_ms"`
	Trace              []TraceEvent       `json:"trace"`
	Events             []Observation      `json:"events"`
	Usage              Usage              `json:"usage"`
	RawUsage           []map[string]any   `json:"raw_usage"`
	ResponseHeaders    map[string]string  `json:"response_headers"`
	Error              *RequestError      `json:"error,omitempty"`
	Warnings           []string           `json:"warnings"`
	Metrics            map[string]any     `json:"metrics"`
	RequestFingerprint string             `json:"request_fingerprint"`
	CacheGroup         string             `json:"cache_group,omitempty"`
	CachePredecessor   string             `json:"cache_predecessor,omitempty"`
}

type Report struct {
	SchemaVersion string            `json:"schema_version"`
	Version       string            `json:"version"`
	RunID         string            `json:"run_id"`
	StartedAt     time.Time         `json:"started_at"`
	EndedAt       time.Time         `json:"ended_at"`
	Config        Config            `json:"config"`
	Environment   map[string]string `json:"environment"`
	Samples       []Sample          `json:"samples"`
	Summary       map[string]any    `json:"summary"`
}
