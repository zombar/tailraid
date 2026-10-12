package soak

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/zombar/tailraid/internal/webui"
)

// --- 23 many users at once ----------------------------------------------------------------

// phaseMultiUser is contention between clients rather than a fault (docs/09 §6): many
// streams into one gateway, two gateways meeting in one folder and in one name, a
// reader racing a writer, a rename under open copies, SMB handle races, browser uploads
// of one name, and the first round again under a leader kill. Each round has a folder
// of its own and every file is read back through a gateway that wrote none of it.
func phaseMultiUser(ctx context.Context, r *Run) error {
	ce, ok := r.Env.(CopyEnv)
	switch {
	case !ok:
		r.skip("environment cannot run copy jobs")
		return nil
	case len(r.gateways()) < 3:
		r.skip("needs three gateways: two clients' and a reader")
		return nil
	}
	dir := join(r.Root, "multiuser")
	if v := r.volume(r.gateways()[0]); v == nil || v.Mkdir(dir) != nil {
		return fmt.Errorf("multiuser: mkdir %s", dir)
	}
	m := &multi{r: r, ce: ce, dir: dir, seed: 0x23 << 32}
	rounds := []struct {
		name string
		run  func(ctx context.Context, label, rdir string) error
	}{
		{"many streams into one gateway", m.manyStreams},
		{"two gateways into one folder", m.oneFolder},
		{"one name created exclusively by three clients", m.exclusiveName},
		{"one name replaced by two clients", m.replacedName},
		{"a gateway reading what another is copying", m.readWhileWritten},
		{"a folder renamed under open copies", m.renameUnderCopies},
		{"SMB handle races", m.handleRaces},
		{"browser uploads of one name through two gateways", m.webUploads},
		{"many streams while the leader is killed", m.streamsUnderLeaderKill},
	}
	for i, rd := range rounds {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		label := fmt.Sprintf("23.%d %s", i+1, rd.name)
		rdir := join(dir, fmt.Sprintf("r%d", i+1))
		if err := rd.run(ctx, label, rdir); err != nil {
			return fmt.Errorf("round %q: %w", rd.name, err)
		}
	}
	r.checkFSMAgreement(ctx, "I-4 I-5", "FSM hashes agree after the many-user rounds")
	return nil
}

// multi carries phase 23's state between its rounds.
type multi struct {
	r    *Run
	ce   CopyEnv
	dir  string
	seed uint64
}

// spec is the next file of a round: its size straddles the chunk size by f.
func (m *multi) spec(path string, f int, mode string, conn int) CopySpec {
	m.seed++
	return CopySpec{Path: path, Size: (f%3+1)*m.r.Profile.ChunkSize + int(m.seed%4093), Seed: m.seed, Mode: mode, Conn: conn}
}

func sumOf(s CopySpec) string {
	h := sha256.Sum256(CopyPayload(s.Size, s.Seed))
	return hex.EncodeToString(h[:])
}

// mkdirOn makes dir through each node, as each client's copy makes its folder.
func (m *multi) mkdirOn(dir string, nodes ...NodeRef) error {
	for _, n := range nodes {
		if err := m.r.volume(n).Mkdir(dir); err != nil {
			return fmt.Errorf("mkdir %s via %s: %w", dir, n.Name, err)
		}
	}
	return nil
}

// start starts one copy job and fails the round's check on an error.
func (m *multi) start(ctx context.Context, n NodeRef, proto string, files []CopySpec) (CopyJob, error) {
	job, err := m.ce.StartCopies(ctx, n, proto, files)
	if err != nil {
		return nil, fmt.Errorf("start %s copies on %s: %w", proto, n.Name, err)
	}
	return job, nil
}

// allSucceeded checks every copy reported success.
func (m *multi) allSucceeded(label, cites string, results []CopyResult, want int, perr error) {
	var failed []string
	for _, res := range results {
		if res.Err != "" {
			failed = append(failed, res.Path+": "+res.Err)
		}
	}
	if perr != nil {
		failed = append(failed, perr.Error())
	}
	m.r.check(cites, label+": every copy succeeded", len(failed) == 0 && len(results) == want,
		fmt.Sprintf("%d of %d copies reported success; %s", len(results)-len(failed), want, strings.Join(failed, "; ")))
}

