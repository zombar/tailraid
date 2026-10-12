package node

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/hashicorp/raft"

	"github.com/zombar/tailraid/internal/clock"
	"github.com/zombar/tailraid/internal/config"
	"github.com/zombar/tailraid/internal/meta"
	"github.com/zombar/tailraid/internal/mirror"
	"github.com/zombar/tailraid/internal/sysfs"
)

const tcs = 16 // chunk size of the test volume

// mirrorRig is one host with one mirror over a real FSM: proposals apply at once, and
// the FSM's hook hands snapshot begins to the mirror set as on a node.
type mirrorRig struct {
	t      *testing.T
	f      *meta.FSM
	idx    uint64
	s      *mirrorSet
	clk    *clock.Fake
	path   string
	mirror meta.NodeIdx
	chunks map[meta.Hash][]byte
}

var rigCluster = [16]byte{0xab}

func newMirrorRig(t *testing.T, pol config.MirrorPolicy) *mirrorRig {
	t.Helper()
	r := &mirrorRig{t: t, f: meta.NewFSM(), clk: clock.NewFake(time.Date(2026, 10, 12, 9, 0, 0, 0, time.UTC)), path: filepath.Join(t.TempDir(), "m"), chunks: map[meta.Hash][]byte{}}
	r.ok(meta.Command{Op: meta.OpSetVolume, SetVolume: &meta.SetVolume{ChunkSize: tcs, K: 1, Now: 1}})
	r.ok(meta.Command{Op: meta.OpNodeUpsert, NodeUpsert: &meta.NodeUpsert{Node: meta.Node{ID: "dev-1", Name: "h1", Kind: meta.KindDevice, Addr: "100.64.0.1"}}})
	id, err := PrepareMirror(r.path, rigCluster, "dev-1", false)
	if err != nil {
		t.Fatal(err)
	}
	r.mirror = meta.NodeIdx(r.ok(meta.Command{Op: meta.OpNodeUpsert, NodeUpsert: &meta.NodeUpsert{Node: meta.Node{ID: id, Name: "h1/m1", Kind: meta.KindMirror, Host: 1}}}).Ino)
	var cfg config.Config
	cfg.Node.Mirrors = []config.Mirror{{Path: r.path}}
	r.s = newMirrorSet(sysfs.OS{}, cfg, pol, rigCluster, r.f, r.clk, slog.New(slog.DiscardHandler))
	// self reads the FSM as the node's does: the hook must never call it, or the apply
	// that runs the hook waits on its own lock (a real node hung at its first snapshot)
	r.s.self = func() (meta.NodeIdx, bool) {
		n, ok := r.f.NodeByID("dev-1")
		return n.Idx, ok
	}
	r.s.selfID = "dev-1"
	r.s.propose = func(_ context.Context, c meta.Command) error { return r.apply(c).Err }
	r.s.read = func(_ context.Context, h meta.Hash) ([]byte, error) {
		b, ok := r.chunks[h]
		if !ok {
			return nil, errors.New("unreadable")
		}
		return b, nil
	}
	r.f.SetSnapshotHook(r.s.onBegin)
	r.s.refresh() // the mirror is open, as after the host's first tick
	return r
}

func (r *mirrorRig) apply(c meta.Command) meta.Result {
	r.t.Helper()
	r.idx++
	res, _ := r.f.Apply(&raft.Log{Index: r.idx, Type: raft.LogCommand, Data: meta.MustEncodeCommand(c)}).(meta.Result)
	return res
}

func (r *mirrorRig) ok(c meta.Command) meta.Result {
	r.t.Helper()
	res := r.apply(c)
	if res.Err != nil {
		r.t.Fatalf("op %d: %v", c.Op, res.Err)
	}
	return res
}

