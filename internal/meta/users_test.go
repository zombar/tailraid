package meta

import (
	"bytes"
	"crypto/ed25519"
	"crypto/rand"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/hashicorp/raft"

	"github.com/zombar/tailraid/internal/testseed"
)

const hour = int64(time.Hour)

// ucode is the i-th test invite code: InviteCodeLen base32 characters.
func ucode(i int) string { return fmt.Sprintf("%010d", i)[:InviteCodeLen] }

func upub(i int) [32]byte {
	var k [32]byte
	k[0], k[1], k[31] = byte(i), byte(i>>8), 1
	return k
}

// users is a small cluster of identities for the tests: kim the first admin (from the
// bootstrap invite), lee and pat plain users.
type users struct {
	f             *FSM
	kim, lee, pat UserIdx
	now           int64
}

func newUsers(t *testing.T) *users {
	t.Helper()
	u := &users{f: NewFSM(), now: 10 * hour}
	applyOne(t, u.f, Command{Op: OpSetVolume, SetVolume: &SetVolume{ChunkSize: 1 << 20, K: 1, M: 0, Now: u.now}})
	u.kim = u.bootstrap(t, "kim", ucode(1))
	u.lee = u.invite(t, u.kim, "lee", false, ucode(2))
	u.pat = u.invite(t, u.kim, "pat", false, ucode(3))
	return u
}

func (u *users) apply(t *testing.T, c Command) Result {
	t.Helper()
	return applyOne(t, u.f, c)
}

func (u *users) must(t *testing.T, c Command) Result {
	t.Helper()
	r := u.apply(t, c)
	if r.Err != nil {
		t.Fatalf("op %d: %v", c.Op, r.Err)
	}
	return r
}

func (u *users) bootstrap(t *testing.T, name, code string) UserIdx {
	t.Helper()
	u.must(t, Command{Op: OpInvite, InviteOp: &InviteOp{Bootstrap: true, Invite: Invite{Hash: HashCode(code), Name: name, Kind: InviteUser, ExpiresAt: u.now + hour}, Now: u.now}})
	r := u.must(t, Command{Op: OpUser, UserOp: &UserOp{Action: UserCreate, Name: name, Code: code, Key: upub(int(HashCode(code)[0]) + 1), Label: "laptop", Now: u.now}})
	return UserIdx(r.Ino)
}

func (u *users) invite(t *testing.T, by UserIdx, name string, admin bool, code string) UserIdx {
	t.Helper()
	u.must(t, Command{Op: OpInvite, Actor: by, InviteOp: &InviteOp{Invite: Invite{Hash: HashCode(code), Name: name, Kind: InviteUser, Admin: admin, ExpiresAt: u.now + hour}, Now: u.now}})
	r := u.must(t, Command{Op: OpUser, UserOp: &UserOp{Action: UserCreate, Name: name, Code: code, Key: upub(int(HashCode(code)[0]) + 300), Label: "phone", Now: u.now}})
	return UserIdx(r.Ino)
}

func (u *users) mkdir(t *testing.T, actor UserIdx, parent Ino, name string) Ino {
	t.Helper()
	return u.must(t, Command{Op: OpMkdir, Actor: actor, Mkdir: &Mkdir{Parent: parent, Name: name, Mode: 0o755, Now: u.now}}).Ino
}

func (u *users) grant(t *testing.T, actor UserIdx, ino Ino, sub Subject, role Role) error {
	t.Helper()
	return u.apply(t, Command{Op: OpGrant, Actor: actor, GrantOp: &GrantOp{Ino: ino, Subject: sub, Role: role, Now: u.now}}).Err
}

func user(idx UserIdx) Subject   { return Subject{Kind: SubjectUser, Idx: uint16(idx)} }
func group(idx GroupIdx) Subject { return Subject{Kind: SubjectGroup, Idx: uint16(idx)} }

// A user is created from an invite the admin minted, under the name the invite carries;
// a second key joins through a key invite the user mints for themself; a key is removed
// by its owner or an admin; disable, enable and remove need an admin; a removed row
// stays and its name is free again under a new index.
// Invariant: I-342, I-343
func TestUsers_Apply_Table(t *testing.T) {
	u := newUsers(t)
	if u.kim != 1 || u.lee != 2 || u.pat != 3 {
		t.Fatalf("indices %d %d %d", u.kim, u.lee, u.pat)
	}
	kim, _ := u.f.User(u.kim)
	if kim.Name != "kim" || kim.State != UserActive || len(kim.Keys) != 1 || kim.Keys[0].Label != "laptop" || kim.By != 0 {
		t.Fatalf("kim %+v", kim)
	}
	if lee, _ := u.f.User(u.lee); lee.By != u.kim || lee.Keys[0].By != u.kim {
		t.Fatalf("lee %+v", lee)
	}
	for _, tc := range []struct {
		name string
		c    Command
		want error
	}{
		{"create under a wrong name", Command{Op: OpUser, UserOp: &UserOp{Action: UserCreate, Name: "zed", Code: ucode(4), Key: upub(9), Now: u.now}}, ErrInvite},
		{"create with a used code", Command{Op: OpUser, UserOp: &UserOp{Action: UserCreate, Name: "lee", Code: ucode(2), Key: upub(9), Now: u.now}}, ErrInvite},
		{"create with a zero key", Command{Op: OpUser, UserOp: &UserOp{Action: UserCreate, Name: "zed", Code: ucode(9), Key: [32]byte{}, Now: u.now}}, ErrBadCommand},
		{"create with a bad name", Command{Op: OpUser, UserOp: &UserOp{Action: UserCreate, Name: "Zed", Code: ucode(9), Key: upub(9), Now: u.now}}, ErrBadCommand},
		{"invite by a plain user", Command{Op: OpInvite, Actor: u.lee, InviteOp: &InviteOp{Invite: Invite{Hash: HashCode(ucode(5)), Name: "zed", Kind: InviteUser, ExpiresAt: u.now + hour}, Now: u.now}}, ErrAccess},
		{"invite by nobody without bootstrap", Command{Op: OpInvite, InviteOp: &InviteOp{Invite: Invite{Hash: HashCode(ucode(5)), Name: "zed", Kind: InviteUser, ExpiresAt: u.now + hour}, Now: u.now}}, ErrAccess},
		{"invite of a taken name", Command{Op: OpInvite, Actor: u.kim, InviteOp: &InviteOp{Invite: Invite{Hash: HashCode(ucode(5)), Name: "lee", Kind: InviteUser, ExpiresAt: u.now + hour}, Now: u.now}}, ErrExist},
		{"invite past the TTL", Command{Op: OpInvite, Actor: u.kim, InviteOp: &InviteOp{Invite: Invite{Hash: HashCode(ucode(5)), Name: "zed", Kind: InviteUser, ExpiresAt: u.now + int64(InviteTTL) + 1}, Now: u.now}}, ErrBadCommand},
		{"key invite for another by a plain user", Command{Op: OpInvite, Actor: u.lee, InviteOp: &InviteOp{Invite: Invite{Hash: HashCode(ucode(6)), Kind: InviteKey, ForUser: u.pat, ExpiresAt: u.now + hour}, Now: u.now}}, ErrAccess},
		{"key invite for oneself", Command{Op: OpInvite, Actor: u.lee, InviteOp: &InviteOp{Invite: Invite{Hash: HashCode(ucode(6)), Kind: InviteKey, ForUser: u.lee, ExpiresAt: u.now + hour}, Now: u.now}}, nil},
		{"key add with the key invite", Command{Op: OpUser, UserOp: &UserOp{Action: UserKeyAdd, Code: ucode(6), Key: upub(60), Label: "desk", Now: u.now}}, nil},
		{"key add with the used code", Command{Op: OpUser, UserOp: &UserOp{Action: UserKeyAdd, Code: ucode(6), Key: upub(61), Now: u.now}}, ErrInvite},
		{"key invite by an admin for another", Command{Op: OpInvite, Actor: u.kim, InviteOp: &InviteOp{Invite: Invite{Hash: HashCode(ucode(7)), Kind: InviteKey, ForUser: u.lee, ExpiresAt: u.now + hour}, Now: u.now}}, nil},
		{"the same key twice", Command{Op: OpUser, UserOp: &UserOp{Action: UserKeyAdd, Code: ucode(7), Key: upub(60), Now: u.now}}, ErrExist},
		{"key remove by another user", Command{Op: OpUser, Actor: u.pat, UserOp: &UserOp{Action: UserKeyRemove, Target: u.lee, KeyID: UserKeyIDOf(upub(60)), Now: u.now}}, ErrAccess},
		{"key remove by its owner", Command{Op: OpUser, Actor: u.lee, UserOp: &UserOp{Action: UserKeyRemove, Target: u.lee, KeyID: UserKeyIDOf(upub(60)), Now: u.now}}, nil},
		{"key remove of a missing key", Command{Op: OpUser, Actor: u.kim, UserOp: &UserOp{Action: UserKeyRemove, Target: u.lee, KeyID: UserKeyIDOf(upub(60)), Now: u.now}}, ErrNotExist},
		{"disable by a plain user", Command{Op: OpUser, Actor: u.lee, UserOp: &UserOp{Action: UserDisable, Target: u.pat, Now: u.now}}, ErrAccess},
		{"disable by an admin", Command{Op: OpUser, Actor: u.kim, UserOp: &UserOp{Action: UserDisable, Target: u.pat, Now: u.now}}, nil},
		{"enable by an admin", Command{Op: OpUser, Actor: u.kim, UserOp: &UserOp{Action: UserEnable, Target: u.pat, Now: u.now}}, nil},
		{"remove by an admin", Command{Op: OpUser, Actor: u.kim, UserOp: &UserOp{Action: UserRemove, Target: u.pat, Now: u.now}}, nil},
		{"disable of a removed row", Command{Op: OpUser, Actor: u.kim, UserOp: &UserOp{Action: UserDisable, Target: u.pat, Now: u.now}}, ErrNotExist},
		{"an unknown action", Command{Op: OpUser, Actor: u.kim, UserOp: &UserOp{Action: 99, Target: u.lee, Now: u.now}}, ErrBadCommand},
	} {
		if err := u.apply(t, tc.c).Err; !errors.Is(err, tc.want) || (tc.want == nil && err != nil) {
			t.Errorf("%s: %v, want %v", tc.name, err, tc.want)
		}
	}
	if pat, _ := u.f.User(u.pat); pat.State != UserRemoved || len(pat.Keys) != 0 {
		t.Fatalf("pat after removal: %+v", pat)
	}
	if _, ok := u.f.UserByName("pat"); ok {
		t.Fatal("a removed name is still found by name")
	}
	if len(u.f.Users()) != 3 {
		t.Fatalf("%d rows, want the removed one kept", len(u.f.Users()))
	}
}

