package soak

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/zombar/tailraid/internal/admin"
	"github.com/zombar/tailraid/internal/rpc"
)

// --- 19 snapshots --------------------------------------------------------------------------
//
// Snapshots on a mirror disk (docs/16) end to end. The environment's scratch mirror is
// registered on its host and comes online within a tick; the phase's files are written
// and a manual snapshot taken. Then one file is overwritten in place, one renamed, a
// folder deleted and purged, one file truncated and a new one written, and GC collects
// the overwritten chunks from the pool. Every file of the first snapshot reads byte-exact
// under /.snapshots through another gateway, the overwritten one from the mirror, the new
// file absent; a write, a mkdir and a remove under /.snapshots are refused. A second
// snapshot adds only the changed chunks and trees. The versions of the overwritten file
// are two contents; a revert gives back the first snapshot's bytes, and a tree restore
// of the deleted folder under a new name its files. The mirror is then unplugged: the
// catalogue still lists its snapshots, reads inside fail, nothing is begun for it, and
// on its return a catch-up snapshot begins within a tick. Manual snapshots are
// permanent (I-183), so the phase takes them only on the environment's scratch mirror;
// on a live cluster it reads the snapshots the cluster has.

// SnapshotInfo is one catalogue row as `snapshot ls --json` prints it (docs/12 §11).
type SnapshotInfo struct {
	ID       uint32 `json:"id"`
	Mirror   string `json:"mirror"`
	Name     string `json:"name"`
	Index    uint64 `json:"index"`
	At       int64  `json:"at"`
	Auto     bool   `json:"auto"`
	State    string `json:"state"`
	Files    uint64 `json:"files"`
	Bytes    uint64 `json:"bytes"`
	NewBytes uint64 `json:"new_bytes"`
	Missing  uint32 `json:"missing,omitempty"`
}

// versionsReply is the answer of the versions op (`tailraid versions --json`).
type versionsReply struct {
	Versions []struct {
		Current   bool   `json:"current"`
		Size      uint64 `json:"size"`
		Snapshots []struct {
			Mirror string `json:"mirror"`
			Name   string `json:"name"`
		} `json:"snapshots"`
	} `json:"versions"`
	Offline []struct {
		Mirror    string `json:"mirror"`
		Snapshots int    `json:"snapshots"`
	} `json:"offline"`
}

// restoreReply is the answer of snapshot_restore and versions_copy.
type restoreReply struct {
	Path   string `json:"path"`
	Report struct {
		Files    int      `json:"files"`
		Dirs     int      `json:"dirs"`
		Uploaded int      `json:"uploaded"`
		Skipped  []string `json:"skipped"`
	} `json:"report"`
}

// The phase's report records (report.go draws them): one snapshotEvent per snapshot the
// phase saw complete, mirrorOffline and mirrorOnline around the unplugged stretch, and
// one roundTripEvent whose text closes the table.
const (
	snapshotEvent  = "snapshot"
	mirrorOffline  = "mirror offline"
	mirrorOnline   = "mirror online"
	roundTripEvent = "snapshot round trip"
)

// snapshotDir is a mirror row name as the mounts show it (pike/m1 → pike-m1).
func snapshotDir(mirror string) string { return strings.ReplaceAll(mirror, "/", "-") }

func phaseSnapshots(ctx context.Context, r *Run) error {
	gws := r.gateways()
	if len(gws) == 0 {
		r.skip("no gateway reachable")
		return nil
	}
	if !r.Env.Caps().Mirror {
		return r.snapshotsReadOnly(ctx, gws)
	}
	if !r.Env.Caps().Mutate {
		r.skip("a snapshot is permanent: the phase takes them only with Caps.Mutate")
		return nil
	}
	ms := &mirrorRun{r: r}
	return ms.run(ctx, gws)
}

