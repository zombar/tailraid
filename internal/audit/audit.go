// Package audit is the audit log (docs/21 §8, I-357, I-358): one record per applied users
// or secrets operation, admin operation, sign-in and refusal, written as gzip JSON-line
// segments under /.tailraid/audit/<node>/<hour>.jsonl.gz, each record carrying the
// SHA-256 of the one before it in its segment. The leader records applied operations,
// every node its own refusals and sign-ins, and the leader drops old segments.
package audit

import (
	"bufio"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"path"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/zombar/tailraid/internal/clock"
	"github.com/zombar/tailraid/internal/logx"
	"github.com/zombar/tailraid/internal/meta"
	"github.com/zombar/tailraid/internal/stats"
)

const (
	// Root is the log's directory in the volume (ephemeral, docs/06 §1).
	Root = "/" + meta.ReservedDir + "/audit"
	// Suffix is a segment's.
	Suffix = ".jsonl.gz"
	// SeenFile is each node's last activity per user, beside its segments.
	SeenFile = "seen.json"
	// RefusalWindow is how long refusals by one actor of one kind fold into one record.
	RefusalWindow = time.Minute
	// hourLayout is a segment's name: its UTC hour.
	hourLayout = "2006-01-02T15"
)

// Results.
const (
	ResultOK      = "ok"
	ResultRefused = "refused"
)

// Record is one audit record.
type Record struct {
	At     int64  `json:"at"`              // unix nanoseconds
	Index  uint64 `json:"index,omitempty"` // the log index of an applied operation
	Node   string `json:"node"`            // the node that recorded it
	Actor  string `json:"actor,omitempty"` // the user, "" for nobody or the node itself
	Key    string `json:"key,omitempty"`   // the signing key's ID, hex, where one was
	Kind   string `json:"kind"`
	Target string `json:"target,omitempty"`
	Result string `json:"result"`
	Detail string `json:"detail,omitempty"`
	Count  int    `json:"count,omitempty"` // refusals folded into this record
	Prev   string `json:"prev,omitempty"`  // SHA-256 of the previous record's line in the segment, hex
}

// chain is a line's hash, the next record's Prev.
func chain(line []byte) string {
	h := sha256.Sum256(line)
	return hex.EncodeToString(h[:])
}

// Encode writes records as a gzip JSON-line segment, chaining each to the one before;
// the first record's Prev is "".
func Encode(w io.Writer, recs []Record) error {
	gz := gzip.NewWriter(w)
	prev := ""
	for i := range recs {
		recs[i].Prev = prev
		line, err := json.Marshal(recs[i])
		if err != nil {
			return err
		}
		if _, err := gz.Write(append(line, '\n')); err != nil {
			return err
		}
		prev = chain(line)
	}
	return gz.Close()
}

// ErrChain is a segment whose records do not chain: a line was changed, dropped or
// added since it was written.
var ErrChain = errors.New("audit: the segment's chain is broken")

// Decode reads a segment and verifies its chain; the records are returned up to the
// first broken link, with ErrChain.
func Decode(r io.Reader) ([]Record, error) {
	gz, err := gzip.NewReader(r)
	if err != nil {
		return nil, err
	}
	defer gz.Close()
	sc := bufio.NewScanner(gz)
	sc.Buffer(make([]byte, 0, 64<<10), 4<<20)
	var out []Record
	prev := ""
	for sc.Scan() {
		line := sc.Bytes()
		if len(bytes.TrimSpace(line)) == 0 {
			continue
		}
		var rec Record
		if err := json.Unmarshal(line, &rec); err != nil {
			return out, fmt.Errorf("%w: %w", ErrChain, err)
		}
		if rec.Prev != prev {
			return out, ErrChain
		}
		out = append(out, rec)
		prev = chain(line)
	}
	if err := sc.Err(); err != nil {
		return out, err
	}
	return out, nil
}

// SegmentName is a segment's name for the hour at.
func SegmentName(at time.Time) string { return at.UTC().Format(hourLayout) + Suffix }

// ParseName reads a segment's hour from its name.
func ParseName(name string) (time.Time, bool) {
	base, ok := strings.CutSuffix(name, Suffix)
	if !ok {
		return time.Time{}, false
	}
	t, err := time.Parse(hourLayout, base)
	if err != nil {
		return time.Time{}, false
	}
	return t, true
}

// Writer is one node's log: the open hour's records are kept in memory and its segment
// rewritten whole on Flush; refusals fold per actor and kind within RefusalWindow; the
// last activity of every user is kept in SeenFile.
type Writer struct {
	Vol  stats.Volume
	Node string
	Clk  clock.Clock
	Log  *slog.Logger

	warn     logx.Limiter // one line a minute about a segment the volume refused (I-124)
	mu       sync.Mutex
	hour     time.Time // the open segment's hour (zero: none)
	recs     []Record  // the open segment's records
	dirty    bool      // records not yet written
	refusals map[refusalKey]*Record
	hits     []int64 // refusal times of the last hour, for status
	seen     map[string]int64
	seenOK   bool // seen holds what the volume had before this run
	seenDirt bool
}