// streams is round (a)'s load: three SMB sessions of four files each and an NFS job of
// four files on two connections, all at once on gateway g.
func (m *multi) streams(ctx context.Context, g NodeRef, rdir string) ([]CopyJob, map[string]string, error) {
	want := map[string]string{}
	smbFiles, nfsFiles := make([]CopySpec, 0, 12), make([]CopySpec, 0, 4)
	for conn := range 3 {
		for f := range 4 {
			s := m.spec(join(rdir, fmt.Sprintf("smb-c%d-%d.bin", conn, f)), f, CopyExclusive, conn)
			smbFiles = append(smbFiles, s)
			want[s.Path] = sumOf(s)
		}
	}
	for f := range 4 {
		s := m.spec(join(rdir, fmt.Sprintf("nfs-c%d-%d.bin", f%2, f)), f, CopyExclusive, f%2)
		nfsFiles = append(nfsFiles, s)
		want[s.Path] = sumOf(s)
	}
	j1, err := m.start(ctx, g, "smb", smbFiles)
	if err != nil {
		return nil, nil, err
	}
	j2, err := m.start(ctx, g, "nfs", nfsFiles)
	if err != nil {
		return nil, nil, err
	}
	return []CopyJob{j1, j2}, want, nil
}

// (a) many streams into one gateway (I-217, I-167).
func (m *multi) manyStreams(ctx context.Context, label, rdir string) error {
	g := m.r.gateways()[0]
	if err := m.mkdirOn(rdir, g); err != nil {
		return err
	}
	jobs, want, err := m.streams(ctx, g, rdir)
	if err != nil {
		return err
	}
	results, perr := m.r.pollCopies(ctx, jobs)
	m.allSucceeded(label, "I-217 I-167", results, len(want), perr)
	if err := m.r.Env.Wait(ctx, m.r.Profile.FlushIdle); err != nil {
		return err
	}
	m.r.checkCopies(ctx, label, []NodeRef{g}, want)
	return nil
}

// (b) two gateways copying one folder tree into the same new folder: both make the same
// subfolders, and a third gateway's listing reaches the union (I-35).
func (m *multi) oneFolder(ctx context.Context, label, rdir string) error {
	gws := m.r.gateways()
	a, b, c := gws[0], gws[1], gws[2]
	want := map[string]string{}
	var jobs []CopyJob
	for i, n := range []NodeRef{a, b} {
		var files []CopySpec
		for sub := range 2 {
			sd := join(rdir, "tree", fmt.Sprintf("sub%d", sub))
			if err := m.mkdirOn(sd, n); err != nil { // the same folders, made through both
				return err
			}
			for f := range 2 {
				s := m.spec(join(sd, fmt.Sprintf("%s-%d.bin", n.Name, f)), f+i, CopyExclusive, 0)
				files = append(files, s)
				want[s.Path] = sumOf(s)
			}
		}
		job, err := m.start(ctx, n, []string{"smb", "nfs"}[i], files)
		if err != nil {
			return err
		}
		jobs = append(jobs, job)
	}
	results, perr := m.r.pollCopies(ctx, jobs)
	m.allSucceeded(label, "I-217", results, len(want), perr)
	if err := m.r.Env.Wait(ctx, m.r.Profile.FlushIdle); err != nil {
		return err
	}
	// the union, listed through the third gateway
	var missing []string
	v := m.r.volume(c)
	for sub := range 2 {
		sd := join(rdir, "tree", fmt.Sprintf("sub%d", sub))
		names, err := v.List(sd)
		if err != nil {
			missing = append(missing, sd+": "+err.Error())
			continue
		}
		for p := range want {
			if join(p, "..") == sd && !slices.Contains(names, strings.TrimPrefix(p, sd+"/")) {
				missing = append(missing, p)
			}
		}
	}
	m.r.check("I-35", fmt.Sprintf("%s: %s lists every file of both copies", label, c.Name), len(missing) == 0, strings.Join(missing, "; "))
	m.r.checkCopies(ctx, label, []NodeRef{a, b}, want)
	return nil
}