// mirrorRun is phase 19 on the environment's scratch mirror.
type mirrorRun struct {
	r      *Run
	name   string  // the mirror's row name
	host   NodeRef // its host
	writer NodeRef // the gateway that writes
	reader NodeRef // another gateway, which reads the snapshots
	dir    string
	labels map[string]string       // snapshot name → its label in the report
	reads  map[string]string       // snapshot name → how its read went
	first  string                  // the mirror's first snapshot
	seen   map[uint32]SnapshotInfo // every row the phase saw, by ID: the schedule thins automatic ones within the hour
}

func (m *mirrorRun) run(ctx context.Context, gws []NodeRef) error {
	r := m.r
	m.writer, m.reader = gws[0], gws[len(gws)-1]
	m.dir = join(r.Root, "snapshots")
	m.labels, m.reads = map[string]string{}, map[string]string{}
	// the phase's files, committed before the mirror exists, so its first sync holds them
	cs := r.Profile.ChunkSize
	before := model{
		join(m.dir, "over.bin"):        r.randomBytes(2*cs + 100),
		join(m.dir, "keep.bin"):        r.randomBytes(cs + 7),
		join(m.dir, "old-name.txt"):    []byte("renamed after the first snapshot\n"),
		join(m.dir, "trunc.bin"):       r.randomBytes(2*cs + 50),
		join(m.dir, "day2", "a.txt"):   []byte("day two, a\n"),
		join(m.dir, "day2", "b.txt"):   []byte("day two, b\n"),
		join(m.dir, "day2", "big.bin"): r.randomBytes(cs + 1),
	}
	wv := r.volume(m.writer)
	if err := r.writeModel(wv, before); err != nil {
		r.checkAt("I-7", "snapshot files written", false, err.Error(), m.dir, "", m.writer.Name)
		return nil
	}
	if !m.drained(ctx) {
		return nil
	}
	name, host, err := r.Env.AddMirror(ctx)
	if err != nil {
		r.check("I-188", "mirror add registers the scratch mirror", false, err.Error())
		return nil
	}
	if name == "" {
		r.skip("the environment has no scratch mirror")
		return nil
	}
	m.name, m.host = name, host
	for _, g := range gws { // read through a gateway that is neither the host nor the writer where there is one
		if g.Idx != host.Idx && g.Idx != m.writer.Idx {
			m.reader = g
		}
	}
	r.setMirrorHost(host)
	if ok := r.poll(ctx, 3*time.Minute, 2*time.Second, func() bool { return m.online() == 1 }); !ok {
		r.check("I-188", "the mirror is online on its host within a tick", false, fmt.Sprintf("%s on %s: %s", name, host.Name, m.mirrorReason()))
		return nil
	}
	r.check("I-188", "the mirror is online on its host within a tick", true, fmt.Sprintf("%s on %s", name, host.Name))
	u := r.undoer(ctx)
	defer u.run()
	// the phases after this one run with the mirror unplugged, as a mirror usually is: a
	// one-minute schedule would otherwise sync and hold GC through every later phase
	u.add("unplug the mirror for the later phases", func(c context.Context) error { return r.Env.SetMirrorOnline(c, m.name, false) })

	// the schedule's own first sync may already run (a mirror with no automatic snapshot
	// is due at once); the manual one waits for it
	snap1 := fmt.Sprintf("soak-%d", time.Now().Unix())
	first, ok := m.take(ctx, snap1)
	if !ok {
		return nil
	}
	rows, _ := m.catalogue(ctx)
	if len(rows) > 0 {
		m.first = rows[len(rows)-1].Name // the catalogue is newest first
	}
	var modelBytes int
	for _, b := range before {
		modelBytes += len(b)
	}
	var firstNew uint64
	for i := range rows {
		if rows[i].Name == m.first {
			firstNew = rows[i].NewBytes
		}
	}
	r.check("I-180 I-179", "the mirror's first snapshot copies the volume", firstNew >= uint64(modelBytes),
		fmt.Sprintf("%s added %s, the phase's files are %s", m.first, humanBytes(float64(firstNew)), humanBytes(float64(modelBytes))))
	oldChunks, _ := r.chunksOf(ctx, join(m.dir, "over.bin"))

	// the changes
	over := r.randomBytes(len(before[join(m.dir, "over.bin")]))
	newBin := r.randomBytes(cs + 3)
	changes := []struct {
		what string
		do   func() error
	}{
		{"overwrite over.bin in place", func() error { return wv.WriteAt(join(m.dir, "over.bin"), 0, over) }},
		{"rename old-name.txt", func() error { return wv.Rename(join(m.dir, "old-name.txt"), join(m.dir, "new-name.txt")) }},
		{"truncate trunc.bin", func() error { return wv.Truncate(join(m.dir, "trunc.bin"), int64(cs/2)) }},
		{"delete day2", func() error {
			for _, f := range []string{"a.txt", "b.txt", "big.bin"} {
				if err := wv.Remove(join(m.dir, "day2", f)); err != nil {
					return err
				}
			}
			return wv.Remove(join(m.dir, "day2"))
		}},
		{"write new.bin", func() error { return wv.WriteAt(join(m.dir, "new.bin"), 0, newBin) }},
	}
	for _, c := range changes {
		if err := c.do(); err != nil {
			r.checkAt("I-7", "change after the first snapshot: "+c.what, false, err.Error(), m.dir, "", m.writer.Name)
			return nil
		}
	}
	if !m.drained(ctx) {
		return nil
	}
	// the overwritten chunks leave the pool: the trash purged, GC after grace
	gone := m.collect(ctx, oldChunks)
	r.check("I-181 I-43", "GC collects the overwritten chunks from the pool once no sync holds them", gone == len(oldChunks) && len(oldChunks) > 0,
		fmt.Sprintf("%d of %d chunks of over.bin's first content gone from the pool", gone, len(oldChunks)))

	// the first snapshot, read through another gateway
	m.readSnapshot(snap1, before, join(m.dir, "new.bin"))
	m.refused(snap1)

	// the second snapshot adds what changed, not the corpus
	snap2 := fmt.Sprintf("soak-%d", time.Now().Unix()+1)
	second, ok := m.take(ctx, snap2)
	if !ok {
		return nil
	}
	rows, _ = m.catalogue(ctx)
	var added uint64
	var between []string
	for i := range rows { // every snapshot after the first manual one, up to the second
		if rows[i].ID > first.ID && rows[i].ID <= second.ID {
			added += rows[i].NewBytes
			between = append(between, rows[i].Name)
		}
	}
	changed := uint64(len(over) + cs/2 + len(newBin))
	r.check("I-180", "a later snapshot adds only the changed chunks and trees", added >= uint64(len(over)) && added <= changed+256<<10,
		fmt.Sprintf("%s added by %v; the changes are %s (the corpus %s)", humanBytes(float64(added)), between, humanBytes(float64(changed)), humanBytes(float64(second.Bytes))))
	m.labels[snap2] = "after 5 changes"

	// versions, revert and the tree restore
	m.versionsAndRestore(ctx, snap1, before)

	// new.bin is only in the second snapshot: deleted and collected, it then reads only
	// from the mirror, which the offline stretch takes away
	newChunks, _ := r.chunksOf(ctx, join(m.dir, "new.bin"))
	if err := wv.Remove(join(m.dir, "new.bin")); err != nil {
		r.checkAt("I-7", "new.bin removed", false, err.Error(), join(m.dir, "new.bin"), "", m.writer.Name)
		return nil
	}
	if n := m.collect(ctx, newChunks); n != len(newChunks) {
		r.event("note", fmt.Sprintf("%d of new.bin's %d chunks still in the pool: the offline read on %s may be served from it", len(newChunks)-n, len(newChunks), m.reader.Name))
	}

	// unplugged and back
	m.offlineAndBack(ctx, u, snap1, snap2, newBin)

	m.recordRoundTrip(ctx, gone, len(oldChunks))
	return nil
}