// A removed name may be invited again; the new row gets a new index and the old row stays.
// Invariant: I-342
func TestUsers_RemovedNameReinvited(t *testing.T) {
	u := newUsers(t)
	u.must(t, Command{Op: OpUser, Actor: u.kim, UserOp: &UserOp{Action: UserRemove, Target: u.pat, Now: u.now}})
	again := u.invite(t, u.kim, "pat", false, ucode(8))
	if again == u.pat || again != 4 {
		t.Fatalf("pat again is %d (was %d)", again, u.pat)
	}
	if old, _ := u.f.User(u.pat); old.State != UserRemoved || old.Name != "pat" {
		t.Fatalf("the old row: %+v", old)
	}
	if got, _ := u.f.UserByName("pat"); got.Idx != again {
		t.Fatalf("by name: %+v", got)
	}
}

// Every bound: the 1025th user, the 17th key, the 257th group, the 4097th grant, the
// 257th open invite and the 17th session (which evicts the oldest instead).
// Invariant: I-342
func TestUsers_Bounds_Table(t *testing.T) {
	u := newUsers(t)
	// users: invites are bounded too, so mint and consume in turn
	for i := len(u.f.Users()); i < MaxUsers; i++ {
		u.invite(t, u.kim, fmt.Sprintf("u%d", i), false, ucode(1000+i))
	}
	u.must(t, Command{Op: OpInvite, Actor: u.kim, InviteOp: &InviteOp{Invite: Invite{Hash: HashCode(ucode(5000)), Name: "overflow", Kind: InviteUser, ExpiresAt: u.now + hour}, Now: u.now}})
	if err := u.apply(t, Command{Op: OpUser, UserOp: &UserOp{Action: UserCreate, Name: "overflow", Code: ucode(5000), Key: upub(5000), Now: u.now}}).Err; !errors.Is(err, ErrUsersFull) {
		t.Fatalf("user %d: %v", MaxUsers+1, err)
	}
	// keys per user
	for i := 1; i < MaxKeysPerUser; i++ {
		u.must(t, Command{Op: OpInvite, Actor: u.lee, InviteOp: &InviteOp{Invite: Invite{Hash: HashCode(ucode(6000 + i)), Kind: InviteKey, ForUser: u.lee, ExpiresAt: u.now + hour}, Now: u.now}})
		u.must(t, Command{Op: OpUser, UserOp: &UserOp{Action: UserKeyAdd, Code: ucode(6000 + i), Key: upub(6000 + i), Now: u.now}})
	}
	u.must(t, Command{Op: OpInvite, Actor: u.lee, InviteOp: &InviteOp{Invite: Invite{Hash: HashCode(ucode(6100)), Kind: InviteKey, ForUser: u.lee, ExpiresAt: u.now + hour}, Now: u.now}})
	if err := u.apply(t, Command{Op: OpUser, UserOp: &UserOp{Action: UserKeyAdd, Code: ucode(6100), Key: upub(6100), Now: u.now}}).Err; !errors.Is(err, ErrUsersFull) {
		t.Fatalf("key %d: %v", MaxKeysPerUser+1, err)
	}
	if lee, _ := u.f.User(u.lee); len(lee.Keys) != MaxKeysPerUser {
		t.Fatalf("%d keys", len(lee.Keys))
	}
	// groups, the two built-ins included
	for i := len(u.f.Groups()); i < MaxGroups; i++ {
		u.must(t, Command{Op: OpGroup, Actor: u.kim, GroupOp: &GroupOp{Action: GroupCreate, Name: fmt.Sprintf("g%d", i), Now: u.now}})
	}
	if err := u.apply(t, Command{Op: OpGroup, Actor: u.kim, GroupOp: &GroupOp{Action: GroupCreate, Name: "overflow", Now: u.now}}).Err; !errors.Is(err, ErrUsersFull) {
		t.Fatalf("group %d: %v", MaxGroups+1, err)
	}
	// grants: one subject on many inodes
	for i := range MaxGrants {
		ino := u.mkdir(t, u.kim, RootIno, fmt.Sprintf("d%d", i))
		if err := u.grant(t, u.kim, ino, user(u.lee), RoleRead); err != nil {
			t.Fatalf("grant %d: %v", i, err)
		}
	}
	ino := u.mkdir(t, u.kim, RootIno, "one-more")
	if err := u.grant(t, u.kim, ino, user(u.lee), RoleRead); !errors.Is(err, ErrUsersFull) {
		t.Fatalf("grant %d: %v", MaxGrants+1, err)
	}
	if err := u.grant(t, u.kim, Ino(2), user(u.lee), RoleWrite); err != nil {
		t.Fatalf("replacing at the limit: %v", err)
	}
	// open invites
	for i := len(u.f.Invites()); i < MaxInvites; i++ {
		u.must(t, Command{Op: OpInvite, Actor: u.kim, InviteOp: &InviteOp{Invite: Invite{Hash: HashCode(ucode(7000 + i)), Name: fmt.Sprintf("i%d", i), Kind: InviteUser, ExpiresAt: u.now + hour}, Now: u.now}})
	}
	if err := u.apply(t, Command{Op: OpInvite, Actor: u.kim, InviteOp: &InviteOp{Invite: Invite{Hash: HashCode(ucode(7999)), Name: "overflow", Kind: InviteUser, ExpiresAt: u.now + hour}, Now: u.now}}).Err; !errors.Is(err, ErrUsersFull) {
		t.Fatalf("invite %d: %v", MaxInvites+1, err)
	}
	// sessions per user: the 17th evicts the oldest
	key := UserKeyIDOf(upub(int(HashCode(ucode(1))[0]) + 1))
	for i := range MaxSessionsPerUser + 1 {
		u.now++
		u.must(t, Command{Op: OpSession, Actor: u.kim, SessionOp: &SessionOp{Hash: HashCode(fmt.Sprintf("s%d", i)), User: u.kim, Key: key, ExpiresAt: u.now + hour, Now: u.now}})
	}
	if _, ok := u.f.Session(HashCode("s0")); ok {
		t.Fatal("the oldest session survived the 17th")
	}
	if n := len(u.f.Sessions()); n != MaxSessionsPerUser {
		t.Fatalf("%d sessions", n)
	}
	if v := CheckInvariants(u.f.st); len(v) != 0 {
		t.Fatalf("invariants: %v", v)
	}
}

