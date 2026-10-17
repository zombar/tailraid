package search

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/zombar/tailraid/internal/clock"
	"github.com/zombar/tailraid/internal/mediatag"
	"github.com/zombar/tailraid/internal/meta"
)

// Config bounds a pass (docs/18 §2 and §5, config.SearchSettings).
type Config struct {
	MaxFile    int   // bytes of a text file indexed
	Workers    int   // files read at once
	Batch      int   // candidates a pass takes
	BatchBytes int64 // text a pass holds
	MaxBytes   int64 // index size past which text is no longer indexed
}

// DefaultConfig is the documented defaults.
func DefaultConfig() Config {
	return Config{MaxFile: 1 << 20, Workers: 2, Batch: 2000, BatchBytes: 64 << 20, MaxBytes: 256 << 20}
}

// SweepAge is how old a segment outside the chain must be before the sweep removes it
// (docs/18 §4, I-206).
const SweepAge = time.Hour

// SweepEvery is how often the pass walks every inode though the change feed answers: it
// finds what no change reports, a text file the size cap once left out (docs/18 §5).
const SweepEvery = time.Hour

// Backlog is how soon the leader's next pass runs while candidates are pending
// (docs/18 §5): a large import is indexed pass after pass, not one per interval.
const Backlog = 30 * time.Second

// ErrLost is a pass that lost the create of its segment to another leader (I-206).
var ErrLost = errors.New("search: another pass wrote this segment")

// Status is `status.search` (docs/12 §8).
type Status struct {
	Docs       int       `json:"docs"`
	Segments   int       `json:"segments"`
	Bytes      int64     `json:"bytes"`
	Catalog    int       `json:"catalog"`
	Pending    int       `json:"pending"`
	LastPass   time.Time `json:"last_pass,omitzero"`
	InProgress bool      `json:"in_progress,omitempty"`
	Capped     bool      `json:"capped,omitempty"`
	Index      string    `json:"index,omitempty"` // "unavailable" when the chain failed to load
}

// PassResult is what one pass did.
type PassResult struct {
	Indexed int    `json:"indexed"`
	Pending int    `json:"pending"`
	Seq     uint64 `json:"seq,omitempty"` // the segment written, 0 when nothing changed
	Rebuilt bool   `json:"rebuilt,omitempty"`
	Capped  bool   `json:"capped,omitempty"`
}

// Service is a node's index: the leader's pass and sweep, every node's queries.
type Service struct {
	Src Source
	Vol Volume
	Cfg Config
	Clk clock.Clock
	Log *slog.Logger

	pass sync.Mutex // one pass at a time

	mu    sync.Mutex
	x     *Index
	xErr  error
	names *nameList
	nfo   map[meta.Ino]nfoMemo // the leader's sidecar reads, by content
	// the leader's pass (I-387): the files to check, the feed index they run to, the last
	// walk of every inode, and the catalogue it last wrote with the libraries it came from
	pending  map[meta.Ino]struct{}
	fedTo    uint64
	walked   time.Time
	cat      []Entry
	catLibs  []meta.Library
	games    map[meta.Ino]gamesMemo
	status   Status
	inFlight bool
	// the field table of the newest segment's catalogue (docs/18 §6a), and that segment
	ft    *fieldTable
	ftSeg *Reader
}

type gamesMemo struct {
	hash  [32]byte
	games map[string]GameInfo
	ok    bool
}

type nfoMemo struct {
	hash [32]byte
	n    NFO
	ok   bool
}

func (s *Service) log() *slog.Logger {
	if s.Log == nil {
		return slog.Default()
	}
	return s.Log
}

// index is the current chain, reloaded when the folder moved.
func (s *Service) index(ctx context.Context) (*Index, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.x != nil && !s.x.Stale(s.Src) {
		return s.x, nil
	}
	x, err := Load(ctx, s.Src, s.Vol)
	if err != nil {
		s.xErr = err
		return nil, err
	}
	s.x, s.xErr = x, nil
	return x, nil
}

