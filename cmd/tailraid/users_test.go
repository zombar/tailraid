package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/cookiejar"
	"strings"
	"testing"

	"github.com/zombar/tailraid/internal/node"
	"github.com/zombar/tailraid/internal/rpc"
	"github.com/zombar/tailraid/internal/users"
)

// inviteFrom picks the name and code out of an invite line.
func inviteFrom(t *testing.T, out string) (name, code string) {
	t.Helper()
	for line := range strings.SplitSeq(out, "\n") {
		if i := strings.Index(line, "tailraid user create "); i >= 0 {
			f := strings.Fields(line[i:])
			if len(f) >= 5 {
				return f[3], f[4]
			}
		}
	}
	t.Fatalf("no invite in:\n%s", out)
	return "", ""
}

// init prints the first admin's invite as the command to run; the name follows --admin.
// Invariant: I-343
func TestCLI_Init_PrintsAdminInvite(t *testing.T) {
	path, _ := devHome(t)
	out, err := run(t, "init", "--config", path, "--token", testToken, "--admin", "kim")
	if err != nil {
		t.Fatalf("init: %v\n%s", err, out)
	}
	name, code := inviteFrom(t, out)
	if name != "kim" || len(code) != 10 {
		t.Fatalf("invite %q %q", name, code)
	}
	if _, err := run(t, "init", "--config", devHomePath(t), "--token", testToken, "--admin", "Kim"); err == nil {
		t.Fatal("a bad admin name was accepted")
	}
}

func devHomePath(t *testing.T) string {
	t.Helper()
	p, _ := devHome(t)
	return p
}

