package node

import (
	"cmp"
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/netip"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"github.com/zombar/tailraid/internal/admin"
	"github.com/zombar/tailraid/internal/clock"
	"github.com/zombar/tailraid/internal/cluster"
	"github.com/zombar/tailraid/internal/config"
	"github.com/zombar/tailraid/internal/mesh"
	"github.com/zombar/tailraid/internal/mesh/portmap"
	"github.com/zombar/tailraid/internal/meta"
	"github.com/zombar/tailraid/internal/rpc"
)

// The node's attachment to the cluster (docs/20): the mesh, one wireguard-go device on a
// TUN whose peers are the member rows, or, under dev.mesh_fake, plain addresses for the
// CLI tests, which cannot open a TUN. Either way the node binds Raft, the RPC and the
// browser UI to its address and authorizes callers by the row at theirs.

// MeshFile is the file serve writes in the config home so the CLI finds its node without
// the config's data: the address, interface and ports.
const MeshFile = "mesh.json"

// ErrNoAddress is a serve or join without the address init or join wrote.
var ErrNoAddress = errors.New("node: no mesh address in the config: run `tailraid init` or `tailraid join <code>`")

// network is what serve and join open.
type network struct {
	addr    netip.Prefix // this device's address with its subnet's bits (the fake: /32)
	key     mesh.PrivateKey
	mesh    *mesh.Mesh      // nil under the fake
	portmap *portmap.Mapper // the gateway mapping of the port (docs/20 §7); nil when off or under the fake
	ifname  string
	port    uint16
	auth    *rpc.TableAuthorizer
	fake    bool
	log     *slog.Logger
	mu      sync.Mutex
	lastEP  []string // the candidates last published
	pubAt   time.Time
}

// pathOf is the prober's view of the mesh's path to a device row (docs/20 §8, I-327):
// the fake has no mesh and every path is a LAN; a row without a key has no path.
func (nw *network) pathOf(node *cluster.Node) func(meta.NodeIdx) meta.PathClass {
	return func(idx meta.NodeIdx) meta.PathClass {
		if nw.mesh == nil {
			return meta.PathLAN
		}
		row, ok := node.FSM.Node(idx)
		if !ok || row.Key == (meta.MeshKey{}) {
			return meta.PathNone
		}
		return nw.mesh.Path(mesh.Key(row.Key))
	}
}

// IP is the device's address.
func (nw *network) IP() netip.Addr { return nw.addr.Addr() }

// Key is the device's public key; zero under the fake.
func (nw *network) Key() mesh.Key {
	if nw.fake {
		return mesh.Key{}
	}
	return nw.key.Public()
}

// Close stops the device and its interface.
func (nw *network) Close() {
	if nw.portmap != nil { // the mapping is given back; a restart maps the same port again
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		nw.portmap.Close(ctx)
		cancel()
	}
	if nw.mesh != nil {
		nw.mesh.Close()
	}
}

// introducer answers introduces once the cluster node exists (mesh.Introducer).
type introducer struct{ node atomic.Pointer[cluster.Node] }

func (i *introducer) Introduce(ctx context.Context, key mesh.Key) (netip.Prefix, error) {
	n := i.node.Load()
	if n == nil {
		return netip.Prefix{}, errors.New("node: not open yet")
	}
	return n.Introduce(ctx, meta.MeshKey(key))
}

// tokenKey is the introduce key of the cluster's current token, nil before the FSM
// exists or while the cluster has no token.
func (i *introducer) tokenKey(clusterID [16]byte) func() []byte {
	return func() []byte {
		n := i.node.Load()
		if n == nil {
			return nil
		}
		tok := n.FSM.Meta().JoinToken
		if !mesh.ValidToken(tok) {
			return nil
		}
		return mesh.IntroduceKey(tok, clusterID)
	}
}

