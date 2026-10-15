package soak

import (
	"context"
	"fmt"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"strings"
	"sync"

	"github.com/zombar/tailraid/internal/rpc"
	"github.com/zombar/tailraid/internal/smbgw"
	"github.com/zombar/tailraid/internal/users"
)

// --- 0 users first (docs/09 §6, I-362) -------------------------------------------------------
//
// A cluster with users is the one people run (a volume gets its first admin at init), so
// a run with Run.UsersFirst makes the admin before phase 1: every phase then runs with the
// FSM checking every command's actor. The driver's mounts and SMB account are bound to
// the admin on every node, the environment signs every admin call as the admin
// (UsersEnv.ActAs), and the driver's browser signs in to every page. Phase 28 reuses the
// admin. Without it, every phase but 28–31 ran on a cluster with no users, where nothing
// is checked, and the CLI's trash purge and snapshot restore were refused for the admin
// on a real cluster while the soak stayed green (2026-10-15).

// usersFirstPhase is the phase a UsersFirst run puts before phase 1.
var usersFirstPhase = phase{"0 users first", phaseUsersFirst}

func phaseUsersFirst(ctx context.Context, r *Run) error {
	su, ok := r.usersPhase(ctx)
	if !ok {
		return nil
	}
	if !su.bootstrapAdmin(ctx, r) {
		return nil
	}
	creds := []string{users.UIDCredential(su.env.MountUID()), users.SMBCredential(smbgw.User)}
	for _, nd := range su.nodes {
		for _, cred := range creds {
			if err := su.op(ctx, nd, su.kim, rpc.AdminOp{Op: "user_bind", Credential: cred, User: "kim"}, nil); err != nil {
				r.check("I-350", "the driver's "+cred+" on "+nd.Name+" bound to the admin", false, err.Error())
				return nil
			}
		}
	}
	r.check("I-350", "the driver's mounts and SMB account bound to the admin on every node", true, "")
	su.env.ActAs(su.kim)
	ws, err := newWebSessions(r, su)
	if err != nil {
		r.check("I-362", "the driver's browser", false, err.Error())
		return nil
	}
	r.web = ws
	for _, n := range r.gateways() {
		base, err := r.Env.WebURL(ctx, n)
		if err != nil || base == "" {
			r.checkSkipped("I-362", "the driver's browser signed in on "+n.Name, "no browser UI in this environment")
			continue
		}
		err = ws.signIn(ctx, n)
		r.check("I-354, I-362", "the driver's browser signed in on "+n.Name+" as the admin", err == nil, errText(err))
	}
	return nil
}

// webSessions is the driver's browser in a UsersFirst run: one cookie jar, signed in on
// every node's page as the acting admin through `web login`. A reply of 401 (the session
// ran out on the fake clock, or a page came back under a new address after a restart)
// signs in again on that page and sends the request once more.
type webSessions struct {
	r      *Run
	su     *soakUsers
	jar    *cookiejar.Jar
	base   http.RoundTripper
	client *http.Client

	mu    sync.Mutex
	pages map[string]NodeRef // a page's host:port → its node
}

func newWebSessions(r *Run, su *soakUsers) (*webSessions, error) {
	jar, err := cookiejar.New(nil)
	if err != nil {
		return nil, err
	}
	ws := &webSessions{r: r, su: su, jar: jar, base: http.DefaultTransport, pages: map[string]NodeRef{}}
	timeout := webHTTP.Timeout
	if c := r.Env.WebClient(); c != nil {
		if c.Transport != nil {
			ws.base = c.Transport
		}
		timeout = c.Timeout
	}
	ws.client = &http.Client{Jar: jar, Timeout: timeout, Transport: &sessionTransport{ws: ws}}
	return ws, nil
}

// signIn runs `web login` on n as the admin and opens the link with the jar.
func (ws *webSessions) signIn(ctx context.Context, n NodeRef) error {
	base, err := ws.r.Env.WebURL(ctx, n)
	if err != nil {
		return err
	}
	if u, perr := url.Parse(base); perr == nil {
		ws.mu.Lock()
		ws.pages[u.Host] = n
		ws.mu.Unlock()
	}
	var rep struct {
		URL string `json:"url"`
	}
	if err := ws.su.op(ctx, n, ws.su.kim, rpc.AdminOp{Op: "web_login"}, &rep); err != nil {
		return err
	}
	link := rep.URL
	if i := strings.Index(link, "/login?"); i >= 0 { // the node names its own address; the driver reaches the page at base
		link = strings.TrimSuffix(base, "/") + link[i:]
	} else {
		return fmt.Errorf("web login printed %q", rep.URL)
	}
	hc := &http.Client{Jar: ws.jar, Timeout: ws.client.Timeout, Transport: ws.base, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, link, http.NoBody)
	if err != nil {
		return err
	}
	resp, err := hc.Do(req)
	if err != nil {
		return err
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusSeeOther {
		return fmt.Errorf("the sign-in link answered %d", resp.StatusCode)
	}
	return nil
}

// pageAt is the node whose page is at host, looking the pages up again when it is new.
func (ws *webSessions) pageAt(ctx context.Context, host string) (NodeRef, bool) {
	ws.mu.Lock()
	n, ok := ws.pages[host]
	ws.mu.Unlock()
	if ok {
		return n, true
	}
	for _, g := range ws.r.gateways() {
		base, err := ws.r.Env.WebURL(ctx, g)
		if err != nil {
			continue
		}
		if u, err := url.Parse(base); err == nil && u.Host == host {
			ws.mu.Lock()
			ws.pages[host] = g
			ws.mu.Unlock()
			return g, true
		}
	}
	return NodeRef{}, false
}

// sessionTransport is the browser's transport: a 401 signs in on that page and the
// request goes once more, with the new cookie (the jar is the client's, so the retry
// sets it here).
type sessionTransport struct{ ws *webSessions }

func (t *sessionTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	resp, err := t.ws.base.RoundTrip(req)
	if err != nil || resp.StatusCode != http.StatusUnauthorized || (req.Body != nil && req.Body != http.NoBody && req.GetBody == nil) {
		return resp, err
	}
	n, ok := t.ws.pageAt(req.Context(), req.URL.Host)
	if !ok {
		return resp, nil
	}
	if err := t.ws.signIn(req.Context(), n); err != nil {
		return resp, nil //nolint:nilerr // the page's own 401 is the answer
	}
	_ = resp.Body.Close()
	again := req.Clone(req.Context())
	if req.GetBody != nil {
		body, err := req.GetBody()
		if err != nil {
			return nil, err
		}
		again.Body = body
	}
	again.Header.Del("Cookie")
	for _, c := range t.ws.jar.Cookies(req.URL) {
		again.AddCookie(c)
	}
	return t.ws.base.RoundTrip(again)
}

// webClientFor is the client a phase drives the pages with: the signed-in browser of a
// UsersFirst run, else the environment's, else the default.
func (r *Run) webClientFor() *http.Client {
	if r.web != nil {
		return r.web.client
	}
	if c := r.Env.WebClient(); c != nil {
		return c
	}
	return webHTTP
}