// Status is this node's view: the chain it has loaded (never a load: the status
// report is built on a timer, I-205), and the pass's counters where it ran.
func (s *Service) Status() Status {
	s.mu.Lock()
	defer s.mu.Unlock()
	st := s.status
	st.InProgress = s.inFlight
	if s.xErr != nil {
		st.Index = "unavailable"
	}
	if x := s.x; x != nil {
		st.Docs, st.Segments, st.Bytes = x.Docs(), len(x.Segs), x.Bytes
		if len(x.Segs) > 0 {
			st.Catalog = len(x.Segs[0].Catalog())
		}
	}
	return st
}

// Warm loads the chain if the folder moved, so the first query after a pass is fast.
func (s *Service) Warm(ctx context.Context) {
	if _, err := s.index(ctx); err != nil {
		s.log().Warn("search: index not loaded", "err", err)
	}
}

// Catalog is the newest segment's catalogue.
func (s *Service) Catalog(ctx context.Context) ([]Entry, error) {
	x, err := s.index(ctx)
	if err != nil {
		return nil, err
	}
	if len(x.Segs) == 0 {
		return nil, nil
	}
	return x.Segs[0].Catalog(), nil
}

// Search answers a query in scope files (docs/18 §6, §6a, I-208, I-412); a malformed one
// is answered with Err set and no hits.
func (s *Service) Search(ctx context.Context, q, under string, limit int) Result {
	pq, perr := ParseQuery(q)
	if perr != nil {
		var qe *QueryError
		if !errors.As(perr, &qe) {
			qe = &QueryError{Msg: perr.Error()}
		}
		return Result{Query: q, Hits: []Hit{}, Err: qe}
	}
	x, err := s.index(ctx)
	s.mu.Lock()
	s.names = freshNames(s.Src, s.names)
	names := s.names
	s.mu.Unlock()
	if err != nil {
		res := (*Index)(nil).search(s.Src, names, q, pq, nil, under, limit)
		res.Index = "unavailable"
		return res
	}
	return x.search(s.Src, names, q, pq, s.fields(x), under, limit)
}

// fields is the field table of x's newest catalogue, built once per segment.
func (s *Service) fields(x *Index) *fieldTable {
	s.mu.Lock()
	defer s.mu.Unlock()
	var seg *Reader
	if len(x.Segs) > 0 {
		seg = x.Segs[0]
	}
	if s.ft == nil || s.ftSeg != seg {
		var es []Entry
		if seg != nil {
			es = seg.Catalog()
		}
		s.ft, s.ftSeg = buildFields(es), seg
	}
	return s.ft
}

// Media answers a query in scope media: the catalogue items it matches, at most limit
// (docs/18 §6a, I-412). An empty query matches nothing; a malformed one is a *QueryError.
func (s *Service) Media(ctx context.Context, q string, limit int) ([]Entry, error) {
	pq, err := ParseQuery(q)
	if err != nil {
		return nil, err
	}
	if len(pq.Terms) == 0 {
		return []Entry{}, nil
	}
	x, err := s.index(ctx)
	if err != nil {
		return nil, err
	}
	items := s.fields(x).Items(pq)
	if limit <= 0 {
		limit = DefaultLimit
	}
	if len(items) > min(limit, MaxLimit) {
		items = items[:min(limit, MaxLimit)]
	}
	return items, nil
}

// Suggest answers the field values for the query's last word (docs/18 §6a, I-413).
func (s *Service) Suggest(ctx context.Context, q string, limit int, allow func(*Entry) bool) ([]SuggestGroup, error) {
	pq, err := ParseQuery(q)
	if err != nil {
		return nil, err
	}
	x, err := s.index(ctx)
	if err != nil {
		return nil, err
	}
	if limit <= 0 {
		limit = DefaultSuggest
	}
	return s.fields(x).Suggest(pq, min(limit, MaxSuggest), allow), nil
}

// candidate is a file the pass will read.
type candidate struct {
	in   meta.Inode
	hash [32]byte
	kind uint8
}

