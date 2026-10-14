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

func newBind(inner conn.Bind, log *slog.Logger) *Bind {
	return &Bind{inner: inner, log: log, cands: map[uint32][]conn.Endpoint{}}
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
			if t := packets[i][0]; t == frameIntroduce || t == frameRelay || t == frameProbe {
				frame := make([]byte, sizes[i])
				copy(frame, packets[i][:sizes[i]])
				sizes[i] = 0
				b.frame(frame, eps[i])
			}
		}
		return n, err
	}
}

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

// Send writes the packets to the endpoint: every candidate for a fan-out, else as is.
func (b *Bind) Send(bufs [][]byte, ep conn.Endpoint) error {
	var n uint64
	for _, buf := range bufs {
		n += uint64(len(buf))
	}
	f, ok := ep.(*fanout)
	if !ok {
		b.tx.Add(n)
		return b.inner.Send(bufs, ep)
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
		if err := b.inner.Send(bufs, c); err != nil {
			if firstErr == nil {
				firstErr = err
			}
			continue
		}
		sent = true
		b.tx.Add(n)
	}
	if !sent {
		return firstErr
	}
	return nil
}

// ParseEndpoint reads `fanout:<idx>` or an address.
func (b *Bind) ParseEndpoint(s string) (conn.Endpoint, error) {
	if idx, ok := strings.CutPrefix(s, fanoutPrefix); ok {
		n, err := strconv.ParseUint(idx, 10, 32)
		if err != nil {
			return nil, fmt.Errorf("mesh: bad fan-out endpoint %q", s)
		}
		return &fanout{idx: uint32(n)}, nil
	}
	return b.inner.ParseEndpoint(s)
}

// SetCandidates replaces a peer's candidates (docs/20 §5, I-322).
func (b *Bind) SetCandidates(idx uint32, cands []netip.AddrPort) error {
	eps := make([]conn.Endpoint, 0, len(cands))
	for _, c := range cands {
		ep, err := b.inner.ParseEndpoint(c.String())
		if err != nil {
			return err
		}
		eps = append(eps, ep)
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if len(eps) == 0 {
		delete(b.cands, idx)
		return nil
	}
	b.cands[idx] = eps
	return nil
}

// Counters are the bytes the socket received and sent since the bind opened.
func (b *Bind) Counters() (rx, tx uint64) { return b.rx.Load(), b.tx.Load() }
