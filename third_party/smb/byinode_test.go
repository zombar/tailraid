package smb

import (
	"encoding/binary"
	"os"
	"sync"
	"testing"

	filesystem "github.com/go-filesystems/interface"
)

// inoFS is a root of plain files with real inode numbers: a rename moves the inode, a
// new file gets a new one, so a test can tell "the file this handle opened" from "the
// file that now has its name". byIno adds the InodeDeleter and NoReplaceRenamer
// capabilities a shared backend offers (tailraid's FSM decides them atomically).
type inoFS struct {
	nothingFS
	mu    sync.Mutex
	files map[string]*inoFile
	next  uint64
	byIno bool
}

type inoFile struct {
	ino  uint64
	data []byte
}

func newInoFS() *inoFS { return &inoFS{files: map[string]*inoFile{}, next: 10} }

func (f *inoFS) Stat(p string) (filesystem.Stat, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if p == "/" {
		return filesystem.NewStat(0o040755, 0, 1), nil
	}
	if x, ok := f.files[p]; ok {
		return filesystem.NewStat(0o100644, uint64(len(x.data)), x.ino), nil
	}
	return nil, os.ErrNotExist
}

func (f *inoFS) WriteFile(p string, d []byte, _ os.FileMode) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if x, ok := f.files[p]; ok {
		x.data = append([]byte(nil), d...)
		return nil
	}
	f.next++
	f.files[p] = &inoFile{ino: f.next, data: append([]byte(nil), d...)}
	return nil
}

func (f *inoFS) ReadFile(p string) ([]byte, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if x, ok := f.files[p]; ok {
		return append([]byte(nil), x.data...), nil
	}
	return nil, os.ErrNotExist
}

func (f *inoFS) DeleteFile(p string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if _, ok := f.files[p]; !ok {
		return os.ErrNotExist
	}
	delete(f.files, p)
	return nil
}

func (f *inoFS) Rename(from, to string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	x, ok := f.files[from]
	if !ok {
		return os.ErrNotExist
	}
	delete(f.files, from)
	f.files[to] = x
	return nil
}

func (f *inoFS) ino(p string) uint64 {
	f.mu.Lock()
	defer f.mu.Unlock()
	if x, ok := f.files[p]; ok {
		return x.ino
	}
	return 0
}

// byInoFS answers the two capabilities itself, as the tailraid adapter does.
type byInoFS struct{ *inoFS }

func (f byInoFS) DeleteFileIno(p string, ino uint64, _ bool) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	x, ok := f.files[p]
	if !ok || x.ino != ino {
		return os.ErrNotExist // another file holds the name: nothing to delete
	}
	delete(f.files, p)
	return nil
}

func (f byInoFS) RenameNoReplace(from, to string) error {
	f.mu.Lock()
	if _, ok := f.files[to]; ok {
		f.mu.Unlock()
		return os.ErrExist
	}
	f.mu.Unlock()
	return f.Rename(from, to)
}

// twoSessions is two connections, as two clients, on one share of fsys.
func twoSessions(fsys filesystem.Filesystem) (*conn, *conn) {
	sh := &share{name: "disk", fsys: fsys}
	c1, c2 := newConn(New(), nil), newConn(New(), nil)
	c1.trees[1] = &treeConn{sh: sh}
	c2.trees[1] = &treeConn{sh: sh}
	return c1, c2
}

// renameHandle sends SET_INFO FileRenameInformation for the handle id.
func renameHandle(t *testing.T, c *conn, id [16]byte, to string, replace bool) uint32 {
	t.Helper()
	name := utf16le(to)
	in := make([]byte, 20, 20+len(name))
	if replace {
		in[0] = 1
	}
	binary.LittleEndian.PutUint32(in[16:], uint32(len(name)))
	in = append(in, name...)
	body := make([]byte, 32)
	body[2], body[3] = infoTypeFile, fileRenameInformation
	binary.LittleEndian.PutUint32(body[4:], uint32(len(in)))
	binary.LittleEndian.PutUint16(body[8:], uint16(headerLen+32))
	copy(body[16:], id[:])
	out, err := c.dispatch(request(cmdSetInfo, 1, append(body, in...)))
	if err != nil {
		t.Fatal(err)
	}
	return statusOf(t, out)
}

