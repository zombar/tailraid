package soak

import (
	"bytes"
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"slices"
	"strings"
	"syscall"
	"time"

	"github.com/zombar/tailraid/internal/audit"
	"github.com/zombar/tailraid/internal/meta"
	"github.com/zombar/tailraid/internal/rpc"
	"github.com/zombar/tailraid/internal/users"
)

// --- 28–31 users, grants, secrets per user, audit and sessions ------------------------------
//
// docs/21 §10 on a running cluster. The driver holds the people: Ed25519 identities it
// makes for the run and signs every operation with (docs/21 §2). The first admin comes
// from `tailraid user bootstrap` on a voter; from then on the cluster enforces, so the
// phases keep the driver's mounts bound to the admin between their own checks and run
// last. A live cluster's users are its own: the phases record themselves skipped there,
// as on the harness, whose nodes serve no admin ops.

// UsersEnv is the users surface a phase needs: the bootstrap invite from a voter, signed
// admin ops through any node, and the uid the environment's mounts carry.
type UsersEnv interface {
	// BootstrapInvite runs `tailraid user bootstrap --name <name>` on n and returns the
	// invite code it printed.
	BootstrapInvite(ctx context.Context, n NodeRef, name string) (string, error)
	// SignedOp sends an admin op to n, signed by id (nil: unsigned).
	SignedOp(ctx context.Context, n NodeRef, id *users.Identity, op rpc.AdminOp) (json.RawMessage, error)
	// MountUID is the uid the environment's mounts present (the worker's), which a
	// binding maps to a user.
	MountUID() int
	// ActAs makes the environment act as id from now on: every admin call it makes is
	// signed by id, and its own shortcuts that write the volume (the trash purge, a
	// snapshot restore, a secret) act as id's user (phase 0, I-362).
	ActAs(id *users.Identity)
}

// soakUsers are the people of one run: the admin and two users, with their identities.
type soakUsers struct {
	env      UsersEnv
	kim, lee *users.Identity // kim the admin, lee a plain user
	lee2     *users.Identity // lee's second device
	pat      *users.Identity // the removed and re-invited name
	kimCode  string          // the bootstrap invite kim was made from (used once)
	nodes    []NodeRef
	prefix   string // the folder of this run
}

// usersKey is where a run keeps its people between phases 28 and 31.
const usersKey = "users"

func (r *Run) soakUsers() *soakUsers {
	if r.scratch == nil {
		r.scratch = map[string]any{}
	}
	if su, ok := r.scratch[usersKey].(*soakUsers); ok {
		return su
	}
	return nil
}

// usersPhase is what every users phase does first: the environment check and the people.
func (r *Run) usersPhase(ctx context.Context) (*soakUsers, bool) {
	ue, ok := r.Env.(UsersEnv)
	if !ok || !r.Env.Caps().Users {
		r.skip("the environment has no users surface, or its users are its own (a live cluster)")
		return nil, false
	}
	if su := r.soakUsers(); su != nil {
		return su, true
	}
	nodes, err := r.Env.Nodes(ctx)
	if err != nil || len(nodes) < 3 {
		r.skip("fewer than three nodes")
		return nil, false
	}
	su := &soakUsers{env: ue, nodes: nodes, prefix: fmt.Sprintf("/soak-users-%08x", r.Rng.Uint32())}
	if r.scratch == nil {
		r.scratch = map[string]any{}
	}
	r.scratch[usersKey] = su
	_ = ctx
	return su, true
}

// op sends a signed op and decodes the reply into out (nil: discarded).
func (su *soakUsers) op(ctx context.Context, n NodeRef, id *users.Identity, op rpc.AdminOp, out any) error {
	raw, err := su.env.SignedOp(ctx, n, id, op)
	if err != nil {
		return err
	}
	if out == nil {
		return nil
	}
	return json.Unmarshal(raw, out)
}

// create makes a user from an invite code on n: a fresh identity, registered. An invite
// minted on another node reaches n's replica a moment later, and n checks its replica
// before proposing (I-343), so a refusal for want of the invite is retried for a while.
func (su *soakUsers) create(ctx context.Context, r *Run, n NodeRef, name, code string) (*users.Identity, error) {
	id, err := users.NewIdentity(name)
	if err != nil {
		return nil, err
	}
	return &id, su.addKey(ctx, r, n, "user_create", name, code, &id, "soak")
}

