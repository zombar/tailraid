package mount

import (
	"context"
	"encoding/binary"
	"errors"
	"log/slog"
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

// Invariant: I-58, I-50
func TestPortmap_RefusesNonLoopback(t *testing.T) {
	p := &Portmap{Port: 2049}
	if err := p.Serve(context.Background(), "0.0.0.0:0"); !errors.Is(err, ErrNotLoopback) {
		t.Fatalf("err = %v", err)
	}
	if err := p.Serve(context.Background(), "nope"); err == nil {
		t.Fatal("bad address accepted")
	}
}

// Reply table of the stub, by hand over the handler: the accept status and payload for
// each request shape mount.exe or anything else on loopback could send (RFC 5531 §9).
// Invariant: I-50
func TestPortmap_ReplyTable(t *testing.T) {
	p := &Portmap{Port: 2049, Log: slog.Default()}
	u32 := func(b []byte, i int) uint32 { return binary.BigEndian.Uint32(b[4*i:]) }
	msg := func(prog, vers, proc uint32, args ...uint32) []byte {
		m := binary.BigEndian.AppendUint32(nil, 11)
		for _, v := range []uint32{0, 2, prog, vers, proc, 0, 0, 0, 0} {
			m = binary.BigEndian.AppendUint32(m, v)
		}
		for _, v := range args {
			m = binary.BigEndian.AppendUint32(m, v)
		}
		return m
	}
	const (
		success      = 0
		progUnavail  = 1
		progMismatch = 2
		procUnavail  = 3
	)
	cases := []struct {
		name   string
		req    []byte
		accept uint32
		rest   []uint32 // words after the accept status
	}{
		{"null", msg(100000, 2, 0), success, nil},
		{"getport nfs v3 tcp", msg(100000, 2, 3, 100003, 3, 6), success, []uint32{2049}},
		{"getport mount v3 tcp", msg(100000, 2, 3, 100005, 3, 6), success, []uint32{2049}},
		{"getport mount v3 udp: not served", msg(100000, 2, 3, 100005, 3, 17), success, []uint32{0}},
		{"getport nfs v2: not served", msg(100000, 2, 3, 100003, 2, 6), success, []uint32{0}},
		{"getport nlm: not registered", msg(100000, 2, 3, 100021, 4, 6), success, []uint32{0}},
		{"unknown procedure", msg(100000, 2, 9), procUnavail, nil},
		{"wrong program", msg(100003, 3, 0), progUnavail, nil},
		{"wrong portmap version", msg(100000, 3, 0), progMismatch, []uint32{2, 2}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			rep := p.handle(c.req)
			if len(rep) < 24 {
				t.Fatalf("reply %x", rep)
			}
			if u32(rep, 0) != 11 || u32(rep, 1) != 1 || u32(rep, 2) != 0 {
				t.Fatalf("header %x", rep[:12])
			}
			if got := u32(rep, 5); got != c.accept {
				t.Fatalf("accept status %d, want %d", got, c.accept)
			}
			for i, w := range c.rest {
				if got := u32(rep, 6+i); got != w {
					t.Fatalf("word %d = %d, want %d", i, got, w)
				}
			}
			if len(rep) != 24+4*len(c.rest) {
				t.Fatalf("reply length %d", len(rep))
			}
		})
	}
	// garbage is dropped, never answered
	if rep := p.handle([]byte{1, 2, 3}); rep != nil {
		t.Fatalf("garbage answered: %x", rep)
	}
}
