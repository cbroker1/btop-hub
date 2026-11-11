//go:build windows

package agent

import (
	"syscall"

	"golang.org/x/sys/windows"
)

// tuneSocket limits retransmission to 30 s (TCP_MAXRT) so a vanished hub is noticed.
func tuneSocket(network, address string, c syscall.RawConn) error {
	return c.Control(func(fd uintptr) {
		const tcpMaxRT = 5
		_ = windows.SetsockoptInt(windows.Handle(fd), windows.IPPROTO_TCP, tcpMaxRT, 30)
	})
}
