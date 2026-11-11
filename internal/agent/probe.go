package agent

import (
	"encoding/json"
	"fmt"
	"io"
	"runtime"
	"time"

	"btophub/internal/collect"
	"btophub/internal/platform"
	"btophub/internal/proto"
)

// Probe prints one sample as JSON together with where every metric came from and how
// long each source took, so a new machine can be debugged from a single paste.
func Probe(c Config, out io.Writer) int {
	start := time.Now()
	col, _, err := newCollector(c, start)
	if err != nil {
		fmt.Fprintln(out, "collector:", err)
		return 1
	}
	defer col.Close()
	setup := time.Since(start)
	col.Collect(time.Now()) // prime counters
	time.Sleep(time.Second)
	t0 := time.Now()
	s := col.Collect(time.Now())
	collectTook := time.Since(t0)
	s.Seq = 0
	cpu, rss, _ := platform.SelfStats()

	token := "missing"
	if c.Token != "" {
		token = "set"
	}
	rep := col.Report()
	res := map[string]any{
		"agent":      proto.Version,
		"platform":   runtime.GOOS + "/" + runtime.GOARCH,
		"time":       time.Now().Format(time.RFC3339),
		"config":     map[string]string{"hub": c.Hub, "id": c.ID, "name": c.Name, "token": token},
		"hub_check":  Check(c.Hub, c.Token),
		"info":       col.Info(),
		"sample":     s,
		"sources":    rep.Sources,
		"timings_ms": rep.Timings,
		"notes":      rep.Notes,
		"self": map[string]any{
			"setup_ms":        setup.Milliseconds(),
			"collect_ms":      float64(collectTook.Microseconds()) / 1000,
			"cpu_ms_total":    cpu.Milliseconds(),
			"rss_mib":         float64(rss) / (1 << 20),
			"child_processes": 0,
		},
	}
	enc := json.NewEncoder(out)
	enc.SetIndent("", "  ")
	enc.SetEscapeHTML(false)
	if err := enc.Encode(res); err != nil {
		return 1
	}
	return 0
}

var _ = collect.SimProfiles
