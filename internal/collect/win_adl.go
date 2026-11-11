//go:build windows && (amd64 || arm64)

package collect

import (
	"encoding/binary"
	"fmt"
	"sort"
	"strings"
	"syscall"
	"unsafe"

	"golang.org/x/sys/windows"

	"btophub/internal/proto"
)

// AMD Display Library (atiadlxx.dll), installed with every AMD graphics driver. It is the
// source LibreHardwareMonitor uses for AMD GPUs: load, clocks, temperatures, power and VRAM,
// without a kernel driver. On APUs such as the Z13's Ryzen AI Max+ 395 the same power-
// management log can also carry the CPU temperature. Layouts follow the ADL SDK headers
// (adl_structures.h: AdapterInfo, ADLPMLogDataOutput, ADLMemoryInfoX4; adl_defines.h:
// ADL_PMLOG_SENSORS, ADL_ASIC_*).

var (
	winModADL          = windows.NewLazySystemDLL("atiadlxx.dll")
	winProcHeapAlloc   = winModKernel32.NewProc("HeapAlloc")
	winProcProcessHeap = winModKernel32.NewProc("GetProcessHeap")
)

const (
	adlOK = 0

	adlAdapterInfoSize = 1572          // AdapterInfo on Windows: 13 ints + 6 strings of 256 chars
	adlPMLogSize       = 4 + 256*8     // ADLPMLogDataOutput: size + 256 {supported, value}
	adlMemInfoX4Size   = 8 + 256 + 7*8 // ADLMemoryInfoX4

	adlASICDiscrete   = 1 << 0
	adlASICIntegrated = 1 << 1

	// ADL_PMLOG_SENSORS
	adlClkGFX      = 1
	adlClkMem      = 2
	adlTempEdge    = 8
	adlActivityGFX = 19
	adlASICPower   = 23
	adlTempVRSoC   = 24
	adlTempHotspot = 27
	adlTempGFX     = 28
	adlTempSoC     = 29
	adlGFXPower    = 30
	adlTempCPU     = 32
	adlAPUPower    = 46 // ADL_PMLOG_SSPAIRED_ASICPOWER
)

var adlSensorNames = map[int]string{
	1: "CLK_GFXCLK", 2: "CLK_MEMCLK", 3: "CLK_SOCCLK", 7: "CLK_VCNCLK", 8: "TEMPERATURE_EDGE", 9: "TEMPERATURE_MEM",
	10: "TEMPERATURE_VRVDDC", 11: "TEMPERATURE_VRMVDD", 14: "FAN_RPM", 15: "FAN_PERCENTAGE", 16: "SOC_VOLTAGE",
	17: "SOC_POWER", 18: "SOC_CURRENT", 19: "INFO_ACTIVITY_GFX", 20: "INFO_ACTIVITY_MEM", 21: "GFX_VOLTAGE",
	22: "MEM_VOLTAGE", 23: "ASIC_POWER", 24: "TEMPERATURE_VRSOC", 27: "TEMPERATURE_HOTSPOT", 28: "TEMPERATURE_GFX",
	29: "TEMPERATURE_SOC", 30: "GFX_POWER", 31: "GFX_CURRENT", 32: "TEMPERATURE_CPU", 33: "CPU_POWER",
	34: "CLK_CPUCLK", 44: "CLK_FCLK", 46: "SSPAIRED_ASICPOWER",
}

// adlAlloc is ADL's ADL_MAIN_MALLOC_CALLBACK: void* (int size). Memory comes from the process
// heap and is never freed; ADL only uses it for a few small, one-time allocations.
var adlAlloc = syscall.NewCallback(func(size uintptr) uintptr {
	heap, _, _ := winProcProcessHeap.Call()
	p, _, _ := winProcHeapAlloc.Call(heap, 8 /* HEAP_ZERO_MEMORY */, size&0x7fffffff)
	return p
})

type winADLGPU struct {
	index      int
	name       string
	integrated bool
	memTotal   int64
	temp       int // chosen ADL_PMLOG temperature sensor, 0 for none
	power      int // chosen power sensor, 0 for none
	cpuTemp    bool
}

// winADL holds one ADL context and heap buffers for its outputs (C never sees a Go stack).
type winADL struct {
	ctx  uintptr
	out  *[16]byte
	pm   *[adlPMLogSize]byte
	mem  *[adlMemInfoX4Size]byte
	gpus []winADLGPU

	pDestroy, pPMLog, pVRAM *windows.LazyProc
}

func (a *winADL) call(p *windows.LazyProc, args ...uintptr) int32 {
	r, _, _ := p.Call(args...)
	return int32(uint32(r))
}

func (a *winADL) outPtr(off int) uintptr { return uintptr(unsafe.Pointer(&a.out[off])) }
func (a *winADL) outI32(off int) int32   { return int32(binary.LittleEndian.Uint32(a.out[off:])) }

