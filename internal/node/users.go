package node

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"net/netip"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/zombar/tailraid/internal/meta"
	"github.com/zombar/tailraid/internal/rpc"
	"github.com/zombar/tailraid/internal/users"
)

// The node side of users, groups and permissions (docs/21): the needs table applied with
// the FSM before an admin op runs (I-345), the user, group, grant and binding ops, the
// invite pre-check and its rate limit (I-343), and the leader's expiry of invites.

// inviteAttempts is how often one source may present a code: once a second (I-343).
const inviteAttempts = time.Second

// authorize applies the needs table (docs/21 §2, I-345) once the cluster has users: an
// op that needs a user has an actor (the server checked the signature), one that needs
// admins has an admin, and one scoped to a path has the role on the inode the path
// names, resolved before the check so a missing path answers as a missing right.
func (a *adminOps) authorize(op rpc.AdminOp) error {
	need, _ := op.Needs()
	st := a.node.FSM
	if need == rpc.NeedNone || !st.Enforcing() {
		return nil
	}
	if op.Actor == 0 {
		return users.ErrUnauthorized
	}
	switch need {
	case rpc.NeedUser:
		return nil
	case rpc.NeedAdmins:
		if !st.IsAdmin(op.Actor) {
			return meta.ErrAccess
		}
		return nil
	case rpc.NeedWriteOn, rpc.NeedAdminOn:
		ino, ok := a.opTarget(op)
		if !ok {
			if st.IsAdmin(op.Actor) {
				return nil // an admin holds the role everywhere: the handler answers what is wrong
			}
			return meta.ErrAccess // a missing path answers as a missing right (I-345)
		}
		want := meta.RoleWrite
		if need == rpc.NeedAdminOn {
			want = meta.RoleAdmin
		}
		if st.Access(op.Actor, ino) < want {
			return meta.ErrAccess
		}
	case rpc.NeedNone:
	}
	return nil
}

// opTarget is the inode a path-scoped op acts on: the folder a trash entry goes back to
// or came from, the destination of a restore, else the path itself.
func (a *adminOps) opTarget(op rpc.AdminOp) (meta.Ino, bool) {
	st := a.node.FSM
	switch op.Op {
	case "trash_restore", "trash_purge":
		in, ok := st.GetInodeAny(meta.Ino(op.Ino))
		if !ok {
			return 0, false
		}
		if op.Op == "trash_restore" && op.To != "" {
			return a.resolve(op.To)
		}
		return in.Parent, true
	case "snapshot_restore":
		if op.To != "" {
			return a.resolve(op.To)
		}
		parts := meta.SplitPath(op.Path)
		if len(parts) == 0 {
			return meta.RootIno, true
		}
		return a.resolveParts(parts[:len(parts)-1])
	}
	return a.resolve(op.Path)
}

func (a *adminOps) resolve(path string) (meta.Ino, bool) {
	return a.resolveParts(meta.SplitPath(path))
}

func (a *adminOps) resolveParts(parts []string) (meta.Ino, bool) {
	in, err := a.node.FSM.Resolve(parts)
	if err != nil {
		return 0, false
	}
	return in.Ino, true
}

// actorName is the name of the op's actor, "" for nobody.
func (a *adminOps) actorName(op rpc.AdminOp) string {
	if op.Actor == 0 {
		return ""
	}
	u, ok := a.node.FSM.User(op.Actor)
	if !ok {
		return ""
	}
	return u.Name
}

// inviteLimiter admits one code attempt a second per source (I-343).
type inviteLimiter struct {
	mu   sync.Mutex
	last map[netip.Addr]time.Time
}

func (l *inviteLimiter) allow(src netip.Addr, now time.Time) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.last == nil {
		l.last = map[netip.Addr]time.Time{}
	}
	if t, ok := l.last[src]; ok && now.Sub(t) < inviteAttempts {
		return false
	}
	l.last[src] = now
	for s, t := range l.last { // bounded: a source silent for a minute is forgotten
		if now.Sub(t) > time.Minute {
			delete(l.last, s)
		}
	}
	return true
}

// UserRow is one row of `user ls`.
type UserRow struct {
	Idx     meta.UserIdx `json:"idx"`
	Name    string       `json:"name"`
	State   string       `json:"state"`
	Keys    []KeyRow     `json:"keys,omitempty"`
	Groups  []string     `json:"groups,omitempty"`
	Created int64        `json:"created"`
	By      string       `json:"by,omitempty"`
}

