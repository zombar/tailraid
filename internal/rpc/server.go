package rpc

import (
	"bytes"
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/netip"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/zombar/tailraid/internal/chunk"
	"github.com/zombar/tailraid/internal/clock"
	"github.com/zombar/tailraid/internal/meta"
)

// ShardStore is what the shard endpoints need from chunk.Store.
// acquire takes a slot of sem unless ctx ends first (a caller that gave up must not hold
// a queue position).
func acquire(ctx context.Context, sem chan struct{}) bool {
	select {
	case sem <- struct{}{}:
		return true
	case <-ctx.Done():
		return false
	}
}

type ShardStore interface {
	Get(k chunk.ShardKey) ([]byte, error) // the verified payload (the self path of Client.Get)
	Open(k chunk.ShardKey) (io.ReadCloser, int64, error)
	PutVerified(k chunk.ShardKey, payload []byte, sum [32]byte) (int64, error) // sum = sha256(payload), verified by the caller
	Has(k chunk.ShardKey) bool
	DeleteIfOlder(k chunk.ShardKey, minAge time.Duration, now time.Time) (bool, int64, error)
	Used() int64
	Capacity() int64
}

// Applier is the Raft side the server forwards to (cluster.Node).
type Applier interface {
	Propose(ctx context.Context, cmd meta.Command) (meta.Result, uint64, error)
	IsLeader() bool
	LeaderRPCAddr() string // host:port of the leader's RPC, or "" when unknown
	AppliedIndex() uint64
	Ready() bool // a leader is known and every committed entry has reached the FSM (I-125)
}

// Fence answers the DELETE fence question (docs/06 §2, docs/14 §4) for one of this
// host's disks: whether the local FSM places the shard on that target. A server without
// one refuses every DELETE.
type Fence interface {
	PlacedOn(target meta.NodeIdx, key chunk.ShardKey) bool
}

// HeaderDisk marks a 404 for an added disk that is not open on this host, so the caller
// reads it as an unavailable target, never as an absent shard (I-90).
const HeaderDisk = "X-Tailraid-Disk"

// Joiner performs the leader-side join and member operations (cluster package).
type Joiner interface {
	Join(ctx context.Context, req JoinRequest) (JoinResponse, error)
	MemberOp(ctx context.Context, op MemberOp) error
}

// Admin runs leader-side maintenance (node package): the reply is JSON.
type Admin interface {
	AdminOp(ctx context.Context, op AdminOp) (any, error)
}

// Options wire a Server.
type Options struct {
	NodeID    string
	ClusterID [16]byte
	Version   string
	// Disk looks up a disk open on this host by its ID, with its target index; DiskHealth
	// lists every configured disk for /health. Both nil on a diskless host (docs/14 §9).
	Disk       func(id string) (ShardStore, meta.NodeIdx, bool)
	DiskHealth func() []DiskHealth
	Applier    Applier
	Fence      Fence
	Joiner     Joiner
	Admin      Admin
	Auth       Authorizer
	Members    func() []meta.Node
	JoinToken  func() string
	Status     func() any // GET /v1/status body (admin report)
	BadSlot    func(key chunk.ShardKey, node meta.NodeIdx)
	Dev        http.Handler           // /dev/* (docs/08): nil unless dev.enabled
	Blocked    func(addr string) bool // /dev/fault rules: inbound requests from these peers get 503
	Clk        clock.Clock
	Logger     *slog.Logger
	MaxShard   int64         // PUT body cap; default 2 MiB
	Grace      time.Duration // DELETE fence minimum shard age; default 10 min
	PutSlots   int           // concurrent PUTs; default 16
	GetSlots   int           // concurrent GETs; default 32
	// BodyTimeout bounds reading a PUT or apply body and writing a GET body, so a peer
	// that stalls mid-transfer cannot hold a slot (I-97); default callTimeout(MaxShard).
	BodyTimeout time.Duration
}

// SlotBudget is the memory the shard RPC server may hold in request and reply bodies at
// once, per direction (docs/04 §9).
const SlotBudget = 256 << 20

// Slots is how many shard PUTs (and, separately, GETs) a server with the given shard
// size may serve at once so that slots × maxShard ≤ SlotBudget, within [4, 32] (I-83).
func Slots(maxShard int64) int {
	if maxShard <= 0 {
		return 32
	}
	return int(min(max(SlotBudget/maxShard, 4), 32))
}

