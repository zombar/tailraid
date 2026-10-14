// Package mesh is tailraid's own WireGuard network (docs/20): one wireguard-go device per
// node on a TUN, its peers taken from the member table, the join code and the introduce
// that admit a device, and the endpoint candidates and fan-out that find a peer.
package mesh

import (
	"crypto/ecdh"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"

	"github.com/zombar/tailraid/internal/keychain"
)

// Key is a device's WireGuard public key: its identity on the mesh (docs/20 §2).
type Key [32]byte

// PrivateKey is the half that never leaves the device's keychain.
type PrivateKey [32]byte

// Errors of the key layer.
var (
	ErrBadKey = errors.New("mesh: bad key")
)

// GenerateKey makes a fresh Curve25519 private key, clamped as WireGuard expects.
func GenerateKey() (PrivateKey, error) {
	var k PrivateKey
	if _, err := rand.Read(k[:]); err != nil {
		return PrivateKey{}, fmt.Errorf("mesh: crypto/rand: %w", err)
	}
	k[0] &= 248
	k[31] &= 127
	k[31] |= 64
	return k, nil
}

// Public derives the public key.
func (k PrivateKey) Public() Key {
	priv, err := ecdh.X25519().NewPrivateKey(k[:])
	if err != nil {
		panic("mesh: curve25519: " + err.Error()) // a 32-byte scalar is always accepted
	}
	var pub Key
	copy(pub[:], priv.PublicKey().Bytes())
	return pub
}

// IsZero reports an unset key.
func (k Key) IsZero() bool { return k == Key{} }

// String is the base64 form WireGuard tools print.
func (k Key) String() string { return base64.StdEncoding.EncodeToString(k[:]) }

// Hex is the form the device's IPC takes.
func (k Key) Hex() string { return hex.EncodeToString(k[:]) }

// ParseKey reads a base64 public key.
func ParseKey(s string) (Key, error) {
	b, err := base64.StdEncoding.DecodeString(s)
	if err != nil || len(b) != len(Key{}) {
		return Key{}, fmt.Errorf("%w: %q", ErrBadKey, s)
	}
	var k Key
	copy(k[:], b)
	return k, nil
}

// Keychain is the consumer-defined slice of internal/keychain the key layer uses.
type Keychain interface {
	Get(name string) (value, source string, err error)
	Set(name, value string) (source string, keyringErr, err error)
	Delete(name string) error
}

// KeyAccount is the keychain account of a cluster's mesh key on this device.
func KeyAccount(clusterID [16]byte) string { return "mesh-" + hex.EncodeToString(clusterID[:]) }

// LoadOrCreateKey returns the device's key for the cluster, making one on the first call
// (I-316). created says whether it was made now.
func LoadOrCreateKey(kc Keychain, clusterID [16]byte) (key PrivateKey, created bool, err error) {
	account := KeyAccount(clusterID)
	v, _, err := kc.Get(account)
	switch {
	case err == nil:
		b, err := hex.DecodeString(v)
		if err != nil || len(b) != len(key) {
			return PrivateKey{}, false, fmt.Errorf("%w: the keychain's %s is not a key", ErrBadKey, account)
		}
		copy(key[:], b)
		return key, false, nil
	case errors.Is(err, keychain.ErrNotFound):
	default:
		return PrivateKey{}, false, fmt.Errorf("mesh: keychain: %w", err)
	}
	key, err = GenerateKey()
	if err != nil {
		return PrivateKey{}, false, err
	}
	if _, _, err := kc.Set(account, hex.EncodeToString(key[:])); err != nil {
		return PrivateKey{}, false, fmt.Errorf("mesh: keychain: %w", err)
	}
	return key, true, nil
}

// ForgetKey removes the device's key for the cluster (`uninstall --forget-identity`).
func ForgetKey(kc Keychain, clusterID [16]byte) error {
	if err := kc.Delete(KeyAccount(clusterID)); err != nil && !errors.Is(err, keychain.ErrNotFound) {
		return fmt.Errorf("mesh: keychain: %w", err)
	}
	return nil
}
