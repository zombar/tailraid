package soak_test

import (
	"bytes"
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/zombar/tailraid/internal/audit"
	"github.com/zombar/tailraid/internal/meta"
	"github.com/zombar/tailraid/internal/rpc"
	"github.com/zombar/tailraid/internal/secrets"
	"github.com/zombar/tailraid/internal/soak"
	"github.com/zombar/tailraid/internal/stats"
	"github.com/zombar/tailraid/internal/testharness"
	"github.com/zombar/tailraid/internal/testseed"
	"github.com/zombar/tailraid/internal/users"
	"github.com/zombar/tailraid/internal/vfs"
)

// The harness's users surface (soak.UsersEnv, phases 28–31): the ops of docs/21 §9 over
// each node's own FSM, as node.adminOps answers them, with the signature check the
// verifier makes (the user named, active, holding the key), the needs table, per-node
// bindings in the node's directory, an audit writer per node on the FSM's apply hook,
// and the mount of a node acting as whoever its uid is bound to.

// harnessCodeKey is the key the harness's sign-in codes are minted under.
var harnessCodeKey = []byte("the harness's cluster key, for codes")

// harnessMountUID is the uid the harness's "mounts" carry.
const harnessMountUID = 1000

type harnessUsers struct {
	mu    sync.Mutex
	bind  map[int]*users.Bindings
	audit map[int]*audit.Writer
}

// usersOf is the env's users state, made on first use with the audit writers and hooks.
func (e *harnessEnv) usersOf() *harnessUsers {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.users != nil {
		return e.users
	}
	hu := &harnessUsers{bind: map[int]*users.Bindings{}, audit: map[int]*audit.Writer{}}
	for _, nd := range e.c.Nodes {
		b, err := users.OpenBindings(nd.Dir)
		if err != nil {
			e.t.Fatal(err)
		}
		hu.bind[nd.I] = b
		w := &audit.Writer{Vol: stats.VFS{FS: nd.FS, St: nd.Node.FSM, Rd: vfs.NewReader(nd.Node.FSM, nd.Prober, nd.Client, nil, nil)}, Node: fmt.Sprintf("n%d", nd.I), Clk: e.c.Clk, Log: slog.New(slog.DiscardHandler)}
		hu.audit[nd.I] = w
		nd.Node.FSM.SetApplyHook(func(cmd meta.Command, index uint64, res meta.Result) {
			if !nd.Node.IsLeader() {
				return
			}
			if rec, ok := audit.Applied(cmd, index, res, nd.Node.FSM); ok {
				w.Record(rec)
			}
		})
	}
	e.t.Cleanup(func() {
		for _, nd := range e.c.Nodes {
			nd.Node.FSM.SetApplyHook(nil)
		}
	})
	e.users = hu
	return hu
}

// mountActor is who node i's mount acts as: the user its uid is bound to, or nobody.
func (e *harnessEnv) mountActor(nd *testharness.ClusterNode) meta.UserIdx {
	hu := e.usersOf()
	hu.mu.Lock()
	b := hu.bind[nd.I]
	hu.mu.Unlock()
	if name, ok := b.Lookup(users.UIDCredential(harnessMountUID)); ok {
		if u, found := nd.Node.FSM.UserByName(name); found {
			return u.Idx
		}
	}
	return 0
}

func (e *harnessEnv) MountCredential() string { return users.UIDCredential(harnessMountUID) }

// BootstrapInvite mints the first admin's invite on n, as `user bootstrap` does on a voter.
func (e *harnessEnv) BootstrapInvite(ctx context.Context, n soak.NodeRef, name string) (string, error) {
	e.usersOf() // the audit hooks before the first users op
	nd := e.node(n)
	code, err := users.NewCode()
	if err != nil {
		return "", err
	}
	now := e.c.Clk.Now()
	inv := meta.Invite{Hash: meta.HashCode(code), Name: name, Kind: meta.InviteUser, Admin: true, ExpiresAt: now.Add(meta.InviteTTL).UnixNano()}
	if err := e.propose(ctx, nd, meta.Command{Op: meta.OpInvite, InviteOp: &meta.InviteOp{Bootstrap: true, Invite: inv, Now: now.UnixNano()}}); err != nil {
		return "", err
	}
	return code, nil
}

func (e *harnessEnv) propose(ctx context.Context, nd *testharness.ClusterNode, cmd meta.Command) error {
	_, err := e.proposeResult(ctx, nd, cmd)
	return err
}

