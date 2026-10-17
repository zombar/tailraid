package search

import (
	"sort"
	"strings"
	"time"

	"github.com/zombar/tailraid/internal/meta"
)

// Query limits (docs/18 §6, I-208).
const (
	DefaultLimit = 50
	MaxLimit     = 200
)

// Hit is one result of GET /api/search (docs/13 §3).
type Hit struct {
	Path   string    `json:"path"`
	Type   string    `json:"type"` // file | dir | symlink
	Size   uint64    `json:"size"`
	Mtime  time.Time `json:"mtime"`
	Ino    meta.Ino  `json:"ino"`
	Match  string    `json:"match"` // name | tags | content | field
	Title  string    `json:"title,omitempty"`
	Artist string    `json:"artist,omitempty"`
	Album  string    `json:"album,omitempty"`
	Terms  []TermHit `json:"terms"`
	score  int
}

// TermHit is a matched term and the byte offset of its first occurrence.
type TermHit struct {
	Term string `json:"term"`
	Off  int    `json:"off"`
}

// Result is the body of GET /api/search.
type Result struct {
	Query string `json:"query"`
	Hits  []Hit  `json:"hits"`
	Index string `json:"index,omitempty"` // "unavailable" when only the names answered
	// Err is a malformed query (docs/18 §6a): the web route answers 400 with its offset.
	Err *QueryError `json:"-"`
}

// nameList is the folded name words of every inode (docs/18 §6): one byte slice, each
// name as " w1 w2 ", kept up to date from the change feed (I-388). Every inode is in it,
// the trash and /.tailraid too, so an entry depends on its own name alone; hitOf filters
// those per hit. An entry replaced or dropped stays in the slice as dead until a compaction.
type nameList struct {
	applied uint64 // the feed's index this list runs to
	buf     []byte
	ents    []nameEnt // ino 0: dead
	at      map[meta.Ino]int
	dead    int
}

type nameEnt struct {
	ino    meta.Ino
	off, n uint32
}

func newNameList(applied uint64) *nameList {
	return &nameList{applied: applied, at: map[meta.Ino]int{}}
}

// set records in's name, replacing what the list held for it; a name with no words, an
// ephemeral one and the root hold nothing.
func (nl *nameList) set(in meta.Inode) {
	nl.drop(in.Ino)
	if in.Ino == meta.RootIno || meta.IsEphemeral(in.Name) {
		return
	}
	ws := Words(in.Name)
	if len(ws) == 0 {
		return
	}
	off := len(nl.buf)
	nl.buf = append(nl.buf, ' ')
	for _, w := range ws {
		nl.buf = append(nl.buf, w.Text...)
		nl.buf = append(nl.buf, ' ')
	}
	nl.at[in.Ino] = len(nl.ents)
	nl.ents = append(nl.ents, nameEnt{ino: in.Ino, off: uint32(off), n: uint32(len(nl.buf) - off)}) //nolint:gosec // a name list fits
}

func (nl *nameList) drop(ino meta.Ino) {
	if i, ok := nl.at[ino]; ok {
		nl.ents[i].ino = 0
		delete(nl.at, ino)
		nl.dead++
	}
}

// compact rewrites the slice without its dead entries once they are the majority.
func (nl *nameList) compact() {
	if nl.dead < 1024 || nl.dead < len(nl.ents)/2 {
		return
	}
	buf := make([]byte, 0, len(nl.buf))
	ents := make([]nameEnt, 0, len(nl.at))
	for _, e := range nl.ents {
		if e.ino == 0 {
			continue
		}
		off := len(buf)
		buf = append(buf, nl.buf[e.off:e.off+e.n]...)
		nl.at[e.ino] = len(ents)
		ents = append(ents, nameEnt{ino: e.ino, off: uint32(off), n: e.n}) //nolint:gosec // a name list fits
	}
	nl.buf, nl.ents, nl.dead = buf, ents, 0
}

