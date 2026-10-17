package vfs

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/zombar/tailraid/internal/sysfs"
	"github.com/zombar/tailraid/internal/sysfs/sysfstest"
)

type rec struct {
	typ    uint8
	off    uint64
	n      uint32
	walOff int64
	data   []byte
}

func replayAll(t *testing.T, w *WAL) []rec {
	t.Helper()
	var out []rec
	err := w.Replay(func(typ uint8, off uint64, n uint32, walOff int64, data []byte) error {
		out = append(out, rec{typ, off, n, walOff, bytes.Clone(data)})
		return nil
	})
	if err != nil {
		t.Fatalf("Replay: %v", err)
	}
	return out
}

// Invariant: I-7, I-37
func TestWAL_AppendReplayRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "7.wal")
	w, err := openWAL(sysfs.OS{}, path)
	if err != nil {
		t.Fatal(err)
	}
	if err := w.AppendBase(1234); err != nil {
		t.Fatal(err)
	}
	o1, err := w.AppendWrite(10, []byte("hello"))
	if err != nil {
		t.Fatal(err)
	}
	if err := w.AppendTruncate(12); err != nil {
		t.Fatal(err)
	}
	o2, err := w.AppendWrite(0, bytes.Repeat([]byte{7}, 70000))
	if err != nil {
		t.Fatal(err)
	}
	buf := make([]byte, 3)
	if err := w.ReadAt(buf, o1+1); err != nil || string(buf) != "ell" {
		t.Fatalf("ReadAt inside record: %q %v", buf, err)
	}
	if w.Size() <= 70000 {
		t.Fatalf("Size = %d", w.Size())
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}

	w2, err := openWAL(sysfs.OS{}, path)
	if err != nil {
		t.Fatal(err)
	}
	defer w2.Close()
	got := replayAll(t, w2)
	if len(got) != 4 {
		t.Fatalf("replayed %d records: %+v", len(got), got)
	}
	if got[0].typ != recBase || got[0].off != 1234 {
		t.Fatalf("base record: %+v", got[0])
	}
	if got[1].typ != recWrite || got[1].off != 10 || got[1].n != 5 || string(got[1].data) != "hello" || got[1].walOff != o1 {
		t.Fatalf("write record: %+v", got[1])
	}
	if got[2].typ != recTruncate || got[2].off != 12 {
		t.Fatalf("truncate record: %+v", got[2])
	}
	if got[3].walOff != o2 || got[3].n != 70000 || !bytes.Equal(got[3].data, bytes.Repeat([]byte{7}, 70000)) {
		t.Fatalf("big record: typ=%d off=%d n=%d walOff=%d (want %d)", got[3].typ, got[3].off, got[3].n, got[3].walOff, o2)
	}
	// appending after replay continues at the end
	if _, err := w2.AppendWrite(5, []byte("x")); err != nil {
		t.Fatal(err)
	}
	if n := len(replayAll(t, w2)); n != 5 {
		t.Fatalf("after append: %d records", n)
	}
}

// Invariant: I-37
// Lifecycle: L-113, L-122
func TestWAL_ReplayStopsAtTornTail(t *testing.T) {
	for _, mode := range []string{"truncate-mid-record", "flip-crc", "flip-data", "truncate-mid-header"} {
		t.Run(mode, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "1.wal")
			w, err := openWAL(sysfs.OS{}, path)
			if err != nil {
				t.Fatal(err)
			}
			if err := w.AppendBase(0); err != nil {
				t.Fatal(err)
			}
			if _, err := w.AppendWrite(0, []byte("first")); err != nil {
				t.Fatal(err)
			}
			goodEnd := w.Size()
			if _, err := w.AppendWrite(100, bytes.Repeat([]byte("Z"), 500)); err != nil {
				t.Fatal(err)
			}
			if err := w.Close(); err != nil {
				t.Fatal(err)
			}
			raw, _ := os.ReadFile(path)
			switch mode {
			case "truncate-mid-record":
				raw = raw[:goodEnd+recHeaderLen+200]
			case "truncate-mid-header":
				raw = raw[:goodEnd+5]
			case "flip-crc":
				raw[goodEnd] ^= 0xff
			case "flip-data":
				raw[goodEnd+recHeaderLen+10] ^= 0x01
			}
			if err := os.WriteFile(path, raw, 0o600); err != nil {
				t.Fatal(err)
			}
			w2, err := openWAL(sysfs.OS{}, path)
			if err != nil {
				t.Fatal(err)
			}
			defer w2.Close()
			got := replayAll(t, w2)
			if len(got) != 2 || string(got[1].data) != "first" {
				t.Fatalf("%s: replayed %+v", mode, got)
			}
			// Invariant: I-37 — the torn tail is physically dropped and the WAL continues from the good end.
			if st, _ := os.Stat(path); st.Size() != goodEnd {
				t.Fatalf("%s: file size %d after replay, want %d", mode, st.Size(), goodEnd)
			}
			if w2.Size() != goodEnd {
				t.Fatalf("%s: Size() %d want %d", mode, w2.Size(), goodEnd)
			}
			o, err := w2.AppendWrite(7, []byte("after"))
			if err != nil {
				t.Fatal(err)
			}
			if o != goodEnd+recHeaderLen {
				t.Fatalf("%s: new record walOff %d want %d", mode, o, goodEnd+recHeaderLen)
			}
		})
	}
}

