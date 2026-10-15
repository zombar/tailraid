package firewall

import (
	"context"
	"errors"
	"fmt"
	"net/netip"
	"os/exec"
	"slices"
	"strings"
	"testing"
)

// fakeTables is iptables and ip6tables over the filter table, enough of them for the
// commands the keeper runs: -S, -C, -N, -F, -X, -A, -I 1 and -D, each rule kept as its
// argument string.
type fakeTables struct {
	chains  map[string]map[string][]string // binary → chain → rules
	ran     []string
	absent  map[string]bool // binaries not installed
	failArg string          // a command containing it fails
}

func newFake(input ...string) *fakeTables {
	f := &fakeTables{chains: map[string]map[string][]string{}, absent: map[string]bool{}}
	for _, bin := range []string{"iptables", "ip6tables"} {
		f.chains[bin] = map[string][]string{"INPUT": slices.Clone(input)}
	}
	return f
}

func (f *fakeTables) run(_ context.Context, name string, args ...string) (string, error) {
	line := name + " " + strings.Join(args, " ")
	f.ran = append(f.ran, line)
	if f.absent[name] {
		return "", fmt.Errorf("%s: %w", name, exec.ErrNotFound)
	}
	if f.failArg != "" && strings.Contains(line, f.failArg) {
		return "", errors.New("refused")
	}
	if len(args) < 3 || args[0] != "-w" {
		return "", fmt.Errorf("unexpected %q", line)
	}
	t := f.chains[name]
	op, chain, rest := args[1], args[2], strings.Join(args[3:], " ")
	rules, exists := t[chain]
	switch op {
	case "-S":
		if !exists {
			return "", errors.New("no chain")
		}
		var b strings.Builder
		if chain == "INPUT" {
			b.WriteString("-P INPUT DROP\n")
		} else {
			b.WriteString("-N " + chain + "\n")
		}
		for _, r := range rules {
			b.WriteString("-A " + chain + " " + r + "\n")
		}
		return b.String(), nil
	case "-C":
		if exists && slices.Contains(rules, rest) {
			return "", nil
		}
		return "", errors.New("bad rule")
	case "-N":
		if exists {
			return "", errors.New("chain exists")
		}
		t[chain] = nil
	case "-F":
		if !exists {
			return "", errors.New("no chain")
		}
		t[chain] = nil
	case "-X":
		if !exists {
			return "", errors.New("no chain")
		}
		delete(t, chain)
	case "-A":
		t[chain] = append(rules, rest)
	case "-I":
		rest = strings.Join(args[4:], " ") // -I INPUT 1 …
		t[chain] = append([]string{rest}, rules...)
	case "-D":
		i := slices.Index(rules, rest)
		if i < 0 {
			return "", errors.New("no such rule")
		}
		t[chain] = slices.Delete(rules, i, i+1)
	default:
		return "", fmt.Errorf("unexpected %q", line)
	}
	return "", nil
}

func (f *fakeTables) mutations() []string {
	var out []string
	for _, l := range f.ran {
		if !strings.Contains(l, " -S ") && !strings.Contains(l, " -C ") {
			out = append(out, l)
		}
	}
	return out
}

func keeper(f *fakeTables) *Keeper {
	return &Keeper{Iface: "tailraid0", Addr: netip.MustParseAddr("100.79.112.1"), Port: 20483, Run: f.run}
}

var (
	wantV4 = []string{"-i tailraid0 -j ACCEPT", "-s 100.79.112.1/32 -i lo -j ACCEPT", "-p udp -m udp --dport 20483 -j ACCEPT"}
	wantV6 = []string{"-p udp -m udp --dport 20483 -j ACCEPT"}
)

func checkKept(t *testing.T, f *fakeTables) {
	t.Helper()
	for bin, want := range map[string][]string{"iptables": wantV4, "ip6tables": wantV6} {
		in := f.chains[bin]["INPUT"]
		if len(in) == 0 || in[0] != "-j "+Chain {
			t.Errorf("%s INPUT does not start with the jump: %q", bin, in)
		}
		if n := slices.Index(in[1:], "-j "+Chain); n >= 0 {
			t.Errorf("%s INPUT holds the jump twice: %q", bin, in)
		}
		if got := f.chains[bin][Chain]; !slices.Equal(got, want) {
			t.Errorf("%s %s = %q, want %q", bin, Chain, got, want)
		}
	}
}

// Behind tailscaled's ts-input and ufw, the keeper puts its own chain first, holding
// exactly the mesh interface, the host's own mesh address on lo and the UDP port.
// Invariant: I-334
func TestFirewall_InstallsChainFirst(t *testing.T) {
	f := newFake("-j ts-input", "-j ufw-before-input")
	k := keeper(f)
	if err := k.Tick(context.Background()); err != nil {
		t.Fatal(err)
	}
	checkKept(t, f)
	if got := f.chains["iptables"]["INPUT"]; !slices.Equal(got, []string{"-j " + Chain, "-j ts-input", "-j ufw-before-input"}) {
		t.Errorf("INPUT = %q: the other rules must stay, in order", got)
	}
	st := k.Status()
	if !st.Kept || st.Err != "" || !st.TSInput {
		t.Errorf("status %+v, want kept with ts-input seen", st)
	}
	f.ran = nil
	if err := k.Tick(context.Background()); err != nil {
		t.Fatal(err)
	}
	if m := f.mutations(); len(m) != 0 {
		t.Errorf("a kept chain is rewritten on the next tick: %q", m)
	}
}

