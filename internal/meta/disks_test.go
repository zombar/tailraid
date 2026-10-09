package meta

import (
	"bytes"
	"testing"
	"time"

	"github.com/zombar/tailraid/internal/testseed"
)

// topology builds a node table of len(disks) hosts: host i is a device (voter when
// voter[i]) with its own store of the given weight, plus disks[i] added disks of weight 2.
// A device pinned to 0 is a host whose system disk does not store shards.
func topology(disks []int, voter []bool, deviceCap uint64) []Node {
	var out []Node
	idx := NodeIdx(1)
	for h, nd := range disks {
		host := idx
		out = append(out, Node{Idx: host, ID: "host", Kind: KindDevice, State: Active, Capacity: deviceCap, Voter: voter[h]})
		idx++
		for range nd {
			out = append(out, Node{Idx: idx, ID: "disk", Kind: KindDisk, State: Active, Capacity: 2, Host: host})
			idx++
		}
	}
	return out
}

func allVoters(n int) []bool {
	v := make([]bool, n)
	for i := range v {
		v[i] = true
	}
	return v
}

func allLive(NodeIdx) bool { return true }

func domainCounts(t *testing.T, p []NodeIdx, nodes []Node) map[NodeIdx]int {
	t.Helper()
	dom := DomainsOf(nodes)
	out := map[NodeIdx]int{}
	for _, n := range p {
		if n != 0 {
			out[dom.Of(n)]++
		}
	}
	return out
}

// legacyPlace is Place before domains existed: the plain top-n by score. Kept here as the
// reference that one target per host must keep matching.
func legacyPlace(h Hash, cands []Candidate, n int) []NodeIdx {
	r := ranked(h, cands)
	if n > len(r) {
		n = len(r)
	}
	out := make([]NodeIdx, n)
	for i := range out {
		out[i] = r[i].Idx
	}
	return out
}

// Invariant: I-87
func TestPlace_OneTargetPerHost_Unchanged(t *testing.T) {
	r := testseed.Seed(t)
	for trial := range 200 {
		nc := 1 + r.Intn(12)
		c := make([]Candidate, nc)
		for i := range c {
			c[i] = Candidate{Idx: NodeIdx(i + 1), Weight: uint64(1 + r.Intn(100)), Voter: r.Intn(2) == 0}
		}
		for i := range uint64(200) {
			h := hashN(uint64(trial)*1000 + i)
			n := 1 + r.Intn(10)
			if got, want := Place(h, c, n), legacyPlace(h, c, n); !equalIdx(got, want) {
				t.Fatalf("trial %d hash %d n %d: %v != %v", trial, i, n, got, want)
			}
		}
	}
}

// Invariant: I-87
func TestPlace_HostCap(t *testing.T) {
	cases := []struct {
		name  string
		disks []int
		n     int
		cap   int // ceil(n / hosts with a target)
	}{
		{"2 hosts x 3 disks, n=4", []int{3, 3}, 4, 2},
		{"2 hosts x 3 disks, n=3", []int{3, 3}, 3, 2},
		{"4 hosts x 3 disks, n=8", []int{3, 3, 3, 3}, 8, 2},
		{"3 hosts x 2 disks, n=5", []int{2, 2, 2}, 5, 2},
		{"1 host x 6 disks, n=4", []int{6}, 4, 4},
	}
	for _, c := range cases {
		nodes := topology(c.disks, allVoters(len(c.disks)), 1)
		cs := Candidates(nodes, allLive)
		for i := range uint64(2000) {
			p := Place(hashN(i), cs, c.n)
			if len(p) != c.n {
				t.Fatalf("%s: placed %d of %d", c.name, len(p), c.n)
			}
			distinctNonZero(t, p, c.name)
			for host, cnt := range domainCounts(t, p, nodes) {
				if cnt > c.cap {
					t.Fatalf("%s hash %d: host %d holds %d > cap %d: %v", c.name, i, host, cnt, c.cap, p)
				}
			}
		}
	}
	// a host with too few targets: the cap cannot hold, the second pass fills from the rest
	nodes := topology([]int{0, 5}, allVoters(2), 1) // host 1: its own store only; host 2: 6 targets
	cs := Candidates(nodes, allLive)
	for i := range uint64(500) {
		p := Place(hashN(i), cs, 6)
		if len(p) != 6 {
			t.Fatalf("uneven: placed %d of 6", len(p))
		}
		distinctNonZero(t, p, "uneven")
	}
}

