package smbgw_test

import (
	"bytes"
	"io"
	"os"
	"testing"
	"time"

	"github.com/zombar/tailraid/internal/testharness"
)

// On the share, \.snapshots\<mirror>\<name>\ reads the snapshot's bytes and refuses every
// change with STATUS_ACCESS_DENIED (docs/15, I-185).
//
// Invariant: I-185
func TestSMB_SnapshotsDir_ReadOnly(t *testing.T) {
	g := startGateway(t)
	old := []byte("the file as the snapshot holds it")
	g.create("f")
	must(t, g.writeAt("/f", 0, old))
	must(t, g.f.Flusher().FlushAll(ctx))
	src := testharness.NewMirrors()
	m := g.f.AddMirror(src, "host/m1")
	g.f.TakeSnapshot(src, m, "s1")
	g.fs.SetMirrors(src)
	must(t, g.writeAt("/f", 0, []byte("CHANGED")))

	const file = `.snapshots\host-m1\s1\f`
	if got := g.readAll(file, len(old)+8); !bytes.Equal(got, old) {
		t.Fatalf("read %q, want %q", got, old)
	}
	ents, err := g.sh.ReadDir(`.snapshots\host-m1`)
	must(t, err)
	if len(ents) != 1 || ents[0].Name() != "s1" || !ents[0].IsDir() {
		t.Fatalf("mirror listing %v", ents)
	}
	denied := map[string]error{
		"mkdir":  g.sh.Mkdir(`.snapshots\host-m1\s1\d`, 0o755),
		"remove": g.sh.Remove(file),
		"rename": g.sh.Rename(file, `.snapshots\host-m1\s1\g`),
		"times":  g.sh.Chtimes(file, time.Unix(1, 0), time.Unix(1, 0)),
	}
	if w, err := g.sh.OpenFile(file, os.O_RDWR, 0); err == nil {
		_, err = w.WriteAt([]byte("x"), 0)
		denied["write"] = err
		_ = w.Close()
	} else {
		denied["write"] = err
	}
	if w, err := g.sh.Create(`.snapshots\host-m1\s1\new`); err == nil {
		_ = w.Close()
		denied["create"] = nil
	} else {
		denied["create"] = err
	}
	for op, err := range denied {
		if ntStatus(err) != statusAccessDenied {
			t.Errorf("%s under .snapshots: %v (status %#x), want STATUS_ACCESS_DENIED", op, err, ntStatus(err))
		}
	}
	r, err := g.sh.Open(file)
	must(t, err)
	got, err := io.ReadAll(r)
	_ = r.Close()
	if err != nil || !bytes.Equal(got, old) {
		t.Fatalf("after the refusals: %q %v", got, err)
	}
}
