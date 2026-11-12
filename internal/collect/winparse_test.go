package collect

import (
	"encoding/binary"
	"math"
	"reflect"
	"strings"
	"testing"
	"unicode/utf16"

	"btophub/internal/proto"
)

func u16(s string) []uint16 { return utf16.Encode([]rune(s)) }

func TestWinParseGPUInstance(t *testing.T) {
	cases := []struct {
		name string
		ok   bool
		want winGPUInstance
	}{
		{"pid_1234_luid_0x00000000_0x0000D2B6_phys_0_eng_3_engtype_3D", true,
			winGPUInstance{PID: 1234, LUID: winLUID{0, 0xD2B6}, Phys: 0, Eng: 3}},
		{"luid_0x00000000_0x0000D2B6_phys_0", true,
			winGPUInstance{PID: -1, LUID: winLUID{0, 0xD2B6}, Phys: 0, Eng: -1}},
		// High part first, lower-case hex, other phys, engtype with spaces.
		{"pid_4_luid_0x0000ab12_0xffffffff_phys_1_eng_12_engtype_Video Codec 0", true,
			winGPUInstance{PID: 4, LUID: winLUID{0xAB12, 0xFFFFFFFF}, Phys: 1, Eng: 12}},
		// GPU Local Adapter Memory style suffix is ignored.
		{"luid_0x00000001_0x00012345_phys_0_part_0", true,
			winGPUInstance{PID: -1, LUID: winLUID{1, 0x12345}, Phys: 0, Eng: -1}},
		{"PID_7_LUID_0X00000000_0X0000D2B6_PHYS_2_ENG_0", true,
			winGPUInstance{PID: 7, LUID: winLUID{0, 0xD2B6}, Phys: 2, Eng: 0}},
		{"", false, winGPUInstance{}},
		{"_Total", false, winGPUInstance{}},
		{"pid__luid_0x0_0x1_phys_0", false, winGPUInstance{}},
		{"pid_12luid_0x0_0x1_phys_0", false, winGPUInstance{}},
		{"luid_0x_0x1_phys_0", false, winGPUInstance{}},
		{"luid_0x000000000_0x1_phys_0", false, winGPUInstance{}}, // 9 hex digits
		{"luid_0x0_0x1_phys_", false, winGPUInstance{}},
		{"luid_0x0_0x1_phys_0_eng_", false, winGPUInstance{}},
		{"luid_0x0_0xG_phys_0", false, winGPUInstance{}},
		{"luid_0x0_0x1_phys_99999999999", false, winGPUInstance{}}, // > uint32
	}
	for _, c := range cases {
		got, ok := winParseGPUInstance(u16(c.name))
		if ok != c.ok {
			t.Errorf("%q: ok = %v, want %v", c.name, ok, c.ok)
			continue
		}
		if ok && got != c.want {
			t.Errorf("%q: got %+v, want %+v", c.name, got, c.want)
		}
	}
}

