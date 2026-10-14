// Package users is the device side of docs/21: a person's identity in their own
// keychain, the signature every mutating request carries, the node's verifier of it,
// and the bindings from a device's local credentials to users.
package users

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/zombar/tailraid/internal/config"
	"github.com/zombar/tailraid/internal/keychain"
	"github.com/zombar/tailraid/internal/meta"
)

// Identity errors.
var (
	ErrNoIdentity = errors.New("users: this device holds no identity for the cluster (tailraid user create <name> <code>)")
	ErrHasIdent   = errors.New("users: this device already holds an identity for the cluster")
	ErrBadIdent   = errors.New("users: the keychain's identity is malformed")
)

// Keychain is what an identity is kept in: the person's own keychain, never the daemon's.
type Keychain interface {
	Get(name string) (value, source string, err error)
	Set(name, value string) (source string, keyringErr, err error)
	Delete(name string) error
}

// Identity is a person's key on this device (docs/21 §1): the name they are known by and
// the Ed25519 key that signs for them. The private half never leaves the keychain but
// into this process.
type Identity struct {
	Name string
	Priv ed25519.PrivateKey
}

// Pub is the public key as the FSM stores it.
func (id Identity) Pub() [32]byte {
	var k [32]byte
	copy(k[:], id.Priv.Public().(ed25519.PublicKey))
	return k
}

// KeyID is the key's short name.
func (id Identity) KeyID() meta.UserKeyID { return meta.UserKeyIDOf(id.Pub()) }

// Account is the keychain account of a cluster's identity on this device.
func Account(clusterID [16]byte) string { return "user-" + hex.EncodeToString(clusterID[:]) }

// UserKeychain is the keychain of the person running the CLI: the OS keyring, with a
// 0600 file under their own config home as the fallback (I-342).
func UserKeychain() *keychain.Store { return keychain.New(config.Home()) }

// Load reads the identity for the cluster; ok is false when the device holds none.
func Load(kc Keychain, clusterID [16]byte) (Identity, bool, error) {
	v, _, err := kc.Get(Account(clusterID))
	if errors.Is(err, keychain.ErrNotFound) {
		return Identity{}, false, nil
	}
	if err != nil {
		return Identity{}, false, fmt.Errorf("users: keychain: %w", err)
	}
	name, seedHex, ok := strings.Cut(v, ":")
	seed, derr := hex.DecodeString(seedHex)
	if !ok || derr != nil || len(seed) != ed25519.SeedSize || !meta.UserName.MatchString(name) {
		return Identity{}, false, ErrBadIdent
	}
	return Identity{Name: name, Priv: ed25519.NewKeyFromSeed(seed)}, true, nil
}

// NewIdentity draws a key for name; nothing is kept until Keep.
func NewIdentity(name string) (Identity, error) {
	if !meta.UserName.MatchString(name) {
		return Identity{}, fmt.Errorf("users: name %q: want %s", name, meta.UserName)
	}
	_, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return Identity{}, err
	}
	return Identity{Name: name, Priv: priv}, nil
}

// Keep stores id as the device's identity for the cluster, replacing any.
func Keep(kc Keychain, clusterID [16]byte, id Identity) error {
	if !meta.UserName.MatchString(id.Name) || len(id.Priv) != ed25519.PrivateKeySize {
		return ErrBadIdent
	}
	if _, _, err := kc.Set(Account(clusterID), id.Name+":"+hex.EncodeToString(id.Priv.Seed())); err != nil {
		return fmt.Errorf("users: keychain: %w", err)
	}
	return nil
}

// Create makes the identity for the cluster under name and keeps it; a device that
// already holds one refuses (one person per OS account per cluster).
func Create(kc Keychain, clusterID [16]byte, name string) (Identity, error) {
	id, err := NewIdentity(name)
	if err != nil {
		return Identity{}, err
	}
	if _, ok, err := Load(kc, clusterID); err != nil && !errors.Is(err, ErrBadIdent) {
		return Identity{}, err
	} else if ok {
		return Identity{}, ErrHasIdent
	}
	if err := Keep(kc, clusterID, id); err != nil {
		return Identity{}, err
	}
	return id, nil
}

// Forget removes the identity for the cluster from this device.
func Forget(kc Keychain, clusterID [16]byte) error {
	if err := kc.Delete(Account(clusterID)); err != nil && !errors.Is(err, keychain.ErrNotFound) {
		return fmt.Errorf("users: keychain: %w", err)
	}
	return nil
}

// CallerUID is the credential a mount on this device presents for the person running
// the CLI: their own uid, never the one sudo hides behind (I-350). On Windows, where
// the SMB account is the credential, it is -1.
func CallerUID() int { return os.Getuid() }

// SudoUser is the name of the person behind a sudo, when there is one: the default name
// for the invite `init` prints (docs/21 §1).
func SudoUser() string {
	if u := os.Getenv("SUDO_USER"); u != "" {
		return strings.ToLower(u)
	}
	return ""
}

// DefaultName is a name for the person running the CLI: the account behind a sudo, else
// the OS user name, lower-cased and cut to the pattern; "" when nothing fits.
func DefaultName(osUser string) string {
	name := SudoUser()
	if name == "" {
		name = strings.ToLower(osUser)
	}
	if i := strings.IndexAny(name, "\\@"); i >= 0 { // DOMAIN\user, user@host
		name = name[:i]
	}
	var b strings.Builder
	for _, r := range name {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') || r == '.' || r == '_' || r == '-' {
			b.WriteRune(r)
		}
	}
	name = b.String()
	if name == "" || name[0] < 'a' || name[0] > 'z' {
		return ""
	}
	if len(name) > meta.MaxUserName {
		name = name[:meta.MaxUserName]
	}
	return name
}

// BootstrapFile is the file `user bootstrap` writes under the data directory to prove it
// runs as the daemon's user on this device (docs/21 §1): a random token the node reads
// and removes.
const BootstrapFile = "bootstrap.token"

// WriteBootstrap writes a fresh token under dataDir and returns it; only a caller who may
// write the daemon's 0700 directory succeeds.
func WriteBootstrap(dataDir string) (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	tok := hex.EncodeToString(b[:])
	if err := os.WriteFile(filepath.Join(dataDir, BootstrapFile), []byte(tok), 0o600); err != nil {
		return "", err
	}
	return tok, nil
}

// TakeBootstrap reads and removes the token under dataDir; ok is false when none is
// there or it differs from want.
func TakeBootstrap(dataDir, want string) bool {
	p := filepath.Join(dataDir, BootstrapFile)
	b, err := os.ReadFile(p) //nolint:gosec // the daemon's own data directory
	_ = os.Remove(p)
	return err == nil && want != "" && string(b) == want
}

// NewCode is a one-time invite code: InviteCodeLen base32 characters (I-343).
func NewCode() (string, error) {
	const alphabet = "abcdefghijklmnopqrstuvwxyz234567"
	var b [meta.InviteCodeLen]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	for i := range b {
		b[i] = alphabet[int(b[i])%len(alphabet)]
	}
	return string(b[:]), nil
}

// UIDCredential is the binding key of a uid.
func UIDCredential(uid int) string { return "uid:" + strconv.Itoa(uid) }

// SMBCredential is the binding key of an SMB session account.
func SMBCredential(account string) string { return "smb:" + account }
