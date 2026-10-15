package mesh

import (
	"bytes"
	"log/slog"
	"net/netip"
	"sync"
	"testing"
	"time"

	"golang.zx2c4.com/wireguard/conn"
	"golang.zx2c4.com/wireguard/tun/tuntest"

	"github.com/zombar/tailraid/internal/clock"
)

// testClusterKey is the cluster key every keyed test node holds (docs/19 §3).
var testClusterKey = bytes.Repeat([]byte{0x5a}, 32)

func clusterKey() []byte { return testClusterKey }

// Invariant: I-328, I-329
func TestRelay_Codecs_Table(t *testing.T) {
	key := relayKey(testClusterKey)
	now := time.Unix(1_800_000_000, 0)
	from, to := Key{1}, Key{2}
	payload := []byte("wireguard packet")
	good := encodeRelay(key, from, to, now.Unix(), payload)
	if len(good) != relayHeaderLen+len(payload) {
		t.Fatalf("frame length %d", len(good))
	}
	hdr, got, ok := decodeRelay(key, good, now)
	if !ok || hdr.from != from || hdr.to != to || !bytes.Equal(got, payload) {
		t.Fatalf("decode: %+v %q %v", hdr, got, ok)
	}
	flipped := bytes.Clone(good)
	flipped[len(flipped)-1] ^= 1 // the payload is under the MAC too
	short := good[:relayHeaderLen]
	cases := []struct {
		name  string
		key   []byte
		frame []byte
		at    time.Time
	}{
		{"wrong key", relayKey(bytes.Repeat([]byte{1}, 32)), good, now},
		{"no key", nil, good, now},
		{"payload changed", key, flipped, now},
		{"no payload", key, short, now},
		{"too old", key, good, now.Add(RelaySkew + time.Second)},
		{"from the future", key, good, now.Add(-RelaySkew - time.Second)},
		{"a probe", key, encodeProbe(key, probeAsk, from, to, now.Unix()), now},
	}
	for _, c := range cases {
		if _, _, ok := decodeRelay(c.key, c.frame, c.at); ok {
			t.Errorf("%s: accepted", c.name)
		}
	}
	probe := encodeProbe(key, probeAsk, from, to, now.Unix())
	if k, f, t2, ok := decodeProbe(key, probe, now); !ok || k != probeAsk || f != from || t2 != to || len(probe) != probeLen {
		t.Fatalf("probe decode: %d %s %s %v (len %d)", k, f, t2, ok, len(probe))
	}
	retargeted := bytes.Clone(probe)
	retargeted[41] ^= 1 // the probed device's key is under the MAC
	if _, _, _, ok := decodeProbe(key, retargeted, now); ok {
		t.Error("a probe with its target changed was accepted")
	}
	echoed := bytes.Clone(probe)
	echoed[1] = probeEcho // the kind is under the MAC too: a reflector cannot turn a probe into an echo
	if _, _, _, ok := decodeProbe(key, echoed, now); ok {
		t.Error("a probe with its kind flipped was accepted")
	}
	if _, _, _, ok := decodeProbe(key, encodeProbe(key, 7, from, to, now.Unix()), now); ok {
		t.Error("an unknown kind was accepted")
	}
	if _, _, _, ok := decodeProbe(key, good, now); ok {
		t.Error("a relay frame decoded as a probe")
	}
	for _, c := range []struct {
		name  string
		key   []byte
		frame []byte
		at    time.Time
	}{
		{"wrong key", relayKey(bytes.Repeat([]byte{1}, 32)), probe, now},
		{"bit flipped", key, append(bytes.Clone(probe[:probeLen-1]), probe[probeLen-1]^1), now},
		{"too old", key, probe, now.Add(RelaySkew + time.Second)},
		{"a relay frame", key, good, now},
	} {
		if _, _, _, ok := decodeProbe(c.key, c.frame, c.at); ok {
			t.Errorf("probe %s: accepted", c.name)
		}
	}
	ep := &relayEP{via: netip.MustParseAddrPort("203.0.113.9:20483"), peer: to}
	back, ok := parseRelayEndpoint(ep.DstToString())
	if !ok || back.via != ep.via || back.peer != ep.peer || !IsRelay(ep.DstToString()) {
		t.Fatalf("endpoint round trip: %+v %v", back, ok)
	}
	if _, ok := parseRelayEndpoint("relay:nonsense"); ok {
		t.Fatal("a malformed relay endpoint parsed")
	}
}