// The whole flow from one device: the admin's identity from init's invite, who they are,
// a group, a grant, the access review, a personal secret set and read back, a key invite
// for a second device, an invite for another person, and an unsigned mutating request
// refused once users exist.
// Invariant: I-343, I-344, I-350, I-356
func TestCLI_UsersFlow(t *testing.T) {
	t.Setenv("TAILRAID_HOME", t.TempDir()) // the person's keychain fallback, never the daemon's
	path, cfg := devHome(t)
	out, err := run(t, "init", "--config", path, "--token", testToken, "--admin", "kim")
	if err != nil {
		t.Fatalf("init: %v\n%s", err, out)
	}
	_, code := inviteFrom(t, out)
	_, addr := serveInBackground(t, cfg)
	// before the first user the FSM still wants an admin for a group: refused
	if out, err := run(t, "group", "create", "family", "--config", path); err == nil || !strings.Contains(err.Error(), "permission denied") {
		t.Fatalf("a group before any identity: %v\n%s", err, out)
	}
	if out, err := run(t, "user", "create", "kim", code, "--config", path); err != nil || !strings.Contains(out, "you are kim") {
		t.Fatalf("user create: %v\n%s", err, out)
	}
	if out, err := run(t, "user", "create", "kim", code, "--config", path); err == nil {
		t.Fatalf("a second create on the device: %s", out)
	}
	if out, err := run(t, "user", "whoami", "--config", path); err != nil || !strings.Contains(out, "kim (admin)") {
		t.Fatalf("whoami: %v\n%s", err, out)
	}
	for _, args := range [][]string{
		{"group", "create", "family"},
		{"group", "add", "family", "kim"},
		{"grant", "/", "@family", "read"},
		{"grant", "/", "kim", "admin"},
	} {
		if out, err := run(t, append(args, "--config", path)...); err != nil {
			t.Fatalf("%v: %v\n%s", args, err, out)
		}
	}
	out, err = run(t, "access", "ls", "--config", path)
	if err != nil || !strings.Contains(out, "@family") || !strings.Contains(out, "admin") {
		t.Fatalf("access ls: %v\n%s", err, out)
	}
	out, err = run(t, "group", "ls", "--config", path)
	if err != nil || !strings.Contains(out, "family") || !strings.Contains(out, "admins") {
		t.Fatalf("group ls: %v\n%s", err, out)
	}
	if out, err := runStdin(t, "hunter2\n", "secret", "set", "token", "--config", path); err != nil || !strings.Contains(out, "user/kim/token") {
		t.Fatalf("secret set: %v\n%s", err, out)
	}
	if out, err := run(t, "secret", "get", "token", "--config", path); err != nil || strings.TrimSpace(out) != "hunter2" {
		t.Fatalf("secret get: %v\n%s", err, out)
	}
	if out, err := run(t, "secret", "ls", "--config", path); err != nil || !strings.Contains(out, "user/kim/token") || strings.Contains(out, "hunter2") {
		t.Fatalf("secret ls: %v\n%s", err, out)
	}
	if out, err := runStdin(t, "k\n", "secret", "set", "tmdb", "--system", "--config", path); err != nil || !strings.Contains(out, "stored tmdb") {
		t.Fatalf("system secret set: %v\n%s", err, out)
	}
	if out, err := run(t, "secret", "ls", "--system", "--config", path); err != nil || !strings.Contains(out, "tmdb") || strings.Contains(out, "user/kim") {
		t.Fatalf("system listing: %v\n%s", err, out)
	}
	if out, err := run(t, "user", "key", "invite", "--config", path); err != nil || !strings.Contains(out, "tailraid user key add ") {
		t.Fatalf("key invite: %v\n%s", err, out)
	}
	out, err = run(t, "user", "invite", "lee", "--config", path)
	if err != nil {
		t.Fatalf("user invite: %v\n%s", err, out)
	}
	if name, _ := inviteFrom(t, out); name != "lee" {
		t.Fatalf("invite for %q", name)
	}
	out, err = run(t, "user", "ls", "--config", path)
	if err != nil || !strings.Contains(out, "kim") || !strings.Contains(out, "admins") {
		t.Fatalf("user ls: %v\n%s", err, out)
	}
	// the mount's credential was bound to kim (I-350)
	raw, err := run(t, "user", "ls", "kim", "--json", "--config", path)
	if err != nil {
		t.Fatal(err)
	}
	var rows []node.UserRow
	if err := json.Unmarshal([]byte(raw), &rows); err != nil || len(rows) != 1 || len(rows[0].Keys) != 1 {
		t.Fatalf("user ls kim: %v %s", err, raw)
	}
	// a raw unsigned admin op is refused at the wire with 401
	probe := rpc.NewClient([16]byte{}, nil)
	h, err := probe.Health(context.Background(), addr)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := rpc.NewClient(mustCID(t, h.ClusterID), nil).Admin(context.Background(), addr, rpc.AdminOp{Op: "gc_now"}); !errors.Is(err, rpc.ErrUnauthorized) {
		t.Fatalf("an unsigned gc_now at the wire: %v", err)
	}
}