// fakeAddr reads dev.mesh_fake: `{"addr": "127.0.0.1"}`.
func fakeAddr(path string) (netip.Prefix, error) {
	b, err := os.ReadFile(path) //nolint:gosec // the fake's file is named in the config
	if err != nil {
		return netip.Prefix{}, err
	}
	var f struct {
		Addr string `json:"addr"`
	}
	if err := json.Unmarshal(b, &f); err != nil {
		return netip.Prefix{}, fmt.Errorf("node: mesh fake %s: %w", path, err)
	}
	a, err := netip.ParseAddr(f.Addr)
	if err != nil {
		return netip.Prefix{}, fmt.Errorf("node: mesh fake %s: bad addr %q", path, f.Addr)
	}
	return netip.PrefixFrom(a, a.BitLen()), nil
}

// openNetwork brings the device up for serve (docs/20 §1, §9): the key from the
// keychain, the TUN with its address and route, the WireGuard device on the mesh port.
// With no address in the config, or no TUN privilege, it fails with the hint.
func openNetwork(cfg config.Config, clusterID [16]byte, intro *introducer, clk clock.Clock, log *slog.Logger) (*network, error) {
	auth := &rpc.TableAuthorizer{}
	if cfg.Dev.Enabled && cfg.Dev.MeshFake != "" {
		addr, err := fakeAddr(cfg.Dev.MeshFake)
		if err != nil {
			return nil, err
		}
		return &network{addr: addr, auth: auth, fake: true, log: log, port: uint16(cfg.Mesh.Port)}, nil //nolint:gosec // validated
	}
	if cfg.Mesh.Address == "" {
		return nil, ErrNoAddress
	}
	addr, err := netip.ParsePrefix(cfg.Mesh.Address)
	if err != nil {
		return nil, fmt.Errorf("node: mesh.address: %w", err)
	}
	key, _, err := mesh.LoadOrCreateKey(deviceKeychain(cfg.Node.DataDir), clusterID)
	if err != nil {
		return nil, err
	}
	nw := &network{addr: addr, key: key, auth: auth, log: log, port: uint16(cfg.Mesh.Port)} //nolint:gosec // validated
	if err := nw.up(cfg, clusterID, intro, clk); err != nil {
		return nil, err
	}
	return nw, nil
}

