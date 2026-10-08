package vfs_test

import (
	"bytes"
	"errors"
	"io"
	"os"
	"sort"
	"syscall"
	"testing"
	"time"

	billytest "github.com/go-git/go-billy/v5/test"
	nfsfile "github.com/willscott/go-nfs/file"
	check "gopkg.in/check.v1"

	"github.com/zombar/tailraid/internal/meta"
	"github.com/zombar/tailraid/internal/testharness"
)

// --- go-billy conformance suites via a gocheck shim ------------------------------------

func TestBillySuites(t *testing.T) { check.TestingT(t) }

// checkTB adapts gocheck's *C to the fabric's tb interface.
type checkTB struct {
	*check.C
	cleanups []func()
}

func (c *checkTB) Helper()                           {}
func (c *checkTB) TempDir() string                   { return c.MkDir() }
func (c *checkTB) Cleanup(f func())                  { c.cleanups = append(c.cleanups, f) }
func (c *checkTB) Fatal(args ...any)                 { c.C.Fatal(args...) }
func (c *checkTB) Fatalf(format string, args ...any) { c.C.Fatalf(format, args...) }
func (c *checkTB) runCleanups() {
	for i := len(c.cleanups) - 1; i >= 0; i-- {
		c.cleanups[i]()
	}
}

type billySuite struct {
	billytest.BasicSuite
	billytest.DirSuite
	tb *checkTB
}

var _ = check.Suite(&billySuite{})

func (s *billySuite) SetUpTest(c *check.C) {
	s.tb = &checkTB{C: c}
	f := testharness.NewFabric(s.tb, 3, 2, 1)
	fs := f.FS()
	s.BasicSuite.FS = fs
	s.DirSuite.FS = fs
}

func (s *billySuite) TearDownTest(_ *check.C) { s.tb.runCleanups() }

// --- gateway call sequences ------------------------------------------------------------

// go-nfs performs Stat, OpenFile(O_RDWR), Seek, Write, Close for every WRITE RPC and
// Open, ReadAt, Close for every READ. This is the sequence that corrupted files in the
// previous attempt.
// Invariant: I-7, I-34
// Lifecycle: L-1, L-2, L-5
func TestVFS_PerRPCOpenCloseSequence(t *testing.T) {
	f := testharness.NewFabric(t, 3, 2, 1)
	fs := f.FS()
	fh, err := fs.Create("/dir/file")
	must(t, err)
	must(t, fh.Close())
	payload := testharness.Pattern(300<<10, 1) // 300 KiB in 64 KiB WRITEs, out of order like macOS
	const wsize = 64 << 10
	var offs []int
	for off := 0; off < len(payload); off += wsize {
		offs = append(offs, off)
	}
	offs[0], offs[2] = offs[2], offs[0]
	for _, off := range offs {
		end := min(off+wsize, len(payload))
		if _, err := fs.Stat("/dir/file"); err != nil {
			t.Fatal(err)
		}
		fh, err := fs.OpenFile("/dir/file", os.O_RDWR, 0o644)
		must(t, err)
		if _, err := fh.Seek(int64(off), io.SeekStart); err != nil {
			t.Fatal(err)
		}
		if n, err := fh.Write(payload[off:end]); err != nil || n != end-off {
			t.Fatalf("write %d: n=%d err=%v", off, n, err)
		}
		must(t, fh.Close())
	}
	readBack := func() []byte {
		fh, err := fs.Open("/dir/file")
		must(t, err)
		defer fh.Close()
		out := make([]byte, len(payload)+10)
		n, err := fh.ReadAt(out, 0)
		if err != nil && !errors.Is(err, io.EOF) {
			t.Fatal(err)
		}
		return out[:n]
	}
	if got := readBack(); !bytes.Equal(got, payload) {
		t.Fatalf("before flush: differs at %d (len %d)", testharness.FirstDiff(got, payload), len(got))
	}
	st, _ := fs.Stat("/dir/file")
	if st.Size() != int64(len(payload)) {
		t.Fatalf("size while dirty %d", st.Size())
	}
	must(t, f.Flusher().FlushAll(ctx))
	if got := readBack(); !bytes.Equal(got, payload) {
		t.Fatalf("after flush: differs at %d", testharness.FirstDiff(got, payload))
	}
	in := f.Inode(f.InodeOf(fs, "/dir/file"))
	if in.Size != uint64(len(payload)) || len(in.Chunks) != 5 {
		t.Fatalf("inode %+v", in)
	}
	// SETATTR size path: OpenFile(O_WRONLY|O_EXCL) + Truncate + Close
	fh, err = fs.OpenFile("/dir/file", os.O_WRONLY|os.O_EXCL, 0)
	must(t, err)
	must(t, fh.Truncate(100<<10))
	must(t, fh.Close())
	if got := readBack(); len(got) != 100<<10 || !bytes.Equal(got, payload[:100<<10]) {
		t.Fatalf("after truncate: len %d", len(got))
	}
	must(t, f.Flusher().FlushAll(ctx))
	if got := readBack(); !bytes.Equal(got, payload[:100<<10]) {
		t.Fatal("after truncate flush")
	}
}