// write creates or overwrites name under dir with content (one chunk per tcs bytes).
func (r *mirrorRig) write(dir meta.Ino, name string, content []byte) meta.Ino {
	r.t.Helper()
	in, found := r.f.Lookup(dir, name)
	ino := in.Ino
	if !found {
		ino = r.ok(meta.Command{Op: meta.OpCreate, Create: &meta.Create{Parent: dir, Name: name, Type: meta.TFile, Mode: 0o644, Now: 2}}).Ino
		in, _ = r.f.GetInode(ino)
	}
	c := &meta.Commit{Ino: ino, BaseVersion: in.Version, HasTruncate: true, NewSize: uint64(len(content)), Now: 3}
	for i := 0; i*tcs < len(content); i++ {
		b := content[i*tcs : min((i+1)*tcs, len(content))]
		h := meta.Hash(sha256.Sum256(b))
		r.chunks[h] = slices.Clone(b)
		c.Writes = append(c.Writes, meta.ChunkWrite{Idx: uint32(i), Hash: h})
		c.NewChunks = append(c.NewChunks, meta.ChunkDesc{Hash: h, Size: uint32(len(b)), K: 1, Placement: []meta.NodeIdx{1}})
	}
	r.ok(meta.Command{Op: meta.OpCommit, Commit: c})
	return ino
}

// drain runs every queued sync to its end, as the worker would.
func (r *mirrorRig) drain() {
	r.t.Helper()
	for {
		select {
		case j := <-r.s.jobs:
			r.s.sync(context.Background(), j, func(context.Context) error { return nil })
			r.s.mu.Lock()
			delete(r.s.inflight, j.sn.ID)
			r.s.mu.Unlock()
		default:
			return
		}
	}
}

// read returns the bytes of path in the snapshot named name on the rig's mirror.
func (r *mirrorRig) read(name, path string) ([]byte, error) {
	r.t.Helper()
	m, err := mirror.Open(sysfs.OS{}, r.path, "")
	if err != nil {
		return nil, err
	}
	mans, _ := m.Manifests()
	for i := range mans {
		if mans[i].Name != name {
			continue
		}
		e, err := m.Resolve(meta.Hash(mans[i].Root), path)
		if err != nil {
			return nil, err
		}
		var b bytes.Buffer
		err = m.ReadFile(e, mans[i].ChunkSize, &b)
		return b.Bytes(), err
	}
	return nil, os.ErrNotExist
}

func pol() config.MirrorPolicy {
	return config.MirrorPolicy{Every: time.Hour, Hourly: 3, Daily: 2, Weekly: 1, Monthly: 1, HoldMax: 24 * time.Hour, StaleAfter: 7 * 24 * time.Hour}
}

// Invariant: I-179
// Lifecycle: L-214
func TestMirror_SyncCapturesIndex(t *testing.T) {
	r := newMirrorRig(t, pol())
	f := r.write(meta.RootIno, "doc", []byte("version one of the document"))
	r.s.tick(context.Background()) // due: no snapshot yet, the mirror is online
	snaps := r.f.Snapshots()
	if len(snaps) != 1 || snaps[0].State != meta.SnapSyncing || !snaps[0].Auto {
		t.Fatalf("after the tick %+v", snaps)
	}
	// the volume moves on before the copy runs: the snapshot is still the volume as of
	// its index
	r.write(meta.RootIno, "doc", []byte("version two, written after the snapshot began"))
	r.write(meta.RootIno, "new", []byte("born later"))
	_ = f
	r.drain()
	sn, _ := r.f.SnapshotRow(snaps[0].ID)
	if sn.State != meta.SnapComplete || sn.Files != 1 || sn.Root == (meta.Hash{}) {
		t.Fatalf("row after the sync %+v", sn)
	}
	got, err := r.read(sn.Name, "/doc")
	if err != nil || string(got) != "version one of the document" {
		t.Fatalf("snapshot doc: %q %v", got, err)
	}
	if _, err := r.read(sn.Name, "/new"); !errors.Is(err, mirror.ErrNotFound) {
		t.Fatalf("a file created after the snapshot is in it: %v", err)
	}
}