func (e *harnessEnv) proposeResult(ctx context.Context, nd *testharness.ClusterNode, cmd meta.Command) (meta.Result, error) {
	res, index, err := nd.Node.Propose(ctx, cmd)
	if err != nil {
		return res, err
	}
	if res.Err != nil {
		return res, res.Err
	}
	return res, nd.Node.FSM.WaitApplied(ctx, index)
}

// SignedOp answers an admin op on n as node.adminOps would: the signature's user and key
// checked, the needs table applied, the op run, the audit record written.
func (e *harnessEnv) SignedOp(ctx context.Context, n soak.NodeRef, id *users.Identity, op rpc.AdminOp) (json.RawMessage, error) {
	nd := e.node(n)
	hu := e.usersOf()
	st := nd.Node.FSM
	now := e.c.Clk.Now()
	w := hu.audit[nd.I]
	if id != nil {
		u, ok := st.UserByName(id.Name)
		kid := id.KeyID()
		if !ok || u.State != meta.UserActive || !slices.ContainsFunc(u.Keys, func(k meta.UserKey) bool { return k.ID() == kid }) {
			w.Refuse(id.Name, "", audit.KindRPC, "/v1/admin", "bad signature")
			return nil, rpc.ErrUnauthorized
		}
		op.Actor, op.SignKey = u.Idx, kid
	}
	need, known := op.Needs()
	if !known {
		return nil, fmt.Errorf("unknown admin op %q", op.Op)
	}
	actorName := func() string {
		if u, ok := st.User(op.Actor); ok {
			return u.Name
		}
		return ""
	}
	refuse := func(err error) (json.RawMessage, error) {
		if st.Enforcing() && need != rpc.NeedNone {
			w.Refuse(actorName(), "", audit.KindOp+op.Op, op.Path+op.Name+op.User, err.Error())
		}
		return nil, err
	}
	if st.Enforcing() && need != rpc.NeedNone {
		if op.Actor == 0 {
			return refuse(users.ErrUnauthorized)
		}
		switch need {
		case rpc.NeedAdmins:
			if !st.IsAdmin(op.Actor) {
				return refuse(meta.ErrAccess)
			}
		case rpc.NeedWriteOn, rpc.NeedAdminOn:
			in, err := st.Resolve(meta.SplitPath(op.Path))
			if err != nil {
				if !st.IsAdmin(op.Actor) {
					return refuse(meta.ErrAccess)
				}
			} else {
				want := meta.RoleWrite
				if need == rpc.NeedAdminOn {
					want = meta.RoleAdmin
				}
				if st.Access(op.Actor, in.Ino) < want {
					return refuse(meta.ErrAccess)
				}
			}
		case rpc.NeedNone, rpc.NeedUser:
		}
	}
	out, err := e.userDispatch(ctx, nd, op, now)
	if err != nil {
		if errors.Is(err, meta.ErrAccess) || errors.Is(err, meta.ErrInvite) || errors.Is(err, meta.ErrLockout) {
			return refuse(err)
		}
		return nil, err
	}
	if st.Enforcing() {
		switch {
		case op.Op == "secret_get":
			w.Record(audit.Record{Actor: actorName(), Kind: audit.KindSecretGet, Target: op.Name})
		case need == rpc.NeedAdmins:
			w.Record(audit.Record{Actor: actorName(), Kind: audit.KindOp + op.Op, Target: op.Path + op.Name + op.User})
		}
	}
	return json.Marshal(out)
}