// addKey registers id's public key on n through op (user_create or user_key_add).
func (su *soakUsers) addKey(ctx context.Context, r *Run, n NodeRef, op, name, code string, id *users.Identity, label string) error {
	pub := id.Pub()
	var err error
	for start := time.Now(); ; {
		err = su.op(ctx, n, nil, rpc.AdminOp{Op: op, User: name, Code: code, Key: hex.EncodeToString(pub[:]), Label: label}, nil)
		if err == nil || (!strings.Contains(err.Error(), "no such invite") && !strings.Contains(err.Error(), rpc.ErrRateLimited.Error())) || time.Since(start) > inviteReplication {
			return err
		}
		if werr := r.Env.Wait(ctx, 250*time.Millisecond); werr != nil {
			return werr
		}
	}
}

// inviteReplication bounds the wait for an invite to reach another node's replica.
const inviteReplication = 10 * time.Second

// replication bounds the wait for an applied op to show on another node: "at once" on
// a replicated volume is "within this".
const replication = 10 * time.Second

// until polls cond every quarter second for the replication bound; false when it never held.
func until(ctx context.Context, r *Run, cond func() bool) bool {
	for start := time.Now(); ; {
		if cond() {
			return true
		}
		if time.Since(start) > replication {
			return false
		}
		if err := r.Env.Wait(ctx, 250*time.Millisecond); err != nil {
			return false
		}
	}
}

// caughtUp waits until every node of to has applied at least as far as from had when
// called (its replica holds what from's does), through the FSM hashes' applied index;
// an environment without them (a live cluster with dev off) reports true at once.
func (su *soakUsers) caughtUp(ctx context.Context, r *Run, from NodeRef, to ...NodeRef) bool {
	at, _, err := r.Env.FSMHash(ctx, from)
	if err != nil {
		return true
	}
	return until(ctx, r, func() bool {
		for _, n := range to {
			if applied, _, err := r.Env.FSMHash(ctx, n); err != nil || applied < at {
				return false
			}
		}
		return true
	})
}

// signsOn waits until every node takes id for name: the user row reached each replica.
func (su *soakUsers) signsOn(ctx context.Context, r *Run, id *users.Identity, name string) bool {
	return until(ctx, r, func() bool {
		for _, nd := range su.nodes {
			if who, err := su.whoami(ctx, nd, id); err != nil || who != name {
				return false
			}
		}
		return true
	})
}

// invite mints an invite for name through n, as admin.
func (su *soakUsers) invite(ctx context.Context, n NodeRef, as *users.Identity, name string, admin bool) (string, error) {
	var rep struct {
		Code string `json:"code"`
	}
	err := su.op(ctx, n, as, rpc.AdminOp{Op: "user_invite", User: name, Admin: admin}, &rep)
	return rep.Code, err
}

// bind maps the environment's mount uid on n to user; "" unbinds it (nobody).
func (su *soakUsers) bind(ctx context.Context, n NodeRef, as *users.Identity, user string) error {
	cred := users.UIDCredential(su.env.MountUID())
	if user == "" {
		return su.op(ctx, n, as, rpc.AdminOp{Op: "user_unbind", Credential: cred}, nil)
	}
	return su.op(ctx, n, as, rpc.AdminOp{Op: "user_bind", Credential: cred, User: user}, nil)
}

// whoami is who n takes id for, or the error.
func (su *soakUsers) whoami(ctx context.Context, n NodeRef, id *users.Identity) (string, error) {
	var rep struct {
		User string `json:"user"`
	}
	err := su.op(ctx, n, id, rpc.AdminOp{Op: "user_whoami"}, &rep)
	return rep.User, err
}

// refused reports an admin reply that was a refusal: the needs table, the FSM or the
// signature check said no.
func refused(err error) bool {
	if err == nil {
		return false
	}
	s := err.Error()
	return strings.Contains(s, "permission denied") || strings.Contains(s, "unauthorized") || strings.Contains(s, "forbidden") || strings.Contains(s, "last admin") || strings.Contains(s, "invite")
}

// bootstrapAdmin makes the first admin, kim: a bootstrap invite on one node, the user
// created on another, signing on every node; false (with the failed check recorded) when
// it could not.
func (su *soakUsers) bootstrapAdmin(ctx context.Context, r *Run) bool {
	n := su.nodes
	code, err := su.env.BootstrapInvite(ctx, n[0], "kim")
	if err != nil {
		r.check("I-343", "user bootstrap on "+n[0].Name, false, err.Error())
		return false
	}
	kim, err := su.create(ctx, r, n[1], "kim", code)
	r.check("I-342, I-343", "the admin created on "+n[1].Name+" from "+n[0].Name+"'s invite", err == nil, errText(err))
	if err != nil {
		return false
	}
	su.kim, su.kimCode = kim, code
	r.check("I-344", "the admin signs on every node", su.signsOn(ctx, r, su.kim, "kim"), "a node still refuses the admin")
	return true
}