// (c) one name created exclusively by three clients: two SMB sessions of one gateway and
// an NFS client of another. Each name has exactly one winner, the rest are told it
// exists, and the file is the winner's (I-222).
func (m *multi) exclusiveName(ctx context.Context, label, rdir string) error {
	gws := m.r.gateways()
	a, b := gws[0], gws[1]
	if err := m.mkdirOn(rdir, a, b); err != nil {
		return err
	}
	// many names, each contended by two SMB sessions of one gateway and an SMB and an NFS
	// client of another: the lost race needs a gateway whose FSM has not yet applied the
	// other's create, which only some of the names meet
	const names = 24
	contenders := map[string][]CopySpec{} // path → each client's spec
	type job struct {
		n     NodeRef
		proto string
		files []CopySpec
	}
	jobs := []*job{{n: b, proto: "nfs"}, {n: a, proto: "smb"}, {n: b, proto: "smb"}}
	for f := range names {
		p := join(rdir, fmt.Sprintf("same-%02d.bin", f))
		for i, j := range jobs {
			for conn := range 1 + i%2 { // a's SMB job contends with itself on two sessions
				s := m.spec(p, f+i, CopyExclusive, conn)
				j.files = append(j.files, s)
				contenders[p] = append(contenders[p], s)
			}
		}
	}
	var started []CopyJob
	for _, j := range jobs {
		cj, err := m.start(ctx, j.n, j.proto, j.files)
		if err != nil {
			return err
		}
		started = append(started, cj)
	}
	results, perr := m.r.pollCopies(ctx, started)
	if perr != nil {
		m.r.check("I-222", label+": every copy ended", false, perr.Error())
		return nil
	}
	// results come back in job order, each job's in its files' order
	bySpec := map[CopySpec]CopyResult{}
	var all []CopySpec
	for _, j := range jobs {
		all = append(all, j.files...)
	}
	for i, s := range all {
		if i < len(results) {
			bySpec[s] = results[i]
		}
	}
	want := map[string]string{}
	var bad []string
	for p, specs := range contenders {
		var winners []CopySpec
		for _, s := range specs {
			res := bySpec[s]
			switch {
			case res.Err == "":
				winners = append(winners, s)
			case !res.Exists:
				bad = append(bad, fmt.Sprintf("%s: a loser saw %q, not that the name exists", p, res.Err))
			}
		}
		if len(winners) != 1 {
			bad = append(bad, fmt.Sprintf("%s: %d clients were told they created it", p, len(winners)))
			continue
		}
		want[p] = sumOf(winners[0])
	}
	m.r.check("I-222", label+": exactly one client creates each name, the others are told it exists", len(bad) == 0, strings.Join(bad, "; "))
	if err := m.r.Env.Wait(ctx, m.r.Profile.FlushIdle); err != nil {
		return err
	}
	m.r.checkCopies(ctx, label+" (the winners)", []NodeRef{a, b}, want)
	return nil
}

// (d) one name replaced by two clients through two gateways with different payloads,
// starts staggered: every gateway reads one payload whole, the same one (I-223).
func (m *multi) replacedName(ctx context.Context, label, rdir string) error {
	gws := m.r.gateways()
	a, b, c := gws[0], gws[1], gws[2]
	if err := m.mkdirOn(rdir, a); err != nil {
		return err
	}
	// the copies are larger than a flush (FileDirtyHi, the WAL cap), so each commits in
	// several flushes and the two interleave: a copy that fits one flush is whole anyway
	size := min(max(16*m.r.Profile.ChunkSize, 40<<20), max(m.r.Profile.LargeFile/2, 16*m.r.Profile.ChunkSize))
	const names = 2
	var fa, fb []CopySpec
	for f := range names {
		p := join(rdir, fmt.Sprintf("replaced-%d.bin", f))
		if err := m.r.volume(a).WriteAt(p, 0, CopyPayload(m.r.Profile.ChunkSize+7, 99)); err != nil { // the file both replace
			return err
		}
		sa, sb := m.spec(p, f, CopyReplace, f), m.spec(p, f+1, CopyReplace, f)
		sa.Size, sb.Size = size+f*4093, size-m.r.Profile.ChunkSize/2+f
		fa, fb = append(fa, sa), append(fb, sb)
	}
	if err := m.r.Env.Wait(ctx, m.r.Profile.FlushIdle); err != nil {
		return err
	}
	j1, err := m.start(ctx, a, "smb", fa)
	if err != nil {
		return err
	}
	j2, err := m.start(ctx, b, "smb", fb)
	if err != nil {
		return err
	}
	results, perr := m.r.pollCopies(ctx, []CopyJob{j1, j2})
	m.allSucceeded(label, "I-223", results, 2*names, perr)
	if err := m.r.Env.Wait(ctx, m.r.Profile.FlushIdle); err != nil {
		return err
	}
	var bad []string
	for f := range names {
		p := fa[f].Path
		whole := map[string]string{sumOf(fa[f]): a.Name, sumOf(fb[f]): b.Name}
		var seen []string
		for _, n := range []NodeRef{a, b, c} {
			sum, _, err := m.r.volume(n).Hash(p)
			got := hex.EncodeToString(sum[:])
			if err != nil || whole[got] == "" {
				bad = append(bad, fmt.Sprintf("%s via %s is neither copy whole (%v)", p, n.Name, err))
				continue
			}
			seen = append(seen, got)
		}
		if len(slices.Compact(seen)) > 1 {
			bad = append(bad, p+": the gateways read different copies")
		}
	}
	m.r.check("I-223", label+": every gateway reads one copy whole, the same one", len(bad) == 0, strings.Join(bad, "; "))
	return nil
}

