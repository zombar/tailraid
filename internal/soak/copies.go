package soak

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"slices"
	"strings"
	"time"
)

// CopyPiece is the write size of a copy job, Explorer's.
const CopyPiece = 1 << 20

// CopySpec is one file a copy job writes.
type CopySpec struct {
	Path string `json:"path"`
	Size int    `json:"size"`
	Seed uint64 `json:"seed"`
	// Mode is how the copy opens the file: CopyExclusive (the default) creates it and
	// fails on an existing name, as Explorer's copy of a new file does; CopyReplace
	// opens it truncated, Explorer's "Replace the file in the destination".
	Mode string `json:"mode,omitempty"`
	// Conn is which of the job's connections copies the file: the job opens one per
	// distinct value, several clients of one gateway (phase 23).
	Conn int `json:"conn,omitempty"`
}

// The open modes of a copy (CopySpec.Mode).
const (
	CopyExclusive = ""
	CopyReplace   = "replace"
)

// CopyResult is how one file's copy ended: Err is what the client saw, "" for success;
// Exists marks a create refused because the name exists (an exclusive copy that lost).
type CopyResult struct {
	Path   string `json:"path"`
	Err    string `json:"err,omitempty"`
	Exists bool   `json:"exists,omitempty"`
}

// CopyPayload is the content of a copied file, a function of its size and seed alone, so
// the driver knows each file's hash without holding the bytes or moving them.
func CopyPayload(size int, seed uint64) []byte {
	b := make([]byte, size)
	x := seed | 1
	for i := 0; i < size; i += 8 {
		x ^= x << 13 // xorshift64
		x ^= x >> 7
		x ^= x << 17
		var w [8]byte
		binary.LittleEndian.PutUint64(w[:], x)
		copy(b[i:], w[:])
	}
	return b
}

// RaceResult is one handle race of phase 23 (round g) as the client saw it.
type RaceResult struct {
	Name   string `json:"name"`
	OK     bool   `json:"ok"`
	Detail string `json:"detail,omitempty"`
}

// RaceEnv is the optional part of an Env that phase 23's SMB handle races drive (round
// g, docs/09 §6): two sessions of node's SMB gateway, one marking a handle
// delete-on-close or holding it open while the other renames and replaces its file.
type RaceEnv interface {
	SMBRaces(ctx context.Context, node NodeRef, dir string) ([]RaceResult, error)
	// HoldDeleteOnClose creates path through node's SMB gateway, writes to it, marks it
	// delete-on-close and holds it open until release closes it (smbclient.Hold).
	HoldDeleteOnClose(ctx context.Context, node NodeRef, path string) (release func(context.Context) error, err error)
}

// CopyJob is copies under way; Poll reports whether all have ended and, then, how.
type CopyJob interface {
	Poll(ctx context.Context) (done bool, results []CopyResult, err error)
}

// CopyEnv is the optional part of an Env that phase 22 drives (docs/09 §6).
type CopyEnv interface {
	// StartCopies copies files through node's gateway the way Windows Explorer does, all
	// at once: over SMB ("smb") on one connection, over NFS ("nfs") with one handle held
	// open per file; each file is created, sized to its final length, written front to
	// back in CopyPiece writes and closed.
	StartCopies(ctx context.Context, node NodeRef, proto string, files []CopySpec) (CopyJob, error)
	// SetCodecView makes observer's prober take about as reading codec v whatever it
	// reports (0: its report again): set on the leader, the rolling upgrade of 2026-10-12
	// in which the leader saw an old member the gateways did not.
	SetCodecView(ctx context.Context, observer, about NodeRef, v uint8) error
	// DelayPuts holds every shard PUT node sends for d first (0: none).
	DelayPuts(ctx context.Context, node NodeRef, d time.Duration) error
	// RestartGraceful stops node letting its shutdown run (flush, leadership handover)
	// and starts it again, as an update does.
	RestartGraceful(ctx context.Context, node NodeRef) error
	// RestartIdle is `update --restart`: RestartGraceful once node's status has shown its
	// gateway idle (admin.WaitIdle, I-221).
	RestartIdle(ctx context.Context, node NodeRef) error
}

