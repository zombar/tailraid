package chunk

import "testing"

// Invariant: I-91
func TestBudget_PerDisk_SharedFilesystem(t *testing.T) {
	const total = 100 * gib // reserve 10 GiB
	cases := []struct {
		name       string
		used, free uint64
		n          int
		want       uint64
	}{
		{"no targets", 0, 50 * gib, 0, 0},
		{"one target: the plain budget", 0, 50 * gib, 1, 40 * gib},
		{"two targets split it", 0, 50 * gib, 2, 20 * gib},
		{"three targets, rounding down", 0, 10*gib + 3, 3, 1},
		{"used by all counts once", 6 * gib, 44 * gib, 2, 20 * gib},
		{"nothing past the reserve", 0, 10 * gib, 4, 0},
	}
	for _, c := range cases {
		got := SharedBudget(c.used, c.free, total, c.n)
		if got != c.want {
			t.Errorf("%s: SharedBudget = %d, want %d", c.name, got, c.want)
		}
		if c.n > 0 && got*uint64(c.n) > Budget(c.used, c.free, total) {
			t.Errorf("%s: %d targets × %d exceed the filesystem's budget", c.name, c.n, got)
		}
	}
}
