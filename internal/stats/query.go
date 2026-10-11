package stats

import (
	"bytes"
	"container/list"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"path"
	"sort"
	"sync"
	"time"

	"github.com/zombar/tailraid/internal/clock"
)

// Member is a device member as the query sees it: where to ask for its live edge and how
// the page links its own UI (docs/13 §1).
type Member struct {
	Name string
	Addr string // the RPC address host:port
	Live bool
	Self bool
	Web  uint16 // the browser UI's port, 0 when off
}

// Fetcher asks one member for its unsealed rows newer than since (GET /v1/stats).
type Fetcher func(ctx context.Context, addr string, since int64) ([]Row, error)

// Caller is the member RPC client's GET.
type Caller interface {
	Call(ctx context.Context, addr, method, path string, body []byte) ([]byte, error)
}

// FetchRPC is the Fetcher over the member RPC.
func FetchRPC(c Caller) Fetcher {
	return func(ctx context.Context, addr string, since int64) ([]Row, error) {
		b, err := c.Call(ctx, addr, http.MethodGet, fmt.Sprintf("/v1/stats?since=%d", since), nil)
		if err != nil {
			return nil, err
		}
		var rows []Row
		if err := json.Unmarshal(b, &rows); err != nil {
			return nil, fmt.Errorf("stats from %s: %w", addr, err)
		}
		return rows, nil
	}
}

// Versioned is a Volume that also names a file's current version, so a parsed segment
// is cached until the file changes (the month segment is rebuilt daily).
type Versioned interface {
	Volume
	Version(ctx context.Context, name string) (string, error)
}

// The live edge's rules (docs/17 §3, I-196).
const (
	FetchTimeout = 2 * time.Second  // a member that misses it leaves a gap
	FetchBackoff = 30 * time.Second // and is not asked again for this long
	FetchShare   = 2 * time.Second  // one fan-out serves every tab asking within it
	FetchIdle    = 10 * time.Second // the fan-out runs only while a browser asked within it
	NameWindow   = time.Hour        // segments named within an hour of the range are opened
)

// Query answers the page: the archive from the finest tier whose retention covers the
// range, the live edge from every live member, merged and deduplicated.
type Query struct {
	Vol        Versioned
	Clk        clock.Clock
	Keep       Keep
	CacheBytes int64 // the parsed-segment cache's bound (stats.cache)
	Self       func(since int64) []Row
	Members    func() []Member
	Fetch      Fetcher
	Log        *slog.Logger

	mu      sync.Mutex
	cache   map[string]*list.Element // path → entry
	lru     list.List
	cached  int64
	live    map[string]*liveEdge // member → its rows and the newest t it reported
	backoff map[string]time.Time
	shared  []Row // the last fan-out's merged live rows
	fetched time.Time
	asked   time.Time
}

type cacheEntry struct {
	path, version string
	rows          []Row
	size          int64
}

type liveEdge struct {
	rows  []Row
	since int64
}

// Range is what the answer covers.
type Range struct {
	From, To time.Time
	Tier     Tier
}

// TierFor picks the finest tier whose retention covers from (docs/17 §3).
func (q *Query) TierFor(from, now time.Time) Tier {
	age := now.Sub(from)
	switch {
	case age <= q.Keep.Raw:
		return Raw
	case age <= q.Keep.Minute:
		return Minute
	}
	return Coarse
}

// Rows returns the rows of [from, to) from the archive, plus the live edge of every live
// member when live is set, deduplicated: node rows by (n, t), cluster rows by t keeping
// the highest term.
func (q *Query) Rows(ctx context.Context, from, to time.Time, live bool) ([]Row, Range, error) {
	now := q.Clk.Now()
	tier := q.TierFor(from, now)
	rows, err := q.archive(ctx, tier, from, to)
	if err != nil {
		return nil, Range{}, err
	}
	if live {
		rows = append(rows, q.liveRows(ctx, from)...)
	}
	return dedup(rows), Range{From: from, To: to, Tier: tier}, nil
}

// archive reads the tier's segments named within NameWindow of the range, from every
// writer, and keeps the rows whose own t falls inside it.
func (q *Query) archive(ctx context.Context, tier Tier, from, to time.Time) ([]Row, error) {
	writers, err := q.Vol.List(ctx, path.Join(Root, tier.Dir()))
	if err != nil {
		return nil, err
	}
	var out []Row
	for _, writer := range writers {
		names, err := q.Vol.List(ctx, path.Join(Root, tier.Dir(), writer))
		if err != nil {
			continue
		}
		sort.Strings(names)
		for _, n := range names {
			p, ok := ParseName(tier, n)
			if !ok || !p.End.After(from.Add(-NameWindow)) || !p.Start.Before(to.Add(NameWindow)) {
				continue
			}
			rows, err := q.segment(ctx, path.Join(Root, tier.Dir(), writer, n))
			if err != nil {
				q.log().Warn("stats: segment skipped", "segment", n, "err", err)
				continue
			}
			for _, r := range rows {
				if r.T >= from.Unix() && r.T < to.Unix() {
					out = append(out, r)
				}
			}
		}
	}
	return out, nil
}

