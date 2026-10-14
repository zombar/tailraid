package mesh

import (
	"bufio"
	"context"
	"fmt"
	"log/slog"
	"net"
	"net/netip"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"golang.zx2c4.com/wireguard/conn"
	"golang.zx2c4.com/wireguard/device"
	"golang.zx2c4.com/wireguard/tun"

	"github.com/zombar/tailraid/internal/clock"
	"github.com/zombar/tailraid/internal/meta"
)

// The path monitor (docs/20 §5, I-323).
const (
	// Keepalive is every peer's persistent keepalive: both sides of a pair fan out at
	// once, which is what opens a cone NAT on each side. wireguard-go sends the first one
	// the moment a peer is added, so a peer that does not know this device yet drops that
	// handshake and the next try is 5 s later: a device that joins is heard within that.
	Keepalive = 25 * time.Second
	// NoKeepalive as Options.Keepalive sends none: the tests drive every handshake.
	NoKeepalive = -1 * time.Second
	// PathDead is how long a pinned peer may send nothing (no packet received from it,
	// no handshake) before it returns to fan-out: three keepalives. A handshake alone is
	// no sign of life, since WireGuard renews one only every two minutes.
	PathDead = 75 * time.Second
	// TickEvery is how often the monitor looks.
	TickEvery = 10 * time.Second
)

// Options configures a Mesh.
type Options struct {
	Port       uint16        // the UDP port; 0 picks one (tests)
	Log        *slog.Logger  // nil: discard
	Clock      clock.Clock   // nil: the real clock
	Inner      conn.Bind     // nil: wireguard-go's standard bind; tests pass a NAT simulator
	ClusterID  [16]byte      // the cluster the introduce server answers for
	TokenKey   func() []byte // the current token's introduce key (IntroduceKey); nil: no introduces answered
	Introducer Introducer    // answers an introduce with the key's address; nil: no introduces answered
	PathDead   time.Duration // 0: PathDead
	Keepalive  time.Duration // 0: Keepalive; NoKeepalive: none
	// ClusterKey is the cluster key of docs/19 §3, which authenticates relay frames and
	// probes (docs/20 §6); nil or empty until fetched: direct paths only.
	ClusterKey func() []byte
	RelayRate  int64          // bytes per second this device forwards for others; 0: RelayRate
	Local      []netip.Prefix // this host's subnets, for the LAN class; nil: read from the interfaces (tests)
}

// Peer is one row the device holds (docs/20 §3).
type Peer struct {
	Idx        uint32 // the row's index: names the fan-out endpoint
	Key        Key
	Addr       netip.Addr
	Candidates []netip.AddrPort
}

// PathClass is what the storage code ranks by (docs/20 §8); it is meta's, so the
// storage packages read it without this package.
type PathClass = meta.PathClass

// The classes, best first.
const (
	PathLAN   = meta.PathLAN
	PathWAN   = meta.PathWAN
	PathRelay = meta.PathRelay
	PathNone  = meta.PathNone
)

// PeerStatus is a peer as the device sees it.
type PeerStatus struct {
	Key           Key
	Endpoint      netip.AddrPort // the pinned endpoint; zero while fanning out or unknown; the member's for a relayed path
	Fanout        bool
	Relay         bool      // pinned through a member (docs/20 §6): Endpoint is the member's
	LastHandshake time.Time // zero when there was none
	LastRx        time.Time // when the received byte count last grew (a keepalive counts); zero when nothing arrived
	Rx, Tx        uint64
	Keepalive     time.Duration
	Class         PathClass
}

// LastSeen is the later of the peer's last handshake and its last received packet: the
// sign of life the path monitor and the class go by.
func (p PeerStatus) LastSeen() time.Time {
	if p.LastRx.After(p.LastHandshake) {
		return p.LastRx
	}
	return p.LastHandshake
}

// rxSeen is what the device last saw of a peer's received bytes, for LastRx.
type rxSeen struct {
	bytes uint64
	at    time.Time
}

