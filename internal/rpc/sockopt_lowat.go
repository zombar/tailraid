//go:build linux || darwin

package rpc

import (
	"syscall"

	"golang.org/x/sys/unix"
)

// lowWater sets TCP_NOTSENT_LOWAT on a socket; a failure leaves the kernel's default,
// which is how the node ran before (I-263).
func lowWater(_, _ string, c syscall.RawConn) error {
	return c.Control(func(fd uintptr) {
		_ = unix.SetsockoptInt(int(fd), unix.IPPROTO_TCP, unix.TCP_NOTSENT_LOWAT, NotSentLowWater)
	})
}
