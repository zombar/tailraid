package meta

import (
	"errors"
	"regexp"
	"slices"
)

// The secrets store's state (docs/19 §4): sealed values by name, and the ID of the one
// cluster key they are sealed under. A value is never here in the clear: the node that
// was given it seals it before proposing (I-306).

// Bounds of the store (I-307).
const (
	MaxSecrets      = 256
	MaxSecretValue  = 16 << 10 // a value's bytes; the ciphertext adds the GCM tag
	SecretTagSize   = 16
	SecretNonceSize = 12
)

// SecretName is a secret's name: never a path, and short.
var SecretName = regexp.MustCompile(`^[a-z0-9][a-z0-9._/-]{0,127}$`)

// KeyID identifies the cluster key (docs/19 §3): never the key.
type KeyID [8]byte

// Secret is one sealed value.
type Secret struct {
	Name   string
	KeyID  KeyID
	Nonce  [SecretNonceSize]byte
	Cipher []byte // AES-256-GCM sealed value with its tag
	At     int64
	By     NodeIdx
}

// SecretKeyInit records the cluster key's ID, once (op 23, I-305).
type SecretKeyInit struct {
	ID  KeyID
	Now int64
}

// SecretPut stores a sealed value under Name, replacing any (op 24).
type SecretPut struct {
	Secret Secret
	Now    int64
}

// SecretDelete removes one secret, or with All every secret and the key ID (op 25).
type SecretDelete struct {
	Name string
	All  bool
	Now  int64
}

func (a *applier) secretKeyInit(c *SecretKeyInit) error {
	if c == nil || c.ID == (KeyID{}) {
		return ErrBadCommand
	}
	if a.st.SecretKeyID != (KeyID{}) {
		return ErrSecretKey // one key per cluster: the loser fetches the winner's (I-305)
	}
	a.st.SecretKeyID = c.ID
	return nil
}

func (a *applier) secretPut(c *SecretPut) error {
	if c == nil {
		return ErrBadCommand
	}
	s := c.Secret
	if !SecretName.MatchString(s.Name) || len(s.Cipher) <= SecretTagSize || len(s.Cipher) > MaxSecretValue+SecretTagSize {
		return ErrBadCommand
	}
	if a.st.SecretKeyID == (KeyID{}) || s.KeyID != a.st.SecretKeyID {
		return ErrSecretKey
	}
	if _, ok := a.st.Secrets[s.Name]; !ok && len(a.st.Secrets) >= MaxSecrets {
		return ErrSecretFull
	}
	s.Cipher = slices.Clone(s.Cipher)
	s.At = c.Now
	a.st.Secrets[s.Name] = &s
	return nil
}

func (a *applier) secretDelete(c *SecretDelete) error {
	if c == nil {
		return ErrBadCommand
	}
	if c.All {
		clear(a.st.Secrets)
		a.st.SecretKeyID = KeyID{}
		return nil
	}
	if _, ok := a.st.Secrets[c.Name]; !ok {
		return ErrNotExist
	}
	delete(a.st.Secrets, c.Name)
	return nil
}

// SecretKeyID is the cluster key's ID, zero while the cluster has none.
func (f *FSM) SecretKeyID() KeyID {
	f.mu.RLock()
	defer f.mu.RUnlock()
	return f.st.SecretKeyID
}

// Secret is a copy of one sealed secret.
func (f *FSM) Secret(name string) (Secret, bool) {
	f.mu.RLock()
	defer f.mu.RUnlock()
	s, ok := f.st.Secrets[name]
	if !ok {
		return Secret{}, false
	}
	cp := *s
	cp.Cipher = slices.Clone(s.Cipher)
	return cp, true
}

// Secrets lists every secret by name, sealed values left out.
func (f *FSM) Secrets() []Secret {
	f.mu.RLock()
	defer f.mu.RUnlock()
	out := make([]Secret, 0, len(f.st.Secrets))
	for _, s := range f.st.Secrets {
		cp := *s
		cp.Cipher = nil
		out = append(out, cp)
	}
	slices.SortFunc(out, func(a, b Secret) int {
		switch {
		case a.Name < b.Name:
			return -1
		case a.Name > b.Name:
			return 1
		}
		return 0
	})
	return out
}

func writeSecret(w *writer, s *Secret) {
	w.str(s.Name)
	w.b = append(w.b, s.KeyID[:]...)
	w.b = append(w.b, s.Nonce[:]...)
	w.bytes(s.Cipher)
	w.i64(s.At)
	w.u16(uint16(s.By))
}

func readSecret(r *reader) Secret {
	var s Secret
	s.Name = r.str()
	copy(s.KeyID[:], r.take(len(s.KeyID)))
	copy(s.Nonce[:], r.take(len(s.Nonce)))
	s.Cipher = r.bytes()
	s.At = r.i64()
	s.By = NodeIdx(r.u16())
	return s
}

// writeSecrets writes the key ID and the secrets in name order (snapshot frame 11).
func writeSecrets(w *writer, st *State) {
	w.b = append(w.b, st.SecretKeyID[:]...)
	names := make([]string, 0, len(st.Secrets))
	for n := range st.Secrets {
		names = append(names, n)
	}
	slices.Sort(names)
	w.uvarint(uint64(len(names)))
	for _, n := range names {
		writeSecret(w, st.Secrets[n])
	}
}

func readSecrets(r *reader, st *State) {
	copy(st.SecretKeyID[:], r.take(len(st.SecretKeyID)))
	n := r.uvarint()
	if r.err == nil && n > MaxSecrets {
		r.fail(errors.New("meta: too many secrets"))
		return
	}
	prev := ""
	for i := uint64(0); i < n && r.err == nil; i++ {
		s := readSecret(r)
		if r.err == nil && (s.Name <= prev || !SecretName.MatchString(s.Name)) {
			r.fail(errors.New("meta: secrets not in canonical order"))
			return
		}
		prev = s.Name
		st.Secrets[s.Name] = &s
	}
}
