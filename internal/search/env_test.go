package search

import (
	"bytes"
	"context"
	"encoding/binary"
	"os"
	"testing"

	"github.com/zombar/tailraid/internal/meta"
	"github.com/zombar/tailraid/internal/testharness"
	"github.com/zombar/tailraid/internal/vfs"
)

// env is one gateway over the in-process fabric: a real FSM, vfs, flusher and reader.
type env struct {
	t   *testing.T
	f   *testharness.Fabric
	fs  *vfs.FS
	svc *Service
}

func newEnv(t *testing.T) *env {
	t.Helper()
	f := testharness.NewFabric(t, 5, 3, 2)
	fs := f.FS()
	fs.SetFlusher(f.Flusher())
	e := &env{t: t, f: f, fs: fs}
	e.svc = e.service()
	return e
}

// service is another node's view of the same volume: its own chain cache.
func (e *env) service() *Service {
	cfg := DefaultConfig()
	cfg.MaxFile = int(testharness.ChunkSize)
	return &Service{Src: e.f.FSM, Vol: VFS{FS: e.fs, St: e.f.FSM, Rd: e.f.ReaderNoCache()}, Cfg: cfg, Clk: e.f.Clk}
}

// write writes data whole, its tail at a non-zero offset, and commits it.
func (e *env) write(name string, data []byte) meta.Ino {
	e.t.Helper()
	f, err := e.fs.OpenFile(name, os.O_RDWR|os.O_CREATE|os.O_TRUNC, 0o644)
	if err != nil {
		e.t.Fatal(err)
	}
	half := len(data) / 2
	if _, err := f.Write(data[:half]); err != nil {
		e.t.Fatal(err)
	}
	if _, err := f.(interface {
		WriteAt([]byte, int64) (int, error)
	}).WriteAt(data[half:], int64(half)); err != nil {
		e.t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		e.t.Fatal(err)
	}
	if err := e.fs.Flush(context.Background(), name); err != nil {
		e.t.Fatal(err)
	}
	return e.ino(name)
}

func (e *env) mkdir(name string) meta.Ino {
	e.t.Helper()
	if err := e.fs.MkdirAll(name, 0o755); err != nil {
		e.t.Fatal(err)
	}
	return e.ino(name)
}

func (e *env) ino(name string) meta.Ino {
	e.t.Helper()
	in, err := e.f.FSM.Resolve(meta.SplitPath(name))
	if err != nil {
		e.t.Fatalf("%s: %v", name, err)
	}
	return in.Ino
}

func (e *env) library(name string, k meta.LibraryKind) {
	e.t.Helper()
	e.f.MustApply(meta.Command{Op: meta.OpSetAttr, SetAttr: &meta.SetAttr{Ino: e.mkdir(name), Mask: meta.AttrFlags, Flags: meta.WithLibrary(0, k), Now: 1}})
}

func (e *env) pass() PassResult {
	e.t.Helper()
	res, err := e.svc.Pass(context.Background())
	if err != nil {
		e.t.Fatal(err)
	}
	return res
}

func (e *env) search(q string) Result {
	return e.svc.Search(context.Background(), q, "", 0)
}

// segments names the index folder's segment files.
func (e *env) segments() []string {
	e.t.Helper()
	dir, err := e.f.FSM.Resolve(meta.SplitPath(Dir))
	if err != nil {
		return nil
	}
	kids, _ := e.f.FSM.ReadDir(dir.Ino)
	var out []string
	for i := range kids {
		out = append(out, kids[i].Name)
	}
	return out
}

func paths(r Result) []string {
	out := []string{}
	for i := range r.Hits {
		out = append(out, r.Hits[i].Match+":"+r.Hits[i].Path)
	}
	return out
}

// id3 is an MP3 head: an ID3v2.3 tag with the frames, after a PRIV frame of pad bytes
// (to push them past the first chunk), then a few bytes of "audio".
func id3(pad int, frames ...string) []byte {
	var body bytes.Buffer
	frame := func(id string, data []byte) {
		body.WriteString(id)
		_ = binary.Write(&body, binary.BigEndian, uint32(len(data))) //nolint:gosec // test sizes
		body.Write([]byte{0, 0})
		body.Write(data)
	}
	if pad > 0 {
		frame("PRIV", append([]byte("x\x00"), make([]byte, pad)...))
	}
	for i := 0; i+1 < len(frames); i += 2 {
		frame(frames[i], append([]byte{3}, frames[i+1]...)) // 3: UTF-8
	}
	n := body.Len()
	out := []byte{'I', 'D', '3', 3, 0, 0, byte(n >> 21 & 0x7f), byte(n >> 14 & 0x7f), byte(n >> 7 & 0x7f), byte(n & 0x7f)}
	out = append(out, body.Bytes()...)
	return append(out, bytes.Repeat([]byte{0xff, 0xfb, 0x90, 0x00}, 64)...)
}