// online is 1 when the host reports the mirror online, 0 when not (a host lists a
// detached mirror by its path alone: the row is known only through the marker), -1 when
// the host does not answer.
func (m *mirrorRun) online() int {
	rep, err := m.r.Env.Status(context.Background(), m.host)
	if err != nil {
		return -1
	}
	for i := range rep.Mirrors {
		if mr := &rep.Mirrors[i]; mr.Name == m.name && mr.Online {
			return 1
		}
	}
	return 0
}

func (m *mirrorRun) mirrorReason() string {
	rep, err := m.r.Env.Status(context.Background(), m.host)
	if err != nil {
		return err.Error()
	}
	var out []string
	for i := range rep.Mirrors {
		mr := &rep.Mirrors[i]
		out = append(out, fmt.Sprintf("%s %s online=%v %s", mr.Name, mr.Path, mr.Online, mr.Reason))
	}
	return fmt.Sprintf("host reports %v", out)
}

// drained waits for the writing gateway's overlay to commit everything (recorded when it
// does not), so a snapshot begun now holds what was written.
func (m *mirrorRun) drained(ctx context.Context) bool {
	r := m.r
	var detail string
	ok := r.poll(ctx, 20*r.Profile.FlushIdle+30*time.Second, r.Profile.FlushIdle, func() bool {
		rep, err := r.Env.Status(ctx, m.writer)
		if err != nil || rep.Gateway == nil {
			detail = fmt.Sprintf("status: %v", err)
			return false
		}
		detail = fmt.Sprintf("%d dirty inodes", rep.Gateway.DirtyInodes)
		return rep.Gateway.DirtyInodes == 0
	})
	if !ok {
		r.checkAt("I-7", "snapshot files committed", false, detail, m.dir, "", m.writer.Name)
	}
	return ok
}

