package nfsgw_test

import (
	"os"
	"sync"
	"testing"

	"github.com/zombar/tailraid/internal/meta"
)

// A CREATE carrying a mode makes the file with that mode in its one Create proposal:
// no SetAttr follows it, UNCHECKED or GUARDED (a copy's file lands whole, docs/04 §6).
// Invariant: I-416
func TestNFS_CreateMode_NoSetAttr(t *testing.T) {
	g := startGateway(t)
	if _, err := g.tgt.Mkdir("/d", 0o755); err != nil {
		t.Fatal(err)
	}
	var mu sync.Mutex
	ops := map[meta.Op]int{}
	g.f.FSM.SetApplyHook(func(cmd meta.Command, _ uint64, _ meta.Result) {
		mu.Lock()
		ops[cmd.Op]++
		mu.Unlock()
	})
	t.Cleanup(func() { g.f.FSM.SetApplyHook(nil) })
	for _, tc := range []struct {
		name string
		how  uint32
	}{{"unchecked", 0}, {"guarded", 1}} {
		if st := g.createHow("/d", tc.name, tc.how); st != 0 {
			t.Fatalf("%s: CREATE status %d", tc.name, st)
		}
		fi, err := g.fs.Stat("/d/" + tc.name)
		if err != nil {
			t.Fatal(err)
		}
		if fi.Mode().Perm() != os.FileMode(0o644) {
			t.Errorf("%s: mode %v, want 0644", tc.name, fi.Mode().Perm())
		}
	}
	mu.Lock()
	defer mu.Unlock()
	if ops[meta.OpCreate] != 2 || ops[meta.OpSetAttr] != 0 {
		t.Errorf("proposed %d creates and %d setattrs, want 2 and 0", ops[meta.OpCreate], ops[meta.OpSetAttr])
	}
}
