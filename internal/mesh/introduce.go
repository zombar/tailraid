package mesh

import (
	"context"
	"crypto/hkdf"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"sync"
	"time"

	"github.com/zombar/tailraid/internal/clock"
)

// The control frames on the mesh socket, told from WireGuard's message types 1–4 by the
// first byte (docs/20 §1).
const (
	frameIntroduce byte = 0x10
	frameRelay     byte = 0x11
	frameProbe     byte = 0x12
)

// The introduce (docs/20 §4, I-319).
const (
	introduceLen      = 1 + 16 + 32 + 8 + introduceMACLen // 73
	introduceMACLen   = 16
	introduceReplyLen = 1 + 4 + 1 + 32 // 38: shorter than the request
	// IntroduceSkew is how far a request's timestamp may be from the receiver's clock.
	IntroduceSkew = 10 * time.Minute
	// IntroduceRetry is how often the joiner resends while unanswered.
	IntroduceRetry = 2 * time.Second
	// IntroduceTimeout is how long it keeps trying.
	IntroduceTimeout = 60 * time.Second
	// introduceHold is the pause a source gets after a failed introduce.
	introduceHold = time.Second
	// introduceWorkers bounds the proposals one node runs for introduces at once.
	introduceWorkers = 16
	maxHeld          = 4096 // sources remembered for the hold
)

// Errors of the introduce.
var (
	ErrIntroduceTimeout = errors.New("mesh: no answer to the introduce (a wrong code, a rotated token, a clock more than 10 minutes off, or no path to the device that printed it)")
	ErrIntroduceReply   = errors.New("mesh: the introduce's reply did not verify")
)

// IntroduceKey derives the key that authenticates introduces from the token: a rotation
// changes it on every node.
func IntroduceKey(token string, clusterID [16]byte) []byte {
	k, err := hkdf.Key(sha256.New, []byte(token), clusterID[:], "tailraid introduce v1", 32)
	if err != nil {
		panic("mesh: hkdf: " + err.Error())
	}
	return k
}

// Introducer answers an introduce: the node asks the leader for the key's address (a
// pending row, or the row the key already has) and returns it with the subnet's bits.
type Introducer interface {
	Introduce(ctx context.Context, key Key) (netip.Prefix, error)
}

type introduceReq struct {
	cluster [16]byte
	key     Key
	at      int64
}

func encodeIntroduce(r introduceReq, mac []byte) []byte {
	b := make([]byte, 0, introduceLen)
	b = append(b, frameIntroduce)
	b = append(b, r.cluster[:]...)
	b = append(b, r.key[:]...)
	b = binary.BigEndian.AppendUint64(b, uint64(r.at)) //nolint:gosec // a unix time
	m := hmac.New(sha256.New, mac)
	m.Write(b)
	return append(b, m.Sum(nil)[:introduceMACLen]...)
}

func decodeIntroduce(b []byte) (introduceReq, bool) {
	if len(b) != introduceLen || b[0] != frameIntroduce {
		return introduceReq{}, false
	}
	var r introduceReq
	copy(r.cluster[:], b[1:17])
	copy(r.key[:], b[17:49])
	r.at = int64(binary.BigEndian.Uint64(b[49:57])) //nolint:gosec // a unix time
	return r, true
}

// replyMAC binds a reply to the request it answers.
func replyMAC(key []byte, addr netip.Prefix, req introduceReq) []byte {
	m := hmac.New(sha256.New, key)
	a := addr.Addr().As4()
	m.Write([]byte{frameIntroduce})
	m.Write(a[:])
	m.Write([]byte{byte(addr.Bits())}) //nolint:gosec // 0..32
	m.Write(req.key[:])
	m.Write(binary.BigEndian.AppendUint64(nil, uint64(req.at))) //nolint:gosec // a unix time
	return m.Sum(nil)
}

func encodeReply(key []byte, addr netip.Prefix, req introduceReq) []byte {
	b := make([]byte, 0, introduceReplyLen)
	a := addr.Addr().As4()
	b = append(b, frameIntroduce)
	b = append(b, a[:]...)
	b = append(b, byte(addr.Bits())) //nolint:gosec // 0..32
	return append(b, replyMAC(key, addr, req)...)
}

func decodeReply(b []byte) (netip.Prefix, []byte, bool) {
	if len(b) != introduceReplyLen || b[0] != frameIntroduce {
		return netip.Prefix{}, nil, false
	}
	addr := netip.AddrFrom4([4]byte(b[1:5]))
	return netip.PrefixFrom(addr, int(b[5])), b[6:], true
}

// introduceServer answers introduces on a member (docs/20 §4).
type introduceServer struct {
	cluster [16]byte
	keyFn   func() []byte // the current token's key; nil or empty: introduces are ignored
	intro   Introducer
	clk     clock.Clock

	mu   sync.Mutex
	held map[netip.Addr]time.Time // sources paused after a failure, until
	sem  chan struct{}
}

func newIntroduceServer(cluster [16]byte, keyFn func() []byte, intro Introducer, clk clock.Clock) *introduceServer {
	return &introduceServer{cluster: cluster, keyFn: keyFn, intro: intro, clk: clk, held: map[netip.Addr]time.Time{}, sem: make(chan struct{}, introduceWorkers)}
}