// Invariant: I-179
func TestMirror_SyncExcludesReservedEphemeralTrash(t *testing.T) {
	r := newMirrorRig(t, pol())
	reserved := r.ok(meta.Command{Op: meta.OpMkdir, Mkdir: &meta.Mkdir{Parent: meta.RootIno, Name: meta.ReservedDir, Mode: 0o755, Now: 1}}).Ino
	r.write(reserved, "thumb.jpg", []byte("a thumbnail"))
	r.write(meta.RootIno, ".song.tailraid-upload", []byte("half an upload"))
	r.write(meta.RootIno, "._song", []byte("finder info"))
	r.write(meta.RootIno, ".DS_Store", []byte("finder view"))
	r.write(meta.RootIno, "gone", []byte("deleted before the snapshot"))
	r.ok(meta.Command{Op: meta.OpTrash, Trash: &meta.Trash{Parent: meta.RootIno, Name: "gone", Now: 5}})
	r.write(meta.RootIno, "kept", []byte("an ordinary file"))
	r.s.tick(context.Background())
	r.drain()
	sn := r.f.Snapshots()[0]
	if sn.Files != 1 {
		t.Fatalf("snapshot holds %d files, want only the ordinary one", sn.Files)
	}
	for _, p := range []string{"/" + meta.ReservedDir, "/.song.tailraid-upload", "/._song", "/.DS_Store", "/gone"} {
		if _, err := r.read(sn.Name, p); !errors.Is(err, mirror.ErrNotFound) {
			t.Errorf("%s is in the snapshot: %v", p, err)
		}
	}
}

// Invariant: I-182
func TestSchedule_TakesAndThins_Table(t *testing.T) {
	now := time.Date(2026, 10, 12, 12, 30, 0, 0, time.UTC)
	at := func(d time.Duration) int64 { return now.Add(-d).UnixNano() }
	rows := []meta.Snapshot{
		{ID: 1, Auto: true, State: meta.SnapComplete, At: at(40 * 24 * time.Hour)}, // last month: the monthly
		{ID: 2, Auto: true, State: meta.SnapComplete, At: at(3 * 24 * time.Hour)},  // this week's oldest day: weekly and daily?
		{ID: 3, Auto: true, State: meta.SnapComplete, At: at(26 * time.Hour)},      // yesterday: the daily
		{ID: 4, Auto: true, State: meta.SnapComplete, At: at(150 * time.Minute)},   // 10:00: an hourly
		{ID: 5, Auto: true, State: meta.SnapComplete, At: at(90 * time.Minute)},    // 11:00: an hourly
		{ID: 6, Auto: true, State: meta.SnapComplete, At: at(80 * time.Minute)},    // 11:10: the same hour, newer
		{ID: 7, Auto: true, State: meta.SnapIncomplete, At: at(30 * time.Minute)},  // 12:00: an hourly
		{ID: 8, Auto: false, State: meta.SnapComplete, At: at(400 * 24 * time.Hour)},
		{ID: 9, Auto: true, State: meta.SnapSyncing, At: at(time.Minute)},
		{ID: 10, Auto: true, State: meta.SnapDeleting, At: at(500 * time.Hour)},
	}
	p := config.MirrorPolicy{Every: time.Hour, Hourly: 3, Daily: 2, Weekly: 1, Monthly: 1}
	got := drop(rows, now, p, false)
	// hourly keeps 7, 6 (newest of 11:00) and 4; daily keeps 7 and 3; weekly 7; monthly
	// 7 and, since only this month counts with 1, nothing older: 1 and 2 go, and 5 (the
	// older one of its hour). The manual 8, the syncing 9 and the deleting 10 are never
	// chosen.
	if want := []meta.SnapID{1, 2, 5}; !slices.Equal(got, want) {
		t.Fatalf("drop %v, want %v", got, want)
	}
	if full := drop(rows, now, p, true); !slices.Equal(full, []meta.SnapID{1, 2, 5, 3}) {
		t.Fatalf("drop under the reserve %v: want the oldest kept automatic one (3) besides", full)
	}
	none := config.MirrorPolicy{}
	if got := drop(rows, now, none, false); !slices.Equal(got, []meta.SnapID{1, 2, 3, 4, 5, 6, 7}) {
		t.Fatalf("keep 0 everywhere drops every automatic one: %v", got)
	}
	// due: never with every 0; at once with no snapshot; by the newest automatic one
	for _, c := range []struct {
		rows  []meta.Snapshot
		every time.Duration
		want  bool
	}{
		{nil, 0, false},
		{nil, time.Hour, true},
		{rows[6:7], time.Hour, false},                                       // 30 minutes old
		{rows[4:5], time.Hour, true},                                        // 90 minutes old
		{[]meta.Snapshot{rows[7]}, time.Hour, true},                         // a manual one does not count
		{[]meta.Snapshot{rows[9]}, time.Hour, true},                         // nor a deleting one
		{[]meta.Snapshot{rows[8]}, time.Hour, false},                        // a sync just begun counts
		{[]meta.Snapshot{{Auto: true, At: at(time.Hour)}}, time.Hour, true}, // exactly every
	} {
		if got := due(c.rows, now, c.every); got != c.want {
			t.Errorf("due(%v, %s) = %v, want %v", c.rows, c.every, got, c.want)
		}
	}
}