// underReserved reports whether ino lies under /.tailraid, which the list leaves out.
func underReserved(src Source, ino meta.Ino) bool {
	parts, err := src.PathOf(ino)
	return err == nil && len(parts) > 0 && parts[0] == meta.ReservedDir
}

// freshNames brings nl up to date: from the change feed, O(the changes) (I-388), or by a
// walk when there is no list yet or the feed cannot answer.
func freshNames(src Source, nl *nameList) *nameList {
	if nl != nil {
		inos, upto, ok := src.Changes(nl.applied)
		if ok {
			for _, ino := range inos {
				if in, ok := src.GetInodeAny(ino); ok {
					nl.set(in)
				} else {
					nl.drop(ino)
				}
			}
			nl.applied = upto
			nl.compact()
			return nl
		}
	}
	return buildNames(src)
}

func buildNames(src Source) *nameList {
	_, upto, _ := src.Changes(src.Applied()) // the index the walk starts at: what changes after it the feed brings
	nl := newNameList(upto)
	src.ForEachInode(func(in meta.Inode) bool {
		nl.set(in) // every inode, the trash and /.tailraid too: an entry is its own name alone, so a move or a restore needs no other
		return true
	})
	return nl
}

// matches reports whether the name holds every word, the last as a prefix.
func (nl *nameList) matches(e nameEnt, words []string) bool {
	s := string(nl.buf[e.off : e.off+e.n])
	for i, w := range words {
		if i == len(words)-1 {
			if !strings.Contains(s, " "+w) {
				return false
			}
		} else if !strings.Contains(s, " "+w+" ") {
			return false
		}
	}
	return true
}

// search answers q over the names and the chain (docs/18 §6); its field terms, matched
// through ft, keep the files of the items they match (§6a, I-412). ft is nil when the
// query has no field terms or no chain loaded, and a field term then matches nothing.
func (x *Index) search(src Source, names *nameList, q string, pq Query, ft *fieldTable, under string, limit int) Result {
	res := Result{Query: q, Hits: []Hit{}}
	var words []string
	var fields []Term
	for _, t := range pq.Terms {
		if t.Field == FieldWord {
			words = append(words, strings.Fields(t.Value)...)
		} else {
			fields = append(fields, t)
		}
	}
	var keep map[meta.Ino]bool // the files of the items the field terms match
	if len(fields) > 0 {
		keep = map[meta.Ino]bool{}
		if ft != nil {
			for _, f := range ft.files(src, sortedKeys(ft.match(fields))) {
				keep[f] = true
			}
		}
	}
	if len(words) == 0 {
		if keep != nil {
			res.Hits = fieldHits(src, keep, under, limit)
		}
		return res
	}
	if limit <= 0 {
		limit = DefaultLimit
	}
	limit = min(limit, MaxLimit)
	var underParts []string
	if p := meta.SplitPath(under); len(p) > 0 {
		in, err := src.Resolve(p)
		if err != nil || in.Type != meta.TDir {
			return res
		}
		underParts = p
	}
	seen := map[meta.Ino]bool{}
	var nameHits, tagHits, contentHits []Hit
	for _, e := range names.ents {
		if e.ino == 0 || !names.matches(e, words) || (keep != nil && !keep[e.ino]) {
			continue
		}
		h, ok := hitOf(src, e.ino, underParts)
		if !ok {
			continue
		}
		h.Match = "name"
		h.score = len(words)
		for _, w := range words {
			h.Terms = append(h.Terms, TermHit{Term: w})
		}
		seen[e.ino] = true
		nameHits = append(nameHits, h)
	}
	if x != nil {
		for key, sc := range x.candidates(words) {
			d := x.Segs[key.seg].Doc(key.doc)
			if s, di, ok := x.FindDoc(d.Ino); !ok || s != key.seg || di != key.doc || seen[d.Ino] || (keep != nil && !keep[d.Ino]) { // superseded, named already, or not a field term's
				continue
			}
			h, ok := hitOf(src, d.Ino, underParts)
			if !ok {
				continue
			}
			h.score, h.Terms = sc.score, sc.terms
			sort.Slice(h.Terms, func(i, j int) bool { return h.Terms[i].Term < h.Terms[j].Term })
			if d.Kind == KindAudio {
				h.Match, h.Title, h.Artist, h.Album = "tags", d.Title, d.Artist, d.Album
				tagHits = append(tagHits, h)
			} else {
				h.Match = "content"
				contentHits = append(contentHits, h)
			}
		}
	}
	for _, group := range [][]Hit{nameHits, tagHits, contentHits} {
		sort.Slice(group, func(i, j int) bool {
			if group[i].score != group[j].score {
				return group[i].score > group[j].score
			}
			if group[i].Path != group[j].Path {
				return group[i].Path < group[j].Path
			}
			return group[i].Ino < group[j].Ino
		})
		res.Hits = append(res.Hits, group...)
	}
	if len(res.Hits) > limit {
		res.Hits = res.Hits[:limit]
	}
	return res
}

