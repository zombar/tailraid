// Package smbclient is the SMB client side of the soak suite's copy jobs and handle
// races (docs/09 §6, phases 22 and 23), shared by the compose worker and the in-process
// harness. It lives apart from internal/soak so the tailraid binary, which runs the
// soak phases for `verify`, does not carry an SMB client.
package smbclient

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"slices"
	"strings"
	"sync"

	"github.com/cloudsoda/go-smb2"

	"github.com/zombar/tailraid/internal/soak"
)

// Dial opens one session to the gateway's share; the func it returns ends it.
type Dial func(ctx context.Context) (*smb2.Share, func(), error)

// Copy copies files the way Explorer does: one session per distinct CopySpec.Conn, all
// files at once, each opened by its Mode, sized to its final length first and written
// front to back in CopyPiece writes. It returns once every copy has ended; a session it
// could not open fails that session's files.
func Copy(ctx context.Context, dial Dial, files []soak.CopySpec) []soak.CopyResult {
	shares := map[int]*smb2.Share{}
	errs := map[int]error{}
	var closers []func()
	for _, f := range files {
		if _, ok := shares[f.Conn]; ok || errs[f.Conn] != nil {
			continue
		}
		sh, end, err := dial(ctx)
		if err != nil {
			errs[f.Conn] = err
			continue
		}
		shares[f.Conn] = sh
		closers = append(closers, end)
	}
	defer func() {
		for _, end := range closers {
			end()
		}
	}()
	results := make([]soak.CopyResult, len(files))
	var wg sync.WaitGroup
	for i, f := range files {
		wg.Go(func() {
			results[i] = soak.CopyResult{Path: f.Path}
			err := errs[f.Conn]
			if err == nil {
				err = copyOne(shares[f.Conn], f)
			}
			if err != nil {
				results[i].Err = err.Error()
				results[i].Exists = errors.Is(err, os.ErrExist)
			}
		})
	}
	wg.Wait()
	return results
}

func copyOne(sh *smb2.Share, f soak.CopySpec) error {
	flag := os.O_RDWR | os.O_CREATE | os.O_EXCL
	if f.Mode == soak.CopyReplace {
		flag = os.O_RDWR | os.O_CREATE | os.O_TRUNC
	}
	h, err := sh.OpenFile(strings.TrimPrefix(f.Path, "/"), flag, 0o644)
	if err != nil {
		return err
	}
	err = Pieces(f, h.Truncate, h.WriteAt)
	return errors.Join(err, h.Close())
}

// Pieces is the copy engine's sequence on an open handle: the final length first, then
// the bytes front to back in CopyPiece writes.
func Pieces(f soak.CopySpec, truncate func(int64) error, writeAt func([]byte, int64) (int, error)) error {
	if err := truncate(int64(f.Size)); err != nil {
		return err
	}
	data := soak.CopyPayload(f.Size, f.Seed)
	for off := 0; off < len(data); off += soak.CopyPiece {
		if _, err := writeAt(data[off:min(off+soak.CopyPiece, len(data))], int64(off)); err != nil {
			return fmt.Errorf("write @%d: %w", off, err)
		}
	}
	return nil
}

// Races runs phase 23's SMB handle races (round g) in dir, two sessions of one gateway
// against each other, and reports each as a check: a delete-on-close removes the file
// its handle opened, not the one that took its old name (I-225, with the own cleanup
// of I-220); a rename without replace onto an existing name is refused and moves
// nothing (I-224); a rename through one session moves the other session's open handle
// (I-224).
func Races(ctx context.Context, dial Dial, dir string) []soak.RaceResult {
	s1, end1, err := dial(ctx)
	if err != nil {
		return []soak.RaceResult{{Name: "sessions", Detail: err.Error()}}
	}
	defer end1()
	s2, end2, err := dial(ctx)
	if err != nil {
		return []soak.RaceResult{{Name: "sessions", Detail: err.Error()}}
	}
	defer end2()
	dir = strings.TrimPrefix(dir, "/")
	return []soak.RaceResult{
		raceDeleteOnClose(s1, s2, dir),
		raceNoReplace(s1, s2, dir),
		raceHandleFollows(s1, s2, dir),
	}
}

