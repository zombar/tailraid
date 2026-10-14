package users

import (
	"crypto/ed25519"
	"errors"
	"net/netip"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/zombar/tailraid/internal/keychain"
	"github.com/zombar/tailraid/internal/meta"
)

// memKeychain is a keychain in memory.
type memKeychain struct{ m map[string]string }

func newMem() *memKeychain { return &memKeychain{m: map[string]string{}} }

func (k *memKeychain) Get(name string) (string, string, error) {
	v, ok := k.m[name]
	if !ok {
		return "", "", keychain.ErrNotFound
	}
	return v, "mem", nil
}

func (k *memKeychain) Set(name, value string) (string, error, error) {
	k.m[name] = value
	return "mem", nil, nil
}

func (k *memKeychain) Delete(name string) error {
	delete(k.m, name)
	return nil
}

// fixedUsers answers UserByName from a map.
type fixedUsers map[string]meta.User

func (f fixedUsers) UserByName(name string) (meta.User, bool) {
	u, ok := f[name]
	return u, ok
}

var cid = [16]byte{1, 2, 3, 4}

func ident(t *testing.T, name string, seed byte) Identity {
	t.Helper()
	var s [ed25519.SeedSize]byte
	s[0], s[31] = seed, 1
	return Identity{Name: name, Priv: ed25519.NewKeyFromSeed(s[:])}
}

func verifier(id Identity, now time.Time) *Verifier {
	u := meta.User{Idx: 7, Name: id.Name, State: meta.UserActive, Keys: []meta.UserKey{{Pub: id.Pub()}}}
	return &Verifier{Users: fixedUsers{id.Name: u}, Cluster: cid, NodeID: "node-a", Now: func() time.Time { return now }}
}

// A signature is deterministic for its inputs, its header round-trips, and the verifier
// names the user and the key it carries.
// Invariant: I-344
func TestSig_Vectors(t *testing.T) {
	id := ident(t, "kim", 3)
	now := time.Unix(1_800_000_000, 0)
	var nonce [16]byte
	nonce[0] = 9
	body := []byte(`{"op":"grant"}`)
	sig := Sign(id, cid, "node-a", "POST", "/v1/admin", body, now.Unix(), nonce)
	again := Sign(id, cid, "node-a", "POST", "/v1/admin", body, now.Unix(), nonce)
	if sig.String() != again.String() {
		t.Fatal("not deterministic")
	}
	want := "user=kim key=" + hexKey(id) + " ts=1800000000 nonce=09000000000000000000000000000000 sig="
	if !strings.HasPrefix(sig.String(), want) {
		t.Fatalf("header %q", sig.String())
	}
	parsed, err := Parse(sig.String())
	if err != nil || parsed.String() != sig.String() {
		t.Fatalf("parse: %v %q", err, parsed.String())
	}
	v := verifier(id, now)
	idx, key, err := v.Verify(sig.String(), netip.MustParseAddr("100.70.3.2"), "POST", "/v1/admin", body)
	if err != nil || idx != 7 || key != id.KeyID() {
		t.Fatalf("verify: %v %d %x", err, idx, key)
	}
	for _, bad := range []string{"", "user=kim", sig.String() + " extra=1", "user=Kim " + sig.String()[9:], strings.Replace(sig.String(), "sig=", "sig=AAAA", 1)} {
		if _, err := Parse(bad); err == nil {
			t.Errorf("parsed %q", bad)
		}
	}
}

func hexKey(id Identity) string {
	k := id.KeyID()
	const hexdigits = "0123456789abcdef"
	var b []byte
	for _, c := range k {
		b = append(b, hexdigits[c>>4], hexdigits[c&15])
	}
	return string(b)
}

