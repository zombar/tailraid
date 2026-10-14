package webui

import (
	"bytes"
	"cmp"
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/zombar/tailraid/internal/admin"
	"github.com/zombar/tailraid/internal/meta"
	"github.com/zombar/tailraid/internal/vfs"
)

// SnapshotService is what the snapshot routes need from the node (docs/13 §3, docs/16).
type SnapshotService interface {
	// Mirrors is every mirror of the cluster as the probes see it (docs/16 §1).
	Mirrors() []admin.MirrorReport
	// Take flushes this gateway's dirty files and begins a manual snapshot named name
	// ("" the UTC minute) on the mirror whose row is named mirror ("" every online one);
	// it returns the begun rows.
	Take(ctx context.Context, name, mirror string) (any, error)
	// Schedule is this node's mirrors config.
	Schedule() Schedule
}

// Schedule is the snapshot schedule of docs/16 §4 as the dashboard's heading shows it.
type Schedule struct {
	Every                          time.Duration
	Hourly, Daily, Weekly, Monthly int
	StaleAfter                     time.Duration
}

// SnapInfo names the snapshot a listing is read as of.
type SnapInfo struct {
	ID     uint32 `json:"id"`
	Name   string `json:"name"`
	Mirror string `json:"mirror"`
	At     string `json:"at"`
}

func snapInfo(ref *vfs.SnapshotRef) *SnapInfo {
	return &SnapInfo{ID: uint32(ref.Snapshot.ID), Name: ref.Snapshot.Name, Mirror: ref.Mirror, At: rfc3339(ref.Snapshot.At)}
}

// errOffline answers 409 {"error","offline":true} (docs/13 §3).
var errOffline = vfs.ErrMirrorOffline

// snapshotOf parses a snapshot ID and finds it: 400 for a bad ID, 404 for none, 409
// offline for one whose mirror is offline.
func (s *Server) snapshotOf(raw string) (vfs.SnapshotRef, error) {
	id, err := strconv.ParseUint(raw, 10, 32)
	if err != nil || id == 0 {
		return vfs.SnapshotRef{}, fmt.Errorf("snapshot %q: %w", raw, meta.ErrBadCommand)
	}
	ref, ok := s.o.FS.Snapshot(meta.SnapID(id)) // the mirrors are the node's facts, not a user's
	if !ok {
		return vfs.SnapshotRef{}, fmt.Errorf("snapshot %d: %w", id, meta.ErrNotExist)
	}
	if !ref.Online {
		return ref, fmt.Errorf("snapshot %s on %s: %w", ref.Snapshot.Name, ref.Mirror, errOffline)
	}
	return ref, nil
}

// resolveAt resolves t as of the snapshot at= names (docs/13 §3): in= with at= is 400.
func (s *Server) resolveAt(r *http.Request, t target) (target, error) {
	if r.URL.Query().Has("in") {
		return t, fmt.Errorf("in= with at=: %w", meta.ErrBadCommand)
	}
	ref, err := s.snapshotOf(r.URL.Query().Get("at"))
	if err != nil {
		return t, err
	}
	t.snap = &ref
	return s.statAt(t)
}

// statAt fills t.in from the snapshot's entry at t.parts.
func (s *Server) statAt(t target) (target, error) {
	t.vpath = t.snap.Root + t.canonical
	if len(t.parts) == 0 {
		t.vpath = t.snap.Root
	}
	fi, err := t.fs.Stat(t.vpath)
	if err != nil {
		return t, s.snapErr(t, err)
	}
	t.in = inodeAt(t.fs, t.vpath, fi)
	if len(t.parts) == 0 {
		t.in.Name = ""
	}
	return t, nil
}

// snapErr keeps a mirror that went away during the request an offline answer.
func (s *Server) snapErr(t target, err error) error {
	if ref, ok := s.o.FS.Snapshot(t.snap.Snapshot.ID); ok && !ref.Online {
		return fmt.Errorf("snapshot %s on %s: %w", ref.Snapshot.Name, ref.Mirror, errOffline)
	}
	return err
}

// inodeAt is the snapshot entry fi (at vpath) in the fields the handlers read.
func inodeAt(fs *vfs.FS, vpath string, fi os.FileInfo) meta.Inode {
	in := meta.Inode{Name: fi.Name(), Type: meta.TFile, Size: uint64(max(fi.Size(), 0)), Mtime: fi.ModTime().UnixNano(), Mode: uint32(fi.Mode().Perm())}
	if x, ok := fi.(interface{ Ino() meta.Ino }); ok {
		in.Ino = x.Ino()
	}
	switch {
	case fi.IsDir():
		in.Type, in.Size = meta.TDir, 0
	case fi.Mode()&os.ModeSymlink != 0:
		in.Type, in.Size = meta.TSymlink, 0
		in.Target, _ = fs.Readlink(vpath)
	}
	return in
}

