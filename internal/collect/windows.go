//go:build windows && (amd64 || arm64)

package collect

// The Windows collector. Everything is read from kernel interfaces, documented Win32
// APIs, one long-lived PDH query, long-lived NVML and ADL handles and, on ASUS laptops, the
// firmware's ATK device: no WMI, no child processes, no cgo. Per-sample output buffers live in the collector (heap memory, reused every
// second); calls into Windows either go through x/sys wrappers with typed pointers to that
// memory or through syscall.SyscallN with the pointer conversion inline in the call.

import (
	"errors"
	"fmt"
	"math"
	"os"
	"runtime"
	"strings"
	"sync"
	"syscall"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
	"golang.org/x/sys/windows/registry"

	"btophub/internal/proto"
)

// Counter paths, English names (PdhAddEnglishCounterW).
const (
	winCtrFreq      = `\Processor Information(_Total)\Processor Frequency`
	winCtrPerf      = `\Processor Information(_Total)\% Processor Performance`
	winCtrMemFree   = `\Memory\Free & Zero Page List Bytes`
	winCtrDiskRead  = `\PhysicalDisk(_Total)\Disk Read Bytes/sec`
	winCtrDiskWrite = `\PhysicalDisk(_Total)\Disk Write Bytes/sec`
	winCtrGPUEng    = `\GPU Engine(*)\Utilization Percentage`
	winCtrGPUMem    = `\GPU Adapter Memory(*)\Dedicated Usage`
	winCtrThermal   = `\Thermal Zone Information(*)\Temperature`
)

// Report.Sources values for the healthy case; constants, so a normal Collect allocates
// nothing for them.
const (
	winSrcCPUFreq  = "PDH " + winCtrFreq + " × % Processor Performance / 100"
	winSrcTempATK  = "ASUS ATK ACPI device (" + winATKPath + ", DSTS 0x00120094): the firmware's CPU temperature, as Armoury Crate shows it"
	winSrcTempPIO  = "PawnIO driver, AMDFamily17 module: Tctl from SMN 0x00059800 (THM_TCON_CUR_TMP)"
	winSrcMemUsed  = "GlobalMemoryStatusEx (total, used = total − available)"
	winSrcMemFree  = "PDH " + winCtrMemFree + " (free)"
	winSrcDiskIO   = `PDH \PhysicalDisk(_Total)\Disk Read Bytes/sec + Disk Write Bytes/sec`
	winSrcBattery  = "GetSystemPowerStatus"
	winSrcUptime   = "GetTickCount64"
	winSrcGPUUtil  = "util: PDH " + winCtrGPUEng + " (summed over processes per engine; busiest engine)"
	winSrcGPUMem   = "memory: PDH " + winCtrGPUMem
	winInfoEvery   = 9 * time.Second // Info() comes about every 10 s; 9 s keeps jitter from skipping one
	winDiskEvery   = 9500 * time.Millisecond
	winRescanEvery = time.Minute
)

type winCollector struct {
	mu     sync.Mutex // Info, Collect, Report and Close may come from different goroutines
	closed bool
	timer  Timer

	sources   map[string]string
	parts     map[string][]string // components of composite sources ("mem", "gpu0")
	notes     map[string]string
	noteOrder []string
	initTimes []string

	info      proto.Info // static parts; Info() adds Net, Battery and BootTime
	pageSize  uint64
	bootMs    int64
	netEvalAt time.Time

	// exports behind the x/sys wrappers, checked once (see winHave)
	haveNtQSI      bool // NtQuerySystemInformation
	haveTick       bool // GetTickCount64
	haveNet        bool // GetIpForwardTable2, FreeMibTable and GetIfEntry2Ex or GetIfEntry2
	haveIfEntry2Ex bool
	haveBest       bool // GetBestInterfaceEx
	haveAddrs      bool // GetAdaptersAddresses
	haveIPIf       bool // GetIpInterfaceEntry
	haveDiskFree   bool // GetDiskFreeSpaceExW
	haveIoctl      bool // CreateFileW, DeviceIoControl, CloseHandle

	// CPU
	sppi      []winSPPI
	sppiRet   uint32
	cpuCur    []winCPUTimes
	cpuPrev   []winCPUTimes
	cpuPrevOK bool
	cpuSource string

	// memory, commit, page file
	haveGMSE  bool
	msx       winMemoryStatusEx
	perfProc  *windows.LazyProc
	srcCached string
	srcCommit string
	perf      winPerfInfo
	pfBuf     []byte
	pfRet     uint32
	swapFiles int // -1 until the next successful read rebuilds the source text

	// PDH: one shared query, collected once per Collect
	pdh            *winPdhQuery
	pdhOK          bool
	pdhCollections int
	ctrFreq        uintptr
	ctrPerf        uintptr
	ctrMemFree     uintptr
	ctrDiskRead    uintptr
	ctrDiskWrite   uintptr

	// CPU temperature when ADL does not report it: the ASUS ATK device, or PawnIO
	cpuVendor string // registry VendorIdentifier
	cpuIdent  string // registry Identifier
	atk       *winATK
	pio       *winPawnIO

	// ACPI thermal zones: a separate query, collected only for Report
	therm    *winPdhQuery
	ctrTherm uintptr
	thermBuf []byte

	// GPUs
	nv               *Nvidia
	adl              *winADL
	gpus             []winGPU // DXGI path only
	ctrGPUEng        uintptr
	ctrGPUMem        uintptr
	gpuEngBuf        []byte
	gpuMemBuf        []byte
	engAcc           winEngineAcc
	memAcc           winMemAcc
	gpuEngSeen       map[winLUID]bool
	gpuMemSeen       map[winLUID]bool
	gpuMissing       int
	gpuRescan        bool
	gpuRescanAt      time.Time
	gpuBadNamesNoted bool
	gpuLUIDsNoted    bool

	// network
	net        winNet
	ifRow      windows.MibIfRow2 // per-sample GetIfEntry2Ex output
	evalRow    windows.MibIfRow2 // chooseNet's GetIfEntry2Ex output
	ipifRow    windows.MibIpInterfaceRow
	routeTable *windows.MibIpForwardTable2
	bestSA     windows.SockaddrInet4
	bestIdx    uint32
	aaBuf      []byte
	aaSize     uint32

	// disk
	sysDrive       string // "C:"
	sysRoot        *uint16
	dsAvail        uint64
	dsTotal        uint64
	dsFree         uint64
	diskOK         bool
	diskUsed       int64
	diskTotal      int64
	diskSpaceAt    time.Time
	diskModelDone  bool
	diskModelTries int
	ioBuf          []byte
	ioQuery        [12]byte
	ioRet          uint32

	// battery
	havePower bool
	sps       winPowerStatus
	battery   bool
	batFlag   int // last BatteryFlag; -1 forces the source text to be rebuilt
}

