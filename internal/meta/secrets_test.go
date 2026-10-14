package meta

import (
	"bytes"
	"errors"
	"reflect"
	"strings"
	"testing"

	"github.com/hashicorp/raft"
)

func sealed(name string, id KeyID, n int) Secret {
	return Secret{Name: name, KeyID: id, Nonce: [SecretNonceSize]byte{1, 2, 3}, Cipher: bytes.Repeat([]byte{7}, n+SecretTagSize), By: 2}
}

// The ops are codec 11 and only it; they round-trip, and an older version carrying one
// is refused.
// Invariant: I-307
func TestSecretOps_Codec11(t *testing.T) {
	id := KeyID{9, 8, 7, 6, 5, 4, 3, 2}
	for _, c := range []Command{
		{Op: OpSecretKeyInit, SecretKeyInit: &SecretKeyInit{ID: id, Now: 5}},
		{Op: OpSecretPut, SecretPut: &SecretPut{Secret: sealed("s3/b2", id, 40), Now: 6}},
		{Op: OpSecretDelete, SecretDelete: &SecretDelete{Name: "tmdb", Now: 7}},
		{Op: OpSecretDelete, SecretDelete: &SecretDelete{All: true, Now: 8}},
	} {
		if v := CommandCodec(c); v != commandVersionV11 || CodecSecrets != commandVersionV11 || CodecVersion != commandVersionV11 {
			t.Fatalf("op %d: version %d", c.Op, v)
		}
		b := MustEncodeCommand(c)
		dec, err := DecodeCommand(b)
		if err != nil || !reflect.DeepEqual(dec, c) {
			t.Fatalf("op %d round trip: %v %+v", c.Op, err, dec)
		}
		for _, v := range []uint8{commandVersion, commandVersionV7, commandVersionV10} {
			old := append([]byte(nil), b...)
			old[0] = v
			if _, err := DecodeCommand(old); err == nil {
				t.Errorf("op %d decoded in version %d", c.Op, v)
			}
		}
	}
}

// One key per cluster: the first init wins, the second is ErrSecretKey; a put under any
// other key, a bad name, an empty or oversized value, or the 257th name is refused; a
// delete of every secret clears the key ID.
// Invariant: I-305, I-307
func TestSecretApply_Table(t *testing.T) {
	f := NewFSM()
	id, other := KeyID{1}, KeyID{2}
	apply := func(c Command) error {
		t.Helper()
		return applyOne(t, f, c).Err
	}
	if err := apply(Command{Op: OpSecretPut, SecretPut: &SecretPut{Secret: sealed("tmdb", id, 8), Now: 1}}); !errors.Is(err, ErrSecretKey) {
		t.Fatalf("a put before any key: %v", err)
	}
	if err := apply(Command{Op: OpSecretKeyInit, SecretKeyInit: &SecretKeyInit{ID: id, Now: 1}}); err != nil {
		t.Fatal(err)
	}
	if err := apply(Command{Op: OpSecretKeyInit, SecretKeyInit: &SecretKeyInit{ID: other, Now: 2}}); !errors.Is(err, ErrSecretKey) || f.SecretKeyID() != id {
		t.Fatalf("a second key: %v, id %v", err, f.SecretKeyID())
	}
	for _, tc := range []struct {
		name string
		s    Secret
		want error
	}{
		{"ok", sealed("tmdb", id, 8), nil},
		{"replace", sealed("tmdb", id, 9), nil},
		{"slash name", sealed("s3/b2", id, 8), nil},
		{"other key", sealed("x", other, 8), ErrSecretKey},
		{"bad name", sealed("../x", id, 8), ErrBadCommand},
		{"upper name", sealed("Tmdb", id, 8), ErrBadCommand},
		{"empty value", sealed("e", id, 0), ErrBadCommand},
		{"largest value", sealed("big", id, MaxSecretValue), nil},
		{"oversized value", sealed("huge", id, MaxSecretValue+1), ErrBadCommand},
		{"long name", sealed(strings.Repeat("a", 129), id, 8), ErrBadCommand},
	} {
		if err := apply(Command{Op: OpSecretPut, SecretPut: &SecretPut{Secret: tc.s, Now: 3}}); !errors.Is(err, tc.want) || tc.want == nil && err != nil {
			t.Errorf("%s: %v, want %v", tc.name, err, tc.want)
		}
	}
	if s, ok := f.Secret("tmdb"); !ok || len(s.Cipher) != 9+SecretTagSize || s.At != 3 || s.By != 2 {
		t.Errorf("tmdb: %+v %v", s, ok)
	}
	for i := len(f.Secrets()); i < MaxSecrets; i++ {
		if err := apply(Command{Op: OpSecretPut, SecretPut: &SecretPut{Secret: sealed("n"+strings.Repeat("x", i%100)+string(rune('a'+i%26))+string(rune('a'+i/26)), id, 1), Now: 4}}); err != nil {
			t.Fatalf("put %d: %v", i, err)
		}
	}
	if n := len(f.Secrets()); n != MaxSecrets {
		t.Fatalf("%d secrets", n)
	}
	if err := apply(Command{Op: OpSecretPut, SecretPut: &SecretPut{Secret: sealed("one-more", id, 1), Now: 5}}); !errors.Is(err, ErrSecretFull) {
		t.Errorf("past the limit: %v", err)
	}
	if err := apply(Command{Op: OpSecretPut, SecretPut: &SecretPut{Secret: sealed("tmdb", id, 2), Now: 5}}); err != nil {
		t.Errorf("replacing at the limit: %v", err)
	}
	if err := apply(Command{Op: OpSecretDelete, SecretDelete: &SecretDelete{Name: "nope", Now: 6}}); !errors.Is(err, ErrNotExist) {
		t.Errorf("delete of a missing name: %v", err)
	}
	if err := apply(Command{Op: OpSecretDelete, SecretDelete: &SecretDelete{Name: "tmdb", Now: 6}}); err != nil {
		t.Fatal(err)
	}
	if _, ok := f.Secret("tmdb"); ok {
		t.Error("tmdb survived its delete")
	}
	if err := apply(Command{Op: OpSecretDelete, SecretDelete: &SecretDelete{All: true, Now: 7}}); err != nil || len(f.Secrets()) != 0 || f.SecretKeyID() != (KeyID{}) {
		t.Errorf("delete all: %v, %d left, id %v", err, len(f.Secrets()), f.SecretKeyID())
	}
	if err := apply(Command{Op: OpSecretKeyInit, SecretKeyInit: &SecretKeyInit{ID: other, Now: 8}}); err != nil {
		t.Errorf("a new key after delete all: %v", err)
	}
}