// Pass indexes the next batch of changed files and writes one segment (docs/18 §5).
func (s *Service) Pass(ctx context.Context) (PassResult, error) {
	s.pass.Lock()
	defer s.pass.Unlock()
	s.mu.Lock()
	s.inFlight = true
	s.mu.Unlock()
	defer func() {
		s.mu.Lock()
		s.inFlight = false
		s.mu.Unlock()
	}()
	res, err := s.runPass(ctx)
	if err == nil && res.Seq != 0 {
		s.Warm(ctx) // the leader's own status and queries see the segment it wrote
	}
	s.mu.Lock()
	s.status.LastPass = s.Clk.Now()
	s.status.Pending, s.status.Capped = res.Pending, res.Capped
	s.mu.Unlock()
	return res, err
}

func (s *Service) runPass(ctx context.Context) (PassResult, error) {
	var res PassResult
	x, err := Load(ctx, s.Src, s.Vol)
	if err != nil {
		return res, err
	}
	cfg := s.Cfg
	if cs := int(s.Src.Volume().ChunkSize); cs > 0 && cfg.MaxFile > cs {
		cfg.MaxFile = cs
	}
	res.Capped = cfg.MaxBytes > 0 && x.Bytes >= cfg.MaxBytes
	// the candidates: what the change feed reported since the last pass (and the files
	// under a changed folder), kept pending until indexed; every inode only when the
	// feed cannot answer, on a service's first pass and every SweepEvery (I-387)
	if s.pending == nil {
		s.pending = map[meta.Ino]struct{}{}
	}
	now := s.Clk.Now()
	changed, upto, fed := s.Src.Changes(s.fedTo)
	full := !fed || s.walked.IsZero() || now.Sub(s.walked) >= SweepEvery
	if full {
		changed = nil
		s.Src.ForEachInode(func(in meta.Inode) bool {
			if in.Type == meta.TFile {
				s.pending[in.Ino] = struct{}{}
			}
			return ctx.Err() == nil
		})
		s.walked = now
	} else {
		for _, ino := range changed {
			s.addPending(ino)
		}
	}
	if err := ctx.Err(); err != nil {
		return res, err
	}
	s.fedTo = upto
	var cands []candidate
	for _, ino := range slices.Sorted(maps.Keys(s.pending)) {
		c, ok := s.candidate(ino, x, res.Capped)
		if !ok {
			delete(s.pending, ino)
			continue
		}
		cands = append(cands, c)
	}
	take, held := 0, int64(0)
	for take < len(cands) && take < max(cfg.Batch, 1) {
		if c := cands[take]; c.kind == KindText {
			n := min(int64(c.in.Size), int64(cfg.MaxFile)) //nolint:gosec // a size
			if take > 0 && held+n > cfg.BatchBytes {
				break
			}
			held += n
		}
		take++
	}
	batch, skipped := s.read(ctx, cands[:take], cfg)
	if err := ctx.Err(); err != nil {
		return res, err
	}
	res.Indexed, res.Pending = len(batch.docs), len(cands)-take+skipped

	// the catalogue: album tags from the newest doc of each track, this batch's first
	byIno := map[meta.Ino]Doc{}
	for _, d := range batch.docs {
		byIno[d.Ino] = d
	}
	tags := func(ino meta.Ino) (Doc, bool) {
		if d, ok := byIno[ino]; ok {
			return d, true
		}
		if si, di, ok := x.FindDoc(ino); ok {
			return x.Segs[si].Doc(di), true
		}
		return Doc{}, false
	}
	// the catalogue: only the libraries a change or a newly indexed track lies under are
	// derived again; the others keep their entries, and a change to the set of libraries
	// derives every one (I-387)
	libs := s.Src.Libraries()
	var rebuild map[meta.Ino]bool // nil: every library
	if !full && s.cat != nil && slices.Equal(libs, s.catLibs) {
		rebuild = s.librariesOver(libs, changed, batch.docs)
	}
	cat := s.cat
	if rebuild == nil || len(rebuild) > 0 {
		c := &catalogue{
			src: s.Src, nfo: func(in meta.Inode) (NFO, bool) { return s.readNFO(ctx, in, cfg) }, tags: tags,
			games: func(in meta.Inode) (map[string]GameInfo, bool) { return s.readGamelist(ctx, in, cfg) }, seen: map[meta.Ino]bool{},
		}
		c.walkLibs(libs, rebuild, s.cat)
		cat = c.out
	}
	// what this pass derived is kept once it is in the chain; a pass that fails before
	// derives every library next time, since the changes it consumed are gone
	s.cat = nil
	written := func() {
		s.cat, s.catLibs = cat, libs
		for _, d := range batch.docs {
			delete(s.pending, d.Ino)
		}
	}
	catBytes := encodeCatalog(cat)
	if len(batch.docs) == 0 && len(x.Segs) > 0 && bytes.Equal(catBytes, x.Segs[0].catalogBytes()) {
		written()
		return res, nil // nothing changed: nothing is written (I-206)
	}
	if len(batch.docs) == 0 && len(x.Segs) == 0 && len(cat) == 0 {
		written()
		return res, nil
	}
	applied := s.Src.Applied()
	seq := x.MaxSeq + 1
	var data []byte
	if len(x.Segs)+1 > RebuildAt {
		fresh, err := Decode(0, Encode(0, nil, applied, batch.docs, batch.terms, nil))
		if err != nil {
			return res, err
		}
		data = merge(append([]*Reader{fresh}, x.Segs...), s.Src, applied, cat)
		res.Rebuilt = true
	} else {
		data = Encode(seq, x.Seqs(), applied, batch.docs, batch.terms, cat)
	}
	name := Dir + "/" + segName(seq)
	if err := s.Vol.Create(ctx, name); err != nil {
		if isExist(err) {
			return res, ErrLost
		}
		return res, err
	}
	if err := s.Vol.WriteFile(ctx, name, data); err != nil {
		return res, err
	}
	written()
	res.Seq = seq
	return res, nil
}

