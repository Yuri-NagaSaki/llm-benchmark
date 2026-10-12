package bench

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"runtime"
	"sync"
	"time"
)

func runID() string {
	b := make([]byte, 12)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	return hex.EncodeToString(b)
}

// Isolate the beginning of the complete input, including system messages.
func prefixedWorkload(w Workload, marker string) Workload {
	b, _ := json.Marshal(w)
	var copy Workload
	_ = json.Unmarshal(b, &copy)
	first := copy.Messages[0]
	switch content := first["content"].(type) {
	case string:
		first["content"] = marker + "\n" + content
	case []any:
		first["content"] = append([]any{map[string]any{"type": "text", "text": marker}}, content...)
	default:
		copy.Messages = append([]map[string]any{{"role": "system", "content": marker}}, copy.Messages...)
	}
	return copy
}

// Run has no retries. Warmup is separately archived; every scheduled request is a sample.
func Run(ctx context.Context, c Config) (Report, error) {
	if err := c.Validate(); err != nil {
		return Report{}, err
	}
	if err := c.CheckCredentials(); err != nil {
		return Report{}, err
	}
	r := Report{SchemaVersion: SchemaVersion, Version: Version, RunID: runID(), StartedAt: time.Now().UTC(), Config: c, Samples: []Sample{}, Environment: map[string]string{"go": runtime.Version(), "os": runtime.GOOS, "arch": runtime.GOARCH, "proxy": "disabled", "automatic_retries": "disabled", "clock": "monotonic offsets in ms; UTC wall timestamps"}}
	r.Environment["effective_schedule"] = c.Run.Schedule
	if c.Run.Mode == "cache" {
		r.Environment["effective_schedule"] = "serial_cache_sequence"
	}
	clients := make([]*http.Client, len(c.Channels))
	for i := range clients {
		clients[i] = newClient(c.Run)
		defer clients[i].CloseIdleConnections()
	}
	measure := func(i int, w Workload, phase string, round int, started time.Time, group, previous string) Sample {
		client := clients[i]
		if c.Run.Connection == "fresh" {
			client = newClient(c.Run)
			defer client.CloseIdleConnections()
		}
		return Measure(ctx, client, c, c.Channels[i], w, Sample{ID: fmt.Sprintf("%s-%d", r.RunID, len(r.Samples)+i), Channel: c.Channels[i].Name, Protocol: c.Channels[i].Protocol, Phase: phase, Round: round, RoundStartedAt: started, ConnectionPolicy: c.Run.Connection, CacheGroup: group, CachePredecessor: previous})
	}
	roundAll := func(w Workload, phase string, round int) {
		started := time.Now().UTC()
		rows := make([]Sample, len(c.Channels))
		if c.Run.Schedule == "parallel" {
			sem := make(chan struct{}, c.Run.Concurrency)
			var wg sync.WaitGroup
			for i := range c.Channels {
				wg.Add(1)
				go func(i int) {
					defer wg.Done()
					sem <- struct{}{}
					defer func() { <-sem }()
					rows[i] = measure(i, w, phase, round, started, "", "")
				}(i)
			}
			wg.Wait()
		} else {
			// Rotate channel order between rounds to reduce a fixed ordering bias.
			for j := range c.Channels {
				i := (j + round) % len(c.Channels)
				rows[i] = measure(i, w, phase, round, started, "", "")
			}
		}
		r.Samples = append(r.Samples, rows...)
	}
	for n := 0; n < c.Run.Warmup && ctx.Err() == nil; n++ {
		roundAll(prefixedWorkload(c.Workload, "[connection-warmup:"+r.RunID+fmt.Sprint(n)+"]"), "warmup", n)
	}
	for n := 0; n < c.Run.Rounds && ctx.Err() == nil; n++ {
		if c.Run.Mode == "cache" {
			// Deliberately sequential. Each channel is an independent seed, then every
			// other configured channel probes the exact same semantic input prefix.
			for seed := range c.Channels {
				if ctx.Err() != nil {
					break
				}
				group := fmt.Sprintf("%s-%d-%d", r.RunID, n, seed)
				w := prefixedWorkload(c.Workload, "[cache-experiment:"+group+"]")
				started := time.Now().UTC()
				cold := measure(seed, w, "cache_cold_candidate", n, started, group, "")
				cold.ID = runID()
				r.Samples = append(r.Samples, cold)
				hot := measure(seed, w, "cache_hot", n, started, group, cold.ID)
				hot.ID = runID()
				r.Samples = append(r.Samples, hot)
				previous := hot.ID
				for j := range c.Channels {
					if j == seed {
						continue
					}
					probe := measure(j, w, "cache_probe", n, started, group, previous)
					probe.ID = runID()
					r.Samples = append(r.Samples, probe)
					previous = probe.ID
				}
			}
		} else {
			roundAll(c.Workload, "measure", n)
		}
		if c.Run.IntervalMS > 0 && n+1 < c.Run.Rounds {
			select {
			case <-ctx.Done():
			case <-time.After(time.Duration(c.Run.IntervalMS) * time.Millisecond):
			}
		}
	}
	r.EndedAt = time.Now().UTC()
	r.Summary = Summarize(r.Samples)
	return r, nil
}