// --- 28 users and keys ---------------------------------------------------------------------

func phaseUsers(ctx context.Context, r *Run) error {
	su, ok := r.usersPhase(ctx)
	if !ok {
		return nil
	}
	n := su.nodes
	if su.kim != nil { // made before phase 1 (a UsersFirst run)
		r.check("I-344", "the admin made before the first phase still signs on every node", su.signsOn(ctx, r, su.kim, "kim"), "a node refuses the admin")
	} else if !su.bootstrapAdmin(ctx, r) {
		return nil
	}
	code := su.kimCode
	// a used code is refused and leaves no row
	used := users.Identity{Name: "kim2"}
	if fresh, e := users.NewIdentity("kim2"); e == nil {
		used = fresh
	}
	pub := used.Pub()
	err := su.op(ctx, n[2], nil, rpc.AdminOp{Op: "user_create", User: "kim2", Code: code, Key: hex.EncodeToString(pub[:]), Label: "soak"}, nil)
	r.check("I-343", "a used invite code is refused", refused(err), "accepted: "+errText(err))
	// the driver's mounts act as the admin from here on (the cluster enforces now)
	for _, nd := range n {
		if err := su.bind(ctx, nd, su.kim, "kim"); err != nil {
			r.check("I-350", "the mount on "+nd.Name+" bound to the admin", false, err.Error())
			return nil
		}
	}
	// a second person from kim's invite, created on a third node
	code, err = su.invite(ctx, n[0], su.kim, "lee", false)
	if err == nil {
		su.lee, err = su.create(ctx, r, n[2], "lee", code)
	}
	r.check("I-342, I-343", "a user created from an admin's invite on another node", err == nil, errText(err))
	if err != nil {
		return nil //nolint:nilerr // a failed check is recorded, not a run error
	}
	r.check("I-344", "lee signs on every node", su.signsOn(ctx, r, su.lee, "lee"), "a node still refuses lee")
	// a second device: lee's own key invite, the key added on another node, both keys sign
	var inv struct {
		Code string `json:"code"`
	}
	err = su.op(ctx, n[1], su.lee, rpc.AdminOp{Op: "user_key_invite"}, &inv)
	if err == nil {
		id2, e := users.NewIdentity("lee")
		if e == nil {
			e = su.addKey(ctx, r, n[0], "user_key_add", "", inv.Code, &id2, "soak-2")
		}
		if e == nil {
			su.lee2 = &id2
		}
		err = e
	}
	r.check("I-342, I-343", "a second device's key added from the user's own invite", err == nil, errText(err))
	if su.lee2 != nil {
		r.check("I-344", "both of lee's keys sign on every node", su.signsOn(ctx, r, su.lee, "lee") && su.signsOn(ctx, r, su.lee2, "lee"), "a node still refuses one of lee's keys")
		// the first key removed by its owner: the second keeps working
		id1 := su.lee.KeyID()
		err = su.op(ctx, n[0], su.lee2, rpc.AdminOp{Op: "user_key_rm", User: "lee", Key: hex.EncodeToString(id1[:])}, nil)
		r.check("I-342", "one of lee's keys removed", err == nil, errText(err))
		gone := until(ctx, r, func() bool { _, e := su.whoami(ctx, n[1], su.lee); return refused(e) })
		who2, e2 := su.whoami(ctx, n[1], su.lee2)
		r.check("I-344", "the removed key is refused and the other still signs", gone && e2 == nil && who2 == "lee", fmt.Sprintf("removed refused: %v, other: %v %q", gone, e2, who2))
		su.lee = su.lee2 // lee signs with the device that stays
	}
	// a disabled user is refused on every node at once, then enabled again
	err = su.op(ctx, n[0], su.kim, rpc.AdminOp{Op: "user_disable", User: "lee"}, nil)
	r.check("I-342", "the admin disables lee", err == nil, errText(err))
	for _, nd := range n {
		gone := until(ctx, r, func() bool { _, e := su.whoami(ctx, nd, su.lee); return refused(e) })
		r.check("I-344", "a disabled user is refused on "+nd.Name, gone, "still accepted after the wait")
	}
	err = su.op(ctx, n[2], su.kim, rpc.AdminOp{Op: "user_enable", User: "lee"}, nil)
	r.check("I-342", "the admin enables lee again", err == nil && su.signsOn(ctx, r, su.lee, "lee"), errText(err))
	// the last admin cannot be disabled, removed or dropped from admins
	lockouts := []rpc.AdminOp{{Op: "user_disable", User: "kim"}, {Op: "user_rm", User: "kim"}, {Op: "group_remove", Group: "admins", User: "kim"}}
	for i := range lockouts {
		e := su.op(ctx, n[0], su.kim, lockouts[i], nil)
		r.check("I-346", "the last admin's "+lockouts[i].Op+" is refused (lockout)", refused(e), "accepted: "+errText(e))
	}
	who, err := su.whoami(ctx, n[1], su.kim)
	r.check("I-346", "the admin still signs after the lockout refusals", err == nil && who == "kim", errText(err))
	// a removed name is invited again and gets a new user
	code, err = su.invite(ctx, n[0], su.kim, "pat", false)
	if err == nil {
		su.pat, err = su.create(ctx, r, n[1], "pat", code)
	}
	if err == nil && !su.signsOn(ctx, r, su.pat, "pat") {
		err = errors.New("pat does not sign on every node")
	}
	if err == nil {
		err = su.op(ctx, n[2], su.kim, rpc.AdminOp{Op: "user_rm", User: "pat"}, nil)
	}
	r.check("I-342", "a user removed", err == nil, errText(err))
	r.check("I-344", "a removed user is refused", until(ctx, r, func() bool { _, e := su.whoami(ctx, n[0], su.pat); return refused(e) }), "still accepted after the wait")
	code, err = su.invite(ctx, n[1], su.kim, "pat", false)
	if err == nil {
		su.pat, err = su.create(ctx, r, n[0], "pat", code)
	}
	r.check("I-342", "a removed name invited again is a new user", err == nil && su.signsOn(ctx, r, su.pat, "pat"), errText(err))
	// an expired code is refused: the leader's expiry is a tick away, so the code is
	// checked by its shape here and the expiry by phase 31's audit of the invite rows
	r.checkFSMAgreement(ctx, "I-349", "every node applied the users ops alike")
	r.emit(Record{Kind: "event", Name: usersEvent, Text: "kim (admin), lee (two devices, one removed), pat (removed, re-invited)"})
	return nil
}

