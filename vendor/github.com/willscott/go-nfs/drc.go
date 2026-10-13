package nfs

import (
	"container/list"
	"context"
	"sync"
	"time"
)

// tailraid: the duplicate request cache (docs/07, I-260). A client that loses its
// connection reconnects and retransmits every request it has no reply to, under the same
// xid. Requests run concurrently (I-167) and a closed connection's handlers run to the
// end, so the copy ran again, alongside or after its original: a REMOVE answered NOENT
// for the file it had just removed, and a WRITE still waiting on the old connection could
// land after the client's next WRITE of the same range. A request the cache covers runs
// once: a copy whose original is under way waits for it, and any copy gets its reply.

// DRCEntries bounds the finished replies kept; DRCAge is how long one is kept at most,
// well past any client's retransmission (Linux retries for minutes only with hard mounts,
// and then on the same xid within that time).
const (
	DRCEntries = 1024
	DRCAge     = 2 * time.Minute
)

// cached are the procedures that change something, and so must not run twice; a READ, a
// GETATTR or a LOOKUP may run again and is not kept.
var cached = map[uint32]map[uint32]bool{
	nfsServiceID: {
		uint32(NFSProcedureSetAttr): true, uint32(NFSProcedureWrite): true, uint32(NFSProcedureCreate): true,
		uint32(NFSProcedureMkDir): true, uint32(NFSProcedureSymlink): true, uint32(NFSProcedureMkNod): true,
		uint32(NFSProcedureRemove): true, uint32(NFSProcedureRmDir): true, uint32(NFSProcedureRename): true,
		uint32(NFSProcedureLink): true, uint32(NFSProcedureCommit): true,
	},
}

// drcKey names one request: the client's address (not its port: a reconnect has a new
// one), the xid, the procedure, and a checksum of the whole frame, so another request
// under a reused xid is not taken for a copy.
type drcKey struct {
	client                string
	xid, prog, vers, proc uint32
	sum                   uint64
}

type drcEntry struct {
	done  chan struct{} // closed once the first run ended
	reply []byte        // its reply; nil if it ended without one
	at    time.Time
	elem  *list.Element // in drc.finished once it ended
}

type drc struct {
	mu       sync.Mutex
	m        map[drcKey]*drcEntry
	finished list.List // of drcKey, oldest first
}

// begin returns k's entry and whether this call is its first run.
func (d *drc) begin(k drcKey) (*drcEntry, bool) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.m == nil {
		d.m = map[drcKey]*drcEntry{}
	}
	if e, ok := d.m[k]; ok {
		return e, false
	}
	e := &drcEntry{done: make(chan struct{})}
	d.m[k] = e
	return e, true
}

// finish records the first run's reply (nil: it ended without one, and a copy runs
// afresh) and drops the oldest finished replies past the bounds.
func (d *drc) finish(k drcKey, reply []byte) {
	d.mu.Lock()
	defer d.mu.Unlock()
	e, ok := d.m[k]
	if !ok {
		return
	}
	now := time.Now()
	if reply == nil {
		delete(d.m, k)
	} else {
		e.reply, e.at = reply, now
		e.elem = d.finished.PushBack(k)
	}
	close(e.done)
	for f := d.finished.Front(); f != nil; f = d.finished.Front() {
		old := d.m[f.Value.(drcKey)]
		if d.finished.Len() <= DRCEntries && now.Sub(old.at) <= DRCAge {
			break
		}
		delete(d.m, f.Value.(drcKey))
		d.finished.Remove(f)
	}
}

func (d *drc) len() int {
	d.mu.Lock()
	defer d.mu.Unlock()
	return len(d.m)
}

// handleOnce runs a request the cache covers at most once: a copy waits for its original
// and answers with its reply.
func (c *conn) handleOnce(ctx context.Context, w *response) error {
	h := w.req.Header
	k := drcKey{client: w.req.client, xid: w.req.xid, prog: h.Prog, vers: h.Vers, proc: h.Proc, sum: w.req.sum}
	for {
		e, first := c.Server.drc.begin(k)
		if first {
			err := c.handle(ctx, w)
			if err != nil {
				c.Server.drc.finish(k, nil)
				return err
			}
			c.Server.drc.finish(k, append([]byte(nil), w.writer.Bytes()...))
			return nil
		}
		select {
		case <-e.done:
		case <-ctx.Done():
			return ctx.Err()
		}
		if e.reply != nil {
			w.responded = true
			_, err := w.writer.Write(e.reply)
			return err
		}
		// the original ended without a reply: this copy runs as the first
	}
}
