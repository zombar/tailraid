package vfs

import (
	"cmp"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
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
	return f.commitChunks(ctx, actor, ino, size, hashes, fetch, nil, nil)
}

// commitChunks is CommitChunks with its chunks resolved at once, each holding a place
// in slots (nil: one at a time), and done told the bytes each chunk slot covers the
// first time it is decided, a hole at once (I-365, I-366).
func (f *Flusher) commitChunks(ctx context.Context, actor meta.UserIdx, ino meta.Ino, size uint64, hashes []meta.Hash, fetch func(ctx context.Context, h meta.Hash) ([]byte, error), slots *gate, done func(int64)) (uploaded int, err error) {
	spans := chunkSpans(size, len(hashes), uint64(f.st.Volume().ChunkSize))
	counted := make([]bool, len(hashes)) // per index: each written by its own goroutine
	report := func(i int) {
		if done != nil && !counted[i] {
			counted[i] = true
			done(spans[i])
		}
	}
	workers := 1
	if slots != nil {
		workers = slots.full
	}
	force, noSums := false, false
	for range 16 {
		base, ok := f.st.GetInode(ino)
		if !ok || base.Type != meta.TFile {
			return 0, meta.ErrNotExist
		}
		slot := make([]*chunkWork, len(hashes))
		var up atomic.Int64
		perr := parallel(ctx, workers, len(hashes), func(i int) error {
			h := hashes[i]
			if h == meta.HashZero {
				report(i)
				return nil
			}
			if slots != nil {
				if err := slots.acquire(ctx); err != nil {
					return err
				}
				defer slots.release()
			}
			w := &chunkWork{idx: uint64(i), hash: h}
			slot[i] = w
			if c, ok := f.st.GetChunk(h); ok && !force {
				if _, present := f.presentSlots(ctx, c); present >= int(c.K) {
					w.k, w.m, w.place, w.size = c.K, c.M, c.Placement, c.Size
					report(i)
					return nil
				}
			}
			data, err := fetch(ctx, h)
			if err != nil {
				return fmt.Errorf("chunk %x: %w", h[:4], err)
			}
			if sha256.Sum256(data) != h {
				return fmt.Errorf("chunk %x: %w", h[:4], mirror.ErrCorrupt)
			}
			w.size = uint32(len(data)) //nolint:gosec // a chunk is at most the chunk size
			if err := f.resolve(ctx, w, data, force); err != nil {
				return err
			}
			up.Add(1)
			report(i)
			return nil
		})
		if perr != nil {
			return 0, perr
		}
		works := make([]*chunkWork, 0, len(hashes))
		for _, w := range slot {
			if w != nil {
				works = append(works, w)
			}
		}
		uploaded = int(up.Load())
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

// chunkSpans is how many bytes each of n chunk slots of a size-byte file covers: cs each
// but the last, or, where the file's chunk list does not fit cs (a snapshot taken at
// another chunk size), size spread over the n slots.
func chunkSpans(size uint64, n int, cs uint64) []int64 {
	if n == 0 {
		return nil
	}
	if cs == 0 || uint64(n-1)*cs >= size || uint64(n)*cs < size { //nolint:gosec // n is a chunk count
		cs = (size + uint64(n) - 1) / uint64(n) //nolint:gosec // n is a chunk count
	}
	out := make([]int64, n)
	left := size
	for i := range out {
		s := min(cs, left)
		out[i] = int64(s) //nolint:gosec // at most the chunk size
		left -= s
	}
	return out
}

// snapEntry resolves src, a path under /.snapshots inside a snapshot, to its node, with
// the actor's right to read it checked (I-348).
func (f *FS) snapEntry(ctx context.Context, src string) (snapNode, error) {
	parts := meta.SplitPath(src)
	if !isSnapshotPath(parts) || len(parts) < 3 {
		return snapNode{}, mapErr("restore", src, meta.ErrNotExist)
	}
	n, err := f.snapLookup(ctx, parts)
	if err != nil {
		return snapNode{}, snapErr("restore", src, err)
	}
	if err := f.checkSnap(&n); err != nil {
		return snapNode{}, mapErr("restore", src, f.refuse("read", src))
	}
	return n, nil
}

// writeEntry makes the live file at ino hold entry e's bytes from mirror m, its chunks
// holding slots and told to done as commitChunks does (nil, nil: one at a time).
func (f *FS) writeEntry(ctx context.Context, ino meta.Ino, m meta.NodeIdx, e *mirror.Entry, slots *gate, done func(int64)) (int, error) {
	if f.fl == nil {
		return 0, errors.New("restore: no flusher")
	}
	if err := f.fl.FlushInode(ctx, ino); err != nil { // the file's dirty bytes land first (I-186)
		return 0, err
	}
	return f.fl.commitChunks(ctx, f.actor, ino, e.Size, e.Chunks, func(ctx context.Context, h meta.Hash) ([]byte, error) {
		return f.snapChunk(ctx, m, h)
	}, slots, done)
}

// RevertFile makes the live file at dst hold the bytes of the snapshot file src (a
// /.snapshots path), keeping the file's inode, name and attributes (docs/16 §6, I-186);
// uploaded counts the chunks copied back from the mirror. The actor reads the snapshot
// entry and holds Admin on the live file (docs/21 §3, I-348).
func (f *FS) RevertFile(ctx context.Context, src, dst string) (uploaded int, err error) {
	n, err := f.snapEntry(ctx, src)
	if err != nil {
		return 0, err
	}
	if n.e.Type != mirror.TypeFile {
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
	if err := f.checkAdmin(in.Ino, dst); err != nil {
		return 0, mapErr("revert", dst, err)
	}
	uploaded, err = f.writeEntry(ctx, in.Ino, n.row.Idx, &n.e, nil, nil)
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
	Refused  []string `json:"refused,omitempty"` // entries the actor may not read (I-348)
}

// The bounds of a tree restore job (docs/16 §6, I-366): the files it writes at once, and
// the chunks being read and written at once across the whole restore.
const (
	RestoreFiles    = 4
	RestoreInFlight = 8
	RestoreYield    = 2 // the chunks in flight while RestoreOptions.Yield says so, one file at a time (I-372)
)

// RestoreProgress is one step of a tree restore, added to its done counts: a chunk's bytes
// decided, a file renamed into place (Files 1, its bytes counted by its chunks), or a file
// that was already there (Files 1, Bytes its size, Existing).
type RestoreProgress struct {
	Files, Bytes int64
	Existing     bool
}

// RestoreOptions shapes a tree restore: Files files at once and InFlight chunks across it
// (under 1: one), and Progress told every step, one call at a time (nil: none). While
// Yield answers true (nil: never), one file and at most RestoreYield chunks run, the
// width read again before each file and chunk starts (I-372).
type RestoreOptions struct {
	Files, InFlight int
	Progress        func(RestoreProgress)
	Yield           func() bool
}

// RestoreSnapshot copies the snapshot entry src (a /.snapshots path: a file, a symlink or a
// directory) to dst in the live volume. A file or symlink must not exist at dst; a
// directory is merged: an existing directory is descended, an existing file or symlink is
// skipped and reported, and .snapshots and .tailraid are never created at the root
// (docs/16 §6, I-186). The actor holds Admin on the directory dst lands in, and the copy
// holds the entries the actor may read, the rest listed as refused (docs/21 §3, I-348).
// It writes one file and one chunk at a time; RestoreSnapshotWith takes more.
func (f *FS) RestoreSnapshot(ctx context.Context, src, dst string) (RestoreReport, error) {
	return f.RestoreSnapshotWith(ctx, src, dst, RestoreOptions{})
}

// RestoreSnapshotWith is RestoreSnapshot writing opt.Files files and opt.InFlight chunks at
// once and telling opt.Progress each step: the folders and links in walk order, the files
// beside them, each under its temporary name until committed, so the tree it leaves is the
// one a restore one chunk at a time leaves (I-365, I-366). A cancelled ctx stops it before
// the next chunk, and the file it was writing leaves no temporary file (I-368).
func (f *FS) RestoreSnapshotWith(ctx context.Context, src, dst string, opt RestoreOptions) (RestoreReport, error) {
	var rep RestoreReport
	n, err := f.snapEntry(ctx, src)
	if err != nil {
		return rep, err
	}
	if err := f.CheckRestoreInto(dst); err != nil {
		return rep, err
	}
	dparts := meta.SplitPath(dst)
	if n.e.Type != mirror.TypeDir {
		if _, err := f.resolve(dst); err == nil {
			return rep, mapErr("restore", dst, meta.ErrExist)
		}
	}
	full := max(1, opt.InFlight)
	r := &restoreRun{
		f: f, opt: opt, rep: &rep,
		slots: newGate(full, min(full, RestoreYield), opt.Yield),
		files: newGate(max(1, opt.Files), 1, opt.Yield),
	}
	err = r.run(ctx, &n, dparts)
	return rep, err
}

// CheckRestoreInto refuses a restore to dst the view may not make: under /.snapshots or
// /.tailraid, or without Admin on the nearest existing ancestor of dst, since the folders
// below it are made by the restore (the root, for a merge into it) (docs/21 §3, I-348).
// The web UI checks it before it hands a restore to the mirror's host (docs/13 §3).
func (f *FS) CheckRestoreInto(dst string) error {
	dparts := meta.SplitPath(dst)
	if isSnapshotPath(dparts) || (len(dparts) > 0 && dparts[0] == meta.ReservedDir) {
		return mapErr("restore", dst, ErrReadOnly)
	}
	into, err := f.nearest(dparts[:max(len(dparts)-1, 0)])
	if err != nil {
		return mapErr("restore", dst, err)
	}
	if err := f.checkAdmin(into.Ino, dst); err != nil {
		return mapErr("restore", dst, err)
	}
	return nil
}

// RestoreTotals is what a tree restore of src into dst writes when nothing is there yet:
// its files and their bytes, leaving out what the restore leaves out (an entry the actor
// may not read, .snapshots and .tailraid at the root). It reads the mirror's trees past
// the gateway's tree cache, which one walk of a large snapshot would only churn (I-365).
func (f *FS) RestoreTotals(ctx context.Context, src, dst string) (files, bytes int64, err error) {
	n, err := f.snapEntry(ctx, src)
	if err != nil {
		return 0, 0, err
	}
	return f.snapTotals(ctx, &n, meta.SplitPath(dst))
}

// snapTotals counts the files under n and their bytes, as a restore to dst would write them.
func (f *FS) snapTotals(ctx context.Context, n *snapNode, dst []string) (files, bytes int64, err error) {
	if leftOut(dst) {
		return 0, 0, nil
	}
	switch n.e.Type {
	case mirror.TypeFile:
		return 1, int64(n.e.Size), nil //nolint:gosec // a file size
	case mirror.TypeDir:
	default:
		return 0, 0, nil
	}
	if err := ctx.Err(); err != nil {
		return 0, 0, err
	}
	src := f.snaps.online(n.row.Idx)
	if src == nil {
		return 0, 0, ErrMirrorOffline
	}
	t, err := src.Tree(ctx, n.row.Idx, n.e.Child)
	if err != nil {
		return 0, 0, err
	}
	for i := range t.Entries {
		child := snapChild(n, &t.Entries[i])
		if f.checkSnap(&child) != nil {
			continue
		}
		cf, cb, err := f.snapTotals(ctx, &child, append(slices.Clip(dst), t.Entries[i].Name))
		if err != nil {
			return 0, 0, err
		}
		files, bytes = files+cf, bytes+cb
	}
	return files, bytes, nil
}

// leftOut reports a name a restore never makes: .snapshots and .tailraid at the root.
func leftOut(dst []string) bool {
	return len(dst) == 1 && (dst[0] == SnapshotDir || dst[0] == meta.ReservedDir)
}

// nearest is the deepest existing ancestor of parts (the root at least).
func (f *FS) nearest(parts []string) (meta.Inode, error) {
	for i := len(parts); ; i-- {
		in, err := f.st.Resolve(parts[:i])
		if err == nil || i == 0 {
			return in, err
		}
	}
}

// restoreRun is one tree restore: the walk makes folders and links and decides what is
// already there, and opt.Files workers write the files it hands them (I-366).
type restoreRun struct {
	f     *FS
	opt   RestoreOptions
	slots *gate      // the chunks in flight across the restore
	files *gate      // the files being written
	mu    sync.Mutex // rep and opt.Progress
	rep   *RestoreReport
}

// fileTask is one file the walk found missing: the snapshot entry and its live path.
type fileTask struct {
	n   snapNode
	dst []string
}

func (r *restoreRun) progress(p RestoreProgress) {
	if r.opt.Progress == nil {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.opt.Progress(p)
}

func (r *restoreRun) note(fn func(rep *RestoreReport)) {
	r.mu.Lock()
	defer r.mu.Unlock()
	fn(r.rep)
}

// run walks n into dst, the files written by the workers; the first error stops the rest.
func (r *restoreRun) run(ctx context.Context, n *snapNode, dst []string) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	var (
		wg    sync.WaitGroup
		mu    sync.Mutex
		first error
	)
	fail := func(err error) {
		mu.Lock()
		defer mu.Unlock()
		if first == nil {
			first = err
			cancel()
		}
	}
	tasks := make(chan fileTask)
	for range max(1, r.opt.Files) {
		wg.Go(func() {
			for t := range tasks {
				if ctx.Err() != nil {
					continue // drained: the walk is stopping
				}
				if err := r.files.acquire(ctx); err != nil {
					continue // stopping: the walk sees ctx too
				}
				err := r.file(ctx, &t)
				r.files.release()
				if err != nil {
					fail(err)
				}
			}
		})
	}
	werr := r.walk(ctx, n, dst, func(t fileTask) error {
		select {
		case tasks <- t:
			return nil
		case <-ctx.Done():
			return ctx.Err()
		}
	})
	close(tasks)
	wg.Wait()
	mu.Lock()
	defer mu.Unlock()
	if first != nil {
		return first
	}
	return werr
}

// walk restores the snapshot node n at dst: a folder made or descended, a link made, a
// missing file handed to give; what exists is skipped and counted, a child the actor may
// not read listed as refused and left out.
func (r *restoreRun) walk(ctx context.Context, n *snapNode, dst []string, give func(fileTask) error) error {
	f := r.f
	m, e := n.row.Idx, &n.e
	name := "/" + path.Join(dst...)
	if leftOut(dst) {
		r.note(func(rep *RestoreReport) { rep.Skipped = append(rep.Skipped, name) })
		return nil
	}
	cur, err := f.st.Resolve(dst)
	exists := err == nil
	switch e.Type {
	case mirror.TypeDir:
		if exists && cur.Type != meta.TDir {
			return r.skip(ctx, n, dst, name)
		}
		if !exists {
			if err := f.MkdirAll(name, os.FileMode(e.Mode&0o7777)); err != nil {
				return err
			}
			r.note(func(rep *RestoreReport) { rep.Dirs++ })
		}
		t, err := f.snapTree(ctx, m, e.Child)
		if err != nil {
			return snapErr("restore", name, err)
		}
		for i := range t.Entries {
			child := snapChild(n, &t.Entries[i])
			if f.checkSnap(&child) != nil {
				r.note(func(rep *RestoreReport) { rep.Refused = append(rep.Refused, path.Join(name, t.Entries[i].Name)) })
				continue
			}
			if err := r.walk(ctx, &child, append(slices.Clip(dst), t.Entries[i].Name), give); err != nil {
				return err
			}
		}
		return nil
	case mirror.TypeSymlink:
		if exists {
			r.note(func(rep *RestoreReport) { rep.Skipped = append(rep.Skipped, name) })
			return nil
		}
		if err := f.Symlink(e.Target, name); err != nil {
			return err
		}
		r.note(func(rep *RestoreReport) { rep.Links++ })
		return nil
	case mirror.TypeFile:
	}
	if exists {
		return r.skip(ctx, n, dst, name)
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	return give(fileTask{n: *n, dst: slices.Clone(dst)})
}

// skip reports name as left alone and counts what the restore would have written there
// as already there, so the done counts still reach the totals (I-365).
func (r *restoreRun) skip(ctx context.Context, n *snapNode, dst []string, name string) error {
	r.note(func(rep *RestoreReport) { rep.Skipped = append(rep.Skipped, name) })
	if r.opt.Progress == nil {
		return nil
	}
	files, bytes, err := r.f.snapTotals(ctx, n, dst)
	if err != nil {
		return snapErr("restore", name, err)
	}
	if files > 0 || bytes > 0 {
		r.progress(RestoreProgress{Files: files, Bytes: bytes, Existing: true})
	}
	return nil
}

// file writes one missing file: under its temporary name in its folder, renamed into place
// once committed, so the name holds the whole file or nothing and an interrupted restore
// leaves no empty file a rerun would skip as present (docs/16 §6).
func (r *restoreRun) file(ctx context.Context, t *fileTask) error {
	f, dst, e := r.f, t.dst, &t.n.e
	name := "/" + path.Join(dst...)
	tmpParts := append(slices.Clip(dst[:len(dst)-1]), RestoreTempName(dst[len(dst)-1]))
	tmp := "/" + path.Join(tmpParts...)
	if old, err := f.st.Resolve(tmpParts); err == nil { // an interrupted run's, or a stopped node's
		if err := f.RemoveIno(tmp, old.Ino, true); err != nil {
			return mapErr("restore", tmp, err)
		}
	}
	fh, err := f.OpenFile(tmp, os.O_RDWR|os.O_CREATE|os.O_EXCL, os.FileMode(e.Mode&0o777))
	if err != nil {
		return err
	}
	ino := fh.(*File).ino
	_ = fh.Close()
	up, err := f.writeEntry(ctx, ino, t.n.row.Idx, e, r.slots, func(b int64) { r.progress(RestoreProgress{Bytes: b}) })
	if err != nil {
		_ = f.RemoveIno(tmp, ino, true) // best effort: a rerun removes it otherwise
		return mapErr("restore", name, err)
	}
	if err := f.RenameNoReplace(tmp, name); err != nil {
		if errors.Is(err, os.ErrExist) { // the name was taken meanwhile: that file stays
			_ = f.RemoveIno(tmp, ino, true)
			r.note(func(rep *RestoreReport) { rep.Skipped = append(rep.Skipped, name) })
			r.progress(RestoreProgress{Files: 1, Existing: true})
			return nil
		}
		return err
	}
	r.note(func(rep *RestoreReport) { rep.Files++; rep.Uploaded += up })
	r.progress(RestoreProgress{Files: 1})
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

// RestoreTempName is the name a restore writes a file under, in its final folder, until
// the file's data is committed and it is renamed into place (docs/16 §6): fixed-length,
// so any final name has one, and the same for the same name, so a rerun finds what an
// interrupted run or a stopped node left.
func RestoreTempName(name string) string {
	sum := sha256.Sum256([]byte(name))
	return ".tailraid-restore-" + hex.EncodeToString(sum[:8])
}

// gate bounds how many of something run at once: full, or low while yield answers true
// (nil: never). The width is read again whenever one would start, so it follows yield
// without anything restarting, and every release wakes the waiters to look again
// (I-372).
type gate struct {
	full, low int
	yield     func() bool
	mu        sync.Mutex
	in        int
	wake      chan struct{}
}

func newGate(full, low int, yield func() bool) *gate {
	return &gate{full: full, low: low, yield: yield, wake: make(chan struct{})}
}

// acquire waits until fewer than the current width run, then takes a place.
func (g *gate) acquire(ctx context.Context) error {
	for {
		limit := g.full
		if g.yield != nil && g.yield() {
			limit = g.low
		}
		g.mu.Lock()
		if g.in < limit {
			g.in++
			g.mu.Unlock()
			return nil
		}
		wake := g.wake
		g.mu.Unlock()
		select {
		case <-wake:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
}

func (g *gate) release() {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.in--
	close(g.wake)
	g.wake = make(chan struct{})
}
