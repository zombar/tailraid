// Package keychain keeps a device's secrets (docs/19 §2, I-309): the OS keyring first (the
// macOS Keychain through /usr/bin/security, the Windows Credential Manager, the Linux
// Secret Service; go-keyring, pure Go), and a 0600 file under a 0700 directory when the
// keyring fails or does not answer in time. A keyring failure never fails the caller.
// The layer is errol's (zombar/errol_v2 internal/keys).
package keychain

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/zalando/go-keyring"

	"github.com/zombar/tailraid/internal/clock"
	"github.com/zombar/tailraid/internal/sysfs"
)

// Service is the keyring service every tailraid account lives under.
const Service = "tailraid"

// Timeout bounds one keyring call: a locked macOS Keychain can wait on a prompt nobody
// sees, and a missing Secret Service can stall on D-Bus. Past it the call counts as a
// keyring failure and the file is used.
const Timeout = 5 * time.Second

// SourceKeyring names a value the OS keyring holds; a value in the fallback file is named
// by the file's path.
const SourceKeyring = "keyring"

var (
	// ErrNotFound is an account no store holds.
	ErrNotFound = errors.New("keychain: not found")
	// ErrAccount is an account name off its pattern.
	ErrAccount = errors.New("keychain: bad account name")
)

// account names are what makes the file fallback's path safe: never a path of their own.
var account = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]{0,127}$`)

// Ring is the OS keyring: go-keyring's three calls, a seam for the tests.
type Ring interface {
	Get(service, user string) (string, error)
	Set(service, user, password string) error
	Delete(service, user string) error
}

type osRing struct{}

func (osRing) Get(s, u string) (string, error) { return keyring.Get(s, u) }
func (osRing) Set(s, u, p string) error        { return keyring.Set(s, u, p) }
func (osRing) Delete(s, u string) error        { return keyring.Delete(s, u) }

// Store is one device's keychain.
type Store struct {
	Dir   string // the fallback files' directory: <data_dir>/keys
	Ring  Ring
	Clock clock.Clock
}

// New is the device's keychain: the OS keyring, with <dataDir>/keys as the fallback.
func New(dataDir string) *Store {
	return &Store{Dir: filepath.Join(dataDir, "keys"), Ring: osRing{}, Clock: clock.Real()}
}

func (s *Store) file(name string) string { return filepath.Join(s.Dir, name+".key") }

func check(name string) error {
	if !account.MatchString(name) {
		return fmt.Errorf("%w: %q", ErrAccount, name)
	}
	return nil
}

var ringMu sync.Mutex

// ring runs one keyring call within Timeout; a call still running then is abandoned (its
// goroutine ends when the OS tool does) and counts as a failure.
func (s *Store) ring(call func() (string, error)) (string, error) {
	type result struct {
		v   string
		err error
	}
	done := make(chan result, 1)
	go func() {
		// the keyring is one resource of the process, and a CLI command's identity read
		// may run beside the daemon's own writes in one test binary (the mock provider's
		// map is not safe to share): one call at a time
		ringMu.Lock()
		v, err := call()
		ringMu.Unlock()
		done <- result{v, err}
	}()
	select {
	case r := <-done:
		return r.v, r.err
	case <-s.Clock.After(Timeout):
		return "", errors.New("keychain: the keyring did not answer in time")
	}
}

// Get is the account's value and where it was: the keyring, else the fallback file.
func (s *Store) Get(name string) (value, source string, err error) {
	if err := check(name); err != nil {
		return "", "", err
	}
	if v, err := s.ring(func() (string, error) { return s.Ring.Get(Service, name) }); err == nil && v != "" {
		return v, SourceKeyring, nil
	}
	b, err := os.ReadFile(s.file(name)) //nolint:gosec // a checked account name under our own dir
	if errors.Is(err, os.ErrNotExist) {
		return "", "", ErrNotFound
	}
	if err != nil {
		return "", "", fmt.Errorf("keychain: read %s: %w", s.file(name), err)
	}
	return strings.TrimSpace(string(b)), s.file(name), nil
}

// Set keeps value under the account: in the keyring when it answers (and then no stale
// file is left behind), else in the fallback file. It reports where it landed and the
// keyring's error, if the file had to be used; only a failure of both is an error.
func (s *Store) Set(name, value string) (source string, keyringErr, err error) {
	if err := check(name); err != nil {
		return "", nil, err
	}
	if value == "" || strings.ContainsAny(value, "\r\n") {
		return "", nil, errors.New("keychain: a value is one non-empty line")
	}
	_, kerr := s.ring(func() (string, error) { return "", s.Ring.Set(Service, name, value) })
	if kerr == nil {
		_ = os.Remove(s.file(name))
		return SourceKeyring, nil, nil
	}
	if err := os.MkdirAll(s.Dir, 0o700); err != nil {
		return "", kerr, err
	}
	if err := sysfs.WriteFileSync(s.file(name), []byte(value+"\n")); err != nil { // 0600
		return "", kerr, fmt.Errorf("keychain: write %s: %w", s.file(name), err)
	}
	return s.file(name), kerr, nil
}

// Delete removes the account from the keyring and the file; ErrNotFound when neither held it.
func (s *Store) Delete(name string) error {
	if err := check(name); err != nil {
		return err
	}
	_, kerr := s.ring(func() (string, error) { return "", s.Ring.Delete(Service, name) })
	ferr := os.Remove(s.file(name))
	if kerr != nil && errors.Is(ferr, os.ErrNotExist) {
		return ErrNotFound
	}
	if ferr != nil && !errors.Is(ferr, os.ErrNotExist) {
		return ferr
	}
	return nil
}

// Source is where the account's value is, false when no store holds it.
func (s *Store) Source(name string) (string, bool) {
	_, src, err := s.Get(name)
	return src, err == nil
}