// New returns the Windows collector. It does not fail because a source is missing: such
// metrics stay nil and Report says why.
func New() (Collector, error) {
	c := &winCollector{
		sources:   map[string]string{},
		parts:     map[string][]string{},
		notes:     map[string]string{},
		info:      proto.Info{OS: "windows", SwapLabel: "page file", HasTemp: false},
		swapFiles: -1,
		batFlag:   -1,
	}
	c.bestSA.Addr = [4]byte{1, 1, 1, 1}
	c.haveTick = winHave(winModKernel32, "GetTickCount64") == nil
	c.haveNtQSI = winHave(winModNtdll, "NtQuerySystemInformation") == nil
	t := time.Now()
	// Each step is guarded: a panic in one becomes "none: ..." for its sources.
	c.guard("initStatic", nil, c.initStatic)
	c.guard("initCPU", func(why string) { c.setSource("cpu", why) }, c.initCPU)
	c.guard("initMem", func(why string) {
		c.part("mem", 0, "used: "+why)
		c.setSource("commit", why)
		c.setSource("swap", why)
	}, c.initMem)
	c.guard("initPDH", func(why string) { // before initGPUs: GPU counters join the shared query
		c.setSource("cpu_freq", why)
		c.setSource("disk_io", why)
		c.part("mem", 2, "free: "+why)
	}, c.initPDH)
	c.guard("initThermal", nil, c.initThermal)
	c.initTimes = append(c.initTimes, fmt.Sprintf("system+PDH %.1f ms", winMs(time.Since(t))))
	c.guard("initGPUs", func(why string) {
		c.nv, c.adl, c.gpus, c.info.GPUs = nil, nil, nil, nil // keep Info and Sample aligned: no GPUs
		c.setSource("gpu0", why)
	}, c.initGPUs)
	c.guard("initNet", func(why string) {
		c.net = winNet{}
		c.setSource("net", why)
	}, c.initNet)
	c.guard("initDisk", func(why string) { c.setSource("disk_space", why) }, c.initDisk)
	c.guard("diskModel", nil, c.refreshDiskModel) // first try here so --probe notes show it
	c.guard("initPower", func(why string) { c.setSource("battery", why) }, c.initPower)
	c.guard("initCPUTemp", func(why string) { // after initGPUs (ADL) and initDisk (haveIoctl)
		if c.atk != nil {
			c.atk.close()
			c.atk, c.info.HasTemp = nil, false
		}
		if c.pio != nil {
			c.pio.close()
			c.pio, c.info.HasTemp = nil, false
		}
		if !c.info.HasTemp {
			c.setSource("cpu_temp", why)
		}
	}, c.initCPUTemp)
	if c.haveTick {
		c.setSource("uptime", winSrcUptime)
	} else {
		c.setSource("uptime", "none: GetTickCount64 not found")
	}
	c.updateBoot(time.Now())
	c.note("init", "init: "+strings.Join(c.initTimes, ", "))
	return c, nil
}

func winMs(d time.Duration) float64 { return float64(d.Microseconds()) / 1000 }

