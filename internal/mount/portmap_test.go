package mount

import (
	"context"
	"encoding/binary"
	"errors"
	"net"
	"testing"
	"time"

	"github.com/willscott/go-nfs-client/nfs/rpc"
)

// Invariant: I-58
func TestPortmap_GetportDumpNull(t *testing.T) {
	tl, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	ul, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	p := &Portmap{Port: 2049}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { _ = p.serve(ctx, tl, ul); close(done) }()
	defer func() { cancel(); <-done }()

	// TCP through the real client library: NFS and MOUNT v3/TCP map to 2049, anything else 0
	cl, err := rpc.DialTCP("tcp", tl.Addr().String(), false)
	if err != nil {
		t.Fatal(err)
	}
	defer cl.Close()
	pm := &rpc.Portmapper{Client: cl}
	for _, tc := range []struct {
		prog, vers, prot uint32
		want             int
	}{{100003, 3, 6, 2049}, {100005, 3, 6, 2049}, {100003, 2, 6, 0}, {100003, 3, 17, 0}, {100021, 4, 6, 0}} {
		port, err := pm.Getport(rpc.Mapping{Prog: tc.prog, Vers: tc.vers, Prot: tc.prot})
		if err != nil || port != tc.want {
			t.Fatalf("getport %+v = %d, %v", tc, port, err)
		}
	}
	// UDP by hand: NULL, DUMP, and an unknown procedure
	call := func(proc uint32, args ...uint32) []byte {
		msg := binary.BigEndian.AppendUint32(nil, 7)                    // xid
		for _, v := range []uint32{0, 2, 100000, 2, proc, 0, 0, 0, 0} { // CALL, rpcvers, prog, vers, proc, cred, verf
			msg = binary.BigEndian.AppendUint32(msg, v)
		}
		for _, v := range args {
			msg = binary.BigEndian.AppendUint32(msg, v)
		}
		uc, err := net.Dial("udp", ul.LocalAddr().String())
		if err != nil {
			t.Fatal(err)
		}
		defer uc.Close()
		_ = uc.SetDeadline(time.Now().Add(5 * time.Second))
		if _, err := uc.Write(msg); err != nil {
			t.Fatal(err)
		}
		buf := make([]byte, 4096)
		n, err := uc.Read(buf)
		if err != nil {
			t.Fatal(err)
		}
		return buf[:n]
	}
	u32 := func(b []byte, i int) uint32 { return binary.BigEndian.Uint32(b[4*i:]) }
	if rep := call(0); u32(rep, 0) != 7 || u32(rep, 1) != 1 || u32(rep, 2) != 0 || u32(rep, 5) != 0 || len(rep) != 24 {
		t.Fatalf("NULL reply %v", rep)
	}
	rep := call(4)
	if u32(rep, 5) != 0 { // accepted
		t.Fatalf("DUMP reply %v", rep)
	}
	var progs []uint32
	for i := 6; u32(rep, i) == 1; i += 5 {
		progs = append(progs, u32(rep, i+1))
		if u32(rep, i+2) != 3 || u32(rep, i+3) != 6 || u32(rep, i+4) != 2049 {
			t.Fatalf("DUMP mapping %v", rep[4*i:])
		}
	}
	if len(progs) != 2 || progs[0] != 100003 || progs[1] != 100005 {
		t.Fatalf("DUMP programs %v", progs)
	}
	if rep := call(9); u32(rep, 5) != 3 { // PROC_UNAVAIL
		t.Fatalf("unknown proc reply %v", rep)
	}
	if rep := call(3, 100003, 3, 6); u32(rep, 5) != 0 || u32(rep, 6) != 2049 {
		t.Fatalf("GETPORT over UDP %v", rep)
	}
}

// Invariant: I-58
func TestPortmap_RefusesNonLoopback(t *testing.T) {
	p := &Portmap{Port: 2049}
	if err := p.Serve(context.Background(), "0.0.0.0:0"); !errors.Is(err, ErrNotLoopback) {
		t.Fatalf("err = %v", err)
	}
	if err := p.Serve(context.Background(), "nope"); err == nil {
		t.Fatal("bad address accepted")
	}
}
