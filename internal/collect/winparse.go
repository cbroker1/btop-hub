// Portable helpers for the Windows collector. They hold the parsing and decision rules,
// so they compile and are unit-tested on every OS; windows.go and win_*.go only make the
// system calls. Every identifier starts with "win" to stay clear of the other collectors.
package collect

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"math"
	"sort"
	"strings"
	"unicode/utf8"

	"btophub/internal/proto"
)

// winTrunc trims s and cuts it to at most max bytes on a rune boundary, so every Info
// string passes proto validation (Windows names can be 256 UTF-16 units long).
func winTrunc(s string, max int) string {
	s = strings.TrimSpace(s)
	if max < 0 {
		max = 0
	}
	if len(s) <= max {
		return s
	}
	for max > 0 && !utf8.RuneStart(s[max]) {
		max--
	}
	return strings.TrimSpace(s[:max])
}

// ---------------------------------------------------------------------------------------
// GPU performance counter instance names

// winLUID is an adapter LUID. PDH prints it as luid_0x<HighPart>_0x<LowPart>.
type winLUID struct{ High, Low uint32 }

func (l winLUID) String() string { return fmt.Sprintf("0x%08X_0x%08X", l.High, l.Low) }

// winGPUInstance is a parsed instance name of the "GPU Engine" or "GPU Adapter Memory"
// counter sets, e.g. "pid_1234_luid_0x00000000_0x0000D2B6_phys_0_eng_3_engtype_3D" or
// "luid_0x00000000_0x0000D2B6_phys_0".
type winGPUInstance struct {
	PID  int64 // -1 when the name has no pid_ part
	LUID winLUID
	Phys uint32
	Eng  int64 // -1 when the name has no eng_ part
}

// winParseGPUInstance parses an instance name as PDH returns it (UTF-16, no NUL), without
// allocating. Anything after phys_N (or eng_N) such as "_engtype_3D" or "_part_0" is ignored.
func winParseGPUInstance(name []uint16) (winGPUInstance, bool) {
	in := winGPUInstance{PID: -1, Eng: -1}
	p := winU16Scanner{s: name}
	if p.lit("pid_") {
		v, ok := p.dec()
		if !ok || !p.lit("_") {
			return in, false
		}
		in.PID = int64(v)
	}
	if !p.lit("luid_0x") {
		return in, false
	}
	hi, ok := p.hex32()
	if !ok || !p.lit("_0x") {
		return in, false
	}
	lo, ok := p.hex32()
	if !ok || !p.lit("_phys_") {
		return in, false
	}
	ph, ok := p.dec()
	if !ok || ph > math.MaxUint32 {
		return in, false
	}
	in.LUID, in.Phys = winLUID{High: hi, Low: lo}, uint32(ph)
	if p.lit("_eng_") {
		e, ok := p.dec()
		if !ok || e > math.MaxUint32 {
			return in, false
		}
		in.Eng = int64(e)
	}
	return in, true
}

// winU16Scanner reads ASCII tokens from a UTF-16 string.
type winU16Scanner struct {
	s []uint16
	i int
}

func winLowerASCII(c uint16) uint16 {
	if c >= 'A' && c <= 'Z' {
		return c + 'a' - 'A'
	}
	return c
}

// lit consumes lit (ASCII, compared case-insensitively) if it comes next.
func (p *winU16Scanner) lit(lit string) bool {
	if len(p.s)-p.i < len(lit) {
		return false
	}
	for j := 0; j < len(lit); j++ {
		if winLowerASCII(p.s[p.i+j]) != winLowerASCII(uint16(lit[j])) {
			return false
		}
	}
	p.i += len(lit)
	return true
}

// dec consumes 1 to 18 decimal digits.
func (p *winU16Scanner) dec() (uint64, bool) {
	var v uint64
	n := 0
	for p.i < len(p.s) && p.s[p.i] >= '0' && p.s[p.i] <= '9' {
		if n == 18 {
			return 0, false
		}
		v = v*10 + uint64(p.s[p.i]-'0')
		p.i++
		n++
	}
	return v, n > 0
}

