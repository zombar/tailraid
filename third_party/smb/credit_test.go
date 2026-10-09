// Copyright (c) 2026, go-filesystems
// SPDX-License-Identifier: BSD-3-Clause

package smb

import (
	"encoding/binary"
	"testing"
)

func TestCapabilitiesFor(t *testing.T) {
	for _, tc := range []struct {
		dialect uint16
		want    uint32
	}{
		{dialectWildcard, 0},
		{dialect202, 0},
		{dialect210, capLargeMTU},
		{dialect300, capLargeMTU},
		{dialect302, capLargeMTU},
	} {
		if got := capabilitiesFor(tc.dialect); got != tc.want {
			t.Errorf("capabilitiesFor(%#04x) = %#x, want %#x", tc.dialect, got, tc.want)
		}
	}
}

// MS-SMB2 3.3.5.2.5, case by case.
func TestCreditChargeTooLow(t *testing.T) {
	field := func(size, off int, v uint32) []byte {
		b := make([]byte, size)
		binary.LittleEndian.PutUint32(b[off:], v)
		return b
	}
	for _, tc := range []struct {
		name   string
		cmd    command
		charge uint16
		body   []byte
		want   bool
	}{
		{"READ 64 KiB, charge 1", cmdRead, 1, field(49, 4, 64<<10), false},
		{"READ 64 KiB + 1, charge 1", cmdRead, 1, field(49, 4, 64<<10+1), true},
		{"READ 1 MiB, charge 16", cmdRead, 16, field(49, 4, 1<<20), false},
		{"READ 1 MiB, charge 15", cmdRead, 15, field(49, 4, 1<<20), true},
		{"READ 1 MiB, charge 0", cmdRead, 0, field(49, 4, 1<<20), true},
		{"READ 64 KiB, charge 0", cmdRead, 0, field(49, 4, 64<<10), false},
		{"WRITE of 1 MiB in the body, charge 1", cmdWrite, 1, make([]byte, 48+1<<20), true},
		{"WRITE of 1 MiB in the body, charge 16", cmdWrite, 16, make([]byte, 48+1<<20-48), false},
		// the header is not payload: a 64 KiB WRITE costs one credit, which is what
		// Windows and the Linux client pay for it
		{"WRITE of 64 KiB, charge 1", cmdWrite, 1, make([]byte, 48+64<<10), false},
		{"WRITE of 64 KiB by Length, charge 1", cmdWrite, 1, field(48+64<<10, 4, 64<<10), false},
		{"WRITE of 64 KiB + 1 by Length, charge 1", cmdWrite, 1, field(48+64<<10+1, 4, 64<<10+1), true},
		{"QUERY_DIRECTORY wanting 1 MiB, charge 1", cmdQueryDirectory, 1, field(33, 28, 1<<20), true},
		{"QUERY_INFO wanting 1 MiB, charge 16", cmdQueryInfo, 16, field(41, 4, 1<<20), false},
		{"CHANGE_NOTIFY wanting 128 KiB, charge 1", cmdChangeNotify, 1, field(32, 4, 128<<10), true},
		{"IOCTL MaxOutputResponse 1 MiB, charge 1", cmdIoctl, 1, field(57, 44, 1<<20), true},
		{"IOCTL MaxInputResponse 1 MiB, charge 16", cmdIoctl, 16, field(57, 32, 1<<20), false},
		{"a READ too short to read Length from", cmdRead, 1, make([]byte, 3), false},
		// 65536 credits, which a uint16 count wraps to zero.
		{"READ 4 GiB - 1, charge 1", cmdRead, 1, field(49, 4, 0xFFFFFFFF), true},
		{"READ 4 GiB - 1, charge 65535", cmdRead, 65535, field(49, 4, 0xFFFFFFFF), true},
		{"IOCTL MaxOutputResponse 4 GiB - 1, charge 1", cmdIoctl, 1, field(57, 44, 0xFFFFFFFF), true},
		{"ECHO, charge 0", cmdEcho, 0, make([]byte, 4), false},
	} {
		h := header{command: tc.cmd, creditCharge: tc.charge}
		if got := creditChargeTooLow(h, tc.body); got != tc.want {
			t.Errorf("%s: too low = %v, want %v", tc.name, got, tc.want)
		}
	}
	if n := creditsNeeded(0xFFFFFFFF); n != 65536 {
		t.Errorf("creditsNeeded(4 GiB - 1) = %d, want 65536", n)
	}
	if creditsNeeded(0) != 1 {
		t.Errorf("creditsNeeded(0) = %d, want 1", creditsNeeded(0))
	}
}

// The check is applied to every request once a dialect is negotiated: a
// megabyte READ that pays one credit is refused before it reaches the file.
func TestAnUnderpaidReadIsRefused(t *testing.T) {
	body := make([]byte, 49)
	binary.LittleEndian.PutUint16(body, 49)
	binary.LittleEndian.PutUint32(body[4:], 1<<20)
	msg := request(cmdRead, 1, body) // CreditCharge 1

	c := &conn{srv: New(), dialect: dialect302}
	out, err := c.dispatch(msg)
	if err != nil {
		t.Fatal(err)
	}
	if st := statusOf(t, out); st != statusInvalidParameter {
		t.Fatalf("a 1 MiB READ charged 1 credit: status %#x, want INVALID_PARAMETER", st)
	}

	binary.LittleEndian.PutUint16(msg[offCreditCharge:], 16)
	out, err = c.dispatch(msg)
	if err != nil {
		t.Fatal(err)
	}
	if st := statusOf(t, out); st == statusInvalidParameter {
		t.Fatalf("a 1 MiB READ charged 16 credits was refused as underpaid")
	}
}
