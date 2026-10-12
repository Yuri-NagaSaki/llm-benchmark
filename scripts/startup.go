//go:build ignore

// A development-only, standard-library startup benchmark. Never calls a model API.
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"math"
	"os"
	"os/exec"
	"runtime"
	"slices"
	"time"
)

func main() {
	current := flag.String("new", "./bin/llm-bench", "new executable")
	baseline := flag.String("baseline", "", "baseline llm-bench executable")
	n := flag.Int("n", 30, "samples per executable")
	flag.Parse()
	if *n < 1 || *baseline == "" {
		fmt.Fprintln(os.Stderr, "require --baseline and positive --n")
		os.Exit(2)
	}
	run := func(path string) float64 {
		c := exec.Command(path, "--help")
		c.Stdout = io.Discard
		c.Stderr = io.Discard
		start := time.Now()
		if err := c.Run(); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		return float64(time.Since(start).Nanoseconds()) / 1e6
	}
	for range 3 {
		run(*baseline)
		run(*current)
	}
	values := map[string][]float64{"baseline": {}, "go": {}}
	for i := range *n {
		if i%2 == 0 {
			values["baseline"] = append(values["baseline"], run(*baseline))
			values["go"] = append(values["go"], run(*current))
		} else {
			values["go"] = append(values["go"], run(*current))
			values["baseline"] = append(values["baseline"], run(*baseline))
		}
	}
	stats := map[string]any{}
	for name, v := range values {
		sorted := slices.Clone(v)
		slices.Sort(sorted)
		median := sorted[len(sorted)/2]
		if len(sorted)%2 == 0 {
			median = (sorted[len(sorted)/2-1] + median) / 2
		}
		stats[name] = map[string]any{"n": len(v), "median_ms": median, "p95_ms": sorted[int(math.Ceil(float64(len(v))*.95))-1], "min_ms": sorted[0], "max_ms": sorted[len(sorted)-1]}
	}
	info, err := os.Stat(*current)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	r := map[string]any{"scenario": "CLI --help; 3 warmups each; alternating order; warm OS cache; stdout/stderr discarded; not API latency", "go_toolchain": runtime.Version(), "os": runtime.GOOS, "arch": runtime.GOARCH, "binary_bytes": info.Size(), "stats": stats, "raw_ms": values}
	encoder := json.NewEncoder(os.Stdout)
	encoder.SetIndent("", "  ")
	if err := encoder.Encode(r); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