func raceDeleteOnClose(s1, s2 *smb2.Share, dir string) soak.RaceResult {
	r := soak.RaceResult{Name: "a delete-on-close removes the file its handle opened (I-225)"}
	copyName, kept := dir+`/copy.bin`, dir+`/kept.bin`
	h1, err := s1.OpenFile(copyName, os.O_RDWR|os.O_CREATE|os.O_EXCL, 0o644)
	if err != nil {
		r.Detail = "create: " + err.Error()
		return r
	}
	if _, err := h1.WriteString("a failed copy"); err != nil {
		r.Detail = "write: " + err.Error()
		return r
	}
	if err := h1.SetDeleteOnClose(); err != nil {
		r.Detail = "mark delete-on-close: " + err.Error()
		return r
	}
	// the other client renames it away and copies a file of its own in under the name
	h2, err := s2.OpenFile(copyName, os.O_RDWR, 0)
	if err != nil {
		r.Detail = "other open: " + err.Error()
		return r
	}
	if err := h2.RenameTo(kept, false); err != nil {
		r.Detail = "other rename: " + err.Error()
		return r
	}
	_ = h2.Close()
	if err := s2.WriteFile(copyName, []byte("someone else's file"), 0o644); err != nil {
		r.Detail = "other create: " + err.Error()
		return r
	}
	if err := h1.Close(); err != nil {
		r.Detail = "close: " + err.Error()
		return r
	}
	got, err := s2.ReadFile(copyName)
	switch {
	case err != nil:
		r.Detail = "the other client's file went: " + err.Error()
	case string(got) != "someone else's file":
		r.Detail = fmt.Sprintf("the name holds %q", got)
	default:
		if _, err := s2.Stat(kept); !errors.Is(err, os.ErrNotExist) {
			r.Detail = fmt.Sprintf("the file the handle opened survived its delete-on-close (%v)", err)
			return r
		}
		r.OK = true
	}
	return r
}

func raceNoReplace(s1, s2 *smb2.Share, dir string) soak.RaceResult {
	r := soak.RaceResult{Name: "a rename without replace onto an existing name is refused (I-224)"}
	a, b := dir+`/a.bin`, dir+`/b.bin`
	if err := s1.WriteFile(a, []byte("a"), 0o644); err != nil {
		r.Detail = err.Error()
		return r
	}
	if err := s2.WriteFile(b, []byte("b, the other client's"), 0o644); err != nil {
		r.Detail = err.Error()
		return r
	}
	h, err := s1.OpenFile(a, os.O_RDWR, 0)
	if err != nil {
		r.Detail = err.Error()
		return r
	}
	rerr := h.RenameTo(b, false)
	_ = h.Close()
	got, err := s2.ReadFile(b)
	switch {
	case rerr == nil:
		r.Detail = "the rename replaced the other client's file"
	case err != nil || string(got) != "b, the other client's":
		r.Detail = fmt.Sprintf("the other client's file is now %q (%v)", got, err)
	default:
		if _, err := s1.Stat(a); err != nil {
			r.Detail = "the refused rename moved the file: " + err.Error()
			return r
		}
		r.OK = true
	}
	return r
}

func raceHandleFollows(s1, s2 *smb2.Share, dir string) soak.RaceResult {
	r := soak.RaceResult{Name: "a rename moves the other session's open handle (I-224)"}
	c, moved := dir+`/c.bin`, dir+`/moved.bin`
	orig := soak.CopyPayload(3000, 23)
	if err := s1.WriteFile(c, orig, 0o644); err != nil {
		r.Detail = err.Error()
		return r
	}
	h1, err := s1.OpenFile(c, os.O_RDONLY, 0)
	if err != nil {
		r.Detail = err.Error()
		return r
	}
	defer h1.Close()
	if err := s2.Rename(c, moved); err != nil {
		r.Detail = "rename: " + err.Error()
		return r
	}
	if err := s2.WriteFile(c, soak.CopyPayload(9000, 24), 0o644); err != nil {
		r.Detail = "new file: " + err.Error()
		return r
	}
	st, err := h1.Stat()
	if err != nil {
		r.Detail = "stat: " + err.Error()
		return r
	}
	got := make([]byte, len(orig))
	if _, err := h1.ReadAt(got, 0); err != nil && !errors.Is(err, io.EOF) {
		r.Detail = "read: " + err.Error()
		return r
	}
	if st.Size() != int64(len(orig)) || !slices.Equal(got, orig) {
		r.Detail = fmt.Sprintf("the open handle answers for another file: %d bytes, want %d", st.Size(), len(orig))
		return r
	}
	r.OK = true
	return r
}

// Hold creates path through a session of its own, writes to it, marks it delete-on-close
// and keeps it open, as a copy engine holds the file it is about to clean up; the func it
// returns closes the handle (the delete happens then) and the session. Phase 23 renames
// and replaces the file through another gateway in between (I-225).
func Hold(ctx context.Context, dial Dial, path string) (release func() error, err error) {
	sh, end, err := dial(ctx)
	if err != nil {
		return nil, err
	}
	h, err := sh.OpenFile(strings.TrimPrefix(path, "/"), os.O_RDWR|os.O_CREATE|os.O_EXCL, 0o644)
	if err != nil {
		end()
		return nil, fmt.Errorf("create: %w", err)
	}
	if _, err := h.WriteString("a failed copy"); err != nil {
		_ = h.Close()
		end()
		return nil, fmt.Errorf("write: %w", err)
	}
	if err := h.SetDeleteOnClose(); err != nil {
		_ = h.Close()
		end()
		return nil, fmt.Errorf("mark delete-on-close: %w", err)
	}
	return func() error {
		defer end()
		return h.Close()
	}, nil
}
