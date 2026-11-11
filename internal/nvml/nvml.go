// Package nvml is a minimal binding to NVIDIA's management library (NVML), loaded at
// runtime: libnvidia-ml.so.1 on Linux (through purego, no cgo) and nvml.dll on Windows.
// Every call is a cheap driver query; nothing here starts a process.
package nvml

import (
	"errors"
	"fmt"
	"strings"
	"sync"
	"unsafe"
)

// Device is an opaque NVML device handle.
type Device uintptr

// Lib is a loaded, initialised NVML.
type Lib struct {
	mu      sync.Mutex
	handle  uintptr
	fns     map[string]uintptr
	scratch *[256]byte // output arguments; heap memory so C never sees a movable stack address
}

var symbols = []string{
	"nvmlInit_v2", "nvmlShutdown", "nvmlDeviceGetCount_v2", "nvmlDeviceGetHandleByIndex_v2",
	"nvmlDeviceGetName", "nvmlDeviceGetUtilizationRates", "nvmlDeviceGetMemoryInfo",
	"nvmlDeviceGetMemoryInfo_v2", "nvmlDeviceGetTemperature", "nvmlDeviceGetPowerUsage",
	"nvmlDeviceGetPerformanceState", "nvmlDeviceGetFanSpeed", "nvmlDeviceGetClockInfo",
	"nvmlDeviceGetPcieThroughput", "nvmlSystemGetDriverVersion",
}

// Open loads and initialises NVML.
func Open() (*Lib, error) {
	h, err := loadLibrary()
	if err != nil {
		return nil, err
	}
	l := &Lib{handle: h, fns: map[string]uintptr{}, scratch: new([256]byte)}
	for _, name := range symbols {
		if p, err := symbol(h, name); err == nil && p != 0 {
			l.fns[name] = p
		}
	}
	for _, need := range []string{"nvmlInit_v2", "nvmlDeviceGetCount_v2", "nvmlDeviceGetHandleByIndex_v2"} {
		if l.fns[need] == 0 {
			return nil, fmt.Errorf("nvml: missing %s", need)
		}
	}
	if err := l.check("nvmlInit_v2"); err != nil {
		return nil, err
	}
	return l, nil
}

// Error is a non-zero nvmlReturn_t.
type Error int32

func (e Error) Error() string {
	switch e {
	case 1:
		return "nvml: not initialized"
	case 2:
		return "nvml: invalid argument"
	case 3:
		return "nvml: not supported"
	case 4:
		return "nvml: no permission"
	case 9:
		return "nvml: driver not loaded"
	case 15:
		return "nvml: gpu is lost"
	}
	return fmt.Sprintf("nvml: error %d", int32(e))
}

var errMissing = errors.New("nvml: function not available")

// check calls name(args...) and turns its nvmlReturn_t into an error. Caller holds no lock;
// this takes l.mu so the scratch buffer is not shared between calls.
func (l *Lib) check(name string, args ...uintptr) error {
	fn := l.fns[name]
	if fn == 0 {
		return errMissing
	}
	if r := int32(call(fn, args...)); r != 0 {
		return Error(r)
	}
	return nil
}

func (l *Lib) out() uintptr { return uintptr(unsafe.Pointer(&l.scratch[0])) }
func (l *Lib) u32() uint32  { return *(*uint32)(unsafe.Pointer(&l.scratch[0])) }
func (l *Lib) clear()       { *l.scratch = [256]byte{} }

func (l *Lib) cstring() string {
	b := l.scratch[:]
	if i := strings.IndexByte(string(b), 0); i >= 0 {
		b = b[:i]
	}
	return string(b)
}

// Count returns the number of GPUs NVML can see (only devices that are present).
func (l *Lib) Count() (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.clear()
	if err := l.check("nvmlDeviceGetCount_v2", l.out()); err != nil {
		return 0, err
	}
	return int(l.u32()), nil
}

// Handle returns device i.
func (l *Lib) Handle(i int) (Device, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.clear()
	if err := l.check("nvmlDeviceGetHandleByIndex_v2", uintptr(i), l.out()); err != nil {
		return 0, err
	}
	return Device(*(*uintptr)(unsafe.Pointer(&l.scratch[0]))), nil
}

