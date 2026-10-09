package chunk

import (
	"bytes"
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"math/rand"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/zombar/tailraid/internal/sysfs"
	"github.com/zombar/tailraid/internal/sysfs/sysfstest"
)

func payload(n int, seed int64) []byte {
	b := make([]byte, n)
	rand.New(rand.NewSource(seed)).Read(b)
	return b
}

func keyFor(p []byte, idx int) ShardKey { return Key(sha256.Sum256(p), 2, 1, idx) }

func openStore(t *testing.T, fsys sysfs.FS, root string, capacity int64) *Store {
	t.Helper()
	s, err := Open(fsys, root, capacity)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	return s
}

func shardPath(root string, k ShardKey) string {
	return filepath.Join(root, "shards", string(k[:2]), string(k))
}

func quarantined(t *testing.T, root string, k ShardKey) bool {
	t.Helper()
	m, _ := filepath.Glob(filepath.Join(root, "quarantine", string(k)+".*"))
	return len(m) > 0
}

func diskUsed(t *testing.T, root string) int64 {
	t.Helper()
	var used int64
	_ = filepath.Walk(filepath.Join(root, "shards"), func(p string, fi os.FileInfo, err error) error {
		if err == nil && !fi.IsDir() && !strings.HasPrefix(fi.Name(), ".tmp-") {
			used += fi.Size()
		}
		return nil
	})
	return used
}

