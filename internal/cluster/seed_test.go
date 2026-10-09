package cluster

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/zombar/tailraid/internal/meta"
)

// The join's member seed round-trips through the data directory, keeps only device rows
// with an address, is absent without error before a join, and is gone after Forget.
//
// Invariant: I-92
func TestMemberSeed_RoundTrip(t *testing.T) {
	dir := t.TempDir()
	if got, err := LoadMemberSeed(dir); err != nil || got != nil {
		t.Fatalf("before a join: %v %v", got, err)
	}
	members := []meta.Node{
		{Idx: 1, ID: "leader", Kind: meta.KindDevice, Addr: "100.64.0.11", TSStableID: "ts-a", State: meta.Active},
		{Idx: 2, ID: "disk", Kind: meta.KindDisk, Host: 1},
		{Idx: 3, ID: "gone", Kind: meta.KindDevice, Addr: "100.64.0.9", State: meta.Removed},
		{Idx: 4, ID: "noaddr", Kind: meta.KindDevice},
	}
	if err := WriteMemberSeed(dir, members); err != nil {
		t.Fatal(err)
	}
	got, err := LoadMemberSeed(dir)
	if err != nil || len(got) != 1 || got[0].ID != "leader" || got[0].TSStableID != "ts-a" {
		t.Fatalf("seed: %+v %v", got, err)
	}
	if err := ForgetMemberSeed(dir); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dir, seedFile)); err == nil {
		t.Fatal("seed file still there after Forget")
	}
	if err := ForgetMemberSeed(dir); err != nil {
		t.Fatalf("forgetting twice: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, seedFile), []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadMemberSeed(dir); err == nil {
		t.Fatal("a corrupt seed must be an error, not an empty table")
	}
}