// What the FSM stores and encodes is the public half only: the private key's bytes
// appear in no command and no snapshot.
// Invariant: I-342
func TestUsers_NoPrivateKeyInState(t *testing.T) {
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	f := NewFSM()
	var k [32]byte
	copy(k[:], pub)
	inv := Command{Op: OpInvite, InviteOp: &InviteOp{Bootstrap: true, Invite: Invite{Hash: HashCode(ucode(1)), Name: "kim", Kind: InviteUser, ExpiresAt: 2 * hour}, Now: hour}}
	create := Command{Op: OpUser, UserOp: &UserOp{Action: UserCreate, Name: "kim", Code: ucode(1), Key: k, Label: "laptop", Now: hour}}
	applyOne(t, f, inv)
	if r := applyOne(t, f, create); r.Err != nil {
		t.Fatal(r.Err)
	}
	var snap bytes.Buffer
	if err := EncodeState(&snap, f.st); err != nil {
		t.Fatal(err)
	}
	seed := priv.Seed()
	for name, b := range map[string][]byte{"the create command": MustEncodeCommand(create), "the snapshot": snap.Bytes()} {
		if bytes.Contains(b, seed) {
			t.Errorf("%s holds the private seed", name)
		}
		if !bytes.Contains(b, pub) {
			t.Errorf("%s lacks the public key", name)
		}
	}
	if u, _ := f.User(1); u.Keys[0].Pub != k || u.Keys[0].ID() != UserKeyIDOf(k) {
		t.Fatalf("stored key %+v", u.Keys[0])
	}
}

// Two devices presenting one code: the log serialises them and exactly one user exists.
// Invariant: I-343
func TestInvite_ConsumedOnce(t *testing.T) {
	f := NewFSM()
	applyOne(t, f, Command{Op: OpInvite, InviteOp: &InviteOp{Bootstrap: true, Invite: Invite{Hash: HashCode(ucode(1)), Name: "kim", Kind: InviteUser, ExpiresAt: 2 * hour}, Now: hour}})
	first := applyOne(t, f, Command{Op: OpUser, UserOp: &UserOp{Action: UserCreate, Name: "kim", Code: ucode(1), Key: upub(1), Now: hour}})
	second := applyOne(t, f, Command{Op: OpUser, UserOp: &UserOp{Action: UserCreate, Name: "kim", Code: ucode(1), Key: upub(2), Now: hour}})
	if first.Err != nil || !errors.Is(second.Err, ErrInvite) {
		t.Fatalf("first %v, second %v", first.Err, second.Err)
	}
	if n := len(f.Users()); n != 1 {
		t.Fatalf("%d users", n)
	}
	if _, ok := f.Invite(HashCode(ucode(1))); ok {
		t.Fatal("the invite survived its use")
	}
}

// An expired code leaves no row; the leader's expiry (actor 0) removes an invite only
// past its time, an admin at any time; a bootstrap invite is always a user invite that
// makes an admin.
// Invariant: I-343
func TestInvite_ExpiredAndUsedRefused(t *testing.T) {
	u := newUsers(t)
	h := HashCode(ucode(20))
	u.must(t, Command{Op: OpInvite, Actor: u.kim, InviteOp: &InviteOp{Invite: Invite{Hash: h, Name: "zed", Kind: InviteUser, ExpiresAt: u.now + hour}, Now: u.now}})
	if err := u.apply(t, Command{Op: OpUser, UserOp: &UserOp{Action: UserCreate, Name: "zed", Code: ucode(20), Key: upub(20), Now: u.now + hour}}).Err; !errors.Is(err, ErrInvite) {
		t.Fatalf("create at the expiry: %v", err)
	}
	if _, ok := u.f.UserByName("zed"); ok {
		t.Fatal("an expired code made a row")
	}
	if err := u.apply(t, Command{Op: OpInvite, InviteOp: &InviteOp{Delete: true, Invite: Invite{Hash: h}, Now: u.now + hour - 1}}).Err; !errors.Is(err, ErrAccess) {
		t.Fatalf("expiry before the time: %v", err)
	}
	if err := u.apply(t, Command{Op: OpInvite, InviteOp: &InviteOp{Delete: true, Invite: Invite{Hash: h}, Now: u.now + hour}}).Err; err != nil {
		t.Fatalf("expiry at the time: %v", err)
	}
	u.now += hour // the FSM clock moved with the expiry
	u.must(t, Command{Op: OpInvite, Actor: u.kim, InviteOp: &InviteOp{Invite: Invite{Hash: h, Name: "zed", Kind: InviteUser, ExpiresAt: u.now + 2*hour}, Now: u.now}})
	if err := u.apply(t, Command{Op: OpInvite, Actor: u.lee, InviteOp: &InviteOp{Delete: true, Invite: Invite{Hash: h}, Now: u.now}}).Err; !errors.Is(err, ErrAccess) {
		t.Fatalf("revoke by a plain user: %v", err)
	}
	if err := u.apply(t, Command{Op: OpInvite, Actor: u.kim, InviteOp: &InviteOp{Delete: true, Invite: Invite{Hash: h}, Now: u.now}}).Err; err != nil {
		t.Fatalf("revoke by an admin: %v", err)
	}
	if err := u.apply(t, Command{Op: OpInvite, InviteOp: &InviteOp{Bootstrap: true, Invite: Invite{Hash: HashCode(ucode(21)), Kind: InviteKey, ForUser: u.lee, ExpiresAt: u.now + hour}, Now: u.now}}).Err; !errors.Is(err, ErrAccess) {
		t.Fatalf("a bootstrap key invite: %v", err)
	}
	u.must(t, Command{Op: OpInvite, InviteOp: &InviteOp{Bootstrap: true, Invite: Invite{Hash: HashCode(ucode(22)), Name: "root2", Kind: InviteUser, ExpiresAt: u.now + hour}, Now: u.now}})
	r := u.must(t, Command{Op: OpUser, UserOp: &UserOp{Action: UserCreate, Name: "root2", Code: ucode(22), Key: upub(22), Now: u.now}})
	if !u.f.st.isAdmin(UserIdx(r.Ino)) {
		t.Fatal("the bootstrap invite did not make an admin")
	}
}