// hex32 consumes 1 to 8 hex digits.
func (p *winU16Scanner) hex32() (uint32, bool) {
	var v uint32
	n := 0
	for p.i < len(p.s) {
		c := p.s[p.i]
		var d uint16
		switch {
		case c >= '0' && c <= '9':
			d = c - '0'
		case c >= 'a' && c <= 'f':
			d = c - 'a' + 10
		case c >= 'A' && c <= 'F':
			d = c - 'A' + 10
		default:
			return v, n > 0
		}
		if n == 8 {
			return 0, false
		}
		v = v<<4 | uint32(d)
		p.i++
		n++
	}
	return v, n > 0
}

// winEngKey identifies one engine of one physical adapter.
type winEngKey struct {
	LUID winLUID
	Phys uint32
	Eng  uint32
}

// winEngineAcc sums "\GPU Engine(*)\Utilization Percentage" over processes per engine
// (instances are per process and engine). The adapter's utilization is then its busiest
// engine, which is what Task Manager shows.
type winEngineAcc map[winEngKey]float64

// add adds one instance's value in percent. Names without an engine and values that are
// not finite or are negative are ignored.
func (a winEngineAcc) add(in winGPUInstance, pct float64) bool {
	if in.Eng < 0 || in.Eng > math.MaxUint32 || math.IsNaN(pct) || math.IsInf(pct, 0) || pct < 0 {
		return false
	}
	a[winEngKey{LUID: in.LUID, Phys: in.Phys, Eng: uint32(in.Eng)}] += pct
	return true
}

// util returns the adapter's utilization in percent (busiest engine, clamped to 0..100),
// or false when no engine of that adapter was added.
func (a winEngineAcc) util(l winLUID) (float64, bool) {
	best, found := 0.0, false
	for k, v := range a {
		if k.LUID == l && (!found || v > best) {
			best, found = v, true
		}
	}
	if !found {
		return 0, false
	}
	return math.Max(0, math.Min(100, best)), true
}

// winMemAcc sums "\GPU Adapter Memory(*)\Dedicated Usage" (bytes) over the physical
// adapters of each LUID.
type winMemAcc map[winLUID]float64

func (a winMemAcc) add(in winGPUInstance, b float64) bool {
	if math.IsNaN(b) || math.IsInf(b, 0) || b < 0 {
		return false
	}
	a[in.LUID] += b
	return true
}

// ---------------------------------------------------------------------------------------
// GPU adapters (DXGI)

// winAdapter is one adapter from IDXGIFactory1::EnumAdapters1 / GetDesc1.
type winAdapter struct {
	Desc         string // DXGI_ADAPTER_DESC1.Description
	VendorID     uint32
	DeviceID     uint32
	SubSysID     uint32
	Revision     uint32
	Dedicated    uint64 // DedicatedVideoMemory
	DedicatedSys uint64 // DedicatedSystemMemory
	Shared       uint64 // SharedSystemMemory
	LUID         winLUID
	Flags        uint32
}

const (
	winDXGIFlagSoftware = 2 // DXGI_ADAPTER_FLAG_SOFTWARE
	winVendorNVIDIA     = 0x10DE
	winVendorAMD        = 0x1002
	winVendorAMD2       = 0x1022
	winVendorIntel      = 0x8086
	winVendorMicrosoft  = 0x1414 // Basic Render Driver, Basic Display, Remote Display, Hyper-V
)

// winGPUVendor maps a PCI vendor id to proto.GPUInfo.Vendor.
func winGPUVendor(id uint32) string {
	switch id {
	case winVendorNVIDIA:
		return "nvidia"
	case winVendorAMD, winVendorAMD2:
		return "amd"
	case winVendorIntel:
		return "intel"
	}
	return "other"
}

