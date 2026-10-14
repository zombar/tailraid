package main

import (
	"bytes"
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/zombar/tailraid/internal/admin"
	"github.com/zombar/tailraid/internal/cluster"
	"github.com/zombar/tailraid/internal/meta"
	"github.com/zombar/tailraid/internal/s3"
	"github.com/zombar/tailraid/internal/soak"
)

// composeNode is one node of test/soak/docker-compose.yml with its worker sidecar. ip is
// the container's address on the compose network, where iptables cuts it; the node's RPC
// and browser UI listen on its mesh address (docs/20), learned from the cluster's status
// into ref.RPC and addr.
type composeNode struct {
	ref    soak.NodeRef
	ip     string
	addr   string // the mesh address, "" until Nodes resolved it
	worker string // http://127.0.0.1:2050X (published port of the sidecar)
}

// composeEnv drives the compose cluster from the host through the worker sidecars and the
// docker compose CLI (docs/09 §6).
type composeEnv struct {
	soak.Admin
	dir   string // compose project directory
	file  string // compose file in dir: docker-compose.yml (4 hosts with disks + a diskless one) or docker-compose.single.yml
	nodes []composeNode
	http  *http.Client
}

func newComposeEnv(dir, file string, n int) *composeEnv {
	e := &composeEnv{dir: dir, file: file, http: &http.Client{Timeout: 90 * time.Second}}
	if file == natFile {
		e.nodes = natNodes()
	}
	for i := 1; i <= n; i++ {
		e.nodes = append(e.nodes, containerNode(i))
	}
	for _, nd := range e.nodes {
		e.Seeds = append(e.Seeds, nd.ref.RPC)
	}
	e.Caller = e
	return e
}

// The NAT rig (docker-compose.nat.yml, docs/20 §10 phase 27): n1 and n5 public, n2
// behind router A (cone) at 172.30.0.21, n3 behind router B (symmetric) at 172.30.0.22.
const (
	natFile         = "docker-compose.nat.yml"
	natConeRouter   = "172.30.0.21"
	natSymmetricRtr = "172.30.0.22"
)

func natNodes() []composeNode {
	n2, n3 := containerNode(2), containerNode(3)
	n2.ip, n3.ip = "172.31.1.12", "172.31.2.13"
	n2.ref.RPC, n3.ref.RPC = n2.ip+":20481", n3.ip+":20481"
	return []composeNode{containerNode(1), n2, n3, containerNode(5)}
}

// NATRoles names the rig's parts for phase 27; not a NAT rig otherwise.
func (e *composeEnv) NATRoles() (soak.NATRoles, bool) {
	if e.file != natFile {
		return soak.NATRoles{}, false
	}
	roles := soak.NATRoles{ConeRouter: natConeRouter, SymmetricRouter: natSymmetricRtr}
	for _, n := range e.nodes {
		switch n.ref.Name {
		case "n2":
			roles.Cone = n.ref
		case "n3":
			roles.Symmetric = n.ref
		default:
			roles.Public = append(roles.Public, n.ref)
		}
	}
	return roles, true
}

// SetPortMap creates or removes the file the cone router's responder answers behind
// (test/soak/router.sh); ResetPortMap drops the reset file it empties itself on.
func (e *composeEnv) SetPortMap(ctx context.Context, on bool) error {
	if on {
		return e.compose(ctx, "exec", "-T", "router-a", "touch", "/run/portmap.on")
	}
	return e.compose(ctx, "exec", "-T", "router-a", "rm", "-f", "/run/portmap.on")
}

func (e *composeEnv) ResetPortMap(ctx context.Context) error {
	return e.compose(ctx, "exec", "-T", "router-a", "touch", "/run/portmap.reset")
}

var _ soak.NATEnv = (*composeEnv)(nil)

// containerNode is container n<i> as the compose files lay it out: 172.30.0.1<i> on the
// network, its worker published on 2050<i>. The RPC label is the container's address
// until the mesh address is known; the worker routes either.
func containerNode(i int) composeNode {
	return composeNode{
		ref:    soak.NodeRef{Name: fmt.Sprintf("n%d", i), Idx: meta.NodeIdx(i), RPC: fmt.Sprintf("172.30.0.%d:20481", 10+i)},
		ip:     fmt.Sprintf("172.30.0.%d", 10+i),
		worker: fmt.Sprintf("http://127.0.0.1:%d", 20500+i),
	}
}