// KeyRow is one key of a user.
type KeyRow struct {
	ID    string `json:"id"`
	Label string `json:"label,omitempty"`
	Added int64  `json:"added"`
}

// GroupRow is one row of `group ls`.
type GroupRow struct {
	Idx     meta.GroupIdx `json:"idx"`
	Name    string        `json:"name"`
	Members []string      `json:"members,omitempty"`
}

// GrantRow is one row of `access ls` (I-358).
type GrantRow struct {
	Path    string `json:"path"`
	Ino     uint64 `json:"ino"`
	Subject string `json:"subject"` // a user's name, or @group
	Role    string `json:"role"`
	By      string `json:"by,omitempty"`
	At      int64  `json:"at"`
}

// InviteReply is what an invite answers with: the one-time code and what it is for.
type InviteReply struct {
	Code    string `json:"code"`
	Name    string `json:"name,omitempty"`
	User    string `json:"user,omitempty"`
	Admin   bool   `json:"admin,omitempty"`
	Expires int64  `json:"expires"`
}

// userOp answers the user, group, grant and binding ops (docs/21 §9).
func (a *adminOps) userOp(ctx context.Context, op rpc.AdminOp) (any, error) {
	st := a.node.FSM
	now := a.clk.Now()
	switch op.Op {
	case "user_invite":
		return a.mintInvite(ctx, op.Actor, false, meta.Invite{Name: op.User, Kind: meta.InviteUser, Admin: op.Admin}, now)
	case "user_key_invite":
		target := op.User
		if target == "" {
			target = a.actorName(op)
		}
		u, ok := st.UserByName(target)
		if !ok {
			return nil, fmt.Errorf("%w: no user %q", meta.ErrNotExist, target)
		}
		if u.Idx != op.Actor && st.Enforcing() && !st.IsAdmin(op.Actor) {
			return nil, meta.ErrAccess // a key invite for another is an admin's
		}
		return a.mintInvite(ctx, op.Actor, false, meta.Invite{Kind: meta.InviteKey, ForUser: u.Idx}, now)
	case "user_bootstrap":
		if !users.TakeBootstrap(a.dataDir, op.Value) {
			return nil, meta.ErrAccess
		}
		self, ok := st.NodeByID(a.node.ID)
		if !ok || !self.Voter {
			return nil, fmt.Errorf("%w: user bootstrap runs on a voter", meta.ErrAccess)
		}
		return a.mintInvite(ctx, 0, true, meta.Invite{Name: op.User, Kind: meta.InviteUser, Admin: true}, now)
	case "user_create", "user_key_add":
		if !a.invites.allow(op.Source, now) {
			return nil, fmt.Errorf("%w: one attempt a second", rpc.ErrRateLimited)
		}
		inv, ok := st.Invite(meta.HashCode(op.Code))
		kind := meta.InviteUser
		if op.Op == "user_key_add" {
			kind = meta.InviteKey
		}
		if !ok || inv.Kind != kind || now.UnixNano() >= inv.ExpiresAt || (kind == meta.InviteUser && inv.Name != op.User) {
			return nil, meta.ErrInvite // checked on the replica first: a guess never reaches the log (I-343)
		}
		var key [32]byte
		if b, err := hex.DecodeString(op.Key); err != nil || len(b) != len(key) {
			return nil, fmt.Errorf("%w: a public key is 32 bytes, hex", meta.ErrBadCommand)
		} else {
			copy(key[:], b)
		}
		action := meta.UserCreate
		if kind == meta.InviteKey {
			action = meta.UserKeyAdd
		}
		cmd := meta.Command{Op: meta.OpUser, UserOp: &meta.UserOp{Action: action, Name: op.User, Code: op.Code, Key: key, Label: op.Label, Now: now.UnixNano()}}
		res, err := a.proposeResult(ctx, cmd)
		if err != nil {
			return nil, err
		}
		u, _ := st.User(meta.UserIdx(res.Ino))
		kid := meta.UserKeyIDOf(key)
		return map[string]any{"user": u.Name, "idx": u.Idx, "key": hex.EncodeToString(kid[:])}, nil
	case "user_key_rm":
		u, ok := st.UserByName(op.User)
		if !ok {
			return nil, fmt.Errorf("%w: no user %q", meta.ErrNotExist, op.User)
		}
		var id meta.UserKeyID
		if b, err := hex.DecodeString(op.Key); err != nil || len(b) != len(id) {
			return nil, fmt.Errorf("%w: a key id is 8 bytes, hex", meta.ErrBadCommand)
		} else {
			copy(id[:], b)
		}
		return nil, a.propose(ctx, meta.Command{Op: meta.OpUser, Actor: op.Actor, UserOp: &meta.UserOp{Action: meta.UserKeyRemove, Target: u.Idx, KeyID: id, Now: now.UnixNano()}})
	case "user_disable", "user_enable", "user_rm":
		u, ok := st.UserByName(op.User)
		if !ok {
			return nil, fmt.Errorf("%w: no user %q", meta.ErrNotExist, op.User)
		}
		action := map[string]meta.UserAction{"user_disable": meta.UserDisable, "user_enable": meta.UserEnable, "user_rm": meta.UserRemove}[op.Op]
		return map[string]string{"user": u.Name}, a.propose(ctx, meta.Command{Op: meta.OpUser, Actor: op.Actor, UserOp: &meta.UserOp{Action: action, Target: u.Idx, Now: now.UnixNano()}})
	case "user_ls":
		return a.userRows(op.User), nil
	case "web_login":
		// a session of the volume (docs/21 §6, I-354): the row through the leader, the
		// token to the browser through a one-time code under the cluster key, the link
		// to this node's own page
		if a.webAddr == "" {
			return nil, errors.New("web login: this node has no web listener (listen.web)")
		}
		key, ok := a.secrets.Local()
		if !ok {
			return nil, errors.New("web login: this node holds no cluster key yet; try again in a minute")
		}
		tok, err := users.NewToken()
		if err != nil {
			return nil, err
		}
		exp := now.Add(a.sessionLife)
		if err := a.propose(ctx, meta.Command{Op: meta.OpSession, Actor: op.Actor, SessionOp: &meta.SessionOp{User: op.Actor, Hash: sha256.Sum256(tok[:]), Key: op.SignKey, ExpiresAt: exp.UnixNano(), Now: now.UnixNano()}}); err != nil {
			return nil, err
		}
		code, err := users.MintCode(key, tok, now)
		if err != nil {
			return nil, err
		}
		return map[string]any{"url": "http://" + a.webAddr + "/login?code=" + code, "expires_at": exp.UTC().Format(time.RFC3339)}, nil
	case "web_logout":
		if op.All {
			return map[string]any{"ended": -1}, a.propose(ctx, meta.Command{Op: meta.OpSession, Actor: op.Actor, SessionOp: &meta.SessionOp{Delete: true, All: true, User: op.Actor, Now: now.UnixNano()}})
		}
		ended := 0
		for _, se := range st.Sessions() { // the sessions this device's key minted
			if se.User != op.Actor || se.Key != op.SignKey {
				continue
			}
			if err := a.propose(ctx, meta.Command{Op: meta.OpSession, Actor: op.Actor, SessionOp: &meta.SessionOp{Delete: true, Hash: se.Hash, Now: now.UnixNano()}}); err != nil {
				return nil, err
			}
			ended++
		}
		return map[string]any{"ended": ended}, nil
	case "audit_ls":
		return a.auditList(ctx, op)
	case "user_whoami":
		u, ok := st.User(op.Actor)
		if !ok {
			return nil, users.ErrUnauthorized
		}
		return map[string]any{"user": u.Name, "idx": u.Idx, "admin": st.IsAdmin(u.Idx)}, nil
	case "user_bind", "user_unbind":
		if a.bindings == nil {
			return nil, errors.New("users: this node keeps no bindings")
		}
		if !users.ValidCredential(op.Credential) {
			return nil, users.ErrBadCredential
		}
		if op.Op == "user_unbind" {
			bound, _ := a.bindings.Lookup(op.Credential)
			if st.Enforcing() && bound != a.actorName(op) && !st.IsAdmin(op.Actor) {
				return nil, meta.ErrAccess
			}
			return map[string]string{"credential": op.Credential}, a.bindings.Unbind(op.Credential)
		}
		if _, ok := st.UserByName(op.User); !ok {
			return nil, fmt.Errorf("%w: no user %q", meta.ErrNotExist, op.User)
		}
		if st.Enforcing() && op.User != a.actorName(op) && !st.IsAdmin(op.Actor) {
			return nil, meta.ErrAccess // a user binds their own credential, an admin anyone's (I-350)
		}
		return map[string]string{"credential": op.Credential, "user": op.User}, a.bindings.Bind(op.Credential, op.User)
	case "group_create":
		return nil, a.propose(ctx, meta.Command{Op: meta.OpGroup, Actor: op.Actor, GroupOp: &meta.GroupOp{Action: meta.GroupCreate, Name: op.Group, Now: now.UnixNano()}})
	case "group_rm", "group_add", "group_remove":
		g, ok := st.GroupByName(op.Group)
		if !ok {
			return nil, fmt.Errorf("%w: no group %q", meta.ErrNotExist, op.Group)
		}
		gop := &meta.GroupOp{Group: g.Idx, Now: now.UnixNano()}
		switch op.Op {
		case "group_rm":
			gop.Action = meta.GroupRemove
		default:
			u, ok := st.UserByName(op.User)
			if !ok {
				return nil, fmt.Errorf("%w: no user %q", meta.ErrNotExist, op.User)
			}
			gop.User = u.Idx
			gop.Action = meta.GroupAddMember
			if op.Op == "group_remove" {
				gop.Action = meta.GroupRemoveMember
			}
		}
		return nil, a.propose(ctx, meta.Command{Op: meta.OpGroup, Actor: op.Actor, GroupOp: gop})
	case "group_ls":
		return a.groupRows(), nil
	case "grant", "revoke":
		ino, ok := a.resolve(op.Path)
		if !ok {
			return nil, fmt.Errorf("%w: %s", meta.ErrNotExist, op.Path)
		}
		sub, err := a.subject(op.User)
		if err != nil {
			return nil, err
		}
		role := meta.RoleNone
		if op.Op == "grant" {
			if role, ok = meta.ParseRole(op.Role); !ok || role == meta.RoleNone {
				return nil, fmt.Errorf("%w: role %q: want read, write or admin", meta.ErrBadCommand, op.Role)
			}
		}
		return map[string]string{"path": op.Path, "subject": op.User, "role": role.String()},
			a.propose(ctx, meta.Command{Op: meta.OpGrant, Actor: op.Actor, GrantOp: &meta.GrantOp{Ino: ino, Subject: sub, Role: role, Now: now.UnixNano()}})
	case "access_ls":
		return a.grantRows(op.Path)
	}
	return nil, fmt.Errorf("unknown admin op %q", op.Op)
}

