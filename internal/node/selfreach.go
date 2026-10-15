package node

import (
	"context"
	"fmt"
	"log/slog"
	"net"
	"sync"
	"time"

	"github.com/zombar/tailraid/internal/admin"
	"github.com/zombar/tailraid/internal/config"
	"github.com/zombar/tailraid/internal/mesh/firewall"
)

// selfDialTimeout bounds the node's dial to its own mesh address.
const selfDialTimeout = 2 * time.Second

// selfCheck dials the node's own RPC listener at its mesh address on every probe tick
// (docs/20 §9, I-335): a host firewall that drops the mesh (tailscaled's ts-input
// dropping 100.64/10 from anything but tailscale0) shows here first, since the CLI and
// every peer reach the node the same way.
type selfCheck struct {
	dial func(ctx context.Context, addr string) error // nil: a TCP dial
	mu   sync.Mutex
	rep  *admin.SelfReach
}

func tcpDial(ctx context.Context, addr string) error {
	d := net.Dialer{Timeout: selfDialTimeout}
	c, err := d.DialContext(ctx, "tcp", addr)
	if err != nil {
		return err
	}
	return c.Close()
}

// check dials addr and records the outcome; a change to unreachable is logged once as
// an error with the cause, and the way back once.
func (s *selfCheck) check(ctx context.Context, addr string, cause func() string, log *slog.Logger) {
	dial := s.dial
	if dial == nil {
		dial = tcpDial
	}
	ctx, cancel := context.WithTimeout(ctx, selfDialTimeout)
	err := dial(ctx, addr)
	cancel()
	rep := &admin.SelfReach{OK: err == nil}
	if err != nil {
		rep.Err, rep.Cause = err.Error(), cause()
	}
	s.mu.Lock()
	prev := s.rep
	s.rep = rep
	s.mu.Unlock()
	switch {
	case err != nil && (prev == nil || prev.OK):
		log.Error("this node cannot reach its own mesh address: peers and the CLI cannot reach it either", "addr", addr, "err", err, "cause", rep.Cause)
	case err == nil && prev != nil && !prev.OK:
		log.Info("own mesh address reachable again", "addr", addr)
	}
}

// report is the last outcome, nil before the first check.
func (s *selfCheck) report() *admin.SelfReach {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.rep == nil {
		return nil
	}
	r := *s.rep
	return &r
}

// selfCause explains an own address the node cannot reach from what it knows of the
// host's firewall (docs/20 §9): fw is nil off Linux or with mesh.firewall off.
func selfCause(goos string, on bool, ifname string, port uint16, fw *firewall.Status) string {
	let := fmt.Sprintf("let %s and UDP port %d through the host firewall", ifname, port)
	switch {
	case goos != "linux":
		return "a host firewall may be dropping the mesh: " + let + " (docs/20 §9)"
	case !on:
		return "mesh.firewall is off: " + let + ", or set mesh.firewall: true (docs/20 §9)"
	case fw == nil:
		return "a host firewall may be dropping the mesh (docs/20 §9)"
	case fw.Backend == "none":
		return fmt.Sprintf("this host has no iptables command, so the node keeps no chain: %s (nftables; firewalld: firewall-cmd --add-port=%d/udp and --zone=trusted --add-interface=%s) (docs/20 §9)", let, port, ifname)
	case !fw.Kept && fw.TSInput:
		return "tailscaled's ts-input drops every packet from 100.64/10 not arriving on tailscale0, and the node's chain ahead of it could not be kept (" + fw.Err + ") (docs/20 §9)"
	case !fw.Kept:
		return "the node's firewall chain could not be kept (" + fw.Err + ") (docs/20 §9)"
	}
	return "the node's chain is first in iptables INPUT, so another layer drops the mesh (firewalld or an nftables table of its own): " + let + " (docs/20 §9)"
}

// newKeeper is the firewall keeper of a Linux device with mesh.firewall on; nil
// elsewhere, and then no firewall command ever runs (I-334).
func newKeeper(cfg config.Config, goos string, nw *network) *firewall.Keeper {
	if goos != "linux" || !cfg.MeshFirewall() || nw.fake {
		return nil
	}
	return &firewall.Keeper{Iface: nw.ifname, Addr: nw.addr.Addr(), Port: nw.port}
}
