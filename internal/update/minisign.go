package update

import (
	"bytes"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"strings"

	"golang.org/x/crypto/blake2b"
)

// Release signing (docs/12 §4, I-128): every release's sha256sums.txt carries a
// sha256sums.txt.minisig produced by cmd/tailraid-sign under the key pair whose public
// half is ReleaseKey. The formats are those of jedisct1/minisign, so the same files
// verify with `minisign -Vm sha256sums.txt -P <key>`: a key is "Ed" || key id (8) ||
// ed25519 public key (32); a signature is "ED" (ed25519 over the BLAKE2b-512 prehash of
// the file; "Ed" is the legacy form over the file itself) || key id || signature (64),
// followed by a trusted comment and a global signature over signature || comment, so the
// comment cannot be swapped between files.

// Errors of signature handling.
var (
	ErrBadSignature = errors.New("update: signature does not verify")
	ErrNoSignature  = errors.New("update: release carries no sha256sums.txt.minisig")
	ErrNoReleaseKey = errors.New("update: this build embeds no release signing key")
	errMalformed    = errors.New("update: malformed minisign file")
)

const (
	algPrehashed = "ED"
	algLegacy    = "Ed"
	untrustedTag = "untrusted comment: "
	trustedTag   = "trusted comment: "
)

// PublicKey is a minisign public key.
type PublicKey struct {
	KeyID [8]byte
	Key   ed25519.PublicKey
}

// SecretKey is the private half; the tool keeps it, the binary never sees one.
type SecretKey struct {
	KeyID [8]byte
	Key   ed25519.PrivateKey
}

// Signature is a parsed .minisig file.
type Signature struct {
	Alg            string
	KeyID          [8]byte
	Sig            [ed25519.SignatureSize]byte
	TrustedComment string
	GlobalSig      [ed25519.SignatureSize]byte
}

// GenerateKey makes a fresh key pair with a random key id.
func GenerateKey(r io.Reader) (PublicKey, SecretKey, error) {
	if r == nil {
		r = rand.Reader
	}
	pub, priv, err := ed25519.GenerateKey(r)
	if err != nil {
		return PublicKey{}, SecretKey{}, err
	}
	var id [8]byte
	if _, err := io.ReadFull(r, id[:]); err != nil {
		return PublicKey{}, SecretKey{}, err
	}
	return PublicKey{KeyID: id, Key: pub}, SecretKey{KeyID: id, Key: priv}, nil
}

// ParsePublicKey reads a minisign public key: the base64 line, with or without the
// untrusted comment line in front of it.
func ParsePublicKey(s string) (PublicKey, error) {
	raw, err := decodeLine(payloadLine(s))
	if err != nil {
		return PublicKey{}, err
	}
	if len(raw) != 2+8+ed25519.PublicKeySize || string(raw[:2]) != algLegacy {
		return PublicKey{}, fmt.Errorf("%w: public key", errMalformed)
	}
	var pk PublicKey
	copy(pk.KeyID[:], raw[2:10])
	pk.Key = ed25519.PublicKey(bytes.Clone(raw[10:]))
	return pk, nil
}

// EncodePublicKey writes the key in minisign's public key file format.
func EncodePublicKey(pk PublicKey, comment string) string {
	raw := append([]byte(algLegacy), pk.KeyID[:]...)
	raw = append(raw, pk.Key...)
	if comment == "" {
		comment = "minisign public key " + keyIDHex(pk.KeyID)
	}
	return untrustedTag + comment + "\n" + base64.StdEncoding.EncodeToString(raw) + "\n"
}

// EncodeSecretKey writes the key in minisign's unencrypted secret key layout (what
// `minisign -G -W` writes): "Ed" || kdf "\x00\x00" || checksum alg "B2" || salt (32) ||
// ops limit (8) || mem limit (8) || key id (8) || secret key (64) || BLAKE2b-256 checksum
// of "Ed" || key id || secret key.
func EncodeSecretKey(sk SecretKey, comment string) string {
	raw := []byte(algLegacy)
	raw = append(raw, 0, 0, 'B', '2')
	raw = append(raw, make([]byte, 32+8+8)...)
	raw = append(raw, sk.KeyID[:]...)
	raw = append(raw, sk.Key...)
	sum := blake2b.Sum256(append(append([]byte(algLegacy), sk.KeyID[:]...), sk.Key...))
	raw = append(raw, sum[:]...)
	if comment == "" {
		comment = "minisign secret key " + keyIDHex(sk.KeyID)
	}
	return untrustedTag + comment + "\n" + base64.StdEncoding.EncodeToString(raw) + "\n"
}