func TestWinEngineAggregation(t *testing.T) {
	a := winLUID{0, 0xD2B6}
	b := winLUID{0, 0xE000}
	samples := []struct {
		name string
		v    float64
	}{
		// Adapter a, engine 0 (3D): two processes add up to 70.
		{"pid_100_luid_0x00000000_0x0000D2B6_phys_0_eng_0_engtype_3D", 40},
		{"pid_200_luid_0x00000000_0x0000D2B6_phys_0_eng_0_engtype_3D", 30},
		// Adapter a, engine 3 (copy): 50, less than engine 0's sum.
		{"pid_100_luid_0x00000000_0x0000D2B6_phys_0_eng_3_engtype_Copy", 50},
		// Adapter b: one engine overshoots 100 with NOCAP100 and must clamp.
		{"pid_300_luid_0x00000000_0x0000E000_phys_0_eng_1_engtype_Compute_0", 80},
		{"pid_301_luid_0x00000000_0x0000E000_phys_0_eng_1_engtype_Compute_0", 35},
		// Ignored: no engine, NaN, negative.
		{"luid_0x00000000_0x0000D2B6_phys_0", 99},
		{"pid_5_luid_0x00000000_0x0000D2B6_phys_0_eng_5_engtype_3D", math.NaN()},
		{"pid_5_luid_0x00000000_0x0000D2B6_phys_0_eng_6_engtype_3D", -5},
	}
	acc := winEngineAcc{}
	for _, s := range samples {
		in, ok := winParseGPUInstance(u16(s.name))
		if !ok {
			t.Fatalf("parse %q", s.name)
		}
		acc.add(in, s.v)
	}
	if u, ok := acc.util(a); !ok || u != 70 {
		t.Errorf("adapter a: %v %v, want 70", u, ok)
	}
	if u, ok := acc.util(b); !ok || u != 100 {
		t.Errorf("adapter b: %v %v, want 100 (clamped)", u, ok)
	}
	if _, ok := acc.util(winLUID{1, 1}); ok {
		t.Errorf("unknown adapter reported")
	}
	// Same engine number on a second physical adapter of the same LUID is a different engine.
	acc2 := winEngineAcc{}
	i1, _ := winParseGPUInstance(u16("pid_1_luid_0x00000000_0x00000010_phys_0_eng_0"))
	i2, _ := winParseGPUInstance(u16("pid_1_luid_0x00000000_0x00000010_phys_1_eng_0"))
	acc2.add(i1, 60)
	acc2.add(i2, 60)
	if u, _ := acc2.util(winLUID{0, 0x10}); u != 60 {
		t.Errorf("per-phys engines: %v, want 60", u)
	}
	if got := proto.PerMille(70.0 / 100); got != 700 {
		t.Errorf("per mille %d", got)
	}

	mem := winMemAcc{}
	m0, _ := winParseGPUInstance(u16("luid_0x00000000_0x0000D2B6_phys_0"))
	m1, _ := winParseGPUInstance(u16("luid_0x00000000_0x0000D2B6_phys_1"))
	mem.add(m0, 1<<30)
	mem.add(m1, 1<<29)
	if mem.add(m0, math.Inf(1)) {
		t.Errorf("Inf accepted")
	}
	if mem[a] != 1<<30+1<<29 {
		t.Errorf("memory sum %v", mem[a])
	}
}

func TestWinPickAdapters(t *testing.T) {
	radeonIGP := winAdapter{Desc: "AMD Radeon(TM) Graphics", VendorID: 0x1002, Dedicated: 512 << 20, LUID: winLUID{0, 1}}
	r4090 := winAdapter{Desc: "NVIDIA GeForce RTX 4090", VendorID: 0x10DE, Dedicated: 24 << 30, LUID: winLUID{0, 2}}
	r3070 := winAdapter{Desc: "NVIDIA GeForce RTX 3070", VendorID: 0x10DE, Dedicated: 8 << 30, LUID: winLUID{0, 3}}
	basic := winAdapter{Desc: "Microsoft Basic Render Driver", VendorID: 0x1414, Flags: 2, LUID: winLUID{0, 4}}
	z13 := winAdapter{Desc: "AMD Radeon(TM) 8060S Graphics", VendorID: 0x1002, Dedicated: 96 << 30, LUID: winLUID{0, 5}}

	// Gaming rig with NVML: nothing from DXGI is shown, the iGPU is noted as hidden.
	shown, notes := winPickAdapters([]winAdapter{radeonIGP, r3070, r4090, basic}, 2)
	if len(shown) != 0 {
		t.Errorf("NVML case shows %v", shown)
	}
	joined := strings.Join(notes, "|")
	if !strings.Contains(joined, "hidden: Radeon (integrated, a discrete GPU is present)") ||
		!strings.Contains(joined, "skipped: Microsoft Basic Render Driver (software adapter)") ||
		strings.Contains(joined, "RTX") {
		t.Errorf("NVML case notes %q", notes)
	}

	// Gaming rig without NVML: NVIDIA via DXGI, primary (most VRAM) first, iGPU hidden.
	shown, notes = winPickAdapters([]winAdapter{radeonIGP, r3070, r4090, basic}, 0)
	if len(shown) != 2 || shown[0].LUID != r4090.LUID || shown[1].LUID != r3070.LUID {
		t.Errorf("DXGI NVIDIA case shows %+v", shown)
	}
	if !strings.Contains(strings.Join(notes, "|"), "hidden: Radeon") {
		t.Errorf("DXGI NVIDIA case notes %q", notes)
	}

	// Z13: the single Radeon, duplicate LUID dropped.
	shown, _ = winPickAdapters([]winAdapter{basic, z13, z13}, 0)
	if len(shown) != 1 || shown[0].Desc != z13.Desc || winGPUVendor(shown[0].VendorID) != "amd" {
		t.Errorf("Z13 case shows %+v", shown)
	}
	if got := CleanGPUName(shown[0].Desc); got != "Radeon 8060S" {
		t.Errorf("name %q", got)
	}
}

