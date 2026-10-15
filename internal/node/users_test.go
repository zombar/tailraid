package node

import (
	"context"
	"errors"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"net/netip"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/zombar/tailraid/internal/meta"
	"github.com/zombar/tailraid/internal/rpc"
	"github.com/zombar/tailraid/internal/secrets"
	"github.com/zombar/tailraid/internal/testharness"
	"github.com/zombar/tailraid/internal/users"
)

// caseLabels collects the string constants of the case clauses inside the named
// functions of a file.
func caseLabels(t *testing.T, file string, funcs ...string) []string {
	t.Helper()
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, file, nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, d := range f.Decls {
		fn, ok := d.(*ast.FuncDecl)
		if !ok || !slices.Contains(funcs, fn.Name.Name) {
			continue
		}
		ast.Inspect(fn.Body, func(n ast.Node) bool {
			cc, ok := n.(*ast.CaseClause)
			if !ok {
				return true
			}
			for _, e := range cc.List {
				if lit, ok := e.(*ast.BasicLit); ok && lit.Kind == token.STRING {
					if s, err := strconv.Unquote(lit.Value); err == nil {
						out = append(out, s)
					}
				}
			}
			return true
		})
	}
	return out
}

// The needs table names every op the dispatcher handles and nothing else, so an op can
// be neither added unclassified nor classified without a handler (I-345).
// Invariant: I-345
func TestAdminOp_Needs_Exhaustive(t *testing.T) {
	handled := map[string]bool{}
	for _, l := range caseLabels(t, "node.go", "dispatch") {
		handled[l] = true
	}
	for _, l := range caseLabels(t, "users.go", "userOp") {
		handled[l] = true
	}
	for _, l := range caseLabels(t, "secrets.go", "secretOp") {
		handled[l] = true
	}
	handled["secret_rm"] = true // secretOp's default branch
	table := rpc.AdminOpNames()
	for _, op := range table {
		if !handled[op] {
			t.Errorf("the table classifies %q, which no dispatcher handles", op)
		}
	}
	for op := range handled {
		if !slices.Contains(table, op) {
			t.Errorf("the dispatcher handles %q, which the table does not classify", op)
		}
	}
}

// withUsers makes kim the first admin and lee a plain user on the harness, through the
// leader, and answers their indices.
func withUsers(t *testing.T, c *testharness.Cluster) (kim, lee meta.UserIdx) {
	t.Helper()
	ctx := context.Background()
	l := c.Leader().Node
	now := c.Clk.Now().UnixNano()
	propose := func(cmd meta.Command) meta.Result {
		t.Helper()
		res, index, err := l.Propose(ctx, cmd)
		if err == nil {
			err = res.Err
		}
		if err != nil {
			t.Fatalf("op %d: %v", cmd.Op, err)
		}
		if err := l.FSM.WaitApplied(ctx, index); err != nil {
			t.Fatal(err)
		}
		return res
	}
	propose(meta.Command{Op: meta.OpInvite, InviteOp: &meta.InviteOp{Bootstrap: true, Invite: meta.Invite{Hash: meta.HashCode("aaaaaaaaaa"), Name: "kim", Kind: meta.InviteUser, ExpiresAt: now + int64(time.Hour)}, Now: now}})
	kim = meta.UserIdx(propose(meta.Command{Op: meta.OpUser, UserOp: &meta.UserOp{Action: meta.UserCreate, Name: "kim", Code: "aaaaaaaaaa", Key: [32]byte{1}, Now: now}}).Ino)
	propose(meta.Command{Op: meta.OpInvite, Actor: kim, InviteOp: &meta.InviteOp{Invite: meta.Invite{Hash: meta.HashCode("bbbbbbbbbb"), Name: "lee", Kind: meta.InviteUser, ExpiresAt: now + int64(time.Hour)}, Now: now}})
	lee = meta.UserIdx(propose(meta.Command{Op: meta.OpUser, UserOp: &meta.UserOp{Action: meta.UserCreate, Name: "lee", Code: "bbbbbbbbbb", Key: [32]byte{2}, Now: now}}).Ino)
	c.Settle()
	return kim, lee
}

