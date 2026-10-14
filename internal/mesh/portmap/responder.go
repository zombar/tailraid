package portmap

import (
	"encoding/binary"
	"log/slog"
	"maps"
	"net"
	"net/netip"
	"sync"
	"time"

	"github.com/zombar/tailraid/internal/clock"
)

// Responder is a gateway's PCP and NAT-PMP side, as the tests and the NAT rig's routers
// run it: it grants UDP mappings of whatever external port is asked or free, counts an
// epoch from its start, and calls Apply for every mapping made or released (the rig's
// router turns that into a DNAT rule). Reset is a router that lost its mappings: the
// table empties and the epoch starts over, which a client sees at its next probe.
type Responder struct {
	External netip.Addr // the external address the gateway answers with
	Lifetime time.Duration
	// Apply is called with every mapping granted (add) or released; nil applies nothing.
	Apply func(m Mapping, add bool) error
	Log   *slog.Logger
	Clock clock.Clock

	mu       sync.Mutex
	conn     *net.UDPConn
	started  time.Time
	off      bool
	table    map[uint16]Mapping // by external port
	byClient map[netip.AddrPort]uint16
	done     chan struct{}
}

// Listen starts answering on addr (":5351" on a router, "127.0.0.1:0" in a test).
func (r *Responder) Listen(addr string) (netip.AddrPort, error) {
	ap, err := net.ResolveUDPAddr("udp4", addr)
	if err != nil {
		return netip.AddrPort{}, err
	}
	conn, err := net.ListenUDP("udp4", ap)
	if err != nil {
		return netip.AddrPort{}, err
	}
	if r.Log == nil {
		r.Log = slog.New(slog.DiscardHandler)
	}
	if r.Clock == nil {
		r.Clock = clock.Real()
	}
	if r.Lifetime == 0 {
		r.Lifetime = 2 * time.Hour
	}
	r.mu.Lock()
	r.conn, r.started, r.table, r.byClient, r.done = conn, r.Clock.Now(), map[uint16]Mapping{}, map[netip.AddrPort]uint16{}, make(chan struct{})
	r.mu.Unlock()
	go r.serve(conn)
	local, _ := netip.ParseAddrPort(conn.LocalAddr().String())
	return local, nil
}

// Close stops answering; the mappings stay applied (a router keeps them).
func (r *Responder) Close() error {
	r.mu.Lock()
	conn, done := r.conn, r.done
	r.mu.Unlock()
	if conn == nil {
		return nil
	}
	err := conn.Close()
	<-done
	return err
}

// SetOff makes the responder silent (a router with the protocols off) or answering again.
func (r *Responder) SetOff(off bool) {
	r.mu.Lock()
	r.off = off
	r.mu.Unlock()
}

// Reset is the router restarting: every mapping is gone (Apply told) and the epoch
// starts over.
func (r *Responder) Reset() {
	r.mu.Lock()
	table := r.table
	r.table, r.byClient = map[uint16]Mapping{}, map[netip.AddrPort]uint16{}
	r.started = r.Clock.Now()
	r.mu.Unlock()
	for _, m := range table {
		r.apply(m, false)
	}
}

// Mappings lists the mappings held, by external port.
func (r *Responder) Mappings() map[uint16]Mapping {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make(map[uint16]Mapping, len(r.table))
	maps.Copy(out, r.table)
	return out
}

// epoch is the seconds since the responder started or last reset.
func (r *Responder) epoch() uint32 {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.epochLocked()
}

func (r *Responder) epochLocked() uint32 {
	return uint32(r.Clock.Now().Sub(r.started) / time.Second) //nolint:gosec // seconds since the start
}

func (r *Responder) apply(m Mapping, add bool) {
	if r.Apply == nil {
		return
	}
	if err := r.Apply(m, add); err != nil {
		r.Log.Warn("portmap responder: apply", "add", add, "external", m.External, "err", err)
	}
}

func (r *Responder) serve(conn *net.UDPConn) {
	defer close(r.done)
	buf := make([]byte, 256)
	for {
		n, from, err := conn.ReadFromUDPAddrPort(buf)
		if err != nil {
			return
		}
		r.mu.Lock()
		off := r.off
		r.mu.Unlock()
		if off || n < 2 {
			continue
		}
		var reply []byte
		switch buf[0] {
		case pmpVersion:
			reply = r.natpmp(buf[:n], from)
		case pcpVersion:
			reply = r.pcp(buf[:n], from)
		}
		if reply != nil {
			_, _ = conn.WriteToUDPAddrPort(reply, from)
		}
	}
}