func TestWinNetClassify(t *testing.T) {
	cases := []struct {
		ifType      uint32
		hw          bool
		alias, desc string
		usable      bool
		kind        string
	}{
		{6, true, "Ethernet", "Marvell AQtion 10Gbit Network Adapter", true, "wired"},
		{71, true, "Wi-Fi", "MediaTek Wi-Fi 7 MT7925 Wireless LAN Card", true, "wireless"},
		{243, true, "Cellular", "Generic Mobile Broadband Adapter", true, "wireless"},
		{24, false, "Loopback Pseudo-Interface 1", "Software Loopback Interface 1", false, "loopback"},
		{6, true, "vEthernet (Default Switch)", "Hyper-V Virtual Ethernet Adapter", false, "virtual"},
		{6, false, "vEthernet (WSL (Hyper-V firewall))", "Hyper-V Virtual Ethernet Adapter #2", false, "virtual"},
		{53, false, "Tailscale", "Tailscale Tunnel", false, "virtual"},
		{53, false, "wg0", "WireGuard Tunnel", false, "virtual"},
		{6, true, "Ethernet 3", "TAP-Windows Adapter V9", false, "virtual"},
		{6, true, "Ethernet 4", "VirtualBox Host-Only Ethernet Adapter", false, "virtual"},
		{6, true, "VMware Network Adapter VMnet8", "VMware Virtual Ethernet Adapter for VMnet8", false, "virtual"},
		{6, true, "ZeroTier One [abc]", "ZeroTier Virtual Port", false, "virtual"},
		{6, true, "Bluetooth Network Connection", "Bluetooth Device (Personal Area Network)", false, "virtual"},
		{6, false, "Ethernet 5", "Some Adapter", false, "virtual"}, // not a hardware interface
		{131, true, "Teredo", "Teredo Tunneling Pseudo-Interface", false, "virtual"},
		{1, true, "Other", "Unknown Hardware", true, ""},
	}
	for _, c := range cases {
		if got := winNetUsable(c.ifType, c.hw, c.alias, c.desc); got != c.usable {
			t.Errorf("%s / %s: usable %v, want %v", c.alias, c.desc, got, c.usable)
		}
		if got := winNetKind(c.ifType, c.hw, c.alias, c.desc); got != c.kind {
			t.Errorf("%s / %s: kind %q, want %q", c.alias, c.desc, got, c.kind)
		}
	}
}

func TestWinOSName(t *testing.T) {
	cases := []struct {
		product, display string
		build            uint32
		want             string
	}{
		{"Windows 10 IoT Enterprise LTSC 2024", "24H2", 26100, "Windows 11 IoT Enterprise LTSC 2024 24H2"},
		{"Windows 10 Pro", "25H2", 26200, "Windows 11 Pro 25H2"},
		{"Windows 10 Home", "22H2", 19045, "Windows 10 Home 22H2"},
		{"Windows 10 Pro", "", 22000, "Windows 11 Pro"},
		{"Windows 10 Pro", "", 21999, "Windows 10 Pro"},
		{"Windows Server 2025 Datacenter", "24H2", 26100, "Windows Server 2025 Datacenter 24H2"},
		{"Windows Server 2022 Standard", "21H2", 20348, "Windows Server 2022 Standard 21H2"},
		{"Windows 100 Fake", "", 26100, "Windows 100 Fake"},
		{"  Windows 10  Pro ", " 24H2 ", 26100, "Windows 11 Pro 24H2"},
		{"", "24H2", 26100, ""},
	}
	for _, c := range cases {
		if got := winOSName(c.product, c.display, c.build); got != c.want {
			t.Errorf("winOSName(%q, %q, %d) = %q, want %q", c.product, c.display, c.build, got, c.want)
		}
	}
}