// lateNode is n6, the device of phase 25 (the compose file's `late` profile).
const lateNode = 6

func (e *composeEnv) Name() string {
	return fmt.Sprintf("docker compose (%s), %d nodes", strings.TrimSuffix(e.file, ".yml"), len(e.nodes))
}

// Nodes returns the compose nodes with the indices the cluster gave them. The containers
// join in whatever order they come up, so n2 may well be node 4: the index is read from
// the cluster's status by name, never assumed from the container number.
func (e *composeEnv) Nodes(ctx context.Context) ([]soak.NodeRef, error) {
	rep, err := e.SeedStatus(ctx)
	if err != nil {
		return nil, err
	}
	if err := resolveIndices(e.nodes, rep.Nodes); err != nil {
		return nil, err
	}
	out := make([]soak.NodeRef, 0, len(e.nodes))
	for _, n := range e.nodes {
		out = append(out, n.ref)
	}
	return out, nil
}

// resolveIndices sets each compose node's index and mesh address from the status report,
// matched by name; the RPC label becomes the mesh address, which the leader's redirects
// name (docs/20 §1).
func resolveIndices(nodes []composeNode, members []admin.NodeReport) error {
	for i := range nodes {
		found := false
		for j := range members {
			if m := &members[j]; m.Name == nodes[i].ref.Name && m.Kind == "device" && m.State != "removed" {
				nodes[i].ref.Idx, found = m.Idx, true
				if m.Addr != "" {
					nodes[i].addr = m.Addr
					nodes[i].ref.RPC = m.RPCAddr()
				}
			}
		}
		if !found {
			return fmt.Errorf("compose node %s is not in the cluster's node table", nodes[i].ref.Name)
		}
	}
	return nil
}

// node finds the compose node behind a ref by name (indices are assigned by the cluster).
func (e *composeEnv) node(ref soak.NodeRef) composeNode {
	for _, n := range e.nodes {
		if n.ref.Name == ref.Name {
			return n
		}
	}
	panic("compose: unknown node " + ref.Name)
}

// webPort is the browser UI's port on the node's mesh address; the compose files publish
// node i's worker proxy of it on 127.0.0.1:2052i, so the driver dials the published port
// while the request names the node's own address, which its Host check expects (docs/13
// §2, I-127).
const webPort = "20482"

func (e *composeEnv) WebClient() *http.Client {
	dialer := &net.Dialer{Timeout: 10 * time.Second}
	return &http.Client{Timeout: 90 * time.Second, Transport: &http.Transport{
		DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
			if host, port, err := net.SplitHostPort(addr); err == nil && port == webPort {
				for _, n := range e.nodes {
					if n.ip == host || (n.addr != "" && n.addr == host) {
						addr = fmt.Sprintf("127.0.0.1:%d", 20520+nodeNumber(n.ref))
					}
				}
			}
			return dialer.DialContext(ctx, network, addr)
		},
	}}
}

// nodeNumber is the i of container n<i>.
func nodeNumber(ref soak.NodeRef) int {
	n, _ := strconv.Atoi(strings.TrimPrefix(ref.Name, "n"))
	return n
}

func (e *composeEnv) Caps() soak.Caps {
	return soak.Caps{Kill: true, Partition: true, Corrupt: true, MultiGateway: true, Disks: true, Mutate: true, S3: e.file == multiFile, AddS3: e.file == multiFile, Stats: true, Mirror: e.file == multiFile, Mesh: e.file != "docker-compose.single.yml"}
}

// multiFile is the compose file with phase 15's bucket, the s3 service (docs/10 §4).
const multiFile = "docker-compose.yml"

