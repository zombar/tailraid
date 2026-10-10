package soak

import (
	"testing"
	"time"
)

// The full profile is the quick one with a larger workload and the same timing: both
// `cmd/soak --profile full` and `tailraid verify --full` run it (docs/09 §6).
func TestProfiles_FullExceedsQuick(t *testing.T) {
	q, f := Quick(), Full()
	if q.Name != "quick" || f.Name != "full" {
		t.Fatalf("names: %q, %q", q.Name, f.Name)
	}
	for _, c := range []struct {
		name       string
		quick, got int
	}{
		{"BigFile", q.BigFile, f.BigFile},
		{"LargeFile", q.LargeFile, f.LargeFile},
		{"SmallFiles", q.SmallFiles, f.SmallFiles},
		{"ChurnOps", q.ChurnOps, f.ChurnOps},
		{"LeaderKills", q.LeaderKills, f.LeaderKills},
	} {
		if c.got <= c.quick {
			t.Errorf("%s: full %d is not above quick %d", c.name, c.got, c.quick)
		}
	}
	if f.LargeFile != 1<<30 {
		t.Errorf("full LargeFile = %d, want 1 GiB", f.LargeFile)
	}
	for _, c := range []struct {
		name       string
		quick, got time.Duration
	}{
		{"FlushIdle", q.FlushIdle, f.FlushIdle},
		{"FillAfter", q.FillAfter, f.FillAfter},
		{"Grace", q.Grace, f.Grace},
		{"SampleEvery", q.SampleEvery, f.SampleEvery},
		{"RestartSettle", q.RestartSettle, f.RestartSettle},
		{"PhaseTimeout", q.PhaseTimeout, f.PhaseTimeout},
	} {
		if c.got != c.quick {
			t.Errorf("%s: full %v differs from quick %v", c.name, c.got, c.quick)
		}
	}
	if f.ChunkSize != q.ChunkSize {
		t.Errorf("ChunkSize: full %d differs from quick %d", f.ChunkSize, q.ChunkSize)
	}
}
