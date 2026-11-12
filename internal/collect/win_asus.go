//go:build windows && (amd64 || arm64)

package collect

// ASUS laptops report the CPU temperature through their ACPI ATK device: the reading Armoury
// Crate and G-Helper show. The ASUS System Control Interface driver (installed with Armoury
// Crate and MyASUS, and required by G-Helper) exposes the device as \\.\ATKACPI, and its DSTS
// ("device status") method answers 0x10000 + °C for device 0x00120094. The device lets
// Everyone read and write and the IOCTL needs no particular access, so the agent can use it as
// LocalService; no ASUS program has to be running. One read evaluates a small ACPI method in
// the firmware: about 0.5 ms of CPU on the Z13, once a second.

import (
	"encoding/binary"
	"fmt"

	"golang.org/x/sys/windows"
)

const (
	winATKPath       = `\\.\ATKACPI`
	winATKIoctl      = 0x0022240C // CTL_CODE(FILE_DEVICE_UNKNOWN, 0x903, METHOD_BUFFERED, FILE_ANY_ACCESS)
	winATKDSTS       = 0x53545344 // "DSTS", device status
	winATKDevCPUTemp = 0x00120094 // CPU temperature, °C
)

// winATK is an open ATK device with heap buffers for its IOCTL.
type winATK struct {
	h   windows.Handle
	in  *[16]byte // method id, argument bytes (8), device id, 0
	out *[16]byte
	ret uint32
}

// winOpenATK opens the ATK device and checks that it reports a CPU temperature. It returns
// the first raw reply for --probe, or why there is no such source.
func winOpenATK() (a *winATK, raw uint32, why string) {
	p, err := windows.UTF16PtrFromString(winATKPath)
	if err != nil {
		return nil, 0, err.Error()
	}
	h, err := windows.CreateFile(p, windows.GENERIC_READ|windows.GENERIC_WRITE,
		windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE, nil, windows.OPEN_EXISTING, 0, 0)
	if err != nil {
		return nil, 0, fmt.Sprintf("no ASUS ATK device (%s: %v)", winATKPath, err)
	}
	a = &winATK{h: h, in: new([16]byte), out: new([16]byte)}
	binary.LittleEndian.PutUint32(a.in[0:], winATKDSTS)
	binary.LittleEndian.PutUint32(a.in[4:], 8)
	binary.LittleEndian.PutUint32(a.in[8:], winATKDevCPUTemp)
	if raw, err = a.read(); err != nil {
		a.close()
		return nil, 0, "ASUS ATK device: DSTS: " + err.Error()
	}
	if _, ok := winASUSTemp(raw); !ok {
		a.close()
		return nil, raw, fmt.Sprintf("the ASUS ATK device reports no CPU temperature (DSTS 0x%08X answered 0x%08X)", winATKDevCPUTemp, raw)
	}
	return a, raw, ""
}

// read returns the raw DSTS reply for the CPU temperature device.
func (a *winATK) read() (uint32, error) {
	*a.out = [16]byte{}
	a.ret = 0
	if err := windows.DeviceIoControl(a.h, winATKIoctl, &a.in[0], uint32(len(a.in)),
		&a.out[0], uint32(len(a.out)), &a.ret, nil); err != nil {
		return 0, err
	}
	if a.ret < 4 {
		return 0, fmt.Errorf("short reply (%d bytes)", a.ret)
	}
	return binary.LittleEndian.Uint32(a.out[0:]), nil
}

func (a *winATK) close() {
	if a.h != 0 {
		windows.CloseHandle(a.h)
		a.h = 0
	}
}