// mintInvite proposes an invite and answers its code; the code is never kept.
func (a *adminOps) mintInvite(ctx context.Context, actor meta.UserIdx, bootstrap bool, inv meta.Invite, now time.Time) (any, error) {
	code, err := users.NewCode()
	if err != nil {
		return nil, err
	}
	inv.Hash = meta.HashCode(code)
	inv.ExpiresAt = now.Add(meta.InviteTTL).UnixNano()
	if err := a.propose(ctx, meta.Command{Op: meta.OpInvite, Actor: actor, InviteOp: &meta.InviteOp{Bootstrap: bootstrap, Invite: inv, Now: now.UnixNano()}}); err != nil {
		return nil, err
	}
	r := InviteReply{Code: code, Name: inv.Name, Admin: inv.Admin, Expires: inv.ExpiresAt}
	if inv.Kind == meta.InviteKey {
		if u, ok := a.node.FSM.User(inv.ForUser); ok {
			r.User = u.Name
		}
	}
	return r, nil
}

// subject reads a grant's subject: a user's name, or @group.
func (a *adminOps) subject(s string) (meta.Subject, error) {
	st := a.node.FSM
	if g, ok := strings.CutPrefix(s, "@"); ok {
		grp, found := st.GroupByName(g)
		if !found {
			return meta.Subject{}, fmt.Errorf("%w: no group %q", meta.ErrNotExist, g)
		}
		return meta.Subject{Kind: meta.SubjectGroup, Idx: uint16(grp.Idx)}, nil
	}
	u, ok := st.UserByName(s)
	if !ok {
		return meta.Subject{}, fmt.Errorf("%w: no user %q", meta.ErrNotExist, s)
	}
	return meta.Subject{Kind: meta.SubjectUser, Idx: uint16(u.Idx)}, nil
}