// snapEntry is an entry as of a snapshot: no dirty, deleted or upload fields, no flags.
func snapEntry(in meta.Inode) Entry {
	e := Entry{Name: in.Name, Type: typeName(in), Size: in.Size, Mtime: time.Unix(0, in.Mtime).UTC(), Mode: in.Mode, Ino: uint64(in.Ino), Target: in.Target}
	if in.Type == meta.TFile {
		e.Thumb = isImage(in.Name) || isAudio(in.Name)
	}
	return e
}

// lsAt lists a folder as of a snapshot (docs/13 §3).
func (s *Server) lsAt(w http.ResponseWriter, t target) {
	fis, err := t.fs.ReadDir(t.vpath)
	if err != nil {
		s.fail(w, s.snapErr(t, err))
		return
	}
	entries := make([]Entry, 0, len(fis))
	for _, fi := range fis {
		if IsOSMetadata(fi.Name()) {
			continue
		}
		entries = append(entries, snapEntry(inodeAt(t.fs, path.Join(t.vpath, fi.Name()), fi)))
	}
	writeJSON(w, http.StatusOK, map[string]any{"path": t.canonical, "dir": DirInfo{Snapshot: snapInfo(t.snap)}, "entries": entries})
}

// thumbAt is a thumbnail as of a snapshot: an image's or an audio file's, made on each
// request and never stored; a folder or a video has none (docs/13 §3).
func (s *Server) thumbAt(w http.ResponseWriter, r *http.Request, t target) {
	var data []byte
	var err error
	switch {
	case t.in.Type == meta.TFile && isImage(t.in.Name):
		data, err = s.makeImageThumb(r.Context(), t)
	case t.in.Type == meta.TFile && isAudio(t.in.Name):
		data, err = s.makeAudioThumb(r.Context(), t)
	default:
		err = noThumb(t.canonical)
	}
	if err != nil {
		switch {
		case errors.Is(err, errNoThumb):
			writeErr(w, http.StatusNotFound, "no thumbnail")
		case errors.Is(err, errTooLarge):
			writeErr(w, http.StatusUnsupportedMediaType, err.Error())
		default:
			s.fail(w, err)
		}
		return
	}
	key := fmt.Sprintf("s%d-%d", t.snap.Snapshot.ID, t.in.Ino)
	thumbHeaders(w, key)
	http.ServeContent(w, r, key, time.Time{}, bytes.NewReader(data))
}

// --- the snapshot routes (docs/13 §3) ----------------------------------------------------

// MirrorJSON is one mirror in GET /api/snapshots.
type MirrorJSON struct {
	ID       uint32    `json:"id"`
	Name     string    `json:"name"` // its directory under /.snapshots: pike-m1
	Host     string    `json:"host"`
	Self     bool      `json:"self,omitempty"`
	Path     string    `json:"path,omitempty"`
	Online   bool      `json:"online"`
	Reason   string    `json:"reason,omitempty"`
	Used     int64     `json:"used"`
	Free     uint64    `json:"free"`
	Total    uint64    `json:"total"`
	LastSeen string    `json:"last_seen,omitempty"`
	Warn     string    `json:"warn,omitempty"` // stale | reserve | incomplete
	Syncing  *SyncJSON `json:"syncing,omitempty"`
}

// SyncJSON is a sync under way.
type SyncJSON struct {
	ID       uint32 `json:"id"`
	Snapshot string `json:"snapshot"`
	Read     uint64 `json:"read"`
	Total    uint64 `json:"total"`
	BytesNew uint64 `json:"bytes_new"`
}

// SnapshotJSON is one catalogue row in GET /api/snapshots.
type SnapshotJSON struct {
	ID       uint32 `json:"id"`
	Mirror   string `json:"mirror"`
	Name     string `json:"name"`
	At       string `json:"at"`
	Auto     bool   `json:"auto"`
	State    string `json:"state"`
	Files    uint64 `json:"files"`
	Bytes    uint64 `json:"bytes"`
	NewBytes uint64 `json:"new_bytes"`
	Missing  uint32 `json:"missing"`
}

// ScheduleJSON is the schedule in GET /api/snapshots: every in seconds (0 none), next
// when the newest automatic snapshot on an online mirror falls due.
type ScheduleJSON struct {
	Every   int64  `json:"every"`
	Hourly  int    `json:"hourly"`
	Daily   int    `json:"daily"`
	Weekly  int    `json:"weekly"`
	Monthly int    `json:"monthly"`
	Next    string `json:"next,omitempty"`
}

