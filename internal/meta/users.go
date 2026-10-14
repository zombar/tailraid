package meta

import (
	"crypto/sha256"
	"errors"
	"regexp"
	"slices"
	"time"
)

// Users, groups and permissions (docs/21): the identity rows, the grants that map a user
// or a group to an inode with a role, the invites that create identities, the browser
// sessions, and the access rule every gateway and the FSM share. Every row here is
// replicated; a private key never is (I-342).

// Bounds (docs/21 §1, §6; I-342).
const (
	MaxUsers           = 1024
	MaxGroups          = 256
	MaxGrants          = 4096
	MaxInvites         = 256
	MaxKeysPerUser     = 16
	MaxSessionsPerUser = 16
	MaxUserName        = 32
	MaxKeyLabel        = 64
	// InviteTTL is the longest an invite may live from its minting (I-343).
	InviteTTL = 24 * time.Hour
	// MaxSessionLife bounds a session's life from its minting: the upper bound of
	// users.session (I-359).
	MaxSessionLife = 720 * time.Hour
	// InviteCodeLen is the length of a one-time code's base32 text (50 bits).
	InviteCodeLen = 10
)

// UserName is a user's or a group's name (docs/21 §1).
var UserName = regexp.MustCompile(`^[a-z][a-z0-9._-]{0,31}$`)

// Users and permissions errors (wire codes in errors.go).
var (
	// ErrAccess refuses an operation the actor's effective role does not cover (I-349).
	ErrAccess = errors.New("meta: permission denied")
	// ErrLockout refuses what would leave admins without an active member (I-346).
	ErrLockout = errors.New("meta: the last admin cannot be disabled, removed or dropped from admins")
	// ErrInvite refuses an invite code that is unknown, used or expired (I-343).
	ErrInvite = errors.New("meta: no such invite, or it has expired")
	// ErrUsersFull refuses a row past one of the bounds above (I-342).
	ErrUsersFull = errors.New("meta: a users table is full")
)

// UserIdx identifies a user; allocated from State.NextUser, never reused, never 0: 0 is
// nobody (an unbound credential) and, on a System command, the node itself.
type UserIdx uint16

// GroupIdx identifies a group; 1 and 2 are the built-in groups.
type GroupIdx uint16

// The built-in groups (I-346): admins holds every right; everyone is every active user
// and has no membership rows.
const (
	GroupAdmins   GroupIdx = 1
	GroupEveryone GroupIdx = 2
)

// UserState is where a user is in its life (docs/21 §1). Removed is terminal.
type UserState uint8

// User states.
const (
	UserActive   UserState = 1
	UserDisabled UserState = 2
	UserRemoved  UserState = 3
)

// String names a user state as the reports print it.
func (s UserState) String() string {
	switch s {
	case UserActive:
		return "active"
	case UserDisabled:
		return "disabled"
	case UserRemoved:
		return "removed"
	}
	return "active"
}

// UserKeyID is the short name of a user's public key: the first 8 bytes of its SHA-256.
type UserKeyID [8]byte

// UserKeyIDOf names a public key.
func UserKeyIDOf(pub [32]byte) UserKeyID {
	sum := sha256.Sum256(pub[:])
	var id UserKeyID
	copy(id[:], sum[:])
	return id
}

// UserKey is one device's key of a user (docs/21 §1).
type UserKey struct {
	Pub     [32]byte // Ed25519 public key
	Label   string   // the device's host name by default
	AddedAt int64
	By      UserIdx
}

// ID is the key's short name.
func (k UserKey) ID() UserKeyID { return UserKeyIDOf(k.Pub) }

// User is one person (docs/21 §1).
type User struct {
	Idx       UserIdx
	Name      string
	State     UserState
	Keys      []UserKey // immutable once stored: every change builds a new slice
	CreatedAt int64
	By        UserIdx // who created it; 0 for the bootstrap
}

// Group is a set of users (docs/21 §1): a role, in the RBAC sense.
type Group struct {
	Idx       GroupIdx
	Name      string
	Members   []UserIdx // sorted, never a duplicate; empty for everyone
	CreatedAt int64
	By        UserIdx
}

// Role is what a grant gives (docs/21 §3), ordered: a higher role includes the lower.
type Role uint8

// Roles.
const (
	RoleNone  Role = 0
	RoleRead  Role = 1
	RoleWrite Role = 2
	RoleAdmin Role = 3
)

var roleWords = [...]string{"none", "read", "write", "admin"}

func (r Role) String() string {
	if int(r) < len(roleWords) {
		return roleWords[r]
	}
	return "none"
}

// ParseRole reads one of read, write, admin (none clears a grant).
func ParseRole(s string) (Role, bool) {
	for i, w := range roleWords {
		if s == w {
			return Role(i), true
		}
	}
	return RoleNone, false
}

// SubjectKind says what a grant's subject is.
type SubjectKind uint8

// Subject kinds.
const (
	SubjectUser  SubjectKind = 1
	SubjectGroup SubjectKind = 2
)

// Subject is who a grant is for: a user or a group.
type Subject struct {
	Kind SubjectKind
	Idx  uint16 // a UserIdx or a GroupIdx
}

// GrantKey keys the grant table: one grant per subject per inode.
type GrantKey struct {
	Ino     Ino
	Subject Subject
}

// Grant gives Subject the Role on Ino and everything under it (docs/21 §3).
type Grant struct {
	Ino     Ino
	Subject Subject
	Role    Role
	At      int64
	By      UserIdx
}

// InviteKind says what a one-time code creates.
type InviteKind uint8

// Invite kinds.
const (
	InviteUser InviteKind = 1 // a new user named Name
	InviteKey  InviteKind = 2 // a key on ForUser
)