// addPending puts a changed file among the candidates. A folder needs nothing: a move or
// a restore changes no file's content, and a file in the trash is indexed too.
func (s *Service) addPending(ino meta.Ino) {
	if in, ok := s.Src.GetInodeAny(ino); ok && in.Type == meta.TFile {
		s.pending[ino] = struct{}{}
	}
}

// candidate is ino when the pass should read it: an audio or text file (text only while
// the index is under its cap) outside /.tailraid whose chain doc is older. A file in the
// trash is indexed too, so a restore needs no pass; a query filters it per hit.
func (s *Service) candidate(ino meta.Ino, x *Index, capped bool) (candidate, bool) {
	in, ok := s.Src.GetInodeAny(ino)
	if !ok || in.Type != meta.TFile || meta.IsEphemeral(in.Name) {
		return candidate{}, false
	}
	kind := uint8(0)
	switch {
	case IsAudio(in.Name):
		kind = KindAudio
	case IsText(in.Name) && !capped:
		kind = KindText
	default:
		return candidate{}, false
	}
	if underReserved(s.Src, ino) {
		return candidate{}, false
	}
	h := ChunksHash(in)
	if si, di, ok := x.FindDoc(ino); ok {
		if d := x.Segs[si].Doc(di); d.Chunks == h && d.Gen == in.Gen {
			return candidate{}, false
		}
	}
	return candidate{in: in, hash: h, kind: kind}, true
}

// librariesOver is the libraries on the ancestor chain of a changed inode or a file this
// pass indexed (its tags feed an album), the nested ones together.
func (s *Service) librariesOver(libs []meta.Library, changed []meta.Ino, docs []Doc) map[meta.Ino]bool {
	isLib := map[meta.Ino]bool{}
	for _, l := range libs {
		isLib[l.Ino] = true
	}
	out := map[meta.Ino]bool{}
	up := func(ino meta.Ino) {
		for cur := ino; cur != 0; {
			if isLib[cur] {
				out[cur] = true
			}
			in, ok := s.Src.GetInodeAny(cur)
			if !ok || cur == meta.RootIno {
				return
			}
			cur = in.Parent
		}
	}
	for _, ino := range changed {
		up(ino)
	}
	for _, d := range docs {
		up(d.Ino)
	}
	return out
}