// userDispatch runs one op (docs/21 §9) against the node.
func (e *harnessEnv) userDispatch(ctx context.Context, nd *testharness.ClusterNode, op rpc.AdminOp, now time.Time) (any, error) {
	st := nd.Node.FSM
	hu := e.usersOf()
	nowNS := now.UnixNano()
	mint := func(inv meta.Invite) (any, error) {
		code, err := users.NewCode()
		if err != nil {
			return nil, err
		}
		inv.Hash, inv.ExpiresAt = meta.HashCode(code), now.Add(meta.InviteTTL).UnixNano()
		if err := e.propose(ctx, nd, meta.Command{Op: meta.OpInvite, Actor: op.Actor, InviteOp: &meta.InviteOp{Invite: inv, Now: nowNS}}); err != nil {
			return nil, err
		}
		return map[string]any{"code": code}, nil
	}
	userOp := func(action meta.UserAction, target meta.UserIdx, kid meta.UserKeyID) (any, error) {
		return nil, e.propose(ctx, nd, meta.Command{Op: meta.OpUser, Actor: op.Actor, UserOp: &meta.UserOp{Action: action, Target: target, KeyID: kid, Now: nowNS}})
	}
	byName := func(name string) (meta.User, error) {
		u, ok := st.UserByName(name)
		if !ok {
			return meta.User{}, fmt.Errorf("%w: no user %q", meta.ErrNotExist, name)
		}
		return u, nil
	}
	switch op.Op {
	case "user_invite":
		return mint(meta.Invite{Name: op.User, Kind: meta.InviteUser, Admin: op.Admin})
	case "user_key_invite":
		target := op.User
		if target == "" {
			u, _ := st.User(op.Actor)
			target = u.Name
		}
		u, err := byName(target)
		if err != nil {
			return nil, err
		}
		if u.Idx != op.Actor && !st.IsAdmin(op.Actor) {
			return nil, meta.ErrAccess
		}
		return mint(meta.Invite{Kind: meta.InviteKey, ForUser: u.Idx})
	case "user_create", "user_key_add":
		inv, ok := st.Invite(meta.HashCode(op.Code))
		kind := meta.InviteUser
		if op.Op == "user_key_add" {
			kind = meta.InviteKey
		}
		if !ok || inv.Kind != kind || nowNS >= inv.ExpiresAt || (kind == meta.InviteUser && inv.Name != op.User) {
			return nil, meta.ErrInvite
		}
		var key [32]byte
		b, err := hex.DecodeString(op.Key)
		if err != nil || len(b) != len(key) {
			return nil, fmt.Errorf("%w: a public key is 32 bytes, hex", meta.ErrBadCommand)
		}
		copy(key[:], b)
		action := meta.UserCreate
		if kind == meta.InviteKey {
			action = meta.UserKeyAdd
		}
		res, err := e.proposeResult(ctx, nd, meta.Command{Op: meta.OpUser, UserOp: &meta.UserOp{Action: action, Name: op.User, Code: op.Code, Key: key, Label: op.Label, Now: nowNS}})
		if err != nil {
			return nil, err
		}
		u, _ := st.User(meta.UserIdx(res.Ino))
		kid := meta.UserKeyIDOf(key)
		return map[string]any{"user": u.Name, "idx": u.Idx, "key": hex.EncodeToString(kid[:])}, nil
	case "user_key_rm":
		u, err := byName(op.User)
		if err != nil {
			return nil, err
		}
		var kid meta.UserKeyID
		b, err := hex.DecodeString(op.Key)
		if err != nil || len(b) != len(kid) {
			return nil, fmt.Errorf("%w: key id", meta.ErrBadCommand)
		}
		copy(kid[:], b)
		return userOp(meta.UserKeyRemove, u.Idx, kid)
	case "user_disable", "user_enable", "user_rm":
		u, err := byName(op.User)
		if err != nil {
			return nil, err
		}
		action := map[string]meta.UserAction{"user_disable": meta.UserDisable, "user_enable": meta.UserEnable, "user_rm": meta.UserRemove}[op.Op]
		return userOp(action, u.Idx, meta.UserKeyID{})
	case "user_whoami":
		u, ok := st.User(op.Actor)
		if !ok {
			return nil, users.ErrUnauthorized
		}
		return map[string]any{"user": u.Name, "idx": u.Idx, "admin": st.IsAdmin(u.Idx)}, nil
	case "user_bind", "user_unbind":
		hu.mu.Lock()
		b := hu.bind[nd.I]
		hu.mu.Unlock()
		me := ""
		if u, ok := st.User(op.Actor); ok {
			me = u.Name
		}
		if op.Op == "user_unbind" {
			bound, _ := b.Lookup(op.Credential)
			if st.Enforcing() && bound != me && !st.IsAdmin(op.Actor) {
				return nil, meta.ErrAccess
			}
			return map[string]string{"credential": op.Credential}, b.Unbind(op.Credential)
		}
		if _, err := byName(op.User); err != nil {
			return nil, err
		}
		if st.Enforcing() && op.User != me && !st.IsAdmin(op.Actor) {
			return nil, meta.ErrAccess
		}
		return map[string]string{"credential": op.Credential, "user": op.User}, b.Bind(op.Credential, op.User)
	case "group_remove", "group_add":
		g, ok := st.GroupByName(op.Group)
		if !ok {
			return nil, fmt.Errorf("%w: no group %q", meta.ErrNotExist, op.Group)
		}
		u, err := byName(op.User)
		if err != nil {
			return nil, err
		}
		action := meta.GroupRemoveMember
		if op.Op == "group_add" {
			action = meta.GroupAddMember
		}
		return nil, e.propose(ctx, nd, meta.Command{Op: meta.OpGroup, Actor: op.Actor, GroupOp: &meta.GroupOp{Action: action, Group: g.Idx, User: u.Idx, Now: nowNS}})
	case "grant", "revoke":
		in, err := st.Resolve(meta.SplitPath(op.Path))
		if err != nil {
			return nil, err
		}
		var sub meta.Subject
		if g, ok := strings.CutPrefix(op.User, "@"); ok {
			grp, found := st.GroupByName(g)
			if !found {
				return nil, fmt.Errorf("%w: no group %q", meta.ErrNotExist, g)
			}
			sub = meta.Subject{Kind: meta.SubjectGroup, Idx: uint16(grp.Idx)}
		} else {
			u, err := byName(op.User)
			if err != nil {
				return nil, err
			}
			sub = meta.Subject{Kind: meta.SubjectUser, Idx: uint16(u.Idx)}
		}
		role := meta.RoleNone
		if op.Op == "grant" {
			var ok bool
			if role, ok = meta.ParseRole(op.Role); !ok || role == meta.RoleNone {
				return nil, fmt.Errorf("%w: role %q", meta.ErrBadCommand, op.Role)
			}
		}
		return map[string]string{"path": op.Path}, e.propose(ctx, nd, meta.Command{Op: meta.OpGrant, Actor: op.Actor, GrantOp: &meta.GrantOp{Ino: in.Ino, Subject: sub, Role: role, Now: nowNS}})
	case "secret_set", "secret_get", "secret_ls", "secret_rm":
		return e.userSecretOp(ctx, nd, op)
	case "web_login":
		u, ok := st.User(op.Actor)
		if !ok {
			return nil, users.ErrUnauthorized
		}
		tok, err := users.NewToken()
		if err != nil {
			return nil, err
		}
		exp := now.Add(168 * time.Hour)
		if err := e.propose(ctx, nd, meta.Command{Op: meta.OpSession, Actor: u.Idx, SessionOp: &meta.SessionOp{User: u.Idx, Hash: tokenHash(tok), Key: op.SignKey, ExpiresAt: exp.UnixNano(), Now: nowNS}}); err != nil {
			return nil, err
		}
		code, err := users.MintCode(harnessCodeKey, tok, now)
		if err != nil {
			return nil, err
		}
		base, err := e.WebURL(ctx, soak.NodeRef{Name: fmt.Sprintf("n%d", nd.I), Idx: nd.Idx()})
		if err != nil {
			return nil, err
		}
		return map[string]any{"url": strings.TrimSuffix(base, "/") + "/login?code=" + code, "expires_at": exp.UTC().Format(time.RFC3339)}, nil
	case "audit_ls":
		w := hu.audit[nd.I]
		f := audit.Filter{User: op.User, Kind: op.Kind, Limit: op.Limit}
		if op.Since != "" {
			since, err := time.Parse(time.RFC3339, op.Since)
			if err != nil {
				return nil, fmt.Errorf("since: %w", meta.ErrBadCommand)
			}
			f.Since = since
		}
		if st.Enforcing() && !st.IsAdmin(op.Actor) {
			u, _ := st.User(op.Actor)
			f.User = u.Name
		}
		for _, other := range hu.audit { // every node's log is in the one volume: flush them all
			_ = other.Flush(ctx, false)
		}
		recs, err := audit.List(ctx, w.Vol, f)
		if err != nil {
			return nil, err
		}
		if recs == nil {
			recs = []audit.Record{}
		}
		return recs, nil
	}
	return nil, fmt.Errorf("harness: unknown admin op %q", op.Op)
}

