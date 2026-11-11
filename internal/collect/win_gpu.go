//go:build windows && (amd64 || arm64)

package collect

// GPUs: NVIDIA through NVML (the shared Nvidia helper); everything else through DXGI for
// the adapter list (once, at New) and the Windows GPU performance counters for load and
// memory. The registry and WMI are never used: the registry keeps cards that were removed
// and Win32_VideoController caps memory at 4 GiB.

import (
	"errors"
	"fmt"
	"math"
	"sort"
	"strings"
	"syscall"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"

	"btophub/internal/proto"
)

var (
	winModDXGI                = windows.NewLazySystemDLL("dxgi.dll")
	winProcCreateDXGIFactory1 = winModDXGI.NewProc("CreateDXGIFactory1")

	// IID_IDXGIFactory1 {770aae78-f26f-4dba-a829-253c83d1b387}
	winIIDDXGIFactory1 = windows.GUID{Data1: 0x770aae78, Data2: 0xf26f, Data3: 0x4dba,
		Data4: [8]byte{0xa8, 0x29, 0x25, 0x3c, 0x83, 0xd1, 0xb3, 0x87}}
)

const (
	// Method table slots. IUnknown: 0 QueryInterface, 1 AddRef, 2 Release. IDXGIObject:
	// 3-6. IDXGIFactory: 7 EnumAdapters .. 11 CreateSoftwareAdapter; IDXGIFactory1: 12
	// EnumAdapters1, 13 IsCurrent. IDXGIAdapter: 7 EnumOutputs, 8 GetDesc, 9
	// CheckInterfaceSupport; IDXGIAdapter1: 10 GetDesc1.
	winVtblRelease       = 2
	winVtblEnumAdapters1 = 12
	winVtblGetDesc1      = 10

	winDXGIErrNotFound = 0x887A0002 // DXGI_ERROR_NOT_FOUND: no more adapters
)

// winDXGIAdapterDesc1 is DXGI_ADAPTER_DESC1 (dxgi.h), 312 bytes on 64-bit Windows.
type winDXGIAdapterDesc1 struct {
	Description           [128]uint16
	VendorID              uint32
	DeviceID              uint32
	SubSysID              uint32
	Revision              uint32
	DedicatedVideoMemory  uintptr
	DedicatedSystemMemory uintptr
	SharedSystemMemory    uintptr
	AdapterLuid           windows.LUID
	Flags                 uint32
}

// winCOM is the start of every COM object: a pointer to its method table. COM objects
// live outside the Go heap, which the garbage collector ignores.
type winCOM struct{ vtbl *[16]uintptr }

func (o *winCOM) release() {
	syscall.SyscallN(o.vtbl[winVtblRelease], uintptr(unsafe.Pointer(o)))
}

// winEnumDXGI lists adapters through IDXGIFactory1::EnumAdapters1 and GetDesc1. Every COM
// object is released before it returns. No COM initialisation is needed for DXGI.
func winEnumDXGI() ([]winAdapter, error) {
	if err := winProcCreateDXGIFactory1.Find(); err != nil {
		return nil, fmt.Errorf("dxgi.dll: %w", err)
	}
	var factory *winCOM
	r, _, _ := syscall.SyscallN(winProcCreateDXGIFactory1.Addr(),
		uintptr(unsafe.Pointer(&winIIDDXGIFactory1)), uintptr(unsafe.Pointer(&factory)))
	if int32(r) < 0 {
		return nil, fmt.Errorf("CreateDXGIFactory1: HRESULT 0x%08X", uint32(r))
	}
	if factory == nil || factory.vtbl == nil {
		return nil, errors.New("CreateDXGIFactory1 returned no factory")
	}
	defer factory.release()
	var out []winAdapter
	for i := uintptr(0); i < 64; i++ {
		var ad *winCOM
		r, _, _ := syscall.SyscallN(factory.vtbl[winVtblEnumAdapters1], uintptr(unsafe.Pointer(factory)),
			i, uintptr(unsafe.Pointer(&ad)))
		if uint32(r) == winDXGIErrNotFound {
			return out, nil
		}
		if int32(r) < 0 {
			return out, fmt.Errorf("IDXGIFactory1::EnumAdapters1(%d): HRESULT 0x%08X", i, uint32(r))
		}
		if ad == nil || ad.vtbl == nil {
			return out, fmt.Errorf("IDXGIFactory1::EnumAdapters1(%d) returned no adapter", i)
		}
		var d winDXGIAdapterDesc1
		r, _, _ = syscall.SyscallN(ad.vtbl[winVtblGetDesc1], uintptr(unsafe.Pointer(ad)), uintptr(unsafe.Pointer(&d)))
		ad.release()
		if int32(r) < 0 {
			return out, fmt.Errorf("IDXGIAdapter1::GetDesc1(%d): HRESULT 0x%08X", i, uint32(r))
		}
		out = append(out, winAdapter{
			Desc:         windows.UTF16ToString(d.Description[:]),
			VendorID:     d.VendorID,
			DeviceID:     d.DeviceID,
			SubSysID:     d.SubSysID,
			Revision:     d.Revision,
			Dedicated:    uint64(d.DedicatedVideoMemory),
			DedicatedSys: uint64(d.DedicatedSystemMemory),
			Shared:       uint64(d.SharedSystemMemory),
			LUID:         winLUID{High: uint32(d.AdapterLuid.HighPart), Low: d.AdapterLuid.LowPart},
			Flags:        d.Flags,
		})
	}
	return out, errors.New("stopped after 64 adapters")
}