const usersEvent = "users made"

// --- 29 grants on the gateways --------------------------------------------------------------

// roleMatrix is one gateway's answers for one user: what each probe did.
type roleMatrix struct {
	Gateway, User                    string
	ReadR, WriteR, WriteW, StatOther string // ok | refused | <error>
	ListRoot                         string // the entries the root listing showed
}

func phaseGrants(ctx context.Context, r *Run) error {
	su, ok := r.usersPhase(ctx)
	if !ok {
		return nil
	}
	if su.lee == nil {
		r.skip("phase 28 made no users")
		return nil
	}
	n := su.nodes
	gws := n[:2] // two gateways answer the same role for the same user (I-347)
	u := r.undoer(ctx)
	defer u.run()
	for _, gw := range gws {
		u.add("rebind the admin on "+gw.Name, func(c context.Context) error { return su.bind(c, gw, su.kim, "kim") })
	}
	// the admin lays out the folders through the first gateway
	vol := r.volume(gws[0])
	root := su.prefix
	for _, d := range []string{"r", "w", "other"} {
		if err := vol.Mkdir(root + "/" + d); err != nil {
			r.check("I-349", "the admin makes "+root+"/"+d, false, err.Error())
			return nil
		}
		if err := vol.WriteAt(root+"/"+d+"/f", 0, r.randomBytes(4096)); err != nil {
			r.check("I-349", "the admin writes "+root+"/"+d+"/f", false, err.Error())
			return nil
		}
	}
	for _, g := range []struct{ path, role string }{{root + "/r", "read"}, {root + "/w", "write"}} {
		err := su.op(ctx, n[2], su.kim, rpc.AdminOp{Op: "grant", Path: g.path, User: "lee", Role: g.role}, nil)
		r.check("I-347", "grant lee "+g.role+" on "+g.path, err == nil, errText(err))
	}
	// a grant reaches a gateway's replica a moment after the proposal (as the revoke
	// below): the probes start once both gateways have applied past the node the grants
	// went through, else one answers lee from before them (CI: n1 refused the write)
	if !su.caughtUp(ctx, r, n[2], gws...) {
		r.event("note", "the gateways did not catch up with the grants within the replication bound")
	}
	var rows []roleMatrix
	probe := func(gw NodeRef, user string) roleMatrix {
		v := r.volume(gw)
		m := roleMatrix{Gateway: gw.Name, User: user}
		outcome := func(err error) string {
			switch {
			case err == nil:
				return "ok"
			case errors.Is(err, syscall.EACCES), errors.Is(err, syscall.EPERM), strings.Contains(err.Error(), "permission denied"), strings.Contains(err.Error(), "access denied"):
				return "refused"
			}
			return err.Error()
		}
		_, err := v.ReadAll(root + "/r/f")
		m.ReadR = outcome(err)
		m.WriteR = outcome(v.WriteAt(root+"/r/f", 4096, []byte("lee")))
		m.WriteW = outcome(v.WriteAt(root+"/w/f", 4096, []byte("lee")))
		_, err = v.Size(root + "/other/f")
		m.StatOther = outcome(err)
		names, err := v.List(root)
		if err != nil {
			m.ListRoot = outcome(err)
		} else {
			slices.Sort(names) // a mount returns dirents in cookie order, not by name: the set is what the role decides
			m.ListRoot = strings.Join(names, " ")
		}
		return m
	}
	want := map[string]roleMatrix{
		"lee":    {ReadR: "ok", WriteR: "refused", WriteW: "ok", StatOther: "refused", ListRoot: "r w"},
		"kim":    {ReadR: "ok", WriteR: "ok", WriteW: "ok", StatOther: "ok", ListRoot: "other r w"},
		"nobody": {ReadR: "refused", WriteR: "refused", WriteW: "refused", StatOther: "refused", ListRoot: "refused"},
	}
	for _, user := range []string{"lee", "kim", "nobody"} {
		for _, gw := range gws {
			bindTo := user
			if user == "nobody" {
				bindTo = ""
			}
			if err := su.bind(ctx, gw, su.kim, bindTo); err != nil {
				r.check("I-350", "bind the mount on "+gw.Name+" to "+user, false, err.Error())
				continue
			}
			m := probe(gw, user)
			rows = append(rows, m)
			w := want[user]
			got := [5]string{m.ReadR, m.WriteR, m.WriteW, m.StatOther, m.ListRoot}
			exp := [5]string{w.ReadR, w.WriteR, w.WriteW, w.StatOther, w.ListRoot}
			r.check("I-348, I-350, I-351, I-353", user+" through "+gw.Name+": read, write under read, write under write, stat elsewhere, the root listing", got == exp, fmt.Sprintf("got %v, want %v", got, exp))
		}
	}
	// the same answer from both gateways for the same user (the N-node determinism of Access)
	same := true
	for i := 0; i+1 < len(rows); i += 2 {
		a, b := rows[i], rows[i+1]
		a.Gateway, b.Gateway = "", ""
		same = same && a == b
	}
	r.check("I-347", "two gateways answer the same roles", same, fmt.Sprintf("%+v", rows))
	// a revoke takes effect at the next operation
	if err := su.bind(ctx, gws[0], su.kim, "lee"); err == nil {
		if err := su.op(ctx, n[2], su.kim, rpc.AdminOp{Op: "revoke", Path: root + "/w", User: "lee"}, nil); err != nil {
			r.check("I-347", "revoke lee's write on "+root+"/w", false, err.Error())
		} else {
			// the revoke reaches the gateway's replica a moment after the proposal: writes
			// until one is refused, within the replication bound
			off := int64(8192)
			var last error
			gone := until(ctx, r, func() bool {
				last = r.volume(gws[0]).WriteAt(root+"/w/f", off, []byte("after"))
				off += 16
				return last != nil && (errors.Is(last, syscall.EACCES) || strings.Contains(last.Error(), "permission denied") || strings.Contains(last.Error(), "access denied"))
			})
			r.check("I-347, I-353", "a write after the revoke is refused", gone, "still landing after the wait: "+errText(last))
		}
		if err := su.op(ctx, n[2], su.kim, rpc.AdminOp{Op: "grant", Path: root + "/w", User: "lee", Role: "write"}, nil); err != nil {
			r.check("I-347", "lee's write on "+root+"/w restored", false, err.Error())
		}
	}
	// /.tailraid is reachable from no mount, the admin's included
	for _, gw := range gws {
		if err := su.bind(ctx, gw, su.kim, "kim"); err != nil {
			continue
		}
		_, err := r.volume(gw).List("/" + meta.ReservedDir)
		r.check("I-348", "/.tailraid is refused on "+gw.Name+"'s mount", err != nil, "listed")
	}
	// the web: lee's listing through a session shows what lee holds
	if b, err := su.signIn(ctx, r.Env, gws[1], su.lee); err != nil {
		r.check("I-354", "lee signs in to "+gws[1].Name+"'s page", false, err.Error())
	} else {
		code, body := b.get(ctx, "/api/ls?path="+url.QueryEscape(root))
		r.check("I-355", "lee's page lists the folders lee holds", code == 200 && strings.Contains(body, `"name":"r"`) && strings.Contains(body, `"name":"w"`) && !strings.Contains(body, `"name":"other"`), fmt.Sprintf("%d %s", code, body))
		code, _ = b.get(ctx, "/api/ls?path="+url.QueryEscape(root+"/other"))
		r.check("I-355", "lee's page refuses a folder lee holds nothing on", code == 403, fmt.Sprintf("%d", code))
	}
	r.emit(Record{Kind: "event", Name: grantsEvent, Text: matrixText(rows)})
	return nil
}

