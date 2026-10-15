package soak

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/zombar/tailraid/internal/meta"
	"github.com/zombar/tailraid/internal/rpc"
	"github.com/zombar/tailraid/internal/users"
)

// fixedUsers answers UserByName from a map, for the verifier of the fake node.
type fixedUsers map[string]meta.User

func (f fixedUsers) UserByName(name string) (meta.User, bool) {
	u, ok := f[name]
	return u, ok
}

// fakeAdminNode is one node's /v1/admin: it records every op with the signature header
// it came with, verifies a present one as the node would, and answers what the test set.
type fakeAdminNode struct {
	t      *testing.T
	nodeID string
	v      *users.Verifier
	mu     sync.Mutex
	ops    []rpc.AdminOp
	sigs   []string // "" for none, the user the signature named, or "bad"
	reply  func(op rpc.AdminOp) (int, string)
	server *httptest.Server
}

func newFakeAdminNode(t *testing.T, cid [16]byte, nodeID string, known fixedUsers) *fakeAdminNode {
	f := &fakeAdminNode{t: t, nodeID: nodeID, v: &users.Verifier{Users: known, Cluster: cid, NodeID: nodeID, Now: time.Now}}
	f.reply = func(rpc.AdminOp) (int, string) { return http.StatusOK, `{"ok":true}` }
	f.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/admin" {
			http.NotFound(w, r)
			return
		}
		body, _ := io.ReadAll(r.Body)
		var op rpc.AdminOp
		_ = json.Unmarshal(body, &op)
		who := ""
		if h := r.Header.Get(users.Header); h != "" {
			who = "bad"
			if idx, _, err := f.v.Verify(h, netip.MustParseAddr("127.0.0.1"), r.Method, r.URL.Path, body); err == nil {
				for name, u := range known {
					if u.Idx == idx {
						who = name
					}
				}
			}
		}
		f.mu.Lock()
		f.ops = append(f.ops, op)
		f.sigs = append(f.sigs, who)
		f.mu.Unlock()
		code, rep := f.reply(op)
		w.WriteHeader(code)
		_, _ = io.WriteString(w, rep)
	}))
	t.Cleanup(f.server.Close)
	return f
}

func (f *fakeAdminNode) addr() string { return strings.TrimPrefix(f.server.URL, "http://") }

func (f *fakeAdminNode) last() (rpc.AdminOp, string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.ops[len(f.ops)-1], f.sigs[len(f.sigs)-1]
}

// A live run's signed op carries a signature by the run's own identity for the node it
// goes to (I-344) and nothing from the device's own signer; an unsigned op carries no
// header at all even though the device's signer would add one.
//
// Invariant: I-344
func TestLiveEnv_SignedOp_SignsForTargetNode(t *testing.T) {
	var cid [16]byte
	cid[0] = 7
	kim, err := users.NewIdentity("kim")
	if err != nil {
		t.Fatal(err)
	}
	pub := kim.Pub()
	known := fixedUsers{"kim": {Idx: 3, Name: "kim", State: meta.UserActive, Keys: []meta.UserKey{{Pub: pub}}}}
	a := newFakeAdminNode(t, cid, "node-a", known)
	b := newFakeAdminNode(t, cid, "node-b", known)
	cl := rpc.NewClient(cid, nil)
	cl.Sign = func(string, string, string, []byte) (string, error) {
		return "user=device key=00 ts=1 nonce=00 sig=AA==", nil
	}
	e := &LiveEnv{Client: cl, Cluster: cid, SelfAddr: a.addr(), ids: map[string]string{a.addr(): "node-a", b.addr(): "node-b"}}
	ctx := context.Background()
	for _, n := range []*fakeAdminNode{a, b} {
		if _, err := e.SignedOp(ctx, NodeRef{Name: n.nodeID, RPC: n.addr()}, &kim, rpc.AdminOp{Op: "user_whoami"}); err != nil {
			t.Fatal(err)
		}
		if op, who := n.last(); op.Op != "user_whoami" || who != "kim" {
			t.Fatalf("%s got %s signed by %q, want kim (the signature is bound to the node it goes to)", n.nodeID, op.Op, who)
		}
	}
	if _, err := e.SignedOp(ctx, NodeRef{Name: "node-a", RPC: a.addr()}, nil, rpc.AdminOp{Op: "user_create", User: "lee"}); err != nil {
		t.Fatal(err)
	}
	if op, who := a.last(); op.Op != "user_create" || who != "" {
		t.Fatalf("an unsigned op reached the node as %q (the device's own signer must stay out)", who)
	}
	// a refusal is the node's error
	a.reply = func(rpc.AdminOp) (int, string) { return http.StatusForbidden, `{"error":"permission denied"}` }
	if _, err := e.SignedOp(ctx, NodeRef{Name: "node-a", RPC: a.addr()}, &kim, rpc.AdminOp{Op: "grant"}); err == nil || !strings.Contains(err.Error(), "permission denied") {
		t.Fatalf("refusal: %v", err)
	}
}