// The s3 service of docker-compose.yml: the nodes reach it on the compose network, the
// driver on its published port; both see the one bucket.
const (
	s3NodeEndpoint   = "http://172.30.0.20:8080"
	s3DriverEndpoint = "http://127.0.0.1:20510"
	s3BucketName     = "tailraid"
	s3Prefix         = "soak"
	s3AccessKey      = "soakaccess"
	s3SecretKey      = "soak/secret+key" //nolint:gosec // the throwaway key of the compose rig's bucket
)

// AddS3 sets the bucket's credentials in the secrets store and runs `node add-s3` on the
// first node, so every gateway can write the bucket (docs/10 §4.1, docs/19 §6).
func (e *composeEnv) AddS3(ctx context.Context, name string) error {
	first := e.nodes[0].ref.Name
	access, secret, _ := cluster.S3SecretNames(name)
	for _, sec := range []struct{ name, value string }{{access, s3AccessKey}, {secret, s3SecretKey}} {
		if err := e.composeIn(ctx, sec.value+"\n", "exec", "-T", first, "tailraid", "secret", "set", sec.name); err != nil {
			return err
		}
	}
	return e.compose(ctx, "exec", "-T", first, "tailraid", "node", "add-s3", name, "--endpoint", s3NodeEndpoint,
		"--bucket", s3BucketName, "--prefix", s3Prefix, "--path-style", "--capacity", "1T")
}

// Bucket is the s3 service's bucket as the driver reaches it.
func (e *composeEnv) Bucket(context.Context) (*s3.Bucket, error) {
	return s3.New(s3.Target{Endpoint: s3DriverEndpoint, Bucket: s3BucketName, Prefix: s3Prefix, PathStyle: true},
		s3.Credentials{AccessKey: s3AccessKey, SecretKey: s3SecretKey}, nil)
}

// diskDir is where an added disk named "<host>/<n>" is mounted in its container: the
// compose files mount the n-th entry of $DISKS at /disks/d<n> (test/soak/entry.sh).
func diskDir(disk soak.NodeRef) (string, error) {
	_, num, ok := strings.Cut(disk.Name, "/")
	if !ok || num == "" {
		return "", fmt.Errorf("compose: %q is not a disk name", disk.Name)
	}
	return "/disks/d" + num, nil
}

// FailDisk hides the disk's marker and restarts its host, as an unmounted disk looks
// after a reboot: the host serves on and reports the disk not OK (docs/14 §3).
func (e *composeEnv) FailDisk(ctx context.Context, disk soak.NodeRef) error {
	return e.moveMarker(ctx, disk, "disk_id", "disk_id.away")
}

// RestoreDisk puts the marker back and restarts the host.
func (e *composeEnv) RestoreDisk(ctx context.Context, disk soak.NodeRef) error {
	return e.moveMarker(ctx, disk, "disk_id.away", "disk_id")
}

// AddDisk runs `node add-disk` on the host's spare mount /disks/spare, which the compose
// files mount on every node but leave out of $DISKS. The phase drains and removes it
// again and never closes it, so it needs no diskDir mapping.
func (e *composeEnv) AddDisk(ctx context.Context, host soak.NodeRef) error {
	return e.compose(ctx, "exec", "-T", host.Name, "tailraid", "node", "add-disk", "/disks/spare", "--capacity", "8G")
}

// The compose file's mirror (docs/16): n2 lists /mirrors/m1 under node.mirrors (MIRRORS
// in test/soak/entry.sh) on a volume of its own; nothing else mounts it.
const (
	mirrorHost = "n2"
	mirrorPath = "/mirrors/m1"
)

// AddMirror runs `tailraid mirror add` on n2 (the config already lists the path, so the
// command only marks it and registers the row) and returns the row name.
func (e *composeEnv) AddMirror(ctx context.Context) (string, soak.NodeRef, error) {
	if e.file != multiFile {
		return "", soak.NodeRef{}, nil
	}
	host := e.node(soak.NodeRef{Name: mirrorHost}).ref
	if err := e.compose(ctx, "exec", "-T", mirrorHost, "tailraid", "mirror", "add", mirrorPath); err != nil {
		return "", soak.NodeRef{}, err
	}
	return mirrorHost + "/m1", host, nil
}