// mirrorReserve is the free space a mirror keeps, as its host counts it (docs/16 §4).
func mirrorReserve(total uint64) uint64 { return max(total/20, 4<<30) }

func (s *Server) snapshots(w http.ResponseWriter, _ *http.Request) {
	if s.o.Snapshots == nil {
		writeErr(w, http.StatusServiceUnavailable, "snapshots are not available on this node")
		return
	}
	sched := s.o.Snapshots.Schedule()
	now := s.o.Clk.Now()
	names := map[meta.NodeIdx]string{}
	nodes := s.o.Store.Nodes()
	for i := range nodes {
		if nodes[i].Kind == meta.KindMirror {
			names[nodes[i].Idx] = vfs.MirrorDirName(&nodes[i])
		}
	}
	all := s.o.Store.Snapshots()
	slices.SortFunc(all, func(a, b meta.Snapshot) int { return cmp.Or(cmp.Compare(b.At, a.At), cmp.Compare(b.ID, a.ID)) })
	snaps := []SnapshotJSON{}
	newest := map[meta.NodeIdx]int64{}
	newestAuto := map[meta.NodeIdx]int64{}
	incomplete := map[meta.NodeIdx]bool{}
	for i := range all {
		sn := &all[i]
		if sn.State == meta.SnapDeleting { // gone for the user (I-183)
			continue
		}
		if _, ok := names[sn.Mirror]; !ok {
			continue
		}
		snaps = append(snaps, SnapshotJSON{
			ID: uint32(sn.ID), Mirror: names[sn.Mirror], Name: sn.Name, At: rfc3339(sn.At), Auto: sn.Auto, State: sn.State.String(),
			Files: sn.Files, Bytes: sn.Bytes, NewBytes: sn.NewBytes, Missing: sn.Missing,
		})
		if sn.State == meta.SnapComplete || sn.State == meta.SnapIncomplete {
			newest[sn.Mirror] = max(newest[sn.Mirror], sn.At)
			if sn.Auto {
				newestAuto[sn.Mirror] = max(newestAuto[sn.Mirror], sn.At)
			}
		}
		incomplete[sn.Mirror] = incomplete[sn.Mirror] || sn.State == meta.SnapIncomplete
	}
	mirrors := []MirrorJSON{}
	var next time.Time
	reports := s.o.Snapshots.Mirrors()
	for i := range reports {
		m := &reports[i]
		if m.Idx == 0 { // a configured path no row claims: the CLI's business
			continue
		}
		row, ok := s.o.Store.Node(m.Idx)
		if !ok {
			continue
		}
		j := MirrorJSON{ID: uint32(m.Idx), Name: vfs.MirrorDirName(&row), Host: m.Host, Self: m.Self, Path: m.Path, Online: m.Online, Reason: m.Reason, Used: m.Used, Free: m.Free, Total: m.Total, LastSeen: rfc3339(m.LastSeen)}
		if m.Syncing != nil {
			j.Syncing = &SyncJSON{ID: m.Syncing.ID, Snapshot: m.Syncing.Snapshot, Read: m.Syncing.Read, Total: m.Syncing.Total, BytesNew: m.Syncing.BytesNew}
		}
		switch {
		case newest[m.Idx] != 0 && sched.StaleAfter > 0 && now.Sub(time.Unix(0, newest[m.Idx])) > sched.StaleAfter:
			j.Warn = "stale"
		case m.Online && m.Total > 0 && m.Free < mirrorReserve(m.Total):
			j.Warn = "reserve"
		case incomplete[m.Idx]:
			j.Warn = "incomplete"
		}
		if m.Online && sched.Every > 0 {
			due := now
			if at := newestAuto[m.Idx]; at != 0 {
				due = time.Unix(0, at).Add(sched.Every)
			}
			if next.IsZero() || due.Before(next) {
				next = due
			}
		}
		mirrors = append(mirrors, j)
	}
	sj := ScheduleJSON{Every: int64(sched.Every / time.Second), Hourly: sched.Hourly, Daily: sched.Daily, Weekly: sched.Weekly, Monthly: sched.Monthly}
	if !next.IsZero() {
		sj.Next = next.UTC().Format(time.RFC3339)
	}
	writeJSON(w, http.StatusOK, map[string]any{"mirrors": mirrors, "snapshots": snaps, "schedule": sj})
}

// validSnapName mirrors the FSM's rule (docs/16 §8): 1..64 bytes, no "/" or NUL, not
// "." or "..".
func validSnapName(name string) bool {
	return name != "" && len(name) <= meta.MaxSnapName && name != "." && name != ".." && !strings.ContainsAny(name, "/\x00")
}

