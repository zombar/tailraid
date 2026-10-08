// Package tailscale reads the host's tailscaled through the `tailscale` CLI (docs/02,
// docs/08). The CLI hides the per-OS LocalAPI transport (unix socket, macOS
// sameuserproof TCP, Windows named pipe) and keeps the tailscale.com module tree out of
// the binary.
package tailscale

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/netip"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"
)

// Errors.
var (
	ErrNotFound    = errors.New("tailscale: no node for that address")
	ErrNotRunning  = errors.New("tailscale: backend is not running")
	ErrCLIMissing  = errors.New("tailscale: CLI not found")
	ErrNoSelfIPv4  = errors.New("tailscale: no IPv4 address on this node")
	ErrCLIFailed   = errors.New("tailscale: CLI failed")
	errBadResponse = errors.New("tailscale: unexpected CLI output")
)

// Client is the consumer-defined interface; CLI and Fake implement it.
type Client interface {
	Status(ctx context.Context) (*Status, error)
	WhoIs(ctx context.Context, ip netip.Addr) (*WhoIs, error)
}

// Status is the subset of `tailscale status --json` we use.
type Status struct {
	BackendState   string                `json:"BackendState"`
	TailscaleIPs   []netip.Addr          `json:"TailscaleIPs"`
	MagicDNSSuffix string                `json:"MagicDNSSuffix"`
	Self           PeerStatus            `json:"Self"`
	Peer           map[string]PeerStatus `json:"Peer"`
}

// PeerStatus describes one node in the status output.
type PeerStatus struct {
	ID           string       `json:"ID"` // StableID
	HostName     string       `json:"HostName"`
	DNSName      string       `json:"DNSName"`
	OS           string       `json:"OS"`
	TailscaleIPs []netip.Addr `json:"TailscaleIPs"`
	Online       bool         `json:"Online"`
	LastSeen     time.Time    `json:"LastSeen"`
	Tags         []string     `json:"Tags"`
}

// WhoIs is the subset of `tailscale whois --json <ip>` we use.
type WhoIs struct {
	Node struct {
		ID        int64          `json:"ID"`
		StableID  string         `json:"StableID"`
		Name      string         `json:"Name"`
		Addresses []netip.Prefix `json:"Addresses"`
		Tags      []string       `json:"Tags"`
		Hostinfo  struct {
			OS       string `json:"OS"`
			Hostname string `json:"Hostname"`
		} `json:"Hostinfo"`
	} `json:"Node"`
	UserProfile struct {
		LoginName   string `json:"LoginName"`
		DisplayName string `json:"DisplayName"`
	} `json:"UserProfile"`
}

// Login is the tailnet user behind a WhoIs answer, or "" when there is none. Tailscale
// reports the pseudo-login "tagged-devices" for every tagged node, so a tagged answer has
// no login: two tagged servers must never match each other under a same-user rule (docs/13).
func (w *WhoIs) Login() string {
	if len(w.Node.Tags) > 0 {
		return ""
	}
	return w.UserProfile.LoginName
}

// IPv4 returns a node's Tailscale IPv4 address.
func (p PeerStatus) IPv4() (netip.Addr, bool) {
	for _, a := range p.TailscaleIPs {
		if a.Is4() {
			return a, true
		}
	}
	return netip.Addr{}, false
}

// SelfIPv4 returns this node's Tailscale IPv4 address from a status.
func SelfIPv4(st *Status) (netip.Addr, error) {
	if st.BackendState != "Running" {
		return netip.Addr{}, fmt.Errorf("%w: state %q", ErrNotRunning, st.BackendState)
	}
	if ip, ok := st.Self.IPv4(); ok {
		return ip, nil
	}
	for _, a := range st.TailscaleIPs {
		if a.Is4() {
			return a, nil
		}
	}
	return netip.Addr{}, ErrNoSelfIPv4
}

// CLI runs the tailscale binary.
type CLI struct {
	Path    string
	Timeout time.Duration // per invocation; default 10 s
}

var _ Client = (*CLI)(nil)

// Find locates the tailscale CLI: an explicit override, then $PATH, then the per-OS
// install locations (the macOS app bundles do not add it to $PATH).
func Find(override string) (string, error) {
	if override != "" {
		if _, err := os.Stat(override); err != nil {
			return "", fmt.Errorf("%w: %s", ErrCLIMissing, override)
		}
		return override, nil
	}
	if p, err := exec.LookPath("tailscale"); err == nil {
		return p, nil
	}
	var candidates []string
	switch runtime.GOOS {
	case "darwin":
		candidates = []string{
			"/Applications/Tailscale.app/Contents/MacOS/Tailscale",
			"/usr/local/bin/tailscale",
			"/opt/homebrew/bin/tailscale",
		}
	case "windows":
		candidates = []string{
			filepath.Join(os.Getenv("ProgramFiles"), "Tailscale", "tailscale.exe"),
			`C:\Program Files\Tailscale\tailscale.exe`,
		}
	default:
		candidates = []string{"/usr/bin/tailscale", "/usr/local/bin/tailscale", "/usr/sbin/tailscale"}
	}
	for _, c := range candidates {
		if _, err := os.Stat(c); err == nil {
			return c, nil
		}
	}
	return "", ErrCLIMissing
}

