// Command soak-client is the worker sidecar of the compose soak suite (docs/09 §6). It
// shares a node's network namespace, so it reaches the node's loopback NFS gateway through
// the pure-Go NFSv3 client (Docker Desktop has no kernel NFS client) and the node's RPC
// from the node's own IP (a WhoIs member). It exposes both to the driver on the host:
//
//	POST /fs         {op, path, off, data, size, to}  → {data, size, names, error}
//	POST /rpc        {method, path, body}              → {body, error}
//	POST /partition  {ips: [...]}                      drop traffic to/from those peers
//	POST /heal                                         lift every drop rule
//	GET  /ready                                        200 once gateway and RPC answer
//
// Usage: soak-client worker <listen> <nfs gateway> <node rpc addr>
package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"os/exec"
	"path"
	"runtime/pprof"
	"strings"
	"sync"
	"time"

	nfsc "github.com/willscott/go-nfs-client/nfs"
	nfsrpc "github.com/willscott/go-nfs-client/nfs/rpc"
	"github.com/willscott/go-nfs-client/nfs/xdr"

	"github.com/zombar/tailraid/internal/config"
	"github.com/zombar/tailraid/internal/rpc"
)

func main() {
	if len(os.Args) != 5 || os.Args[1] != "worker" {
		fmt.Fprintln(os.Stderr, "usage: soak-client worker <listen> <nfs gateway host:port> <node rpc host:port>")
		os.Exit(2)
	}
	// same log format as the nodes (TAILRAID_LOG_FORMAT / TAILRAID_LOG_LEVEL)
	logger := config.Default().Logger().With("src", "soak-worker")
	slog.SetDefault(logger)
	w := &worker{nfsAddr: os.Args[3], rpcAddr: os.Args[4]}
	w.setupChain()
	mux := http.NewServeMux()
	mux.HandleFunc("GET /ready", w.ready)
	mux.HandleFunc("POST /fs", w.fs)
	mux.HandleFunc("POST /rpc", w.rpc)
	mux.HandleFunc("POST /partition", w.partition)
	mux.HandleFunc("POST /heal", w.heal)
	mux.HandleFunc("GET /goroutines", func(rw http.ResponseWriter, _ *http.Request) {
		rw.Header().Set("Content-Type", "text/plain; charset=utf-8")
		_ = pprof.Lookup("goroutine").WriteTo(rw, 2)
	})
	logger.Info("soak worker listening", "addr", os.Args[2], "nfs", w.nfsAddr, "rpc", w.rpcAddr)
	srv := &http.Server{Addr: os.Args[2], Handler: mux, ReadHeaderTimeout: 5 * time.Second, ErrorLog: slog.NewLogLogger(logger.Handler(), slog.LevelWarn)}
	if err := srv.ListenAndServe(); err != nil {
		logger.Error("worker stopped", "err", err)
		os.Exit(1)
	}
}

type worker struct {
	nfsAddr, rpcAddr string

	mu     sync.Mutex
	tgt    *nfsc.Target
	closeT func()
	gen    uint64 // bumped on every dropTarget: a request that outran its client must not drop the next one
	client *rpc.Client
}

// opDeadline bounds one gateway operation. The pure-Go NFS client times a call out after
// DefaultReadTimeout and forgets its reply channel, but a reply that lands after that
// blocks the client's receive loop for good (unbuffered channel, nobody reading), after
// which every call on that connection hangs. Two defences: the call timeout is raised
// well above the gateway's longest legitimate stall (a metadata operation retries for up
// to the 30 s Stale threshold during a leader election), and a request that still
// overruns opDeadline answers with an error and drops the connection so the next request
// dials a fresh client, leaving the stuck goroutine behind.
const opDeadline = 75 * time.Second

func init() { nfsrpc.DefaultReadTimeout = 60 * time.Second }

// --- NFS target ------------------------------------------------------------------------

