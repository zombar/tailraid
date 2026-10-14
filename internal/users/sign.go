package users

import (
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"net/netip"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/zombar/tailraid/internal/meta"
)

// The signature a mutating request carries (docs/21 §2, I-344), in the shape of RFC 9421
// HTTP Message Signatures: one header naming the user, the key, a timestamp and a nonce,
// and an Ed25519 signature over the cluster ID, the target node ID, the method, the path,
// the body's SHA-256, the timestamp and the nonce.

// Header is the request header's name.
const Header = "Tailraid-Sig"

// Defaults of the verifier (docs/21 §2, I-359).
const (
	DefaultSkew = 5 * time.Minute
	// NonceCache bounds the nonces a node remembers; at one signed request a second it
	// covers far more than twice the skew.
	NonceCache = 4096
	// MaxFailures consecutive failures on one key from one source disable that source for
	// LockoutFor (NIST SP 800-63B's rate limit of a verifier).
	MaxFailures = 100
	LockoutFor  = time.Minute
)

// Signature errors; the verifier answers ErrUnauthorized for every bad signature, with
// no hint which part failed.
var (
	ErrUnauthorized = errors.New("users: unauthorized")
	ErrBadSignature = errors.New("users: malformed signature header")
)

// Signature is one request's signature.
type Signature struct {
	User  string
	Key   meta.UserKeyID
	TS    int64 // unix seconds
	Nonce [16]byte
	Sig   []byte
}

// signedBytes is what is signed: every part length-prefixed, so no two inputs share bytes.
func signedBytes(cluster [16]byte, nodeID, method, path string, bodyHash [32]byte, ts int64, nonce [16]byte) []byte {
	var b []byte
	put := func(p []byte) {
		b = binary.LittleEndian.AppendUint32(b, uint32(len(p))) //nolint:gosec // bounded by the request's own size
		b = append(b, p...)
	}
	b = append(b, "tailraid-sig v1"...)
	put(cluster[:])
	put([]byte(nodeID))
	put([]byte(method))
	put([]byte(path))
	put(bodyHash[:])
	b = binary.LittleEndian.AppendUint64(b, uint64(ts)) //nolint:gosec // a unix time
	put(nonce[:])
	return b
}

// Sign signs a request for id, bound to the cluster and the node it is sent to.
func Sign(id Identity, cluster [16]byte, nodeID, method, path string, body []byte, ts int64, nonce [16]byte) Signature {
	msg := signedBytes(cluster, nodeID, method, path, sha256.Sum256(body), ts, nonce)
	return Signature{User: id.Name, Key: id.KeyID(), TS: ts, Nonce: nonce, Sig: ed25519.Sign(id.Priv, msg)}
}

// NewNonce draws a nonce.
func NewNonce() ([16]byte, error) {
	var n [16]byte
	_, err := rand.Read(n[:])
	return n, err
}

// String is the header's value: `user=<name> key=<hex> ts=<unix> nonce=<hex> sig=<base64>`.
func (s Signature) String() string {
	return fmt.Sprintf("user=%s key=%s ts=%d nonce=%s sig=%s", s.User, hex.EncodeToString(s.Key[:]), s.TS, hex.EncodeToString(s.Nonce[:]), base64.StdEncoding.EncodeToString(s.Sig))
}

// Parse reads a header's value.
func Parse(v string) (Signature, error) {
	var s Signature
	seen := map[string]bool{}
	for f := range strings.FieldsSeq(v) {
		k, val, ok := strings.Cut(f, "=")
		if !ok || seen[k] {
			return Signature{}, ErrBadSignature
		}
		seen[k] = true
		var err error
		switch k {
		case "user":
			if !meta.UserName.MatchString(val) {
				return Signature{}, ErrBadSignature
			}
			s.User = val
		case "key":
			var b []byte
			if b, err = hex.DecodeString(val); err != nil || len(b) != len(s.Key) {
				return Signature{}, ErrBadSignature
			}
			copy(s.Key[:], b)
		case "ts":
			if s.TS, err = strconv.ParseInt(val, 10, 64); err != nil {
				return Signature{}, ErrBadSignature
			}
		case "nonce":
			var b []byte
			if b, err = hex.DecodeString(val); err != nil || len(b) != len(s.Nonce) {
				return Signature{}, ErrBadSignature
			}
			copy(s.Nonce[:], b)
		case "sig":
			if s.Sig, err = base64.StdEncoding.DecodeString(val); err != nil || len(s.Sig) != ed25519.SignatureSize {
				return Signature{}, ErrBadSignature
			}
		default:
			return Signature{}, ErrBadSignature
		}
	}
	if len(seen) != 5 {
		return Signature{}, ErrBadSignature
	}
	return s, nil
}

