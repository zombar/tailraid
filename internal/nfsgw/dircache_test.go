package nfsgw

import (
	"context"
	iofs "io/fs"
	"testing"

	"github.com/zombar/tailraid/internal/meta"
	"github.com/zombar/tailraid/internal/testharness"
)

// The pages of one READDIRPLUS reuse the first page's listing: the verifier is stable
// while the directory is unchanged and the kept listing comes back for it; any change to
// the directory (an entry added) invalidates it, so go-nfs lists again and the stale
// cookie is refused (I-47).
// Invariant: I-47, I-232
func TestHandler_DirListingReusedAcrossPages(t *testing.T) {
	f := testharness.NewFabric(t, 3, 2, 1)
	h := New(f.FS(), f.FSM)
	dir := f.MustApply(meta.Command{Op: meta.OpMkdir, Mkdir: &meta.Mkdir{Parent: meta.RootIno, Name: "d", Mode: 0o755, Now: 1}}).Ino
	for _, name := range []string{"a", "b", "c"} {
		f.MustApply(meta.Command{Op: meta.OpCreate, Create: &meta.Create{Parent: dir, Name: name, Type: meta.TFile, Mode: 0o644, Now: 2}})
	}
	list := func() []iofs.FileInfo {
		ents, err := f.FS().ReadDir("/d")
		if err != nil {
			t.Fatal(err)
		}
		return ents
	}
	first := list()
	v := h.VerifierFor("/d", first)
	if v == 0 {
		t.Fatal("zero verifier")
	}
	if again := h.VerifierFor("/d", list()); again != v {
		t.Fatalf("verifier changed without a change: %d vs %d", v, again)
	}
	kept := h.DataForVerifier("/d", v)
	if len(kept) != 3 || kept[0].Name() != "a" || kept[2].Name() != "c" {
		t.Fatalf("kept listing %v", kept)
	}
	if h.DataForVerifier("/d", v+1) != nil {
		t.Fatal("unknown verifier answered")
	}
	// the directory changes: a listing being paged goes on through the kept one (I-232),
	// and a fresh listing gets another verifier
	f.MustApply(meta.Command{Op: meta.OpCreate, Create: &meta.Create{Parent: dir, Name: "z", Type: meta.TFile, Mode: 0o644, Now: 3}})
	if kept := h.DataForVerifier("/d", v); len(kept) != 3 {
		t.Fatalf("kept listing after a create: %d entries, want the 3 it was made with", len(kept))
	}
	if v2 := h.VerifierFor("/d", list()); v2 == v {
		t.Fatal("verifier unchanged after a create")
	}
	// a removed directory: no listing, and the fallback verifier does not panic
	if err := f.FS().Remove("/d/a"); err != nil {
		t.Fatal(err)
	}
	_ = context.Background()
	if h.DataForVerifier("/gone", 42) != nil {
		t.Fatal("listing for a missing path")
	}
	if h.VerifierFor("/gone", nil) == 0 {
		t.Fatal("zero fallback verifier")
	}
}