// Invariant: I-182
func TestSchedule_OfflineMirrorNothingHeld_CatchUpOnAttach(t *testing.T) {
	r := newMirrorRig(t, pol())
	r.write(meta.RootIno, "a", []byte("some bytes"))
	away := r.path + ".unplugged"
	if err := os.Rename(r.path, away); err != nil {
		t.Fatal(err)
	}
	for range 3 {
		r.s.tick(context.Background())
		r.clk.Advance(2 * time.Hour)
	}
	if n := len(r.f.Snapshots()); n != 0 {
		t.Fatalf("%d snapshots begun for an offline mirror: nothing may be held for it", n)
	}
	if rep, warn := r.s.report(r.clk.Now(), nil); len(rep) != 1 || rep[0].Online || rep[0].Reason == "" || len(warn) != 0 {
		t.Fatalf("report of the offline mirror %+v %v", rep, warn)
	}
	if err := os.Rename(away, r.path); err != nil {
		t.Fatal(err)
	}
	r.s.tick(context.Background()) // the catch-up: due at once on the first tick back
	if snaps := r.f.Snapshots(); len(snaps) != 1 || snaps[0].State != meta.SnapSyncing {
		t.Fatalf("after attach %+v", snaps)
	}
	r.drain()
	if rep, _ := r.s.report(r.clk.Now(), r.f.Snapshots()); !rep[0].Online || rep[0].Newest == 0 {
		t.Fatalf("report after the catch-up %+v", rep)
	}
}

// Invariant: I-182, I-183
func TestSchedule_ThinThenSweep(t *testing.T) {
	p := pol()
	p.Hourly, p.Daily, p.Weekly, p.Monthly = 1, 0, 0, 0
	r := newMirrorRig(t, p)
	for i := range 3 {
		r.write(meta.RootIno, "f", bytes.Repeat([]byte{byte(i + 1)}, 2*tcs))
		r.s.tick(context.Background())
		r.drain()
		r.clk.Advance(time.Hour)
	}
	// a manual snapshot is permanent: no thinning or reserve ever takes it
	r.ok(meta.Command{Op: meta.OpSnapshotBegin, SnapshotBegin: &meta.SnapshotBegin{Mirror: r.mirror, Name: "keep me", Now: r.clk.Now().UnixNano()}})
	r.drain()
	r.s.tick(context.Background()) // marks the old automatic ones Deleting, begins a new one
	r.drain()
	r.s.tick(context.Background()) // the host sweeps them off the disk and ends the deletions
	var names []string
	for _, sn := range r.f.Snapshots() {
		names = append(names, sn.Name+"/"+sn.State.String())
	}
	if len(names) != 2 || !strings.Contains(strings.Join(names, " "), "keep me/complete") {
		t.Fatalf("catalogue after thinning %v: want the newest automatic one and the manual one", names)
	}
	m, _ := mirror.Open(sysfs.OS{}, r.path, "")
	if mans, _ := m.Manifests(); len(mans) != 2 {
		t.Fatalf("%d manifests on the disk, want 2", len(mans))
	}
}