// Every part of the signed bytes matters, the timestamp must sit inside the skew, a
// nonce is good once, a disabled user and an unknown key are refused, and every
// refusal is the one error.
// Invariant: I-344
func TestSig_SkewAndReplay_Table(t *testing.T) {
	id := ident(t, "kim", 3)
	now := time.Unix(1_800_000_000, 0)
	src := netip.MustParseAddr("100.70.3.2")
	body := []byte("{}")
	fresh := func() (string, [16]byte) {
		n, _ := NewNonce()
		return Sign(id, cid, "node-a", "POST", "/v1/admin", body, now.Unix(), n).String(), n
	}
	v := verifier(id, now)
	for _, tc := range []struct {
		name   string
		hdr    string
		method string
		path   string
		body   []byte
		want   error
	}{
		{"good", "", "POST", "/v1/admin", body, nil},
		{"other method", "", "PUT", "/v1/admin", body, ErrUnauthorized},
		{"other path", "", "POST", "/v1/members", body, ErrUnauthorized},
		{"other body", "", "POST", "/v1/admin", []byte("{ }"), ErrUnauthorized},
		{"at the skew", Sign(id, cid, "node-a", "POST", "/v1/admin", body, now.Add(-DefaultSkew).Unix(), [16]byte{1}).String(), "POST", "/v1/admin", body, nil},
		{"past the skew", Sign(id, cid, "node-a", "POST", "/v1/admin", body, now.Add(-DefaultSkew-time.Second).Unix(), [16]byte{2}).String(), "POST", "/v1/admin", body, ErrUnauthorized},
		{"from the future", Sign(id, cid, "node-a", "POST", "/v1/admin", body, now.Add(DefaultSkew+time.Second).Unix(), [16]byte{3}).String(), "POST", "/v1/admin", body, ErrUnauthorized},
		{"other cluster", Sign(id, [16]byte{9}, "node-a", "POST", "/v1/admin", body, now.Unix(), [16]byte{4}).String(), "POST", "/v1/admin", body, ErrUnauthorized},
		{"unknown key", Sign(ident(t, "kim", 4), cid, "node-a", "POST", "/v1/admin", body, now.Unix(), [16]byte{5}).String(), "POST", "/v1/admin", body, ErrUnauthorized},
		{"unknown user", Sign(ident(t, "zed", 3), cid, "node-a", "POST", "/v1/admin", body, now.Unix(), [16]byte{6}).String(), "POST", "/v1/admin", body, ErrUnauthorized},
		{"malformed", "user=kim", "POST", "/v1/admin", body, ErrUnauthorized},
	} {
		hdr := tc.hdr
		if hdr == "" {
			hdr, _ = fresh()
		}
		_, _, err := v.Verify(hdr, src, tc.method, tc.path, tc.body)
		if !errors.Is(err, tc.want) || (tc.want == nil && err != nil) {
			t.Errorf("%s: %v, want %v", tc.name, err, tc.want)
		}
	}
	// a nonce is good once
	hdr, _ := fresh()
	if _, _, err := v.Verify(hdr, src, "POST", "/v1/admin", body); err != nil {
		t.Fatal(err)
	}
	if _, _, err := v.Verify(hdr, src, "POST", "/v1/admin", body); !errors.Is(err, ErrUnauthorized) {
		t.Fatalf("a replay: %v", err)
	}
	// a disabled user
	u, _ := v.Users.UserByName("kim")
	u.State = meta.UserDisabled
	v.Users = fixedUsers{"kim": u}
	hdr, _ = fresh()
	if _, _, err := v.Verify(hdr, src, "POST", "/v1/admin", body); !errors.Is(err, ErrUnauthorized) {
		t.Fatalf("a disabled user: %v", err)
	}
	// the nonce cache is bounded
	v = verifier(id, now)
	for i := range NonceCache + 10 {
		var n [16]byte
		n[0], n[1] = byte(i), byte(i>>8)
		if _, _, err := v.Verify(Sign(id, cid, "node-a", "POST", "/v1/admin", body, now.Unix(), n).String(), src, "POST", "/v1/admin", body); err != nil {
			t.Fatalf("nonce %d: %v", i, err)
		}
	}
	if len(v.nonces) > NonceCache {
		t.Fatalf("%d nonces kept", len(v.nonces))
	}
}

// A signature names the node it is for: the one another node receives is refused, so a
// captured request replays nowhere else.
// Invariant: I-344
func TestSig_TargetNodeBound(t *testing.T) {
	id := ident(t, "kim", 3)
	now := time.Unix(1_800_000_000, 0)
	src := netip.MustParseAddr("100.70.3.2")
	hdr := Sign(id, cid, "node-a", "POST", "/v1/admin", nil, now.Unix(), [16]byte{1}).String()
	a, b := verifier(id, now), verifier(id, now)
	b.NodeID = "node-b"
	if _, _, err := a.Verify(hdr, src, "POST", "/v1/admin", nil); err != nil {
		t.Fatal(err)
	}
	if _, _, err := b.Verify(hdr, src, "POST", "/v1/admin", nil); !errors.Is(err, ErrUnauthorized) {
		t.Fatalf("another node accepted it: %v", err)
	}
}