// (e) a gateway reading a file another gateway is copying over: every read is, chunk by
// chunk, the old bytes, the new ones or a hole of the sized file, never other bytes
// (I-36); once the copy has ended and FlushIdle passed, the reader sees it whole.
func (m *multi) readWhileWritten(ctx context.Context, label, rdir string) error {
	gws := m.r.gateways()
	a, b := gws[0], gws[1]
	if err := m.mkdirOn(rdir, a); err != nil {
		return err
	}
	cs := m.r.Profile.ChunkSize
	p := join(rdir, "read-while-written.bin")
	s := m.spec(p, 2, CopyReplace, 0)
	s.Size = 3*cs + 11
	old := CopyPayload(s.Size, 7)
	if err := m.r.volume(a).WriteAt(p, 0, old); err != nil {
		return err
	}
	if err := m.r.Env.Wait(ctx, m.r.Profile.FlushIdle); err != nil {
		return err
	}
	job, err := m.start(ctx, a, "smb", []CopySpec{s})
	if err != nil {
		return err
	}
	fresh := CopyPayload(s.Size, s.Seed)
	zero := make([]byte, cs)
	var torn []string
	reads := 0
	for range 200 {
		if done, _, _ := job.Poll(ctx); done {
			break
		}
		got, err := m.r.volume(b).ReadAll(p)
		reads++
		if err == nil {
			for i := 0; i < len(got); i += cs {
				end := min(i+cs, len(got))
				chunk := got[i:end]
				okOld := end <= len(old) && bytes.Equal(chunk, old[i:end])
				okNew := end <= len(fresh) && bytes.Equal(chunk, fresh[i:end])
				if !okOld && !okNew && !bytes.Equal(chunk, zero[:len(chunk)]) {
					torn = append(torn, fmt.Sprintf("read %d: chunk at %d is neither old, new nor a hole", reads, i))
					break
				}
			}
		}
		if err := m.r.Env.Wait(ctx, 100*time.Millisecond); err != nil {
			return err
		}
	}
	results, perr := m.r.pollCopies(ctx, []CopyJob{job})
	m.allSucceeded(label, "I-36", results, 1, perr)
	m.r.check("I-36", fmt.Sprintf("%s: %d reads through %s saw no torn chunk", label, reads, b.Name), len(torn) == 0, strings.Join(torn, "; "))
	if err := m.r.Env.Wait(ctx, m.r.Profile.FlushIdle); err != nil {
		return err
	}
	m.r.checkCopies(ctx, label, []NodeRef{a}, map[string]string{p: sumOf(s)})
	return nil
}