// takeSnapshot is POST /api/snapshot (docs/13 §3, I-187): the serving gateway flushes
// its dirty files, then a manual snapshot begins on the mirror named (its /.snapshots
// directory) or on every online one.
func (s *Server) takeSnapshot(w http.ResponseWriter, r *http.Request) {
	if s.o.Snapshots == nil {
		writeErr(w, http.StatusServiceUnavailable, "snapshots are not available on this node")
		return
	}
	var body struct {
		Name   string `json:"name"`
		Mirror string `json:"mirror"`
	}
	if !decode(w, r, 4096, &body) {
		return
	}
	if body.Name != "" && !validSnapName(body.Name) {
		writeErr(w, http.StatusBadRequest, "a snapshot name is 1 to 64 bytes, without /")
		return
	}
	row := ""
	if body.Mirror != "" {
		nodes := s.o.Store.Nodes()
		for i := range nodes {
			if nodes[i].Kind == meta.KindMirror && nodes[i].State != meta.Removed && (vfs.MirrorDirName(&nodes[i]) == body.Mirror || nodes[i].Name == body.Mirror) {
				row = nodes[i].Name
			}
		}
		if row == "" {
			writeErr(w, http.StatusNotFound, "no mirror "+body.Mirror)
			return
		}
	}
	begun, err := s.o.Snapshots.Take(r.Context(), body.Name, row)
	if err != nil {
		s.fail(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, begun)
}

// VersionJSON is one distinct content of a file in GET /api/versions.
type VersionJSON struct {
	Key       string          `json:"key"`
	Size      uint64          `json:"size"`
	Mtime     string          `json:"mtime"`
	Current   bool            `json:"current,omitempty"`
	Snapshots []VersionSnapJS `json:"snapshots"`
}

// VersionSnapJS is a snapshot holding a version.
type VersionSnapJS struct {
	ID     uint32 `json:"id"`
	Mirror string `json:"mirror"`
	Name   string `json:"name"`
	At     string `json:"at"`
	Auto   bool   `json:"auto"`
}

// versions is GET /api/versions (docs/13 §3, docs/16 §6).
func (s *Server) versions(w http.ResponseWriter, r *http.Request) {
	parts, canonical := cleanPath(r.URL.Query().Get("path"))
	if len(parts) == 0 || reserved(parts) {
		writeErr(w, http.StatusBadRequest, "versions "+canonical+": not a file of the volume")
		return
	}
	in, err := s.o.Store.Resolve(parts)
	if err != nil {
		s.fail(w, err)
		return
	}
	if !s.readable(r, in.Ino) {
		s.fail(w, fmt.Errorf("versions %s: %w", canonical, syscall.EACCES))
		return
	}
	if in.Type != meta.TFile {
		s.fail(w, fmt.Errorf("versions %s: %w", canonical, meta.ErrIsDir))
		return
	}
	vs, off, err := s.fs(r).Versions(r.Context(), canonical)
	if err != nil {
		s.fail(w, err)
		return
	}
	out := make([]VersionJSON, 0, len(vs))
	for i := range vs {
		v := &vs[i]
		j := VersionJSON{Key: "now", Size: v.Size, Mtime: rfc3339(v.Mtime), Current: v.Current, Snapshots: []VersionSnapJS{}}
		for _, sn := range v.Snapshots {
			auto := false
			if row, ok := s.o.Store.SnapshotRow(sn.ID); ok {
				auto = row.Auto
			}
			j.Snapshots = append(j.Snapshots, VersionSnapJS{ID: uint32(sn.ID), Mirror: sn.Mirror, Name: sn.Name, At: rfc3339(sn.At), Auto: auto})
		}
		if !v.Current && len(v.Snapshots) > 0 {
			j.Key = strconv.FormatUint(uint64(v.Snapshots[0].ID), 10)
		}
		out = append(out, j)
	}
	type offline struct {
		Mirror string `json:"mirror"`
		Count  int    `json:"count"`
	}
	offs := []offline{}
	for _, o := range off {
		offs = append(offs, offline{Mirror: o.Mirror, Count: o.Snapshots})
	}
	writeJSON(w, http.StatusOK, map[string]any{"path": canonical, "versions": out, "offline": offs})
}

// liveFile resolves a live file of the volume for a revert or a copy.
func (s *Server) liveFile(raw string) (string, error) {
	parts, canonical := cleanPath(raw)
	if len(parts) == 0 || reserved(parts) {
		return canonical, fmt.Errorf("%s: %w", canonical, meta.ErrBadCommand)
	}
	in, err := s.o.Store.Resolve(parts)
	if err != nil {
		return canonical, err
	}
	if in.Type != meta.TFile {
		return canonical, fmt.Errorf("%s: %w", canonical, meta.ErrIsDir)
	}
	return canonical, nil
}

// revert is POST /api/revert (docs/13 §3, I-186, I-187).
func (s *Server) revert(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Path     string `json:"path"`
		Snapshot uint32 `json:"snapshot"`
	}
	if !decode(w, r, 8192, &body) {
		return
	}
	p, err := s.liveFile(body.Path)
	if err != nil {
		s.fail(w, err)
		return
	}
	ref, err := s.snapshotOf(strconv.FormatUint(uint64(body.Snapshot), 10))
	if err != nil {
		s.fail(w, err)
		return
	}
	n, err := s.fs(r).RevertFile(r.Context(), ref.Root+p, p)
	if err != nil {
		s.fail(w, s.snapErr(target{snap: &ref}, err))
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"path": p, "uploaded": n})
}

