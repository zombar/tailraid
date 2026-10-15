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
	"strings"
	"sync"
	"time"

	"github.com/zombar/tailraid/internal/admin"
	"github.com/zombar/tailraid/internal/cluster"
	"github.com/zombar/tailraid/internal/meta"
	"github.com/zombar/tailraid/internal/rpc"
	"github.com/zombar/tailraid/internal/s3"
	"github.com/zombar/tailraid/internal/soak"
	"github.com/zombar/tailraid/internal/users"
)

// composeNode is one node of test/soak/docker-compose.yml with its worker sidecar.
type composeNode struct {
	ref    soak.NodeRef
	ip     string
	worker string // http://127.0.0.1:2050X (published port of the sidecar)
}

// composeEnv drives the compose cluster from the host through the worker sidecars and the
// docker compose CLI (docs/09 §6).
type composeEnv struct {
	// the cluster ID and each node's ID, for signing (identityOf)
	idMu    sync.Mutex
	cid     [16]byte
	nodeIDs map[string]string
	act     *users.Identity // ActAs: every mutating RPC is signed by it (under idMu)

	soak.Admin
	dir   string // compose project directory
	file  string // compose file in dir: docker-compose.yml (4 hosts with disks + a diskless one) or docker-compose.single.yml
	nodes []composeNode
	http  *http.Client
}

func newComposeEnv(dir, file string, n int) *composeEnv {
	e := &composeEnv{dir: dir, file: file, http: &http.Client{Timeout: 90 * time.Second}}
	for i := 1; i <= n; i++ {
		e.nodes = append(e.nodes, composeNode{
			ref:    soak.NodeRef{Name: fmt.Sprintf("n%d", i), Idx: meta.NodeIdx(i), RPC: fmt.Sprintf("172.30.0.%d:20481", 10+i)},
			ip:     fmt.Sprintf("172.30.0.%d", 10+i),
			worker: fmt.Sprintf("http://127.0.0.1:%d", 20500+i),
		})
		e.Seeds = append(e.Seeds, e.nodes[i-1].ref.RPC)
	}
	e.Caller = e
	return e
}

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