// (f) the folder of copies under way renamed through another gateway: the copies, SMB
// and NFS, end well and every file is whole under the new name (I-16, I-224).
func (m *multi) renameUnderCopies(ctx context.Context, label, rdir string) error {
	gws := m.r.gateways()
	a, b := gws[0], gws[1]
	src, dst := join(rdir, "src"), join(rdir, "dst")
	if err := m.mkdirOn(src, a); err != nil {
		return err
	}
	var smb, nfs []CopySpec
	for f := range 3 {
		smb = append(smb, m.spec(join(src, fmt.Sprintf("smb-%d.bin", f)), f+1, CopyExclusive, 0))
		nfs = append(nfs, m.spec(join(src, fmt.Sprintf("nfs-%d.bin", f)), f+1, CopyExclusive, 0))
	}
	j1, err := m.start(ctx, a, "smb", smb)
	if err != nil {
		return err
	}
	j2, err := m.start(ctx, a, "nfs", nfs)
	if err != nil {
		return err
	}
	// the rename lands once every copy has made its file (a copy that creates its file in
	// a folder already renamed away makes the folder again: the vfs makes a missing
	// parent on create, which is another round's question), while they still write
	names := map[string]bool{}
	for _, s := range append(append([]CopySpec(nil), smb...), nfs...) {
		names[strings.TrimPrefix(s.Path, src+"/")] = true
	}
	for range 100 {
		listed, _ := m.r.volume(b).List(src)
		n := 0
		for _, l := range listed {
			if names[l] {
				n++
			}
		}
		if n == len(names) {
			break
		}
		if err := m.r.Env.Wait(ctx, 50*time.Millisecond); err != nil {
			return err
		}
	}
	rerr := m.r.volume(b).Rename(src, dst)
	m.r.check("I-16", fmt.Sprintf("%s: %s renames the folder mid-copy", label, b.Name), rerr == nil, fmt.Sprint(rerr))
	results, perr := m.r.pollCopies(ctx, []CopyJob{j1, j2})
	m.allSucceeded(label, "I-16 I-224", results, len(smb)+len(nfs), perr)
	if err := m.r.Env.Wait(ctx, m.r.Profile.FlushIdle); err != nil {
		return err
	}
	want := map[string]string{}
	for _, s := range append(smb, nfs...) {
		want[strings.Replace(s.Path, src, dst, 1)] = sumOf(s)
	}
	m.r.checkCopies(ctx, label+" (under the new name)", []NodeRef{a}, want)
	return nil
}

// (g) SMB handle races on one gateway (I-224, I-225).
func (m *multi) handleRaces(ctx context.Context, label, rdir string) error {
	re, ok := m.r.Env.(RaceEnv)
	if !ok {
		m.r.checkSkipped("I-224 I-225", label, "environment cannot run SMB handle races")
		return nil
	}
	g := m.r.gateways()[0]
	if err := m.mkdirOn(rdir, g); err != nil {
		return err
	}
	results, err := re.SMBRaces(ctx, g, rdir)
	if err != nil {
		m.r.check("I-224 I-225", label+": the races ran", false, err.Error())
		return nil
	}
	for _, res := range results {
		m.r.check("I-224 I-225", label+": "+res.Name, res.OK, res.Detail)
	}
	// the case I-225 is for: the rename and the new file come through another gateway,
	// whose rename this gateway's open handle cannot follow, so only the inode tells the
	// handle's file from the one now under its name
	gws := m.r.gateways()
	b, c := gws[1], gws[2]
	xdir := join(rdir, "across")
	if err := m.mkdirOn(xdir, g); err != nil {
		return err
	}
	p, kept := join(xdir, "copy.bin"), join(xdir, "kept.bin")
	theirs := []byte("someone else's file")
	release, err := re.HoldDeleteOnClose(ctx, g, p)
	if err != nil {
		m.r.check("I-225", label+": a delete-on-close handle held open", false, err.Error())
		return nil
	}
	if err := m.r.Env.Wait(ctx, m.r.Profile.FlushIdle); err != nil { // the create reaches b
		return err
	}
	if err := m.r.volume(b).Rename(p, kept); err != nil {
		return fmt.Errorf("rename via %s: %w", b.Name, err)
	}
	if err := m.r.volume(b).WriteAt(p, 0, theirs); err != nil {
		return fmt.Errorf("new file via %s: %w", b.Name, err)
	}
	if err := m.r.Env.Wait(ctx, m.r.Profile.FlushIdle); err != nil { // b's file committed and applied on g
		return err
	}
	rerr := release(ctx)
	if err := m.r.Env.Wait(ctx, m.r.Profile.FlushIdle); err != nil {
		return err
	}
	got, err := m.r.volume(c).ReadAll(p)
	m.r.check("I-225", fmt.Sprintf("%s: a delete-on-close through %s leaves the file %s put under its name", label, g.Name, b.Name),
		rerr == nil && err == nil && bytes.Equal(got, theirs), fmt.Sprintf("close: %v; read via %s: %q, %v", rerr, c.Name, got, err))
	return nil
}