// winGPU is one GPU shown through DXGI and the GPU performance counters.
type winGPU struct {
	ad   winAdapter
	info proto.GPUInfo
	src  string // Report.Sources key: "gpu0", "gpu1", ...
}

func winGPUTempSource(vendor, nvmlWhy string) string {
	switch vendor {
	case "amd":
		return "none: AMD's driver library (atiadlxx.dll) did not load (see the adl note), so only the Windows GPU counters are used"
	case "intel":
		return "none: needs the Intel driver library (IGCL); not implemented"
	case "nvidia":
		return "none: needs NVML, which is not available (" + nvmlWhy + ")"
	}
	return "none: no vendor library for this GPU; not implemented"
}

func winI64(u uint64) int64 {
	if u > math.MaxInt64 {
		return math.MaxInt64
	}
	return int64(u)
}

// initGPUs opens NVML and enumerates DXGI adapters once. With NVML GPUs those are shown
// and every other adapter is hidden; otherwise DXGI adapters are shown and read through
// the PDH GPU counters, which join the shared query.
func (c *winCollector) initGPUs() {
	t0 := time.Now()
	nv, nvWhy := OpenNvidia()
	c.initTimes = append(c.initTimes, fmt.Sprintf("NVML %.1f ms", winMs(time.Since(t0))))
	if nv != nil {
		c.note("nvml", nvWhy)
	} else {
		c.note("nvml", "nvml: "+nvWhy)
	}
	// NVIDIA GPUs found: show exactly those. DXGI is not touched, because enumerating
	// adapters loads every vendor's user-mode driver into this process (tens of MB).
	if nv != nil {
		c.nv = nv
		infos := nv.Infos()
		if len(infos) > proto.MaxGPUs {
			infos = infos[:proto.MaxGPUs]
		}
		c.info.GPUs = infos
		for i, g := range infos {
			c.setSource(fmt.Sprintf("gpu%d", i), "NVML: "+g.Name)
		}
		c.note("hidden", "other adapters (integrated GPUs) are not shown: an NVIDIA GPU is present")
		return
	}
	// AMD: the driver's own library gives load, clocks, temperature, power and the exact VRAM
	// split, like LibreHardwareMonitor (and btop4win on the Z13) without its kernel driver.
	t2 := time.Now()
	adl, adlWhy, adlNotes := winOpenADL()
	c.initTimes = append(c.initTimes, fmt.Sprintf("ADL %.1f ms", winMs(time.Since(t2))))
	for i, n := range adlNotes {
		c.note(fmt.Sprintf("adl%d", i), n)
	}
	if adl != nil {
		c.adl = adl
		c.info.GPUs = adl.infos()
		for i, g := range adl.gpus {
			src := fmt.Sprintf("gpu%d", i)
			c.setSource(src, "ADL (atiadlxx.dll): "+g.name+": load INFO_ACTIVITY_GFX, clock CLK_GFXCLK, VRAM used DedicatedVRAMUsage, total MemoryInfoX4")
			if g.temp != 0 {
				c.setSource(src+"_temp", "ADL PMLog TEMPERATURE_"+strings.ToUpper(adlTempName(g.temp)))
			} else {
				c.setSource(src+"_temp", "none: ADL reports no GPU temperature on this adapter")
			}
			if g.power != 0 {
				c.setSource(src+"_power", "ADL PMLog "+adlSensorNames[g.power])
			}
			if g.cpuTemp && !c.info.HasTemp {
				c.info.HasTemp = true
				c.setSource("cpu_temp", "ADL PMLog TEMPERATURE_CPU (reported by the APU's power management)")
			}
		}
		return
	}
	c.note("adl", "adl: "+adlWhy)
	t1 := time.Now()
	ads, derr := winEnumDXGI()
	c.initTimes = append(c.initTimes, fmt.Sprintf("DXGI %.1f ms", winMs(time.Since(t1))))
	if derr != nil {
		c.note("dxgi", "dxgi: "+derr.Error())
	}
	for i, a := range ads {
		c.note(fmt.Sprintf("dxgi%d", i), fmt.Sprintf(
			"dxgi adapter %d: %q vendor 0x%04X device 0x%04X subsys 0x%08X rev 0x%02X, dedicated %d MiB, dedicated system %d MiB, shared %d MiB, LUID %s, flags 0x%X",
			i, a.Desc, a.VendorID, a.DeviceID, a.SubSysID, a.Revision, a.Dedicated>>20, a.DedicatedSys>>20, a.Shared>>20, a.LUID, a.Flags))
	}
	nvCount := 0
	if nv != nil {
		nvCount = len(nv.Infos())
	}
	shown, notes := winPickAdapters(ads, nvCount)
	for _, n := range notes {
		c.note(n, n)
	}

	for i, a := range shown {
		v := winGPUVendor(a.VendorID)
		g := winGPU{ad: a, src: fmt.Sprintf("gpu%d", i)}
		g.info = proto.GPUInfo{
			Name:       winTrunc(CleanGPUName(a.Desc), proto.MaxStringLen),
			Vendor:     v,
			MemTotal:   winI64(a.Dedicated),
			Integrated: v == "amd" || v == "intel", // shown only because no NVIDIA GPU exists
		}
		c.gpus = append(c.gpus, g)
		c.info.GPUs = append(c.info.GPUs, g.info)
		c.setSource(g.src+"_temp", winGPUTempSource(v, nvWhy))
	}
	if len(c.gpus) == 0 {
		why := "none: no GPU found (" + nvWhy
		if derr != nil {
			why += "; DXGI: " + derr.Error()
		}
		c.setSource("gpu0", why+")")
		return
	}

	c.engAcc, c.memAcc = winEngineAcc{}, winMemAcc{}
	c.gpuEngSeen, c.gpuMemSeen = map[winLUID]bool{}, map[winLUID]bool{}
	utilSrc, memSrc := "util: none: PDH unavailable", "memory: none: PDH unavailable"
	if c.pdh != nil {
		if h, err := c.pdh.add(winCtrGPUEng); err != nil {
			utilSrc = "util: none: PDH " + winCtrGPUEng + ": " + err.Error()
		} else {
			c.ctrGPUEng, utilSrc = h, winSrcGPUUtil
		}
		if h, err := c.pdh.add(winCtrGPUMem); err != nil {
			memSrc = "memory: none: PDH " + winCtrGPUMem + ": " + err.Error()
		} else {
			c.ctrGPUMem, memSrc = h, winSrcGPUMem
		}
	}
	for i := range c.gpus {
		g := &c.gpus[i]
		c.part(g.src, 0, winGPUAdapterSource(g.ad))
		c.part(g.src, 1, utilSrc)
		c.part(g.src, 2, memSrc)
	}
}

