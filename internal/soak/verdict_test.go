package soak

import (
	"errors"
	"testing"
)

// The m+1 corruption check must not pass by accident: too few corruptions is a failure,
// wrong bytes are a failure, and only a reader that may hold the chunk in its cache may
// serve it exact.
func TestPhase_CorruptMPlusOne_FailsIfUnderCounted(t *testing.T) {
	eio := errors.New("EIO")
	cases := []struct {
		name       string
		corrupted  int
		coldReader bool
		err        error
		exact      bool
		ok         bool
	}{
		{"too few corruptions, clean error", 1, true, eio, false, false},
		{"m+1 corrupt, cold reader, clean error", 2, true, eio, false, true},
		{"m+1 corrupt, cold reader, exact bytes", 2, true, nil, true, false},
		{"m+1 corrupt, cached reader, exact bytes", 2, false, nil, true, true},
		{"m+1 corrupt, wrong bytes", 2, false, nil, false, false},
		{"more than m+1 corrupt, clean error", 3, true, eio, false, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			ok, text := corruptionVerdict(c.corrupted, 1, c.coldReader, c.err, c.exact)
			if ok != c.ok {
				t.Fatalf("ok=%v (%s), want %v", ok, text, c.ok)
			}
		})
	}
}

// Stored bytes can shrink between two samples (GC, migration); the delta is signed.
func TestPhase_StorageShrink_NoWrap(t *testing.T) {
	if d := storedDelta(1000, 900); d != -100 {
		t.Fatalf("shrink delta %d", d)
	}
	if d := storedDelta(900, 1000); d != 100 {
		t.Fatalf("growth delta %d", d)
	}
	if d := storedDelta(0, 0); d != 0 {
		t.Fatalf("zero delta %d", d)
	}
}
