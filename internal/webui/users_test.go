package webui_test

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/zombar/tailraid/internal/admin"
	"github.com/zombar/tailraid/internal/meta"
	"github.com/zombar/tailraid/internal/search"
	"github.com/zombar/tailraid/internal/testharness"
	"github.com/zombar/tailraid/internal/users"
	"github.com/zombar/tailraid/internal/webui"
)

// codeKey is the key the test cluster's sign-in codes are minted under.
var codeKey = []byte("the cluster key of the test volume")

// sessionsOn wires the session options: codes under codeKey, a logout that removes
// the row on the fabric.
func sessionsOn(f *testharness.Fabric) func(*webui.Options) {
	return func(o *webui.Options) {
		o.CodeKey = func() ([]byte, bool) { return codeKey, true }
		o.Logout = func(_ context.Context, actor meta.UserIdx, hash [32]byte) error {
			return f.Apply(meta.Command{Op: meta.OpSession, Actor: actor, SessionOp: &meta.SessionOp{Delete: true, Hash: hash, Now: f.Clk.Now().UnixNano()}}).Err
		}
	}
}

// usersUI is a UI over a fabric with kim (the admin) and lee: /r a folder lee reads,
// /w one they write, /other one they hold nothing on, each with a file.
func usersUI(t *testing.T, opts ...func(*webui.Options)) (u *ui, kim, lee meta.UserIdx) {
	t.Helper()
	var f *testharness.Fabric
	u = newUIWith(t, func(fab *testharness.Fabric) { f = fab }, append([]func(*webui.Options){func(o *webui.Options) { sessionsOn(f)(o) }}, opts...)...)
	for _, d := range []string{"/r", "/w", "/other"} {
		must(t, u.fs.MkdirAll(d, 0o755))
		u.write(d+"/f", 0, testharness.Pattern(64, 2))
	}
	kim, lee = f.Users()
	f.Grant(kim, "/r", lee, meta.RoleRead)
	f.Grant(kim, "/w", lee, meta.RoleWrite)
	return u, kim, lee
}

// session mints a session row for user on the fabric and returns its token.
func (u *ui) session(user meta.UserIdx, life time.Duration) [users.TokenSize]byte {
	u.t.Helper()
	tok, err := users.NewToken()
	must(u.t, err)
	usr, ok := u.f.FSM.User(user)
	if !ok {
		u.t.Fatalf("no user %d", user)
	}
	now := u.f.Clk.Now()
	r := u.f.Apply(meta.Command{Op: meta.OpSession, Actor: user, SessionOp: &meta.SessionOp{User: user, Hash: sha256.Sum256(tok[:]), Key: usr.Keys[0].ID(), ExpiresAt: now.Add(life).UnixNano(), Now: now.UnixNano()}})
	must(u.t, r.Err)
	return tok
}

// code mints a sign-in code for tok, as `web login` and a hand-off do.
func (u *ui) code(tok [users.TokenSize]byte) string {
	u.t.Helper()
	c, err := users.MintCode(codeKey, tok, u.f.Clk.Now())
	must(u.t, err)
	return c
}

