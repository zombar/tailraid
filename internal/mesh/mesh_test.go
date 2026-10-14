package mesh

import (
	"context"
	"errors"
	"net"
	"net/netip"
	"testing"
	"time"

	"golang.zx2c4.com/wireguard/conn"
	"golang.zx2c4.com/wireguard/tun/tuntest"

	"github.com/zombar/tailraid/internal/clock"
)

// A test node: a device on a channel TUN with a real loopback socket.
type node struct {
	t    *testing.T
	m    *Mesh
	tun  *tuntest.ChannelTUN
	addr netip.Addr
	idx  uint32
	clk  *clock.Fake
}

var testCluster = [16]byte{1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12, 13, 14, 15, 16}

func newNode(t *testing.T, idx uint32, o Options) *node {
	t.Helper()
	key, err := GenerateKey()
	if err != nil {
		t.Fatal(err)
	}
	clk := clock.NewFake(time.Now())
	if o.Clock == nil {
		o.Clock = clk
	}
	if o.Keepalive == 0 {
		o.Keepalive = NoKeepalive // the tests send every handshake themselves
	}
	o.ClusterID = testCluster
	ct := tuntest.NewChannelTUN()
	m, err := New(ct.TUN(), key, o)
	if err != nil {
		t.Fatal(err)
	}
	n := &node{t: t, m: m, tun: ct, addr: netip.AddrFrom4([4]byte{100, 100, 0, byte(idx)}), idx: idx, clk: clk}
	t.Cleanup(m.Close)
	return n
}

// loopback is the node's socket as a candidate.
func (n *node) loopback() netip.AddrPort {
	return netip.AddrPortFrom(netip.MustParseAddr("127.0.0.1"), n.m.Port())
}

func (n *node) peer(cands ...netip.AddrPort) Peer {
	return Peer{Idx: n.idx, Key: n.m.Key(), Addr: n.addr, Candidates: cands}
}

// ping sends one ICMP packet from a's TUN to b's address and waits for it on b's TUN.
func ping(t *testing.T, a, b *node, wait time.Duration) bool {
	t.Helper()
	pkt := tuntest.Ping(b.addr, a.addr)
	deadline := time.After(wait)
	select {
	case a.tun.Outbound <- pkt:
	case <-deadline:
		t.Fatal("the device did not read the TUN")
	}
	for {
		select {
		case got := <-b.tun.Inbound:
			if len(got) == len(pkt) {
				return true
			}
		case <-deadline:
			return false
		}
	}
}

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	tick := time.NewTicker(20 * time.Millisecond)
	defer tick.Stop()
	deadline := time.After(10 * time.Second)
	for {
		if cond() {
			return
		}
		select {
		case <-tick.C:
		case <-deadline:
			t.Fatalf("timed out waiting for %s", what)
		}
	}
}

func statusOf(t *testing.T, n *node, key Key) PeerStatus {
	t.Helper()
	peers, err := n.m.Peers()
	if err != nil {
		t.Fatal(err)
	}
	for i := range peers {
		if peers[i].Key == key {
			return peers[i]
		}
	}
	t.Fatalf("peer %s not on the device", key)
	return PeerStatus{}
}

// Invariant: I-317, I-323
func TestMesh_PeersExchangePackets(t *testing.T) {
	a, b := newNode(t, 1, Options{}), newNode(t, 2, Options{})
	if err := a.m.SetPeers([]Peer{b.peer(b.loopback())}); err != nil {
		t.Fatal(err)
	}
	if err := b.m.SetPeers([]Peer{a.peer(a.loopback())}); err != nil {
		t.Fatal(err)
	}
	if !ping(t, a, b, 10*time.Second) {
		t.Fatal("a's packet never reached b")
	}
	if !ping(t, b, a, 10*time.Second) {
		t.Fatal("b's packet never reached a")
	}
	st := statusOf(t, a, b.m.Key())
	if st.Fanout || st.Endpoint != b.loopback() || st.LastHandshake.IsZero() {
		t.Fatalf("a's view of b after the exchange: %+v", st)
	}
	if st.Class != PathWAN { // loopback is no interface subnet
		t.Fatalf("class %s, want wan", st.Class)
	}
	if c := a.m.Path(b.m.Key()); c != PathWAN {
		t.Fatalf("Path = %s", c)
	}
	if c := a.m.Path(Key{9}); c != PathNone {
		t.Fatalf("Path of a stranger = %s", c)
	}
	if st.Keepalive != 0 {
		t.Fatalf("the test device keeps alive every %s", st.Keepalive)
	}
	rx, tx := a.m.Counters()
	if rx == 0 || tx == 0 {
		t.Fatalf("counters %d %d", rx, tx)
	}
	// a removed peer's packets die at the tunnel (I-317)
	if err := b.m.SetPeers(nil); err != nil {
		t.Fatal(err)
	}
	if ping(t, a, b, 1500*time.Millisecond) {
		t.Fatal("b still delivers a's packets after removing a")
	}
	if peers, _ := b.m.Peers(); len(peers) != 0 {
		t.Fatalf("b still lists %d peers", len(peers))
	}
}