// Invariant: I-87
func TestPlace_HostCap_NNodeDeterminism(t *testing.T) {
	r := testseed.Seed(t)
	nodes := topology([]int{2, 3, 1, 4}, []bool{true, true, false, true}, 3)
	base := Candidates(nodes, allLive)
	for i := range uint64(3000) {
		h := hashN(i)
		want := PlaceKM(h, base, 4, 2)
		for node := range 5 {
			if got := PlaceKM(h, shuffled(r, base), 4, 2); !equalIdx(got, want) {
				t.Fatalf("hash %d node %d: %v != %v", i, node, got, want)
			}
		}
	}
}

// Invariant: I-87
func TestFillPlacement_HostCap(t *testing.T) {
	// 2 hosts x (own store + 2 disks), 2+2: cap 2 per host; one slot of host A held, refill
	// the other three: host A must receive at most one more.
	nodes := topology([]int{2, 2}, allVoters(2), 2)
	dom := DomainsOf(nodes)
	cs := Candidates(nodes, allLive)
	for i := range uint64(2000) {
		h := hashN(i)
		place := []NodeIdx{1, 0, 0, 0}
		FillPlacement(h, place, ExcludeIdx(cs, place), 2, dom)
		distinctNonZero(t, place, "fill")
		for host, cnt := range domainCounts(t, place, nodes) {
			if cnt > 2 {
				t.Fatalf("hash %d: host %d holds %d: %v", i, host, cnt, place)
			}
		}
		for j, n := range place {
			if n == 0 {
				t.Fatalf("hash %d: slot %d unfilled: %v", i, j, place)
			}
		}
	}
	// a nil domain map keeps the old behaviour: every node is its own domain
	c := cands(1, 1, 1, 1, 1)
	place := make([]NodeIdx, 4)
	FillPlacement(hashN(7), place, c, 2, nil)
	if !equalIdx(place, PlaceKM(hashN(7), c, 2, 2)) {
		t.Fatalf("nil domains: %v", place)
	}
}

// Invariant: I-87, I-77
func TestCandidates_DiskDomainAndVoter(t *testing.T) {
	nodes := topology([]int{2, 1}, []bool{true, false}, 1)
	// host 1 (voter): idx 1, disks 2 and 3; host 4 (non-voter): idx 4, disk 5
	nodes = append(nodes,
		Node{Idx: 6, Kind: KindDisk, State: Draining, Capacity: 2, Host: 1},
		Node{Idx: 7, Kind: KindDisk, State: Active, Capacity: 0, Host: 1})
	cs := Candidates(nodes, allLive)
	want := []Candidate{
		{Idx: 1, Weight: 1, Voter: true},
		{Idx: 2, Weight: 2, Voter: true, Domain: 1},
		{Idx: 3, Weight: 2, Voter: true, Domain: 1},
		{Idx: 4, Weight: 1, Voter: false},
		{Idx: 5, Weight: 2, Voter: false, Domain: 4},
	}
	if len(cs) != len(want) {
		t.Fatalf("got %+v", cs)
	}
	for i := range want {
		if cs[i] != want[i] {
			t.Errorf("cand %d = %+v, want %+v", i, cs[i], want[i])
		}
	}
	// a host that is not Active takes its disks out of placement with it
	nodes[0].State = Draining
	for _, c := range Candidates(nodes, allLive) {
		if c.domain() == 1 {
			t.Errorf("disk %d of a draining host is a candidate", c.Idx)
		}
	}
	// a dead disk (liveness from the prober, I-90) is not a candidate; its host still is
	nodes[0].State = Active
	cs = Candidates(nodes, func(n NodeIdx) bool { return n != 2 })
	for _, c := range cs {
		if c.Idx == 2 {
			t.Errorf("dead disk is a candidate")
		}
	}
}

