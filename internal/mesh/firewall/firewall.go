// Package firewall keeps the node's own chain first in the Linux filter table's INPUT
// chain, so a host firewall (ufw's default deny, tailscaled's ts-input, which drops every
// packet from 100.64/10 that did not arrive on tailscale0) never drops the mesh
// (docs/20 §9, I-334). tailscaled rebuilds its chains whenever it starts, so the keeper
// checks on every tick and puts its jump back on top.
package firewall

import (
	"context"
	"errors"
	"fmt"
	"net/netip"
	"os/exec"
	"slices"
	"strconv"
	"strings"
	"sync"
)

// Chain is the node's chain in iptables and ip6tables.
const Chain = "tailraid-input"

// Runner runs one command and returns its standard output.
type Runner func(ctx context.Context, name string, args ...string) (string, error)

// ExecRunner runs commands with os/exec, keeping their output for the error.
func ExecRunner(ctx context.Context, name string, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, name, args...) //nolint:gosec // the keeper's own fixed commands
	var stderr strings.Builder
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		return "", fmt.Errorf("%s %s: %w: %s", name, strings.Join(args, " "), err, strings.TrimSpace(stderr.String()))
	}
	return string(out), nil
}

// Keeper keeps Chain and the jump to it.
type Keeper struct {
	Iface string     // the mesh interface
	Addr  netip.Addr // the device's mesh address: what the host sends itself on lo
	Port  uint16     // the mesh's UDP port
	Run   Runner     // nil: ExecRunner

	mu     sync.Mutex
	status Status
}

// Status is the keeper as status reports it (docs/20 §10).
type Status struct {
	Backend string `json:"backend"`            // iptables, or none on a host without it
	Kept    bool   `json:"kept"`               // the last tick left the chain first
	TSInput bool   `json:"ts_input,omitempty"` // tailscaled's chain is in INPUT
	Err     string `json:"error,omitempty"`    // the last tick's failure
}

// table is one binary and the rules its chain holds.
type table struct {
	bin   string
	rules [][]string
}

func (k *Keeper) tables() []table {
	port := strconv.Itoa(int(k.Port))
	udp := []string{"-p", "udp", "-m", "udp", "--dport", port, "-j", "ACCEPT"}
	return []table{
		{bin: "iptables", rules: [][]string{
			{"-i", k.Iface, "-j", "ACCEPT"},
			{"-s", k.Addr.String() + "/32", "-i", "lo", "-j", "ACCEPT"},
			udp,
		}},
		{bin: "ip6tables", rules: [][]string{udp}},
	}
}

func (k *Keeper) run(ctx context.Context, bin string, args ...string) (string, error) {
	run := k.Run
	if run == nil {
		run = ExecRunner
	}
	return run(ctx, bin, append([]string{"-w"}, args...)...)
}

// Tick checks both tables and repairs what drifted: the chain's rules and a jump that
// is not the one first rule of INPUT. A missing binary is skipped; an error is
// reported and the next tick tries again.
func (k *Keeper) Tick(ctx context.Context) error {
	st := Status{Backend: "none"}
	var errs []error
	for _, t := range k.tables() {
		input, err := k.run(ctx, t.bin, "-S", "INPUT")
		if errors.Is(err, exec.ErrNotFound) {
			continue
		}
		if t.bin == "iptables" {
			st.Backend = "iptables"
		}
		if err == nil {
			st.TSInput = st.TSInput || strings.Contains(input, "-j ts-input")
			err = k.keep(ctx, t, input)
		}
		if err != nil {
			errs = append(errs, err)
		}
	}
	err := errors.Join(errs...)
	st.Kept = st.Backend == "iptables" && err == nil
	if err != nil {
		st.Err = err.Error()
	}
	k.mu.Lock()
	k.status = st
	k.mu.Unlock()
	return err
}

// keep repairs one table given its INPUT listing.
func (k *Keeper) keep(ctx context.Context, t table, input string) error {
	if !k.chainExact(ctx, t) {
		if _, err := k.run(ctx, t.bin, "-S", Chain); err != nil {
			if _, err := k.run(ctx, t.bin, "-N", Chain); err != nil {
				return err
			}
		} else if _, err := k.run(ctx, t.bin, "-F", Chain); err != nil {
			return err
		}
		for _, r := range t.rules {
			if _, err := k.run(ctx, t.bin, append([]string{"-A", Chain}, r...)...); err != nil {
				return err
			}
		}
	}
	first, jumps := jumpsIn(input)
	if first && jumps == 1 {
		return nil
	}
	for range jumps {
		if _, err := k.run(ctx, t.bin, "-D", "INPUT", "-j", Chain); err != nil {
			return err
		}
	}
	_, err := k.run(ctx, t.bin, "-I", "INPUT", "1", "-j", Chain)
	return err
}

// chainExact reports whether Chain holds exactly t's rules: as many rules as listed,
// each one present (-C matches a rule however iptables prints it).
func (k *Keeper) chainExact(ctx context.Context, t table) bool {
	out, err := k.run(ctx, t.bin, "-S", Chain)
	if err != nil {
		return false
	}
	n := 0
	for l := range strings.Lines(out) {
		if strings.HasPrefix(l, "-A ") {
			n++
		}
	}
	if n != len(t.rules) {
		return false
	}
	for _, r := range t.rules {
		if _, err := k.run(ctx, t.bin, append([]string{"-C", Chain}, r...)...); err != nil {
			return false
		}
	}
	return true
}

// jumpsIn reads an INPUT listing: whether its first rule is the jump to Chain, and how
// many jumps to Chain it holds.
func jumpsIn(input string) (first bool, jumps int) {
	var rules []string
	for l := range strings.Lines(input) {
		if l = strings.TrimSpace(l); strings.HasPrefix(l, "-A INPUT ") {
			rules = append(rules, strings.TrimPrefix(l, "-A INPUT "))
		}
	}
	jump := "-j " + Chain
	return len(rules) > 0 && rules[0] == jump, len(slices.DeleteFunc(rules, func(r string) bool { return r != jump }))
}

// Close removes the jumps and the chain from both tables; what fails is left behind.
func (k *Keeper) Close(ctx context.Context) {
	for _, t := range k.tables() {
		input, err := k.run(ctx, t.bin, "-S", "INPUT")
		if err != nil {
			continue
		}
		_, jumps := jumpsIn(input)
		for range jumps {
			_, _ = k.run(ctx, t.bin, "-D", "INPUT", "-j", Chain)
		}
		if _, err := k.run(ctx, t.bin, "-F", Chain); err == nil {
			_, _ = k.run(ctx, t.bin, "-X", Chain)
		}
	}
	k.mu.Lock()
	k.status.Kept = false
	k.mu.Unlock()
}

// Status reports the last tick.
func (k *Keeper) Status() Status {
	k.mu.Lock()
	defer k.mu.Unlock()
	return k.status
}
