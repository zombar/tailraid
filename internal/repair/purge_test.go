package repair_test

import (
	"testing"
	"time"

	"github.com/zombar/tailraid/internal/meta"
	"github.com/zombar/tailraid/internal/repair"
	"github.com/zombar/tailraid/internal/testharness"
)

func must(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

// trashedChunk commits one chunk under /a and deletes /a into the trash at the fake
// clock's now; it returns the chunk and the root's inode.
func trashedChunk(t *testing.T, f *testharness.Fabric) (meta.Chunk, meta.Ino) {
	t.Helper()
	c := flushedChunk(t, f)
	ino := f.Inode(f.InodeOf(f.FS(), "/a")).Ino
	f.MustApply(meta.Command{Op: meta.OpTrash, Trash: &meta.Trash{Parent: meta.RootIno, Name: "a", Now: f.Clk.Now().UnixNano()}})
	return c, ino
}

// The retention purge takes a root strictly after retention on the FSM clock, never a
// younger one, and never anything when the retention is zero; the chunks it frees are
// tombstoned at that moment, so GC collects them one grace later.
//
// Invariant: I-157
// Lifecycle: L-201, L-207
func TestGC_PurgeTrash_RetentionBoundary(t *testing.T) {
	const retention, grace = 30 * 24 * time.Hour, 10 * time.Minute
	for _, tc := range []struct {
		name      string
		retention time.Duration
		age       time.Duration
		purged    int
	}{
		{"retention-1", retention, retention - time.Nanosecond, 0},
		{"retention", retention, retention, 0},
		{"retention+1", retention, retention + time.Nanosecond, 1},
		{"never", 0, 365 * 24 * time.Hour, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := testharness.NewFabric(t, 3, 2, 1)
			c, ino := trashedChunk(t, f)
			deleted := f.Clk.Now()
			f.Clk.Set(deleted.Add(tc.age))
			gc := repair.NewGC(f.FSM, f, f, f, f.Clk, grace, nil)
			gc.Retention = tc.retention
			res, err := gc.RunOnce(ctx)
			if err != nil {
				t.Fatal(err)
			}
			if res.Purged != tc.purged || res.Victims != 0 {
				t.Fatalf("%+v, want %d purged and nothing collected yet", res, tc.purged)
			}
			_, inTrash := f.FSM.GetInodeAny(ino)
			if inTrash == (tc.purged == 1) {
				t.Fatalf("root in the trash: %v", inTrash)
			}
			if ch, _ := f.FSM.GetChunk(c.Hash); (ch.TombstonedAt != 0) != (tc.purged == 1) {
				t.Fatalf("chunk after the pass: %+v", ch)
			}
			if tc.purged == 0 {
				return
			}
			// tombstoned at the purge, on the FSM clock: collected one grace later (L-201)
			f.Clk.Set(f.Clk.Now().Add(grace + time.Nanosecond))
			if res, err = gc.RunOnce(ctx); err != nil || res.Victims != 1 {
				t.Fatalf("after grace: %+v %v", res, err)
			}
		})
	}
}

// A backlog of due roots drains in batches of Limit inodes per entry.
//
// Invariant: I-157
func TestGC_PurgeTrash_DrainsInBatches(t *testing.T) {
	f := testharness.NewFabric(t, 3, 2, 1)
	fs := f.FS()
	const n = 7
	for i := range n {
		fh, err := fs.Create("/f" + string(rune('a'+i)))
		must(t, err)
		must(t, fh.Close())
		must(t, fs.Remove("/f"+string(rune('a'+i))))
	}
	if st := f.FSM.Stats(); st.TrashInodes != n {
		t.Fatalf("trash: %+v", st)
	}
	gc := repair.NewGC(f.FSM, f, f, f, f.Clk, time.Minute, nil)
	gc.Batch = 2
	before := f.Ops()
	purged, err := gc.PurgeTrash(ctx, f.Clk.Now().UnixNano()+1, 0)
	must(t, err)
	if purged != n || f.FSM.Stats().TrashInodes != 0 {
		t.Fatalf("purged %d, trash %+v", purged, f.FSM.Stats())
	}
	entries := 0
	for _, op := range f.Ops()[len(before):] {
		if op == meta.OpPurge {
			entries++
		}
	}
	if entries != (n+1)/2 {
		t.Fatalf("%d purge entries for %d roots in batches of 2", entries, n)
	}
}

// A purge and then a collection, through RunOnce as the leader's tick runs it.
//
// Lifecycle: L-201
func TestGC_PurgeThenCollect(t *testing.T) {
	f := testharness.NewFabric(t, 3, 2, 1)
	c, _ := trashedChunk(t, f)
	gc := repair.NewGC(f.FSM, f, f, f, f.Clk, time.Minute, nil)
	gc.Retention = time.Hour
	f.Clk.Set(f.Clk.Now().Add(time.Hour + time.Second))
	res, err := gc.RunOnce(ctx)
	must(t, err)
	if res.Purged != 1 || res.Victims != 0 {
		t.Fatalf("first pass: %+v", res)
	}
	f.Clk.Set(f.Clk.Now().Add(time.Minute + time.Second))
	res, err = gc.RunOnce(ctx)
	must(t, err)
	if res.Victims != 1 {
		t.Fatalf("second pass: %+v", res)
	}
	if _, ok := f.FSM.GetChunk(c.Hash); ok {
		t.Fatal("entry survived GC")
	}
}

// The retention purge is the node's own sweep: once users exist it proposes as System,
// which the FSM admits for a Purge, so the trash still empties after retention. Before,
// it proposed as nobody and every pass failed.
//
// Invariant: I-157, I-349
func TestGC_PurgeTrash_RetentionUnderUsers(t *testing.T) {
	f := testharness.NewFabric(t, 3, 2, 1)
	_, ino := trashedChunk(t, f)
	now := f.Clk.Now().UnixNano()
	f.MustApply(meta.Command{Op: meta.OpInvite, InviteOp: &meta.InviteOp{Bootstrap: true, Invite: meta.Invite{Hash: meta.HashCode("aaaaaaaaaa"), Name: "kim", Kind: meta.InviteUser, ExpiresAt: now + int64(time.Hour)}, Now: now}})
	f.MustApply(meta.Command{Op: meta.OpUser, UserOp: &meta.UserOp{Action: meta.UserCreate, Name: "kim", Code: "aaaaaaaaaa", Key: [32]byte{1}, Now: now}})
	if !f.FSM.Enforcing() {
		t.Fatal("users exist but the FSM does not enforce")
	}
	f.Clk.Set(f.Clk.Now().Add(31 * 24 * time.Hour))
	gc := repair.NewGC(f.FSM, f, f, f, f.Clk, time.Minute, nil)
	gc.Retention = 30 * 24 * time.Hour
	res, err := gc.RunOnce(ctx)
	if err != nil || res.Purged != 1 {
		t.Fatalf("%+v %v, want the root purged", res, err)
	}
	if _, inTrash := f.FSM.GetInodeAny(ino); inTrash {
		t.Fatal("root still in the trash")
	}
}
