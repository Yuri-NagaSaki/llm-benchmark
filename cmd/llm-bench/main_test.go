package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Yuri-NagaSaki/llm-benchmark/internal/bench"
)

func testEndpoint(t *testing.T) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Error(err)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		if strings.HasSuffix(r.URL.Path, "/responses") {
			if body["input"] == nil || body["messages"] != nil {
				t.Error("Responses payload not translated")
			}
			fmt.Fprint(w, "data: {\"type\":\"response.output_text.delta\",\"delta\":\"好\"}\n\ndata: {\"type\":\"response.completed\",\"response\":{\"model\":\"fixture\",\"usage\":{\"input_tokens\":8,\"output_tokens\":1,\"output_tokens_details\":{\"reasoning_tokens\":0}}}}\n\n")
		} else {
			if body["messages"] == nil {
				t.Error("Chat messages missing")
			}
			fmt.Fprint(w, "data: {\"model\":\"fixture\",\"choices\":[{\"delta\":{\"content\":\"好\"},\"finish_reason\":\"stop\"}],\"usage\":{\"prompt_tokens\":8,\"completion_tokens\":1,\"completion_tokens_details\":{\"reasoning_tokens\":0}}}\n\ndata: [DONE]\n\n")
		}
	}))
}

func TestCLICompareArchivesBothProtocolsAndReplays(t *testing.T) {
	s := testEndpoint(t)
	defer s.Close()
	path := filepath.Join(t.TempDir(), "nested", "report.json")
	args := []string{"--base-url", s.URL, "--model", "chat-id", "--anonymous", "--compare-base-url", s.URL, "--compare-model", "responses-id", "--compare-protocol", "responses", "--n", "1", "--json", path}
	var out, errOut bytes.Buffer
	if code := execute(args, &out, &errOut); code != 0 {
		t.Fatalf("exit %d: %s", code, errOut.String())
	}
	r, err := bench.ReadReport(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(r.Samples) != 2 || r.Samples[0].Protocol == r.Samples[1].Protocol {
		t.Fatalf("missing comparison: %+v", r.Samples)
	}
	for _, sample := range r.Samples {
		if sample.Metrics["text_decode_tps"] != nil {
			t.Error("single chunk fabricated decode TPS")
		}
		if sample.Metrics["output_tps_e2e"] == nil {
			t.Error("end to end rate missing")
		}
	}
	var replay bytes.Buffer
	if code := execute([]string{"report", path}, &replay, &errOut); code != 0 {
		t.Fatal(errOut.String())
	}
	if out.String() != replay.String()+"JSON: "+path+"\n" {
		t.Error("terminal and archived report differ")
	}
}

func TestCLIJSONStdoutContainsOnlyArchive(t *testing.T) {
	s := testEndpoint(t)
	defer s.Close()
	var out, errOut bytes.Buffer
	if code := execute([]string{"--base-url", s.URL, "--model", "fixture", "--anonymous", "--n", "1", "--json", "-"}, &out, &errOut); code != 0 {
		t.Fatal(code, errOut.String())
	}
	var r bench.Report
	if err := json.Unmarshal(out.Bytes(), &r); err != nil {
		t.Fatal(err)
	}
	if r.SchemaVersion != "1.0" || len(r.Samples) != 1 {
		t.Fatal("invalid stdout report")
	}
}

func TestCLIValidateDoesNotNeedCredentialAndRejectsAmbiguity(t *testing.T) {
	config := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(config, []byte(`{"channels":[{"name":"test","base_url":"https://example.test/v1","model":"test","protocol":"chat","api_key_env":"LLM_BENCH_TEST_UNSET_KEY"}]}`), 0600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("LLM_BENCH_TEST_UNSET_KEY", "")
	var out, errOut bytes.Buffer
	if code := execute([]string{"--config", config, "--validate"}, &out, &errOut); code != 0 {
		t.Fatal(code, errOut.String())
	}
	if code := execute([]string{"--config", config, "--base-url", "https://different.test", "--validate"}, &out, &errOut); code != 2 {
		t.Fatal("ambiguous override accepted")
	}
	if code := execute([]string{"--config", config, "--prompt", "x", "--messages", "[]", "--validate"}, &out, &errOut); code != 2 {
		t.Fatal("ambiguous workload accepted")
	}
}

func TestCLIFailureIsArchived(t *testing.T) {
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Request-ID", "fixture-429")
		http.Error(w, "quota exceeded", 429)
	}))
	defer s.Close()
	p := filepath.Join(t.TempDir(), "error.json")
	var out, errOut bytes.Buffer
	if code := execute([]string{"--base-url", s.URL, "--model", "fixture", "--anonymous", "--n", "1", "--json", p}, &out, &errOut); code != 1 {
		t.Fatalf("want exit 1, got %d: %s", code, errOut.String())
	}
	r, err := bench.ReadReport(p)
	if err != nil {
		t.Fatal(err)
	}
	if r.Samples[0].HTTPStatus != 429 || r.Samples[0].RequestID != "fixture-429" || r.Samples[0].Error == nil {
		t.Fatal("failed sample lost")
	}
}

func TestCLIArgSafety(t *testing.T) {
	for _, args := range [][]string{{"--api-key", "not-a-real-key"}, {"--timeout", "NaN", "--validate"}, {"unknown-command"}, {"report"}} {
		var out, errOut bytes.Buffer
		if execute(args, &out, &errOut) != 2 {
			t.Fatalf("accepted invalid args %v", args)
		}
	}
	var out, errOut bytes.Buffer
	if execute([]string{"--base-url", "http://localhost:1", "--model", "fixture", "--anonymous", "--compare-model", "ignored", "--validate"}, &out, &errOut) != 2 {
		t.Fatal("silently ignored comparison")
	}
	for _, args := range [][]string{{"--help"}, {"--version"}, {"version"}} {
		var out, errOut bytes.Buffer
		if execute(args, &out, &errOut) != 0 {
			t.Fatalf("help/version failed %v", args)
		}
	}
}

func TestCLICustomBodyHeadersAndMessagesReachWire(t *testing.T) {
	t.Setenv("LLM_BENCH_HEADER_FIXTURE", "synthetic-header-value")
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Error(err)
		}
		if body["enable_thinking"] != false || len(body["messages"].([]any)) != 2 || r.Header.Get("X-Gateway-Token") != "synthetic-header-value" {
			t.Errorf("custom request not delivered: %+v", body)
		}
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{\"content\":\"ok\"},\"finish_reason\":\"stop\"}]}\n\ndata: [DONE]\n\n")
	}))
	defer srv.Close()
	file := filepath.Join(t.TempDir(), "messages.json")
	if err := os.WriteFile(file, []byte(`[{"role":"system","content":"be concise"},{"role":"user","content":"hello"}]`), 0600); err != nil {
		t.Fatal(err)
	}
	var out, errOut bytes.Buffer
	code := execute([]string{"--base-url", srv.URL, "--model", "fixture", "--anonymous", "--n", "1", "--messages", "@" + file, "--extra-body", `{"enable_thinking":false}`, "--custom-headers", `{"X-Gateway-Token":{"env":"LLM_BENCH_HEADER_FIXTURE"}}`, "--json", "-"}, &out, &errOut)
	if code != 0 {
		t.Fatal(code, errOut.String())
	}
	if strings.Contains(out.String(), "synthetic-header-value") {
		t.Fatal("header credential leaked into report")
	}
}