// copyFiles is how many files each job copies; their sizes straddle the chunk size.
const copyFiles = 4

// --- 22 copies under faults --------------------------------------------------------------

// phaseCopies copies files the way Explorer does through two gateways while one fault
// at a time hits the rest of the cluster, and checks every copy landed whole (docs/09 §6).
func phaseCopies(ctx context.Context, r *Run) error {
	ce, ok := r.Env.(CopyEnv)
	switch {
	case !ok:
		r.skip("environment cannot run copy jobs")
		return nil
	case !r.Env.Caps().Kill || !r.Env.Caps().Partition:
		r.skip("environment cannot restart or partition nodes")
		return nil
	case !r.Env.Caps().Mutate:
		r.skip("the faults change the cluster: needs --destructive")
		return nil
	case len(r.gateways()) < 3:
		r.skip("needs three gateways: two copying, one faulted")
		return nil
	}
	dir := join(r.Root, "copies")
	if v := r.volume(r.gateways()[0]); v == nil || v.Mkdir(dir) != nil {
		return fmt.Errorf("copies: mkdir %s", dir)
	}
	rounds := []struct {
		name string
		// fault starts while the copies run; undo, when not nil, ends it after the
		// round's checks, so a fault in force is what the checks see
		fault func(ctx context.Context, copiers []NodeRef) (what string, undo func(context.Context) error, err error)
	}{
		{"a rolling update", func(ctx context.Context, copiers []NodeRef) (string, func(context.Context) error, error) {
			var done []string
			for _, n := range r.nodes {
				if slices.Contains(copiers, n) {
					continue
				}
				if err := ce.RestartGraceful(ctx, n); err != nil {
					return "", nil, fmt.Errorf("restart %s: %w", n.Name, err)
				}
				if ok, detail := r.waitHealthy(ctx, r.Profile.RestartSettle+60*time.Second); !ok {
					return "", nil, fmt.Errorf("after %s: %s", n.Name, detail)
				}
				done = append(done, n.Name)
			}
			return "restarted " + strings.Join(done, ", "), nil, nil
		}},
		{"a leader kill", func(ctx context.Context, _ []NodeRef) (string, func(context.Context) error, error) {
			l := r.leaderRef(ctx)
			if err := r.Env.Kill(ctx, l); err != nil {
				return "", nil, err
			}
			if err := r.Env.Wait(ctx, 3*time.Second); err != nil {
				return "", nil, err
			}
			return "killed " + l.Name, nil, r.Env.Start(ctx, l)
		}},
		{"a minority partition", func(ctx context.Context, copiers []NodeRef) (string, func(context.Context) error, error) {
			l := r.leaderRef(ctx)
			var cut NodeRef
			for _, n := range r.nodes {
				if n != l && !slices.Contains(copiers, n) {
					cut = n
					break
				}
			}
			if cut.Name == "" {
				return "no node to cut off", nil, nil
			}
			var rest []NodeRef
			for _, n := range r.nodes {
				if n != cut {
					rest = append(rest, n)
				}
			}
			if err := r.Env.Partition(ctx, []NodeRef{cut}, rest); err != nil {
				return "", nil, err
			}
			if err := r.Env.Wait(ctx, 5*time.Second); err != nil {
				return "", nil, err
			}
			return "cut off " + cut.Name, nil, r.Env.Heal(ctx)
		}},
		{"the leader seeing a member on codec 4", func(ctx context.Context, copiers []NodeRef) (string, func(context.Context) error, error) {
			l := r.leaderRef(ctx)
			var old NodeRef
			for _, n := range r.nodes {
				if n != l && !slices.Contains(copiers, n) {
					old = n
					break
				}
			}
			if old.Name == "" {
				return "no member to age", nil, nil
			}
			if err := ce.SetCodecView(ctx, l, old, 4); err != nil {
				return "", nil, err
			}
			// held through the checks: the copies must commit while the leader and the
			// gateways disagree, as they did for the whole of the 2026-10-12 upgrade
			return l.Name + " takes " + old.Name + " for codec 4", func(ctx context.Context) error {
				return ce.SetCodecView(ctx, l, old, 0)
			}, nil
		}},
		{"slow shard PUTs", func(ctx context.Context, _ []NodeRef) (string, func(context.Context) error, error) {
			for _, n := range r.nodes {
				if err := ce.DelayPuts(ctx, n, 300*time.Millisecond); err != nil {
					return "", nil, err
				}
			}
			return "every shard PUT held 300 ms", func(ctx context.Context) error {
				for _, n := range r.nodes {
					if err := ce.DelayPuts(ctx, n, 0); err != nil {
						return err
					}
				}
				return nil
			}, nil
		}},
		{"an update of a copying gateway", func(ctx context.Context, copiers []NodeRef) (string, func(context.Context) error, error) {
			// the update runs beside the copies, as an administrator's would: it must wait
			// for them (I-221), and their results are read before it restarts the node
			done := make(chan error, 1)
			go func() { done <- ce.RestartIdle(ctx, copiers[0]) }()
			return copiers[0].Name + " updated once idle", func(context.Context) error { return <-done }, nil
		}},
	}
	var seed uint64 = 0x22
	for i, rd := range rounds {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		copiers := r.copiers(ctx)
		if len(copiers) < 2 {
			return fmt.Errorf("round %q: fewer than two gateways besides the leader", rd.name)
		}
		rdir := join(dir, fmt.Sprintf("r%d", i+1))
		for _, n := range copiers { // through each: a folder reaches another gateway only once it has replicated
			if err := r.volume(n).Mkdir(rdir); err != nil {
				return err
			}
		}
		want := map[string]string{} // path → hex sha256
		var jobs []CopyJob
		for j, n := range copiers {
			proto := []string{"smb", "nfs"}[j%2]
			var specs []CopySpec
			for f := range copyFiles {
				seed++
				size := (f+2)*r.Profile.ChunkSize + int(seed%4093)
				p := join(rdir, fmt.Sprintf("%s-%s-%d.bin", n.Name, proto, f))
				specs = append(specs, CopySpec{Path: p, Size: size, Seed: seed})
				sum := sha256.Sum256(CopyPayload(size, seed))
				want[p] = hex.EncodeToString(sum[:])
			}
			job, err := ce.StartCopies(ctx, n, proto, specs)
			if err != nil {
				return fmt.Errorf("round %q: start copies on %s: %w", rd.name, n.Name, err)
			}
			jobs = append(jobs, job)
		}
		what, undo, ferr := rd.fault(ctx, copiers)
		if ferr != nil {
			r.check("", fmt.Sprintf("22.%d %s: the fault ran", i+1, rd.name), false, ferr.Error())
		} else {
			r.event("fault", fmt.Sprintf("22.%d %s: %s", i+1, rd.name, what))
		}
		results, perr := r.pollCopies(ctx, jobs)
		var failed []string
		for _, res := range results {
			if res.Err != "" {
				failed = append(failed, res.Path+": "+res.Err)
			}
		}
		if perr != nil {
			failed = append(failed, perr.Error())
		}
		r.check("I-217 I-219 I-221", fmt.Sprintf("22.%d %s: every copy succeeded", i+1, rd.name), len(failed) == 0 && len(results) == len(want),
			fmt.Sprintf("%d of %d copies reported success; %s", len(results)-len(failed), len(want), strings.Join(failed, "; ")))
		healthy, hdetail := r.waitHealthy(ctx, r.Profile.RestartSettle+60*time.Second)
		r.check("L-120", fmt.Sprintf("22.%d %s: cluster healthy after the fault", i+1, rd.name), healthy, hdetail)
		if err := r.Env.Wait(ctx, r.Profile.FlushIdle); err != nil {
			return err
		}
		r.checkCopies(ctx, fmt.Sprintf("22.%d %s", i+1, rd.name), copiers, want)
		if undo != nil {
			if err := undo(ctx); err != nil {
				return fmt.Errorf("round %q: end the fault: %w", rd.name, err)
			}
		}
	}
	r.checkFSMAgreement(ctx, "I-4 I-5", "FSM hashes agree after the copies under faults")
	return nil
}

