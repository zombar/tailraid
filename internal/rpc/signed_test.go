package rpc

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/netip"
	"testing"

	"github.com/zombar/tailraid/internal/meta"
	"github.com/zombar/tailraid/internal/users"
)

// recordingAdmin keeps the last op it was handed.
type recordingAdmin struct{ last AdminOp }

func (r *recordingAdmin) AdminOp(_ context.Context, op AdminOp) (any, error) {
	r.last = op
	return map[string]string{"ok": "1"}, nil
}

// Once the cluster has users, an admin op that needs one is refused with 401 before the
// handler sees it unless its Tailraid-Sig verifies, and the actor the signature names
// reaches the handler; a read-only op needs none; before the first user nothing is
// checked; a member op that needs admins is 401 unsigned and 403 for a plain user; an op
// the table does not know is refused; the client's Sign hook puts the header on.
// Invariant: I-344, I-345
func TestRPC_MutatingOpsNeedSignature(t *testing.T) {
	rec := &recordingAdmin{}
	enforcing, admin := true, false
	e := newEnvWith(t, func(o *Options) {
		o.Admin = rec
		o.Signed = &Signed{
			Verify: func(hdr string, src netip.Addr, method, path string, body []byte) (meta.UserIdx, meta.UserKeyID, error) {
				if hdr == "ok" && method == http.MethodPost && (path == "/v1/admin" || path == "/v1/members") && len(body) > 0 && src.IsValid() {
					return 7, meta.UserKeyID{1}, nil
				}
				return 0, meta.UserKeyID{}, users.ErrUnauthorized
			},
			Enforcing: func() bool { return enforcing },
			IsAdmin:   func(u meta.UserIdx) bool { return admin },
		}
	})
	c := e.client()
	post := func(path string, body any, hdr string) int {
		t.Helper()
		b, _ := json.Marshal(body)
		h := map[string]string{"Content-Type": "application/json"}
		if hdr != "" {
			h[users.Header] = hdr
		}
		resp, err := c.do(ctx, http.MethodPost, e.addr, path, b, h)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		return resp.StatusCode
	}
	for _, tc := range []struct {
		name string
		op   string
		hdr  string
		want int
	}{
		{"read-only, unsigned", "trash_ls", "", http.StatusOK},
		{"admins op, unsigned", "repair_now", "", http.StatusUnauthorized},
		{"admins op, bad header", "repair_now", "nope", http.StatusUnauthorized},
		{"admins op, signed", "repair_now", "ok", http.StatusOK},
		{"user op, signed", "secret_ls", "ok", http.StatusOK},
		{"unknown op", "frobnicate", "ok", http.StatusBadRequest},
	} {
		rec.last = AdminOp{}
		if got := post("/v1/admin", AdminOp{Op: tc.op}, tc.hdr); got != tc.want {
			t.Errorf("%s: %d, want %d", tc.name, got, tc.want)
		}
		if tc.want == http.StatusOK && tc.hdr == "ok" && rec.last.Actor != 7 {
			t.Errorf("%s: the handler saw actor %d", tc.name, rec.last.Actor)
		}
		if tc.want != http.StatusOK && rec.last.Op != "" {
			t.Errorf("%s: the handler ran", tc.name)
		}
		if tc.want == http.StatusOK && !rec.last.Source.IsLoopback() {
			t.Errorf("%s: source %v", tc.name, rec.last.Source)
		}
	}
	// a member op: unsigned 401, a plain user 403, an admin through; a member's own op unsigned
	if got := post("/v1/members", MemberOp{Op: "set_voter", NodeIdx: 1}, ""); got != http.StatusUnauthorized {
		t.Errorf("member op unsigned: %d", got)
	}
	if got := post("/v1/members", MemberOp{Op: "set_voter", NodeIdx: 1}, "ok"); got != http.StatusForbidden {
		t.Errorf("member op by a plain user: %d", got)
	}
	admin = true
	if got := post("/v1/members", MemberOp{Op: "set_voter", NodeIdx: 1}, "ok"); got != http.StatusOK {
		t.Errorf("member op by an admin: %d", got)
	}
	if got := post("/v1/members", MemberOp{Op: "set_version", NodeIdx: 1, Version: "v"}, ""); got != http.StatusOK {
		t.Errorf("a member's own op unsigned: %d", got)
	}
	// before the first user nothing is checked
	enforcing = false
	if got := post("/v1/admin", AdminOp{Op: "repair_now"}, ""); got != http.StatusOK || rec.last.Actor != 0 {
		t.Errorf("before users: %d, actor %d", got, rec.last.Actor)
	}
	enforcing = true
	// the client's Sign hook
	c.Sign = func(addr, method, path string, body []byte) (string, error) {
		if addr != e.addr || method != http.MethodPost || path != "/v1/admin" || !bytes.Contains(body, []byte("gc_now")) {
			return "", errors.New("unexpected request")
		}
		return "ok", nil
	}
	if _, err := c.Admin(ctx, e.addr, AdminOp{Op: "gc_now"}); err != nil || rec.last.Actor != 7 {
		t.Fatalf("signed through the client: %v, actor %d", err, rec.last.Actor)
	}
	c.Sign = nil
	if _, err := c.Admin(ctx, e.addr, AdminOp{Op: "gc_now"}); !errors.Is(err, ErrUnauthorized) {
		t.Fatalf("unsigned through the client: %v", err)
	}
	// the actor on the body is never trusted
	rec.last = AdminOp{}
	if got := post("/v1/admin", map[string]any{"op": "trash_ls", "actor": 9}, ""); got != http.StatusOK || rec.last.Actor != 0 {
		t.Errorf("a body's actor: %d, actor %d", got, rec.last.Actor)
	}
}

// Every op the table classifies is reachable, and the needs it answers follow the doc's
// refinements: a scrub, the whole trash and the system secrets need admins.
// Invariant: I-345
func TestAdminOp_Needs_Refinements(t *testing.T) {
	for _, tc := range []struct {
		op   AdminOp
		want Need
	}{
		{AdminOp{Op: "check"}, NeedNone},
		{AdminOp{Op: "check", Scrub: true}, NeedAdmins},
		{AdminOp{Op: "trash_purge"}, NeedAdminOn},
		{AdminOp{Op: "trash_purge", All: true}, NeedAdmins},
		{AdminOp{Op: "secret_ls"}, NeedUser},
		{AdminOp{Op: "secret_ls", All: true}, NeedAdmins},
		{AdminOp{Op: "grant"}, NeedAdminOn},
		{AdminOp{Op: "trash_restore"}, NeedWriteOn},
		{AdminOp{Op: "user_create"}, NeedNone},
	} {
		if got, ok := tc.op.Needs(); !ok || got != tc.want {
			t.Errorf("%+v: %d %v, want %d", tc.op, got, ok, tc.want)
		}
	}
	if _, ok := (AdminOp{Op: "nope"}).Needs(); ok {
		t.Error("an unknown op is classified")
	}
	if n := len(AdminOpNames()); n < 40 {
		t.Errorf("%d ops in the table", n)
	}
	if (MemberOp{Op: "update_endpoints"}).Needs() != NeedNone || (MemberOp{Op: "remove"}).Needs() != NeedAdmins {
		t.Error("member op needs")
	}
}
