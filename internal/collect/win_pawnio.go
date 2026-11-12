//go:build windows && (amd64 || arm64)

package collect

// AMD desktop CPUs keep their temperature where only a kernel driver can read it: Windows has
// no API for it, and tools such as HWiNFO load a driver of their own. The agent can use PawnIO
// (https://pawnio.eu), a small signed driver that runs signed modules, each limited to the job
// it was written for; LibreHardwareMonitor and FanControl read their sensors through it. Its
// AMDFamily17 module reads the System Management Network of AMD CPUs from family 17h (Zen) to
// 1Ah (Zen 5), where THM_TCON_CUR_TMP holds Tctl: what HWiNFO shows as "CPU (Tctl/Tdie)" and
// Linux's k10temp as Tctl. The module is embedded, as published (pawnio/NOTICE).
//
// PawnIO admits only SYSTEM and Administrators, so the service can use it only when installed
// with install.ps1 -CpuTemp, which runs it as LocalSystem with all but basic privileges
// removed. A read is one IOCTL taking microseconds, made while holding the Access_PCI mutex
// that tools reading the same registers share.

import (
	_ "embed"
	"encoding/binary"
	"errors"
	"fmt"
	"runtime"
	"unsafe"

	"golang.org/x/sys/windows"
)

//go:embed pawnio/AMDFamily17.bin
var winPawnIOAMDFamily17 []byte

const (
	winPawnIOPath      = `\\?\GLOBALROOT\Device\PawnIO`
	winPawnIOLoad      = 0xA1B22084 // CTL_CODE(41394, 0x821, METHOD_BUFFERED, FILE_ANY_ACCESS)
	winPawnIOExecute   = 0xA1B22104 // CTL_CODE(41394, 0x841, METHOD_BUFFERED, FILE_ANY_ACCESS)
	winPawnIOVersion   = 0xA1B22184 // CTL_CODE(41394, 0x861, METHOD_BUFFERED, FILE_ANY_ACCESS)
	winAccessPCI       = `Global\Access_PCI`
	winAccessPCIWaitMs = 50
	// Created by the agent, the mutex lets SYSTEM and Administrators open it fully (HWiNFO,
	// LibreHardwareMonitor and FanControl run elevated and ask for full access), and everyone
	// else wait on it (SYNCHRONIZE | MUTANT_QUERY_STATE, .NET's Mutex.OpenExisting rights).
	// SYSTEM's default DACL would leave elevated programs only SYNCHRONIZE.
	winAccessPCISDDL = "D:(A;;GA;;;SY)(A;;GA;;;BA)(A;;0x00100001;;;WD)"
)

// winPawnIO is an open PawnIO handle with the AMDFamily17 module loaded, and heap buffers
// for reading Tctl.
type winPawnIO struct {
	h       windows.Handle
	mutex   windows.Handle // Global\Access_PCI; 0 when it could not be opened
	in      []byte         // "ioctl_read_smn", THM_TCON_CUR_TMP
	out     *[8]byte
	ret     uint32
	version string // driver version, for --probe
	mutexBy string // how the mutex was obtained, for --probe
}

