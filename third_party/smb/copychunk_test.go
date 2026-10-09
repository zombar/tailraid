// Copyright (c) 2026, go-filesystems
// SPDX-License-Identifier: BSD-3-Clause

package smb_test

import (
	"bytes"
	"context"
	"net"
	"sync/atomic"
	"testing"
	"time"

	"github.com/cloudsoda/go-smb2"
)

// countingConn counts what the client receives.
type countingConn struct {
	net.Conn
	in *atomic.Int64
}

func (c countingConn) Read(p []byte) (int, error) {
	n, err := c.Conn.Read(p)
	c.in.Add(int64(n))
	return n, err
}

// A copy within a share is made by the server: go-smb2 asks for a resume key
// and sends FSCTL_SRV_COPYCHUNK, and the bytes never come to the client. The
// client receiving far less than the file is the proof.
func TestACopyWithinAShareStaysOnTheServer(t *testing.T) {
	addr, srv := serve(t)
	fs := newMemFS(true)
	// Under 16 MiB: go-smb2's copy loop does not move its offsets past the
	// first 16 MiB of a larger file.
	want := bytes.Repeat([]byte("0123456789abcdef"), (5<<20+7)/16)
	if err := fs.WriteFile("/src.bin", want, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := srv.Share("data", fs); err != nil {
		t.Fatal(err)
	}
	var in atomic.Int64
	raw, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	d := &smb2.Dialer{Initiator: &smb2.NTLMInitiator{User: "alice", Password: "hunter2"}}
	s, err := d.DialConn(ctx, countingConn{raw, &in}, addr)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Logoff()
	share, err := s.Mount("data")
	if err != nil {
		t.Fatal(err)
	}
	defer share.Umount()
	src, err := share.Open("src.bin")
	if err != nil {
		t.Fatal(err)
	}
	defer src.Close()
	dst, err := share.Create("dst.bin")
	if err != nil {
		t.Fatal(err)
	}
	before := in.Load()
	n, err := dst.ReadFrom(src)
	dst.Close()
	if err != nil || n != int64(len(want)) {
		t.Fatalf("copied %d, %v; want %d", n, err, len(want))
	}
	if got := in.Load() - before; got > 64<<10 {
		t.Fatalf("the client received %d bytes for a %d-byte copy: it was not made on the server", got, len(want))
	}
	got, err := fs.ReadFile("/dst.bin")
	if err != nil || !bytes.Equal(got, want) {
		t.Fatalf("the copy has %d bytes, not the %d of the source (%v)", len(got), len(want), err)
	}
}