func winGPUAdapterSource(a winAdapter) string {
	return fmt.Sprintf("DXGI %q (LUID %s)", a.Desc, a.LUID)
}

// sampleGPUs reads the DXGI-path GPUs from the last PDH collection. Utilization is the
// busiest engine after summing processes, like Task Manager; memory is dedicated usage.
func (c *winCollector) sampleGPUs() []proto.GPUSample {
	out := make([]proto.GPUSample, len(c.gpus))
	if c.pdh == nil || !c.pdhOK {
		return out // collectPDH already recorded why
	}
	warm := c.pdhCollections >= 2
	clear(c.engAcc)
	clear(c.memAcc)
	clear(c.gpuEngSeen)
	clear(c.gpuMemSeen)
	engValid, bad := false, 0
	if c.ctrGPUEng != 0 {
		items, err := c.pdh.array(c.ctrGPUEng, &c.gpuEngBuf)
		switch {
		case err != nil && !warm && winPdhNotReadyErr(err):
		case err != nil:
			c.gpuPartAll(1, "util: none: PDH "+winCtrGPUEng+": "+err.Error())
		default:
			for i := range items {
				it := &items[i]
				in, ok := winParseGPUInstance(winPdhName(c.gpuEngBuf, it))
				if !ok {
					bad++
					continue
				}
				c.gpuEngSeen[in.LUID] = true
				if st := it.FmtValue.CStatus; (st == 0 || st == 1) && c.engAcc.add(in, it.FmtValue.Value) {
					engValid = true
				}
			}
			c.gpuPartAll(1, winSrcGPUUtil)
		}
	}
	if c.ctrGPUMem != 0 {
		items, err := c.pdh.array(c.ctrGPUMem, &c.gpuMemBuf)
		if err != nil {
			c.gpuPartAll(2, "memory: none: PDH "+winCtrGPUMem+": "+err.Error())
		} else {
			for i := range items {
				it := &items[i]
				in, ok := winParseGPUInstance(winPdhName(c.gpuMemBuf, it))
				if !ok {
					bad++
					continue
				}
				c.gpuMemSeen[in.LUID] = true
				if st := it.FmtValue.CStatus; st == 0 || st == 1 {
					c.memAcc.add(in, it.FmtValue.Value)
				}
			}
			c.gpuPartAll(2, winSrcGPUMem)
		}
	}
	if bad > 0 && !c.gpuBadNamesNoted {
		c.gpuBadNamesNoted = true
		c.note("gpu_names", fmt.Sprintf("gpu: %d PDH GPU instance names could not be parsed and were skipped", bad))
	}
	if !c.gpuLUIDsNoted && (len(c.gpuEngSeen) > 0 || len(c.gpuMemSeen) > 0) {
		// Once, for --probe: these must match the DXGI adapter LUIDs listed above.
		c.gpuLUIDsNoted = true
		c.note("gpu_luids", fmt.Sprintf("gpu: PDH counters list LUIDs: engines %s, memory %s",
			winLUIDList(c.gpuEngSeen), winLUIDList(c.gpuMemSeen)))
	}
	missing := false
	for i := range c.gpus {
		l := c.gpus[i].ad.LUID
		if u, ok := c.engAcc.util(l); ok {
			out[i].Util = proto.U16(proto.PerMille(u / 100))
		} else if engValid && !c.gpuEngSeen[l] && c.gpuMemSeen[l] {
			out[i].Util = proto.U16(0) // adapter is present and no process has work queued on it
		}
		if m, ok := c.memAcc[l]; ok {
			out[i].MemUsed = proto.I64(int64(m))
		}
		if !c.gpuEngSeen[l] && !c.gpuMemSeen[l] && (len(c.gpuEngSeen) > 0 || len(c.gpuMemSeen) > 0) {
			missing = true
		}
	}
	// The counters list adapters but not ours: the LUID probably changed (driver restart or
	// update). After 10 such samples Info() enumerates DXGI again and re-matches by PCI id.
	if missing {
		c.gpuMissing++
		if c.gpuMissing >= 10 {
			c.gpuRescan = true
		}
	} else {
		c.gpuMissing = 0
	}
	return out
}

