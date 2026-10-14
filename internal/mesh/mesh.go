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
	Endpoint      netip.AddrPort // the pinned endpoint; zero while fanning out or unknown
	Fanout        bool
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

	mu    sync.Mutex
	peers map[Key]Peer
	local []netip.Prefix // this host's own subnets, for the LAN class
	seen  map[Key]rxSeen // per peer: the received bytes last read and when they last grew
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
	m := &Mesh{bind: newBind(inner, log), log: log, clk: clk, key: key, dead: o.PathDead, ka: o.Keepalive, peers: map[Key]Peer{}, seen: map[Key]rxSeen{}}
	if m.dead == 0 {
		m.dead = PathDead
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
			if len(old.Candidates) == 0 && len(p.Candidates) > 0 {
				// a row that arrived before its endpoints (a joiner publishes them from its
				// first tick): the device had nowhere to send, so it fans out now
				sb.WriteString("endpoint=" + (&fanout{idx: p.Idx}).DstToString() + "\n")
			}
			continue // the pinned endpoint stays; the monitor decides when to fan out again
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
	for _, pfx := range local {
		if pfx.Contains(p.Endpoint.Addr().Unmap()) {
			return PathLAN
		}
	}
	return PathWAN
}

// Tick is the path monitor: a peer not heard from for PathDead (no packet received, no
// handshake), or never, is sent back to fan-out so its candidates are tried again
// (I-323); the host's own subnets are re-read for the LAN class.
func (m *Mesh) Tick() error {
	m.refreshLocal()
	peers, err := m.Peers()
	if err != nil {
		return err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	var sb strings.Builder
	for i := range peers {
		p := &peers[i]
		if p.Fanout {
			continue
		}
		row, ok := m.peers[p.Key]
		if !ok || len(row.Candidates) == 0 {
			continue
		}
		if !p.Endpoint.IsValid() { // never had an endpoint: its candidates came after the row
			sb.WriteString("public_key=" + p.Key.Hex() + "\nupdate_only=true\nendpoint=" + (&fanout{idx: row.Idx}).DstToString() + "\n")
			continue
		}
		if seen := p.LastSeen(); !seen.IsZero() && m.clk.Now().Sub(seen) <= m.dead {
			continue
		}
		sb.WriteString("public_key=" + p.Key.Hex() + "\nupdate_only=true\nendpoint=" + (&fanout{idx: row.Idx}).DstToString() + "\n")
	}
	if sb.Len() == 0 {
		return nil
	}
	if err := m.dev.IpcSet(sb.String()); err != nil {
		return fmt.Errorf("mesh: fan out again: %w", err)
	}
	return nil
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