// Server serves the node RPC.
type Server struct {
	joinMu   sync.Mutex
	joinLast map[string]time.Time // source IP → last failed join attempt
	o        Options
	clusID   string
	putSem   chan struct{}
	getSem   chan struct{}
	// background requests (HeaderPriority) take one of these before an ordinary slot, so
	// client-path requests always find the other half free (docs/06 §4, I-175)
	bgPutSem chan struct{}
	bgGetSem chan struct{}
	handler  http.Handler
}

// backgroundSlots is the share of slots background requests may hold: half, at least one.
func backgroundSlots(slots int) int { return max(1, slots/2) }

// background reports whether r carries the background priority.
func background(r *http.Request) bool { return r.Header.Get(HeaderPriority) == PriorityBackground }

// NewServer validates options and builds the handler.
func NewServer(o Options) (*Server, error) {
	if o.Applier == nil || o.Auth == nil || o.Members == nil {
		return nil, errors.New("rpc: Applier, Auth and Members are required")
	}
	if o.MaxShard == 0 {
		o.MaxShard = 2 << 20
	}
	if o.BodyTimeout == 0 {
		o.BodyTimeout = callTimeout(o.MaxShard)
	}
	if o.Grace == 0 {
		o.Grace = 10 * time.Minute
	}
	if o.PutSlots == 0 {
		o.PutSlots = 16
	}
	if o.GetSlots == 0 {
		o.GetSlots = 32
	}
	if o.Clk == nil {
		o.Clk = clock.Real()
	}
	if o.Logger == nil {
		o.Logger = slog.Default()
	}
	s := &Server{
		o: o, clusID: meta.HexID(o.ClusterID), putSem: make(chan struct{}, o.PutSlots), getSem: make(chan struct{}, o.GetSlots),
		bgPutSem: make(chan struct{}, backgroundSlots(o.PutSlots)), bgGetSem: make(chan struct{}, backgroundSlots(o.GetSlots)),
	}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /health", s.health)
	mux.Handle("/v1/disks/", s.clusterCheck(s.member(http.HandlerFunc(s.diskShards))))
	mux.Handle("POST /v1/apply", s.clusterCheck(s.member(http.HandlerFunc(s.apply))))
	mux.Handle("POST /v1/members", s.clusterCheck(http.HandlerFunc(s.members))) // membership, or the update_addr exception (docs/08 step 6, I-93)
	mux.Handle("POST /v1/bad-slot", s.clusterCheck(s.member(http.HandlerFunc(s.badSlot))))
	mux.Handle("POST /v1/admin", s.clusterCheck(s.member(http.HandlerFunc(s.admin))))
	mux.Handle("GET /v1/status", s.clusterCheck(s.member(http.HandlerFunc(s.status))))
	mux.Handle("POST /v1/join", http.HandlerFunc(s.join)) // token + claimed identity, not membership
	if o.Dev != nil {
		mux.Handle("/dev/", s.clusterCheck(s.member(o.Dev)))
	}
	var h http.Handler = mux
	if o.Blocked != nil { // a /dev/fault rule cuts inbound traffic too
		inner := h
		h = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if o.Blocked(r.RemoteAddr) {
				writeErr(w, http.StatusServiceUnavailable, "dropped by a fault rule")
				return
			}
			inner.ServeHTTP(w, r)
		})
	}
	s.handler = h
	return s, nil
}

// Handler returns the HTTP handler (the test harness serves it on its own listener).
func (s *Server) Handler() http.Handler { return s.handler }

// Listen binds the RPC listener. A non-loopback bind requires a real authorizer.
func Listen(ctx context.Context, addr string, auth Authorizer) (net.Listener, error) {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		return nil, fmt.Errorf("rpc: listen address: %w", err)
	}
	ip := net.ParseIP(host)
	if _, loopbackOnly := auth.(AllowAll); loopbackOnly && (ip == nil || !ip.IsLoopback()) {
		return nil, fmt.Errorf("rpc: refusing to bind %q without an authorizer", addr)
	}
	var lc net.ListenConfig
	return lc.Listen(ctx, "tcp", addr)
}

// Serve runs the HTTP server on l until ctx is done.
func (s *Server) Serve(ctx context.Context, l net.Listener) error {
	return ServeHTTP(ctx, l, s.handler)
}

