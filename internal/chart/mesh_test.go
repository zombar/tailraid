package chart

import (
	"fmt"
	"os"
	"regexp"
	"strings"
	"testing"
)

const dashboardPage = "../../docs/assets/lookdev/dashboard.html"

// lookdevMesh is the dashboard lookdev's Mesh section: pike's view of atlas (lan), nomad
// (wan), honker (dead, no path) and lumen (relayed through atlas).
func lookdevMesh() string {
	self := MeshNode{Name: "pike", Title: "pike: this device, 100.64.0.12, utun4, udp 20483, mapped at 203.0.113.9:20483 by pcp"}
	members := []MeshNode{
		{Name: "atlas", Href: "http://100.64.0.3:20482/#/", Title: "atlas: leader, voter, 100.64.0.3; lan 192.168.1.20:20483, seen 1s ago, handshake 42s ago; open atlas's dashboard"},
		{Name: "nomad", Href: "http://100.64.0.40:20482/#/", Title: "nomad: non-voter, draining, 100.64.0.40; wan 203.0.113.40:20483, seen 0s ago; open nomad's dashboard"},
		{Name: "honker", Off: true, Title: "honker: dead 3m0s, 100.64.0.31; no path (fanning out to 2 candidates); no web listener"},
		{Name: "lumen", Href: "http://100.64.0.5:20482/#/", Title: "lumen: voter, 100.64.0.5; relayed through atlas, seen 2s ago; open lumen's dashboard"},
	}
	edges := []MeshEdge{
		{To: "atlas", Class: "lan", Title: "atlas: lan 192.168.1.20:20483, seen 1s ago, handshake 42s ago"},
		{To: "nomad", Class: "wan", Title: "nomad: wan 203.0.113.40:20483, seen 0s ago, handshake 1m10s ago"},
		{To: "honker", Class: "none", Title: "honker: none, last seen 3m0s ago (fanning out to 2 candidates)"},
		{To: "lumen", Via: "atlas", Class: "relay", Title: "lumen: relayed through atlas, seen 2s ago"},
	}
	return Mesh("Paths from pike (this device)", self, members, edges)
}

var meshBlock = regexp.MustCompile(`(?s)<svg class="chart mesh".*?</svg>`)

// The dashboard lookdev's Mesh picture is the package's rendering of its data set, so the
// signed-off markup and the code agree. UPDATE_LOOKDEV=1 rewrites the block.
// Invariant: I-331
func TestChart_LookdevMeshMatchesDashboard(t *testing.T) {
	page, err := os.ReadFile(dashboardPage)
	if err != nil {
		t.Fatal(err)
	}
	want := lookdevMesh()
	blocks := meshBlock.FindAllStringIndex(string(page), -1)
	if len(blocks) != 1 {
		t.Fatalf("the page has %d mesh blocks, want 1", len(blocks))
	}
	if os.Getenv("UPDATE_LOOKDEV") != "" {
		out := string(page[:blocks[0][0]]) + want + string(page[blocks[0][1]:])
		if err := os.WriteFile(dashboardPage, []byte(out), 0o644); err != nil { //nolint:gosec // the lookdev page
			t.Fatal(err)
		}
		t.Log("rewrote the lookdev's mesh block")
	}
	if got := string(page[blocks[0][0]:blocks[0][1]]); got != want {
		t.Errorf("the mesh block differs from the package's rendering; run with UPDATE_LOOKDEV=1 to rewrite the page")
	}
}

var (
	edgeRe = regexp.MustCompile(`<line class="mesh-edge ([a-z]+)" x1="(\d+)" y1="(\d+)" x2="(\d+)" y2="(\d+)">`)
	nodeRe = regexp.MustCompile(`<g class="mesh-node([^"]*)">(<a class="mesh-link" href="([^"]*)">)?<circle cx="(\d+)" cy="(\d+)"`)
)