// catalogue is the mirror's rows, newest first; it remembers every row it returns.
func (m *mirrorRun) catalogue(ctx context.Context) ([]SnapshotInfo, error) {
	rows, err := m.r.snapshotList(ctx, m.writer, m.name)
	if m.seen == nil {
		m.seen = map[uint32]SnapshotInfo{}
	}
	for i := range rows {
		m.seen[rows[i].ID] = rows[i]
	}
	return rows, err
}

// snapshotList is the catalogue of one mirror (every mirror for ""), newest first.
func (r *Run) snapshotList(ctx context.Context, n NodeRef, mirror string) ([]SnapshotInfo, error) {
	raw, err := r.Env.SnapshotOp(ctx, n, rpc.AdminOp{Op: "snapshot_ls", Mirror: mirror})
	if err != nil {
		return nil, err
	}
	var rows []SnapshotInfo
	err = json.Unmarshal(raw, &rows)
	return rows, err
}

// take begins a manual snapshot on the mirror, retrying while another sync runs there,
// and waits for it to complete; it records the snapshot.
func (m *mirrorRun) take(ctx context.Context, name string) (SnapshotInfo, bool) {
	r := m.r
	var err error
	began := r.poll(ctx, 3*time.Minute, 2*time.Second, func() bool {
		_, err = r.Env.SnapshotOp(ctx, m.host, rpc.AdminOp{Op: "snapshot_create", Mirror: m.name, Name: name})
		return err == nil
	})
	if !began {
		r.check("I-179", "snapshot create "+name, false, fmt.Sprintf("%v", err))
		return SnapshotInfo{}, false
	}
	sn, ok := m.await(ctx, func(s *SnapshotInfo) bool { return s.Name == name })
	r.check("I-179 I-180", "snapshot "+name+" completes on "+m.name, ok && sn.State == "complete",
		fmt.Sprintf("%s: %d files, %s, added %s", sn.State, sn.Files, humanBytes(float64(sn.Bytes)), humanBytes(float64(sn.NewBytes))))
	return sn, ok && sn.State == "complete"
}