// One OS account holding two users: the second created from an invite beside the first,
// `user use` rebinding the mount before switching, `--identity` for one command, and a name not
// held or a refused bind changing nothing.
// Invariant: I-360
func TestCLI_UserUse(t *testing.T) {
	t.Setenv("TAILRAID_HOME", t.TempDir())
	path, cfg := devHome(t)
	out, err := run(t, "init", "--config", path, "--token", testToken, "--admin", "kim")
	if err != nil {
		t.Fatalf("init: %v\n%s", err, out)
	}
	_, code := inviteFrom(t, out)
	serveInBackground(t, cfg)
	if out, err := run(t, "user", "create", "kim", code, "--config", path); err != nil {
		t.Fatalf("user create kim: %v\n%s", err, out)
	}
	out, err = run(t, "user", "invite", "lee", "--config", path)
	if err != nil {
		t.Fatalf("user invite: %v\n%s", err, out)
	}
	_, code = inviteFrom(t, out)
	if out, err := run(t, "user", "create", "lee", code, "--config", path); err != nil || !strings.Contains(out, "you are lee") {
		t.Fatalf("a second user in the same OS account: %v\n%s", err, out)
	}
	bound := func() string {
		t.Helper()
		b, err := users.OpenBindings(cfg.Node.DataDir)
		if err != nil {
			t.Fatal(err)
		}
		u, _ := b.Lookup(mountCredential())
		return u
	}
	whoami := func(extra ...string) string {
		t.Helper()
		out, err := run(t, append([]string{"user", "whoami", "--config", path}, extra...)...)
		if err != nil {
			t.Fatalf("whoami %v: %v\n%s", extra, err, out)
		}
		return strings.TrimSpace(out)
	}
	if got := whoami(); got != "lee (user)" || bound() != "lee" {
		t.Fatalf("after the second create: %q, bound to %q", got, bound())
	}
	if got := whoami("--identity", "kim"); got != "kim (admin)" {
		t.Fatalf("--identity kim: %q", got)
	}
	if got := whoami(); got != "lee (user)" || bound() != "lee" {
		t.Fatalf("--identity moved the identity or binding: %q, %q", got, bound())
	}
	if out, err := runStdin(t, "v\n", "secret", "set", "token", "--identity", "kim", "--config", path); err != nil || !strings.Contains(out, "user/kim/token") {
		t.Fatalf("a personal secret --identity kim: %v\n%s", err, out)
	}
	if out, err := run(t, "user", "use", "--config", path); err != nil || out != "  kim\n* lee\n" {
		t.Fatalf("user use alone: %v\n%q", err, out)
	}
	if out, err := run(t, "user", "use", "kim", "--config", path); err != nil || !strings.Contains(out, "you are kim") {
		t.Fatalf("user use kim: %v\n%s", err, out)
	}
	if got := whoami(); got != "kim (admin)" || bound() != "kim" {
		t.Fatalf("after use kim: %q, bound to %q", got, bound())
	}
	if out, err := run(t, "user", "use", "ann", "--config", path); !errors.Is(err, users.ErrNoIdentity) {
		t.Fatalf("use of a name not held: %v\n%s", err, out)
	}
	if out, err := run(t, "user", "whoami", "--identity", "ann", "--config", path); !errors.Is(err, users.ErrNoIdentity) {
		t.Fatalf("--identity a name not held: %v\n%s", err, out)
	}
	// a disabled user's signature is refused: the bind fails, nothing switches
	if out, err := run(t, "user", "disable", "lee", "--config", path); err != nil {
		t.Fatalf("disable lee: %v\n%s", err, out)
	}
	if out, err := run(t, "user", "use", "lee", "--config", path); err == nil {
		t.Fatalf("use of a disabled user: %s", out)
	}
	if got := whoami(); got != "kim (admin)" || bound() != "kim" {
		t.Fatalf("a refused use changed something: %q, bound to %q", got, bound())
	}
}

func mustCID(t *testing.T, hexID string) [16]byte {
	t.Helper()
	var cid [16]byte
	if _, err := fmt.Sscanf(hexID, "%32x", &cid); err != nil {
		b := []byte(hexID)
		for i := 0; i < 16 && 2*i+1 < len(b); i++ {
			_, _ = fmt.Sscanf(string(b[2*i:2*i+2]), "%02x", &cid[i])
		}
	}
	return cid
}

// user bootstrap runs as the daemon's user on a voter: it writes a token the node reads
// once and answers an admin invite; a wrong token is refused.
// Invariant: I-343
func TestCLI_UserBootstrap(t *testing.T) {
	t.Setenv("TAILRAID_HOME", t.TempDir())
	path, cfg := devHome(t)
	if out, err := run(t, "init", "--config", path, "--token", testToken); err != nil {
		t.Fatalf("init: %v\n%s", err, out)
	}
	_, addr := serveInBackground(t, cfg)
	out, err := run(t, "user", "bootstrap", "--name", "root2", "--config", path)
	if err != nil {
		t.Fatalf("bootstrap: %v\n%s", err, out)
	}
	if name, code := inviteFrom(t, out); name != "root2" || len(code) != 10 {
		t.Fatalf("invite %q %q", name, code)
	}
	probe := rpc.NewClient([16]byte{}, nil)
	h, err := probe.Health(context.Background(), addr)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := rpc.NewClient(mustCID(t, h.ClusterID), nil).Admin(context.Background(), addr, rpc.AdminOp{Op: "user_bootstrap", Value: "nope", User: "x"}); !errors.Is(err, rpc.ErrForbidden) {
		t.Fatalf("a wrong token: %v", err)
	}
}