// copyVersion is POST /api/copy: the file as of the snapshot, beside it under name.
func (s *Server) copyVersion(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Path     string `json:"path"`
		Snapshot uint32 `json:"snapshot"`
		Name     string `json:"name"`
	}
	if !decode(w, r, 8192, &body) {
		return
	}
	if err := validName(body.Name); err != nil {
		s.fail(w, err)
		return
	}
	parts, p := cleanPath(body.Path)
	if len(parts) == 0 || reserved(parts) {
		writeErr(w, http.StatusBadRequest, "copy "+p+": not a file of the volume")
		return
	}
	ref, err := s.snapshotOf(strconv.FormatUint(uint64(body.Snapshot), 10))
	if err != nil {
		s.fail(w, err)
		return
	}
	src := target{snap: &ref, parts: parts, canonical: p, fs: s.fs(r)}
	if src, err = s.statAt(src); err != nil {
		s.fail(w, err)
		return
	}
	if src.in.Type != meta.TFile {
		s.fail(w, fmt.Errorf("copy %s: %w", p, meta.ErrIsDir))
		return
	}
	dst := path.Join(path.Dir(p), body.Name)
	rep, err := s.fs(r).RestoreSnapshot(r.Context(), src.vpath, dst)
	if err != nil {
		s.fail(w, s.snapErr(src, err))
		return
	}
	dparts, _ := cleanPath(dst)
	in, err := s.o.Store.Resolve(dparts)
	if err != nil {
		s.fail(w, err)
		return
	}
	e := s.entry(s.fs(r), in, dst)
	e.Path = dst
	writeJSON(w, http.StatusCreated, map[string]any{"entry": e, "uploaded": rep.Uploaded})
}

// restoreTree is POST /api/restore-tree (docs/13 §3): the snapshot's entry at path
// recreated under to (its parent unless given) as name (its own unless given).
func (s *Server) restoreTree(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Snapshot uint32 `json:"snapshot"`
		Path     string `json:"path"`
		To       string `json:"to"`
		Name     string `json:"name"`
	}
	if !decode(w, r, 8192, &body) {
		return
	}
	parts, p := cleanPath(body.Path)
	if reserved(parts) {
		writeErr(w, http.StatusBadRequest, "restore "+p+": reserved")
		return
	}
	ref, err := s.snapshotOf(strconv.FormatUint(uint64(body.Snapshot), 10))
	if err != nil {
		s.fail(w, err)
		return
	}
	src := target{snap: &ref, parts: parts, canonical: p, fs: s.fs(r)}
	if src, err = s.statAt(src); err != nil {
		s.fail(w, err)
		return
	}
	name := body.Name
	if name == "" {
		name = path.Base(p)
		if len(parts) == 0 {
			name = ref.Snapshot.Name // the whole snapshot lands as a folder named after it
		}
	}
	if err := validName(name); err != nil {
		s.fail(w, err)
		return
	}
	to := path.Dir(p)
	if body.To != "" {
		_, to = cleanPath(body.To)
	}
	dst := path.Join(to, name)
	rep, err := s.fs(r).RestoreSnapshot(r.Context(), src.vpath, dst)
	if err != nil {
		s.fail(w, s.snapErr(src, err))
		return
	}
	skipped := rep.Skipped
	if len(skipped) > MaxBatch {
		skipped = skipped[:MaxBatch]
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"path": dst, "files": rep.Files, "dirs": rep.Dirs, "links": rep.Links,
		"skipped": append([]string{}, skipped...), "skipped_count": len(rep.Skipped), "uploaded": rep.Uploaded,
	})
}
