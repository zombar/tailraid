package cluster_test

import (
	"bytes"
	"encoding/json"
	"fmt"
	"testing"

	"github.com/zombar/tailraid/internal/meta"
	"github.com/zombar/tailraid/internal/search"
	"github.com/zombar/tailraid/internal/testharness"
	"github.com/zombar/tailraid/internal/vfs"
)

// searchOf is node i's index service, as serve builds it.
func searchOf(c *testharness.Cluster, i int) *search.Service {
	nd := c.Nodes[i]
	return &search.Service{
		Src: nd.Node.FSM, Clk: c.Clk, Cfg: search.DefaultConfig(),
		Vol: search.VFS{FS: nd.FS, St: nd.Node.FSM, Rd: vfs.NewReader(nd.Node.FSM, nd.Prober, nd.Client, nil, nil)},
	}
}

func leaderIndex(c *testharness.Cluster) int {
	l := c.Leader()
	for i, nd := range c.Nodes {
		if nd == l {
			return i
		}
	}
	return -1
}

func flagLibrary(t *testing.T, c *testharness.Cluster, path string, k meta.LibraryKind) {
	t.Helper()
	leader := c.Leader()
	in, err := leader.Node.FSM.Resolve(meta.SplitPath(path))
	must(t, err)
	res, _, err := leader.Node.Propose(ctx, meta.Command{Op: meta.OpSetAttr, SetAttr: &meta.SetAttr{Ino: in.Ino, Mask: meta.AttrFlags, Flags: meta.WithLibrary(0, k), Now: c.Clk.Now().UnixNano()}})
	if err != nil || res.Err != nil {
		t.Fatalf("flag %s: %v %v", path, res.Err, err)
	}
}

// The leader's segment is flushed before its name appears: a follower that sees the
// name reads a complete segment, and every node answers a query and lists the
// catalogue with identical bytes.
// Invariant: I-206, I-208, I-211
func TestCluster_SearchDeterministicAcrossNodes(t *testing.T) {
	c := testharness.NewCluster(t, 3, 2, 1)
	defer c.Close()
	c.WriteFile(0, "/docs/readme.md", []byte("tailraid keeps every shard twice"))
	c.WriteFile(0, "/docs/shard notes.txt", []byte("notes about shards"))
	c.WriteFile(1, "/films/Arrival (2016)/arrival.mkv", testharness.Pattern(300, 1))
	c.WriteFile(1, "/films/Arrival (2016)/movie.nfo", []byte(`<movie><title>Arrival</title><plot>Heptapods.</plot></movie>`))
	c.Settle()
	flagLibrary(t, c, "/films", meta.LibraryMovies)
	li := leaderIndex(c)
	res, err := searchOf(c, li).Pass(ctx)
	if err != nil || res.Indexed != 2 || res.Seq != 1 {
		t.Fatalf("pass %+v %v", res, err)
	}
	c.Settle()
	var want []byte
	for i := range c.Nodes {
		svc := searchOf(c, i)
		r := svc.Search(ctx, "shard", "", 0)
		cat, err := svc.Catalog(ctx)
		must(t, err)
		if st := svc.Status(); st.Docs != 2 || st.Segments != 1 || st.Index != "" || st.Catalog != 1 {
			t.Fatalf("n%d status %+v", i, st)
		}
		got, _ := json.Marshal(struct {
			R search.Result
			C []search.Entry
		}{r, cat})
		if i == 0 {
			want = got
			if len(r.Hits) != 2 || r.Hits[0].Match != "name" || r.Hits[1].Match != "content" || len(cat) != 1 || cat[0].Plot != "Heptapods." {
				t.Fatalf("n0: %s", got)
			}
			continue
		}
		if !bytes.Equal(got, want) {
			t.Fatalf("n%d differs:\n%s\n%s", i, got, want)
		}
	}
}

// A new leader continues from the chain the old one wrote: nothing indexed twice.
// Invariant: I-206
func TestIndexer_LeaderChange_ContinuesFromChain(t *testing.T) {
	c := testharness.NewCluster(t, 3, 2, 1)
	defer c.Close()
	for i := range 3 {
		c.WriteFile(0, fmt.Sprintf("/n%d.txt", i), []byte(fmt.Sprintf("before%d", i)))
	}
	c.Settle()
	old := leaderIndex(c)
	if res, err := searchOf(c, old).Pass(ctx); err != nil || res.Indexed != 3 {
		t.Fatalf("first pass %+v %v", res, err)
	}
	c.Settle()
	c.Kill(old)
	next := leaderIndex(c)
	c.WriteFile(next, "/after.txt", []byte("after"))
	c.Settle()
	svc := searchOf(c, next)
	res, err := svc.Pass(ctx)
	if err != nil || res.Indexed != 1 || res.Seq != 2 {
		t.Fatalf("new leader's pass %+v %v", res, err)
	}
	for _, q := range []string{"before1", "after"} {
		if len(svc.Search(ctx, q, "", 0).Hits) != 1 {
			t.Fatalf("%s not found after the leader change", q)
		}
	}
}