// Invariant: I-34
// Lifecycle: L-2
func TestVFS_WriteAtNonZeroOffset_PreservesPrefix(t *testing.T) {
	f := testharness.NewFabric(t, 3, 2, 1)
	fs := f.FS()
	fh, _ := fs.Create("/f")
	prefix := testharness.Pattern(70<<10, 2)
	_, _ = fh.Write(prefix)
	_ = fh.Close()
	must(t, f.Flusher().FlushAll(ctx))
	fh, _ = fs.OpenFile("/f", os.O_RDWR, 0)
	_, _ = fh.Seek(65<<10, io.SeekStart)
	_, _ = fh.Write([]byte("PATCH"))
	_ = fh.Close()
	want := bytes.Clone(prefix)
	copy(want[65<<10:], "PATCH")
	verify := func(stage string) {
		fh, _ := fs.Open("/f")
		got := make([]byte, len(want))
		if _, err := fh.ReadAt(got, 0); err != nil && !errors.Is(err, io.EOF) {
			t.Fatal(err)
		}
		if !bytes.Equal(got, want) {
			t.Fatalf("%s: differs at %d", stage, testharness.FirstDiff(got, want))
		}
	}
	verify("dirty")
	must(t, f.Flusher().FlushAll(ctx))
	verify("flushed")
}

// Lifecycle: L-18
func TestVFS_RenameDirtyFile_FlushCommitsSameIno(t *testing.T) {
	f := testharness.NewFabric(t, 3, 2, 1)
	fs := f.FS()
	fh, _ := fs.Create("/a")
	_, _ = fh.Write([]byte("dirty bytes"))
	_ = fh.Close()
	ino := f.InodeOf(fs, "/a")
	must(t, fs.Rename("/a", "/b"))
	must(t, f.Flusher().FlushAll(ctx))
	in := f.Inode(ino)
	if in.Name != "b" || in.Size != 11 || in.Version != 1 {
		t.Fatalf("%+v", in)
	}
	if got := f.ReadFile(ino); string(got) != "dirty bytes" {
		t.Fatalf("%q", got)
	}
}

// Lifecycle: L-13
func TestVFS_RemoveDirtyFile_DropsOverlay(t *testing.T) {
	f := testharness.NewFabric(t, 3, 2, 1)
	fs := f.FS()
	fh, _ := fs.Create("/a")
	_, _ = fh.Write([]byte("gone"))
	_ = fh.Close()
	ino := f.InodeOf(fs, "/a")
	must(t, fs.Remove("/a"))
	if _, ok := f.Ov.Size(ino, 0); ok {
		t.Fatal("overlay still tracks removed inode")
	}
	if _, err := fh.Write([]byte("x")); err == nil {
		t.Fatal("write to removed inode succeeded")
	}
	must(t, f.Flusher().FlushAll(ctx))
	if f.FSM.Stats().Chunks != 0 {
		t.Fatal("chunks committed for a removed file")
	}
}

