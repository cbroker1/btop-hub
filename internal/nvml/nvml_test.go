//go:build linux

package nvml

import (
	"syscall"
	"testing"
	"time"
)

// TestLive runs only where an NVIDIA driver is installed.
func TestLive(t *testing.T) {
	l, err := Open()
	if err != nil {
		t.Skip("no NVML:", err)
	}
	defer l.Close()
	n, err := l.Count()
	if err != nil || n == 0 {
		t.Skip("no GPUs:", err)
	}
	t.Logf("driver %s, %d GPU(s)", l.DriverVersion(), n)
	for i := 0; i < n; i++ {
		d, err := l.Handle(i)
		if err != nil {
			t.Fatal(err)
		}
		name, err := l.Name(d)
		if err != nil || name == "" {
			t.Fatalf("name: %q %v", name, err)
		}
		total, used, err := l.Memory(d)
		if err != nil || total == 0 || used > total {
			t.Fatalf("memory: %d %d %v", total, used, err)
		}
		u, m, _ := l.Utilization(d)
		temp, _ := l.Temperature(d)
		p, _ := l.Power(d)
		ps, _ := l.PState(d)
		fan, fanErr := l.Fan(d)
		gc, _ := l.Clock(d, ClockGraphics)
		mc, _ := l.Clock(d, ClockMem)
		t.Logf("gpu%d %q mem %d/%d MiB util %d%%/%d%% temp %dC power %.1fW P%d fan %d%% (%v) clocks %d/%d MHz",
			i, name, used>>20, total>>20, u, m, temp, float64(p)/1000, ps, fan, fanErr, gc, mc)

		// Cost of the PCIe query: wall time and CPU time (rusage) for 10 calls.
		var r0, r1 syscall.Rusage
		syscall.Getrusage(syscall.RUSAGE_SELF, &r0)
		start := time.Now()
		for k := 0; k < 10; k++ {
			l.PcieThroughput(d, PcieTx)
		}
		wall := time.Since(start)
		syscall.Getrusage(syscall.RUSAGE_SELF, &r1)
		cpu := time.Duration(r1.Utime.Nano()+r1.Stime.Nano()-r0.Utime.Nano()-r0.Stime.Nano()) * time.Nanosecond
		tx, err := l.PcieThroughput(d, PcieTx)
		t.Logf("pcie tx %d KB/s err %v; 10 calls: wall %v cpu %v", tx, err, wall, cpu)

		syscall.Getrusage(syscall.RUSAGE_SELF, &r0)
		start = time.Now()
		for k := 0; k < 100; k++ {
			l.Utilization(d)
			l.Memory(d)
			l.Temperature(d)
			l.Power(d)
			l.PState(d)
			l.Fan(d)
			l.Clock(d, ClockGraphics)
			l.Clock(d, ClockMem)
		}
		wall = time.Since(start)
		syscall.Getrusage(syscall.RUSAGE_SELF, &r1)
		cpu = time.Duration(r1.Utime.Nano()+r1.Stime.Nano()-r0.Utime.Nano()-r0.Stime.Nano()) * time.Nanosecond
		t.Logf("100 full reads: wall %v cpu %v (per read %v)", wall, cpu, cpu/100)
	}
}
