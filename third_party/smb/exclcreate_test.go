package smb

import (
	"os"
	"testing"

	filesystem "github.com/go-filesystems/interface"
)

// racingFS is a gateway whose view lags: Stat has not seen a file another gateway made,
// and the exclusive create, decided where the views agree, finds it there.
type racingFS struct {
	nothingFS
	files     map[string][]byte
	taken     map[string]bool // made elsewhere, not yet seen by Stat
	truncated []string
}

func (r *racingFS) Stat(p string) (filesystem.Stat, error) {
	if p == "/" {
		return filesystem.NewStat(0o040755, 0, 1), nil
	}
	if b, ok := r.files[p]; ok {
		return filesystem.NewStat(0o100644, uint64(len(b)), 2), nil
	}
	return nil, os.ErrNotExist
}

func (r *racingFS) WriteFile(p string, d []byte, _ os.FileMode) error {
	r.truncated = append(r.truncated, p)
	r.files[p] = append([]byte(nil), d...)
	return nil
}

func (r *racingFS) CreateExcl(p string, _ os.FileMode) error {
	if r.taken[p] {
		return &os.PathError{Op: "open", Path: p, Err: os.ErrExist}
	}
	if _, ok := r.files[p]; ok {
		return &os.PathError{Op: "open", Path: p, Err: os.ErrExist}
	}
	r.files[p] = nil
	return nil
}

// FILE_CREATE is the exclusive create of the backend: a name another gateway took first
// is STATUS_OBJECT_NAME_COLLISION even when this gateway's Stat has not seen it, and the
// file is never truncated; a free name is created without a truncating write.
// Invariant: I-222
func TestSMB_FileCreate_RacingBackend_Collision(t *testing.T) {
	r := &racingFS{files: map[string][]byte{}, taken: map[string]bool{"/theirs.bin": true}}
	c, _ := opened(t, r, "/", false)
	if st, _ := createFile(t, c, "theirs.bin", dispCreate); st != statusObjectNameCollision {
		t.Fatalf("FILE_CREATE of a name taken elsewhere answered %#x, want STATUS_OBJECT_NAME_COLLISION", st)
	}
	st, id := createFile(t, c, "mine.bin", dispCreate)
	if st != statusSuccess {
		t.Fatalf("FILE_CREATE of a free name answered %#x", st)
	}
	closeHandle(t, c, id)
	if _, ok := r.files["/mine.bin"]; !ok {
		t.Fatal("the free name was not created")
	}
	if len(r.truncated) != 0 {
		t.Fatalf("FILE_CREATE wrote through the truncating path: %v", r.truncated)
	}
}
