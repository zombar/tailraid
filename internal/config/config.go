// Package config loads the node configuration (docs/02, docs/07).
package config

import (
	"crypto/rand"
	"encoding/base32"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math"
	"net"
	"net/netip"
	"os"
	"os/user"
	posix "path"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/goccy/go-yaml"
)

// Config is the YAML node configuration. Volume policy (k, m, chunk size) is not here:
// it lives in the replicated metadata and is set by `init`.
type Config struct {
	Node struct {
		Name    string   `yaml:"name"`
		DataDir string   `yaml:"data_dir"`
		Voter   bool     `yaml:"voter"`
		Disks   []Disk   `yaml:"disks"`             // added disks, each a placement target of its own (docs/14)
		Mirrors []Mirror `yaml:"mirrors,omitempty"` // disks given to snapshots, never placement targets (docs/16)
	} `yaml:"node"`
	Listen struct {
		Raft int    `yaml:"raft"` // tailnet TCP port
		RPC  int    `yaml:"rpc"`  // tailnet TCP port
		Web  int    `yaml:"web"`  // tailnet TCP port of the browser UI (docs/13); 0 disables it
		NFS  string `yaml:"nfs"`  // loopback host:port of the NFS gateway (macOS, Linux; docs/07); "" disables it
		SMB  string `yaml:"smb"`  // loopback host:port of the SMB gateway (Windows; docs/15); "" disables it
	} `yaml:"listen"`
	Mount struct {
		Path  string `yaml:"path"`
		Owner string `yaml:"owner,omitempty"` // "uid:gid" the gateways give files; set by install on macOS from SUDO_UID, where serve runs as root (docs/20 §9)
	} `yaml:"mount"`
	SMB struct {
		Password string `yaml:"password"` // the share's password; init and join generate one when listen.smb is set (docs/15 §6)
	} `yaml:"smb"`
	Mesh struct {
		Port      int    `yaml:"port"`                 // the device's UDP port (docs/20 §1)
		Endpoint  string `yaml:"endpoint,omitempty"`   // a public host:port the operator knows reaches this device
		Subnet    string `yaml:"subnet,omitempty"`     // init only: the cluster's /24 (default: derived from the cluster ID)
		Address   string `yaml:"address,omitempty"`    // this device's address with the subnet's bits, written by init and join; survives reset
		Interface string `yaml:"interface,omitempty"`  // the interface name on Linux and Windows (default tailraid0 / tailraid); macOS picks a utun
		RelayRate string `yaml:"relay_rate,omitempty"` // bytes per second this device forwards for others (default 20M; "0": no limit; docs/20 §6)
		PortMap   *bool  `yaml:"portmap,omitempty"`    // ask the default gateway (PCP, NAT-PMP) to map the port (default true; docs/20 §7)
	} `yaml:"mesh"`
	Log struct {
		Level  string `yaml:"level"`  // debug | info | warn | error (env TAILRAID_LOG_LEVEL overrides)
		Format string `yaml:"format"` // text | json (env TAILRAID_LOG_FORMAT overrides)
		File   string `yaml:"file"`   // append here instead of stderr (env TAILRAID_LOG_FILE overrides)
	} `yaml:"log"`
	Repair struct {
		Grace     string `yaml:"grace"`      // GC grace and shard DELETE fence age (default 10m)
		FillAfter string `yaml:"fill_after"` // dead-node hold-down before re-placing its slots (default 10m)
		S3Scrub   string `yaml:"s3_scrub"`   // bucket bytes the scrub may fetch per day to verify objects (default 1G; "0": HEAD only)
		// written only when set, so a release from before these keys can still read a
		// file this build wrote (a rollback after update, I-61)
		Workers int    `yaml:"workers,omitempty"` // chunks a repair pass works on at once (default 4, at most 64; docs/06 §4)
		Rate    string `yaml:"rate,omitempty"`    // shard bytes per second repair may move (default 50M; "0": no limit)
		// how long the background scrub takes to verify the whole volume (default 168h; "0": off; docs/06 §5)
		ScrubPeriod string `yaml:"scrub_period,omitempty"`
		// the percentage of the stored bytes that must be movable away from home before
		// placement-only moves start (default 5; "0": only `volume rebalance`; docs/14 §7)
		HomeStart string `yaml:"home_start,omitempty"`
		// background work on relayed paths (docs/20 §8): avoid (the default) defers a move,
		// fill or scrub read whose only source or target is relayed unless the slot is bad;
		// allow runs it
		Relay string `yaml:"relay,omitempty"`
	} `yaml:"repair"`
	// these sections are written only when set, so a release from before them can still
	// read a file this build wrote (a rollback after update, I-61)
	Trash struct {
		Retention string `yaml:"retention,omitempty"` // how long a deleted entry waits before the leader purges it (default 720h; "0" never, docs/12 §10)
	} `yaml:"trash,omitempty"`
	Cache struct {
		Size string `yaml:"size,omitempty"` // the chunk cache's budget for unpinned bytes (default 1GiB, at least 64MiB; docs/12 §10)
	} `yaml:"cache,omitempty"`
	// Mirrors is the snapshot schedule of docs/16 §4, run by each mirror's host.
	Mirrors struct {
		Every string `yaml:"every,omitempty"` // an automatic snapshot of every online mirror this often (default 1h; "0" none)
		Keep  struct {
			Hourly  *int `yaml:"hourly,omitempty"`
			Daily   *int `yaml:"daily,omitempty"`
			Weekly  *int `yaml:"weekly,omitempty"`
			Monthly *int `yaml:"monthly,omitempty"`
		} `yaml:"keep,omitempty"`
		Rate       string `yaml:"rate,omitempty"`        // bytes a second a sync reads from the pool (default 50M; "0" no limit)
		HoldMax    string `yaml:"hold_max,omitempty"`    // a sync older than this is ended as failed (default 24h; at least 1h)
		StaleAfter string `yaml:"stale_after,omitempty"` // check warns for a mirror whose newest snapshot is this old (default 168h)
	} `yaml:"mirrors,omitempty"`
	// Stats is the Stats page's history (docs/17 §5): every key is written only when set
	// (I-61), so a release from before them still starts after a rollback.
	Stats struct {
		Every string `yaml:"every,omitempty"` // the sample interval (default 10s; 5s..1m)
		Keep  struct {
			Raw    string `yaml:"raw,omitempty"`    // the 10 s tier (default 48h; at least 26h)
			Minute string `yaml:"minute,omitempty"` // the 1-min tier (default 720h; at least 48h)
			Coarse string `yaml:"coarse,omitempty"` // the 10-min tier (default 17520h)
		} `yaml:"keep,omitempty"`
		Cache string `yaml:"cache,omitempty"` // the gateway's parsed-segment cache (default 32MiB)
	} `yaml:"stats,omitempty"`
	// Search is the leader's index of docs/18 (12 §10): every key is written only when
	// set (I-61).
	Search struct {
		Interval   string `yaml:"interval,omitempty"`    // how often the leader looks for changed files (default 10m; "0" off)
		MaxFile    string `yaml:"max_file,omitempty"`    // how much of a text file is indexed (default 1MiB; 64KiB to one chunk)
		Workers    int    `yaml:"workers,omitempty"`     // files a pass reads at once (default 2; 1..16)
		Batch      int    `yaml:"batch,omitempty"`       // candidates a pass takes (default 2000; 1..100000)
		BatchBytes string `yaml:"batch_bytes,omitempty"` // text a pass holds while it writes its segment (default 64MiB; at least 1MiB)
		MaxBytes   string `yaml:"max_bytes,omitempty"`   // index size past which contents are no longer indexed (default 256MiB; at least 16MiB)
	} `yaml:"search,omitempty"`
	// Users and the audit log (docs/21 §9, I-359): every key is written only when set
	// (I-61).
	Users struct {
		ClockSkew    string `yaml:"clock_skew,omitempty"`    // a signed request's timestamp may be this far from the node's clock (default 5m; 1m..1h)
		Session      string `yaml:"session,omitempty"`       // a browser session's life (default 168h; 1h..720h)
		DormantAfter string `yaml:"dormant_after,omitempty"` // check warns for a user silent this long (default 2160h; 24h..8760h)
	} `yaml:"users,omitempty"`
	Audit struct {
		Retention string `yaml:"retention,omitempty"` // the leader drops older audit segments (default 8760h; 168h..87600h)
	} `yaml:"audit,omitempty"`
	Dev struct {
		Enabled bool `yaml:"enabled"`
		// MeshFake replaces the mesh with plain addresses for the CLI tests, which cannot
		// open a TUN: a JSON file `{"addr": "127.0.0.1"}`; the node binds that address and
		// joins without an introduce (docs/09 §3).
		MeshFake string `yaml:"mesh_fake,omitempty"`
	} `yaml:"dev"`
	// Path is the file Load read, never written to it: the daemon rewrites the file
	// there (node.voter follows the member row, docs/12 §7, I-176).
	Path string `yaml:"-"`
}

