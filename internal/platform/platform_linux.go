//go:build linux

package platform

import (
	"context"
	"os"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// SetLowPriority renices the agent to 10 unless something (systemd's Nice=) already did.
func SetLowPriority() error {
	cur, err := syscall.Getpriority(syscall.PRIO_PROCESS, 0)
	if err != nil {
		return err
	}
	// The raw syscall returns 20 - nice.
	if 20-cur >= 10 {
		return nil
	}
	return syscall.Setpriority(syscall.PRIO_PROCESS, 0, 10)
}

// SelfStats reads this process's CPU time and resident memory from /proc. The agent never
// starts child processes, so these numbers are the whole cost.
func SelfStats() (time.Duration, int64, error) {
	b, err := os.ReadFile("/proc/self/stat")
	if err != nil {
		return 0, 0, err
	}
	s := string(b)
	// Fields after the parenthesised command name; utime and stime are fields 14 and 15.
	if i := strings.LastIndexByte(s, ')'); i >= 0 {
		s = s[i+2:]
	}
	f := strings.Fields(s)
	if len(f) < 22 {
		return 0, 0, syscall.EINVAL
	}
	ut, _ := strconv.ParseInt(f[11], 10, 64)
	st, _ := strconv.ParseInt(f[12], 10, 64)
	rssPages, _ := strconv.ParseInt(f[21], 10, 64)
	const ticks = 100 // USER_HZ is 100 on every Linux ABI Go supports
	cpu := time.Duration(ut+st) * time.Second / ticks
	return cpu, rssPages * int64(os.Getpagesize()), nil
}

// RunService reports that Linux agents are never Windows services; systemd runs them directly.
func RunService(name string, run func(ctx context.Context) error) (bool, error) { return false, nil }

// LogPath is empty: on Linux the agent logs to stderr (the journal under systemd).
func LogPath() string { return "" }

// TrimMemory is a no-op on Linux: the kernel reclaims start-up pages under pressure.
func TrimMemory() {}
