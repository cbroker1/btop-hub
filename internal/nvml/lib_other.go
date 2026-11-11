//go:build !linux && !windows

package nvml

import "errors"

func loadLibrary() (uintptr, error) { return 0, errors.New("nvml: unsupported platform") }
func symbol(h uintptr, name string) (uintptr, error) {
	return 0, errors.New("nvml: unsupported platform")
}
func call(fn uintptr, args ...uintptr) uintptr { return 1 }
