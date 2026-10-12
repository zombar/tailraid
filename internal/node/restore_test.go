package node

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"testing"

	"github.com/zombar/tailraid/internal/rpc"
	"github.com/zombar/tailraid/internal/testharness"
	"github.com/zombar/tailraid/internal/vfs"
)

func fabricWrite(t *testing.T, fs *vfs.FS, name string, b []byte) {
	t.Helper()
	fh, err := fs.Create(name)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := fh.Write(b); err != nil {
		t.Fatal(err)
	}
	_ = fh.Close()
}

func fabricRead(t *testing.T, fs *vfs.FS, name string) []byte {
	t.Helper()
	fh, err := fs.Open(name)
	if err != nil {
		t.Fatalf("open %s: %v", name, err)
	}
	defer fh.Close()
	b, err := io.ReadAll(fh)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// The restore and versions ops run on the node asked, through its vfs: snapshot restore
// to the same path (refused when it exists), under --to, as --as; versions lists, reverts
// and copies; a ref names the mirror by its row name or its directory (docs/12 §11).
//
// Invariant: I-186, I-188
func TestAdminOps_RestoreAndVersions(t *testing.T) {
	f := testharness.NewFabric(t, 3, 2, 1)
	fs := f.FS()
	fs.SetFlusher(f.Flusher())
	old := []byte("the report as it was")
	fabricWrite(t, fs, "/docs/report.txt", old)
	if err := f.Flusher().FlushAll(context.Background()); err != nil {
		t.Fatal(err)
	}
	src := testharness.NewMirrors()
	m := f.AddMirror(src, "pike/m1")
	f.TakeSnapshot(src, m, "s1")
	fs.SetMirrors(src)
	fabricWrite(t, fs, "/docs/report.txt", []byte("rewritten since"))
	if err := f.Flusher().FlushAll(context.Background()); err != nil {
		t.Fatal(err)
	}
	a := &adminOps{fs: fs}
	ctx := context.Background()
	do := func(op rpc.AdminOp) (any, error) { return a.restoreOp(ctx, op) }

	if _, err := do(rpc.AdminOp{Op: "snapshot_restore", Mirror: "pike/m1", Name: "s1", Path: "docs/report.txt"}); !errors.Is(err, os.ErrExist) {
		t.Fatalf("restore over the live file: %v", err)
	}
	for _, c := range []struct {
		op   rpc.AdminOp
		want string
	}{
		{rpc.AdminOp{Op: "snapshot_restore", Mirror: "pike/m1", Name: "s1", Path: "/docs/report.txt", To: "/old"}, "/old/report.txt"},
		{rpc.AdminOp{Op: "snapshot_restore", Mirror: "pike-m1", Name: "s1", Path: "/docs/report.txt", As: "report (s1).txt"}, "/docs/report (s1).txt"},
		{rpc.AdminOp{Op: "versions_copy", Mirror: "pike/m1", Name: "s1", Path: "/docs/report.txt", As: "copy.txt"}, "/docs/copy.txt"},
	} {
		if _, err := do(c.op); err != nil {
			t.Fatalf("%+v: %v", c.op, err)
		}
		if got := fabricRead(t, fs, c.want); !bytes.Equal(got, old) {
			t.Fatalf("%s holds %q", c.want, got)
		}
	}
	raw, err := do(rpc.AdminOp{Op: "versions", Path: "docs/report.txt"})
	if err != nil {
		t.Fatal(err)
	}
	vr := raw.(VersionsReply)
	if vr.Path != "/docs/report.txt" || len(vr.Versions) != 2 || !vr.Versions[0].Current || len(vr.Versions[0].Snapshots) != 0 || vr.Versions[1].Snapshots[0].Mirror != "pike-m1" {
		t.Fatalf("versions %+v", vr)
	}
	if _, err := do(rpc.AdminOp{Op: "versions_revert", Mirror: "pike/m1", Name: "s1", Path: "/docs/report.txt"}); err != nil {
		t.Fatal(err)
	}
	if got := fabricRead(t, fs, "/docs/report.txt"); !bytes.Equal(got, old) {
		t.Fatalf("after revert %q", got)
	}
	for _, bad := range []rpc.AdminOp{
		{Op: "versions_revert", Path: "/docs/report.txt"},
		{Op: "versions_copy", Mirror: "pike/m1", Name: "s1", Path: "/docs/report.txt"},
		{Op: "versions_copy", Mirror: "pike/m1", Name: "s1", Path: "/docs/report.txt", As: "a/b"},
		{Op: "snapshot_restore", Mirror: "pike/m1", Name: "s1", Path: "/docs/report.txt", As: "x/y"},
		{Op: "snapshot_restore", Mirror: "pike/m1", Name: "nope", Path: "/docs/report.txt", As: "z"},
	} {
		if _, err := do(bad); err == nil {
			t.Fatalf("%+v succeeded", bad)
		}
	}
}