// browser is a client with a cookie jar: one browser on one origin.
func (u *ui) browser() *http.Client {
	jar, err := cookiejar.New(nil)
	must(u.t, err)
	return &http.Client{Jar: jar, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
}

// signIn opens a fresh code for user in a new browser and returns it.
func (u *ui) signIn(user meta.UserIdx) *http.Client {
	u.t.Helper()
	b := u.browser()
	resp := u.doWith(b, http.MethodGet, "/login?code="+u.code(u.session(user, time.Hour))+"&to=/%23/browse/", nil)
	resp.Body.Close()
	if resp.StatusCode != http.StatusSeeOther || resp.Header.Get("Location") != "/#/browse/" {
		u.t.Fatalf("login: %d %q", resp.StatusCode, resp.Header.Get("Location"))
	}
	return b
}

func (u *ui) doWith(c *http.Client, method, path string, body io.Reader, hdr ...string) *http.Response {
	u.t.Helper()
	req, err := http.NewRequestWithContext(ctx, method, u.hs.URL+path, body)
	must(u.t, err)
	for i := 0; i+1 < len(hdr); i += 2 {
		req.Header.Set(hdr[i], hdr[i+1])
	}
	resp, err := c.Do(req)
	must(u.t, err)
	return resp
}

// getAs is a GET in browser b: the status and the body.
func (u *ui) getAs(b *http.Client, path string) (int, string) {
	u.t.Helper()
	resp := u.doWith(b, http.MethodGet, path, nil)
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(body)
}

// postAs is a POST with the UI header in browser b.
func (u *ui) postAs(b *http.Client, path, body string) (int, string) {
	u.t.Helper()
	resp := u.doWith(b, http.MethodPost, path, strings.NewReader(body), webui.HeaderUI, "1", "Content-Type", "application/json")
	defer resp.Body.Close()
	out, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(out)
}

// A sign-in code works once: the first visit sets the cookie and continues to `to`,
// the second shows the sign-in page with the line about a used code; a code older than
// five minutes, one under another key and one for a session that is gone are refused
// the same way; `to` stays on this origin; without a code the page shows the command.
// Invariant: I-354
func TestWebUI_Login_CodeOnce(t *testing.T) {
	u, kim, _ := usersUI(t)
	tok := u.session(kim, time.Hour)
	first := u.code(tok)
	b := u.browser()
	resp := u.doWith(b, http.MethodGet, "/login?code="+first+"&to=//evil.example/x", nil)
	resp.Body.Close()
	if resp.StatusCode != http.StatusSeeOther || resp.Header.Get("Location") != "/" {
		t.Fatalf("first visit: %d %q", resp.StatusCode, resp.Header.Get("Location"))
	}
	if code, body := u.getAs(b, "/api/session"); code != 200 || !strings.Contains(body, `"user":"kim"`) || !strings.Contains(body, `"admin":true`) {
		t.Fatalf("session after the visit: %d %s", code, body)
	}
	bad := map[string]string{"used": first}
	expired := u.code(u.session(kim, time.Hour))
	u.f.Clk.Advance(users.CodeLife + time.Second)
	bad["expired"] = expired
	other, err := users.MintCode([]byte("another key"), tok, u.f.Clk.Now())
	must(t, err)
	bad["other key"] = other
	gone := u.session(kim, time.Hour)
	bad["session gone"] = u.code(gone)
	must(t, u.f.Apply(meta.Command{Op: meta.OpSession, Actor: kim, SessionOp: &meta.SessionOp{Delete: true, Hash: sha256.Sum256(gone[:]), Now: u.f.Clk.Now().UnixNano()}}).Err)
	bad["junk"] = "not-a-code"
	for name, c := range bad {
		fresh := u.browser()
		code, body := u.getAs(fresh, "/login?code="+c)
		if code != 200 || !strings.Contains(body, "already used") || !strings.Contains(body, "tailraid web login") {
			t.Errorf("%s: %d, page says %q", name, code, firstLine(body))
		}
		if code, _ := u.getAs(fresh, "/api/session"); code != 401 {
			t.Errorf("%s: a session after a refused code: %d", name, code)
		}
	}
	code, body := u.getAs(u.browser(), "/login")
	if code != 200 || strings.Contains(body, "already used") || !strings.Contains(body, "tailraid web login") {
		t.Fatalf("the plain page: %d %q", code, firstLine(body))
	}
}

func firstLine(s string) string {
	if i := strings.Index(s, "<main"); i >= 0 {
		s = s[i:]
	}
	if len(s) > 160 {
		s = s[:160]
	}
	return s
}

// The session's life: a request without a cookie is sent to /login (a page) or 401 (the
// API, a file); with a session the page answers; past users.session, after the user is
// disabled, after the minting key is removed, after `web logout` and after `--all` the
// next request is refused on every route; before the first user, no cookie is needed.
// Invariant: I-354
func TestWebUI_Session_Lifecycle_Table(t *testing.T) {
	var f *testharness.Fabric
	open := newUIWith(t, func(fab *testharness.Fabric) { f = fab }, func(o *webui.Options) { sessionsOn(f)(o) })
	if code, _, _ := open.get("/api/ls?path=/"); code != 200 {
		t.Fatalf("before the first user, without a cookie: %d", code)
	}
	if resp := open.do(http.MethodGet, "/login", nil); resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("/login before users: %d", resp.StatusCode)
	}
	u, kim, lee := usersUI(t)
	for path, want := range map[string]int{"/": 303, "/api/ls?path=/": 401, "/files/r/f": 401, "/api/status": 401, "/assets/app.css": 200, "/login": 200} {
		resp := u.do(http.MethodGet, path, nil)
		resp.Body.Close()
		if resp.StatusCode != want {
			t.Errorf("%s without a cookie: %d, want %d", path, resp.StatusCode, want)
		}
		if want == 303 && resp.Header.Get("Location") != "/login?to=%2F" {
			t.Errorf("%s: sent to %q", path, resp.Header.Get("Location"))
		}
	}
	type step struct {
		name string
		end  func(b *http.Client, tok [users.TokenSize]byte)
	}
	for _, st := range []step{
		{"expired", func(_ *http.Client, _ [users.TokenSize]byte) { u.f.Clk.Advance(time.Hour + time.Second) }},
		{"disabled", func(_ *http.Client, _ [users.TokenSize]byte) {
			must(t, u.f.Apply(meta.Command{Op: meta.OpUser, Actor: kim, UserOp: &meta.UserOp{Action: meta.UserDisable, Target: lee, Now: u.f.Clk.Now().UnixNano()}}).Err)
		}},
		{"key removed", func(_ *http.Client, _ [users.TokenSize]byte) {
			// lee's second device gets a key, then the first device's key (the minting one) goes
			lu, _ := u.f.FSM.User(lee)
			now := u.f.Clk.Now().UnixNano()
			must(t, u.f.Apply(meta.Command{Op: meta.OpInvite, Actor: kim, InviteOp: &meta.InviteOp{Invite: meta.Invite{Hash: meta.HashCode("cccccccccc"), Kind: meta.InviteKey, ForUser: lee, ExpiresAt: now + int64(time.Hour)}, Now: now}}).Err)
			must(t, u.f.Apply(meta.Command{Op: meta.OpUser, UserOp: &meta.UserOp{Action: meta.UserKeyAdd, Target: lee, Code: "cccccccccc", Key: [32]byte{9}, Now: now}}).Err)
			must(t, u.f.Apply(meta.Command{Op: meta.OpUser, Actor: kim, UserOp: &meta.UserOp{Action: meta.UserKeyRemove, Target: lee, KeyID: lu.Keys[0].ID(), Now: now}}).Err)
		}},
		{"logout", func(b *http.Client, _ [users.TokenSize]byte) {
			if code, _ := u.postAs(b, "/api/logout", ""); code != 204 {
				t.Fatalf("logout: %d", code)
			}
		}},
		{"logout --all", func(_ *http.Client, _ [users.TokenSize]byte) {
			must(t, u.f.Apply(meta.Command{Op: meta.OpSession, Actor: lee, SessionOp: &meta.SessionOp{Delete: true, All: true, User: lee, Now: u.f.Clk.Now().UnixNano()}}).Err)
		}},
	} {
		t.Run(st.name, func(t *testing.T) {
			if st.name == "disabled" {
				t.Cleanup(func() { // the next steps need lee back
					must(t, u.f.Apply(meta.Command{Op: meta.OpUser, Actor: kim, UserOp: &meta.UserOp{Action: meta.UserEnable, Target: lee, Now: u.f.Clk.Now().UnixNano()}}).Err)
				})
			}
			tok := u.session(lee, time.Hour)
			b := u.browser()
			resp := u.doWith(b, http.MethodGet, "/login?code="+u.code(tok), nil)
			resp.Body.Close()
			if code, body := u.getAs(b, "/api/session"); code != 200 || !strings.Contains(body, `"user":"lee"`) {
				t.Fatalf("signed in: %d %s", code, body)
			}
			st.end(b, tok)
			if code, _ := u.getAs(b, "/api/ls?path=/r"); code != 401 {
				t.Fatalf("after %s: %d, want 401", st.name, code)
			}
			if resp := u.doWith(b, http.MethodGet, "/", nil); resp.StatusCode != http.StatusSeeOther {
				t.Fatalf("after %s, the page: %d", st.name, resp.StatusCode)
			}
		})
	}
}