// Mesh is one node's device.
type Mesh struct {
	dev   *device.Device
	bind  *Bind
	log   *slog.Logger
	clk   clock.Clock
	key   PrivateKey
	dead  time.Duration
	ka    time.Duration
	intro *introduceServer

	relay *relayState
	fixed bool // Options.Local given: the subnets are not re-read

	mu          sync.Mutex
	peers       map[Key]Peer
	local       []netip.Prefix    // this host's own subnets, for the LAN class
	seen        map[Key]rxSeen    // per peer: the received bytes last read and when they last grew
	fanoutSince map[Key]time.Time // when a peer last entered fan-out, for the relay candidates
	probedAt    map[Key]time.Time // when a peer's better candidates were last probed
}

// New starts a device on tunDev with the key, listening on the port (I-317).
func New(tunDev tun.Device, key PrivateKey, o Options) (*Mesh, error) {
	log := o.Log
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	clk := o.Clock
	if clk == nil {
		clk = clock.Real()
	}
	inner := o.Inner
	if inner == nil {
		inner = conn.NewStdNetBind()
	}
	relay := newRelayState(clk)
	relay.self = key.Public()
	rate := o.RelayRate
	if rate == 0 {
		rate = RelayRate
	}
	relay.limit = newRateLimiter(rate, clk)
	m := &Mesh{bind: newBind(inner, log, relay), relay: relay, log: log, clk: clk, key: key, dead: o.PathDead, ka: o.Keepalive, peers: map[Key]Peer{}, seen: map[Key]rxSeen{}, fanoutSince: map[Key]time.Time{}, probedAt: map[Key]time.Time{}}
	if m.dead == 0 {
		m.dead = PathDead
	}
	if o.Local != nil {
		m.local, m.fixed = o.Local, true
	}
	relay.onProbe = m.pinDirect
	if o.ClusterKey != nil {
		m.SetClusterKey(o.ClusterKey)
	}
	switch {
	case m.ka == 0:
		m.ka = Keepalive
	case m.ka < 0:
		m.ka = 0
	}
	if o.Introducer != nil && o.TokenKey != nil {
		m.intro = newIntroduceServer(o.ClusterID, o.TokenKey, o.Introducer, clk)
	}
	m.bind.SetControl(m.control)
	verbose := device.DiscardLogf
	if log.Enabled(context.Background(), slog.LevelDebug) {
		verbose = func(format string, args ...any) { log.Debug("wireguard: " + fmt.Sprintf(format, args...)) }
	}
	m.dev = device.NewDevice(tunDev, m.bind, &device.Logger{
		Verbosef: verbose,
		Errorf:   func(format string, args ...any) { log.Error("wireguard: " + fmt.Sprintf(format, args...)) },
	})
	conf := "private_key=" + Key(key).Hex() + "\nlisten_port=" + strconv.Itoa(int(o.Port)) + "\n"
	if err := m.dev.IpcSet(conf); err != nil {
		m.dev.Close()
		return nil, fmt.Errorf("mesh: configure device: %w", err)
	}
	if err := m.dev.Up(); err != nil {
		m.dev.Close()
		return nil, fmt.Errorf("mesh: bring the device up: %w", err)
	}
	m.refreshLocal()
	return m, nil
}

// control answers the frames the bind hands over.
func (m *Mesh) control(ctx context.Context, src netip.AddrPort, frame []byte) []byte {
	switch frame[0] {
	case frameIntroduce:
		return m.intro.handle(ctx, frame, src)
	default:
		return nil // relay and probe frames land with docs/20 §6
	}
}

// Close stops the device, its socket and the interface.
func (m *Mesh) Close() {
	m.dev.Close()
}

// Port is the UDP port the socket bound.
func (m *Mesh) Port() uint16 { return m.bind.Port() }

// Key is the device's public key.
func (m *Mesh) Key() Key { return m.key.Public() }

// Counters are the socket's bytes in and out.
func (m *Mesh) Counters() (rx, tx uint64) { return m.bind.Counters() }