// (h) browser uploads of one name through two gateways at once: plain ones both succeed
// and the name holds one of them whole; exclusive ones (If-None-Match: *) succeed once
// and the other is told 412 (I-226).
func (m *multi) webUploads(ctx context.Context, label, rdir string) error {
	gws := m.r.gateways()
	a, b := gws[0], gws[1]
	var bases []string
	for _, n := range []NodeRef{a, b} {
		base, err := m.r.Env.WebURL(ctx, n)
		if err != nil {
			return err
		}
		if base == "" {
			m.r.checkSkipped("I-226", label, "environment has no browser UI")
			return nil
		}
		bases = append(bases, strings.TrimSuffix(base, "/"))
	}
	if err := m.mkdirOn(rdir, a); err != nil {
		return err
	}
	hc := m.r.Env.WebClient()
	if hc == nil {
		hc = webHTTP
	}
	put := func(base, p string, body []byte, exclusive bool) (int, error) {
		req, err := http.NewRequestWithContext(ctx, http.MethodPut, base+"/files"+encodeWebPath(p), bytes.NewReader(body))
		if err != nil {
			return 0, err
		}
		req.Header.Set(webui.HeaderUI, "1")
		if exclusive {
			req.Header.Set("If-None-Match", "*")
		}
		resp, err := hc.Do(req)
		if err != nil {
			return 0, err
		}
		_ = resp.Body.Close()
		return resp.StatusCode, nil
	}
	both := func(p string, exclusive bool) ([2]int, [2][]byte) {
		var codes [2]int
		var bodies [2][]byte
		var wg sync.WaitGroup
		for i := range 2 {
			bodies[i] = CopyPayload(m.r.Profile.ChunkSize+100*(i+1), m.seed+uint64(i))
			wg.Go(func() {
				code, err := put(bases[i], p, bodies[i], exclusive)
				if err != nil {
					code = -1
				}
				codes[i] = code
			})
		}
		wg.Wait()
		m.seed += 2
		return codes, bodies
	}
	plain := join(rdir, "uploaded.bin")
	codes, bodies := both(plain, false)
	m.r.check("I-226", fmt.Sprintf("%s: both plain uploads succeed (%d, %d)", label, codes[0], codes[1]), codes[0]/100 == 2 && codes[1]/100 == 2, "")
	if err := m.r.Env.Wait(ctx, m.r.Profile.FlushIdle); err != nil {
		return err
	}
	got, err := m.r.volume(gws[2]).ReadAll(plain)
	m.r.check("I-226", label+": the name holds one upload whole", err == nil && (bytes.Equal(got, bodies[0]) || bytes.Equal(got, bodies[1])),
		fmt.Sprintf("%d bytes, err %v", len(got), err))
	excl := join(rdir, "exclusive.bin")
	codes, _ = both(excl, true)
	wins := 0
	for _, c := range codes {
		if c/100 == 2 {
			wins++
		} else if c != http.StatusPreconditionFailed {
			wins = -9 // any other answer fails the check
		}
	}
	m.r.check("I-226", fmt.Sprintf("%s: of two exclusive uploads one succeeds and one is told 412 (%d, %d)", label, codes[0], codes[1]), wins == 1, "")
	return nil
}

// (i) round (a) again on a gateway that is not the leader, while the leader is killed
// and brought back (I-95, I-217).
func (m *multi) streamsUnderLeaderKill(ctx context.Context, label, rdir string) error {
	if c := m.r.Env.Caps(); !c.Kill || !c.Mutate {
		m.r.checkSkipped("I-95 I-217", label, "needs kills and --destructive")
		return nil
	}
	copiers := m.r.copiers(ctx)
	if len(copiers) == 0 {
		return fmt.Errorf("no gateway besides the leader")
	}
	g := copiers[0]
	if err := m.mkdirOn(rdir, g); err != nil {
		return err
	}
	jobs, want, err := m.streams(ctx, g, rdir)
	if err != nil {
		return err
	}
	l := m.r.leaderRef(ctx)
	if err := m.r.Env.Kill(ctx, l); err != nil {
		return err
	}
	m.r.event("fault", fmt.Sprintf("%s: killed %s", label, l.Name))
	if err := m.r.Env.Wait(ctx, 3*time.Second); err != nil {
		return err
	}
	if err := m.r.Env.Start(ctx, l); err != nil {
		return err
	}
	results, perr := m.r.pollCopies(ctx, jobs)
	m.allSucceeded(label, "I-95 I-217", results, len(want), perr)
	healthy, detail := m.r.waitHealthy(ctx, m.r.Profile.RestartSettle+60*time.Second)
	m.r.check("L-120", label+": cluster healthy after the kill", healthy, detail)
	if err := m.r.Env.Wait(ctx, m.r.Profile.FlushIdle); err != nil {
		return err
	}
	m.r.checkCopies(ctx, label, []NodeRef{g}, want)
	return nil
}