// guard runs one init or Info-refresh step. A panic in it (an export the winHave checks
// did not foresee, a malformed reply) is recovered: fail turns the step's sources into
// "none: ...", a note records it, and the agent keeps running without that source.
// Per-sample steps are contained by Timer.Measure instead.
func (c *winCollector) guard(step string, fail func(why string), f func()) {
	defer func() {
		if r := recover(); r != nil {
			if fail != nil {
				fail(fmt.Sprintf("none: %s panicked: %v", step, r))
			}
			c.note("panic:"+step, fmt.Sprintf("panic in %s (recovered): %v", step, r))
		}
	}()
	f()
}

// ---------------------------------------------------------------------------------------
// Collector interface

func (c *winCollector) Info() proto.Info {
	c.mu.Lock()
	defer c.mu.Unlock()
	now := time.Now()
	if !c.closed {
		if d := now.Sub(c.netEvalAt); c.haveNet && (d >= winInfoEvery || d < 0) {
			c.timer.Measure("info_net", func() {
				c.guard("chooseNet", func(why string) {
					if !c.net.ok { // otherwise keep following the previous interface
						c.setSource("net", why)
					}
				}, c.chooseNet)
			})
		}
		if !c.diskModelDone && c.diskModelTries < 3 {
			c.timer.Measure("info_disk_model", func() { c.guard("diskModel", nil, c.refreshDiskModel) })
		}
		if c.gpuRescan {
			if d := now.Sub(c.gpuRescanAt); c.gpuRescanAt.IsZero() || d >= winRescanEvery || d < 0 {
				c.timer.Measure("info_dxgi_rescan", func() { c.guard("rescanGPUs", nil, c.rescanGPUs) })
			}
		}
		c.guard("updateBoot", nil, func() { c.updateBoot(now) })
	}
	info := c.info
	info.GPUs = append([]proto.GPUInfo(nil), c.info.GPUs...)
	info.Net = c.net.info
	info.Battery = c.battery
	info.BootTime = c.bootMs
	return info
}

func (c *winCollector) Collect(now time.Time) proto.Sample {
	c.mu.Lock()
	defer c.mu.Unlock()
	s := proto.Sample{T: now.UnixMilli()}
	if !c.closed {
		c.timer.Measure("collect_total", func() { c.collect(&s, now) })
	}
	return s
}

func (c *winCollector) collect(s *proto.Sample, now time.Time) {
	if c.pdh != nil {
		c.timer.Measure("pdh_collect", c.collectPDH)
	}
	c.timer.Measure("cpu", func() { c.sampleCPU(s) })
	if c.pdhOK {
		c.timer.Measure("pdh_read", func() { c.samplePDH(s) })
	}
	c.timer.Measure("mem", func() { c.sampleMem(s) })
	c.timer.Measure("perfinfo", func() { c.samplePerfInfo(s) })
	c.timer.Measure("swap", func() { c.sampleSwap(s) })
	c.timer.Measure("net", func() { c.sampleNet(s, now) })
	c.timer.Measure("battery", func() { c.sampleBattery(s) })
	if d := now.Sub(c.diskSpaceAt); c.diskSpaceAt.IsZero() || d >= winDiskEvery || d < 0 {
		c.timer.Measure("disk_space", func() { c.readDiskSpace(now) })
	}
	if c.diskOK {
		s.DiskUsed, s.DiskTotal = proto.I64(c.diskUsed), proto.I64(c.diskTotal)
	}
	if c.haveTick {
		c.timer.Measure("uptime", func() { s.Uptime = proto.I64(int64(windows.DurationSinceBoot() / time.Second)) })
	}
	switch {
	case c.nv != nil:
		c.timer.Measure("nvml", func() { s.GPU = c.nv.Sample() })
		if len(s.GPU) > proto.MaxGPUs {
			s.GPU = s.GPU[:proto.MaxGPUs]
		}
	case c.adl != nil:
		c.timer.Measure("adl", func() { s.GPU, s.CPUTemp = c.adl.sample() })
	case len(c.gpus) > 0:
		c.timer.Measure("gpu_pdh", func() { s.GPU = c.sampleGPUs() })
	}
	if c.atk != nil || c.pio != nil {
		c.timer.Measure("cpu_temp", func() { c.sampleCPUTemp(s) })
	}
}

func (c *winCollector) Report() Report {
	c.mu.Lock()
	defer c.mu.Unlock()
	if !c.closed && c.therm != nil {
		c.timer.Measure("report_thermal", c.readThermal)
	}
	src := make(map[string]string, len(c.sources))
	for k, v := range c.sources {
		src[k] = v
	}
	notes := make([]string, 0, len(c.noteOrder))
	for _, k := range c.noteOrder {
		notes = append(notes, c.notes[k])
	}
	notes = append(notes, c.timer.Panics()...) // per-sample steps contained by Measure
	return Report{Sources: src, Timings: c.timer.Snapshot(), Notes: notes}
}

func (c *winCollector) Close() {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		return
	}
	c.closed = true
	if c.pdh != nil {
		c.pdh.close()
	}
	if c.therm != nil {
		c.therm.close()
	}
	if c.nv != nil {
		c.nv.Close()
	}
	if c.adl != nil {
		c.adl.close()
	}
	if c.atk != nil {
		c.atk.close()
	}
	if c.pio != nil {
		c.pio.close()
	}
}