// `web login` prints a one-time link to this node's page that signs a browser in to
// the volume; opening it sets the cookie and the page answers who it is; `web logout`
// ends the sessions this device's key minted and the next request is refused.
// Invariant: I-354
func TestCLI_WebLogin(t *testing.T) {
	t.Setenv("TAILRAID_HOME", t.TempDir())
	path, cfg := devHome(t)
	out, err := run(t, "init", "--config", path, "--token", testToken, "--admin", "kim")
	if err != nil {
		t.Fatalf("init: %v\n%s", err, out)
	}
	_, code := inviteFrom(t, out)
	serveInBackground(t, cfg)
	if out, err := run(t, "user", "create", "kim", code, "--config", path); err != nil {
		t.Fatalf("user create: %v\n%s", err, out)
	}
	out, err = run(t, "web", "login", "--config", path)
	if err != nil {
		t.Fatalf("web login: %v\n%s", err, out)
	}
	var link string
	for f := range strings.FieldsSeq(out) {
		if strings.Contains(f, "/login?code=") {
			link = f
		}
	}
	if link == "" {
		t.Fatalf("no link in:\n%s", out)
	}
	jar, err := cookiejar.New(nil)
	if err != nil {
		t.Fatal(err)
	}
	client := &http.Client{Jar: jar, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	resp, err := client.Get(link) //nolint:noctx // a test client
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("the link: %d", resp.StatusCode)
	}
	base, _, _ := strings.Cut(link, "/login")
	who := func() (int, string) {
		r, err := client.Get(base + "/api/session") //nolint:noctx // a test client
		if err != nil {
			t.Fatal(err)
		}
		defer r.Body.Close()
		b, _ := io.ReadAll(r.Body)
		return r.StatusCode, string(b)
	}
	if st, body := who(); st != 200 || !strings.Contains(body, `"user":"kim"`) {
		t.Fatalf("signed in: %d %s", st, body)
	}
	if out, err := run(t, "web", "logout", "--config", path); err != nil || !strings.Contains(out, "1 browser session") {
		t.Fatalf("web logout: %v\n%s", err, out)
	}
	if st, _ := who(); st != 401 {
		t.Fatalf("after the logout: %d", st)
	}
}

// `status --json` carries users: the count of users and open sessions and this node's
// refusals of the last hour, once the cluster has users; `check` reports no dormant
// user on a fresh volume; `audit ls` answers on a node whose segments cannot land.
// Invariant: I-358
func TestStatus_Users(t *testing.T) {
	t.Setenv("TAILRAID_HOME", t.TempDir())
	path, cfg := devHome(t)
	out, err := run(t, "init", "--config", path, "--token", testToken, "--admin", "kim")
	if err != nil {
		t.Fatalf("init: %v\n%s", err, out)
	}
	_, code := inviteFrom(t, out)
	serveInBackground(t, cfg)
	if out, err := run(t, "status", "--json", "--config", path); err != nil || strings.Contains(out, `"users"`) {
		t.Fatalf("status before users carries a users report: %v\n%s", err, out)
	}
	if out, err := run(t, "user", "create", "kim", code, "--config", path); err != nil {
		t.Fatalf("user create: %v\n%s", err, out)
	}
	if out, err := run(t, "web", "login", "--config", path); err != nil {
		t.Fatalf("web login: %v\n%s", err, out)
	}
	out, err = run(t, "status", "--json", "--config", path)
	flat := strings.Join(strings.Fields(out), "")
	if err != nil || !strings.Contains(flat, `"users":{"count":1,"sessions":1,"refused_1h":0}`) {
		t.Fatalf("status users: %v\n%s", err, out)
	}
	// the test node is diskless: its audit segments cannot land in the volume (the
	// writer keeps them and warns), so `audit ls` answers from what it could read
	if out, err := run(t, "audit", "ls", "--config", path); err != nil || !strings.Contains(out, "no records") {
		t.Fatalf("audit ls on a diskless node: %v\n%s", err, out)
	}
	if out, err := run(t, "check", "--config", path); err != nil || strings.Contains(out, "dormant") {
		t.Fatalf("check: %v\n%s", err, out)
	}
}
