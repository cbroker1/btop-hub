//go:build !linux && !windows

package agent

import "syscall"

func tuneSocket(network, address string, c syscall.RawConn) error { return nil }