// ParseSecretKey reads an unencrypted secret key written by EncodeSecretKey or
// `minisign -G -W`.
func ParseSecretKey(s string) (SecretKey, error) {
	raw, err := decodeLine(payloadLine(s))
	if err != nil {
		return SecretKey{}, err
	}
	const n = 2 + 2 + 2 + 32 + 8 + 8 + 8 + ed25519.PrivateKeySize + 32
	if len(raw) != n || string(raw[:2]) != algLegacy {
		return SecretKey{}, fmt.Errorf("%w: secret key", errMalformed)
	}
	if raw[2] != 0 || raw[3] != 0 {
		return SecretKey{}, errors.New("update: the secret key is password-protected; tailraid-sign reads only unencrypted (-W) keys")
	}
	var sk SecretKey
	body := raw[2+2+2+32+8+8:]
	copy(sk.KeyID[:], body[:8])
	sk.Key = ed25519.PrivateKey(bytes.Clone(body[8 : 8+ed25519.PrivateKeySize]))
	sum := blake2b.Sum256(append(append([]byte(algLegacy), sk.KeyID[:]...), sk.Key...))
	if !bytes.Equal(sum[:], body[8+ed25519.PrivateKeySize:]) {
		return SecretKey{}, fmt.Errorf("%w: secret key checksum", errMalformed)
	}
	return sk, nil
}

// Sign produces the .minisig text for msg: prehashed ("ED"), with the trusted comment
// bound by the global signature.
func Sign(sk SecretKey, msg []byte, untrusted, trusted string) string {
	h := blake2b.Sum512(msg)
	sig := ed25519.Sign(sk.Key, h[:])
	global := ed25519.Sign(sk.Key, append(bytes.Clone(sig), []byte(trusted)...))
	raw := append([]byte(algPrehashed), sk.KeyID[:]...)
	raw = append(raw, sig...)
	if untrusted == "" {
		untrusted = "signature from tailraid-sign key " + keyIDHex(sk.KeyID)
	}
	return untrustedTag + untrusted + "\n" + base64.StdEncoding.EncodeToString(raw) + "\n" +
		trustedTag + trusted + "\n" + base64.StdEncoding.EncodeToString(global) + "\n"
}

// ParseSignature reads a .minisig file.
func ParseSignature(s string) (Signature, error) {
	var lines []string
	for l := range strings.SplitSeq(strings.ReplaceAll(s, "\r\n", "\n"), "\n") {
		if strings.TrimSpace(l) != "" {
			lines = append(lines, l)
		}
	}
	if len(lines) != 4 || !strings.HasPrefix(lines[0], untrustedTag) || !strings.HasPrefix(lines[2], trustedTag) {
		return Signature{}, fmt.Errorf("%w: want untrusted comment, signature, trusted comment, global signature", errMalformed)
	}
	raw, err := decodeLine(lines[1])
	if err != nil {
		return Signature{}, err
	}
	if len(raw) != 2+8+ed25519.SignatureSize {
		return Signature{}, fmt.Errorf("%w: signature", errMalformed)
	}
	var sig Signature
	sig.Alg = string(raw[:2])
	if sig.Alg != algPrehashed && sig.Alg != algLegacy {
		return Signature{}, fmt.Errorf("%w: algorithm %q", errMalformed, sig.Alg)
	}
	copy(sig.KeyID[:], raw[2:10])
	copy(sig.Sig[:], raw[10:])
	sig.TrustedComment = strings.TrimPrefix(lines[2], trustedTag)
	global, err := decodeLine(lines[3])
	if err != nil {
		return Signature{}, err
	}
	if len(global) != ed25519.SignatureSize {
		return Signature{}, fmt.Errorf("%w: global signature", errMalformed)
	}
	copy(sig.GlobalSig[:], global)
	return sig, nil
}

// Verify checks sig over msg under pk: the key id must match, the (prehashed or legacy)
// signature must verify, and the global signature must bind the trusted comment.
func (pk PublicKey) Verify(msg []byte, sig Signature) error {
	if sig.KeyID != pk.KeyID {
		return fmt.Errorf("%w: signed by key %s, want %s", ErrBadSignature, keyIDHex(sig.KeyID), keyIDHex(pk.KeyID))
	}
	m := msg
	if sig.Alg == algPrehashed {
		h := blake2b.Sum512(msg)
		m = h[:]
	}
	if !ed25519.Verify(pk.Key, m, sig.Sig[:]) {
		return ErrBadSignature
	}
	if !ed25519.Verify(pk.Key, append(bytes.Clone(sig.Sig[:]), []byte(sig.TrustedComment)...), sig.GlobalSig[:]) {
		return fmt.Errorf("%w: trusted comment", ErrBadSignature)
	}
	return nil
}

// VerifyFile is the one-call form: msg verifies against the .minisig text under the
// public key text.
func VerifyFile(pubKey string, msg []byte, minisig string) error {
	pk, err := ParsePublicKey(pubKey)
	if err != nil {
		return err
	}
	sig, err := ParseSignature(minisig)
	if err != nil {
		return err
	}
	return pk.Verify(msg, sig)
}

// payloadLine returns the first line that is not a comment (the base64 payload).
func payloadLine(s string) string {
	for l := range strings.SplitSeq(strings.ReplaceAll(s, "\r\n", "\n"), "\n") {
		l = strings.TrimSpace(l)
		if l == "" || strings.HasPrefix(l, untrustedTag) || strings.HasPrefix(l, trustedTag) {
			continue
		}
		return l
	}
	return ""
}

func decodeLine(l string) ([]byte, error) {
	raw, err := base64.StdEncoding.DecodeString(strings.TrimSpace(l))
	if err != nil {
		return nil, fmt.Errorf("%w: %w", errMalformed, err)
	}
	return raw, nil
}

func keyIDHex(id [8]byte) string { return fmt.Sprintf("%X", id[:]) }
