package meta

import (
	"cmp"
	"errors"
	"maps"
	"math"
	"slices"
	"strings"
)

// Playback rows (docs/13 §10c, docs/02 §Apply rules, codec 17): for each viewer, where it
// stopped in each file it played, whether it watched it, and how often it played it.

// ErrPlaybackFull refuses a new viewer past PlaybackViewers (I-405).
var ErrPlaybackFull = errors.New("meta: too many playback viewers")

// PlayAction is what a Playback command does to its rows.
type PlayAction uint8

// Playback actions.
const (
	PlayPosition  PlayAction = 1 // one inode: Pos and Dur reported by the page
	PlayPlayed    PlayAction = 2 // one inode: a play counted
	PlayWatched   PlayAction = 3 // 1..MaxPlayBatch inodes marked watched
	PlayUnwatched PlayAction = 4 // 1..MaxPlayBatch inodes marked unwatched
	PlayHide      PlayAction = 5 // one inode: off the Continue watching row
)

// Bounds of the playback rows (docs/02 §Apply rules, I-405).
const (
	ViewerMax       = 128  // a viewer key's bytes
	PlaybackRows    = 5000 // rows per viewer; the oldest (At, Ino) goes past it
	PlaybackViewers = 256  // viewers; a new one past it is ErrPlaybackFull
	PlayOnMax       = 64   // the reporting node's name
	MaxPlayBatch    = 1000 // inodes of one Watched or Unwatched, the web UI's MaxBatch
)

// Playback is one viewer's report or action (op 34).
type Playback struct {
	Viewer string
	Action PlayAction
	Inos   []Ino
	Pos    uint64 // milliseconds
	Dur    uint64 // milliseconds
	On     string // the reporting node's name
	Now    int64
}

// PlayRow is a viewer's state for one file.
type PlayRow struct {
	Pos, Dur uint64 // milliseconds
	Watched  bool
	Plays    uint32
	Hidden   bool
	At       int64 // the FSM clock of the last change
	On       string
}

// Viewer keys: a user's, or a Tailscale login's before the cluster has users.
const (
	ViewerUserPrefix  = "u:"
	ViewerLoginPrefix = "t:"
)

// validViewer reports whether v is a viewer key of docs/13 §10c.
func validViewer(v string) bool {
	if len(v) < 3 || len(v) > ViewerMax {
		return false
	}
	return strings.HasPrefix(v, ViewerUserPrefix) || strings.HasPrefix(v, ViewerLoginPrefix)
}

// WatchedAt reports whether a position marks a file of the given duration watched:
// 90 % of it, or within its last five minutes when it is longer than thirty (I-405).
// Integer milliseconds, so every node agrees.
func WatchedAt(pos, dur uint64) bool {
	if dur == 0 {
		return false
	}
	const five, thirty = 5 * 60 * 1000, 30 * 60 * 1000
	if pos >= dur-dur/10 {
		return true
	}
	return dur > thirty && pos >= dur-five
}

// playIndex is the applier's map from an inode to the viewers holding a row of it, so a
// file deleted for good drops its rows without a walk of every viewer. Kept by the apply,
// rebuilt on Restore, never encoded (I-405).
type playIndex struct{ byIno map[Ino]map[string]struct{} }

func newPlayIndex() *playIndex { return &playIndex{byIno: map[Ino]map[string]struct{}{}} }

func buildPlayIndex(st *State) *playIndex {
	p := newPlayIndex()
	for v, rows := range st.Playback {
		for ino := range rows {
			p.add(ino, v)
		}
	}
	return p
}

func (p *playIndex) add(ino Ino, viewer string) {
	if p == nil {
		return
	}
	set := p.byIno[ino]
	if set == nil {
		set = map[string]struct{}{}
		p.byIno[ino] = set
	}
	set[viewer] = struct{}{}
}

func (p *playIndex) remove(ino Ino, viewer string) {
	if p == nil {
		return
	}
	if set := p.byIno[ino]; set != nil {
		delete(set, viewer)
		if len(set) == 0 {
			delete(p.byIno, ino)
		}
	}
}

// drop removes every viewer's row of ino, a file deleted for good, and every viewer left
// with none.
func (p *playIndex) drop(st *State, ino Ino) {
	if p == nil {
		return
	}
	for v := range p.byIno[ino] {
		rows := st.Playback[v]
		delete(rows, ino)
		if len(rows) == 0 {
			delete(st.Playback, v)
		}
	}
	delete(p.byIno, ino)
}

func playIndexDrift(want, kept *playIndex) []string {
	if kept == nil {
		return []string{"no playback index kept"}
	}
	if !maps.EqualFunc(want.byIno, kept.byIno, maps.Equal) {
		return []string{"playback index drifted"}
	}
	return nil
}

