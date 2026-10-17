package meta

import (
	"bytes"
	"errors"
	"reflect"
	"strings"
	"testing"
)

// Codec 14 is a header around a lower-version body: every users op round-trips at
// version 14, an old op with an actor round-trips under the header and is version 14,
// one without an actor is still its own version, a header without an actor around an old
// op is refused as non-canonical, a users op in any lower version is refused, and the
// first byte of every such entry is 14, which is what an older binary's decoder refuses.
// Invariant: I-349
func TestCodec14_OlderReaderRefuses(t *testing.T) {
	inv := Invite{Hash: HashCode(ucode(1)), Name: "kim", Kind: InviteUser, Admin: true, ExpiresAt: 9, By: 1}
	for _, c := range []Command{
		{Op: OpInvite, Actor: 1, InviteOp: &InviteOp{Invite: inv, Now: 5}},
		{Op: OpInvite, InviteOp: &InviteOp{Bootstrap: true, Invite: inv, Now: 5}},
		{Op: OpInvite, InviteOp: &InviteOp{Delete: true, Invite: Invite{Hash: inv.Hash}, Now: 5}},
		{Op: OpUser, UserOp: &UserOp{Action: UserCreate, Name: "kim", Code: ucode(1), Key: upub(1), Label: "laptop", Now: 6}},
		{Op: OpUser, Actor: 2, UserOp: &UserOp{Action: UserKeyRemove, Target: 2, KeyID: UserKeyIDOf(upub(1)), Now: 6}},
		{Op: OpGroup, Actor: 1, GroupOp: &GroupOp{Action: GroupCreate, Name: "family", Now: 7}},
		{Op: OpGroup, Actor: 1, GroupOp: &GroupOp{Action: GroupAddMember, Group: 3, User: 2, Now: 7}},
		{Op: OpGrant, Actor: 1, GrantOp: &GrantOp{Ino: 5, Subject: Subject{Kind: SubjectGroup, Idx: 3}, Role: RoleWrite, Now: 8}},
		{Op: OpSession, Actor: 2, SessionOp: &SessionOp{Hash: HashCode("tok"), User: 2, Key: UserKeyIDOf(upub(1)), ExpiresAt: 99, Now: 9}},
		{Op: OpSession, Actor: 2, SessionOp: &SessionOp{Delete: true, All: true, User: 2, Now: 9}},
		{Op: OpMkdir, Actor: 2, Mkdir: &Mkdir{Parent: 1, Name: "d", Mode: 0o755, Now: 10}},
		{Op: OpPurge, System: true, Purge: &Purge{Before: 11, Now: 11}},
		{Op: OpCommit, Actor: 3, Commit: &Commit{Ino: 4, HasTruncate: true, RewriteStart: true, Now: 12}}, // a codec-9 body under the header
		{Op: OpTrash, Actor: 3, Trash: &Trash{Parent: 1, Name: "f", Own: true, Now: 12}},                  // a codec-8 body
		{Op: OpSecretPut, Actor: 2, SecretPut: &SecretPut{Secret: Secret{Name: "user/lee/x", KeyID: KeyID{1}, Cipher: bytes.Repeat([]byte{7}, 20), By: 1, ByUser: 2}, Now: 13}},
	} {
		if v := CommandCodec(c); v != commandVersionV14 || CodecUsers != commandVersionV14 || CodecVersion < commandVersionV15 {
			t.Fatalf("op %d: version %d", c.Op, v)
		}
		b := MustEncodeCommand(c)
		if b[0] != commandVersionV14 {
			t.Fatalf("op %d: first byte %d", c.Op, b[0])
		}
		dec, err := DecodeCommand(b)
		if err != nil || !reflect.DeepEqual(normalizeCommand(dec), normalizeCommand(c)) {
			t.Fatalf("op %d round trip: %v\n got %+v\nwant %+v", c.Op, err, dec, c)
		}
	}
	// an old op without an actor is still its own version
	if b := MustEncodeCommand(Command{Op: OpMkdir, Mkdir: &Mkdir{Parent: 1, Name: "d", Now: 1}}); b[0] != commandVersion {
		t.Fatalf("a plain mkdir is version %d", b[0])
	}
	// a header without an actor around an old op is not canonical
	plain := MustEncodeCommand(Command{Op: OpMkdir, Mkdir: &Mkdir{Parent: 1, Name: "d", Now: 1}})
	if _, err := DecodeCommand(append([]byte{commandVersionV14, 0, 0, 0}, plain...)); err == nil {
		t.Fatal("a codec-14 header without an actor was accepted")
	}
	// a secret without its user under the header is not canonical either
	s := MustEncodeCommand(Command{Op: OpSecretPut, Actor: 1, SecretPut: &SecretPut{Secret: Secret{Name: "x", KeyID: KeyID{1}, Cipher: bytes.Repeat([]byte{7}, 20)}, Now: 1}})
	if s[0] != commandVersionV14 || s[4] != commandVersionV11 {
		t.Fatalf("a secret put by an actor: %x", s[:6])
	}
	// a users op in any lower version is refused
	users := MustEncodeCommand(Command{Op: OpGroup, Actor: 1, GroupOp: &GroupOp{Action: GroupCreate, Name: "g", Now: 1}})
	for _, v := range []uint8{commandVersion, commandVersionV11, 13} {
		old := append([]byte{v}, users[4:]...)
		old[0] = v
		if _, err := DecodeCommand(old); err == nil {
			t.Errorf("a group op decoded in version %d", v)
		}
		wrapped := append([]byte(nil), users...)
		wrapped[4] = v // the inner version
		if _, err := DecodeCommand(wrapped); err == nil {
			t.Errorf("a group op decoded under inner version %d", v)
		}
	}
	// a malformed users payload is refused, never panics
	for _, in := range [][]byte{users[:len(users)-1], append(append([]byte(nil), users...), 1), {commandVersionV14, 1, 0, 0, commandVersionV14, byte(OpGrant)}} {
		if _, err := DecodeCommand(in); err == nil {
			t.Errorf("accepted %x", in)
		}
	}
}

