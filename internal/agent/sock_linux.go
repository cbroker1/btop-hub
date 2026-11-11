//go:build linux

package agent

import "syscall"

// tuneSocket makes a dead hub show up within 30 s even while unacknowledged data is queued.
func tuneSocket(network, address string, c syscall.RawConn) error {
	return c.Control(func(fd uintptr) {
		const tcpUserTimeout = 0x12
		_ = syscall.SetsockoptInt(int(fd), syscall.IPPROTO_TCP, tcpUserTimeout, 30000)
	})
}
