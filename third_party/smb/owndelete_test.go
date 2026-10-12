package smb

import (
	"encoding/binary"
	"os"
	"testing"

	filesystem "github.com/go-filesystems/interface"
)

// guardedFS is a root of plain files that refuses every plain delete, as a protected
// folder does, and lets the handle that created a file delete it (OwnDeleter), recording
// what it was asked.
type guardedFS struct {
	nothingFS
	files                  map[string][]byte
	ownChecks, plainChecks []string
	ownDeletes             []string
}

func (g *guardedFS) Stat(p string) (filesystem.Stat, error) {
	if p == "/" {
		return filesystem.NewStat(0o040755, 0, 1), nil
	}
	if b, ok := g.files[p]; ok {
		return filesystem.NewStat(0o100644, uint64(len(b)), 2), nil
	}
	return nil, os.ErrNotExist
}
func (g *guardedFS) WriteFile(p string, d []byte, _ os.FileMode) error {
	g.files[p] = append([]byte(nil), d...)
	return nil
}

func (g *guardedFS) CanDelete(p string) error {
	g.plainChecks = append(g.plainChecks, p)
	return os.ErrPermission
}
func (g *guardedFS) DeleteFile(string) error { return os.ErrPermission }
func (g *guardedFS) CanDeleteOwn(p string) error {
	g.ownChecks = append(g.ownChecks, p)
	return nil
}
func (g *guardedFS) DeleteOwnFile(p string) error {
	g.ownDeletes = append(g.ownDeletes, p)
	delete(g.files, p)
	return nil
}

func createFile(t *testing.T, c *conn, name string, disp uint32) (uint32, [16]byte) {
	t.Helper()
	n := utf16le(name)
	body := make([]byte, 56)
	binary.LittleEndian.PutUint32(body[36:], disp)
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

func markDelete(t *testing.T, c *conn, id [16]byte) uint32 {
	t.Helper()
	body := make([]byte, 32)
	body[2], body[3] = infoTypeFile, fileDispositionInformation
	binary.LittleEndian.PutUint32(body[4:], 1)
	binary.LittleEndian.PutUint16(body[8:], uint16(headerLen+32))
	copy(body[16:], id[:])
	out, err := c.dispatch(request(cmdSetInfo, 1, append(body, 1)))
	if err != nil {
		t.Fatal(err)
	}
	return statusOf(t, out)
}

func closeHandle(t *testing.T, c *conn, id [16]byte) {
	t.Helper()
	body := make([]byte, 24)
	binary.LittleEndian.PutUint16(body[0:], 24)
	copy(body[8:], id[:])
	if _, err := c.dispatch(request(cmdClose, 1, body)); err != nil {
		t.Fatal(err)
	}
}

// A failed copy's cleanup: the handle whose CREATE made the file marks it delete-on-close
// and closes it, and the backend's own-delete removes it where a plain delete is refused.
// A handle that opened an existing file is asked the plain question and refused.
// Invariant: I-220
func TestDeleteOnClose_OwnCreationIsOwnCleanup(t *testing.T) {
	g := &guardedFS{files: map[string][]byte{}}
	c, _ := opened(t, g, "/", false)
	st, id := createFile(t, c, "copy.bin", dispOpenIf)
	if st != statusSuccess {
		t.Fatalf("create answered %#x", st)
	}
	if st := markDelete(t, c, id); st != statusSuccess {
		t.Fatalf("marking its own file answered %#x", st)
	}
	closeHandle(t, c, id)
	if len(g.ownChecks) != 1 || len(g.ownDeletes) != 1 || g.ownDeletes[0] != "/copy.bin" {
		t.Fatalf("own checks %v, own deletes %v", g.ownChecks, g.ownDeletes)
	}
	if _, err := g.Stat("/copy.bin"); err == nil {
		t.Fatal("the failed copy's file is still there")
	}
	// an existing file, opened rather than created, is refused
	g.files["/kept.bin"] = []byte("x")
	st, id = createFile(t, c, "kept.bin", dispOpenIf)
	if st != statusSuccess {
		t.Fatalf("open answered %#x", st)
	}
	if st := markDelete(t, c, id); st != statusAccessDenied {
		t.Fatalf("marking an existing file answered %#x, want STATUS_ACCESS_DENIED", st)
	}
	closeHandle(t, c, id)
	if _, err := g.Stat("/kept.bin"); err != nil || len(g.ownDeletes) != 1 {
		t.Fatalf("the existing file went: %v, own deletes %v", err, g.ownDeletes)
	}
}