// winPickAdapters applies the display rules to the DXGI adapters. nvml is the number of
// GPUs NVML reported; when it is > 0 those are shown instead and every non-NVIDIA adapter
// is hidden. Without NVML, NVIDIA adapters that DXGI still sees count as discrete too, so
// an integrated GPU next to them is hidden the same way. Software and Microsoft adapters
// are skipped, duplicate LUIDs dropped. shown is primary first (most dedicated memory).
func winPickAdapters(all []winAdapter, nvml int) (shown []winAdapter, notes []string) {
	seen := map[winLUID]bool{}
	var cand []winAdapter
	for _, a := range all {
		name := CleanGPUName(a.Desc)
		switch {
		case a.Flags&winDXGIFlagSoftware != 0:
			notes = append(notes, fmt.Sprintf("skipped: %s (software adapter)", name))
			continue
		case a.VendorID == winVendorMicrosoft:
			notes = append(notes, fmt.Sprintf("skipped: %s (Microsoft adapter)", name))
			continue
		case seen[a.LUID]:
			continue
		}
		seen[a.LUID] = true
		cand = append(cand, a)
	}
	discrete := nvml > 0
	for _, a := range cand {
		if a.VendorID == winVendorNVIDIA {
			discrete = true
		}
	}
	for _, a := range cand {
		if discrete && a.VendorID != winVendorNVIDIA {
			notes = append(notes, fmt.Sprintf("hidden: %s (integrated, a discrete GPU is present)", CleanGPUName(a.Desc)))
			continue
		}
		if nvml > 0 {
			continue // NVIDIA GPUs are shown through NVML
		}
		shown = append(shown, a)
	}
	sort.SliceStable(shown, func(i, j int) bool { return shown[i].Dedicated > shown[j].Dedicated })
	if len(shown) > proto.MaxGPUs {
		shown = shown[:proto.MaxGPUs]
	}
	return shown, notes
}

// ---------------------------------------------------------------------------------------
// Network interfaces

// IF_TYPE values (ipifcons.h).
const (
	winIfTypeEthernet = 6
	winIfTypeLoopback = 24
	winIfTypeWiFi     = 71
	winIfTypeTunnel   = 131
	winIfTypeWWANPP   = 243
	winIfTypeWWANPP2  = 244
)

// winVirtualWords mark adapters that are not the machine's own link to the network.
var winVirtualWords = []string{
	"hyper-v", "vethernet", "wsl", "docker", "virtualbox", "vboxnet", "vmware", "vmnet",
	"tap-", "tap adapter", "wintun", "wireguard", "tailscale", "zerotier", "loopback",
	"bluetooth", "virtual", "vpn", "tunnel", "pseudo-interface", "teredo", "isatap", "6to4",
	"ip-https", "kernel debug", "npcap", "wan miniport", "nordlynx", "openvpn", "anyconnect",
	"fortinet", "globalprotect", "pangp", "hamachi", "radmin",
}

// winVirtualName reports whether an interface alias or description names a virtual,
// tunnel, loopback or Bluetooth adapter.
func winVirtualName(alias, desc string) bool {
	s := strings.ToLower(alias + "\x00" + desc)
	for _, w := range winVirtualWords {
		if strings.Contains(s, w) {
			return true
		}
	}
	return false
}

// winNetUsable reports whether an interface may be followed as the machine's network
// link: a hardware interface that is not a loopback, a tunnel or a known virtual adapter.
func winNetUsable(ifType uint32, hardware bool, alias, desc string) bool {
	return hardware && ifType != winIfTypeLoopback && ifType != winIfTypeTunnel && !winVirtualName(alias, desc)
}

// winNetKind classifies the followed interface for proto.NetInfo.Kind.
func winNetKind(ifType uint32, hardware bool, alias, desc string) string {
	switch {
	case ifType == winIfTypeLoopback:
		return "loopback"
	case !winNetUsable(ifType, hardware, alias, desc):
		return "virtual"
	case ifType == winIfTypeWiFi, ifType == winIfTypeWWANPP, ifType == winIfTypeWWANPP2:
		return "wireless"
	case ifType == winIfTypeEthernet:
		return "wired"
	}
	return ""
}

// ---------------------------------------------------------------------------------------
// OS, power, CPU, memory

// winOSName builds the OS name from the registry's ProductName and DisplayVersion. Windows
// 11 kept ProductName "Windows 10 ..." so builds >= 22000 are renamed (Windows Server 2022
// is build 20348 and Server 2025 says "Windows Server", so neither is touched).
func winOSName(product, display string, build uint32) string {
	p := strings.Join(strings.Fields(product), " ")
	if p == "" {
		return ""
	}
	if build >= 22000 && (p == "Windows 10" || strings.HasPrefix(p, "Windows 10 ")) {
		p = "Windows 11" + p[len("Windows 10"):]
	}
	if d := strings.TrimSpace(display); d != "" && !strings.Contains(p, d) {
		p += " " + d
	}
	return p
}

