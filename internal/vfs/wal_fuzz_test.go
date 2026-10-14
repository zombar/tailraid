package vfs

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/zombar/tailraid/internal/sysfs"
)

// Replay of any byte string never panics, yields only records whose data length matches
// their header and whose bytes lie inside the file, cuts the file back to the end of the
// last intact record, and a second replay returns the same records; a record's actor
// survives the replay as its other fields do.
//
// Invariant: I-37, I-353
func FuzzWALReplay(f *testing.F) {
	dir := f.TempDir()
	w, err := openWAL(sysfs.OS{}, filepath.Join(dir, "seed.wal"))
	if err != nil {
		f.Fatal(err)
	}
	for i, p := range [][]byte{[]byte("hello"), {}, []byte("a longer record that spans more bytes")} {
		if _, err := w.AppendWrite(uint64(i*100), p); err != nil {
			f.Fatal(err)
		}
	}
	_ = w.Close()
	valid, err := os.ReadFile(filepath.Join(dir, "seed.wal"))
	if err != nil {
		f.Fatal(err)
	}
	f.Add(valid)
	f.Add(valid[:len(valid)-3]) // torn tail
	f.Add(append([]byte{}, valid[:recHeaderLen]...))
	f.Add([]byte{})
	f.Add(make([]byte, recHeaderLen*3))
	f.Fuzz(func(t *testing.T, b []byte) {
		path := filepath.Join(t.TempDir(), "x.wal")
		if err := os.WriteFile(path, b, 0o644); err != nil {
			t.Fatal(err)
		}
		w, err := openWAL(sysfs.OS{}, path)
		if err != nil {
			t.Fatal(err)
		}
		defer w.Close()
		first := replayAll(t, w)
		var end int64
		for _, r := range first {
			if int(r.n) != len(r.data) || r.walOff < recHeaderLen || r.walOff+int64(r.n) > int64(len(b)) {
				t.Fatalf("record %+v outside a %d-byte log", r, len(b))
			}
			end = r.walOff + int64(r.n)
		}
		st, err := os.Stat(path)
		if err != nil {
			t.Fatal(err)
		}
		if st.Size() != end || w.Discarded != int64(len(b))-end {
			t.Fatalf("after replay: size %d, discarded %d; records end at %d of %d", st.Size(), w.Discarded, end, len(b))
		}
		if second := replayAll(t, w); !reflect.DeepEqual(first, second) {
			t.Fatalf("second replay differs: %d vs %d records", len(first), len(second))
		}
	})
}