// The first admin's invite comes from this device's own admin when it holds one, else
// from a bootstrap token written into the daemon's data directory and sent to this
// device's own node alone (docs/21 §1).
//
// Invariant: I-343
func TestLiveEnv_BootstrapInvite_Table(t *testing.T) {
	var cid [16]byte
	a := newFakeAdminNode(t, cid, "node-a", fixedUsers{})
	b := newFakeAdminNode(t, cid, "node-b", fixedUsers{})
	for _, n := range []*fakeAdminNode{a, b} {
		n.reply = func(op rpc.AdminOp) (int, string) {
			if op.Op == "user_invite" && !op.Admin {
				return http.StatusBadRequest, `{"error":"not an admin invite"}`
			}
			return http.StatusOK, `{"code":"c-` + op.Op + `","admin":true}`
		}
	}
	dir := t.TempDir()
	ctx := context.Background()
	t.Run("an admin here invites through any node", func(t *testing.T) {
		e := &LiveEnv{Client: rpc.NewClient(cid, nil), SelfAddr: a.addr(), DataDir: dir, AdminHere: true}
		code, err := e.BootstrapInvite(ctx, NodeRef{Name: "node-b", RPC: b.addr()}, "kim")
		if err != nil || code != "c-user_invite" {
			t.Fatalf("%q %v", code, err)
		}
		if op, _ := b.last(); op.User != "kim" || !op.Admin {
			t.Fatalf("op %+v", op)
		}
		if _, err := os.Stat(filepath.Join(dir, users.BootstrapFile)); err == nil {
			t.Fatal("a bootstrap token was written although the device's admin minted the invite")
		}
	})
	t.Run("no admin here: bootstrap on this device's node", func(t *testing.T) {
		e := &LiveEnv{Client: rpc.NewClient(cid, nil), SelfAddr: a.addr(), DataDir: dir}
		code, err := e.BootstrapInvite(ctx, NodeRef{Name: "node-a", RPC: a.addr()}, "kim")
		if err != nil || code != "c-user_bootstrap" {
			t.Fatalf("%q %v", code, err)
		}
		op, who := a.last()
		tok, rerr := os.ReadFile(filepath.Join(dir, users.BootstrapFile))
		if rerr != nil || op.Value == "" || op.Value != string(tok) || op.User != "kim" || who != "" {
			t.Fatalf("op %+v signed %q, token file %q %v", op, who, tok, rerr)
		}
	})
	t.Run("no admin here and another node: refused", func(t *testing.T) {
		e := &LiveEnv{Client: rpc.NewClient(cid, nil), SelfAddr: a.addr(), DataDir: dir}
		if _, err := e.BootstrapInvite(ctx, NodeRef{Name: "node-b", RPC: b.addr()}, "kim"); err == nil || !strings.Contains(err.Error(), "this device's own node") {
			t.Fatalf("err %v", err)
		}
	})
	t.Run("an unwritable data directory says what to do", func(t *testing.T) {
		e := &LiveEnv{Client: rpc.NewClient(cid, nil), SelfAddr: a.addr(), DataDir: filepath.Join(dir, "missing", "deeper")}
		if _, err := e.BootstrapInvite(ctx, NodeRef{Name: "node-a", RPC: a.addr()}, "kim"); err == nil || !strings.Contains(err.Error(), "daemon's user") {
			t.Fatalf("err %v", err)
		}
	})
	t.Run("the admin's refusal falls back to the bootstrap", func(t *testing.T) {
		a.reply = func(op rpc.AdminOp) (int, string) {
			if op.Op == "user_invite" {
				return http.StatusForbidden, `{"error":"permission denied"}`
			}
			return http.StatusOK, `{"code":"c-boot"}`
		}
		e := &LiveEnv{Client: rpc.NewClient(cid, nil), SelfAddr: a.addr(), DataDir: dir, AdminHere: true}
		code, err := e.BootstrapInvite(ctx, NodeRef{Name: "node-a", RPC: a.addr()}, "kim")
		if err != nil || code != "c-boot" {
			t.Fatalf("%q %v", code, err)
		}
	})
}

// A live run's capabilities carry Users only with --users, apart from the destructive
// flags, and its mount credential is what the CLI gave it.
func TestLiveEnv_UsersCap(t *testing.T) {
	e := &LiveEnv{Users: true, MountCred: users.UIDCredential(501)}
	if got := e.Caps(); !got.Users || got.Mutate || got.Corrupt {
		t.Fatalf("caps %+v", got)
	}
	if e.MountCredential() != "uid:501" {
		t.Fatal(e.MountCredential())
	}
	if (&LiveEnv{Destructive: true, Restarts: true}).Caps().Users {
		t.Fatal("--destructive --restarts turned the users phases on")
	}
}