func TestWinBattery(t *testing.T) {
	intp := func(v int) *int { return &v }
	cases := []struct {
		ac, flag, pct uint8
		present       bool
		percent       *int
		state         int
	}{
		{1, 128, 255, false, nil, proto.BatUnknown}, // desktop: no system battery
		{255, 255, 255, false, nil, proto.BatUnknown},
		{1, 9, 63, true, intp(63), proto.BatCharging}, // high + charging
		{0, 1, 79, true, intp(79), proto.BatDischarging},
		{0, 2, 20, true, intp(20), proto.BatDischarging},
		{1, 1, 100, true, intp(100), proto.BatFull},
		{1, 1, 80, true, intp(80), proto.BatFull}, // on AC, charge limit reached
		{1, 0, 255, true, nil, proto.BatFull},     // percent unknown
		{255, 1, 50, true, intp(50), proto.BatUnknown},
		{1, 8, 101, true, nil, proto.BatCharging}, // invalid percent
	}
	for _, c := range cases {
		present, pct, state := winBattery(c.ac, c.flag, c.pct)
		if present != c.present || state != c.state || !reflect.DeepEqual(pct, c.percent) {
			t.Errorf("winBattery(%d,%d,%d) = %v %v %d, want %v %v %d", c.ac, c.flag, c.pct,
				present, pct, state, c.present, c.percent, c.state)
		}
	}
}

func TestWinCPUBusy(t *testing.T) {
	prev := []winCPUTimes{{Idle: 100, Kernel: 200, User: 50}, {Idle: 0, Kernel: 0, User: 0}}
	// cpu0: Δkernel 100 (of which idle 75), Δuser 0 -> busy 25/100.
	// cpu1: Δkernel 600 (idle 0), Δuser 400 -> busy 1000/1000.
	cur := []winCPUTimes{{Idle: 175, Kernel: 300, User: 50}, {Idle: 0, Kernel: 600, User: 400}}
	got := winCPUBusy(prev, cur)
	if !reflect.DeepEqual(got, []uint16{250, 1000}) {
		t.Errorf("busy %v", got)
	}
	if winCPUBusy(prev, prev) != nil {
		t.Errorf("no elapsed time must give nil")
	}
	if winCPUBusy(prev, cur[:1]) != nil {
		t.Errorf("length mismatch must give nil")
	}
	back := []winCPUTimes{{Idle: 50, Kernel: 300, User: 50}, {Idle: 0, Kernel: 600, User: 400}}
	if winCPUBusy(prev, back) != nil {
		t.Errorf("counter going backwards must give nil")
	}
	// Idle slightly above kernel+user (rounding): clamp to 0 busy.
	odd := []winCPUTimes{{Idle: 210, Kernel: 300, User: 50}, {Idle: 0, Kernel: 600, User: 400}}
	if got := winCPUBusy(prev, odd); got == nil || got[0] != 0 {
		t.Errorf("idle overshoot %v", got)
	}
}

func le32(b []byte, off int, v uint32) { binary.LittleEndian.PutUint32(b[off:], v) }

func TestWinCountCores(t *testing.T) {
	// Three records: core (48 bytes), core (48 bytes), something else (16 bytes).
	b := make([]byte, 48+48+16)
	le32(b, 0, 0)
	le32(b, 4, 48)
	le32(b, 48, 0)
	le32(b, 52, 48)
	le32(b, 96, 2)
	le32(b, 100, 16)
	if n, err := winCountCores(b); err != nil || n != 2 {
		t.Errorf("cores %d %v", n, err)
	}
	bad := append([]byte(nil), b...)
	le32(bad, 52, 4) // size below the header
	if _, err := winCountCores(bad); err == nil {
		t.Errorf("bad size accepted")
	}
	if _, err := winCountCores(b[:50]); err == nil {
		t.Errorf("truncated buffer accepted")
	}
	if _, err := winCountCores(nil); err == nil {
		t.Errorf("empty buffer accepted")
	}
}

func TestWinSumPageFiles(t *testing.T) {
	if total, used, n, err := winSumPageFiles(nil); err != nil || total != 0 || used != 0 || n != 0 {
		t.Errorf("no page file: %d %d %d %v", total, used, n, err)
	}
	// Two entries; the first one's name follows it, so the second starts at 64.
	b := make([]byte, 64+32+40)
	le32(b, 0, 64)
	le32(b, 4, 1000)
	le32(b, 8, 10)
	le32(b, 64, 0)
	le32(b, 68, 500)
	le32(b, 72, 5)
	total, used, n, err := winSumPageFiles(b)
	if err != nil || total != 1500 || used != 15 || n != 2 {
		t.Errorf("two page files: %d %d %d %v", total, used, n, err)
	}
	over := append([]byte(nil), b...)
	le32(over, 0, 120) // second entry would overrun
	if _, _, _, err := winSumPageFiles(over); err == nil {
		t.Errorf("overrun accepted")
	}
	loop := append([]byte(nil), b...)
	le32(loop, 0, 8) // smaller than an entry
	if _, _, _, err := winSumPageFiles(loop); err == nil {
		t.Errorf("tiny NextEntryOffset accepted")
	}
	if _, _, _, err := winSumPageFiles(b[:20]); err == nil {
		t.Errorf("short buffer accepted")
	}
}

