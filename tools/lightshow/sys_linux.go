//go:build linux

package main

import (
	"syscall"
	"unsafe"

	"github.com/ebitengine/purego"
)

func openLib(names ...string) (uintptr, error) {
	var last error
	for _, n := range names {
		h, err := purego.Dlopen(n, purego.RTLD_NOW|purego.RTLD_GLOBAL)
		if err == nil {
			return h, nil
		}
		last = err
	}
	return 0, last
}

func sym(h uintptr, name string) (uintptr, error) { return purego.Dlsym(h, name) }

func ccall(fn uintptr, args ...uintptr) uintptr {
	r, _, _ := purego.SyscallN(fn, args...)
	return r
}

func cudaLibNames() []string   { return []string{"libcuda.so.1"} }
func openCLLibNames() []string { return []string{"libOpenCL.so.1"} }

// pinThread binds the calling OS thread to one CPU.
func pinThread(cpu int) error {
	var mask [16]uint64
	if cpu >= len(mask)*64 {
		return nil
	}
	mask[cpu/64] = 1 << (uint(cpu) % 64)
	_, _, e := syscall.RawSyscall(syscall.SYS_SCHED_SETAFFINITY, 0, unsafe.Sizeof(mask), uintptr(unsafe.Pointer(&mask)))
	if e != 0 {
		return e
	}
	return nil
}

// Nice is per thread on Linux: lower the calling thread to 19, below the agent's 10.
func lowerThreadPriority() { _ = syscall.Setpriority(syscall.PRIO_PROCESS, 0, 19) }
func lowerPriority() error { return syscall.Setpriority(syscall.PRIO_PROCESS, 0, 19) }