// Invite is the hash of a one-time code and what presenting it does (docs/21 §1).
type Invite struct {
	Hash      [32]byte // SHA-256 of the code's text
	Name      string   // InviteUser: the user's name
	Kind      InviteKind
	ForUser   UserIdx // InviteKey: the user the key joins
	Admin     bool    // InviteUser: the new user joins admins
	ExpiresAt int64
	By        UserIdx // 0: a bootstrap invite (init, or root on a voter)
}

// Session is a browser's sign-in to the volume (docs/21 §6): the hash of its token, the
// user, the key that minted it and when it ends.
type Session struct {
	Hash      [32]byte
	User      UserIdx
	Key       UserKeyID
	At        int64
	ExpiresAt int64
}

// HashCode is how a code or a token is stored: never the text itself.
func HashCode(text string) [32]byte { return sha256.Sum256([]byte(text)) }

// --- ops (28–32, codec 14) ----------------------------------------------------------

// InviteOp mints an invite, or removes one (op 28). A mint by actor 0 needs Bootstrap
// (init, or root on a voter) and is then always an admin invite; a removal by actor 0 is
// the leader's expiry and needs the invite to be past its time.
type InviteOp struct {
	Delete    bool
	Bootstrap bool
	Invite    Invite
	Now       int64
}

// UserAction is what a UserOp does.
type UserAction uint8

// User actions.
const (
	UserCreate    UserAction = 1 // Name, Code, Key, Label: consumes an InviteUser
	UserKeyAdd    UserAction = 2 // Code, Key, Label: consumes an InviteKey
	UserKeyRemove UserAction = 3 // Target, UserKeyID: the user's own, or an admin
	UserDisable   UserAction = 4 // Target: admins
	UserEnable    UserAction = 5 // Target: admins
	UserRemove    UserAction = 6 // Target: admins; the row stays as Removed
)

// UserOp changes a user (op 29).
type UserOp struct {
	Action UserAction
	Name   string
	Code   string // the invite's text; hashed and matched in apply, never stored
	Key    [32]byte
	Label  string
	KeyID  UserKeyID
	Target UserIdx
	Now    int64
}

// GroupAction is what a GroupOp does.
type GroupAction uint8

// Group actions.
const (
	GroupCreate       GroupAction = 1 // Name
	GroupRemove       GroupAction = 2 // Group: never a built-in
	GroupAddMember    GroupAction = 3 // Group, User
	GroupRemoveMember GroupAction = 4 // Group, User
)

// GroupOp changes a group (op 30); every action needs admins.
type GroupOp struct {
	Action GroupAction
	Name   string
	Group  GroupIdx
	User   UserIdx
	Now    int64
}

// GrantOp sets a grant, or with RoleNone clears it (op 31); needs Admin on Ino.
type GrantOp struct {
	Ino     Ino
	Subject Subject
	Role    Role
	Now     int64
}

// SessionOp adds a session for the actor, or removes one (op 32): the actor's own by
// Hash, or with All every session of User (the actor's own, or any as an admin).
type SessionOp struct {
	Delete    bool
	All       bool
	Hash      [32]byte
	User      UserIdx
	Key       UserKeyID
	ExpiresAt int64
	Now       int64
}

// --- the access rule --------------------------------------------------------------

// Enforcing reports whether the permission checks run: once the first user exists
// (docs/21 §4, I-349). Before that a cluster behaves as it did without users.
func (s *State) Enforcing() bool { return len(s.Users) > 0 }

// isAdmin reports whether user is an active member of admins.
func (s *State) isAdmin(user UserIdx) bool {
	u := s.Users[user]
	if u == nil || u.State != UserActive {
		return false
	}
	g := s.Groups[GroupAdmins]
	if g == nil {
		return false
	}
	_, ok := slices.BinarySearch(g.Members, user)
	return ok
}

// subjects lists the grant subjects that reach user: the user, every group holding it,
// and everyone. nil for a user that is not active.
func (s *State) subjects(user UserIdx) []Subject {
	u := s.Users[user]
	if u == nil || u.State != UserActive {
		return nil
	}
	out := []Subject{{Kind: SubjectUser, Idx: uint16(user)}, {Kind: SubjectGroup, Idx: uint16(GroupEveryone)}}
	for idx, g := range s.Groups {
		if _, ok := slices.BinarySearch(g.Members, user); ok {
			out = append(out, Subject{Kind: SubjectGroup, Idx: uint16(idx)})
		}
	}
	return out
}

// Access is the effective role of user at ino (docs/21 §3, I-347): the highest role
// among the grants on ino and its ancestors whose subject is the user, one of its groups
// or everyone; Admin for a member of admins; None for nobody, a disabled or removed user,
// and anything under the reserved tree. A pure function of the state: no clock, no
// liveness, so every node answers the same at the same index.
func Access(s *State, user UserIdx, ino Ino) Role {
	if user == 0 {
		return RoleNone
	}
	if s.lineage(ino).reserved {
		return RoleNone
	}
	if s.isAdmin(user) {
		return RoleAdmin
	}
	subs := s.subjects(user)
	if subs == nil {
		return RoleNone
	}
	best := RoleNone
	for cur, steps := ino, 0; steps <= len(s.Inodes); steps++ {
		for _, sub := range subs {
			if g := s.Grants[GrantKey{Ino: cur, Subject: sub}]; g != nil && g.Role > best {
				best = g.Role
				if best == RoleAdmin {
					return best
				}
			}
		}
		in, ok := s.Inodes[cur]
		if !ok || cur == RootIno || in.Parent == 0 {
			return best
		}
		cur = in.Parent
	}
	return best
}

// Traverses reports whether user may look up and stat ino as the ancestor of something
// they hold a role on (docs/21 §3): a role on ino itself, or a grant for one of their
// subjects anywhere under it.
func Traverses(s *State, user UserIdx, ino Ino) bool {
	if Access(s, user, ino) != RoleNone {
		return true
	}
	subs := s.subjects(user)
	if subs == nil {
		return false
	}
	for key := range s.Grants {
		if !slices.Contains(subs, key.Subject) {
			continue
		}
		for cur, steps := key.Ino, 0; steps <= len(s.Inodes); steps++ {
			if cur == ino {
				return true
			}
			in, ok := s.Inodes[cur]
			if !ok || cur == RootIno || in.Parent == 0 {
				break
			}
			cur = in.Parent
		}
	}
	return false
}

