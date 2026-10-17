package soak

import (
	"bytes"
	"context"
	"fmt"
	"path"
	"time"
)

// --- 34 small files -----------------------------------------------------------------------
//
// A copy of small files lands whole (docs/04 §6, docs/09 §6): each file made the way
// `cp -p` makes one (create, write at an offset, mode and the source's times) and its
// AppleDouble `._` companion the same way, through one gateway. The gateway proposes a
// create per file and no SetAttr, holds the SETATTRs, shows the mode and times at once,
// and after the idle flush every file reads back exact on another gateway with them; a
// SETATTR that changes nothing on a clean file proposes nothing.

const (
	smallFiles = 40
	// smallSlack is what the gateway's own writers under /.tailraid (stats, audit) may
	// propose meanwhile: a SetAttr or two is theirs, not the copy's
	smallSlack = 2
)

func phaseSmallFiles(ctx context.Context, r *Run) error {
	gws := r.gateways()
	gw := gws[0]
	av, ok := r.attrVolume(gw)
	if !ok {
		r.skip("the environment's volume sets no attributes")
		return nil
	}
	v := r.volume(gw)
	before, err := r.Env.Status(ctx, gw)
	if err != nil {
		return err
	}
	if before.Gateway == nil || before.Gateway.Proposals == nil {
		r.skip("the gateway reports no proposals (status without the counters)")
		return nil
	}
	dir := join(r.Root, "small")
	if err := v.Mkdir(dir); err != nil {
		return err
	}
	src := time.Date(2019, 3, 14, 15, 9, 26, 0, time.UTC)
	type file struct {
		p    string
		data []byte
	}
	var files []file
	t := time.Now()
	for i := range smallFiles {
		for _, name := range []string{fmt.Sprintf("f%02d.txt", i), fmt.Sprintf("._f%02d.txt", i)} {
			p := path.Join(dir, name)
			data := r.randomBytes(100 + i)
			if err := v.WriteAt(p, 3, data); err != nil {
				r.check("I-401", "a small file copied", false, fmt.Sprintf("%s: %v", p, err))
				return nil
			}
			if err := av.SetAttr(p, 0o640, src.Add(time.Duration(i)*time.Second)); err != nil {
				r.check("I-401", "a small file's attributes set", false, fmt.Sprintf("%s: %v", p, err))
				return nil
			}
			files = append(files, file{p, append(make([]byte, 3), data...)})
		}
	}
	took := time.Since(t)
	r.event("small files copied", fmt.Sprintf("%d files with their mode and times in %s (%.0f ms a file) via %s", len(files), took.Round(time.Millisecond), float64(took.Milliseconds())/float64(len(files)), gw.Name))
	after, err := r.Env.Status(ctx, gw)
	if err != nil {
		return err
	}
	delta := func(a, b map[string]uint64, op string) int64 { return int64(b[op]) - int64(a[op]) } //nolint:gosec // counters
	p0, p1 := before.Gateway.Proposals, after.Gateway.Proposals
	creates, sets := delta(p0, p1, "create"), delta(p0, p1, "setattr")
	r.check("I-401 I-403", "the copy proposed a create per file and no SetAttr",
		creates >= int64(len(files)) && sets <= smallSlack,
		fmt.Sprintf("%d creates, %d setattrs for %d files; %d SETATTRs held", creates, sets, len(files), after.Gateway.AttrsDeferred-before.Gateway.AttrsDeferred))
	r.check("I-403", "status counts the held SETATTRs", after.Gateway.AttrsDeferred-before.Gateway.AttrsDeferred >= uint64(len(files)),
		fmt.Sprintf("attrs_deferred %d → %d", before.Gateway.AttrsDeferred, after.Gateway.AttrsDeferred))
	// the writing gateway shows them at once
	shown := 0
	for i, f := range files {
		mode, mtime, err := av.Attrs(f.p)
		if err == nil && mode == 0o640 && mtime.Equal(src.Add(time.Duration(i/2)*time.Second)) {
			shown++
		}
	}
	r.checkAt("I-401", "the writing gateway shows each file's mode and times at once", shown == len(files), fmt.Sprintf("%d of %d", shown, len(files)), dir, "", gw.Name)
	// after the idle flush, every gateway has them with the data
	if err := r.Env.Wait(ctx, r.Profile.FlushIdle); err != nil {
		return err
	}
	for range 20 {
		rep, err := r.Env.Status(ctx, gw)
		if err == nil && rep.Gateway != nil && rep.Gateway.Staged == 0 {
			break
		}
		if err := r.Env.Wait(ctx, r.Profile.FlushIdle); err != nil {
			return err
		}
	}
	other := gw
	if len(gws) > 1 {
		other = gws[1]
	}
	ov, _ := r.attrVolume(other)
	exact, landed := 0, 0
	var first string
	for i, f := range files {
		got, err := r.volume(other).ReadAll(f.p)
		if err == nil && bytes.Equal(got, f.data) {
			exact++
		}
		if ov == nil {
			continue
		}
		mode, mtime, err := ov.Attrs(f.p)
		if err == nil && mode == 0o640 && mtime.Equal(src.Add(time.Duration(i/2)*time.Second)) {
			landed++
		} else if first == "" {
			first = fmt.Sprintf("%s: mode %o mtime %v err %v", f.p, mode, mtime, err)
		}
	}
	r.checkAt("I-401", "every file reads back exact on another gateway", exact == len(files), fmt.Sprintf("%d of %d via %s", exact, len(files), other.Name), dir, "", other.Name)
	if ov != nil {
		r.checkAt("I-401", "every file has the copy's mode and times on another gateway", landed == len(files), fmt.Sprintf("%d of %d via %s; %s", landed, len(files), other.Name, first), dir, "", other.Name)
	}
	// a SETATTR that changes nothing on a clean file proposes nothing
	mid, err := r.Env.Status(ctx, gw)
	if err != nil {
		return err
	}
	for i, f := range files[:10] {
		if err := av.SetAttr(f.p, 0o640, src.Add(time.Duration(i/2)*time.Second)); err != nil {
			return err
		}
	}
	end, err := r.Env.Status(ctx, gw)
	if err != nil {
		return err
	}
	noop := delta(mid.Gateway.Proposals, end.Gateway.Proposals, "setattr")
	r.check("I-402", "a SETATTR that changes nothing proposes nothing", noop <= smallSlack, fmt.Sprintf("%d setattrs for 10 unchanged files", noop))
	for _, f := range files {
		_ = v.Remove(f.p)
	}
	_ = v.Remove(dir)
	return nil
}

// attrVolume is node's volume as an AttrVolume, when the environment's is one (the
// measuring wrapper of Run.volume hides it).
func (r *Run) attrVolume(node NodeRef) (AttrVolume, bool) {
	v, ok := r.Env.Volume(node)
	if !ok {
		return nil, false
	}
	av, ok := v.(AttrVolume)
	return av, ok
}
