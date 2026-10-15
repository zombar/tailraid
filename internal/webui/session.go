package webui

import (
	"context"
	"crypto/sha256"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/zombar/tailraid/internal/admin"
	"github.com/zombar/tailraid/internal/meta"
	"github.com/zombar/tailraid/internal/users"
	"github.com/zombar/tailraid/internal/vfs"
)

// Sessions (docs/21 §6, I-354): once the cluster has users, every request but /login and
// /assets/* carries a session cookie whose token's hash is a row of the FSM, read on
// every request, and the session's user is the actor of the request (I-355). A token
// reaches a browser through a one-time code: `web login` mints one on a node, a
// hand-off between two nodes' pages another, each sealed under the cluster key, so any
// node opens any node's code and a code leaked from a history or a log yields no token.
// The cookie is SameSite=Lax, not Strict: a hand-off is a navigation one node's page
// starts to another's origin, and a Strict cookie is withheld for that navigation's
// whole redirect chain, so the page would land on the sign-in it just passed. Lax
// still keeps the cookie off every cross-site POST, and every mutating route is a POST
// or PUT that also needs the UI header (docs/13 §3).

// SessionCookie holds the session token on one origin.
const SessionCookie = "tailraid_session"

type sessionKey int

const ctxSession sessionKey = 1

// session is the signed-in user of a request.
type session struct {
	token [users.TokenSize]byte
	hash  [32]byte
	user  meta.User
	admin bool
}

// sessions reports whether a session is required: exactly while the cluster has users.
func (s *Server) sessions() bool { return s.o.Store.Enforcing() }

// withSession runs after authz: with users, a request outside /login and /assets/*
// needs a live session, whose user rides in the context; without users, every
// request passes as before.
func (s *Server) withSession(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !s.sessions() || r.URL.Path == "/login" || strings.HasPrefix(r.URL.Path, "/assets/") {
			next.ServeHTTP(w, r)
			return
		}
		se, ok := s.sessionOf(r)
		if !ok {
			s.noSession(w, r)
			return
		}
		w.Header().Set(userNote, se.user.Name)
		defer w.Header().Del(userNote)
		next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), ctxSession, se)))
	})
}

// sessionOf reads the cookie and checks its session against the replica: the row
// exists and has not expired, its user is active and still holds the key that minted
// it (a disabled user, a removed key or `web logout` is refused at the next request).
func (s *Server) sessionOf(r *http.Request) (session, bool) {
	c, err := r.Cookie(SessionCookie)
	if err != nil {
		return session{}, false
	}
	tok, ok := users.ParseToken(c.Value)
	if !ok {
		return session{}, false
	}
	hash := sha256.Sum256(tok[:])
	row, ok := s.o.Store.Session(hash)
	if !ok || row.ExpiresAt <= s.o.Clk.Now().UnixNano() {
		return session{}, false
	}
	u, ok := s.o.Store.User(row.User)
	if !ok || u.State != meta.UserActive {
		return session{}, false
	}
	hasKey := false
	for _, k := range u.Keys {
		if k.ID() == row.Key {
			hasKey = true
		}
	}
	if !hasKey {
		return session{}, false
	}
	return session{token: tok, hash: hash, user: u, admin: s.o.Store.IsAdmin(u.Idx)}, true
}

// noSession answers a request without one: an API or file request is 401, a page goes
// to the sign-in page with the way back.
func (s *Server) noSession(w http.ResponseWriter, r *http.Request) {
	if s.o.Audit != nil {
		s.o.Audit.Refused("", "web", r.URL.Path, "no session")
	}
	if strings.HasPrefix(r.URL.Path, "/api/") || strings.HasPrefix(r.URL.Path, "/files/") {
		writeErr(w, http.StatusUnauthorized, "sign in: run `tailraid web login` and open the link it prints")
		return
	}
	http.Redirect(w, r, "/login?to="+url.QueryEscape(r.URL.RequestURI()), http.StatusSeeOther)
}

// userOf is the user a handler answered for: fail has the writer alone, so the
// session middleware notes the user on it (never sent: the header is private to the
// response's building and dropped before the write).
func (s *Server) userOf(w http.ResponseWriter) string {
	u := w.Header().Get(userNote)
	w.Header().Del(userNote)
	return u
}

// userNote is the writer header the session middleware notes the user on.
const userNote = "X-Tailraid-Session-User"

// sessionIn is the request's session, if the cluster has users.
func sessionIn(r *http.Request) (session, bool) {
	se, ok := r.Context().Value(ctxSession).(session)
	return se, ok
}

// fs is the filesystem a request is served: the session's user's view, or the node's
// own before the cluster has users (I-355).
func (s *Server) fs(r *http.Request) *vfs.FS {
	if se, ok := sessionIn(r); ok {
		return s.o.FS.As(se.user.Idx)
	}
	return s.o.FS
}

// readable reports whether the request's user holds Read on ino (always, before users).
func (s *Server) readable(r *http.Request, ino meta.Ino) bool {
	se, ok := sessionIn(r)
	return !ok || s.o.Store.Access(se.user.Idx, ino) >= meta.RoleRead
}

// traversable is readable, or on the way to something the user holds a role on.
func (s *Server) traversable(r *http.Request, ino meta.Ino) bool {
	se, ok := sessionIn(r)
	return !ok || s.o.Store.Traverses(se.user.Idx, ino)
}