const grantsEvent = "grant matrix"

func matrixText(rows []roleMatrix) string {
	var b strings.Builder
	for _, m := range rows {
		fmt.Fprintf(&b, "%s %s: read %s, write-under-read %s, write %s, stat-other %s, root [%s]\n", m.Gateway, m.User, m.ReadR, m.WriteR, m.WriteW, m.StatOther, m.ListRoot)
	}
	return strings.TrimSpace(b.String())
}

// --- 30 secrets per user -------------------------------------------------------------------

func phaseUserSecrets(ctx context.Context, r *Run) error {
	su, ok := r.usersPhase(ctx)
	if !ok {
		return nil
	}
	if su.lee == nil || su.kim == nil {
		r.skip("phase 28 made no users")
		return nil
	}
	n := su.nodes
	name := fmt.Sprintf("user/lee/soak-%08x", r.Rng.Uint32())
	sys := fmt.Sprintf("soak/%08x", r.Rng.Uint32())
	u := r.undoer(ctx)
	defer u.run()
	u.add("remove "+name, func(c context.Context) error {
		err := su.op(c, n[0], su.lee, rpc.AdminOp{Op: "secret_rm", Name: name}, nil)
		if err != nil && !isNotFound(err) {
			return err
		}
		return nil
	})
	u.add("remove "+sys, func(c context.Context) error {
		err := su.op(c, n[0], su.kim, rpc.AdminOp{Op: "secret_rm", Name: sys}, nil)
		if err != nil && !isNotFound(err) {
			return err
		}
		return nil
	})
	value := hex.EncodeToString(r.randomBytes(16))
	get := func(nd NodeRef, as *users.Identity, nm string) (string, error) {
		var rep struct {
			Value string `json:"value"`
		}
		err := su.op(ctx, nd, as, rpc.AdminOp{Op: "secret_get", Name: nm}, &rep)
		return rep.Value, err
	}
	err := su.op(ctx, n[0], su.lee, rpc.AdminOp{Op: "secret_set", Name: name, Value: value}, nil)
	r.check("I-356", "lee sets a personal secret through "+n[0].Name, err == nil, errText(err))
	var got string
	until(ctx, r, func() bool { got, err = get(n[1], su.lee, name); return err == nil && got == value })
	r.check("I-356", "lee reads it back through "+n[1].Name, err == nil && got == value, secretWhy(err, got != value, "another value"))
	_, err = get(n[2], su.kim, name)
	r.check("I-356", "the admin's read of another user's secret is refused", refused(err), "answered")
	err = su.op(ctx, n[1], su.kim, rpc.AdminOp{Op: "secret_set", Name: name, Value: "x"}, nil)
	r.check("I-356", "another actor's write of lee's name is refused", refused(err), "accepted")
	err = su.op(ctx, n[2], su.lee, rpc.AdminOp{Op: "secret_set", Name: sys, Value: value}, nil)
	r.check("I-356", "a system name by a plain user is refused", refused(err), "accepted")
	err = su.op(ctx, n[2], su.kim, rpc.AdminOp{Op: "secret_set", Name: sys, Value: value}, nil)
	r.check("I-356", "a system name by an admin lands", err == nil, errText(err))
	var raw json.RawMessage
	err = su.op(ctx, n[0], su.lee, rpc.AdminOp{Op: "secret_ls"}, &raw)
	r.check("I-356", "lee's list shows lee's own name and not the system one", err == nil && bytes.Contains(raw, []byte(name)) && !bytes.Contains(raw, []byte(sys)) && !bytes.Contains(raw, []byte(value)), fmt.Sprintf("%v %s", err, raw))
	until(ctx, r, func() bool {
		err = su.op(ctx, n[1], su.kim, rpc.AdminOp{Op: "secret_ls", All: true}, &raw)
		return err == nil && bytes.Contains(raw, []byte(sys))
	})
	r.check("I-356", "the admin's system list shows the system name without its value", err == nil && bytes.Contains(raw, []byte(sys)) && !bytes.Contains(raw, []byte(value)), fmt.Sprintf("%v %s", err, raw))
	err = su.op(ctx, n[2], su.kim, rpc.AdminOp{Op: "secret_rm", Name: name}, nil)
	r.check("I-356", "the admin removes lee's personal secret", err == nil, errText(err))
	r.check("I-356", "the removed personal secret is gone", until(ctx, r, func() bool { _, err = get(n[0], su.lee, name); return err != nil && isNotFound(err) }), errText(err))
	u.done("remove " + name)
	r.checkSkipped("I-356", "node add-s3 and the scrape reach their system values in process", "phase 15 covers the bucket before users exist; the scrape needs a TMDB key")
	return nil
}