// Invariant: I-35, I-37
// Lifecycle: L-172
func TestWAL_Compact_KeepsOnlyLive(t *testing.T) {
	path := filepath.Join(t.TempDir(), "3.wal")
	w, err := openWAL(sysfs.OS{}, path)
	if err != nil {
		t.Fatal(err)
	}
	defer w.Close()
	if err := w.AppendBase(0); err != nil {
		t.Fatal(err)
	}
	dead, _ := w.AppendWrite(0, bytes.Repeat([]byte("D"), 10000))
	live1, _ := w.AppendWrite(50, []byte("live-one"))
	live2, _ := w.AppendWrite(200, []byte("live-two"))
	_ = dead
	recs := []walRec{
		{typ: recBase, off: 777},
		{typ: recTruncate, off: 120},
		{typ: recWrite, off: 52, src: live1 + 2, n: 4}, // split extent: "ve-o"
		{typ: recWrite, off: 200, src: live2, n: 8},
	}
	offs, err := w.Compact(recs)
	if err != nil {
		t.Fatal(err)
	}
	if len(offs) != len(recs) {
		t.Fatalf("offs = %v", offs)
	}
	if w.Bytes() >= 10000 {
		t.Fatalf("compacted log %d bytes still holds dead data", w.Bytes())
	}
	if got := readWAL(t, w, offs[2], 4); string(got) != "ve-o" {
		t.Fatalf("live1 after compact: %q", got)
	}
	if got := readWAL(t, w, offs[3], 8); string(got) != "live-two" {
		t.Fatalf("live2 after compact: %q", got)
	}
	// survives reopen, and nothing but the compacted records remain
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	w2, err := openWAL(sysfs.OS{}, path)
	if err != nil {
		t.Fatal(err)
	}
	defer w2.Close()
	got := replayAll(t, w2)
	if len(got) != 4 || got[0].typ != recBase || got[0].off != 777 || got[1].typ != recTruncate || got[1].off != 120 ||
		string(got[2].data) != "ve-o" || got[2].off != 52 || string(got[3].data) != "live-two" {
		t.Fatalf("after compact+reopen: %+v", got)
	}
	if _, err := os.Stat(path + ".new"); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf(".new file left behind: %v", err)
	}
}

func readWAL(t *testing.T, w *WAL, off int64, n int) []byte {
	t.Helper()
	buf := make([]byte, n)
	if err := w.ReadAt(buf, off); err != nil {
		t.Fatalf("ReadAt(%d,%d): %v", off, n, err)
	}
	return buf
}

func TestWAL_ReadAtBeyondEnd(t *testing.T) {
	w, err := openWAL(sysfs.OS{}, filepath.Join(t.TempDir(), "2.wal"))
	if err != nil {
		t.Fatal(err)
	}
	defer w.Close()
	o, _ := w.AppendWrite(0, []byte("ab"))
	if err := w.ReadAt(make([]byte, 5), o); err == nil {
		t.Fatal("ReadAt past end succeeded")
	}
}