// SetClusterKey gives the device the cluster key relay frames and probes are
// authenticated under (docs/20 §6); keyFn is asked on every use, so a key fetched later
// is picked up without a restart. Without it the device uses direct paths only.
func (m *Mesh) SetClusterKey(keyFn func() []byte) {
	m.relay.mu.Lock()
	m.relay.key = nil
	m.relay.keyFn = keyFn
	m.relay.mu.Unlock()
	m.refreshRelayKey()
}

// refreshRelayKey derives the relay key from the cluster key when one is available.
func (m *Mesh) refreshRelayKey() {
	m.relay.mu.Lock()
	defer m.relay.mu.Unlock()
	if m.relay.keyFn == nil || m.relay.key != nil {
		return
	}
	if ck := m.relay.keyFn(); len(ck) > 0 {
		m.relay.key = relayKey(ck)
	}
}

// Relayed is what this device forwarded for others, bytes by (from, to) (I-328).
func (m *Mesh) Relayed() map[[2]Key]uint64 { return m.relay.Relayed() }

// pinDirect moves a peer to a candidate that answered a probe (I-328, I-329).
func (m *Mesh) pinDirect(peer Key, to netip.AddrPort) {
	if err := m.dev.IpcSet("public_key=" + peer.Hex() + "\nupdate_only=true\nendpoint=" + to.String() + "\n"); err != nil {
		m.log.Warn("mesh: pin probed endpoint", "err", err)
		return
	}
	m.log.Info("mesh: moved to a direct path", "peer", peer.String(), "endpoint", to.String())
}

// Introduce runs the joiner's side of the introduce through this device's socket, so the
// reflexive endpoint the member sees is the mesh port's (docs/20 §4).
func (m *Mesh) Introduce(ctx context.Context, code JoinCode) (netip.Prefix, error) {
	return introduce(ctx, m.bind, code, m.key.Public(), m.clk)
}

// SetPeers makes the device's peers exactly peers (I-317): rows no longer listed are
// removed, new ones added with their candidates and the fan-out endpoint, known ones keep
// their pinned endpoint and get their candidates refreshed.
func (m *Mesh) SetPeers(peers []Peer) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	want := make(map[Key]Peer, len(peers))
	for _, p := range peers {
		if p.Key.IsZero() || p.Key == m.key.Public() {
			continue
		}
		want[p.Key] = p
	}
	var sb strings.Builder
	for k := range m.peers {
		if _, keep := want[k]; !keep {
			sb.WriteString("public_key=" + k.Hex() + "\nremove=true\n")
			if err := m.bind.SetCandidates(m.peers[k].Idx, nil); err != nil {
				return err
			}
		}
	}
	for k, p := range want {
		if err := m.bind.SetCandidates(p.Idx, p.Candidates); err != nil {
			return err
		}
		old, known := m.peers[k]
		sb.WriteString("public_key=" + k.Hex() + "\n")
		if known && old.Addr == p.Addr {
			sb.WriteString("update_only=true\n")
			// the pinned endpoint stays, relayed or direct; a peer that never had one (its
			// row came before its endpoints) fans out at the monitor's next tick
			continue
		}
		sb.WriteString("replace_allowed_ips=true\nallowed_ip=" + p.Addr.String() + "/32\n")
		sb.WriteString("persistent_keepalive_interval=" + strconv.Itoa(int(m.ka/time.Second)) + "\n")
		if len(p.Candidates) > 0 {
			sb.WriteString("endpoint=" + (&fanout{idx: p.Idx}).DstToString() + "\n")
		}
	}
	if sb.Len() > 0 {
		if err := m.dev.IpcSet(sb.String()); err != nil {
			return fmt.Errorf("mesh: set peers: %w", err)
		}
	}
	m.peers = want
	return nil
}

