package main

import (
	"errors"
	"fmt"
	"os"
	"strings"
	"testing"
)

// A timed-out dial to the node's own address points at a host firewall and the log; a
// refused one only asks whether serve runs.
// Invariant: I-335
func TestUnreachableHint_Table(t *testing.T) {
	for _, tc := range []struct {
		name     string
		err      error
		firewall bool
	}{
		{"deadline", fmt.Errorf("rpc: %w", os.ErrDeadlineExceeded), true},
		{"flattened timeout", errors.New(`Get "http://100.79.112.1:20481/v1/status": dial tcp 100.79.112.1:0->100.79.112.1:20481: i/o timeout`), true},
		{"refused", errors.New("dial tcp 127.0.0.1:20481: connect: connection refused"), false},
	} {
		got := unreachableHint(tc.err)
		if !strings.Contains(got, "tailraid serve") || strings.Contains(got, "firewall") != tc.firewall {
			t.Errorf("%s: %q, firewall hint want %v", tc.name, got, tc.firewall)
		}
	}
}
