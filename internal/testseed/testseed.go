// Package testseed is the one seed helper for randomized tests (docs/09 §5): every such
// test takes -seed and prints the seed it used so a failure can be replayed.
package testseed

import (
	"flag"
	"math/rand"
	"testing"
	"time"
)

var seedFlag = flag.Int64("seed", 0, "seed for randomized tests (0 = time-based)")

// Seed returns a generator seeded from -seed, or from the clock when the flag is 0, and
// logs the seed with the command line that replays it.
func Seed(t testing.TB) *rand.Rand {
	t.Helper()
	s := *seedFlag
	if s == 0 {
		s = time.Now().UnixNano()
	}
	t.Logf("seed=%d (re-run with -seed=%d)", s, s)
	return rand.New(rand.NewSource(s)) //nolint:gosec // test randomness
}