// ServeHTTP serves h on l with the node's timeouts until ctx is done, then shuts down
// gracefully (5 s). The web UI listener (docs/13) shares it.
func ServeHTTP(ctx context.Context, l net.Listener, h http.Handler) error {
	srv := &http.Server{Handler: h, ReadHeaderTimeout: 5 * time.Second, IdleTimeout: 2 * time.Minute}
	errc := make(chan error, 1)
	go func() { errc <- srv.Serve(l) }()
	select {
	case <-ctx.Done():
		sctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = srv.Shutdown(sctx)
		<-errc
		return nil
	case err := <-errc:
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return err
	}
}

// --- middleware -------------------------------------------------------------------

func writeErr(w http.ResponseWriter, code int, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(ErrorBody{Error: msg})
}

// SourceAddr is the TCP peer address of a request (never a header), unmapped from IPv6.
func SourceAddr(r *http.Request) (netip.Addr, bool) {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return netip.Addr{}, false
	}
	ip, err := netip.ParseAddr(host)
	if err != nil {
		return netip.Addr{}, false
	}
	return ip.Unmap(), true
}

func (s *Server) clusterCheck(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get(HeaderCluster) != s.clusID {
			writeErr(w, http.StatusConflict, "cluster id mismatch")
			return
		}
		w.Header().Set(HeaderNodeID, s.o.NodeID)
		next.ServeHTTP(w, r)
	})
}

type ctxKey int

const memberKey ctxKey = 1

// member authorizes before any body is read (I-18).
func (s *Server) member(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		src, ok := SourceAddr(r)
		if !ok {
			writeErr(w, http.StatusForbidden, "unparseable source address")
			return
		}
		m, err := s.o.Auth.Allow(r.Context(), src)
		if err != nil {
			if errors.Is(err, ErrUnavailable) {
				writeErr(w, http.StatusServiceUnavailable, err.Error())
				return
			}
			writeErr(w, http.StatusForbidden, err.Error())
			return
		}
		next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), memberKey, m)))
	})
}

// --- handlers ---------------------------------------------------------------------