// authorize is the FSM's check of a user-facing command (docs/21 §4, I-349): nothing
// while no user exists; a System command only on the reserved tree or for a Purge; else
// the actor's effective role on every opTarget must cover the role the command needs.
func (a *applier) authorize(c Command) error {
	st := a.st
	if !st.Enforcing() {
		return nil
	}
	targets, need, user := a.targets(c)
	if !user {
		return nil
	}
	if c.System {
		if c.Op == OpPurge {
			return nil
		}
		for _, t := range targets {
			if !st.lineage(t.ino).reserved && (t.name != ReservedDir || t.ino != RootIno) {
				return ErrAccess
			}
		}
		return nil
	}
	for _, t := range targets {
		if Access(st, c.Actor, t.ino) < need {
			return ErrAccess
		}
	}
	return nil
}

// opTarget is one inode a command acts on; name is the entry it creates there, if any.
type opTarget struct {
	ino  Ino
	name string
}

// targets lists what c touches and the role that needs; user is false for a command the
// check does not cover (the node's own placement, GC, membership and identity ops, which
// check their actors themselves).
func (a *applier) targets(c Command) (ts []opTarget, need Role, user bool) {
	switch c.Op { //nolint:exhaustive // every other op is not user-facing
	case OpCreate:
		if c.Create != nil {
			return []opTarget{{c.Create.Parent, c.Create.Name}}, RoleWrite, true
		}
	case OpMkdir:
		if c.Mkdir != nil {
			return []opTarget{{c.Mkdir.Parent, c.Mkdir.Name}}, RoleWrite, true
		}
	case OpUnlink:
		if c.Unlink != nil {
			return []opTarget{{c.Unlink.Parent, ""}}, RoleWrite, true
		}
	case OpRmdir:
		if c.Rmdir != nil {
			return []opTarget{{c.Rmdir.Parent, ""}}, RoleWrite, true
		}
	case OpTrash:
		if c.Trash != nil {
			return []opTarget{{c.Trash.Parent, ""}}, RoleWrite, true
		}
	case OpRename:
		if c.Rename != nil {
			return []opTarget{{c.Rename.OldParent, ""}, {c.Rename.NewParent, c.Rename.NewName}}, RoleWrite, true
		}
	case OpSetAttr:
		if c.SetAttr != nil {
			return []opTarget{{c.SetAttr.Ino, ""}}, RoleWrite, true
		}
	case OpRestore:
		if c.Restore != nil {
			parent := c.Restore.NewParent
			if parent == 0 {
				if in, ok := a.st.Inodes[c.Restore.Ino]; ok {
					parent = in.Parent
				}
			}
			return []opTarget{{parent, ""}}, RoleWrite, true
		}
	case OpPurge:
		if c.Purge != nil {
			if c.Purge.Ino == 0 {
				return nil, RoleAdmin, true // the sweep: System only (nobody holds Admin on nothing)
			}
			parent := Ino(0)
			if in, ok := a.st.Inodes[c.Purge.Ino]; ok {
				parent = in.Parent
			}
			return []opTarget{{parent, ""}}, RoleAdmin, true
		}
	case OpCommit:
		if c.Commit != nil && c.Commit.RewriteStart {
			return []opTarget{{c.Commit.Ino, ""}}, RoleWrite, true
		}
	}
	return nil, RoleNone, false
}

// Access, under the FSM's read lock, for the gateways.
func (f *FSM) Access(user UserIdx, ino Ino) Role {
	f.mu.RLock()
	defer f.mu.RUnlock()
	return Access(f.st, user, ino)
}

// Traverses, under the FSM's read lock.
func (f *FSM) Traverses(user UserIdx, ino Ino) bool {
	f.mu.RLock()
	defer f.mu.RUnlock()
	return Traverses(f.st, user, ino)
}

// Enforcing, under the FSM's read lock.
func (f *FSM) Enforcing() bool {
	f.mu.RLock()
	defer f.mu.RUnlock()
	return f.st.Enforcing()
}

// --- apply ------------------------------------------------------------------------

// dropGrants forgets every grant on an inode that leaves the table for good.
func (s *State) dropGrants(ino Ino) {
	for key := range s.Grants {
		if key.Ino == ino {
			delete(s.Grants, key)
		}
	}
}

// ensureGroups creates the built-in groups with the first user (I-346).
func (s *State) ensureGroups(now int64) {
	if s.Groups[GroupAdmins] == nil {
		s.Groups[GroupAdmins] = &Group{Idx: GroupAdmins, Name: "admins", CreatedAt: now}
	}
	if s.Groups[GroupEveryone] == nil {
		s.Groups[GroupEveryone] = &Group{Idx: GroupEveryone, Name: "everyone", CreatedAt: now}
	}
	if s.NextGroup <= GroupEveryone {
		s.NextGroup = GroupEveryone + 1
	}
}

// activeAdmins counts the active members of admins.
func (s *State) activeAdmins() int {
	g := s.Groups[GroupAdmins]
	if g == nil {
		return 0
	}
	n := 0
	for _, m := range g.Members {
		if u := s.Users[m]; u != nil && u.State == UserActive {
			n++
		}
	}
	return n
}

// lockout reports whether taking user out of admins (by state or by membership) would
// leave the group without an active member (I-346).
func (s *State) lockout(user UserIdx) bool {
	return s.isAdmin(user) && s.activeAdmins() <= 1
}

func (s *State) userByName(name string) *User {
	for _, u := range s.Users {
		if u.Name == name && u.State != UserRemoved {
			return u
		}
	}
	return nil
}

