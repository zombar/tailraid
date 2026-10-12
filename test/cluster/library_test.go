package cluster_test

import (
	"errors"
	"testing"

	"github.com/zombar/tailraid/internal/meta"
	"github.com/zombar/tailraid/internal/testharness"
)

// A SetAttr carrying the library bits is codec 6: the leader refuses it with ErrCodec
// while a live member reads an older codec, the other flags still set at codec 4, and
// the kind lands once every member is current and is the same on every node.
// Invariant: I-210
func TestCluster_Codec6RefusedWhileMemberOld(t *testing.T) {
	c := testharness.NewCluster(t, 3, 2, 1)
	defer c.Close()
	c.WriteFile(0, "/films/a", testharness.Pattern(100, 1))
	c.Settle()
	leader := c.Leader()
	films, err := leader.Node.FSM.Resolve([]string{"films"})
	must(t, err)
	now := c.Clk.Now().UnixNano()
	leader.Node.MinCodec = func() uint8 { return meta.CodecVersion - 1 } // one member still reads codec 5
	defer func() { leader.Node.MinCodec = nil }()
	res, _, err := leader.Node.Propose(ctx, meta.Command{Op: meta.OpSetAttr, SetAttr: &meta.SetAttr{Ino: films.Ino, Mask: meta.AttrFlags, Flags: meta.WithLibrary(0, meta.LibraryMovies), Now: now}})
	if err != nil || !errors.Is(res.Err, meta.ErrCodec) {
		t.Fatalf("library flag with an old member: %v %v, want ErrCodec", res.Err, err)
	}
	res, _, err = leader.Node.Propose(ctx, meta.Command{Op: meta.OpSetAttr, SetAttr: &meta.SetAttr{Ino: films.Ino, Mask: meta.AttrFlags, Flags: meta.FlagNSFW, Now: now}})
	if err != nil || res.Err != nil {
		t.Fatalf("nsfw with an old member: %v %v", res.Err, err)
	}
	leader.Node.MinCodec = func() uint8 { return meta.CodecVersion }
	res, _, err = leader.Node.Propose(ctx, meta.Command{Op: meta.OpSetAttr, SetAttr: &meta.SetAttr{Ino: films.Ino, Mask: meta.AttrFlags, Flags: meta.WithLibrary(meta.FlagNSFW, meta.LibraryMovies), Now: now}})
	if err != nil || res.Err != nil {
		t.Fatalf("library flag after the update: %v %v", res.Err, err)
	}
	c.Settle()
	for i := range c.Nodes {
		libs := c.Nodes[i].Node.FSM.Libraries()
		if len(libs) != 1 || libs[0].Ino != films.Ino || libs[0].Kind != meta.LibraryMovies || libs[0].Path != "/films" {
			t.Fatalf("n%d libraries: %+v", i, libs)
		}
		if k := c.Nodes[i].Node.FSM.EffectiveLibrary(films.Ino); k != meta.LibraryMovies {
			t.Fatalf("n%d effective kind %s", i, k)
		}
	}
}