// SetMirrorOnline moves the mirror's directory away and back, as an unplugged disk
// looks to its host's next tick (docs/16 §1); no restart.
func (e *composeEnv) SetMirrorOnline(ctx context.Context, _ string, online bool) error {
	from, to := mirrorPath, mirrorPath+".away"
	if online {
		from, to = to, from
	}
	return e.compose(ctx, "exec", "-T", mirrorHost, "mv", from, to)
}

func (e *composeEnv) moveMarker(ctx context.Context, disk soak.NodeRef, from, to string) error {
	dir, err := diskDir(disk)
	if err != nil {
		return err
	}
	host := soak.NodeRef{Name: disk.Host}
	if err := e.compose(ctx, "exec", "-T", disk.Host, "mv", dir+"/"+from, dir+"/"+to); err != nil {
		return err
	}
	return e.Restart(ctx, []soak.NodeRef{e.node(host).ref})
}

// --- worker calls --------------------------------------------------------------------------

func (e *composeEnv) post(ctx context.Context, n composeNode, path string, req, resp any) error {
	body, _ := json.Marshal(req)
	hreq, err := http.NewRequestWithContext(ctx, http.MethodPost, n.worker+path, bytes.NewReader(body))
	if err != nil {
		return err
	}
	hreq.Header.Set("Content-Type", "application/json")
	hresp, err := e.http.Do(hreq)
	if err != nil {
		return fmt.Errorf("%s worker: %w", n.ref.Name, err)
	}
	defer hresp.Body.Close()
	if hresp.StatusCode/100 != 2 {
		var msg bytes.Buffer
		_, _ = msg.ReadFrom(hresp.Body)
		return fmt.Errorf("%s worker %s: %s: %s", n.ref.Name, path, hresp.Status, strings.TrimSpace(msg.String()))
	}
	if resp != nil {
		return json.NewDecoder(hresp.Body).Decode(resp)
	}
	return nil
}

type rpcReq struct {
	Method string `json:"method"`
	Path   string `json:"path"`
	Body   []byte `json:"body"`
}

type rpcResp struct {
	Body  []byte `json:"body,omitempty"`
	Error string `json:"error,omitempty"`
}

func (e *composeEnv) call(ctx context.Context, ref soak.NodeRef, method, path string, body []byte) ([]byte, error) {
	var resp rpcResp
	if err := e.post(ctx, e.node(ref), "/rpc", rpcReq{method, path, body}, &resp); err != nil {
		return nil, err
	}
	if resp.Error != "" {
		return nil, errors.New(resp.Error)
	}
	return resp.Body, nil
}

func (e *composeEnv) Volume(ref soak.NodeRef) (soak.Volume, bool) {
	return &httpVolume{env: e, node: e.node(ref)}, true
}

type fsReq struct {
	Op   string `json:"op"`
	Path string `json:"path"`
	Off  int64  `json:"off"`
	Data []byte `json:"data"`
	Size int64  `json:"size"`
	To   string `json:"to"`
}

type fsResp struct {
	Data  []byte   `json:"data,omitempty"`
	Size  int64    `json:"size"`
	Names []string `json:"names,omitempty"`
	Hash  string   `json:"hash,omitempty"` // hex sha256 (op "hash")
	Error string   `json:"error,omitempty"`
}

// httpVolume is a Volume served by a worker sidecar.
type httpVolume struct {
	env  *composeEnv
	node composeNode
}

func (v *httpVolume) do(req fsReq) (fsResp, error) {
	var resp fsResp
	if err := v.env.post(context.Background(), v.node, "/fs", req, &resp); err != nil {
		return resp, err
	}
	if resp.Error != "" {
		if strings.Contains(resp.Error, "does not exist") || strings.Contains(resp.Error, "NOENT") || strings.Contains(resp.Error, "no such") {
			return resp, fmt.Errorf("%w: %s", errNotExist, resp.Error)
		}
		// the pure-Go NFS client reports the wire status by name; a kernel mount would
		// give EACCES, which satisfies os.ErrPermission (docs/07, a protected folder)
		if strings.Contains(resp.Error, "ACCES") || strings.Contains(resp.Error, "permission denied") {
			return resp, fmt.Errorf("%w: %s", os.ErrPermission, resp.Error)
		}
		return resp, errors.New(resp.Error)
	}
	return resp, nil
}