func TestVFS_SymlinkAndChange(t *testing.T) {
	f := testharness.NewFabric(t, 3, 2, 1)
	fs := f.FS()
	must(t, fs.Symlink("/target", "/d/link"))
	if got, err := fs.Readlink("/d/link"); err != nil || got != "/target" {
		t.Fatalf("readlink %q %v", got, err)
	}
	st, _ := fs.Lstat("/d/link")
	if st.Mode()&os.ModeSymlink == 0 || st.Size() != 7 {
		t.Fatalf("lstat %v %d", st.Mode(), st.Size())
	}
	fh, _ := fs.Create("/d/f")
	_ = fh.Close()
	must(t, fs.Chmod("/d/f", 0o600))
	must(t, fs.Chown("/d/f", 7, 9))
	mt := time.Unix(1_600_000_000, 123)
	must(t, fs.Chtimes("/d/f", mt, mt))
	st, _ = fs.Stat("/d/f")
	sys := st.Sys().(*nfsfile.FileInfo)
	if st.Mode().Perm() != 0o600 || sys.UID != 7 || sys.GID != 9 || !st.ModTime().Equal(mt) || sys.Fileid != uint64(f.InodeOf(fs, "/d/f")) {
		t.Fatalf("stat %v sys %+v mtime %v", st.Mode(), sys, st.ModTime())
	}
	if _, err := fs.Readlink("/d/f"); err == nil {
		t.Fatal("readlink on a file")
	}
	ents, _ := fs.ReadDir("/d")
	if len(ents) != 2 || ents[0].Name() != "f" || ents[1].Name() != "link" {
		t.Fatalf("readdir %v", ents)
	}
}