// A hundred consecutive failures on one key from one source refuse that source for a
// minute, even with a good signature; a success in between resets the count; another
// source is untouched.
// Invariant: I-344
func TestSig_FailureLockout(t *testing.T) {
	id := ident(t, "kim", 3)
	now := time.Unix(1_800_000_000, 0)
	v := verifier(id, now)
	v.Now = func() time.Time { return now }
	src, other := netip.MustParseAddr("100.70.3.2"), netip.MustParseAddr("100.70.3.3")
	bad := Sign(ident(t, "kim", 4), cid, "node-a", "POST", "/v1/admin", nil, now.Unix(), [16]byte{1})
	bad.User = "kim"
	bad.Key = id.KeyID() // kim's key with a wrong signature
	good := func(n byte) string {
		return Sign(id, cid, "node-a", "POST", "/v1/admin", nil, now.Unix(), [16]byte{n, 1}).String()
	}
	for range MaxFailures - 1 {
		_, _, _ = v.Verify(bad.String(), src, "POST", "/v1/admin", nil)
	}
	if _, _, err := v.Verify(good(1), src, "POST", "/v1/admin", nil); err != nil {
		t.Fatalf("before the hundredth failure: %v", err)
	}
	for range MaxFailures {
		_, _, _ = v.Verify(bad.String(), src, "POST", "/v1/admin", nil)
	}
	if _, _, err := v.Verify(good(2), src, "POST", "/v1/admin", nil); !errors.Is(err, ErrUnauthorized) {
		t.Fatalf("locked out: %v", err)
	}
	if _, _, err := v.Verify(good(3), other, "POST", "/v1/admin", nil); err != nil {
		t.Fatalf("another source: %v", err)
	}
	now = now.Add(LockoutFor + time.Second)
	if _, _, err := v.Verify(good(4), src, "POST", "/v1/admin", nil); err != nil {
		t.Fatalf("after the minute: %v", err)
	}
}

// The identity lives in the keychain under the cluster's account as name and seed; a
// second create refuses, a malformed value is reported, forget removes it; a code is
// ten base32 characters and a bootstrap token is read once.
// Invariant: I-342, I-343
func TestIdentity_Keychain(t *testing.T) {
	kc := newMem()
	if _, ok, err := Load(kc, cid); ok || err != nil {
		t.Fatalf("empty: %v %v", ok, err)
	}
	id, err := Create(kc, cid, "kim")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Create(kc, cid, "lee"); !errors.Is(err, ErrHasIdent) {
		t.Fatalf("a second identity: %v", err)
	}
	if _, err := Create(newMem(), cid, "Kim"); err == nil {
		t.Fatal("a bad name")
	}
	got, ok, err := Load(kc, cid)
	if err != nil || !ok || got.Name != "kim" || got.Pub() != id.Pub() || got.KeyID() != id.KeyID() {
		t.Fatalf("load: %v %v %+v", err, ok, got)
	}
	if v := kc.m[Account(cid)]; !strings.HasPrefix(v, "kim:") || strings.Contains(v, string(id.Priv.Seed())) || len(v) != 4+2*ed25519.SeedSize {
		t.Fatalf("stored %q", v)
	}
	kc.m[Account(cid)] = "kim:zz"
	if _, _, err := Load(kc, cid); !errors.Is(err, ErrBadIdent) {
		t.Fatalf("malformed: %v", err)
	}
	if err := Forget(kc, cid); err != nil {
		t.Fatal(err)
	}
	if _, ok, _ := Load(kc, cid); ok {
		t.Fatal("forgotten identity found")
	}
	if err := Forget(kc, cid); err != nil {
		t.Fatalf("forget twice: %v", err)
	}
	code, err := NewCode()
	if err != nil || len(code) != meta.InviteCodeLen || strings.Trim(code, "abcdefghijklmnopqrstuvwxyz234567") != "" {
		t.Fatalf("code %q %v", code, err)
	}
	dir := t.TempDir()
	tok, err := WriteBootstrap(dir)
	if err != nil {
		t.Fatal(err)
	}
	if runtime.GOOS != "windows" {
		if fi, _ := os.Stat(filepath.Join(dir, BootstrapFile)); fi.Mode().Perm() != 0o600 {
			t.Fatalf("token file %04o", fi.Mode().Perm())
		}
	}
	if TakeBootstrap(dir, "other") {
		t.Fatal("a wrong token taken")
	}
	if TakeBootstrap(dir, tok) {
		t.Fatal("a token survived its first take")
	}
	tok, _ = WriteBootstrap(dir)
	if !TakeBootstrap(dir, tok) || TakeBootstrap(dir, tok) {
		t.Fatal("the token is read once")
	}
}