var errNotExist = errors.New("not found")

func (v *httpVolume) Mkdir(p string) error { _, err := v.do(fsReq{Op: "mkdir", Path: p}); return err }

func (v *httpVolume) WriteAt(p string, off int64, data []byte) error {
	_, err := v.do(fsReq{Op: "write", Path: p, Off: off, Data: data})
	return err
}

func (v *httpVolume) ReadAll(p string) ([]byte, error) {
	r, err := v.do(fsReq{Op: "read", Path: p})
	return r.Data, err
}

func (v *httpVolume) Truncate(p string, size int64) error {
	_, err := v.do(fsReq{Op: "truncate", Path: p, Size: size})
	return err
}

func (v *httpVolume) Rename(from, to string) error {
	_, err := v.do(fsReq{Op: "rename", Path: from, To: to})
	return err
}

func (v *httpVolume) Remove(p string) error { _, err := v.do(fsReq{Op: "remove", Path: p}); return err }

func (v *httpVolume) Size(p string) (int64, error) {
	r, err := v.do(fsReq{Op: "size", Path: p})
	return r.Size, err
}

func (v *httpVolume) Hash(p string) ([32]byte, int64, error) {
	r, err := v.do(fsReq{Op: "hash", Path: p})
	var sum [32]byte
	if err != nil {
		return sum, 0, err
	}
	b, err := hex.DecodeString(r.Hash)
	if err != nil {
		return sum, 0, fmt.Errorf("hash reply %q: %w", r.Hash, err)
	}
	if len(b) != 32 {
		return sum, 0, fmt.Errorf("hash reply %q: not a sha256", r.Hash)
	}
	copy(sum[:], b)
	return sum, r.Size, nil
}

func (v *httpVolume) List(p string) ([]string, error) {
	r, err := v.do(fsReq{Op: "list", Path: p})
	return r.Names, err
}

// --- admin surface: soak.Admin through the workers -----------------------------------------

// Call implements admin.Caller: the RPC is proxied by the worker of the node that owns addr.
func (e *composeEnv) Call(ctx context.Context, addr, method, path string, body []byte) ([]byte, error) {
	ref, ok := e.byRPC(addr)
	if !ok {
		return nil, fmt.Errorf("no compose node at %s", addr)
	}
	return e.call(ctx, ref, method, path, body)
}

// byRPC maps a node RPC address (host:port) to its NodeRef: the mesh address once known,
// or the container's, which the seeds name before the first status.
func (e *composeEnv) byRPC(addr string) (soak.NodeRef, bool) {
	for _, n := range e.nodes {
		if n.ref.RPC == addr || n.ip+":20481" == addr {
			return n.ref, true
		}
	}
	return soak.NodeRef{}, false
}

// Goroutines returns the node's dump followed by its worker's own (the client side of a
// hang is as telling as the gateway's).
func (e *composeEnv) Goroutines(ctx context.Context, ref soak.NodeRef) (string, error) {
	node, err := e.Admin.Goroutines(ctx, ref)
	if err != nil {
		return "", err
	}
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, e.node(ref).worker+"/goroutines", http.NoBody)
	resp, werr := e.http.Do(req)
	if werr != nil {
		return node, nil
	}
	defer resp.Body.Close()
	var wb bytes.Buffer
	_, _ = wb.ReadFrom(resp.Body)
	return node + "\n\n==== worker " + workerName(ref) + " ====\n\n" + wb.String(), nil
}

// --- faults through docker compose and the workers ----------------------------------------

func (e *composeEnv) compose(ctx context.Context, args ...string) error {
	cmd := exec.CommandContext(ctx, "docker", append([]string{"compose", "-f", e.file, "--profile", "late"}, args...)...) //nolint:gosec // the driver runs docker compose on its own rig
	cmd.Dir = e.dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("docker compose %s: %w: %s", strings.Join(args, " "), err, bytes.TrimSpace(out))
	}
	return nil
}

