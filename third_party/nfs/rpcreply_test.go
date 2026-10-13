package nfs

import (
	"bytes"
	"encoding/binary"
	"io"
	"net"
	"testing"

	"github.com/willscott/go-nfs-client/nfs/rpc"
	"github.com/willscott/go-nfs-client/nfs/xdr"
)

// reply is the RPC part of one reply (RFC 5531 §9): MSG_ACCEPTED with its accept_stat, or
// MSG_DENIED with its reject_stat, and the version range a mismatch carries.
type reply struct {
	denied    bool
	stat      uint32
	low, high uint32
}

func call(t *testing.T, c net.Conn, rpcvers, prog, vers, proc uint32, args []byte) reply {
	t.Helper()
	var body bytes.Buffer
	_ = xdr.Write(&body, uint32(7))
	_ = xdr.Write(&body, uint32(0))
	_ = xdr.Write(&body, rpc.Header{Rpcvers: rpcvers, Prog: prog, Vers: vers, Proc: proc, Cred: rpc.AuthNull, Verf: rpc.AuthNull})
	body.Write(args)
	var out bytes.Buffer
	_ = binary.Write(&out, binary.BigEndian, uint32(body.Len())|1<<31)
	out.Write(body.Bytes())
	if _, err := c.Write(out.Bytes()); err != nil {
		t.Fatal(err)
	}
	var mark [4]byte
	if _, err := io.ReadFull(c, mark[:]); err != nil {
		t.Fatal(err)
	}
	b := make([]byte, binary.BigEndian.Uint32(mark[:])&^(1<<31))
	if _, err := io.ReadFull(c, b); err != nil {
		t.Fatal(err)
	}
	u := func() uint32 {
		if len(b) < 4 {
			t.Fatalf("reply too short")
		}
		v := binary.BigEndian.Uint32(b)
		b = b[4:]
		return v
	}
	u() // xid
	if u() != 1 {
		t.Fatal("not a reply")
	}
	var r reply
	r.denied = u() == 1
	if !r.denied {
		u()              // verifier flavor
		b = b[u():]      // verifier body
		r.stat = u()     // accept_stat
		if r.stat == 2 { // PROG_MISMATCH
			r.low, r.high = u(), u()
		}
		return r
	}
	r.stat = u()     // reject_stat
	if r.stat == 0 { // RPC_MISMATCH
		r.low, r.high = u(), u()
	} else {
		r.low = u() // auth_stat
	}
	return r
}

// Every RPC reply carries the RFC 5531 code for what went wrong: an unknown program is
// PROG_UNAVAIL, a version this server does not speak PROG_MISMATCH with the range it
// does (NFS 3, MOUNT 3), an unknown procedure PROC_UNAVAIL, arguments that do not
// decode GARBAGE_ARGS, and an RPC version other than 2 is denied with RPC_MISMATCH 2..2,
// each field big-endian. Before, the accept_stat values were shifted by one (a missing
// procedure went out as PROG_MISMATCH with no range), versions were not checked at all
// and a denial wrote a reject_stat of 5.
// Invariant: I-244, I-245
func TestConn_RPCReplyCodes(t *testing.T) {
	huge := make([]byte, 8)
	binary.BigEndian.PutUint32(huge, 0xFFFFFFF0) // a handle claiming 4 GiB
	cases := []struct {
		name                      string
		rpcvers, prog, vers, proc uint32
		args                      []byte
		want                      reply
	}{
		{"NFS NULL", 2, nfsServiceID, 3, 0, nil, reply{stat: 0}},
		{"unknown program", 2, 123456, 1, 0, nil, reply{stat: 1}},
		{"NFS version 4", 2, nfsServiceID, 4, 0, nil, reply{stat: 2, low: 3, high: 3}},
		{"NFS version 2", 2, nfsServiceID, 2, 1, nil, reply{stat: 2, low: 3, high: 3}},
		{"MOUNT version 1", 2, mountServiceID, 1, 1, nil, reply{stat: 2, low: 3, high: 3}},
		{"unknown NFS procedure", 2, nfsServiceID, 3, 99, nil, reply{stat: 3}},
		{"GETATTR of a 4 GiB handle", 2, nfsServiceID, 3, 1, huge, reply{stat: 4}},
		{"GETATTR with no handle", 2, nfsServiceID, 3, 1, nil, reply{stat: 4}},
		{"RPC version 3", 3, nfsServiceID, 3, 0, nil, reply{denied: true, stat: 0, low: 2, high: 2}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := serveTCP(t)
			if got := call(t, c, tc.rpcvers, tc.prog, tc.vers, tc.proc, tc.args); got != tc.want {
				t.Fatalf("reply %+v, want %+v", got, tc.want)
			}
		})
	}
}