// A rename without ReplaceIfExists onto an existing name is a collision and moves
// nothing, whether the backend decides it (NoReplaceRenamer) or the server looks first;
// with it, the existing file is replaced.
// Invariant: I-224
func TestRename_NoReplace_Collision(t *testing.T) {
	for _, tc := range []struct {
		name string
		fsys func(*inoFS) filesystem.Filesystem
	}{
		{"server looks", func(f *inoFS) filesystem.Filesystem { return f }},
		{"backend decides", func(f *inoFS) filesystem.Filesystem { return byInoFS{f} }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newInoFS()
			must(t, f.WriteFile("/a", []byte("a"), 0))
			must(t, f.WriteFile("/b", []byte("b"), 0))
			c, _ := twoSessions(tc.fsys(f))
			st, id := createFile(t, c, "a", dispOpen)
			if st != statusSuccess {
				t.Fatalf("open answered %#x", st)
			}
			if st := renameHandle(t, c, id, `\b`, false); st != statusObjectNameCollision {
				t.Fatalf("no-replace rename onto an existing name answered %#x, want STATUS_OBJECT_NAME_COLLISION", st)
			}
			if got, _ := f.ReadFile("/b"); string(got) != "b" || f.ino("/a") == 0 {
				t.Fatalf("the collision moved something: /a ino %d, /b %q", f.ino("/a"), got)
			}
			if st := renameHandle(t, c, id, `\b`, true); st != statusSuccess {
				t.Fatalf("replacing rename answered %#x", st)
			}
			if got, _ := f.ReadFile("/b"); string(got) != "a" {
				t.Fatalf("after the replacing rename /b is %q", got)
			}
			closeHandle(t, c, id)
		})
	}
}

// A rename through one session moves every open handle of the file on the share,
// another session's included: that handle answers for the file at its new name, never
// for whatever then takes the old one.
// Invariant: I-224
func TestRename_OtherHandlesFollow(t *testing.T) {
	f := newInoFS()
	must(t, f.WriteFile("/a", []byte("the file"), 0))
	c1, c2 := twoSessions(f)
	_, h1 := createFile(t, c1, "a", dispOpen)
	_, h2 := createFile(t, c2, "a", dispOpen)
	if st := renameHandle(t, c2, h2, `\moved`, false); st != statusSuccess {
		t.Fatalf("rename answered %#x", st)
	}
	must(t, f.WriteFile("/a", []byte("a new file"), 0))
	if p := c1.fileByID(h1[:]).p(); p != "/moved" {
		t.Fatalf("the other session's handle names %q, want /moved", p)
	}
	closeHandle(t, c1, h1)
	closeHandle(t, c2, h2)
}

// A delete-on-close removes the file its handle opened or nothing: another session
// renames the file away and creates a new one under its old name before the CLOSE, and
// the CLOSE removes the renamed file (its handle followed it) while the new one stays,
// whether the backend checks the inode or the server does.
// Invariant: I-225
func TestDeleteOnClose_RenamedAway_NewFileSurvives(t *testing.T) {
	for _, tc := range []struct {
		name string
		fsys func(*inoFS) filesystem.Filesystem
	}{
		{"server checks", func(f *inoFS) filesystem.Filesystem { return f }},
		{"backend checks", func(f *inoFS) filesystem.Filesystem { return byInoFS{f} }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newInoFS()
			c1, c2 := twoSessions(tc.fsys(f))
			st, h1 := createFile(t, c1, "copy.bin", dispOpenIf)
			if st != statusSuccess {
				t.Fatalf("create answered %#x", st)
			}
			mine := f.ino("/copy.bin")
			if st := markDelete(t, c1, h1); st != statusSuccess {
				t.Fatalf("delete-on-close answered %#x", st)
			}
			_, h2 := createFile(t, c2, "copy.bin", dispOpen)
			if st := renameHandle(t, c2, h2, `\kept.bin`, false); st != statusSuccess {
				t.Fatalf("rename answered %#x", st)
			}
			closeHandle(t, c2, h2)
			must(t, f.WriteFile("/copy.bin", []byte("someone else's"), 0))
			theirs := f.ino("/copy.bin")
			closeHandle(t, c1, h1)
			if f.ino("/copy.bin") != theirs {
				t.Fatal("the CLOSE deleted the new file that took the old name")
			}
			if f.ino("/kept.bin") == mine {
				t.Fatal("the file the handle opened survived its delete-on-close")
			}
		})
	}
}

func must(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}