func (s *Server) health(w http.ResponseWriter, _ *http.Request) {
	h := Health{
		NodeID: s.o.NodeID, ClusterID: s.clusID, Version: s.o.Version, Applied: s.o.Applier.AppliedIndex(), Codec: meta.CodecVersion, Ready: s.o.Applier.Ready(),
		Leader: s.o.Applier.IsLeader(),
	}
	if s.o.DiskHealth != nil {
		h.Disks = s.o.DiskHealth()
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(h)
}

// diskShards serves /v1/disks/{id}/shards/{key}, the shard endpoint of a disk: the only
// shard route, since a device stores nothing (docs/14 §4, §9).
func (s *Server) diskShards(w http.ResponseWriter, r *http.Request) {
	rest := strings.TrimPrefix(r.URL.Path, "/v1/disks/")
	id, k, ok := strings.Cut(rest, "/shards/")
	if !ok || id == "" {
		writeErr(w, http.StatusNotFound, "no such endpoint")
		return
	}
	var store ShardStore
	var target meta.NodeIdx
	if s.o.Disk != nil {
		store, target, ok = s.o.Disk(id)
	}
	if !ok || store == nil {
		w.Header().Set(HeaderDisk, "closed")
		writeErr(w, http.StatusNotFound, "disk "+id+" is not open on this host")
		return
	}
	key := chunk.ShardKey(k)
	placed := func() bool { return s.o.Fence == nil || s.o.Fence.PlacedOn(target, key) } // no fence: refuse the delete
	s.serveShard(w, r, store, key, placed)
}

// serveShard is the shard endpoint over one store; placed is the DELETE fence's
// "placed on this target" question.
func (s *Server) serveShard(w http.ResponseWriter, r *http.Request, store ShardStore, key chunk.ShardKey, placed func() bool) {
	if _, _, _, _, err := chunk.ParseKey(key); err != nil {
		writeErr(w, http.StatusBadRequest, "bad shard key")
		return
	}
	switch r.Method {
	case http.MethodHead:
		if !store.Has(key) {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		w.WriteHeader(http.StatusOK)
	case http.MethodGet:
		if background(r) {
			if !acquire(r.Context(), s.bgGetSem) {
				writeErr(w, http.StatusServiceUnavailable, "cancelled while queued")
				return
			}
			defer func() { <-s.bgGetSem }()
		}
		if !acquire(r.Context(), s.getSem) {
			writeErr(w, http.StatusServiceUnavailable, "cancelled while queued")
			return
		}
		defer func() { <-s.getSem }()
		rc, size, err := store.Open(key)
		switch {
		case errors.Is(err, chunk.ErrNotFound):
			writeErr(w, http.StatusNotFound, "no such shard")
			return
		case errors.Is(err, chunk.ErrCorrupt):
			writeErr(w, http.StatusUnprocessableEntity, "shard corrupt")
			return
		case err != nil:
			writeErr(w, http.StatusInternalServerError, err.Error())
			return
		}
		defer rc.Close()
		_ = http.NewResponseController(w).SetWriteDeadline(time.Now().Add(s.o.BodyTimeout)) // I-97
		w.Header().Set("Content-Type", "application/octet-stream")
		w.Header().Set("Content-Length", strconv.FormatInt(size, 10))
		_, _ = io.Copy(w, rc)
	case http.MethodPut:
		if background(r) {
			if !acquire(r.Context(), s.bgPutSem) {
				writeErr(w, http.StatusServiceUnavailable, "cancelled while queued")
				return
			}
			defer func() { <-s.bgPutSem }()
		}
		if !acquire(r.Context(), s.putSem) {
			writeErr(w, http.StatusServiceUnavailable, "cancelled while queued")
			return
		}
		defer func() { <-s.putSem }()
		if r.ContentLength > s.o.MaxShard {
			writeErr(w, http.StatusRequestEntityTooLarge, "shard too large")
			return
		}
		_ = http.NewResponseController(w).SetReadDeadline(time.Now().Add(s.o.BodyTimeout))    // a stalled body frees the slot (I-97)
		buf := bytes.NewBuffer(make([]byte, 0, max(0, min(r.ContentLength, s.o.MaxShard))+1)) // sized by Content-Length: one allocation
		_, err := buf.ReadFrom(http.MaxBytesReader(w, r.Body, s.o.MaxShard))
		body := buf.Bytes()
		if err != nil {
			if _, ok := errors.AsType[*http.MaxBytesError](err); ok {
				writeErr(w, http.StatusRequestEntityTooLarge, "shard too large")
				return
			}
			writeErr(w, http.StatusBadRequest, "read body: "+err.Error())
			return
		}
		sum := sha256.Sum256(body) // once per hop: verified against the header, then handed to the store
		if want := r.Header.Get(HeaderShardSum); want != "" && !strings.EqualFold(want, hex.EncodeToString(sum[:])) {
			writeErr(w, http.StatusBadRequest, "shard hash mismatch")
			return
		}
		if _, err := store.PutVerified(key, body, sum); err != nil {
			if errors.Is(err, chunk.ErrFull) {
				writeErr(w, http.StatusInsufficientStorage, "no space")
				return
			}
			writeErr(w, http.StatusInternalServerError, err.Error())
			return
		}
		w.WriteHeader(http.StatusOK)
	case http.MethodDelete:
		idx, err := strconv.ParseUint(r.Header.Get(HeaderLogIndex), 10, 64)
		if err != nil {
			writeErr(w, http.StatusBadRequest, "missing or bad "+HeaderLogIndex)
			return
		}
		if s.o.Applier.AppliedIndex() < idx {
			writeErr(w, http.StatusPreconditionFailed, "fence: log index not yet applied here")
			return
		}
		if placed() {
			writeErr(w, http.StatusPreconditionFailed, "fence: shard is placed on this node")
			return
		}
		deleted, _, err := store.DeleteIfOlder(key, s.o.Grace, s.o.Clk.Now())
		if err != nil {
			writeErr(w, http.StatusInternalServerError, err.Error())
			return
		}
		if !deleted && store.Has(key) {
			writeErr(w, http.StatusPreconditionFailed, "fence: shard younger than grace")
			return
		}
		w.WriteHeader(http.StatusOK)
	default:
		writeErr(w, http.StatusMethodNotAllowed, "method not allowed")
	}
}

func (s *Server) apply(w http.ResponseWriter, r *http.Request) {
	_ = http.NewResponseController(w).SetReadDeadline(time.Now().Add(s.o.BodyTimeout)) // I-97
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 4<<20))
	if err != nil {
		writeErr(w, http.StatusBadRequest, "read body")
		return
	}
	cmd, err := meta.DecodeCommand(body)
	if err != nil {
		writeErr(w, http.StatusBadRequest, "bad command")
		return
	}
	if !s.o.Applier.IsLeader() {
		s.notLeader(w)
		return
	}
	res, index, err := s.o.Applier.Propose(r.Context(), cmd)
	if err != nil {
		if errors.Is(err, ErrNotLeader) || errors.Is(err, ErrNoLeader) {
			s.notLeader(w) // not applied: the caller retries elsewhere
			return
		}
		writeErr(w, http.StatusInternalServerError, err.Error()) // dispatched, outcome unknown (I-95)
		return
	}
	w.Header().Set(HeaderIndex, strconv.FormatUint(index, 10))
	w.Header().Set("Content-Type", "application/octet-stream")
	_, _ = w.Write(meta.EncodeResult(res))
}

