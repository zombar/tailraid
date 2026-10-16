package node

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"testing"
	"time"

	"github.com/zombar/tailraid/internal/meta"
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

// fabricUsers makes kim the first admin and lee a plain user on the fabric.
func fabricUsers(t *testing.T, f *testharness.Fabric) (kim, lee meta.UserIdx) {
	t.Helper()
	now := f.Clk.Now().UnixNano()
	f.MustApply(meta.Command{Op: meta.OpInvite, InviteOp: &meta.InviteOp{Bootstrap: true, Invite: meta.Invite{Hash: meta.HashCode("aaaaaaaaaa"), Name: "kim", Kind: meta.InviteUser, ExpiresAt: now + int64(time.Hour)}, Now: now}})
	kim = meta.UserIdx(f.MustApply(meta.Command{Op: meta.OpUser, UserOp: &meta.UserOp{Action: meta.UserCreate, Name: "kim", Code: "aaaaaaaaaa", Key: [32]byte{1}, Now: now}}).Ino)
	f.MustApply(meta.Command{Op: meta.OpInvite, Actor: kim, InviteOp: &meta.InviteOp{Invite: meta.Invite{Hash: meta.HashCode("bbbbbbbbbb"), Name: "lee", Kind: meta.InviteUser, ExpiresAt: now + int64(time.Hour)}, Now: now}})
	lee = meta.UserIdx(f.MustApply(meta.Command{Op: meta.OpUser, UserOp: &meta.UserOp{Action: meta.UserCreate, Name: "lee", Code: "bbbbbbbbbb", Key: [32]byte{2}, Now: now}}).Ino)
	return kim, lee
}

// Once users exist the restore ops write as the user who signed them: an admin restores
// a tree, copies a version and reverts a file; a plain user without a role there is
// refused by the FSM and nothing lands. Before, they wrote through the node's System
// view, which the FSM admits only under /.tailraid, so an admin's `snapshot restore`
// and `versions --copy` failed with "permission denied" (2026-10-15).
//
// Invariant: I-348, I-367
func TestAdminOps_RestoreUnderUsers(t *testing.T) {
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
	kim, lee := fabricUsers(t, f)
	jobs := runJobs(t, f, fs, src)
	a := &adminOps{fs: fs, restores: jobs}
	ctx := context.Background()
	reply, err := a.restoreOp(ctx, rpc.AdminOp{Op: "snapshot_restore", Actor: lee, Mirror: "pike/m1", Name: "s1", Path: "/docs/report.txt", To: "/old"})
	if err != nil {
		t.Fatal(err)
	}
	if j := awaitJob(t, jobs, reply, rpc.RestoreFailed); j.Error == "" { // the job runs as lee, who holds no role there
		t.Fatal("a plain user's refused restore says nothing")
	}
	if _, err := f.FSM.Resolve([]string{"old", "report.txt"}); err == nil {
		t.Fatal("the refused restore left a file")
	}
	for _, op := range []rpc.AdminOp{
		{Op: "snapshot_restore", Actor: kim, Mirror: "pike/m1", Name: "s1", Path: "/docs/report.txt", To: "/old"},
		{Op: "versions_copy", Actor: kim, Mirror: "pike/m1", Name: "s1", Path: "/docs/report.txt", As: "copy.txt"},
		{Op: "versions_revert", Actor: kim, Mirror: "pike/m1", Name: "s1", Path: "/docs/report.txt"},
	} {
		reply, err := a.restoreOp(ctx, op)
		if err != nil {
			t.Fatalf("%s by an admin: %v", op.Op, err)
		}
		if op.Op == "snapshot_restore" {
			awaitJob(t, jobs, reply, rpc.RestoreDone)
		}
	}
	for _, p := range []string{"/old/report.txt", "/docs/copy.txt", "/docs/report.txt"} {
		if got := fabricRead(t, fs, p); !bytes.Equal(got, old) {
			t.Fatalf("%s holds %q", p, got)
		}
	}
}

// The restore and versions ops run on the node asked, through its vfs, a snapshot restore
// as a job on the mirror's host: to the same path (refused when it exists), under --to, as --as; versions lists, reverts
// and copies; a ref names the mirror by its row name or its directory (docs/12 §11).
//
// Invariant: I-186, I-188, I-363
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
	jobs := runJobs(t, f, fs, src)
	a := &adminOps{fs: fs, restores: jobs}
	ctx := context.Background()
	do := func(op rpc.AdminOp) (any, error) {
		reply, err := a.restoreOp(ctx, op)
		if err == nil && op.Op == "snapshot_restore" { // a job on the mirror's host, this one (docs/16 §6)
			awaitJob(t, jobs, reply, rpc.RestoreDone)
		}
		return reply, err
	}

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

// A restore's reply lists at most ReplyPaths skipped and refused paths, so a re-run over
// a large tree (every file skipped) fits the client's reply bound; the counts are whole.
func TestRestoreReply_BoundsPaths(t *testing.T) {
	for _, n := range []int{0, 1, ReplyPaths - 1, ReplyPaths, ReplyPaths + 1} {
		paths := make([]string, n)
		for i := range paths {
			paths[i] = fmt.Sprintf("/f%d", i)
		}
		reply := restoreReply("/x", vfs.RestoreReport{Files: 3, Skipped: paths, Refused: paths})
		rep := reply["report"].(vfs.RestoreReport)
		want := min(n, ReplyPaths)
		if len(rep.Skipped) != want || len(rep.Refused) != want || rep.Files != 3 {
			t.Fatalf("n=%d: report %d skipped, %d refused, %d files", n, len(rep.Skipped), len(rep.Refused), rep.Files)
		}
		if reply["skipped_count"] != n || reply["refused_count"] != n || reply["path"] != "/x" {
			t.Fatalf("n=%d: reply %v %v %v", n, reply["skipped_count"], reply["refused_count"], reply["path"])
		}
		if n > 0 && rep.Skipped[0] != "/f0" {
			t.Fatalf("n=%d: first skipped %q", n, rep.Skipped[0])
		}
	}
}
