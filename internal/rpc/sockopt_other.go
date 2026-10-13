//go:build !linux && !darwin

package rpc

import "syscall"

// lowWater does nothing where the platform has no TCP_NOTSENT_LOWAT (Windows).
func lowWater(_, _ string, _ syscall.RawConn) error { return nil }