// JoinInterval is how long a source IP waits after a failed join attempt.
const JoinInterval = time.Second

// joinAllowed reports whether src may attempt a join now: not within JoinInterval of its
// last failed attempt. Successful joins are never throttled (a scripted install joins
// several devices quickly from one host).
func (s *Server) joinAllowed(src string) bool {
	now := s.o.Clk.Now()
	s.joinMu.Lock()
	defer s.joinMu.Unlock()
	last, ok := s.joinLast[src]
	return !ok || now.Sub(last) >= JoinInterval
}

// joinFailed records a failed attempt from src.
func (s *Server) joinFailed(src string) {
	now := s.o.Clk.Now()
	s.joinMu.Lock()
	defer s.joinMu.Unlock()
	if s.joinLast == nil {
		s.joinLast = map[string]time.Time{}
	}
	s.joinLast[src] = now
	if len(s.joinLast) > 10_000 { // bounded: forget entries older than the interval
		for k, t := range s.joinLast {
			if now.Sub(t) >= JoinInterval {
				delete(s.joinLast, k)
			}
		}
	}
}

func (s *Server) notLeader(w http.ResponseWriter) {
	if addr := s.o.Applier.LeaderRPCAddr(); addr != "" {
		w.Header().Set(HeaderLeader, addr)
		writeErr(w, http.StatusMisdirectedRequest, "not the leader")
		return
	}
	writeErr(w, http.StatusServiceUnavailable, "no leader")
}

func (s *Server) join(w http.ResponseWriter, r *http.Request) {
	if r.Header.Get(HeaderCluster) != "" && r.Header.Get(HeaderCluster) != s.clusID {
		writeErr(w, http.StatusConflict, "cluster id mismatch")
		return
	}
	src, ok := SourceAddr(r)
	if !ok {
		writeErr(w, http.StatusForbidden, "unparseable source address")
		return
	}
	var req JoinRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 64<<10)).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, "bad join request")
		return
	}
	// after a failed attempt a source IP gets one attempt per second (docs/08 threat
	// table); the token compare is constant-time so a wrong token leaks nothing
	if !s.joinAllowed(src.String()) {
		writeErr(w, http.StatusTooManyRequests, "join: one attempt per second per source after a failure")
		return
	}
	if s.o.JoinToken == nil || req.Token == "" || subtle.ConstantTimeCompare([]byte(req.Token), []byte(s.o.JoinToken())) != 1 {
		s.joinFailed(src.String())
		writeErr(w, http.StatusForbidden, "bad join token")
		return
	}
	id, err := s.o.Auth.Identify(r.Context(), src)
	if err != nil {
		s.joinFailed(src.String())
		writeErr(w, http.StatusForbidden, "join: "+err.Error())
		return
	}
	if id != req.TSStableID {
		writeErr(w, http.StatusForbidden, "join: claimed tailnet identity does not match the caller")
		return
	}
	// the record's address is the caller's (I-94): a token holder cannot point a member
	// record at another device
	if req.Addr != "" && req.Addr != src.String() {
		writeErr(w, http.StatusForbidden, "join: addr must be the caller's own address or empty")
		return
	}
	req.Addr = src.String()
	if s.o.Joiner == nil {
		writeErr(w, http.StatusServiceUnavailable, "join not available")
		return
	}
	resp, err := s.o.Joiner.Join(r.Context(), req)
	if err != nil {
		if errors.Is(err, ErrNotLeader) || errors.Is(err, ErrNoLeader) {
			s.notLeader(w)
			return
		}
		if errors.Is(err, meta.ErrUnknownOutcome) {
			// the row's proposal was in flight when the leader lost its lease (a voter
			// just added and not yet caught up): the joiner retries under the same node
			// ID, which keeps its row and index whichever way the proposal went (I-94, I-95)
			writeErr(w, http.StatusServiceUnavailable, "join: "+err.Error())
			return
		}
		writeErr(w, http.StatusBadRequest, "join: "+err.Error())
		return
	}
	s.o.Auth.Purge()
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(resp)
}

