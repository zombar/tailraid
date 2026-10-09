// Copyright (c) 2026, go-filesystems
// SPDX-License-Identifier: BSD-3-Clause

package smb_test

import (
	"sync/atomic"
	"testing"

	fssmb "github.com/go-filesystems/smb"
)

// What a client is told a share's size and free space are, read through
// go-smb2's Statfs (FileFsFullSizeInformation), for each way of saying it.
func TestShareCapacityAsAClientReadsIt(t *testing.T) {
	addr, srv := serve(t)
	var avail atomic.Uint64
	avail.Store(24 << 20)
	shares := map[string][]fssmb.ShareOption{
		"placeholder": nil,
		"fixed":       {fssmb.WithCapacity(64<<20, 16<<20)},
		"live":        {fssmb.WithCapacity(1, 1), fssmb.WithCapacityFunc(func() (uint64, uint64) { return 32 << 20, avail.Load() })},
		"lastwins":    {fssmb.WithCapacityFunc(func() (uint64, uint64) { return 1 << 40, 0 }), fssmb.WithCapacity(64<<20, 16<<20)},
		"nilfunc":     {fssmb.WithCapacity(64<<20, 16<<20), fssmb.WithCapacityFunc(nil)},
		"unknown":     {fssmb.WithCapacity(0, 5<<20)},
		"overfull":    {fssmb.WithCapacity(8<<20, 9<<20)},
	}
	for name, opts := range shares {
		if err := srv.Share(name, newMemFS(false), opts...); err != nil {
			t.Fatal(err)
		}
	}
	s, done := dial(t, addr, "alice", "hunter2")
	defer done()
	statfs := func(share string) (total, free, avail uint64) {
		t.Helper()
		fs, err := s.Mount(share)
		if err != nil {
			t.Fatalf("%s: %v", share, err)
		}
		defer fs.Umount()
		fi, err := fs.Statfs(".")
		if err != nil {
			t.Fatalf("%s: statfs: %v", share, err)
		}
		if fi.BlockSize() != 4096 {
			t.Fatalf("%s: block size %d, want 4096", share, fi.BlockSize())
		}
		bs := fi.BlockSize()
		return fi.TotalBlockCount() * bs, fi.FreeBlockCount() * bs, fi.AvailableBlockCount() * bs
	}
	for _, tc := range []struct {
		share        string
		total, avail uint64
	}{
		{"placeholder", 4 << 30, 2 << 30},
		{"fixed", 64 << 20, 16 << 20},
		{"live", 32 << 20, 24 << 20},
		{"lastwins", 64 << 20, 16 << 20},
		{"nilfunc", 4 << 30, 2 << 30},
		{"unknown", 4 << 30, 2 << 30},
		{"overfull", 8 << 20, 8 << 20},
	} {
		total, free, av := statfs(tc.share)
		if total != tc.total || free != tc.avail || av != tc.avail {
			t.Errorf("%s: total %d free %d available %d, want %d/%d/%d", tc.share, total, free, av, tc.total, tc.avail, tc.avail)
		}
	}
	// Asked again, not remembered: the free space a write used is gone.
	avail.Store(1 << 20)
	if total, free, _ := statfs("live"); total != 32<<20 || free != 1<<20 {
		t.Errorf("live, after a write: total %d free %d, want %d/%d", total, free, 32<<20, 1<<20)
	}
}
