package bench

import (
	"bytes"
	"context"
	"crypto/sha256"
	"crypto/tls"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptrace"
	"os"
	"strings"
	"sync"
	"time"
	"unicode/utf8"
)

// Each channel owns one pool. Fresh mode creates a new pool for each sample.
func newClient(run RunConfig) *http.Client {
	t := &http.Transport{
		DialContext:         (&net.Dialer{Timeout: time.Duration(run.ConnectTimeoutSeconds * float64(time.Second)), KeepAlive: 30 * time.Second}).DialContext,
		ForceAttemptHTTP2:   true,
		MaxIdleConns:        32,
		MaxIdleConnsPerHost: 8,
		IdleConnTimeout:     90 * time.Second,
		TLSHandshakeTimeout: time.Duration(run.ConnectTimeoutSeconds * float64(time.Second)),
		TLSClientConfig:     &tls.Config{MinVersion: tls.VersionTLS12},
	}
	// No implicit proxy, redirects or application retries: all change measurement semantics.
	return &http.Client{Transport: t, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
}

type requestTrace struct {
	mu      sync.Mutex
	start   time.Time
	events  []TraceEvent
	timings map[string]float64
	reused  *bool
}

func (t *requestTrace) mark(name, attempt string, success *bool) {
	t.mu.Lock()
	defer t.mu.Unlock()
	offset := float64(time.Since(t.start).Nanoseconds()) / 1e6
	t.events = append(t.events, TraceEvent{Name: name, OffsetMS: offset, Attempt: attempt, Succeeded: success})
	if _, ok := t.timings[name]; !ok {
		t.timings[name] = offset
	}
}

func (t *requestTrace) hooks() *httptrace.ClientTrace {
	return &httptrace.ClientTrace{
		GetConn:      func(string) { t.mark("get_connection", "", nil) },
		DNSStart:     func(httptrace.DNSStartInfo) { t.mark("dns_start", "", nil) },
		DNSDone:      func(i httptrace.DNSDoneInfo) { ok := i.Err == nil; t.mark("dns_done", "", &ok) },
		ConnectStart: func(network, address string) { t.mark("tcp_start", network+":"+address, nil) },
		ConnectDone: func(network, address string, err error) {
			ok := err == nil
			t.mark("tcp_done", network+":"+address, &ok)
		},
		TLSHandshakeStart: func() { t.mark("tls_start", "", nil) },
		TLSHandshakeDone:  func(_ tls.ConnectionState, err error) { ok := err == nil; t.mark("tls_done", "", &ok) },
		GotConn: func(i httptrace.GotConnInfo) {
			t.mu.Lock()
			v := i.Reused
			t.reused = &v
			t.mu.Unlock()
			t.mark("got_connection", i.Conn.RemoteAddr().Network()+":"+i.Conn.RemoteAddr().String(), nil)
		},
		WroteRequest:         func(i httptrace.WroteRequestInfo) { ok := i.Err == nil; t.mark("request_written", "", &ok) },
		GotFirstResponseByte: func() { t.mark("first_response_byte", "", nil) },
	}
}

func mergeUsage(to *Usage, from *Usage) {
	if from == nil {
		return
	}
	if from.InputTokens != nil {
		to.InputTokens = from.InputTokens
	}
	if from.OutputTokens != nil {
		to.OutputTokens = from.OutputTokens
	}
	if from.ReasoningTokens != nil {
		to.ReasoningTokens = from.ReasoningTokens
	}
	if from.TextOutputTokens != nil {
		to.TextOutputTokens = from.TextOutputTokens
	}
	if from.CachedInputTokens != nil {
		to.CachedInputTokens = from.CachedInputTokens
	}
	if from.CacheMissInputTokens != nil {
		to.CacheMissInputTokens = from.CacheMissInputTokens
	}
	if from.CacheWriteTokens != nil {
		to.CacheWriteTokens = from.CacheWriteTokens
	}
}

func Measure(ctx context.Context, client *http.Client, c Config, ch Channel, workload Workload, s Sample) Sample {
	s.Outcome = "failed"
	s.ReasoningCheck = "not_requested"
	s.TimingsMS = map[string]float64{}
	s.Trace = []TraceEvent{}
	s.Events = []Observation{}
	s.RawUsage = []map[string]any{}
	s.Warnings = []string{}
	s.ResponseHeaders = map[string]string{}
	start := time.Now()
	s.StartedAt = start.UTC()
	trace := &requestTrace{start: start, timings: map[string]float64{}, events: []TraceEvent{}}
	redactor := NewRedactor(c)
	thinking := c.ThinkingFor(ch)
	deferFinalize := func() {
		elapsed := float64(time.Since(start).Nanoseconds()) / 1e6
		s.TimingsMS["total"] = elapsed
		if terminalAt, ok := s.TimingsMS["protocol_terminal"]; ok {
			s.TimingsMS["total"] = terminalAt
			s.TimingsMS["connection_cleanup"] = elapsed - terminalAt
		}
		s.EndedAt = start.Add(time.Duration(s.TimingsMS["total"] * 1e6)).UTC()
		trace.mu.Lock()
		s.Trace = append(s.Trace, trace.events...)
		s.ConnectionReused = trace.reused
		for k, v := range trace.timings {
			s.TimingsMS[k] = v
		}
		trace.mu.Unlock()
		if s.Error != nil {
			s.Error.Message = redactor.Text(s.Error.Message)
			s.Error.BodyExcerpt = redactor.Text(s.Error.BodyExcerpt)
		}
		FinalizeSample(&s, ch, thinking)
	}
	body, err := BuildBody(ch, workload, thinking)
	if err != nil {
		s.Error = &RequestError{Kind: "configuration", Message: err.Error()}
		deferFinalize()
		return s
	}
	encoded, err := json.Marshal(body)
	if err != nil {
		s.Error = &RequestError{Kind: "configuration", Message: err.Error()}
		deferFinalize()
		return s
	}
	hash := sha256.Sum256(encoded)
	s.RequestFingerprint = hex.EncodeToString(hash[:])
	ctx, cancel := context.WithTimeout(ctx, time.Duration(c.Run.TimeoutSeconds*float64(time.Second)))
	defer cancel()
	ctx = httptrace.WithClientTrace(ctx, trace.hooks())
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, ch.URL(), bytes.NewReader(encoded))
	if err != nil {
		s.Error = &RequestError{Kind: "configuration", Message: err.Error()}
		deferFinalize()
		return s
	}
	// Prevent net/http from transparently replaying a request on a stale reused socket.
	req.GetBody = nil
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "text/event-stream")
	req.Header.Set("User-Agent", "llm-benchmark/"+Version)
	for k, v := range ch.Headers {
		value := v.Value
		if v.Env != "" {
			value = os.Getenv(v.Env)
		}
		req.Header.Set(k, value)
	}
	if !ch.Anonymous {
		if ch.Protocol == "anthropic" {
			req.Header.Set("x-api-key", os.Getenv(ch.APIKeyEnv))
		} else {
			req.Header.Set("Authorization", "Bearer "+os.Getenv(ch.APIKeyEnv))
		}
	}
	if ch.Protocol == "anthropic" && req.Header.Get("anthropic-version") == "" {
		req.Header.Set("anthropic-version", "2023-06-01")
	}
	resp, err := client.Do(req)
	if err != nil {
		s.Error = &RequestError{Kind: "transport", Message: err.Error()}
		if errors.Is(err, context.DeadlineExceeded) {
			s.Error.Kind = "timeout"
		}
		if errors.Is(err, context.Canceled) {
			s.Error.Kind = "canceled"
		}
		deferFinalize()
		return s
	}
	defer resp.Body.Close()
	s.TimingsMS["response_headers"] = float64(time.Since(start).Nanoseconds()) / 1e6
	s.HTTPStatus = resp.StatusCode
	s.HTTPVersion = resp.Proto
	for _, k := range []string{"x-request-id", "request-id", "x-amzn-requestid", "x-tc-requestid", "retry-after", "x-ratelimit-limit-tokens", "x-ratelimit-remaining-tokens", "x-ratelimit-reset-tokens", "server-timing"} {
		if v := resp.Header.Get(k); v != "" {
			s.ResponseHeaders[k] = redactor.Text(v)
		}
	}
	if ch.TPMHeader != "" {
		s.ResponseHeaders[strings.ToLower(ch.TPMHeader)] = redactor.Text(resp.Header.Get(ch.TPMHeader))
	}
	for _, k := range []string{"x-request-id", "request-id", "x-tc-requestid", "x-amzn-requestid"} {
		if v := resp.Header.Get(k); v != "" {
			s.RequestID = redactor.Text(v)
			break
		}
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		s.Error = &RequestError{Kind: "http", Message: resp.Status, BodyExcerpt: string(b)}
		deferFinalize()
		return s
	}
	if !strings.HasPrefix(strings.ToLower(resp.Header.Get("Content-Type")), "text/event-stream") {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		s.Error = &RequestError{Kind: "protocol", Message: "expected text/event-stream", BodyExcerpt: string(b)}
		deferFinalize()
		return s
	}
	d := NewDecoder(ch.Protocol, ch.UsagePaths)
	terminal := false
	finished := false
	var drainTimer *time.Timer
	defer func() {
		if drainTimer != nil {
			drainTimer.Stop()
		}
	}()
	err = ReadSSE(resp.Body, func(frame SSEFrame) error {
		if terminal {
			return errors.New("protocol data received after terminal event")
		}
		now := float64(time.Since(start).Nanoseconds()) / 1e6
		if _, ok := s.TimingsMS["first_sse"]; !ok {
			s.TimingsMS["first_sse"] = now
		}
		events, e := d.Feed(frame)
		if e != nil {
			return e
		}
		for _, event := range events {
			if len(s.Events) >= 1_000_000 {
				return errors.New("event limit exceeded")
			}
			s.Events = append(s.Events, Observation{Kind: event.Kind, OffsetMS: now, Characters: utf8.RuneCountInString(event.Text), TokenCount: event.TokenCount})
			if event.Text != "" && (event.Kind == "text" || event.Kind == "reasoning" || event.Kind == "reasoning_summary" || event.Kind == "tool") {
				if _, ok := s.TimingsMS["first_"+event.Kind]; !ok {
					s.TimingsMS["first_"+event.Kind] = now
				}
				s.TimingsMS["last_"+event.Kind] = now
			}
			if event.Model != "" {
				s.Model = event.Model
			}
			if event.ResponseID != "" {
				s.ResponseID = event.ResponseID
			}
			mergeUsage(&s.Usage, event.Usage)
			if event.RawUsage != nil {
				s.RawUsage = append(s.RawUsage, event.RawUsage)
			}
			if event.FinishReason != "" {
				s.FinishReason = event.FinishReason
			}
			if event.Outcome != "" {
				s.Outcome = event.Outcome
			}
			if event.Kind == "finish" {
				finished = true
			}
			if event.Kind == "terminal" {
				terminal = true
				s.TimingsMS["protocol_terminal"] = now
				// A valid protocol terminal completes the sample. Allow a short,
				// unmeasured drain for HTTP connection reuse; never wait for the
				// entire model deadline after a gateway leaves the body open.
				drainTimer = time.AfterFunc(25*time.Millisecond, cancel)
			}
			if event.Error != nil {
				s.Error = event.Error
				s.Outcome = "failed"
			}
		}
		return nil
	})
	s.TimingsMS["stream_end"] = float64(time.Since(start).Nanoseconds()) / 1e6
	if terminal {
		s.TimingsMS["stream_end"] = s.TimingsMS["protocol_terminal"]
		if err == nil {
			s.TimingsMS["transport_eof"] = float64(time.Since(start).Nanoseconds()) / 1e6
		}
	}
	if terminal && err != nil && ctx.Err() != nil {
		s.Warnings = append(s.Warnings, "HTTP body did not reach EOF before cleanup deadline; protocol result retained and connection closed")
	} else if err != nil {
		s.Outcome = "failed"
		s.Error = &RequestError{Kind: "stream", Message: err.Error()}
		if errors.Is(ctx.Err(), context.DeadlineExceeded) {
			s.Error.Kind = "timeout"
		}
		if errors.Is(ctx.Err(), context.Canceled) {
			s.Error.Kind = "canceled"
		}
	}
	if !terminal || !finished {
		s.Outcome = "failed"
		if s.Error == nil {
			s.Error = &RequestError{Kind: "truncated_stream", Message: "stream ended without an explicit protocol terminal and finish status"}
		}
	}
	if s.Error != nil {
		s.Outcome = "failed"
	}
	hasOutput := false
	for _, e := range s.Events {
		if e.Kind == "tool" || e.Characters > 0 && (e.Kind == "text" || e.Kind == "reasoning" || e.Kind == "reasoning_summary") {
			hasOutput = true
		}
	}
	if s.Outcome == "completed" && !hasOutput {
		s.Outcome = "failed"
		s.Error = &RequestError{Kind: "empty_output", Message: "completed stream contained no observable text, reasoning, summary or tool output"}
	}
	deferFinalize()
	return s
}
