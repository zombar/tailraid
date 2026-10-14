package mesh

import (
	"bytes"
	"crypto/ecdh"
	"errors"
	"testing"

	"github.com/zombar/tailraid/internal/keychain"
)

// fakeKeychain is an in-memory Keychain.
type fakeKeychain struct {
	m   map[string]string
	err error
}

func (f *fakeKeychain) Get(name string) (string, string, error) {
	if f.err != nil {
		return "", "", f.err
	}
	v, ok := f.m[name]
	if !ok {
		return "", "", keychain.ErrNotFound
	}
	return v, "fake", nil
}

func (f *fakeKeychain) Set(name, value string) (string, error, error) {
	if f.err != nil {
		return "", nil, f.err
	}
	if f.m == nil {
		f.m = map[string]string{}
	}
	f.m[name] = value
	return "fake", nil, nil
}

func (f *fakeKeychain) Delete(name string) error {
	if _, ok := f.m[name]; !ok {
		return keychain.ErrNotFound
	}
	delete(f.m, name)
	return nil
}

// Invariant: I-316
func TestKey_GeneratePublicParse(t *testing.T) {
	priv, err := GenerateKey()
	if err != nil {
		t.Fatal(err)
	}
	if priv[0]&7 != 0 || priv[31]&128 != 0 || priv[31]&64 == 0 {
		t.Fatalf("private key is not clamped: %x", priv)
	}
	pub := priv.Public()
	ref, err := ecdh.X25519().NewPrivateKey(priv[:])
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(ref.PublicKey().Bytes(), pub[:]) {
		t.Fatal("public key differs from crypto/ecdh's")
	}
	back, err := ParseKey(pub.String())
	if err != nil || back != pub {
		t.Fatalf("ParseKey(%s) = %v, %v", pub, back, err)
	}
	if _, err := ParseKey("not a key"); !errors.Is(err, ErrBadKey) {
		t.Fatalf("ParseKey(junk) = %v, want ErrBadKey", err)
	}
	if _, err := ParseKey("AAAA"); !errors.Is(err, ErrBadKey) {
		t.Fatalf("ParseKey(short) = %v, want ErrBadKey", err)
	}
	if pub.IsZero() || (Key{}).IsZero() != true {
		t.Fatal("IsZero")
	}
	if len(pub.Hex()) != 64 {
		t.Fatalf("Hex: %q", pub.Hex())
	}
}

// Invariant: I-316
func TestKey_KeychainLoadCreateForget(t *testing.T) {
	kc := &fakeKeychain{}
	var cluster [16]byte
	cluster[0] = 7
	k1, created, err := LoadOrCreateKey(kc, cluster)
	if err != nil || !created {
		t.Fatalf("first load: %v created=%v", err, created)
	}
	if _, ok := kc.m[KeyAccount(cluster)]; !ok {
		t.Fatalf("the key is not under the account %s: %v", KeyAccount(cluster), kc.m)
	}
	k2, created, err := LoadOrCreateKey(kc, cluster)
	if err != nil || created || k2 != k1 {
		t.Fatalf("second load: %v created=%v same=%v", err, created, k2 == k1)
	}
	var other [16]byte
	other[0] = 8
	k3, _, err := LoadOrCreateKey(kc, other)
	if err != nil || k3 == k1 {
		t.Fatal("another cluster must get another key")
	}
	if err := ForgetKey(kc, cluster); err != nil {
		t.Fatal(err)
	}
	if err := ForgetKey(kc, cluster); err != nil {
		t.Fatalf("forgetting twice: %v", err)
	}
	k4, created, err := LoadOrCreateKey(kc, cluster)
	if err != nil || !created || k4 == k1 {
		t.Fatalf("after forget: %v created=%v same=%v", err, created, k4 == k1)
	}
	kc.m[KeyAccount(cluster)] = "zz"
	if _, _, err := LoadOrCreateKey(kc, cluster); !errors.Is(err, ErrBadKey) {
		t.Fatalf("a corrupt value: %v", err)
	}
	broken := &fakeKeychain{err: errors.New("keyring down")}
	if _, _, err := LoadOrCreateKey(broken, cluster); err == nil {
		t.Fatal("a keychain error must surface")
	}
}