// recInner is a conn.Bind that records what it sends.
type recInner struct {
	mu   sync.Mutex
	sent []struct {
		to  string
		buf []byte
	}
}

func (r *recInner) Open(uint16) ([]conn.ReceiveFunc, uint16, error) { return nil, 0, nil }
func (r *recInner) Close() error                                    { return nil }
func (r *recInner) SetMark(uint32) error                            { return nil }
func (r *recInner) BatchSize() int                                  { return 1 }
func (r *recInner) ParseEndpoint(s string) (conn.Endpoint, error) {
	ap, err := netip.ParseAddrPort(s)
	if err != nil {
		return nil, err
	}
	return &conn.StdNetEndpoint{AddrPort: ap}, nil
}

func (r *recInner) Send(bufs [][]byte, ep conn.Endpoint) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, b := range bufs {
		r.sent = append(r.sent, struct {
			to  string
			buf []byte
		}{ep.DstToString(), bytes.Clone(b)})
	}
	return nil
}

func (r *recInner) count() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.sent)
}

// A relay forwards a frame only between two peers it holds direct sessions with, only
// with a valid MAC, unchanged, and within its rate; a frame for itself is handed to
// WireGuard from a relay endpoint through the sender; a probe for itself matches the
// candidate it probed, any other verified probe is echoed.
// Invariant: I-328, I-329
func TestBind_RelayForwardRules(t *testing.T) {
	inner := &recInner{}
	clk := newFakeClock()
	rs := newRelayState(clk)
	self, a, b, stranger := Key{9}, Key{1}, Key{2}, Key{3}
	rs.self = self
	rs.key = relayKey(testClusterKey)
	rs.limit = newRateLimiter(1000, clk)
	bind := newBind(inner, discardLog(), rs)
	aEP, bEP := netip.MustParseAddrPort("10.0.0.1:1"), netip.MustParseAddrPort("10.0.0.2:2")
	if err := bind.setForward(map[Key]netip.AddrPort{a: aEP, b: bEP}); err != nil {
		t.Fatal(err)
	}
	from := &conn.StdNetEndpoint{AddrPort: aEP}
	now := clk.Now().Unix()
	frame := encodeRelay(rs.key, a, b, now, bytes.Repeat([]byte{7}, 100))
	if _, _, ok := bind.relayIn(frame, from); ok || inner.count() != 1 || !bytes.Equal(inner.sent[0].buf, frame) || inner.sent[0].to != bEP.String() {
		t.Fatalf("a frame between two sessions: handed in %v, sent %d", ok, inner.count())
	}
	if got := rs.Relayed()[[2]Key{a, b}]; got != uint64(len(frame)) {
		t.Fatalf("relayed bytes %d, want %d", got, len(frame))
	}
	for _, c := range []struct {
		name  string
		frame []byte
	}{
		{"to a stranger", encodeRelay(rs.key, a, stranger, now, []byte("x"))},
		{"from a stranger", encodeRelay(rs.key, stranger, b, now, []byte("x"))},
		{"bad mac", encodeRelay(relayKey(bytes.Repeat([]byte{1}, 32)), a, b, now, []byte("x"))},
		{"stale", encodeRelay(rs.key, a, b, now-int64(RelaySkew.Seconds())-5, []byte("x"))},
	} {
		if _, _, ok := bind.relayIn(c.frame, from); ok || inner.count() != 1 {
			t.Errorf("%s: handed in %v, sent %d", c.name, ok, inner.count())
		}
	}
	// the rate: 1000 bytes a second, 100 of it spent; the next big frame waits
	big := encodeRelay(rs.key, a, b, now, bytes.Repeat([]byte{7}, 900))
	if _, _, ok := bind.relayIn(big, from); ok || inner.count() != 1 {
		t.Fatalf("over the rate: handed in %v, sent %d", ok, inner.count())
	}
	clk.Advance(2 * time.Second)
	if _, _, ok := bind.relayIn(big, from); ok || inner.count() != 2 {
		t.Fatalf("after the refill: handed in %v, sent %d", ok, inner.count())
	}
	// for this device: the payload goes in from the relay endpoint through the sender
	mine := encodeRelay(rs.key, a, self, clk.Now().Unix(), []byte("to me"))
	payload, ep, ok := bind.relayIn(mine, from)
	rep, isRelay := ep.(*relayEP)
	if !ok || string(payload) != "to me" || !isRelay || rep.via != aEP || rep.peer != a {
		t.Fatalf("a frame for this device: %q %+v %v", payload, ep, ok)
	}
	// a probe from a peer for this device is answered with an echo under this device's
	// MAC, one for another device is dropped; the echo of this device's own probe pins
	// the probed candidate, the probe itself reflected from the candidate pins nothing (a
	// candidate that is this device's own address, or a UDP echo service), and an echo
	// nobody waits for does nothing
	bind.probeIn(encodeProbe(rs.key, probeAsk, a, b, clk.Now().Unix()), from)
	if inner.count() != 2 {
		t.Fatalf("a probe for another device was answered: %d sends", inner.count())
	}
	probe := encodeProbe(rs.key, probeAsk, a, self, clk.Now().Unix())
	bind.probeIn(probe, from)
	if k, p, tg, ok := decodeProbe(rs.key, inner.sent[2].buf, clk.Now()); inner.count() != 3 || !ok || k != probeEcho || p != a || tg != self || inner.sent[2].to != aEP.String() {
		t.Fatalf("probe echo: %d sends, kind %d ok %v", inner.count(), k, ok)
	}
	var pinned []string
	rs.onProbe = func(peer Key, to netip.AddrPort) { pinned = append(pinned, peer.String()+"@"+to.String()) }
	cand := netip.MustParseAddrPort("10.0.0.5:5")
	if err := bind.Probe(cand, b); err != nil {
		t.Fatal(err)
	}
	sent := inner.sent[3].buf
	bind.probeIn(sent, &conn.StdNetEndpoint{AddrPort: cand}) // the probe itself reflected: no echo, no pin
	if len(pinned) != 0 || inner.count() != 4 {
		t.Fatalf("a reflected probe pinned or was answered: %v, %d sends", pinned, inner.count())
	}
	echo := encodeProbe(rs.key, probeEcho, self, b, clk.Now().Unix())
	bind.probeIn(echo, &conn.StdNetEndpoint{AddrPort: netip.MustParseAddrPort("10.0.0.6:6")})                     // from elsewhere: no match
	bind.probeIn(encodeProbe(rs.key, probeEcho, self, a, clk.Now().Unix()), &conn.StdNetEndpoint{AddrPort: cand}) // names another device: no match
	bind.probeIn(echo, &conn.StdNetEndpoint{AddrPort: cand})
	bind.probeIn(echo, &conn.StdNetEndpoint{AddrPort: cand}) // once
	if len(pinned) != 1 || pinned[0] != b.String()+"@"+cand.String() {
		t.Fatalf("pinned %v", pinned)
	}
	if err := bind.Probe(cand, b); err != nil {
		t.Fatal(err)
	}
	clk.Advance(probeWindow + time.Second)
	bind.probeIn(encodeProbe(rs.key, probeEcho, self, b, clk.Now().Unix()), &conn.StdNetEndpoint{AddrPort: cand})
	if len(pinned) != 1 {
		t.Fatalf("a late echo pinned: %v", pinned)
	}
	// without the key nothing is forwarded, echoed or sent through a relay
	rs.mu.Lock()
	rs.key = nil
	rs.mu.Unlock()
	if _, _, ok := bind.relayIn(frame, from); ok || inner.count() != 5 {
		t.Fatalf("without the key: handed in %v, sent %d", ok, inner.count())
	}
	if err := bind.Send([][]byte{[]byte("pkt")}, &relayEP{via: aEP, peer: b}); err == nil {
		t.Fatal("a send through a relay without the key")
	}
}