// Invariant: I-88
func TestAutoPolicy_Disks_Table(t *testing.T) {
	cases := []struct {
		name    string
		disks   []int
		voter   []bool
		devCap  uint64
		k, m    uint8
		dk, hst int // Tolerance
	}{
		{"1 x 1", []int{0}, allVoters(1), 1, 1, 0, 0, 0},
		{"1 host, system disk + 3 disks", []int{3}, allVoters(1), 1, 2, 2, 2, 0},
		{"1 host, 4 disks, system pinned 0", []int{4}, allVoters(1), 0, 2, 2, 2, 0},
		{"2 x 1", []int{0, 0}, allVoters(2), 1, 1, 1, 1, 1},
		{"2 hosts x 3 targets", []int{2, 2}, allVoters(2), 1, 2, 2, 2, 1},
		{"3 x 1", []int{0, 0, 0}, allVoters(3), 1, 2, 1, 1, 1},
		{"4 hosts x 3 targets", []int{2, 2, 2, 2}, allVoters(4), 1, 6, 2, 2, 1},
		{"voter + laptop with 3 disks", []int{0, 3}, []bool{true, false}, 1, 1, 2, 2, 1},
	}
	for _, c := range cases {
		nodes := topology(c.disks, c.voter, c.devCap)
		k, m := AutoPolicy(nodes)
		if k != c.k || m != c.m {
			t.Errorf("%s: %d+%d, want %d+%d", c.name, k, m, c.k, c.m)
		}
		if d, h := Tolerance(nodes, k, m); d != c.dk || h != c.hst {
			t.Errorf("%s: tolerates %d disks %d hosts, want %d, %d", c.name, d, h, c.dk, c.hst)
		}
	}
}

// Invariant: I-88, I-75
func TestAutoPolicy_OneTargetPerHost_Unchanged(t *testing.T) {
	// the device-count rule of I-75, restated, over the whole grid
	rule := func(v, s int) (uint8, uint8) {
		n := v + s
		m := 0
		switch {
		case n <= 1:
		case n <= 3:
			m = 1
		default:
			m = 2
		}
		k := max(min(max(v, 1), n-m, MaxAutoK), 1)
		return uint8(k), uint8(m)
	}
	for v := range 10 {
		for s := range 10 {
			if v+s == 0 {
				continue
			}
			wk, wm := rule(v, s)
			if k, m := AutoPolicy(devices(v, s)); k != wk || m != wm {
				t.Errorf("v=%d s=%d: %d+%d, want %d+%d", v, s, k, m, wk, wm)
			}
		}
	}
}

// Invariant: I-88
func TestAutoPolicy_SurvivesOneHost(t *testing.T) {
	r := testseed.Seed(t)
	for range 500 {
		hosts := 2 + r.Intn(5)
		disks := make([]int, hosts)
		voter := make([]bool, hosts)
		for i := range disks {
			disks[i] = r.Intn(5)
			voter[i] = r.Intn(3) > 0
		}
		nodes := topology(disks, voter, 1)
		k, m := AutoPolicy(nodes)
		if _, h := Tolerance(nodes, k, m); h < 1 {
			t.Fatalf("disks %v voters %v: %d+%d survives no host", disks, voter, k, m)
		}
		if err := PolicyAllowed(nodes, allLiveness{}, k, m); err != nil {
			t.Fatalf("disks %v: %d+%d not allowed: %v", disks, k, m, err)
		}
	}
}

type allLiveness struct{}

