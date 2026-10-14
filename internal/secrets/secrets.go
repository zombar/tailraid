// Package secrets is the secrets store as one node sees it (docs/19): values sealed with
// AES-256-GCM under a per-name key derived from the cluster key, kept in the FSM; the
// cluster key in this device's keychain, fetched from a member when the device has none.
// Only the node a value is given to seals it, and only a node that needs a value opens it.
package secrets

import (
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/hkdf"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"sync"

	"github.com/zombar/tailraid/internal/keychain"
	"github.com/zombar/tailraid/internal/meta"
)

// KeySize is the cluster key's length.
const KeySize = 32

// TMDBName is the TMDB key's name in the store (docs/19 §6, I-308).
const TMDBName = "tmdb"

var (
	// ErrNoKey is a store whose key no reachable member holds (docs/19 §3: lost
	// everywhere, `secret rm --all` starts again).
	ErrNoKey = errors.New("secrets: no member holds the cluster key")
	// ErrNotFound is a name the store does not hold.
	ErrNotFound = errors.New("secrets: no such secret")
	// ErrValue is a value off its bounds: empty, past meta.MaxSecretValue, or with a newline.
	ErrValue = errors.New("secrets: a value is 1 to 16 KiB on one line")
	// ErrName is a name off meta.SecretName.
	ErrName = errors.New("secrets: a name is lower-case letters, digits and . _ / -, at most 128")
	// ErrOpen is a sealed value that does not open: another key, another name, or altered.
	ErrOpen = errors.New("secrets: the sealed value does not open")
)

// State is what the store reads of the FSM.
type State interface {
	Meta() meta.Meta
	SecretKeyID() meta.KeyID
	Secret(name string) (meta.Secret, bool)
	Secrets() []meta.Secret
}

// Keychain is the device's keychain (internal/keychain).
type Keychain interface {
	Get(account string) (value, source string, err error)
	Set(account, value string) (source string, keyringErr, err error)
}

// Store is one node's view of the store.
type Store struct {
	State State
	Keys  Keychain
	// Propose applies a command through the leader and waits for it locally.
	Propose func(ctx context.Context, c meta.Command) error
	// Fetch asks the members, in turn, for the cluster key (GET /v1/secrets/key).
	Fetch func(ctx context.Context) ([]byte, error)
	Self  func() meta.NodeIdx
	Now   func() int64

	mu     sync.Mutex
	key    []byte            // the cluster key, once read and checked
	opened map[string]opened // Opened's values by name
}

// opened is a value Opened decrypted, kept while its secret is unchanged.
type opened struct {
	nonce [meta.SecretNonceSize]byte
	id    meta.KeyID
	value string
}

// IDOf is the key's ID: the first 8 bytes of HMAC-SHA256(key, "tailraid key id").
func IDOf(key []byte) meta.KeyID {
	m := hmac.New(sha256.New, key)
	m.Write([]byte("tailraid key id"))
	var id meta.KeyID
	copy(id[:], m.Sum(nil))
	return id
}

// account is the keychain account of a cluster's key.
func account(cluster [16]byte) string { return "cluster-" + hex.EncodeToString(cluster[:]) }

// aead is the AES-256-GCM of one name: HKDF-SHA256(key, salt = the cluster ID, info =
// "tailraid secret v1 " + name).
func aead(key []byte, cluster [16]byte, name string) (cipher.AEAD, error) {
	k, err := hkdf.Key(sha256.New, key, cluster[:], "tailraid secret v1 "+name, 32)
	if err != nil {
		return nil, err
	}
	b, err := aes.NewCipher(k)
	if err != nil {
		return nil, err
	}
	return cipher.NewGCM(b)
}

// Seal seals value for name under key, with a random nonce and the name as additional data.
func Seal(key []byte, cluster [16]byte, name string, value []byte) (nonce [meta.SecretNonceSize]byte, sealed []byte, err error) {
	g, err := aead(key, cluster, name)
	if err != nil {
		return nonce, nil, err
	}
	n := make([]byte, g.NonceSize()) // a fresh random nonce per seal
	if _, err := rand.Read(n); err != nil {
		return nonce, nil, err
	}
	sealed = g.Seal(nil, n, value, []byte(name))
	copy(nonce[:], n)
	return nonce, sealed, nil
}