// up opens the TUN, configures it and starts the device.
func (nw *network) up(cfg config.Config, clusterID [16]byte, intro *introducer, clk clock.Clock) error {
	tunDev, ifname, err := mesh.CreateTUN(cfg.Mesh.Interface, mesh.MTU, clusterID)
	if err != nil {
		return err
	}
	nw.ifname = ifname
	rate, err := cfg.MeshRelayRateBytes()
	if err != nil {
		_ = tunDev.Close()
		return err
	}
	o := mesh.Options{Port: nw.port, Log: nw.log, Clock: clk, ClusterID: clusterID, RelayRate: rate}
	if intro != nil {
		o.Introducer, o.TokenKey = intro, intro.tokenKey(clusterID)
	}
	m, err := mesh.New(tunDev, nw.key, o)
	if err != nil {
		_ = tunDev.Close()
		return err
	}
	nw.mesh = m
	cmds, err := mesh.Commands(runtime.GOOS, ifname, nw.addr, mesh.MTU)
	if err != nil {
		m.Close()
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := mesh.Apply(ctx, cmds, mesh.ExecRunner); err != nil {
		m.Close()
		return err
	}
	nw.log.Info("mesh up", "addr", nw.addr, "interface", ifname, "port", m.Port())
	if cfg.MeshPortMap() {
		nw.portmap = &portmap.Mapper{Port: m.Port(), Clock: clk, Log: nw.log}
	}
	return nil
}

// tickPortMap drives the gateway mapping (docs/20 §7, I-330).
func (nw *network) tickPortMap(ctx context.Context) {
	if nw.portmap != nil {
		nw.portmap.Tick(ctx)
	}
}

// external is the mapped endpoint, as a candidate, or "".
func (nw *network) external() string {
	if nw.portmap == nil {
		return ""
	}
	if ep, ok := nw.portmap.External(); ok {
		return ep.String()
	}
	return ""
}

// meshPeers turns member rows and pending rows into the device's peers (docs/20 §3,
// I-317): every device row with a key that is not removed, with its candidates, and
// every pending row with none (it reaches us). self is left out.
func meshPeers(rows []meta.Node, pending []meta.Pending, self mesh.Key) []mesh.Peer {
	var out []mesh.Peer
	seen := map[mesh.Key]bool{}
	// a removed row's key is no peer whatever another source (the member seed, which
	// may lag the table) says of it: its next packet dies at the tunnel (I-318)
	for i := range rows {
		if r := &rows[i]; r.Kind == meta.KindDevice && r.State == meta.Removed && !r.Key.IsZero() {
			seen[mesh.Key(r.Key)] = true
		}
	}
	for i := range rows {
		r := &rows[i]
		if r.Kind != meta.KindDevice || r.State == meta.Removed || r.Key.IsZero() || r.Addr == "" {
			continue
		}
		k := mesh.Key(r.Key)
		if k == self || seen[k] {
			continue
		}
		addr, err := netip.ParseAddr(r.Addr)
		if err != nil {
			continue
		}
		seen[k] = true
		out = append(out, mesh.Peer{Idx: uint32(r.Idx), Key: k, Addr: addr, Candidates: parseCandidates(r.Endpoints)})
	}
	for i := range pending {
		p := &pending[i]
		k := mesh.Key(p.Key)
		if k == self || seen[k] {
			continue
		}
		addr, err := netip.ParseAddr(p.Addr)
		if err != nil {
			continue
		}
		seen[k] = true
		out = append(out, mesh.Peer{Idx: pendingIdx(k), Key: k, Addr: addr})
	}
	slices.SortFunc(out, func(a, b mesh.Peer) int { return slices.Compare(a.Key[:], b.Key[:]) })
	return out
}

// pendingIdx names a pending peer's fan-out: above every row index, from its key.
func pendingIdx(k mesh.Key) uint32 {
	return 1<<31 | uint32(k[0])<<16 | uint32(k[1])<<8 | uint32(k[2])
}

// parseCandidates keeps the endpoints that are addresses; a name is resolved once at
// publication by the device that owns it, never here.
func parseCandidates(eps []string) []netip.AddrPort {
	var out []netip.AddrPort
	for _, e := range eps {
		if ap, err := netip.ParseAddrPort(e); err == nil {
			out = append(out, ap)
		}
	}
	return out
}

// refreshPeers sets the device's peers from the table, the seed and the pending rows.
func (nw *network) refreshPeers(node *cluster.Node) {
	if nw.mesh == nil {
		return
	}
	rows := node.FSM.Nodes()
	rows = append(rows, nw.auth.Seed()...)
	if err := nw.mesh.SetPeers(meshPeers(rows, node.FSM.Pending(), nw.Key())); err != nil {
		nw.log.Warn("mesh peers", "err", err)
	}
}

// candidates are this device's endpoint candidates (docs/20 §5, I-322): its interface
// addresses with the mesh port, mesh.endpoint, and the reflexive addresses members see,
// deduplicated and at most meta.MaxEndpoints.
func candidates(port uint16, configured, external string, seenFroms []string) []string {
	var out []string
	seen := map[string]bool{}
	add := func(e string) {
		if e == "" || seen[e] || len(out) >= meta.MaxEndpoints {
			return
		}
		seen[e] = true
		out = append(out, e)
	}
	add(configured)
	add(external) // the gateway's mapping (docs/20 §7)
	local, _ := mesh.LocalEndpoints(port)
	for _, ap := range local {
		add(ap.String())
	}
	for _, e := range seenFroms {
		add(e)
	}
	return out
}

// publishEndpoints proposes update_endpoints when the candidates changed, at most once
// a minute (I-322).
func (nw *network) publishEndpoints(ctx context.Context, cfg config.Config, node *cluster.Node, client *rpc.Client, self meta.NodeIdx, seenFroms []string, now time.Time) {
	if nw.mesh == nil {
		return
	}
	cands := candidates(nw.port, cfg.Mesh.Endpoint, nw.external(), seenFroms)
	nw.mu.Lock()
	unchanged := slices.Equal(cands, nw.lastEP)
	tooSoon := !nw.pubAt.IsZero() && now.Sub(nw.pubAt) < time.Minute
	nw.mu.Unlock()
	if unchanged || tooSoon {
		return
	}
	if row, ok := node.FSM.Node(self); ok && slices.Equal(row.Endpoints, cands) {
		nw.mu.Lock()
		nw.lastEP = cands
		nw.mu.Unlock()
		return
	}
	if err := selfMemberOp(ctx, node, client, rpc.MemberOp{Op: "update_endpoints", NodeIdx: self, Endpoints: cands}); err != nil {
		nw.log.Warn("endpoints not published, will retry", "err", err)
		return
	}
	nw.mu.Lock()
	nw.lastEP, nw.pubAt = cands, now
	nw.mu.Unlock()
	nw.log.Info("endpoints published", "endpoints", cands)
}

// seenFrom is the endpoint the device sees for the member at src, for /health.
func (nw *network) seenFrom(node *cluster.Node) func(netip.Addr) string {
	return func(src netip.Addr) string {
		if nw.mesh == nil {
			return ""
		}
		row, ok := node.FSM.NodeByAddr(src.Unmap().String())
		if !ok || row.Key.IsZero() {
			return ""
		}
		peers, err := nw.mesh.Peers()
		if err != nil {
			return ""
		}
		return directEndpoint(peers, mesh.Key(row.Key))
	}
}

// directEndpoint is the UDP source this device sees a peer's packets from, when the path
// to it is direct: a relayed peer's endpoint is the relay's, and a fanning-out one has
// none, so a caller on either learns no candidate from this member (I-322).
func directEndpoint(peers []mesh.PeerStatus, key mesh.Key) string {
	for i := range peers {
		p := &peers[i]
		if p.Key == key && p.Endpoint.IsValid() && !p.Relay && !p.Fanout {
			return p.Endpoint.String()
		}
	}
	return ""
}

// writeMeshFile records where the CLI finds this node.
func writeMeshFile(home string, addr netip.Addr, ifname string, port, rpcPort uint16) error {
	b, err := json.Marshal(struct {
		Addr      string `json:"addr"`
		Interface string `json:"interface,omitempty"`
		Port      uint16 `json:"port"`
		RPCPort   uint16 `json:"rpc_port"`
	}{addr.String(), ifname, port, rpcPort})
	if err != nil {
		return err
	}
	if err := os.MkdirAll(home, 0o755); err != nil { //nolint:gosec // the config home is readable by the user's CLI
		return err
	}
	return os.WriteFile(filepath.Join(home, MeshFile), b, 0o644) //nolint:gosec // world-readable by design: the CLI reads it (docs/20 §3)
}

// ReadMeshFile is the CLI's side: the node's RPC address from mesh.json, or "" when
// serve has not written one.
func ReadMeshFile(home string) (rpcAddr string, meshAddr netip.Addr, ok bool) {
	b, err := os.ReadFile(filepath.Join(home, MeshFile)) //nolint:gosec // the mesh file in the config home
	if err != nil {
		return "", netip.Addr{}, false
	}
	var f struct {
		Addr    string `json:"addr"`
		RPCPort uint16 `json:"rpc_port"`
	}
	if err := json.Unmarshal(b, &f); err != nil || f.RPCPort == 0 {
		return "", netip.Addr{}, false
	}
	a, err := netip.ParseAddr(f.Addr)
	if err != nil {
		return "", netip.Addr{}, false
	}
	return net.JoinHostPort(a.String(), strconv.Itoa(int(f.RPCPort))), a, true
}

// keylessCheck refuses a state from before the mesh: a device row without a key cannot
// be a peer, and the cluster is re-created (docs/20 §10, I-326).
func keylessCheck(st *meta.FSM) error {
	rows := st.Nodes()
	for i := range rows {
		if n := &rows[i]; n.Kind == meta.KindDevice && n.State != meta.Removed && n.Key.IsZero() {
			return fmt.Errorf("node %s (%d) has no mesh key: this cluster predates the mesh (docs/20); re-create it with `tailraid init` and `tailraid join <code>`", n.Name, n.Idx)
		}
	}
	return nil
}

// report is the status report's mesh block (docs/20 §10, I-325).
func (nw *network) report(node *cluster.Node, cfg config.Config, now time.Time) *admin.MeshReport {
	rep := &admin.MeshReport{Addr: nw.IP().String(), Interface: nw.ifname, Port: nw.port, RPCPort: uint16(cfg.Listen.RPC), Fake: nw.fake} //nolint:gosec // validated
	if !nw.fake {
		rep.Key = nw.Key().String()
	}
	if self, ok := node.SelfIdx(); ok {
		if row, ok := node.FSM.Node(self); ok {
			rep.Endpoints = row.Endpoints
		}
	}
	if nw.mesh == nil {
		return rep
	}
	rep.RxBytes, rep.TxBytes = nw.mesh.Counters()
	if nw.portmap != nil {
		st := nw.portmap.Status()
		rep.PortMap = &st
	}
	peers, err := nw.mesh.Peers()
	if err != nil {
		return rep
	}
	byKey := map[mesh.Key]*meta.Node{}
	rows := node.FSM.Nodes()
	for i := range rows {
		if row := &rows[i]; row.Kind == meta.KindDevice && !row.Key.IsZero() {
			byKey[mesh.Key(row.Key)] = row
		}
	}
	// a relayed path names the member it goes through: the peer whose pinned endpoint is
	// the relay's address
	viaName := map[netip.AddrPort]string{}
	for i := range peers {
		if p := &peers[i]; !p.Relay && p.Endpoint.IsValid() {
			if row, ok := byKey[p.Key]; ok {
				viaName[p.Endpoint] = row.Name
			}
		}
	}
	nameOf := func(k mesh.Key) string {
		if row, ok := byKey[k]; ok {
			return row.Name
		}
		return k.String()
	}
	for from, n := range nw.mesh.Relayed() {
		if rep.Relayed == nil {
			rep.Relayed = map[string]uint64{}
		}
		rep.Relayed[nameOf(from[0])+">"+nameOf(from[1])] = n
	}
	for i := range peers {
		p := &peers[i]
		mp := admin.MeshPeer{Key: p.Key.String(), Class: p.Class.String(), Rx: p.Rx, Tx: p.Tx}
		if row, ok := byKey[p.Key]; ok {
			mp.Idx, mp.Name = row.Idx, row.Name
		}
		if p.Endpoint.IsValid() {
			mp.Endpoint = p.Endpoint.String()
		}
		if p.Relay {
			mp.Via = viaName[p.Endpoint]
			if mp.Via == "" {
				mp.Via = p.Endpoint.String()
			}
		}
		if !p.LastHandshake.IsZero() {
			mp.HandshakeAge = now.Sub(p.LastHandshake).Round(time.Second).String()
		}
		if seen := p.LastSeen(); !seen.IsZero() {
			mp.SeenAge = now.Sub(seen).Round(time.Second).String()
		}
		rep.Peers = append(rep.Peers, mp)
	}
	return rep
}

// InitIdentity is init's side (docs/20 §3): a fresh cluster ID, the subnet derived from
// it unless given, this device at the subnet's first host, its key from the keychain,
// and the options Bootstrap needs. It records the address in the config file at path.
// Under dev.mesh_fake the identity is the fake address with no key and no subnet.
type InitIdentity struct {
	Addr netip.Addr
	Key  mesh.Key
	Host uint8
	Port uint16
}

// PrepareInit is init's side: see InitIdentity.
func PrepareInit(cfg config.Config, path, subnetFlag string, port int) (InitIdentity, cluster.Options, meta.Node, error) {
	var cid [16]byte
	if _, err := rand.Read(cid[:]); err != nil {
		return InitIdentity{}, cluster.Options{}, meta.Node{}, err
	}
	ident := InitIdentity{Port: uint16(port)} //nolint:gosec // validated
	self := meta.Node{Kind: meta.KindDevice}
	opts := cluster.Options{ClusterID: cid}
	if cfg.Dev.Enabled && cfg.Dev.MeshFake != "" {
		addr, err := fakeAddr(cfg.Dev.MeshFake)
		if err != nil {
			return InitIdentity{}, cluster.Options{}, meta.Node{}, err
		}
		ident.Addr, ident.Host = addr.Addr(), 1
		self.Addr = addr.Addr().String()
		self.Endpoints = []string{net.JoinHostPort(self.Addr, strconv.Itoa(port))} // the code's endpoint: where join finds the RPC
		opts.RaftAddr = net.JoinHostPort(self.Addr, strconv.Itoa(cfg.Listen.Raft))
		return ident, opts, self, nil
	}
	subnet := cmp.Or(subnetFlag, cfg.Mesh.Subnet)
	var prefix netip.Prefix
	if subnet == "" {
		prefix = mesh.Subnet(cid)
	} else {
		p, err := netip.ParsePrefix(subnet)
		if err != nil || !p.Addr().Is4() || p.Bits() != mesh.SubnetBits {
			return InitIdentity{}, cluster.Options{}, meta.Node{}, fmt.Errorf("mesh subnet %q: want an IPv4 /%d", subnet, mesh.SubnetBits)
		}
		prefix = p.Masked()
	}
	addr, err := mesh.HostAddr(prefix, mesh.FirstHost)
	if err != nil {
		return InitIdentity{}, cluster.Options{}, meta.Node{}, err
	}
	key, _, err := mesh.LoadOrCreateKey(deviceKeychain(cfg.Node.DataDir), cid)
	if err != nil {
		return InitIdentity{}, cluster.Options{}, meta.Node{}, err
	}
	ident.Addr, ident.Key, ident.Host = addr, key.Public(), mesh.FirstHost
	cfg.Mesh.Address = netip.PrefixFrom(addr, mesh.SubnetBits).String()
	cfg.Mesh.Subnet = prefix.String()
	if err := config.Save(path, cfg); err != nil {
		return InitIdentity{}, cluster.Options{}, meta.Node{}, err
	}
	self.Addr, self.Key, self.MeshPort = addr.String(), meta.MeshKey(key.Public()), ident.Port
	self.Endpoints = candidates(ident.Port, cfg.Mesh.Endpoint, "", nil)
	// init creates no interface (I-324): Raft listens on loopback for the bootstrap and
	// the configuration records the mesh address serve will bind
	opts.RaftAddr = "127.0.0.1:0"
	opts.RaftAdvertise = net.JoinHostPort(addr.String(), strconv.Itoa(cfg.Listen.Raft))
	opts.MeshSubnet = prefix.String()
	return ident, opts, self, nil
}

// LocalAddr is where the CLI reaches this device's node: the mesh file serve wrote
// beside the config, else the address the config holds, with the RPC port.
func LocalAddr(cfg config.Config) (string, error) {
	home := config.Home()
	if cfg.Path != "" {
		home = filepath.Dir(cfg.Path)
	}
	if addr, _, ok := ReadMeshFile(home); ok {
		return addr, nil
	}
	if cfg.Dev.Enabled && cfg.Dev.MeshFake != "" {
		a, err := fakeAddr(cfg.Dev.MeshFake)
		if err != nil {
			return "", err
		}
		return net.JoinHostPort(a.Addr().String(), strconv.Itoa(cfg.Listen.RPC)), nil
	}
	if cfg.Mesh.Address == "" {
		return "", ErrNoAddress
	}
	p, err := netip.ParsePrefix(cfg.Mesh.Address)
	if err != nil {
		return "", fmt.Errorf("node: mesh.address: %w", err)
	}
	return net.JoinHostPort(p.Addr().String(), strconv.Itoa(cfg.Listen.RPC)), nil
}