// ---------------------------------------------------------------------------------------
// Report bookkeeping

func (c *winCollector) setSource(key, v string) {
	if c.sources[key] != v {
		c.sources[key] = v
	}
}

// part sets component i of a composite source and rebuilds its text only when it changed.
func (c *winCollector) part(key string, i int, v string) {
	p := c.parts[key]
	if i < len(p) && p[i] == v {
		return
	}
	for len(p) <= i {
		p = append(p, "")
	}
	p[i] = v
	c.parts[key] = p
	var b strings.Builder
	for _, s := range p {
		if s == "" {
			continue
		}
		if b.Len() > 0 {
			b.WriteString("; ")
		}
		b.WriteString(s)
	}
	c.sources[key] = b.String()
}

// note sets the note under key, keeping first-insertion order.
func (c *winCollector) note(key, text string) {
	if _, ok := c.notes[key]; !ok {
		c.noteOrder = append(c.noteOrder, key)
	}
	c.notes[key] = text
}

func (c *winCollector) dropNote(key string) {
	if _, ok := c.notes[key]; !ok {
		return
	}
	delete(c.notes, key)
	for i, k := range c.noteOrder {
		if k == key {
			c.noteOrder = append(c.noteOrder[:i], c.noteOrder[i+1:]...)
			break
		}
	}
}

// ---------------------------------------------------------------------------------------
// Static machine description

func (c *winCollector) initStatic() {
	if h, err := os.Hostname(); err == nil {
		c.info.Hostname = winTrunc(h, proto.MaxStringLen)
	} else {
		c.note("hostname", "hostname: "+err.Error())
	}

	var product, display string
	if k, err := registry.OpenKey(registry.LOCAL_MACHINE, `SOFTWARE\Microsoft\Windows NT\CurrentVersion`, registry.QUERY_VALUE); err == nil {
		var perr error
		product, _, perr = k.GetStringValue("ProductName")
		display, _, _ = k.GetStringValue("DisplayVersion") // absent before Windows 10 20H2
		_ = k.Close()
		if perr != nil {
			c.note("os_name", "os: ProductName: "+perr.Error())
		}
	} else {
		c.note("os_name", "os: registry: "+err.Error())
	}
	var v windows.OsVersionInfoEx
	if err := winHave(winModNtdll, "RtlGetVersion"); err == nil {
		v = *windows.RtlGetVersion()
	} else {
		c.note("os_build", "os: RtlGetVersion: "+err.Error())
	}
	c.info.OSName = winTrunc(winOSName(product, display, v.BuildNumber), proto.MaxStringLen)
	c.note("os", fmt.Sprintf("os: ProductName %q, DisplayVersion %q, RtlGetVersion %d.%d.%d -> %q",
		product, display, v.MajorVersion, v.MinorVersion, v.BuildNumber, c.info.OSName))

	k, err := registry.OpenKey(registry.LOCAL_MACHINE, `HARDWARE\DESCRIPTION\System\CentralProcessor\0`, registry.QUERY_VALUE)
	if err != nil {
		c.note("cpu_model", "cpu model: registry: "+err.Error())
		return
	}
	c.cpuVendor, _, _ = k.GetStringValue("VendorIdentifier") // "AuthenticAMD"
	c.cpuIdent, _, _ = k.GetStringValue("Identifier")        // "AMD64 Family 26 Model 68 Stepping 0"
	name, _, err := k.GetStringValue("ProcessorNameString")
	_ = k.Close()
	if err != nil {
		c.note("cpu_model", "cpu model: ProcessorNameString: "+err.Error())
		return
	}
	c.info.CPUModel = winTrunc(CleanCPUName(name), proto.MaxStringLen)
	c.note("cpu_model", fmt.Sprintf("cpu model: ProcessorNameString %q -> %q", name, c.info.CPUModel))
}

func (c *winCollector) updateBoot(now time.Time) {
	if !c.haveTick {
		return
	}
	b := now.UnixMilli() - windows.DurationSinceBoot().Milliseconds()
	// Keep the value stable across calls; only a real change (clock step) replaces it.
	if c.bootMs == 0 || b-c.bootMs > 2000 || c.bootMs-b > 2000 {
		c.bootMs = b
	}
}

// ---------------------------------------------------------------------------------------
// CPU

func (c *winCollector) initCPU() {
	n := 0
	if winHave(winModKernel32, "GetActiveProcessorCount") == nil {
		n = int(windows.GetActiveProcessorCount(windows.ALL_PROCESSOR_GROUPS))
	}
	if n < 1 || n > 4096 {
		n = runtime.NumCPU()
	}
	if n < 1 {
		n = 1
	}
	c.sppi = make([]winSPPI, n)
	if k, err := c.readSPPI(); err != nil {
		c.setSource("cpu", "none: NtQuerySystemInformation(SystemProcessorPerformanceInformation): "+err.Error())
	} else {
		c.info.Threads = k
		c.cpuSource = fmt.Sprintf("NtQuerySystemInformation(SystemProcessorPerformanceInformation), %d logical processors", k)
		c.setSource("cpu", c.cpuSource)
		if k != n {
			c.note("cpu_groups", fmt.Sprintf("cpu: %d logical processors are active but the call covers %d (one processor group)", n, k))
		}
	}
	if cores, err := winCoreCount(); err != nil {
		c.note("cpu_cores", "cpu cores: GetLogicalProcessorInformationEx: "+err.Error())
	} else {
		c.info.Cores = cores
	}
}