// copiers picks two gateways that are not the leader, so that a round faulting the
// leader or the other nodes leaves the copying gateways up.
func (r *Run) copiers(ctx context.Context) []NodeRef {
	l := r.leaderRef(ctx)
	var out []NodeRef
	for _, n := range r.gateways() {
		if n != l {
			out = append(out, n)
		}
		if len(out) == 2 {
			break
		}
	}
	return out
}

// pollCopies waits for every job to end, letting time pass between polls (the harness's
// clock and flushes move only then), for at most the phase's share of its timeout.
func (r *Run) pollCopies(ctx context.Context, jobs []CopyJob) ([]CopyResult, error) {
	limit := 10 * time.Minute
	if r.Profile.PhaseTimeout > 0 {
		limit = r.Profile.PhaseTimeout / 8
	}
	deadline := time.Now().Add(limit)
	results := make([][]CopyResult, len(jobs))
	ended := make([]bool, len(jobs))
	for {
		all := true
		for i, j := range jobs {
			if ended[i] {
				continue
			}
			done, res, err := j.Poll(ctx)
			if err != nil {
				return flatten(results), err
			}
			if done {
				ended[i], results[i] = true, res
			} else {
				all = false
			}
		}
		if all {
			return flatten(results), nil
		}
		if time.Now().After(deadline) {
			return flatten(results), fmt.Errorf("copies still running after %s", limit)
		}
		if err := r.Env.Wait(ctx, 500*time.Millisecond); err != nil {
			return flatten(results), err
		}
	}
}