type batch struct {
	docs  []Doc
	terms map[string][]Posting
}

// read reads the candidates Workers at a time and tokenizes them; a file whose read
// fails is left for the next pass (counted in skipped).
func (s *Service) read(ctx context.Context, cands []candidate, cfg Config) (batch, int) {
	type out struct {
		doc   Doc
		words map[string]*Posting
		ok    bool
	}
	outs := make([]out, len(cands))
	next := make(chan int)
	var wg sync.WaitGroup
	for range max(cfg.Workers, 1) {
		wg.Go(func() {
			for i := range next {
				c := cands[i]
				o := out{doc: Doc{Ino: c.in.Ino, Gen: c.in.Gen, Chunks: c.hash, Kind: c.kind}, words: map[string]*Posting{}}
				n := cfg.MaxFile
				if c.kind == KindAudio {
					n = int(s.Src.Volume().ChunkSize)
				}
				b, err := s.Vol.ReadPrefix(ctx, c.in, n)
				if err != nil {
					s.log().Warn("search: file not read", "ino", c.in.Ino, "err", err)
					continue
				}
				add := func(text string, off bool) {
					for _, w := range Words(text) {
						p := o.words[w.Text]
						if p == nil {
							p = &Posting{Off: w.Off}
							if !off {
								p.Off = 0
							}
							o.words[w.Text] = p
						}
						p.TF++
					}
				}
				if c.kind == KindAudio {
					t, _ := mediatag.Read(bytes.NewReader(b), int64(c.in.Size)) //nolint:gosec // a size
					o.doc.Title, o.doc.Artist, o.doc.Album, o.doc.Genre, o.doc.Year = t.Title, firstOf(t.Artist, t.AlbumArtist), t.Album, t.Genre, t.Year
					add(strings.Join([]string{t.Title, t.Artist, t.AlbumArtist, t.Album, t.Genre}, " "), false)
				} else {
					add(string(b), true)
				}
				o.ok = true
				outs[i] = o
			}
		})
	}
feed:
	for i := range cands {
		select {
		case next <- i:
		case <-ctx.Done():
			break feed
		}
	}
	close(next)
	wg.Wait()
	bt := batch{terms: map[string][]Posting{}}
	skipped := 0
	for i := range outs {
		if !outs[i].ok {
			skipped++
			continue
		}
		di := len(bt.docs)
		bt.docs = append(bt.docs, outs[i].doc)
		for w, p := range outs[i].words {
			bt.terms[w] = append(bt.terms[w], Posting{Doc: di, TF: p.TF, Off: p.Off})
		}
	}
	return bt, skipped
}

// readNFO reads a sidecar once per content (the leader's memo, docs/18 §8).
func (s *Service) readNFO(ctx context.Context, in meta.Inode, cfg Config) (NFO, bool) {
	h := ChunksHash(in)
	s.mu.Lock()
	if m, ok := s.nfo[in.Ino]; ok && m.hash == h {
		s.mu.Unlock()
		return m.n, m.ok
	}
	s.mu.Unlock()
	b, err := s.Vol.ReadPrefix(ctx, in, cfg.MaxFile)
	if err != nil {
		return NFO{}, false
	}
	n, ok := DecodeNFO(b)
	s.mu.Lock()
	if s.nfo == nil {
		s.nfo = map[meta.Ino]nfoMemo{}
	}
	s.nfo[in.Ino] = nfoMemo{hash: h, n: n, ok: ok}
	s.mu.Unlock()
	return n, ok
}

// readGamelist reads a platform's gamelist.xml once per content, as readNFO a sidecar
// (docs/18 §8b).
func (s *Service) readGamelist(ctx context.Context, in meta.Inode, cfg Config) (map[string]GameInfo, bool) {
	h := ChunksHash(in)
	s.mu.Lock()
	if m, ok := s.games[in.Ino]; ok && m.hash == h {
		s.mu.Unlock()
		return m.games, m.ok
	}
	s.mu.Unlock()
	b, err := s.Vol.ReadPrefix(ctx, in, cfg.MaxFile)
	if err != nil {
		return nil, false
	}
	g, ok := DecodeGamelist(b)
	s.mu.Lock()
	if s.games == nil {
		s.games = map[meta.Ino]gamesMemo{}
	}
	s.games[in.Ino] = gamesMemo{hash: h, games: g, ok: ok}
	s.mu.Unlock()
	return g, ok
}