// Name returns the device's marketing name, e.g. "NVIDIA RTX A6000".
func (l *Lib) Name(d Device) (string, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.clear()
	if err := l.check("nvmlDeviceGetName", uintptr(d), l.out(), 96); err != nil {
		return "", err
	}
	return l.cstring(), nil
}

// DriverVersion returns the driver version string.
func (l *Lib) DriverVersion() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.clear()
	if err := l.check("nvmlSystemGetDriverVersion", l.out(), 80); err != nil {
		return ""
	}
	return l.cstring()
}

// Utilization returns GPU and memory-controller busy percent over the driver's last sample period.
func (l *Lib) Utilization(d Device) (gpu, mem uint32, err error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.clear()
	if err = l.check("nvmlDeviceGetUtilizationRates", uintptr(d), l.out()); err != nil {
		return
	}
	p := (*[2]uint32)(unsafe.Pointer(&l.scratch[0]))
	return p[0], p[1], nil
}

// Memory returns total and used framebuffer bytes. It prefers the v2 call, whose "used"
// excludes memory the driver reserves (matching nvidia-smi), and falls back to v1.
func (l *Lib) Memory(d Device) (total, used uint64, err error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.clear()
	// nvmlMemory_v2_t { unsigned int version; unsigned long long total, reserved, free, used; } = 40 bytes
	*(*uint32)(unsafe.Pointer(&l.scratch[0])) = 40 | 2<<24
	if err = l.check("nvmlDeviceGetMemoryInfo_v2", uintptr(d), l.out()); err == nil {
		p := (*[5]uint64)(unsafe.Pointer(&l.scratch[0])) // [0] holds version+padding
		return p[1], p[4], nil
	}
	l.clear()
	// nvmlMemory_t { unsigned long long total, free, used; }
	if err = l.check("nvmlDeviceGetMemoryInfo", uintptr(d), l.out()); err != nil {
		return
	}
	p := (*[3]uint64)(unsafe.Pointer(&l.scratch[0]))
	return p[0], p[2], nil
}

func (l *Lib) getU32(name string, args ...uintptr) (uint32, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.clear()
	if err := l.check(name, append(args, l.out())...); err != nil {
		return 0, err
	}
	return l.u32(), nil
}

// Temperature returns the GPU core temperature in °C.
func (l *Lib) Temperature(d Device) (uint32, error) {
	return l.getU32("nvmlDeviceGetTemperature", uintptr(d), 0) // NVML_TEMPERATURE_GPU
}

// Power returns board power draw in milliwatts.
func (l *Lib) Power(d Device) (uint32, error) { return l.getU32("nvmlDeviceGetPowerUsage", uintptr(d)) }

// PState returns the performance state 0..15.
func (l *Lib) PState(d Device) (int, error) {
	v, err := l.getU32("nvmlDeviceGetPerformanceState", uintptr(d))
	if err == nil && v > 15 {
		err = Error(3)
	}
	return int(v), err
}

// Fan returns the fan speed in percent of maximum.
func (l *Lib) Fan(d Device) (uint32, error) { return l.getU32("nvmlDeviceGetFanSpeed", uintptr(d)) }

// Clock types for Clock.
const (
	ClockGraphics = 0
	ClockMem      = 2
)

// Clock returns the current clock in MHz.
func (l *Lib) Clock(d Device, typ int) (uint32, error) {
	return l.getU32("nvmlDeviceGetClockInfo", uintptr(d), uintptr(typ))
}

// PCIe counters for PcieThroughput.
const (
	PcieTx = 0
	PcieRx = 1
)

// PcieThroughput returns PCIe traffic in KB/s. NVML measures it over about 20 ms, so this
// call blocks for that long; callers should not use it on the sampling hot path.
func (l *Lib) PcieThroughput(d Device, counter int) (uint32, error) {
	return l.getU32("nvmlDeviceGetPcieThroughput", uintptr(d), uintptr(counter))
}

// Close shuts NVML down.
func (l *Lib) Close() {
	l.mu.Lock()
	defer l.mu.Unlock()
	_ = l.check("nvmlShutdown")
}
