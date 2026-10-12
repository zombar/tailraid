// Package webui serves the browser dashboard and file browser (docs/13): one HTTP handler
// over the vfs and the FSM on its own tailnet listener, authorized from the TCP source by
// the member-or-same-user WhoIs rule. Cluster administration stays in the CLI.
package webui

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net"
	"net/http"
	"net/netip"
	"os"
	"strings"
	"syscall"
	"time"

	"github.com/zombar/tailraid/internal/admin"
	"github.com/zombar/tailraid/internal/chunk"
	"github.com/zombar/tailraid/internal/clock"
	"github.com/zombar/tailraid/internal/logx"
	"github.com/zombar/tailraid/internal/meta"
	"github.com/zombar/tailraid/internal/pins"
	"github.com/zombar/tailraid/internal/rpc"
	"github.com/zombar/tailraid/internal/stats"
	"github.com/zombar/tailraid/internal/vfs"
)

// HeaderUI must be present on every mutating request (docs/13 §3): a page on another
// origin cannot make the browser send it, and there is no CORS to grant it.
const HeaderUI = "X-Tailraid-UI"

// Authorizer is what the UI needs from rpc.WhoIsAuthorizer (docs/13 §2).
type Authorizer interface {
	// Allow returns the member behind src under the RPC rule (docs/08), or an error.
	Allow(ctx context.Context, src netip.Addr) (meta.Node, error)
	// User returns the tailnet StableID and login behind src ("" login for tagged devices).
	User(ctx context.Context, src netip.Addr) (stableID, login string, err error)
}

// Options wire a Server.
type Options struct {
	FS     *vfs.FS
	Store  *meta.Store
	Auth   Authorizer
	Login  string       // this node's tailnet login; "" = cluster members only
	Status func() any   // the admin report (same closure as the RPC status endpoint)
	Stats  *stats.Query // the Stats page's history (docs/17 §3); nil = 503 on /api/stats
	Chunks func(ctx context.Context, path string, offset, limit int) (int, []admin.ChunkReport, error)
	Clk    clock.Clock
	Logger *slog.Logger
	// Uploads (docs/13 §4, I-82): concurrent uploads allowed and how long a body may send
	// nothing; zero values take DefaultUploadSlots and DefaultBodyTimeout.
	UploadSlots int
	BodyTimeout time.Duration
	// Retention is trash.retention, for each deleted entry's purge_at (0: none).
	Retention time.Duration
	// Pins is this node's pinned folders (docs/13 §4); nil answers 503 on the pin routes.
	Pins Pinner
	// Search is the node's index (docs/18 §6); nil answers 503 on /api/search.
	Search Searcher
	// Catalog is the node's media catalogue (docs/18 §8); nil lists the Media tab's items
	// by their names only.
	Catalog Cataloger
	// Hosts are the Host header values the UI answers (I-127): the listener's
	// <tsIP>:port and the node's MagicDNS name with the port. Any other Host is a 421,
	// so a page on another origin that resolves a name to this node's IP (DNS
	// rebinding) gets nothing. Empty allows every Host (tests).
	Hosts []string
}

// Hosts builds Options.Hosts for a listener address and a MagicDNS name ("" to skip).
func Hosts(addr, dnsName string) []string {
	out := []string{addr}
	if _, port, err := net.SplitHostPort(addr); err == nil && dnsName != "" {
		out = append(out, net.JoinHostPort(dnsName, port))
	}
	return out
}

// Server is the web UI handler.
type Server struct {
	o        Options
	handler  http.Handler
	cache    statusCache
	up       uploads
	refused  logx.Limiter  // one "refused" line a minute (I-124): a probe loop is not a log flood
	decoders chan struct{} // thumbnail decodes in flight (ThumbDecoders)
	covers   coverCache
}

// New validates the options and builds the handler.
func New(o Options) (*Server, error) {
	if o.FS == nil || o.Store == nil || o.Auth == nil {
		return nil, errors.New("webui: FS, Store and Auth are required")
	}
	if o.Clk == nil {
		o.Clk = clock.Real()
	}
	if o.Logger == nil {
		o.Logger = slog.Default()
	}
	if o.UploadSlots <= 0 {
		o.UploadSlots = DefaultUploadSlots
	}
	if o.BodyTimeout <= 0 {
		o.BodyTimeout = DefaultBodyTimeout
	}
	s := &Server{o: o, up: uploads{inflight: map[string]bool{}, slots: make(chan struct{}, o.UploadSlots)}, decoders: make(chan struct{}, ThumbDecoders)}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /{$}", s.index)
	mux.HandleFunc("GET /assets/{file}", s.asset)
	mux.HandleFunc("GET /api/status", s.status)
	mux.HandleFunc("GET /api/stats", s.stats)
	mux.HandleFunc("GET /api/ls", s.ls)
	mux.HandleFunc("GET /api/stat", s.stat)
	mux.HandleFunc("GET /api/trash", s.trash)
	mux.HandleFunc("GET /api/chunks", s.chunks)
	mux.HandleFunc("GET /api/thumb", s.thumbGet)
	mux.HandleFunc("PUT /api/thumb", csrf(s.thumbPut))
	mux.HandleFunc("GET /api/tags", s.tags)
	mux.HandleFunc("GET /api/pins", s.pinList)
	mux.HandleFunc("GET /api/search", s.search)
	mux.HandleFunc("GET /api/render", s.render)
	mux.HandleFunc("GET /api/media", s.media)
	mux.HandleFunc("GET /files/{path...}", s.get) // a GET pattern serves HEAD too
	mux.HandleFunc("PUT /files/{path...}", csrf(s.put))
	mux.HandleFunc("POST /api/mkdir", csrf(s.mkdir))
	mux.HandleFunc("POST /api/rename", csrf(s.rename))
	mux.HandleFunc("POST /api/move", csrf(s.move))
	mux.HandleFunc("POST /api/delete", csrf(s.del))
	mux.HandleFunc("POST /api/restore", csrf(s.restore))
	mux.HandleFunc("POST /api/purge", csrf(s.purge))
	mux.HandleFunc("POST /api/flags", csrf(s.flags))
	mux.HandleFunc("POST /api/pin", csrf(s.pin))
	s.refused.Clk = o.Clk
	s.handler = s.hostCheck(s.authz(mux))
	return s, nil
}