// userSecretOp is the node's secret op with the owner rule of docs/21 §7.
func (e *harnessEnv) userSecretOp(ctx context.Context, nd *testharness.ClusterNode, op rpc.AdminOp) (any, error) {
	st := nd.Node.FSM
	me := ""
	if u, ok := st.User(op.Actor); ok {
		me = u.Name
	}
	store := e.secretStore(nd.I)
	if op.Op != "secret_ls" && st.Enforcing() {
		owner, personal := meta.SecretOwner(op.Name)
		own := personal && owner == me
		adminMay := st.IsAdmin(op.Actor) && (!personal || op.Op == "secret_rm")
		if !own && !adminMay {
			return nil, meta.ErrAccess
		}
	}
	switch op.Op {
	case "secret_set":
		return map[string]string{"name": op.Name}, store.Set(ctx, op.Actor, op.Name, op.Value)
	case "secret_get":
		v, err := store.Get(ctx, op.Name)
		if err != nil {
			return nil, err
		}
		return map[string]string{"name": op.Name, "value": v}, nil
	case "secret_rm":
		return map[string]string{"name": op.Name}, store.Remove(ctx, op.Actor, op.Name, false)
	default:
		all := store.List(ctx)
		if !st.Enforcing() {
			return all, nil
		}
		if op.All && !st.IsAdmin(op.Actor) {
			return nil, meta.ErrAccess
		}
		out := make([]secrets.Entry, 0, len(all))
		for _, en := range all {
			owner, personal := meta.SecretOwner(en.Name)
			if (op.All && !personal) || (!op.All && personal && owner == me) {
				out = append(out, en)
			}
		}
		return out, nil
	}
}