// await polls the catalogue until a row matching want is complete or incomplete.
func (m *mirrorRun) await(ctx context.Context, want func(*SnapshotInfo) bool) (SnapshotInfo, bool) {
	var got SnapshotInfo
	ok := m.r.poll(ctx, 5*time.Minute, 2*time.Second, func() bool {
		rows, err := m.catalogue(ctx)
		if err != nil {
			return false
		}
		for i := range rows {
			if want(&rows[i]) && (rows[i].State == "complete" || rows[i].State == "incomplete") {
				got = rows[i]
				return true
			}
		}
		return false
	})
	return got, ok
}

// collect purges the trash and runs GC until none of the chunks is left in the pool
// (three rounds of grace at most) and returns how many are gone.
func (m *mirrorRun) collect(ctx context.Context, chunks []admin.ChunkReport) int {
	r := m.r
	gone := 0
	for round := 0; round < 3 && gone < len(chunks); round++ {
		if _, err := r.Env.PurgeTrash(ctx); err != nil {
			r.event("note", "trash purge: "+err.Error())
		}
		if err := r.Env.Wait(ctx, r.Profile.Grace); err != nil {
			return gone
		}
		if _, err := r.gcNow(ctx); err != nil {
			r.event("note", "gc: "+err.Error())
		}
		gone = 0
		for i := range chunks {
			reps, err := r.Env.Chunks(ctx, r.leaderRef(ctx), chunks[i].Hash)
			if err == nil && (len(reps) == 0 || !reps[0].Found) {
				gone++
			}
		}
	}
	return gone
}

// readSnapshot reads every file of want under the snapshot through the reader and checks
// absent is not there.
func (m *mirrorRun) readSnapshot(snap string, want model, absent string) {
	r := m.r
	v := r.volume(m.reader)
	root := join("/.snapshots", snapshotDir(m.name), snap)
	bad, first := 0, ""
	for _, p := range sortedPaths(want) {
		got, err := v.ReadAll(join(root, p))
		switch {
		case err != nil:
			bad++
			if first == "" {
				first = fmt.Sprintf("%s: %v", p, err)
			}
		case !bytes.Equal(got, want[p]):
			bad++
			if first == "" {
				first = fmt.Sprintf("%s: differs at %d (len %d want %d)", p, firstDiff(got, want[p]), len(got), len(want[p]))
			}
		}
	}
	r.checkAt("I-185 I-179", "every file of "+snap+" reads exact under /.snapshots through "+m.reader.Name, bad == 0,
		fmt.Sprintf("%d files, %d differ %s", len(want), bad, first), "", "", m.reader.Name)
	_, err := v.Size(join(root, absent))
	r.check("I-179", "a file written after the snapshot is absent from it", err != nil, fmt.Sprintf("%s: %v", absent, err))
	if bad == 0 {
		m.reads[snap] = "exact · the overwritten file from the mirror, the rest from the pool"
	} else {
		m.reads[snap] = "FAIL · " + first
	}
}

// refused checks that a write, a mkdir and a remove under the snapshot fail.
func (m *mirrorRun) refused(snap string) {
	r := m.r
	v := r.volume(m.reader)
	root := join("/.snapshots", snapshotDir(m.name), snap)
	var landed []string
	if err := v.WriteAt(join(root, m.dir, "keep.bin"), 0, []byte("x")); err == nil {
		landed = append(landed, "write")
	}
	if err := v.Mkdir(join(root, m.dir, "new-dir")); err == nil {
		landed = append(landed, "mkdir")
	}
	if err := v.Remove(join(root, m.dir, "keep.bin")); err == nil {
		landed = append(landed, "remove")
	}
	r.check("I-185", "a write, a mkdir and a remove under /.snapshots are refused", len(landed) == 0, fmt.Sprintf("accepted: %v", landed))
}

