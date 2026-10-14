package portmap

import (
	"context"
	"encoding/binary"
	"fmt"
	"net/netip"
	"time"
)

// NAT-PMP (RFC 6886): version 0; opcodes 0 public address, 1 map UDP, 2 map TCP; a
// reply's opcode is the request's plus 128; result 0 is success.
const (
	pmpVersion      = 0
	pmpOpPublic     = 0
	pmpOpMapUDP     = 1
	pmpOpReply      = 0x80
	pmpPublicReply  = 12
	pmpMappingReply = 16
)

// NATPMP is the NAT-PMP client of one gateway.
type NATPMP struct {
	gw netip.AddrPort
}

// NewNATPMP makes the client of the gateway at gw, port being the protocol port.
func NewNATPMP(gw netip.Addr, port uint16) *NATPMP {
	return &NATPMP{gw: netip.AddrPortFrom(gw, port)}
}

func (c *NATPMP) Name() string { return "natpmp" }

func (c *NATPMP) Probe(ctx context.Context) (netip.Addr, uint32, error) {
	reply, err := exchange(ctx, c.gw, []byte{pmpVersion, pmpOpPublic})
	if err != nil {
		return netip.Addr{}, 0, err
	}
	if len(reply) < pmpPublicReply || reply[0] != pmpVersion || reply[1] != pmpOpPublic|pmpOpReply {
		return netip.Addr{}, 0, fmt.Errorf("natpmp: unexpected reply (%d bytes)", len(reply))
	}
	if code := binary.BigEndian.Uint16(reply[2:4]); code != 0 {
		return netip.Addr{}, 0, fmt.Errorf("natpmp: result %d", code)
	}
	return netip.AddrFrom4([4]byte(reply[8:12])), binary.BigEndian.Uint32(reply[4:8]), nil
}

func (c *NATPMP) Map(ctx context.Context, internal, suggested uint16, lifetime time.Duration) (Mapping, error) {
	ext, epoch, err := c.Probe(ctx)
	if err != nil {
		return Mapping{}, err
	}
	req := make([]byte, 12)
	req[0], req[1] = pmpVersion, pmpOpMapUDP
	binary.BigEndian.PutUint16(req[4:6], internal)
	binary.BigEndian.PutUint16(req[6:8], suggested)
	binary.BigEndian.PutUint32(req[8:12], uint32(lifetime/time.Second)) //nolint:gosec // a lifetime of hours
	reply, err := exchange(ctx, c.gw, req)
	if err != nil {
		return Mapping{}, err
	}
	if len(reply) < pmpMappingReply || reply[0] != pmpVersion || reply[1] != pmpOpMapUDP|pmpOpReply {
		return Mapping{}, fmt.Errorf("natpmp: unexpected mapping reply (%d bytes)", len(reply))
	}
	if code := binary.BigEndian.Uint16(reply[2:4]); code != 0 {
		return Mapping{}, fmt.Errorf("%w: natpmp result %d", ErrRefused, code)
	}
	m := Mapping{
		Protocol: c.Name(), Gateway: c.gw.Addr(), ExternalIP: ext, Epoch: epoch,
		Internal: binary.BigEndian.Uint16(reply[8:10]), External: binary.BigEndian.Uint16(reply[10:12]),
		Lifetime: time.Duration(binary.BigEndian.Uint32(reply[12:16])) * time.Second,
	}
	if m.Lifetime == 0 && lifetime > 0 {
		return Mapping{}, fmt.Errorf("%w: natpmp granted no lifetime", ErrRefused)
	}
	return m, nil
}

func (c *NATPMP) Release(ctx context.Context, m Mapping) error {
	req := make([]byte, 12)
	req[0], req[1] = pmpVersion, pmpOpMapUDP
	binary.BigEndian.PutUint16(req[4:6], m.Internal)
	_, err := exchange(ctx, c.gw, req) // a lifetime of 0 and no external port: delete
	return err
}