// admins and everyone exist with the first user; neither can be removed, everyone takes
// no members; a group's grant reaches its members and goes with the group.
// Invariant: I-346
func TestGroups_Builtin(t *testing.T) {
	u := newUsers(t)
	gs := u.f.Groups()
	if len(gs) != 2 || gs[0].Idx != GroupAdmins || gs[0].Name != "admins" || len(gs[0].Members) != 1 || gs[0].Members[0] != u.kim || gs[1].Idx != GroupEveryone || gs[1].Name != "everyone" || len(gs[1].Members) != 0 {
		t.Fatalf("built-in groups %+v", gs)
	}
	for _, tc := range []struct {
		name string
		c    Command
		want error
	}{
		{"remove admins", Command{Op: OpGroup, Actor: u.kim, GroupOp: &GroupOp{Action: GroupRemove, Group: GroupAdmins, Now: u.now}}, ErrBadCommand},
		{"remove everyone", Command{Op: OpGroup, Actor: u.kim, GroupOp: &GroupOp{Action: GroupRemove, Group: GroupEveryone, Now: u.now}}, ErrBadCommand},
		{"add to everyone", Command{Op: OpGroup, Actor: u.kim, GroupOp: &GroupOp{Action: GroupAddMember, Group: GroupEveryone, User: u.lee, Now: u.now}}, ErrBadCommand},
		{"create by a plain user", Command{Op: OpGroup, Actor: u.lee, GroupOp: &GroupOp{Action: GroupCreate, Name: "family", Now: u.now}}, ErrAccess},
		{"create", Command{Op: OpGroup, Actor: u.kim, GroupOp: &GroupOp{Action: GroupCreate, Name: "family", Now: u.now}}, nil},
		{"create a taken name", Command{Op: OpGroup, Actor: u.kim, GroupOp: &GroupOp{Action: GroupCreate, Name: "family", Now: u.now}}, ErrExist},
		{"create a bad name", Command{Op: OpGroup, Actor: u.kim, GroupOp: &GroupOp{Action: GroupCreate, Name: "Family", Now: u.now}}, ErrBadCommand},
		{"add a member", Command{Op: OpGroup, Actor: u.kim, GroupOp: &GroupOp{Action: GroupAddMember, Group: 3, User: u.pat, Now: u.now}}, nil},
		{"add a missing user", Command{Op: OpGroup, Actor: u.kim, GroupOp: &GroupOp{Action: GroupAddMember, Group: 3, User: 99, Now: u.now}}, ErrNotExist},
	} {
		if err := u.apply(t, tc.c).Err; !errors.Is(err, tc.want) || (tc.want == nil && err != nil) {
			t.Errorf("%s: %v, want %v", tc.name, err, tc.want)
		}
	}
	dir := u.mkdir(t, u.kim, RootIno, "photos")
	if err := u.grant(t, u.kim, dir, group(3), RoleWrite); err != nil {
		t.Fatal(err)
	}
	if got := u.f.Access(u.pat, dir); got != RoleWrite {
		t.Fatalf("pat through family: %s", got)
	}
	if got := u.f.Access(u.lee, dir); got != RoleNone {
		t.Fatalf("lee outside family: %s", got)
	}
	u.must(t, Command{Op: OpGroup, Actor: u.kim, GroupOp: &GroupOp{Action: GroupRemoveMember, Group: 3, User: u.pat, Now: u.now}})
	if got := u.f.Access(u.pat, dir); got != RoleNone {
		t.Fatalf("pat after leaving: %s", got)
	}
	u.must(t, Command{Op: OpGroup, Actor: u.kim, GroupOp: &GroupOp{Action: GroupRemove, Group: 3, Now: u.now}})
	if len(u.f.Grants()) != 0 {
		t.Fatalf("the group's grant survived it: %+v", u.f.Grants())
	}
	if err := u.grant(t, u.kim, dir, group(GroupEveryone), RoleRead); err != nil {
		t.Fatal(err)
	}
	if got := u.f.Access(u.lee, dir); got != RoleRead {
		t.Fatalf("lee through everyone: %s", got)
	}
}

// The last active admin can be neither disabled, removed nor dropped from admins; once
// another admin exists, all three are allowed.
// Invariant: I-346
func TestGroups_LastAdminLockout(t *testing.T) {
	u := newUsers(t)
	for name, c := range map[string]Command{
		"disable": {Op: OpUser, Actor: u.kim, UserOp: &UserOp{Action: UserDisable, Target: u.kim, Now: u.now}},
		"remove":  {Op: OpUser, Actor: u.kim, UserOp: &UserOp{Action: UserRemove, Target: u.kim, Now: u.now}},
		"drop":    {Op: OpGroup, Actor: u.kim, GroupOp: &GroupOp{Action: GroupRemoveMember, Group: GroupAdmins, User: u.kim, Now: u.now}},
	} {
		if err := u.apply(t, c).Err; !errors.Is(err, ErrLockout) {
			t.Errorf("%s of the last admin: %v", name, err)
		}
	}
	u.must(t, Command{Op: OpGroup, Actor: u.kim, GroupOp: &GroupOp{Action: GroupAddMember, Group: GroupAdmins, User: u.lee, Now: u.now}})
	u.must(t, Command{Op: OpGroup, Actor: u.lee, GroupOp: &GroupOp{Action: GroupRemoveMember, Group: GroupAdmins, User: u.kim, Now: u.now}})
	if u.f.st.isAdmin(u.kim) || !u.f.st.isAdmin(u.lee) {
		t.Fatal("admins after the hand-over")
	}
	if err := u.apply(t, Command{Op: OpUser, Actor: u.lee, UserOp: &UserOp{Action: UserDisable, Target: u.lee, Now: u.now}}).Err; !errors.Is(err, ErrLockout) {
		t.Fatalf("lee is now the last admin: %v", err)
	}
	u.must(t, Command{Op: OpGroup, Actor: u.lee, GroupOp: &GroupOp{Action: GroupAddMember, Group: GroupAdmins, User: u.kim, Now: u.now}})
	u.must(t, Command{Op: OpUser, Actor: u.lee, UserOp: &UserOp{Action: UserDisable, Target: u.kim, Now: u.now}})
	if err := u.apply(t, Command{Op: OpUser, Actor: u.lee, UserOp: &UserOp{Action: UserRemove, Target: u.lee, Now: u.now}}).Err; !errors.Is(err, ErrLockout) {
		t.Fatalf("a disabled admin does not count: %v", err)
	}
}

