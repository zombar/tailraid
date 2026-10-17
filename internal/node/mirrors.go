package node

import (
	"cmp"
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/zombar/tailraid/internal/admin"
	"github.com/zombar/tailraid/internal/clock"
	"github.com/zombar/tailraid/internal/cluster"
	"github.com/zombar/tailraid/internal/config"
	"github.com/zombar/tailraid/internal/meta"
	"github.com/zombar/tailraid/internal/mirror"
	"github.com/zombar/tailraid/internal/rpc"
	"github.com/zombar/tailraid/internal/sysfs"
)

// mirrorSet is this host's mirrors (docs/16): it opens the configured paths whose marker
// names this cluster and a mirror row of this host, syncs every snapshot begun on them,
// and runs each one's schedule, thinning, free-space reserve and reconciliation. Only
// the host knows whether its disk is attached, so the host drives its mirrors; the
// leader only ends a sync that outlived hold_max (expireSyncs).
type mirrorSet struct {
	fsys      sysfs.FS
	paths     []string
	pol       config.MirrorPolicy
	clusterID string
	st        *meta.Store
	propose   func(ctx context.Context, cmd meta.Command) error
	read      func(ctx context.Context, h meta.Hash) ([]byte, error)
	self      func() (meta.NodeIdx, bool)
	selfID    string                      // this node's ID, for the FSM hook (it may not read the FSM)
	minCodec  func() uint8                // the oldest codec a live member reads; nil: every one is current
	held      func() map[meta.SnapID]bool // the snapshots this host's unfinished restore jobs read (docs/16 §6); nil: none
	clk       clock.Clock
	log       *slog.Logger

	tickMu    sync.Mutex // one tick at a time: the maintenance loop's and `mirror add`'s
	refreshMu sync.Mutex // one refresh at a time, so a disk is opened once and its sweep excludes every write
	mu        sync.Mutex
	open      map[meta.NodeIdx]*mirrorDisk // online mirrors by row
	seen      []mirrorDisk                 // every configured path, as the last tick saw it (status)
	inflight  map[meta.SnapID]bool
	syncing   map[meta.NodeIdx]syncing // the snapshot being copied to each mirror, for /health
	jobs      chan syncJob
	lastID    map[string]string // path → the mirror ID its marker last named (refreshMu): an unplugged disk keeps its row
}

// syncing is a sync under way: the snapshot and the file bytes it holds.
type syncing struct {
	id    meta.SnapID
	name  string
	total uint64
}

type mirrorDisk struct {
	Path   string
	Row    meta.Node
	Online bool
	Reason string // why it is offline
	Used   int64
	Free   uint64
	Total  uint64
	m      *mirror.Mirror
}

type syncJob struct {
	sn    meta.Snapshot
	clone *meta.State
}

// mirrorReserve is the free space a mirror keeps: max(5 %, 4 GiB) of its filesystem.
func mirrorReserve(total uint64) uint64 { return max(total/20, 4<<30) }

func newMirrorSet(fsys sysfs.FS, cfg config.Config, pol config.MirrorPolicy, clusterID [16]byte, st *meta.Store, clk clock.Clock, log *slog.Logger) *mirrorSet {
	s := &mirrorSet{
		fsys: fsys, pol: pol, clusterID: hex.EncodeToString(clusterID[:]), st: st, clk: clk, log: log,
		open: map[meta.NodeIdx]*mirrorDisk{}, inflight: map[meta.SnapID]bool{}, syncing: map[meta.NodeIdx]syncing{}, jobs: make(chan syncJob, 4), lastID: map[string]string{},
	}
	for _, m := range cfg.Node.Mirrors {
		s.paths = append(s.paths, m.Path)
	}
	return s
}