// TrashRetention parses trash.retention: 30 days when empty, 0 for "never purged on its
// own", otherwise at least an hour (docs/12 §10, I-157).
func (c Config) TrashRetention() (time.Duration, error) {
	switch c.Trash.Retention {
	case "":
		return 30 * 24 * time.Hour, nil
	case "0":
		return 0, nil
	}
	d, err := time.ParseDuration(c.Trash.Retention)
	if err != nil || d < time.Hour {
		return 0, fmt.Errorf("config: trash.retention %q: want 0 or a duration of at least 1h", c.Trash.Retention)
	}
	return d, nil
}

// MinCacheSize is the smallest cache.size: under it the cache thrashes on one large file.
const MinCacheSize = 64 << 20

// CacheSize parses cache.size: 1 GiB when empty, otherwise at least MinCacheSize
// (docs/12 §10, I-162).
func (c Config) CacheSize() (int64, error) {
	if c.Cache.Size == "" {
		return 1 << 30, nil
	}
	n, err := ParseBytes(c.Cache.Size)
	if err != nil || n < MinCacheSize {
		return 0, fmt.Errorf("config: cache.size %q: want a size of at least 64MiB", c.Cache.Size)
	}
	return int64(n), nil
}

// Mirror is one disk given to snapshots (docs/16 §1).
type Mirror struct {
	Path string `yaml:"path"`
}

// MirrorPolicy is the parsed snapshot schedule (docs/16 §4).
type MirrorPolicy struct {
	Every                          time.Duration // 0: no automatic snapshots
	Hourly, Daily, Weekly, Monthly int
	Rate                           int64 // bytes a second; 0 no limit
	HoldMax, StaleAfter            time.Duration
}

// MaxKeep bounds each mirrors.keep count.
const MaxKeep = 10000

// UsersPolicy is the parsed users and audit keys (docs/21 §9, I-359).
type UsersPolicy struct {
	ClockSkew      time.Duration
	Session        time.Duration
	DormantAfter   time.Duration
	AuditRetention time.Duration
}

