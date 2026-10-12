package bench

import (
	"bytes"
	"errors"
	"math"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func reportFixture() Report {
	s := metricFixture()
	FinalizeSample(&s, Channel{Pricing: &Price{Currency: "USD", InputPerMillion: 1, OutputPerMillion: 2, Source: "fixture", AsOf: "2026-10-12"}}, ThinkingConfig{Mode: "default"})
	return Report{SchemaVersion: SchemaVersion, Version: Version, RunID: "test", StartedAt: time.Date(2026, 10, 12, 0, 0, 0, 0, time.UTC), Samples: []Sample{s}, Summary: Summarize([]Sample{s})}
}

func TestReportRoundtripRendersStoredSummary(t *testing.T) {
	report := reportFixture()
	var original bytes.Buffer
	if err := RenderReport(&original, report); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "report.json")
	if err := WriteReport(path, report, Redactor{}); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0600 {
		t.Fatalf("mode=%o", info.Mode().Perm())
	}
	restored, err := ReadReport(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(restored.Samples) != 1 || restored.Samples[0].Usage.ReasoningTokens == nil || *restored.Samples[0].Usage.ReasoningTokens != 0 || restored.Samples[0].Metrics["text_decode_tps"] != nil {
		t.Fatal("roundtrip lost null/zero/count semantics")
	}
	var replay bytes.Buffer
	if err := RenderReport(&replay, restored); err != nil {
		t.Fatal(err)
	}
	if original.String() != replay.String() {
		t.Fatalf("replay changed:\noriginal=%s\nreplay=%s", original.String(), replay.String())
	}
	if !strings.Contains(replay.String(), "USD") || !strings.Contains(replay.String(), "max") || !strings.Contains(replay.String(), "近似") || strings.Contains(replay.String(), "↑") || strings.Contains(replay.String(), "↓") {
		t.Fatalf("unclear terminal output: %s", replay.String())
	}
	// Rendering is a replay of the stored summary, never a fresh sample aggregation.
	restored.Samples = nil
	var detached bytes.Buffer
	if err := RenderReport(&detached, restored); err != nil {
		t.Fatal(err)
	}
	if detached.String() != replay.String() {
		t.Fatal("render recomputed instead of using stored summary")
	}
}

func TestReportWriteIsAtomicAndRedacted(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "report.json")
	old := []byte("existing report")
	if err := os.WriteFile(path, old, 0644); err != nil {
		t.Fatal(err)
	}
	report := reportFixture()
	report.Environment = map[string]string{"message": "secret-fixture", "Authorization": "Bearer example"}
	if err := WriteReport(path, report, Redactor{values: []string{"secret-fixture"}}); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), "secret-fixture") || strings.Contains(string(data), "Bearer example") {
		t.Fatalf("redaction failed: %s", data)
	}
	report.Summary["invalid"] = math.NaN()
	if err := WriteReport(path, report, Redactor{}); err == nil {
		t.Fatal("NaN report must fail encoding")
	}
	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(data, after) {
		t.Fatal("encoding failure replaced prior report")
	}
	files, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(files) != 1 {
		t.Fatalf("temporary file leak: %v", files)
	}
}

func TestReportSchemaAndMalformedInput(t *testing.T) {
	for _, body := range []string{`{"schema_version":"99","summary":{"groups":[]}}`, `{"schema_version":"1.0"}`, `{"schema_version":"1.0","summary":{"groups":{}}}`, `{"schema_version":"1.0","summary":{"groups":[]}} {}`, `{`} {
		path := filepath.Join(t.TempDir(), "invalid.json")
		if err := os.WriteFile(path, []byte(body), 0600); err != nil {
			t.Fatal(err)
		}
		if _, err := ReadReport(path); err == nil {
			t.Fatalf("accepted malformed report %s", body)
		}
	}
	report := reportFixture()
	report.SchemaVersion = "99"
	if err := WriteReport(filepath.Join(t.TempDir(), "x.json"), report, Redactor{}); err == nil {
		t.Fatal("wrote unsupported schema")
	}
	if err := RenderReport(&bytes.Buffer{}, report); err == nil {
		t.Fatal("rendered unsupported schema")
	}
}

func TestReportAtomicRenameFailureCleansTemporaryFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "destination")
	if err := os.Mkdir(path, 0700); err != nil {
		t.Fatal(err)
	}
	if err := WriteReport(path, reportFixture(), Redactor{}); err == nil {
		t.Fatal("rename over a directory must fail")
	}
	files, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(files) != 1 || files[0].Name() != "destination" || !files[0].IsDir() {
		t.Fatalf("destination damaged or temporary file leaked: %v", files)
	}
}

type failingReportWriter struct{}

func (failingReportWriter) Write([]byte) (int, error) { return 0, errors.New("write failed") }
func TestRenderReportPropagatesWriteFailure(t *testing.T) {
	if err := RenderReport(failingReportWriter{}, reportFixture()); err == nil {
		t.Fatal("writer error swallowed")
	}
}
