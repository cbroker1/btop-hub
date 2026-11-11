// Package collect reads machine metrics. Each platform provides New(); the simulator
// provides NewSim(). Collectors must not start a process per sample or query WMI while
// sampling: everything is read from kernel interfaces or long-lived library handles.
package collect

import (
	"fmt"
	"regexp"
	"strings"
	"sync"
	"time"

	"btophub/internal/proto"
)

// Collector produces one sample per call to Collect. It is used from one goroutine.
type Collector interface {
	// Info returns the machine description. Called at start and every few seconds;
	// it may change (default-route interface, disk size).
	Info() proto.Info
	// Collect takes a sample. Rates are computed against the previous call.
	// The first call primes counters, so its rate fields may be nil.
	Collect(now time.Time) proto.Sample
	// Report says where each metric came from, for --probe.
	Report() Report
	Close()
}

// Report explains a collector's sources, for --probe output.
type Report struct {
	Sources map[string]string  `json:"sources"`    // metric -> source used, or "none: why"
	Timings map[string]float64 `json:"timings_ms"` // source -> milliseconds of the last Collect
	Notes   []string           `json:"notes,omitempty"`
}

// Timer accumulates per-source timings for a Report. Not safe for concurrent use
// unless the mutex is used, which Measure does.
type Timer struct {
	mu     sync.Mutex
	m      map[string]float64
	panics map[string]string
}

// Measure runs f and records how long it took under name. A panic inside f (say, a Windows
// export missing on an older build) is contained: that source yields nothing this time and the
// panic is reported by Panics, instead of taking the agent down.
func (t *Timer) Measure(name string, f func()) {
	start := time.Now()
	func() {
		defer func() {
			if r := recover(); r != nil {
				t.mu.Lock()
				if t.panics == nil {
					t.panics = map[string]string{}
				}
				t.panics[name] = fmt.Sprint(r)
				t.mu.Unlock()
			}
		}()
		f()
	}()
	d := float64(time.Since(start).Microseconds()) / 1000
	t.mu.Lock()
	if t.m == nil {
		t.m = map[string]float64{}
	}
	t.m[name] = d
	t.mu.Unlock()
}

// Panics lists sources that panicked, for --probe notes.
func (t *Timer) Panics() []string {
	t.mu.Lock()
	defer t.mu.Unlock()
	var out []string
	for k, v := range t.panics {
		out = append(out, "panic in "+k+": "+v)
	}
	return out
}

// Snapshot returns a copy of the recorded timings.
func (t *Timer) Snapshot() map[string]float64 {
	t.mu.Lock()
	defer t.mu.Unlock()
	out := make(map[string]float64, len(t.m))
	for k, v := range t.m {
		out[k] = v
	}
	return out
}

var (
	reCPUNoise = regexp.MustCompile(`(?i)\((r|tm)\)|\bcpu\b|\bprocessor\b|\b\d+-core\b|\bwith radeon.*$|\bw/ radeon.*$|@.*$`)
	reSpaces   = regexp.MustCompile(`\s+`)
)

// CleanCPUName shortens a CPU brand string: "AMD Ryzen 9 7950X 16-Core Processor" -> "Ryzen 9 7950X".
func CleanCPUName(s string) string {
	s = reCPUNoise.ReplaceAllString(s, " ")
	s = strings.TrimSpace(reSpaces.ReplaceAllString(s, " "))
	for _, p := range []string{"AMD ", "Intel ", "Genuine "} {
		s = strings.TrimPrefix(s, p)
	}
	// Some AMD brand strings are upper case ("RYZEN AI MAX+ 395"); write the words as AMD
	// does elsewhere ("Ryzen AI Max+ 395").
	s = strings.NewReplacer("RYZEN", "Ryzen", "MAX+", "Max+", "THREADRIPPER", "Threadripper", "ATHLON", "Athlon").Replace(s)
	return strings.TrimSpace(s)
}

var reGPUNoise = regexp.MustCompile(`(?i)\((r|tm)\)`)

// CleanGPUName shortens a GPU name: "NVIDIA GeForce RTX 4090" -> "RTX 4090",
// "AMD Radeon(TM) 8060S Graphics" -> "Radeon 8060S".
func CleanGPUName(s string) string {
	s = reGPUNoise.ReplaceAllString(s, "")
	s = strings.TrimSpace(reSpaces.ReplaceAllString(s, " "))
	for _, p := range []string{"NVIDIA ", "GeForce ", "AMD ", "Intel(R) ", "Intel "} {
		s = strings.TrimPrefix(s, p)
	}
	s = strings.TrimSuffix(s, " Graphics")
	return strings.TrimSpace(s)
}

// Rate turns two counter readings into a per-second rate, or nil if the
// counter went backwards (reset) or no time passed.
func Rate(prev, cur uint64, dt time.Duration) *int64 {
	if cur < prev || dt <= 0 {
		return nil
	}
	v := int64(float64(cur-prev) / dt.Seconds())
	return &v
}