// winBattery maps SYSTEM_POWER_STATUS. BatteryFlag 128 means no system battery and 255
// unknown status: both report no battery. Percent 255 is unknown.
func winBattery(acLine, flag, percent uint8) (present bool, pct *int, state int) {
	if flag == 255 || flag&128 != 0 {
		return false, nil, proto.BatUnknown
	}
	if percent <= 100 {
		v := int(percent)
		pct = &v
	}
	switch {
	case flag&8 != 0:
		state = proto.BatCharging
	case acLine == 0:
		state = proto.BatDischarging
	case acLine == 1:
		state = proto.BatFull
	default:
		state = proto.BatUnknown
	}
	return true, pct, state
}

// winCPUTimes holds one logical processor's SYSTEM_PROCESSOR_PERFORMANCE_INFORMATION
// counters in 100 ns units. KernelTime includes IdleTime.
type winCPUTimes struct{ Idle, Kernel, User int64 }

// winCPUBusy turns two readings into busy per mille per processor:
// busy = Δ(kernel+user−idle) / Δ(kernel+user). It returns nil when the readings do not
// line up, a counter went backwards or no time passed, rather than inventing zeros.
func winCPUBusy(prev, cur []winCPUTimes) []uint16 {
	if len(prev) != len(cur) || len(cur) == 0 {
		return nil
	}
	out := make([]uint16, len(cur))
	for i := range cur {
		di := cur[i].Idle - prev[i].Idle
		dk := cur[i].Kernel - prev[i].Kernel
		du := cur[i].User - prev[i].User
		total := dk + du
		if di < 0 || dk < 0 || du < 0 || total <= 0 {
			return nil
		}
		busy := total - di
		if busy < 0 {
			busy = 0
		}
		out[i] = proto.PerMille(float64(busy) / float64(total))
	}
	return out
}

// winCountCores counts RelationProcessorCore (0) records in a
// GetLogicalProcessorInformationEx buffer. Every SYSTEM_LOGICAL_PROCESSOR_INFORMATION_EX
// starts with Relationship and Size (uint32 each) and is Size bytes long.
func winCountCores(b []byte) (int, error) {
	n, off := 0, 0
	for off < len(b) {
		if len(b)-off < 8 {
			return 0, fmt.Errorf("truncated record at %d of %d bytes", off, len(b))
		}
		rel := binary.LittleEndian.Uint32(b[off:])
		size := binary.LittleEndian.Uint32(b[off+4:])
		if size < 8 || uint64(size) > uint64(len(b)-off) {
			return 0, fmt.Errorf("bad record size %d at %d of %d bytes", size, off, len(b))
		}
		if rel == 0 {
			n++
		}
		off += int(size)
	}
	if n == 0 {
		return 0, fmt.Errorf("no processor core records in %d bytes", len(b))
	}
	return n, nil
}

// winPageFileEntry is the size of SYSTEM_PAGEFILE_INFORMATION on 64-bit Windows:
// NextEntryOffset, TotalSize, TotalInUse, PeakUsage (uint32, sizes in pages), then a
// UNICODE_STRING (Length, MaximumLength uint16, 4 bytes padding, Buffer pointer).
const winPageFileEntry = 32

// winSumPageFiles walks the NtQuerySystemInformation(SystemPageFileInformation) buffer and
// sums current size and use in pages. An empty buffer means no page file (a real reading).
func winSumPageFiles(b []byte) (totalPages, usedPages uint64, files int, err error) {
	off := 0
	for len(b) > 0 {
		if off < 0 || off > len(b)-winPageFileEntry {
			return 0, 0, 0, fmt.Errorf("page file entry at %d overruns the %d-byte buffer", off, len(b))
		}
		next := binary.LittleEndian.Uint32(b[off:])
		totalPages += uint64(binary.LittleEndian.Uint32(b[off+4:]))
		usedPages += uint64(binary.LittleEndian.Uint32(b[off+8:]))
		files++
		if next == 0 {
			break
		}
		if next < winPageFileEntry || files >= 64 {
			return 0, 0, 0, fmt.Errorf("bad NextEntryOffset %d after %d entries", next, files)
		}
		off += int(next)
	}
	if usedPages > totalPages {
		return 0, 0, 0, fmt.Errorf("page files report %d pages in use of %d", usedPages, totalPages)
	}
	return totalPages, usedPages, files, nil
}

