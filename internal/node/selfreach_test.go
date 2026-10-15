package node

import (
	"context"
	"errors"
	"log/slog"
	"net"
	"net/netip"
	"strings"
	"sync"
	"testing"

	"github.com/zombar/tailraid/internal/config"
	"github.com/zombar/tailraid/internal/mesh/firewall"
)

// The self-check reports a listening address as reachable and a refused or dropped one
// as not, with the cause the host's firewall state explains.
// Invariant: I-335
func TestSelfReach_Table(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	wg.Go(func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			_ = c.Close()
		}
	})
	defer wg.Wait()
	defer ln.Close()
	closed, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	closedAddr := closed.Addr().String()
	_ = closed.Close()

	log := slog.New(slog.DiscardHandler)
	var s selfCheck
	if s.report() != nil {
		t.Fatal("a report before the first check")
	}
	s.check(context.Background(), ln.Addr().String(), func() string { return "cause" }, log)
	if r := s.report(); r == nil || !r.OK || r.Err != "" || r.Cause != "" {
		t.Errorf("listening: %+v", r)
	}
	s.check(context.Background(), closedAddr, func() string { return "cause" }, log)
	if r := s.report(); r == nil || r.OK || r.Err == "" || r.Cause != "cause" {
		t.Errorf("refused: %+v", r)
	}
	s.dial = func(ctx context.Context, _ string) error { <-ctx.Done(); return ctx.Err() } // a firewall that drops
	s.check(context.Background(), "100.79.112.1:20481", func() string { return "dropped" }, log)
	if r := s.report(); r == nil || r.OK || !strings.Contains(r.Err, "deadline") || r.Cause != "dropped" {
		t.Errorf("dropped: %+v", r)
	}

	for _, tc := range []struct {
		name string
		goos string
		on   bool
		fw   *firewall.Status
		want string
	}{
		{"mac", "darwin", true, nil, "UDP port 20483"},
		{"off", "linux", false, nil, "mesh.firewall is off"},
		{"no iptables", "linux", true, &firewall.Status{Backend: "none"}, "firewall-cmd --add-port=20483/udp"},
		{"tailscale, not kept", "linux", true, &firewall.Status{Backend: "iptables", TSInput: true, Err: "denied"}, "ts-input"},
		{"not kept", "linux", true, &firewall.Status{Backend: "iptables", Err: "denied"}, "could not be kept (denied)"},
		{"kept", "linux", true, &firewall.Status{Backend: "iptables", Kept: true}, "another layer"},
	} {
		if got := selfCause(tc.goos, tc.on, "tailraid0", 20483, tc.fw); !strings.Contains(got, tc.want) {
			t.Errorf("%s: cause %q lacks %q", tc.name, got, tc.want)
		}
	}
}

// A run of failed checks logs one error, the way back one line, and a run of good
// checks nothing.
// Invariant: I-335
func TestSelfReach_LogsOncePerChange(t *testing.T) {
	h := &countHandler{}
	log := slog.New(h)
	fail := errors.New("i/o timeout")
	var cur error
	s := selfCheck{dial: func(context.Context, string) error { return cur }}
	for _, step := range []error{nil, nil, fail, fail, fail, nil, nil, fail} {
		cur = step
		s.check(context.Background(), "a:1", func() string { return "" }, log)
	}
	if h.errors != 2 || h.infos != 1 {
		t.Errorf("logged %d errors and %d infos, want 2 and 1", h.errors, h.infos)
	}
}

type countHandler struct {
	mu            sync.Mutex
	errors, infos int
}

func (h *countHandler) Enabled(context.Context, slog.Level) bool { return true }
func (h *countHandler) Handle(_ context.Context, r slog.Record) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	switch r.Level { //nolint:exhaustive // the check logs errors and infos only
	case slog.LevelError:
		h.errors++
	case slog.LevelInfo:
		h.infos++
	}
	return nil
}
func (h *countHandler) WithAttrs([]slog.Attr) slog.Handler { return h }
func (h *countHandler) WithGroup(string) slog.Handler      { return h }

// The keeper exists only on Linux with mesh.firewall on and a real device; elsewhere no
// firewall command can run.
// Invariant: I-334
func TestNewKeeper_OnlyLinuxAndOn(t *testing.T) {
	off := false
	nw := &network{ifname: "tailraid0", addr: netip.MustParsePrefix("100.79.112.1/24"), port: 20483}
	for _, tc := range []struct {
		name string
		goos string
		off  bool
		fake bool
		want bool
	}{
		{"linux", "linux", false, false, true},
		{"linux off", "linux", true, false, false},
		{"linux fake", "linux", false, true, false},
		{"darwin", "darwin", false, false, false},
		{"windows", "windows", false, false, false},
	} {
		cfg := config.Default()
		if tc.off {
			cfg.Mesh.Firewall = &off
		}
		nw.fake = tc.fake
		k := newKeeper(cfg, tc.goos, nw)
		if (k != nil) != tc.want {
			t.Errorf("%s: keeper %v, want %v", tc.name, k != nil, tc.want)
		}
		if k != nil && (k.Iface != "tailraid0" || k.Addr.String() != "100.79.112.1" || k.Port != 20483) {
			t.Errorf("%s: keeper %+v", tc.name, k)
		}
	}
}