// The bindings file: bind, look up, list, unbind, a bad credential refused, 0600, read
// back on reopen.
// Invariant: I-350
func TestBindings_File(t *testing.T) {
	dir := t.TempDir()
	b, err := OpenBindings(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		cred, user string
		want       error
	}{
		{"uid:501", "kim", nil},
		{"smb:tailraid", "kim", nil},
		{"uid:502", "lee", nil},
		{"uid:", "kim", ErrBadCredential},
		{"uid:5a", "kim", ErrBadCredential},
		{"smb:", "kim", ErrBadCredential},
		{"gid:1", "kim", ErrBadCredential},
		{"uid:503", "Kim", errors.New("users: name")},
	} {
		err := b.Bind(tc.cred, tc.user)
		switch {
		case tc.want == nil && err != nil:
			t.Errorf("%s: %v", tc.cred, err)
		case tc.want != nil && err == nil:
			t.Errorf("%s: bound", tc.cred)
		case tc.want != nil && errors.Is(tc.want, ErrBadCredential) && !errors.Is(err, ErrBadCredential):
			t.Errorf("%s: %v", tc.cred, err)
		}
	}
	if u, ok := b.Lookup("uid:501"); !ok || u != "kim" {
		t.Fatalf("lookup %q %v", u, ok)
	}
	if _, ok := b.Lookup("uid:999"); ok {
		t.Fatal("an unbound uid is someone")
	}
	if runtime.GOOS != "windows" {
		if fi, _ := os.Stat(filepath.Join(dir, BindingsFile)); fi.Mode().Perm() != 0o600 {
			t.Fatalf("file %04o", fi.Mode().Perm())
		}
	}
	again, err := OpenBindings(dir)
	if err != nil {
		t.Fatal(err)
	}
	if l := again.List(); len(l) != 3 || l[0].Credential != "smb:tailraid" || l[1].User != "kim" || l[2].User != "lee" {
		t.Fatalf("list %+v", l)
	}
	if err := again.Unbind("uid:502"); err != nil {
		t.Fatal(err)
	}
	if err := again.Unbind("uid:502"); err != nil {
		t.Fatalf("unbind twice: %v", err)
	}
	third, _ := OpenBindings(dir)
	if _, ok := third.Lookup("uid:502"); ok || len(third.List()) != 2 {
		t.Fatal("unbind did not reach the file")
	}
	if err := os.WriteFile(filepath.Join(dir, BindingsFile), []byte("{"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := OpenBindings(dir); err == nil {
		t.Fatal("a malformed file was opened")
	}
}

// The credential a person binds is their own uid, never the one behind a sudo; the
// default name comes from SUDO_USER before the OS user and is cut to the pattern.
// Invariant: I-350
func TestBindings_OwnUidNeverSudo(t *testing.T) {
	t.Setenv("SUDO_UID", "0")
	t.Setenv("SUDO_USER", "Root.Admin@corp")
	if CallerUID() != os.Getuid() {
		t.Fatalf("CallerUID %d, want %d", CallerUID(), os.Getuid())
	}
	if UIDCredential(501) != "uid:501" || SMBCredential("tailraid") != "smb:tailraid" {
		t.Fatal("credential forms")
	}
	if got := DefaultName("ignored"); got != "root.admin" {
		t.Fatalf("with SUDO_USER: %q", got)
	}
	t.Setenv("SUDO_USER", "")
	for in, want := range map[string]string{"Kim": "kim", "CORP\\Lee": "corp", "pat@host": "pat", "9lives": "", "jo hn": "john", strings.Repeat("a", 40): strings.Repeat("a", meta.MaxUserName), "": ""} {
		if got := DefaultName(in); got != want {
			t.Errorf("%q: %q, want %q", in, got, want)
		}
	}
}