// isAdmin reports whether the request's user is an admin (always, before users).
func (s *Server) isAdmin(r *http.Request) bool {
	se, ok := sessionIn(r)
	return !ok || se.admin
}

// login is GET /login (docs/13 §3): with a good code, the cookie and the way on; else
// the sign-in page, saying so after a bad code. Without users there is nothing to sign
// in to: the root.
func (s *Server) login(w http.ResponseWriter, r *http.Request) {
	if !s.sessions() {
		http.Redirect(w, r, "/", http.StatusSeeOther)
		return
	}
	q := r.URL.Query()
	code := q.Get("code")
	if code == "" {
		s.signinPage(w, false)
		return
	}
	if s.o.CodeKey == nil {
		writeErr(w, http.StatusServiceUnavailable, "sessions are not wired on this node")
		return
	}
	key, ok := s.o.CodeKey()
	if !ok {
		writeErr(w, http.StatusServiceUnavailable, "this node holds no cluster key yet")
		return
	}
	now := s.o.Clk.Now()
	tok, nonce, exp, err := users.OpenCode(key, code, now)
	if err != nil || !s.codes.Use(nonce, exp, now) {
		s.signinPage(w, true)
		return
	}
	row, ok := s.o.Store.Session(sha256.Sum256(tok[:]))
	if !ok || row.ExpiresAt <= now.UnixNano() {
		s.signinPage(w, true)
		return
	}
	if s.o.Audit != nil {
		if u, ok := s.o.Store.User(row.User); ok {
			s.o.Audit.SignedIn(u.Name)
		}
	}
	http.SetCookie(w, &http.Cookie{ //nolint:gosec // no Secure: the page is plain HTTP inside the mesh (docs/13 §2), which is the transport boundary
		Name: SessionCookie, Value: users.TokenText(tok), Path: "/", HttpOnly: true, SameSite: http.SameSiteLaxMode,
		MaxAge: int(time.Duration(row.ExpiresAt-now.UnixNano()) / time.Second), // relative: the browser's clock need not be the node's
	})
	http.Redirect(w, r, safeTo(q.Get("to")), http.StatusSeeOther) //nolint:gosec // safeTo keeps it a path on this origin
}

// safeTo keeps a redirect on this origin: a path under /, else the root.
func safeTo(to string) string {
	if to == "" || !strings.HasPrefix(to, "/") || strings.HasPrefix(to, "//") || strings.ContainsAny(to, "\\\r\n") {
		return "/"
	}
	return to
}

// handoff is GET /api/handoff?to=<member> (docs/13 §3): the member's /login with a
// one-time code minted from this request's session, so that origin signs in to it.
func (s *Server) handoff(w http.ResponseWriter, r *http.Request) {
	se, ok := sessionIn(r)
	if !ok {
		writeErr(w, http.StatusNotFound, "no users: every member's page is open")
		return
	}
	name := r.URL.Query().Get("to")
	target, ok := s.memberPage(name)
	if !ok {
		writeErr(w, http.StatusNotFound, fmt.Sprintf("handoff: no member %q with a web listener", name))
		return
	}
	if s.o.CodeKey == nil {
		writeErr(w, http.StatusServiceUnavailable, "sessions are not wired on this node")
		return
	}
	key, ok := s.o.CodeKey()
	if !ok {
		writeErr(w, http.StatusServiceUnavailable, "this node holds no cluster key yet")
		return
	}
	code, err := users.MintCode(key, se.token, s.o.Clk.Now())
	if err != nil {
		s.fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"url": target + "/login?code=" + code})
}

// memberPage is the origin of a member's own page, from this node's status report.
func (s *Server) memberPage(name string) (string, bool) {
	rep, ok := s.report().(admin.Report)
	if !ok {
		if p, isPtr := s.report().(*admin.Report); isPtr && p != nil {
			rep, ok = *p, true
		}
	}
	if !ok {
		return "", false
	}
	for i := range rep.Nodes {
		n := &rep.Nodes[i]
		if n.Name != name || n.Web == 0 || n.Addr == "" {
			continue
		}
		host := n.Addr
		if h, _, err := net.SplitHostPort(n.Addr); err == nil {
			host = h
		}
		return "http://" + net.JoinHostPort(host, fmt.Sprint(n.Web)), true
	}
	return "", false
}

// logout is POST /api/logout (docs/13 §3): the session row goes, which ends it on every
// node, and the cookie with it.
func (s *Server) logout(w http.ResponseWriter, r *http.Request) {
	se, ok := sessionIn(r)
	if !ok {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	if s.o.Logout == nil {
		writeErr(w, http.StatusServiceUnavailable, "sessions are not wired on this node")
		return
	}
	if err := s.o.Logout(r.Context(), se.user.Idx, se.hash); err != nil {
		s.fail(w, err)
		return
	}
	http.SetCookie(w, &http.Cookie{Name: SessionCookie, Value: "", Path: "/", HttpOnly: true, SameSite: http.SameSiteLaxMode, MaxAge: -1}) //nolint:gosec // plain HTTP inside the mesh
	w.WriteHeader(http.StatusNoContent)
}

// sessionInfo is GET /api/session (docs/13 §3): who the page is signed in as; 404 before
// the cluster has users, when the page is open and shows no account.
func (s *Server) sessionInfo(w http.ResponseWriter, r *http.Request) {
	se, ok := sessionIn(r)
	if !ok {
		writeErr(w, http.StatusNotFound, "no users: the page is open to members")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"user": se.user.Name, "admin": se.admin})
}