// Peers reports every peer with its path (I-325).
func (m *Mesh) Peers() ([]PeerStatus, error) {
	raw, err := m.dev.IpcGet()
	if err != nil {
		return nil, fmt.Errorf("mesh: read device: %w", err)
	}
	now := m.clk.Now()
	var out []PeerStatus
	var cur *PeerStatus
	sc := bufio.NewScanner(strings.NewReader(raw))
	for sc.Scan() {
		k, v, ok := strings.Cut(sc.Text(), "=")
		if !ok {
			continue
		}
		switch k {
		case "public_key":
			out = append(out, PeerStatus{})
			cur = &out[len(out)-1]
			b, err := decodeHexKey(v)
			if err != nil {
				return nil, err
			}
			cur.Key = b
		case "endpoint":
			if cur == nil {
				continue
			}
			if IsFanout(v) {
				cur.Fanout = true
			} else if rep, ok := parseRelayEndpoint(v); ok {
				cur.Relay, cur.Endpoint = true, rep.via
			} else if ap, err := netip.ParseAddrPort(v); err == nil {
				cur.Endpoint = ap
			}
		case "last_handshake_time_sec":
			if cur == nil {
				continue
			}
			if s, err := strconv.ParseInt(v, 10, 64); err == nil && s > 0 {
				cur.LastHandshake = time.Unix(s, 0)
			}
		case "rx_bytes":
			if cur != nil {
				cur.Rx, _ = strconv.ParseUint(v, 10, 64)
			}
		case "tx_bytes":
			if cur != nil {
				cur.Tx, _ = strconv.ParseUint(v, 10, 64)
			}
		case "persistent_keepalive_interval":
			if cur != nil {
				s, _ := strconv.Atoi(v)
				cur.Keepalive = time.Duration(s) * time.Second
			}
		}
	}
	m.mu.Lock()
	local := m.local
	for i := range out {
		p := &out[i]
		// a received byte is a sign of life a handshake is not: keepalives arrive every
		// Keepalive, handshakes only every two minutes
		s, known := m.seen[p.Key]
		switch {
		case !known && p.Rx > 0:
			s = rxSeen{bytes: p.Rx, at: now}
		case known && p.Rx > s.bytes:
			s = rxSeen{bytes: p.Rx, at: now}
		case !known:
			s = rxSeen{}
		}
		m.seen[p.Key] = s
		p.LastRx = s.at
	}
	for k := range m.seen {
		if _, still := m.peers[k]; !still {
			delete(m.seen, k)
		}
	}
	m.mu.Unlock()
	for i := range out {
		out[i].Class = classify(out[i], now, m.dead, local)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Key.String() < out[j].Key.String() })
	return out, nil
}

func classify(p PeerStatus, now time.Time, dead time.Duration, local []netip.Prefix) PathClass {
	seen := p.LastSeen()
	if p.Fanout || !p.Endpoint.IsValid() || seen.IsZero() || now.Sub(seen) > dead {
		return PathNone
	}
	if p.Relay {
		return PathRelay
	}
	if onLocal(p.Endpoint.Addr(), local) {
		return PathLAN
	}
	return PathWAN
}

