package chunk

import "fmt"

// A device's shard budget derives from its data disk (docs/01 §Node budget): what the
// store holds plus what the disk can still give, minus a reserve that keeps the volume
// from driving the disk to ENOSPC. Pinning it through node.capacity bypasses all of this.
const (
	BudgetReserveMin  = uint64(8 << 30) // never budget the last 8 GiB
	BudgetReserveFrac = 10              // nor the last tenth of the filesystem
	BudgetStepMin     = uint64(1 << 30) // a change smaller than this (or 5 %) is noise
	BudgetStepFrac    = 20
)

// Budget is used + free − reserve, floored at 0, with reserve = max(total/10, 8 GiB).
func Budget(used, free, total uint64) uint64 {
	reserve := max(total/BudgetReserveFrac, BudgetReserveMin)
	avail := used + free
	if avail <= reserve {
		return 0
	}
	return avail - reserve
}

// SharedBudget is the budget of each of n targets whose stores share one filesystem: the
// Budget of their summed used bytes and the filesystem's free and total, divided equally,
// so the targets never promise more than the disk can give (docs/14 §3, I-91).
func SharedBudget(used, free, total uint64, n int) uint64 {
	if n <= 0 {
		return 0
	}
	return Budget(used, free, total) / uint64(n)
}

// BudgetChanged reports whether a re-derived budget moved enough to be worth a Raft
// proposal: more than max(5 % of the old value, 1 GiB) either way.
func BudgetChanged(old, cur uint64) bool {
	step := max(old/BudgetStepFrac, BudgetStepMin)
	if cur > old {
		return cur-old > step
	}
	return old-cur > step
}

// Disk reports the store's bytes and its disk's free and total bytes (the budget's inputs).
func (s *Store) Disk() (used, free, total uint64, err error) {
	free, total, err = s.fsys.Free(s.root)
	if err != nil {
		return 0, 0, 0, fmt.Errorf("chunk: measuring %s: %w", s.root, err)
	}
	return uint64(max(s.Used(), 0)), free, total, nil
}

// Root is the store's directory.
func (s *Store) Root() string { return s.root }

// Rebudget measures the data disk, derives the budget and sets the store's cap to it.
func (s *Store) Rebudget() (uint64, error) {
	used, free, total, err := s.Disk()
	if err != nil {
		return 0, err
	}
	b := Budget(used, free, total)
	s.SetCapacity(int64(b))
	return b, nil
}