type docKey struct{ seg, doc int }

type docScore struct {
	score int
	terms []TermHit
}

// candidates ANDs the words over the chain: exact terms, the last word as a prefix.
func (x *Index) candidates(words []string) map[docKey]*docScore {
	var cur map[docKey]*docScore
	for i, w := range words {
		next := map[docKey]*docScore{}
		for si, s := range x.Segs {
			var offs map[string]int
			if i == len(words)-1 {
				offs = s.Prefix(w)
			} else if off, ok := s.Lookup(w); ok {
				offs = map[string]int{w: off}
			}
			for term, off := range offs {
				for _, p := range s.Postings(off) {
					k := docKey{si, p.Doc}
					if i > 0 && cur[k] == nil {
						continue
					}
					sc := next[k]
					if sc == nil {
						sc = &docScore{}
						if i > 0 {
							*sc = *cur[k]
						}
						next[k] = sc
					}
					sc.score += p.TF
					sc.terms = append(sc.terms, TermHit{Term: term, Off: p.Off})
				}
			}
		}
		cur = next
	}
	return cur
}

// fieldHits lists the files of a query of field terms only, by path (docs/18 §6a).
func fieldHits(src Source, files map[meta.Ino]bool, under string, limit int) []Hit {
	if limit <= 0 {
		limit = DefaultLimit
	}
	limit = min(limit, MaxLimit)
	var underParts []string
	if p := meta.SplitPath(under); len(p) > 0 {
		in, err := src.Resolve(p)
		if err != nil || in.Type != meta.TDir {
			return []Hit{}
		}
		underParts = p
	}
	hits := []Hit{}
	for ino := range files {
		if h, ok := hitOf(src, ino, underParts); ok {
			h.Match, h.Terms = "field", []TermHit{}
			hits = append(hits, h)
		}
	}
	sort.Slice(hits, func(i, j int) bool {
		if hits[i].Path != hits[j].Path {
			return hits[i].Path < hits[j].Path
		}
		return hits[i].Ino < hits[j].Ino
	})
	if len(hits) > limit {
		hits = hits[:limit]
	}
	return hits
}

// hitOf names a live inode outside /.tailraid and the trash, within under.
func hitOf(src Source, ino meta.Ino, under []string) (Hit, bool) {
	in, ok := src.GetInode(ino)
	if !ok || meta.IsEphemeral(in.Name) {
		return Hit{}, false
	}
	parts, err := src.PathOf(ino)
	if err != nil || (len(parts) > 0 && parts[0] == meta.ReservedDir) || len(parts) < len(under) {
		return Hit{}, false
	}
	for i := range under {
		if parts[i] != under[i] {
			return Hit{}, false
		}
	}
	h := Hit{Path: "/" + strings.Join(parts, "/"), Type: "file", Size: in.Size, Mtime: time.Unix(0, in.Mtime).UTC(), Ino: ino}
	switch in.Type {
	case meta.TDir:
		h.Type = "dir"
	case meta.TSymlink:
		h.Type = "symlink"
	case meta.TFile:
	}
	return h, true
}
