//go:build windows

package admin

import (
	"testing"

	"golang.org/x/sys/windows"
)

// The interface table is summed without loopback adapters and the tailnet row is picked
// out by its index.
// Invariant: I-190
func TestVitals_IfTableSum(t *testing.T) {
	rows := []windows.MibIfRow2{
		{InterfaceIndex: 1, Type: windows.IF_TYPE_SOFTWARE_LOOPBACK, InOctets: 99, OutOctets: 99},
		{InterfaceIndex: 4, Type: windows.IF_TYPE_ETHERNET_CSMACD, InOctets: 1000, OutOctets: 200},
		{InterfaceIndex: 12, Type: windows.IF_TYPE_TUNNEL, InOctets: 50, OutOctets: 7},
	}
	rx, tx, trx, ttx, ok := sumIfRows(rows, 12)
	if !ok || rx != 1050 || tx != 207 || trx != 50 || ttx != 7 {
		t.Fatalf("got %d %d %d %d %v", rx, tx, trx, ttx, ok)
	}
	if _, _, _, _, ok := sumIfRows(nil, 0); ok {
		t.Fatal("an empty table reported counters")
	}
}