// The effective role is the highest grant on the inode or an ancestor for the user, a
// group holding it or everyone; admins hold Admin everywhere; nobody, a disabled user
// and the reserved tree get None.
// Invariant: I-347
func TestAccess_Table(t *testing.T) {
	u := newUsers(t)
	a := u.mkdir(t, u.kim, RootIno, "a")
	b := u.mkdir(t, u.kim, a, "b")
	c := u.mkdir(t, u.kim, b, "c")
	other := u.mkdir(t, u.kim, RootIno, "other")
	u.must(t, Command{Op: OpGroup, Actor: u.kim, GroupOp: &GroupOp{Action: GroupCreate, Name: "family", Now: u.now}})
	u.must(t, Command{Op: OpGroup, Actor: u.kim, GroupOp: &GroupOp{Action: GroupAddMember, Group: 3, User: u.pat, Now: u.now}})
	for _, g := range []struct {
		ino  Ino
		sub  Subject
		role Role
	}{{a, user(u.lee), RoleRead}, {b, group(3), RoleWrite}, {c, user(u.pat), RoleAdmin}, {c, user(u.lee), RoleWrite}, {other, group(GroupEveryone), RoleRead}} {
		if err := u.grant(t, u.kim, g.ino, g.sub, g.role); err != nil {
			t.Fatal(err)
		}
	}
	zed := u.invite(t, u.kim, "zed", false, ucode(30))
	u.must(t, Command{Op: OpUser, Actor: u.kim, UserOp: &UserOp{Action: UserDisable, Target: zed, Now: u.now}})
	for _, tc := range []struct {
		who  UserIdx
		ino  Ino
		want Role
	}{
		{u.kim, RootIno, RoleAdmin},
		{u.kim, c, RoleAdmin},
		{u.lee, RootIno, RoleNone},
		{u.lee, a, RoleRead},
		{u.lee, b, RoleRead},
		{u.lee, c, RoleWrite},
		{u.lee, other, RoleRead},
		{u.pat, a, RoleNone},
		{u.pat, b, RoleWrite},
		{u.pat, c, RoleAdmin},
		{u.pat, other, RoleRead},
		{0, c, RoleNone},
		{zed, other, RoleNone},
		{99, c, RoleNone},
	} {
		if got := u.f.Access(tc.who, tc.ino); got != tc.want {
			t.Errorf("user %d at %d: %s, want %s", tc.who, tc.ino, got, tc.want)
		}
	}
	if !u.f.Traverses(u.lee, RootIno) || u.f.Traverses(u.pat, a) == false || u.f.Traverses(zed, RootIno) || u.f.Traverses(0, RootIno) {
		t.Fatal("traversal: lee and pat reach the root and a, a disabled user and nobody nothing")
	}
	if got := u.f.Grants(); len(got) != 5 || got[0].Ino != a || got[0].By != u.kim || got[0].At != u.now {
		t.Fatalf("grants %+v", got)
	}
	if err := u.grant(t, u.kim, a, user(u.lee), RoleNone); err != nil || u.f.Access(u.lee, a) != RoleNone || u.f.Access(u.lee, c) != RoleWrite {
		t.Fatalf("clearing a grant: %v", err)
	}
	if err := u.grant(t, u.kim, a, user(u.lee), RoleNone); !errors.Is(err, ErrNotExist) {
		t.Fatalf("clearing it twice: %v", err)
	}
	if err := u.grant(t, u.kim, a, user(u.lee), 4); !errors.Is(err, ErrBadCommand) {
		t.Fatalf("an unknown role: %v", err)
	}
	if err := u.grant(t, u.kim, 999, user(u.lee), RoleRead); !errors.Is(err, ErrNotExist) {
		t.Fatalf("a missing inode: %v", err)
	}
	if err := u.grant(t, u.lee, a, user(u.pat), RoleRead); !errors.Is(err, ErrAccess) {
		t.Fatalf("a grant by a user without Admin there: %v", err)
	}
	if err := u.grant(t, u.pat, c, user(u.lee), RoleRead); err != nil {
		t.Fatalf("a grant by the subtree's admin: %v", err)
	}
}

// Access is a function of the state alone: N replicas fed one random log answer the same
// role for every (user, inode) pair and encode byte-identical snapshots.
// Invariant: I-347
func TestAccess_NNodeDeterminism(t *testing.T) {
	seed, rng := testseed.New(t)
	const n = 3
	fsms := make([]*FSM, n)
	for i := range fsms {
		fsms[i] = NewFSM()
	}
	apply := func(c Command) {
		idx++
		b := MustEncodeCommand(c)
		var first Result
		for i, f := range fsms {
			r := f.Apply(&raft.Log{Index: idx, Data: b}).(Result)
			if i == 0 {
				first = r
			} else if !errors.Is(r.Err, first.Err) || r.Ino != first.Ino {
				t.Fatalf("seed %d: replica %d answered %+v, replica 0 %+v for op %d", seed, i, r, first, c.Op)
			}
		}
	}
	now := hour
	apply(Command{Op: OpInvite, InviteOp: &InviteOp{Bootstrap: true, Invite: Invite{Hash: HashCode(ucode(1)), Name: "kim", Kind: InviteUser, ExpiresAt: now + hour}, Now: now}})
	apply(Command{Op: OpUser, UserOp: &UserOp{Action: UserCreate, Name: "kim", Code: ucode(1), Key: upub(1), Now: now}})
	dirs := []Ino{RootIno}
	users := []UserIdx{1}
	groups := []GroupIdx{GroupAdmins, GroupEveryone}
	for step := 2; step < 400; step++ {
		now++
		switch rng.Intn(6) {
		case 0:
			apply(Command{Op: OpInvite, Actor: 1, InviteOp: &InviteOp{Invite: Invite{Hash: HashCode(ucode(step)), Name: fmt.Sprintf("u%d", step), Kind: InviteUser, Admin: rng.Intn(4) == 0, ExpiresAt: now + hour}, Now: now}})
			apply(Command{Op: OpUser, UserOp: &UserOp{Action: UserCreate, Name: fmt.Sprintf("u%d", step), Code: ucode(step), Key: upub(step), Now: now}})
			users = append(users, UserIdx(len(users)+1))
		case 1:
			apply(Command{Op: OpGroup, Actor: 1, GroupOp: &GroupOp{Action: GroupCreate, Name: fmt.Sprintf("g%d", step), Now: now}})
			groups = append(groups, GroupIdx(len(groups)+1))
		case 2:
			apply(Command{Op: OpGroup, Actor: 1, GroupOp: &GroupOp{Action: GroupAddMember, Group: groups[rng.Intn(len(groups))], User: users[rng.Intn(len(users))], Now: now}})
		case 3:
			parent := dirs[rng.Intn(len(dirs))]
			apply(Command{Op: OpMkdir, Actor: 1, Mkdir: &Mkdir{Parent: parent, Name: fmt.Sprintf("d%d", step), Mode: 0o755, Now: now}})
			dirs = append(dirs, Ino(step+1)) // NextIno grew by one per mkdir that succeeded; a failure is impossible here
		case 4, 5:
			sub := user(users[rng.Intn(len(users))])
			if rng.Intn(2) == 0 {
				sub = group(groups[rng.Intn(len(groups))])
			}
			apply(Command{Op: OpGrant, Actor: 1, GrantOp: &GrantOp{Ino: dirs[rng.Intn(len(dirs))], Subject: sub, Role: Role(1 + rng.Intn(3)), Now: now}})
		}
	}
	var want bytes.Buffer
	if err := EncodeState(&want, fsms[0].st); err != nil {
		t.Fatal(err)
	}
	for i := 1; i < n; i++ {
		var got bytes.Buffer
		if err := EncodeState(&got, fsms[i].st); err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(want.Bytes(), got.Bytes()) {
			t.Fatalf("seed %d: replica %d's snapshot differs", seed, i)
		}
		for _, usr := range users {
			for _, d := range dirs {
				if a, b := fsms[0].Access(usr, d), fsms[i].Access(usr, d); a != b {
					t.Fatalf("seed %d: user %d at %d: replica 0 %s, replica %d %s", seed, usr, d, a, i, b)
				}
			}
		}
	}
	if v := CheckInvariants(fsms[0].st); len(v) != 0 {
		t.Fatalf("seed %d: invariants: %v", seed, v)
	}
}