// playback applies op 34 (docs/02 §Apply rules, I-405): every check before any change.
func (a *applier) playback(c *Playback) error {
	if c == nil || !validViewer(c.Viewer) || len(c.On) > PlayOnMax {
		return ErrBadCommand
	}
	st := a.st
	switch c.Action {
	case PlayPosition, PlayPlayed, PlayHide:
		if len(c.Inos) != 1 {
			return ErrBadCommand
		}
	case PlayWatched, PlayUnwatched:
		if len(c.Inos) == 0 || len(c.Inos) > MaxPlayBatch {
			return ErrBadCommand
		}
	default:
		return ErrBadCommand
	}
	if c.Action == PlayPosition && c.Dur == 0 {
		return ErrBadCommand
	}
	for _, ino := range c.Inos {
		in, ok := st.Inodes[ino]
		if !ok || in.Type != TFile || st.lineage(ino).inTrash {
			return ErrNotExist
		}
	}
	rows := st.Playback[c.Viewer]
	if c.Action == PlayHide && rows[c.Inos[0]] == nil {
		return ErrNotExist
	}
	if rows == nil && len(st.Playback) >= PlaybackViewers {
		return ErrPlaybackFull
	}
	now := a.clock(c.Now)
	if rows == nil {
		rows = map[Ino]*PlayRow{}
		st.Playback[c.Viewer] = rows
	}
	row := func(ino Ino) *PlayRow {
		r := rows[ino]
		if r == nil {
			r = &PlayRow{}
			rows[ino] = r
			a.stats.play.add(ino, c.Viewer)
		}
		r.At, r.On = now, c.On
		return r
	}
	for _, ino := range c.Inos {
		switch c.Action {
		case PlayPosition:
			r := row(ino)
			r.Dur, r.Pos, r.Hidden = c.Dur, min(c.Pos, c.Dur), false
			if WatchedAt(r.Pos, r.Dur) {
				r.Watched = true
			}
		case PlayPlayed:
			r := row(ino)
			if r.Plays < math.MaxUint32 {
				r.Plays++
			}
		case PlayWatched:
			row(ino).Watched = true
		case PlayUnwatched:
			r := row(ino)
			r.Watched, r.Pos = false, 0
		case PlayHide:
			row(ino).Hidden = true
		}
	}
	if over := len(rows) - PlaybackRows; over > 0 {
		inos := slices.SortedFunc(maps.Keys(rows), func(x, y Ino) int {
			if rows[x].At != rows[y].At {
				return cmp.Compare(rows[x].At, rows[y].At)
			}
			return cmp.Compare(int64(x), int64(y))
		})
		for _, ino := range inos[:over] {
			delete(rows, ino)
			a.stats.play.remove(ino, c.Viewer)
		}
	}
	return nil
}

// PlaybackRow returns a viewer's row of a file.
func (f *FSM) PlaybackRow(viewer string, ino Ino) (PlayRow, bool) {
	f.mu.RLock()
	defer f.mu.RUnlock()
	r, ok := f.st.Playback[viewer][ino]
	if !ok {
		return PlayRow{}, false
	}
	return *r, true
}

// PlaybackRows returns a copy of every row of a viewer, by inode.
func (f *FSM) PlaybackRows(viewer string) map[Ino]PlayRow {
	f.mu.RLock()
	defer f.mu.RUnlock()
	rows := f.st.Playback[viewer]
	out := make(map[Ino]PlayRow, len(rows))
	for ino, r := range rows {
		out[ino] = *r
	}
	return out
}

func clonePlayback(src map[string]map[Ino]*PlayRow) map[string]map[Ino]*PlayRow {
	out := make(map[string]map[Ino]*PlayRow, len(src))
	for v, rows := range src {
		cp := make(map[Ino]*PlayRow, len(rows))
		for ino, r := range rows {
			rc := *r
			cp[ino] = &rc
		}
		out[v] = cp
	}
	return out
}

// writePlayback writes the rows (snapshot frame 16): viewers in byte order, each one's
// rows by inode.
func writePlayback(w *writer, st *State) {
	viewers := slices.Sorted(maps.Keys(st.Playback))
	w.uvarint(uint64(len(viewers)))
	for _, v := range viewers {
		rows := st.Playback[v]
		w.str(v)
		inos := slices.Sorted(maps.Keys(rows))
		w.uvarint(uint64(len(inos)))
		for _, ino := range inos {
			r := rows[ino]
			w.u64(uint64(ino))
			w.u64(r.Pos)
			w.u64(r.Dur)
			w.bool(r.Watched)
			w.u32(r.Plays)
			w.bool(r.Hidden)
			w.i64(r.At)
			w.str(r.On)
		}
		w.spill()
	}
}

func readPlayback(r *reader, st *State) {
	n := r.length(PlaybackViewers)
	for range n {
		v := r.str()
		rows := map[Ino]*PlayRow{}
		m := r.length(PlaybackRows)
		for range m {
			ino := Ino(r.u64())
			rows[ino] = &PlayRow{Pos: r.u64(), Dur: r.u64(), Watched: r.bool(), Plays: r.u32(), Hidden: r.bool(), At: r.i64(), On: r.str()}
		}
		if r.err != nil {
			return
		}
		st.Playback[v] = rows
	}
}

func writePlaybackPayload(w *writer, p *Playback) {
	w.str(p.Viewer)
	w.u8(uint8(p.Action))
	w.uvarint(uint64(len(p.Inos)))
	for _, ino := range p.Inos {
		w.u64(uint64(ino))
	}
	w.u64(p.Pos)
	w.u64(p.Dur)
	w.str(p.On)
	w.i64(p.Now)
}

func readPlaybackPayload(r *reader) *Playback {
	p := &Playback{Viewer: r.str(), Action: PlayAction(r.u8())}
	if n := r.length(MaxPlayBatch); n > 0 {
		p.Inos = make([]Ino, n)
		for i := range p.Inos {
			p.Inos[i] = Ino(r.u64())
		}
	}
	p.Pos, p.Dur, p.On, p.Now = r.u64(), r.u64(), r.str(), r.i64()
	return p
}