// UsersPolicy parses the users and audit keys, each within its bounds: clock_skew 5m
// (1m..1h), session 168h (1h..720h), dormant_after 2160h (24h..8760h), audit.retention
// 8760h (168h..87600h).
func (c Config) UsersPolicy() (UsersPolicy, error) {
	p := UsersPolicy{ClockSkew: 5 * time.Minute, Session: 168 * time.Hour, DormantAfter: 2160 * time.Hour, AuditRetention: 8760 * time.Hour}
	for _, k := range []struct {
		key, s string
		lo, hi time.Duration
		out    *time.Duration
	}{
		{"users.clock_skew", c.Users.ClockSkew, time.Minute, time.Hour, &p.ClockSkew},
		{"users.session", c.Users.Session, time.Hour, 720 * time.Hour, &p.Session},
		{"users.dormant_after", c.Users.DormantAfter, 24 * time.Hour, 8760 * time.Hour, &p.DormantAfter},
		{"audit.retention", c.Audit.Retention, 168 * time.Hour, 87600 * time.Hour, &p.AuditRetention},
	} {
		if k.s == "" {
			continue
		}
		d, err := time.ParseDuration(k.s)
		if err != nil {
			return p, fmt.Errorf("config: %s %q: %w", k.key, k.s, errBadDuration(err))
		}
		if d < k.lo || d > k.hi {
			return p, fmt.Errorf("config: %s %s: want %s to %s", k.key, k.s, k.lo, k.hi)
		}
		*k.out = d
	}
	return p, nil
}

// Mirroring parses the mirrors section: every 1h (0, or at least a minute), keep 24, 30,
// 8 and 12 (0..MaxKeep), rate 50M, hold_max 24h (at least 1h), stale_after 168h.
func (c Config) Mirroring() (MirrorPolicy, error) {
	m := c.Mirrors
	p := MirrorPolicy{Every: time.Hour, Hourly: 24, Daily: 30, Weekly: 8, Monthly: 12, Rate: 50 << 20, HoldMax: 24 * time.Hour, StaleAfter: 7 * 24 * time.Hour}
	dur := func(key, s string, min time.Duration, zero bool, out *time.Duration) error {
		if s == "" {
			return nil
		}
		if s == "0" && zero {
			*out = 0
			return nil
		}
		d, err := time.ParseDuration(s)
		if err != nil || d < min {
			return fmt.Errorf("config: mirrors.%s %q: want a duration of at least %s", key, s, min)
		}
		*out = d
		return nil
	}
	if err := dur("every", m.Every, time.Minute, true, &p.Every); err != nil {
		return p, err
	}
	if err := dur("hold_max", m.HoldMax, time.Hour, false, &p.HoldMax); err != nil {
		return p, err
	}
	if err := dur("stale_after", m.StaleAfter, time.Hour, false, &p.StaleAfter); err != nil {
		return p, err
	}
	for key, kv := range map[string]struct {
		v   *int
		out *int
	}{"hourly": {m.Keep.Hourly, &p.Hourly}, "daily": {m.Keep.Daily, &p.Daily}, "weekly": {m.Keep.Weekly, &p.Weekly}, "monthly": {m.Keep.Monthly, &p.Monthly}} {
		if kv.v == nil {
			continue
		}
		if *kv.v < 0 || *kv.v > MaxKeep {
			return p, fmt.Errorf("config: mirrors.keep.%s %d: want 0..%d", key, *kv.v, MaxKeep)
		}
		*kv.out = *kv.v
	}
	if m.Rate != "" {
		n, err := ParseBytes(m.Rate)
		if err != nil {
			return p, fmt.Errorf("config: mirrors.rate: %w", err)
		}
		p.Rate = int64(n)
	}
	return p, nil
}

// Disk is one added disk: its mount path and its budget ("" derives it from the disk).
type Disk struct {
	Path     string `yaml:"path"`
	Capacity string `yaml:"capacity"`
}

// RepairDurations parses the repair knobs (empty = default).
func (c Config) RepairDurations() (grace, fillAfter time.Duration, err error) {
	grace, fillAfter = 10*time.Minute, 10*time.Minute
	if c.Repair.Grace != "" {
		if grace, err = time.ParseDuration(c.Repair.Grace); err != nil || grace <= 0 {
			return 0, 0, fmt.Errorf("config: repair.grace %q: %w", c.Repair.Grace, errBadDuration(err))
		}
	}
	if c.Repair.FillAfter != "" {
		if fillAfter, err = time.ParseDuration(c.Repair.FillAfter); err != nil || fillAfter <= 0 {
			return 0, 0, fmt.Errorf("config: repair.fill_after %q: %w", c.Repair.FillAfter, errBadDuration(err))
		}
	}
	return grace, fillAfter, nil
}

// S3ScrubBytes parses repair.s3_scrub, the bucket bytes the scrub may fetch per day
// (docs/06 §5, I-151): 1 GiB when empty, 0 for HEAD only.
func (c Config) S3ScrubBytes() (int64, error) {
	if c.Repair.S3Scrub == "" {
		return 1 << 30, nil
	}
	n, err := ParseBytes(c.Repair.S3Scrub)
	if err != nil || n > 1<<62 {
		return 0, fmt.Errorf("config: repair.s3_scrub %q: bad size", c.Repair.S3Scrub)
	}
	return int64(n), nil //nolint:gosec // bounded above
}

// RepairWorkers parses repair.workers, the chunks a repair pass works on at once
// (docs/06 §4, I-174): 0 when unset (the repairer's default), otherwise 1..64.
func (c Config) RepairWorkers() (int, error) {
	if w := c.Repair.Workers; w < 0 || w > 64 {
		return 0, fmt.Errorf("config: repair.workers %d: want 1..64", w)
	}
	return c.Repair.Workers, nil
}

// MaxScrubPeriod bounds repair.scrub_period: a year.
const MaxScrubPeriod = 8760 * time.Hour

// RepairScrubPeriod parses repair.scrub_period (docs/06 §5, I-215): set reports whether
// the key is present; "0" turns the background scrub off, otherwise 1h..MaxScrubPeriod.
func (c Config) RepairScrubPeriod() (d time.Duration, set bool, err error) {
	switch c.Repair.ScrubPeriod {
	case "":
		return 0, false, nil
	case "0":
		return 0, true, nil
	}
	d, err = time.ParseDuration(c.Repair.ScrubPeriod)
	if err != nil || d < time.Hour || d > MaxScrubPeriod {
		return 0, false, fmt.Errorf("config: repair.scrub_period %q: want 0 or a duration of 1h to 8760h", c.Repair.ScrubPeriod)
	}
	return d, true, nil
}

