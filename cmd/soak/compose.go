package main

import (
	"bytes"
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os/exec"
	"strings"
	"time"

	"github.com/zombar/tailraid/internal/admin"
	"github.com/zombar/tailraid/internal/meta"
	"github.com/zombar/tailraid/internal/soak"
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
	soak.Admin
	dir   string // compose project directory
	file  string // compose file in dir: docker-compose.yml (4 hosts) or docker-compose.single.yml
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

func (e *composeEnv) Caps() soak.Caps {
	return soak.Caps{Kill: true, Partition: true, Corrupt: true, MultiGateway: true, Disks: true}
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
