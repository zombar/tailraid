package stats

import (
	"testing"
	"time"
)

var utc = func(y int, mo time.Month, d, h, mi int) time.Time { return time.Date(y, mo, d, h, mi, 0, 0, time.UTC) }

// A segment is named by its tier, period and writer, and the name reads back to its
// period: a whole raw hour, a partial hour from its first row's minute, a day, a month.
// Invariant: I-192
func TestSegmentName_RoundTrip_Table(t *testing.T) {
	for _, tc := range []struct {
		tier       Tier
		first      time.Time
		writer     string
		name       string
		start, end time.Time
	}{
		{Raw, utc(2026, 10, 11, 14, 0), "jockey", "2026-10-11T14-jockey.jsonl.gz", utc(2026, 10, 11, 14, 0), utc(2026, 10, 11, 15, 0)},
		{Raw, utc(2026, 10, 11, 14, 23), "jockey", "2026-10-11T1423-jockey.jsonl.gz", utc(2026, 10, 11, 14, 23), utc(2026, 10, 11, 15, 0)},
		{Raw, utc(2026, 10, 11, 23, 59), "cluster", "2026-10-11T2359-cluster.jsonl.gz", utc(2026, 10, 11, 23, 59), utc(2026, 10, 12, 0, 0)},
		{Raw, utc(2026, 10, 11, 14, 23).Add(15 * time.Second), "jockey", "2026-10-11T1423-jockey.jsonl.gz", utc(2026, 10, 11, 14, 23), utc(2026, 10, 11, 15, 0)},
		{Minute, utc(2026, 10, 11, 0, 0), "node0", "2026-10-11-node0.jsonl.gz", utc(2026, 10, 11, 0, 0), utc(2026, 10, 12, 0, 0)},
		{Coarse, utc(2026, 10, 1, 0, 0), "node0", "2026-10-node0.jsonl.gz", utc(2026, 10, 1, 0, 0), utc(2026, 11, 1, 0, 0)},
		{Coarse, utc(2026, 12, 1, 0, 0), "a-b", "2026-12-a-b.jsonl.gz", utc(2026, 12, 1, 0, 0), utc(2027, 1, 1, 0, 0)},
	} {
		got := SegmentName(tc.tier, tc.first, tc.writer)
		if got != tc.name {
			t.Errorf("%v %s: name %q, want %q", tc.tier, tc.first, got, tc.name)
		}
		p, ok := ParseName(tc.tier, tc.name)
		if !ok || !p.Start.Equal(tc.start) || !p.End.Equal(tc.end) || p.Writer != tc.writer {
			t.Errorf("%s: parsed %+v %v, want %s..%s by %s", tc.name, p, ok, tc.start, tc.end, tc.writer)
		}
	}
	// the seconds form of a second seal within one minute reads back to the second
	if p, ok := ParseName(Raw, "2026-10-11T142315-jockey.jsonl.gz"); !ok || !p.Start.Equal(utc(2026, 10, 11, 14, 23).Add(15*time.Second)) || !p.End.Equal(utc(2026, 10, 11, 15, 0)) {
		t.Errorf("seconds form: %+v %v", p, ok)
	}
	for _, bad := range []string{"", "x.jsonl.gz", "2026-10-11T14.jsonl.gz", "2026-10-11T14-jockey.jsonl", "2026-13-11T14-a.jsonl.gz", "2026-10-11T1460-a.jsonl.gz", "2026-10-11T142-a.jsonl.gz", "2026-10-11T1423159-a.jsonl.gz", ".tmp-2026-10-11T14-a.jsonl.gz"} {
		if _, ok := ParseName(Raw, bad); ok {
			t.Errorf("%q parsed as a raw segment", bad)
		}
	}
	if _, ok := ParseName(Minute, "2026-10-11T14-jockey.jsonl.gz"); ok {
		t.Error("a raw name parsed as a minute segment")
	}
}

// Every tier knows its directory, row step and the period a row's instant falls in.
// Invariant: I-193
func TestTier_PeriodOf(t *testing.T) {
	at := utc(2026, 10, 11, 14, 23).Add(17 * time.Second)
	for _, tc := range []struct {
		tier       Tier
		dir        string
		step       time.Duration
		start, end time.Time
	}{
		{Raw, "10s", 10 * time.Second, utc(2026, 10, 11, 14, 0), utc(2026, 10, 11, 15, 0)},
		{Minute, "1m", time.Minute, utc(2026, 10, 11, 0, 0), utc(2026, 10, 12, 0, 0)},
		{Coarse, "10m", 10 * time.Minute, utc(2026, 10, 1, 0, 0), utc(2026, 11, 1, 0, 0)},
	} {
		if tc.tier.Dir() != tc.dir || tc.tier.Step() != tc.step {
			t.Errorf("%v: dir %q step %v", tc.tier, tc.tier.Dir(), tc.tier.Step())
		}
		s, e := tc.tier.Period(at)
		if !s.Equal(tc.start) || !e.Equal(tc.end) {
			t.Errorf("%v: period %s..%s, want %s..%s", tc.tier, s, e, tc.start, tc.end)
		}
	}
}

// A segment expires exactly when its period ended more than keep ago: the boundary table.
// Invariant: I-194
func TestSegment_Expired_Table(t *testing.T) {
	end := utc(2026, 10, 11, 15, 0)
	keep := 48 * time.Hour
	for _, tc := range []struct {
		now  time.Time
		want bool
	}{
		{end, false},
		{end.Add(time.Second), false},
		{end.Add(keep - time.Second), false},
		{end.Add(keep), false},
		{end.Add(keep + time.Second), true},
		{end.Add(400 * time.Hour), true},
	} {
		if got := Expired(Period{End: end}, keep, tc.now); got != tc.want {
			t.Errorf("now = end + %v: expired %v, want %v", tc.now.Sub(end), got, tc.want)
		}
	}
}