// Tick is the path monitor (docs/20 §5, §6): a peer not heard from for PathDead (no
// packet received, no handshake), or never, is sent back to fan-out so its candidates
// are tried again (I-323); a peer fanning out for RelayAfter with the cluster key at
// hand gets a candidate through every member this device holds a direct session with
// (I-328); a peer on a relayed path probes its direct candidates every
// RelayRetryDirect and one on a wan path its lan candidates every ProbeEvery (I-329);
// the host's own subnets are re-read for the LAN class, and the relay role's forwarding
// table is the peers with a direct session.
func (m *Mesh) Tick() error {
	m.refreshLocal()
	m.refreshRelayKey()
	peers, err := m.Peers()
	if err != nil {
		return err
	}
	now := m.clk.Now()
	m.relay.mu.Lock()
	haveKey := len(m.relay.key) > 0
	m.relay.mu.Unlock()
	// the members this device reaches directly: what it forwards between, and what it
	// offers as relays to a peer it cannot reach
	forward := map[Key]netip.AddrPort{}
	for i := range peers {
		if p := &peers[i]; !p.Fanout && !p.Relay && p.Endpoint.IsValid() && p.Class != PathNone {
			forward[p.Key] = p.Endpoint
		}
	}
	if err := m.bind.setForward(forward); err != nil {
		return err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	var sb strings.Builder
	for i := range peers {
		p := &peers[i]
		row, ok := m.peers[p.Key]
		if !ok {
			continue
		}
		seen := p.LastSeen()
		alive := !seen.IsZero() && now.Sub(seen) <= m.dead
		switch {
		case p.Fanout, !p.Endpoint.IsValid():
			// fanning out, or never had an endpoint: after RelayAfter the relay candidates
			// join the direct ones; a peer with neither has nothing to do
			since, known := m.fanoutSince[p.Key]
			if !known {
				since = now
				m.fanoutSince[p.Key] = now
			}
			eps := m.directEndpoints(row)
			if haveKey && now.Sub(since) >= RelayAfter {
				eps = append(eps, m.relayEndpoints(row, forward)...)
			}
			if len(eps) == 0 {
				continue
			}
			m.bind.setCandidateEndpoints(row.Idx, eps)
			if !p.Fanout {
				sb.WriteString("public_key=" + p.Key.Hex() + "\nupdate_only=true\nendpoint=" + (&fanout{idx: row.Idx}).DstToString() + "\n")
			}
		case !alive:
			delete(m.fanoutSince, p.Key)
			if len(row.Candidates) == 0 && len(forward) == 0 {
				continue
			}
			m.fanoutSince[p.Key] = now
			m.bind.setCandidateEndpoints(row.Idx, m.directEndpoints(row))
			sb.WriteString("public_key=" + p.Key.Hex() + "\nupdate_only=true\nendpoint=" + (&fanout{idx: row.Idx}).DstToString() + "\n")
		default:
			delete(m.fanoutSince, p.Key)
			m.probeBetter(p, row, now)
		}
	}
	if sb.Len() == 0 {
		return nil
	}
	if err := m.dev.IpcSet(sb.String()); err != nil {
		return fmt.Errorf("mesh: fan out again: %w", err)
	}
	return nil
}

// directEndpoints are a row's candidates as endpoints.
func (m *Mesh) directEndpoints(row Peer) []conn.Endpoint {
	var out []conn.Endpoint
	for _, c := range row.Candidates {
		if ep, err := m.bind.inner.ParseEndpoint(c.String()); err == nil {
			out = append(out, ep)
		}
	}
	return out
}

// relayEndpoints are the candidates `via M` for every member M in forward but the peer
// itself (docs/20 §6).
func (m *Mesh) relayEndpoints(row Peer, forward map[Key]netip.AddrPort) []conn.Endpoint {
	keys := make([]Key, 0, len(forward))
	for k := range forward {
		if k != row.Key {
			keys = append(keys, k)
		}
	}
	sort.Slice(keys, func(i, j int) bool { return keys[i].String() < keys[j].String() })
	out := make([]conn.Endpoint, 0, len(keys))
	for _, k := range keys {
		out = append(out, &relayEP{via: forward[k], peer: row.Key})
	}
	return out
}

// probeBetter sends the probes a pinned peer is due (I-329): on a relayed path every
// direct candidate every RelayRetryDirect, on a wan path the lan candidates every
// ProbeEvery; a lan path probes nothing. Called under m.mu.
func (m *Mesh) probeBetter(p *PeerStatus, row Peer, now time.Time) {
	targets, every := m.probeTargets(p, row)
	if len(targets) == 0 || now.Sub(m.probedAt[p.Key]) < every {
		return
	}
	m.probedAt[p.Key] = now
	for _, c := range targets {
		if err := m.bind.Probe(c, p.Key); err != nil {
			m.log.Debug("mesh: probe", "to", c.String(), "err", err)
		}
	}
}

// probeTargets picks the candidates a pinned peer probes and how often: every direct
// candidate every RelayRetryDirect on a relayed path, the lan ones every ProbeEvery on
// a wan path, none on a lan path or while the cluster key is missing.
func (m *Mesh) probeTargets(p *PeerStatus, row Peer) ([]netip.AddrPort, time.Duration) {
	m.relay.mu.Lock()
	haveKey := len(m.relay.key) > 0
	m.relay.mu.Unlock()
	if !haveKey {
		return nil, 0
	}
	switch p.Class {
	case PathRelay:
		return row.Candidates, RelayRetryDirect
	case PathWAN:
		var lan []netip.AddrPort
		for _, c := range row.Candidates {
			if c != p.Endpoint && onLocal(c.Addr(), m.local) {
				lan = append(lan, c)
			}
		}
		return lan, ProbeEvery
	default:
		return nil, 0
	}
}

func onLocal(a netip.Addr, local []netip.Prefix) bool {
	for _, pfx := range local {
		if pfx.Contains(a.Unmap()) {
			return true
		}
	}
	return false
}

// Path is one peer's class now (docs/20 §8).
func (m *Mesh) Path(key Key) PathClass {
	peers, err := m.Peers()
	if err != nil {
		return PathNone
	}
	for i := range peers {
		if peers[i].Key == key {
			return peers[i].Class
		}
	}
	return PathNone
}

func (m *Mesh) refreshLocal() {
	if m.fixed {
		return
	}
	pfx, err := LocalPrefixes()
	if err != nil {
		return
	}
	m.mu.Lock()
	m.local = pfx
	m.mu.Unlock()
}

func decodeHexKey(s string) (Key, error) {
	var k Key
	if len(s) != 2*len(k) {
		return k, fmt.Errorf("%w: %q", ErrBadKey, s)
	}
	for i := range k {
		b, err := strconv.ParseUint(s[2*i:2*i+2], 16, 8)
		if err != nil {
			return Key{}, fmt.Errorf("%w: %q", ErrBadKey, s)
		}
		k[i] = byte(b)
	}
	return k, nil
}

// LocalEndpoints are this host's candidates (I-322): every non-loopback, non-link-local
// address of an interface that is up, with the mesh port, IPv4 first.
func LocalEndpoints(port uint16) ([]netip.AddrPort, error) {
	pfx, err := LocalPrefixes()
	if err != nil {
		return nil, err
	}
	out := make([]netip.AddrPort, 0, len(pfx))
	for _, p := range pfx {
		out = append(out, netip.AddrPortFrom(p.Addr(), port))
	}
	return out, nil
}

// LocalPrefixes are the host's own subnets, the mesh's excluded: an endpoint inside one
// is a LAN path.
func LocalPrefixes() ([]netip.Prefix, error) {
	ifs, err := net.Interfaces()
	if err != nil {
		return nil, fmt.Errorf("mesh: interfaces: %w", err)
	}
	var out []netip.Prefix
	for _, ifc := range ifs {
		if ifc.Flags&net.FlagUp == 0 || ifc.Flags&net.FlagLoopback != 0 {
			continue
		}
		addrs, err := ifc.Addrs()
		if err != nil {
			continue
		}
		for _, a := range addrs {
			ipn, ok := a.(*net.IPNet)
			if !ok {
				continue
			}
			ip, ok := netip.AddrFromSlice(ipn.IP)
			if !ok {
				continue
			}
			ip = ip.Unmap()
			if ip.IsLoopback() || ip.IsLinkLocalUnicast() || ip.IsMulticast() || ip.IsUnspecified() {
				continue
			}
			bits, _ := ipn.Mask.Size()
			if ip.Is4() && len(ipn.Mask) == 16 {
				bits -= 96
			}
			if meshSpace.Contains(ip) {
				continue
			}
			out = append(out, netip.PrefixFrom(ip, bits))
		}
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].Addr().Is4() && !out[j].Addr().Is4() })
	return out, nil
}

// meshSpace is where cluster subnets live (docs/20 §3): never a candidate.
var meshSpace = netip.MustParsePrefix("100.64.0.0/10")
