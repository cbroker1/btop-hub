//go:build windows

package main

import (
	"os"
	"path/filepath"
	"syscall"
)

var (
	kernel32                  = syscall.NewLazyDLL("kernel32.dll")
	procSetThreadAffinityMask = kernel32.NewProc("SetThreadAffinityMask")
	procSetPriorityClass      = kernel32.NewProc("SetPriorityClass")
	procGetCurrentProcess     = kernel32.NewProc("GetCurrentProcess")
)

func sys32(name string) string {
	root := os.Getenv("SystemRoot")
	if root == "" {
		root = `C:\Windows`
	}
	return filepath.Join(root, "System32", name)
}

func openLib(names ...string) (uintptr, error) {
	var last error
	for _, n := range names {
		h, err := syscall.LoadLibrary(n)
		if err == nil {
			return uintptr(h), nil
		}
		last = err
	}
	return 0, last
}

func sym(h uintptr, name string) (uintptr, error) {
	return syscall.GetProcAddress(syscall.Handle(h), name)
}

func ccall(fn uintptr, args ...uintptr) uintptr {
	r, _, _ := syscall.SyscallN(fn, args...)
	return r
}

func cudaLibNames() []string   { return []string{sys32("nvcuda.dll")} }
func openCLLibNames() []string { return []string{sys32("OpenCL.dll")} }

// pinThread binds the calling OS thread to one logical processor (group 0).
func pinThread(cpu int) error {
	if cpu >= 64 {
		return nil
	}
	const currentThread = ^uintptr(1) // the (HANDLE)-2 pseudo-handle
	r, _, err := procSetThreadAffinityMask.Call(currentThread, uintptr(1)<<uint(cpu))
	if r == 0 {
		return err
	}
	return nil
}

func lowerThreadPriority() {}

// lowerPriority puts the whole process in the idle priority class, below the agent's
// below-normal, so the agent and anything interactive always run first.
func lowerPriority() error {
	const idlePriorityClass = 0x40
	h, _, _ := procGetCurrentProcess.Call()
	if r, _, err := procSetPriorityClass.Call(h, idlePriorityClass); r == 0 {
		return err
	}
	return nil
}
