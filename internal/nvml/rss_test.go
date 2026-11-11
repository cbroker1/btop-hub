//go:build linux

package nvml

import (
	"os"
	"strings"
	"testing"
)

func rssAndCuda() (string, bool) {
	b, _ := os.ReadFile("/proc/self/status")
	rss := ""
	for _, l := range strings.Split(string(b), "\n") {
		if strings.HasPrefix(l, "VmRSS") {
			rss = strings.TrimSpace(strings.TrimPrefix(l, "VmRSS:"))
		}
	}
	m, _ := os.ReadFile("/proc/self/maps")
	return rss, strings.Contains(string(m), "libcuda")
}

// TestWhichCallLoadsCUDA reports memory after each kind of NVML call (run with -run CUDA -v).
func TestWhichCallLoadsCUDA(t *testing.T) {
	if os.Getenv("NVML_STEPS") == "" {
		t.Skip("set NVML_STEPS=1")
	}
	step := func(name string) { r, c := rssAndCuda(); t.Logf("%-14s rss %-10s libcuda=%v", name, r, c) }
	step("start")
	l, err := Open()
	if err != nil {
		t.Skip(err)
	}
	step("init")
	d, _ := l.Handle(0)
	l.Name(d)
	step("handle+name")
	l.Utilization(d)
	step("utilization")
	l.Temperature(d)
	l.Power(d)
	l.PState(d)
	l.Fan(d)
	step("temp/power/ps/fan")
	l.Clock(d, ClockGraphics)
	l.Clock(d, ClockMem)
	step("clocks")
	l.Memory(d)
	step("memory")
	l.PcieThroughput(d, PcieTx)
	step("pcie")
}