func (a *adminOps) subjectName(s meta.Subject) string {
	st := a.node.FSM
	if s.Kind == meta.SubjectGroup {
		for _, g := range st.Groups() {
			if g.Idx == meta.GroupIdx(s.Idx) {
				return "@" + g.Name
			}
		}
		return fmt.Sprintf("@group-%d", s.Idx)
	}
	if u, ok := st.User(meta.UserIdx(s.Idx)); ok {
		return u.Name
	}
	return fmt.Sprintf("user-%d", s.Idx)
}

func (a *adminOps) userRows(only string) []UserRow {
	st := a.node.FSM
	groups := st.Groups()
	var out []UserRow
	for _, u := range st.Users() {
		if only != "" && u.Name != only {
			continue
		}
		if u.State == meta.UserRemoved && only == "" {
			continue
		}
		row := UserRow{Idx: u.Idx, Name: u.Name, State: u.State.String(), Created: u.CreatedAt}
		if by, ok := st.User(u.By); ok {
			row.By = by.Name
		}
		for _, k := range u.Keys {
			kid := k.ID()
			row.Keys = append(row.Keys, KeyRow{ID: hex.EncodeToString(kid[:]), Label: k.Label, Added: k.AddedAt})
		}
		for _, g := range groups {
			for _, m := range g.Members {
				if m == u.Idx {
					row.Groups = append(row.Groups, g.Name)
				}
			}
		}
		out = append(out, row)
	}
	return out
}