func (s *State) groupByName(name string) *Group {
	for _, g := range s.Groups {
		if g.Name == name {
			return g
		}
	}
	return nil
}

// consumeInvite matches a code against the open invites and removes the one it names;
// nil with ErrInvite when none, or the one found has expired.
func (a *applier) consumeInvite(code string, now int64, kind InviteKind) (*Invite, error) {
	if len(code) != InviteCodeLen {
		return nil, ErrInvite
	}
	inv := a.st.Invites[HashCode(code)]
	if inv == nil || inv.Kind != kind || now >= inv.ExpiresAt {
		return nil, ErrInvite
	}
	cp := *inv
	delete(a.st.Invites, inv.Hash)
	return &cp, nil
}

func (a *applier) inviteOp(c Command) error {
	p := c.InviteOp
	if p == nil {
		return ErrBadCommand
	}
	st := a.st
	now := a.clock(p.Now)
	inv := p.Invite
	if p.Delete {
		cur := st.Invites[inv.Hash]
		if cur == nil {
			return ErrNotExist
		}
		if c.Actor == 0 {
			if now < cur.ExpiresAt {
				return ErrAccess // the leader's expiry removes only what has expired
			}
		} else if !st.isAdmin(c.Actor) {
			return ErrAccess
		}
		delete(st.Invites, inv.Hash)
		return nil
	}
	switch {
	case inv.Hash == [32]byte{}, inv.ExpiresAt <= now, inv.ExpiresAt > now+int64(InviteTTL):
		return ErrBadCommand
	case inv.Kind == InviteUser && !UserName.MatchString(inv.Name):
		return ErrBadCommand
	case inv.Kind == InviteKey && (inv.Name != "" || inv.Admin):
		return ErrBadCommand
	case inv.Kind != InviteUser && inv.Kind != InviteKey:
		return ErrBadCommand
	}
	if c.Actor == 0 {
		if !p.Bootstrap || inv.Kind != InviteUser {
			return ErrAccess
		}
		inv.Admin = true // the bootstrap invite always makes an admin
	} else {
		if inv.Kind == InviteKey && inv.ForUser == c.Actor {
			// a user invites a key of their own
		} else if !st.isAdmin(c.Actor) {
			return ErrAccess
		}
	}
	if inv.Kind == InviteUser && st.userByName(inv.Name) != nil {
		return ErrExist
	}
	if inv.Kind == InviteKey {
		u := st.Users[inv.ForUser]
		if u == nil || u.State == UserRemoved {
			return ErrNotExist
		}
	}
	if _, ok := st.Invites[inv.Hash]; !ok && len(st.Invites) >= MaxInvites {
		return ErrUsersFull
	}
	inv.By = c.Actor
	st.Invites[inv.Hash] = &inv
	return nil
}

func (a *applier) userOp(c Command) (Result, error) {
	p := c.UserOp
	if p == nil {
		return Result{}, ErrBadCommand
	}
	st := a.st
	now := a.clock(p.Now)
	switch p.Action {
	case UserCreate:
		if !UserName.MatchString(p.Name) || p.Key == [32]byte{} || len(p.Label) > MaxKeyLabel {
			return Result{}, ErrBadCommand
		}
		inv := st.Invites[HashCode(p.Code)]
		if len(p.Code) != InviteCodeLen || inv == nil || inv.Kind != InviteUser || inv.Name != p.Name || now >= inv.ExpiresAt {
			return Result{}, ErrInvite
		}
		if st.userByName(p.Name) != nil {
			return Result{}, ErrExist
		}
		if len(st.Users) >= MaxUsers {
			return Result{}, ErrUsersFull
		}
		delete(st.Invites, inv.Hash)
		st.ensureGroups(now)
		if st.NextUser == 0 {
			st.NextUser = 1
		}
		u := &User{
			Idx: st.NextUser, Name: p.Name, State: UserActive, CreatedAt: now, By: inv.By,
			Keys: []UserKey{{Pub: p.Key, Label: p.Label, AddedAt: now, By: inv.By}},
		}
		st.NextUser++
		st.Users[u.Idx] = u
		if inv.Admin {
			g := st.Groups[GroupAdmins]
			g.Members = insertSorted(g.Members, u.Idx)
		}
		return Result{Ino: Ino(u.Idx)}, nil
	case UserKeyAdd:
		if p.Key == [32]byte{} || len(p.Label) > MaxKeyLabel {
			return Result{}, ErrBadCommand
		}
		inv, err := a.consumeInvite(p.Code, now, InviteKey)
		if err != nil {
			return Result{}, err
		}
		u := st.Users[inv.ForUser]
		if u == nil || u.State == UserRemoved {
			return Result{}, ErrNotExist
		}
		if len(u.Keys) >= MaxKeysPerUser {
			return Result{}, ErrUsersFull
		}
		for _, k := range u.Keys {
			if k.Pub == p.Key {
				return Result{}, ErrExist
			}
		}
		u.Keys = append(slices.Clone(u.Keys), UserKey{Pub: p.Key, Label: p.Label, AddedAt: now, By: inv.By})
		return Result{Ino: Ino(u.Idx)}, nil
	case UserKeyRemove:
		u := st.Users[p.Target]
		if u == nil || u.State == UserRemoved {
			return Result{}, ErrNotExist
		}
		if c.Actor != p.Target && !st.isAdmin(c.Actor) {
			return Result{}, ErrAccess
		}
		i := slices.IndexFunc(u.Keys, func(k UserKey) bool { return k.ID() == p.KeyID })
		if i < 0 {
			return Result{}, ErrNotExist
		}
		u.Keys = slices.Delete(slices.Clone(u.Keys), i, i+1)
		st.dropSessionsOfKey(p.Target, p.KeyID)
		return Result{Ino: Ino(u.Idx)}, nil
	case UserDisable, UserEnable, UserRemove:
		if !st.isAdmin(c.Actor) {
			return Result{}, ErrAccess
		}
		u := st.Users[p.Target]
		if u == nil || u.State == UserRemoved {
			return Result{}, ErrNotExist
		}
		if p.Action != UserEnable && st.lockout(p.Target) {
			return Result{}, ErrLockout
		}
		switch p.Action {
		case UserDisable:
			u.State = UserDisabled
			st.dropSessionsOf(p.Target)
		case UserEnable:
			u.State = UserActive
		case UserRemove:
			u.State = UserRemoved
			u.Keys = nil
			st.dropSessionsOf(p.Target)
			for _, g := range st.Groups {
				g.Members = removeSorted(g.Members, p.Target)
			}
			for key := range st.Grants {
				if key.Subject == (Subject{Kind: SubjectUser, Idx: uint16(p.Target)}) {
					delete(st.Grants, key)
				}
			}
		default:
			return Result{}, ErrBadCommand
		}
		return Result{Ino: Ino(u.Idx)}, nil
	}
	return Result{}, ErrBadCommand
}

