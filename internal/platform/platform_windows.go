//go:build windows

package platform

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"syscall"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
	"golang.org/x/sys/windows/registry"
	"golang.org/x/sys/windows/svc"
)

// Neither function is wrapped by golang.org/x/sys/windows.
var (
	modkernel32                 = windows.NewLazySystemDLL("kernel32.dll")
	procK32GetProcessMemoryInfo = modkernel32.NewProc("K32GetProcessMemoryInfo") // Windows 7+
	procSetProcessInformation   = modkernel32.NewProc("SetProcessInformation")   // Windows 8+
	procK32EmptyWorkingSet      = modkernel32.NewProc("K32EmptyWorkingSet")      // Windows 7+
	procQueryProcessCycleTime   = modkernel32.NewProc("QueryProcessCycleTime")   // Windows Vista+
)

// tscHz is the rate of the cycle counter QueryProcessCycleTime reports: the processor's
// rated clock ("~MHz" in the registry), which the invariant TSC of current AMD and Intel
// CPUs follows. 0 when unknown.
var tscHz = sync.OnceValue(func() float64 {
	k, err := registry.OpenKey(registry.LOCAL_MACHINE, `HARDWARE\DESCRIPTION\System\CentralProcessor\0`, registry.QUERY_VALUE)
	if err != nil {
		return 0
	}
	defer k.Close()
	mhz, _, err := k.GetIntegerValue("~MHz")
	if err != nil || mhz < 100 || mhz > 20000 {
		return 0
	}
	return float64(mhz) * 1e6
})

// processMemoryCounters mirrors PROCESS_MEMORY_COUNTERS (psapi.h): two DWORDs, then eight SIZE_Ts.
type processMemoryCounters struct {
	cb                         uint32
	pageFaultCount             uint32
	peakWorkingSetSize         uintptr
	workingSetSize             uintptr
	quotaPeakPagedPoolUsage    uintptr
	quotaPagedPoolUsage        uintptr
	quotaPeakNonPagedPoolUsage uintptr
	quotaNonPagedPoolUsage     uintptr
	pagefileUsage              uintptr
	peakPagefileUsage          uintptr
}

// processPowerThrottlingState mirrors PROCESS_POWER_THROTTLING_STATE (processthreadsapi.h).
type processPowerThrottlingState struct {
	version     uint32
	controlMask uint32
	stateMask   uint32
}

const (
	processPowerThrottling               = 4 // PROCESS_INFORMATION_CLASS ProcessPowerThrottling
	processPowerThrottlingCurrentVersion = 1 // PROCESS_POWER_THROTTLING_CURRENT_VERSION
	processPowerThrottlingExecutionSpeed = 1 // PROCESS_POWER_THROTTLING_EXECUTION_SPEED
)

// Layout checks against the C declarations; a mismatch fails the build. PROCESS_MEMORY_COUNTERS
// is 72 bytes on amd64 and arm64 (40 on 386) with WorkingSetSize at offset 16 (12 on 386);
// PROCESS_POWER_THROTTLING_STATE is 12 bytes everywhere.
var (
	_ [8 + 8*unsafe.Sizeof(uintptr(0))]byte = [unsafe.Sizeof(processMemoryCounters{})]byte{}
	_ [8 + unsafe.Sizeof(uintptr(0))]byte   = [unsafe.Offsetof(processMemoryCounters{}.workingSetSize)]byte{}
	_ [12]byte                              = [unsafe.Sizeof(processPowerThrottlingState{})]byte{}
)

// SetLowPriority puts the agent in the below-normal priority class (unless something already
// made it idle) and opts it into EcoQoS, so Windows prefers efficient cores and low clocks for
// it. EcoQoS needs Windows 10 1709 or later; failing to set it is not an error.
func SetLowPriority() error {
	h := windows.CurrentProcess()
	var err error
	if cur, _ := windows.GetPriorityClass(h); cur != windows.IDLE_PRIORITY_CLASS {
		err = windows.SetPriorityClass(h, windows.BELOW_NORMAL_PRIORITY_CLASS)
	}
	if procSetProcessInformation.Find() == nil {
		st := processPowerThrottlingState{
			version:     processPowerThrottlingCurrentVersion,
			controlMask: processPowerThrottlingExecutionSpeed,
			stateMask:   processPowerThrottlingExecutionSpeed,
		}
		syscall.SyscallN(procSetProcessInformation.Addr(), uintptr(h), processPowerThrottling,
			uintptr(unsafe.Pointer(&st)), unsafe.Sizeof(st))
	}
	return err
}

