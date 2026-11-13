//go:build linux

package collect

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"btophub/internal/proto"
)

func put(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

// fakeAMD lays out drm/<card>/device like amdgpu does: sysfs files (values get a newline, as in
// sysfs), a driver symlink, and, when format > 0, a gpu_metrics table header with that format_revision.
func fakeAMD(t *testing.T, drm, card string, format byte, files map[string]string) string {
	t.Helper()
	dev := filepath.Join(drm, card, "device")
	for name, v := range files {
		put(t, filepath.Join(dev, name), v+"\n")
	}
	if format > 0 {
		put(t, filepath.Join(dev, "gpu_metrics"), string([]byte{8, 1, format, 0, 0xAA, 0xBB}))
	}
	if err := os.Symlink("../../../bus/pci/drivers/amdgpu", filepath.Join(dev, "driver")); err != nil {
		t.Fatal(err)
	}
	return dev
}

// fixtures points the name lookups at test files for the duration of a test.
func fixtures(t *testing.T, ids, cpuinfo string) {
	t.Helper()
	dir := t.TempDir()
	oldIDs, oldCPU := amdgpuIDsPath, cpuinfoPath
	amdgpuIDsPath, cpuinfoPath = filepath.Join(dir, "amdgpu.ids"), filepath.Join(dir, "cpuinfo")
	t.Cleanup(func() { amdgpuIDsPath, cpuinfoPath = oldIDs, oldCPU })
	put(t, amdgpuIDsPath, ids)
	put(t, cpuinfoPath, cpuinfo)
}

const z13CPU = "processor\t: 0\nmodel name\t: AMD RYZEN AI MAX+ 395 w/ Radeon 8060S\n"
const idsFile = "# List of AMDGPU IDs\n#\n1586,\tC1,\tRadeon 8060S Graphics\n7480,\tC0,\tAMD Radeon RX 7600\n"

// The Z13 (Radeon 8060S) on Linux at its 512 MiB BIOS carve-out while Halogen holds 37.6 GiB of GTT,
// with the values its sysfs reported on 2026-11-13 (gpu_metrics format 3, no product_name file).
func TestAMDZ13(t *testing.T) {
	fixtures(t, idsFile, z13CPU)
	drm := t.TempDir()
	fakeAMD(t, drm, "card1", 3, map[string]string{
		"device":                      "0x1586",
		"revision":                    "0xc1",
		"mem_info_vram_total":         "536870912",
		"mem_info_vram_used":          "306798592",
		"mem_info_gtt_total":          "67077443584",
		"mem_info_gtt_used":           "40339013632",
		"gpu_busy_percent":            "37",
		"hwmon/hwmon5/temp1_input":    "38000",
		"hwmon/hwmon5/power1_input":   "5642000",
		"hwmon/hwmon5/power1_average": "5760000",
		"hwmon/hwmon5/freq1_input":    "600000000",
	})
	// A connector entry with everything a GPU has: only its name keeps it out.
	fakeAMD(t, drm, "card1-eDP-1", 3, map[string]string{"mem_info_vram_total": "536870912"})

	gpus := findAMDIn(drm)
	if len(gpus) != 1 {
		t.Fatalf("found %d GPUs, want 1 (the connector must be skipped)", len(gpus))
	}
	g := gpus[0]
	if g.info.Name != "Radeon 8060S" {
		t.Errorf("Name = %q, want Radeon 8060S (from amdgpu.ids)", g.info.Name)
	}
	if !g.info.Integrated || !g.info.Shared || !g.gtt {
		t.Errorf("integrated %v, shared %v, gtt %v; want all true", g.info.Integrated, g.info.Shared, g.gtt)
	}
	if want := int64(536870912 + 67077443584); g.info.MemTotal != want {
		t.Errorf("MemTotal = %d, want VRAM + GTT = %d", g.info.MemTotal, want)
	}
	d := g.describe()
	for _, s := range []string{"hwmon5/power1_input", "package", "VRAM+GTT", "hwmon5/freq1_input", "hwmon5/temp1_input"} {
		if !strings.Contains(d, s) {
			t.Errorf("describe() = %q, missing %q", d, s)
		}
	}

	c := &linux{amd: gpus}
	var s proto.Sample
	c.collectGPU(&s)
	if len(s.GPU) != 1 {
		t.Fatalf("got %d GPU samples, want 1", len(s.GPU))
	}
	gs := s.GPU[0]
	if gs.Util == nil || *gs.Util != 370 {
		t.Errorf("Util = %v, want 370 per mille", gs.Util)
	}
	if want := int64(306798592 + 40339013632); gs.MemUsed == nil || *gs.MemUsed != want {
		t.Errorf("MemUsed = %v, want VRAM + GTT used = %d", gs.MemUsed, want)
	} else if *gs.MemUsed > g.info.MemTotal {
		t.Errorf("MemUsed %d exceeds MemTotal %d", *gs.MemUsed, g.info.MemTotal)
	}
	if gs.Temp == nil || *gs.Temp != 38 {
		t.Errorf("Temp = %v, want 38", gs.Temp)
	}
	if gs.Power == nil || *gs.Power < 5.641 || *gs.Power > 5.643 {
		t.Errorf("Power = %v, want 5.642 W (power1_input, not power1_average)", gs.Power)
	}
	if gs.Clock == nil || *gs.Clock != 600 {
		t.Errorf("Clock = %v, want 600 MHz", gs.Clock)
	}
}

// Classification follows gpu_metrics, not size: the same APU with a 96 GiB carve-out still counts
// GTT, a 4 GiB discrete card does not, and the discrete card sorts first.
func TestAMDClassification(t *testing.T) {
	fixtures(t, idsFile, z13CPU)
	drm := t.TempDir()
	fakeAMD(t, drm, "card0", 3, map[string]string{"device": "0x1586", "revision": "0xc1",
		"mem_info_vram_total": "103079215104", "mem_info_gtt_total": "16632184832",
		"mem_info_vram_used": "1073741824", "mem_info_gtt_used": "2147483648"})
	fakeAMD(t, drm, "card1", 1, map[string]string{"device": "0x7480", "revision": "0xc0",
		"mem_info_vram_total": "4294967296", "mem_info_gtt_total": "67077443584",
		"mem_info_vram_used": "1073741824", "mem_info_gtt_used": "2147483648",
		"hwmon/hwmon2/power1_average": "45000000"})
	gpus := findAMDIn(drm)
	if len(gpus) != 2 {
		t.Fatalf("found %d GPUs, want 2", len(gpus))
	}
	dg, apu := gpus[0], gpus[1]
	if dg.info.Integrated || dg.info.Shared || dg.info.MemTotal != 4294967296 || dg.info.Name != "Radeon RX 7600" {
		t.Errorf("first GPU = %+v, want the 4 GiB discrete card, VRAM only, named from amdgpu.ids", dg.info)
	}
	if !apu.info.Integrated || !apu.info.Shared || apu.info.MemTotal != 103079215104+16632184832 {
		t.Errorf("second GPU = %+v, want the 96 GiB-carve-out APU with VRAM + GTT", apu.info)
	}
	if filepath.Base(dg.power) != "power1_average" {
		t.Errorf("power source = %q, want the power1_average fallback", dg.power)
	}
	c := &linux{amd: gpus}
	var s proto.Sample
	c.collectGPU(&s)
	if s.GPU[0].MemUsed == nil || *s.GPU[0].MemUsed != 1073741824 {
		t.Errorf("discrete MemUsed = %v, want VRAM only (1 GiB)", s.GPU[0].MemUsed)
	}
	if s.GPU[0].Power == nil || *s.GPU[0].Power != 45 {
		t.Errorf("discrete Power = %v, want 45 W from power1_average", s.GPU[0].Power)
	}
	if s.GPU[0].Clock != nil || s.GPU[0].Temp != nil {
		t.Errorf("no freq1/temp1 files, yet clock/temp were reported: %+v", s.GPU[0])
	}
}

// A GTT read that fails sends no memory value at all rather than VRAM alone against a VRAM+GTT
// total, and names fall back to the CPU's "w/ Radeon ..." and then to a generic name.
func TestAMDFallbacks(t *testing.T) {
	fixtures(t, "# empty\n", z13CPU)
	drm := t.TempDir()
	fakeAMD(t, drm, "card1", 3, map[string]string{"device": "0x1586", "revision": "0xc1",
		"mem_info_vram_total": "536870912", "mem_info_gtt_total": "67077443584", "mem_info_vram_used": "306798592"})
	gpus := findAMDIn(drm)
	if len(gpus) != 1 || gpus[0].info.Name != "Radeon 8060S" {
		t.Fatalf("got %+v, want one GPU named Radeon 8060S from the CPU name", gpus)
	}
	c := &linux{amd: gpus}
	var s proto.Sample
	c.collectGPU(&s)
	if s.GPU[0].MemUsed != nil {
		t.Errorf("MemUsed = %d with mem_info_gtt_used missing, want nil", *s.GPU[0].MemUsed)
	}

	fixtures(t, "# empty\n", "model name\t: AMD Ryzen 9 7950X 16-Core Processor\n")
	if n := amdName(gpus[0].dir, true); n != "Radeon Graphics" {
		t.Errorf("amdName without amdgpu.ids or a w/ suffix = %q, want Radeon Graphics", n)
	}
	// Without a gpu_metrics header, size decides: a 512 MiB carve-out is an APU.
	drm2 := t.TempDir()
	fakeAMD(t, drm2, "card0", 0, map[string]string{"mem_info_vram_total": "536870912", "mem_info_gtt_total": "1073741824"})
	if g := findAMDIn(drm2); len(g) != 1 || !g[0].info.Integrated || !g[0].info.Shared {
		t.Errorf("no gpu_metrics, 512 MiB VRAM: got %+v, want an integrated GPU with shared memory", g)
	}
}