// sensor returns ADLPMLogDataOutput.sensors[id] from the last query.
func (a *winADL) sensor(id int) (int32, bool) {
	off := 4 + 8*id
	sup := binary.LittleEndian.Uint32(a.pm[off:])
	return int32(binary.LittleEndian.Uint32(a.pm[off+4:])), sup != 0
}

func (a *winADL) queryPMLog(index int) bool {
	*a.pm = [adlPMLogSize]byte{}
	binary.LittleEndian.PutUint32(a.pm[0:], adlPMLogSize)
	return a.call(a.pPMLog, a.ctx, uintptr(index), uintptr(unsafe.Pointer(&a.pm[0]))) == adlOK
}

func adlCString(b []byte) string {
	if i := strings.IndexByte(string(b), 0); i >= 0 {
		b = b[:i]
	}
	return strings.TrimSpace(string(b))
}

// winOpenADL initialises ADL and lists the AMD GPUs to show: discrete ones if there are any,
// otherwise the integrated one. notes describe what was found, for --probe.
func winOpenADL() (a *winADL, why string, notes []string) {
	if err := winHave(winModADL, "ADL2_Main_Control_Create", "ADL2_Main_Control_Destroy",
		"ADL2_Adapter_NumberOfAdapters_Get", "ADL2_Adapter_AdapterInfo_Get", "ADL2_New_QueryPMLogData_Get"); err != nil {
		return nil, "ADL (atiadlxx.dll) not available: " + err.Error(), nil
	}
	a = &winADL{out: new([16]byte), pm: new([adlPMLogSize]byte), mem: new([adlMemInfoX4Size]byte),
		pDestroy: winModADL.NewProc("ADL2_Main_Control_Destroy"),
		pPMLog:   winModADL.NewProc("ADL2_New_QueryPMLogData_Get"),
		pVRAM:    winModADL.NewProc("ADL2_Adapter_DedicatedVRAMUsage_Get")}
	if r := a.call(winModADL.NewProc("ADL2_Main_Control_Create"), adlAlloc, 1, a.outPtr(0)); r != adlOK {
		return nil, fmt.Sprintf("ADL2_Main_Control_Create returned %d", r), nil
	}
	a.ctx = *(*uintptr)(unsafe.Pointer(&a.out[0]))
	fail := func(s string) (*winADL, string, []string) { a.close(); return nil, s, notes }

	*a.out = [16]byte{}
	if r := a.call(winModADL.NewProc("ADL2_Adapter_NumberOfAdapters_Get"), a.ctx, a.outPtr(0)); r != adlOK {
		return fail(fmt.Sprintf("ADL2_Adapter_NumberOfAdapters_Get returned %d", r))
	}
	n := int(a.outI32(0))
	if n <= 0 || n > 256 {
		return fail(fmt.Sprintf("ADL reports %d adapters", n))
	}
	buf := make([]byte, n*adlAdapterInfoSize)
	for i := 0; i < n; i++ {
		binary.LittleEndian.PutUint32(buf[i*adlAdapterInfoSize:], adlAdapterInfoSize)
	}
	if r := a.call(winModADL.NewProc("ADL2_Adapter_AdapterInfo_Get"), a.ctx, uintptr(unsafe.Pointer(&buf[0])), uintptr(len(buf))); r != adlOK {
		return fail(fmt.Sprintf("ADL2_Adapter_AdapterInfo_Get returned %d", r))
	}

	haveASIC := winHave(winModADL, "ADL2_Adapter_ASICFamilyType_Get") == nil
	haveMem := winHave(winModADL, "ADL2_Adapter_MemoryInfoX4_Get") == nil
	seen := map[[3]int32]bool{}
	var all []winADLGPU
	for i := 0; i < n; i++ {
		e := buf[i*adlAdapterInfoSize : (i+1)*adlAdapterInfoSize]
		i32 := func(off int) int32 { return int32(binary.LittleEndian.Uint32(e[off:])) }
		index, bus, dev, fn, vendor := i32(4), i32(264), i32(268), i32(272), i32(276)
		udid, name := adlCString(e[8:264]), adlCString(e[280:536])
		if udid == "" || (vendor != 1002 && vendor != 0x1002) {
			continue
		}
		key := [3]int32{bus, dev, fn}
		if seen[key] { // one entry per display output; keep the first
			continue
		}
		seen[key] = true
		g := winADLGPU{index: int(index), name: winTrunc(CleanGPUName(name), proto.MaxStringLen)}
		if haveASIC {
			*a.out = [16]byte{}
			if a.call(winModADL.NewProc("ADL2_Adapter_ASICFamilyType_Get"), a.ctx, uintptr(index), a.outPtr(0), a.outPtr(4)) == adlOK {
				types, valid := a.outI32(0), a.outI32(4)
				g.integrated = types&valid&adlASICIntegrated != 0 && types&valid&adlASICDiscrete == 0
			}
		}
		if haveMem {
			*a.mem = [adlMemInfoX4Size]byte{}
			if a.call(winModADL.NewProc("ADL2_Adapter_MemoryInfoX4_Get"), a.ctx, uintptr(index), uintptr(unsafe.Pointer(&a.mem[0]))) == adlOK {
				if v := int64(binary.LittleEndian.Uint64(a.mem[0:])); v > 0 {
					g.memTotal = v
				}
			}
		}
		// Which power-management sensors this GPU reports, and the best temperature.
		var have []string
		if a.queryPMLog(g.index) {
			ids := make([]int, 0, 32)
			for id := 1; id < 256; id++ {
				if v, ok := a.sensor(id); ok {
					ids = append(ids, id)
					nm := adlSensorNames[id]
					if nm == "" {
						nm = fmt.Sprint(id)
					}
					have = append(have, fmt.Sprintf("%s=%d", nm, v))
				}
			}
			sort.Ints(ids)
			for _, id := range []int{adlTempEdge, adlTempGFX, adlTempHotspot, adlTempSoC, adlTempVRSoC} {
				if v, ok := a.sensor(id); ok && v > 0 && v < 150 {
					g.temp = id
					break
				}
			}
			for _, id := range []int{adlASICPower, adlAPUPower, adlGFXPower} {
				if v, ok := a.sensor(id); ok && v > 0 {
					g.power = id
					break
				}
			}
			if v, ok := a.sensor(adlTempCPU); ok && v > 0 && v < 150 {
				g.cpuTemp = true
			}
		}
		notes = append(notes, fmt.Sprintf("adl adapter %d: %q bus %d, %s, VRAM %d MiB, PMLog sensors: %s",
			g.index, name, bus, map[bool]string{true: "integrated", false: "discrete or unknown"}[g.integrated],
			g.memTotal>>20, strings.Join(have, " ")))
		all = append(all, g)
	}
	if len(all) == 0 {
		return fail("ADL lists no AMD GPU")
	}
	// Show discrete GPUs; an integrated one only when it is the only GPU.
	for _, g := range all {
		if !g.integrated {
			a.gpus = append(a.gpus, g)
		}
	}
	if len(a.gpus) == 0 {
		a.gpus = all[:1]
	} else if len(a.gpus) < len(all) {
		notes = append(notes, "adl: hiding the integrated GPU (a discrete AMD GPU is present)")
	}
	sort.SliceStable(a.gpus, func(i, j int) bool { return a.gpus[i].memTotal > a.gpus[j].memTotal })
	return a, "", notes
}