// A session is the volume's, not a node's: a second node over the same FSM accepts a
// code and a cookie of the same session, and a hand-off from one node's page signs the
// other node's origin into it; a logout on one node ends it on both.
// Invariant: I-354
func TestWebUI_SessionOnEveryNode(t *testing.T) {
	// the first node's status names the second with its web port, so a hand-off knows the address
	var twoHost string
	var twoPort int
	u, kim, _ := usersUI(t, func(o *webui.Options) {
		o.Status = func() any {
			return admin.Report{Node: "one", Nodes: []admin.NodeReport{{Idx: 1, Name: "one", Addr: "127.0.0.1", Web: 1}, {Idx: 2, Name: "two", Addr: twoHost, Web: uint16(twoPort), Live: true}}}
		}
	})
	two := newUIOn(t, u.f, func(o *webui.Options) {
		sessionsOn(u.f)(o)
		o.Status = func() any { return admin.Report{Node: "two"} }
	})
	twoURL, err := url.Parse(two.hs.URL)
	must(t, err)
	twoHost = twoURL.Hostname()
	twoPort, err = strconv.Atoi(twoURL.Port())
	must(t, err)
	b := u.signIn(kim)
	// the same cookie is good on the second node's origin: one session, every node
	oneURL, err := url.Parse(u.hs.URL)
	must(t, err)
	var cookie *http.Cookie
	for _, c := range b.Jar.Cookies(oneURL) {
		if c.Name == webui.SessionCookie {
			cookie = c
		}
	}
	if cookie == nil {
		t.Fatal("no session cookie after the sign-in")
	}
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, two.hs.URL+"/api/session", http.NoBody)
	req.AddCookie(cookie)
	resp, err := http.DefaultClient.Do(req)
	must(t, err)
	resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("the session on the second node: %d", resp.StatusCode)
	}
	// the hand-off: a link to two's page goes through one's /api/handoff
	code, body := u.getAs(b, "/api/handoff?to=two")
	if code != 200 {
		t.Fatalf("handoff: %d %s", code, body)
	}
	var hand struct{ URL string }
	must(t, json.Unmarshal([]byte(body), &hand))
	if !strings.HasPrefix(hand.URL, two.hs.URL+"/login?code=") {
		t.Fatalf("handoff url %q", hand.URL)
	}
	b2 := two.browser()
	resp = two.doWith(b2, http.MethodGet, strings.TrimPrefix(hand.URL, two.hs.URL)+"&to=/%23/stats", nil)
	resp.Body.Close()
	if resp.StatusCode != http.StatusSeeOther || resp.Header.Get("Location") != "/#/stats" {
		t.Fatalf("the hand-off's login: %d %q", resp.StatusCode, resp.Header.Get("Location"))
	}
	if code, body := two.getAs(b2, "/api/session"); code != 200 || !strings.Contains(body, `"user":"kim"`) {
		t.Fatalf("after the hand-off: %d %s", code, body)
	}
	if code, _ := u.getAs(b, "/api/handoff?to=honker"); code != 404 {
		t.Fatalf("a hand-off to an unknown member: %d", code)
	}
	// a logout on two ends the one session everywhere
	if code, _ := two.postAs(b2, "/api/logout", ""); code != 204 {
		t.Fatalf("logout on two: %d", code)
	}
	if code, _ := u.getAs(b, "/api/session"); code != 401 {
		t.Fatalf("after the logout on two, one: %d", code)
	}
}