func (c *CLI) run(ctx context.Context, args ...string) ([]byte, error) {
	timeout := c.Timeout
	if timeout == 0 {
		timeout = 10 * time.Second
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, c.Path, args...)
	cmd.WaitDelay = 500 * time.Millisecond // do not hang on grandchildren holding the pipes
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Run(); err != nil {
		msg := strings.TrimSpace(stderr.String())
		if msg == "" {
			msg = strings.TrimSpace(stdout.String())
		}
		return stdout.Bytes(), fmt.Errorf("%w: %s %s: %w: %s", ErrCLIFailed, filepath.Base(c.Path), strings.Join(args, " "), err, msg)
	}
	return stdout.Bytes(), nil
}

// Status runs `tailscale status --json`.
func (c *CLI) Status(ctx context.Context) (*Status, error) {
	out, err := c.run(ctx, "status", "--json")
	if err != nil {
		return nil, err
	}
	var st Status
	if err := json.Unmarshal(out, &st); err != nil {
		return nil, fmt.Errorf("%w: status: %w", errBadResponse, err)
	}
	return &st, nil
}

// WhoIs runs `tailscale whois --json <ip>`. An unknown address is ErrNotFound.
func (c *CLI) WhoIs(ctx context.Context, ip netip.Addr) (*WhoIs, error) {
	out, err := c.run(ctx, "whois", "--json", ip.String())
	if err != nil {
		if errors.Is(err, ErrCLIFailed) {
			return nil, fmt.Errorf("%w: %s", ErrNotFound, ip)
		}
		return nil, err
	}
	var w WhoIs
	if err := json.Unmarshal(out, &w); err != nil {
		return nil, fmt.Errorf("%w: whois: %w", errBadResponse, err)
	}
	if w.Node.StableID == "" {
		return nil, fmt.Errorf("%w: %s", ErrNotFound, ip)
	}
	return &w, nil
}

// Fake is an in-memory Client for tests and `--dev`.
type Fake struct {
	St    Status
	Nodes map[netip.Addr]WhoIs
	Err   error // returned by every call when set
}

var _ Client = (*Fake)(nil)

// NewFake builds a running tailnet with the given self address and peers (addr → stable ID).
func NewFake(self netip.Addr, selfID string, peers map[netip.Addr]string) *Fake {
	f := &Fake{Nodes: map[netip.Addr]WhoIs{}}
	f.St = Status{BackendState: "Running", TailscaleIPs: []netip.Addr{self}, MagicDNSSuffix: "fake.ts.net",
		Self: PeerStatus{ID: selfID, HostName: "self", TailscaleIPs: []netip.Addr{self}, Online: true}, Peer: map[string]PeerStatus{}}
	f.Add(self, selfID, "self")
	for addr, id := range peers {
		f.Add(addr, id, "peer-"+id)
	}
	return f
}

// Add registers a node.
func (f *Fake) Add(addr netip.Addr, stableID, host string) {
	var w WhoIs
	w.Node.StableID = stableID
	w.Node.Name = host + ".fake.ts.net."
	w.Node.Addresses = []netip.Prefix{netip.PrefixFrom(addr, addr.BitLen())}
	w.Node.Hostinfo.Hostname = host
	f.Nodes[addr] = w
	if addr != f.St.Self.TailscaleIPs[0] {
		f.St.Peer["nodekey:"+stableID] = PeerStatus{ID: stableID, HostName: host, TailscaleIPs: []netip.Addr{addr}, Online: true}
	}
}

// SetUser sets the login name and tags WhoIs reports for a known node; unknown addresses
// are ignored.
func (f *Fake) SetUser(addr netip.Addr, login string, tags ...string) {
	w, ok := f.Nodes[addr]
	if !ok {
		return
	}
	w.UserProfile.LoginName = login
	w.Node.Tags = tags
	f.Nodes[addr] = w
}

// Remove forgets a node (re-auth or deletion).
func (f *Fake) Remove(addr netip.Addr) {
	if w, ok := f.Nodes[addr]; ok {
		delete(f.St.Peer, "nodekey:"+w.Node.StableID)
	}
	delete(f.Nodes, addr)
}

func (f *Fake) Status(context.Context) (*Status, error) {
	if f.Err != nil {
		return nil, f.Err
	}
	st := f.St
	return &st, nil
}

func (f *Fake) WhoIs(_ context.Context, ip netip.Addr) (*WhoIs, error) {
	if f.Err != nil {
		return nil, f.Err
	}
	w, ok := f.Nodes[ip]
	if !ok {
		return nil, fmt.Errorf("%w: %s", ErrNotFound, ip)
	}
	return &w, nil
}

// LoadFake reads a Fake from a JSON file of the form {"status": <status>, "whois": {"<ip>": <whois>}}
// for `--dev` runs without a tailnet (docker-compose soak).
func LoadFake(path string) (*Fake, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var raw struct {
		Status Status           `json:"status"`
		WhoIs  map[string]WhoIs `json:"whois"`
	}
	if err := json.Unmarshal(b, &raw); err != nil {
		return nil, fmt.Errorf("tailscale: fake file %s: %w", path, err)
	}
	f := &Fake{St: raw.Status, Nodes: map[netip.Addr]WhoIs{}}
	for s := range raw.WhoIs {
		ip, err := netip.ParseAddr(s)
		if err != nil {
			return nil, fmt.Errorf("tailscale: fake file %s: bad address %q", path, s)
		}
		f.Nodes[ip] = raw.WhoIs[s]
	}
	return f, nil
}