func (a *applier) groupOp(c Command) (Result, error) {
	p := c.GroupOp
	if p == nil {
		return Result{}, ErrBadCommand
	}
	st := a.st
	if !st.isAdmin(c.Actor) {
		return Result{}, ErrAccess
	}
	now := a.clock(p.Now)
	switch p.Action {
	case GroupCreate:
		if !UserName.MatchString(p.Name) {
			return Result{}, ErrBadCommand
		}
		if st.groupByName(p.Name) != nil {
			return Result{}, ErrExist
		}
		if len(st.Groups) >= MaxGroups {
			return Result{}, ErrUsersFull
		}
		g := &Group{Idx: st.NextGroup, Name: p.Name, CreatedAt: now, By: c.Actor}
		st.NextGroup++
		st.Groups[g.Idx] = g
		return Result{Ino: Ino(g.Idx)}, nil
	case GroupRemove:
		if p.Group == GroupAdmins || p.Group == GroupEveryone {
			return Result{}, ErrBadCommand
		}
		if st.Groups[p.Group] == nil {
			return Result{}, ErrNotExist
		}
		delete(st.Groups, p.Group)
		for key := range st.Grants {
			if key.Subject == (Subject{Kind: SubjectGroup, Idx: uint16(p.Group)}) {
				delete(st.Grants, key)
			}
		}
		return Result{}, nil
	case GroupAddMember, GroupRemoveMember:
		if p.Group == GroupEveryone {
			return Result{}, ErrBadCommand // everyone has no rows
		}
		g := st.Groups[p.Group]
		u := st.Users[p.User]
		if g == nil || u == nil || u.State == UserRemoved {
			return Result{}, ErrNotExist
		}
		if p.Action == GroupAddMember {
			g.Members = insertSorted(g.Members, p.User)
			return Result{}, nil
		}
		if p.Group == GroupAdmins && st.lockout(p.User) {
			return Result{}, ErrLockout
		}
		g.Members = removeSorted(g.Members, p.User)
		return Result{}, nil
	}
	return Result{}, ErrBadCommand
}

func (a *applier) grantOp(c Command) error {
	p := c.GrantOp
	if p == nil {
		return ErrBadCommand
	}
	st := a.st
	if p.Role > RoleAdmin {
		return ErrBadCommand
	}
	if _, ok := st.Inodes[p.Ino]; !ok {
		return ErrNotExist
	}
	if st.lineage(p.Ino).reserved {
		return ErrBadCommand // outside every grant walk (I-348)
	}
	switch p.Subject.Kind {
	case SubjectUser:
		if u := st.Users[UserIdx(p.Subject.Idx)]; u == nil || u.State == UserRemoved {
			return ErrNotExist
		}
	case SubjectGroup:
		if st.Groups[GroupIdx(p.Subject.Idx)] == nil {
			return ErrNotExist
		}
	default:
		return ErrBadCommand
	}
	if Access(st, c.Actor, p.Ino) < RoleAdmin {
		return ErrAccess
	}
	key := GrantKey{Ino: p.Ino, Subject: p.Subject}
	if p.Role == RoleNone {
		if _, ok := st.Grants[key]; !ok {
			return ErrNotExist
		}
		delete(st.Grants, key)
		return nil
	}
	if _, ok := st.Grants[key]; !ok && len(st.Grants) >= MaxGrants {
		return ErrUsersFull
	}
	st.Grants[key] = &Grant{Ino: p.Ino, Subject: p.Subject, Role: p.Role, At: a.clock(p.Now), By: c.Actor}
	return nil
}

func (a *applier) sessionOp(c Command) error {
	p := c.SessionOp
	if p == nil {
		return ErrBadCommand
	}
	st := a.st
	now := a.clock(p.Now)
	if p.Delete {
		if p.All {
			if p.User == 0 || (c.Actor != p.User && !st.isAdmin(c.Actor)) {
				return ErrAccess
			}
			st.dropSessionsOf(p.User)
			return nil
		}
		s := st.Sessions[p.Hash]
		if s == nil {
			return ErrNotExist
		}
		if c.Actor != s.User && !st.isAdmin(c.Actor) {
			return ErrAccess
		}
		delete(st.Sessions, p.Hash)
		return nil
	}
	if p.Hash == [32]byte{} || p.ExpiresAt <= now || p.ExpiresAt > now+int64(MaxSessionLife) {
		return ErrBadCommand
	}
	u := st.Users[c.Actor]
	if c.Actor == 0 || u == nil || u.State != UserActive || p.User != c.Actor {
		return ErrAccess
	}
	if !slices.ContainsFunc(u.Keys, func(k UserKey) bool { return k.ID() == p.Key }) {
		return ErrAccess
	}
	if _, ok := st.Sessions[p.Hash]; ok {
		return ErrExist
	}
	// at most MaxSessionsPerUser: the oldest goes
	var mine []*Session
	for _, s := range st.Sessions {
		if s.User == c.Actor {
			mine = append(mine, s)
		}
	}
	if len(mine) >= MaxSessionsPerUser {
		slices.SortFunc(mine, func(x, y *Session) int {
			if x.At != y.At {
				return int(x.At - y.At)
			}
			return slices.Compare(x.Hash[:], y.Hash[:])
		})
		for _, s := range mine[:len(mine)-MaxSessionsPerUser+1] {
			delete(st.Sessions, s.Hash)
		}
	}
	st.Sessions[p.Hash] = &Session{Hash: p.Hash, User: c.Actor, Key: p.Key, At: now, ExpiresAt: p.ExpiresAt}
	return nil
}