// composeIn runs docker compose with stdin (a secret: never in the arguments or an error).
func (e *composeEnv) composeIn(ctx context.Context, stdin string, args ...string) error {
	cmd := exec.CommandContext(ctx, "docker", append([]string{"compose", "-f", e.file, "--profile", "late"}, args...)...) //nolint:gosec // the driver runs docker compose on its own rig
	cmd.Dir = e.dir
	cmd.Stdin = strings.NewReader(stdin)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("docker compose %s: %w: %s", strings.Join(args, " "), err, bytes.TrimSpace(out))
	}
	return nil
}

// composeOut runs docker compose and returns its output.
func (e *composeEnv) composeOut(ctx context.Context, args ...string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, "docker", append([]string{"compose", "-f", e.file, "--profile", "late"}, args...)...) //nolint:gosec // the driver runs docker compose on its own rig
	cmd.Dir = e.dir
	return cmd.CombinedOutput()
}

// TakeEvidence saves what a failed check needs to be explained after the rig is gone
// (I-270): the containers' state, and for every node its last 2,000 log lines with its
// worker's and its `tailraid status --json`, each in a file of its own under dir. Every
// file is attempted; the first error is returned.
func (e *composeEnv) TakeEvidence(ctx context.Context, dir string) error {
	if err := os.MkdirAll(dir, 0o750); err != nil {
		return err
	}
	var first error
	save := func(name string, args ...string) {
		out, err := e.composeOut(ctx, args...)
		if werr := os.WriteFile(filepath.Join(dir, name), out, 0o644); werr != nil && first == nil { //nolint:gosec // evidence beside the report
			first = werr
		}
		if err != nil && first == nil {
			first = fmt.Errorf("%s: %w", name, err)
		}
	}
	save("ps.txt", "ps", "--all")
	for _, n := range e.nodes {
		ref := n.ref
		save(ref.Name+".log", "logs", "--no-color", "--timestamps", "--tail", "2000", ref.Name, workerName(ref))
		save(ref.Name+"-status.json", "exec", "-T", ref.Name, "tailraid", "status", "--json")
	}
	return first
}

func workerName(ref soak.NodeRef) string { return "w" + strings.TrimPrefix(ref.Name, "n") }

func (e *composeEnv) Kill(ctx context.Context, ref soak.NodeRef) error {
	return e.compose(ctx, "kill", ref.Name, workerName(ref))
}

func (e *composeEnv) Start(ctx context.Context, ref soak.NodeRef) error {
	if err := e.compose(ctx, "start", ref.Name, workerName(ref)); err != nil {
		return err
	}
	return e.waitReady(ctx, ref, 90*time.Second)
}

func (e *composeEnv) Restart(ctx context.Context, nodes []soak.NodeRef) error {
	args := make([]string, 0, 3+len(nodes))
	args = append(args, "restart", "-t", "2")
	for _, n := range nodes {
		args = append(args, n.Name)
	}
	if err := e.compose(ctx, args...); err != nil {
		return err
	}
	args = []string{"restart", "-t", "1"}
	for _, n := range nodes {
		args = append(args, workerName(n))
	}
	if err := e.compose(ctx, args...); err != nil {
		return err
	}
	for _, n := range nodes {
		if err := e.waitReady(ctx, n, 120*time.Second); err != nil {
			return err
		}
	}
	return nil
}

func (e *composeEnv) waitReady(ctx context.Context, ref soak.NodeRef, timeout time.Duration) error {
	n := e.node(ref)
	deadline := time.Now().Add(timeout)
	for {
		req, _ := http.NewRequestWithContext(ctx, http.MethodGet, n.worker+"/ready", http.NoBody)
		resp, err := e.http.Do(req)
		if err == nil {
			_ = resp.Body.Close()
			if resp.StatusCode/100 == 2 {
				return nil
			}
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("%s not ready after %s", ref.Name, timeout)
		}
		if err := e.Wait(ctx, time.Second); err != nil {
			return err
		}
	}
}