func (allLiveness) IsLive(NodeIdx) bool           { return true }
func (allLiveness) DeadFor(NodeIdx) time.Duration { return 0 }

// Invariant: I-88
func TestTolerance_Table(t *testing.T) {
	cases := []struct {
		name    string
		disks   []int
		k, m    uint8
		dk, hst int
	}{
		{"one host", []int{3}, 2, 2, 2, 0},
		{"2 hosts, 3+1", []int{1, 1}, 3, 1, 1, 0},
		{"2 hosts, 2+2", []int{1, 1}, 2, 2, 2, 1},
		{"3 hosts, 2+1", []int{0, 0, 0}, 2, 1, 1, 1},
		{"4 hosts x 3, 6+2", []int{2, 2, 2, 2}, 6, 2, 2, 1},
		{"4 hosts x 3, 4+4", []int{2, 2, 2, 2}, 4, 4, 4, 2},
		{"uneven: 1 + 5 targets, 2+2", []int{0, 4}, 2, 2, 2, 0},
		{"no parity", []int{1, 1}, 2, 0, 0, 0},
	}
	for _, c := range cases {
		nodes := topology(c.disks, allVoters(len(c.disks)), 1)
		if d, h := Tolerance(nodes, c.k, c.m); d != c.dk || h != c.hst {
			t.Errorf("%s: %d disks %d hosts, want %d, %d", c.name, d, h, c.dk, c.hst)
		}
	}
}

// set-redundancy needs k+m live Active nodes at the moment it is set (moved from the
// admin package, which only wrapped PolicyAllowed).
func TestPolicyAllowed_LiveCountBoundary(t *testing.T) {
	dev := KindDevice
	nodes := []Node{{Idx: 1, Kind: dev, State: Active}, {Idx: 2, Kind: dev, State: Active}, {Idx: 3, Kind: dev, State: Active}, {Idx: 4, Kind: dev, State: Draining}}
	cases := []struct {
		k, m uint8
		ok   bool
	}{
		{1, 1, true}, {2, 0, true}, {2, 1, true}, {3, 0, true}, {2, 2, false}, {4, 0, false}, {0, 1, false}, {200, 57, false},
	}
	for _, c := range cases {
		err := PolicyAllowed(nodes, allLiveness{}, c.k, c.m)
		if (err == nil) != c.ok {
			t.Fatalf("policy %d+%d with 3 live active nodes: err=%v, want ok=%v", c.k, c.m, err, c.ok)
		}
	}
}

// Invariant: I-88
func TestPolicyAllowed_CountsTargets(t *testing.T) {
	nodes := topology([]int{3}, allVoters(1), 1) // 4 targets on one host
	for _, c := range []struct {
		k, m uint8
		ok   bool
	}{{2, 1, true}, {2, 2, true}, {3, 2, false}} {
		err := PolicyAllowed(nodes, allLiveness{}, c.k, c.m)
		if (err == nil) != c.ok {
			t.Errorf("%d+%d: %v", c.k, c.m, err)
		}
	}
	// a device pinned to 0 with disks is not a target
	nodes[0].Capacity = 0
	if err := PolicyAllowed(nodes, allLiveness{}, 2, 2); err == nil {
		t.Errorf("2+2 allowed on 3 targets")
	}
}

