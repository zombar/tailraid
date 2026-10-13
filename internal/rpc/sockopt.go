package rpc

import (
	"net"
	"time"
)

// NotSentLowWater is the most unsent data a bulk socket of the node RPC (a shard PUT's body,
// a GET's reply) keeps queued in the kernel, where the platform has TCP_NOTSENT_LOWAT
// (Linux, macOS): the kernel autotunes a send buffer to megabytes, and a small control call
// on another socket to the same host then waits behind all of it on the way out. Data in
// flight is not limited, so throughput holds (docs/05 §6, I-263,
// docs/reports/rpc-transports-2026-10.md).
const NotSentLowWater = 128 << 10

// shardDialer dials the shard transport's connections, with the low water mark.
func shardDialer() *net.Dialer {
	return &net.Dialer{Timeout: 3 * time.Second, Control: lowWater}
}

// controlDialer dials the control transport's, with the kernel's defaults.
func controlDialer() *net.Dialer { return &net.Dialer{Timeout: 3 * time.Second} }

// lowWaterListener sets the low water mark on every connection it accepts: a listening
// socket's options are not reliably inherited by the connections it accepts.
type lowWaterListener struct{ net.Listener }

func (l lowWaterListener) Accept() (net.Conn, error) {
	c, err := l.Listener.Accept()
	if err != nil {
		return nil, err
	}
	if tc, ok := c.(*net.TCPConn); ok {
		if sc, err := tc.SyscallConn(); err == nil {
			_ = lowWater("tcp", c.LocalAddr().String(), sc)
		}
	}
	return c, nil
}