// The store never writes a payload larger than the volume's maximum chunk size, and
// never trusts a header that claims one.
// Invariant: I-11, I-67
func TestStore_PayloadBoundaries(t *testing.T) {
	for _, n := range []int{MaxPayload - 1, MaxPayload, MaxPayload + 1} {
		t.Run(fmt.Sprint(n), func(t *testing.T) {
			s := openStore(t, sysfs.OS{}, t.TempDir(), 0)
			p := make([]byte, n)
			p[n/2] = 7
			k := keyFor(p, 1)
			_, err := s.Put(k, p)
			if n > MaxPayload {
				if err == nil || s.Has(k) {
					t.Fatalf("payload of %d accepted", n)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			got, err := s.Get(k)
			if err != nil || len(got) != n || got[n/2] != 7 {
				t.Fatalf("round trip of %d bytes: len %d err %v", n, len(got), err)
			}
		})
	}
}

// Invariant: I-11
// Lifecycle: L-125, L-126, L-128
func TestStore_PutGetRoundTrip(t *testing.T) {
	const mib = 1 << 20
	for _, n := range []int{0, 1, 4095, 4096, mib, mib + 1} {
		t.Run(fmt.Sprint(n), func(t *testing.T) {
			root := t.TempDir()
			s := openStore(t, sysfs.OS{}, root, 0)
			p := payload(n, int64(n))
			k := keyFor(p, 2)
			added, err := s.Put(k, p)
			if err != nil {
				t.Fatal(err)
			}
			if added != int64(HeaderSize+n) {
				t.Fatalf("added = %d, want %d", added, HeaderSize+n)
			}
			if !s.Has(k) {
				t.Fatal("Has false after Put")
			}
			got, err := s.Get(k)
			if err != nil || !bytes.Equal(got, p) {
				t.Fatalf("Get: %v (len %d)", err, len(got))
			}
			rc, size, err := s.Open(k)
			if err != nil || size != int64(n) {
				t.Fatalf("Open: %v size %d", err, size)
			}
			all, _ := io.ReadAll(rc)
			_ = rc.Close()
			if !bytes.Equal(all, p) {
				t.Fatal("Open content mismatch")
			}
			if s.Used() != added {
				t.Fatalf("Used = %d, want %d", s.Used(), added)
			}
			temps, _ := filepath.Glob(filepath.Join(root, "shards", "*", ".tmp-*"))
			if len(temps) != 0 {
				t.Fatalf("temp files left: %v", temps)
			}
			// on-disk format: magic, len, hash, payload
			raw, _ := os.ReadFile(shardPath(root, k))
			if string(raw[:4]) != "TRS1" || binary.LittleEndian.Uint32(raw[4:8]) != uint32(n) {
				t.Fatalf("bad header %x", raw[:8])
			}
			h := sha256.Sum256(p)
			if !bytes.Equal(raw[8:40], h[:]) || !bytes.Equal(raw[40:], p) {
				t.Fatal("header hash or payload mismatch on disk")
			}
		})
	}
}

func TestStore_GetMissingIsErrNotFound(t *testing.T) {
	s := openStore(t, sysfs.OS{}, t.TempDir(), 0)
	if _, err := s.Get(keyFor([]byte("x"), 0)); !errors.Is(err, ErrNotFound) {
		t.Fatalf("err = %v", err)
	}
	if s.Has(keyFor([]byte("x"), 0)) {
		t.Fatal("Has true for missing")
	}
}

func corruptCases() map[string]func(raw []byte) []byte {
	return map[string]func(raw []byte) []byte{
		"BitFlip":   func(r []byte) []byte { r[HeaderSize+10] ^= 0x01; return r },
		"Truncated": func(r []byte) []byte { return r[:len(r)-7] },
		"ZeroFilled": func(r []byte) []byte {
			for i := range r {
				r[i] = 0
			}
			return r
		},
		"BadMagic":   func(r []byte) []byte { r[0] = 'X'; return r },
		"HeaderHash": func(r []byte) []byte { r[8] ^= 0xff; return r },
		"LenTooBig":  func(r []byte) []byte { binary.LittleEndian.PutUint32(r[4:8], 1<<30); return r },
		"Empty":      func(r []byte) []byte { return nil },
	}
}

// Invariant: I-11
// Lifecycle: L-129, L-154, L-107, L-108
func TestStore_GetVerifiesHash_Corrupted(t *testing.T) {
	for name, mut := range corruptCases() {
		t.Run(name, func(t *testing.T) {
			root := t.TempDir()
			s := openStore(t, sysfs.OS{}, root, 0)
			p := payload(5000, 7)
			k := keyFor(p, 0)
			if _, err := s.Put(k, p); err != nil {
				t.Fatal(err)
			}
			raw, _ := os.ReadFile(shardPath(root, k))
			if err := os.WriteFile(shardPath(root, k), mut(raw), 0o644); err != nil {
				t.Fatal(err)
			}
			got, err := s.Get(k)
			if !errors.Is(err, ErrCorrupt) {
				t.Fatalf("err = %v, want ErrCorrupt", err)
			}
			if got != nil {
				t.Fatal("corrupt shard returned bytes")
			}
			if _, err := os.Stat(shardPath(root, k)); !errors.Is(err, os.ErrNotExist) {
				t.Fatal("corrupt file still visible at its shard path")
			}
			if !quarantined(t, root, k) {
				t.Fatal("corrupt file was not moved to quarantine")
			}
			if s.Has(k) {
				t.Fatal("Has true after quarantine")
			}
			// Same-size corruption keeps accounting exact; a size-changing corruption
			// (truncation, emptying) can only be reconciled by the rescan on Open.
			if name != "Truncated" && name != "Empty" && s.Used() != 0 {
				t.Fatalf("Used = %d after quarantine, want 0", s.Used())
			}
			if re := openStore(t, sysfs.OS{}, root, 0); re.Used() != diskUsed(t, root) {
				t.Fatalf("after reopen Used %d != disk %d", re.Used(), diskUsed(t, root))
			}
			// Open must behave identically (a second corrupt file).
			if _, err := s.Put(k, p); err != nil {
				t.Fatal(err)
			}
			raw, _ = os.ReadFile(shardPath(root, k))
			_ = os.WriteFile(shardPath(root, k), mut(raw), 0o644)
			if _, _, err := s.Open(k); !errors.Is(err, ErrCorrupt) {
				t.Fatalf("Open err = %v, want ErrCorrupt", err)
			}
		})
	}
}

func TestStore_QuarantineCapped(t *testing.T) {
	root := t.TempDir()
	s := openStore(t, sysfs.OS{}, root, 0)
	p := payload(100, 1)
	k := keyFor(p, 0)
	for range QuarantineMax + 10 {
		_, _ = s.Put(k, p)
		raw, _ := os.ReadFile(shardPath(root, k))
		raw[HeaderSize] ^= 1
		_ = os.WriteFile(shardPath(root, k), raw, 0o644)
		if _, err := s.Get(k); !errors.Is(err, ErrCorrupt) {
			t.Fatal(err)
		}
	}
	m, _ := filepath.Glob(filepath.Join(root, "quarantine", "*"))
	if len(m) > QuarantineMax {
		t.Fatalf("quarantine has %d entries, cap %d", len(m), QuarantineMax)
	}
}

// Lifecycle: L-142
func TestStore_TempNeverServed(t *testing.T) {
	root := t.TempDir()
	s := openStore(t, sysfs.OS{}, root, 0)
	p := payload(10, 2)
	k := keyFor(p, 0)
	dir := filepath.Join(root, "shards", string(k[:2]))
	_ = os.MkdirAll(dir, 0o755)
	tmp := filepath.Join(dir, ".tmp-abc")
	_ = os.WriteFile(tmp, p, 0o644)
	if _, err := s.Get(k); !errors.Is(err, ErrNotFound) {
		t.Fatalf("Get err = %v", err)
	}
	if s.Has(ShardKey(".tmp-abc")) {
		t.Fatal("Has served a temp name")
	}
	// Lifecycle: L-127 — Open removes temps of any age.
	fresh := filepath.Join(dir, ".tmp-fresh")
	_ = os.WriteFile(fresh, p, 0o644)
	openStore(t, sysfs.OS{}, root, 0)
	for _, f := range []string{tmp, fresh} {
		if _, err := os.Stat(f); err == nil {
			t.Fatalf("temp %s survived Open", f)
		}
	}
	var seen []ShardKey
	_ = s.Walk(func(k ShardKey, _ int64, _ time.Time) error { seen = append(seen, k); return nil })
	if len(seen) != 0 {
		t.Fatalf("Walk listed temp: %v", seen)
	}
}

// Invariant: I-11
// Lifecycle: L-127, L-120
func TestStore_CrashBeforeRename_LeavesNoVisibleShard(t *testing.T) {
	root := t.TempDir()
	fl := sysfstest.New(sysfs.OS{})
	s := openStore(t, fl, root, 0)
	p := payload(3000, 3)
	k := keyFor(p, 1)
	for _, step := range []sysfstest.Step{sysfstest.StepWrite, sysfstest.StepSync, sysfstest.StepClose, sysfstest.StepRename} {
		fl.CrashAt(step)
		func() {
			defer func() {
				if r := recover(); r != nil && !sysfstest.IsCrash(r) {
					panic(r)
				}
			}()
			_, _ = s.Put(k, p)
		}()
		fl.Restart()
		s2 := openStore(t, fl, root, 0) // restart: sweeps temps
		if s2.Has(k) {
			t.Fatalf("crash at %v: shard visible", step)
		}
		if _, err := s2.Get(k); !errors.Is(err, ErrNotFound) {
			t.Fatalf("crash at %v: Get err = %v", step, err)
		}
		temps, _ := filepath.Glob(filepath.Join(root, "shards", "*", ".tmp-*"))
		if len(temps) != 0 {
			t.Fatalf("crash at %v: temps not swept on Open: %v", step, temps)
		}
		if s2.Used() != 0 {
			t.Fatalf("crash at %v: Used = %d", step, s2.Used())
		}
		s = s2
	}
}

// Invariant: I-11
// Lifecycle: L-126, L-117
func TestStore_CrashAfterRenameBeforeDirSync_ShardStillValid(t *testing.T) {
	root := t.TempDir()
	fl := sysfstest.New(sysfs.OS{})
	s := openStore(t, fl, root, 0)
	p := payload(3000, 4)
	k := keyFor(p, 1)
	fl.CrashAt(sysfstest.StepSyncDir)
	func() {
		defer func() {
			if r := recover(); r != nil && !sysfstest.IsCrash(r) {
				panic(r)
			}
		}()
		_, _ = s.Put(k, p)
	}()
	fl.Restart()
	s2 := openStore(t, fl, root, 0)
	// After a crash between rename and directory sync the entry may or may not have
	// survived; either way nothing corrupt is ever served and accounting matches disk.
	got, err := s2.Get(k)
	switch {
	case err == nil:
		if !bytes.Equal(got, p) {
			t.Fatal("served bytes differ from payload")
		}
	case errors.Is(err, ErrNotFound):
	default:
		t.Fatalf("shard after crash-after-rename: %v (must be valid or missing)", err)
	}
	if s2.Used() != diskUsed(t, root) {
		t.Fatalf("Used %d != disk %d", s2.Used(), diskUsed(t, root))
	}
}

// Invariant: I-11
// Lifecycle: L-136
func TestStore_PutDifferentPayloadSameKey_Replaces(t *testing.T) {
	root := t.TempDir()
	s := openStore(t, sysfs.OS{}, root, 0)
	a := payload(1000, 1)
	b := payload(1500, 2) // the key names a's hash; a repair could legitimately overwrite
	k := keyFor(a, 0)
	if _, err := s.Put(k, a); err != nil {
		t.Fatal(err)
	}
	added, err := s.Put(k, b)
	if err != nil {
		t.Fatal(err)
	}
	if added != int64(len(b)-len(a)) {
		t.Fatalf("added = %d, want delta %d", added, len(b)-len(a))
	}
	got, err := s.Get(k)
	if err != nil || !bytes.Equal(got, b) {
		t.Fatalf("Get after replace: %v", err)
	}
	if s.Used() != diskUsed(t, root) {
		t.Fatalf("Used %d != disk %d", s.Used(), diskUsed(t, root))
	}
	temps, _ := filepath.Glob(filepath.Join(root, "shards", "*", ".tmp-*"))
	if len(temps) != 0 {
		t.Fatalf("temps: %v", temps)
	}
	// Replacing the file whose header was corrupted also works (header unreadable → replace).
	raw, _ := os.ReadFile(shardPath(root, k))
	_ = os.WriteFile(shardPath(root, k), raw[:3], 0o644)
	if _, err := s.Put(k, a); err != nil {
		t.Fatal(err)
	}
	if got, err := s.Get(k); err != nil || !bytes.Equal(got, a) {
		t.Fatalf("Get after replacing short file: %v", err)
	}
	// Bytes removed behind the store's back are only reconciled by the rescan on Open.
	if re := openStore(t, sysfs.OS{}, root, 0); re.Used() != diskUsed(t, root) {
		t.Fatalf("after reopen Used %d != disk %d", re.Used(), diskUsed(t, root))
	}
}

// Invariant: I-14
// Lifecycle: L-145
func TestStore_DeleteIfOlder_RefusesYoungShard(t *testing.T) {
	root := t.TempDir()
	s := openStore(t, sysfs.OS{}, root, 0)
	p := payload(100, 1)
	k := keyFor(p, 0)
	_, _ = s.Put(k, p)
	now := time.Now()
	if del, n, err := s.DeleteIfOlder(k, 10*time.Minute, now); err != nil || del || n != 0 {
		t.Fatalf("young shard: deleted=%v n=%d err=%v", del, n, err)
	}
	if !s.Has(k) {
		t.Fatal("young shard removed")
	}
	old := now.Add(-time.Hour)
	_ = os.Chtimes(shardPath(root, k), old, old)
	if del, n, err := s.DeleteIfOlder(k, 10*time.Minute, now); err != nil || !del || n != int64(HeaderSize+100) {
		t.Fatalf("old shard: deleted=%v n=%d err=%v", del, n, err)
	}
	if del, _, err := s.DeleteIfOlder(k, 10*time.Minute, now); err != nil || del {
		t.Fatalf("missing shard: deleted=%v err=%v", del, err)
	}
	if s.Used() != 0 {
		t.Fatalf("Used = %d", s.Used())
	}
}

// Invariant: I-11
// Lifecycle: L-127, L-129, L-108
func TestStore_TornWrite_DetectedOnRead(t *testing.T) {
	root := t.TempDir()
	fl := sysfstest.New(sysfs.OS{})
	s := openStore(t, fl, root, 0)
	p := payload(3000, 5)
	k := keyFor(p, 2)
	// Crash at Sync with only part of the file on disk, then (as a hostile filesystem would)
	// pretend the rename happened anyway by renaming the torn temp into place.
	fl.TornWriteAt(sysfstest.StepSync, HeaderSize+1000)
	func() {
		defer func() {
			if r := recover(); r != nil && !sysfstest.IsCrash(r) {
				panic(r)
			}
		}()
		_, _ = s.Put(k, p)
	}()
	fl.Restart()
	temps, _ := filepath.Glob(filepath.Join(root, "shards", "*", ".tmp-*"))
	if len(temps) != 1 {
		t.Fatalf("temps = %v", temps)
	}
	if err := os.Rename(temps[0], shardPath(root, k)); err != nil {
		t.Fatal(err)
	}
	s2 := openStore(t, fl, root, 0)
	got, err := s2.Get(k)
	if !errors.Is(err, ErrCorrupt) || got != nil {
		t.Fatalf("torn shard: got %d bytes, err %v; want ErrCorrupt", len(got), err)
	}
	if !quarantined(t, root, k) {
		t.Fatal("torn shard not quarantined")
	}
}

// Lifecycle: L-126, L-136
func TestStore_ConcurrentPutSameHash(t *testing.T) {
	root := t.TempDir()
	s := openStore(t, sysfs.OS{}, root, 0)
	p := payload(50_000, 6)
	k := keyFor(p, 0)
	var wg sync.WaitGroup
	var total int64
	var mu sync.Mutex
	for range 32 {
		wg.Go(func() {
			added, err := s.Put(k, p)
			if err != nil {
				t.Error(err)
			}
			mu.Lock()
			total += added
			mu.Unlock()
		})
	}
	wg.Wait()
	if total != int64(HeaderSize+len(p)) {
		t.Fatalf("sum of added = %d, want exactly one file's worth %d", total, HeaderSize+len(p))
	}
	if s.Used() != diskUsed(t, root) {
		t.Fatalf("Used %d != disk %d", s.Used(), diskUsed(t, root))
	}
	got, err := s.Get(k)
	if err != nil || !bytes.Equal(got, p) {
		t.Fatal(err)
	}
	temps, _ := filepath.Glob(filepath.Join(root, "shards", "*", ".tmp-*"))
	if len(temps) != 0 {
		t.Fatalf("temps left: %v", temps)
	}
}

// Lifecycle: L-136
func TestStore_DupPutRefreshesMtime(t *testing.T) {
	root := t.TempDir()
	s := openStore(t, sysfs.OS{}, root, 0)
	p := payload(100, 8)
	k := keyFor(p, 0)
	if _, err := s.Put(k, p); err != nil {
		t.Fatal(err)
	}
	old := time.Now().Add(-48 * time.Hour)
	_ = os.Chtimes(shardPath(root, k), old, old)
	added, err := s.Put(k, p)
	if err != nil || added != 0 {
		t.Fatalf("dup put: added %d err %v", added, err)
	}
	st, _ := os.Stat(shardPath(root, k))
	if st.ModTime().Before(time.Now().Add(-time.Hour)) {
		t.Fatalf("mtime not refreshed: %v", st.ModTime())
	}
}

// Lifecycle: L-123
func TestStore_DiskFull_ReturnsErrAndNoPartialShard(t *testing.T) {
	root := t.TempDir()
	fl := sysfstest.New(sysfs.OS{})
	s := openStore(t, fl, root, 0)
	p := payload(10_000, 9)
	k := keyFor(p, 0)
	fl.WriteErrAfter(HeaderSize+2000, syscall.ENOSPC)
	if _, err := s.Put(k, p); !errors.Is(err, syscall.ENOSPC) {
		t.Fatalf("err = %v, want ENOSPC", err)
	}
	if s.Has(k) {
		t.Fatal("partial shard visible")
	}
	temps, _ := filepath.Glob(filepath.Join(root, "shards", "*", ".tmp-*"))
	if len(temps) != 0 {
		t.Fatalf("partial temp left behind: %v", temps)
	}
	if s.Used() != 0 {
		t.Fatalf("Used = %d", s.Used())
	}
	// FailAt on each step also leaves nothing behind.
	for _, step := range []sysfstest.Step{sysfstest.StepCreateTemp, sysfstest.StepWrite, sysfstest.StepSync, sysfstest.StepClose, sysfstest.StepRename} {
		fl.FailAt(step, errors.New("io"))
		if _, err := s.Put(k, p); err == nil {
			t.Fatalf("step %v: expected error", step)
		}
		if s.Has(k) || s.Used() != 0 {
			t.Fatalf("step %v: partial state", step)
		}
		temps, _ := filepath.Glob(filepath.Join(root, "shards", "*", ".tmp-*"))
		if len(temps) != 0 {
			t.Fatalf("step %v: temps left: %v", step, temps)
		}
	}
}

// Lifecycle: L-138
// Concurrent Puts of distinct keys near the capacity never push the store over it: the
// reservation is atomic across keys, exactly as many succeed as fit, the rest get ErrFull
// and leave nothing behind.
// Lifecycle: L-138
func TestStore_ConcurrentPutsNearCapacity_NeverExceed(t *testing.T) {
	const fit = 5
	size := int64(HeaderSize + 100)
	root := t.TempDir()
	s := openStore(t, sysfs.OS{}, root, fit*size)
	const writers = 24
	var wg sync.WaitGroup
	var okCount, fullCount atomic.Int32
	var otherErr atomic.Value
	for i := range writers {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			p := payload(100, int64(1000+i))
			_, err := s.Put(keyFor(p, 0), p)
			switch {
			case err == nil:
				okCount.Add(1)
			case errors.Is(err, ErrFull):
				fullCount.Add(1)
			default:
				otherErr.Store(err)
			}
		}(i)
	}
	wg.Wait()
	if e := otherErr.Load(); e != nil {
		t.Fatal(e)
	}
	if okCount.Load() != fit || fullCount.Load() != writers-fit {
		t.Fatalf("%d puts succeeded, %d full; want %d and %d", okCount.Load(), fullCount.Load(), fit, writers-fit)
	}
	if s.Used() != fit*size || s.Used() > s.Capacity() {
		t.Fatalf("used %d of %d", s.Used(), s.Capacity())
	}
	files := 0
	_ = s.Walk(func(ShardKey, int64, time.Time) error { files++; return nil })
	if files != fit {
		t.Fatalf("%d shard files on disk", files)
	}
	temps, _ := filepath.Glob(filepath.Join(root, "shards", "*", ".tmp-*"))
	if len(temps) != 0 {
		t.Fatalf("temps: %v", temps)
	}
	// a reopened store recounts the same usage
	s2 := openStore(t, sysfs.OS{}, root, fit*size)
	if s2.Used() != fit*size {
		t.Fatalf("recounted used %d", s2.Used())
	}
}

func TestStore_CapacityEnforced(t *testing.T) {
	root := t.TempDir()
	s := openStore(t, sysfs.OS{}, root, 3*(HeaderSize+100))
	for i := range 3 {
		p := payload(100, int64(100+i))
		if _, err := s.Put(keyFor(p, 0), p); err != nil {
			t.Fatal(err)
		}
	}
	p := payload(100, 999)
	if _, err := s.Put(keyFor(p, 0), p); !errors.Is(err, ErrFull) {
		t.Fatalf("err = %v, want ErrFull", err)
	}
	if s.Has(keyFor(p, 0)) {
		t.Fatal("rejected shard visible")
	}
	temps, _ := filepath.Glob(filepath.Join(root, "shards", "*", ".tmp-*"))
	if len(temps) != 0 {
		t.Fatalf("temps: %v", temps)
	}
	if s.Capacity() != 3*(HeaderSize+100) || s.Used() != 3*(HeaderSize+100) {
		t.Fatalf("Used/Capacity = %d/%d", s.Used(), s.Capacity())
	}
	// Dup put of an existing shard is allowed at full capacity.
	q := payload(100, 100)
	if _, err := s.Put(keyFor(q, 0), q); err != nil {
		t.Fatalf("dup put at capacity: %v", err)
	}
	// Free one, then the rejected shard fits.
	if _, err := s.Delete(keyFor(q, 0)); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Put(keyFor(p, 0), p); err != nil {
		t.Fatalf("after delete: %v", err)
	}
}

// Lifecycle: L-120, L-134
func TestStore_CapacityAccounting(t *testing.T) {
	root := t.TempDir()
	s := openStore(t, sysfs.OS{}, root, 0)
	var keys []ShardKey
	for i := range 10 {
		p := payload(1000+i, int64(i))
		k := keyFor(p, i%3)
		keys = append(keys, k)
		if _, err := s.Put(k, p); err != nil {
			t.Fatal(err)
		}
	}
	if s.Used() != diskUsed(t, root) {
		t.Fatalf("Used %d != disk %d", s.Used(), diskUsed(t, root))
	}
	for _, k := range keys[:4] {
		freed, err := s.Delete(k)
		if err != nil || freed <= 0 {
			t.Fatalf("Delete: %d %v", freed, err)
		}
	}
	if s.Used() != diskUsed(t, root) {
		t.Fatalf("after delete Used %d != disk %d", s.Used(), diskUsed(t, root))
	}
	s2 := openStore(t, sysfs.OS{}, root, 0)
	if s2.Used() != diskUsed(t, root) {
		t.Fatalf("after reopen Used %d != disk %d", s2.Used(), diskUsed(t, root))
	}
}

func TestStore_DeleteIdempotent(t *testing.T) {
	s := openStore(t, sysfs.OS{}, t.TempDir(), 0)
	p := payload(10, 1)
	k := keyFor(p, 0)
	_, _ = s.Put(k, p)
	if n, err := s.Delete(k); err != nil || n != int64(HeaderSize+10) {
		t.Fatalf("first delete: %d %v", n, err)
	}
	if n, err := s.Delete(k); err != nil || n != 0 {
		t.Fatalf("second delete: %d %v", n, err)
	}
	if s.Used() != 0 {
		t.Fatalf("Used = %d", s.Used())
	}
}

func TestStore_WalkListsAll(t *testing.T) {
	root := t.TempDir()
	s := openStore(t, sysfs.OS{}, root, 0)
	want := map[ShardKey]int64{}
	for i := range 20 {
		p := payload(10*i, int64(i))
		k := keyFor(p, i%3)
		want[k] = int64(HeaderSize + len(p))
		_, _ = s.Put(k, p)
	}
	got := map[ShardKey]int64{}
	err := s.Walk(func(k ShardKey, size int64, mtime time.Time) error {
		if mtime.IsZero() {
			t.Fatal("zero mtime")
		}
		got[k] = size
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != len(want) {
		t.Fatalf("Walk listed %d, want %d", len(got), len(want))
	}
	for k, sz := range want {
		if got[k] != sz {
			t.Fatalf("%s size %d want %d", k, got[k], sz)
		}
	}
	stop := errors.New("stop")
	n := 0
	err = s.Walk(func(ShardKey, int64, time.Time) error { n++; return stop })
	if !errors.Is(err, stop) || n != 1 {
		t.Fatalf("Walk did not stop on error: n=%d err=%v", n, err)
	}
}

func TestStore_OpenRejectsBadRoot(t *testing.T) {
	root := filepath.Join(t.TempDir(), "file")
	_ = os.WriteFile(root, []byte("x"), 0o644)
	if _, err := Open(sysfs.OS{}, root, 0); err == nil {
		t.Fatal("expected error opening store on a file")
	}
}

func FuzzStore_PutGet(f *testing.F) {
	f.Add([]byte{}, 0)
	f.Add([]byte("hello"), 1)
	f.Add(payload(4096, 1), 255)
	root := f.TempDir()
	s, err := Open(sysfs.OS{}, root, 0)
	if err != nil {
		f.Fatal(err)
	}
	f.Fuzz(func(t *testing.T, p []byte, idx int) {
		if idx < 0 || idx > 2 {
			return
		}
		k := keyFor(p, idx)
		if _, err := s.Put(k, p); err != nil {
			t.Fatal(err)
		}
		got, err := s.Get(k)
		if err != nil || !bytes.Equal(got, p) {
			t.Fatalf("round trip: %v", err)
		}
		if _, err := s.Delete(k); err != nil {
			t.Fatal(err)
		}
	})
}
