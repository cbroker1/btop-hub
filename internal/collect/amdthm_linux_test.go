package collect

import (
	"encoding/binary"
	"os"
	"path/filepath"
	"testing"

	"btophub/internal/proto"
)

// z13Metrics is a gpu_metrics v3.0 table shaped like the Z13's (264 bytes, format 3, content 0),
// with the throttle counters as its sysfs read on 2026-11-14 except where a test changes them.
func z13Metrics(core, gfx, soc uint32) []byte {
	b := make([]byte, 264)
	b[0], b[1], b[2], b[3] = 8, 1, 3, 0
	binary.LittleEndian.PutUint16(b[222:], 5150) // current_core_maxfreq
	binary.LittleEndian.PutUint16(b[224:], 2900) // current_gfx_maxfreq
	for i, v := range []uint32{36508, 902357, 2615935, 11713971, core, gfx, soc} {
		binary.LittleEndian.PutUint32(b[amdV3ThmOff+4*i:], v)
	}
	return b
}

func TestAMDV3Thm(t *testing.T) {
	got, ok := amdV3Thm(z13Metrics(3287, 94, 0))
	if !ok || got != (amdThm{core: 3287, gfx: 94, soc: 0}) {
		t.Errorf("amdV3Thm = %+v, %v; want {3287 94 0}", got, ok)
	}
	short := z13Metrics(1, 2, 3)[:200]
	if _, ok := amdV3Thm(short); ok {
		t.Error("a table too short for the counters was read")
	}
	v2 := z13Metrics(1, 2, 3)
	v2[2] = 2 // format 2: another layout
	if _, ok := amdV3Thm(v2); ok {
		t.Error("a gpu_metrics v2 table was read as v3.0")
	}
}

// The Z13 end to end: the GPU and CPU are flagged only in a second when their own thermal counter
// grew, never on the first sample, and not for the power-limit counters.
func TestAMDThmSamples(t *testing.T) {
	dev := filepath.Join(t.TempDir(), "card1", "device")
	if err := os.MkdirAll(dev, 0o755); err != nil {
		t.Fatal(err)
	}
	write := func(core, gfx, soc uint32) {
		if err := os.WriteFile(filepath.Join(dev, "gpu_metrics"), z13Metrics(core, gfx, soc), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write(3287, 94, 0)
	c := &linux{sources: map[string]string{}, tempPath: "/k10temp", cpuLimit: 100,
		amd: []amdGPU{{dir: dev, info: proto.GPUInfo{Integrated: true}}}}
	c.findAPULimits()
	if !c.amd[0].thm || !c.cpuFromAPU || c.amd[0].info.Throttle != amdThmGPUDesc || c.cpuThrottle != amdThmCPUDesc {
		t.Fatalf("findAPULimits: thm=%v cpuFromAPU=%v gpu=%q cpu=%q", c.amd[0].thm, c.cpuFromAPU, c.amd[0].info.Throttle, c.cpuThrottle)
	}
	for _, step := range []struct {
		name           string
		core, gfx, soc uint32
		gpu, cpu       bool
	}{
		{"first sample: nothing to compare", 3287, 94, 0, false, false},
		{"idle: counters hold", 3287, 94, 0, false, false},
		{"CPU held back by heat", 3300, 94, 0, false, true},
		{"GPU held back by heat", 3300, 120, 0, true, false},
		{"SoC held back by heat", 3300, 120, 5, true, false},
		{"counters reset: not throttling", 0, 0, 0, false, false},
	} {
		write(step.core, step.gfx, step.soc)
		var s proto.Sample
		c.collectGPU(&s)
		if s.GPU[0].Hot != step.gpu || s.CPUHot != step.cpu {
			t.Errorf("%s: GPU hot %v, CPU hot %v; want %v, %v", step.name, s.GPU[0].Hot, s.CPUHot, step.gpu, step.cpu)
		}
	}
}