// Which role each namespace command needs: create, mkdir, unlink, rmdir, trash, rename
// within and across subtrees, SetAttr, restore (on the entry's own parent and on its
// destination) and a rewrite's Commit need Write; purge needs Admin, and the whole-trash
// sweep is System's or an admin's; nobody and a Read grant are refused.
// Invariant: I-348, I-349
func TestAccess_Roles_Table(t *testing.T) {
	u := newUsers(t)
	rd := u.mkdir(t, u.kim, RootIno, "rd")
	wr := u.mkdir(t, u.kim, RootIno, "wr")
	ad := u.mkdir(t, u.kim, RootIno, "ad")
	for _, g := range []struct {
		ino  Ino
		role Role
	}{{rd, RoleRead}, {wr, RoleWrite}, {ad, RoleAdmin}} {
		if err := u.grant(t, u.kim, g.ino, user(u.lee), g.role); err != nil {
			t.Fatal(err)
		}
	}
	mk := func(parent Ino, name string) Ino {
		return u.must(t, Command{Op: OpCreate, Actor: u.kim, Create: &Create{Parent: parent, Name: name, Type: TFile, Mode: 0o644, Now: u.now}}).Ino
	}
	fRd, fWr, fAd := mk(rd, "f"), mk(wr, "f"), mk(ad, "f")
	mkdir := func(parent Ino, name string) Ino { return u.mkdir(t, u.kim, parent, name) }
	dRd, dWr := mkdir(rd, "d"), mkdir(wr, "d")
	trashIt := func(parent Ino, name string) Ino {
		ino := u.f.st.Inodes[parent].Children[name]
		u.must(t, Command{Op: OpTrash, Actor: u.kim, Trash: &Trash{Parent: parent, Name: name, Now: u.now}})
		return ino
	}
	tRd, tWr, tAd := trashIt(rd, "f"), trashIt(wr, "f"), trashIt(ad, "f")
	_ = fRd
	_ = fWr
	_ = fAd
	for _, tc := range []struct {
		name string
		c    Command
		want error
	}{
		{"create under read", Command{Op: OpCreate, Actor: u.lee, Create: &Create{Parent: rd, Name: "n", Type: TFile, Now: u.now}}, ErrAccess},
		{"create under write", Command{Op: OpCreate, Actor: u.lee, Create: &Create{Parent: wr, Name: "n", Type: TFile, Now: u.now}}, nil},
		{"create by nobody", Command{Op: OpCreate, Create: &Create{Parent: wr, Name: "n2", Type: TFile, Now: u.now}}, ErrAccess},
		{"mkdir under read", Command{Op: OpMkdir, Actor: u.lee, Mkdir: &Mkdir{Parent: rd, Name: "n", Now: u.now}}, ErrAccess},
		{"mkdir under write", Command{Op: OpMkdir, Actor: u.lee, Mkdir: &Mkdir{Parent: wr, Name: "m", Now: u.now}}, nil},
		{"unlink under read", Command{Op: OpUnlink, Actor: u.lee, Unlink: &Unlink{Parent: rd, Name: "d", Now: u.now}}, ErrAccess},
		{"rmdir under read", Command{Op: OpRmdir, Actor: u.lee, Rmdir: &Rmdir{Parent: rd, Name: "d", Now: u.now}}, ErrAccess},
		{"rmdir under write", Command{Op: OpRmdir, Actor: u.lee, Rmdir: &Rmdir{Parent: wr, Name: "d", Now: u.now}}, nil},
		{"trash under read", Command{Op: OpTrash, Actor: u.lee, Trash: &Trash{Parent: rd, Name: "d", Now: u.now}}, ErrAccess},
		{"trash under write", Command{Op: OpTrash, Actor: u.lee, Trash: &Trash{Parent: wr, Name: "n", Now: u.now}}, nil},
		{"rename within write", Command{Op: OpRename, Actor: u.lee, Rename: &Rename{OldParent: wr, OldName: "m", NewParent: wr, NewName: "m2", Now: u.now}}, nil},
		{"rename out of read", Command{Op: OpRename, Actor: u.lee, Rename: &Rename{OldParent: rd, OldName: "d", NewParent: wr, NewName: "d", Now: u.now}}, ErrAccess},
		{"rename into read", Command{Op: OpRename, Actor: u.lee, Rename: &Rename{OldParent: wr, OldName: "m2", NewParent: rd, NewName: "m2", Now: u.now}}, ErrAccess},
		{"setattr under read", Command{Op: OpSetAttr, Actor: u.lee, SetAttr: &SetAttr{Ino: dRd, Mask: AttrMode, Mode: 0o700, Now: u.now}}, ErrAccess},
		{"setattr under write", Command{Op: OpSetAttr, Actor: u.lee, SetAttr: &SetAttr{Ino: dWr, Mask: AttrMode, Mode: 0o700, Now: u.now}}, ErrAccess}, // dWr was rmdir'd above: gone
		{"setattr on write root", Command{Op: OpSetAttr, Actor: u.lee, SetAttr: &SetAttr{Ino: wr, Mask: AttrMode, Mode: 0o700, Now: u.now}}, nil},
		{"restore under read", Command{Op: OpRestore, Actor: u.lee, Restore: &Restore{Ino: tRd, Now: u.now}}, ErrAccess},
		{"restore a read entry into write", Command{Op: OpRestore, Actor: u.lee, Restore: &Restore{Ino: tRd, NewParent: wr, NewName: "taken", Now: u.now}}, ErrAccess}, // the entry's own parent needs Write too
		{"restore a write entry into read", Command{Op: OpRestore, Actor: u.lee, Restore: &Restore{Ino: tWr, NewParent: rd, NewName: "moved", Now: u.now}}, ErrAccess},
		{"restore an unknown entry", Command{Op: OpRestore, Actor: u.lee, Restore: &Restore{Ino: 999_999, NewParent: wr, Now: u.now}}, ErrAccess}, // the check runs before the lookup
		{"restore under write", Command{Op: OpRestore, Actor: u.lee, Restore: &Restore{Ino: tWr, Now: u.now}}, nil},
		{"purge under write", Command{Op: OpPurge, Actor: u.lee, Purge: &Purge{Ino: tWr, Now: u.now}}, ErrAccess}, // the check runs before the lookup: a user without Admin learns nothing about the entry
		{"purge under admin", Command{Op: OpPurge, Actor: u.lee, Purge: &Purge{Ino: tAd, Now: u.now}}, nil},
		{"rewrite under read", Command{Op: OpCommit, Actor: u.lee, Commit: &Commit{Ino: fRd, HasTruncate: true, RewriteStart: true, Now: u.now}}, ErrAccess},
		{"rewrite under write", Command{Op: OpCommit, Actor: u.lee, Commit: &Commit{Ino: fWr, HasTruncate: true, RewriteStart: true, Now: u.now}}, nil},
		{"the sweep by a user", Command{Op: OpPurge, Actor: u.kim, Purge: &Purge{Before: u.now + 1, Now: u.now}}, nil},             // an admin holds Admin everywhere; the empty target list passes
		{"the sweep by a plain user", Command{Op: OpPurge, Actor: u.lee, Purge: &Purge{Before: u.now + 1, Now: u.now}}, ErrAccess}, // the whole trash is the admins' (docs/21 §2)
	} {
		if err := u.apply(t, tc.c).Err; !errors.Is(err, tc.want) || (tc.want == nil && err != nil) {
			t.Errorf("%s: %v, want %v", tc.name, err, tc.want)
		}
	}
	if _, ok := u.f.st.Inodes[tAd]; ok {
		t.Fatal("the purge under admin left the root")
	}
	if v := CheckInvariants(u.f.st); len(v) != 0 {
		t.Fatalf("invariants: %v", v)
	}
}

