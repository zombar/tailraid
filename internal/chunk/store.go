package chunk

import (
	"bytes"
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/zombar/tailraid/internal/meta"

	"github.com/zombar/tailraid/internal/sysfs"
)

// On-disk shard file: magic | payloadLen u32 LE | sha256(payload) | payload.
const (
	magic      = "TRS1"
	HeaderSize = 4 + 4 + 32
	// MaxPayload bounds payloadLen so a corrupt header cannot drive a huge allocation; it
	// is the volume's maximum chunk size (I-67), so a k=1 shard of the largest chunk fits.
	MaxPayload = meta.MaxChunkSize
	tmpPrefix  = ".tmp-"
	// QuarantineMax caps the number of corrupt files kept for forensics.
	QuarantineMax = 64
)

var (
	ErrNotFound = errors.New("chunk: shard not found")
	ErrCorrupt  = errors.New("chunk: shard corrupt")
	ErrFull     = errors.New("chunk: store capacity exceeded")
)

// Store is a node's shard directory. All methods are safe for concurrent use.
type Store struct {
	fsys     sysfs.FS
	root     string
	capacity atomic.Int64 // 0 = unlimited; follows the derived budget (Rebudget)
	used     atomic.Int64
	lmu      sync.Mutex
	locks    map[ShardKey]*keyLock // Put/Delete/quarantine of one key serialize; other keys never wait
	dirs     sync.Map              // prefix directories known to exist (created once per process)
	qmu      sync.Mutex
}

type keyLock struct {
	mu   sync.Mutex
	refs int
}