// winOpenPawnIO opens PawnIO, loads the module and reads Tctl once. It returns that raw
// register value for --probe, or why there is no such source.
func winOpenPawnIO() (p *winPawnIO, raw uint32, why string) {
	path, err := windows.UTF16PtrFromString(winPawnIOPath)
	if err != nil {
		return nil, 0, err.Error()
	}
	h, err := windows.CreateFile(path, windows.GENERIC_READ|windows.GENERIC_WRITE,
		windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE, nil, windows.OPEN_EXISTING, 0, 0)
	switch {
	case errors.Is(err, windows.ERROR_FILE_NOT_FOUND) || errors.Is(err, windows.ERROR_PATH_NOT_FOUND):
		return nil, 0, "PawnIO is not installed (pawnio.eu; then reinstall the agent with -CpuTemp)"
	case errors.Is(err, windows.ERROR_ACCESS_DENIED):
		return nil, 0, "PawnIO admits only SYSTEM and Administrators: reinstall the agent with -CpuTemp"
	case err != nil:
		return nil, 0, "PawnIO: " + err.Error()
	}
	p = &winPawnIO{h: h, out: new([8]byte)}
	fail := func(s string) (*winPawnIO, uint32, string) { p.close(); return nil, raw, s }

	ver := new([4]byte)
	if windows.DeviceIoControl(h, winPawnIOVersion, nil, 0, &ver[0], 4, &p.ret, nil) == nil && p.ret == 4 {
		v := binary.LittleEndian.Uint32(ver[:])
		p.version = fmt.Sprintf("%d.%d.%d", v>>16, v>>8&0xFF, v&0xFF)
	}
	blob := winPawnIOAMDFamily17
	if err := windows.DeviceIoControl(h, winPawnIOLoad, &blob[0], uint32(len(blob)), nil, 0, &p.ret, nil); err != nil {
		return fail("PawnIO: loading the AMDFamily17 module: " + err.Error())
	}
	if p.in, err = winPawnIOCall("ioctl_read_smn", winZenTctlSMN); err != nil {
		return fail(err.Error())
	}
	p.openMutex()
	if raw, err = p.readTctl(); err != nil {
		return fail("PawnIO: reading Tctl: " + err.Error())
	}
	if _, ok := winZenTctl(raw); !ok {
		return fail(fmt.Sprintf("PawnIO: the Tctl register (SMN 0x%08X) reads 0x%08X", winZenTctlSMN, raw))
	}
	return p, raw, ""
}

// openMutex creates or opens the Access_PCI mutex. Without it reads still work, at the small
// risk of clashing with another program reading these registers at the same moment.
func (p *winPawnIO) openMutex() {
	name, _ := windows.UTF16PtrFromString(winAccessPCI)
	var sa *windows.SecurityAttributes
	if sd, err := windows.SecurityDescriptorFromString(winAccessPCISDDL); err == nil {
		sa = &windows.SecurityAttributes{Length: uint32(unsafe.Sizeof(windows.SecurityAttributes{})), SecurityDescriptor: sd}
	}
	m, err := windows.CreateMutex(sa, false, name)
	switch {
	case m != 0 && errors.Is(err, windows.ERROR_ALREADY_EXISTS):
		p.mutex, p.mutexBy = m, "opened (another program created it)"
	case m != 0:
		p.mutex, p.mutexBy = m, "created"
	default:
		// It exists with a DACL that refuses full access; waiting needs only SYNCHRONIZE.
		if m, err2 := windows.OpenMutex(windows.SYNCHRONIZE, false, name); err2 == nil {
			p.mutex, p.mutexBy = m, "opened for SYNCHRONIZE"
		} else {
			p.mutexBy = fmt.Sprintf("unavailable (%v; %v)", err, err2)
		}
	}
}

// readTctl returns the raw THM_TCON_CUR_TMP value. A mutex belongs to the thread that waited
// for it, so the goroutine stays on its thread until the mutex is released.
func (p *winPawnIO) readTctl() (uint32, error) {
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()
	if p.mutex != 0 {
		ev, err := windows.WaitForSingleObject(p.mutex, winAccessPCIWaitMs)
		switch ev {
		case windows.WAIT_OBJECT_0, windows.WAIT_ABANDONED:
			defer windows.ReleaseMutex(p.mutex)
		case uint32(windows.WAIT_TIMEOUT):
			return 0, fmt.Errorf("another program held the Access_PCI mutex for %d ms", winAccessPCIWaitMs)
		default:
			return 0, fmt.Errorf("waiting for the Access_PCI mutex: %v", err)
		}
	}
	*p.out = [8]byte{}
	p.ret = 0
	if err := windows.DeviceIoControl(p.h, winPawnIOExecute, &p.in[0], uint32(len(p.in)),
		&p.out[0], uint32(len(p.out)), &p.ret, nil); err != nil {
		return 0, err
	}
	if p.ret != uint32(len(p.out)) {
		return 0, fmt.Errorf("short reply (%d bytes)", p.ret)
	}
	return uint32(binary.LittleEndian.Uint64(p.out[:])), nil
}

func (p *winPawnIO) close() {
	if p.h != 0 {
		windows.CloseHandle(p.h)
		p.h = 0
	}
	if p.mutex != 0 {
		windows.CloseHandle(p.mutex)
		p.mutex = 0
	}
}