// grant makes or renews a mapping for the client's internal port at the external port
// asked (or the same number, or the next free one); a lifetime of 0 releases it.
func (r *Responder) grant(from netip.AddrPort, proto string, internal, suggested uint16, lifetime time.Duration) Mapping {
	key := netip.AddrPortFrom(from.Addr().Unmap(), internal)
	r.mu.Lock()
	defer r.mu.Unlock()
	if lifetime == 0 {
		if ext, ok := r.byClient[key]; ok {
			m := r.table[ext]
			delete(r.table, ext)
			delete(r.byClient, key)
			r.mu.Unlock()
			r.apply(m, false)
			r.mu.Lock()
		}
		return Mapping{Protocol: proto, Gateway: r.External, ExternalIP: r.External, Internal: internal, Epoch: r.epochLocked()}
	}
	if ext, ok := r.byClient[key]; ok {
		m := r.table[ext]
		m.Lifetime, m.Epoch = min(lifetime, r.Lifetime), r.epochLocked()
		r.table[ext] = m
		return m
	}
	ext := suggested
	if ext == 0 {
		ext = internal
	}
	for taken := true; taken; ext++ {
		_, taken = r.table[ext]
	}
	ext--
	m := Mapping{Protocol: proto, Client: key.Addr(), Gateway: r.External, ExternalIP: r.External, Internal: internal, External: ext, Lifetime: min(lifetime, r.Lifetime), Epoch: r.epochLocked()}
	r.table[ext], r.byClient[key] = m, ext
	r.mu.Unlock()
	r.apply(m, true)
	r.mu.Lock()
	return m
}

func (r *Responder) natpmp(req []byte, from netip.AddrPort) []byte {
	switch req[1] {
	case pmpOpPublic:
		b := make([]byte, pmpPublicReply)
		b[1] = pmpOpPublic | pmpOpReply
		binary.BigEndian.PutUint32(b[4:8], r.epoch())
		a := r.External.As4()
		copy(b[8:12], a[:])
		return b
	case pmpOpMapUDP:
		if len(req) < 12 {
			return nil
		}
		m := r.grant(from, "natpmp", binary.BigEndian.Uint16(req[4:6]), binary.BigEndian.Uint16(req[6:8]), time.Duration(binary.BigEndian.Uint32(req[8:12]))*time.Second)
		b := make([]byte, pmpMappingReply)
		b[1] = pmpOpMapUDP | pmpOpReply
		binary.BigEndian.PutUint32(b[4:8], m.Epoch)
		binary.BigEndian.PutUint16(b[8:10], m.Internal)
		binary.BigEndian.PutUint16(b[10:12], m.External)
		binary.BigEndian.PutUint32(b[12:16], uint32(m.Lifetime/time.Second)) //nolint:gosec // hours
		return b
	}
	return nil
}

func (r *Responder) pcp(req []byte, from netip.AddrPort) []byte {
	if len(req) < pcpHeader {
		return nil
	}
	switch req[1] {
	case pcpOpAnnounce:
		b := make([]byte, pcpHeader)
		b[0], b[1] = pcpVersion, pcpOpAnnounce|pcpOpReply
		binary.BigEndian.PutUint32(b[8:12], r.epoch())
		return b
	case pcpOpMap:
		if len(req) < pcpMapLen || req[pcpHeader+12] != pcpUDP {
			return nil
		}
		op := req[pcpHeader:]
		m := r.grant(from, "pcp", binary.BigEndian.Uint16(op[16:18]), binary.BigEndian.Uint16(op[18:20]), time.Duration(binary.BigEndian.Uint32(req[4:8]))*time.Second)
		b := make([]byte, pcpMapLen)
		b[0], b[1] = pcpVersion, pcpOpMap|pcpOpReply
		binary.BigEndian.PutUint32(b[4:8], uint32(m.Lifetime/time.Second)) //nolint:gosec // hours
		binary.BigEndian.PutUint32(b[8:12], m.Epoch)
		out := b[pcpHeader:]
		copy(out[0:12], op[0:12])
		out[12] = pcpUDP
		binary.BigEndian.PutUint16(out[16:18], m.Internal)
		binary.BigEndian.PutUint16(out[18:20], m.External)
		a := r.External.Unmap().As16()
		copy(out[20:36], a[:])
		return b
	}
	return nil
}