// A path-scoped op resolves its path before the check: a user without the role learns
// nothing about a missing path (ErrAccess), an admin gets the real answer; a grant by
// the subtree's admin lands and the review lists it.
// Invariant: I-345
func TestAdminOp_PathResolvesBeforeCheck(t *testing.T) {
	c := testharness.NewCluster(t, 3, 2, 1)
	defer c.Close()
	c.WriteFile(0, "/docs/a", testharness.Pattern(100, 1))
	c.Settle()
	kim, lee := withUsers(t, c)
	ops := opsFor(c, c.Leader())
	ctx := context.Background()
	if _, err := ops.AdminOp(ctx, rpc.AdminOp{Op: "grant", Actor: lee, Path: "/missing", User: "lee", Role: "read"}); !errors.Is(err, meta.ErrAccess) {
		t.Fatalf("a missing path for a plain user: %v", err)
	}
	if _, err := ops.AdminOp(ctx, rpc.AdminOp{Op: "grant", Actor: kim, Path: "/missing", User: "lee", Role: "read"}); !errors.Is(err, meta.ErrNotExist) {
		t.Fatalf("a missing path for an admin: %v", err)
	}
	if _, err := ops.AdminOp(ctx, rpc.AdminOp{Op: "grant", Actor: lee, Path: "/docs", User: "lee", Role: "read"}); !errors.Is(err, meta.ErrAccess) {
		t.Fatalf("a grant by a user without admin there: %v", err)
	}
	if _, err := ops.AdminOp(ctx, rpc.AdminOp{Op: "grant", Actor: kim, Path: "/docs", User: "lee", Role: "admin"}); err != nil {
		t.Fatalf("a grant by an admin: %v", err)
	}
	if _, err := ops.AdminOp(ctx, rpc.AdminOp{Op: "grant", Actor: lee, Path: "/docs", User: "@everyone", Role: "read"}); err != nil {
		t.Fatalf("a grant by the subtree's admin: %v", err)
	}
	if _, err := ops.AdminOp(ctx, rpc.AdminOp{Op: "grant", Actor: lee, Path: "/", User: "@everyone", Role: "read"}); !errors.Is(err, meta.ErrAccess) {
		t.Fatalf("a grant above the subtree: %v", err)
	}
	if _, err := ops.AdminOp(ctx, rpc.AdminOp{Op: "repair_now", Actor: lee}); !errors.Is(err, meta.ErrAccess) {
		t.Fatalf("an admins op by a plain user: %v", err)
	}
	if _, err := ops.AdminOp(ctx, rpc.AdminOp{Op: "repair_now"}); !errors.Is(err, users.ErrUnauthorized) {
		t.Fatalf("an admins op by nobody: %v", err)
	}
	raw, err := ops.AdminOp(ctx, rpc.AdminOp{Op: "access_ls", Path: "/docs"})
	if err != nil {
		t.Fatal(err)
	}
	rows := raw.([]GrantRow)
	if len(rows) != 2 || rows[0].Path != "/docs" || rows[0].Subject != "@everyone" || rows[1].Subject != "lee" || rows[1].Role != "admin" || rows[1].By != "kim" {
		t.Fatalf("access ls: %+v", rows)
	}
	if _, err := ops.AdminOp(ctx, rpc.AdminOp{Op: "revoke", Actor: lee, Path: "/docs", User: "@everyone"}); err != nil {
		t.Fatalf("a revoke by the subtree's admin: %v", err)
	}
	if raw, _ := ops.AdminOp(ctx, rpc.AdminOp{Op: "user_ls"}); len(raw.([]UserRow)) != 2 || raw.([]UserRow)[0].Groups[0] != "admins" {
		t.Fatalf("user ls: %+v", raw)
	}
}