// segment reads one file through the cache, keyed by path and version, bounded by
// CacheBytes with the least recently used dropped first.
func (q *Query) segment(ctx context.Context, p string) ([]Row, error) {
	version, err := q.Vol.Version(ctx, p)
	if err != nil {
		return nil, err
	}
	q.mu.Lock()
	if q.cache == nil {
		q.cache = map[string]*list.Element{}
	}
	if el, ok := q.cache[p]; ok {
		e := el.Value.(*cacheEntry)
		if e.version == version {
			q.lru.MoveToFront(el)
			q.mu.Unlock()
			return e.rows, nil
		}
		q.lru.Remove(el)
		delete(q.cache, p)
		q.cached -= e.size
	}
	q.mu.Unlock()
	data, err := q.Vol.ReadFile(ctx, p)
	if err != nil {
		return nil, err
	}
	rows, err := Decode(bytes.NewReader(data))
	if err != nil {
		return nil, err
	}
	size := int64(len(data)) * 8 // parsed rows take about eight times the gzipped bytes
	q.mu.Lock()
	defer q.mu.Unlock()
	if q.CacheBytes > 0 && size <= q.CacheBytes {
		for q.cached+size > q.CacheBytes && q.lru.Len() > 0 {
			old := q.lru.Back()
			e := old.Value.(*cacheEntry)
			q.lru.Remove(old)
			delete(q.cache, e.path)
			q.cached -= e.size
		}
		q.cache[p] = q.lru.PushFront(&cacheEntry{path: p, version: version, rows: rows, size: size})
		q.cached += size
	}
	return rows, nil
}

// Cached is how many bytes the parsed-segment cache holds.
func (q *Query) Cached() int64 {
	q.mu.Lock()
	defer q.mu.Unlock()
	return q.cached
}

// liveRows is the live edge: this node's own ring and, within one shared fan-out every
// FetchShare, every other live member's rows since the newest instant it reported; a
// member that misses FetchTimeout leaves a gap and is not asked again for FetchBackoff.
func (q *Query) liveRows(ctx context.Context, from time.Time) []Row {
	now := q.Clk.Now()
	q.mu.Lock()
	q.asked = now
	if !q.fetched.IsZero() && now.Sub(q.fetched) < FetchShare {
		rows := q.shared
		q.mu.Unlock()
		return q.sinceFrom(rows, from)
	}
	if q.live == nil {
		q.live, q.backoff = map[string]*liveEdge{}, map[string]time.Time{}
	}
	var members []Member
	if q.Members != nil {
		members = q.Members()
	}
	type ask struct {
		m     Member
		since int64
	}
	var asks []ask
	for _, m := range members {
		if m.Self || !m.Live || q.Fetch == nil {
			continue
		}
		if until, ok := q.backoff[m.Name]; ok && now.Before(until) {
			continue
		}
		e := q.live[m.Name]
		if e == nil {
			e = &liveEdge{since: from.Unix() - 1}
			q.live[m.Name] = e
		}
		asks = append(asks, ask{m, e.since})
	}
	q.mu.Unlock()
	type reply struct {
		name string
		rows []Row
		err  error
	}
	replies := make(chan reply, len(asks))
	for _, a := range asks {
		go func(a ask) {
			fctx, cancel := context.WithTimeout(ctx, FetchTimeout)
			defer cancel()
			rows, err := q.Fetch(fctx, a.m.Addr, a.since)
			replies <- reply{a.m.Name, rows, err}
		}(a)
	}
	q.mu.Lock()
	defer q.mu.Unlock()
	for range asks {
		r := <-replies
		if r.err != nil {
			q.backoff[r.name] = now.Add(FetchBackoff)
			q.log().Warn("stats: no live rows from member", "member", r.name, "err", r.err)
			continue
		}
		e := q.live[r.name]
		e.rows = append(e.rows, r.rows...)
		for _, row := range r.rows {
			e.since = max(e.since, row.T)
		}
		if cut := now.Add(-2 * time.Hour).Unix(); len(e.rows) > 0 && e.rows[0].T < cut {
			i := sort.Search(len(e.rows), func(i int) bool { return e.rows[i].T >= cut })
			e.rows = append([]Row(nil), e.rows[i:]...)
		}
	}
	var all []Row
	if q.Self != nil {
		all = append(all, q.Self(from.Unix()-1)...)
	}
	for _, m := range members {
		if e := q.live[m.Name]; e != nil {
			all = append(all, e.rows...)
		}
	}
	q.shared, q.fetched = all, now
	return q.sinceFrom(all, from)
}

func (q *Query) sinceFrom(rows []Row, from time.Time) []Row {
	var out []Row
	for _, r := range rows {
		if r.T >= from.Unix() {
			out = append(out, r)
		}
	}
	return out
}

// Asking reports whether a browser asked for the live edge within FetchIdle.
func (q *Query) Asking() bool {
	q.mu.Lock()
	defer q.mu.Unlock()
	return !q.asked.IsZero() && q.Clk.Now().Sub(q.asked) < FetchIdle
}

func (q *Query) log() *slog.Logger {
	if q.Log == nil {
		return slog.Default()
	}
	return q.Log
}

// dedup keeps one row per (node, t) and, for the cluster rows, one per t with the
// highest term; the result is ordered by t, then by node name.
func dedup(rows []Row) []Row {
	type key struct {
		t int64
		n string
	}
	best := map[key]int{}
	var out []Row
	for _, r := range rows {
		k := key{r.T, r.N}
		if r.C != nil {
			k.n = "\x00cluster"
		}
		if i, ok := best[k]; ok {
			if r.C != nil && r.Term > out[i].Term {
				out[i] = r
			}
			continue
		}
		best[k] = len(out)
		out = append(out, r)
	}
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].T != out[j].T {
			return out[i].T < out[j].T
		}
		return out[i].N < out[j].N
	})
	return out
}