// Invariant: I-22
func TestCodec_DiskNode_RoundTrip(t *testing.T) {
	disk := Node{Idx: 0, ID: "d1", Name: "pike/1", Kind: KindDisk, Capacity: 99, Host: 3, JoinedAt: 5}
	b := MustEncodeCommand(Command{Op: OpNodeUpsert, NodeUpsert: &NodeUpsert{Node: disk}})
	if b[0] != 2 {
		t.Errorf("disk upsert written as version %d, want 2", b[0])
	}
	c, err := DecodeCommand(b)
	if err != nil || c.NodeUpsert.Node != disk {
		t.Fatalf("decode: %+v %v", c.NodeUpsert, err)
	}
	// a device upsert keeps the version-1 bytes, so a cluster without disks writes what an
	// older node reads (docs/12 §6)
	dev := Node{ID: "n1", Name: "pike", Kind: KindDevice, Capacity: 7, Voter: true}
	b = MustEncodeCommand(Command{Op: OpNodeUpsert, NodeUpsert: &NodeUpsert{Node: dev}})
	if b[0] != 1 {
		t.Errorf("device upsert written as version %d, want 1", b[0])
	}
	if c, err := DecodeCommand(b); err != nil || c.NodeUpsert.Node != dev {
		t.Fatalf("decode device: %+v %v", c.NodeUpsert, err)
	}

	st := NewState()
	st.Nodes[1] = &Node{Idx: 1, ID: "n1", Kind: KindDevice, State: Active, Capacity: 1}
	st.Nodes[2] = &Node{Idx: 2, ID: "d1", Kind: KindDisk, State: Active, Capacity: 2, Host: 1}
	st.NextNode = 3
	var a, b2 bytes.Buffer
	if err := EncodeState(&a, st); err != nil {
		t.Fatal(err)
	}
	got, err := DecodeState(bytes.NewReader(a.Bytes()))
	if err != nil {
		t.Fatal(err)
	}
	if *got.Nodes[2] != *st.Nodes[2] {
		t.Fatalf("snapshot node: %+v", got.Nodes[2])
	}
	if err := EncodeState(&b2, got); err != nil || !bytes.Equal(a.Bytes(), b2.Bytes()) {
		t.Fatalf("re-encode differs (%v)", err)
	}
}

// Invariant: I-42
func TestFSM_DiskUpsert_HostRules(t *testing.T) {
	tf := newTFSM(t)
	host := NodeIdx(tf.ok(Command{Op: OpNodeUpsert, NodeUpsert: &NodeUpsert{Node: Node{ID: "h", Kind: KindDevice, Capacity: 1}}}).Ino)
	s3 := NodeIdx(tf.ok(Command{Op: OpNodeUpsert, NodeUpsert: &NodeUpsert{Node: Node{ID: "s", Kind: KindS3, Capacity: 1}}}).Ino)
	for _, c := range []struct {
		name string
		host NodeIdx
	}{{"no host", 0}, {"unknown host", 99}, {"S3 host", s3}} {
		tf.fails(Command{Op: OpNodeUpsert, NodeUpsert: &NodeUpsert{Node: Node{ID: "d-" + c.name, Kind: KindDisk, Capacity: 2, Host: c.host}}}, ErrBadCommand)
	}
	disk := NodeIdx(tf.ok(Command{Op: OpNodeUpsert, NodeUpsert: &NodeUpsert{Node: Node{ID: "d", Kind: KindDisk, Capacity: 2, Host: host}}}).Ino)
	// a device row never carries a host
	tf.fails(Command{Op: OpNodeUpsert, NodeUpsert: &NodeUpsert{Node: Node{ID: "x", Kind: KindDevice, Host: host}}}, ErrBadCommand)
	// a disk of a host that is not Active cannot be added
	tf.ok(Command{Op: OpNodeSetState, NodeSetState: &NodeSetState{Idx: host, State: Draining}})
	tf.fails(Command{Op: OpNodeUpsert, NodeUpsert: &NodeUpsert{Node: Node{ID: "d2", Kind: KindDisk, Capacity: 2, Host: host}}}, ErrBadCommand)
	// removing a host is refused while one of its disks is not removed
	tf.fails(Command{Op: OpNodeSetState, NodeSetState: &NodeSetState{Idx: host, State: Removed}}, ErrNodeNotEmpty)
	tf.ok(Command{Op: OpNodeSetState, NodeSetState: &NodeSetState{Idx: disk, State: Removed}})
	tf.ok(Command{Op: OpNodeSetState, NodeSetState: &NodeSetState{Idx: host, State: Removed}})
}