// resolveIndices sets each compose node's index from the status report, matched by name.
func resolveIndices(nodes []composeNode, members []admin.NodeReport) error {
	for i := range nodes {
		found := false
		for j := range members {
			if members[j].Name == nodes[i].ref.Name {
				nodes[i].ref.Idx, found = members[j].Idx, true
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

// webPort is the browser UI's container port; the compose files publish node i's on
// 127.0.0.1:2052i, so the driver dials the published port while the request names the
// node's own address, which its Host check expects (docs/13 §2, I-127).
const webPort = "20482"

func (e *composeEnv) WebClient() *http.Client {
	dialer := &net.Dialer{Timeout: 10 * time.Second}
	return &http.Client{Timeout: 90 * time.Second, Transport: &http.Transport{
		DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
			if host, port, err := net.SplitHostPort(addr); err == nil && port == webPort {
				for i, n := range e.nodes {
					if n.ip == host {
						addr = fmt.Sprintf("127.0.0.1:%d", 20520+i+1)
					}
				}
			}
			return dialer.DialContext(ctx, network, addr)
		},
	}}
}

func (e *composeEnv) Caps() soak.Caps {
	return soak.Caps{Kill: true, Partition: true, Corrupt: true, MultiGateway: true, Disks: true, Mutate: true, S3: e.file == multiFile, AddS3: e.file == multiFile, Stats: true, Mirror: e.file == multiFile, Users: true}
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
	Sig    string `json:"sig,omitempty"` // a Tailraid-Sig header for the node (docs/21 §2)
}

type rpcResp struct {
	Body  []byte `json:"body,omitempty"`
	Error string `json:"error,omitempty"`
}

func (e *composeEnv) call(ctx context.Context, ref soak.NodeRef, method, path string, body []byte) ([]byte, error) {
	sig, err := e.actSig(ctx, ref, method, path, body)
	if err != nil {
		return nil, err
	}
	var resp rpcResp
	if err := e.post(ctx, e.node(ref), "/rpc", rpcReq{Method: method, Path: path, Body: body, Sig: sig}, &resp); err != nil {
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

// byRPC maps a node RPC address (host:port) to its NodeRef.
func (e *composeEnv) byRPC(addr string) (soak.NodeRef, bool) {
	for _, n := range e.nodes {
		if n.ref.RPC == addr {
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
	cmd := exec.CommandContext(ctx, "docker", append([]string{"compose", "-f", e.file}, args...)...) //nolint:gosec // the driver runs docker compose on its own rig
	cmd.Dir = e.dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("docker compose %s: %w: %s", strings.Join(args, " "), err, bytes.TrimSpace(out))
	}
	return nil
}

// composeIn runs docker compose with stdin (a secret: never in the arguments or an error).
func (e *composeEnv) composeIn(ctx context.Context, stdin string, args ...string) error {
	cmd := exec.CommandContext(ctx, "docker", append([]string{"compose", "-f", e.file}, args...)...) //nolint:gosec // the driver runs docker compose on its own rig
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
	cmd := exec.CommandContext(ctx, "docker", append([]string{"compose", "-f", e.file}, args...)...) //nolint:gosec // the driver runs docker compose on its own rig
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

// --- users (phases 28–31): the bootstrap invite, signed ops, the worker's uid ---------------

// BootstrapInvite runs `tailraid user bootstrap` in the node's container (as root, the
// daemon's user there) and returns the code it printed.
func (e *composeEnv) BootstrapInvite(ctx context.Context, n soak.NodeRef, name string) (string, error) {
	out, err := e.composeOut(ctx, "exec", "-T", n.Name, "tailraid", "user", "bootstrap", "--name", name)
	if err != nil {
		return "", fmt.Errorf("user bootstrap on %s: %w: %s", n.Name, err, strings.TrimSpace(string(out)))
	}
	for line := range strings.SplitSeq(string(out), "\n") {
		if i := strings.Index(line, "tailraid user create "); i >= 0 {
			if f := strings.Fields(line[i:]); len(f) >= 5 {
				return f[4], nil
			}
		}
	}
	return "", fmt.Errorf("user bootstrap on %s printed no invite:\n%s", n.Name, out)
}

// SignedOp sends an admin op to n through its worker, signed by id for n's own node ID
// (the verifier binds a signature to its target, I-344).
func (e *composeEnv) SignedOp(ctx context.Context, n soak.NodeRef, id *users.Identity, op rpc.AdminOp) (json.RawMessage, error) {
	body, err := json.Marshal(op)
	if err != nil {
		return nil, err
	}
	sig := ""
	if id != nil {
		cid, nodeID, err := e.identityOf(ctx, n)
		if err != nil {
			return nil, err
		}
		nonce, err := users.NewNonce()
		if err != nil {
			return nil, err
		}
		sig = users.Sign(*id, cid, nodeID, http.MethodPost, "/v1/admin", body, time.Now().Unix(), nonce).String()
	}
	var resp rpcResp
	if err := e.post(ctx, e.node(n), "/rpc", rpcReq{Method: http.MethodPost, Path: "/v1/admin", Body: body, Sig: sig}, &resp); err != nil {
		return nil, err
	}
	if resp.Error != "" {
		return nil, errors.New(resp.Error)
	}
	return resp.Body, nil
}

// identityOf is the cluster ID and n's node ID, from n's status (cached).
func (e *composeEnv) identityOf(ctx context.Context, n soak.NodeRef) ([16]byte, string, error) {
	e.idMu.Lock()
	defer e.idMu.Unlock()
	if e.nodeIDs == nil {
		e.nodeIDs = map[string]string{}
	}
	if nodeID, ok := e.nodeIDs[n.Name]; ok && e.cid != ([16]byte{}) {
		return e.cid, nodeID, nil
	}
	rep, err := e.Status(ctx, n)
	if err != nil {
		return [16]byte{}, "", err
	}
	raw, err := hex.DecodeString(rep.ClusterID)
	if err != nil || len(raw) != 16 {
		return [16]byte{}, "", fmt.Errorf("status of %s: cluster id %q", n.Name, rep.ClusterID)
	}
	copy(e.cid[:], raw)
	for i := range rep.Nodes {
		if row := &rep.Nodes[i]; row.Idx == n.Idx {
			e.nodeIDs[n.Name] = row.ID
			return e.cid, row.ID, nil
		}
	}
	return [16]byte{}, "", fmt.Errorf("status of %s lists no row %d", n.Name, n.Idx)
}

// ActAs signs every mutating RPC the environment sends from now on as id (docs/21 §2):
// a run with -users-first makes the admin before phase 1, and the cluster then refuses
// an unsigned admin or member op (phase 0, I-362).
func (e *composeEnv) ActAs(id *users.Identity) {
	e.idMu.Lock()
	defer e.idMu.Unlock()
	e.act = id
}

// actSig is the Tailraid-Sig of a mutating request to ref as the acting identity, ""
// when nobody acts or the request reads.
func (e *composeEnv) actSig(ctx context.Context, ref soak.NodeRef, method, path string, body []byte) (string, error) {
	if method == http.MethodGet || !strings.HasPrefix(path, "/v1/") { // before the lock: identityOf reads status under it
		return "", nil
	}
	e.idMu.Lock()
	id := e.act
	e.idMu.Unlock()
	if id == nil {
		return "", nil
	}
	cid, nodeID, err := e.identityOf(ctx, ref)
	if err != nil {
		return "", err
	}
	nonce, err := users.NewNonce()
	if err != nil {
		return "", err
	}
	return users.Sign(*id, cid, nodeID, method, path, body, time.Now().Unix(), nonce).String(), nil
}

// MountUID is the worker's: it mounts the volume as root (AUTH_SYS uid 0).
func (e *composeEnv) MountUID() int { return 0 }

var _ soak.UsersEnv = (*composeEnv)(nil)