// SelfStats reports the CPU time this process has used and its working set. The agent
// starts no child processes, so these numbers are its whole cost.
//
// CPU time comes from the cycle counter: GetProcessTimes only charges a thread when the
// 15.6 ms clock tick lands while it runs, and the agent wakes on a tick, works for a
// millisecond or two and sleeps again, so tick accounting reports almost nothing for it.
func SelfStats() (time.Duration, int64, error) {
	h := windows.CurrentProcess()
	var cpu time.Duration
	var cycles uint64
	if hz := tscHz(); hz > 0 && procQueryProcessCycleTime.Find() == nil {
		if r, _, _ := syscall.SyscallN(procQueryProcessCycleTime.Addr(), uintptr(h), uintptr(unsafe.Pointer(&cycles))); r != 0 {
			cpu = time.Duration(float64(cycles) / hz * float64(time.Second))
		}
	}
	var created, exited, kernel, user windows.Filetime
	if cpu == 0 {
		if err := windows.GetProcessTimes(h, &created, &exited, &kernel, &user); err != nil {
			return 0, 0, err
		}
		// Kernel and user times are durations in 100 ns ticks.
		cpu = time.Duration(filetimeTicks(kernel)+filetimeTicks(user)) * 100
	}
	if err := procK32GetProcessMemoryInfo.Find(); err != nil {
		return 0, 0, err
	}
	var pmc processMemoryCounters
	pmc.cb = uint32(unsafe.Sizeof(pmc))
	r1, _, e1 := syscall.SyscallN(procK32GetProcessMemoryInfo.Addr(), uintptr(h),
		uintptr(unsafe.Pointer(&pmc)), uintptr(pmc.cb))
	if r1 == 0 {
		if e1 == 0 {
			return 0, 0, syscall.EINVAL
		}
		return 0, 0, e1
	}
	return cpu, int64(pmc.workingSetSize), nil
}

func filetimeTicks(ft windows.Filetime) int64 {
	return int64(ft.HighDateTime)<<32 | int64(ft.LowDateTime)
}

// isService caches whether the service control manager started this process. LogPath is
// called before RunService, and the check walks the whole process list.
var isService = sync.OnceValues(svc.IsWindowsService)

const (
	// stopTimeout bounds how long Stop or Shutdown waits for run to return.
	stopTimeout = 10 * time.Second
	// exitRunFailed is the service-specific exit code reported when run fails on its own. The
	// SCM applies the restart failure actions to it only if the service's failure-actions flag
	// is set (sc.exe failureflag btop-agent 1), as the install script does.
	exitRunFailed = 1
)

// RunService runs run under the service control manager when the SCM started this process,
// and returns false without doing anything otherwise. As a service, run's context is cancelled
// on Stop or Shutdown and run gets stopTimeout to return. If run returns on its own with an
// error, the service stops with a non-zero exit code so the SCM's failure actions restart it.
// The error returned is run's, or the dispatcher's; the caller should exit afterwards.
func RunService(name string, run func(ctx context.Context) error) (bool, error) {
	is, detectErr := isService()
	if detectErr == nil && !is {
		return false, nil
	}
	h := &handler{run: run, result: make(chan error, 1)}
	if err := svc.Run(name, h); err != nil {
		if detectErr != nil && errors.Is(err, windows.ERROR_FAILED_SERVICE_CONTROLLER_CONNECT) {
			return false, nil // detection failed, and the dispatcher says the SCM did not start us
		}
		return true, err
	}
	select {
	case err := <-h.result:
		return true, err
	default:
		return true, nil // the dispatcher returned without running the handler
	}
}

// handler implements svc.Handler around run.
type handler struct {
	run    func(ctx context.Context) error
	result chan error // run's outcome, sent once (buffered) before Execute returns
}

func (h *handler) Execute(_ []string, req <-chan svc.ChangeRequest, status chan<- svc.Status) (bool, uint32) {
	status <- svc.Status{State: svc.StartPending, WaitHint: 10000}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- h.run(ctx) }()
	status <- svc.Status{State: svc.Running, Accepts: svc.AcceptStop | svc.AcceptShutdown}

	for {
		select {
		case err := <-done:
			h.result <- err
			if err != nil {
				return true, exitRunFailed
			}
			return false, 0
		case c := <-req:
			switch c.Cmd {
			case svc.Interrogate:
				status <- c.CurrentStatus
			case svc.Stop, svc.Shutdown:
				status <- svc.Status{State: svc.StopPending, WaitHint: uint32((stopTimeout + 5*time.Second) / time.Millisecond)}
				cancel()
				h.result <- awaitStop(done, req, status)
				// A requested stop always exits 0: a failure code would make the SCM restart us.
				return false, 0
			}
		}
	}
}

// awaitStop waits up to stopTimeout for run to return after its context was cancelled, still
// answering the SCM: its control requests arrive on an unbuffered channel.
func awaitStop(done <-chan error, req <-chan svc.ChangeRequest, status chan<- svc.Status) error {
	timeout := time.NewTimer(stopTimeout)
	defer timeout.Stop()
	for {
		select {
		case err := <-done:
			if errors.Is(err, context.Canceled) {
				return nil
			}
			return err
		case <-timeout.C:
			return fmt.Errorf("stopped without waiting for the agent: it did not finish within %v", stopTimeout)
		case c := <-req:
			if c.Cmd == svc.Interrogate {
				status <- c.CurrentStatus
			}
		}
	}
}

// LogPath is %ProgramData%\btop-agent\agent.log when running as a service, whose stderr goes
// nowhere, and "" otherwise, so interactive runs and --probe log to the console.
func LogPath() string {
	if is, err := isService(); err != nil || !is {
		return ""
	}
	dir := os.Getenv("ProgramData")
	if dir == "" {
		dir = `C:\ProgramData`
	}
	return filepath.Join(dir, "btop-agent", "agent.log")
}

// TrimMemory releases pages that only start-up needed (NVML's and PDH's initialisation, DLL
// loading) from the working set; pages each sample touches come straight back. Best effort.
func TrimMemory() {
	if procK32EmptyWorkingSet.Find() != nil {
		return
	}
	procK32EmptyWorkingSet.Call(uintptr(windows.CurrentProcess()))
}