// The WAL runs on the sysfs.FS seam so fsync failures and crashes can be injected. A
// failed fsync must surface from the append: the WRITE is then not acknowledged (I-7).
// Invariant: I-7
func TestWAL_SyncFailure_ReturnsError(t *testing.T) {
	ff := sysfstest.New(sysfs.OS{})
	w, err := openWAL(ff, filepath.Join(t.TempDir(), "1.wal"))
	if err != nil {
		t.Fatal(err)
	}
	defer w.Close()
	if err := w.AppendBase(0); err != nil {
		t.Fatal(err)
	}
	boom := errors.New("boom")
	ff.FailAt(sysfstest.StepSync, boom)
	if _, err := w.AppendWrite(4096, []byte("x")); !errors.Is(err, boom) {
		t.Fatalf("AppendWrite after fsync failure: err=%v, want %v", err, boom)
	}
	// The next append must not report a size that includes the un-synced record.
	if _, err := w.AppendWrite(8192, []byte("y")); err != nil {
		t.Fatal(err)
	}
	recs := replayAll(t, w)
	if len(recs) != 2 || recs[1].off != 8192 {
		t.Fatalf("replayed %+v, want base + write@8192", recs)
	}
}

// A new WAL file's directory entry is fsynced before the first record is appended, so an
// acknowledged WRITE never depends on an entry that power loss could drop. Reopening an
// existing log does not sync the directory.
// Invariant: I-7, I-63
func TestWAL_CreateFsyncsDirectory(t *testing.T) {
	ff := sysfstest.New(sysfs.OS{})
	path := filepath.Join(t.TempDir(), "1.wal")
	boom := errors.New("boom")
	ff.FailAt(sysfstest.StepSyncDir, boom)
	if _, err := openWAL(ff, path); !errors.Is(err, boom) {
		t.Fatalf("openWAL of a new file without a directory fsync: err=%v, want %v", err, boom)
	}
	w, err := openWAL(ff, path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := w.AppendWrite(4096, []byte("x")); err != nil {
		t.Fatal(err)
	}
	must(t, w.Close())
	ff.FailAt(sysfstest.StepSyncDir, boom) // must stay armed: reopen does not sync the dir
	w2, err := openWAL(ff, path)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer w2.Close()
	if recs := replayAll(t, w2); len(recs) != 1 || recs[0].off != 4096 {
		t.Fatalf("replayed %+v", recs)
	}
	if err := ff.SyncDir(filepath.Dir(path)); !errors.Is(err, boom) {
		t.Fatal("reopening an existing WAL synced the directory")
	}
}

// After a failed fsync the record is not acknowledged; a crash right after must not
// replay it (its bytes may still have reached the disk).
// Invariant: I-37
func TestWAL_AppendAfterFailedSync_NoResurrectedRecord(t *testing.T) {
	ff := sysfstest.New(sysfs.OS{})
	path := filepath.Join(t.TempDir(), "1.wal")
	w, err := openWAL(ff, path)
	if err != nil {
		t.Fatal(err)
	}
	must(t, w.AppendBase(0))
	boom := errors.New("boom")
	ff.FailAt(sysfstest.StepSync, boom)
	if _, err := w.AppendWrite(4096, []byte("never acknowledged")); !errors.Is(err, boom) {
		t.Fatalf("err=%v want %v", err, boom)
	}
	must(t, w.Close()) // the process dies here
	w2, err := openWAL(ff, path)
	if err != nil {
		t.Fatal(err)
	}
	defer w2.Close()
	if recs := replayAll(t, w2); len(recs) != 1 || recs[0].typ != recBase {
		t.Fatalf("replayed %+v, want the base record only", recs)
	}
}

// A record is bounded so a corrupt length can never drive a huge allocation on replay:
// the largest record is accepted and read back, one byte more is refused up front.
// Invariant: I-37
func TestWAL_RecordSizeBoundaries(t *testing.T) {
	for _, n := range []int{maxRecordData - 1, maxRecordData, maxRecordData + 1} {
		t.Run(fmt.Sprint(n), func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "9.wal")
			w, err := openWAL(sysfs.OS{}, path)
			if err != nil {
				t.Fatal(err)
			}
			data := make([]byte, n)
			data[n-1] = 9
			_, err = w.AppendWrite(4096, data)
			if n > maxRecordData {
				if err == nil {
					t.Fatal("over-sized record accepted")
				}
				if w.Size() != 0 {
					t.Fatalf("WAL grew to %d on a refused record", w.Size())
				}
				must(t, w.Close())
				return
			}
			must(t, err)
			must(t, w.Close())
			w2, err := openWAL(sysfs.OS{}, path)
			if err != nil {
				t.Fatal(err)
			}
			defer w2.Close()
			recs := replayAll(t, w2)
			if len(recs) != 1 || int(recs[0].n) != n || recs[0].data[n-1] != 9 {
				t.Fatalf("replayed %d records, n=%d", len(recs), recs[0].n)
			}
		})
	}
}