// readSPPI fills c.sppi and returns the number of processors reported.
func (c *winCollector) readSPPI() (int, error) {
	const size = uint32(unsafe.Sizeof(winSPPI{}))
	if !c.haveNtQSI {
		return 0, errors.New("ntdll has no NtQuerySystemInformation")
	}
	for attempt := 0; attempt < 4; attempt++ {
		c.sppiRet = 0
		err := windows.NtQuerySystemInformation(windows.SystemProcessorPerformanceInformation,
			unsafe.Pointer(&c.sppi[0]), uint32(len(c.sppi))*size, &c.sppiRet)
		if err == windows.STATUS_INFO_LENGTH_MISMATCH && len(c.sppi) < 4096 {
			c.sppi = make([]winSPPI, 2*len(c.sppi))
			continue
		}
		if err != nil {
			return 0, err
		}
		n := int(c.sppiRet / size)
		if n < 1 || n > len(c.sppi) {
			return 0, fmt.Errorf("unexpected return length %d", c.sppiRet)
		}
		return n, nil
	}
	return 0, fmt.Errorf("buffer of %d entries still too small", len(c.sppi))
}

func (c *winCollector) sampleCPU(s *proto.Sample) {
	n, err := c.readSPPI()
	if err != nil {
		c.cpuPrevOK = false
		c.setSource("cpu", "none: NtQuerySystemInformation(SystemProcessorPerformanceInformation): "+err.Error())
		return
	}
	cur := c.cpuCur[:0]
	for i := 0; i < n; i++ {
		p := &c.sppi[i]
		cur = append(cur, winCPUTimes{Idle: p.IdleTime, Kernel: p.KernelTime, User: p.UserTime})
	}
	if c.cpuPrevOK {
		s.CPU = winCPUBusy(c.cpuPrev, cur) // nil when the processor count changed
	}
	c.cpuPrev, c.cpuCur, c.cpuPrevOK = cur, c.cpuPrev, true
	if n != c.info.Threads {
		c.info.Threads = n
		c.cpuSource = fmt.Sprintf("NtQuerySystemInformation(SystemProcessorPerformanceInformation), %d logical processors", n)
	}
	c.setSource("cpu", c.cpuSource)
}

// winCoreCount counts physical cores with GetLogicalProcessorInformationEx(RelationProcessorCore),
// which covers every processor group.
func winCoreCount() (int, error) {
	p := winProcGetLogicalProcessorInformationEx
	if err := p.Find(); err != nil {
		return 0, err
	}
	var n uint32
	r, _, e := syscall.SyscallN(p.Addr(), 0, 0, uintptr(unsafe.Pointer(&n)))
	if r != 0 || e != windows.ERROR_INSUFFICIENT_BUFFER || n == 0 || n > 1<<22 {
		return 0, fmt.Errorf("size query: %v (%d bytes)", e, n)
	}
	buf := make([]byte, n)
	r, _, e = syscall.SyscallN(p.Addr(), 0, uintptr(unsafe.Pointer(&buf[0])), uintptr(unsafe.Pointer(&n)))
	if r == 0 {
		return 0, e
	}
	if int(n) > len(buf) {
		return 0, fmt.Errorf("returned %d bytes into %d", n, len(buf))
	}
	return winCountCores(buf[:n])
}

// initCPUTemp finds the CPU temperature when AMD's driver library has not (it reports one only
// on some APUs). Windows itself has none: ASUS laptops give it through their firmware's ATK
// device, and otherwise it takes a kernel driver: PawnIO, for AMD Ryzen CPUs.
func (c *winCollector) initCPUTemp() {
	if c.info.HasTemp {
		return // ADL TEMPERATURE_CPU, see initGPUs
	}
	var why []string
	if c.adl != nil {
		why = append(why, "AMD ADL reports no CPU temperature for this GPU")
	}
	if !c.haveIoctl {
		why = append(why, "kernel32 lacks CreateFileW/DeviceIoControl")
		c.setSource("cpu_temp", "none: "+strings.Join(why, "; "))
		return
	}
	a, raw, w := winOpenATK()
	if a != nil {
		t, _ := winASUSTemp(raw)
		c.atk, c.info.HasTemp = a, true
		c.setSource("cpu_temp", winSrcTempATK)
		c.note("cpu_temp", fmt.Sprintf("cpu temperature: ASUS ATK DSTS 0x%08X = 0x%08X (%.0f °C)", winATKDevCPUTemp, raw, t))
		return
	}
	why = append(why, w)
	if !winAMDZen(c.cpuVendor, c.cpuIdent) {
		why = append(why, fmt.Sprintf("PawnIO is used for AMD family 17h-1Ah CPUs only, and this is %q %q", c.cpuVendor, c.cpuIdent))
		c.setSource("cpu_temp", "none: "+strings.Join(why, "; "))
		return
	}
	p, raw, w := winOpenPawnIO()
	if p == nil {
		why = append(why, w)
		c.setSource("cpu_temp", "none: "+strings.Join(why, "; "))
		return
	}
	t, _ := winZenTctl(raw)
	c.pio, c.info.HasTemp = p, true
	c.setSource("cpu_temp", winSrcTempPIO)
	c.note("cpu_temp", fmt.Sprintf("cpu temperature: PawnIO %s, AMDFamily17 module (PawnIO.Modules 0.2.11), Tctl register 0x%08X = %.3f °C, Access_PCI mutex %s",
		p.version, raw, t, p.mutexBy))
}

