package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"strings"

	"github.com/Yuri-NagaSaki/llm-benchmark/internal/bench"
)

func main() { os.Exit(execute(os.Args[1:], os.Stdout, os.Stderr)) }

func execute(args []string, out, stderr io.Writer) int {
	if len(args) > 0 && (args[0] == "--version" || args[0] == "version") {
		fmt.Fprintln(out, "llm-bench "+bench.Version)
		return 0
	}
	if len(args) > 0 && args[0] == "report" {
		return reportCommand(args[1:], out, stderr)
	}
	if len(args) > 0 && args[0] == "run" {
		args = args[1:]
	}
	f := flag.NewFlagSet("llm-bench", flag.ContinueOnError)
	f.SetOutput(stderr)
	configPath := f.String("config", "", "channel matrix JSON file")
	jsonPath := f.String("json", "results/report.json", "archive path; '-' writes JSON to stdout")
	base := f.String("base-url", "", "primary base URL including API version")
	model := f.String("model", "", "primary model ID")
	protocol := f.String("protocol", "chat", "chat, responses, or anthropic")
	keyEnv := f.String("api-key-env", "", "environment variable containing the primary key")
	profile := f.String("profile", "generic", "generic, deepseek, tencent, or aliyun")
	anonymous := f.Bool("anonymous", false, "no API authentication (local servers)")
	compareBase := f.String("compare-base-url", "", "secondary base URL")
	compareModel := f.String("compare-model", "", "secondary model ID")
	compareProtocol := f.String("compare-protocol", "chat", "secondary protocol")
	compareKey := f.String("compare-api-key-env", "", "secondary key environment variable")
	compareProfile := f.String("compare-profile", "generic", "secondary profile")
	extra := f.String("extra-body", "", "primary root JSON object or @file")
	compareExtra := f.String("compare-extra-body", "", "secondary root JSON object or @file")
	headers := f.String("custom-headers", "", "primary header specifications JSON or @file")
	prompt := f.String("prompt", "", "user text")
	messages := f.String("messages", "", "message array JSON or @file (multi-turn/image/file)")
	rounds := f.Int("n", 5, "measured rounds")
	warmup := f.Int("warmup", 0, "separately archived warmup rounds with isolated prefixes")
	maxTokens := f.Int("max-tokens", 256, "maximum output tokens")
	thinking := f.String("thinking", "default", "default, on, off")
	effort := f.String("effort", "", "thinking effort with --thinking on")
	schedule := f.String("schedule", "parallel", "parallel or serial")
	connection := f.String("connection", "reuse", "reuse or fresh")
	mode := f.String("mode", "standard", "standard or cache")
	concurrency := f.Int("concurrency", 8, "maximum simultaneous channel requests")
	timeout := f.Float64("timeout", 120, "whole request deadline in seconds, including streaming")
	connectTimeout := f.Float64("connect-timeout", 10, "connection and TLS timeout in seconds")
	validate := f.Bool("validate", false, "validate configuration and payloads without requests or keys")
	f.Usage = func() {
		fmt.Fprintln(stderr, "Usage: llm-bench run --config matrix.json --json results/report.json\n       llm-bench --base-url URL --model ID --api-key-env ENV [options]\n       llm-bench report results/report.json\n\nKeys are read from named environment variables. Values are never CLI arguments.")
		f.PrintDefaults()
	}
	if err := f.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}
	if f.NArg() != 0 {
		fmt.Fprintln(stderr, "unexpected positional arguments; use --help")
		return 2
	}
	if *compareBase == "" {
		orphan := false
		f.Visit(func(v *flag.Flag) {
			if strings.HasPrefix(v.Name, "compare-") {
				orphan = true
			}
		})
		if orphan {
			fmt.Fprintln(stderr, "comparison options require --compare-base-url")
			return 2
		}
	}
	c := bench.DefaultConfig()
	var err error
	if *configPath != "" {
		file, e := os.Open(*configPath)
		if e != nil {
			fmt.Fprintln(stderr, e)
			return 2
		}
		c, err = bench.LoadConfig(file)
		file.Close()
		if err != nil {
			fmt.Fprintln(stderr, err)
			return 2
		}
		for _, name := range []string{"base-url", "model", "protocol", "api-key-env", "profile", "anonymous", "compare-base-url", "compare-model", "compare-protocol", "compare-api-key-env", "compare-profile", "extra-body", "compare-extra-body", "custom-headers"} {
			conflict := false
			f.Visit(func(v *flag.Flag) {
				if v.Name == name {
					conflict = true
				}
			})
			if conflict {
				fmt.Fprintln(stderr, "channel flags cannot be combined with --config; edit the channel in config")
				return 2
			}
		}
	} else {
		c.Channels = []bench.Channel{{Name: "primary", BaseURL: *base, Model: *model, Protocol: *protocol, APIKeyEnv: *keyEnv, Anonymous: *anonymous, Profile: *profile}}
		if *compareBase != "" {
			c.Channels = append(c.Channels, bench.Channel{Name: "secondary", BaseURL: *compareBase, Model: *compareModel, Protocol: *compareProtocol, APIKeyEnv: *compareKey, Anonymous: *anonymous, Profile: *compareProfile})
		}
		if *extra != "" {
			err = parseJSONArg(*extra, &c.Channels[0].ExtraBody)
		}
		if err == nil && *headers != "" {
			err = parseJSONArg(*headers, &c.Channels[0].Headers)
		}
		if err == nil && *compareExtra != "" {
			if len(c.Channels) < 2 {
				err = errors.New("compare-extra-body requires compare-base-url")
			} else {
				err = parseJSONArg(*compareExtra, &c.Channels[1].ExtraBody)
			}
		}
		if err != nil {
			fmt.Fprintln(stderr, "invalid custom JSON:", err)
			return 2
		}
	}
	// Only explicitly supplied flags override a file's values.
	visited := map[string]bool{}
	f.Visit(func(v *flag.Flag) { visited[v.Name] = true })
	if visited["n"] {
		c.Run.Rounds = *rounds
	}
	if visited["warmup"] {
		c.Run.Warmup = *warmup
	}
	if visited["schedule"] {
		c.Run.Schedule = *schedule
	}
	if visited["connection"] {
		c.Run.Connection = *connection
	}
	if visited["mode"] {
		c.Run.Mode = *mode
	}
	if visited["concurrency"] {
		c.Run.Concurrency = *concurrency
	}
	if visited["timeout"] {
		c.Run.TimeoutSeconds = *timeout
	}
	if visited["connect-timeout"] {
		c.Run.ConnectTimeoutSeconds = *connectTimeout
	}
	if visited["max-tokens"] {
		c.Workload.MaxOutputTokens = *maxTokens
	}
	if visited["thinking"] {
		c.Thinking.Mode = *thinking
	}
	if visited["effort"] {
		c.Thinking.Effort = *effort
	}
	if visited["prompt"] && visited["messages"] {
		fmt.Fprintln(stderr, "choose either --prompt or --messages")
		return 2
	}
	if visited["prompt"] {
		c.Workload.Messages = []map[string]any{{"role": "user", "content": *prompt}}
	}
	if visited["messages"] {
		if err := parseJSONArg(*messages, &c.Workload.Messages); err != nil {
			fmt.Fprintln(stderr, err)
			return 2
		}
	}
	if err := c.Validate(); err != nil {
		fmt.Fprintln(stderr, err)
		return 2
	}
	if *validate {
		fmt.Fprintf(out, "Valid configuration: %d channels, %d rounds, %s mode. No requests sent.\n", len(c.Channels), c.Run.Rounds, c.Run.Mode)
		return 0
	}
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt)
	defer cancel()
	r, err := bench.Run(ctx, c)
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 2
	}
	redactor := bench.NewRedactor(c)
	data, err := redactor.JSON(r)
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 2
	}
	var safeReport bench.Report
	if err := json.Unmarshal(data, &safeReport); err != nil {
		fmt.Fprintln(stderr, err)
		return 2
	}
	if *jsonPath == "-" {
		fmt.Fprintln(out, string(data))
	} else {
		if err := os.MkdirAll(filepath.Dir(*jsonPath), 0700); err != nil {
			fmt.Fprintln(stderr, err)
			return 2
		}
		if err := bench.WriteReport(*jsonPath, safeReport, redactor); err != nil {
			fmt.Fprintln(stderr, err)
			return 2
		}
		if err := bench.RenderReport(out, safeReport); err != nil {
			fmt.Fprintln(stderr, err)
			return 2
		}
		fmt.Fprintln(out, "JSON:", *jsonPath)
	}
	if ctx.Err() != nil {
		return 130
	}
	for _, s := range r.Samples {
		if s.Phase != "warmup" && s.Outcome != "completed" {
			return 1
		}
	}
	return 0
}

func parseJSONArg(s string, target any) error {
	if strings.HasPrefix(s, "@") {
		b, err := os.ReadFile(strings.TrimPrefix(s, "@"))
		if err != nil {
			return err
		}
		s = string(b)
	}
	d := json.NewDecoder(strings.NewReader(s))
	d.DisallowUnknownFields()
	if err := d.Decode(target); err != nil {
		return err
	}
	if err := d.Decode(new(any)); err != io.EOF {
		return errors.New("expected exactly one JSON value")
	}
	return nil
}

func reportCommand(args []string, out, stderr io.Writer) int {
	if len(args) != 1 {
		fmt.Fprintln(stderr, "Usage: llm-bench report report.json")
		return 2
	}
	r, err := bench.ReadReport(args[0])
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 2
	}
	if err := bench.RenderReport(out, r); err != nil {
		fmt.Fprintln(stderr, err)
		return 2
	}
	return 0
}