// Snapshot frame 15 carries the users, groups, grants, invites, sessions and a secret's
// user byte for byte; frame 11 of the same state reads back with none of them and
// NextUser and NextGroup zero.
// Invariant: I-349
func TestUsersState_Snapshot(t *testing.T) {
	u := newUsers(t)
	dir := u.mkdir(t, u.kim, RootIno, "d")
	u.must(t, Command{Op: OpGroup, Actor: u.kim, GroupOp: &GroupOp{Action: GroupCreate, Name: "family", Now: u.now}})
	u.must(t, Command{Op: OpGroup, Actor: u.kim, GroupOp: &GroupOp{Action: GroupAddMember, Group: 3, User: u.lee, Now: u.now}})
	if err := u.grant(t, u.kim, dir, group(3), RoleWrite); err != nil {
		t.Fatal(err)
	}
	if err := u.grant(t, u.kim, dir, user(u.pat), RoleRead); err != nil {
		t.Fatal(err)
	}
	u.must(t, Command{Op: OpInvite, Actor: u.kim, InviteOp: &InviteOp{Invite: Invite{Hash: HashCode(ucode(40)), Name: "zed", Kind: InviteUser, ExpiresAt: u.now + hour}, Now: u.now}})
	key := UserKeyIDOf(upub(int(HashCode(ucode(2))[0]) + 300))
	u.must(t, Command{Op: OpSession, Actor: u.lee, SessionOp: &SessionOp{Hash: HashCode("tok"), User: u.lee, Key: key, ExpiresAt: u.now + hour, Now: u.now}})
	var cid KeyID
	cid[0] = 9
	u.must(t, Command{Op: OpSecretKeyInit, SecretKeyInit: &SecretKeyInit{ID: cid, Now: u.now}})
	u.must(t, Command{Op: OpSecretPut, Actor: u.lee, SecretPut: &SecretPut{Secret: sealed("user/lee/t", cid, 8), Now: u.now}})
	var a, b bytes.Buffer
	if err := EncodeState(&a, u.f.st); err != nil {
		t.Fatal(err)
	}
	if !bytes.HasPrefix(a.Bytes(), []byte("TRM1\x0f")) {
		t.Fatalf("header %x", a.Bytes()[:5])
	}
	got, err := DecodeState(bytes.NewReader(a.Bytes()))
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, u.f.st) {
		t.Fatalf("frame 15 round trip\n got %+v\nwant %+v", got, u.f.st)
	}
	if err := EncodeState(&b, got); err != nil || !bytes.Equal(a.Bytes(), b.Bytes()) {
		t.Fatalf("re-encode differs: %v", err)
	}
	if got.Secrets["user/lee/t"].ByUser != u.lee || len(got.Users) != 3 || len(got.Groups) != 3 || len(got.Grants) != 2 || len(got.Invites) != 1 || len(got.Sessions) != 1 {
		t.Fatalf("decoded %+v", got)
	}
	// frame 11: the same state without the users
	var old bytes.Buffer
	if err := encodeStateVersion(&old, u.f.st, stateVersionV11); err != nil {
		t.Fatal(err)
	}
	v11, err := DecodeState(bytes.NewReader(old.Bytes()))
	if err != nil {
		t.Fatal(err)
	}
	if len(v11.Users) != 0 || len(v11.Groups) != 0 || len(v11.Grants) != 0 || len(v11.Invites) != 0 || len(v11.Sessions) != 0 || v11.NextUser != 0 || v11.NextGroup != 0 || v11.Enforcing() {
		t.Fatalf("frame 11 carried users: %+v", v11)
	}
	if v11.Secrets["user/lee/t"].ByUser != 0 || v11.Secrets["user/lee/t"].By != 2 {
		t.Fatalf("frame 11 secret: %+v", v11.Secrets["user/lee/t"])
	}
	// a snapshot out of canonical order is refused
	bad := append([]byte(nil), a.Bytes()...)
	if _, err := DecodeState(bytes.NewReader(bad[:len(bad)-3])); err == nil {
		t.Fatal("a truncated frame 15 was accepted")
	}
	// the clone is deep: a change to the clone leaves the original
	cl := u.f.st.Clone()
	cl.Users[u.lee].Keys[0].Label = "changed"
	cl.Groups[3].Members = append(cl.Groups[3].Members, 99)
	delete(cl.Grants, GrantKey{Ino: dir, Subject: user(u.pat)})
	if u.f.st.Users[u.lee].Keys[0].Label == "changed" || len(u.f.st.Groups[3].Members) != 1 || len(u.f.st.Grants) != 2 {
		t.Fatal("the clone shares the original's rows")
	}
	if v := CheckInvariants(u.f.st); len(v) != 0 {
		t.Fatalf("invariants: %v", v)
	}
	if !errors.Is(applyOne(t, u.f, Command{Op: OpGrant, Actor: u.kim, GrantOp: &GrantOp{Ino: dir, Subject: Subject{Kind: 9, Idx: 1}, Role: RoleRead, Now: u.now}}).Err, ErrBadCommand) {
		t.Fatal("an unknown subject kind")
	}
}

// A snapshot frame 13 or 14 is 0.9.x's, whose mesh fields sit before the users tables:
// it is refused by name with the way out (re-create, docs/12 §6), never decoded as this
// release's layout; this release writes its users state as frame 15.
// Invariant: I-349
func TestDecodeState_MeshFramesRefused(t *testing.T) {
	var buf bytes.Buffer
	if err := EncodeState(&buf, NewState()); err != nil {
		t.Fatal(err)
	}
	if v := buf.Bytes()[4]; v != 15 {
		t.Fatalf("this release writes frame %d, want 15", v)
	}
	for _, v := range []byte{13, 14} {
		b := append([]byte(nil), buf.Bytes()...)
		b[4] = v
		_, err := DecodeState(bytes.NewReader(b))
		if !errors.Is(err, ErrMeshSnapshot) || !strings.Contains(err.Error(), "docs/12 §6") {
			t.Errorf("frame %d: %v, want ErrMeshSnapshot naming docs/12 §6", v, err)
		}
	}
}
