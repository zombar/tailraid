// SPDX-License-Identifier: BSD-3-Clause

package smb

import (
	"encoding/binary"
	"strings"
	"testing"
)

// connectAs does a TREE_CONNECT as one user on a fresh connection, and hands
// back the status and the access mask the server granted.
func connectAs(t *testing.T, c *conn, user, share string) (uint32, uint32) {
	t.Helper()
	c.sessions[0] = &session{user: user}
	n := utf16le(`\\127.0.0.1\` + share)
	body := make([]byte, 8)
	binary.LittleEndian.PutUint16(body[4:], uint16(headerLen+8))
	binary.LittleEndian.PutUint16(body[6:], uint16(len(n)))
	out, err := c.dispatch(request(cmdTreeConnect, 0, append(body, n...)))
	if err != nil {
		t.Fatalf("%s connecting to %s: %v", user, share, err)
	}
	h, _ := parseHeader(out)
	if h.status != statusSuccess {
		return h.status, 0
	}
	return h.status, binary.LittleEndian.Uint32(out[headerLen+12:])
}

// Three shares, two people, and every combination of the two lists.
//
// The access mask is checked as well as the status, because a client acts on
// it: told read-only, a file manager greys the actions out; told it may write,
// it offers them and fails one at a time, which looks like a broken share
// rather than a permission.
func TestWhoMayConnectAndWhoMayWrite(t *testing.T) {
	srv := New()
	srv.AddUser("alice", "hunter2")
	srv.AddUser("bob", "swordfish")
	for _, s := range []struct {
		name string
		opts []ShareOption
	}{
		{"open", nil},
		{"alices", []ShareOption{AllowUsers("alice")}},
		{"shared", []ShareOption{WriteUsers("alice")}},
		{"museum", []ShareOption{ReadOnly(), AllowUsers("alice", "bob")}},
	} {
		if err := srv.Share(s.name, nothingFS{}, s.opts...); err != nil {
			t.Fatal(err)
		}
	}

	for _, tc := range []struct {
		user, share string
		wantStatus  uint32
		wantAccess  uint32
		why         string
	}{
		{"alice", "open", statusSuccess, accessAll, "no lists at all is everyone, read-write"},
		{"bob", "open", statusSuccess, accessAll, "no lists at all is everyone, read-write"},
		{"alice", "alices", statusSuccess, accessAll, "named in allow"},
		{"bob", "alices", statusAccessDenied, 0, "not named in allow"},
		{"alice", "shared", statusSuccess, accessAll, "named in writers"},
		{"bob", "shared", statusSuccess, accessRead, "may connect, may not write"},
		{"alice", "museum", statusSuccess, accessRead, "read-only beats being allowed"},
		{"bob", "museum", statusSuccess, accessRead, "read-only beats being allowed"},
	} {
		st, access := connectAs(t, newConn(srv, nil), tc.user, tc.share)
		if st != tc.wantStatus {
			t.Errorf("%s on %s: status %#x, want %#x (%s)", tc.user, tc.share, st, tc.wantStatus, tc.why)
			continue
		}
		if access != tc.wantAccess {
			t.Errorf("%s on %s: access %#08x, want %#08x (%s)", tc.user, tc.share, access, tc.wantAccess, tc.why)
		}
	}
}

// The same share, two people, at the same time, on ONE connection.
//
// This is why a tree id resolves to what the connection may do rather than to
// the share: the share is a single object, and the answer to "may this write"
// is not a property of it.
func TestTwoPeopleHoldOneShareWithDifferentAnswers(t *testing.T) {
	srv := New()
	srv.AddUser("alice", "hunter2")
	srv.AddUser("bob", "swordfish")
	if err := srv.Share("shared", nothingFS{}, WriteUsers("alice")); err != nil {
		t.Fatal(err)
	}
	c := newConn(srv, nil)
	if _, access := connectAs(t, c, "bob", "shared"); access != accessRead {
		t.Errorf("bob was granted %#08x, want read-only", access)
	}
	if _, access := connectAs(t, c, "alice", "shared"); access != accessAll {
		t.Errorf("alice was granted %#08x after bob connected, want everything", access)
	}
	// Two trees, one share underneath, and the read-only flag on the tree.
	if len(c.trees) != 2 {
		t.Fatalf("%d trees, want two", len(c.trees))
	}
	if c.trees[1].sh != c.trees[2].sh {
		t.Error("the two trees do not point at the same share")
	}
	if !c.trees[1].ro || c.trees[2].ro {
		t.Errorf("read-only came out as %v and %v, want true then false", c.trees[1].ro, c.trees[2].ro)
	}
}

// A volume a person may not write to says so in its attributes, which is what
// a client shows on the disk itself rather than on the first refusal.
func TestAReadOnlyVolumeSaysSoInItsAttributes(t *testing.T) {
	const readOnlyVolume = 0x00080000
	sh := &share{name: "disk", fsys: nothingFS{}}
	for _, ro := range []bool{false, true} {
		b := fsInfo(fsAttributeInformation, sh, ro)
		if len(b) < 4 {
			t.Fatalf("attribute information came back as %d bytes", len(b))
		}
		got := binary.LittleEndian.Uint32(b)&readOnlyVolume != 0
		if got != ro {
			t.Errorf("read-only %v: the volume reported %v", ro, got)
		}
	}
}

func TestTheAccessListsCompose(t *testing.T) {
	sh := &share{}
	AllowUsers("alice")(sh)
	AllowUsers("bob")(sh)
	WriteUsers("alice")(sh)
	if !sh.mayConnect("bob") || !sh.mayConnect("alice") || sh.mayConnect("carol") {
		t.Errorf("allow came out as %v", sh.allow)
	}
	if sh.readOnlyFor("alice") || !sh.readOnlyFor("bob") {
		t.Errorf("writers came out as %v", sh.writers)
	}
	ReadOnly()(sh)
	if !sh.readOnlyFor("alice") {
		t.Error("a read-only share let a writer write")
	}
}

// A server can prove somebody from the MD4 of their password -- the "NT hash"
// -- which is what a directory keeps: Samba's sambaNTPassword, or a column
// beside it in a database.
//
// The judge is a client this project did not write: it computes the same proof
// from the PASSWORD, so a server that accepts it from the hash alone has the
// arithmetic right.
func TestAUserProvedFromAnNTHash(t *testing.T) {
	srv := New()
	// alice is added the ordinary way; bob only as a hash, the way a
	// directory would have him.
	srv.AddUser("alice", "hunter2")
	if err := srv.AddUserHash("bob", md4sum(utf16le("swordfish"))); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name string
		hash []byte
		want string
	}{
		{"too short", make([]byte, 15), "16 bytes"},
		{"too long", make([]byte, 17), "16 bytes"},
		{"nothing at all", nil, "16 bytes"},
	} {
		if err := srv.AddUserHash("mallory", tc.hash); err == nil ||
			!strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: %v", tc.name, err)
		}
	}

	// Both prove the same way: NTOWFv2 is an HMAC keyed by that MD4, so the
	// two paths meet before the challenge is ever touched.
	ch, _, err := newChallenge("GOFS")
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		user, password string
		want           bool
	}{
		{"alice", "hunter2", true},
		{"bob", "swordfish", true},
		{"bob", "hunter2", false},
	} {
		cred, ok := srv.credentialFor(tc.user)
		if !ok {
			t.Fatalf("%s is not known", tc.user)
		}
		auth := clientAuth(t, ch, tc.user, "", tc.password)
		if _, got := ch.verify(auth, cred); got != tc.want {
			t.Errorf("%s with %q: accepted = %v, want %v", tc.user, tc.password, got, tc.want)
		}
	}
}