// versionsAndRestore lists over.bin's versions, reverts it to the first snapshot, and
// restores the deleted folder under a new name.
func (m *mirrorRun) versionsAndRestore(ctx context.Context, snap1 string, before model) {
	r := m.r
	over := join(m.dir, "over.bin")
	raw, err := r.Env.SnapshotOp(ctx, m.reader, rpc.AdminOp{Op: "versions", Path: over})
	var vr versionsReply
	if err == nil {
		err = json.Unmarshal(raw, &vr)
	}
	held := false
	for _, v := range vr.Versions {
		for _, s := range v.Snapshots {
			held = held || (!v.Current && s.Mirror == snapshotDir(m.name) && s.Name == snap1)
		}
	}
	r.check("I-186", "versions of the overwritten file are two contents, the first held by "+snap1, err == nil && len(vr.Versions) >= 2 && held,
		fmt.Sprintf("%d versions, err %v", len(vr.Versions), err))

	_, err = r.Env.SnapshotOp(ctx, m.reader, rpc.AdminOp{Op: "versions_revert", Path: over, Mirror: m.name, Name: snap1})
	if err == nil {
		err = r.Env.Wait(ctx, r.Profile.FlushIdle)
	}
	var got []byte
	if err == nil {
		got, err = r.volume(m.writer).ReadAll(over)
	}
	r.checkAt("I-186", "a revert gives back the snapshot's bytes", err == nil && bytes.Equal(got, before[over]),
		fmt.Sprintf("%d bytes read, want %d, %v", len(got), len(before[over]), err), over, "", m.writer.Name)

	raw, err = r.Env.SnapshotOp(ctx, m.reader, rpc.AdminOp{Op: "snapshot_restore", Path: join(m.dir, "day2"), Mirror: m.name, Name: snap1, As: "restored-day2"})
	var rr restoreReply
	if err == nil {
		err = json.Unmarshal(raw, &rr)
	}
	restored := model{}
	for p, b := range before {
		if rest, ok := strings.CutPrefix(p, join(m.dir, "day2")+"/"); ok {
			restored[join(m.dir, "restored-day2", rest)] = b
		}
	}
	bad, first := -1, ""
	if err == nil {
		_ = r.Env.Wait(ctx, r.Profile.FlushIdle)
		bad, first = r.verifyModel(r.volume(m.writer), restored)
	}
	r.check("I-186", "the deleted folder restored from "+snap1+" under a new name reads exact", err == nil && bad == 0 && rr.Report.Files == len(restored),
		fmt.Sprintf("%d files restored (%d uploaded from the mirror, %d skipped), %d differ %s, %v", rr.Report.Files, rr.Report.Uploaded, len(rr.Report.Skipped), bad, first, err))
	m.reads["restore"] = fmt.Sprintf("restore-tree recreated the deleted folder as restored-day2 (%d created, %d skipped)", rr.Report.Files, len(rr.Report.Skipped))
}

