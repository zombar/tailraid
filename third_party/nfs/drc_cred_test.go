package nfs

import (
	"bytes"
	"encoding/binary"
	"testing"

	"github.com/willscott/go-nfs-client/nfs/rpc"
	"github.com/willscott/go-nfs-client/nfs/xdr"
)

// frameAs is frame with the AUTH_SYS credential cred (built once: a credential carries a
// stamp, and a retransmission repeats the original's bytes).
func frameAs(xid, proc uint32, cred rpc.Auth, pad int) []byte {
	var body bytes.Buffer
	_ = xdr.Write(&body, xid)
	_ = xdr.Write(&body, uint32(0)) // call
	_ = xdr.Write(&body, rpc.Header{Rpcvers: 2, Prog: testProg, Vers: 1, Proc: proc, Cred: cred, Verf: rpc.AuthNull})
	body.Write(make([]byte, pad))
	var out bytes.Buffer
	_ = binary.Write(&out, binary.BigEndian, uint32(body.Len())|1<<31)
	out.Write(body.Bytes())
	return out.Bytes()
}

// The duplicate request cache keys on the credential as well as the xid: the same call
// under one xid from two uids runs twice, each uid getting its own reply, while the
// same uid's retransmission is answered from the cache. Before, two uids replaying one
// xid could share a reply (docs/21 §5).
// Invariant: I-351, I-260
func TestDRC_KeyedOnCredential(t *testing.T) {
	dial := serveTCPDial(t)
	drcRuns.Store(0)
	u501, u502 := rpc.NewAuthUnix("test", 501, 20).Auth(), rpc.NewAuthUnix("test", 502, 20).Auth()
	a := dial()
	_, _ = a.Write(frameAs(77, drcProc, u501, 8))
	<-drcIn
	if run := replyRun(t, a); run != 1 {
		t.Fatalf("uid 501's call answered by run %d", run)
	}
	b := dial()
	_, _ = b.Write(frameAs(77, drcProc, u502, 8))
	<-drcIn
	if run := replyRun(t, b); run != 2 || drcRuns.Load() != 2 {
		t.Fatalf("uid 502's call under the same xid answered by run %d after %d runs, want a run of its own (2)", run, drcRuns.Load())
	}
	c := dial()
	_, _ = c.Write(frameAs(77, drcProc, u501, 8))
	if run := replyRun(t, c); run != 1 || drcRuns.Load() != 2 {
		t.Fatalf("uid 501's retransmission answered by run %d after %d runs, want run 1 of 2", run, drcRuns.Load())
	}
}
