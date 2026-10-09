// Copyright (c) 2026, go-filesystems
// SPDX-License-Identifier: BSD-3-Clause

package smb_test

import (
	"bytes"
	"io"
	"sync/atomic"
	"testing"

	filesystem "github.com/go-filesystems/interface"
)

// widestReadFS records the largest ReadAt the server asked of a file.
type widestReadFS struct {
	*memFS
	widest atomic.Int64
}

func (w *widestReadFS) OpenFile(p string) (filesystem.File, error) {
	f, err := w.memFS.OpenFile(p)
	if err != nil {
		return nil, err
	}
	return widestReadFile{f, &w.widest}, nil
}

type widestReadFile struct {
	filesystem.File
	widest *atomic.Int64
}

func (f widestReadFile) ReadAt(p []byte, off int64) (int, error) {
	for {
		w := f.widest.Load()
		if int64(len(p)) <= w || f.widest.CompareAndSwap(w, int64(len(p))) {
			break
		}
	}
	return f.File.ReadAt(p, off)
}

// A client reads in requests of up to MaxReadSize -- a megabyte -- only when
// the server claims multi-credit operations (SMB2_GLOBAL_CAP_LARGE_MTU).
// Without the claim go-smb2 holds every READ to 64 KiB, as the Linux kernel
// client does, and the server sees nothing wider.
func TestReadsAreWiderThan64KiB(t *testing.T) {
	addr, srv := serve(t)
	fs := &widestReadFS{memFS: newMemFS(true)}
	want := bytes.Repeat([]byte("0123456789abcdef"), 3<<20/16)
	if err := fs.WriteFile("/big.bin", want, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := srv.Share("data", fs); err != nil {
		t.Fatal(err)
	}
	s, done := dial(t, addr, "alice", "hunter2")
	defer done()
	share, err := s.Mount("data")
	if err != nil {
		t.Fatal(err)
	}
	defer share.Umount()
	f, err := share.Open("big.bin")
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	var got bytes.Buffer
	if _, err := io.CopyBuffer(&got, struct{ io.Reader }{f}, make([]byte, 1<<20)); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got.Bytes(), want) {
		t.Fatalf("read %d bytes, not the %d written", got.Len(), len(want))
	}
	if w := fs.widest.Load(); w <= 64<<10 {
		t.Fatalf("the widest read the server made was %d bytes: the client stayed at 64 KiB", w)
	}
}
