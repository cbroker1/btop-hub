//go:build windows && (amd64 || arm64)

package collect

// System DLL procedures that golang.org/x/sys/windows v0.50 does not wrap, the structures
// they fill, and compile-time layout checks for every structure the collector reads,
// including the x/sys ones. A wrong layout fails the build instead of reading garbage.
// The expected numbers are the 64-bit (LLP64) Windows SDK layouts; amd64 and arm64 agree.

import (
	"unsafe"

	"golang.org/x/sys/windows"
)

var (
	winModKernel32 = windows.NewLazySystemDLL("kernel32.dll")
	winModNtdll    = windows.NewLazySystemDLL("ntdll.dll")
	winModIphlpapi = windows.NewLazySystemDLL("iphlpapi.dll")
	winModPsapi    = windows.NewLazySystemDLL("psapi.dll")

	winProcGlobalMemoryStatusEx             = winModKernel32.NewProc("GlobalMemoryStatusEx")
	winProcK32GetPerformanceInfo            = winModKernel32.NewProc("K32GetPerformanceInfo")
	winProcGetPerformanceInfo               = winModPsapi.NewProc("GetPerformanceInfo")
	winProcGetSystemPowerStatus             = winModKernel32.NewProc("GetSystemPowerStatus")
	winProcGetLogicalProcessorInformationEx = winModKernel32.NewProc("GetLogicalProcessorInformationEx")
	winProcGetIfEntry2                      = winModIphlpapi.NewProc("GetIfEntry2") // fallback before Windows 10 1703
)

// winHave reports whether every named export of dll exists. The x/sys wrappers panic when
// their export is missing (LazyProc.Addr), so each one the collector calls is checked
// here first. Windows 10/11 has all of them; Wine, for one, lacks some.
func winHave(dll *windows.LazyDLL, names ...string) error {
	for _, n := range names {
		if err := dll.NewProc(n).Find(); err != nil {
			return err
		}
	}
	return nil
}

// winMemoryStatusEx is MEMORYSTATUSEX (sysinfoapi.h).
type winMemoryStatusEx struct {
	Length               uint32
	MemoryLoad           uint32
	TotalPhys            uint64
	AvailPhys            uint64
	TotalPageFile        uint64
	AvailPageFile        uint64
	TotalVirtual         uint64
	AvailVirtual         uint64
	AvailExtendedVirtual uint64
}

// winPerfInfo is PERFORMANCE_INFORMATION (psapi.h): cb, 4 bytes padding, ten SIZE_T
// counts (pages), three DWORD counts, padded to 8.
type winPerfInfo struct {
	Cb                uint32
	CommitTotal       uintptr
	CommitLimit       uintptr
	CommitPeak        uintptr
	PhysicalTotal     uintptr
	PhysicalAvailable uintptr
	SystemCache       uintptr
	KernelTotal       uintptr
	KernelPaged       uintptr
	KernelNonpaged    uintptr
	PageSize          uintptr
	HandleCount       uint32
	ProcessCount      uint32
	ThreadCount       uint32
}

// winPowerStatus is SYSTEM_POWER_STATUS (winbase.h).
type winPowerStatus struct {
	ACLineStatus        uint8
	BatteryFlag         uint8
	BatteryLifePercent  uint8
	SystemStatusFlag    uint8
	BatteryLifeTime     uint32
	BatteryFullLifeTime uint32
}

// winSPPI is SYSTEM_PROCESSOR_PERFORMANCE_INFORMATION (winternl.h; DpcTime, InterruptTime
// and InterruptCount are the SDK's Reserved1[2] and Reserved2). KernelTime includes IdleTime.
type winSPPI struct {
	IdleTime       int64
	KernelTime     int64
	UserTime       int64
	DpcTime        int64
	InterruptTime  int64
	InterruptCount uint32
	_              uint32
}

// winPageFileInfo is SYSTEM_PAGEFILE_INFORMATION (ntexapi.h). It is only used for the
// layout check: winSumPageFiles reads the same offsets from a byte buffer.
type winPageFileInfo struct {
	NextEntryOffset   uint32
	TotalSize         uint32 // pages
	TotalInUse        uint32 // pages
	PeakUsage         uint32 // pages
	NameLength        uint16 // UNICODE_STRING PageFileName
	NameMaximumLength uint16
	_                 uint32
	NameBuffer        uintptr
}