// A member's address alone admits nothing once users exist: the page goes to /login
// and the API is 401; a non-member with a valid cookie is still 403 (the member rule
// runs first); a user's session on a member device is admitted.
// Invariant: I-354, I-68
func TestWebUI_MemberAddressAlone_Refused(t *testing.T) {
	u, kim, _ := usersUI(t)
	if code, _, _ := u.get("/api/status"); code != 401 {
		t.Fatalf("a member without a session: %d", code)
	}
	b := u.signIn(kim)
	if code, _ := u.getAs(b, "/api/status"); code != 200 {
		t.Fatalf("a member with a session: %d", code)
	}
	u.member.Store(false)
	if code, _ := u.getAs(b, "/api/status"); code != 403 {
		t.Fatalf("a non-member with a session: %d", code)
	}
	if code, _ := u.getAs(b, "/login"); code != 403 {
		t.Fatalf("a non-member's /login: %d", code)
	}
}

// A revoked right shows at the next request, on the same session: lee's Write on /w
// gone, their mkdir there is 403; a disabled user is 401 at the next request (the
// session row stays until it expires).
// Invariant: I-354, I-355
func TestWebUI_RevokedAtNextRequest(t *testing.T) {
	u, kim, lee := usersUI(t)
	b := u.signIn(lee)
	if code, body := u.postAs(b, "/api/mkdir", `{"path":"/w/d"}`); code != 201 {
		t.Fatalf("mkdir with Write: %d %s", code, body)
	}
	u.f.Grant(kim, "/w", lee, meta.RoleNone)
	if code, _ := u.postAs(b, "/api/mkdir", `{"path":"/w/e"}`); code != 403 {
		t.Fatalf("mkdir after the revoke: %d, want 403", code)
	}
	if _, err := u.f.FSM.Resolve([]string{"w", "e"}); err == nil {
		t.Fatal("the refused mkdir landed")
	}
	must(t, u.f.Apply(meta.Command{Op: meta.OpUser, Actor: kim, UserOp: &meta.UserOp{Action: meta.UserDisable, Target: lee, Now: u.f.Clk.Now().UnixNano()}}).Err)
	if code, _ := u.getAs(b, "/api/ls?path=/r"); code != 401 {
		t.Fatalf("a disabled user's next request: %d", code)
	}
}

