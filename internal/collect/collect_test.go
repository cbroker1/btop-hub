package collect

import (
	"testing"
	"time"
)

func TestCleanNames(t *testing.T) {
	for in, want := range map[string]string{
		"AMD Ryzen 9 7950X 16-Core Processor":   "Ryzen 9 7950X",
		"AMD Ryzen 9 9950X3D 16-Core Processor": "Ryzen 9 9950X3D",
		"AMD RYZEN AI MAX+ 395 w/ Radeon 8060S": "Ryzen AI Max+ 395",
		"Intel(R) Core(TM) i9-13900K":           "Core i9-13900K",
	} {
		if got := CleanCPUName(in); got != want {
			t.Errorf("CleanCPUName(%q) = %q, want %q", in, got, want)
		}
	}
	for in, want := range map[string]string{
		"NVIDIA RTX A6000":              "RTX A6000",
		"NVIDIA GeForce RTX 4090":       "RTX 4090",
		"AMD Radeon(TM) 8060S Graphics": "Radeon 8060S",
	} {
		if got := CleanGPUName(in); got != want {
			t.Errorf("CleanGPUName(%q) = %q, want %q", in, got, want)
		}
	}
}

// The simulator is deterministic and never invents fields a real machine lacks.
func TestSim(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	for _, p := range SimProfiles {
		a, err := NewSim(p, DefaultSimOptions(p), now)
		if err != nil {
			t.Fatal(err)
		}
		b, _ := NewSim(p, DefaultSimOptions(p), now)
		sa, sb := a.Collect(now), b.Collect(now)
		if len(sa.CPU) != a.Info().Threads || sa.CPU[3] != sb.CPU[3] {
			t.Errorf("%s: not deterministic or wrong thread count", p)
		}
		if len(sa.GPU) != len(a.Info().GPUs) {
			t.Errorf("%s: gpu count mismatch", p)
		}
		if !a.Info().HasTemp && sa.CPUTemp != nil {
			t.Errorf("%s: reports a CPU temperature without a sensor", p)
		}
	}
	g, _ := NewSim("gaming-rig", DefaultSimOptions("gaming-rig"), now)
	if !g.Before(now.Add(-42*time.Minute), now) || g.Before(now.Add(-40*time.Minute), now) {
		t.Error("gaming rig should have booted 41 minutes ago")
	}
}