func (c *winCollector) sampleCPUTemp(s *proto.Sample) {
	if c.atk != nil {
		raw, err := c.atk.read()
		if err != nil {
			c.setSource("cpu_temp", "none: ASUS ATK device: DSTS: "+err.Error())
			return
		}
		t, ok := winASUSTemp(raw)
		if !ok {
			c.setSource("cpu_temp", fmt.Sprintf("none: the ASUS ATK device answered 0x%08X", raw))
			return
		}
		s.CPUTemp = proto.F64(t)
		c.setSource("cpu_temp", winSrcTempATK)
		return
	}
	raw, err := c.pio.readTctl()
	if err != nil {
		c.setSource("cpu_temp", "none: PawnIO: "+err.Error())
		return
	}
	t, ok := winZenTctl(raw)
	if !ok {
		c.setSource("cpu_temp", fmt.Sprintf("none: PawnIO: the Tctl register reads 0x%08X", raw))
		return
	}
	s.CPUTemp = proto.F64(t)
	c.setSource("cpu_temp", winSrcTempPIO)
}

// ---------------------------------------------------------------------------------------
// Memory, commit, page file

func (c *winCollector) initMem() {
	c.pageSize = uint64(os.Getpagesize()) // replaced by PERFORMANCE_INFORMATION.PageSize below
	c.pfBuf = make([]byte, 4096)
	if err := winProcGlobalMemoryStatusEx.Find(); err != nil {
		c.part("mem", 0, "used: none: GlobalMemoryStatusEx: "+err.Error())
	} else {
		c.haveGMSE = true
		var s proto.Sample
		c.sampleMem(&s) // MemTotal for the first Info()
	}
	name := ""
	switch {
	case winProcK32GetPerformanceInfo.Find() == nil:
		c.perfProc, name = winProcK32GetPerformanceInfo, "K32GetPerformanceInfo"
	case winProcGetPerformanceInfo.Find() == nil:
		c.perfProc, name = winProcGetPerformanceInfo, "GetPerformanceInfo (psapi)"
	}
	if c.perfProc == nil {
		c.part("mem", 1, "cached: none: K32GetPerformanceInfo not found")
		c.setSource("commit", "none: K32GetPerformanceInfo not found")
	} else {
		c.srcCached = name + " SystemCache × PageSize (cached)"
		c.srcCommit = name + " CommitTotal, CommitLimit × PageSize"
		var s proto.Sample
		c.samplePerfInfo(&s) // learns the page size
	}
	c.setSource("swap", "NtQuerySystemInformation(SystemPageFileInformation)")
}

func (c *winCollector) sampleMem(s *proto.Sample) {
	if !c.haveGMSE {
		return
	}
	c.msx = winMemoryStatusEx{Length: uint32(unsafe.Sizeof(winMemoryStatusEx{}))}
	r, _, e := syscall.SyscallN(winProcGlobalMemoryStatusEx.Addr(), uintptr(unsafe.Pointer(&c.msx)))
	switch {
	case r == 0:
		c.part("mem", 0, "used: none: GlobalMemoryStatusEx: "+e.Error())
	case c.msx.TotalPhys == 0 || c.msx.AvailPhys > c.msx.TotalPhys:
		c.part("mem", 0, "used: none: GlobalMemoryStatusEx returned inconsistent values")
	default:
		c.info.MemTotal = winI64(c.msx.TotalPhys)
		s.MemUsed = proto.I64(winI64(c.msx.TotalPhys - c.msx.AvailPhys))
		c.part("mem", 0, winSrcMemUsed)
	}
}

