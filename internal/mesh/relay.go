package mesh

import (
	"crypto/hkdf"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/binary"
	"maps"
	"net/netip"
	"strings"
	"sync"
	"time"

	"golang.zx2c4.com/wireguard/conn"

	"github.com/zombar/tailraid/internal/clock"
)

// The relay and the path probe (docs/20 §6, I-328, I-329). A relay frame is
// `0x11 ‖ from ‖ to ‖ unix seconds ‖ MAC ‖ WireGuard packet`, authenticated under a key
// derived from the cluster key (docs/19 §3), which every member holds: a relay forwards
// nothing it cannot verify, and never holds a key that opens the payload. A probe is
// `0x12 ‖ kind ‖ prober ‖ target ‖ unix seconds ‖ MAC`: the device the probe names
// answers with the kind set to echo under a MAC of its own, so a reflector (a candidate
// that is the prober's own address, a UDP echo service) returns nothing the prober
// takes for an answer, and the prober, seeing the echo from a candidate, pins it.
const (
	relayMACLen    = 16
	relayHeaderLen = 1 + 32 + 32 + 8 + relayMACLen     // 89
	probeLen       = 1 + 1 + 32 + 32 + 8 + relayMACLen // 90: the kind, the prober's key, the probed device's key
	probeAsk       = 0                                 // the probe itself
	probeEcho      = 1                                 // the target's answer
	// RelaySkew is how far a frame's timestamp may be from the receiver's clock.
	RelaySkew = 10 * time.Minute
	// RelayAfter is how long a peer's direct candidates fan out unanswered before the
	// relay candidates join them.
	RelayAfter = 10 * time.Second
	// RelayRetryDirect is how often a device on a relayed path probes its direct
	// candidates.
	RelayRetryDirect = 30 * time.Second
	// ProbeEvery is how often a device on a wan path probes its lan candidates.
	ProbeEvery = 60 * time.Second
	// RelayRate is the default of mesh.relay_rate: what a device forwards for others.
	RelayRate = 20 << 20
	// MaxRelayFrame bounds a relayed packet: the WireGuard packet of one MTU-sized
	// datagram plus the header.
	MaxRelayFrame = relayHeaderLen + MTU + 128
)

// relayKey derives the frames' MAC key from the cluster key.
func relayKey(cluster []byte) []byte {
	k, err := hkdf.Key(sha256.New, cluster, nil, "tailraid relay v1", 32)
	if err != nil {
		panic("mesh: hkdf: " + err.Error())
	}
	return k
}

// relayEP is a peer's endpoint through a member: packets to it are sent to via as relay
// frames, and the member forwards them to the peer (I-328).
type relayEP struct {
	via  netip.AddrPort
	peer Key
}

const relayPrefix = "relay:"

func (e *relayEP) ClearSrc()           {}
func (e *relayEP) SrcToString() string { return "" }
func (e *relayEP) DstToString() string {
	return relayPrefix + e.via.String() + "/" + e.peer.Hex()
}

func (e *relayEP) DstToBytes() []byte {
	b := e.via.Addr().AsSlice()
	return append(b, byte(e.via.Port()>>8), byte(e.via.Port()))
}
func (e *relayEP) DstIP() netip.Addr { return e.via.Addr() }
func (e *relayEP) SrcIP() netip.Addr { return netip.Addr{} }

// parseRelayEndpoint reads the string form; ok is false for anything else.
func parseRelayEndpoint(s string) (*relayEP, bool) {
	rest, ok := strings.CutPrefix(s, relayPrefix)
	if !ok {
		return nil, false
	}
	via, keyHex, ok := strings.Cut(rest, "/")
	if !ok {
		return nil, false
	}
	ap, err := netip.ParseAddrPort(via)
	if err != nil {
		return nil, false
	}
	k, err := decodeHexKey(keyHex)
	if err != nil {
		return nil, false
	}
	return &relayEP{via: ap, peer: k}, true
}

// IsRelay reports whether an endpoint string names a path through a member.
func IsRelay(endpoint string) bool { return strings.HasPrefix(endpoint, relayPrefix) }

func encodeRelay(key []byte, from, to Key, at int64, payload []byte) []byte {
	b := make([]byte, 0, relayHeaderLen+len(payload))
	b = append(b, frameRelay)
	b = append(b, from[:]...)
	b = append(b, to[:]...)
	b = binary.BigEndian.AppendUint64(b, uint64(at)) //nolint:gosec // a unix time
	b = append(b, make([]byte, relayMACLen)...)
	b = append(b, payload...)
	copy(b[relayHeaderLen-relayMACLen:relayHeaderLen], relayMAC(key, b))
	return b
}

// relayMAC authenticates the whole frame but its own MAC bytes.
func relayMAC(key, frame []byte) []byte {
	m := hmac.New(sha256.New, key)
	m.Write(frame[:relayHeaderLen-relayMACLen])
	m.Write(frame[relayHeaderLen:])
	return m.Sum(nil)[:relayMACLen]
}