// harnessWebAudit is the web UI's audit hook on the harness.
type harnessWebAudit struct{ w *audit.Writer }

func (h harnessWebAudit) SignedIn(user string) {
	h.w.Record(audit.Record{Actor: user, Kind: audit.KindSignIn})
}

func (h harnessWebAudit) Refused(user, _, route, detail string) {
	h.w.Refuse(user, "", audit.KindWeb, route, detail)
}

var _ soak.UsersEnv = (*harnessEnv)(nil)

// Phases 28–31 on the harness: every check they make passes, none is skipped but the
// one about the bucket and the scrape, and the run's report carries the Users section.
// Invariant: I-342, I-347, I-354, I-356, I-357
func TestSoak_UsersAgainstHarness(t *testing.T) {
	c := testharness.NewCluster(t, 3, 2, 1)
	defer c.Close()
	env := &harnessEnv{t: t, c: c}
	var jsonl bytes.Buffer
	seed, rng := testseed.New(t)
	run := &soak.Run{Env: env, Profile: harnessProfile(), Out: &jsonl, Rng: rng, Seed: seed, Phases: soak.StandardPhases("28", "29", "30", "31")}
	sum, err := run.Execute(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	for _, ph := range sum.Phases {
		if ph.Skipped {
			t.Errorf("%s skipped: %s", ph.Name, ph.Reason)
		}
		if len(ph.Checks) == 0 {
			t.Errorf("%s has no checks", ph.Name)
		}
		for _, chk := range ph.Checks {
			switch {
			case chk.OK != nil && !*chk.OK:
				t.Errorf("%s: %s: %s", ph.Name, chk.Name, chk.Text)
			case chk.OK == nil && !strings.Contains(chk.Name, "add-s3"):
				t.Errorf("%s: %s skipped: %s", ph.Name, chk.Name, chk.Text)
			}
		}
	}
	var html bytes.Buffer
	if err := soak.Render(&html, sum); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(html.String(), "<h2>Users</h2>") || !strings.Contains(html.String(), "grant matrix") {
		t.Fatal("the report has no Users section")
	}
}

// oneGateway is the harness with a volume on its first node alone, as a live run has
// (`verify --users` drives the verifying device's own mount): phase 29 probes the one
// gateway and leaves the two-gateway check skipped.
type oneGateway struct{ *harnessEnv }

func (e oneGateway) Volume(n soak.NodeRef) (soak.Volume, bool) {
	if e.node(n).I != 0 {
		return nil, false
	}
	return e.harnessEnv.Volume(n)
}

func (e oneGateway) Caps() soak.Caps {
	c := e.harnessEnv.Caps()
	c.MultiGateway = false
	return c
}

// The users phases run against one gateway, as a live `verify --users` does: no phase
// skipped, no check failed, and the one check that needs two gateways skipped by name.
func TestSoak_UsersOneGatewayAgainstHarness(t *testing.T) {
	c := testharness.NewCluster(t, 3, 2, 1)
	defer c.Close()
	env := oneGateway{&harnessEnv{t: t, c: c}}
	var jsonl bytes.Buffer
	seed, rng := testseed.New(t)
	run := &soak.Run{Env: env, Profile: harnessProfile(), Out: &jsonl, Rng: rng, Seed: seed, Phases: soak.StandardPhases("28", "29", "30", "31")}
	sum, err := run.Execute(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	for _, ph := range sum.Phases {
		if ph.Skipped {
			t.Errorf("%s skipped: %s", ph.Name, ph.Reason)
		}
		for _, chk := range ph.Checks {
			switch {
			case chk.OK != nil && !*chk.OK:
				t.Errorf("%s: %s: %s", ph.Name, chk.Name, chk.Text)
			case chk.OK == nil && !strings.Contains(chk.Name, "add-s3") && !strings.Contains(chk.Name, "two gateways"):
				t.Errorf("%s: %s skipped: %s", ph.Name, chk.Name, chk.Text)
			}
		}
	}
}