// hostCheck refuses a request whose Host is not one of ours with 421 before anything
// else runs (I-127).
func (s *Server) hostCheck(next http.Handler) http.Handler {
	if len(s.o.Hosts) == 0 {
		return next
	}
	allowed := map[string]bool{}
	for _, h := range s.o.Hosts {
		allowed[strings.ToLower(h)] = true
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !allowed[strings.ToLower(r.Host)] {
			writeErr(w, http.StatusMisdirectedRequest, "this node does not answer to "+r.Host)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// Handler returns the HTTP handler (the tests drive it through httptest).
func (s *Server) Handler() http.Handler { return s.handler }

// Serve runs the handler on l until ctx is done.
func (s *Server) Serve(ctx context.Context, l net.Listener) error {
	return rpc.ServeHTTP(ctx, l, s.handler)
}

// authz implements docs/13 §2 from the TCP source alone, before any body byte is read.
func (s *Server) authz(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		src, ok := rpc.SourceAddr(r)
		if !ok {
			writeErr(w, http.StatusForbidden, "no source address")
			return
		}
		_, err := s.o.Auth.Allow(r.Context(), src)
		if err == nil {
			next.ServeHTTP(w, r)
			return
		}
		if errors.Is(err, rpc.ErrUnavailable) {
			writeErr(w, http.StatusServiceUnavailable, err.Error())
			return
		}
		_, login, err := s.o.Auth.User(r.Context(), src)
		switch {
		case err == nil && login != "" && login == s.o.Login:
			next.ServeHTTP(w, r)
		case errors.Is(err, rpc.ErrUnavailable):
			writeErr(w, http.StatusServiceUnavailable, err.Error())
		default:
			s.refused.Warn(s.o.Logger, "web ui: refused", "src", src, "login", login, "err", err)
			writeErr(w, http.StatusForbidden, "not a cluster member and not this node's user")
		}
	})
}

// csrf refuses a mutating request without HeaderUI before its body is read (I-71).
func csrf(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get(HeaderUI) == "" {
			writeErr(w, http.StatusForbidden, "missing "+HeaderUI+" header")
			return
		}
		next(w, r)
	}
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

func writeErr(w http.ResponseWriter, code int, msg string) {
	writeJSON(w, code, map[string]string{"error": msg})
}

// fail maps an error to its status (docs/13 §3) and writes it.
func (s *Server) fail(w http.ResponseWriter, err error) {
	code := errStatus(err)
	switch {
	case code == http.StatusServiceUnavailable && errors.Is(err, syscall.EROFS):
		w.Header().Set("Retry-After", "5")
	case code == http.StatusServiceUnavailable && (errors.Is(err, meta.ErrCodec) || errors.Is(err, syscall.EOPNOTSUPP)):
		w.Header().Set("Retry-After", "30") // a member too old for the command (docs/12 §6)
	}
	if code == http.StatusInternalServerError {
		s.o.Logger.Error("web ui", "err", err)
	}
	writeErr(w, code, err.Error())
}

func errStatus(err error) int {
	switch {
	case errors.Is(err, os.ErrNotExist), errors.Is(err, meta.ErrNotExist):
		return http.StatusNotFound
	case errors.Is(err, os.ErrExist), errors.Is(err, meta.ErrExist), errors.Is(err, admin.ErrAmbiguous),
		errors.Is(err, meta.ErrInTrash), errors.Is(err, meta.ErrNotEmpty), errors.Is(err, syscall.ENOTEMPTY):
		return http.StatusConflict
	case errors.Is(err, meta.ErrProtected), errors.Is(err, syscall.EPERM), errors.Is(err, os.ErrPermission):
		return http.StatusForbidden // under a protected folder (I-154, I-163)
	case errors.Is(err, syscall.EISDIR), errors.Is(err, meta.ErrIsDir), errors.Is(err, syscall.ENOTDIR), errors.Is(err, meta.ErrNotDir),
		errors.Is(err, syscall.EINVAL), errors.Is(err, syscall.ENAMETOOLONG), errors.Is(err, meta.ErrNameTooLong), errors.Is(err, meta.ErrBadCommand),
		errors.Is(err, meta.ErrLoop), errors.Is(err, pins.ErrRoot):
		return http.StatusBadRequest
	case errors.Is(err, syscall.ENOSPC), errors.Is(err, chunk.ErrFull):
		return http.StatusInsufficientStorage
	case errors.Is(err, syscall.EROFS), errors.Is(err, syscall.EIO), errors.Is(err, meta.ErrCodec), errors.Is(err, syscall.EOPNOTSUPP):
		return http.StatusServiceUnavailable
	default:
		return http.StatusInternalServerError
	}
}