// Two devices behind symmetric NATs, which cannot punch (TestNAT_SymmetricToSymmetric_
// NeedsARelay), reach each other through a member both hold a direct session with: the
// relay candidates join after RelayAfter, the relay forwards both ways and counts it,
// and both sides see the path as relayed.
// Invariant: I-328
func TestNAT_SymmetricToSymmetric_ThroughARelay(t *testing.T) {
	a, natA := nattedWith(t, 1, true, Options{ClusterKey: clusterKey})
	b, natB := nattedWith(t, 2, true, Options{ClusterKey: clusterKey})
	c := newNode(t, 3, Options{ClusterKey: clusterKey})
	cAddr := c.loopback().String()
	if err := c.m.SetPeers([]Peer{a.peer(natA.Reflexive(cAddr)), b.peer(natB.Reflexive(cAddr))}); err != nil {
		t.Fatal(err)
	}
	third := "127.0.0.1:9"
	if err := a.m.SetPeers([]Peer{b.peer(natB.Reflexive(third)), c.peer(c.loopback())}); err != nil {
		t.Fatal(err)
	}
	if err := b.m.SetPeers([]Peer{a.peer(natA.Reflexive(third)), c.peer(c.loopback())}); err != nil {
		t.Fatal(err)
	}
	for _, p := range [][2]*node{{a, c}, {c, a}, {b, c}, {c, b}} {
		if !ping(t, p[0], p[1], 10*time.Second) {
			t.Fatalf("no direct path %d→%d", p[0].idx, p[1].idx)
		}
	}
	if ping(t, a, b, 2*time.Second) {
		t.Fatal("two symmetric NATs punched")
	}
	// c's forwarding table holds a and b; a's and b's fan-out to each other has failed
	// for RelayAfter, so each gets the candidate via c
	for _, n := range []*node{a, b, c} {
		if err := n.m.Tick(); err != nil {
			t.Fatal(err)
		}
	}
	a.clk.Advance(RelayAfter)
	b.clk.Advance(RelayAfter)
	for _, n := range []*node{a, b} {
		if err := n.m.Tick(); err != nil {
			t.Fatal(err)
		}
	}
	if !ping(t, a, b, 10*time.Second) || !ping(t, b, a, 10*time.Second) {
		t.Fatal("no path through the relay")
	}
	for _, p := range [][2]*node{{a, b}, {b, a}} {
		st := statusOf(t, p[0], p[1].m.Key())
		if !st.Relay || st.Class != PathRelay || st.Endpoint != c.loopback() {
			t.Fatalf("%d's view of %d: %+v, want relayed through c", p[0].idx, p[1].idx, st)
		}
	}
	rel := c.m.Relayed()
	if rel[[2]Key{a.m.Key(), b.m.Key()}] == 0 || rel[[2]Key{b.m.Key(), a.m.Key()}] == 0 {
		t.Fatalf("relayed bytes %v", rel)
	}
	if ab := c.m.Relayed()[[2]Key{a.m.Key(), b.m.Key()}]; ab < relayHeaderLen+32 {
		t.Fatalf("relayed %d bytes a→b: less than one handshake", ab)
	}
}