// The store and the key ID survive a snapshot byte for byte; listing leaves the sealed
// values out; an older frame decodes to an empty store.
// Invariant: I-306, I-307
func TestSecretState_Snapshot(t *testing.T) {
	f := NewFSM()
	id := KeyID{3, 1, 4}
	applyOne(t, f, Command{Op: OpSecretKeyInit, SecretKeyInit: &SecretKeyInit{ID: id, Now: 1}})
	applyOne(t, f, Command{Op: OpSecretPut, SecretPut: &SecretPut{Secret: sealed("tmdb", id, 30), Now: 2}})
	applyOne(t, f, Command{Op: OpSecretPut, SecretPut: &SecretPut{Secret: sealed("s3/b2", id, 50), Now: 3}})
	var a, b bytes.Buffer
	if err := EncodeState(&a, f.st); err != nil {
		t.Fatal(err)
	}
	got, err := DecodeState(bytes.NewReader(a.Bytes()))
	if err != nil {
		t.Fatal(err)
	}
	if got.SecretKeyID != id || len(got.Secrets) != 2 || !bytes.Equal(got.Secrets["tmdb"].Cipher, f.st.Secrets["tmdb"].Cipher) {
		t.Fatalf("decoded %v %d", got.SecretKeyID, len(got.Secrets))
	}
	if err := EncodeState(&b, got); err != nil || !bytes.Equal(a.Bytes(), b.Bytes()) {
		t.Fatalf("re-encoding differs: %v", err)
	}
	for _, s := range f.Secrets() {
		if s.Cipher != nil {
			t.Errorf("Secrets() carries %s's sealed value", s.Name)
		}
	}
	c := f.st.Clone()
	c.Secrets["tmdb"].Cipher[0] ^= 1
	if f.st.Secrets["tmdb"].Cipher[0] == c.Secrets["tmdb"].Cipher[0] {
		t.Error("Clone shares a sealed value")
	}
	var old bytes.Buffer
	if err := encodeStateVersion(&old, f.st, stateVersionV9); err != nil {
		t.Fatal(err)
	}
	if o, err := DecodeState(bytes.NewReader(old.Bytes())); err != nil || len(o.Secrets) != 0 || o.SecretKeyID != (KeyID{}) {
		t.Errorf("frame 9: %v %+v", err, o)
	}
}

// idx numbers the log entries the secrets tests apply.
var idx uint64

func applyOne(t *testing.T, f *FSM, c Command) Result {
	t.Helper()
	idx++
	return f.Apply(&raft.Log{Index: idx, Data: MustEncodeCommand(c)}).(Result)
}
