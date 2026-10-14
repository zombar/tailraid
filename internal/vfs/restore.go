package vfs

import (
	"cmp"
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"os"
	"path"
	"slices"
	"strings"
	"syscall"

	"github.com/zombar/tailraid/internal/meta"
	"github.com/zombar/tailraid/internal/mirror"
)

// CommitChunks makes file ino's content exactly size bytes of the chunks hashes, one per
// chunk slot (HashZero a hole), in one Commit that truncates to zero first (docs/16 §6). A
// chunk the pool holds with k readable slots is committed by its hash with no upload
// (L-217); any other is read through fetch, checked against its hash and filled or
// uploaded as a flush would (L-216). It returns how many chunks it uploaded or filled.
func (f *Flusher) CommitChunks(ctx context.Context, actor meta.UserIdx, ino meta.Ino, size uint64, hashes []meta.Hash, fetch func(ctx context.Context, h meta.Hash) ([]byte, error)) (uploaded int, err error) {
	force, noSums := false, false
	for range 16 {
		base, ok := f.st.GetInode(ino)
		if !ok || base.Type != meta.TFile {
			return 0, meta.ErrNotExist
		}
		works := make([]*chunkWork, 0, len(hashes))
		uploaded = 0
		for i, h := range hashes {
			if h == meta.HashZero {
				continue
			}
			w := &chunkWork{idx: uint64(i), hash: h}
			works = append(works, w)
			if c, ok := f.st.GetChunk(h); ok && !force {
				if _, present := f.presentSlots(ctx, c); present >= int(c.K) {
					w.k, w.m, w.place, w.size = c.K, c.M, c.Placement, c.Size
					continue
				}
			}
			data, err := fetch(ctx, h)
			if err != nil {
				return 0, fmt.Errorf("chunk %x: %w", h[:4], err)
			}
			if sha256.Sum256(data) != h {
				return 0, fmt.Errorf("chunk %x: %w", h[:4], mirror.ErrCorrupt)
			}
			w.size = uint32(len(data)) //nolint:gosec // a chunk is at most the chunk size
			if err := f.resolve(ctx, w, data, force); err != nil {
				return 0, err
			}
			uploaded++
		}
		// a truncate to zero, then the chunks: buildCommit's truncate case sizes it to size
		cmd := f.buildCommit(&FlushPlan{Ino: ino, Size: size, SizeSet: true}, base, works, false, noSums)
		pctx, cancel := context.WithTimeout(ctx, f.cfg.ProposeTimeout)
		res, index, perr := f.prop.Propose(pctx, f.actorCommand(actor, meta.Command{Op: meta.OpCommit, Commit: cmd}))
		cancel()
		switch {
		case perr != nil:
			return 0, fmt.Errorf("propose commit: %w", perr)
		case res.Err == nil:
			return uploaded, f.st.WaitApplied(ctx, index)
		case errors.Is(res.Err, meta.ErrConflict):
			continue
		case errors.Is(res.Err, meta.ErrUnknownChunk): // a dedup'd chunk was collected meanwhile
			force = true
			continue
		case errors.Is(res.Err, meta.ErrCodec) && !noSums && carriesSums(cmd):
			noSums = true // the leader sees an older member than this gateway does (I-219)
			continue
		default:
			return 0, fmt.Errorf("commit rejected: %w", res.Err)
		}
	}
	return 0, errRebaseLimit
}

// snapEntry resolves src, a path under /.snapshots inside a snapshot, to its mirror and
// entry.
func (f *FS) snapEntry(ctx context.Context, src string) (meta.NodeIdx, mirror.Entry, error) {
	parts := meta.SplitPath(src)
	if !isSnapshotPath(parts) || len(parts) < 3 {
		return 0, mirror.Entry{}, mapErr("restore", src, meta.ErrNotExist)
	}
	n, err := f.snapLookup(ctx, parts)
	if err != nil {
		return 0, mirror.Entry{}, snapErr("restore", src, err)
	}
	return n.row.Idx, n.e, nil
}

// writeEntry makes the live file at ino hold entry e's bytes from mirror m.
func (f *FS) writeEntry(ctx context.Context, ino meta.Ino, m meta.NodeIdx, e *mirror.Entry) (int, error) {
	if f.fl == nil {
		return 0, errors.New("restore: no flusher")
	}
	if err := f.fl.FlushInode(ctx, ino); err != nil { // the file's dirty bytes land first (I-186)
		return 0, err
	}
	return f.fl.CommitChunks(ctx, f.actor, ino, e.Size, e.Chunks, func(ctx context.Context, h meta.Hash) ([]byte, error) {
		return f.snapChunk(ctx, m, h)
	})
}