// Open opens a sealed value of name: ErrOpen under another key or name, or altered.
func Open(key []byte, cluster [16]byte, s meta.Secret) ([]byte, error) {
	g, err := aead(key, cluster, s.Name)
	if err != nil {
		return nil, err
	}
	v, err := g.Open(nil, s.Nonce[:], s.Cipher, []byte(s.Name))
	if err != nil {
		return nil, ErrOpen
	}
	return v, nil
}

// Key is the cluster key: this node's copy, else its keychain's, else a member's; each
// checked against the FSM's key ID before it is used or kept (I-304).
func (s *Store) Key(ctx context.Context) ([]byte, error) {
	want := s.State.SecretKeyID()
	if want == (meta.KeyID{}) {
		return nil, ErrNoKey
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.key != nil && IDOf(s.key) == want {
		return s.key, nil
	}
	acct := account(s.State.Meta().ClusterID)
	if v, _, err := s.Keys.Get(acct); err == nil {
		if k, err := base64.StdEncoding.DecodeString(v); err == nil && len(k) == KeySize && IDOf(k) == want {
			s.key = k
			return k, nil
		}
	}
	if s.Fetch == nil {
		return nil, ErrNoKey
	}
	k, err := s.Fetch(ctx)
	if err != nil || len(k) != KeySize || IDOf(k) != want {
		return nil, ErrNoKey
	}
	if _, _, err := s.Keys.Set(acct, base64.StdEncoding.EncodeToString(k)); err != nil {
		return nil, fmt.Errorf("secrets: keep the fetched key: %w", err)
	}
	s.key = k
	return k, nil
}

// Local is the key this node holds itself, for a member that asks (GET /v1/secrets/key):
// never fetched, and only when its ID is the FSM's (I-304).
func (s *Store) Local() ([]byte, bool) {
	want := s.State.SecretKeyID()
	if want == (meta.KeyID{}) {
		return nil, false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.key != nil && IDOf(s.key) == want {
		return s.key, true
	}
	v, _, err := s.Keys.Get(account(s.State.Meta().ClusterID))
	if err != nil {
		return nil, false
	}
	k, err := base64.StdEncoding.DecodeString(v)
	if err != nil || len(k) != KeySize || IDOf(k) != want {
		return nil, false
	}
	s.key = k
	return k, true
}

// ensureKey is the store's key, made now when the cluster has none: kept in the keychain
// first, then its ID proposed; a node that loses the race fetches the winner's (I-305).
func (s *Store) ensureKey(ctx context.Context) ([]byte, error) {
	if s.State.SecretKeyID() != (meta.KeyID{}) {
		return s.Key(ctx)
	}
	k := make([]byte, KeySize)
	if _, err := rand.Read(k); err != nil {
		return nil, err
	}
	if _, _, err := s.Keys.Set(account(s.State.Meta().ClusterID), base64.StdEncoding.EncodeToString(k)); err != nil {
		return nil, fmt.Errorf("secrets: keep the new key: %w", err)
	}
	err := s.Propose(ctx, meta.Command{Op: meta.OpSecretKeyInit, SecretKeyInit: &meta.SecretKeyInit{ID: IDOf(k), Now: s.Now()}})
	switch {
	case errors.Is(err, meta.ErrSecretKey): // another node's key won
		return s.Key(ctx)
	case err != nil:
		return nil, err
	}
	s.mu.Lock()
	s.key = k
	s.mu.Unlock()
	return k, nil
}

func checkValue(v string) error {
	if v == "" || len(v) > meta.MaxSecretValue || strings.ContainsAny(v, "\r\n") {
		return ErrValue
	}
	return nil
}

// Set seals value under name and stores it, replacing any value of that name; the
// cluster's key is made first when it has none.
func (s *Store) Set(ctx context.Context, name, value string) error {
	if !meta.SecretName.MatchString(name) {
		return ErrName
	}
	if err := checkValue(value); err != nil {
		return err
	}
	k, err := s.ensureKey(ctx)
	if err != nil {
		return err
	}
	nonce, sealed, err := Seal(k, s.State.Meta().ClusterID, name, []byte(value))
	if err != nil {
		return err
	}
	sec := meta.Secret{Name: name, KeyID: IDOf(k), Nonce: nonce, Cipher: sealed, By: s.Self()}
	return s.Propose(ctx, meta.Command{Op: meta.OpSecretPut, SecretPut: &meta.SecretPut{Secret: sec, Now: s.Now()}})
}

// Get opens the value of name.
func (s *Store) Get(ctx context.Context, name string) (string, error) {
	sec, ok := s.State.Secret(name)
	if !ok {
		return "", ErrNotFound
	}
	k, err := s.Key(ctx)
	if err != nil {
		return "", err
	}
	v, err := Open(k, s.State.Meta().ClusterID, sec)
	if err != nil {
		return "", err
	}
	return string(v), nil
}

// Entry is one secret as `secret ls` shows it: never its value.
type Entry struct {
	Name     string       `json:"name"`
	At       int64        `json:"at"`
	By       meta.NodeIdx `json:"by"`
	Readable bool         `json:"readable"` // this node can open it
}

// List is every secret, by name, and whether this node can open each.
func (s *Store) List(ctx context.Context) []Entry {
	k, kerr := s.Key(ctx)
	all := s.State.Secrets()
	out := make([]Entry, 0, len(all))
	for _, sec := range all {
		e := Entry{Name: sec.Name, At: sec.At, By: sec.By}
		if kerr == nil {
			if full, ok := s.State.Secret(sec.Name); ok {
				_, err := Open(k, s.State.Meta().ClusterID, full)
				e.Readable = err == nil
			}
		}
		out = append(out, e)
	}
	return out
}

// Remove removes name, or with all every secret and the key ID (a fresh key comes with
// the next Set).
func (s *Store) Remove(ctx context.Context, name string, all bool) error {
	if !all && !meta.SecretName.MatchString(name) {
		return ErrName
	}
	err := s.Propose(ctx, meta.Command{Op: meta.OpSecretDelete, SecretDelete: &meta.SecretDelete{Name: name, All: all, Now: s.Now()}})
	if errors.Is(err, meta.ErrNotExist) {
		return ErrNotFound
	}
	if err == nil && all {
		s.mu.Lock()
		s.key = nil
		s.mu.Unlock()
	}
	return err
}

// Opened is every secret under prefix this node opens now, by name. It never fetches the
// key, so a hot path (a bucket's credentials on every shard request) never waits on a
// member, and opens a secret again only when it changed (I-308).
func (s *Store) Opened(prefix string) map[string]string {
	out := map[string]string{}
	k, ok := s.Local()
	if !ok {
		return out
	}
	cluster := s.State.Meta().ClusterID
	s.mu.Lock()
	defer s.mu.Unlock()
	next := map[string]opened{}
	for name, o := range s.opened {
		if !strings.HasPrefix(name, prefix) {
			next[name] = o // another prefix's: kept as it was
		}
	}
	for _, sec := range s.State.Secrets() {
		if !strings.HasPrefix(sec.Name, prefix) {
			continue
		}
		if o, ok := s.opened[sec.Name]; ok && o.nonce == sec.Nonce && o.id == sec.KeyID {
			next[sec.Name], out[sec.Name] = o, o.value
			continue
		}
		full, ok := s.State.Secret(sec.Name)
		if !ok {
			continue
		}
		v, err := Open(k, cluster, full)
		if err != nil {
			continue
		}
		next[sec.Name] = opened{nonce: full.Nonce, id: full.KeyID, value: string(v)}
		out[sec.Name] = string(v)
	}
	s.opened = next
	return out
}

// keychainOK is the compile-time check that keychain.Store satisfies Keychain.
var _ Keychain = (*keychain.Store)(nil)