// Open prepares root/shards and root/quarantine, deletes every leftover temp file regardless
// of age (one process owns a data dir, so nothing can be mid-write at open), and seeds Used
// from disk.
// capacity 0 means unlimited.
func Open(fsys sysfs.FS, root string, capacity int64) (*Store, error) {
	s := &Store{fsys: fsys, root: root, locks: map[ShardKey]*keyLock{}}
	s.capacity.Store(capacity)
	for _, d := range []string{s.shardsDir(), s.quarantineDir()} {
		if err := fsys.MkdirAll(d, 0o755); err != nil {
			return nil, fmt.Errorf("chunk: open %s: %w", d, err)
		}
	}
	var used int64
	err := s.walkFiles(func(dir string, de fs.DirEntry) error {
		name := de.Name()
		if strings.HasPrefix(name, tmpPrefix) {
			return fsys.Remove(filepath.Join(dir, name))
		}
		fi, err := de.Info()
		if err != nil {
			return err
		}
		used += fi.Size()
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("chunk: scan %s: %w", root, err)
	}
	s.used.Store(used)
	return s, nil
}

func (s *Store) shardsDir() string     { return filepath.Join(s.root, "shards") }
func (s *Store) quarantineDir() string { return filepath.Join(s.root, "quarantine") }

func (s *Store) path(k ShardKey) string {
	return filepath.Join(s.shardsDir(), string(k[:2]), string(k))
}

// lock serializes operations on one key and returns the unlock. Keys never share a lock,
// so a PUT's two fsyncs never hold up a PUT of another shard.
func (s *Store) lock(k ShardKey) func() {
	s.lmu.Lock()
	l := s.locks[k]
	if l == nil {
		l = &keyLock{}
		s.locks[k] = l
	}
	l.refs++
	s.lmu.Unlock()
	l.mu.Lock()
	return func() {
		l.mu.Unlock()
		s.lmu.Lock()
		l.refs--
		if l.refs == 0 {
			delete(s.locks, k)
		}
		s.lmu.Unlock()
	}
}

// mkdirOnce creates a prefix directory the first time a shard lands in it.
func (s *Store) mkdirOnce(dir string) error {
	if _, ok := s.dirs.Load(dir); ok {
		return nil
	}
	if err := s.fsys.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	s.dirs.Store(dir, struct{}{})
	return nil
}

// Used returns the bytes of visible shard files; Capacity the configured budget (0 = none).
func (s *Store) Used() int64     { return s.used.Load() }
func (s *Store) Capacity() int64 { return s.capacity.Load() }

// SetCapacity changes the budget; a PUT already past the new cap is not undone, the
// next one is refused (ErrFull) until the sweep or a delete makes room.
func (s *Store) SetCapacity(n int64) { s.capacity.Store(max(n, 0)) }

// Has reports whether a shard file is visible under its final name.
// Path returns the on-disk path of a shard key (used by scrub tooling and tests).
func (s *Store) Path(k ShardKey) string { return s.path(k) }

func (s *Store) Has(k ShardKey) bool {
	if _, _, _, _, err := ParseKey(k); err != nil {
		return false
	}
	fi, err := s.fsys.Stat(s.path(k))
	return err == nil && !fi.IsDir()
}

// Put stores payload under k durably: temp file in the shard's directory → header+payload
// → fsync → close → rename → directory fsync. A shard already present is not rewritten; its
// mtime is refreshed (sweep grace) and added is 0. Over capacity returns ErrFull before any
// byte is written. On any failure no partial file remains visible.
func (s *Store) Put(k ShardKey, payload []byte) (added int64, err error) {
	return s.PutVerified(k, payload, sha256.Sum256(payload))
}

// PutVerified is Put for a caller that already holds sha256(payload): the RPC server
// verified it against the sender's header, the local client computed it for that header.
// The shard is hashed once per hop; a wrong sum makes every later Get report ErrCorrupt.
func (s *Store) PutVerified(k ShardKey, payload []byte, sum [32]byte) (added int64, err error) {
	if _, _, _, _, err := ParseKey(k); err != nil {
		return 0, err
	}
	if len(payload) > MaxPayload {
		return 0, fmt.Errorf("chunk: payload of %d bytes exceeds MaxPayload %d", len(payload), MaxPayload)
	}
	defer s.lock(k)()

	final := s.path(k)
	size := int64(HeaderSize + len(payload))
	var oldSize int64 // size of an existing file with this key that will be replaced
	if fi, err := s.fsys.Stat(final); err == nil && !fi.IsDir() {
		if hdr, err := s.readHeader(final); err == nil && bytes.Equal(hdr[8:40], sum[:]) {
			now := time.Now()
			if err := s.fsys.Chtimes(final, now, now); err != nil {
				return 0, fmt.Errorf("chunk: refresh %s: %w", k, err)
			}
			return 0, nil
		}
		oldSize = fi.Size() // different or unreadable content: replace atomically below
	}
	// Reserve the bytes before writing, atomically against concurrent Puts of other keys
	// (the per-key lock does not order those): the store never exceeds its capacity
	// (L-138). The reservation is released on any failure below.
	if capacity := s.capacity.Load(); capacity > 0 {
		for {
			cur := s.used.Load()
			if cur-oldSize+size > capacity {
				return 0, fmt.Errorf("%w: used %d + %d > %d", ErrFull, cur, size, capacity)
			}
			if s.used.CompareAndSwap(cur, cur+size) {
				break
			}
		}
	} else {
		s.used.Add(size)
	}
	release := func() { s.used.Add(-size) }
	dir := filepath.Dir(final)
	if err := s.mkdirOnce(dir); err != nil {
		release()
		return 0, fmt.Errorf("chunk: mkdir %s: %w", dir, err)
	}
	f, err := s.fsys.CreateTemp(dir, tmpPrefix+"*")
	if err != nil {
		release()
		return 0, fmt.Errorf("chunk: create temp for %s: %w", k, err)
	}
	tmp := f.Name()
	cleanup := func() { _ = f.Close(); _ = s.fsys.Remove(tmp); release() }

	var hdr [HeaderSize]byte
	copy(hdr[:4], magic)
	binary.LittleEndian.PutUint32(hdr[4:8], uint32(len(payload)))
	copy(hdr[8:40], sum[:])
	if _, err := f.Write(hdr[:]); err != nil {
		cleanup()
		return 0, fmt.Errorf("chunk: write header %s: %w", k, err)
	}
	if _, err := f.Write(payload); err != nil {
		cleanup()
		return 0, fmt.Errorf("chunk: write payload %s: %w", k, err)
	}
	if err := f.Sync(); err != nil {
		cleanup()
		return 0, fmt.Errorf("chunk: fsync %s: %w", k, err)
	}
	if err := f.Close(); err != nil {
		_ = s.fsys.Remove(tmp)
		release()
		return 0, fmt.Errorf("chunk: close %s: %w", k, err)
	}
	if err := s.fsys.Rename(tmp, final); err != nil {
		_ = s.fsys.Remove(tmp)
		release()
		return 0, fmt.Errorf("chunk: rename %s: %w", k, err)
	}
	s.used.Add(-oldSize) // the new bytes were reserved above; the replaced file is gone
	// The rename is only durable once the directory is synced; that is part of the ack.
	// If the sync fails the shard may or may not survive a crash, but it is complete and
	// verifiable either way, so the caller is told it is not durable and may retry.
	if err := s.fsys.SyncDir(dir); err != nil {
		return size - oldSize, fmt.Errorf("chunk: sync dir %s: %w", dir, err)
	}
	return size - oldSize, nil
}

// readHeader returns the first HeaderSize bytes of p.
func (s *Store) readHeader(p string) ([]byte, error) {
	f, err := s.fsys.Open(p)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	hdr := make([]byte, HeaderSize)
	if _, err := io.ReadFull(f, hdr); err != nil {
		return nil, err
	}
	return hdr, nil
}

// DeleteIfOlder is Delete guarded by age, for every delete that reasoned about the shard
// earlier (GC, repair and the local sweep): a shard whose mtime is younger than minAge may
// belong to an in-flight flush and is refused (deleted=false, no error). The age is read
// under the key lock, so a Put that refreshed the shard in the meantime wins (I-14).
func (s *Store) DeleteIfOlder(k ShardKey, minAge time.Duration, now time.Time) (deleted bool, size int64, err error) {
	if _, _, _, _, err := ParseKey(k); err != nil {
		return false, 0, err
	}
	defer s.lock(k)()
	fi, err := s.fsys.Stat(s.path(k))
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return false, 0, nil
		}
		return false, 0, fmt.Errorf("chunk: stat %s: %w", k, err)
	}
	if !fi.ModTime().Before(now.Add(-minAge)) { // exactly minAge old is still young (docs/06)
		return false, 0, nil
	}
	size, err = s.deleteLocked(k)
	return err == nil && size > 0, size, err
}