// CutUDP drops the UDP between a and b both ways and nothing else (phase 26): the mesh
// goes dark between them while the TCP rules stay.
func (e *composeEnv) CutUDP(ctx context.Context, a, b soak.NodeRef) error {
	if err := e.post(ctx, e.node(a), "/partition", map[string]any{"ips": []string{e.node(b).ip}, "udp": true}, nil); err != nil {
		return err
	}
	return e.post(ctx, e.node(b), "/partition", map[string]any{"ips": []string{e.node(a).ip}, "udp": true}, nil)
}

func (e *composeEnv) Partition(ctx context.Context, a, b []soak.NodeRef) error {
	ips := func(side []soak.NodeRef) []string {
		out := make([]string, 0, len(side))
		for _, n := range side {
			out = append(out, e.node(n).ip)
		}
		return out
	}
	for _, n := range a {
		if err := e.post(ctx, e.node(n), "/partition", map[string]any{"ips": ips(b)}, nil); err != nil {
			return err
		}
	}
	for _, n := range b {
		if err := e.post(ctx, e.node(n), "/partition", map[string]any{"ips": ips(a)}, nil); err != nil {
			return err
		}
	}
	return nil
}

func (e *composeEnv) Heal(ctx context.Context) error {
	var first error
	for _, n := range e.nodes {
		if err := e.post(ctx, n, "/heal", map[string]any{}, nil); err != nil && first == nil {
			first = err
		}
	}
	return first
}

// ForgetSecretKey wipes a node's own copy of the cluster key and restarts it (phase 24):
// a container has no Secret Service, so the key is the keychain's fallback file under
// the data dir (docs/19 §2); the restart drops the copy the node holds in memory.
func (e *composeEnv) ForgetSecretKey(ctx context.Context, ref soak.NodeRef) error {
	if err := e.compose(ctx, "exec", "-T", ref.Name, "sh", "-c", `rm -f "$TAILRAID_HOME"/data/keys/cluster-*.key`); err != nil {
		return err
	}
	return e.Restart(ctx, []soak.NodeRef{ref})
}

var _ soak.SecretEnv = (*composeEnv)(nil)

// --- the mesh (phase 25): codes, the late device, a throwaway join, a re-join ------------

// composeLate runs docker compose and returns its output; every call names the `late`
// profile (n6 and ntry), since compose refuses to restart, kill or remove a profile's
// container without it, and the profile changes nothing for the other services.
func (e *composeEnv) composeLate(ctx context.Context, args ...string) ([]byte, error) {
	out, err := e.composeOut(ctx, args...)
	if err != nil {
		return out, fmt.Errorf("docker compose %s: %w: %s", strings.Join(args, " "), err, bytes.TrimSpace(out))
	}
	return out, nil
}

// JoinCode is `tailraid token` on the node: the current token with its key and endpoints.
func (e *composeEnv) JoinCode(ctx context.Context, n soak.NodeRef) (string, error) {
	out, err := e.composeOut(ctx, "exec", "-T", n.Name, "tailraid", "token")
	if err != nil {
		return "", fmt.Errorf("token on %s: %w: %s", n.Name, err, bytes.TrimSpace(out))
	}
	return lastField(out), nil
}

// RotateToken is `tailraid token rotate` through the node; the new code is printed.
func (e *composeEnv) RotateToken(ctx context.Context, n soak.NodeRef) (string, error) {
	out, err := e.composeOut(ctx, "exec", "-T", n.Name, "tailraid", "token", "rotate")
	if err != nil {
		return "", fmt.Errorf("token rotate on %s: %w: %s", n.Name, err, bytes.TrimSpace(out))
	}
	return lastField(out), nil
}

// lastField is the last whitespace-separated word of a command's output: the code.
func lastField(out []byte) string {
	f := strings.Fields(string(out))
	if len(f) == 0 {
		return ""
	}
	return f[len(f)-1]
}

// writeCode puts a join code into the shared /codes volume through the first running
// node (the bootstrap may be down when phase 25 asks).
func (e *composeEnv) writeCode(ctx context.Context, file, code string) error {
	var last error
	for _, n := range e.nodes {
		if nodeNumber(n.ref) == lateNode {
			continue
		}
		last = e.composeIn(ctx, code+"\n", "exec", "-T", n.ref.Name, "sh", "-c", "cat > "+file)
		if last == nil {
			return nil
		}
	}
	return last
}

