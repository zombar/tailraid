package rpc

import (
	"bytes"
	"crypto/sha256"
	"errors"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"github.com/zombar/tailraid/internal/chunk"
	"github.com/zombar/tailraid/internal/meta"
	"github.com/zombar/tailraid/internal/sysfs"
)

type diskFence struct {
	fakeFence
	onDisk map[chunk.ShardKey]bool
}

func (f diskFence) PlacedOn(_ meta.NodeIdx, k chunk.ShardKey) bool { return f.onDisk[k] }

// withDisk gives the env an added disk "d1" (target 2), served under /v1/disks/d1/.
func (e *env) withDisk(t *testing.T) *chunk.Store {
	t.Helper()
	d, err := chunk.Open(sysfs.OS{}, filepath.Join(t.TempDir(), "d1"), 1<<30)
	if err != nil {
		t.Fatal(err)
	}
	e.srv.o.Disk = func(id string) (ShardStore, meta.NodeIdx, bool) {
		if id == "d1" {
			return d, 2, true
		}
		return e.diskLookup(id)
	}
	e.srv.o.DiskHealth = func() []DiskHealth {
		return []DiskHealth{{ID: "d1", Used: d.Used(), Capacity: d.Capacity(), Free: d.Capacity() - d.Used(), OK: true}, {ID: "d2", Reason: "disk_id missing"}}
	}
	return d
}

// diskClient routes target 1 to the server's own store and targets 2 and 3 to its added
// disks "d1" (open) and "d9" (not open there), all over HTTP.
func (e *env) diskClient() *Client {
	c := NewClient(e.cid, func(idx meta.NodeIdx) (string, bool, bool) {
		if idx >= 1 && idx <= 3 {
			return e.addr, false, true
		}
		return "", false, false
	})
	c.SetDisks(func(idx meta.NodeIdx) (string, bool) {
		switch idx {
		case 1:
			return "d0", true
		case 2:
			return "d1", true
		case 3:
			return "d9", true
		}
		return "", false
	}, nil)
	return c
}

// Invariant: I-86, I-90
func TestChunkRPC_DiskPath_RoundTrip(t *testing.T) {
	e := newEnv(t)
	disk := e.withDisk(t)
	c := e.diskClient()
	for _, n := range []int{0, 1, (1 << 16) - 1, 1 << 16} {
		p := bytes.Repeat([]byte{byte(n), 3}, n/2+1)[:n]
		k := key(p, 1)
		if err := c.Put(ctx, 2, k, p); err != nil {
			t.Fatalf("put %d: %v", n, err)
		}
		if !disk.Has(k) || e.store.Has(k) {
			t.Fatalf("put %d landed on the wrong store", n)
		}
		if ok, err := c.Has(ctx, 2, k); err != nil || !ok {
			t.Fatalf("has %d: %v %v", n, ok, err)
		}
		if got, err := c.Get(ctx, 2, k); err != nil || !bytes.Equal(got, p) {
			t.Fatalf("get %d: %v", n, err)
		}
		if ok, _ := c.Has(ctx, 1, k); ok {
			t.Fatalf("disk 0 reports the added disk's shard")
		}
	}
	// a disk the host does not have open is unavailable, never "shard absent" (I-90)
	p := []byte("x")
	if err := c.Put(ctx, 3, key(p, 0), p); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("put to a closed disk: %v", err)
	}
	if _, err := c.Get(ctx, 3, key(p, 0)); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("get from a closed disk: %v", err)
	}
	if ok, err := c.Has(ctx, 3, key(p, 0)); ok || !errors.Is(err, ErrUnavailable) {
		t.Fatalf("head on a closed disk: %v %v", ok, err)
	}
	// /health lists every disk (the server's own d0 is replaced by withDisk's two)
	h, err := c.Health(ctx, e.addr)
	if err != nil || len(h.Disks) != 2 || !h.Disks[0].OK || h.Disks[1].OK {
		t.Fatalf("health: %+v %v", h.Disks, err)
	}
}

// Invariant: I-13, I-14
func TestChunkRPC_DiskDeleteFence(t *testing.T) {
	e := newEnv(t)
	disk := e.withDisk(t)
	c := e.diskClient()
	p := []byte("fenced")
	k := key(p, 2)
	if _, err := disk.PutVerified(k, p, sumOf(p)); err != nil {
		t.Fatal(err)
	}
	e.clk.Advance(time.Hour) // older than grace
	applied := strconv.FormatUint(e.app.AppliedIndex(), 10)
	del := func() int {
		resp, err := c.do(ctx, "DELETE", e.addr, "/v1/disks/d1/shards/"+string(k), nil, map[string]string{HeaderLogIndex: applied})
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		return resp.StatusCode
	}
	// a server without a fence refuses every delete
	e.srv.o.Fence = nil
	if st := del(); st != 412 {
		t.Fatalf("no fence: status %d", st)
	}
	f := diskFence{fakeFence: e.fence, onDisk: map[chunk.ShardKey]bool{k: true}}
	e.srv.o.Fence = f
	if st := del(); st != 412 || !disk.Has(k) {
		t.Fatalf("placed on the disk: status %d", st)
	}
	f.onDisk[k] = false
	if st := del(); st != 200 || disk.Has(k) {
		t.Fatalf("unplaced: status %d", st)
	}
}

// Invariant: I-86
func TestClient_LocalDiskRouting(t *testing.T) {
	dir := t.TempDir()
	own, _ := chunk.Open(sysfs.OS{}, filepath.Join(dir, "own"), 1<<30)
	d1, _ := chunk.Open(sysfs.OS{}, filepath.Join(dir, "d1"), 1<<30)
	c := NewClient([16]byte{1}, func(meta.NodeIdx) (string, bool, bool) { return "unused:1", true, true })
	c.SetDisks(func(idx meta.NodeIdx) (string, bool) {
		switch idx {
		case 1:
			return "own", true
		case 2:
			return "d1", true
		case 3:
			return "closed", true
		}
		return "", false
	}, func(id string) ShardStore {
		switch id {
		case "own":
			return own
		case "d1":
			return d1
		}
		return nil
	})
	// a device row (here 4) is never a target: unavailable, not "absent" (docs/14 §9)
	if _, err := c.Get(ctx, 4, key([]byte("x"), 0)); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("get from a device index: %v", err)
	}
	p := []byte("local")
	k := key(p, 0)
	if err := c.Put(ctx, 2, k, p); err != nil || !d1.Has(k) || own.Has(k) {
		t.Fatalf("local disk put: %v", err)
	}
	if got, err := c.Get(ctx, 2, k); err != nil || !bytes.Equal(got, p) {
		t.Fatalf("local disk get: %v", err)
	}
	if err := c.Put(ctx, 1, k, p); err != nil || !own.Has(k) {
		t.Fatalf("own store put: %v", err)
	}
	if err := c.Put(ctx, 3, k, p); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("closed local disk: %v", err)
	}
	if ok, err := c.Has(ctx, 3, k); ok || !errors.Is(err, ErrUnavailable) {
		t.Fatalf("closed local disk has: %v %v", ok, err)
	}
}

func sumOf(p []byte) [32]byte { return sha256.Sum256(p) }