// Get returns the verified payload of k. A file whose header, length or hash does not
// match is moved to quarantine and ErrCorrupt is returned; it is never served.
func (s *Store) Get(k ShardKey) ([]byte, error) {
	if _, _, _, _, err := ParseKey(k); err != nil {
		return nil, err
	}
	// read, verify and quarantine under the key lock: a Put that replaces the file
	// between the read and the quarantine would otherwise lose its good shard
	defer s.lock(k)()
	raw, err := s.readFile(s.path(k))
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, ErrNotFound
		}
		return nil, fmt.Errorf("chunk: read %s: %w", k, err)
	}
	payload, verr := verify(raw)
	if verr != nil {
		s.quarantineLocked(k)
		return nil, fmt.Errorf("%w: %s: %w", ErrCorrupt, k, verr)
	}
	return payload, nil
}

// Open is Get for streaming callers (the RPC server): the shard is fully verified first.
func (s *Store) Open(k ShardKey) (io.ReadCloser, int64, error) {
	p, err := s.Get(k)
	if err != nil {
		return nil, 0, err
	}
	return io.NopCloser(bytes.NewReader(p)), int64(len(p)), nil
}

// Delete removes k and returns the bytes freed; a missing shard is not an error.
func (s *Store) Delete(k ShardKey) (int64, error) {
	if _, _, _, _, err := ParseKey(k); err != nil {
		return 0, err
	}
	defer s.lock(k)()
	return s.deleteLocked(k)
}

func (s *Store) deleteLocked(k ShardKey) (int64, error) {
	p := s.path(k)
	fi, err := s.fsys.Stat(p)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return 0, nil
		}
		return 0, fmt.Errorf("chunk: stat %s: %w", k, err)
	}
	if err := s.fsys.Remove(p); err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return 0, nil
		}
		return 0, fmt.Errorf("chunk: remove %s: %w", k, err)
	}
	s.used.Add(-fi.Size())
	return fi.Size(), nil
}

// Walk calls fn for every visible shard (temp files excluded) until fn returns an error.
func (s *Store) Walk(fn func(k ShardKey, size int64, mtime time.Time) error) error {
	return s.walkFiles(func(dir string, de fs.DirEntry) error {
		name := de.Name()
		if strings.HasPrefix(name, tmpPrefix) {
			return nil
		}
		if _, _, _, _, err := ParseKey(ShardKey(name)); err != nil {
			return nil //nolint:nilerr // foreign file; not ours to report
		}
		fi, err := de.Info()
		if err != nil {
			return err
		}
		return fn(ShardKey(name), fi.Size(), fi.ModTime())
	})
}