// A deleted root keeps its parent, so a trash entry evaluates on the grants of the place
// it came from; purge under write fails and restore under write works there.
// Invariant: I-348
func TestAccess_TrashEvaluatesOnOriginalParent(t *testing.T) {
	u := newUsers(t)
	wr := u.mkdir(t, u.kim, RootIno, "wr")
	if err := u.grant(t, u.kim, wr, user(u.lee), RoleWrite); err != nil {
		t.Fatal(err)
	}
	f := u.must(t, Command{Op: OpCreate, Actor: u.lee, Create: &Create{Parent: wr, Name: "f", Type: TFile, Now: u.now}}).Ino
	u.must(t, Command{Op: OpTrash, Actor: u.lee, Trash: &Trash{Parent: wr, Name: "f", Now: u.now}})
	if got := u.f.Access(u.lee, f); got != RoleWrite {
		t.Fatalf("the trash entry: %s", got)
	}
	if err := u.apply(t, Command{Op: OpPurge, Actor: u.lee, Purge: &Purge{Ino: f, Now: u.now}}).Err; !errors.Is(err, ErrAccess) {
		t.Fatalf("purge with write: %v", err)
	}
	u.must(t, Command{Op: OpRestore, Actor: u.lee, Restore: &Restore{Ino: f, Now: u.now}})
	// a grant on the file itself goes when the file goes for good
	if err := u.grant(t, u.kim, f, user(u.pat), RoleRead); err != nil {
		t.Fatal(err)
	}
	u.must(t, Command{Op: OpUnlink, Actor: u.lee, Unlink: &Unlink{Parent: wr, Name: "f", Now: u.now}})
	if n := len(u.f.Grants()); n != 1 {
		t.Fatalf("%d grants after the unlink, want the folder's alone", n)
	}
}

// /.tailraid is outside every grant walk: a grant there is refused, an admin holds
// nothing there, a user's create under it is refused, and only a System command writes it.
// Invariant: I-348, I-349
func TestAccess_TailraidTreeExcluded(t *testing.T) {
	u := newUsers(t)
	res := u.must(t, Command{Op: OpMkdir, System: true, Mkdir: &Mkdir{Parent: RootIno, Name: ReservedDir, Mode: 0o755, Now: u.now}}).Ino
	if err := u.grant(t, u.kim, res, user(u.lee), RoleRead); !errors.Is(err, ErrBadCommand) {
		t.Fatalf("a grant on the reserved tree: %v", err)
	}
	if got := u.f.Access(u.kim, res); got != RoleNone {
		t.Fatalf("an admin on the reserved tree: %s", got)
	}
	if err := u.apply(t, Command{Op: OpMkdir, Actor: u.kim, Mkdir: &Mkdir{Parent: res, Name: "thumbs", Now: u.now}}).Err; !errors.Is(err, ErrAccess) {
		t.Fatalf("an admin's mkdir under it: %v", err)
	}
	thumbs := u.must(t, Command{Op: OpMkdir, System: true, Mkdir: &Mkdir{Parent: res, Name: "thumbs", Now: u.now}}).Ino
	u.must(t, Command{Op: OpCreate, System: true, Create: &Create{Parent: thumbs, Name: "t.jpg", Type: TFile, Now: u.now}})
	if err := u.apply(t, Command{Op: OpCreate, System: true, Create: &Create{Parent: RootIno, Name: "stray", Type: TFile, Now: u.now}}).Err; !errors.Is(err, ErrAccess) {
		t.Fatalf("a System create outside the reserved tree: %v", err)
	}
}

// Before the first user every command passes with no actor; from the first user on, a
// user-facing command without an actor is refused, with the right actor applied, and the
// node's own ops (placement, GC, membership) never check.
// Invariant: I-349
func TestFSM_ActorEnforced_Table(t *testing.T) {
	f := NewFSM()
	now := hour
	if !errors.Is(applyOne(t, f, Command{Op: OpMkdir, Mkdir: &Mkdir{Parent: RootIno, Name: "before", Now: now}}).Err, nil) || f.Enforcing() {
		t.Fatal("a mkdir with no actor before users")
	}
	applyOne(t, f, Command{Op: OpInvite, InviteOp: &InviteOp{Bootstrap: true, Invite: Invite{Hash: HashCode(ucode(1)), Name: "kim", Kind: InviteUser, ExpiresAt: now + hour}, Now: now}})
	applyOne(t, f, Command{Op: OpUser, UserOp: &UserOp{Action: UserCreate, Name: "kim", Code: ucode(1), Key: upub(1), Now: now}})
	if !f.Enforcing() {
		t.Fatal("not enforcing after the first user")
	}
	for _, tc := range []struct {
		name string
		c    Command
		want error
	}{
		{"mkdir by nobody", Command{Op: OpMkdir, Mkdir: &Mkdir{Parent: RootIno, Name: "x", Now: now}}, ErrAccess},
		{"mkdir by the admin", Command{Op: OpMkdir, Actor: 1, Mkdir: &Mkdir{Parent: RootIno, Name: "x", Now: now}}, nil},
		{"mkdir by an unknown actor", Command{Op: OpMkdir, Actor: 7, Mkdir: &Mkdir{Parent: RootIno, Name: "y", Now: now}}, ErrAccess},
		{"a node upsert, no actor", Command{Op: OpNodeUpsert, NodeUpsert: &NodeUpsert{Node: Node{ID: "n1", Name: "n1", Kind: KindDevice, Addr: "100.70.3.1"}}}, nil},
		{"a meta set, no actor", Command{Op: OpMetaSet, MetaSet: &MetaSet{JoinToken: "abcdefgh"}}, nil},
		{"a GC pass, no actor", Command{Op: OpGCDelete, GCDelete: &GCDelete{Before: now}}, nil},
		{"the sweep as System", Command{Op: OpPurge, System: true, Purge: &Purge{Before: now, Now: now}}, nil},
	} {
		if err := applyOne(t, f, tc.c).Err; !errors.Is(err, tc.want) || (tc.want == nil && err != nil) {
			t.Errorf("%s: %v, want %v", tc.name, err, tc.want)
		}
	}
}

