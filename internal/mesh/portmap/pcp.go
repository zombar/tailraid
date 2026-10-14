package portmap

import (
	"context"
	"crypto/rand"
	"encoding/binary"
	"fmt"
	"net/netip"
	"time"
)

// PCP (RFC 6887): version 2; opcodes 0 ANNOUNCE, 1 MAP; a reply's opcode is the
// request's plus 128; the client address and the external address are 16-byte,
// IPv4-mapped; a MAP carries a 12-byte nonce the reply echoes.
const (
	pcpVersion    = 2
	pcpOpAnnounce = 0
	pcpOpMap      = 1
	pcpOpReply    = 0x80
	pcpUDP        = 17
	pcpHeader     = 24
	pcpMapLen     = pcpHeader + 36
	pcpNotAuth    = 2
)

// PCP is the PCP client of one gateway.
type PCP struct {
	gw    netip.AddrPort
	local netip.Addr
}

// NewPCP makes the client of the gateway at gw for the host at local.
func NewPCP(gw, local netip.Addr, port uint16) *PCP {
	return &PCP{gw: netip.AddrPortFrom(gw, port), local: local}
}

func (c *PCP) Name() string { return "pcp" }

func (c *PCP) header(op byte, lifetime uint32) []byte {
	b := make([]byte, pcpHeader)
	b[0], b[1] = pcpVersion, op
	binary.BigEndian.PutUint32(b[4:8], lifetime)
	a := c.local.Unmap().As16()
	copy(b[8:24], a[:])
	return b
}

// Probe is an ANNOUNCE: the gateway's epoch; its external address comes with a MAP, so
// a probe maps nothing and reports no address.
func (c *PCP) Probe(ctx context.Context) (netip.Addr, uint32, error) {
	reply, err := exchange(ctx, c.gw, c.header(pcpOpAnnounce, 0))
	if err != nil {
		return netip.Addr{}, 0, err
	}
	if len(reply) < pcpHeader || reply[0] != pcpVersion || reply[1] != pcpOpAnnounce|pcpOpReply {
		return netip.Addr{}, 0, fmt.Errorf("pcp: unexpected reply (%d bytes)", len(reply))
	}
	if reply[3] != 0 {
		return netip.Addr{}, 0, fmt.Errorf("pcp: result %d", reply[3])
	}
	return netip.Addr{}, binary.BigEndian.Uint32(reply[8:12]), nil
}

func (c *PCP) mapRequest(internal, suggested uint16, lifetime time.Duration, suggestedIP netip.Addr) ([]byte, [12]byte) {
	b := c.header(pcpOpMap, uint32(lifetime/time.Second)) //nolint:gosec // a lifetime of hours
	var nonce [12]byte
	_, _ = rand.Read(nonce[:])
	op := make([]byte, 36)
	copy(op[0:12], nonce[:])
	op[12] = pcpUDP
	binary.BigEndian.PutUint16(op[16:18], internal)
	binary.BigEndian.PutUint16(op[18:20], suggested)
	if suggestedIP.IsValid() {
		a := suggestedIP.Unmap().As16()
		copy(op[20:36], a[:])
	}
	return append(b, op...), nonce
}

func (c *PCP) Map(ctx context.Context, internal, suggested uint16, lifetime time.Duration) (Mapping, error) {
	req, nonce := c.mapRequest(internal, suggested, lifetime, netip.Addr{})
	reply, err := exchange(ctx, c.gw, req)
	if err != nil {
		return Mapping{}, err
	}
	if len(reply) < pcpMapLen || reply[0] != pcpVersion || reply[1] != pcpOpMap|pcpOpReply {
		return Mapping{}, fmt.Errorf("pcp: unexpected mapping reply (%d bytes)", len(reply))
	}
	switch reply[3] {
	case 0:
	case pcpNotAuth:
		return Mapping{}, fmt.Errorf("%w: pcp not authorized (the router has PCP off)", ErrRefused)
	default:
		return Mapping{}, fmt.Errorf("%w: pcp result %d", ErrRefused, reply[3])
	}
	op := reply[pcpHeader:]
	if [12]byte(op[0:12]) != nonce {
		return Mapping{}, fmt.Errorf("pcp: the reply's nonce is not the request's")
	}
	ext := netip.AddrFrom16([16]byte(op[20:36])).Unmap()
	m := Mapping{
		Protocol: c.Name(), Gateway: c.gw.Addr(), ExternalIP: ext, Epoch: binary.BigEndian.Uint32(reply[8:12]),
		Internal: binary.BigEndian.Uint16(op[16:18]), External: binary.BigEndian.Uint16(op[18:20]),
		Lifetime: time.Duration(binary.BigEndian.Uint32(reply[4:8])) * time.Second,
	}
	if m.Lifetime == 0 && lifetime > 0 {
		return Mapping{}, fmt.Errorf("%w: pcp granted no lifetime", ErrRefused)
	}
	return m, nil
}

func (c *PCP) Release(ctx context.Context, m Mapping) error {
	req, _ := c.mapRequest(m.Internal, m.External, 0, m.ExternalIP)
	_, err := exchange(ctx, c.gw, req)
	return err
}