// Invariant: I-323
func TestBind_FanoutPinsOnFirstAuthenticatedPacket(t *testing.T) {
	a, b := newNode(t, 1, Options{}), newNode(t, 2, Options{})
	// a decoy candidate: a socket of ours that counts what it receives and answers nothing
	decoy, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}
	defer decoy.Close()
	counted := make(chan int, 1024)
	done := make(chan struct{})
	go func() {
		defer close(done)
		buf := make([]byte, 2048)
		for {
			n, _, err := decoy.ReadFromUDPAddrPort(buf)
			if err != nil {
				return
			}
			counted <- n
		}
	}()
	decoyAP := netip.MustParseAddrPort(decoy.LocalAddr().String())
	if err := a.m.SetPeers([]Peer{b.peer(decoyAP, b.loopback())}); err != nil {
		t.Fatal(err)
	}
	// b does not hold a yet, so nothing a sends is answered: a is fanning out
	if st := statusOf(t, a, b.m.Key()); !st.Fanout {
		t.Fatalf("before any answer a must fan out: %+v", st)
	}
	if err := b.m.SetPeers([]Peer{a.peer(a.loopback())}); err != nil {
		t.Fatal(err)
	}
	if !ping(t, a, b, 10*time.Second) {
		t.Fatal("the ping through the fan-out failed")
	}
	waitFor(t, "a to pin b", func() bool {
		st := statusOf(t, a, b.m.Key())
		return !st.Fanout && st.Endpoint == b.loopback()
	})
	// the decoy saw the handshake initiation (a fan-out) and nothing after the pin
	seen := 0
	drain := func() {
		for {
			select {
			case n := <-counted:
				seen++
				if n > 200 { // a handshake initiation is 148 bytes; data would be larger
					t.Fatalf("the decoy received a %d-byte packet: data was fanned out", n)
				}
			default:
				return
			}
		}
	}
	drain()
	if seen == 0 {
		t.Fatal("the decoy never saw the fan-out")
	}
	for range 5 {
		if !ping(t, a, b, 10*time.Second) {
			t.Fatal("a later ping failed")
		}
	}
	before := seen
	drain()
	if seen != before {
		t.Fatalf("the decoy received %d packets after the pin", seen-before)
	}
	_ = decoy.Close()
	<-done
}

// Invariant: I-323
func TestMesh_DeadPathReturnsToFanout(t *testing.T) {
	a, b := newNode(t, 1, Options{}), newNode(t, 2, Options{})
	if err := a.m.SetPeers([]Peer{b.peer(b.loopback())}); err != nil {
		t.Fatal(err)
	}
	if err := b.m.SetPeers([]Peer{a.peer(a.loopback())}); err != nil {
		t.Fatal(err)
	}
	if !ping(t, a, b, 10*time.Second) {
		t.Fatal("ping")
	}
	waitFor(t, "the pin", func() bool { return !statusOf(t, a, b.m.Key()).Fanout })
	if err := a.m.Tick(); err != nil {
		t.Fatal(err)
	}
	if st := statusOf(t, a, b.m.Key()); st.Fanout {
		t.Fatal("a fresh handshake must keep its pin")
	}
	a.clk.Advance(PathDead + time.Second)
	if err := a.m.Tick(); err != nil {
		t.Fatal(err)
	}
	st := statusOf(t, a, b.m.Key())
	if !st.Fanout || st.Class != PathNone {
		t.Fatalf("after %s without a handshake: %+v", PathDead, st)
	}
	// a membership refresh keeps the fan-out and the candidates; the next packet pins again
	if err := a.m.SetPeers([]Peer{b.peer(b.loopback())}); err != nil {
		t.Fatal(err)
	}
	if !ping(t, a, b, 10*time.Second) {
		t.Fatal("ping after the fan-out")
	}
	// the session is still good, so nothing comes back until b speaks: its next packet
	// (here a ping; on a node the keepalive within 25 s) is the authenticated one that pins
	if !ping(t, b, a, 10*time.Second) {
		t.Fatal("ping back after the fan-out")
	}
	waitFor(t, "the second pin", func() bool { return !statusOf(t, a, b.m.Key()).Fanout })
}