// A device on a relayed path probes its direct candidates every RelayRetryDirect and
// moves to the one that answers; a peer whose row names no endpoint reaches it through
// the relay alone until then.
// Invariant: I-328, I-329
func TestRelay_LeavesForADirectPath(t *testing.T) {
	a := newNode(t, 1, Options{ClusterKey: clusterKey})
	b := newNode(t, 2, Options{ClusterKey: clusterKey})
	c := newNode(t, 3, Options{ClusterKey: clusterKey})
	if err := c.m.SetPeers([]Peer{a.peer(a.loopback()), b.peer(b.loopback())}); err != nil {
		t.Fatal(err)
	}
	if err := a.m.SetPeers([]Peer{b.peer(), c.peer(c.loopback())}); err != nil { // b's endpoints unknown
		t.Fatal(err)
	}
	if err := b.m.SetPeers([]Peer{a.peer(), c.peer(c.loopback())}); err != nil {
		t.Fatal(err)
	}
	for _, p := range [][2]*node{{a, c}, {c, a}, {b, c}, {c, b}} {
		if !ping(t, p[0], p[1], 10*time.Second) {
			t.Fatalf("no direct path %d→%d", p[0].idx, p[1].idx)
		}
	}
	for _, n := range []*node{a, b, c} {
		if err := n.m.Tick(); err != nil {
			t.Fatal(err)
		}
	}
	a.clk.Advance(RelayAfter)
	if err := a.m.Tick(); err != nil {
		t.Fatal(err)
	}
	if !ping(t, a, b, 10*time.Second) || !ping(t, b, a, 10*time.Second) {
		t.Fatal("no path through the relay")
	}
	if st := statusOf(t, a, b.m.Key()); !st.Relay || st.Class != PathRelay {
		t.Fatalf("a's view of b: %+v, want relayed", st)
	}
	// b's endpoint is published: the pinned relay path stays until a probe of the direct
	// candidate is answered; the monitor probes at its next tick and every
	// RelayRetryDirect after, and a moves the moment b echoes
	if err := a.m.SetPeers([]Peer{b.peer(b.loopback()), c.peer(c.loopback())}); err != nil {
		t.Fatal(err)
	}
	if st := statusOf(t, a, b.m.Key()); !st.Relay {
		t.Fatalf("a left the relay without a probe: %+v", st)
	}
	if err := a.m.Tick(); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "a direct path to b", func() bool {
		st := statusOf(t, a, b.m.Key())
		return !st.Relay && st.Endpoint == b.loopback()
	})
	if !ping(t, a, b, 10*time.Second) {
		t.Fatal("no packet on the direct path")
	}
	if st := statusOf(t, a, b.m.Key()); st.Class != PathWAN { // loopback is no interface subnet
		t.Fatalf("a's view of b after the move: %+v", st)
	}
}

