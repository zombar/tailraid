// Package config loads the node configuration (docs/02, docs/07).
package config

import (
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/goccy/go-yaml"
)

// Config is the YAML node configuration. Volume policy (k, m, chunk size) is not here:
// it lives in the replicated metadata and is set by `init`.
type Config struct {
	Node struct {
		Name     string `yaml:"name"`
		DataDir  string `yaml:"data_dir"`
		Capacity string `yaml:"capacity"` // e.g. "500G"; shard-byte budget and placement weight
		Voter    bool   `yaml:"voter"`
	} `yaml:"node"`
	Listen struct {
		Raft int    `yaml:"raft"` // tailnet TCP port
		RPC  int    `yaml:"rpc"`  // tailnet TCP port
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
	} `yaml:"log"`
	Dev struct {
		Enabled       bool   `yaml:"enabled"`
		TailscaleFake string `yaml:"tailscale_fake"` // JSON fixture replacing the tailscale CLI
	} `yaml:"dev"`
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

// Default returns the defaults for this OS.
func Default() Config {
	var c Config
	host, _ := os.Hostname()
	c.Node.Name = host
	c.Node.DataDir = filepath.Join(Home(), "data")
	c.Node.Capacity = "100G"
	c.Listen.Raft = 20480
	c.Listen.RPC = 20481
	c.Listen.NFS = "127.0.0.1:20490"
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
	b, err := os.ReadFile(path)
	if err != nil {
		return c, err
	}
	if err := yaml.UnmarshalWithOptions(b, &c, yaml.Strict()); err != nil {
		return c, fmt.Errorf("config %s: %w", path, err)
	}
	return c, c.Validate()
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
	if _, err := ParseBytes(c.Node.Capacity); err != nil {
		return fmt.Errorf("config: node.capacity: %w", err)
	}
	return nil
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
	mult := map[string]float64{"": 1, "B": 1, "K": 1 << 10, "KB": 1 << 10, "KIB": 1 << 10, "M": 1 << 20, "MB": 1 << 20, "MIB": 1 << 20,
		"G": 1 << 30, "GB": 1 << 30, "GIB": 1 << 30, "T": 1 << 40, "TB": 1 << 40, "TIB": 1 << 40}
	m, ok := mult[upper(unit)]
	if !ok || num < 0 {
		return 0, fmt.Errorf("bad size %q", s)
	}
	return uint64(num * m), nil
}

func upper(s string) string {
	b := []byte(s)
	for i, c := range b {
		if c >= 'a' && c <= 'z' {
			b[i] = c - 32
		}
	}
	return string(b)
}

// Logger builds the process logger from config and environment. Output goes to stderr,
// which service managers and Docker capture.
func (c Config) Logger() *slog.Logger {
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
	case "warn", "warning":
		lvl = slog.LevelWarn
	case "error":
		lvl = slog.LevelError
	default:
		lvl = slog.LevelInfo
	}
	opts := &slog.HandlerOptions{Level: lvl}
	if strings.EqualFold(format, "json") {
		return slog.New(slog.NewJSONHandler(os.Stderr, opts))
	}
	return slog.New(slog.NewTextHandler(os.Stderr, opts))
}