// ---------------------------------------------------------------------------------------
// Disks

// winDriveLetter returns "C:" for input starting with a drive letter and colon ("c:",
// `C:\Windows`), or "".
func winDriveLetter(s string) string {
	if len(s) < 2 || s[1] != ':' {
		return ""
	}
	if c := s[0] | 0x20; c < 'a' || c > 'z' {
		return ""
	}
	if len(s) > 2 && s[2] != '\\' && s[2] != '/' {
		return ""
	}
	return strings.ToUpper(s[:2])
}

// winBusName names a STORAGE_BUS_TYPE value (winioctl.h); unknown values give "".
func winBusName(t uint32) string {
	switch t {
	case 0x01:
		return "SCSI"
	case 0x02:
		return "ATAPI"
	case 0x03:
		return "ATA"
	case 0x04:
		return "IEEE 1394"
	case 0x06:
		return "Fibre Channel"
	case 0x07:
		return "USB"
	case 0x08:
		return "RAID"
	case 0x09:
		return "iSCSI"
	case 0x0A:
		return "SAS"
	case 0x0B:
		return "SATA"
	case 0x0C:
		return "SD"
	case 0x0D:
		return "MMC"
	case 0x0E:
		return "Virtual"
	case 0x0F:
		return "File-backed virtual"
	case 0x10:
		return "Storage Spaces"
	case 0x11:
		return "NVMe"
	case 0x12:
		return "SCM"
	case 0x13:
		return "UFS"
	case 0x14:
		return "NVMe-oF"
	}
	return ""
}

// winStorageHeader is the fixed part of STORAGE_DEVICE_DESCRIPTOR up to RawPropertiesLength;
// RawDeviceProperties (and usually the strings) start at this offset.
const winStorageHeader = 36

// winParseStorageDescriptor reads a STORAGE_DEVICE_DESCRIPTOR returned by
// IOCTL_STORAGE_QUERY_PROPERTY: VendorIdOffset @12, ProductIdOffset @16, BusType @28.
// Strings are NUL-terminated ASCII at those offsets; offset 0 means absent. Offsets that
// point into the header or past the returned bytes are treated as absent.
func winParseStorageDescriptor(b []byte) (vendor, product string, bus uint32, err error) {
	if len(b) < 32 {
		return "", "", 0, fmt.Errorf("STORAGE_DEVICE_DESCRIPTOR too short (%d bytes)", len(b))
	}
	vendor = winCString(b, binary.LittleEndian.Uint32(b[12:]))
	product = winCString(b, binary.LittleEndian.Uint32(b[16:]))
	return vendor, product, binary.LittleEndian.Uint32(b[28:]), nil
}

func winCString(b []byte, off uint32) string {
	if off < winStorageHeader || uint64(off) >= uint64(len(b)) {
		return ""
	}
	s := b[off:]
	if i := bytes.IndexByte(s, 0); i >= 0 {
		s = s[:i]
	}
	clean := make([]byte, 0, len(s))
	for _, c := range s {
		if c >= 0x20 && c < 0x7F {
			clean = append(clean, c)
		}
	}
	return strings.Join(strings.Fields(string(clean)), " ")
}

// winDiskModel joins vendor and product ids, dropping generic vendor ids ("ATA", "NVMe")
// and a vendor the product id already starts with.
func winDiskModel(vendor, product string) string {
	v, p := strings.TrimSpace(vendor), strings.TrimSpace(product)
	switch strings.ToUpper(v) {
	case "ATA", "NVME", "SCSI", "GENERIC", "(STANDARD DISK DRIVES)":
		v = ""
	}
	switch {
	case p == "":
		return v
	case v == "" || strings.HasPrefix(strings.ToUpper(p), strings.ToUpper(v)):
		return p
	}
	return v + " " + p
}

// ---------------------------------------------------------------------------------------
// PDH status codes (pdhmsg.h)

