package stats

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path"
	"path/filepath"
	"slices"
	"sort"
	"time"

	"github.com/zombar/tailraid/internal/sysfs"
)

// Volume is what the writer needs of the volume: whole-file writes that are committed
// before the name is visible, reads of committed files that touch no chunk cache, a
// directory listing and a delete (docs/17 §2).
type Volume interface {
	WriteFile(ctx context.Context, name string, data []byte) error
	ReadFile(ctx context.Context, name string) ([]byte, error)
	List(ctx context.Context, dir string) ([]string, error) // the entries' names; none for a missing directory
	Remove(ctx context.Context, name string) error
}

// Keep is the retention of the three tiers (docs/17 §5).
type Keep struct {
	Raw, Minute, Coarse time.Duration
}

// DefaultKeep is 48 h, 30 d and 2 y.
var DefaultKeep = Keep{Raw: 48 * time.Hour, Minute: 720 * time.Hour, Coarse: 17520 * time.Hour}

// Writer owns every stats write of one node (I-192): the node rows under its own name,
// the cluster rows under "cluster" while the node leads, sealed at the hour boundary, on
// stop and on demand; the rollups; and, on the leader, the retention.
type Writer struct {
	Vol   Volume
	Name  string // the member name: this node's directory
	Spill string // a local directory for a seal the cluster cannot take ("" = none)
	Log   *slog.Logger

	node    ring
	cluster ring
}

// Add keeps a row until the next seal: node rows under the writer's name, cluster rows
// under "cluster".
func (w *Writer) Add(row Row) {
	if row.C != nil {
		w.cluster.append(row)
		return
	}
	w.node.append(row)
}

// Live returns at most MaxLive unsealed rows newer than since, oldest first: this node's
// own rows, and its cluster rows while it leads (I-199).
func (w *Writer) Live(since int64) []Row {
	rows := append(w.node.since(since, MaxLive), w.cluster.since(since, MaxLive)...)
	sort.SliceStable(rows, func(i, j int) bool { return rows[i].T < rows[j].T })
	if len(rows) > MaxLive {
		rows = rows[len(rows)-MaxLive:]
	}
	return rows
}

// Unsealed is how many rows wait for the next seal.
func (w *Writer) Unsealed() int { return w.node.len() + w.cluster.len() }

func (w *Writer) log() *slog.Logger {
	if w.Log == nil {
		return slog.Default()
	}
	return w.Log
}

// Seal writes every row before upTo as raw segments (one per writer), named by the first
// row's hour or minute. A segment the cluster cannot take (no leader, a stale gateway) is
// spilled locally and written at the next start; without a spill directory the rows go
// back to wait for the next seal. The error is the first write's.
func (w *Writer) Seal(ctx context.Context, upTo time.Time) error {
	var first error
	for _, r := range []struct {
		ring   *ring
		writer string
	}{{&w.node, w.Name}, {&w.cluster, ClusterWriter}} {
		rows := r.ring.cut(upTo)
		if len(rows) == 0 {
			continue
		}
		if err := w.writeRaw(ctx, r.writer, rows); err != nil {
			if first == nil {
				first = err
			}
			if w.Spill == "" || w.spill(r.writer, rows) != nil {
				r.ring.giveBack(rows)
			}
		}
	}
	return first
}

// writeRaw writes one raw segment of rows (all before one hour boundary).
func (w *Writer) writeRaw(ctx context.Context, writer string, rows []Row) error {
	var buf bytes.Buffer
	if err := Encode(&buf, rows); err != nil {
		return err
	}
	first := time.Unix(rows[0].T, 0).UTC()
	dir := path.Join(Root, Raw.Dir(), writer)
	name := SegmentName(Raw, first, writer)
	if w.exists(ctx, dir, name) { // a second seal within the minute: the seconds form
		name = first.Format("2006-01-02T150405") + "-" + writer + suffix
	}
	return w.Vol.WriteFile(ctx, path.Join(dir, name), buf.Bytes())
}

func (w *Writer) exists(ctx context.Context, dir, name string) bool {
	names, err := w.Vol.List(ctx, dir)
	return err == nil && slices.Contains(names, name)
}

// spill keeps rows the cluster could not take under Spill/<writer>/<name>.
func (w *Writer) spill(writer string, rows []Row) error {
	var buf bytes.Buffer
	if err := Encode(&buf, rows); err != nil {
		return err
	}
	dir := filepath.Join(w.Spill, writer)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	name := SegmentName(Raw, time.Unix(rows[0].T, 0).UTC(), writer)
	if err := sysfs.WriteFileSync(filepath.Join(dir, name), buf.Bytes()); err != nil {
		return err
	}
	w.log().Warn("stats: segment spilled locally", "writer", writer, "segment", name, "rows", len(rows))
	return nil
}

// Recover writes every spilled segment into the volume and removes the local copy; a
// segment the volume still refuses stays for the next start.
func (w *Writer) Recover(ctx context.Context) error {
	if w.Spill == "" {
		return nil
	}
	writers, err := os.ReadDir(w.Spill)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return err
	}
	var first error
	for _, wd := range writers {
		if !wd.IsDir() {
			continue
		}
		files, err := os.ReadDir(filepath.Join(w.Spill, wd.Name()))
		if err != nil {
			continue
		}
		for _, f := range files {
			if _, ok := ParseName(Raw, f.Name()); !ok {
				continue
			}
			local := filepath.Join(w.Spill, wd.Name(), f.Name())
			data, err := os.ReadFile(local) //nolint:gosec // our own spill directory
			if err != nil {
				continue
			}
			if err := w.Vol.WriteFile(ctx, path.Join(Root, Raw.Dir(), wd.Name(), f.Name()), data); err != nil {
				if first == nil {
					first = err
				}
				continue
			}
			_ = os.Remove(local)
			w.log().Info("stats: spilled segment written", "writer", wd.Name(), "segment", f.Name())
		}
	}
	return first
}

