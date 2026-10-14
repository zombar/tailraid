// Package portmap asks the default gateway for a mapping of the mesh's UDP port, PCP
// (RFC 6887) first and NAT-PMP (RFC 6886) next, so a device behind an ordinary router is
// reachable directly from anywhere (docs/20 §7, I-330). The clients and the responder
// used by the tests and the NAT rig are written after tunnelmesh's internal/portmap.
package portmap

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"time"
)

// The protocol port both PCP and NAT-PMP answer on.
const Port = 5351

// Mapping is one mapping a gateway granted.
type Mapping struct {
	Protocol   string     // "pcp" or "natpmp"
	Client     netip.Addr // the host mapped, as the responder saw it (zero on the client's side)
	Gateway    netip.Addr
	ExternalIP netip.Addr
	Internal   uint16
	External   uint16
	Lifetime   time.Duration
	Epoch      uint32 // the gateway's seconds since its mappings were last lost
}

// Endpoint is the mapping's external endpoint.
func (m Mapping) Endpoint() netip.AddrPort { return netip.AddrPortFrom(m.ExternalIP, m.External) }

// Client speaks one protocol to one gateway.
type Client interface {
	Name() string
	// Probe asks the gateway whether it answers: its external address and epoch.
	Probe(ctx context.Context) (external netip.Addr, epoch uint32, err error)
	// Map asks for a UDP mapping of the internal port, suggesting an external port (0: any)
	// and a lifetime; the gateway may grant other values.
	Map(ctx context.Context, internal, suggested uint16, lifetime time.Duration) (Mapping, error)
	// Release gives the mapping back (a lifetime of 0).
	Release(ctx context.Context, m Mapping) error
}

// Errors of the package.
var (
	ErrNoGateway  = errors.New("portmap: no default gateway")
	ErrNoResponse = errors.New("portmap: the gateway answers neither PCP nor NAT-PMP")
	ErrRefused    = errors.New("portmap: the gateway refused the mapping")
)

// ProbeTimeout is how long one protocol's probe waits for the gateway.
const ProbeTimeout = 1500 * time.Millisecond

// Discover finds the protocol the gateway at gw speaks, PCP first, and returns its client
// with what the probe learned; local is this host's address toward the gateway, which
// PCP names in its requests. port is the gateway's protocol port (0: Port).
func Discover(ctx context.Context, gw, local netip.Addr, port uint16) (Client, netip.Addr, uint32, error) {
	if port == 0 {
		port = Port
	}
	var last error
	for _, c := range []Client{NewPCP(gw, local, port), NewNATPMP(gw, port)} {
		pctx, cancel := context.WithTimeout(ctx, ProbeTimeout)
		ext, epoch, err := c.Probe(pctx)
		cancel()
		if err == nil {
			return c, ext, epoch, nil
		}
		last = err
		if ctx.Err() != nil {
			return nil, netip.Addr{}, 0, ctx.Err()
		}
	}
	return nil, netip.Addr{}, 0, fmt.Errorf("%w: %w", ErrNoResponse, last)
}

// exchange sends one request to the gateway and returns the first reply within the
// context's deadline; a fresh socket per exchange, as the protocols expect.
func exchange(ctx context.Context, gw netip.AddrPort, req []byte) ([]byte, error) {
	conn, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4zero})
	if err != nil {
		return nil, err
	}
	defer conn.Close()
	deadline, ok := ctx.Deadline()
	if !ok {
		deadline = time.Now().Add(ProbeTimeout)
	}
	if err := conn.SetDeadline(deadline); err != nil {
		return nil, err
	}
	if _, err := conn.WriteToUDPAddrPort(req, gw); err != nil {
		return nil, err
	}
	buf := make([]byte, 256)
	for {
		n, from, err := conn.ReadFromUDPAddrPort(buf)
		if err != nil {
			return nil, err
		}
		if from.Addr().Unmap() == gw.Addr().Unmap() {
			return buf[:n], nil
		}
	}
}