// add takes a path `mirror add` just wrote to the config and opens it now (docs/16 §1):
// the running node never rereads its config. It returns how the path looks: "online", or
// why it is offline.
func (s *mirrorSet) add(path string) string {
	s.refreshMu.Lock()
	if !slices.Contains(s.paths, path) {
		s.paths = append(s.paths, path)
	}
	s.refreshMu.Unlock()
	s.refresh()
	s.mu.Lock()
	defer s.mu.Unlock()
	for i := range s.seen {
		if d := &s.seen[i]; d.Path == path {
			if d.Online {
				return "online"
			}
			return d.Reason
		}
	}
	return "not configured"
}

// onBegin is the FSM hook: a snapshot begun on one of this host's mirrors takes the
// clone of the state at its index; every other node returns at once (docs/16 §3). It
// runs under the FSM's apply lock, so it knows its host by node ID, never by asking the
// FSM (a read there would wait on the lock this call holds).
func (s *mirrorSet) onBegin(sn meta.Snapshot, _ meta.NodeIdx, hostID string, clone func() *meta.State) {
	if hostID == "" || hostID != s.selfID {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	select {
	case s.jobs <- syncJob{sn: sn, clone: clone()}:
		s.inflight[sn.ID] = true
	default: // never on a host that syncs one snapshot per mirror; failed by the next tick
	}
}

// worker syncs the queued snapshots one at a time until ctx ends.
func (s *mirrorSet) worker(ctx context.Context, ready func(context.Context) error) {
	for {
		select {
		case <-ctx.Done():
			return
		case j := <-s.jobs:
			s.sync(ctx, j, ready)
			s.mu.Lock()
			delete(s.inflight, j.sn.ID)
			s.mu.Unlock()
		}
	}
}

func (s *mirrorSet) sync(ctx context.Context, j syncJob, ready func(context.Context) error) {
	if err := ready(ctx); err != nil {
		return
	}
	// a job replayed from the log at start is for a row a later entry already ended
	if row, ok := s.st.SnapshotRow(j.sn.ID); !ok || row.State != meta.SnapSyncing {
		return
	}
	end := meta.SnapshotEnd{ID: j.sn.ID}
	s.mu.Lock()
	d := s.open[j.sn.Mirror]
	s.mu.Unlock()
	if d == nil { // added since the last tick (`mirror add` then `snapshot create`): look now
		s.refresh()
		s.mu.Lock()
		d = s.open[j.sn.Mirror]
		s.mu.Unlock()
	}
	if d == nil {
		s.log.Warn("snapshot sync: the mirror is offline", "snapshot", j.sn.Name, "mirror", j.sn.Mirror)
	} else {
		src := mirror.StateSource(j.clone, s.read)
		total, _ := mirror.TotalBytes(src, uint64(meta.RootIno))
		s.mu.Lock()
		s.syncing[j.sn.Mirror] = syncing{id: j.sn.ID, name: j.sn.Name, total: total}
		s.mu.Unlock()
		defer func() {
			s.mu.Lock()
			delete(s.syncing, j.sn.Mirror)
			s.mu.Unlock()
		}()
		man, err := d.m.WriteSnapshot(ctx, src, uint64(meta.RootIno), mirror.Manifest{
			ID: uint32(j.sn.ID), Name: j.sn.Name, Index: j.sn.Index, At: j.sn.At, Auto: j.sn.Auto, ChunkSize: j.clone.Vol.ChunkSize,
		})
		if err != nil {
			s.log.Warn("snapshot sync failed", "snapshot", j.sn.Name, "mirror", d.Row.Name, "err", err)
		} else {
			end = meta.SnapshotEnd{ID: j.sn.ID, OK: true, Root: meta.Hash(man.Root), Files: man.Files, Bytes: man.Bytes, NewBytes: man.NewBytes, Missing: uint32(len(man.Missing))}
			s.log.Info("snapshot synced", "snapshot", man.Name, "mirror", d.Row.Name, "files", man.Files, "new_bytes", man.NewBytes, "missing", len(man.Missing))
		}
	}
	end.Now = s.clk.Now().UnixNano()
	if err := s.propose(context.WithoutCancel(ctx), meta.Command{Op: meta.OpSnapshotEnd, SnapshotEnd: &end}); err != nil && !errors.Is(err, meta.ErrNotExist) {
		s.log.Warn("snapshot end not recorded", "snapshot", j.sn.Name, "err", err)
	}
}

// refresh opens the configured mirrors whose marker names this cluster and a live mirror
// row of this host, and closes the others (docs/16 §1: online or offline).
func (s *mirrorSet) refresh() {
	s.refreshMu.Lock()
	defer s.refreshMu.Unlock()
	self, ok := s.self()
	rows := map[string]meta.Node{}
	nodes := s.st.Nodes()
	for i := range nodes {
		n := nodes[i]
		if n.Kind == meta.KindMirror && n.State != meta.Removed {
			rows[n.ID] = n
		}
	}
	seen := make([]mirrorDisk, 0, len(s.paths))
	open := map[meta.NodeIdx]*mirrorDisk{}
	for _, p := range s.paths {
		d := mirrorDisk{Path: p}
		mk, err := mirror.ReadMarker(s.fsys, p)
		row, known := rows[mk.MirrorID]
		switch {
		case err != nil:
			d.Reason = "not attached: " + err.Error()
		case mk.ClusterID != s.clusterID:
			d.Reason = "the marker names another cluster"
		case !known || !ok || row.Host != self:
			d.Reason = "not registered on this host (tailraid mirror add)"
		}
		if err == nil && known && ok && row.Host == self {
			s.lastID[p] = mk.MirrorID
		}
		if d.Reason == "" {
			d.Row = row
			s.mu.Lock()
			prev := s.open[row.Idx]
			s.mu.Unlock()
			if prev != nil && prev.Path == p {
				d.m = prev.m
			} else if d.m, err = mirror.Open(s.fsys, p, s.clusterID); err != nil {
				d.Reason = "open: " + err.Error()
			}
		}
		if d.Reason == "" {
			d.Online, d.Used = true, d.m.Used()
			d.Free, d.Total, _ = s.fsys.Free(p)
			open[d.Row.Idx] = &d
		}
		seen = append(seen, d)
	}
	s.rowsOfDetached(seen, rows, self, ok)
	s.mu.Lock()
	s.open, s.seen = open, seen
	s.mu.Unlock()
}

// rowsOfDetached names the row of each configured path whose disk is not attached, so
// status and check still say which mirror is away and warn once it has been away past
// stale_after (I-188): the row its marker named when it was last seen, or, when exactly
// one path is detached and exactly one row of this host is unmatched, that row.
func (s *mirrorSet) rowsOfDetached(seen []mirrorDisk, rows map[string]meta.Node, self meta.NodeIdx, ok bool) {
	if !ok {
		return
	}
	matched := map[meta.NodeIdx]bool{}
	var detached []int
	for i := range seen {
		d := &seen[i]
		if d.Row.Idx == 0 && strings.HasPrefix(d.Reason, "not attached") {
			if row, known := rows[s.lastID[d.Path]]; known && row.Host == self {
				d.Row = row
			}
		}
		if d.Row.Idx != 0 {
			matched[d.Row.Idx] = true
		} else if strings.HasPrefix(d.Reason, "not attached") {
			detached = append(detached, i)
		}
	}
	var unmatched []meta.Node
	for id := range rows {
		if rows[id].Host == self && !matched[rows[id].Idx] {
			unmatched = append(unmatched, rows[id])
		}
	}
	if len(detached) == 1 && len(unmatched) == 1 {
		seen[detached[0]].Row = unmatched[0]
	}
}

// tick is one minute of the host's mirror work, in this order: refresh, end the syncs
// this host lost (a restart drops its clone), reconcile each disk and import what it
// holds with no row, delete for space and thin, then begin what is due (docs/16 §4, §5,
// I-182, I-183). While a live member reads a codec older than 7 it only refreshes.
func (s *mirrorSet) tick(ctx context.Context) {
	s.tickMu.Lock()
	defer s.tickMu.Unlock()
	s.refresh()
	if s.minCodec != nil && s.minCodec() < meta.CodecMirrors {
		return // an older member could not decode a snapshot command: nothing is proposed (docs/16 §4)
	}
	now := s.clk.Now()
	snaps := s.st.Snapshots()
	s.mu.Lock()
	open := maps.Clone(s.open)
	inflight := maps.Clone(s.inflight)
	s.mu.Unlock()
	for _, idx := range slices.Sorted(maps.Keys(open)) {
		d := open[idx]
		var rows []meta.Snapshot
		var mrows []mirror.Row
		for i := range snaps {
			if snaps[i].Mirror == idx {
				rows = append(rows, snaps[i])
				mrows = append(mrows, mirror.Row{ID: uint32(snaps[i].ID), Deleting: snaps[i].State == meta.SnapDeleting})
			}
		}
		syncing := false
		for _, r := range rows {
			if r.State != meta.SnapSyncing {
				continue
			}
			if inflight[r.ID] {
				syncing = true
				continue
			}
			s.do(ctx, "end a lost sync", meta.Command{Op: meta.OpSnapshotEnd, SnapshotEnd: &meta.SnapshotEnd{ID: r.ID, Now: now.UnixNano()}})
		}
		removed, imported, freed, err := d.m.Reconcile(mrows)
		if err != nil {
			s.log.Warn("mirror reconcile failed", "mirror", d.Row.Name, "err", err)
		}
		for _, id := range removed {
			s.do(ctx, "end a deletion", meta.Command{Op: meta.OpSnapshotDelete, SnapshotDelete: &meta.SnapshotDelete{ID: meta.SnapID(id), Done: true, Now: now.UnixNano()}})
		}
		if freed > 0 {
			s.log.Info("mirror swept", "mirror", d.Row.Name, "freed", freed)
		}
		if len(imported) > 0 {
			s.importManifests(ctx, d, imported, now)
			rows = rows[:0]
			for _, sn := range s.st.Snapshots() {
				if sn.Mirror == idx {
					rows = append(rows, sn)
				}
			}
		}
		if !syncing {
			s.heal(ctx, d, rows, now)
		}
		full := d.Total > 0 && d.Free < mirrorReserve(d.Total)
		for _, id := range s.unheld(drop(rows, now, s.pol, full)) {
			s.do(ctx, "thin", meta.Command{Op: meta.OpSnapshotDelete, SnapshotDelete: &meta.SnapshotDelete{ID: id, Now: now.UnixNano()}})
		}
		if !syncing && !full && due(rows, now, s.pol.Every) {
			name := now.UTC().Format("2006-01-02-1504Z")
			err := s.propose(ctx, meta.Command{Op: meta.OpSnapshotBegin, SnapshotBegin: &meta.SnapshotBegin{Mirror: idx, Name: name, Auto: true, Now: now.UnixNano()}})
			if err != nil && !errors.Is(err, meta.ErrExist) { // ErrExist: taken this minute, never retried (I-182)
				s.log.Warn("snapshot not begun", "mirror", d.Row.Name, "err", err)
			}
		}
	}
}

// heal shortens the missing list of every incomplete snapshot on d with the chunks the
// mirror holds now or the pool reads now, and proposes each row's new count (docs/16 §5,
// I-391). The manifest is rewritten first, so a heal lost to a failed proposal is
// proposed again from the shorter manifest on the next tick. Nothing is done while a
// live member reads a codec older than 15, which could not decode the heal.
func (s *mirrorSet) heal(ctx context.Context, d *mirrorDisk, rows []meta.Snapshot, now time.Time) {
	if s.minCodec != nil && s.minCodec() < meta.CodecHeal {
		return
	}
	for _, r := range rows {
		if r.State != meta.SnapIncomplete {
			continue
		}
		left, err := d.m.Heal(ctx, uint32(r.ID), s.read)
		if err != nil {
			s.log.Warn("mirror: snapshot not healed", "mirror", d.Row.Name, "snapshot", r.Name, "err", err)
			continue
		}
		if uint32(left) >= r.Missing {
			continue
		}
		err = s.propose(ctx, meta.Command{Op: meta.OpSnapshotHeal, SnapshotHeal: &meta.SnapshotHeal{ID: r.ID, Missing: uint32(left), Now: now.UnixNano()}})
		if err != nil && !errors.Is(err, meta.ErrNotExist) {
			s.log.Warn("mirror: snapshot heal not recorded", "mirror", d.Row.Name, "snapshot", r.Name, "err", err)
			continue
		}
		s.log.Info("mirror: snapshot healed", "mirror", d.Row.Name, "snapshot", r.Name, "missing", left, "was", r.Missing)
	}
}

// importManifests gives every manifest on a mirror with no catalogue row a row of its
// own and moves the manifest to the row's ID (docs/16 §5, I-183): a mirror removed and
// added back, copied to a larger disk or adopted from another cluster lists its
// snapshots again. A row of the same name and root is an import a crash or an unknown
// outcome interrupted: only the rename is left to do.
func (s *mirrorSet) importManifests(ctx context.Context, d *mirrorDisk, mans []mirror.Manifest, now time.Time) {
	var after uint32
	if all, err := d.m.Manifests(); err == nil {
		for i := range all {
			after = max(after, all[i].ID)
		}
	}
	for i := range mans {
		man := &mans[i]
		name := man.Name
		to, ok := s.importedAs(d.Row.Idx, name, meta.Hash(man.Root))
		for _, try := range []string{man.Name, importName(man.Name, man.ID)} {
			if ok {
				break
			}
			name = try
			err := s.propose(ctx, meta.Command{Op: meta.OpSnapshotImport, SnapshotImport: &meta.SnapshotImport{
				Mirror: d.Row.Idx, After: meta.SnapID(after), Name: try, At: man.At, Auto: man.Auto, Root: meta.Hash(man.Root),
				Files: man.Files, Bytes: man.Bytes, NewBytes: man.NewBytes, Missing: uint32(len(man.Missing)), Now: now.UnixNano(),
			}})
			if err != nil && !errors.Is(err, meta.ErrExist) {
				s.log.Warn("mirror: snapshot not imported", "mirror", d.Row.Name, "snapshot", man.Name, "err", err)
				break
			}
			to, ok = s.importedAs(d.Row.Idx, try, meta.Hash(man.Root))
		}
		if !ok {
			continue // kept on the disk as it is; the next tick tries again
		}
		if err := d.m.Renumber(man.ID, uint32(to), name); err != nil {
			s.log.Warn("mirror: imported snapshot not renumbered", "mirror", d.Row.Name, "snapshot", man.Name, "err", err)
			continue
		}
		s.log.Info("mirror: snapshot imported", "mirror", d.Row.Name, "snapshot", name, "id", to)
	}
}

// importedAs finds the catalogue row of mirror idx that a manifest named name with root
// became.
func (s *mirrorSet) importedAs(idx meta.NodeIdx, name string, root meta.Hash) (meta.SnapID, bool) {
	for _, sn := range s.st.Snapshots() {
		if sn.Mirror == idx && sn.Name == name && sn.Root == root && (sn.State == meta.SnapComplete || sn.State == meta.SnapIncomplete) {
			return sn.ID, true
		}
	}
	return 0, false
}

// importName is the name a manifest is imported under when its own is taken on the
// mirror by another snapshot: "<name>.<old id>", within the name bound.
func importName(name string, id uint32) string {
	suffix := fmt.Sprintf(".%d", id)
	if len(name)+len(suffix) > meta.MaxSnapName {
		name = strings.ToValidUTF8(name[:meta.MaxSnapName-len(suffix)], "")
	}
	return name + suffix
}

func (s *mirrorSet) do(ctx context.Context, what string, cmd meta.Command) {
	if err := s.propose(ctx, cmd); err != nil && !errors.Is(err, meta.ErrNotExist) {
		s.log.Warn("mirror: "+what+" failed", "err", err)
	}
}

// due reports whether a mirror's schedule wants a snapshot now: its newest automatic
// snapshot is older than every, or it has none (an offline stretch makes the first tick
// after attach due at once: the catch-up). every 0 takes none.
func due(rows []meta.Snapshot, now time.Time, every time.Duration) bool {
	if every <= 0 {
		return false
	}
	var newest int64
	for _, r := range rows {
		if r.Auto && r.State != meta.SnapDeleting {
			newest = max(newest, r.At)
		}
	}
	return newest == 0 || now.Sub(time.Unix(0, newest)) >= every
}

// unheld is ids without the snapshots an unfinished restore job reads: thinning and the
// free-space reserve leave them until the job ends (docs/16 §4, I-364).
func (s *mirrorSet) unheld(ids []meta.SnapID) []meta.SnapID {
	if s.held == nil {
		return ids
	}
	held := s.held()
	return slices.DeleteFunc(ids, func(id meta.SnapID) bool { return held[id] })
}

// drop picks the automatic snapshots of one mirror to delete (docs/16 §4): every one
// outside the keep set (the newest of each of the last hourly hour, daily day, weekly
// ISO-week and monthly month buckets, UTC), and when the mirror is under its reserve the
// oldest automatic one besides. A manual snapshot is never chosen; a syncing or deleting
// one neither. Oldest first.
func drop(rows []meta.Snapshot, now time.Time, pol config.MirrorPolicy, full bool) []meta.SnapID {
	var auto []meta.Snapshot
	for _, r := range rows {
		if r.Auto && (r.State == meta.SnapComplete || r.State == meta.SnapIncomplete) {
			auto = append(auto, r)
		}
	}
	sort.Slice(auto, func(i, j int) bool {
		return auto[i].At > auto[j].At || (auto[i].At == auto[j].At && auto[i].ID > auto[j].ID)
	})
	keep := map[meta.SnapID]bool{}
	for _, b := range []struct {
		n   int
		key func(time.Time) int64
	}{
		{pol.Hourly, func(t time.Time) int64 { return t.Unix() / 3600 }},
		{pol.Daily, func(t time.Time) int64 { return t.Unix() / 86400 }},
		{pol.Weekly, func(t time.Time) int64 { return (t.Unix()/86400 + 3) / 7 }}, // ISO weeks start on Monday; 1970-01-01 was a Thursday
		{pol.Monthly, func(t time.Time) int64 { return int64(t.Year())*12 + int64(t.Month()) }},
	} {
		cur := b.key(now.UTC())
		taken := map[int64]bool{}
		for _, r := range auto { // newest first: the first one met in a bucket is its newest
			k := b.key(time.Unix(0, r.At).UTC())
			if cur-k < int64(b.n) && !taken[k] {
				taken[k] = true
				keep[r.ID] = true
			}
		}
	}
	var out []meta.SnapID
	for _, r := range slices.Backward(auto) { // oldest first
		if !keep[r.ID] {
			out = append(out, r.ID)
		}
	}
	if full {
		for _, r := range slices.Backward(auto) {
			if keep[r.ID] {
				out = append(out, r.ID) // the oldest one kept goes for space
				break
			}
		}
	}
	return out
}

// expireSyncs is the leader's part (I-181): a sync older than hold_max is ended as
// failed, so a host that died mid-copy cannot hold GC back for longer than that.
func expireSyncs(ctx context.Context, snaps []meta.Snapshot, now time.Time, holdMax time.Duration, propose func(context.Context, meta.Command) error) int {
	n := 0
	for _, r := range snaps {
		if r.State == meta.SnapSyncing && now.Sub(time.Unix(0, r.At)) > holdMax {
			if err := propose(ctx, meta.Command{Op: meta.OpSnapshotEnd, SnapshotEnd: &meta.SnapshotEnd{ID: r.ID, Now: now.UnixNano()}}); err == nil {
				n++
			}
		}
	}
	return n
}

// report is what `status` shows of this host's configured paths, and the warnings
// `status` and `check` carry (docs/12 §11, I-188): a mirror whose newest snapshot is older
// than stale_after (offline that long, or failing), an online one whose schedule is
// behind by twice every, one under its free-space reserve, and an incomplete snapshot on
// any. clusterMirrors merges the paths into the cluster-wide list.
func (s *mirrorSet) report(now time.Time, snaps []meta.Snapshot) ([]admin.MirrorReport, []string) {
	if s == nil {
		return nil, nil
	}
	s.mu.Lock()
	seen := slices.Clone(s.seen)
	s.mu.Unlock()
	var out []admin.MirrorReport
	var warn []string
	for i := range seen {
		d := &seen[i]
		r := admin.MirrorReport{Idx: d.Row.Idx, Name: d.Row.Name, Self: true, Path: d.Path, Online: d.Online, Reason: d.Reason, Used: d.Used, Free: d.Free, Total: d.Total}
		syncing := false
		var newest *meta.Snapshot // the newest finished snapshot: only it warns when incomplete (I-392)
		for j := range snaps {
			sn := &snaps[j]
			if sn.Mirror != d.Row.Idx || d.Row.Idx == 0 {
				continue
			}
			switch sn.State {
			case meta.SnapSyncing:
				syncing = true
			case meta.SnapComplete, meta.SnapIncomplete:
				r.Newest = max(r.Newest, sn.At)
				if newest == nil || sn.At > newest.At || (sn.At == newest.At && sn.ID > newest.ID) {
					newest = sn
				}
			case meta.SnapDeleting:
			}
		}
		if newest != nil && newest.State == meta.SnapIncomplete {
			warn = append(warn, fmt.Sprintf("mirror %s: snapshot %s is missing %d chunks (the next sync tries them again)", d.Row.Name, newest.Name, newest.Missing))
		}
		name := cmp.Or(d.Row.Name, d.Path)
		age := now.Sub(time.Unix(0, r.Newest))
		switch {
		case d.Row.Idx == 0:
		case r.Newest != 0 && age > s.pol.StaleAfter:
			warn = append(warn, fmt.Sprintf("mirror %s: newest snapshot is %s old (%s)", name, age.Round(time.Hour), cmp.Or(d.Reason, "online")))
		case d.Online && s.pol.Every > 0 && r.Newest != 0 && age > 2*s.pol.Every && !syncing:
			warn = append(warn, fmt.Sprintf("mirror %s: no snapshot for %s while online", name, age.Round(time.Minute)))
		}
		if d.Online && d.Total > 0 && d.Free < mirrorReserve(d.Total) {
			warn = append(warn, fmt.Sprintf("mirror %s: under its free-space reserve; the oldest automatic snapshots go first", name))
		}
		out = append(out, r)
	}
	return out, warn
}

// mirrorProbe is what clusterMirrors needs from the prober (docs/16 §1).
type mirrorProbe interface {
	Mirror(idx meta.NodeIdx) (cluster.MirrorProbe, bool)
	MirrorOnline(idx meta.NodeIdx) bool
}

// clusterMirrors is every mirror of the cluster as `status` shows it (docs/12 §11): each
// registered row with its host, online by the probes, the use and the sync under way from
// its host's last health reply, and the newest complete snapshot from the catalogue; the
// rows of this host carry their path, and this host's configured paths that no row
// claims are listed after them.
func clusterMirrors(nodes []meta.Node, snaps []meta.Snapshot, pr mirrorProbe, self meta.NodeIdx, local []admin.MirrorReport) []admin.MirrorReport {
	names := map[meta.NodeIdx]string{}
	for i := range nodes {
		names[nodes[i].Idx] = nodes[i].Name
	}
	var out []admin.MirrorReport
	for i := range nodes {
		nd := &nodes[i]
		if nd.Kind != meta.KindMirror || nd.State == meta.Removed {
			continue
		}
		r := admin.MirrorReport{Idx: nd.Idx, Name: nd.Name, Host: names[nd.Host], Self: nd.Host == self, Online: pr.MirrorOnline(nd.Idx)}
		if mp, ok := pr.Mirror(nd.Idx); ok {
			h := mp.Health
			r.Reason, r.Used, r.Free, r.Total = h.Reason, h.Used, h.Free, h.Total
			if !mp.LastSeen.IsZero() {
				r.LastSeen = mp.LastSeen.UnixNano()
			}
			if h.Syncing != nil && r.Online {
				r.Syncing = &admin.SyncReport{ID: h.Syncing.ID, Snapshot: h.Syncing.Snapshot, Read: h.Syncing.Read, Total: h.Syncing.Total, BytesNew: h.Syncing.BytesNew}
			}
		} else {
			r.Reason = "not heard from its host yet"
		}
		if !r.Online && r.Reason == "" {
			r.Reason = "its host is not live"
		}
		for j := range snaps {
			if sn := &snaps[j]; sn.Mirror == nd.Idx && (sn.State == meta.SnapComplete || sn.State == meta.SnapIncomplete) {
				r.Newest = max(r.Newest, sn.At)
			}
		}
		for j := range local {
			if local[j].Idx == nd.Idx {
				r.Path = local[j].Path
			}
		}
		out = append(out, r)
	}
	for j := range local {
		if local[j].Idx == 0 {
			out = append(out, local[j])
		}
	}
	return out
}

// health lists this host's registered mirrors for /health (docs/16 §1): online or not,
// the disk's use and the sync under way with its progress.
func (s *mirrorSet) health() []rpc.MirrorHealth {
	if s == nil {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []rpc.MirrorHealth
	for i := range s.seen {
		d := &s.seen[i]
		if d.Row.Idx == 0 { // not registered here: nothing for the other nodes to match
			continue
		}
		h := rpc.MirrorHealth{ID: d.Row.ID, Online: d.Online, Reason: d.Reason, Used: d.Used, Free: d.Free, Total: d.Total}
		if sy, ok := s.syncing[d.Row.Idx]; ok && d.m != nil {
			read, added, _ := d.m.Progress()
			h.Syncing = &rpc.SyncHealth{ID: uint32(sy.id), Snapshot: sy.name, Read: min(read, sy.total), Total: sy.total, BytesNew: added}
		}
		out = append(out, h)
	}
	return out
}

// PrepareMirror marks path as a mirror of this cluster for host (docs/16 §1), or returns
// the ID of the mirror of this cluster already there (a moved disk re-homes). A mirror of
// another cluster is refused unless adopt is set; then its marker is rewritten to this
// cluster and host, and the host imports its snapshots (docs/16 §5, §7).
func PrepareMirror(path string, clusterID [16]byte, hostID string, adopt bool) (string, error) {
	fsys := sysfs.OS{}
	cid := hex.EncodeToString(clusterID[:])
	if mk, err := mirror.ReadMarker(fsys, path); err == nil {
		if mk.ClusterID != cid {
			if !adopt {
				return "", fmt.Errorf("%w (%s): `tailraid mirror add --adopt` takes it over", mirror.ErrForeign, mk.ClusterID)
			}
			if mk, err = mirror.Adopt(fsys, path, cid, hostID); err != nil {
				return "", err
			}
		}
		return mk.MirrorID, nil
	} else if !errors.Is(err, mirror.ErrNotMirror) {
		return "", err
	}
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	id := hex.EncodeToString(b[:])
	return id, mirror.Create(fsys, path, mirror.Marker{ClusterID: cid, MirrorID: id, HostID: hostID, Created: time.Now().UnixNano()})
}