// RepairHomeStart parses repair.home_start (docs/14 §7, I-295), a percentage of 0 to 100
// with an optional "%", as a fraction: set reports whether the key is present, and 0
// keeps the home gate closed unless `volume rebalance` opens it.
func (c Config) RepairHomeStart() (f float64, set bool, err error) {
	if c.Repair.HomeStart == "" {
		return 0, false, nil
	}
	v, err := strconv.ParseFloat(strings.TrimSuffix(strings.TrimSpace(c.Repair.HomeStart), "%"), 64)
	if err != nil || v < 0 || v > 100 || math.IsNaN(v) {
		return 0, false, fmt.Errorf("config: repair.home_start %q: want a percentage of 0 to 100", c.Repair.HomeStart)
	}
	return v / 100, true, nil
}

// RepairRateBytes parses repair.rate, the shard bytes a second repair may move: set is
// false when the key is empty (the repairer's default), and 0 means no limit.
func (c Config) RepairRateBytes() (n int64, set bool, err error) {
	if c.Repair.Rate == "" {
		return 0, false, nil
	}
	u, err := ParseBytes(c.Repair.Rate)
	if err != nil || u > 1<<62 {
		return 0, false, fmt.Errorf("config: repair.rate %q: bad size", c.Repair.Rate)
	}
	return int64(u), true, nil //nolint:gosec // bounded above
}

// MeshRelayRateBytes parses mesh.relay_rate: what this device forwards for others per
// second (docs/20 §6); the mesh's default when unset, 0 for "0".
func (c Config) MeshRelayRateBytes() (int64, error) {
	if c.Mesh.RelayRate == "" {
		return 0, nil
	}
	u, err := ParseBytes(c.Mesh.RelayRate)
	if err != nil || u > 1<<62 {
		return 0, fmt.Errorf("config: mesh.relay_rate %q: bad size", c.Mesh.RelayRate)
	}
	return int64(u), nil //nolint:gosec // bounded above
}

// MeshPortMap reports mesh.portmap: on unless set false (docs/20 §7).
func (c Config) MeshPortMap() bool { return c.Mesh.PortMap == nil || *c.Mesh.PortMap }

// RepairAvoidRelay reports repair.relay: true unless it is "allow" (docs/20 §8).
func (c Config) RepairAvoidRelay() (bool, error) {
	switch c.Repair.Relay {
	case "", "avoid":
		return true, nil
	case "allow":
		return false, nil
	}
	return false, fmt.Errorf("config: repair.relay %q: want avoid or allow", c.Repair.Relay)
}

// StatsEvery parses stats.every, the sample interval of docs/17 §1: 10 s when unset,
// otherwise 5 s to 1 min (I-194).
func (c Config) StatsEvery() (time.Duration, error) {
	return statsDuration("stats.every", c.Stats.Every, 10*time.Second, 5*time.Second, time.Minute)
}

// StatsKeep parses the three retentions of docs/17 §5: raw at least 26 h (a day must be
// whole when it is rolled up), minute at least 48 h, coarse at least a day.
func (c Config) StatsKeep() (raw, minute, coarse time.Duration, err error) {
	const year = 365 * 24 * time.Hour
	if raw, err = statsDuration("stats.keep.raw", c.Stats.Keep.Raw, 48*time.Hour, 26*time.Hour, 10*year); err != nil {
		return 0, 0, 0, err
	}
	if minute, err = statsDuration("stats.keep.minute", c.Stats.Keep.Minute, 720*time.Hour, 48*time.Hour, 10*year); err != nil {
		return 0, 0, 0, err
	}
	if coarse, err = statsDuration("stats.keep.coarse", c.Stats.Keep.Coarse, 17520*time.Hour, 24*time.Hour, 100*year); err != nil {
		return 0, 0, 0, err
	}
	return raw, minute, coarse, nil
}

// StatsCacheBytes parses stats.cache, the gateway's parsed-segment cache: 32 MiB when
// unset, at least 1 MiB.
func (c Config) StatsCacheBytes() (int64, error) {
	if c.Stats.Cache == "" {
		return 32 << 20, nil
	}
	n, err := ParseBytes(c.Stats.Cache)
	if err != nil || n < 1<<20 || n > 1<<40 {
		return 0, fmt.Errorf("config: stats.cache %q: want a size of 1MiB to 1TiB", c.Stats.Cache)
	}
	return int64(n), nil //nolint:gosec // bounded above
}

// SearchSettings is the search block parsed (docs/18 §2 and §5, docs/12 §10).
type SearchSettings struct {
	Interval             time.Duration // 0: the pass is off
	MaxFile              int
	Workers, Batch       int
	BatchBytes, MaxBytes int64
}

// SearchSettings parses the search block: the defaults when unset, an error outside the bounds
// the comments on Config.Search give. MaxFile's upper bound is the chunk size, which
// the daemon applies once it knows it.
func (c Config) SearchSettings() (SearchSettings, error) {
	s := SearchSettings{Interval: 10 * time.Minute, MaxFile: 1 << 20, Workers: 2, Batch: 2000, BatchBytes: 64 << 20, MaxBytes: 256 << 20}
	var err error
	if c.Search.Interval == "0" {
		s.Interval = 0
	} else if s.Interval, err = statsDuration("search.interval", c.Search.Interval, s.Interval, time.Minute, 24*time.Hour); err != nil {
		return s, err
	}
	if n, err := searchBytes("search.max_file", c.Search.MaxFile, int64(s.MaxFile), 64<<10, 64<<20); err != nil {
		return s, err
	} else {
		s.MaxFile = int(n)
	}
	if w := c.Search.Workers; w != 0 {
		if w < 1 || w > 16 {
			return s, fmt.Errorf("config: search.workers %d: want 1..16", w)
		}
		s.Workers = w
	}
	if b := c.Search.Batch; b != 0 {
		if b < 1 || b > 100000 {
			return s, fmt.Errorf("config: search.batch %d: want 1..100000", b)
		}
		s.Batch = b
	}
	if s.BatchBytes, err = searchBytes("search.batch_bytes", c.Search.BatchBytes, s.BatchBytes, 1<<20, 1<<40); err != nil {
		return s, err
	}
	if s.MaxBytes, err = searchBytes("search.max_bytes", c.Search.MaxBytes, s.MaxBytes, 16<<20, 1<<40); err != nil {
		return s, err
	}
	return s, nil
}

