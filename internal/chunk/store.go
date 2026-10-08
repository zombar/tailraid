package chunk

import (
	"bytes"
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"fmt"
	"hash/fnv"
	"io"
	"io/fs"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/gurgeh/tailraid/internal/sysfs"
)

// On-disk shard file: magic | payloadLen u32 LE | sha256(payload) | payload.
const (
	magic      = "TRS1"
	HeaderSize = 4 + 4 + 32
	// MaxPayload bounds payloadLen so a corrupt header cannot drive a huge allocation.
	MaxPayload = 64 << 20
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
	capacity int64
	used     atomic.Int64
	locks    [64]sync.Mutex // striped per-key locks: Put/Delete/quarantine of one key serialize
	qmu      sync.Mutex
}

// Open prepares root/shards and root/quarantine, deletes every leftover temp file regardless
// of age (one process owns a data dir, so nothing can be mid-write at open), and seeds Used
// from disk.
// capacity 0 means unlimited.
func Open(fsys sysfs.FS, root string, capacity int64) (*Store, error) {
	s := &Store{fsys: fsys, root: root, capacity: capacity}
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

func (s *Store) lock(k ShardKey) *sync.Mutex {
	h := fnv.New32a()
	_, _ = h.Write([]byte(k))
	return &s.locks[h.Sum32()%uint32(len(s.locks))]
}

// Used returns the bytes of visible shard files; Capacity the configured budget (0 = none).
func (s *Store) Used() int64     { return s.used.Load() }
func (s *Store) Capacity() int64 { return s.capacity }

// Has reports whether a shard file is visible under its final name.
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
	if _, _, _, _, err := ParseKey(k); err != nil {
		return 0, err
	}
	mu := s.lock(k)
	mu.Lock()
	defer mu.Unlock()

	final := s.path(k)
	size := int64(HeaderSize + len(payload))
	sum := sha256.Sum256(payload)
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
	if s.capacity > 0 && s.used.Load()-oldSize+size > s.capacity {
		return 0, fmt.Errorf("%w: used %d + %d > %d", ErrFull, s.used.Load(), size, s.capacity)
	}
	dir := filepath.Dir(final)
	if err := s.fsys.MkdirAll(dir, 0o755); err != nil {
		return 0, fmt.Errorf("chunk: mkdir %s: %w", dir, err)
	}
	f, err := s.fsys.CreateTemp(dir, tmpPrefix+"*")
	if err != nil {
		return 0, fmt.Errorf("chunk: create temp for %s: %w", k, err)
	}
	tmp := f.Name()
	cleanup := func() { _ = f.Close(); _ = s.fsys.Remove(tmp) }

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
		return 0, fmt.Errorf("chunk: close %s: %w", k, err)
	}
	if err := s.fsys.Rename(tmp, final); err != nil {
		_ = s.fsys.Remove(tmp)
		return 0, fmt.Errorf("chunk: rename %s: %w", k, err)
	}
	s.used.Add(size - oldSize)
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

// DeleteIfOlder is Delete guarded by age, for deletes requested over the network (GC and
// repair): a shard whose mtime is younger than minAge may belong to an in-flight flush and
// is refused (deleted=false, no error). The local sweep uses plain Delete with its own
// grace check.
func (s *Store) DeleteIfOlder(k ShardKey, minAge time.Duration, now time.Time) (deleted bool, size int64, err error) {
	if _, _, _, _, err := ParseKey(k); err != nil {
		return false, 0, err
	}
	mu := s.lock(k)
	mu.Lock()
	defer mu.Unlock()
	fi, err := s.fsys.Stat(s.path(k))
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return false, 0, nil
		}
		return false, 0, fmt.Errorf("chunk: stat %s: %w", k, err)
	}
	if fi.ModTime().After(now.Add(-minAge)) {
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
	raw, err := s.readFile(s.path(k))
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, ErrNotFound
		}
		return nil, fmt.Errorf("chunk: read %s: %w", k, err)
	}
	payload, verr := verify(raw)
	if verr != nil {
		s.quarantine(k)
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
	mu := s.lock(k)
	mu.Lock()
	defer mu.Unlock()
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
			return nil // foreign file; not ours to report
		}
		fi, err := de.Info()
		if err != nil {
			return err
		}
		return fn(ShardKey(name), fi.Size(), fi.ModTime())
	})
}

func (s *Store) walkFiles(fn func(dir string, de fs.DirEntry) error) error {
	subs, err := s.fsys.ReadDir(s.shardsDir())
	if err != nil {
		return err
	}
	for _, sub := range subs {
		if !sub.IsDir() {
			continue
		}
		dir := filepath.Join(s.shardsDir(), sub.Name())
		ents, err := s.fsys.ReadDir(dir)
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
	return io.ReadAll(io.LimitReader(f, HeaderSize+MaxPayload+1))
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

// quarantine moves a corrupt shard out of the serving path, keeping at most QuarantineMax
// files for inspection (oldest removed first). Never deletes silently.
func (s *Store) quarantine(k ShardKey) {
	mu := s.lock(k)
	mu.Lock()
	defer mu.Unlock()
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
		_ = s.fsys.Remove(src) // last resort: a corrupt file must not stay serveable
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
	i := strings.LastIndexByte(name, '.')
	if i < 0 {
		return 0
	}
	var n int64
	for _, c := range name[i+1:] {
		if c < '0' || c > '9' {
			return 0
		}
		n = n*10 + int64(c-'0')
	}
	return n
}