// Invariant: I-181
func TestSchedule_LostAndExpiredSyncsEnded(t *testing.T) {
	p := pol()
	p.Every = 0 // no automatic snapshot: this test begins its own
	r := newMirrorRig(t, p)
	r.f.SetSnapshotHook(nil) // a restart: the begin's clone is gone
	r.ok(meta.Command{Op: meta.OpSnapshotBegin, SnapshotBegin: &meta.SnapshotBegin{Mirror: r.mirror, Name: "lost", Now: r.clk.Now().UnixNano()}})
	r.f.SetSnapshotHook(r.s.onBegin)
	r.s.tick(context.Background())
	for _, sn := range r.f.Snapshots() {
		if sn.Name == "lost" {
			t.Fatalf("a sync whose clone is gone still holds GC: %+v", sn)
		}
	}
	// the leader's part: a sync older than hold_max is ended, whoever its host is
	r.f.SetSnapshotHook(nil)
	id := r.ok(meta.Command{Op: meta.OpSnapshotBegin, SnapshotBegin: &meta.SnapshotBegin{Mirror: r.mirror, Name: "stuck", Now: r.clk.Now().UnixNano()}}).SnapID
	propose := func(_ context.Context, c meta.Command) error { return r.apply(c).Err }
	if n := expireSyncs(context.Background(), r.f.Snapshots(), r.clk.Now().Add(time.Hour), 24*time.Hour, propose); n != 0 {
		t.Fatalf("a young sync was ended")
	}
	if n := expireSyncs(context.Background(), r.f.Snapshots(), r.clk.Now().Add(25*time.Hour), 24*time.Hour, propose); n != 1 {
		t.Fatalf("expired %d syncs, want 1", n)
	}
	if _, ok := r.f.SnapshotRow(id); ok {
		t.Fatal("the stuck sync's row survived hold_max")
	}
}