// segments lists a writer's segments at a tier, by name.
func (w *Writer) segments(ctx context.Context, t Tier, writer string) ([]string, error) {
	names, err := w.Vol.List(ctx, path.Join(Root, t.Dir(), writer))
	if err != nil {
		return nil, err
	}
	var out []string
	for _, n := range names {
		if _, ok := ParseName(t, n); ok {
			out = append(out, n)
		}
	}
	sort.Strings(out)
	return out, nil
}

// readRows reads a writer's rows at a tier whose segments overlap [from, to), filtered by
// row time: a name is never trusted over a row's own t (docs/17 §2).
func (w *Writer) readRows(ctx context.Context, t Tier, writer string, from, to time.Time) ([]Row, bool, error) {
	names, err := w.segments(ctx, t, writer)
	if err != nil {
		return nil, false, err
	}
	var rows []Row
	found := false
	for _, n := range names {
		p, _ := ParseName(t, n)
		if !p.End.After(from) || !p.Start.Before(to) {
			continue
		}
		found = true
		data, err := w.Vol.ReadFile(ctx, path.Join(Root, t.Dir(), writer, n))
		if err != nil {
			return nil, true, fmt.Errorf("read %s: %w", n, err)
		}
		got, err := Decode(bytes.NewReader(data))
		if err != nil {
			w.log().Warn("stats: unreadable segment skipped", "segment", n, "err", err)
			continue
		}
		for _, r := range got {
			if r.T >= from.Unix() && r.T < to.Unix() {
				rows = append(rows, r)
			}
		}
	}
	return rows, found, nil
}

// RollupDay derives a writer's minute segment of a UTC day from its sealed raw segments
// and rebuilds the month's coarse segment: the month file's rows before the day plus the
// day's rows (I-193). Nothing is written for a day without raw rows.
func (w *Writer) RollupDay(ctx context.Context, writer string, day time.Time) error {
	start, end := Minute.Period(day)
	raw, _, err := w.readRows(ctx, Raw, writer, start, end)
	if err != nil {
		return err
	}
	if len(raw) == 0 {
		return nil
	}
	var buf bytes.Buffer
	if err := Encode(&buf, Rollup(raw, Minute)); err != nil {
		return err
	}
	if err := w.Vol.WriteFile(ctx, path.Join(Root, Minute.Dir(), writer, SegmentName(Minute, start, writer)), buf.Bytes()); err != nil {
		return fmt.Errorf("minute segment: %w", err)
	}
	mStart, _ := Coarse.Period(day)
	month, _, err := w.readRows(ctx, Coarse, writer, mStart, start)
	if err != nil {
		return err
	}
	month = append(month, Rollup(raw, Coarse)...)
	buf.Reset()
	if err := Encode(&buf, month); err != nil {
		return err
	}
	if err := w.Vol.WriteFile(ctx, path.Join(Root, Coarse.Dir(), writer, SegmentName(Coarse, mStart, writer)), buf.Bytes()); err != nil {
		return fmt.Errorf("month segment: %w", err)
	}
	return nil
}

// CatchUp rolls up every day within keepRaw of now whose minute segment is missing while
// its raw segments exist, for the writers given (I-193); yesterday included, today never.
func (w *Writer) CatchUp(ctx context.Context, now time.Time, keepRaw time.Duration, writers ...string) error {
	today, _ := Minute.Period(now)
	var first error
	for _, writer := range writers {
		have := map[string]bool{}
		names, err := w.segments(ctx, Minute, writer)
		if err != nil {
			return err
		}
		for _, n := range names {
			have[n] = true
		}
		for day := today.Add(-keepRaw).Truncate(24 * time.Hour); day.Before(today); day = day.AddDate(0, 0, 1) {
			if have[SegmentName(Minute, day, writer)] {
				continue
			}
			if _, found, err := w.readRows(ctx, Raw, writer, day, day.AddDate(0, 0, 1)); err != nil || !found {
				continue
			}
			if err := w.RollupDay(ctx, writer, day); err != nil && first == nil {
				first = err
			}
		}
	}
	return first
}

// Retain removes every segment of every writer whose period ended more than its tier's
// keep ago (I-194); the leader runs it hourly. It returns how many it removed.
func (w *Writer) Retain(ctx context.Context, now time.Time, keep Keep) (int, error) {
	removed := 0
	var first error
	for _, t := range Tiers {
		k := keep.Raw
		switch t {
		case Minute:
			k = keep.Minute
		case Coarse:
			k = keep.Coarse
		case Raw:
		}
		writers, err := w.Vol.List(ctx, path.Join(Root, t.Dir()))
		if err != nil {
			return removed, err
		}
		for _, writer := range writers {
			names, err := w.segments(ctx, t, writer)
			if err != nil {
				continue
			}
			for _, n := range names {
				p, _ := ParseName(t, n)
				if !Expired(p, k, now) {
					continue
				}
				if err := w.Vol.Remove(ctx, path.Join(Root, t.Dir(), writer, n)); err != nil {
					if first == nil {
						first = err
					}
					continue
				}
				removed++
			}
		}
	}
	return removed, first
}