type refusalKey struct{ actor, kind string }

func (w *Writer) now() time.Time {
	if w.Clk == nil {
		return time.Now()
	}
	return w.Clk.Now()
}

func (w *Writer) log() *slog.Logger {
	if w.Log == nil {
		return slog.Default()
	}
	return w.Log
}

func (w *Writer) dir() string { return path.Join(Root, w.Node) }

// Record adds a record; At, Node and Result default to now, this node and ok.
func (w *Writer) Record(rec Record) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.addLocked(w.fill(rec))
}

func (w *Writer) fill(rec Record) Record {
	if rec.At == 0 {
		rec.At = w.now().UnixNano()
	}
	if rec.Node == "" {
		rec.Node = w.Node
	}
	if rec.Result == "" {
		rec.Result = ResultOK
	}
	return rec
}

// addLocked places a record in its hour, writing the hour that ended first.
func (w *Writer) addLocked(rec Record) {
	at := time.Unix(0, rec.At).UTC().Truncate(time.Hour)
	if w.hour.IsZero() || !at.Equal(w.hour) {
		if !w.hour.IsZero() && w.dirty {
			_ = w.writeLocked(context.Background())
		}
		w.hour, w.dirty = at, false
		w.recs = w.existing(at) // after a restart the hour's earlier records stay in the chain
	}
	w.recs = append(w.recs, rec)
	w.dirty = true
	if rec.Actor != "" {
		if w.seen == nil {
			w.seen = map[string]int64{}
		}
		if rec.At > w.seen[rec.Actor] {
			w.seen[rec.Actor] = rec.At
			w.seenDirt = true
		}
	}
}

// existing reads the hour's segment as written so far, if any.
func (w *Writer) existing(hour time.Time) []Record {
	data, err := w.Vol.ReadFile(context.Background(), path.Join(w.dir(), SegmentName(hour)))
	if err != nil {
		return nil
	}
	recs, err := Decode(bytes.NewReader(data))
	if err != nil {
		w.log().Warn("audit: the open segment does not chain; its earlier records are kept as read", "segment", SegmentName(hour), "err", err)
	}
	return recs
}

// Refuse records a refusal: the first of its actor and kind within RefusalWindow is the
// record, the rest add to its count (I-357).
func (w *Writer) Refuse(actor, key, kind, target, detail string) {
	w.mu.Lock()
	defer w.mu.Unlock()
	now := w.now()
	w.hits = append(w.hits, now.UnixNano())
	k := refusalKey{actor, kind}
	if w.refusals == nil {
		w.refusals = map[refusalKey]*Record{}
	}
	if r, ok := w.refusals[k]; ok {
		r.Count++
		return
	}
	rec := w.fill(Record{At: now.UnixNano(), Actor: actor, Key: key, Kind: kind, Target: target, Result: ResultRefused, Detail: detail, Count: 1})
	w.refusals[k] = &rec
}

// RefusedLastHour counts the refusals this node saw in the last hour (status.users).
func (w *Writer) RefusedLastHour() int {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.trimHits(w.now())
	return len(w.hits)
}

func (w *Writer) trimHits(now time.Time) {
	cut := now.Add(-time.Hour).UnixNano()
	i := 0
	for i < len(w.hits) && w.hits[i] < cut {
		i++
	}
	w.hits = w.hits[i:]
}

// Flush turns the refusal windows that closed into records, writes the open segment
// when it changed and the seen file when it changed; force closes every window (a seal
// at shutdown).
func (w *Writer) Flush(ctx context.Context, force bool) error {
	w.mu.Lock()
	defer w.mu.Unlock()
	now := w.now()
	w.trimHits(now)
	for k, r := range w.refusals {
		if force || now.Sub(time.Unix(0, r.At)) >= RefusalWindow {
			w.addLocked(*r)
			delete(w.refusals, k)
		}
	}
	var err error
	if w.dirty {
		err = w.writeLocked(ctx)
	}
	if w.seenDirt {
		if serr := w.writeSeenLocked(ctx); serr != nil && err == nil {
			err = serr
		}
	}
	return err
}

func (w *Writer) writeLocked(ctx context.Context) error {
	var buf bytes.Buffer
	if err := Encode(&buf, w.recs); err != nil {
		return err
	}
	if err := w.Vol.WriteFile(ctx, path.Join(w.dir(), SegmentName(w.hour)), buf.Bytes()); err != nil {
		w.warn.Clk = w.Clk
		w.warn.Warn(w.log(), "audit: segment not written; its records are kept for the next flush", "segment", SegmentName(w.hour), "err", err)
		return err
	}
	w.dirty = false
	return nil
}