// Invariant: I-323
func TestBind_FanoutWithoutCandidates(t *testing.T) {
	a := newNode(t, 1, Options{})
	b := newNode(t, 2, Options{})
	if err := a.m.SetPeers([]Peer{b.peer()}); err != nil {
		t.Fatal(err)
	}
	st := statusOf(t, a, b.m.Key())
	if st.Fanout || st.Endpoint.IsValid() || st.Class != PathNone {
		t.Fatalf("a peer without candidates has no endpoint: %+v", st)
	}
	if err := a.m.Tick(); err != nil {
		t.Fatal(err)
	}
	if err := a.m.bind.Send([][]byte{{1}}, &fanout{idx: 99}); !errors.Is(err, errNoCandidate) {
		t.Fatalf("send to an unknown fan-out: %v", err)
	}
	if _, err := a.m.bind.ParseEndpoint("fanout:x"); err == nil {
		t.Fatal("a bad fan-out endpoint parsed")
	}
	ep, err := a.m.bind.ParseEndpoint("fanout:7")
	if err != nil || ep.DstToString() != "fanout:7" || ep.DstIP().IsValid() || ep.SrcToString() != "" || len(ep.DstToBytes()) != 4 {
		t.Fatalf("fanout endpoint: %v %v", ep, err)
	}
	if _, err := a.m.bind.ParseEndpoint("127.0.0.1:1"); err != nil {
		t.Fatal(err)
	}
	if _, ok := any(a.m.bind).(conn.Bind); !ok {
		t.Fatal("Bind is not a conn.Bind")
	}
}

// Invariant: I-325
func TestClassify_Table(t *testing.T) {
	now := time.Now()
	local := []netip.Prefix{netip.MustParsePrefix("192.168.1.0/24"), netip.MustParsePrefix("2001:db8::/64")}
	lan := netip.MustParseAddrPort("192.168.1.9:20483")
	wan := netip.MustParseAddrPort("203.0.113.5:20483")
	v6 := netip.MustParseAddrPort("[2001:db8::7]:20483")
	cases := []struct {
		name string
		p    PeerStatus
		want PathClass
	}{
		{"lan fresh", PeerStatus{Endpoint: lan, LastHandshake: now.Add(-time.Minute)}, PathLAN},
		{"lan v6", PeerStatus{Endpoint: v6, LastHandshake: now}, PathLAN},
		{"wan fresh", PeerStatus{Endpoint: wan, LastHandshake: now.Add(-time.Minute)}, PathWAN},
		{"stale", PeerStatus{Endpoint: lan, LastHandshake: now.Add(-PathDead - time.Second)}, PathNone},
		{"at the edge", PeerStatus{Endpoint: lan, LastHandshake: now.Add(-PathDead)}, PathLAN},
		{"old handshake, recent packet", PeerStatus{Endpoint: lan, LastHandshake: now.Add(-3 * time.Minute), LastRx: now.Add(-20 * time.Second)}, PathLAN},
		{"old handshake, old packet", PeerStatus{Endpoint: wan, LastHandshake: now.Add(-3 * time.Minute), LastRx: now.Add(-PathDead - time.Second)}, PathNone},
		{"packet only", PeerStatus{Endpoint: wan, LastRx: now}, PathWAN},
		{"never", PeerStatus{Endpoint: lan}, PathNone},
		{"fanning out", PeerStatus{Fanout: true, LastHandshake: now}, PathNone},
		{"no endpoint", PeerStatus{LastHandshake: now}, PathNone},
	}
	for _, tc := range cases {
		if got := classify(tc.p, now, PathDead, local); got != tc.want {
			t.Errorf("%s: %s, want %s", tc.name, got, tc.want)
		}
	}
	for _, c := range []PathClass{PathLAN, PathWAN, PathRelay, PathNone, PathClass(99)} {
		if c.String() == "" {
			t.Errorf("%d has no name", c)
		}
	}
}