func (a *adminOps) groupRows() []GroupRow {
	st := a.node.FSM
	groups := st.Groups()
	out := make([]GroupRow, 0, len(groups))
	for _, g := range groups {
		row := GroupRow{Idx: g.Idx, Name: g.Name}
		for _, m := range g.Members {
			if u, ok := st.User(m); ok {
				row.Members = append(row.Members, u.Name)
			}
		}
		out = append(out, row)
	}
	return out
}

// grantRows lists the grants under path (every grant when ""), by path then subject.
func (a *adminOps) grantRows(path string) ([]GrantRow, error) {
	st := a.node.FSM
	var under meta.Ino
	if path != "" {
		ino, ok := a.resolve(path)
		if !ok {
			return nil, fmt.Errorf("%w: %s", meta.ErrNotExist, path)
		}
		under = ino
	}
	out := []GrantRow{}
	for _, g := range st.Grants() {
		parts, err := st.PathOf(g.Ino)
		if err != nil {
			continue
		}
		p := "/" + strings.Join(parts, "/")
		if under != 0 && g.Ino != under && !isUnder(st, g.Ino, under) {
			continue
		}
		row := GrantRow{Path: p, Ino: uint64(g.Ino), Subject: a.subjectName(g.Subject), Role: g.Role.String(), At: g.At}
		if by, ok := st.User(g.By); ok {
			row.By = by.Name
		}
		out = append(out, row)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Path != out[j].Path {
			return out[i].Path < out[j].Path
		}
		return out[i].Subject < out[j].Subject
	})
	return out, nil
}

func isUnder(st *meta.FSM, ino, anc meta.Ino) bool {
	for cur := ino; ; {
		in, ok := st.GetInodeAny(cur)
		if !ok || cur == meta.RootIno || in.Parent == 0 {
			return false
		}
		if in.Parent == anc {
			return true
		}
		cur = in.Parent
	}
}

// proposeResult is propose with the command's result.
func (a *adminOps) proposeResult(ctx context.Context, cmd meta.Command) (meta.Result, error) {
	res, index, err := a.node.Propose(ctx, cmd)
	if err == nil {
		err = res.Err
	}
	if err != nil {
		return res, err
	}
	return res, a.node.FSM.WaitApplied(ctx, index)
}

// expireInvites is the leader's tick: every invite past its time is removed (I-343).
func (a *adminOps) expireInvites(ctx context.Context) {
	if !a.node.IsLeader() {
		return
	}
	now := a.clk.Now()
	for _, inv := range a.node.FSM.Invites() {
		if now.UnixNano() < inv.ExpiresAt {
			continue
		}
		if err := a.propose(ctx, meta.Command{Op: meta.OpInvite, InviteOp: &meta.InviteOp{Delete: true, Invite: meta.Invite{Hash: inv.Hash}, Now: now.UnixNano()}}); err != nil && !errors.Is(err, meta.ErrNotExist) {
			a.log.Warn("invite expiry failed", "err", err)
		}
	}
}

// secretAllowed is the owner rule the node applies before a secret op reaches the FSM
// (docs/21 §7, I-356): a personal name is its owner's, a system name an admin's; a get
// of a system name is for the two consumers an admin runs and is never printed.
func (a *adminOps) secretAllowed(op rpc.AdminOp) error {
	st := a.node.FSM
	if !st.Enforcing() {
		return nil
	}
	owner, personal := meta.SecretOwner(op.Name)
	if personal && owner == a.actorName(op) {
		return nil
	}
	if st.IsAdmin(op.Actor) && (!personal || op.Op == "secret_rm") {
		return nil
	}
	return meta.ErrAccess
}