func searchBytes(key, v string, def, lo, hi int64) (int64, error) {
	if v == "" {
		return def, nil
	}
	n, err := ParseBytes(v)
	if err != nil || int64(n) < lo || int64(n) > hi { //nolint:gosec // hi bounds it
		return 0, fmt.Errorf("config: %s %q: want a size of %d to %d bytes", key, v, lo, hi)
	}
	return int64(n), nil //nolint:gosec // bounded above
}

func statsDuration(key, s string, def, lo, hi time.Duration) (time.Duration, error) {
	if s == "" {
		return def, nil
	}
	d, err := time.ParseDuration(s)
	if err != nil || d < lo || d > hi {
		return 0, fmt.Errorf("config: %s %q: want a duration of %v to %v", key, s, lo, hi)
	}
	return d, nil
}

func errBadDuration(err error) error {
	if err != nil {
		return err
	}
	return errors.New("must be positive")
}

// Home returns the per-OS configuration directory, or $TAILRAID_HOME.
func Home() string {
	home, _ := os.UserHomeDir()
	return homeFor(runtime.GOOS, os.Geteuid(), home, os.Getenv, func(name string) (string, error) {
		u, err := user.Lookup(name)
		if err != nil {
			return "", err
		}
		return u.HomeDir, nil
	})
}

// homeFor is Home as a function of its inputs: the OS, the effective uid, the process's
// home directory, the environment and a lookup of a user's home. On macOS root's home
// is the system one (the daemon's, docs/20 §9); on Linux a command run under sudo (a
// `join`, which needs CAP_NET_ADMIN) keeps the invoking user's home, since the user's
// unit names it and the user owns the data: root's own home only without `SUDO_USER`.
func homeFor(goos string, euid int, home string, env func(string) string, lookupHome func(string) (string, error)) string {
	if h := env("TAILRAID_HOME"); h != "" {
		return h
	}
	switch goos { // POSIX paths are joined as such whatever host runs this (the table test runs on Windows too)
	case "darwin":
		if euid == 0 { // the root daemon's home (docs/20 §9)
			return "/Library/Application Support/tailraid"
		}
		return posix.Join(home, "Library", "Application Support", "tailraid")
	case "windows":
		if pd := env("ProgramData"); pd != "" {
			return filepath.Join(pd, "tailraid")
		}
		return `C:\ProgramData\tailraid`
	default:
		if x := env("XDG_CONFIG_HOME"); x != "" {
			return posix.Join(x, "tailraid")
		}
		if su := env("SUDO_USER"); euid == 0 && su != "" && su != "root" {
			if h, err := lookupHome(su); err == nil && h != "" {
				home = h
			}
		}
		return posix.Join(home, ".config", "tailraid")
	}
}

// DefaultPath is the config file location.
func DefaultPath() string { return filepath.Join(Home(), "config.yaml") }

// The listeners' defaults (docs/08 §Loopback-only listeners, docs/12).
const (
	DefaultRaftPort = 20480
	DefaultRPCPort  = 20481
	DefaultWebPort  = 20482
	DefaultMeshPort = 20483 // UDP (docs/20 §1)
	DefaultNFSAddr  = "127.0.0.1:20490"
)

// Default returns the defaults for this OS.
func Default() Config {
	var c Config
	host, _ := os.Hostname()
	c.Node.Name = host
	c.Node.DataDir = filepath.Join(Home(), "data")
	c.Listen.Raft = DefaultRaftPort
	c.Listen.RPC = DefaultRPCPort
	c.Listen.Web = DefaultWebPort
	c.Mesh.Port = DefaultMeshPort
	c.Mesh.Interface = "tailraid0"
	c.Listen.NFS = DefaultNFSAddr
	c.Log.Level, c.Log.Format = "info", "text"
	home, _ := os.UserHomeDir()
	c.Mount.Path = filepath.Join(home, "TailRAID")
	if runtime.GOOS == "windows" { // Windows mounts over SMB (docs/15)
		c.Listen.NFS = ""
		c.Listen.SMB = DefaultSMBAddr
		c.Mount.Path = "T:"
		c.Mesh.Interface = "tailraid"
	}
	return c
}

// DefaultSMBAddr is the SMB gateway's loopback address: not 445, which Windows' own
// Server service owns (docs/15 §1).
const DefaultSMBAddr = "127.0.0.1:20445"

// NewSMBPassword is a fresh share password: 24 base32 characters from 15 random bytes.
func NewSMBPassword() string {
	var b [15]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic("config: crypto/rand: " + err.Error())
	}
	return base32.StdEncoding.WithPadding(base32.NoPadding).EncodeToString(b[:])
}

// EnsureSMBPassword generates the share password when the SMB gateway is enabled and the
// config has none (init and join call it before saving); it reports whether it did.
func (c *Config) EnsureSMBPassword() bool {
	if c.Listen.SMB == "" || c.SMB.Password != "" {
		return false
	}
	c.SMB.Password = NewSMBPassword()
	return true
}

// Load reads a config file strictly (unknown keys are errors) over the defaults and
// records the path in Path, so the daemon can write the file back (docs/12 §7, I-176).
func Load(path string) (Config, error) {
	c := Default()
	b, err := os.ReadFile(path) //nolint:gosec // the config file the operator named
	if err != nil {
		return c, err
	}
	if err := yaml.UnmarshalWithOptions(b, &c, yaml.Strict()); err != nil {
		return c, fmt.Errorf("config %s: %w", path, err)
	}
	c.expandHome()
	c.Path = path
	return c, c.Validate()
}