// The secrets store once users exist (docs/21 §7): a system name is the admins' to put
// and delete, a personal name its owner's to put and the owner's or an admin's to delete,
// each secret records its user, and the two bounds are separate.
// Invariant: I-356
func TestSecrets_PersonalNamespace(t *testing.T) {
	f := NewFSM()
	id := UserKeyID{}
	var cid KeyID
	copy(cid[:], []byte{1, 2, 3, 4, 5, 6, 7, 8})
	_ = id
	applyOne(t, f, Command{Op: OpSecretKeyInit, SecretKeyInit: &SecretKeyInit{ID: cid, Now: 1}})
	if r := applyOne(t, f, Command{Op: OpSecretPut, SecretPut: &SecretPut{Secret: sealed("tmdb", cid, 8), Now: 2}}); r.Err != nil {
		t.Fatalf("a put before users: %v", r.Err)
	}
	u := &users{f: f, now: 10 * hour}
	u.kim = u.bootstrap(t, "kim", ucode(1))
	u.lee = u.invite(t, u.kim, "lee", false, ucode(2))
	u.pat = u.invite(t, u.kim, "pat", false, ucode(3))
	put := func(actor UserIdx, name string) Command {
		return Command{Op: OpSecretPut, Actor: actor, SecretPut: &SecretPut{Secret: sealed(name, cid, 8), Now: u.now}}
	}
	del := func(actor UserIdx, name string) Command {
		return Command{Op: OpSecretDelete, Actor: actor, SecretDelete: &SecretDelete{Name: name, Now: u.now}}
	}
	for _, tc := range []struct {
		name string
		c    Command
		want error
	}{
		{"system put by a plain user", put(u.lee, "tmdb"), ErrAccess},
		{"system put by nobody", put(0, "tmdb"), ErrAccess},
		{"system put by an admin", put(u.kim, "tmdb"), nil},
		{"personal put by another", put(u.kim, "user/lee/token"), ErrAccess},
		{"personal put by the owner", put(u.lee, "user/lee/token"), nil},
		{"personal put under a missing user", put(u.lee, "user/zed/token"), ErrAccess},
		{"personal delete by another plain user", del(u.pat, "user/lee/token"), ErrAccess},
		{"personal delete by an admin", del(u.kim, "user/lee/token"), nil},
		{"system delete by a plain user", del(u.lee, "tmdb"), ErrAccess},
		{"delete all by a plain user", Command{Op: OpSecretDelete, Actor: u.lee, SecretDelete: &SecretDelete{All: true, Now: u.now}}, ErrAccess},
	} {
		if err := u.apply(t, tc.c).Err; !errors.Is(err, tc.want) || (tc.want == nil && err != nil) {
			t.Errorf("%s: %v, want %v", tc.name, err, tc.want)
		}
	}
	if s, _ := f.Secret("tmdb"); s.ByUser != u.kim {
		t.Fatalf("tmdb's user: %+v", s)
	}
	for i := range MaxSecretsPerUser {
		if err := u.apply(t, put(u.lee, fmt.Sprintf("user/lee/s%d", i))).Err; err != nil {
			t.Fatalf("personal secret %d: %v", i, err)
		}
	}
	if err := u.apply(t, put(u.lee, "user/lee/one-more")).Err; !errors.Is(err, ErrSecretFull) {
		t.Fatalf("the 65th personal secret: %v", err)
	}
	if err := u.apply(t, put(u.pat, "user/pat/first")).Err; err != nil {
		t.Fatalf("another user's first: %v", err)
	}
	if err := u.apply(t, put(u.kim, "s3/b2/access_key")).Err; err != nil {
		t.Fatalf("a system secret beside the personal ones: %v", err)
	}
	var snap bytes.Buffer
	if err := EncodeState(&snap, f.st); err != nil {
		t.Fatal(err)
	}
	got, err := DecodeState(bytes.NewReader(snap.Bytes()))
	if err != nil || got.Secrets["user/pat/first"].ByUser != u.pat || len(got.Users) != 3 {
		t.Fatalf("snapshot: %v, %+v", err, got.Secrets["user/pat/first"])
	}
	if owner, ok := SecretOwner("user/lee/x/y"); !ok || owner != "lee" {
		t.Fatalf("owner %q %v", owner, ok)
	}
	if _, ok := SecretOwner("user/"); ok {
		t.Fatal("an empty owner")
	}
	if _, ok := SecretOwner("tmdb"); ok {
		t.Fatal("a system name has an owner")
	}
}

// Sessions in the FSM (docs/21 §6): a user adds one with their own key, removes their own
// or has an admin do it, every session of a user goes with a disable or an all-delete,
// a removed key takes its sessions, and the life is bounded.
// Invariant: I-354
func TestSessions_Apply(t *testing.T) {
	u := newUsers(t)
	kimKey := UserKeyIDOf(upub(int(HashCode(ucode(1))[0]) + 1))
	leeKey := UserKeyIDOf(upub(int(HashCode(ucode(2))[0]) + 300))
	add := func(actor, usr UserIdx, key UserKeyID, name string, life int64) Command {
		return Command{Op: OpSession, Actor: actor, SessionOp: &SessionOp{Hash: HashCode(name), User: usr, Key: key, ExpiresAt: u.now + life, Now: u.now}}
	}
	for _, tc := range []struct {
		name string
		c    Command
		want error
	}{
		{"own key", add(u.lee, u.lee, leeKey, "a", hour), nil},
		{"another's key", add(u.lee, u.lee, kimKey, "b", hour), ErrAccess},
		{"for another user", add(u.lee, u.kim, kimKey, "c", hour), ErrAccess},
		{"by nobody", add(0, u.lee, leeKey, "d", hour), ErrAccess},
		{"too long a life", add(u.lee, u.lee, leeKey, "e", int64(MaxSessionLife)+1), ErrBadCommand},
		{"the longest life", add(u.lee, u.lee, leeKey, "e", int64(MaxSessionLife)), nil},
		{"already present", add(u.lee, u.lee, leeKey, "a", hour), ErrExist},
		{"delete another's", Command{Op: OpSession, Actor: u.pat, SessionOp: &SessionOp{Delete: true, Hash: HashCode("a"), Now: u.now}}, ErrAccess},
		{"delete own", Command{Op: OpSession, Actor: u.lee, SessionOp: &SessionOp{Delete: true, Hash: HashCode("a"), Now: u.now}}, nil},
		{"delete a missing one", Command{Op: OpSession, Actor: u.lee, SessionOp: &SessionOp{Delete: true, Hash: HashCode("a"), Now: u.now}}, ErrNotExist},
		{"admin deletes another's", Command{Op: OpSession, Actor: u.kim, SessionOp: &SessionOp{Delete: true, Hash: HashCode("e"), Now: u.now}}, nil},
		{"all of another by a plain user", Command{Op: OpSession, Actor: u.pat, SessionOp: &SessionOp{Delete: true, All: true, User: u.lee, Now: u.now}}, ErrAccess},
	} {
		if err := u.apply(t, tc.c).Err; !errors.Is(err, tc.want) || (tc.want == nil && err != nil) {
			t.Errorf("%s: %v, want %v", tc.name, err, tc.want)
		}
	}
	u.must(t, add(u.lee, u.lee, leeKey, "f", hour))
	u.must(t, add(u.lee, u.lee, leeKey, "g", hour))
	if s, ok := u.f.Session(HashCode("f")); !ok || s.User != u.lee || s.Key != leeKey || s.At != u.now {
		t.Fatalf("session f: %+v %v", s, ok)
	}
	u.must(t, Command{Op: OpSession, Actor: u.lee, SessionOp: &SessionOp{Delete: true, All: true, User: u.lee, Now: u.now}})
	if n := len(u.f.Sessions()); n != 0 {
		t.Fatalf("%d sessions after all-delete", n)
	}
	u.must(t, add(u.lee, u.lee, leeKey, "h", hour))
	u.must(t, Command{Op: OpUser, Actor: u.lee, UserOp: &UserOp{Action: UserKeyRemove, Target: u.lee, KeyID: leeKey, Now: u.now}})
	if _, ok := u.f.Session(HashCode("h")); ok {
		t.Fatal("a session survived its key")
	}
	u.must(t, add(u.kim, u.kim, kimKey, "i", hour))
	u.must(t, Command{Op: OpGroup, Actor: u.kim, GroupOp: &GroupOp{Action: GroupAddMember, Group: GroupAdmins, User: u.pat, Now: u.now}})
	u.must(t, Command{Op: OpUser, Actor: u.pat, UserOp: &UserOp{Action: UserDisable, Target: u.kim, Now: u.now}})
	if _, ok := u.f.Session(HashCode("i")); ok {
		t.Fatal("a session survived the user's disable")
	}
	if strings.Contains(fmt.Sprint(u.f.Sessions()), "i") {
		t.Fatal("listing")
	}
}

// The role words round-trip through ParseRole, and anything else parses as nothing.
// Invariant: I-347
func TestRole_Words(t *testing.T) {
	for _, r := range []Role{RoleNone, RoleRead, RoleWrite, RoleAdmin} {
		got, ok := ParseRole(r.String())
		if !ok || got != r {
			t.Errorf("%s: %s %v", r, got, ok)
		}
	}
	if _, ok := ParseRole("owner"); ok {
		t.Error("owner parsed")
	}
	if Role(9).String() != "none" {
		t.Error("an unknown role has a word")
	}
}