// probeTargets picks the lan candidates of a wan path every ProbeEvery, every direct
// candidate of a relayed path every RelayRetryDirect, nothing for a lan path or
// without the cluster key.
// Invariant: I-329
func TestProbeTargets_Table(t *testing.T) {
	local := []netip.Prefix{netip.MustParsePrefix("192.168.0.0/16")}
	lan, lan2, wan := netip.MustParseAddrPort("192.168.1.5:20483"), netip.MustParseAddrPort("192.168.1.6:20483"), netip.MustParseAddrPort("203.0.113.5:20483")
	keyed := newNode(t, 1, Options{ClusterKey: clusterKey, Local: local})
	keyless := newNode(t, 2, Options{Local: local})
	row := Peer{Key: Key{2}, Candidates: []netip.AddrPort{lan, wan, lan2}}
	cases := []struct {
		name  string
		m     *Mesh
		p     PeerStatus
		want  []netip.AddrPort
		every time.Duration
	}{
		{"wan path: its lan candidates", keyed.m, PeerStatus{Endpoint: wan, Class: PathWAN}, []netip.AddrPort{lan, lan2}, ProbeEvery},
		{"relayed: every direct candidate", keyed.m, PeerStatus{Endpoint: wan, Relay: true, Class: PathRelay}, []netip.AddrPort{lan, wan, lan2}, RelayRetryDirect},
		{"lan path: nothing", keyed.m, PeerStatus{Endpoint: lan, Class: PathLAN}, nil, 0},
		{"none: nothing", keyed.m, PeerStatus{Class: PathNone}, nil, 0},
		{"no key: nothing", keyless.m, PeerStatus{Endpoint: wan, Class: PathWAN}, nil, 0},
	}
	for _, c := range cases {
		got, every := c.m.probeTargets(&c.p, row)
		if len(got) != len(c.want) || every != c.every {
			t.Errorf("%s: %v every %s, want %v every %s", c.name, got, every, c.want, c.every)
			continue
		}
		for i := range got {
			if got[i] != c.want[i] {
				t.Errorf("%s: %v, want %v", c.name, got, c.want)
			}
		}
	}
}

func newFakeClock() *clock.Fake { return clock.NewFake(time.Now()) }

func discardLog() *slog.Logger { return slog.New(slog.DiscardHandler) }

