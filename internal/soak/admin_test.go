package soak

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/zombar/tailraid/internal/admin"
	"github.com/zombar/tailraid/internal/meta"
	"github.com/zombar/tailraid/internal/repair"
	"github.com/zombar/tailraid/internal/rpc"
)

// fakeCaller answers status from a fixed report and records every other call; one address
// may refuse admin calls with a 421-shaped not-leader error naming the real leader.
type fakeCaller struct {
	rep      admin.Report
	down     map[string]bool
	redirect map[string]string // addr → leader it names
	calls    []string
	reply    any
}

func (f *fakeCaller) Call(_ context.Context, addr, method, path string, body []byte) ([]byte, error) {
	f.calls = append(f.calls, method+" "+addr+path)
	if f.down[addr] {
		return nil, errors.New("dial: connection refused")
	}
	if path == "/v1/status" {
		return json.Marshal(f.rep)
	}
	if to, ok := f.redirect[addr]; ok {
		return nil, fmt.Errorf("%w (leader %s)", rpc.ErrNotLeader, to)
	}
	if f.reply == nil {
		return []byte("{}"), nil
	}
	return json.Marshal(f.reply)
}

func report(leader string, addrs ...string) admin.Report {
	var rep admin.Report
	for i, a := range addrs {
		host, port := a[:strings.IndexByte(a, ':')], a[strings.IndexByte(a, ':')+1:]
		var p uint16
		if _, err := fmt.Sscan(port, &p); err != nil {
			panic(err)
		}
		rep.Nodes = append(rep.Nodes, admin.NodeReport{Idx: meta.NodeIdx(i + 1), Name: fmt.Sprintf("n%d", i+1), Kind: "device", Addr: host, RPCPort: p, Leader: a == leader})
	}
	return rep
}

// The leader is found through the first seed that answers; a stale leader's 421 is
// followed to the node it names; fsck and check replies decode into the admin types.
func TestAdmin_SeedsRedirectsAndReplies(t *testing.T) {
	fc := &fakeCaller{rep: report("10.0.0.2:20481", "10.0.0.1:20481", "10.0.0.2:20481", "10.0.0.3:20481"),
		down: map[string]bool{"10.0.0.1:20481": true}, redirect: map[string]string{"10.0.0.2:20481": "10.0.0.3:20481"}}
	a := &Admin{Caller: fc, Seeds: []string{"10.0.0.1:20481", "10.0.0.2:20481"}}
	ctx := context.Background()
	if l, err := a.LeaderAddr(ctx); err != nil || l != "10.0.0.2:20481" {
		t.Fatalf("leader %q, %v", l, err)
	}
	fc.reply = admin.CheckReport{Scan: repair.Stats{Scanned: 3}}
	st, err := a.Scrub(ctx)
	if err != nil || st.Scanned != 3 {
		t.Fatalf("scrub %+v, %v", st, err)
	}
	want := []string{"GET 10.0.0.1:20481/v1/status", "GET 10.0.0.2:20481/v1/status", "POST 10.0.0.2:20481/v1/admin", "POST 10.0.0.3:20481/v1/admin"}
	if got := fc.calls[len(fc.calls)-4:]; strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("calls %v", got)
	}
	fc.reply = admin.FsckReport{Violations: []string{"I-1: refcount"}, OrphanShards: 2}
	ok, orphans, text, err := a.Fsck(ctx, NodeRef{Name: "n3", RPC: "10.0.0.3:20481"})
	if err != nil || ok || orphans != 2 || text != "1 violations, 2 orphan shards; I-1: refcount" {
		t.Fatalf("fsck %v %d %q %v", ok, orphans, text, err)
	}
	if fc.calls[len(fc.calls)-1] != http.MethodPost+" 10.0.0.3:20481/v1/admin" {
		t.Fatalf("fsck went to %s", fc.calls[len(fc.calls)-1])
	}
	// no seed answers: a clear error, not a panic
	fc.down["10.0.0.2:20481"] = true
	if _, err := a.LeaderAddr(ctx); err == nil || !strings.Contains(err.Error(), "tailraid serve") {
		t.Fatalf("all seeds down: %v", err)
	}
}
