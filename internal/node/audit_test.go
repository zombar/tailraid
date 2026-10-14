package node

import (
	"bytes"
	"context"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/zombar/tailraid/internal/audit"
	"github.com/zombar/tailraid/internal/meta"
	"github.com/zombar/tailraid/internal/rpc"
	"github.com/zombar/tailraid/internal/testharness"
	"github.com/zombar/tailraid/internal/users"
)

// memVol is an in-memory audit volume.
type memVol struct {
	mu    sync.Mutex
	files map[string][]byte
}

func newMemVol() *memVol { return &memVol{files: map[string][]byte{}} }

func (m *memVol) WriteFile(_ context.Context, name string, data []byte) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.files[name] = bytes.Clone(data)
	return nil
}

func (m *memVol) ReadFile(_ context.Context, name string) ([]byte, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	b, ok := m.files[name]
	if !ok {
		return nil, os.ErrNotExist
	}
	return bytes.Clone(b), nil
}

func (m *memVol) List(_ context.Context, dir string) ([]string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	seen := map[string]bool{}
	var out []string
	for p := range m.files {
		rest, ok := strings.CutPrefix(p, dir+"/")
		if !ok {
			continue
		}
		name, _, _ := strings.Cut(rest, "/")
		if !seen[name] {
			seen[name] = true
			out = append(out, name)
		}
	}
	return out, nil
}

func (m *memVol) Remove(_ context.Context, name string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.files, name)
	return nil
}

// auditOn gives ops an audit writer over a fresh in-memory volume.
func auditOn(c *testharness.Cluster, ops *adminOps) *memVol {
	vol := newMemVol()
	ops.audit = &audit.Writer{Vol: vol, Node: ops.node.ID, Clk: c.Clk}
	return vol
}

// The leader alone records applied operations: with the apply hook on every node, the
// users and secrets ops a cluster applies leave one record each on the leader's log and
// none on a follower's; an admin op the leader refuses is a refusal record.
// Invariant: I-357
func TestAudit_LeaderOnly(t *testing.T) {
	c := testharness.NewCluster(t, 3, 2, 1)
	defer c.Close()
	leader := opsFor(c, c.Leader())
	var follower *adminOps
	for _, nd := range c.Nodes {
		if nd != c.Leader() {
			follower = opsFor(c, nd)
			break
		}
	}
	lvol, fvol := auditOn(c, leader), auditOn(c, follower)
	leader.node.FSM.SetApplyHook(leader.audited)
	follower.node.FSM.SetApplyHook(follower.audited)
	defer leader.node.FSM.SetApplyHook(nil) // before Close: the hooks must not outlive their writers
	defer follower.node.FSM.SetApplyHook(nil)
	kim, lee := withUsers(t, c) // an admin invite, kim, an invite, lee: four applied ops
	if _, err := leader.AdminOp(ctx, rpc.AdminOp{Op: "group_create", Actor: lee, Group: "x"}); err == nil {
		t.Fatal("lee created a group")
	}
	if _, err := leader.AdminOp(ctx, rpc.AdminOp{Op: "group_create", Actor: kim, Group: "family"}); err != nil {
		t.Fatal(err)
	}
	applied := c.Leader().Node.FSM.Applied()
	if err := follower.node.FSM.WaitApplied(ctx, applied); err != nil {
		t.Fatal(err)
	}
	for _, o := range []*adminOps{leader, follower} {
		if err := o.audit.Flush(ctx, true); err != nil {
			t.Fatal(err)
		}
	}
	lrecs, err := audit.List(ctx, lvol, audit.Filter{})
	if err != nil {
		t.Fatal(err)
	}
	kinds := map[string]int{}
	for _, r := range lrecs {
		kinds[r.Kind+" "+r.Result]++
	}
	want := map[string]int{"invite ok": 2, "user.create ok": 2, "group.create ok": 1, "op.group_create ok": 1, "op.group_create refused": 1}
	for k, n := range want {
		if kinds[k] != n {
			t.Errorf("leader's log: %s %d, want %d (all: %v)", k, kinds[k], n, kinds)
		}
	}
	if frecs, err := audit.List(ctx, fvol, audit.Filter{}); err != nil || len(frecs) != 0 {
		t.Fatalf("the follower recorded %d applied ops: %v %v", len(frecs), frecs, err)
	}
	// audit ls: a user their own records, an admin everyone's (I-358)
	raw, err := leader.AdminOp(ctx, rpc.AdminOp{Op: "audit_ls", Actor: lee, User: "kim"})
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range raw.([]audit.Record) {
		if r.Actor != "lee" {
			t.Fatalf("lee's listing shows %+v", r)
		}
	}
	raw, err = leader.AdminOp(ctx, rpc.AdminOp{Op: "audit_ls", Actor: kim, Kind: "op."})
	if err != nil || len(raw.([]audit.Record)) != 2 {
		t.Fatalf("the admin's listing of the ops: %v %+v", err, raw)
	}
}

// check warns for a user with no record in users.dormant_after and for a binding to a
// user that does not exist, and disables no one.
// Invariant: I-358
func TestCheck_DormantWarning(t *testing.T) {
	c := testharness.NewCluster(t, 3, 2, 1)
	defer c.Close()
	ops := opsFor(c, c.Leader())
	auditOn(c, ops)
	ops.dormant = time.Hour
	b, err := users.OpenBindings(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	ops.bindings = b
	if w := ops.userWarnings(ctx); len(w) != 0 {
		t.Fatalf("before users: %v", w)
	}
	kim, lee := withUsers(t, c)
	if err := b.Bind("uid:501", "kim"); err != nil {
		t.Fatal(err)
	}
	if err := b.Bind("uid:502", "ghost"); err != nil {
		t.Fatal(err)
	}
	c.Clk.Advance(2 * time.Hour)
	ops.audit.Record(audit.Record{Actor: "kim", Kind: audit.KindSignIn}) // kim is seen, lee never
	if err := ops.audit.Flush(ctx, true); err != nil {
		t.Fatal(err)
	}
	got := strings.Join(ops.userWarnings(ctx), "\n")
	if !strings.Contains(got, "user lee has not signed in") || strings.Contains(got, "user kim has") || !strings.Contains(got, `uid:502 to "ghost"`) {
		t.Fatalf("warnings:\n%s", got)
	}
	for _, u := range []meta.UserIdx{kim, lee} {
		if row, _ := c.Leader().Node.FSM.User(u); row.State != meta.UserActive {
			t.Fatalf("user %d is %v: check disabled someone", u, row.State)
		}
	}
}
