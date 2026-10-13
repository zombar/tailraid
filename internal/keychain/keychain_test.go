package keychain

import (
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/zalando/go-keyring"
	"go.uber.org/goleak"

	"github.com/zombar/tailraid/internal/clock"
)

func TestMain(m *testing.M) { goleak.VerifyTestMain(m) }

// unixModes is an OS with permission bits; Windows guards the file by its user profile's
// ACL instead, and reports every file 0666 (docs/19 §2: "0600 where the OS has modes").
var unixModes = runtime.GOOS != "windows"

// fakeRing is a keyring that holds values, fails every call, or blocks each call until
// released.
type fakeRing struct {
	mu      sync.Mutex
	vals    map[string]string
	fail    bool
	release chan struct{} // non-nil: every call waits on it
}

func (r *fakeRing) wait() error {
	if r.release != nil {
		<-r.release
	}
	if r.fail {
		return errors.New("no keyring here")
	}
	return nil
}

func (r *fakeRing) Get(s, u string) (string, error) {
	if err := r.wait(); err != nil {
		return "", err
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	v, ok := r.vals[s+"/"+u]
	if !ok {
		return "", keyring.ErrNotFound
	}
	return v, nil
}

func (r *fakeRing) Set(s, u, p string) error {
	if err := r.wait(); err != nil {
		return err
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.vals[s+"/"+u] = p
	return nil
}

func (r *fakeRing) Delete(s, u string) error {
	if err := r.wait(); err != nil {
		return err
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, ok := r.vals[s+"/"+u]; !ok {
		return keyring.ErrNotFound
	}
	delete(r.vals, s+"/"+u)
	return nil
}

// never is a clock whose timers never fire: a keyring that answers always wins.
type never struct{ clock.Clock }

func (never) After(time.Duration) <-chan time.Time { return nil }

// instant is a clock whose timers fire at once: a keyring that blocks always loses.
type instant struct{ clock.Clock }

func (instant) After(time.Duration) <-chan time.Time {
	c := make(chan time.Time, 1)
	c <- time.Time{}
	return c
}

func store(t *testing.T, r *fakeRing, c clock.Clock) *Store {
	t.Helper()
	if r.vals == nil {
		r.vals = map[string]string{}
	}
	return &Store{Dir: filepath.Join(t.TempDir(), "keys"), Ring: r, Clock: c}
}

// A keyring that answers holds the value; no file is written.
// Invariant: I-309
func TestKeychain_KeyringFirst(t *testing.T) {
	r := &fakeRing{}
	s := store(t, r, never{clock.Real()})
	src, kerr, err := s.Set("cluster-abc", "c2VjcmV0")
	if err != nil || kerr != nil || src != SourceKeyring {
		t.Fatalf("set: %q %v %v", src, kerr, err)
	}
	if _, err := os.Stat(s.file("cluster-abc")); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("a file was written beside the keyring: %v", err)
	}
	if v, src, err := s.Get("cluster-abc"); v != "c2VjcmV0" || src != SourceKeyring || err != nil {
		t.Errorf("get: %q %q %v", v, src, err)
	}
	if err := s.Delete("cluster-abc"); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.Get("cluster-abc"); !errors.Is(err, ErrNotFound) {
		t.Errorf("after delete: %v", err)
	}
	if err := s.Delete("cluster-abc"); !errors.Is(err, ErrNotFound) {
		t.Errorf("deleting nothing: %v", err)
	}
}

// A keyring that fails, or does not answer in time, never fails the caller: the value
// goes to a 0600 file under a 0700 directory, and Source names it.
// Invariant: I-309
func TestKeychain_FallbackFile_Table(t *testing.T) {
	for _, tc := range []struct {
		name  string
		ring  *fakeRing
		clock clock.Clock
	}{
		{"keyring fails", &fakeRing{fail: true}, never{clock.Real()}},
		{"keyring hangs", &fakeRing{release: make(chan struct{})}, instant{clock.Real()}},
	} {
		s := store(t, tc.ring, tc.clock)
		src, kerr, err := s.Set("tmdb", "abc123")
		if err != nil || kerr == nil || src != s.file("tmdb") {
			t.Fatalf("%s: set %q %v %v", tc.name, src, kerr, err)
		}
		fi, err := os.Stat(s.file("tmdb"))
		if err != nil || unixModes && fi.Mode().Perm() != 0o600 {
			t.Errorf("%s: file %v %v", tc.name, fi, err)
		}
		if di, err := os.Stat(s.Dir); err != nil || unixModes && di.Mode().Perm() != 0o700 {
			t.Errorf("%s: dir %v %v", tc.name, di, err)
		}
		if v, src, err := s.Get("tmdb"); v != "abc123" || src != s.file("tmdb") || err != nil {
			t.Errorf("%s: get %q %q %v", tc.name, v, src, err)
		}
		if got, ok := s.Source("tmdb"); !ok || got != s.file("tmdb") {
			t.Errorf("%s: source %q %v", tc.name, got, ok)
		}
		if err := s.Delete("tmdb"); err != nil {
			t.Errorf("%s: delete %v", tc.name, err)
		}
		if tc.ring.release != nil {
			close(tc.ring.release) // let the abandoned calls end
		}
	}
}

// A keyring that answers again takes the value back, leaving no stale file.
// Invariant: I-309
func TestKeychain_KeyringRecovers(t *testing.T) {
	r := &fakeRing{fail: true}
	s := store(t, r, never{clock.Real()})
	if _, _, err := s.Set("tmdb", "old"); err != nil {
		t.Fatal(err)
	}
	r.fail = false
	if src, _, err := s.Set("tmdb", "new"); err != nil || src != SourceKeyring {
		t.Fatalf("set: %q %v", src, err)
	}
	if _, err := os.Stat(s.file("tmdb")); !errors.Is(err, os.ErrNotExist) {
		t.Error("the stale file was left behind")
	}
	if v, _, _ := s.Get("tmdb"); v != "new" {
		t.Errorf("get %q", v)
	}
}

// An account is never a path, and a value is one non-empty line.
// Invariant: I-309
func TestKeychain_Names_Table(t *testing.T) {
	s := store(t, &fakeRing{}, never{clock.Real()})
	for _, tc := range []struct {
		name string
		ok   bool
	}{
		{"cluster-0123abcd", true},
		{"s3.b2", true},
		{"a", true},
		{strings.Repeat("a", 128), true},
		{strings.Repeat("a", 129), false},
		{"", false},
		{"../etc", false},
		{"a/b", false},
		{"Tmdb", false},
		{".hidden", false},
	} {
		_, _, err := s.Set(tc.name, "v")
		if (err == nil) != tc.ok || !tc.ok && !errors.Is(err, ErrAccount) {
			t.Errorf("Set(%q): %v, want ok=%v", tc.name, err, tc.ok)
		}
	}
	for _, v := range []string{"", "a\nb", "a\r"} {
		if _, _, err := s.Set("tmdb", v); err == nil {
			t.Errorf("value %q accepted", v)
		}
	}
}

// The OS keyring is go-keyring's: the mock provider sees the service and the account.
func TestKeychain_GoKeyring(t *testing.T) {
	keyring.MockInit()
	s := &Store{Dir: t.TempDir(), Ring: osRing{}, Clock: never{clock.Real()}}
	if src, _, err := s.Set("tmdb", "k"); err != nil || src != SourceKeyring {
		t.Fatalf("set %q %v", src, err)
	}
	if v, err := keyring.Get(Service, "tmdb"); v != "k" || err != nil {
		t.Errorf("go-keyring holds %q %v", v, err)
	}
	if d := t.TempDir(); New(d).Dir != filepath.Join(d, "keys") {
		t.Error("the fallback dir is not <data_dir>/keys")
	}
}