func (w *Writer) writeSeenLocked(ctx context.Context) error {
	if !w.seenOK { // merge what the volume holds from before a restart
		if data, err := w.Vol.ReadFile(ctx, path.Join(w.dir(), SeenFile)); err == nil {
			var old map[string]int64
			if json.Unmarshal(data, &old) == nil {
				for u, at := range old {
					if at > w.seen[u] {
						w.seen[u] = at
					}
				}
			}
		}
		w.seenOK = true
	}
	data, err := json.Marshal(w.seen)
	if err != nil {
		return err
	}
	if err := w.Vol.WriteFile(ctx, path.Join(w.dir(), SeenFile), data); err != nil {
		return err
	}
	w.seenDirt = false
	return nil
}

// Retain drops every segment of every node whose hour ended more than keep ago
// (I-358); the leader runs it.
func Retain(ctx context.Context, vol stats.Volume, keep time.Duration, now time.Time) (int, error) {
	nodes, err := vol.List(ctx, Root)
	if err != nil {
		return 0, err
	}
	dropped := 0
	for _, n := range nodes {
		names, err := vol.List(ctx, path.Join(Root, n))
		if err != nil {
			return dropped, err
		}
		for _, name := range names {
			hour, ok := ParseName(name)
			if !ok || now.Sub(hour.Add(time.Hour)) <= keep {
				continue
			}
			if err := vol.Remove(ctx, path.Join(Root, n, name)); err != nil {
				return dropped, err
			}
			dropped++
		}
	}
	return dropped, nil
}

// Filter narrows a listing.
type Filter struct {
	Since time.Time // records at or after it; zero for all
	User  string    // the actor; "" for every actor
	Kind  string    // the kind, or a prefix ending in "."; "" for every kind
	Limit int       // at most this many, newest first; 0 for every record
}

// List merges every node's segments, newest record first, through the filter; a
// segment whose chain is broken contributes its intact records and one record saying so.
func List(ctx context.Context, vol stats.Volume, f Filter) ([]Record, error) {
	nodes, err := vol.List(ctx, Root)
	if err != nil {
		return nil, err
	}
	var out []Record
	for _, n := range nodes {
		names, err := vol.List(ctx, path.Join(Root, n))
		if err != nil {
			return nil, err
		}
		for _, name := range names {
			hour, ok := ParseName(name)
			if !ok || (!f.Since.IsZero() && hour.Add(time.Hour).Before(f.Since)) {
				continue
			}
			data, err := vol.ReadFile(ctx, path.Join(Root, n, name))
			if err != nil {
				return nil, err
			}
			recs, derr := Decode(bytes.NewReader(data))
			if derr != nil {
				recs = append(recs, Record{At: hour.UnixNano(), Node: n, Kind: "audit.chain", Result: ResultRefused, Detail: name + ": " + derr.Error()})
			}
			for i := range recs {
				if f.keeps(&recs[i]) {
					out = append(out, recs[i])
				}
			}
		}
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].At > out[j].At })
	if f.Limit > 0 && len(out) > f.Limit {
		out = out[:f.Limit]
	}
	return out, nil
}

func (f Filter) keeps(r *Record) bool {
	if !f.Since.IsZero() && r.At < f.Since.UnixNano() {
		return false
	}
	if f.User != "" && r.Actor != f.User {
		return false
	}
	if f.Kind != "" && r.Kind != f.Kind && (!strings.HasSuffix(f.Kind, ".") || !strings.HasPrefix(r.Kind, f.Kind)) {
		return false
	}
	return true
}

// Seen is every node's last activity per user, merged: the latest record naming them.
func Seen(ctx context.Context, vol stats.Volume) (map[string]int64, error) {
	nodes, err := vol.List(ctx, Root)
	if err != nil {
		return nil, err
	}
	out := map[string]int64{}
	for _, n := range nodes {
		data, err := vol.ReadFile(ctx, path.Join(Root, n, SeenFile))
		if err != nil {
			continue
		}
		var m map[string]int64
		if json.Unmarshal(data, &m) != nil {
			continue
		}
		for u, at := range m {
			if at > out[u] {
				out[u] = at
			}
		}
	}
	return out, nil
}

// Dormant names the active users with no record for longer than after: created before
// the window and never seen, or last seen before it (I-358; check warns, disables none).
func Dormant(users []meta.User, seen map[string]int64, after time.Duration, now time.Time) []string {
	cut := now.Add(-after).UnixNano()
	var out []string
	for _, u := range users {
		if u.State != meta.UserActive || u.CreatedAt >= cut {
			continue
		}
		if at, ok := seen[u.Name]; !ok || at < cut {
			out = append(out, u.Name)
		}
	}
	slices.Sort(out)
	return out
}