func TestWinStorageDescriptor(t *testing.T) {
	b := make([]byte, 120)
	le32(b, 0, 40)
	le32(b, 4, 120)
	le32(b, 12, 0) // no vendor
	le32(b, 16, 40)
	le32(b, 28, 0x11)
	copy(b[40:], "Samsung SSD 990 PRO 4TB                 \x00")
	v, p, bus, err := winParseStorageDescriptor(b)
	if err != nil || v != "" || p != "Samsung SSD 990 PRO 4TB" || winBusName(bus) != "NVMe" {
		t.Errorf("nvme: %q %q %d %v", v, p, bus, err)
	}
	if m := winDiskModel(v, p); m != "Samsung SSD 990 PRO 4TB" {
		t.Errorf("model %q", m)
	}
	// SATA with a vendor id, string running to the end of the buffer without NUL.
	b2 := make([]byte, 60)
	le32(b2, 12, 40)
	le32(b2, 16, 46)
	le32(b2, 28, 0x0B)
	copy(b2[40:], "WDC  \x00Blue SA510 2.5")
	v, p, bus, _ = winParseStorageDescriptor(b2)
	if v != "WDC" || p != "Blue SA510 2.5" || winBusName(bus) != "SATA" || winDiskModel(v, p) != "WDC Blue SA510 2.5" {
		t.Errorf("sata: %q %q %d", v, p, bus)
	}
	// Offsets pointing into the header or past the end are absent.
	b3 := make([]byte, 40)
	le32(b3, 12, 8)
	le32(b3, 16, 400)
	if v, p, _, _ := winParseStorageDescriptor(b3); v != "" || p != "" {
		t.Errorf("bad offsets gave %q %q", v, p)
	}
	if _, _, _, err := winParseStorageDescriptor(b3[:20]); err == nil {
		t.Errorf("short descriptor accepted")
	}
	for in, want := range map[[2]string]string{
		{"ATA", "Samsung SSD 870 EVO 1TB"}: "Samsung SSD 870 EVO 1TB",
		{"NVMe", "WD_BLACK SN850X 2000GB"}: "WD_BLACK SN850X 2000GB",
		{"Samsung", "Samsung SSD 980"}:     "Samsung SSD 980",
		{"Kingston", ""}:                   "Kingston",
		{"", ""}:                           "",
	} {
		if got := winDiskModel(in[0], in[1]); got != want {
			t.Errorf("winDiskModel%q = %q, want %q", in, got, want)
		}
	}
	for code, want := range map[uint32]string{0x11: "NVMe", 0x0B: "SATA", 0x07: "USB", 0x03: "ATA", 0x0A: "SAS", 0x08: "RAID", 0x0E: "Virtual", 0x7F: ""} {
		if got := winBusName(code); got != want {
			t.Errorf("bus 0x%X = %q, want %q", code, got, want)
		}
	}
}