type relayFrame struct {
	from, to Key
	at       int64
}

// decodeRelay reads a frame's header and checks its MAC and timestamp; the payload is
// the frame's tail.
func decodeRelay(key, frame []byte, now time.Time) (relayFrame, []byte, bool) {
	if len(key) == 0 || len(frame) < relayHeaderLen+1 || frame[0] != frameRelay {
		return relayFrame{}, nil, false
	}
	if !hmac.Equal(frame[relayHeaderLen-relayMACLen:relayHeaderLen], relayMAC(key, frame)) {
		return relayFrame{}, nil, false
	}
	var r relayFrame
	copy(r.from[:], frame[1:33])
	copy(r.to[:], frame[33:65])
	r.at = int64(binary.BigEndian.Uint64(frame[65:73])) //nolint:gosec // a unix time
	if d := now.Sub(time.Unix(r.at, 0)); d > RelaySkew || d < -RelaySkew {
		return relayFrame{}, nil, false
	}
	return r, frame[relayHeaderLen:], true
}

func encodeProbe(key []byte, kind byte, prober, target Key, at int64) []byte {
	b := make([]byte, 0, probeLen)
	b = append(b, frameProbe, kind)
	b = append(b, prober[:]...)
	b = append(b, target[:]...)
	b = binary.BigEndian.AppendUint64(b, uint64(at)) //nolint:gosec // a unix time
	m := hmac.New(sha256.New, key)
	m.Write(b)
	return append(b, m.Sum(nil)[:relayMACLen]...)
}

// decodeProbe reads a probe or an echo and checks its MAC and timestamp: the kind, the
// prober's key and the key of the device it is for.
func decodeProbe(key, frame []byte, now time.Time) (kind byte, prober, target Key, ok bool) {
	if len(key) == 0 || len(frame) != probeLen || frame[0] != frameProbe || frame[1] > probeEcho {
		return 0, Key{}, Key{}, false
	}
	m := hmac.New(sha256.New, key)
	m.Write(frame[:probeLen-relayMACLen])
	if !hmac.Equal(frame[probeLen-relayMACLen:], m.Sum(nil)[:relayMACLen]) {
		return 0, Key{}, Key{}, false
	}
	kind = frame[1]
	copy(prober[:], frame[2:34])
	copy(target[:], frame[34:66])
	at := int64(binary.BigEndian.Uint64(frame[66:74])) //nolint:gosec // a unix time
	if d := now.Sub(time.Unix(at, 0)); d > RelaySkew || d < -RelaySkew {
		return 0, Key{}, Key{}, false
	}
	return kind, prober, target, true
}

// rateLimiter is a token bucket in bytes: what a relay forwards for others (I-328).
type rateLimiter struct {
	perSec int64
	clk    clock.Clock
	mu     sync.Mutex
	tokens int64
	last   time.Time
}

func newRateLimiter(perSec int64, clk clock.Clock) *rateLimiter {
	return &rateLimiter{perSec: perSec, clk: clk, tokens: perSec}
}

// take reports whether n bytes are within credit now and spends them if so; the bucket
// refills at perSec and holds at most one second's worth.
func (l *rateLimiter) take(n int64) bool {
	if l == nil || l.perSec <= 0 {
		return true
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	now := l.clk.Now()
	if !l.last.IsZero() {
		l.tokens += int64(now.Sub(l.last).Seconds() * float64(l.perSec))
	}
	l.last = now
	if l.tokens > l.perSec {
		l.tokens = l.perSec
	}
	if l.tokens < n {
		return false
	}
	l.tokens -= n
	return true
}

// relayState is the Bind's relay and probe side: the MAC key, this device's key, the
// forwarding table (peers with a direct session, for the relay role), the rate limiter,
// the outstanding probes and the counters.
type relayState struct {
	mu      sync.Mutex
	key     []byte        // the relay key, derived once the cluster key is at hand
	keyFn   func() []byte // the cluster key, asked until it answers
	self    Key
	forward map[Key]conn.Endpoint // peers this device forwards for: their direct endpoints
	limit   *rateLimiter
	probes  map[netip.AddrPort]probeWait // candidates probed, waiting for the echo
	relayed map[[2]Key]uint64            // bytes forwarded, by (from, to)
	onProbe func(peer Key, from netip.AddrPort)
	clk     clock.Clock
}

type probeWait struct {
	peer  Key
	until time.Time
}

func newRelayState(clk clock.Clock) *relayState {
	return &relayState{forward: map[Key]conn.Endpoint{}, probes: map[netip.AddrPort]probeWait{}, relayed: map[[2]Key]uint64{}, clk: clk}
}

// Relayed is what this device forwarded for others, by (from, to).
func (s *relayState) Relayed() map[[2]Key]uint64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make(map[[2]Key]uint64, len(s.relayed))
	maps.Copy(out, s.relayed)
	return out
}