func (s *State) dropSessionsOf(user UserIdx) {
	for h, se := range s.Sessions {
		if se.User == user {
			delete(s.Sessions, h)
		}
	}
}

func (s *State) dropSessionsOfKey(user UserIdx, key UserKeyID) {
	for h, se := range s.Sessions {
		if se.User == user && se.Key == key {
			delete(s.Sessions, h)
		}
	}
}

func insertSorted(list []UserIdx, u UserIdx) []UserIdx {
	i, found := slices.BinarySearch(list, u)
	if found {
		return list
	}
	return slices.Insert(slices.Clone(list), i, u)
}

func removeSorted(list []UserIdx, u UserIdx) []UserIdx {
	i, found := slices.BinarySearch(list, u)
	if !found {
		return list
	}
	return slices.Delete(slices.Clone(list), i, i+1)
}

// --- read API ---------------------------------------------------------------------

// User is a copy of one user row.
func (f *FSM) User(idx UserIdx) (User, bool) {
	f.mu.RLock()
	defer f.mu.RUnlock()
	return copyUser(f.st.Users[idx])
}

// UserByName is the live (not removed) user of that name.
func (f *FSM) UserByName(name string) (User, bool) {
	f.mu.RLock()
	defer f.mu.RUnlock()
	return copyUser(f.st.userByName(name))
}

// copyUser is a row's copy with its own key slice; nil is not a row.
func copyUser(u *User) (User, bool) {
	if u == nil {
		return User{}, false
	}
	cp := *u
	cp.Keys = slices.Clone(u.Keys)
	return cp, true
}

// Users lists every user row, removed ones included, by index.
func (f *FSM) Users() []User {
	f.mu.RLock()
	defer f.mu.RUnlock()
	out := make([]User, 0, len(f.st.Users))
	for _, u := range f.st.Users {
		cp := *u
		cp.Keys = slices.Clone(u.Keys)
		out = append(out, cp)
	}
	slices.SortFunc(out, func(a, b User) int { return int(a.Idx) - int(b.Idx) })
	return out
}

// Groups lists every group by index, members included.
func (f *FSM) Groups() []Group {
	f.mu.RLock()
	defer f.mu.RUnlock()
	idxs := make([]GroupIdx, 0, len(f.st.Groups))
	for idx := range f.st.Groups {
		idxs = append(idxs, idx)
	}
	slices.Sort(idxs)
	out := make([]Group, 0, len(idxs))
	for _, idx := range idxs {
		g := *f.st.Groups[idx]
		g.Members = slices.Clone(g.Members)
		out = append(out, g)
	}
	return out
}

// GroupByName is the group of that name.
func (f *FSM) GroupByName(name string) (Group, bool) {
	f.mu.RLock()
	defer f.mu.RUnlock()
	g := f.st.groupByName(name)
	if g == nil {
		return Group{}, false
	}
	cp := *g
	cp.Members = slices.Clone(g.Members)
	return cp, true
}

// Grants lists every grant, by inode then subject (the access review, I-358).
func (f *FSM) Grants() []Grant {
	f.mu.RLock()
	defer f.mu.RUnlock()
	out := make([]Grant, 0, len(f.st.Grants))
	for _, g := range f.st.Grants {
		out = append(out, *g)
	}
	slices.SortFunc(out, compareGrants)
	return out
}

func compareGrants(a, b Grant) int {
	switch {
	case a.Ino != b.Ino:
		return int(a.Ino - b.Ino)
	case a.Subject.Kind != b.Subject.Kind:
		return int(a.Subject.Kind) - int(b.Subject.Kind)
	}
	return int(a.Subject.Idx) - int(b.Subject.Idx)
}

// Invite is the open invite of a code's hash, if any.
func (f *FSM) Invite(hash [32]byte) (Invite, bool) {
	f.mu.RLock()
	defer f.mu.RUnlock()
	inv := f.st.Invites[hash]
	if inv == nil {
		return Invite{}, false
	}
	return *inv, true
}

// Invites lists the open invites in hash order (the leader's tick picks the expired).
func (f *FSM) Invites() []Invite {
	f.mu.RLock()
	defer f.mu.RUnlock()
	out := make([]Invite, 0, len(f.st.Invites))
	for _, inv := range f.st.Invites {
		out = append(out, *inv)
	}
	slices.SortFunc(out, func(a, b Invite) int { return slices.Compare(a.Hash[:], b.Hash[:]) })
	return out
}

// Session is the session of a token's hash, if any.
func (f *FSM) Session(hash [32]byte) (Session, bool) {
	f.mu.RLock()
	defer f.mu.RUnlock()
	s := f.st.Sessions[hash]
	if s == nil {
		return Session{}, false
	}
	return *s, true
}

// Sessions lists every session by user then time.
func (f *FSM) Sessions() []Session {
	f.mu.RLock()
	defer f.mu.RUnlock()
	out := make([]Session, 0, len(f.st.Sessions))
	for _, s := range f.st.Sessions {
		out = append(out, *s)
	}
	slices.SortFunc(out, func(a, b Session) int {
		switch {
		case a.User != b.User:
			return int(a.User) - int(b.User)
		case a.At != b.At:
			return int(a.At - b.At)
		}
		return slices.Compare(a.Hash[:], b.Hash[:])
	})
	return out
}

