package rpc

import (
	"bytes"
	"fmt"
	"net"
	"testing"
	"time"

	"github.com/zombar/tailraid/internal/meta"
)

// A PUT whose body stalls holds its slot only until the body deadline: with one slot, a
// stalled upload does not block the next peer for longer than that.
//
// Invariant: I-97
func TestPut_StalledBodyFreesSlot(t *testing.T) {
	e := newEnvWith(t, func(o *Options) { o.PutSlots, o.BodyTimeout = 1, 300*time.Millisecond })
	c := e.client()
	// a raw PUT that announces 100 bytes and sends 10, then stays silent
	conn, err := net.Dial("tcp", e.addr)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	stalled := key([]byte("stalled"), 0)
	fmt.Fprintf(conn, "PUT /v1/disks/d0/shards/%s HTTP/1.1\r\nHost: %s\r\n%s: %s\r\nContent-Length: 100\r\n\r\n0123456789", stalled, e.addr, HeaderCluster, meta.HexID(e.cid))
	// the only slot is taken until the deadline; a real PUT behind it must still land
	start := time.Now()
	p := bytes.Repeat([]byte{7}, 100)
	if err := c.Put(ctx, 1, key(p, 0), p); err != nil {
		t.Fatalf("PUT behind a stalled body: %v", err)
	}
	if took := time.Since(start); took > 5*time.Second {
		t.Fatalf("the stalled body held the slot for %s", took)
	}
	if !e.store.Has(key(p, 0)) {
		t.Fatal("the second PUT did not land")
	}
	if e.store.Has(stalled) {
		t.Fatal("the stalled PUT was stored")
	}
}
