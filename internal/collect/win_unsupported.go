//go:build windows && !amd64 && !arm64

package collect

import (
	"errors"
	"runtime"
)

// New is not available here: the Windows collector mirrors 64-bit structure layouts
// (checked at compile time in win_sys.go), which differ on 32-bit Windows.
func New() (Collector, error) {
	return nil, errors.New("collect: the Windows collector supports amd64 and arm64, not " + runtime.GOARCH)
}
