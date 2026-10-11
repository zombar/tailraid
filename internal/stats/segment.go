package stats

import (
	"regexp"
	"strings"
	"time"
)

// Root is the volume directory the series live under (docs/17 §2, docs/06 §1: ephemeral).
const Root = "/.tailraid/stats"

// ClusterWriter is the directory name of the leader's cluster rows.
const ClusterWriter = "cluster"

// Tier is one resolution of the archive.
type Tier int

// The tiers (docs/17 §2).
const (
	Raw    Tier = iota // one file per UTC hour
	Minute             // one file per UTC day
	Coarse             // one file per UTC month, rebuilt daily
)

// Tiers in order, finest first.
var Tiers = []Tier{Raw, Minute, Coarse}

// Dir is the tier's directory under Root.
func (t Tier) Dir() string {
	switch t {
	case Minute:
		return "1m"
	case Coarse:
		return "10m"
	case Raw:
	}
	return "10s"
}

func (t Tier) String() string { return t.Dir() }

// Step is the tier's row interval (the raw tier's is the sampler's own; 10 s nominal).
func (t Tier) Step() time.Duration {
	switch t {
	case Minute:
		return time.Minute
	case Coarse:
		return 10 * time.Minute
	case Raw:
	}
	return 10 * time.Second
}

// Period is the [Start, End) a segment at this tier covers around at: the UTC hour, day
// or month.
func (t Tier) Period(at time.Time) (start, end time.Time) {
	at = at.UTC()
	switch t {
	case Minute:
		start = time.Date(at.Year(), at.Month(), at.Day(), 0, 0, 0, 0, time.UTC)
		return start, start.AddDate(0, 0, 1)
	case Coarse:
		start = time.Date(at.Year(), at.Month(), 1, 0, 0, 0, 0, time.UTC)
		return start, start.AddDate(0, 1, 0)
	case Raw:
	}
	start = at.Truncate(time.Hour)
	return start, start.Add(time.Hour)
}

const suffix = ".jsonl.gz"

// SegmentName names a segment by its tier, the instant of its first row and its writer:
// a raw hour `2026-10-11T14-jockey`, a partial hour `2026-10-11T1423-jockey` (the second
// seal within one minute takes the seconds form, `2026-10-11T142315-jockey`), a day
// `2026-10-11-jockey`, a month `2026-10-jockey` (I-192).
func SegmentName(t Tier, first time.Time, writer string) string {
	first = first.UTC()
	var stamp string
	switch t {
	case Minute:
		stamp = first.Format("2006-01-02")
	case Coarse:
		stamp = first.Format("2006-01")
	default:
		if first.Minute() == 0 {
			stamp = first.Format("2006-01-02T15")
		} else {
			stamp = first.Format("2006-01-02T1504")
		}
	}
	return stamp + "-" + writer + suffix
}

// Period is what a segment's name says about it.
type Period struct {
	Start, End time.Time
	Writer     string
}

var (
	rawName    = regexp.MustCompile(`^(\d{4}-\d{2}-\d{2}T\d{2})(\d{2}(?:\d{2})?)?-(.+)$`)
	minuteName = regexp.MustCompile(`^(\d{4}-\d{2}-\d{2})-(.+)$`)
	coarseName = regexp.MustCompile(`^(\d{4}-\d{2})-(.+)$`)
)

// ParseName reads a segment's period and writer back from its name; ok is false for
// anything else in the directory (a temp, a stranger).
func ParseName(t Tier, name string) (Period, bool) {
	base, ok := strings.CutSuffix(name, suffix)
	if !ok || base == "" || strings.HasPrefix(base, ".") {
		return Period{}, false
	}
	switch t {
	case Minute:
		m := minuteName.FindStringSubmatch(base)
		if m == nil {
			return Period{}, false
		}
		start, err := time.Parse("2006-01-02", m[1])
		if err != nil || m[2] == "" {
			return Period{}, false
		}
		return Period{Start: start, End: start.AddDate(0, 0, 1), Writer: m[2]}, true
	case Coarse:
		m := coarseName.FindStringSubmatch(base)
		if m == nil {
			return Period{}, false
		}
		start, err := time.Parse("2006-01", m[1])
		if err != nil || m[2] == "" {
			return Period{}, false
		}
		return Period{Start: start, End: start.AddDate(0, 1, 0), Writer: m[2]}, true
	case Raw:
	}
	m := rawName.FindStringSubmatch(base)
	if m == nil {
		return Period{}, false
	}
	start, err := time.Parse("2006-01-02T15", m[1])
	if err != nil || m[3] == "" {
		return Period{}, false
	}
	if m[2] != "" { // a partial hour: its first row's minute, or second after a second seal within the minute
		layout := "2006-01-02T1504"
		if len(m[2]) == 4 {
			layout = "2006-01-02T150405"
		}
		fine, err := time.Parse(layout, m[1]+m[2])
		if err != nil {
			return Period{}, false
		}
		start = fine
	}
	return Period{Start: start, End: start.Truncate(time.Hour).Add(time.Hour), Writer: m[3]}, true
}

// Expired reports whether a segment's period ended more than keep ago (I-194).
func Expired(p Period, keep time.Duration, now time.Time) bool {
	return now.Sub(p.End) > keep
}
