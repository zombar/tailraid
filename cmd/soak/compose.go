package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os/exec"
	"strings"
	"time"

	"github.com/zombar/tailraid/internal/admin"
	"github.com/zombar/tailraid/internal/meta"
	"github.com/zombar/tailraid/internal/repair"
	"github.com/zombar/tailraid/internal/rpc"
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
	dir   string // compose project directory
	nodes []composeNode
	http  *http.Client
}

func newComposeEnv(dir string, n int) *composeEnv {
	e := &composeEnv{dir: dir, http: &http.Client{Timeout: 90 * time.Second}}
	for i := 1; i <= n; i++ {
		e.nodes = append(e.nodes, composeNode{
			ref:    soak.NodeRef{Name: fmt.Sprintf("n%d", i), Idx: meta.NodeIdx(i), RPC: fmt.Sprintf("172.30.0.%d:20481", 10+i)},
			ip:     fmt.Sprintf("172.30.0.%d", 10+i),
			worker: fmt.Sprintf("http://127.0.0.1:%d", 20500+i),
		})
	}
	return e
}

func (e *composeEnv) Name() string { return fmt.Sprintf("docker compose, %d nodes", len(e.nodes)) }

func (e *composeEnv) Nodes(context.Context) ([]soak.NodeRef, error) {
	out := make([]soak.NodeRef, 0, len(e.nodes))
	for _, n := range e.nodes {
		out = append(out, n.ref)
	}
	return out, nil
}

func (e *composeEnv) node(ref soak.NodeRef) composeNode { return e.nodes[int(ref.Idx)-1] }

func (e *composeEnv) Caps() soak.Caps {
	return soak.Caps{Kill: true, Partition: true, Corrupt: true, MultiGateway: true}
}

func (e *composeEnv) Wait(ctx context.Context, d time.Duration) error {
	select {
	case <-time.After(d):
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
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
func (v *httpVolume) List(p string) ([]string, error) {
	r, err := v.do(fsReq{Op: "list", Path: p})
	return r.Names, err
}

// --- admin surface -------------------------------------------------------------------------

func (e *composeEnv) Status(ctx context.Context, ref soak.NodeRef) (admin.Report, error) {
	var rep admin.Report
	body, err := e.call(ctx, ref, http.MethodGet, "/v1/status", nil)
	if err != nil {
		return rep, err
	}
	err = json.Unmarshal(body, &rep)
	return rep, err
}

// leader finds the node that currently leads (by status).
func (e *composeEnv) leader(ctx context.Context) (soak.NodeRef, error) {
	var last error
	for _, n := range e.nodes {
		rep, err := e.Status(ctx, n.ref)
		if err != nil {
			last = err
			continue
		}
		for i := range rep.Nodes {
			if rep.Nodes[i].Leader {
				return e.nodes[int(rep.Nodes[i].Idx)-1].ref, nil
			}
		}
	}
	if last == nil {
		last = errors.New("no leader")
	}
	return soak.NodeRef{}, last
}

func (e *composeEnv) adminOp(ctx context.Context, ref soak.NodeRef, op rpc.AdminOp, out any) error {
	body, _ := json.Marshal(op)
	raw, err := e.call(ctx, ref, http.MethodPost, "/v1/admin", body)
	if err != nil {
		return err
	}
	if out == nil {
		return nil
	}
	return json.Unmarshal(raw, out)
}

func (e *composeEnv) leaderOp(ctx context.Context, op rpc.AdminOp, out any) error {
	var last error
	for attempt := 0; attempt < 5; attempt++ { // the leader may move between lookup and call
		l, err := e.leader(ctx)
		if err == nil {
			err = e.adminOp(ctx, l, op, out)
		}
		if err == nil {
			return nil
		}
		last = err
		if werr := e.Wait(ctx, time.Second); werr != nil {
			return werr
		}
	}
	return last
}

func (e *composeEnv) RepairNow(ctx context.Context) (repair.Stats, error) {
	var st repair.Stats
	err := e.leaderOp(ctx, rpc.AdminOp{Op: "repair_now"}, &st)
	return st, err
}

func (e *composeEnv) Scrub(ctx context.Context) (repair.Stats, error) {
	var rep struct {
		Scan repair.Stats `json:"scan"`
	}
	err := e.leaderOp(ctx, rpc.AdminOp{Op: "check", Scrub: true}, &rep)
	return rep.Scan, err
}

func (e *composeEnv) GCNow(ctx context.Context) (repair.Result, error) {
	var res repair.Result
	err := e.leaderOp(ctx, rpc.AdminOp{Op: "gc_now"}, &res)
	return res, err
}

func (e *composeEnv) SetPolicy(ctx context.Context, k, m uint8) error {
	return e.leaderOp(ctx, rpc.AdminOp{Op: "set_policy", K: k, M: m}, nil)
}

func (e *composeEnv) SetState(ctx context.Context, ref soak.NodeRef, state meta.NodeState) error {
	l, err := e.leader(ctx)
	if err != nil {
		return err
	}
	body, _ := json.Marshal(rpc.MemberOp{Op: "set_state", NodeIdx: ref.Idx, State: state})
	_, err = e.call(ctx, l, http.MethodPost, "/v1/members", body)
	return err
}

func (e *composeEnv) Fsck(ctx context.Context, ref soak.NodeRef) (bool, string, error) {
	var rep struct {
		Violations []string `json:"violations"`
		Orphans    int      `json:"orphan_shards_on_this_node"`
		OK         bool     `json:"ok"`
	}
	if err := e.adminOp(ctx, ref, rpc.AdminOp{Op: "fsck"}, &rep); err != nil {
		return false, "", err
	}
	return rep.OK, fmt.Sprintf("%d violations, %d orphan shards", len(rep.Violations), rep.Orphans) + strings.Join(rep.Violations, "; "), nil
}

func (e *composeEnv) Chunks(ctx context.Context, ref soak.NodeRef, path string) ([]admin.ChunkReport, error) {
	var reps []admin.ChunkReport
	err := e.adminOp(ctx, ref, rpc.AdminOp{Op: "chunk", Chunk: path}, &reps)
	return reps, err
}

func (e *composeEnv) FSMHash(ctx context.Context, ref soak.NodeRef) (uint64, string, error) {
	var h struct {
		Applied uint64 `json:"applied"`
		Hash    string `json:"hash"`
	}
	body, err := e.call(ctx, ref, http.MethodGet, "/dev/fsm-hash", nil)
	if err != nil {
		return 0, "", err
	}
	err = json.Unmarshal(body, &h)
	return h.Applied, h.Hash, err
}

func (e *composeEnv) Corrupt(ctx context.Context, ref soak.NodeRef, key string) error {
	_, err := e.call(ctx, ref, http.MethodPost, "/dev/corrupt?key="+key, nil)
	return err
}

// --- faults through docker compose and the workers ----------------------------------------

func (e *composeEnv) compose(ctx context.Context, args ...string) error {
	cmd := exec.CommandContext(ctx, "docker", append([]string{"compose"}, args...)...)
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
			resp.Body.Close()
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
