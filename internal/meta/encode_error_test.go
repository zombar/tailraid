package meta

import (
	"errors"
	"testing"
)

// A command without its payload, or with an unknown op, is ErrBadCommand from the
// encoder: the leader reports it, it does not panic on the proposer's mistake.
//
// Invariant: I-26
func TestEncodeCommand_BadCommandIsError(t *testing.T) {
	for _, c := range []Command{{Op: OpUnlink}, {Op: OpCommit}, {Op: Op(250)}} {
		b, err := EncodeCommand(c)
		if !errors.Is(err, ErrBadCommand) || b != nil {
			t.Errorf("op %d: %v %v, want ErrBadCommand and no bytes", c.Op, b, err)
		}
	}
	if _, err := EncodeCommand(Command{Op: OpUnlink, Unlink: &Unlink{Parent: 1, Name: "a"}}); err != nil {
		t.Fatalf("a good command: %v", err)
	}
}