// merge writes one segment from the chain, newest first: each inode's newest doc while
// the file is live, every term's postings renumbered, the dictionaries walked in step
// so nothing but the output is held (docs/18 §4).
func merge(segs []*Reader, src Source, applied uint64, cat []Entry) []byte {
	type ref struct{ seg, doc int }
	var docs []Doc
	var refs []ref
	have := map[meta.Ino]bool{}
	for si, s := range segs {
		for di := range s.NumDocs() {
			d := s.Doc(di)
			if have[d.Ino] {
				continue
			}
			have[d.Ino] = true
			if _, ok := src.GetInodeAny(d.Ino); !ok { // a purged file; one in the trash stays (I-387)
				continue
			}
			docs = append(docs, d)
			refs = append(refs, ref{si, di})
		}
	}
	order := make([]int, len(docs))
	for i := range order {
		order[i] = i
	}
	sort.Slice(order, func(a, b int) bool { return docs[order[a]].Ino < docs[order[b]].Ino })
	remap := make([]map[int]int, len(segs))
	for i := range remap {
		remap[i] = map[int]int{}
	}
	sorted := make([]Doc, len(docs))
	for newIdx, i := range order {
		sorted[newIdx] = docs[i]
		remap[refs[i].seg][refs[i].doc] = newIdx
	}
	e := NewEncoder(0, nil, applied, sorted)
	its := make([]*termIter, len(segs))
	cur := make([]string, len(segs))
	off := make([]int, len(segs))
	live := make([]bool, len(segs))
	for i, s := range segs {
		if s.nTerms > 0 {
			its[i] = s.iter(0)
			cur[i], off[i], live[i] = its[i].next()
		}
	}
	for {
		term, found := "", false
		for i := range segs {
			if live[i] && (!found || cur[i] < term) {
				term, found = cur[i], true
			}
		}
		if !found {
			break
		}
		var posts []Posting
		for i, s := range segs {
			if !live[i] || cur[i] != term {
				continue
			}
			for _, p := range s.Postings(off[i]) {
				if nd, ok := remap[i][p.Doc]; ok {
					p.Doc = nd
					posts = append(posts, p)
				}
			}
			cur[i], off[i], live[i] = its[i].next()
		}
		if len(posts) > 0 {
			sort.Slice(posts, func(a, b int) bool { return posts[a].Doc < posts[b].Doc })
			e.Term(term, posts)
		}
	}
	return e.Finish(cat)
}

// Sweep removes what lies in the index folder outside the current chain once it is
// SweepAge old: a rebuild's predecessors, a broken segment, an abandoned temp (I-206).
func (s *Service) Sweep(ctx context.Context) (int, error) {
	s.pass.Lock()
	defer s.pass.Unlock()
	x, err := Load(ctx, s.Src, s.Vol)
	if err != nil {
		return 0, err
	}
	keep := map[uint64]bool{}
	for _, seq := range x.Seqs() {
		keep[seq] = true
	}
	dir, err := s.Src.Resolve(meta.SplitPath(Dir))
	if err != nil {
		return 0, nil //nolint:nilerr // no folder: nothing to sweep
	}
	kids, err := s.Src.ReadDir(dir.Ino)
	if err != nil {
		return 0, fmt.Errorf("search sweep: %w", err)
	}
	now := s.Clk.Now()
	n := 0
	for i := range kids {
		k := kids[i]
		if seq, ok := parseSeg(k.Name); ok && keep[seq] {
			continue
		}
		if now.Sub(time.Unix(0, k.Mtime)) < SweepAge {
			continue
		}
		if err := s.Vol.Remove(ctx, Dir+"/"+k.Name); err != nil {
			return n, fmt.Errorf("search sweep %s: %w", k.Name, err)
		}
		n++
	}
	return n, nil
}
