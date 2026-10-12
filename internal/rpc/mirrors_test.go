package rpc

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/zombar/tailraid/internal/meta"
	"github.com/zombar/tailraid/internal/mirror"
	"github.com/zombar/tailraid/internal/sysfs"
)

// withMirror gives the env mirror row 5, open on this host and holding one snapshot of
// a root with one file; it returns the mirror, the root tree's hash and the file's chunk.
func (e *env) withMirror(t *testing.T) (m *mirror.Mirror, root, obj meta.Hash, data []byte) {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "m1")
	if err := mirror.Create(sysfs.OS{}, dir, mirror.Marker{Format: mirror.Format, ClusterID: "c", MirrorID: "m1"}); err != nil {
		t.Fatal(err)
	}
	m, err := mirror.Open(sysfs.OS{}, dir, "c")
	if err != nil {
		t.Fatal(err)
	}
	data = []byte("bytes of a file as the snapshot saw it")
	obj = sha256.Sum256(data)
	st := &meta.State{Inodes: map[meta.Ino]*meta.Inode{
		meta.RootIno: {Ino: meta.RootIno, Type: meta.TDir, Children: map[string]meta.Ino{"f": 2}},
		2:            {Ino: 2, Gen: 1, Type: meta.TFile, Name: "f", Size: uint64(len(data)), Chunks: []meta.Hash{obj}},
	}}
	read := func(context.Context, meta.Hash) ([]byte, error) { return data, nil }
	man, err := m.WriteSnapshot(ctx, mirror.StateSource(st, read), uint64(meta.RootIno), mirror.Manifest{ID: 1, Name: "s", ChunkSize: 1 << 20})
	if err != nil {
		t.Fatal(err)
	}
	e.srv.o.Mirror = func(idx meta.NodeIdx) (*mirror.Mirror, bool) { return m, idx == 5 }
	return m, meta.Hash(man.Root), obj, data
}

// mirrorClient routes rows 5 and 6 to the server: 5 is open there, 6 is not.
func (e *env) mirrorClient() *Client {
	return NewClient(e.cid, func(idx meta.NodeIdx) (string, bool, bool) {
		if idx == 5 || idx == 6 {
			return e.addr, false, true
		}
		return "", false, false
	})
}

// A mirror's objects and trees are served to a member by row over the node RPC and
// verified against their names on arrival; a missing one is ErrNotFound, a mirror not
// open on the host is unavailable, a stranger is refused (docs/16 §6).
//
// Invariant: I-185
func TestMirrorRPC_ObjectsAndTrees(t *testing.T) {
	e := newEnv(t)
	m, root, obj, data := e.withMirror(t)
	c := e.mirrorClient()

	got, err := c.MirrorObject(ctx, 5, obj)
	if err != nil || !bytes.Equal(got, data) {
		t.Fatalf("object: %q %v", got, err)
	}
	tr, err := c.MirrorTree(ctx, 5, root)
	if err != nil {
		t.Fatal(err)
	}
	want, _ := m.Tree(root)
	if len(tr.Entries) != 1 || tr.Entries[0].Name != "f" || tr.Entries[0].Chunks[0] != want.Entries[0].Chunks[0] {
		t.Fatalf("tree: %+v", tr)
	}

	if _, err := c.MirrorObject(ctx, 5, meta.Hash{9}); !errors.Is(err, mirror.ErrNotFound) {
		t.Fatalf("missing object: %v", err)
	}
	if _, err := c.MirrorTree(ctx, 5, meta.Hash{9}); !errors.Is(err, mirror.ErrNotFound) {
		t.Fatalf("missing tree: %v", err)
	}
	if _, err := c.MirrorObject(ctx, 6, obj); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("mirror not open there: %v", err)
	}
	if _, err := c.MirrorObject(ctx, 7, obj); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("row without an address: %v", err)
	}

	// a damaged object on the disk is refused by the host, never served
	objPath := filepath.Join(m.Root(), "objects", filepathHex(obj)[:2], filepathHex(obj))
	raw, err := os.ReadFile(objPath)
	if err != nil {
		t.Fatal(err)
	}
	raw[len(raw)-1] ^= 1
	if err := os.WriteFile(objPath, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := c.MirrorObject(ctx, 5, obj); !errors.Is(err, mirror.ErrCorrupt) {
		t.Fatalf("damaged object: %v", err)
	}

	e.ts.Remove(memberIP)
	e.auth.Purge()
	if _, err := c.MirrorTree(ctx, 5, root); !errors.Is(err, ErrForbidden) {
		t.Fatalf("stranger: %v", err)
	}
}

func filepathHex(h meta.Hash) string { return hex.EncodeToString(h[:]) }