// Every route answers what the session's user holds Read on and no more: the root
// listing shows the way to lee's folders, a folder they hold nothing on and its file
// are 403 on ls, stat, files, thumb, tags, chunks and versions, search hits and the
// trash are filtered, a pin is an admin's, and a mutation carries the user (a PUT under
// /r is 403, under /w lands as lee).
// Invariant: I-355
func TestWebUI_RoutesFilterByRole(t *testing.T) {
	hits := &fakeSearcher{}
	u, kim, lee := usersUI(t, func(o *webui.Options) { o.Search = hits })
	must(t, u.fs.As(kim).MkdirAll("/other/deep", 0o755))
	must(t, u.fs.As(kim).Remove("/other/f")) // into the trash under /other
	rIn, _ := u.f.FSM.Resolve([]string{"r", "f"})
	oIn, _ := u.f.FSM.Resolve([]string{"other", "deep"})
	hits.hits = []search.Hit{{Path: "/r/f", Ino: rIn.Ino, Type: "file"}, {Path: "/other/deep", Ino: oIn.Ino, Type: "dir"}}
	b := u.signIn(lee)
	code, body := u.getAs(b, "/api/ls?path=/")
	if code != 200 || strings.Contains(body, `"name":"other"`) || !strings.Contains(body, `"name":"r"`) || !strings.Contains(body, `"name":"w"`) {
		t.Fatalf("lee's root: %d %s", code, body)
	}
	for _, path := range []string{"/api/ls?path=/other", "/api/stat?path=/other", "/api/stat?path=/other/deep", "/files/other/deep", "/api/thumb?path=/other/deep", "/api/tags?path=/other/deep", "/api/chunks?path=/other/deep", "/api/versions?path=/other/deep"} {
		if code, _ := u.getAs(b, path); code != 403 {
			t.Errorf("%s: %d, want 403", path, code)
		}
	}
	if code, body := u.getAs(b, "/api/search?q=f"); code != 200 || strings.Contains(body, "/other/deep") || !strings.Contains(body, "/r/f") {
		t.Errorf("search hits: %d %s", code, body)
	}
	if code, body := u.getAs(b, "/api/trash"); code != 200 || strings.Contains(body, `"/other/f"`) {
		t.Errorf("lee's trash: %d %s", code, body)
	}
	if code, _ := u.postAs(b, "/api/pin", `{"path":"/r","pinned":true}`); code != 403 {
		t.Errorf("lee's pin: %d, want 403", code)
	}
	put := func(c *http.Client, path string) int {
		resp := u.doWith(c, http.MethodPut, "/files"+path, strings.NewReader("hello"), webui.HeaderUI, "1", "Content-Length", "5")
		resp.Body.Close()
		return resp.StatusCode
	}
	if code := put(b, "/r/new"); code != 403 {
		t.Errorf("a PUT under Read: %d, want 403", code)
	}
	if code := put(b, "/w/new"); code != 201 && code != 200 {
		t.Errorf("a PUT under Write: %d", code)
	}
	if _, err := u.f.FSM.Resolve([]string{"w", "new"}); err != nil {
		t.Errorf("the PUT under Write did not land: %v", err)
	}
	a := u.signIn(kim)
	if code, body := u.getAs(a, "/api/trash"); code != 200 || !strings.Contains(body, `"/other/f"`) {
		t.Errorf("the admin's trash: %d %s", code, body)
	}
	if code, body := u.getAs(a, "/api/ls?path=/"); code != 200 || !strings.Contains(body, `"name":"other"`) {
		t.Errorf("the admin's root: %d %s", code, body)
	}
}

