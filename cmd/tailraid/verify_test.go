package main

import (
	"testing"
	"time"

	"github.com/zombar/tailraid/internal/soak"
)

// verify runs the quick profile unless --full names the long one; either way the
// cluster's repair knobs and chunk size replace the compose rig's (docs/09 §6).
func TestVerifyProfile_Table(t *testing.T) {
	for _, c := range []struct {
		name      string
		full      bool
		chunk     int
		wantName  string
		wantLarge int
		wantChunk int
	}{
		{"quick", false, 0, "live", soak.Quick().LargeFile, 0},
		{"full", true, 0, "live-full", soak.Full().LargeFile, 0},
		{"chunk override", true, 64 << 10, "live-full", soak.Full().LargeFile, 64 << 10},
	} {
		t.Run(c.name, func(t *testing.T) {
			p := verifyProfile(c.full, c.chunk, 7*time.Second, 9*time.Second)
			if p.Name != c.wantName || p.LargeFile != c.wantLarge || p.ChunkSize != c.wantChunk {
				t.Fatalf("profile %+v", p)
			}
			if p.Grace != 7*time.Second || p.FillAfter != 9*time.Second {
				t.Fatalf("repair knobs not taken from the cluster: %+v", p)
			}
		})
	}
}