func (w *worker) target() (*nfsc.Target, uint64, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.tgt != nil {
		return w.tgt, w.gen, nil
	}
	cl, err := nfsrpc.DialTCP("tcp", w.nfsAddr, false)
	if err != nil {
		return nil, 0, err
	}
	type mountArgs struct {
		nfsrpc.Header
		Dirpath string
	}
	res, err := cl.Call(&mountArgs{Rpcvers: 2, Prog: nfsc.MountProg, Vers: nfsc.MountVers, Proc: nfsc.MountProc3MNT, Cred: nfsrpc.AuthNull, Verf: nfsrpc.AuthNull, Dirpath: "/"})
	if err != nil {
		cl.Close()
		return nil, 0, err
	}
	if status, _ := xdr.ReadUint32(res); status != nfsc.MNT3Ok {
		cl.Close()
		return nil, 0, fmt.Errorf("mount status %d", status)
	}
	fh, err := xdr.ReadOpaque(res)
	if err != nil {
		cl.Close()
		return nil, 0, err
	}
	tgt, err := nfsc.NewTargetWithClient(cl, nfsrpc.NewAuthUnix("soak", 0, 0).Auth(), fh, "/", 0)
	if err != nil {
		cl.Close()
		return nil, 0, err
	}
	w.tgt, w.closeT = tgt, cl.Close
	return tgt, w.gen, nil
}

// dropCurrent drops whatever connection is current.
func (w *worker) dropCurrent() {
	w.mu.Lock()
	gen := w.gen
	w.mu.Unlock()
	w.dropTarget(gen)
}

// dropTarget forgets the connection of generation gen after a transport error or an
// overrun (the node may have restarted, or the client wedged); a later generation is
// left alone.
func (w *worker) dropTarget(gen uint64) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.gen != gen {
		return
	}
	if w.closeT != nil {
		w.closeT()
	}
	w.tgt, w.closeT = nil, nil
	w.gen++
}