func TestWinDriveLetter(t *testing.T) {
	for in, want := range map[string]string{"C:": "C:", "c:": "C:", `D:\Windows`: "D:", "C:/x": "C:", "": "", "C": "", "1:": "", `\\?\C:`: "", "CC:": "", "C:x": ""} {
		if got := winDriveLetter(in); got != want {
			t.Errorf("winDriveLetter(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestWinTrunc(t *testing.T) {
	if got := winTrunc("  abc  ", 10); got != "abc" {
		t.Errorf("%q", got)
	}
	s := strings.Repeat("é", 100) // 200 bytes
	got := winTrunc(s, 127)
	if len(got) > 127 || !strings.HasPrefix(s, got) || len(got) != 126 {
		t.Errorf("rune boundary: %d bytes", len(got))
	}
}

func TestWinPdhStatus(t *testing.T) {
	if s := winPdhStatus(0xC0000BB8); s != "PDH_CSTATUS_NO_OBJECT (0xC0000BB8)" {
		t.Errorf("%q", s)
	}
	if s := winPdhStatus(5); s != "Win32 error 5" {
		t.Errorf("%q", s)
	}
	if s := winPdhStatus(0xC0001234); s != "PDH status 0xC0001234" {
		t.Errorf("%q", s)
	}
	if !winPdhNotReady(winPdhInvalidData) || !winPdhNotReady(winPdhCStatusInvalid) || winPdhNotReady(0xC0000BB8) {
		t.Errorf("winPdhNotReady")
	}
}

func TestWinASUSTemp(t *testing.T) {
	cases := []struct {
		v    uint32
		want float64
		ok   bool
	}{
		{0x0001002F, 47, true}, // Z13 at idle
		{0x00010025, 37, true},
		{0x00010095, 149, true},
		{0x00010096, 0, false}, // 150 °C: not a plausible reading
		{0x00010000, 0, false}, // present but reads 0 (no discrete GPU)
		{0xFFFFFFFE, 0, false}, // unsupported device
		{0x00000000, 0, false}, // older firmware: unsupported
		{0x0000002F, 0, false}, // value without the presence bit
		{0x0003002F, 0, false}, // other status bits set: not a plain temperature
	}
	for _, c := range cases {
		got, ok := winASUSTemp(c.v)
		if got != c.want || ok != c.ok {
			t.Errorf("winASUSTemp(0x%08X) = %v, %v; want %v, %v", c.v, got, ok, c.want, c.ok)
		}
	}
}

func TestWinZenTctl(t *testing.T) {
	reg := func(eighths uint32, flags uint32) uint32 { return eighths<<21 | flags }
	cases := []struct {
		v    uint32
		want float64
		ok   bool
	}{
		{reg(45*8+1, 0), 45.125, true},    // plain Tctl
		{reg(94*8+4, 0x3FF), 94.5, true},  // low bits are not part of the value
		{reg((49+40)*8, 1<<19), 40, true}, // range select: -49 °C offset
		{reg((49+40)*8, 3<<16), 40, true}, // both Tj-select bits: same offset
		{reg((49+40)*8, 1<<16), 89, true}, // one Tj-select bit alone: no offset
		{reg(30*8, 1<<19), 0, false},      // offset below zero: not a reading
		{reg(150*8, 0), 0, false},         // 150 °C: implausible
		{0, 0, false},                     // failed read
		{0xFFFFFFFF, 0, false},            // failed read
	}
	for _, c := range cases {
		got, ok := winZenTctl(c.v)
		if got != c.want || ok != c.ok {
			t.Errorf("winZenTctl(0x%08X) = %v, %v; want %v, %v", c.v, got, ok, c.want, c.ok)
		}
	}
}

func TestWinAMDZen(t *testing.T) {
	cases := []struct {
		vendor, id string
		want       bool
	}{
		{"AuthenticAMD", "AMD64 Family 26 Model 68 Stepping 0", true},  // Ryzen 9 9950X3D (1Ah)
		{"AuthenticAMD", "AMD64 Family 26 Model 112 Stepping 0", true}, // Ryzen AI Max+ 395
		{"AuthenticAMD", "AMD64 Family 23 Model 113 Stepping 0", true}, // Zen 2 (17h)
		{"AuthenticAMD", "AMD64 Family 25 Model 97 Stepping 2", true},  // Zen 4 (19h)
		{"AuthenticAMD", "AMD64 Family 22 Model 48 Stepping 1", false}, // 16h, before Zen
		{"AuthenticAMD", "AMD64 Family 27 Model 0 Stepping 0", false},  // a family the module does not know
		{"GenuineIntel", "Intel64 Family 6 Model 183 Stepping 1", false},
		{"AuthenticAMD", "AMD64 Family x Model 1", false},
		{"AuthenticAMD", "", false},
	}
	for _, c := range cases {
		if got := winAMDZen(c.vendor, c.id); got != c.want {
			t.Errorf("winAMDZen(%q, %q) = %v, want %v", c.vendor, c.id, got, c.want)
		}
	}
}

func TestWinPawnIOCall(t *testing.T) {
	b, err := winPawnIOCall("ioctl_read_smn", winZenTctlSMN)
	if err != nil {
		t.Fatal(err)
	}
	if len(b) != 40 || string(b[:14]) != "ioctl_read_smn" || b[14] != 0 || b[31] != 0 ||
		binary.LittleEndian.Uint64(b[32:]) != 0x00059800 {
		t.Errorf("winPawnIOCall = % X", b)
	}
	for _, bad := range []string{"", strings.Repeat("x", 32), "a\x00b"} {
		if _, err := winPawnIOCall(bad); err == nil {
			t.Errorf("winPawnIOCall(%q) accepted", bad)
		}
	}
}