func (s *Store) walkFiles(fn func(dir string, de fs.DirEntry) error) error {
	return walkTwoLevel(s.fsys, s.shardsDir(), fn)
}

// walkTwoLevel calls fn for every file under root/<prefix>/ (the shard store's and the
// cache's layout), stopping at fn's first error.
func walkTwoLevel(fsys sysfs.FS, root string, fn func(dir string, de fs.DirEntry) error) error {
	subs, err := fsys.ReadDir(root)
	if err != nil {
		return err
	}
	for _, sub := range subs {
		if !sub.IsDir() {
			continue
		}
		dir := filepath.Join(root, sub.Name())
		ents, err := fsys.ReadDir(dir)
		if err != nil {
			return err
		}
		for _, de := range ents {
			if de.IsDir() {
				continue
			}
			if err := fn(dir, de); err != nil {
				return err
			}
		}
	}
	return nil
}

func (s *Store) readFile(p string) ([]byte, error) {
	f, err := s.fsys.Open(p)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	const limit = HeaderSize + MaxPayload + 1
	if fi, err := f.Stat(); err == nil && fi.Size() <= limit { // one allocation of the known size
		buf := make([]byte, fi.Size())
		n, err := io.ReadFull(f, buf)
		if err != nil && !errors.Is(err, io.ErrUnexpectedEOF) {
			return nil, err
		}
		return buf[:n], nil
	}
	return io.ReadAll(io.LimitReader(f, limit))
}

// verify checks the TRS1 header against the payload and returns the payload.
func verify(raw []byte) ([]byte, error) {
	if len(raw) < HeaderSize {
		return nil, errors.New("short header")
	}
	if string(raw[:4]) != magic {
		return nil, errors.New("bad magic")
	}
	n := binary.LittleEndian.Uint32(raw[4:8])
	if n > MaxPayload || int(n) != len(raw)-HeaderSize {
		return nil, fmt.Errorf("length %d does not match file size %d", n, len(raw)-HeaderSize)
	}
	payload := raw[HeaderSize:]
	sum := sha256.Sum256(payload)
	if !bytes.Equal(sum[:], raw[8:40]) {
		return nil, errors.New("payload hash mismatch")
	}
	return payload, nil
}

// quarantineLocked moves a corrupt shard out of the serving path, keeping at most
// QuarantineMax files for inspection (oldest removed first). Never deletes silently. The
// caller holds the key lock. The budget is credited only for a file that actually left.
func (s *Store) quarantineLocked(k ShardKey) {
	src := s.path(k)
	fi, err := s.fsys.Stat(src)
	if err != nil {
		return // already gone (concurrent quarantine)
	}
	size := fi.Size() // what actually leaves disk; a corruption may have changed the length
	s.qmu.Lock()
	defer s.qmu.Unlock()
	dst := filepath.Join(s.quarantineDir(), fmt.Sprintf("%s.%d", k, time.Now().UnixNano()))
	if err := s.fsys.Rename(src, dst); err != nil {
		if rerr := s.fsys.Remove(src); rerr != nil && !errors.Is(rerr, fs.ErrNotExist) { // last resort: a corrupt file must not stay serveable
			return // still on disk: the budget still holds it
		}
	}
	s.used.Add(-size)
	ents, err := s.fsys.ReadDir(s.quarantineDir())
	if err != nil || len(ents) <= QuarantineMax {
		return
	}
	sort.Slice(ents, func(i, j int) bool { return ents[i].Name() < ents[j].Name() })
	// Names end in the unix-nano timestamp; sort by that to find the oldest.
	sort.SliceStable(ents, func(i, j int) bool {
		return suffixNano(ents[i].Name()) < suffixNano(ents[j].Name())
	})
	for _, de := range ents[:len(ents)-QuarantineMax] {
		_ = s.fsys.Remove(filepath.Join(s.quarantineDir(), de.Name()))
	}
}

func suffixNano(name string) int64 {
	_, after, ok := strings.CutLast(name, ".")
	if !ok {
		return 0
	}
	var n int64
	for _, c := range after {
		if c < '0' || c > '9' {
			return 0
		}
		n = n*10 + int64(c-'0')
	}
	return n
}
