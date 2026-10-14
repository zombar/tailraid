package node

import (
	"context"
	"errors"
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
	for _, l := range caseLabels(t, "node.go", "AdminOp") {
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
		t.Fatalf("a second attempt within the second: %v", err)
	}
	if _, err := ops.AdminOp(ctx, rpc.AdminOp{Op: "user_create", Source: other, User: "zed", Code: code, Key: key}); !errors.Is(err, meta.ErrInvite) {
		t.Fatalf("the right code under another name: %v", err)
	}
	c.Clk.Advance(2 * time.Second)
	if _, err := ops.AdminOp(ctx, rpc.AdminOp{Op: "user_create", Source: src, User: "pat", Code: code, Key: key}); err != nil {
		t.Fatalf("the right code after the second: %v", err)
	}
	if _, ok := c.Leader().Node.FSM.UserByName("pat"); !ok {
		t.Fatal("pat was not made")
	}
	c.Clk.Advance(2 * time.Second)
	if _, err := ops.AdminOp(ctx, rpc.AdminOp{Op: "user_create", Source: src, User: "pat", Code: code, Key: key}); !errors.Is(err, meta.ErrInvite) {
		t.Fatalf("a used code: %v", err)
	}
	if n := len(c.Leader().Node.FSM.Users()); n != 3 {
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
