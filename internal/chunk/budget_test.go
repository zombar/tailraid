package chunk

import (
	"crypto/sha256"
	"errors"
	"path/filepath"
	"testing"

	"github.com/zombar/tailraid/internal/sysfs"
)

const gib = uint64(1 << 30)

// Invariant: I-74
func TestStore_Budget_Boundaries(t *testing.T) {
	const total = 100 * gib // reserve = 10 GiB (the tenth wins over the 8 GiB floor)
	reserve := 10 * gib
	cases := []struct {
		name            string
		used, free, tot uint64
		want            uint64
	}{
		{"empty disk, nothing free", 0, 0, total, 0},
		{"one byte free", 0, 1, total, 0},
		{"reserve-1 free", 0, reserve - 1, total, 0},
		{"reserve free", 0, reserve, total, 0},
		{"reserve+1 free", 0, reserve + 1, total, 1},
		{"used counts toward the budget", 5 * gib, reserve, total, 5 * gib},
		{"used alone below the reserve", 3 * gib, 0, total, 0},
		{"half free", 10 * gib, 40 * gib, total, 40 * gib},
		{"small disk: the 8 GiB floor wins", 0, 10 * gib, 20 * gib, 2 * gib},
		{"tiny disk: everything is reserve", 1 * gib, 2 * gib, 4 * gib, 0},
	}
	for _, c := range cases {
		if got := Budget(c.used, c.free, c.tot); got != c.want {
			t.Errorf("%s: Budget(%d, %d, %d) = %d, want %d", c.name, c.used, c.free, c.tot, got, c.want)
		}
	}
}

// Invariant: I-74
func TestStore_BudgetChanged_Hysteresis(t *testing.T) {
	cases := []struct {
		old, cur uint64
		want     bool
	}{
		{100 * gib, 100 * gib, false},
		{100 * gib, 100*gib + 5*gib, false},    // exactly 5 %: not past it
		{100 * gib, 100*gib + 5*gib + 1, true}, // past 5 %
		{100 * gib, 95 * gib, false},
		{100 * gib, 95*gib - 1, true},
		{10 * gib, 10*gib + gib, false}, // the 1 GiB floor: exactly at it
		{10 * gib, 10*gib + gib + 1, true},
		{10 * gib, 9*gib + 1, false},
		{0, gib, false},
		{0, gib + 1, true},
		{gib + 1, 0, true},
	}
	for _, c := range cases {
		if got := BudgetChanged(c.old, c.cur); got != c.want {
			t.Errorf("BudgetChanged(%d, %d) = %v, want %v", c.old, c.cur, got, c.want)
		}
	}
}

// freeFS reports a fixed free space on top of the real filesystem.
type freeFS struct {
	sysfs.FS
	free, total uint64
}

func (f freeFS) Free(string) (uint64, uint64, error) { return f.free, f.total, nil }

// Invariant: I-74
func TestStore_SetCapacity_CapFollows(t *testing.T) {
	unit := int64(HeaderSize + 100)
	s, err := Open(sysfs.OS{}, filepath.Join(t.TempDir(), "shards"), 3*unit)
	if err != nil {
		t.Fatal(err)
	}
	put := func(i byte) error {
		payload := make([]byte, 100)
		payload[0] = i
		_, err := s.PutVerified(Key(sha256.Sum256(payload), 1, 0, 0), payload, sha256.Sum256(payload))
		return err
	}
	for i := range byte(3) {
		if err := put(i); err != nil {
			t.Fatal(err)
		}
	}
	if err := put(3); !errors.Is(err, ErrFull) {
		t.Fatalf("fourth put at the cap: %v", err)
	}
	// lowering the cap below what is held keeps the held shards and refuses the next put
	s.SetCapacity(2 * unit)
	if s.Used() != 3*unit || s.Capacity() != 2*unit {
		t.Fatalf("after lowering: used %d cap %d", s.Used(), s.Capacity())
	}
	if err := put(4); !errors.Is(err, ErrFull) {
		t.Fatalf("put above a lowered cap: %v", err)
	}
	// raising it lets the next put through; 0 means unlimited
	s.SetCapacity(4 * unit)
	if err := put(5); err != nil {
		t.Fatal(err)
	}
	s.SetCapacity(0)
	if err := put(6); err != nil {
		t.Fatalf("unlimited: %v", err)
	}
	// Rebudget derives from the disk and sets the cap: 20 GiB disk, 10 GiB free → 2 GiB
	s.fsys = freeFS{FS: sysfs.OS{}, free: 10 * gib, total: 20 * gib}
	b, err := s.Rebudget()
	if err != nil || b != 2*gib+uint64(s.Used()) || s.Capacity() != int64(b) {
		t.Fatalf("Rebudget = %d (cap %d) %v; used %d", b, s.Capacity(), err, s.Used())
	}
}