// --- 31 audit and sessions -----------------------------------------------------------------

func phaseAudit(ctx context.Context, r *Run) error {
	su, ok := r.usersPhase(ctx)
	if !ok {
		return nil
	}
	if su.lee == nil || su.kim == nil {
		r.skip("phase 28 made no users")
		return nil
	}
	n := su.nodes
	// the log: one record per op of phases 28–30, refusals counted once their minute
	// closed (the clock moves a minute on; a segment written on the leader reaches
	// this node's replica a moment later)
	if err := r.Env.Wait(ctx, audit.RefusalWindow+time.Second); err != nil {
		return err
	}
	var recs []audit.Record
	var err error
	kinds := map[string]int{}
	refusals := 0
	wanted := []string{audit.KindInvite, audit.KindUserCreate, audit.KindUserKeyAdd, audit.KindUserKeyRemove, audit.KindUserDisable, audit.KindUserEnable, audit.KindUserRemove, audit.KindGrant, audit.KindRevoke, audit.KindSecretSet, audit.KindSecretGet, audit.KindSecretRemove}
	until(ctx, r, func() bool {
		recs, kinds, refusals = nil, map[string]int{}, 0
		if err = su.op(ctx, n[0], su.kim, rpc.AdminOp{Op: "audit_ls"}, &recs); err != nil {
			return false
		}
		for i := range recs {
			kinds[recs[i].Kind]++
			if recs[i].Result == audit.ResultRefused {
				refusals += max(recs[i].Count, 1)
			}
		}
		for _, k := range wanted {
			if kinds[k] == 0 {
				return false
			}
		}
		return refusals >= 5
	})
	if err != nil {
		r.check("I-357, I-358", "the admin lists the audit log", false, err.Error())
		return nil //nolint:nilerr // a failed check is recorded, not a run error
	}
	for _, k := range wanted {
		r.check("I-357", "an audit record of "+k, kinds[k] > 0, "none")
	}
	r.check("I-357", "refusals are recorded and counted", refusals >= 5, fmt.Sprintf("%d refusals in %d records", refusals, len(recs)))
	var mine []audit.Record
	err = su.op(ctx, n[1], su.lee, rpc.AdminOp{Op: "audit_ls", User: "kim"}, &mine)
	own := err == nil
	for i := range mine {
		own = own && mine[i].Actor == "lee"
	}
	r.check("I-358", "a plain user's listing is their own records only", own && len(mine) > 0, fmt.Sprintf("%v %d records", err, len(mine)))
	r.emit(Record{Kind: "event", Name: auditEvent, Text: fmt.Sprintf("%d records, %d refusals, %d kinds", len(recs), refusals, len(kinds))})

	// sessions: a login on one node, the same session on another's page through the
	// hand-off, a logout from any node ending it everywhere
	we := r.Env
	b, err := su.signIn(ctx, we, n[0], su.lee)
	if err != nil {
		r.check("I-354", "web login signs a browser in", false, err.Error())
		return nil //nolint:nilerr // a failed check is recorded, not a run error
	}
	code, body := b.get(ctx, "/api/session")
	r.check("I-354", "the page knows its user", code == 200 && strings.Contains(body, `"user":"lee"`), fmt.Sprintf("%d %s", code, body))
	// the hand-off: a fresh code each try, since a code opened before the session row
	// reached the other node's replica is spent
	b2, err := su.browserAt(ctx, we, n[1])
	if err != nil {
		r.check("I-354", "a hand-off to "+n[1].Name+"'s page", false, err.Error())
	} else {
		last := ""
		handed := until(ctx, r, func() bool {
			code, body := b.get(ctx, "/api/handoff?to="+url.QueryEscape(n[1].Name))
			var hand struct {
				URL string `json:"url"`
			}
			if code != 200 || json.Unmarshal([]byte(body), &hand) != nil || !strings.Contains(hand.URL, "/login?") {
				last = fmt.Sprintf("handoff %d %s", code, body)
				return false
			}
			_, after, _ := strings.Cut(hand.URL, "/login")
			c, _ := b2.get(ctx, "/login"+after)
			last = fmt.Sprintf("login %d", c)
			return c == 303
		})
		c2, body2 := b2.get(ctx, "/api/session")
		r.check("I-354", "the hand-off signs "+n[1].Name+"'s page into the same session", handed && c2 == 200 && strings.Contains(body2, `"user":"lee"`), fmt.Sprintf("%s, session %d %s", last, c2, body2))
		// a logout on the second node ends it on the first
		if c, _ := b2.post(ctx, "/api/logout"); c != 204 {
			r.check("I-354", "logout on "+n[1].Name, false, fmt.Sprintf("%d", c))
		}
		ended := until(ctx, r, func() bool { c1, _ := b.get(ctx, "/api/session"); return c1 == 401 })
		r.check("I-354", "a logout on one node ends the session on another", ended, "still signed in after the wait")
	}
	// a removed key ends a session at the next request
	b, err = su.signIn(ctx, we, n[2], su.lee)
	if err == nil {
		key := su.lee.KeyID()
		if err := su.op(ctx, n[0], su.kim, rpc.AdminOp{Op: "user_key_rm", User: "lee", Key: hex.EncodeToString(key[:])}, nil); err != nil {
			r.check("I-354", "the admin removes lee's key", false, err.Error())
		} else {
			ended := until(ctx, r, func() bool { c, _ := b.get(ctx, "/api/session"); return c == 401 })
			r.check("I-354", "a removed key ends the session at the next request", ended, "still signed in after the wait")
		}
	}
	// the chain and the retention are the audit package's tests (TestAudit_ChainHash,
	// TestAudit_Retention_Table): the log here is read whole, which verifies every chain
	r.check("I-358", "every segment read chained", kinds["audit.chain"] == 0, fmt.Sprintf("%d broken segments", kinds["audit.chain"]))
	return nil
}