// --- wire -------------------------------------------------------------------------

func writeUser(w *writer, u *User) {
	w.u16(uint16(u.Idx))
	w.str(u.Name)
	w.u8(uint8(u.State))
	w.i64(u.CreatedAt)
	w.u16(uint16(u.By))
	w.uvarint(uint64(len(u.Keys)))
	for _, k := range u.Keys {
		w.b = append(w.b, k.Pub[:]...)
		w.str(k.Label)
		w.i64(k.AddedAt)
		w.u16(uint16(k.By))
	}
}

func readUser(r *reader) User {
	u := User{Idx: UserIdx(r.u16()), Name: r.str(), State: UserState(r.u8()), CreatedAt: r.i64(), By: UserIdx(r.u16())}
	if n := r.length(MaxKeysPerUser); n > 0 {
		u.Keys = make([]UserKey, n)
		for i := range u.Keys {
			k := &u.Keys[i]
			copy(k.Pub[:], r.take(len(k.Pub)))
			k.Label = r.str()
			k.AddedAt = r.i64()
			k.By = UserIdx(r.u16())
		}
	}
	return u
}

func writeGroup(w *writer, g *Group) {
	w.u16(uint16(g.Idx))
	w.str(g.Name)
	w.i64(g.CreatedAt)
	w.u16(uint16(g.By))
	w.uvarint(uint64(len(g.Members)))
	for _, m := range g.Members {
		w.u16(uint16(m))
	}
}

func readGroup(r *reader) Group {
	g := Group{Idx: GroupIdx(r.u16()), Name: r.str(), CreatedAt: r.i64(), By: UserIdx(r.u16())}
	if n := r.length(MaxUsers); n > 0 {
		g.Members = make([]UserIdx, n)
		for i := range g.Members {
			g.Members[i] = UserIdx(r.u16())
		}
	}
	return g
}

func writeSubject(w *writer, s Subject) {
	w.u8(uint8(s.Kind))
	w.u16(s.Idx)
}

func readSubject(r *reader) Subject { return Subject{Kind: SubjectKind(r.u8()), Idx: r.u16()} }

func writeGrant(w *writer, g *Grant) {
	w.u64(uint64(g.Ino))
	writeSubject(w, g.Subject)
	w.u8(uint8(g.Role))
	w.i64(g.At)
	w.u16(uint16(g.By))
}

func readGrant(r *reader) Grant {
	return Grant{Ino: Ino(r.u64()), Subject: readSubject(r), Role: Role(r.u8()), At: r.i64(), By: UserIdx(r.u16())}
}

func writeInvite(w *writer, inv *Invite) {
	w.b = append(w.b, inv.Hash[:]...)
	w.str(inv.Name)
	w.u8(uint8(inv.Kind))
	w.u16(uint16(inv.ForUser))
	w.bool(inv.Admin)
	w.i64(inv.ExpiresAt)
	w.u16(uint16(inv.By))
}

func readInvite(r *reader) Invite {
	var inv Invite
	copy(inv.Hash[:], r.take(len(inv.Hash)))
	inv.Name = r.str()
	inv.Kind = InviteKind(r.u8())
	inv.ForUser = UserIdx(r.u16())
	inv.Admin = r.bool()
	inv.ExpiresAt = r.i64()
	inv.By = UserIdx(r.u16())
	return inv
}

func writeSession(w *writer, s *Session) {
	w.b = append(w.b, s.Hash[:]...)
	w.u16(uint16(s.User))
	w.b = append(w.b, s.Key[:]...)
	w.i64(s.At)
	w.i64(s.ExpiresAt)
}

func readSession(r *reader) Session {
	var s Session
	copy(s.Hash[:], r.take(len(s.Hash)))
	s.User = UserIdx(r.u16())
	copy(s.Key[:], r.take(len(s.Key)))
	s.At = r.i64()
	s.ExpiresAt = r.i64()
	return s
}

// writeUsers writes the users, groups, grants, invites and sessions in canonical order
// (snapshot frame 14).
func writeUsers(w *writer, st *State) {
	w.u16(uint16(st.NextUser))
	w.u16(uint16(st.NextGroup))
	users := make([]UserIdx, 0, len(st.Users))
	for idx := range st.Users {
		users = append(users, idx)
	}
	slices.Sort(users)
	w.uvarint(uint64(len(users)))
	for _, idx := range users {
		writeUser(w, st.Users[idx])
	}
	groups := make([]GroupIdx, 0, len(st.Groups))
	for idx := range st.Groups {
		groups = append(groups, idx)
	}
	slices.Sort(groups)
	w.uvarint(uint64(len(groups)))
	for _, idx := range groups {
		writeGroup(w, st.Groups[idx])
	}
	grants := make([]Grant, 0, len(st.Grants))
	for _, g := range st.Grants {
		grants = append(grants, *g)
	}
	slices.SortFunc(grants, compareGrants)
	w.uvarint(uint64(len(grants)))
	for i := range grants {
		writeGrant(w, &grants[i])
	}
	invites := make([][32]byte, 0, len(st.Invites))
	for h := range st.Invites {
		invites = append(invites, h)
	}
	slices.SortFunc(invites, func(a, b [32]byte) int { return slices.Compare(a[:], b[:]) })
	w.uvarint(uint64(len(invites)))
	for _, h := range invites {
		writeInvite(w, st.Invites[h])
	}
	sessions := make([][32]byte, 0, len(st.Sessions))
	for h := range st.Sessions {
		sessions = append(sessions, h)
	}
	slices.SortFunc(sessions, func(a, b [32]byte) int { return slices.Compare(a[:], b[:]) })
	w.uvarint(uint64(len(sessions)))
	for _, h := range sessions {
		writeSession(w, st.Sessions[h])
	}
}