// RevertFile makes the live file at dst hold the bytes of the snapshot file src (a
// /.snapshots path), keeping the file's inode, name and attributes (docs/16 §6, I-186);
// uploaded counts the chunks copied back from the mirror.
func (f *FS) RevertFile(ctx context.Context, src, dst string) (uploaded int, err error) {
	m, e, err := f.snapEntry(ctx, src)
	if err != nil {
		return 0, err
	}
	if e.Type != mirror.TypeFile {
		return 0, mapErr("revert", src, meta.ErrIsDir)
	}
	if isSnapshotPath(meta.SplitPath(dst)) {
		return 0, mapErr("revert", dst, ErrReadOnly)
	}
	in, err := f.resolve(dst)
	if err != nil {
		return 0, mapErr("revert", dst, err)
	}
	if in.Type != meta.TFile {
		return 0, mapErr("revert", dst, meta.ErrIsDir)
	}
	uploaded, err = f.writeEntry(ctx, in.Ino, m, &e)
	return uploaded, mapErr("revert", dst, err)
}

// RestoreReport is what a restore did: entries created, chunks uploaded from the mirror,
// and the entries left alone because the target existed.
type RestoreReport struct {
	Files    int      `json:"files"`
	Dirs     int      `json:"dirs"`
	Links    int      `json:"links"`
	Uploaded int      `json:"uploaded"`
	Skipped  []string `json:"skipped,omitempty"`
}

// RestoreSnapshot copies the snapshot entry src (a /.snapshots path: a file, a symlink or a
// directory) to dst in the live volume. A file or symlink must not exist at dst; a
// directory is merged: an existing directory is descended, an existing file or symlink is
// skipped and reported, and .snapshots and .tailraid are never created at the root
// (docs/16 §6, I-186).
func (f *FS) RestoreSnapshot(ctx context.Context, src, dst string) (RestoreReport, error) {
	var rep RestoreReport
	m, e, err := f.snapEntry(ctx, src)
	if err != nil {
		return rep, err
	}
	dparts := meta.SplitPath(dst)
	if isSnapshotPath(dparts) || (len(dparts) > 0 && dparts[0] == meta.ReservedDir) {
		return rep, mapErr("restore", dst, ErrReadOnly)
	}
	if e.Type != mirror.TypeDir {
		if _, err := f.resolve(dst); err == nil {
			return rep, mapErr("restore", dst, meta.ErrExist)
		}
	}
	err = f.restoreEntry(ctx, m, &e, dparts, &rep)
	return rep, err
}

func (f *FS) restoreEntry(ctx context.Context, m meta.NodeIdx, e *mirror.Entry, dst []string, rep *RestoreReport) error {
	name := "/" + path.Join(dst...)
	if len(dst) == 1 && (dst[0] == SnapshotDir || dst[0] == meta.ReservedDir) {
		rep.Skipped = append(rep.Skipped, name)
		return nil
	}
	cur, err := f.st.Resolve(dst)
	exists := err == nil
	switch e.Type {
	case mirror.TypeDir:
		if exists && cur.Type != meta.TDir {
			rep.Skipped = append(rep.Skipped, name)
			return nil
		}
		if !exists {
			if err := f.MkdirAll(name, os.FileMode(e.Mode&0o7777)); err != nil {
				return err
			}
			rep.Dirs++
		}
		t, err := f.snapTree(ctx, m, e.Child)
		if err != nil {
			return snapErr("restore", name, err)
		}
		for i := range t.Entries {
			if err := f.restoreEntry(ctx, m, &t.Entries[i], append(slices.Clip(dst), t.Entries[i].Name), rep); err != nil {
				return err
			}
		}
		return nil
	case mirror.TypeSymlink:
		if exists {
			rep.Skipped = append(rep.Skipped, name)
			return nil
		}
		if err := f.Symlink(e.Target, name); err != nil {
			return err
		}
		rep.Links++
		return nil
	case mirror.TypeFile:
	}
	if exists {
		rep.Skipped = append(rep.Skipped, name)
		return nil
	}
	fh, err := f.OpenFile(name, os.O_RDWR|os.O_CREATE|os.O_EXCL, os.FileMode(e.Mode&0o777))
	if err != nil {
		return err
	}
	ino := fh.(*File).ino
	_ = fh.Close()
	n, err := f.writeEntry(ctx, ino, m, e)
	if err != nil {
		return mapErr("restore", name, err)
	}
	rep.Files++
	rep.Uploaded += n
	return nil
}

