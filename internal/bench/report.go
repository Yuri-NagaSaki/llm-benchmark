package bench

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"text/tabwriter"
)

// RenderReport renders the stored summary so a saved report replays identically.
func RenderReport(w io.Writer, report Report) error {
	if err := validateReportEnvelope(report); err != nil {
		return err
	}
	if _, err := fmt.Fprintf(w, "LLM Benchmark %s · 运行 %s\n", displayText(report.Version), displayText(report.RunID)); err != nil {
		return err
	}
	groups, ok := report.Summary["groups"].([]any)
	if !ok {
		return errors.New("report summary groups must be an array")
	}
	table := tabwriter.NewWriter(w, 0, 4, 2, ' ', 0)
	if _, err := fmt.Fprintln(table, "渠道 / 协议\t阶段\t样本 / 完成 / 不完整 / 失败\t正文首响应 p50 ms\t原始推理首响应 p50 ms\t总耗时 p50 ms\t尾部 ms\t正文解码 tok/s\t正文近似 tok/s\t输出全程 tok/s\t成本（已知样本）"); err != nil {
		return err
	}
	for _, item := range groups {
		g, ok := item.(map[string]any)
		if !ok {
			return errors.New("report summary contains an invalid group")
		}
		metrics, ok := g["metrics"].(map[string]any)
		if !ok {
			return errors.New("report summary group is missing metrics")
		}
		tail := stat(metrics, "total_ms", "max")
		tailLabel := "max"
		if v := stat(metrics, "total_ms", "p99"); v != nil {
			tail, tailLabel = v, "p99"
		}
		cost := formatCosts(g["cost_by_currency"])
		if _, err := fmt.Fprintf(table, "%s / %s\t%s\t%s / %s / %s / %s\t%s\t%s\t%s\t%s %s\t%s\t%s\t%s\t%s\n",
			displayText(metricString(g["channel"])), displayText(metricString(g["protocol"])), displayText(metricString(g["phase"])),
			metricString(g["sample_count"]), metricString(g["completed"]), metricString(g["incomplete"]), metricString(g["failed"]),
			formatNumber(stat(metrics, "first_text_ms", "p50")), formatNumber(stat(metrics, "first_reasoning_ms", "p50")),
			formatNumber(stat(metrics, "total_ms", "p50")), formatNumber(tail), tailLabel,
			formatNumber(stat(metrics, "text_decode_tps", "p50")), formatNumber(stat(metrics, "text_decode_tps_estimate", "p50")),
			formatNumber(stat(metrics, "output_tps_e2e", "p50")), cost,
		); err != nil {
			return err
		}
	}
	if err := table.Flush(); err != nil {
		return err
	}
	_, err := fmt.Fprintln(w, "\n延迟单位为毫秒；速度单位为 token/秒。— 表示缺失或不可测，不能解释为零。\n正文解码、正文近似和输出全程速度均显示成功样本的 p50；近似值假设首块只有 1 token。\n预热独立列出；失败与不完整样本不进入分位数。少于 100 个有效延迟样本显示最慢一次 / max，否则显示经验 p99。\n成本按币种分别汇总，包含已知用量的失败请求；缺失成本不记为零。JSON 保留每个指标的有效样本数、p50、p95、max 和可用时的 p99。")
	return err
}

func stat(metrics map[string]any, name, field string) any {
	if m, ok := metrics[name].(map[string]any); ok {
		return m[field]
	}
	return nil
}

func formatNumber(value any) string {
	n, ok := metricNumber(value)
	if !ok {
		return "—"
	}
	return fmt.Sprintf("%.2f", n)
}

func displayText(s string) string {
	return strings.Map(func(r rune) rune {
		if r < 32 || r == 127 || r == '\u2028' || r == '\u2029' {
			return ' '
		}
		return r
	}, s)
}

func formatCosts(value any) string {
	costs, ok := value.(map[string]any)
	if !ok || len(costs) == 0 {
		return "—"
	}
	currencies := make([]string, 0, len(costs))
	for currency := range costs {
		currencies = append(currencies, currency)
	}
	sort.Strings(currencies)
	parts := make([]string, 0, len(currencies))
	for _, currency := range currencies {
		entry, ok := costs[currency].(map[string]any)
		if !ok {
			continue
		}
		n, ok := metricNumber(entry["total"])
		if !ok {
			continue
		}
		parts = append(parts, fmt.Sprintf("%.8f %s (%s)", n, displayText(currency), metricString(entry["known_samples"])))
	}
	if len(parts) == 0 {
		return "—"
	}
	return strings.Join(parts, "; ")
}

// WriteReport writes a private temporary file in the destination directory and
// atomically renames it into place only after encoding and syncing succeeds.
func WriteReport(path string, report Report, redactor Redactor) (err error) {
	if err := validateReportEnvelope(report); err != nil {
		return err
	}
	data, err := redactor.JSON(report)
	if err != nil {
		return fmt.Errorf("encode report: %w", err)
	}
	data = append(data, '\n')
	temp, err := os.CreateTemp(filepath.Dir(path), "."+filepath.Base(path)+"-*.tmp")
	if err != nil {
		return fmt.Errorf("create report: %w", err)
	}
	tempPath := temp.Name()
	defer func() { temp.Close(); os.Remove(tempPath) }()
	if err = temp.Chmod(0600); err != nil {
		return fmt.Errorf("secure report permissions: %w", err)
	}
	if _, err = temp.Write(data); err != nil {
		return fmt.Errorf("write report: %w", err)
	}
	if err = temp.Sync(); err != nil {
		return fmt.Errorf("sync report: %w", err)
	}
	if err = temp.Close(); err != nil {
		return fmt.Errorf("close report: %w", err)
	}
	if err = os.Rename(tempPath, path); err != nil {
		return fmt.Errorf("publish report: %w", err)
	}
	return nil
}

// ReadReport validates the versioned report envelope without recomputing metrics.
func ReadReport(path string) (Report, error) {
	var report Report
	file, err := os.Open(path)
	if err != nil {
		return report, err
	}
	defer file.Close()
	decoder := json.NewDecoder(file)
	if err = decoder.Decode(&report); err != nil {
		return report, fmt.Errorf("decode report: %w", err)
	}
	if err = decoder.Decode(new(any)); err != io.EOF {
		return report, errors.New("report must contain exactly one JSON object")
	}
	if err := validateReportEnvelope(report); err != nil {
		return report, err
	}
	return report, nil
}

func validateReportEnvelope(report Report) error {
	if report.SchemaVersion != SchemaVersion {
		return fmt.Errorf("unsupported report schema %q (supported: %s)", report.SchemaVersion, SchemaVersion)
	}
	if report.Summary == nil {
		return errors.New("report is missing its summary")
	}
	groups, ok := report.Summary["groups"].([]any)
	if !ok {
		return errors.New("report summary groups must be an array")
	}
	for _, item := range groups {
		group, ok := item.(map[string]any)
		if !ok {
			return errors.New("report summary contains an invalid group")
		}
		if _, ok := group["metrics"].(map[string]any); !ok {
			return errors.New("report summary group is missing metrics")
		}
	}
	return nil
}