// Users is what the verifier reads: the live user of a name.
type Users interface {
	UserByName(name string) (meta.User, bool)
}

// Verifier checks signatures for one node (docs/21 §2, I-344): the user must be active,
// the key theirs, the timestamp within Skew of Now, the nonce unseen within twice the
// skew, and the signature valid over this node's ID. A source that fails MaxFailures
// times in a row on one key is refused for LockoutFor.
type Verifier struct {
	Users   Users
	Cluster [16]byte
	NodeID  string
	Skew    time.Duration
	Now     func() time.Time

	mu       sync.Mutex
	nonces   map[[16]byte]time.Time
	failures map[failKey]*failure
}

type failKey struct {
	key meta.UserKeyID
	src netip.Addr
}

type failure struct {
	count int
	until time.Time
}

// Verify checks the header value hdr for a request from src; it answers the user the
// signature names. Every refusal is ErrUnauthorized.
func (v *Verifier) Verify(hdr string, src netip.Addr, method, path string, body []byte) (meta.UserIdx, meta.UserKeyID, error) {
	sig, err := Parse(hdr)
	if err != nil {
		return 0, meta.UserKeyID{}, ErrUnauthorized
	}
	now := v.Now()
	skew := v.Skew
	if skew <= 0 {
		skew = DefaultSkew
	}
	v.mu.Lock()
	defer v.mu.Unlock()
	fk := failKey{sig.Key, src}
	if f := v.failures[fk]; f != nil && now.Before(f.until) {
		return 0, meta.UserKeyID{}, ErrUnauthorized
	}
	fail := func() (meta.UserIdx, meta.UserKeyID, error) {
		if v.failures == nil {
			v.failures = map[failKey]*failure{}
		}
		f := v.failures[fk]
		if f == nil {
			f = &failure{}
			v.failures[fk] = f
		}
		f.count++
		if f.count >= MaxFailures {
			f.until = now.Add(LockoutFor)
			f.count = 0
		}
		return 0, meta.UserKeyID{}, ErrUnauthorized
	}
	u, ok := v.Users.UserByName(sig.User)
	if !ok || u.State != meta.UserActive {
		return fail()
	}
	var pub []byte
	for _, k := range u.Keys {
		if k.ID() == sig.Key {
			pub = k.Pub[:]
		}
	}
	if pub == nil {
		return fail()
	}
	ts := time.Unix(sig.TS, 0)
	if d := now.Sub(ts); d > skew || d < -skew {
		return fail()
	}
	v.sweepNonces(now, 2*skew)
	if _, seen := v.nonces[sig.Nonce]; seen {
		return fail()
	}
	if !ed25519.Verify(pub, signedBytes(v.Cluster, v.NodeID, method, path, sha256.Sum256(body), sig.TS, sig.Nonce), sig.Sig) {
		return fail()
	}
	if v.nonces == nil {
		v.nonces = map[[16]byte]time.Time{}
	}
	v.nonces[sig.Nonce] = now
	delete(v.failures, fk)
	return u.Idx, sig.Key, nil
}

// sweepNonces forgets nonces older than keep, and the oldest beyond NonceCache.
func (v *Verifier) sweepNonces(now time.Time, keep time.Duration) {
	for n, at := range v.nonces {
		if now.Sub(at) > keep {
			delete(v.nonces, n)
		}
	}
	for len(v.nonces) >= NonceCache {
		var oldest [16]byte
		var oldestAt time.Time
		first := true
		for n, at := range v.nonces {
			if first || at.Before(oldestAt) {
				oldest, oldestAt, first = n, at, false
			}
		}
		delete(v.nonces, oldest)
	}
}