// VersionSnap is one snapshot holding a version.
type VersionSnap struct {
	Mirror string      `json:"mirror"`
	Name   string      `json:"name"`
	ID     meta.SnapID `json:"id"`
	At     int64       `json:"at"`
}

// Version is one distinct content of a file across the snapshots, newest first; Current
// is the live content.
type Version struct {
	Current   bool          `json:"current,omitempty"`
	Size      uint64        `json:"size"`
	Mtime     int64         `json:"mtime"`
	Snapshots []VersionSnap `json:"snapshots"`
}

// OfflineMirror counts the snapshots of a mirror the versions could not look into.
type OfflineMirror struct {
	Mirror    string `json:"mirror"`
	Snapshots int    `json:"snapshots"`
}

// Versions lists the versions of the live file at p (docs/16 §6): the path is resolved
// in every snapshot of every online mirror, snapshots whose entry has the same chunk list
// are one version, and the live content comes first, with the snapshots that hold it (or
// none: "in no snapshot yet"). Versions follow the path, not the inode.
func (f *FS) Versions(ctx context.Context, p string) ([]Version, []OfflineMirror, error) {
	parts := meta.SplitPath(p)
	if len(parts) == 0 || isSnapshotPath(parts) {
		return nil, nil, mapErr("versions", p, syscall.EINVAL)
	}
	type group struct {
		v   Version
		key string
	}
	var groups []*group
	byKey := map[string]*group{}
	key := func(size uint64, hs []meta.Hash) string {
		var b strings.Builder
		fmt.Fprintf(&b, "%d", size)
		for _, h := range hs {
			b.Write(h[:])
		}
		return b.String()
	}
	in, err := f.resolve(p)
	if err == nil && in.Type == meta.TFile {
		if _, dirty := f.ov.Size(in.Ino, in.Size); !dirty {
			g := &group{v: Version{Current: true, Size: in.Size, Mtime: in.Mtime, Snapshots: []VersionSnap{}}, key: key(in.Size, in.Chunks)}
			groups, byKey[g.key] = append(groups, g), g
		} else {
			groups = append(groups, &group{v: Version{Current: true, Size: f.info(in).size, Mtime: in.Mtime, Snapshots: []VersionSnap{}}})
		}
	}
	var offline []OfflineMirror
	rows := f.mirrorRows()
	for i := range rows {
		dir := mirrorDirName(&rows[i])
		sns := f.snapshotsOf(rows[i].Idx)
		if f.snaps.online(rows[i].Idx) == nil {
			if len(sns) > 0 {
				offline = append(offline, OfflineMirror{Mirror: dir, Snapshots: len(sns)})
			}
			continue
		}
		for j := range sns {
			n, err := f.snapLookup(ctx, append([]string{SnapshotDir, dir, sns[j].Name}, parts...))
			if err != nil || n.e.Type != mirror.TypeFile {
				continue
			}
			k := key(n.e.Size, n.e.Chunks)
			g := byKey[k]
			if g == nil {
				g = &group{v: Version{Size: n.e.Size, Mtime: n.e.Mtime}, key: k}
				groups, byKey[k] = append(groups, g), g
			}
			g.v.Snapshots = append(g.v.Snapshots, VersionSnap{Mirror: dir, Name: sns[j].Name, ID: sns[j].ID, At: sns[j].At})
		}
	}
	newest := func(g *group) int64 {
		var at int64
		for _, s := range g.v.Snapshots {
			at = max(at, s.At)
		}
		return at
	}
	slices.SortStableFunc(groups, func(a, b *group) int {
		switch {
		case a.v.Current != b.v.Current:
			if a.v.Current {
				return -1
			}
			return 1
		case newest(a) != newest(b):
			if newest(a) > newest(b) {
				return -1
			}
			return 1
		}
		return 0
	})
	out := make([]Version, len(groups))
	for i, g := range groups {
		slices.SortFunc(g.v.Snapshots, func(a, b VersionSnap) int { return cmp.Compare(b.At, a.At) })
		out[i] = g.v
	}
	return out, offline, nil
}
