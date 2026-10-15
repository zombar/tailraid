package mesh

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/netip"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"golang.zx2c4.com/wireguard/conn"
)

// Bind is the mesh's conn.Bind (docs/20 §5): wireguard-go's standard bind underneath, so
// its batching and offloads stay, with three things on top: the fan-out endpoint
// `fanout:<idx>` that sends a packet to every candidate of a peer that has no session,
// the control frames (introduce, relay, probe) told from WireGuard packets by their first
// byte and answered off the device's path, and byte counters.
type Bind struct {
	inner conn.Bind
	log   *slog.Logger
	relay *relayState

	mu      sync.Mutex
	cands   map[uint32][]conn.Endpoint
	port    uint16
	control func(ctx context.Context, src netip.AddrPort, frame []byte) []byte // nil: frames are dropped
	replies chan []byte                                                        // introduce replies while a join runs
	ctx     context.Context
	cancel  context.CancelFunc

	rx, tx atomic.Uint64
}

// fanoutPrefix is the endpoint form a peer gets while it has no session.
const fanoutPrefix = "fanout:"

var errNoCandidate = errors.New("mesh: no candidate endpoint for the peer")

func newBind(inner conn.Bind, log *slog.Logger, relay *relayState) *Bind {
	return &Bind{inner: inner, log: log, relay: relay, cands: map[uint32][]conn.Endpoint{}}
}

// fanout is the endpoint of a peer without a session: every candidate at once (I-323).
type fanout struct{ idx uint32 }

func (f *fanout) ClearSrc()           {}
func (f *fanout) SrcToString() string { return "" }
func (f *fanout) DstToString() string {
	return fanoutPrefix + strconv.FormatUint(uint64(f.idx), 10)
}

func (f *fanout) DstToBytes() []byte {
	return []byte{byte(f.idx >> 24), byte(f.idx >> 16), byte(f.idx >> 8), byte(f.idx)}
}
func (f *fanout) DstIP() netip.Addr { return netip.Addr{} }
func (f *fanout) SrcIP() netip.Addr { return netip.Addr{} }

// IsFanout reports whether an endpoint string names the fan-out of a peer.
func IsFanout(endpoint string) bool { return strings.HasPrefix(endpoint, fanoutPrefix) }

// Open opens the socket; the receive functions filter the control frames out. The
// device closes and reopens the bind on every Up, so Open and Close pair any number of
// times.
func (b *Bind) Open(port uint16) ([]conn.ReceiveFunc, uint16, error) {
	fns, actual, err := b.inner.Open(port)
	if err != nil {
		return nil, 0, err
	}
	b.mu.Lock()
	b.port = actual
	if b.cancel != nil {
		b.cancel()
	}
	b.ctx, b.cancel = context.WithCancel(context.Background())
	b.mu.Unlock()
	out := make([]conn.ReceiveFunc, len(fns))
	for i, fn := range fns {
		out[i] = b.wrap(fn)
	}
	return out, actual, nil
}

func (b *Bind) wrap(fn conn.ReceiveFunc) conn.ReceiveFunc {
	return func(packets [][]byte, sizes []int, eps []conn.Endpoint) (int, error) {
		n, err := fn(packets, sizes, eps)
		for i := range n {
			if sizes[i] == 0 {
				continue
			}
			b.rx.Add(uint64(sizes[i])) //nolint:gosec // a size
			switch packets[i][0] {
			case frameIntroduce:
				frame := make([]byte, sizes[i])
				copy(frame, packets[i][:sizes[i]])
				sizes[i] = 0
				b.frame(frame, eps[i])
			case frameRelay:
				// for this device: the WireGuard packet goes in from the relay endpoint, so
				// the peer's replies go back the same way; for another: forwarded (I-328)
				if payload, ep, ok := b.relayIn(packets[i][:sizes[i]], eps[i]); ok {
					sizes[i] = copy(packets[i], payload)
					eps[i] = ep
				} else {
					sizes[i] = 0
				}
			case frameProbe:
				b.probeIn(packets[i][:sizes[i]], eps[i])
				sizes[i] = 0
			}
		}
		return n, err
	}
}