// Once users exist, every trash op proposes as the user who signed it: an admin restores
// an entry, purges one and purges the whole trash; a plain user without Admin where an
// entry lived purges nothing. Before, the proposals carried no actor, so the FSM refused
// all three for the admin too ("purge: meta: permission denied", 2026-10-15).
// Invariant: I-349
func TestTrashOps_ProposeAsTheSigner(t *testing.T) {
	c := testharness.NewCluster(t, 3, 2, 1)
	defer c.Close()
	for _, p := range []string{"/docs/a", "/docs/b", "/docs/c", "/docs/d"} {
		c.WriteFile(0, p, testharness.Pattern(100, 1))
	}
	c.Settle()
	inos := map[string]meta.Ino{}
	for _, n := range []string{"a", "b", "c", "d"} {
		in, err := c.Leader().Node.FSM.Resolve([]string{"docs", n})
		must(t, err)
		inos[n] = in.Ino
		must(t, c.Nodes[0].FS.Remove("/docs/"+n))
	}
	c.Settle()
	kim, lee := withUsers(t, c)
	ops := opsFor(c, c.Leader())
	ctx := context.Background()
	if _, err := ops.AdminOp(ctx, rpc.AdminOp{Op: "trash_purge", Actor: lee, Ino: uint64(inos["b"])}); !errors.Is(err, meta.ErrAccess) {
		t.Fatalf("a purge by a plain user: %v", err)
	}
	if _, err := ops.AdminOp(ctx, rpc.AdminOp{Op: "trash_purge", Actor: lee, All: true}); !errors.Is(err, meta.ErrAccess) {
		t.Fatalf("a purge of the whole trash by a plain user: %v", err)
	}
	if _, err := ops.AdminOp(ctx, rpc.AdminOp{Op: "trash_restore", Actor: kim, Ino: uint64(inos["a"])}); err != nil {
		t.Fatalf("a restore by an admin: %v", err)
	}
	if _, err := c.Leader().Node.FSM.Resolve([]string{"docs", "a"}); err != nil {
		t.Fatalf("restored entry: %v", err)
	}
	if _, err := ops.AdminOp(ctx, rpc.AdminOp{Op: "trash_purge", Actor: kim, Ino: uint64(inos["b"])}); err != nil {
		t.Fatalf("a purge of one entry by an admin: %v", err)
	}
	raw, err := ops.AdminOp(ctx, rpc.AdminOp{Op: "trash_purge", Actor: kim, All: true})
	if err != nil {
		t.Fatalf("a purge of the whole trash by an admin: %v", err)
	}
	if got := raw.(map[string]int)["purged"]; got != 2 || c.Leader().Node.FSM.Stats().TrashInodes != 0 {
		t.Fatalf("purged %d, trash %+v, want c and d gone", got, c.Leader().Node.FSM.Stats())
	}
}