// Invariant: I-183
// No user path removes a snapshot: SnapshotDelete is proposed by the host's mirror tick
// alone, so no admin op, CLI verb, member op or web route can carry it.
func TestAdminOps_NoSnapshotDelete(t *testing.T) {
	root := filepath.Join("..", "..")
	var users []string
	for _, dir := range []string{"cmd", "internal"} {
		err := filepath.WalkDir(filepath.Join(root, dir), func(p string, d os.DirEntry, err error) error {
			if err != nil || d.IsDir() || !strings.HasSuffix(p, ".go") || strings.HasSuffix(p, "_test.go") {
				return err
			}
			b, err := os.ReadFile(p)
			if err != nil {
				return err
			}
			if strings.Contains(string(b), "OpSnapshotDelete") && !strings.Contains(p, filepath.Join("internal", "meta")) {
				users = append(users, filepath.ToSlash(p))
			}
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	if want := []string{"../../internal/node/mirrors.go"}; !slices.Equal(users, want) {
		t.Fatalf("SnapshotDelete is proposed from %v, want only %v", users, want)
	}
}

// warnLog points the rig's mirror set at a buffer that takes its warnings.
func (r *mirrorRig) warnLog() *bytes.Buffer {
	var b bytes.Buffer
	r.s.log = slog.New(slog.NewTextHandler(&b, &slog.HandlerOptions{Level: slog.LevelWarn}))
	return &b
}

// countProposals wraps the rig's proposer and counts what the mirror set proposes.
func (r *mirrorRig) countProposals() map[meta.Op]int {
	n := map[meta.Op]int{}
	r.s.propose = func(_ context.Context, c meta.Command) error {
		n[c.Op]++
		return r.apply(c).Err
	}
	return n
}

// readd removes the rig's mirror row and registers the disk again, as `mirror remove`
// then `mirror add` do: the catalogue forgets every snapshot on it.
func (r *mirrorRig) readd() {
	r.t.Helper()
	row, _ := r.f.Node(r.mirror)
	r.ok(meta.Command{Op: meta.OpNodeSetState, NodeSetState: &meta.NodeSetState{Idx: r.mirror, State: meta.Removed, Now: r.clk.Now().UnixNano()}})
	r.mirror = meta.NodeIdx(r.ok(meta.Command{Op: meta.OpNodeUpsert, NodeUpsert: &meta.NodeUpsert{Node: meta.Node{ID: row.ID, Name: row.Name, Kind: meta.KindMirror, Host: 1}}}).Ino)
	r.s.refresh()
}

func (r *mirrorRig) manifests() []mirror.Manifest {
	r.t.Helper()
	m, err := mirror.Open(sysfs.OS{}, r.path, "")
	if err != nil {
		r.t.Fatal(err)
	}
	mans, err := m.Manifests()
	if err != nil {
		r.t.Fatal(err)
	}
	return mans
}

// An automatic snapshot whose minute name is taken counts as taken that minute: one
// attempt a tick, no warning, and the next minute's name begins (docs/16 §4).
// Invariant: I-182
func TestSchedule_AutoNameTaken_NoRetry(t *testing.T) {
	r := newMirrorRig(t, pol())
	r.write(meta.RootIno, "a", []byte("bytes"))
	minute := r.clk.Now().UTC().Format("2006-01-02-1504Z")
	r.ok(meta.Command{Op: meta.OpSnapshotBegin, SnapshotBegin: &meta.SnapshotBegin{Mirror: r.mirror, Name: minute, Now: r.clk.Now().UnixNano()}}) // a manual one under the minute's name
	r.drain()
	warn := r.warnLog()
	n := r.countProposals()
	r.s.tick(context.Background())
	if n[meta.OpSnapshotBegin] != 1 || warn.Len() != 0 {
		t.Fatalf("begins %d, warnings %q: want one attempt and no warning", n[meta.OpSnapshotBegin], warn.String())
	}
	if snaps := r.f.Snapshots(); len(snaps) != 1 || snaps[0].Auto {
		t.Fatalf("catalogue %+v: want only the manual one", snaps)
	}
	r.clk.Advance(time.Minute)
	r.s.tick(context.Background())
	if snaps := r.f.Snapshots(); len(snaps) != 2 || !snaps[1].Auto || snaps[1].Name == minute {
		t.Fatalf("the next minute: %+v", snaps)
	}
}

// While a live member reads a codec older than 7 the host proposes nothing: no begin,
// no thinning, no end of a lost sync, no import (docs/16 §4).
// Invariant: I-182
func TestSchedule_SkipsWhileCodecOld(t *testing.T) {
	p := pol()
	p.Hourly, p.Daily, p.Weekly, p.Monthly = 1, 0, 0, 0
	r := newMirrorRig(t, p)
	r.write(meta.RootIno, "a", []byte("bytes"))
	for range 2 {
		r.s.tick(context.Background())
		r.drain()
		r.clk.Advance(2 * time.Hour)
	}
	r.f.SetSnapshotHook(nil) // a lost sync as well
	r.ok(meta.Command{Op: meta.OpSnapshotBegin, SnapshotBegin: &meta.SnapshotBegin{Mirror: r.mirror, Name: "lost", Now: r.clk.Now().UnixNano()}})
	r.f.SetSnapshotHook(r.s.onBegin)
	codec := uint8(meta.CodecMirrors - 1)
	r.s.minCodec = func() uint8 { return codec }
	n := r.countProposals()
	for range 3 {
		r.s.tick(context.Background())
		r.clk.Advance(2 * time.Hour)
	}
	if len(n) != 0 {
		t.Fatalf("proposed %v while a member reads codec %d", n, codec)
	}
	if rep, _ := r.s.report(r.clk.Now(), r.f.Snapshots()); len(rep) != 1 || !rep[0].Online {
		t.Fatalf("the refresh still runs: %+v", rep)
	}
	codec = meta.CodecMirrors
	r.s.tick(context.Background())
	if n[meta.OpSnapshotEnd] != 1 || n[meta.OpSnapshotDelete] == 0 {
		t.Fatalf("once every member reads codec 7: %v", n)
	}
}

// A Deleting row whose mirror went offline before its host swept it waits; the first
// tick after attach removes the manifest, sweeps and ends the deletion (docs/16 §5).
// Invariant: I-183
func TestMirror_OfflineDeleteReconciledOnAttach(t *testing.T) {
	p := pol()
	p.Every, p.Hourly, p.Daily, p.Weekly, p.Monthly = 0, 0, 1, 0, 0 // today's newest stays
	r := newMirrorRig(t, p)
	auto := func(name string, content byte) meta.SnapID {
		r.write(meta.RootIno, "f", bytes.Repeat([]byte{content}, 2*tcs))
		id := r.ok(meta.Command{Op: meta.OpSnapshotBegin, SnapshotBegin: &meta.SnapshotBegin{Mirror: r.mirror, Name: name, Auto: true, Now: r.clk.Now().UnixNano()}}).SnapID
		r.drain()
		r.clk.Advance(2 * time.Hour)
		return id
	}
	old := auto("old", 1)
	auto("new", 2)
	r.s.tick(context.Background()) // thinning marks the old one Deleting
	if sn, _ := r.f.SnapshotRow(old); sn.State != meta.SnapDeleting {
		t.Fatalf("old row %+v, want deleting", sn)
	}
	away := r.path + ".unplugged"
	if err := os.Rename(r.path, away); err != nil {
		t.Fatal(err)
	}
	for range 3 {
		r.s.tick(context.Background())
		r.clk.Advance(time.Hour)
	}
	if sn, ok := r.f.SnapshotRow(old); !ok || sn.State != meta.SnapDeleting {
		t.Fatalf("the deletion of an offline mirror's snapshot moved on: %+v %v", sn, ok)
	}
	if err := os.Rename(away, r.path); err != nil {
		t.Fatal(err)
	}
	r.s.tick(context.Background())
	if _, ok := r.f.SnapshotRow(old); ok {
		t.Fatal("the row survived the tick after attach")
	}
	if mans := r.manifests(); len(mans) != 1 || mans[0].Name != "new" {
		t.Fatalf("manifests after attach %+v", mans)
	}
	if got, err := r.read("new", "/f"); err != nil || !bytes.Equal(got, bytes.Repeat([]byte{2}, 2*tcs)) {
		t.Fatalf("the kept snapshot: %v", err)
	}
	m, _ := mirror.Open(sysfs.OS{}, r.path, "")
	if old := sha256.Sum256(bytes.Repeat([]byte{1}, tcs)); m.HasObject(old) {
		t.Fatal("the deleted snapshot's object was not swept")
	}
}

// A mirror removed and added back has no rows: each manifest on it becomes a row again,
// with its own name, time and kind, under an ID above every manifest on the disk; a
// crash between the import and the rename is finished by the next tick; a name another
// snapshot took is imported as <name>.<old id>; later syncs never touch the imported
// manifests (docs/16 §5).
// Invariant: I-183
func TestMirror_RowlessManifestsImportedOnTick(t *testing.T) {
	p := pol()
	p.Every = 0
	r := newMirrorRig(t, p)
	take := func(name string, auto bool, content string) {
		r.write(meta.RootIno, "doc", []byte(content))
		r.ok(meta.Command{Op: meta.OpSnapshotBegin, SnapshotBegin: &meta.SnapshotBegin{Mirror: r.mirror, Name: name, Auto: auto, Now: r.clk.Now().UnixNano()}})
		r.drain()
		r.clk.Advance(time.Hour)
	}
	take("a", true, "first")
	take("b", false, "second")
	take("c", false, "third")
	before := r.f.Snapshots()
	r.readd()
	if n := len(r.f.Snapshots()); n != 0 {
		t.Fatalf("%d rows after mirror remove", n)
	}
	take("c", false, "a new c, taken before the host's tick") // takes the name "c"
	// a crash after an import of "b" was proposed, before its rename: the row exists
	bman := before[1]
	r.ok(meta.Command{Op: meta.OpSnapshotImport, SnapshotImport: &meta.SnapshotImport{Mirror: r.mirror, After: 3, Name: "b", At: bman.At, Root: bman.Root, Files: bman.Files, Bytes: bman.Bytes, Now: r.clk.Now().UnixNano()}})
	warn := r.warnLog()
	r.s.tick(context.Background())
	if warn.Len() != 0 {
		t.Fatalf("warnings: %s", warn)
	}
	byName := map[string]meta.Snapshot{}
	for _, sn := range r.f.Snapshots() {
		byName[sn.Name] = sn
	}
	if len(byName) != 4 {
		t.Fatalf("catalogue after the tick %+v", byName)
	}
	for _, c := range []struct {
		name, was, content string
	}{{"a", "a", "first"}, {"b", "b", "second"}, {"c.3", "c", "third"}, {"c", "c", "a new c, taken before the host's tick"}} {
		sn, ok := byName[c.name]
		if !ok || sn.State != meta.SnapComplete {
			t.Fatalf("%s: %+v", c.name, sn)
		}
		if got, err := r.read(c.name, "/doc"); err != nil || string(got) != c.content {
			t.Errorf("%s reads %q %v", c.name, got, err)
		}
		for _, old := range before {
			if old.Name == c.was && c.name != "c" && (sn.At != old.At || sn.Auto != old.Auto || sn.Root != old.Root || sn.ID <= 3) {
				t.Errorf("%s imported as %+v from %+v", c.name, sn, old)
			}
		}
	}
	ids := map[uint32]bool{}
	for _, man := range r.manifests() {
		ids[man.ID] = true
		if sn := byName[man.Name]; uint32(sn.ID) != man.ID {
			t.Errorf("manifest %s carries ID %d, its row %d", man.Name, man.ID, sn.ID)
		}
	}
	if len(ids) != 4 {
		t.Fatalf("manifest IDs %v: want one file per row", ids)
	}
	// nothing is left to import, and the next sync takes a fresh ID
	n := r.countProposals()
	r.s.tick(context.Background())
	if n[meta.OpSnapshotImport] != 0 {
		t.Fatalf("a second tick imported again: %v", n)
	}
	take("d", false, "fourth")
	if len(r.manifests()) != 5 {
		t.Fatal("a sync after the imports replaced a manifest")
	}
}

// freeFS reports a chosen free space for every path, as a nearly full mirror disk would.
type freeFS struct {
	sysfs.OS
	free, total uint64
}

func (f freeFS) Free(string) (free, total uint64, err error) { return f.free, f.total, nil }

// The warnings `status` and `check` carry for a host's mirrors (docs/12 §11): none for a
// healthy one; an incomplete snapshot; a schedule behind by twice every while online; a
// mirror under its free-space reserve; and one whose newest snapshot is older than
// stale_after, offline that long.
// Invariant: I-188
func TestCheck_MirrorWarnings(t *testing.T) {
	r := newMirrorRig(t, pol())
	r.write(meta.RootIno, "a", []byte("bytes"))
	r.s.tick(context.Background())
	r.drain()
	warns := func() string {
		r.s.refresh()
		_, w := r.s.report(r.clk.Now(), r.f.Snapshots())
		return strings.Join(w, "\n")
	}
	if w := warns(); w != "" {
		t.Fatalf("a healthy mirror warns: %s", w)
	}
	// an incomplete snapshot: its chunk is unreadable
	r.write(meta.RootIno, "lost", []byte("this chunk cannot be read"))
	for h := range r.chunks {
		if string(r.chunks[h]) == "this chunk canno" {
			delete(r.chunks, h)
		}
	}
	r.ok(meta.Command{Op: meta.OpSnapshotBegin, SnapshotBegin: &meta.SnapshotBegin{Mirror: r.mirror, Name: "partial", Now: r.clk.Now().UnixNano()}})
	r.drain()
	if w := warns(); !strings.Contains(w, "snapshot partial is missing 1 chunks") {
		t.Fatalf("incomplete: %q", w)
	}
	// the schedule is behind: the newest snapshot is older than twice every
	r.clk.Advance(3 * time.Hour)
	if w := warns(); !strings.Contains(w, "no snapshot for 3h0m0s while online") {
		t.Fatalf("behind: %q", w)
	}
	// under the reserve: max(5 %, 4 GiB) of the filesystem
	r.s.fsys = freeFS{free: 3 << 30, total: 100 << 30}
	if w := warns(); !strings.Contains(w, "under its free-space reserve") {
		t.Fatalf("reserve: %q", w)
	}
	r.s.fsys = freeFS{free: 6 << 30, total: 100 << 30}
	if w := warns(); strings.Contains(w, "reserve") {
		t.Fatalf("6 GiB free of 100 GiB is above the reserve: %q", w)
	}
	r.s.fsys = sysfs.OS{}
	// offline past stale_after
	if err := os.Rename(r.path, r.path+".away"); err != nil {
		t.Fatal(err)
	}
	r.clk.Advance(8 * 24 * time.Hour)
	w := warns()
	if !strings.Contains(w, "mirror h1/m1: newest snapshot is 195h0m0s old (not attached") || strings.Contains(w, "while online") {
		t.Fatalf("stale: %q", w)
	}
	if rep, _ := r.s.report(r.clk.Now(), r.f.Snapshots()); len(rep) != 1 || rep[0].Name != "h1/m1" || rep[0].Online {
		t.Fatalf("an unplugged mirror is reported by its name: %+v", rep)
	}
}