// offlineAndBack unplugs the mirror: the catalogue keeps listing its snapshots, reads
// inside fail on its host and on another gateway, nothing is begun for it; plugged back,
// a catch-up snapshot begins within a tick and the second snapshot's new.bin, by then
// only on the mirror, reads exact again.
func (m *mirrorRun) offlineAndBack(ctx context.Context, u *undo, snap1, snap2 string, newBin []byte) {
	r := m.r
	if err := r.Env.SetMirrorOnline(ctx, m.name, false); err != nil {
		r.check("I-188", "the mirror unplugged", false, err.Error())
		return
	}
	u.add("plug the mirror back in", func(c context.Context) error { return r.Env.SetMirrorOnline(c, m.name, true) })
	if !r.poll(ctx, 3*time.Minute, 2*time.Second, func() bool { return m.online() == 0 }) {
		r.check("I-188", "the host sees the unplugged mirror offline within a tick", false, m.mirrorReason())
		return
	}
	r.check("I-188", "the host sees the unplugged mirror offline within a tick", true, m.name)
	r.event(mirrorOffline, m.name)
	rows, err := m.catalogue(ctx)
	listed, newest := 0, uint32(0)
	for i := range rows {
		if rows[i].Name == snap1 || rows[i].Name == snap2 {
			listed++
		}
		newest = max(newest, rows[i].ID)
	}
	r.check("I-185", "the catalogue still lists the offline mirror's snapshots", err == nil && listed == 2,
		fmt.Sprintf("%d rows, both manual ones listed: %v, %v", len(rows), listed == 2, err))
	// the host knows its disk is detached; another gateway asks the host for what the
	// pool no longer holds (new.bin's chunks)
	_, herr := r.volume(m.host).ReadAll(join("/.snapshots", snapshotDir(m.name), snap1, m.dir, "keep.bin"))
	remote := join("/.snapshots", snapshotDir(m.name), snap2, m.dir, "new.bin")
	_, rerr := r.volume(m.reader).ReadAll(remote)
	r.check("I-185", "reads inside an offline mirror's snapshot fail", herr != nil && rerr != nil,
		fmt.Sprintf("on %s: %v; on %s: %v", m.host.Name, herr, m.reader.Name, rerr))
	// longer than a schedule interval offline: nothing is begun for it
	if err := r.Env.Wait(ctx, 75*time.Second); err != nil {
		return
	}
	rows, _ = m.catalogue(ctx)
	begun := 0
	for i := range rows {
		if rows[i].ID > newest {
			begun++
		}
		newest = max(newest, rows[i].ID)
	}
	r.check("I-182", "nothing is begun for an offline mirror", begun == 0, fmt.Sprintf("%d rows begun while offline", begun))
	if err := r.Env.SetMirrorOnline(ctx, m.name, true); err != nil {
		r.check("I-188", "the mirror plugged back in", false, err.Error())
		return
	}
	u.done("plug the mirror back in")
	r.event(mirrorOnline, m.name)
	catchup, ok := m.await(ctx, func(s *SnapshotInfo) bool { return s.Auto && s.ID > newest })
	r.check("I-182", "a catch-up snapshot begins within a tick of the mirror's return", ok && catchup.State == "complete",
		fmt.Sprintf("%s %s, added %s", catchup.Name, catchup.State, humanBytes(float64(catchup.NewBytes))))
	if ok {
		m.labels[catchup.Name] = "catch-up on attach"
	}
	got, err := r.volume(m.reader).ReadAll(remote)
	r.checkAt("I-185", "a file only the mirror holds reads exact once it is back", err == nil && bytes.Equal(got, newBin),
		fmt.Sprintf("%d bytes, want %d, %v", len(got), len(newBin), err), remote, "", m.reader.Name)
	if err == nil && bytes.Equal(got, newBin) {
		m.reads[snap2] = "exact · new.bin from the mirror once it was back"
	}
}