// handle answers one introduce, or nothing: silence is the answer to every refusal.
func (s *introduceServer) handle(ctx context.Context, pkt []byte, src netip.AddrPort) []byte {
	if s == nil || s.intro == nil || s.keyFn == nil {
		return nil
	}
	key := s.keyFn()
	req, ok := decodeIntroduce(pkt)
	if !ok || len(key) == 0 {
		return nil
	}
	now := s.clk.Now()
	if s.heldNow(src.Addr(), now) {
		return nil
	}
	fail := func() []byte {
		s.hold(src.Addr(), now.Add(introduceHold))
		return nil
	}
	if req.cluster != s.cluster {
		return fail()
	}
	want := encodeIntroduce(req, key)
	if !hmac.Equal(want[len(want)-introduceMACLen:], pkt[len(pkt)-introduceMACLen:]) {
		return fail()
	}
	if d := now.Sub(time.Unix(req.at, 0)); d > IntroduceSkew || d < -IntroduceSkew {
		return fail()
	}
	select {
	case s.sem <- struct{}{}:
		defer func() { <-s.sem }()
	case <-ctx.Done():
		return nil
	}
	addr, err := s.intro.Introduce(ctx, req.key)
	if err != nil || !addr.IsValid() || !addr.Addr().Is4() {
		return nil // not the sender's fault: no hold, no answer
	}
	return encodeReply(key, addr, req)
}

func (s *introduceServer) heldNow(a netip.Addr, now time.Time) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	until, ok := s.held[a]
	if !ok {
		return false
	}
	if !now.Before(until) {
		delete(s.held, a)
		return false
	}
	return true
}

func (s *introduceServer) hold(a netip.Addr, until time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.held) >= maxHeld {
		for k, t := range s.held {
			if !until.Before(t) {
				delete(s.held, k)
			}
		}
	}
	s.held[a] = until
}

// introduce is the joiner's side: the request to every endpoint of the code until one
// answers with a verified address (I-319, I-321).
func introduce(ctx context.Context, b *Bind, code JoinCode, key Key, clk clock.Clock) (netip.Prefix, error) {
	mac := IntroduceKey(code.Token, code.ClusterID)
	replies := b.awaitIntroduce()
	defer b.stopIntroduce()
	var sent []introduceReq
	deadline := clk.NewTimer(IntroduceTimeout)
	defer deadline.Stop()
	for {
		req := introduceReq{cluster: code.ClusterID, key: key, at: clk.Now().Unix()}
		sent = append(sent, req)
		if len(sent) > 8 {
			sent = sent[1:]
		}
		frame := encodeIntroduce(req, mac)
		var lastErr error
		for _, e := range code.Endpoints {
			for _, ap := range resolveEndpoint(ctx, e) {
				if err := b.SendControl(frame, ap); err != nil {
					lastErr = err
				}
			}
		}
		retry := clk.NewTimer(IntroduceRetry)
	wait:
		for {
			select {
			case <-ctx.Done():
				retry.Stop()
				return netip.Prefix{}, ctx.Err()
			case <-deadline.C():
				retry.Stop()
				if lastErr != nil {
					return netip.Prefix{}, fmt.Errorf("%w: %w", ErrIntroduceTimeout, lastErr)
				}
				return netip.Prefix{}, ErrIntroduceTimeout
			case <-retry.C():
				break wait
			case r := <-replies:
				addr, got, ok := decodeReply(r)
				if !ok {
					continue
				}
				for _, req := range sent {
					if hmac.Equal(got, replyMAC(mac, addr, req)) {
						retry.Stop()
						return addr, nil
					}
				}
				b.log.Warn("introduce: an unverified reply", "from", "mesh")
			}
		}
	}
}

// ResolveEndpoint turns host:port into addresses: an address as it is, a name through the
// resolver (nothing while it does not resolve). The joiner uses it for the code's
// endpoints on every send, the node for mesh.endpoint when it publishes its candidates
// (docs/20 §5): a peer gets addresses, never a name to resolve itself.
func ResolveEndpoint(ctx context.Context, e string) []netip.AddrPort { return resolveEndpoint(ctx, e) }

// resolveEndpoint turns host:port into addresses; a name that does not resolve now is
// retried with the next send.
func resolveEndpoint(ctx context.Context, e string) []netip.AddrPort {
	if ap, err := netip.ParseAddrPort(e); err == nil {
		return []netip.AddrPort{ap}
	}
	host, port, err := net.SplitHostPort(e)
	if err != nil {
		return nil
	}
	rctx, cancel := context.WithTimeout(ctx, IntroduceRetry)
	defer cancel()
	p, err := net.DefaultResolver.LookupPort(rctx, "udp", port)
	if err != nil {
		return nil
	}
	addrs, err := net.DefaultResolver.LookupNetIP(rctx, "ip", host)
	if err != nil {
		return nil
	}
	out := make([]netip.AddrPort, 0, len(addrs))
	for _, a := range addrs {
		out = append(out, netip.AddrPortFrom(a.Unmap(), uint16(p))) //nolint:gosec // a port
	}
	return out
}