// Model-vs-filesystem oracle through the billy API, with flushes and a gateway restart.
// Invariant: I-1, I-34, I-35
// Lifecycle: L-1, L-2, L-3, L-4, L-5, L-6, L-8, L-11, L-12, L-14, L-16, L-17
func TestVFS_OracleRandomOps(t *testing.T) {
	rng := testharness.Seed(t)
	ops := 200
	if !testing.Short() {
		ops = 4000
	}
	f := testharness.NewFabric(t, 3, 2, 1)
	fs := f.FS()
	fl := f.Flusher()
	cs := int(testharness.ChunkSize)
	model := map[string][]byte{}
	names := []string{"/a", "/b", "/d/c", "/d/e/f"}
	pickName := func() string { return names[rng.Intn(len(names))] }
	pickOff := func(size int) int {
		switch rng.Intn(6) {
		case 0:
			return 0
		case 1:
			return size
		case 2:
			return size + rng.Intn(cs)
		case 3:
			return rng.Intn(4)*cs + cs - 1
		case 4:
			return rng.Intn(4)*cs + 1
		default:
			if size == 0 {
				return 0
			}
			return rng.Intn(size)
		}
	}
	verify := func(stage string) {
		t.Helper()
		for name, want := range model {
			fh, err := fs.Open(name)
			if err != nil {
				t.Fatalf("%s: open %s: %v", stage, name, err)
			}
			got := make([]byte, len(want)+cs)
			n, err := fh.ReadAt(got, 0)
			if err != nil && !errors.Is(err, io.EOF) {
				t.Fatalf("%s: read %s: %v", stage, name, err)
			}
			_ = fh.Close()
			if !bytes.Equal(got[:n], want) {
				t.Fatalf("%s: %s differs at %d (len %d want %d)", stage, name, testharness.FirstDiff(got[:n], want), n, len(want))
			}
			st, _ := fs.Stat(name)
			if st.Size() != int64(len(want)) {
				t.Fatalf("%s: %s size %d want %d", stage, name, st.Size(), len(want))
			}
		}
		for _, name := range names {
			if _, ok := model[name]; !ok {
				if _, err := fs.Stat(name); !errors.Is(err, os.ErrNotExist) {
					t.Fatalf("%s: %s should not exist: %v", stage, name, err)
				}
			}
		}
	}
	for i := 0; i < ops; i++ {
		name := pickName()
		cur, exists := model[name]
		switch r := rng.Intn(12); {
		case r < 6: // write at offset through the per-RPC sequence
			fh, err := fs.OpenFile(name, os.O_RDWR|os.O_CREATE, 0o644)
			must(t, err)
			if !exists {
				cur = nil
			}
			off := pickOff(len(cur))
			data := testharness.Pattern(1+rng.Intn(cs/2), byte(rng.Intn(256)))
			_, _ = fh.Seek(int64(off), io.SeekStart)
			if _, err := fh.Write(data); err != nil {
				t.Fatalf("op %d write: %v", i, err)
			}
			_ = fh.Close()
			if end := off + len(data); end > len(cur) {
				cur = append(cur, make([]byte, end-len(cur))...)
			}
			copy(cur[off:], data)
			model[name] = cur
		case r < 8 && exists: // truncate
			size := pickOff(len(cur))
			fh, err := fs.OpenFile(name, os.O_WRONLY|os.O_EXCL, 0)
			must(t, err)
			must(t, fh.Truncate(int64(size)))
			_ = fh.Close()
			if size < len(cur) {
				cur = cur[:size]
			} else {
				cur = append(cur, make([]byte, size-len(cur))...)
			}
			model[name] = cur
		case r == 8 && exists: // rename over another name
			dst := pickName()
			if dst == name {
				break
			}
			must(t, fs.Rename(name, dst))
			model[dst] = cur
			delete(model, name)
		case r == 9 && exists: // remove
			must(t, fs.Remove(name))
			delete(model, name)
		case r == 10: // flush some or all
			if rng.Intn(2) == 0 {
				must(t, fl.FlushAll(ctx))
			} else if exists {
				must(t, fl.FlushInode(ctx, f.InodeOf(fs, name)))
			}
		default: // read-only step
		}
		if i%50 == 49 {
			verify("step")
		}
	}
	verify("final")
	must(t, fl.FlushAll(ctx))
	verify("flushed")
	if d := f.Ov.Dirty(); len(d) != 0 {
		t.Fatalf("dirty after FlushAll: %+v", d)
	}
	// every model file is now served purely from committed chunks by a cold reader
	keys := make([]string, 0, len(model))
	for k := range model {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, name := range keys {
		in := f.Inode(f.InodeOf(fs, name))
		got := make([]byte, len(model[name]))
		if _, err := f.ReaderNoCache().ReadAt(ctx, in, got, 0); err != nil && !errors.Is(err, io.EOF) {
			t.Fatalf("cold read %s: %v", name, err)
		}
		if !bytes.Equal(got, model[name]) {
			t.Fatalf("cold read %s differs at %d", name, testharness.FirstDiff(got, model[name]))
		}
	}
}

// A write or truncate that would take the file past meta.MaxFileSize is refused with
// EFBIG before anything reaches the WAL or the FSM.
// Invariant: I-64
// Lifecycle: L-182
func TestVFS_SizeBeyondMax_EFBIG(t *testing.T) {
	f := testharness.NewFabric(t, 3, 2, 1)
	fs := f.FS()
	fh, err := fs.Create("/big")
	must(t, err)
	limit := int64(meta.MaxFileSize(testharness.ChunkSize))
	if _, err := fh.Seek(4096, io.SeekStart); err != nil {
		t.Fatal(err)
	}
	if _, err := fh.Write([]byte("ten  bytes")); err != nil {
		t.Fatal(err)
	}
	if _, err := fh.Seek(limit-1, io.SeekStart); err != nil {
		t.Fatal(err)
	}
	if _, err := fh.Write([]byte("x")); err != nil {
		t.Fatalf("write of the last allowed byte: %v", err)
	}
	if _, err := fh.Write([]byte("y")); !errors.Is(err, syscall.EFBIG) {
		t.Fatalf("write past the limit: err=%v, want EFBIG", err)
	}
	if err := fh.Truncate(limit); err != nil {
		t.Fatalf("truncate to the limit: %v", err)
	}
	if err := fh.Truncate(limit + 1); !errors.Is(err, syscall.EFBIG) {
		t.Fatalf("truncate past the limit: err=%v, want EFBIG", err)
	}
	must(t, fh.Close())
	st, err := fs.Stat("/big")
	must(t, err)
	if st.Size() != limit {
		t.Fatalf("size %d want %d", st.Size(), limit)
	}
}