func (c *winCollector) samplePerfInfo(s *proto.Sample) {
	if c.perfProc == nil {
		return
	}
	c.perf = winPerfInfo{Cb: uint32(unsafe.Sizeof(winPerfInfo{}))}
	r, _, e := syscall.SyscallN(c.perfProc.Addr(), uintptr(unsafe.Pointer(&c.perf)), uintptr(unsafe.Sizeof(winPerfInfo{})))
	if r == 0 {
		c.part("mem", 1, "cached: none: "+e.Error())
		c.setSource("commit", "none: "+e.Error())
		return
	}
	if ps := uint64(c.perf.PageSize); ps >= 4096 && ps <= 1<<21 && ps&(ps-1) == 0 {
		c.pageSize = ps
	}
	ps := c.pageSize
	s.MemCached = proto.I64(winI64(uint64(c.perf.SystemCache) * ps))
	c.part("mem", 1, c.srcCached)
	if c.perf.CommitLimit == 0 || c.perf.CommitTotal > c.perf.CommitLimit*2 {
		c.setSource("commit", "none: PERFORMANCE_INFORMATION has inconsistent commit values")
		return
	}
	s.CommitUsed = proto.I64(winI64(uint64(c.perf.CommitTotal) * ps))
	s.CommitLimit = proto.I64(winI64(uint64(c.perf.CommitLimit) * ps))
	c.setSource("commit", c.srcCommit)
}

func (c *winCollector) sampleSwap(s *proto.Sample) {
	if !c.haveNtQSI {
		c.setSource("swap", "none: ntdll has no NtQuerySystemInformation")
		return
	}
	for attempt := 0; attempt < 4; attempt++ {
		c.pfRet = 0
		err := windows.NtQuerySystemInformation(windows.SystemPageFileInformation,
			unsafe.Pointer(&c.pfBuf[0]), uint32(len(c.pfBuf)), &c.pfRet)
		if (err == windows.STATUS_INFO_LENGTH_MISMATCH || err == windows.STATUS_BUFFER_TOO_SMALL) && len(c.pfBuf) < 1<<20 {
			c.pfBuf = make([]byte, 2*len(c.pfBuf))
			continue
		}
		if err != nil {
			c.swapFiles = -1
			c.setSource("swap", "none: NtQuerySystemInformation(SystemPageFileInformation): "+err.Error())
			return
		}
		if int(c.pfRet) > len(c.pfBuf) {
			c.swapFiles = -1
			c.setSource("swap", fmt.Sprintf("none: SystemPageFileInformation returned %d bytes into %d", c.pfRet, len(c.pfBuf)))
			return
		}
		total, used, files, perr := winSumPageFiles(c.pfBuf[:c.pfRet])
		if perr != nil {
			c.swapFiles = -1
			c.setSource("swap", "none: SystemPageFileInformation: "+perr.Error())
			return
		}
		// No page file gives 0 and 0: a real reading.
		s.SwapTotal = proto.I64(winI64(total * c.pageSize))
		s.SwapUsed = proto.I64(winI64(used * c.pageSize))
		if files != c.swapFiles {
			c.swapFiles = files
			c.setSource("swap", fmt.Sprintf("NtQuerySystemInformation(SystemPageFileInformation): %d page file(s), %d-byte pages", files, c.pageSize))
		}
		return
	}
	c.swapFiles = -1
	c.setSource("swap", "none: SystemPageFileInformation needs more than 1 MiB")
}

// ---------------------------------------------------------------------------------------
// PDH

func (c *winCollector) initPDH() {
	q, err := winPdhOpen()
	if err != nil {
		why := "none: " + err.Error()
		c.setSource("cpu_freq", why)
		c.setSource("disk_io", why)
		c.part("mem", 2, "free: "+why)
		return
	}
	c.pdh = q
	add := func(path string) (uintptr, string) {
		h, err := q.add(path)
		if err != nil {
			return 0, "none: PDH " + path + ": " + err.Error()
		}
		return h, ""
	}
	pair := func(key, src, a, b string) (uintptr, uintptr) {
		ha, why := add(a)
		if why == "" {
			var hb uintptr
			if hb, why = add(b); why == "" {
				c.setSource(key, src)
				return ha, hb
			}
			q.remove(ha)
		}
		c.setSource(key, why)
		return 0, 0
	}
	c.ctrFreq, c.ctrPerf = pair("cpu_freq", winSrcCPUFreq, winCtrFreq, winCtrPerf)
	c.ctrDiskRead, c.ctrDiskWrite = pair("disk_io", winSrcDiskIO, winCtrDiskRead, winCtrDiskWrite)
	var why string
	if c.ctrMemFree, why = add(winCtrMemFree); why != "" {
		c.part("mem", 2, "free: "+why)
	} else {
		c.part("mem", 2, winSrcMemFree)
	}
}

func (c *winCollector) collectPDH() {
	c.pdhOK = false // stays false if collect panics, so no stale values are read
	err := c.pdh.collect()
	if err == nil {
		c.pdhOK = true
		c.pdhCollections++
		return
	}
	c.pdhOK = false
	why := "none: PdhCollectQueryData: " + err.Error()
	if c.ctrFreq != 0 {
		c.setSource("cpu_freq", why)
	}
	if c.ctrDiskRead != 0 {
		c.setSource("disk_io", why)
	}
	if c.ctrMemFree != 0 {
		c.part("mem", 2, "free: "+why)
	}
	c.gpuPartAll(1, "util: "+why)
	c.gpuPartAll(2, "memory: "+why)
}