const (
	winPdhMoreData       = 0x800007D2
	winPdhNoData         = 0x800007D5
	winPdhInvalidData    = 0xC0000BC6
	winPdhCStatusInvalid = 0xC0000BBA
)

// winPdhStatus names a PDH_STATUS or PDH_CSTATUS value.
func winPdhStatus(code uint32) string {
	var name string
	switch code {
	case 0x00000000:
		name = "PDH_CSTATUS_VALID_DATA"
	case 0x00000001:
		name = "PDH_CSTATUS_NEW_DATA"
	case 0x800007D0:
		name = "PDH_CSTATUS_NO_MACHINE"
	case 0x800007D1:
		name = "PDH_CSTATUS_NO_INSTANCE"
	case winPdhMoreData:
		name = "PDH_MORE_DATA"
	case 0x800007D3:
		name = "PDH_CSTATUS_ITEM_NOT_VALIDATED"
	case 0x800007D4:
		name = "PDH_RETRY"
	case winPdhNoData:
		name = "PDH_NO_DATA"
	case 0x800007D6:
		name = "PDH_CALC_NEGATIVE_DENOMINATOR"
	case 0x800007D7:
		name = "PDH_CALC_NEGATIVE_TIMEBASE"
	case 0x800007D8:
		name = "PDH_CALC_NEGATIVE_VALUE"
	case 0xC0000BB8:
		name = "PDH_CSTATUS_NO_OBJECT"
	case 0xC0000BB9:
		name = "PDH_CSTATUS_NO_COUNTER"
	case winPdhCStatusInvalid:
		name = "PDH_CSTATUS_INVALID_DATA"
	case 0xC0000BBB:
		name = "PDH_MEMORY_ALLOCATION_FAILURE"
	case 0xC0000BBC:
		name = "PDH_INVALID_HANDLE"
	case 0xC0000BBD:
		name = "PDH_INVALID_ARGUMENT"
	case 0xC0000BBE:
		name = "PDH_FUNCTION_NOT_FOUND"
	case 0xC0000BBF:
		name = "PDH_CSTATUS_NO_COUNTERNAME"
	case 0xC0000BC0:
		name = "PDH_CSTATUS_BAD_COUNTERNAME"
	case 0xC0000BC1:
		name = "PDH_INVALID_BUFFER"
	case 0xC0000BC2:
		name = "PDH_INSUFFICIENT_BUFFER"
	case 0xC0000BC4:
		name = "PDH_INVALID_PATH"
	case 0xC0000BC5:
		name = "PDH_INVALID_INSTANCE"
	case winPdhInvalidData:
		name = "PDH_INVALID_DATA"
	case 0xC0000BC8:
		name = "PDH_CANNOT_READ_NAME_STRINGS"
	case 0xC0000BD3:
		name = "PDH_NOT_IMPLEMENTED"
	case 0xC0000BDB:
		name = "PDH_ACCESS_DENIED"
	}
	if name != "" {
		return fmt.Sprintf("%s (0x%08X)", name, code)
	}
	if code < 0x10000 {
		return fmt.Sprintf("Win32 error %d", code)
	}
	return fmt.Sprintf("PDH status 0x%08X", code)
}

// winPdhNotReady reports codes that mean "no valid value this time": a rate counter that
// has only one collection, or a transient calculation problem.
func winPdhNotReady(code uint32) bool {
	switch code {
	case winPdhInvalidData, winPdhCStatusInvalid, winPdhNoData,
		0x800007D6, 0x800007D7, 0x800007D8, 0x800007D3:
		return true
	}
	return false
}

// ---------------------------------------------------------------------------------------
// ASUS ATK ACPI

// winASUSTemp decodes the ATK "device status" (DSTS) reply for a temperature device: bit 16
// says the device is present and the low 16 bits are °C. Firmware answers 0xFFFFFFFE (older
// firmware 0) for a device it does not have, and 0x10000 (present, 0 °C) where the sensor
// exists but reads nothing, such as the GPU temperature of a laptop without a discrete GPU.
func winASUSTemp(v uint32) (float64, bool) {
	if v>>16 != 1 {
		return 0, false
	}
	t := v & 0xFFFF
	if t == 0 || t >= 150 {
		return 0, false
	}
	return float64(t), true
}