type fsReq struct {
	Op   string `json:"op"` // mkdir | write | read | truncate | rename | remove | size | list
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

func (w *worker) fs(rw http.ResponseWriter, r *http.Request) {
	var req fsReq
	if err := json.NewDecoder(http.MaxBytesReader(rw, r.Body, 64<<20)).Decode(&req); err != nil {
		http.Error(rw, err.Error(), http.StatusBadRequest)
		return
	}
	resp := w.doFS(req)
	rw.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(rw).Encode(resp)
}

func (w *worker) doFS(req fsReq) fsResp {
	t, gen, err := w.target()
	if err != nil {
		return fsResp{Error: "gateway: " + err.Error()}
	}
	done := make(chan fsResp, 1)
	go func() { done <- w.doFSOn(t, req) }()
	select {
	case resp := <-done:
		if resp.Error != "" && (strings.Contains(resp.Error, "EOF") || strings.Contains(resp.Error, "connection")) {
			w.dropTarget(gen)
		}
		return resp
	case <-time.After(opDeadline):
		slog.Warn("gateway operation overran; dropping the NFS client", "op", req.Op, "path", req.Path, "deadline", opDeadline)
		w.dropTarget(gen)
		return fsResp{Error: fmt.Sprintf("gateway: %s %s overran %s", req.Op, req.Path, opDeadline)}
	}
}

// doFSOn runs one operation on a target.
func (w *worker) doFSOn(t *nfsc.Target, req fsReq) (resp fsResp) {
	fail := func(err error) fsResp {
		if err == nil {
			return fsResp{}
		}
		return fsResp{Error: err.Error()}
	}
	switch req.Op {
	case "mkdir":
		return fail(mkdirAll(t, req.Path))
	case "write":
		return fail(writeAt(t, req.Path, req.Off, req.Data))
	case "read":
		data, err := readAll(t, req.Path)
		if err != nil {
			return fail(err)
		}
		return fsResp{Data: data, Size: int64(len(data))}
	case "truncate":
		return fail(t.Setattr(req.Path, nfsc.Sattr3{Size: nfsc.SetSize{SetIt: true, Size: uint64(req.Size)}}))
	case "rename":
		return fail(rename(t, req.Path, req.To))
	case "remove":
		fi, _, err := t.Lookup(req.Path)
		if err != nil {
			return fail(err)
		}
		if fi.IsDir() {
			return fail(t.RmDir(req.Path))
		}
		return fail(t.Remove(req.Path))
	case "size":
		fi, _, err := t.Lookup(req.Path)
		if err != nil {
			return fail(err)
		}
		return fsResp{Size: fi.Size()}
	case "hash":
		sum, n, err := hashFile(t, req.Path)
		if err != nil {
			return fail(err)
		}
		return fsResp{Hash: hex.EncodeToString(sum[:]), Size: n}
	case "list":
		ents, err := t.ReadDirPlus(req.Path)
		if err != nil {
			return fail(err)
		}
		var names []string
		for _, e := range ents {
			if n := e.FileName; n != "." && n != ".." {
				names = append(names, n)
			}
		}
		return fsResp{Names: names}
	default:
		return fsResp{Error: "unknown op " + req.Op}
	}
}

func writeAt(t *nfsc.Target, p string, off int64, data []byte) error {
	if _, _, err := t.Lookup(p); err != nil {
		if _, err := t.Create(p, 0o644); err != nil {
			return fmt.Errorf("create %s: %w", p, err)
		}
	}
	if len(data) == 0 {
		return nil
	}
	f, err := t.OpenFile(p, 0o644)
	if err != nil {
		return fmt.Errorf("open %s: %w", p, err)
	}
	defer f.Close()
	if _, err := f.Seek(off, io.SeekStart); err != nil {
		return err
	}
	for len(data) > 0 { // the client library writes in one RPC; keep pieces at 64 KiB
		n := min(len(data), 64<<10)
		if _, err := f.Write(data[:n]); err != nil {
			return fmt.Errorf("write %s@%d: %w", p, off, err)
		}
		data = data[n:]
		off += int64(n)
	}
	return nil
}

// hashFile streams the file through sha256 in 64 KiB reads: a large file is checked
// without the worker holding it.
func hashFile(t *nfsc.Target, p string) ([32]byte, int64, error) {
	var sum [32]byte
	fi, _, err := t.Lookup(p)
	if err != nil {
		return sum, 0, err
	}
	f, err := t.Open(p)
	if err != nil {
		return sum, 0, err
	}
	defer f.Close()
	h := sha256.New()
	buf := make([]byte, 64<<10)
	var got int64
	for got < fi.Size() {
		n, err := f.ReadAt(buf[:min(int64(len(buf)), fi.Size()-got)], got)
		if n == 0 && err != nil {
			return sum, got, err
		}
		h.Write(buf[:n])
		got += int64(n)
	}
	copy(sum[:], h.Sum(nil))
	return sum, got, nil
}

func readAll(t *nfsc.Target, p string) ([]byte, error) {
	fi, _, err := t.Lookup(p)
	if err != nil {
		return nil, err
	}
	f, err := t.Open(p)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	out := make([]byte, fi.Size())
	var got int64
	for got < int64(len(out)) {
		n, err := f.ReadAt(out[got:min(got+(64<<10), int64(len(out)))], got)
		if n == 0 && err != nil {
			return nil, fmt.Errorf("read %s@%d: %w", p, got, err)
		}
		got += int64(n)
	}
	return out, nil
}

func rename(t *nfsc.Target, from, to string) error {
	fromDir, fromName := path.Split(from)
	toDir, toName := path.Split(to)
	_, ffh, err := t.Lookup(fromDir)
	if err != nil {
		return err
	}
	_, tfh, err := t.Lookup(toDir)
	if err != nil {
		return err
	}
	type renameArgs struct {
		nfsrpc.Header
		From nfsc.Diropargs3
		To   nfsc.Diropargs3
	}
	res, err := t.Call(&renameArgs{
		Rpcvers: 2, Prog: nfsc.Nfs3Prog, Vers: nfsc.Nfs3Vers, Proc: nfsc.NFSProc3Rename, Cred: nfsrpc.NewAuthUnix("soak", 0, 0).Auth(), Verf: nfsrpc.AuthNull,
		From: nfsc.Diropargs3{FH: ffh, Filename: fromName},
		To:   nfsc.Diropargs3{FH: tfh, Filename: toName},
	})
	if err != nil {
		return err
	}
	status, _ := xdr.ReadUint32(res)
	return nfsc.NFS3Error(status)
}

func mkdirAll(t *nfsc.Target, p string) error {
	cur := "/"
	for part := range strings.SplitSeq(strings.Trim(p, "/"), "/") {
		if part == "" {
			continue
		}
		cur = path.Join(cur, part)
		if _, _, err := t.Lookup(cur); err != nil {
			if _, err := t.Mkdir(cur, 0o755); err != nil {
				return fmt.Errorf("mkdir %s: %w", cur, err)
			}
		}
	}
	return nil
}

// --- node RPC ----------------------------------------------------------------------------

type rpcReq struct {
	Method string `json:"method"`
	Path   string `json:"path"`
	Body   []byte `json:"body"`
}

type rpcResp struct {
	Body  []byte `json:"body,omitempty"`
	Error string `json:"error,omitempty"`
}

func (w *worker) rpcClient(ctx context.Context) (*rpc.Client, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.client != nil {
		return w.client, nil
	}
	probe := rpc.NewClient([16]byte{}, nil, nil)
	h, err := probe.Health(ctx, w.rpcAddr)
	if err != nil {
		return nil, err
	}
	cid, err := hex.DecodeString(h.ClusterID)
	if err != nil || len(cid) != 16 {
		return nil, errors.New("bad cluster id in /health")
	}
	var id [16]byte
	copy(id[:], cid)
	w.client = rpc.NewClient(id, nil, nil)
	return w.client, nil
}

func (w *worker) rpc(rw http.ResponseWriter, r *http.Request) {
	var req rpcReq
	if err := json.NewDecoder(http.MaxBytesReader(rw, r.Body, 1<<20)).Decode(&req); err != nil {
		http.Error(rw, err.Error(), http.StatusBadRequest)
		return
	}
	var resp rpcResp
	ctx, cancel := context.WithTimeout(r.Context(), 60*time.Second)
	defer cancel()
	c, err := w.rpcClient(ctx)
	if err != nil {
		resp.Error = err.Error()
	} else if body, err := c.Call(ctx, w.rpcAddr, req.Method, req.Path, req.Body); err != nil {
		resp.Error = err.Error()
	} else {
		resp.Body = body
	}
	rw.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(rw).Encode(resp)
}

func (w *worker) ready(rw http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()
	if _, err := w.rpcClient(ctx); err != nil {
		http.Error(rw, "rpc: "+err.Error(), http.StatusServiceUnavailable)
		return
	}
	if _, _, err := w.target(); err != nil {
		http.Error(rw, "nfs: "+err.Error(), http.StatusServiceUnavailable)
		return
	}
	rw.WriteHeader(http.StatusNoContent)
}

// --- partitions (iptables chain SOAK on INPUT and OUTPUT) ---------------------------------

func iptables(args ...string) error {
	out, err := exec.CommandContext(context.Background(), "iptables", args...).CombinedOutput() //nolint:gosec // the soak client partitions its own container
	if err != nil {
		return fmt.Errorf("iptables %s: %w: %s", strings.Join(args, " "), err, bytes.TrimSpace(out))
	}
	return nil
}

func (w *worker) setupChain() {
	_ = iptables("-N", "SOAK")
	_ = iptables("-F", "SOAK")
	if err := iptables("-C", "INPUT", "-j", "SOAK"); err != nil {
		_ = iptables("-I", "INPUT", "-j", "SOAK")
	}
	if err := iptables("-C", "OUTPUT", "-j", "SOAK"); err != nil {
		_ = iptables("-I", "OUTPUT", "-j", "SOAK")
	}
}

func (w *worker) partition(rw http.ResponseWriter, r *http.Request) {
	var req struct {
		IPs []string `json:"ips"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(rw, r.Body, 64<<10)).Decode(&req); err != nil {
		http.Error(rw, err.Error(), http.StatusBadRequest)
		return
	}
	if err := iptables("-F", "SOAK"); err != nil {
		http.Error(rw, err.Error(), http.StatusInternalServerError)
		return
	}
	for _, ip := range req.IPs {
		for _, rule := range [][]string{{"-A", "SOAK", "-s", ip, "-j", "DROP"}, {"-A", "SOAK", "-d", ip, "-j", "DROP"}} {
			if err := iptables(rule...); err != nil {
				http.Error(rw, err.Error(), http.StatusInternalServerError)
				return
			}
		}
	}
	w.dropCurrent() // a partition rule set: the next request dials afresh
	rw.WriteHeader(http.StatusNoContent)
}

func (w *worker) heal(rw http.ResponseWriter, _ *http.Request) {
	if err := iptables("-F", "SOAK"); err != nil {
		http.Error(rw, err.Error(), http.StatusInternalServerError)
		return
	}
	rw.WriteHeader(http.StatusNoContent)
}