// A probe is answered only by the device it names: one that lands on another member
// gets silence, so a candidate that is really another member (a relay's address a
// member mistook for the caller's) pins nothing, and an echo pins only the device the
// probe was for.
// Invariant: I-329
func TestBind_ProbeAnsweredByItsTargetOnly(t *testing.T) {
	a := newNode(t, 1, Options{ClusterKey: clusterKey})
	b := newNode(t, 2, Options{ClusterKey: clusterKey})
	c := newNode(t, 3, Options{ClusterKey: clusterKey})
	for _, n := range []*node{a, b, c} {
		others := []Peer{}
		for _, o := range []*node{a, b, c} {
			if o != n {
				others = append(others, o.peer(o.loopback()))
			}
		}
		if err := n.m.SetPeers(others); err != nil {
			t.Fatal(err)
		}
	}
	for _, p := range [][2]*node{{a, b}, {a, c}} {
		if !ping(t, p[0], p[1], 10*time.Second) {
			t.Fatalf("no direct path %d→%d", p[0].idx, p[1].idx)
		}
	}
	var pinned []string
	var mu sync.Mutex
	a.m.relay.mu.Lock()
	a.m.relay.onProbe = func(peer Key, from netip.AddrPort) {
		mu.Lock()
		pinned = append(pinned, peer.String()+"@"+from.String())
		mu.Unlock()
	}
	a.m.relay.mu.Unlock()
	// a probes c's socket as if it were b's: c is not b, so c answers nothing
	if err := a.m.bind.Probe(c.loopback(), b.m.Key()); err != nil {
		t.Fatal(err)
	}
	// a probes c's socket for c: c answers and a pins c there
	if err := a.m.bind.Probe(c.loopback(), c.m.Key()); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "c's echo", func() bool {
		mu.Lock()
		defer mu.Unlock()
		return len(pinned) > 0
	})
	mu.Lock()
	defer mu.Unlock()
	if len(pinned) != 1 || pinned[0] != c.m.Key().String()+"@"+c.loopback().String() {
		t.Fatalf("pinned %v, want c alone at its socket", pinned)
	}
}

// A pin holds against a peer that still sends through the relay: the device sends a
// keepalive on the new path at once, the peer roams to the direct address it came from,
// and the pair is direct both ways; without it the peer's next relayed packet would roam
// the device back to the relay and the pin would be lost every probe interval.
// Invariant: I-329
func TestRelay_PinSticksWhilePeerStillRelays(t *testing.T) {
	a := newNode(t, 1, Options{ClusterKey: clusterKey})
	b := newNode(t, 2, Options{ClusterKey: clusterKey})
	c := newNode(t, 3, Options{ClusterKey: clusterKey})
	if err := c.m.SetPeers([]Peer{a.peer(a.loopback()), b.peer(b.loopback())}); err != nil {
		t.Fatal(err)
	}
	if err := a.m.SetPeers([]Peer{b.peer(), c.peer(c.loopback())}); err != nil { // b's endpoints unknown yet
		t.Fatal(err)
	}
	if err := b.m.SetPeers([]Peer{a.peer(), c.peer(c.loopback())}); err != nil { // and b never learns a's
		t.Fatal(err)
	}
	for _, p := range [][2]*node{{a, c}, {c, a}, {b, c}, {c, b}} {
		if !ping(t, p[0], p[1], 10*time.Second) {
			t.Fatalf("no direct path %d→%d", p[0].idx, p[1].idx)
		}
	}
	for _, n := range []*node{a, b, c} {
		if err := n.m.Tick(); err != nil {
			t.Fatal(err)
		}
	}
	a.clk.Advance(RelayAfter)
	b.clk.Advance(RelayAfter)
	for _, n := range []*node{a, b} {
		if err := n.m.Tick(); err != nil {
			t.Fatal(err)
		}
	}
	if !ping(t, a, b, 10*time.Second) || !ping(t, b, a, 10*time.Second) {
		t.Fatal("no path through the relay")
	}
	// b keeps sending to a through the relay the whole time, as a peer with traffic does
	stop := make(chan struct{})
	var sender sync.WaitGroup
	sender.Go(func() {
		tick := time.NewTicker(20 * time.Millisecond)
		defer tick.Stop()
		for {
			select {
			case <-stop:
				return
			case <-tick.C:
				select {
				case b.tun.Outbound <- tuntest.Ping(a.addr, b.addr):
				case <-stop:
					return
				}
			case <-a.tun.Inbound: // drained, so the device never blocks on it
			}
		}
	})
	defer sender.Wait()
	defer close(stop)
	// a learns b's address and probes it; b knows nothing of a's and keeps relaying
	if err := a.m.SetPeers([]Peer{b.peer(b.loopback()), c.peer(c.loopback())}); err != nil {
		t.Fatal(err)
	}
	if err := a.m.Tick(); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "b to learn a's direct address from the keepalive", func() bool {
		st := statusOf(t, b, a.m.Key())
		return !st.Relay && st.Endpoint == a.loopback()
	})
	waitFor(t, "a's pin to hold", func() bool {
		st := statusOf(t, a, b.m.Key())
		return !st.Relay && st.Endpoint == b.loopback()
	})
}