// The chart draws exactly the edges given, each in its class from the centre, a relayed
// one from its relay's position; every node with a page is a link around its circle and
// name, a dead one is dimmed; the ring is an ellipse offset by half a step, so the
// members of a four-node cluster sit at the corners of a flattened X.
// Invariant: I-331
func TestChart_Mesh_EdgesNodesAndLinks(t *testing.T) {
	svg := lookdevMesh()
	edges := edgeRe.FindAllStringSubmatch(svg, -1)
	if len(edges) != 4 {
		t.Fatalf("%d edges, want 4", len(edges))
	}
	nodes := nodeRe.FindAllStringSubmatch(svg, -1)
	if len(nodes) != 5 {
		t.Fatalf("%d nodes, want 5", len(nodes))
	}
	pos := map[string][2]string{}
	for i, name := range []string{"pike", "atlas", "nomad", "honker", "lumen"} {
		pos[name] = [2]string{nodes[i][4], nodes[i][5]}
	}
	for i, e := range []struct{ class, from, to string }{{"lan", "pike", "atlas"}, {"wan", "pike", "nomad"}, {"none", "pike", "honker"}, {"relay", "atlas", "lumen"}} {
		got := edges[i]
		if got[1] != e.class || [2]string{got[2], got[3]} != pos[e.from] || [2]string{got[4], got[5]} != pos[e.to] {
			t.Errorf("edge %d: %v, want %s from %s to %s", i, got[1:], e.class, e.from, e.to)
		}
	}
	if !strings.Contains(nodes[0][1], "self") || nodes[0][2] != "" {
		t.Errorf("this device: %q linked %q", nodes[0][1], nodes[0][2])
	}
	if nodes[1][3] != "http://100.64.0.3:20482/#/" || nodes[3][2] != "" || !strings.Contains(nodes[3][1], "off") {
		t.Errorf("atlas link %q, honker link %q class %q", nodes[1][3], nodes[3][2], nodes[3][1])
	}
	// the flattened X: the corners mirror each other about the centre
	if pos["atlas"][0] != pos["lumen"][0] || pos["nomad"][0] != pos["honker"][0] || pos["atlas"][1] != pos["nomad"][1] || pos["honker"][1] != pos["lumen"][1] {
		t.Errorf("positions: %v", pos)
	}
	if strings.Count(svg, `class="mesh-name"`) != 5 || strings.Count(svg, "<title>") != 9 {
		t.Errorf("names %d, titles %d", strings.Count(svg, `class="mesh-name"`), strings.Count(svg, "<title>"))
	}
}

// Past twelve members the ring keeps every edge and node but drops the names; a relayed
// edge whose relay is not on the ring is drawn from the centre; names and titles are
// escaped; a class the sheet does not know draws as none.
// Invariant: I-331, I-197
func TestChart_Mesh_ManyAndHostile(t *testing.T) {
	var members []MeshNode
	var edges []MeshEdge
	for i := range 13 {
		n := fmt.Sprintf("n%d", i)
		members = append(members, MeshNode{Name: n, Title: n})
		edges = append(edges, MeshEdge{To: n, Class: "lan"})
	}
	svg := Mesh("t", MeshNode{Name: "me"}, members, edges)
	if strings.Contains(svg, "mesh-name") || len(edgeRe.FindAllString(svg, -1)) != 13 || len(nodeRe.FindAllString(svg, -1)) != 14 {
		t.Errorf("13 members: names kept or counts off")
	}
	svg = Mesh("<t>", MeshNode{Name: "me&", Title: "<me>"}, []MeshNode{{Name: "<a>", Title: "a\"", Href: "http://x/\"><script>"}},
		[]MeshEdge{{To: "<a>", Via: "nobody", Class: "relay", Title: "<r>"}, {To: "<a>", Class: "odd"}})
	if strings.Contains(svg, "<a>") || strings.Contains(svg, "<me>") || strings.Contains(svg, "<r>") || strings.Contains(svg, "<script>") {
		t.Errorf("unescaped: %s", svg)
	}
	edges2 := edgeRe.FindAllStringSubmatch(svg, -1)
	if len(edges2) != 2 || edges2[0][2] != "380" || edges2[0][3] != "130" || edges2[1][1] != "none" {
		t.Errorf("relay without a relay on the ring, odd class: %v", edges2)
	}
}
