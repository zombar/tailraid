// Command soak-client is the worker sidecar of the compose soak suite (docs/09 §6). It
// shares a node's network namespace, so it reaches the node's loopback NFS gateway through
// the pure-Go NFSv3 client (Docker Desktop has no kernel NFS client) and the node's RPC
// from the node's own IP (a WhoIs member). It exposes both to the driver on the host:
//
//	POST /fs         {op, path, off, data, size, to}  → {data, size, names, error}
//	POST /rpc        {method, path, body}              → {body, error}
//	POST /partition  {ips: [...]}                      drop traffic to/from those peers
//	POST /heal                                         lift every drop rule
//	POST /copy       {proto, files}                    → {id}: copies the way Explorer does
//	GET  /copy/{id}                                    → {done, results}
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
	"net"
	"net/http"
	"net/http/httputil"
	"net/netip"
	"net/url"
	"os"
	"os/exec"
	"path"
	"runtime/pprof"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/cloudsoda/go-smb2"
	nfsc "github.com/willscott/go-nfs-client/nfs"
	nfsrpc "github.com/willscott/go-nfs-client/nfs/rpc"
	"github.com/willscott/go-nfs-client/nfs/xdr"

	"github.com/zombar/tailraid/internal/config"
	"github.com/zombar/tailraid/internal/mesh/portmap"
	"github.com/zombar/tailraid/internal/rpc"
	"github.com/zombar/tailraid/internal/smbgw"
	"github.com/zombar/tailraid/internal/soak"
	"github.com/zombar/tailraid/internal/soak/smbclient"
)

func main() {
	if len(os.Args) == 6 && os.Args[1] == "natpmp" {
		runResponder(os.Args[2], os.Args[3], os.Args[4], os.Args[5])
		return
	}
	if len(os.Args) != 5 || os.Args[1] != "worker" {
		fmt.Fprintln(os.Stderr, "usage: soak-client worker <listen> <nfs gateway host:port> <mesh interface> | natpmp <listen> <external ip> <on file> <reset file>")
		os.Exit(2)
	}
	// same log format as the nodes (TAILRAID_LOG_FORMAT / TAILRAID_LOG_LEVEL)
	logger := config.Default().Logger().With("src", "soak-worker")
	slog.SetDefault(logger)
	w := &worker{nfsAddr: os.Args[3], ifname: os.Args[4]}
	w.setupChain()
	go w.serveWebProxy(logger)
	mux := http.NewServeMux()
	mux.HandleFunc("GET /ready", w.ready)
	mux.HandleFunc("POST /fs", w.fs)
	mux.HandleFunc("POST /rpc", w.rpc)
	mux.HandleFunc("POST /partition", w.partition)
	mux.HandleFunc("POST /heal", w.heal)
	mux.HandleFunc("POST /copy", w.copyStart)
	mux.HandleFunc("GET /copy/{id}", w.copyState)
	mux.HandleFunc("POST /smbraces", w.smbRaces)
	mux.HandleFunc("POST /smbhold", w.smbHold)
	mux.HandleFunc("POST /smbrelease", w.smbRelease)
	mux.HandleFunc("GET /goroutines", func(rw http.ResponseWriter, _ *http.Request) {
		rw.Header().Set("Content-Type", "text/plain; charset=utf-8")
		_ = pprof.Lookup("goroutine").WriteTo(rw, 2)
	})
	logger.Info("soak worker listening", "addr", os.Args[2], "nfs", w.nfsAddr, "mesh interface", w.ifname)
	srv := &http.Server{Addr: os.Args[2], Handler: mux, ReadHeaderTimeout: 5 * time.Second, ErrorLog: slog.NewLogLogger(logger.Handler(), slog.LevelWarn)}
	if err := srv.ListenAndServe(); err != nil {
		logger.Error("worker stopped", "err", err)
		os.Exit(1)
	}
}

type worker struct {
	nfsAddr string
	ifname  string // the node's mesh interface (docs/20 §1): its address carries the RPC and the browser UI

	mu     sync.Mutex
	tgt    *nfsc.Target
	closeT func()
	gen    uint64 // bumped on every dropTarget: a request that outran its client must not drop the next one
	client *rpc.Client

	jobs   map[string]*copyJob     // soak phase 22's copies, by id
	held   map[string]func() error // phase 23's delete-on-close handles held open, by id
	nextID int
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
	tgt, closeT, err := dialTarget(w.nfsAddr)
	if err != nil {
		return nil, 0, err
	}
	w.tgt, w.closeT = tgt, closeT
	return tgt, w.gen, nil
}

