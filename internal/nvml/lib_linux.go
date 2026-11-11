//go:build linux

package nvml

import (
	"github.com/ebitengine/purego"
)

func loadLibrary() (uintptr, error) {
	var err error
	for _, name := range []string{"libnvidia-ml.so.1", "libnvidia-ml.so"} {
		var h uintptr
		if h, err = purego.Dlopen(name, purego.RTLD_NOW|purego.RTLD_GLOBAL); err == nil {
			return h, nil
		}
	}
	return 0, err
}

func symbol(h uintptr, name string) (uintptr, error) { return purego.Dlsym(h, name) }

func call(fn uintptr, args ...uintptr) uintptr {
	r1, _, _ := purego.SyscallN(fn, args...)
	return r1
}
