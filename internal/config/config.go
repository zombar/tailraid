// Package config loads the node configuration (docs/02, docs/07).
package config

import (
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"os"
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
		Name    string `yaml:"name"`
		DataDir string `yaml:"data_dir"`
		Voter   bool   `yaml:"voter"`
		Disks   []Disk `yaml:"disks"` // added disks, each a placement target of its own (docs/14)
	} `yaml:"node"`
	Listen struct {
		Raft int    `yaml:"raft"` // tailnet TCP port
		RPC  int    `yaml:"rpc"`  // tailnet TCP port
		Web  int    `yaml:"web"`  // tailnet TCP port of the browser UI (docs/13); 0 disables it
		NFS  string `yaml:"nfs"`  // loopback host:port
	} `yaml:"listen"`
	Mount struct {
		Path string `yaml:"path"`
	} `yaml:"mount"`
	Tailscale struct {
		CLI string `yaml:"cli"` // path override for the tailscale binary
	} `yaml:"tailscale"`
	Log struct {
		Level  string `yaml:"level"`  // debug | info | warn | error (env TAILRAID_LOG_LEVEL overrides)
		Format string `yaml:"format"` // text | json (env TAILRAID_LOG_FORMAT overrides)
		File   string `yaml:"file"`   // append here instead of stderr (env TAILRAID_LOG_FILE overrides)
	} `yaml:"log"`
	Repair struct {
		Grace     string `yaml:"grace"`      // GC grace and shard DELETE fence age (default 10m)
		FillAfter string `yaml:"fill_after"` // dead-node hold-down before re-placing its slots (default 10m)
	} `yaml:"repair"`
	Dev struct {
		Enabled       bool   `yaml:"enabled"`
		TailscaleFake string `yaml:"tailscale_fake"` // JSON fixture replacing the tailscale CLI
	} `yaml:"dev"`
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

func errBadDuration(err error) error {
	if err != nil {
		return err
	}
	return errors.New("must be positive")
}

// Home returns the per-OS configuration directory, or $TAILRAID_HOME.
func Home() string {
	if h := os.Getenv("TAILRAID_HOME"); h != "" {
		return h
	}
	switch runtime.GOOS {
	case "darwin":
		home, _ := os.UserHomeDir()
		return filepath.Join(home, "Library", "Application Support", "tailraid")
	case "windows":
		if pd := os.Getenv("ProgramData"); pd != "" {
			return filepath.Join(pd, "tailraid")
		}
		return `C:\ProgramData\tailraid`
	default:
		if x := os.Getenv("XDG_CONFIG_HOME"); x != "" {
			return filepath.Join(x, "tailraid")
		}
		home, _ := os.UserHomeDir()
		return filepath.Join(home, ".config", "tailraid")
	}
}

// DefaultPath is the config file location.
func DefaultPath() string { return filepath.Join(Home(), "config.yaml") }

// The listeners' defaults (docs/08 §Loopback-only listeners, docs/12).
const (
	DefaultRaftPort = 20480
	DefaultRPCPort  = 20481
	DefaultWebPort  = 20482
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
	c.Listen.NFS = DefaultNFSAddr
	c.Log.Level, c.Log.Format = "info", "text"
	home, _ := os.UserHomeDir()
	c.Mount.Path = filepath.Join(home, "TailRAID")
	if runtime.GOOS == "windows" {
		c.Listen.NFS = "127.0.0.1:2049"
		c.Mount.Path = "T:"
	}
	return c
}

// Load reads a config file strictly (unknown keys are errors) over the defaults.
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
	if c.Listen.NFS == "" {
		return errors.New("config: listen.nfs is required")
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
	if w := c.Listen.Web; w < 0 || w > 65535 {
		return fmt.Errorf("config: listen.web %d: want 0 (off) or a TCP port", w)
	} else if w != 0 && (w == c.Listen.RPC || w == c.Listen.Raft) {
		return fmt.Errorf("config: listen.web %d must differ from listen.rpc and listen.raft", w)
	}
	// the rest of I-126: every port in range and distinct, known log settings, an
	// absolute mount path
	_, nfsPort, err := net.SplitHostPort(c.Listen.NFS)
	if err != nil {
		return fmt.Errorf("config: listen.nfs %q: want host:port", c.Listen.NFS)
	}
	np, err := strconv.Atoi(nfsPort)
	if err != nil || np < 1 || np > 65535 {
		return fmt.Errorf("config: listen.nfs %q: want a TCP port", c.Listen.NFS)
	}
	used := map[int]string{}
	for _, l := range []struct {
		name string
		port int
	}{{"listen.raft", c.Listen.Raft}, {"listen.rpc", c.Listen.RPC}, {"listen.nfs", np}, {"listen.web", c.Listen.Web}} {
		if l.name == "listen.web" && l.port == 0 {
			continue
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
	return nil
}

// Warnings lists what serve should say about the config file at path and the data
// directory before it runs (I-126): a mode that lets other users of the machine read
// the join token or the shards. Windows has no mode bits worth reading.
func (c Config) Warnings(path string) []string {
	if runtime.GOOS == "windows" {
		return nil
	}
	var out []string
	if st, err := os.Stat(path); err == nil && st.Mode().Perm()&0o077 != 0 {
		out = append(out, fmt.Sprintf("config file %s is readable by other users (mode %04o): chmod 600 %s", path, st.Mode().Perm(), path))
	}
	if st, err := os.Stat(c.Node.DataDir); err == nil && st.Mode().Perm()&0o077 != 0 {
		out = append(out, fmt.Sprintf("data directory %s is accessible to other users (mode %04o): chmod 700 %s", c.Node.DataDir, st.Mode().Perm(), c.Node.DataDir))
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

// Logger is OpenLogger without the closer: the log file stays open for the process.
func (c Config) Logger() *slog.Logger {
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
