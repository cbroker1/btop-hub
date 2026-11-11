//go:build !linux && !windows

package platform

import (
	"context"
	"errors"
	"time"
)

func SetLowPriority() error                    { return nil }
func SelfStats() (time.Duration, int64, error) { return 0, 0, errors.New("unsupported") }
func RunService(name string, run func(ctx context.Context) error) (bool, error) {
	return false, nil
}
func LogPath() string { return "" }

func TrimMemory() {}