// PUT /api/thumb stores the frame as the node's own (System) only after Read on the
// source: lee's frame for a file under /other is 403 and nothing is written, for the
// file under /r it lands under /.tailraid/thumbs.
// Invariant: I-355
func TestWebUI_ThumbPut_SystemAfterRead(t *testing.T) {
	u, _, lee := usersUI(t)
	b := u.signIn(lee)
	jpeg := []byte{0xFF, 0xD8, 0xFF, 0xE0, 1, 2, 3}
	putThumb := func(path string) int {
		in, err := u.f.FSM.Resolve(meta.SplitPath(path))
		must(t, err)
		resp := u.doWith(b, http.MethodPut, fmt.Sprintf("/api/thumb?path=%s&version=%d", url.QueryEscape(path), in.Version), strings.NewReader(string(jpeg)), webui.HeaderUI, "1", "Content-Type", "image/jpeg")
		resp.Body.Close()
		return resp.StatusCode
	}
	if code := putThumb("/other/f"); code != 403 {
		t.Fatalf("a frame for a file lee may not read: %d", code)
	}
	thumbs, err := u.f.FSM.Resolve([]string{meta.ReservedDir, "thumbs"})
	if err == nil {
		if kids, _ := u.f.FSM.ReadDir(thumbs.Ino); len(kids) != 0 {
			t.Fatalf("a frame was written: %d entries", len(kids))
		}
	}
	if code := putThumb("/r/f"); code != 201 {
		t.Fatalf("a frame for a file lee reads: %d", code)
	}
	thumbs, err = u.f.FSM.Resolve([]string{meta.ReservedDir, "thumbs"})
	must(t, err)
	if kids, _ := u.f.FSM.ReadDir(thumbs.Ino); len(kids) != 1 {
		t.Fatalf("the frame did not land: %d entries", len(kids))
	}
}
