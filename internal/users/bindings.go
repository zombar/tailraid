package users

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"

	"github.com/zombar/tailraid/internal/meta"
	"github.com/zombar/tailraid/internal/sysfs"
)

// Bindings map a device's local credentials to users (docs/21 §5, I-350): the AUTH_SYS
// uid of an NFS call (`uid:<n>`) and the account of an SMB session (`smb:<account>`).
// The map is the device's alone, in <data_dir>/bindings.json (0600), changed only by
// signed operations on that node. An unbound credential is nobody.
type Bindings struct {
	path string
	mu   sync.RWMutex
	m    map[string]string // credential → user name
}

// BindingsFile is the map's file under the data directory.
const BindingsFile = "bindings.json"

// ErrBadCredential is a credential off its two forms.
var ErrBadCredential = errors.New("users: a credential is uid:<n> or smb:<account>")

// OpenBindings reads the device's map, empty when the file is absent.
func OpenBindings(dataDir string) (*Bindings, error) {
	b := &Bindings{path: filepath.Join(dataDir, BindingsFile), m: map[string]string{}}
	data, err := os.ReadFile(b.path) //nolint:gosec // the daemon's own data directory
	if errors.Is(err, os.ErrNotExist) {
		return b, nil
	}
	if err != nil {
		return nil, err
	}
	if err := json.Unmarshal(data, &b.m); err != nil {
		return nil, fmt.Errorf("users: %s: %w", b.path, err)
	}
	return b, nil
}

// ValidCredential reports whether cred is one of the two forms.
func ValidCredential(cred string) bool {
	if n, ok := strings.CutPrefix(cred, "uid:"); ok {
		return n != "" && strings.Trim(n, "0123456789") == ""
	}
	if a, ok := strings.CutPrefix(cred, "smb:"); ok {
		return a != "" && !strings.ContainsAny(a, " \t\r\n")
	}
	return false
}

// Bind maps cred to user and writes the file.
func (b *Bindings) Bind(cred, user string) error {
	if !ValidCredential(cred) {
		return ErrBadCredential
	}
	if !meta.UserName.MatchString(user) {
		return fmt.Errorf("users: name %q: want %s", user, meta.UserName)
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	b.m[cred] = user
	return b.saveLocked()
}

// Unbind forgets cred; a credential that was not bound is not an error.
func (b *Bindings) Unbind(cred string) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	if _, ok := b.m[cred]; !ok {
		return nil
	}
	delete(b.m, cred)
	return b.saveLocked()
}

// Lookup is the user bound to cred, if any.
func (b *Bindings) Lookup(cred string) (string, bool) {
	b.mu.RLock()
	defer b.mu.RUnlock()
	u, ok := b.m[cred]
	return u, ok
}

// Binding is one row of the map.
type Binding struct {
	Credential string `json:"credential"`
	User       string `json:"user"`
}

// List is every binding, by credential.
func (b *Bindings) List() []Binding {
	b.mu.RLock()
	defer b.mu.RUnlock()
	out := make([]Binding, 0, len(b.m))
	for c, u := range b.m {
		out = append(out, Binding{Credential: c, User: u})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Credential < out[j].Credential })
	return out
}

// saveLocked writes the map atomically, 0600.
func (b *Bindings) saveLocked() error {
	data, err := json.MarshalIndent(b.m, "", "  ")
	if err != nil {
		return err
	}
	return sysfs.WriteFileSync(b.path, data)
}