func adlTempName(id int) string {
	return strings.ToLower(strings.TrimPrefix(adlSensorNames[id], "TEMPERATURE_"))
}

// infos describes the shown GPUs.
func (a *winADL) infos() []proto.GPUInfo {
	out := make([]proto.GPUInfo, len(a.gpus))
	for i, g := range a.gpus {
		out[i] = proto.GPUInfo{Name: g.name, Vendor: "amd", MemTotal: g.memTotal, Integrated: g.integrated}
	}
	return out
}

// sample reads every shown GPU, and the CPU temperature when the APU reports it.
func (a *winADL) sample() (gs []proto.GPUSample, cpuTemp *float64) {
	gs = make([]proto.GPUSample, len(a.gpus))
	for i, g := range a.gpus {
		s := &gs[i]
		if a.queryPMLog(g.index) {
			if v, ok := a.sensor(adlActivityGFX); ok && v >= 0 && v <= 100 {
				s.Util = proto.U16(proto.PerMille(float64(v) / 100))
			}
			if v, ok := a.sensor(adlClkGFX); ok && v > 0 {
				s.Clock = proto.Int(int(v))
			}
			if v, ok := a.sensor(adlClkMem); ok && v > 0 {
				s.MemClock = proto.Int(int(v))
			}
			if g.temp != 0 {
				if v, ok := a.sensor(g.temp); ok && v > 0 && v < 150 {
					s.Temp = proto.F64(float64(v))
				}
			}
			if g.power != 0 {
				if v, ok := a.sensor(g.power); ok && v > 0 {
					s.Power = proto.F64(float64(v))
				}
			}
			if g.cpuTemp && cpuTemp == nil {
				if v, ok := a.sensor(adlTempCPU); ok && v > 0 && v < 150 {
					cpuTemp = proto.F64(float64(v))
				}
			}
		}
		*a.out = [16]byte{}
		if a.call(a.pVRAM, a.ctx, uintptr(g.index), a.outPtr(0)) == adlOK {
			if mb := a.outI32(0); mb >= 0 {
				s.MemUsed = proto.I64(int64(mb) << 20)
			}
		}
	}
	return gs, cpuTemp
}

func (a *winADL) close() {
	if a.ctx != 0 {
		a.call(a.pDestroy, a.ctx)
		a.ctx = 0
	}
}