func readUsers(r *reader, st *State) {
	st.NextUser = UserIdx(r.u16())
	st.NextGroup = GroupIdx(r.u16())
	n := r.length(MaxUsers)
	var prevU UserIdx
	for i := 0; i < n && r.err == nil; i++ {
		u := readUser(r)
		if r.err == nil && (u.Idx == 0 || u.Idx <= prevU || !UserName.MatchString(u.Name) || u.State < UserActive || u.State > UserRemoved) {
			r.fail(errors.New("meta: users not in canonical order"))
			return
		}
		prevU = u.Idx
		st.Users[u.Idx] = &u
	}
	n = r.length(MaxGroups)
	var prevG GroupIdx
	for i := 0; i < n && r.err == nil; i++ {
		g := readGroup(r)
		if r.err == nil && (g.Idx == 0 || g.Idx <= prevG || !UserName.MatchString(g.Name) || !slices.IsSorted(g.Members)) {
			r.fail(errors.New("meta: groups not in canonical order"))
			return
		}
		prevG = g.Idx
		st.Groups[g.Idx] = &g
	}
	n = r.length(MaxGrants)
	var prev Grant
	for i := 0; i < n && r.err == nil; i++ {
		g := readGrant(r)
		if r.err == nil && ((i > 0 && compareGrants(g, prev) <= 0) || g.Role == RoleNone || g.Role > RoleAdmin || (g.Subject.Kind != SubjectUser && g.Subject.Kind != SubjectGroup)) {
			r.fail(errors.New("meta: grants not in canonical order"))
			return
		}
		prev = g
		st.Grants[GrantKey{Ino: g.Ino, Subject: g.Subject}] = &g
	}
	n = r.length(MaxInvites)
	var prevH [32]byte
	for i := 0; i < n && r.err == nil; i++ {
		inv := readInvite(r)
		if r.err == nil && (i > 0 && slices.Compare(inv.Hash[:], prevH[:]) <= 0) {
			r.fail(errors.New("meta: invites not in canonical order"))
			return
		}
		prevH = inv.Hash
		st.Invites[inv.Hash] = &inv
	}
	n = r.length(MaxUsers * MaxSessionsPerUser)
	prevH = [32]byte{}
	for i := 0; i < n && r.err == nil; i++ {
		s := readSession(r)
		if r.err == nil && (i > 0 && slices.Compare(s.Hash[:], prevH[:]) <= 0) {
			r.fail(errors.New("meta: sessions not in canonical order"))
			return
		}
		prevH = s.Hash
		st.Sessions[s.Hash] = &s
	}
}

// Command payload codec for ops 28–32 (inside the codec-14 header, command.go).

func writeUsersPayload(w *writer, c Command) {
	switch c.Op { //nolint:exhaustive // the users ops only
	case OpInvite:
		p := mustPayload(c.InviteOp, c.Op)
		w.bool(p.Delete)
		w.bool(p.Bootstrap)
		writeInvite(w, &p.Invite)
		w.i64(p.Now)
	case OpUser:
		p := mustPayload(c.UserOp, c.Op)
		w.u8(uint8(p.Action))
		w.str(p.Name)
		w.str(p.Code)
		w.b = append(w.b, p.Key[:]...)
		w.str(p.Label)
		w.b = append(w.b, p.KeyID[:]...)
		w.u16(uint16(p.Target))
		w.i64(p.Now)
	case OpGroup:
		p := mustPayload(c.GroupOp, c.Op)
		w.u8(uint8(p.Action))
		w.str(p.Name)
		w.u16(uint16(p.Group))
		w.u16(uint16(p.User))
		w.i64(p.Now)
	case OpGrant:
		p := mustPayload(c.GrantOp, c.Op)
		w.u64(uint64(p.Ino))
		writeSubject(w, p.Subject)
		w.u8(uint8(p.Role))
		w.i64(p.Now)
	case OpSession:
		p := mustPayload(c.SessionOp, c.Op)
		w.bool(p.Delete)
		w.bool(p.All)
		w.b = append(w.b, p.Hash[:]...)
		w.u16(uint16(p.User))
		w.b = append(w.b, p.Key[:]...)
		w.i64(p.ExpiresAt)
		w.i64(p.Now)
	}
}

func readUsersPayload(r *reader, c *Command) {
	switch c.Op { //nolint:exhaustive // the users ops only
	case OpInvite:
		p := &InviteOp{Delete: r.bool(), Bootstrap: r.bool()}
		p.Invite = readInvite(r)
		p.Now = r.i64()
		c.InviteOp = p
	case OpUser:
		p := &UserOp{Action: UserAction(r.u8()), Name: r.str(), Code: r.str()}
		copy(p.Key[:], r.take(len(p.Key)))
		p.Label = r.str()
		copy(p.KeyID[:], r.take(len(p.KeyID)))
		p.Target = UserIdx(r.u16())
		p.Now = r.i64()
		c.UserOp = p
	case OpGroup:
		c.GroupOp = &GroupOp{Action: GroupAction(r.u8()), Name: r.str(), Group: GroupIdx(r.u16()), User: UserIdx(r.u16()), Now: r.i64()}
	case OpGrant:
		c.GrantOp = &GrantOp{Ino: Ino(r.u64()), Subject: readSubject(r), Role: Role(r.u8()), Now: r.i64()}
	case OpSession:
		p := &SessionOp{Delete: r.bool(), All: r.bool()}
		copy(p.Hash[:], r.take(len(p.Hash)))
		p.User = UserIdx(r.u16())
		copy(p.Key[:], r.take(len(p.Key)))
		p.ExpiresAt = r.i64()
		p.Now = r.i64()
		c.SessionOp = p
	}
}

// isUsersOp reports whether op is one of 28–32.
func isUsersOp(op Op) bool { return op >= OpInvite && op <= OpSession }