// members authorizes like every other endpoint (a member, before the body is read) with
// one exception (docs/08 step 6, I-93): update_addr arrives from an address that is, by
// definition, not in the member table yet. A non-member's body is read (64 KiB at most)
// only to learn the op; update_addr is then allowed when WhoIs(src) is the StableID of the
// member named in it, and it changes nothing but that member's address.
func (s *Server) members(w http.ResponseWriter, r *http.Request) {
	src, ok := SourceAddr(r)
	if !ok {
		writeErr(w, http.StatusForbidden, "unparseable source address")
		return
	}
	_, authErr := s.o.Auth.Allow(r.Context(), src)
	if authErr != nil && errors.Is(authErr, ErrUnavailable) {
		writeErr(w, http.StatusServiceUnavailable, authErr.Error())
		return
	}
	var op MemberOp
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 64<<10)).Decode(&op); err != nil {
		writeErr(w, http.StatusBadRequest, "bad member op")
		return
	}
	if authErr != nil {
		if op.Op != "update_addr" {
			writeErr(w, http.StatusForbidden, authErr.Error())
			return
		}
		id, err := s.o.Auth.Identify(r.Context(), src)
		if err != nil {
			writeErr(w, http.StatusForbidden, "update_addr: "+err.Error())
			return
		}
		var named meta.Node
		if s.o.Members != nil {
			members := s.o.Members()
			for i := range members {
				if members[i].Idx == op.NodeIdx && members[i].State != meta.Removed {
					named = members[i]
				}
			}
		}
		if named.Idx == 0 || named.TSStableID != id {
			writeErr(w, http.StatusForbidden, "update_addr: the caller is not the tailnet node of the member it names")
			return
		}
		op = MemberOp{Op: "update_addr", NodeIdx: op.NodeIdx, Addr: op.Addr}
	}
	if s.o.Joiner == nil {
		writeErr(w, http.StatusServiceUnavailable, "members not available")
		return
	}
	if err := s.o.Joiner.MemberOp(r.Context(), op); err != nil {
		if errors.Is(err, ErrNotLeader) || errors.Is(err, ErrNoLeader) {
			s.notLeader(w)
			return
		}
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	s.o.Auth.Purge()
	w.WriteHeader(http.StatusOK)
}

func (s *Server) badSlot(w http.ResponseWriter, r *http.Request) {
	var b BadSlot
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4<<10)).Decode(&b); err != nil {
		writeErr(w, http.StatusBadRequest, "bad report")
		return
	}
	if _, _, _, _, err := chunk.ParseKey(chunk.ShardKey(b.Key)); err != nil {
		writeErr(w, http.StatusBadRequest, "bad shard key")
		return
	}
	if s.o.BadSlot != nil {
		s.o.BadSlot(chunk.ShardKey(b.Key), b.Node)
	}
	w.WriteHeader(http.StatusAccepted)
}

func (s *Server) status(w http.ResponseWriter, _ *http.Request) {
	var body any = map[string]any{"members": s.o.Members()}
	if s.o.Status != nil {
		body = s.o.Status()
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(body)
}

func (s *Server) admin(w http.ResponseWriter, r *http.Request) {
	var op AdminOp
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 64<<10)).Decode(&op); err != nil {
		writeErr(w, http.StatusBadRequest, "bad admin op")
		return
	}
	if s.o.Admin == nil {
		writeErr(w, http.StatusServiceUnavailable, "admin not available")
		return
	}
	reply, err := s.o.Admin.AdminOp(r.Context(), op)
	if err != nil {
		if errors.Is(err, ErrNotLeader) || errors.Is(err, ErrNoLeader) {
			s.notLeader(w)
			return
		}
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(reply)
}