func winLUIDList(m map[winLUID]bool) string {
	l := make([]string, 0, len(m))
	for k := range m {
		l = append(l, k.String())
	}
	sort.Strings(l)
	return "[" + strings.Join(l, " ") + "]"
}

func (c *winCollector) gpuPartAll(i int, v string) {
	for k := range c.gpus {
		c.part(c.gpus[k].src, i, v)
	}
}

// rescanGPUs re-enumerates DXGI after the shown adapters' LUIDs vanished from the GPU
// counters and moves each to the new LUID of an adapter with the same PCI ids. The GPU
// list itself never changes, so Info and Sample stay aligned.
func (c *winCollector) rescanGPUs() {
	c.gpuRescan, c.gpuMissing, c.gpuRescanAt = false, 0, time.Now()
	ads, err := winEnumDXGI()
	if err != nil {
		c.note("gpu_rescan", "gpu: DXGI re-enumeration failed: "+err.Error())
		return
	}
	used := map[winLUID]bool{}
	for _, g := range c.gpus {
		used[g.ad.LUID] = true
	}
	for i := range c.gpus {
		g := &c.gpus[i]
		if c.gpuEngSeen[g.ad.LUID] || c.gpuMemSeen[g.ad.LUID] {
			continue
		}
		for _, a := range ads {
			if used[a.LUID] || a.VendorID != g.ad.VendorID || a.DeviceID != g.ad.DeviceID ||
				a.SubSysID != g.ad.SubSysID || a.Revision != g.ad.Revision {
				continue
			}
			c.note("gpu_rescan_"+g.src, fmt.Sprintf("%s: LUID changed from %s to %s (driver restarted?)", g.src, g.ad.LUID, a.LUID))
			used[a.LUID] = true
			g.ad.LUID = a.LUID
			c.part(g.src, 0, winGPUAdapterSource(g.ad))
			break
		}
	}
}
