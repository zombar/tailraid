package update

import (
	"bytes"
	"crypto/ed25519"
	"encoding/base64"
	"errors"
	"math/rand"
	"strings"
	"testing"

	"golang.org/x/crypto/blake2b"
)

// A signature made by Sign verifies under the matching public key and under nothing
// else; one changed byte in the file, the signature, the key id, the trusted comment or
// the global signature fails; the legacy "Ed" form verifies too; malformed files are
// errors, never panics; keys round-trip through their file formats.
//
// Invariant: I-128
func TestMinisign_SignVerifyAndTamper(t *testing.T) {
	rng := rand.New(rand.NewSource(1)) //nolint:gosec // deterministic test keys
	pub, sec, err := GenerateKey(rng)
	if err != nil {
		t.Fatal(err)
	}
	other, _, _ := GenerateKey(rng)
	msg := []byte("abc  tailraid_linux_amd64\n")
	sigText := Sign(sec, msg, "", "timestamp:1\tfile:sha256sums.txt")
	pubText := EncodePublicKey(pub, "")
	if err := VerifyFile(pubText, msg, sigText); err != nil {
		t.Fatalf("good signature: %v", err)
	}
	if err := VerifyFile(EncodePublicKey(other, ""), msg, sigText); !errors.Is(err, ErrBadSignature) {
		t.Fatalf("other key: %v", err)
	}
	if err := VerifyFile(pubText, append([]byte("x"), msg...), sigText); !errors.Is(err, ErrBadSignature) {
		t.Fatalf("changed file: %v", err)
	}
	sig, err := ParseSignature(sigText)
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct {
		name string
		mut  func(*Signature)
	}{
		{"signature byte", func(s *Signature) { s.Sig[5] ^= 1 }},
		{"key id", func(s *Signature) { s.KeyID[0] ^= 1 }},
		{"trusted comment", func(s *Signature) { s.TrustedComment += "x" }},
		{"global signature", func(s *Signature) { s.GlobalSig[0] ^= 1 }},
	} {
		bad := sig
		c.mut(&bad)
		if err := pub.Verify(msg, bad); !errors.Is(err, ErrBadSignature) {
			t.Fatalf("%s tampered: %v", c.name, err)
		}
	}
	// the legacy form signs the file itself; a prehashed signature presented as legacy fails
	legacy := sig
	legacy.Alg = algLegacy
	copy(legacy.Sig[:], ed25519.Sign(sec.Key, msg))
	copy(legacy.GlobalSig[:], ed25519.Sign(sec.Key, append(bytes.Clone(legacy.Sig[:]), []byte(legacy.TrustedComment)...)))
	if err := pub.Verify(msg, legacy); err != nil {
		t.Fatalf("legacy form: %v", err)
	}
	wrongAlg := sig
	wrongAlg.Alg = algLegacy
	if err := pub.Verify(msg, wrongAlg); !errors.Is(err, ErrBadSignature) {
		t.Fatalf("prehashed bytes under the legacy algorithm: %v", err)
	}
	// the prehash is BLAKE2b-512 of the file, as minisign defines it
	h := blake2b.Sum512(msg)
	if !ed25519.Verify(pub.Key, h[:], sig.Sig[:]) {
		t.Fatal("signature is not over the BLAKE2b-512 prehash")
	}
	// key file round trips
	pk2, err := ParsePublicKey(pubText)
	if err != nil || pk2.KeyID != pub.KeyID || !bytes.Equal(pk2.Key, pub.Key) {
		t.Fatalf("public key round trip: %v", err)
	}
	if pk3, err := ParsePublicKey(strings.Split(pubText, "\n")[1]); err != nil || pk3.KeyID != pub.KeyID {
		t.Fatalf("public key without its comment line: %v", err)
	}
	sk2, err := ParseSecretKey(EncodeSecretKey(sec, ""))
	if err != nil || sk2.KeyID != sec.KeyID || !bytes.Equal(sk2.Key, sec.Key) {
		t.Fatalf("secret key round trip: %v", err)
	}
	encrypted := EncodeSecretKey(sec, "")
	raw, _ := base64.StdEncoding.DecodeString(strings.Split(encrypted, "\n")[1])
	raw[2], raw[3] = 'S', 'c'
	if _, err := ParseSecretKey(untrustedTag + "x\n" + base64.StdEncoding.EncodeToString(raw) + "\n"); err == nil || !strings.Contains(err.Error(), "password") {
		t.Fatalf("encrypted key: %v", err)
	}
	for _, bad := range []string{"", "untrusted comment: x\n", "untrusted comment: x\nnot base64!\n", sigText[:len(sigText)-10], strings.Repeat("A", 200)} {
		if _, err := ParseSignature(bad); err == nil {
			t.Fatalf("malformed signature accepted: %q", bad)
		}
		if _, err := ParsePublicKey(bad); err == nil {
			t.Fatalf("malformed public key accepted: %q", bad)
		}
	}
}

// The signature file has minisign's exact shape: the four lines, "ED" with the key id,
// 64-byte signatures.
//
// Invariant: I-128
func TestMinisign_FileShape(t *testing.T) {
	rng := rand.New(rand.NewSource(2)) //nolint:gosec // deterministic test keys
	_, sec, _ := GenerateKey(rng)
	text := Sign(sec, []byte("m"), "untrusted", "trusted")
	lines := strings.Split(strings.TrimSuffix(text, "\n"), "\n")
	if len(lines) != 4 || lines[0] != "untrusted comment: untrusted" || lines[2] != "trusted comment: trusted" {
		t.Fatalf("lines: %q", lines)
	}
	raw, err := base64.StdEncoding.DecodeString(lines[1])
	if err != nil || len(raw) != 2+8+64 || string(raw[:2]) != "ED" || !bytes.Equal(raw[2:10], sec.KeyID[:]) {
		t.Fatalf("signature line: %v %x", err, raw)
	}
	if g, err := base64.StdEncoding.DecodeString(lines[3]); err != nil || len(g) != 64 {
		t.Fatalf("global signature line: %v", err)
	}
}