// relayIn handles a relay frame: for this device it returns the WireGuard packet and the
// relay endpoint it came through; for another it forwards the frame, when this device
// holds direct sessions with both ends, the MAC verifies and the rate allows, and
// returns nothing (I-328).
func (b *Bind) relayIn(frame []byte, from conn.Endpoint) ([]byte, conn.Endpoint, bool) {
	r := b.relay
	if r == nil {
		return nil, nil, false
	}
	r.mu.Lock()
	key, self := r.key, r.self
	r.mu.Unlock()
	hdr, payload, ok := decodeRelay(key, frame, r.clk.Now())
	if !ok || len(frame) > MaxRelayFrame {
		return nil, nil, false
	}
	src, err := netip.ParseAddrPort(from.DstToString())
	if err != nil {
		return nil, nil, false
	}
	if hdr.to == self {
		return payload, &relayEP{via: src, peer: hdr.from}, true
	}
	r.mu.Lock()
	_, fromOK := r.forward[hdr.from]
	to, toOK := r.forward[hdr.to]
	limit := r.limit
	r.mu.Unlock()
	if !fromOK || !toOK || !limit.take(int64(len(frame))) {
		return nil, nil, false
	}
	if err := b.inner.Send([][]byte{frame}, to); err != nil {
		b.log.Debug("relay forward", "to", to.DstToString(), "err", err)
		return nil, nil, false
	}
	b.tx.Add(uint64(len(frame)))
	r.mu.Lock()
	r.relayed[[2]Key{hdr.from, hdr.to}] += uint64(len(frame))
	r.mu.Unlock()
	return nil, nil, false
}

// probeIn answers a path probe: an echo of this device's own probe from a candidate it
// probed is what the monitor pins, when the echo names the device probed; a probe for
// this device is answered with an echo under this device's own MAC; a probe for anyone
// else, a probe that came back unanswered (a reflector) and an echo nobody waits for are
// dropped (I-329).
func (b *Bind) probeIn(frame []byte, from conn.Endpoint) {
	r := b.relay
	if r == nil {
		return
	}
	r.mu.Lock()
	key, self := r.key, r.self
	r.mu.Unlock()
	now := r.clk.Now()
	kind, prober, target, ok := decodeProbe(key, frame, now)
	if !ok {
		return
	}
	src, err := netip.ParseAddrPort(from.DstToString())
	if err != nil {
		return
	}
	switch {
	case kind == probeEcho && prober == self: // the device probed answered: pin that candidate
		r.mu.Lock()
		w, waiting := r.probes[src]
		waiting = waiting && w.peer == target // an echo naming another device leaves the wait
		if waiting {
			delete(r.probes, src)
		}
		onProbe := r.onProbe
		r.mu.Unlock()
		if waiting && !now.After(w.until) && onProbe != nil {
			onProbe(w.peer, src)
		}
	case kind == probeAsk && target == self && prober != self: // a probe for this device: the echo, under this device's MAC
		if err := b.SendControl(encodeProbe(key, probeEcho, prober, target, now.Unix()), src); err != nil {
			b.log.Debug("probe echo", "to", src.String(), "err", err)
		}
	default: // for another device, or this device's own probe reflected: silence, so a wrong candidate pins nothing
	}
}

// Probe sends a path probe to a candidate of a peer and remembers it for the echo.
func (b *Bind) Probe(cand netip.AddrPort, peer Key) error {
	r := b.relay
	if r == nil {
		return nil
	}
	r.mu.Lock()
	key, self := r.key, r.self
	if len(key) == 0 {
		r.mu.Unlock()
		return nil
	}
	now := r.clk.Now()
	r.probes[cand] = probeWait{peer: peer, until: now.Add(probeWindow)}
	for c, w := range r.probes { // the ones never answered
		if now.After(w.until) {
			delete(r.probes, c)
		}
	}
	r.mu.Unlock()
	return b.SendControl(encodeProbe(key, probeAsk, self, peer, now.Unix()), cand)
}

// probeWindow is how long a probe's echo is awaited.
const probeWindow = 10 * time.Second

// frame dispatches one control frame off the receive path.
func (b *Bind) frame(frame []byte, from conn.Endpoint) {
	src, err := netip.ParseAddrPort(from.DstToString())
	if err != nil {
		return
	}
	b.mu.Lock()
	control, replies, ctx := b.control, b.replies, b.ctx
	b.mu.Unlock()
	if ctx == nil {
		return
	}
	if frame[0] == frameIntroduce && len(frame) == introduceReplyLen {
		if replies != nil {
			select {
			case replies <- frame:
			default:
			}
		}
		return
	}
	if control == nil {
		return
	}
	go func() {
		if reply := control(ctx, src, frame); reply != nil {
			if err := b.SendControl(reply, src); err != nil {
				b.log.Debug("control reply", "to", src.String(), "err", err)
			}
		}
	}()
}

// SetControl installs the handler of incoming control frames.
func (b *Bind) SetControl(h func(ctx context.Context, src netip.AddrPort, frame []byte) []byte) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.control = h
}

func (b *Bind) awaitIntroduce() <-chan []byte {
	ch := make(chan []byte, 8)
	b.mu.Lock()
	b.replies = ch
	b.mu.Unlock()
	return ch
}

func (b *Bind) stopIntroduce() {
	b.mu.Lock()
	b.replies = nil
	b.mu.Unlock()
}

// SendControl sends one control frame to an address.
func (b *Bind) SendControl(frame []byte, to netip.AddrPort) error {
	ep, err := b.inner.ParseEndpoint(to.String())
	if err != nil {
		return err
	}
	b.tx.Add(uint64(len(frame)))
	return b.inner.Send([][]byte{frame}, ep)
}