// Invariant: I-322
func TestLocalEndpoints_NeverLoopbackOrMesh(t *testing.T) {
	eps, err := LocalEndpoints(20483)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range eps {
		a := e.Addr()
		if a.IsLoopback() || a.IsLinkLocalUnicast() || meshSpace.Contains(a) || e.Port() != 20483 {
			t.Errorf("candidate %s", e)
		}
	}
}

// Invariant: I-319, I-321
func TestIntroduce_EndToEnd(t *testing.T) {
	token := NewToken()
	want := netip.MustParsePrefix("100.100.0.7/24")
	intro := &fakeIntroducer{addr: want}
	b := newNode(t, 2, Options{TokenKey: func() []byte { return IntroduceKey(token, testCluster) }, Introducer: intro})
	a := newNode(t, 1, Options{})
	code := JoinCode{ClusterID: testCluster, Token: token, Key: b.m.Key(), Host: 2, Port: b.m.Port(), RPCPort: 20481, Endpoints: []string{"127.0.0.1:1", b.loopback().String()}}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	got, err := a.m.Introduce(ctx, code)
	if err != nil {
		t.Fatal(err)
	}
	if got != want {
		t.Fatalf("introduced as %s, want %s", got, want)
	}
	if k := intro.last(); k != a.m.Key() {
		t.Fatalf("the member introduced %s, want %s", k, a.m.Key())
	}
	// a wrong token: silence, and the joiner times out on its own clock
	code.Token = NewToken()
	errc := make(chan error, 1)
	go func() {
		_, err := a.m.Introduce(ctx, code)
		errc <- err
	}()
	waitFor(t, "the member to hold the source", func() bool {
		b.m.intro.mu.Lock()
		defer b.m.intro.mu.Unlock()
		return len(b.m.intro.held) == 1
	})
	a.clk.Advance(IntroduceTimeout + time.Second)
	select {
	case err := <-errc:
		if !errors.Is(err, ErrIntroduceTimeout) {
			t.Fatalf("wrong token: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the introduce did not time out")
	}
}

// Invariant: I-323
func TestMesh_KeepaliveIsOnByDefault(t *testing.T) {
	a, b := newNode(t, 1, Options{Keepalive: Keepalive}), newNode(t, 2, Options{})
	if err := a.m.SetPeers([]Peer{b.peer(b.loopback())}); err != nil {
		t.Fatal(err)
	}
	if st := statusOf(t, a, b.m.Key()); st.Keepalive != Keepalive {
		t.Fatalf("keepalive %s, want %s", st.Keepalive, Keepalive)
	}
}

// A peer whose row arrived before its endpoints (a joiner publishes them from its first
// tick) is sent nothing until they come, and fans out at the monitor's next tick once
// they do: the device does not wait for the dead-path window to notice a peer it never
// had an endpoint for.
// Invariant: I-322, I-323
func TestMesh_CandidatesArrivingLaterFanOut(t *testing.T) {
	a := newNode(t, 1, Options{})
	b := newNode(t, 2, Options{})
	if err := a.m.SetPeers([]Peer{b.peer()}); err != nil { // the row, no endpoints yet
		t.Fatal(err)
	}
	if err := b.m.SetPeers([]Peer{a.peer(a.loopback())}); err != nil {
		t.Fatal(err)
	}
	if st := statusOf(t, a, b.m.Key()); st.Fanout || st.Endpoint.IsValid() {
		t.Fatalf("a peer without candidates has an endpoint: %+v", st)
	}
	if err := a.m.SetPeers([]Peer{b.peer(b.loopback())}); err != nil { // the endpoints published
		t.Fatal(err)
	}
	if err := a.m.Tick(); err != nil { // the monitor sees a peer with candidates and no endpoint
		t.Fatal(err)
	}
	if st := statusOf(t, a, b.m.Key()); !st.Fanout {
		t.Fatalf("the peer does not fan out once it has candidates: %+v", st)
	}
	if !ping(t, a, b, 10*time.Second) {
		t.Fatal("no packet reached b once its endpoints were known")
	}
	if st := statusOf(t, a, b.m.Key()); st.Fanout || st.Endpoint != b.loopback() {
		t.Fatalf("a did not pin b: %+v", st)
	}
}
