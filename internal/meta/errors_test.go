package meta

import (
	"errors"
	"testing"
)

// Invariant: I-26
func TestErrorCode_RoundTrip(t *testing.T) {
	all := []error{
		ErrExist, ErrNotExist, ErrNotEmpty, ErrIsDir, ErrNotDir, ErrLoop, ErrConflict,
		ErrUnknownChunk, ErrNeedTailRewrite, ErrBadCommand, ErrNoSuchNode, ErrNodeNotEmpty, ErrPolicy,
		ErrNameTooLong, ErrProtected, ErrInTrash, ErrCodec,
	}
	seen := map[uint8]bool{}
	for _, e := range all {
		c := ErrorCode(e)
		if c == 0 {
			t.Errorf("%v has code 0", e)
		}
		if seen[c] {
			t.Errorf("%v duplicate code %d", e, c)
		}
		seen[c] = true
		got, ok := ErrorFromCode(c)
		if !ok || !errors.Is(got, e) {
			t.Errorf("ErrorFromCode(%d) = %v,%v want %v", c, got, ok, e)
		}
	}
	if ErrorCode(nil) != 0 {
		t.Error("nil must be code 0")
	}
	if got, ok := ErrorFromCode(0); got != nil || !ok {
		t.Error("code 0 must decode to nil")
	}
	if _, ok := ErrorFromCode(200); ok {
		t.Error("unknown code accepted")
	}
	if ErrorCode(errors.New("other")) != 0 {
		t.Error("non-sentinel must map to 0")
	}
}