// Close closes the socket and ends the control handlers.
func (b *Bind) Close() error {
	b.mu.Lock()
	if b.cancel != nil {
		b.cancel()
		b.cancel, b.ctx = nil, nil
	}
	b.mu.Unlock()
	return b.inner.Close()
}

// SetMark passes the socket mark through.
func (b *Bind) SetMark(mark uint32) error { return b.inner.SetMark(mark) }

// BatchSize is the inner bind's.
func (b *Bind) BatchSize() int { return b.inner.BatchSize() }

// Port is the port the socket bound (0 before Open).
func (b *Bind) Port() uint16 {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.port
}

// Send writes the packets to the endpoint: every candidate for a fan-out, through the
// member for a relay endpoint, else as is.
func (b *Bind) Send(bufs [][]byte, ep conn.Endpoint) error {
	f, ok := ep.(*fanout)
	if !ok {
		return b.sendTo(bufs, ep)
	}
	b.mu.Lock()
	cands := b.cands[f.idx]
	b.mu.Unlock()
	if len(cands) == 0 {
		return errNoCandidate
	}
	var firstErr error
	sent := false
	for _, c := range cands {
		if err := b.sendTo(bufs, c); err != nil {
			if firstErr == nil {
				firstErr = err
			}
			continue
		}
		sent = true
	}
	if !sent {
		return firstErr
	}
	return nil
}

// sendTo sends to one endpoint: a relay endpoint wraps each packet in a relay frame to
// the member (I-328), which needs the relay key; anything else goes as is.
func (b *Bind) sendTo(bufs [][]byte, ep conn.Endpoint) error {
	var n uint64
	for _, buf := range bufs {
		n += uint64(len(buf))
	}
	rep, isRelay := ep.(*relayEP)
	if !isRelay {
		b.tx.Add(n)
		return b.inner.Send(bufs, ep)
	}
	r := b.relay
	if r == nil {
		return errNoRelayKey
	}
	r.mu.Lock()
	key, self := r.key, r.self
	r.mu.Unlock()
	if len(key) == 0 {
		return errNoRelayKey
	}
	via, err := b.inner.ParseEndpoint(rep.via.String())
	if err != nil {
		return err
	}
	now := r.clk.Now().Unix()
	frames := make([][]byte, 0, len(bufs))
	for _, buf := range bufs {
		frames = append(frames, encodeRelay(key, self, rep.peer, now, buf))
		n += relayHeaderLen
	}
	b.tx.Add(n)
	return b.inner.Send(frames, via)
}

var errNoRelayKey = errors.New("mesh: no cluster key yet: direct paths only")

// ParseEndpoint reads `fanout:<idx>`, `relay:<via>/<key>` or an address.
func (b *Bind) ParseEndpoint(s string) (conn.Endpoint, error) {
	if rep, ok := parseRelayEndpoint(s); ok {
		return rep, nil
	}
	if idx, ok := strings.CutPrefix(s, fanoutPrefix); ok {
		n, err := strconv.ParseUint(idx, 10, 32)
		if err != nil {
			return nil, fmt.Errorf("mesh: bad fan-out endpoint %q", s)
		}
		return &fanout{idx: uint32(n)}, nil
	}
	return b.inner.ParseEndpoint(s)
}

// SetCandidates replaces a peer's candidates (docs/20 §5, I-322) with direct ones.
func (b *Bind) SetCandidates(idx uint32, cands []netip.AddrPort) error {
	eps := make([]conn.Endpoint, 0, len(cands))
	for _, c := range cands {
		ep, err := b.inner.ParseEndpoint(c.String())
		if err != nil {
			return err
		}
		eps = append(eps, ep)
	}
	b.setCandidateEndpoints(idx, eps)
	return nil
}

// setCandidateEndpoints replaces a peer's candidates with endpoints of any kind: direct
// ones and relay ones through members (docs/20 §6).
func (b *Bind) setCandidateEndpoints(idx uint32, eps []conn.Endpoint) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if len(eps) == 0 {
		delete(b.cands, idx)
		return
	}
	b.cands[idx] = eps
}

// setForward replaces the relay role's table: the peers this device holds a direct
// session with, by their pinned endpoints (I-328).
func (b *Bind) setForward(table map[Key]netip.AddrPort) error {
	r := b.relay
	if r == nil {
		return nil
	}
	eps := make(map[Key]conn.Endpoint, len(table))
	for k, ap := range table {
		ep, err := b.inner.ParseEndpoint(ap.String())
		if err != nil {
			return err
		}
		eps[k] = ep
	}
	r.mu.Lock()
	r.forward = eps
	r.mu.Unlock()
	return nil
}

// Counters are the bytes the socket received and sent since the bind opened.
func (b *Bind) Counters() (rx, tx uint64) { return b.rx.Load(), b.tx.Load() }