// LateJoin starts n6 and its worker (the `late` profile) joining by the code written to
// /codes/late, waits for it to be ready and returns it with the index the cluster gave it.
func (e *composeEnv) LateJoin(ctx context.Context, code string) (soak.NodeRef, error) {
	if err := e.writeCode(ctx, "/codes/late", code); err != nil {
		return soak.NodeRef{}, err
	}
	late := containerNode(lateNode)
	if _, err := e.composeLate(ctx, "up", "-d", "--no-recreate", late.ref.Name, workerName(late.ref)); err != nil {
		return soak.NodeRef{}, err
	}
	if !slices.ContainsFunc(e.nodes, func(n composeNode) bool { return n.ref.Name == late.ref.Name }) {
		e.nodes = append(e.nodes, late)
	}
	if err := e.waitReady(ctx, late.ref, 180*time.Second); err != nil {
		return soak.NodeRef{}, err
	}
	if _, err := e.Nodes(ctx); err != nil { // resolves its index and mesh address
		return soak.NodeRef{}, err
	}
	return e.node(late.ref).ref, nil
}

// RemoveLate stops n6 and its worker and deletes their state, so the next phase 25 starts
// it afresh; a run that never started it has nothing to do.
func (e *composeEnv) RemoveLate(ctx context.Context) error {
	late := containerNode(lateNode)
	if _, err := e.composeLate(ctx, "rm", "-s", "-f", "-v", late.ref.Name, workerName(late.ref)); err != nil {
		return err
	}
	project := os.Getenv("COMPOSE_PROJECT_NAME")
	if project == "" {
		project = strings.ToLower(filepath.Base(e.dir))
	}
	// the named volumes outlive the containers: removed here, retried while the engine
	// still counts a container just removed as using them, and checked gone
	vols := []string{project + "_" + late.ref.Name, project + "_" + late.ref.Name + "-d1"}
	var last error
	for range 10 {
		cmd := exec.CommandContext(ctx, "docker", append([]string{"volume", "rm", "-f"}, vols...)...) //nolint:gosec // the rig's own volumes
		out, err := cmd.CombinedOutput()
		if err == nil {
			if left, _ := exec.CommandContext(ctx, "docker", append([]string{"volume", "inspect"}, vols...)...).Output(); len(bytes.TrimSpace(left)) <= 2 { //nolint:gosec // the rig's own volumes
				last = nil
				break
			}
		}
		last = fmt.Errorf("docker volume rm %v: %w: %s", vols, err, bytes.TrimSpace(out))
		if err := e.Wait(ctx, time.Second); err != nil {
			return err
		}
	}
	if last != nil {
		return last
	}
	e.nodes = slices.DeleteFunc(e.nodes, func(n composeNode) bool { return n.ref.Name == late.ref.Name })
	return nil
}

// TryJoin runs the throwaway ntry container once with the code in /codes/try: the output
// of its `tailraid join` and the failure, if the join failed.
func (e *composeEnv) TryJoin(ctx context.Context, code string) (string, error) {
	if err := e.writeCode(ctx, "/codes/try", code); err != nil {
		return "", err
	}
	out, err := e.composeLate(ctx, "run", "--rm", "-T", "--no-deps", "ntry")
	return string(out), err
}

// RejoinAfterReset leaves the node a code in its home and restarts it: the entry script
// resets its Raft state and identity and joins again by the code (test/soak/entry.sh).
func (e *composeEnv) RejoinAfterReset(ctx context.Context, n soak.NodeRef, code string) error {
	if err := e.composeIn(ctx, code+"\n", "exec", "-T", n.Name, "sh", "-c", `cat > "$TAILRAID_HOME/rejoin"`); err != nil {
		return err
	}
	return e.Restart(ctx, []soak.NodeRef{n})
}

var _ soak.MeshEnv = (*composeEnv)(nil)
