//go:build windows

package nvml

import (
	"os"
	"path/filepath"
	"syscall"

	"golang.org/x/sys/windows"
)

// Current drivers install nvml.dll in System32; older ones put it under NVSMI.
func loadLibrary() (uintptr, error) {
	h, err := windows.LoadLibraryEx("nvml.dll", 0, windows.LOAD_LIBRARY_SEARCH_SYSTEM32)
	if err == nil {
		return uintptr(h), nil
	}
	pf := os.Getenv("ProgramW6432")
	if pf == "" {
		pf = os.Getenv("ProgramFiles")
	}
	if pf != "" {
		p := filepath.Join(pf, "NVIDIA Corporation", "NVSMI", "nvml.dll")
		if h2, err2 := windows.LoadLibraryEx(p, 0, windows.LOAD_WITH_ALTERED_SEARCH_PATH); err2 == nil {
			return uintptr(h2), nil
		}
	}
	return 0, err
}

func symbol(h uintptr, name string) (uintptr, error) {
	return windows.GetProcAddress(windows.Handle(h), name)
}

func call(fn uintptr, args ...uintptr) uintptr {
	r1, _, _ := syscall.SyscallN(fn, args...)
	return r1
}