// winSLPIExHeader is the start of SYSTEM_LOGICAL_PROCESSOR_INFORMATION_EX (winnt.h); only
// for the layout check of winCountCores.
type winSLPIExHeader struct {
	Relationship uint32
	Size         uint32
}

// Layout checks. Each line fails to compile unless the left size equals the right one.
var (
	_ [64]byte  = [unsafe.Sizeof(winMemoryStatusEx{})]byte{}
	_ [8]byte   = [unsafe.Offsetof(winMemoryStatusEx{}.TotalPhys)]byte{}
	_ [16]byte  = [unsafe.Offsetof(winMemoryStatusEx{}.AvailPhys)]byte{}
	_ [104]byte = [unsafe.Sizeof(winPerfInfo{})]byte{}
	_ [8]byte   = [unsafe.Offsetof(winPerfInfo{}.CommitTotal)]byte{}
	_ [16]byte  = [unsafe.Offsetof(winPerfInfo{}.CommitLimit)]byte{}
	_ [48]byte  = [unsafe.Offsetof(winPerfInfo{}.SystemCache)]byte{}
	_ [80]byte  = [unsafe.Offsetof(winPerfInfo{}.PageSize)]byte{}
	_ [88]byte  = [unsafe.Offsetof(winPerfInfo{}.HandleCount)]byte{}
	_ [12]byte  = [unsafe.Sizeof(winPowerStatus{})]byte{}
	_ [4]byte   = [unsafe.Offsetof(winPowerStatus{}.BatteryLifeTime)]byte{}
	_ [48]byte  = [unsafe.Sizeof(winSPPI{})]byte{}
	_ [8]byte   = [unsafe.Offsetof(winSPPI{}.KernelTime)]byte{}
	_ [16]byte  = [unsafe.Offsetof(winSPPI{}.UserTime)]byte{}
	_ [40]byte  = [unsafe.Offsetof(winSPPI{}.InterruptCount)]byte{}

	_ [winPageFileEntry]byte = [unsafe.Sizeof(winPageFileInfo{})]byte{}
	_ [4]byte                = [unsafe.Offsetof(winPageFileInfo{}.TotalSize)]byte{}
	_ [8]byte                = [unsafe.Offsetof(winPageFileInfo{}.TotalInUse)]byte{}
	_ [24]byte               = [unsafe.Offsetof(winPageFileInfo{}.NameBuffer)]byte{}
	_ [8]byte                = [unsafe.Sizeof(winSLPIExHeader{})]byte{}

	// PDH_FMT_COUNTERVALUE and PDH_FMT_COUNTERVALUE_ITEM_W (pdh.h).
	_ [16]byte = [unsafe.Sizeof(winPdhValue{})]byte{}
	_ [8]byte  = [unsafe.Offsetof(winPdhValue{}.Value)]byte{}
	_ [24]byte = [unsafe.Sizeof(winPdhItem{})]byte{}
	_ [8]byte  = [unsafe.Offsetof(winPdhItem{}.FmtValue)]byte{}

	// DXGI_ADAPTER_DESC1 (dxgi.h).
	_ [312]byte = [unsafe.Sizeof(winDXGIAdapterDesc1{})]byte{}
	_ [256]byte = [unsafe.Offsetof(winDXGIAdapterDesc1{}.VendorID)]byte{}
	_ [260]byte = [unsafe.Offsetof(winDXGIAdapterDesc1{}.DeviceID)]byte{}
	_ [264]byte = [unsafe.Offsetof(winDXGIAdapterDesc1{}.SubSysID)]byte{}
	_ [268]byte = [unsafe.Offsetof(winDXGIAdapterDesc1{}.Revision)]byte{}
	_ [272]byte = [unsafe.Offsetof(winDXGIAdapterDesc1{}.DedicatedVideoMemory)]byte{}
	_ [280]byte = [unsafe.Offsetof(winDXGIAdapterDesc1{}.DedicatedSystemMemory)]byte{}
	_ [288]byte = [unsafe.Offsetof(winDXGIAdapterDesc1{}.SharedSystemMemory)]byte{}
	_ [296]byte = [unsafe.Offsetof(winDXGIAdapterDesc1{}.AdapterLuid)]byte{}
	_ [304]byte = [unsafe.Offsetof(winDXGIAdapterDesc1{}.Flags)]byte{}
	_ [8]byte   = [unsafe.Sizeof(windows.LUID{})]byte{}
	_ [16]byte  = [unsafe.Sizeof(windows.GUID{})]byte{}

	// x/sys structures the collector reads: MIB_IF_ROW2 (netioapi.h, 1352 bytes).
	_ [1352]byte = [unsafe.Sizeof(windows.MibIfRow2{})]byte{}
	_ [28]byte   = [unsafe.Offsetof(windows.MibIfRow2{}.Alias)]byte{}
	_ [542]byte  = [unsafe.Offsetof(windows.MibIfRow2{}.Description)]byte{}
	_ [1128]byte = [unsafe.Offsetof(windows.MibIfRow2{}.Type)]byte{}
	_ [1152]byte = [unsafe.Offsetof(windows.MibIfRow2{}.InterfaceAndOperStatusFlags)]byte{}
	_ [1156]byte = [unsafe.Offsetof(windows.MibIfRow2{}.OperStatus)]byte{}
	_ [1200]byte = [unsafe.Offsetof(windows.MibIfRow2{}.ReceiveLinkSpeed)]byte{}
	_ [1208]byte = [unsafe.Offsetof(windows.MibIfRow2{}.InOctets)]byte{}
	_ [1280]byte = [unsafe.Offsetof(windows.MibIfRow2{}.OutOctets)]byte{}

	// MIB_IPFORWARD_ROW2 (104 bytes) inside MIB_IPFORWARD_TABLE2 (rows start at 8).
	_ [104]byte = [unsafe.Sizeof(windows.MibIpForwardRow2{})]byte{}
	_ [12]byte  = [unsafe.Offsetof(windows.MibIpForwardRow2{}.DestinationPrefix)]byte{}
	_ [84]byte  = [unsafe.Offsetof(windows.MibIpForwardRow2{}.Metric)]byte{}
	_ [8]byte   = [unsafe.Offsetof(windows.MibIpForwardTable2{}.Table)]byte{}
	_ [28]byte  = [unsafe.Offsetof(windows.IpAddressPrefix{}.PrefixLength)]byte{}

	// MIB_IPINTERFACE_ROW: x/sys declares the two NL_INTERFACE_OFFLOAD_ROD bytes as
	// uint32, so its struct is 176 bytes where the SDK's is 168. Windows writes 168 bytes
	// into the larger Go struct (safe); Metric, the only field read, is at the SDK offset.
	_ [148]byte = [unsafe.Offsetof(windows.MibIpInterfaceRow{}.Metric)]byte{}
	_ [8]byte   = [unsafe.Offsetof(windows.MibIpInterfaceRow{}.InterfaceLuid)]byte{}
	_           = [unsafe.Sizeof(windows.MibIpInterfaceRow{}) - 168]byte{} // fails if smaller than the SDK's 168

	// IP_ADAPTER_ADDRESSES_LH / IP_ADAPTER_UNICAST_ADDRESS_LH / SOCKET_ADDRESS: the fields walked.
	_ [4]byte  = [unsafe.Offsetof(windows.IpAdapterAddresses{}.IfIndex)]byte{}
	_ [8]byte  = [unsafe.Offsetof(windows.IpAdapterAddresses{}.Next)]byte{}
	_ [24]byte = [unsafe.Offsetof(windows.IpAdapterAddresses{}.FirstUnicastAddress)]byte{}
	_ [8]byte  = [unsafe.Offsetof(windows.IpAdapterUnicastAddress{}.Next)]byte{}
	_ [16]byte = [unsafe.Offsetof(windows.IpAdapterUnicastAddress{}.Address)]byte{}
	_ [16]byte = [unsafe.Sizeof(windows.SocketAddress{})]byte{}
)
