package cluster_test

import (
	"errors"
	"testing"
	"time"

	"github.com/zombar/tailraid/internal/meta"
	"github.com/zombar/tailraid/internal/testharness"
)

// A cluster upgraded in place from a release before users (a rolling update): while a
// live member reads codec 11, the leader refuses a users op and an actor-carrying command
// with ErrCodec and a plain command still applies; once every member reads codec 14 the
// bootstrap invite lands, the same on every node, and enforcement still waits for the
// first user.
// Invariant: I-349
func TestCluster_Codec14RefusedWhileMemberOld(t *testing.T) {
	c := testharness.NewCluster(t, 3, 2, 1)
	defer c.Close()
	c.Settle()
	leader := c.Leader()
	now := c.Clk.Now().UnixNano()
	propose := func(cmd meta.Command) meta.Result {
		res, _, err := leader.Node.Propose(ctx, cmd)
		must(t, err)
		return res
	}
	invite := meta.Command{Op: meta.OpInvite, InviteOp: &meta.InviteOp{Bootstrap: true, Invite: meta.Invite{
		Hash: meta.HashCode("first-admin"), Name: "kim", Kind: meta.InviteUser, Admin: true, ExpiresAt: now + int64(24*time.Hour),
	}, Now: now}}
	leader.Node.MinCodec = func() uint8 { return meta.CodecSecrets } // one member still reads codec 11
	defer func() { leader.Node.MinCodec = nil }()
	if res := propose(invite); !errors.Is(res.Err, meta.ErrCodec) {
		t.Fatalf("a users op with an old member: %v, want ErrCodec", res.Err)
	}
	withActor := meta.Command{Op: meta.OpMkdir, Actor: 1, Mkdir: &meta.Mkdir{Parent: meta.RootIno, Name: "a", Mode: 0o755, Now: now}}
	if res := propose(withActor); !errors.Is(res.Err, meta.ErrCodec) {
		t.Fatalf("an actor-carrying mkdir with an old member: %v, want ErrCodec", res.Err)
	}
	if res := propose(meta.Command{Op: meta.OpMkdir, Mkdir: &meta.Mkdir{Parent: meta.RootIno, Name: "b", Mode: 0o755, Now: now}}); res.Err != nil {
		t.Fatalf("a plain mkdir with an old member: %v", res.Err)
	}
	leader.Node.MinCodec = func() uint8 { return meta.CodecVersion }
	if res := propose(invite); res.Err != nil {
		t.Fatalf("the bootstrap invite after the update: %v", res.Err)
	}
	c.Settle()
	for i := range c.Nodes {
		fsm := c.Nodes[i].Node.FSM
		if len(fsm.Invites()) != 1 || fsm.Enforcing() {
			t.Fatalf("n%d: %d invites, enforcing %v; want one invite and no enforcement before the first user", i, len(fsm.Invites()), fsm.Enforcing())
		}
	}
}