// recordRoundTrip emits one snapshotEvent per snapshot of the mirror and the round trip
// summary (report.go's figures and table).
func (m *mirrorRun) recordRoundTrip(ctx context.Context, gone, overwritten int) {
	r := m.r
	now, err := m.catalogue(ctx)
	if err != nil {
		return
	}
	still := map[uint32]bool{}
	for i := range now {
		still[now[i].ID] = true
	}
	ids := slices.Sorted(maps.Keys(m.seen)) // oldest first
	for _, id := range ids {
		s := m.seen[id]
		state := s.State
		switch {
		case still[id]:
		case state == "syncing":
			state = "ended unfinished" // the sync failed and its row went (docs/16 §3)
		default:
			state += ", thinned since" // an automatic one the hourly bucket replaced (docs/16 §4)
		}
		label := m.labels[s.Name]
		switch {
		case s.Name == m.first:
			label = "first"
		case label == "" && s.Auto:
			label = "automatic"
		case label == "":
			label = "manual"
		}
		r.emit(Record{Kind: "event", Name: snapshotEvent, Text: s.Name, Attrs: map[string]string{
			"mirror": snapshotDir(s.Mirror), "label": label, "at": strconv.FormatInt(s.At, 10), "index": strconv.FormatUint(s.Index, 10),
			"files": strconv.FormatUint(s.Files, 10), "bytes": strconv.FormatUint(s.Bytes, 10), "new_bytes": strconv.FormatUint(s.NewBytes, 10),
			"state": state, "auto": strconv.FormatBool(s.Auto), "read": m.reads[s.Name],
		}})
	}
	r.event(roundTripEvent, fmt.Sprintf("GC collected %d of the %d overwritten chunks from the pool after the first sync ended; %s; offline: the catalogue kept listing the snapshots and reads inside failed; back: a catch-up began within one tick",
		gone, overwritten, m.reads["restore"]))
}

// snapshotsReadOnly is the phase on a cluster whose mirrors are its own: it takes no
// snapshot (one it took could never be removed, I-183) and reads what is there.
func (r *Run) snapshotsReadOnly(ctx context.Context, gws []NodeRef) error {
	rows, err := r.snapshotList(ctx, gws[0], "")
	if err != nil {
		if errors.Is(err, context.Canceled) {
			return err
		}
		r.skip("snapshot ls: " + err.Error())
		return nil
	}
	var sn *SnapshotInfo
	for i := range rows {
		if rows[i].State == "complete" {
			sn = &rows[i]
			break
		}
	}
	if sn == nil {
		r.skip("the cluster has no complete snapshot (no mirror, or none synced)")
		return nil
	}
	root := join("/.snapshots", snapshotDir(sn.Mirror), sn.Name)
	v := r.volume(gws[0])
	names, err := v.List(root)
	r.check("I-185", "the newest complete snapshot lists under /.snapshots", err == nil, fmt.Sprintf("%s/%s: %d entries, %v", sn.Mirror, sn.Name, len(names), err))
	if err != nil {
		return nil //nolint:nilerr // recorded as a failed check; the run goes on
	}
	// the first few files at its root read the same through every gateway
	read, differ := 0, 0
	for _, n := range names {
		if read >= 5 {
			break
		}
		p := join(root, n)
		want, _, err := v.Hash(p)
		if err != nil {
			continue // a folder, or a mirror offline
		}
		read++
		for _, g := range gws[1:] {
			if got, _, err := r.volume(g).Hash(p); err != nil || got != want {
				differ++
			}
		}
	}
	if read == 0 {
		r.checkSkipped("I-185", "a snapshot's files read the same through every gateway", "no file at the snapshot's root, or its mirror is offline")
	} else {
		r.check("I-185", "a snapshot's files read the same through every gateway", differ == 0, fmt.Sprintf("%d files through %d gateways, %d differ", read, len(gws), differ))
	}
	var landed []string
	if err := v.Mkdir(join(root, "_soak-refused")); err == nil {
		landed = append(landed, "mkdir")
	}
	r.check("I-185", "a mkdir under /.snapshots is refused", len(landed) == 0, fmt.Sprintf("accepted: %v", landed))
	return nil
}

// setMirrorHost makes the sampler record the host's mirrors from now on.
func (r *Run) setMirrorHost(n NodeRef) {
	r.mu.Lock()
	r.mirrorHost = &n
	r.mu.Unlock()
}

// poll calls cond every step (Env.Wait: real time, or the fake clock) until it holds or
// timeout of wall time has passed.
func (r *Run) poll(ctx context.Context, timeout, step time.Duration, cond func() bool) bool {
	deadline := time.Now().Add(timeout)
	for {
		if cond() {
			return true
		}
		if time.Now().After(deadline) || r.Env.Wait(ctx, step) != nil {
			return false
		}
	}
}