// dialTarget mounts the gateway's root over a connection of its own.
func dialTarget(addr string) (*nfsc.Target, func(), error) {
	cl, err := nfsrpc.DialTCP("tcp", addr, false)
	if err != nil {
		return nil, nil, err
	}
	type mountArgs struct {
		nfsrpc.Header
		Dirpath string
	}
	res, err := cl.Call(&mountArgs{Rpcvers: 2, Prog: nfsc.MountProg, Vers: nfsc.MountVers, Proc: nfsc.MountProc3MNT, Cred: nfsrpc.AuthNull, Verf: nfsrpc.AuthNull, Dirpath: "/"})
	if err != nil {
		cl.Close()
		return nil, nil, err
	}
	if status, _ := xdr.ReadUint32(res); status != nfsc.MNT3Ok {
		cl.Close()
		return nil, nil, fmt.Errorf("mount status %d", status)
	}
	fh, err := xdr.ReadOpaque(res)
	if err != nil {
		cl.Close()
		return nil, nil, err
	}
	tgt, err := nfsc.NewTargetWithClient(cl, nfsrpc.NewAuthUnix("soak", 0, 0).Auth(), fh, "/", 0)
	if err != nil {
		cl.Close()
		return nil, nil, err
	}
	return tgt, func() { cl.Close() }, nil
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
				// as mkdir -p: a folder another client made meanwhile (NFS3ERR_EXIST, now
				// decided across gateways, I-238) is the folder this one wanted
				if fi, _, lerr := t.Lookup(cur, false); !errors.Is(err, os.ErrExist) || lerr != nil || !fi.IsDir() {
					return fmt.Errorf("mkdir %s: %w", cur, err)
				}
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

// meshAddr is the node's mesh address, read from its interface each time: the worker
// shares the node's network namespace, and the address exists only once the node has
// joined (a joiner gets it from the introduce, docs/20 §4).
func (w *worker) meshAddr() (string, error) {
	ifc, err := net.InterfaceByName(w.ifname)
	if err != nil {
		return "", fmt.Errorf("mesh interface %s: %w", w.ifname, err)
	}
	addrs, err := ifc.Addrs()
	if err != nil {
		return "", err
	}
	for _, a := range addrs {
		if ipn, ok := a.(*net.IPNet); ok && ipn.IP.To4() != nil {
			return ipn.IP.String(), nil
		}
	}
	return "", fmt.Errorf("mesh interface %s has no IPv4 address yet", w.ifname)
}

// rpcAddr is the node's RPC on its mesh address.
func (w *worker) rpcAddr() (string, error) {
	addr, err := w.meshAddr()
	if err != nil {
		return "", err
	}
	return net.JoinHostPort(addr, rpcPort), nil
}

// The node's ports on its mesh address, and the worker's proxy of the browser UI: the
// compose files publish webProxyAddr, since a published port reaches the container's
// network address and not its mesh address.
const (
	rpcPort      = "20481"
	webPort      = "20482"
	webProxyAddr = "0.0.0.0:20496"
)

// serveWebProxy serves the node's browser UI (docs/13) on webProxyAddr, resolving the mesh
// address per request and naming it in the Host header, which the UI's check expects
// (I-127); the paths are passed through unchanged (api.ts uses absolute /api/ paths).
func (w *worker) serveWebProxy(logger *slog.Logger) {
	proxy := &httputil.ReverseProxy{
		Rewrite: func(r *httputil.ProxyRequest) {
			addr, err := w.meshAddr()
			if err != nil {
				addr = "127.0.0.1" // refused by nothing listening: a 502 to the driver
			}
			target := &url.URL{Scheme: "http", Host: net.JoinHostPort(addr, webPort)}
			r.SetURL(target)
			r.Out.Host = target.Host
		},
		ErrorLog: slog.NewLogLogger(logger.Handler(), slog.LevelWarn),
	}
	srv := &http.Server{Addr: webProxyAddr, Handler: proxy, ReadHeaderTimeout: 5 * time.Second}
	if err := srv.ListenAndServe(); err != nil {
		logger.Error("web proxy stopped", "err", err)
	}
}

func (w *worker) rpcClient(ctx context.Context) (*rpc.Client, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.client != nil {
		return w.client, nil
	}
	addr, err := w.rpcAddr()
	if err != nil {
		return nil, err
	}
	probe := rpc.NewClient([16]byte{}, nil)
	h, err := probe.Health(ctx, addr)
	if err != nil {
		return nil, err
	}
	cid, err := hex.DecodeString(h.ClusterID)
	if err != nil || len(cid) != 16 {
		return nil, errors.New("bad cluster id in /health")
	}
	var id [16]byte
	copy(id[:], cid)
	w.client = rpc.NewClient(id, nil)
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
	addr := ""
	if err == nil {
		addr, err = w.rpcAddr()
	}
	if err != nil {
		resp.Error = err.Error()
	} else if body, err := c.Call(ctx, addr, req.Method, req.Path, req.Body); err != nil {
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

// partition drops every packet to and from the addresses given; with udp set, only UDP
// (the mesh's transport, docs/20 §5: the tunnel dies, the TCP rules stay untouched).
func (w *worker) partition(rw http.ResponseWriter, r *http.Request) {
	var req struct {
		IPs []string `json:"ips"`
		UDP bool     `json:"udp"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(rw, r.Body, 64<<10)).Decode(&req); err != nil {
		http.Error(rw, err.Error(), http.StatusBadRequest)
		return
	}
	if err := iptables("-F", "SOAK"); err != nil {
		http.Error(rw, err.Error(), http.StatusInternalServerError)
		return
	}
	proto := []string{}
	if req.UDP {
		proto = []string{"-p", "udp"}
	}
	for _, ip := range req.IPs {
		for _, rule := range [][]string{append(append([]string{"-A", "SOAK"}, proto...), "-s", ip, "-j", "DROP"), append(append([]string{"-A", "SOAK"}, proto...), "-d", ip, "-j", "DROP")} {
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

// --- copies (soak phase 22) ---------------------------------------------------------------

// smbAddr is the node's loopback SMB gateway; SMB_PASSWORD is the fixed password entry.sh
// writes into every node's config.
const smbAddr = "127.0.0.1:20445"

type copyJob struct {
	mu      sync.Mutex
	done    bool
	results []soak.CopyResult
}

func (w *worker) copyStart(rw http.ResponseWriter, r *http.Request) {
	var req struct {
		Proto string          `json:"proto"`
		Files []soak.CopySpec `json:"files"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(rw, r.Body, 1<<20)).Decode(&req); err != nil {
		http.Error(rw, err.Error(), http.StatusBadRequest)
		return
	}
	var run func() []soak.CopyResult
	switch req.Proto {
	case "smb":
		// one connection per distinct Conn, as several Windows clients have to a server
		run = func() []soak.CopyResult { return smbclient.Copy(context.Background(), smbDial, req.Files) }
	case "nfs":
		// connections of the job's own, one per distinct Conn: the shared one is dropped
		// when a partition is set, which would cut the copies rather than the cluster
		targets := map[int]*nfsc.Target{}
		var closers []func()
		for _, f := range req.Files {
			if _, ok := targets[f.Conn]; ok {
				continue
			}
			t, closeT, err := dialTarget(w.nfsAddr)
			if err != nil {
				for _, c := range closers {
					c()
				}
				http.Error(rw, "nfs: "+err.Error(), http.StatusServiceUnavailable)
				return
			}
			targets[f.Conn], closers = t, append(closers, closeT)
		}
		run = func() []soak.CopyResult {
			defer func() {
				for _, c := range closers {
					c()
				}
			}()
			results := make([]soak.CopyResult, len(req.Files))
			var wg sync.WaitGroup
			for i, f := range req.Files {
				wg.Go(func() { // concurrent calls on one target, as the worker's /fs makes
					results[i] = soak.CopyResult{Path: f.Path}
					if err := nfsCopy(targets[f.Conn], f); err != nil {
						results[i].Err = err.Error()
						results[i].Exists = errors.Is(err, os.ErrExist)
					}
				})
			}
			wg.Wait()
			return results
		}
	default:
		http.Error(rw, "unknown proto "+req.Proto, http.StatusBadRequest)
		return
	}
	job := &copyJob{}
	w.mu.Lock()
	if w.jobs == nil {
		w.jobs = map[string]*copyJob{}
	}
	w.nextID++
	id := fmt.Sprintf("%d", w.nextID)
	w.jobs[id] = job
	w.mu.Unlock()
	go func() {
		results := run()
		job.mu.Lock()
		job.done, job.results = true, results
		job.mu.Unlock()
	}()
	rw.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(rw).Encode(map[string]string{"id": id})
}

// smbDial opens a session to the node's loopback SMB gateway with the fixed password
// entry.sh writes into every node's config.
func smbDial(ctx context.Context) (*smb2.Share, func(), error) {
	d := &smb2.Dialer{Initiator: &smb2.NTLMInitiator{User: smbgw.User, Password: os.Getenv("SMB_PASSWORD")}}
	sess, err := d.Dial(ctx, smbAddr)
	if err != nil {
		return nil, nil, fmt.Errorf("smb: %w", err)
	}
	sh, err := sess.Mount(smbgw.ShareName)
	if err != nil {
		_ = sess.Logoff()
		return nil, nil, fmt.Errorf("smb: %w", err)
	}
	return sh, func() { _ = sh.Umount(); _ = sess.Logoff() }, nil
}

// nfsCopy copies one file over NFS with one handle held across every write: an
// exclusive copy creates with CREATE GUARDED (the client library sends only UNCHECKED,
// which would truncate a file another client made), a replacing one with UNCHECKED.
func nfsCopy(t *nfsc.Target, f soak.CopySpec) error {
	if f.Mode == soak.CopyReplace {
		if _, err := t.Create(f.Path, 0o644); err != nil {
			return fmt.Errorf("create: %w", err)
		}
	} else if err := createGuarded(t, f.Path); err != nil {
		return fmt.Errorf("create: %w", err)
	}
	if err := t.Setattr(f.Path, nfsc.Sattr3{Size: nfsc.SetSize{SetIt: true, Size: uint64(f.Size)}}); err != nil {
		return fmt.Errorf("size: %w", err)
	}
	h, err := t.OpenFile(f.Path, 0o644)
	if err != nil {
		return err
	}
	defer h.Close()
	data := soak.CopyPayload(f.Size, f.Seed)
	for off := 0; off < len(data); off += soak.CopyPiece {
		if _, err := h.Seek(int64(off), io.SeekStart); err != nil {
			return err
		}
		if _, err := h.Write(data[off:min(off+soak.CopyPiece, len(data))]); err != nil {
			return fmt.Errorf("write @%d: %w", off, err)
		}
	}
	return nil
}

// createGuarded sends CREATE GUARDED for p; NFS3ERR_EXIST is os.ErrExist.
func createGuarded(t *nfsc.Target, p string) error {
	dir, name := path.Split(p)
	_, fh, err := t.Lookup(dir)
	if err != nil {
		return err
	}
	type how struct {
		Mode uint32
		Attr nfsc.Sattr3
	}
	res, err := t.Call(&struct {
		Header nfsrpc.Header
		Where  nfsc.Diropargs3
		HW     how
	}{
		Header: nfsrpc.Header{Rpcvers: 2, Prog: nfsc.Nfs3Prog, Vers: nfsc.Nfs3Vers, Proc: nfsc.NFSProc3Create, Cred: nfsrpc.AuthNull, Verf: nfsrpc.AuthNull},
		Where:  nfsc.Diropargs3{FH: fh, Filename: name},
		HW:     how{Mode: 1, Attr: nfsc.Sattr3{Mode: nfsc.SetMode{SetIt: true, Mode: 0o644}}},
	})
	if err != nil {
		return err
	}
	switch status, err := xdr.ReadUint32(res); {
	case err != nil:
		return err
	case status == uint32(nfsc.NFS3ErrExist):
		return os.ErrExist
	case status != 0:
		return fmt.Errorf("create guarded: NFS status %d", status)
	}
	return nil
}

// smbRaces runs phase 23's SMB handle races (smbclient.Races) on this node's gateway.
func (w *worker) smbRaces(rw http.ResponseWriter, r *http.Request) {
	var req struct {
		Dir string `json:"dir"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(rw, r.Body, 1<<16)).Decode(&req); err != nil {
		http.Error(rw, err.Error(), http.StatusBadRequest)
		return
	}
	rw.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(rw).Encode(smbclient.Races(r.Context(), smbDial, req.Dir))
}

func (w *worker) copyState(rw http.ResponseWriter, r *http.Request) {
	w.mu.Lock()
	job := w.jobs[r.PathValue("id")]
	w.mu.Unlock()
	if job == nil {
		http.Error(rw, "no such job", http.StatusNotFound)
		return
	}
	job.mu.Lock()
	done, results := job.done, job.results
	job.mu.Unlock()
	if done { // read once it has ended: the job goes, so the table does not grow for the worker's life
		w.mu.Lock()
		delete(w.jobs, r.PathValue("id"))
		w.mu.Unlock()
	}
	rw.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(rw).Encode(map[string]any{"done": done, "results": results})
}

// smbHold holds a delete-on-close handle open on this node's gateway (smbclient.Hold)
// until /smbrelease names it.
func (w *worker) smbHold(rw http.ResponseWriter, r *http.Request) {
	var req struct {
		Path string `json:"path"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(rw, r.Body, 1<<16)).Decode(&req); err != nil {
		http.Error(rw, err.Error(), http.StatusBadRequest)
		return
	}
	release, err := smbclient.Hold(context.Background(), smbDial, req.Path)
	if err != nil {
		http.Error(rw, err.Error(), http.StatusConflict)
		return
	}
	w.mu.Lock()
	if w.held == nil {
		w.held = map[string]func() error{}
	}
	w.nextID++
	id := fmt.Sprintf("h%d", w.nextID)
	w.held[id] = release
	w.mu.Unlock()
	rw.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(rw).Encode(map[string]string{"id": id})
}

func (w *worker) smbRelease(rw http.ResponseWriter, r *http.Request) {
	var req struct {
		ID string `json:"id"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(rw, r.Body, 1<<16)).Decode(&req); err != nil {
		http.Error(rw, err.Error(), http.StatusBadRequest)
		return
	}
	w.mu.Lock()
	release := w.held[req.ID]
	delete(w.held, req.ID)
	w.mu.Unlock()
	if release == nil {
		http.Error(rw, "no such handle", http.StatusNotFound)
		return
	}
	if err := release(); err != nil {
		http.Error(rw, err.Error(), http.StatusInternalServerError)
		return
	}
	rw.Header().Set("Content-Type", "application/json")
	_, _ = rw.Write([]byte("{}"))
}

// --- the NAT rig's port-mapping responder (phase 27, docs/20 §7) -----------------------

// runResponder answers PCP and NAT-PMP on a router of the NAT rig, turning every mapping
// into a DNAT rule on the router's public side (the real effect of a mapping), while the
// on file exists; the reset file, when it appears, empties the table and the epoch (a
// router that restarted) and is removed.
func runResponder(listen, external, onFile, resetFile string) {
	logger := config.Default().Logger().With("src", "portmap-responder")
	ext, err := netip.ParseAddr(external)
	if err != nil {
		logger.Error("bad external address", "err", err)
		os.Exit(2)
	}
	r := &portmap.Responder{External: ext, Log: logger, Apply: func(m portmap.Mapping, add bool) error {
		op := "-D"
		if add {
			op = "-A"
		}
		return iptables("-t", "nat", op, "PREROUTING", "-d", ext.String(), "-p", "udp", "--dport", strconv.Itoa(int(m.External)), "-j", "DNAT", "--to-destination", net.JoinHostPort(m.Client.String(), strconv.Itoa(int(m.Internal))))
	}}
	if _, err := r.Listen(listen); err != nil {
		logger.Error("responder listen", "err", err)
		os.Exit(1)
	}
	logger.Info("portmap responder listening", "addr", listen, "external", external, "on", onFile, "reset", resetFile)
	for {
		_, err := os.Stat(onFile) //nolint:gosec // the rig's own flag files, named on the command line
		r.SetOff(err != nil)
		if _, err := os.Stat(resetFile); err == nil { //nolint:gosec // as above
			r.Reset()
			_ = os.Remove(resetFile) //nolint:gosec // as above
			logger.Info("portmap responder reset: every mapping gone, the epoch over")
		}
		time.Sleep(time.Second) //nolint:forbidigo // a sidecar's poll
	}
}
