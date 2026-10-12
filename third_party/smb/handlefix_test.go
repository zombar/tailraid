package smb

import (
	"encoding/binary"
	"os"
	"testing"

	filesystem "github.com/go-filesystems/interface"
)

// seenRacingFS is racingFS whose exclusive create, finding the name taken elsewhere,
// leaves this gateway having seen that file, as vfs does (it waits for the refusal's
// index), and which makes directories exclusively too.
type seenRacingFS struct {
	racingFS
	dirs  map[string]bool
	dtake map[string]bool // directories made elsewhere, not yet seen
}

func (r *seenRacingFS) Stat(p string) (filesystem.Stat, error) {
	if r.dirs[p] {
		return filesystem.NewStat(0o040755, 0, 3), nil
	}
	return r.racingFS.Stat(p)
}

func (r *seenRacingFS) CreateExcl(p string, perm os.FileMode) error {
	if r.taken[p] {
		r.files[p] = []byte("theirs")
		delete(r.taken, p)
		return &os.PathError{Op: "open", Path: p, Err: os.ErrExist}
	}
	return r.racingFS.CreateExcl(p, perm)
}

func (r *seenRacingFS) MkDir(p string, _ os.FileMode) error {
	r.dirs[p] = true // MkdirAll: an existing directory is no error
	return nil
}

func (r *seenRacingFS) MkDirExcl(p string, _ os.FileMode) error {
	if r.dtake[p] || r.dirs[p] {
		r.dirs[p] = true
		return &os.PathError{Op: "mkdir", Path: p, Err: os.ErrExist}
	}
	r.dirs[p] = true
	return nil
}

// FILE_OPEN_IF of a name another gateway created first opens that file: it is never
// truncated, whatever this gateway's Stat had seen.
// Regression: I-222
func TestSMB_OpenIf_RacingBackend_NoTruncate(t *testing.T) {
	r := &seenRacingFS{racingFS: racingFS{files: map[string][]byte{}, taken: map[string]bool{"/theirs.bin": true}}, dirs: map[string]bool{}}
	c, _ := opened(t, r, "/", false)
	st, id := createFile(t, c, "theirs.bin", dispOpenIf)
	if st != statusSuccess {
		t.Fatalf("FILE_OPEN_IF answered %#x", st)
	}
	closeHandle(t, c, id)
	if got := string(r.files["/theirs.bin"]); got != "theirs" || len(r.truncated) != 0 {
		t.Fatalf("FILE_OPEN_IF truncated the other gateway's file: %q, truncating writes %v", got, r.truncated)
	}
}

// FILE_CREATE of a directory another gateway made first is a collision, decided by the
// backend; a free name is made.
// Regression: I-222
func TestSMB_FileCreateDir_RacingBackend_Collision(t *testing.T) {
	r := &seenRacingFS{racingFS: racingFS{files: map[string][]byte{}}, dirs: map[string]bool{}, dtake: map[string]bool{"/theirs": true}}
	c, _ := opened(t, r, "/", false)
	if st, _ := createDir(t, c, "theirs", dispCreate); st != statusObjectNameCollision {
		t.Fatalf("FILE_CREATE of a directory taken elsewhere answered %#x, want STATUS_OBJECT_NAME_COLLISION", st)
	}
	st, id := createDir(t, c, "mine", dispCreate)
	if st != statusSuccess {
		t.Fatalf("FILE_CREATE of a free directory answered %#x", st)
	}
	closeHandle(t, c, id)
}

// A byte-range lock follows its file through a rename by another session: the handle
// that took it unlocks it at the new name, and the file now holding the old name is
// free of it.
// Regression: I-224
func TestSMB_Lock_FollowsRename(t *testing.T) {
	f := newInoFS()
	must(t, f.WriteFile("/a", []byte("the file"), 0))
	c1, c2 := twoSessions(f)
	_, h1 := createFile(t, c1, "a", dispOpen)
	_, h2 := createFile(t, c2, "a", dispOpen)
	if st := lockReq(t, c1, h1, 0, 100, lockExclusive|lockFailImmediately); st != statusSuccess {
		t.Fatalf("lock answered %#x", st)
	}
	if st := renameHandle(t, c2, h2, `\moved`, false); st != statusSuccess {
		t.Fatalf("rename answered %#x", st)
	}
	must(t, f.WriteFile("/a", []byte("a new file"), 0))
	_, h3 := createFile(t, c2, "a", dispOpen)
	if st := lockReq(t, c2, h3, 0, 10, lockExclusive|lockFailImmediately); st != statusSuccess {
		t.Fatalf("the new file under the old name is locked by the moved one: %#x", st)
	}
	if st := lockReq(t, c1, h1, 0, 100, lockUnlock); st != statusSuccess {
		t.Fatalf("unlock through the moved handle answered %#x", st)
	}
	closeHandle(t, c1, h1)
	closeHandle(t, c2, h2)
	closeHandle(t, c2, h3)
}

func createDir(t *testing.T, c *conn, name string, disp uint32) (uint32, [16]byte) {
	t.Helper()
	n := utf16le(name)
	body := make([]byte, 56)
	binary.LittleEndian.PutUint32(body[36:], disp)
	binary.LittleEndian.PutUint32(body[40:], optDirectoryFile)
	binary.LittleEndian.PutUint16(body[44:], uint16(headerLen+56))
	binary.LittleEndian.PutUint16(body[46:], uint16(len(n)))
	out, err := c.dispatch(request(cmdCreate, 1, append(body, n...)))
	if err != nil {
		t.Fatal(err)
	}
	var id [16]byte
	if st := statusOf(t, out); st == statusSuccess {
		copy(id[:], out[headerLen+64:])
	}
	return statusOf(t, out), id
}

func lockReq(t *testing.T, c *conn, id [16]byte, off, length uint64, flags uint32) uint32 {
	t.Helper()
	body := make([]byte, 48)
	binary.LittleEndian.PutUint16(body[2:], 1)
	copy(body[8:], id[:])
	binary.LittleEndian.PutUint64(body[24:], off)
	binary.LittleEndian.PutUint64(body[32:], length)
	binary.LittleEndian.PutUint32(body[40:], flags)
	return statusOf(t, mustDispatch(t, c, cmdLock, body))
}