// tailscaled rebuilds its chains when it starts and inserts its jump at the top; the
// next tick puts ours back ahead of it, once.
// Invariant: I-334
func TestFirewall_TickPutsJumpBackOnTop(t *testing.T) {
	f := newFake("-j ts-input")
	k := keeper(f)
	if err := k.Tick(context.Background()); err != nil {
		t.Fatal(err)
	}
	for _, bin := range []string{"iptables", "ip6tables"} {
		in := f.chains[bin]["INPUT"]
		f.chains[bin]["INPUT"] = append([]string{"-j ts-input"}, slices.DeleteFunc(slices.Clone(in), func(r string) bool { return r == "-j ts-input" })...)
	}
	if err := k.Tick(context.Background()); err != nil {
		t.Fatal(err)
	}
	checkKept(t, f)

	// a jump of ours left further down (two copies) is reduced to the one at the top
	f.chains["iptables"]["INPUT"] = []string{"-j ts-input", "-j " + Chain, "-j ufw", "-j " + Chain}
	if err := k.Tick(context.Background()); err != nil {
		t.Fatal(err)
	}
	checkKept(t, f)
	if got := f.chains["iptables"]["INPUT"]; !slices.Equal(got, []string{"-j " + Chain, "-j ts-input", "-j ufw"}) {
		t.Errorf("INPUT = %q", got)
	}
}

// A chain someone emptied, edited or deleted is written again exactly.
// Invariant: I-334
func TestFirewall_ChainDriftRewritten(t *testing.T) {
	for name, drift := range map[string]func(f *fakeTables){
		"flushed": func(f *fakeTables) { f.chains["iptables"][Chain] = nil },
		"extra":   func(f *fakeTables) { f.chains["iptables"][Chain] = append(f.chains["iptables"][Chain], "-j DROP") },
		"missing": func(f *fakeTables) { f.chains["iptables"][Chain] = f.chains["iptables"][Chain][1:] },
		"deleted": func(f *fakeTables) {
			delete(f.chains["ip6tables"], Chain)
			f.chains["ip6tables"]["INPUT"] = f.chains["ip6tables"]["INPUT"][1:]
		},
	} {
		t.Run(name, func(t *testing.T) {
			f := newFake("-j ts-input")
			k := keeper(f)
			if err := k.Tick(context.Background()); err != nil {
				t.Fatal(err)
			}
			drift(f)
			if err := k.Tick(context.Background()); err != nil {
				t.Fatal(err)
			}
			checkKept(t, f)
		})
	}
}

// Shutdown removes the jump and the chain from both tables and leaves the rest.
// Invariant: I-334
func TestFirewall_CloseRemoves(t *testing.T) {
	f := newFake("-j ts-input")
	k := keeper(f)
	if err := k.Tick(context.Background()); err != nil {
		t.Fatal(err)
	}
	k.Close(context.Background())
	for _, bin := range []string{"iptables", "ip6tables"} {
		if got := f.chains[bin]["INPUT"]; !slices.Equal(got, []string{"-j ts-input"}) {
			t.Errorf("%s INPUT after close = %q", bin, got)
		}
		if _, ok := f.chains[bin][Chain]; ok {
			t.Errorf("%s still holds %s", bin, Chain)
		}
	}
	if k.Status().Kept {
		t.Error("closed keeper reports kept")
	}
}

// A host without iptables gets nothing run but the lookups, and says so; without
// ip6tables the IPv4 chain is still kept.
// Invariant: I-334
func TestFirewall_NoIptablesNothingRuns(t *testing.T) {
	f := newFake()
	f.absent["iptables"], f.absent["ip6tables"] = true, true
	k := keeper(f)
	if err := k.Tick(context.Background()); err != nil {
		t.Fatalf("no iptables is not an error: %v", err)
	}
	k.Close(context.Background())
	if m := f.mutations(); len(m) != 0 {
		t.Errorf("ran %q on a host without iptables", m)
	}
	if st := k.Status(); st.Kept || st.Backend != "none" {
		t.Errorf("status %+v, want none", st)
	}

	f = newFake()
	f.absent["ip6tables"] = true
	k = keeper(f)
	if err := k.Tick(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := f.chains["iptables"][Chain]; !slices.Equal(got, wantV4) {
		t.Errorf("iptables chain %q", got)
	}
	if !k.Status().Kept {
		t.Error("the IPv4 chain is kept without ip6tables")
	}
}

// A refused command is an error and a status, never a panic, and the next tick tries
// again.
// Invariant: I-334
func TestFirewall_FailureIsReported(t *testing.T) {
	f := newFake("-j ts-input")
	f.failArg = "-I INPUT"
	k := keeper(f)
	err := k.Tick(context.Background())
	if err == nil {
		t.Fatal("a refused insert is not reported")
	}
	if st := k.Status(); st.Kept || !strings.Contains(st.Err, "refused") {
		t.Errorf("status %+v", st)
	}
	f.failArg = ""
	if err := k.Tick(context.Background()); err != nil {
		t.Fatal(err)
	}
	checkKept(t, f)
	if st := k.Status(); !st.Kept || st.Err != "" {
		t.Errorf("status after recovery %+v", st)
	}
}