// samplePDH formats the single-instance counters. Rate counters have no value after the
// first collection; until the second one the init-time source text stays.
func (c *winCollector) samplePDH(s *proto.Sample) {
	warm := c.pdhCollections >= 2
	fail := func(err error) bool { return warm || !winPdhNotReadyErr(err) }
	if c.ctrFreq != 0 {
		f, err := c.pdh.value(c.ctrFreq)
		perf := 0.0
		if err == nil {
			perf, err = c.pdh.value(c.ctrPerf)
		}
		switch {
		case err != nil:
			if fail(err) {
				c.setSource("cpu_freq", "none: PDH Processor Information: "+err.Error())
			}
		case f > 0 && perf > 0:
			s.CPUFreq = proto.F64(f * perf / 100)
			c.setSource("cpu_freq", winSrcCPUFreq)
		default:
			c.setSource("cpu_freq", "none: Processor Frequency or % Processor Performance reads 0")
		}
	}
	if c.ctrMemFree != 0 {
		if v, err := c.pdh.value(c.ctrMemFree); err != nil {
			if fail(err) {
				c.part("mem", 2, "free: none: PDH: "+err.Error())
			}
		} else if v >= 0 && v < math.MaxInt64 {
			s.MemFree = proto.I64(int64(v))
			c.part("mem", 2, winSrcMemFree)
		}
	}
	if c.ctrDiskRead != 0 {
		r, err := c.pdh.value(c.ctrDiskRead)
		w := 0.0
		if err == nil {
			w, err = c.pdh.value(c.ctrDiskWrite)
		}
		switch {
		case err != nil:
			if fail(err) {
				c.setSource("disk_io", "none: PDH PhysicalDisk: "+err.Error())
			}
		case r >= 0 && w >= 0 && r < math.MaxInt64 && w < math.MaxInt64:
			s.DiskRead, s.DiskWrite = proto.I64(int64(r)), proto.I64(int64(w))
			c.setSource("disk_io", winSrcDiskIO)
		}
	}
}

// initThermal opens a separate query for ACPI thermal zones. It is collected only when a
// Report is made, so the sampling loop never pays for it.
func (c *winCollector) initThermal() {
	q, err := winPdhOpen()
	if err != nil {
		c.note("thermal", "thermal zones: "+err.Error())
		return
	}
	h, err := q.add(winCtrThermal)
	if err != nil {
		q.close()
		c.note("thermal", "thermal zones: PDH "+winCtrThermal+": "+err.Error())
		return
	}
	c.therm, c.ctrTherm = q, h
}

// readThermal puts each ACPI thermal zone's temperature into Notes. The counter reports
// Kelvin. These zones are firmware sensors (often a fixed value) and are never reported as
// CPU temperature.
func (c *winCollector) readThermal() {
	if err := c.therm.collect(); err != nil {
		c.note("thermal", "thermal zones: PdhCollectQueryData: "+err.Error())
		return
	}
	items, err := c.therm.array(c.ctrTherm, &c.thermBuf)
	if err != nil {
		c.note("thermal", "thermal zones: "+err.Error())
		return
	}
	if len(items) == 0 {
		c.note("thermal", "thermal zones: none listed")
		return
	}
	c.dropNote("thermal")
	for i := range items {
		it := &items[i]
		name := windows.UTF16ToString(winPdhName(c.thermBuf, it))
		key := "thermal:" + name
		k := it.FmtValue.Value
		switch st := it.FmtValue.CStatus; {
		case st != 0 && st != 1:
			c.note(key, fmt.Sprintf("thermal zone %s: no value (%s)", name, winPdhStatus(st)))
		case k <= 0 || math.IsNaN(k) || math.IsInf(k, 0):
			c.note(key, fmt.Sprintf("thermal zone %s: invalid reading %v K", name, k))
		default:
			c.note(key, fmt.Sprintf("thermal zone %s: %.1f °C (ACPI zone, information only; not used as CPU temperature)", name, k-273.15))
		}
	}
}

// ---------------------------------------------------------------------------------------
// Battery

func (c *winCollector) initPower() {
	if err := winProcGetSystemPowerStatus.Find(); err != nil {
		c.setSource("battery", "none: GetSystemPowerStatus: "+err.Error())
		return
	}
	c.havePower = true
	var s proto.Sample
	c.sampleBattery(&s) // Info.Battery before the first Collect
}

func (c *winCollector) sampleBattery(s *proto.Sample) {
	if !c.havePower {
		return
	}
	r, _, e := syscall.SyscallN(winProcGetSystemPowerStatus.Addr(), uintptr(unsafe.Pointer(&c.sps)))
	if r == 0 {
		c.batFlag = -1
		c.setSource("battery", "none: GetSystemPowerStatus: "+e.Error())
		return
	}
	present, pct, state := winBattery(c.sps.ACLineStatus, c.sps.BatteryFlag, c.sps.BatteryLifePercent)
	c.battery = present
	if f := int(c.sps.BatteryFlag); f != c.batFlag {
		c.batFlag = f
		if present {
			c.setSource("battery", winSrcBattery)
		} else {
			c.setSource("battery", fmt.Sprintf("none: no system battery (GetSystemPowerStatus BatteryFlag %d)", f))
		}
	}
	if present {
		s.Battery, s.BatState = pct, state
	}
}