// expandHome turns a leading "~" into the user's home directory in every path the
// config names (I-126): a mount path written as ~/TailRAID is otherwise created
// literally under the working directory.
func (c *Config) expandHome() {
	c.Node.DataDir = ExpandHome(c.Node.DataDir)
	c.Mount.Path = ExpandHome(c.Mount.Path)
	c.Log.File = ExpandHome(c.Log.File)
	for i := range c.Node.Disks {
		c.Node.Disks[i].Path = ExpandHome(c.Node.Disks[i].Path)
	}
	for i := range c.Node.Mirrors {
		c.Node.Mirrors[i].Path = ExpandHome(c.Node.Mirrors[i].Path)
	}
}

// ExpandHome replaces a leading "~" or "~/" with the home directory.
func ExpandHome(p string) string {
	if p != "~" && !strings.HasPrefix(p, "~/") && !strings.HasPrefix(p, `~\`) {
		return p
	}
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return p
	}
	return filepath.Join(home, p[1:])
}

// Save writes the config, creating the directory.
func Save(path string, c Config) error {
	if err := c.Validate(); err != nil {
		return err
	}
	b, err := yaml.Marshal(c)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// Validate checks the fields that must be sane before serve starts.
func (c Config) Validate() error {
	if c.Node.DataDir == "" {
		return errors.New("config: node.data_dir is required")
	}
	if c.Listen.NFS == "" && c.Listen.SMB == "" {
		return errors.New("config: one of listen.nfs or listen.smb is required")
	}
	seen := map[string]bool{}
	for i, d := range c.Node.Disks {
		if d.Path == "" || !filepath.IsAbs(d.Path) {
			return fmt.Errorf("config: node.disks[%d].path %q: want an absolute path", i, d.Path)
		}
		p := filepath.Clean(d.Path)
		if seen[p] || p == filepath.Clean(c.Node.DataDir) {
			return fmt.Errorf("config: node.disks[%d].path %q is listed twice or is the data directory", i, d.Path)
		}
		seen[p] = true
		if d.Capacity != "" {
			if _, err := ParseBytes(d.Capacity); err != nil {
				return fmt.Errorf("config: node.disks[%d].capacity: %w", i, err)
			}
		}
	}
	for i, m := range c.Node.Mirrors {
		if m.Path == "" || !filepath.IsAbs(m.Path) {
			return fmt.Errorf("config: node.mirrors[%d].path %q: want an absolute path", i, m.Path)
		}
		p := filepath.Clean(m.Path)
		if seen[p] || p == filepath.Clean(c.Node.DataDir) {
			return fmt.Errorf("config: node.mirrors[%d].path %q is listed twice, is a disk or is the data directory", i, m.Path)
		}
		seen[p] = true
	}
	if _, err := c.Mirroring(); err != nil {
		return err
	}
	if _, err := c.UsersPolicy(); err != nil {
		return err
	}
	if w := c.Listen.Web; w < 0 || w > 65535 {
		return fmt.Errorf("config: listen.web %d: want 0 (off) or a TCP port", w)
	} else if w != 0 && (w == c.Listen.RPC || w == c.Listen.Raft) {
		return fmt.Errorf("config: listen.web %d must differ from listen.rpc and listen.raft", w)
	}
	if err := c.validateMesh(); err != nil {
		return err
	}
	// the rest of I-126: every port in range and distinct, known log settings, an
	// absolute mount path
	// a gateway listener is "" when that gateway is off (docs/15 §4); a set one is host:port
	gatewayPort := func(name, addr string) (int, error) {
		if addr == "" {
			return 0, nil
		}
		_, port, err := net.SplitHostPort(addr)
		if err != nil {
			return 0, fmt.Errorf("config: %s %q: want host:port", name, addr)
		}
		n, err := strconv.Atoi(port)
		if err != nil || n < 1 || n > 65535 {
			return 0, fmt.Errorf("config: %s %q: want a TCP port", name, addr)
		}
		return n, nil
	}
	np, err := gatewayPort("listen.nfs", c.Listen.NFS)
	if err != nil {
		return err
	}
	sp, err := gatewayPort("listen.smb", c.Listen.SMB)
	if err != nil {
		return err
	}
	used := map[int]string{}
	for _, l := range []struct {
		name string
		port int
	}{{"listen.raft", c.Listen.Raft}, {"listen.rpc", c.Listen.RPC}, {"listen.nfs", np}, {"listen.smb", sp}, {"listen.web", c.Listen.Web}} {
		if l.port == 0 && (l.name == "listen.web" || l.name == "listen.nfs" || l.name == "listen.smb") {
			continue // off
		}
		if l.port < 1 || l.port > 65535 {
			return fmt.Errorf("config: %s %d: want a TCP port", l.name, l.port)
		}
		if other, dup := used[l.port]; dup {
			return fmt.Errorf("config: %s and %s both use port %d", other, l.name, l.port)
		}
		used[l.port] = l.name
	}
	switch strings.ToLower(c.Log.Level) {
	case "", "debug", "info", "warn", "warning", "error":
	default:
		return fmt.Errorf("config: log.level %q: want debug, info, warn or error", c.Log.Level)
	}
	switch strings.ToLower(c.Log.Format) {
	case "", "text", "json":
	default:
		return fmt.Errorf("config: log.format %q: want text or json", c.Log.Format)
	}
	if c.Mount.Path != "" && runtime.GOOS != "windows" && !filepath.IsAbs(c.Mount.Path) {
		return fmt.Errorf("config: mount.path %q: want an absolute path (~ is expanded)", c.Mount.Path)
	}
	if _, _, err := c.RepairScrubPeriod(); err != nil {
		return err
	}
	if _, _, err := c.RepairHomeStart(); err != nil {
		return err
	}
	if _, err := c.TrashRetention(); err != nil {
		return err
	}
	if _, err := c.CacheSize(); err != nil {
		return err
	}
	if _, err := c.StatsEvery(); err != nil {
		return err
	}
	if _, _, _, err := c.StatsKeep(); err != nil {
		return err
	}
	if _, err := c.StatsCacheBytes(); err != nil {
		return err
	}
	if _, err := c.SearchSettings(); err != nil {
		return err
	}
	return nil
}

// PathEntry is one path the daemon owns on this device: the data directory, a disk or a
// mirror (docs/08 §Files on disk).
type PathEntry struct {
	Kind string `json:"kind"` // "data directory", "disk" or "mirror"
	Path string `json:"path"`
}

// Paths lists the data directory, then every disk, then every mirror.
func (c Config) Paths() []PathEntry {
	out := make([]PathEntry, 0, 1+len(c.Node.Disks)+len(c.Node.Mirrors))
	out = append(out, PathEntry{Kind: "data directory", Path: c.Node.DataDir})
	for _, d := range c.Node.Disks {
		out = append(out, PathEntry{Kind: "disk", Path: d.Path})
	}
	for _, m := range c.Node.Mirrors {
		out = append(out, PathEntry{Kind: "mirror", Path: m.Path})
	}
	return out
}

// Warnings lists what serve should say about the config file at path, the data
// directory, the disks and the mirrors before it runs, and what status and check on the
// device repeat (I-126, I-341): a mode that lets other users of the machine read the join
// token or the shards, or an owner that is not the daemon's user. A path that does not
// exist is init's problem, not a warning. Windows has no mode bits worth reading.
func (c Config) Warnings(path string) []string {
	if runtime.GOOS == "windows" {
		return nil
	}
	uid := os.Getuid()
	var out []string
	warn := func(kind, p, how string, want int) {
		st, err := os.Stat(p)
		if err != nil {
			return
		}
		if perm := st.Mode().Perm(); perm&0o077 != 0 {
			out = append(out, fmt.Sprintf("%s %s is %s other users (mode %04o): chmod %o %s", kind, p, how, perm, want, p))
		}
		if owner, ok := ownedBy(st); ok && owner != uid {
			out = append(out, fmt.Sprintf("%s %s is owned by uid %d, not the daemon's uid %d: chown %d %s", kind, p, owner, uid, uid, p))
		}
	}
	warn("config file", path, "readable by", 0o600)
	for _, e := range c.Paths() {
		warn(e.Kind, e.Path, "accessible to", 0o700)
	}
	return out
}

// ParseBytes parses "500G", "1.5T", "100M", "4096" (binary units).
func ParseBytes(s string) (uint64, error) {
	if s == "" {
		return 0, errors.New("empty size")
	}
	var num float64
	var unit string
	if _, err := fmt.Sscanf(s, "%g%s", &num, &unit); err != nil {
		if _, err2 := fmt.Sscanf(s, "%g", &num); err2 != nil {
			return 0, fmt.Errorf("bad size %q", s)
		}
	}
	mult := map[string]float64{
		"": 1, "B": 1, "K": 1 << 10, "KB": 1 << 10, "KIB": 1 << 10, "M": 1 << 20, "MB": 1 << 20, "MIB": 1 << 20,
		"G": 1 << 30, "GB": 1 << 30, "GIB": 1 << 30, "T": 1 << 40, "TB": 1 << 40, "TIB": 1 << 40,
	}
	m, ok := mult[strings.ToUpper(unit)]
	if !ok || num < 0 {
		return 0, fmt.Errorf("bad size %q", s)
	}
	return uint64(num * m), nil
}

// OpenLogger builds the logger from log.level/log.format/log.file and the TAILRAID_LOG_*
// overrides. The LogFile is nil when output goes to stderr (service managers and Docker
// capture it); every method of a nil LogFile is a no-op, so callers close and reopen it
// without checking.
func (c Config) OpenLogger() (*slog.Logger, *LogFile) {
	level, format := c.Log.Level, c.Log.Format
	if v := os.Getenv("TAILRAID_LOG_LEVEL"); v != "" {
		level = v
	}
	if v := os.Getenv("TAILRAID_LOG_FORMAT"); v != "" {
		format = v
	}
	var lvl slog.Level
	switch strings.ToLower(level) {
	case "debug":
		lvl = slog.LevelDebug
	case "info", "":
		lvl = slog.LevelInfo
	case "warn", "warning":
		lvl = slog.LevelWarn
	case "error":
		lvl = slog.LevelError
	default:
		fmt.Fprintf(os.Stderr, "tailraid: unknown log level %q; using info\n", level)
		lvl = slog.LevelInfo
	}
	opts := &slog.HandlerOptions{Level: lvl}
	var out io.Writer = os.Stderr
	var lf *LogFile
	if file := c.LogFile(); file != "" {
		if f, err := openLogFile(file); err == nil {
			out, lf = f, f
		} else {
			fmt.Fprintf(os.Stderr, "tailraid: log file %s: %v; logging to stderr\n", file, err)
		}
	}
	if strings.EqualFold(format, "json") {
		return slog.New(slog.NewJSONHandler(out, opts)), lf
	}
	return slog.New(slog.NewTextHandler(out, opts)), lf
}

// Logger is the logger for a CLI command: the configured level and format, always to
// stderr. Only `serve` opens log.file (OpenLogger); a command run by a user must not need
// the service's log directory (%ProgramData% on Windows is writable by administrators
// only) and must not interleave its lines with the service's.
func (c Config) Logger() *slog.Logger {
	c.Log.File = ""
	saved := os.Getenv("TAILRAID_LOG_FILE")
	if saved != "" {
		_ = os.Unsetenv("TAILRAID_LOG_FILE")
		defer func() { _ = os.Setenv("TAILRAID_LOG_FILE", saved) }()
	}
	l, _ := c.OpenLogger()
	return l
}

// LogFile returns the effective log file path ("" = stderr).
func (c Config) LogFile() string {
	if v := os.Getenv("TAILRAID_LOG_FILE"); v != "" {
		return v
	}
	return c.Log.File
}

// The log file rotates at MaxLogSize keeping LogKeep generations (I-124): serve.log,
// serve.log.1 … serve.log.5, oldest dropped.
const (
	MaxLogSize = 64 << 20
	LogKeep    = 5
)

// LogFile is the open log.file. It rotates itself past its size and reopens on request
// (SIGHUP, after an external rotation), so a node that runs for months never fills a
// disk with its own log and never keeps writing into a file that was renamed away. A
// nil *LogFile stands for stderr and accepts every call.
type LogFile struct {
	mu      sync.Mutex
	path    string
	f       *os.File
	size    int64
	maxSize int64
	keep    int
}

func openLogFile(path string) (*LogFile, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, err
	}
	l := &LogFile{path: path, maxSize: MaxLogSize, keep: LogKeep}
	if err := l.openLocked(); err != nil {
		return nil, err
	}
	return l, nil
}

func (l *LogFile) openLocked() error {
	f, err := os.OpenFile(l.path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600) //nolint:gosec // log.file from the config
	if err != nil {
		return err
	}
	st, err := f.Stat()
	if err != nil {
		_ = f.Close()
		return err
	}
	l.f, l.size = f, st.Size()
	return nil
}

// Write appends p, rotating first when the file would grow past its size.
func (l *LogFile) Write(p []byte) (int, error) {
	if l == nil {
		return os.Stderr.Write(p)
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.f != nil && l.size > 0 && l.size+int64(len(p)) > l.maxSize {
		l.rotateLocked()
	}
	if l.f == nil { // a rotation or reopen failed: try again rather than drop the line
		if err := l.openLocked(); err != nil {
			return 0, err
		}
	}
	n, err := l.f.Write(p)
	l.size += int64(n)
	return n, err
}

// rotateLocked shifts serve.log.N to .N+1 (the oldest falls off), moves the current file
// to .1 and opens a fresh one.
func (l *LogFile) rotateLocked() {
	_ = l.f.Close()
	l.f = nil
	for i := l.keep - 1; i >= 1; i-- {
		_ = os.Rename(fmt.Sprintf("%s.%d", l.path, i), fmt.Sprintf("%s.%d", l.path, i+1))
	}
	_ = os.Rename(l.path, l.path+".1")
	_ = l.openLocked()
}

// Reopen closes and reopens the path: what an external rotation needs after it renamed
// the file (SIGHUP on Unix).
func (l *LogFile) Reopen() error {
	if l == nil {
		return nil
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.f != nil {
		_ = l.f.Close()
		l.f = nil
	}
	return l.openLocked()
}

// Close releases the file.
func (l *LogFile) Close() error {
	if l == nil {
		return nil
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.f == nil {
		return nil
	}
	err := l.f.Close()
	l.f = nil
	return err
}

// validateMesh checks the mesh keys (docs/20 §10): a UDP port, an endpoint of the form
// host:port, a subnet that is an IPv4 /24, an address with its subnet's bits.
func (c *Config) validateMesh() error {
	if _, err := c.MeshRelayRateBytes(); err != nil {
		return err
	}
	if _, err := c.RepairAvoidRelay(); err != nil {
		return err
	}
	if p := c.Mesh.Port; p < 1 || p > 65535 {
		return fmt.Errorf("config: mesh.port %d: want a UDP port", p)
	}
	if e := c.Mesh.Endpoint; e != "" {
		if _, _, err := net.SplitHostPort(e); err != nil {
			return fmt.Errorf("config: mesh.endpoint %q: want host:port", e)
		}
	}
	if s := c.Mesh.Subnet; s != "" {
		p, err := netip.ParsePrefix(s)
		if err != nil || !p.Addr().Is4() || p.Bits() != 24 {
			return fmt.Errorf("config: mesh.subnet %q: want an IPv4 /24", s)
		}
	}
	if a := c.Mesh.Address; a != "" {
		p, err := netip.ParsePrefix(a)
		if err != nil || !p.Addr().Is4() || p.Bits() != 24 {
			return fmt.Errorf("config: mesh.address %q: want an IPv4 address with its /24 (as init or join wrote it)", a)
		}
	}
	if f := c.Dev.MeshFake; f != "" && !c.Dev.Enabled {
		return errors.New("config: dev.mesh_fake needs dev.enabled")
	}
	if _, _, err := c.MountOwner(); err != nil {
		return err
	}
	return nil
}

// MountOwner is mount.owner as a uid and gid; ok is false when it is unset (the
// gateways then use the process's own).
func (c *Config) MountOwner() (uid, gid uint32, err error) {
	o := c.Mount.Owner
	if o == "" {
		return 0, 0, nil
	}
	u, g, found := strings.Cut(o, ":")
	if !found {
		return 0, 0, fmt.Errorf("config: mount.owner %q: want uid:gid", o)
	}
	ui, err1 := strconv.ParseUint(u, 10, 32)
	gi, err2 := strconv.ParseUint(g, 10, 32)
	if err1 != nil || err2 != nil {
		return 0, 0, fmt.Errorf("config: mount.owner %q: want uid:gid", o)
	}
	return uint32(ui), uint32(gi), nil
}

// SystemHome is the configuration directory of a service that runs as root: on macOS
// `/Library/Application Support/tailraid`, where a root daemon's `init` wrote (docs/20
// §9); elsewhere Home().
func SystemHome() string {
	if h := os.Getenv("TAILRAID_HOME"); h != "" {
		return h
	}
	if runtime.GOOS == "darwin" {
		return "/Library/Application Support/tailraid"
	}
	return Home()
}