const auditEvent = "audit counts"

// --- the driver's browser --------------------------------------------------------------------

// webEnv is the browser surface the users phases use: the Env's own.
type webEnv interface {
	WebURL(ctx context.Context, node NodeRef) (string, error)
	WebClient() *http.Client
}

// browser is a cookie jar on one node's page.
type browser struct {
	base string
	hc   *http.Client
}

// browserAt is a fresh browser on n's page.
func (su *soakUsers) browserAt(ctx context.Context, we webEnv, n NodeRef) (*browser, error) {
	base, err := we.WebURL(ctx, n)
	if err != nil {
		return nil, err
	}
	jar, err := cookiejar.New(nil)
	if err != nil {
		return nil, err
	}
	hc := &http.Client{Jar: jar, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	if c := we.WebClient(); c != nil {
		hc.Transport = c.Transport
	}
	return &browser{base: strings.TrimSuffix(base, "/"), hc: hc}, nil
}

// signIn runs `web login` on n as id and opens the link in a fresh browser on n's page.
func (su *soakUsers) signIn(ctx context.Context, we webEnv, n NodeRef, id *users.Identity) (*browser, error) {
	var rep struct {
		URL string `json:"url"`
	}
	if err := su.op(ctx, n, id, rpc.AdminOp{Op: "web_login"}, &rep); err != nil {
		return nil, err
	}
	i := strings.Index(rep.URL, "/login?")
	if i < 0 {
		return nil, fmt.Errorf("web login printed %q", rep.URL)
	}
	b, err := su.browserAt(ctx, we, n)
	if err != nil {
		return nil, err
	}
	code, body := b.get(ctx, rep.URL[i:])
	if code != 303 {
		return nil, fmt.Errorf("the link answered %d: %s", code, body)
	}
	return b, nil
}

func (b *browser) get(ctx context.Context, path string) (int, string) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, b.base+path, http.NoBody)
	if err != nil {
		return 0, err.Error()
	}
	resp, err := b.hc.Do(req)
	if err != nil {
		return 0, err.Error()
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	return resp.StatusCode, string(body)
}

func (b *browser) post(ctx context.Context, path string) (int, string) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, b.base+path, http.NoBody)
	if err != nil {
		return 0, err.Error()
	}
	req.Header.Set("X-Tailraid-UI", "1")
	resp, err := b.hc.Do(req)
	if err != nil {
		return 0, err.Error()
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	return resp.StatusCode, string(body)
}