func flatten(rs [][]CopyResult) []CopyResult {
	var out []CopyResult
	for _, r := range rs {
		out = append(out, r...)
	}
	return out
}

// checkCopies reads every copy back through a gateway that did not write it and asks the
// leader for its chunks: the bytes are the payload's, and no file is all holes.
func (r *Run) checkCopies(ctx context.Context, label string, copiers []NodeRef, want map[string]string) {
	var reader NodeRef
	for _, n := range r.gateways() {
		if !slices.Contains(copiers, n) {
			reader = n
			break
		}
	}
	if reader.Name == "" {
		reader = copiers[len(copiers)-1]
	}
	v := r.volume(reader)
	var bad, holes []string
	paths := make([]string, 0, len(want))
	for p := range want {
		paths = append(paths, p)
	}
	slices.Sort(paths)
	for _, p := range paths {
		// a file committed through one gateway is visible on another once the commit has
		// replicated to it; a reader just restarted may still be catching up
		var (
			sum [32]byte
			err error
		)
		for try := 0; ; try++ {
			if sum, _, err = v.Hash(p); err == nil && hex.EncodeToString(sum[:]) == want[p] || try == 30 {
				break
			}
			if werr := r.Env.Wait(ctx, time.Second); werr != nil {
				break
			}
		}
		if got := hex.EncodeToString(sum[:]); err != nil || got != want[p] {
			bad = append(bad, fmt.Sprintf("%s (%v)", p, err))
		}
		chunks, err := r.chunksOf(ctx, p)
		if err != nil || len(chunks) == 0 {
			continue // a file that did not read back is already in bad
		}
		zero := 0
		for i := range chunks {
			if chunks[i].State == "zero" {
				zero++
			}
		}
		if zero == len(chunks) {
			holes = append(holes, p)
		}
	}
	r.check("I-7", fmt.Sprintf("%s: every copy reads back whole via %s", label, reader.Name), len(bad) == 0,
		fmt.Sprintf("%d of %d differ: %s", len(bad), len(want), strings.Join(bad, "; ")))
	r.check("I-7", label+": no copy is a file of holes", len(holes) == 0,
		fmt.Sprintf("%d of %d all holes: %s", len(holes), len(want), strings.Join(holes, "; ")))
}
