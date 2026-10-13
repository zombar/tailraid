//go:build linux || darwin

package rpc

import (
	"net"
	"syscall"
	"testing"

	"golang.org/x/sys/unix"
)

func notSentLowAt(t *testing.T, c net.Conn) int {
	t.Helper()
	sc, err := c.(syscall.Conn).SyscallConn()
	if err != nil {
		t.Fatal(err)
	}
	var v int
	var gerr error
	if err := sc.Control(func(fd uintptr) { v, gerr = unix.GetsockoptInt(int(fd), unix.IPPROTO_TCP, unix.TCP_NOTSENT_LOWAT) }); err != nil {
		t.Fatal(err)
	}
	if gerr != nil {
		t.Fatal(gerr)
	}
	return v
}

// The bulk side of the node RPC keeps at most NotSentLowWater unsent bytes in the kernel
// per socket: the shard client's connections (a PUT's body) and every connection the RPC
// listener accepts (a GET's reply). A small control call to the same host then waits
// behind far less queued shard data (docs/reports/rpc-transports-2026-10.md). The control
// transport's connections keep the kernel's default.
// Invariant: I-263
func TestNotSentLowWater_OnTheBulkSockets(t *testing.T) {
	l, err := Listen(t.Context(), "127.0.0.1:0", AllowAll{})
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	accepted := make(chan net.Conn, 2)
	go func() {
		for {
			c, err := l.Accept()
			if err != nil {
				return
			}
			accepted <- c
		}
	}()
	shard, err := shardDialer().DialContext(t.Context(), "tcp", l.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer shard.Close()
	if got := notSentLowAt(t, shard); got != NotSentLowWater {
		t.Errorf("a shard connection's TCP_NOTSENT_LOWAT is %d, want %d", got, NotSentLowWater)
	}
	server := <-accepted
	defer server.Close()
	if got := notSentLowAt(t, server); got != NotSentLowWater {
		t.Errorf("an accepted RPC connection's TCP_NOTSENT_LOWAT is %d, want %d", got, NotSentLowWater)
	}
	ctl, err := controlDialer().DialContext(t.Context(), "tcp", l.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer ctl.Close()
	<-accepted
	if got := notSentLowAt(t, ctl); got == NotSentLowWater {
		t.Errorf("a control connection has the bulk sockets' low water mark")
	}
}