// A code is checked on the node's replica before anything is proposed, one attempt a
// second per source, and a wrong or used code answers ErrInvite with no row made.
// Invariant: I-343
func TestInvite_RateLimitedPerSource(t *testing.T) {
	c := testharness.NewCluster(t, 3, 2, 1)
	defer c.Close()
	kim, _ := withUsers(t, c)
	ops := opsFor(c, c.Leader())
	ctx := context.Background()
	raw, err := ops.AdminOp(ctx, rpc.AdminOp{Op: "user_invite", Actor: kim, User: "pat"})
	if err != nil {
		t.Fatal(err)
	}
	code := raw.(InviteReply).Code
	src, other := netip.MustParseAddr("100.70.3.9"), netip.MustParseAddr("100.70.3.10")
	key := strings.Repeat("ab", 32)
	if _, err := ops.AdminOp(ctx, rpc.AdminOp{Op: "user_create", Source: src, User: "pat", Code: "wrongwrong", Key: key}); !errors.Is(err, meta.ErrInvite) {
		t.Fatalf("a wrong code: %v", err)
	}
	if _, err := ops.AdminOp(ctx, rpc.AdminOp{Op: "user_create", Source: src, User: "pat", Code: code, Key: key}); !errors.Is(err, rpc.ErrRateLimited) {
		t.Fatalf("a second attempt within the second of a refusal: %v", err)
	}
	if _, err := ops.AdminOp(ctx, rpc.AdminOp{Op: "user_create", Source: other, User: "zed", Code: code, Key: key}); !errors.Is(err, meta.ErrInvite) {
		t.Fatalf("the right code under another name: %v", err)
	}
	c.Clk.Advance(2 * time.Second)
	if _, err := ops.AdminOp(ctx, rpc.AdminOp{Op: "user_create", Source: src, User: "pat", Code: code, Key: key}); err != nil {
		t.Fatalf("the right code after the second: %v", err)
	}
	// a success throttles nothing: a scripted setup creates its people in a row (soak
	// phase 28 created two within a second from one host and the second was refused)
	raw, err = ops.AdminOp(ctx, rpc.AdminOp{Op: "user_invite", Actor: kim, User: "quin"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ops.AdminOp(ctx, rpc.AdminOp{Op: "user_create", Source: src, User: "quin", Code: raw.(InviteReply).Code, Key: strings.Repeat("cd", 32)}); err != nil {
		t.Fatalf("a create right after a successful one from the same source: %v", err)
	}
	if _, ok := c.Leader().Node.FSM.UserByName("pat"); !ok {
		t.Fatal("pat was not made")
	}
	c.Clk.Advance(2 * time.Second)
	if _, err := ops.AdminOp(ctx, rpc.AdminOp{Op: "user_create", Source: src, User: "pat", Code: code, Key: key}); !errors.Is(err, meta.ErrInvite) {
		t.Fatalf("a used code: %v", err)
	}
	if n := len(c.Leader().Node.FSM.Users()); n != 4 { // kim, lee, pat and quin
		t.Fatalf("%d users", n)
	}
	// the leader's tick expires what is past its time
	if _, err := ops.AdminOp(ctx, rpc.AdminOp{Op: "user_invite", Actor: kim, User: "zed"}); err != nil {
		t.Fatal(err)
	}
	c.Clk.Advance(meta.InviteTTL + time.Second)
	ops.expireInvites(ctx)
	if n := len(c.Leader().Node.FSM.Invites()); n != 0 {
		t.Fatalf("%d invites after the expiry", n)
	}
}

// harnessSecrets is a secrets store over a harness node.
func harnessSecrets(c *testharness.Cluster, nd *testharness.ClusterNode) *secrets.Store {
	return &secrets.Store{
		State: nd.Node.FSM, Keys: &memKeychain{vals: map[string]string{}},
		Self: func() meta.NodeIdx { return nd.Idx() }, Now: func() int64 { return c.Clk.Now().UnixNano() },
		Propose: func(ctx context.Context, cmd meta.Command) error {
			res, index, err := nd.Node.Propose(ctx, cmd)
			if err == nil {
				err = res.Err
			}
			if err != nil {
				return err
			}
			return nd.Node.FSM.WaitApplied(ctx, index)
		},
	}
}

// A personal secret is its owner's to set and get; another user, an admin included, is
// refused the get; the owner's listing shows their own names only.
// Invariant: I-356
func TestSecrets_GetOwnerOnly(t *testing.T) {
	c := testharness.NewCluster(t, 3, 2, 1)
	defer c.Close()
	kim, lee := withUsers(t, c)
	ops := opsFor(c, c.Leader())
	ops.secrets = harnessSecrets(c, c.Leader())
	ctx := context.Background()
	if _, err := ops.AdminOp(ctx, rpc.AdminOp{Op: "secret_set", Actor: lee, Name: "user/lee/token", Value: "hunter2"}); err != nil {
		t.Fatal(err)
	}
	if _, err := ops.AdminOp(ctx, rpc.AdminOp{Op: "secret_set", Actor: kim, Name: "user/lee/other", Value: "x"}); !errors.Is(err, meta.ErrAccess) {
		t.Fatalf("an admin setting another's personal secret: %v", err)
	}
	raw, err := ops.AdminOp(ctx, rpc.AdminOp{Op: "secret_get", Actor: lee, Name: "user/lee/token"})
	if err != nil || raw.(map[string]string)["value"] != "hunter2" {
		t.Fatalf("the owner's get: %v %v", err, raw)
	}
	if _, err := ops.AdminOp(ctx, rpc.AdminOp{Op: "secret_get", Actor: kim, Name: "user/lee/token"}); !errors.Is(err, meta.ErrAccess) {
		t.Fatalf("an admin's get of another's personal secret: %v", err)
	}
	if _, err := ops.AdminOp(ctx, rpc.AdminOp{Op: "secret_set", Actor: kim, Name: "tmdb", Value: "k"}); err != nil {
		t.Fatal(err)
	}
	raw, err = ops.AdminOp(ctx, rpc.AdminOp{Op: "secret_ls", Actor: lee})
	if err != nil || len(raw.([]secrets.Entry)) != 1 || raw.([]secrets.Entry)[0].Name != "user/lee/token" || raw.([]secrets.Entry)[0].ByUser != lee {
		t.Fatalf("lee's listing: %v %+v", err, raw)
	}
	if _, err := ops.AdminOp(ctx, rpc.AdminOp{Op: "secret_rm", Actor: kim, Name: "user/lee/token"}); err != nil {
		t.Fatalf("an admin removing a personal secret: %v", err)
	}
}

// A system secret is set, listed and opened by admins only, and never by a plain user;
// the listing carries no value.
// Invariant: I-356
func TestSecrets_SystemNeverPrinted(t *testing.T) {
	c := testharness.NewCluster(t, 3, 2, 1)
	defer c.Close()
	kim, lee := withUsers(t, c)
	ops := opsFor(c, c.Leader())
	ops.secrets = harnessSecrets(c, c.Leader())
	ctx := context.Background()
	if _, err := ops.AdminOp(ctx, rpc.AdminOp{Op: "secret_set", Actor: lee, Name: "tmdb", Value: "k"}); !errors.Is(err, meta.ErrAccess) {
		t.Fatalf("a system set by a plain user: %v", err)
	}
	if _, err := ops.AdminOp(ctx, rpc.AdminOp{Op: "secret_set", Actor: kim, Name: "tmdb", Value: "k"}); err != nil {
		t.Fatal(err)
	}
	if _, err := ops.AdminOp(ctx, rpc.AdminOp{Op: "secret_get", Actor: lee, Name: "tmdb"}); !errors.Is(err, meta.ErrAccess) {
		t.Fatalf("a system get by a plain user: %v", err)
	}
	if _, err := ops.AdminOp(ctx, rpc.AdminOp{Op: "secret_ls", Actor: lee, All: true}); !errors.Is(err, meta.ErrAccess) {
		t.Fatalf("the system listing by a plain user: %v", err)
	}
	raw, err := ops.AdminOp(ctx, rpc.AdminOp{Op: "secret_ls", Actor: kim, All: true})
	if err != nil || len(raw.([]secrets.Entry)) != 1 || raw.([]secrets.Entry)[0].Name != "tmdb" {
		t.Fatalf("the system listing by an admin: %v %+v", err, raw)
	}
	if raw, _ := ops.AdminOp(ctx, rpc.AdminOp{Op: "secret_get", Actor: kim, Name: "tmdb"}); raw.(map[string]string)["value"] != "k" {
		t.Fatalf("the consumer's get by an admin: %v", raw)
	}
}

// Every admin op, once users exist, does for an admin what it does without users: the
// ops that change the volume run here as the first admin and nothing they propose is
// refused for want of an actor; the rest say why they never propose a user-checked
// command, or name the test that runs them under users. A new op must take a row. Two
// ops had proposed as nobody (trash restore and purge) and two had written as System
// (snapshot restore, versions --copy), so an admin's own command was refused once the
// first user existed (2026-10-15).
// Invariant: I-345, I-348, I-349
func TestAdminOps_AsAdmin_UnderUsers(t *testing.T) {
	notRun := map[string]string{
		"chunk": "reads only", "cache_ls": "this node's pins, no proposal", "search": "reads only",
		"snapshot_ls": "reads only", "scrape_plan": "reads only", "user_whoami": "reads only",
		"cache_pin": "this node's pins, no proposal", "cache_unpin": "this node's pins, no proposal",
		"open_disk": "this host's disks, no user-checked proposal", "open_mirror": "this host's mirrors, no user-checked proposal",
		"export": "writes a directory outside the volume", "snapshot": "writes a directory outside the volume",
		"snapshot_create":  "snapshot rows only, not user-checked",
		"index_now":        "the index under /.tailraid, written as System",
		"stats_seal":       "stats under /.tailraid, written as System: TestAdminOp_StatsSeal",
		"set_policy":       "volume rows only, not user-checked",
		"snapshot_restore": "needs a mirror: TestAdminOps_RestoreUnderUsers", "versions_revert": "needs a mirror: TestAdminOps_RestoreUnderUsers",
		"versions_copy": "needs a mirror: TestAdminOps_RestoreUnderUsers",
		"user_create":   "an invite's own check: TestInvite_RateLimitedPerSource", "user_bootstrap": "the bootstrap's own check: cmd/tailraid TestCLI_UserBootstrap",
		"user_key_add": "an invite's own check: cmd/tailraid TestCLI_UsersFlow", "user_key_invite": "the users applier: cmd/tailraid TestCLI_UsersFlow",
		"user_key_rm": "the users applier: cmd/tailraid TestCLI_UsersFlow", "user_bind": "this device's bindings file, no proposal",
		"user_unbind": "this device's bindings file, no proposal", "web_login": "a session row as the signer: cmd/tailraid TestCLI_WebLogin",
		"web_logout": "a session row as the signer: cmd/tailraid TestCLI_WebLogin", "audit_ls": "reads only: TestAudit_LeaderOnly",
	}
	c := testharness.NewCluster(t, 3, 2, 1)
	defer c.Close()
	for _, p := range []string{"/docs/a", "/docs/b", "/docs/c"} {
		c.WriteFile(0, p, testharness.Pattern(100, 1))
	}
	c.Settle()
	inos := map[string]uint64{}
	for _, n := range []string{"a", "b", "c"} {
		in, err := c.Leader().Node.FSM.Resolve([]string{"docs", n})
		must(t, err)
		inos[n] = uint64(in.Ino)
		must(t, c.Nodes[0].FS.Remove("/docs/"+n))
	}
	c.Settle()
	kim, _ := withUsers(t, c)
	l := c.Leader()
	ops := opsFor(c, l)
	ops.fs = l.FS
	ops.secrets = harnessSecrets(c, l)
	var refused []string
	l.Node.FSM.SetApplyHook(func(cmd meta.Command, _ uint64, res meta.Result) {
		if errors.Is(res.Err, meta.ErrAccess) {
			refused = append(refused, fmt.Sprintf("op %d actor %d system %v", cmd.Op, cmd.Actor, cmd.System))
		}
	})
	defer l.Node.FSM.SetApplyHook(nil)
	run := []rpc.AdminOp{
		{Op: "trash_ls"},
		{Op: "versions", Path: "/docs"},
		{Op: "fsck"},
		{Op: "check"},
		{Op: "access_ls", Path: "/"},
		{Op: "user_ls"},
		{Op: "group_ls"},
		{Op: "trash_restore", Ino: inos["a"]},
		{Op: "trash_purge", Ino: inos["b"]},
		{Op: "trash_purge", All: true},
		{Op: "grant", Path: "/docs", User: "lee", Role: "read"},
		{Op: "revoke", Path: "/docs", User: "lee"},
		{Op: "repair_now"},
		{Op: "gc_now"},
		{Op: "rebalance", DryRun: true},
		{Op: "rotate_token"},
		{Op: "secret_set", Name: "tmdb", Value: "k"},
		{Op: "secret_set", Name: "user/kim/t", Value: "v"},
		{Op: "secret_get", Name: "user/kim/t"},
		{Op: "secret_ls"},
		{Op: "secret_rm", Name: "user/kim/t"},
		{Op: "user_invite", User: "quin"},
		{Op: "group_create", Group: "crew"},
		{Op: "group_add", Group: "crew", User: "lee"},
		{Op: "group_remove", Group: "crew", User: "lee"},
		{Op: "group_rm", Group: "crew"},
		{Op: "user_disable", User: "lee"},
		{Op: "user_enable", User: "lee"},
		{Op: "user_rm", User: "lee"},
	}
	ran := map[string]bool{}
	ctx := context.Background()
	for _, op := range run {
		ran[op.Op] = true
		op.Actor = kim
		if _, err := ops.AdminOp(ctx, op); err != nil {
			t.Errorf("%s by an admin: %v", op.Op, err)
		}
		c.Settle()
	}
	for _, r := range refused {
		t.Errorf("the FSM refused an admin op's proposal: %s", r)
	}
	for _, name := range rpc.AdminOpNames() {
		if !ran[name] && notRun[name] == "" {
			t.Errorf("admin op %q is neither run here as an admin under users nor says why not", name)
		}
		if ran[name] && notRun[name] != "" {
			t.Errorf("admin op %q is run here and also listed as not run", name)
		}
	}
}
