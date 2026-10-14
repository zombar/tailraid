package webui_test

import (
	"encoding/json"
	"net/http"
	"regexp"
	"strings"
	"testing"

	"github.com/zombar/tailraid/internal/admin"
	"github.com/zombar/tailraid/internal/mesh/portmap"
	"github.com/zombar/tailraid/internal/webui"
)

// meshReport is a device's status: three peers on a lan, a relayed and a dead no-path
// member, one of them without a web listener.
func meshReport() admin.Report {
	return admin.Report{
		Node: "pike",
		Nodes: []admin.NodeReport{
			{Idx: 1, Name: "pike", Kind: "device", State: "active", Addr: "100.64.0.12", Voter: true, Self: true, Live: true, Web: 20482},
			{Idx: 2, Name: "atlas", Kind: "device", State: "active", Addr: "100.64.0.3", Voter: true, Leader: true, Live: true, Web: 20482, RTT: "4ms"},
			{Idx: 3, Name: "lumen", Kind: "device", State: "active", Addr: "100.64.0.5", Voter: true, Live: true, Web: 20482, RTT: "61ms"},
			{Idx: 4, Name: "honker", Kind: "device", State: "draining", Addr: "100.64.0.31", Live: false, DeadFor: "3m0s"},
			{Idx: 5, Name: "atlas/1", Kind: "disk", Host: 2, State: "active", Live: true},
		},
		Mesh: &admin.MeshReport{
			Addr: "100.64.0.12", Interface: "utun4", Port: 20483,
			PortMap: &portmap.Status{Protocol: "pcp", External: "203.0.113.9:20483"},
			Peers: []admin.MeshPeer{
				{Idx: 2, Name: "atlas", Class: "lan", Endpoint: "192.168.1.20:20483", SeenAge: "1s", HandshakeAge: "42s"},
				{Idx: 3, Name: "lumen", Class: "relay", Via: "atlas", SeenAge: "2s"},
				{Idx: 4, Name: "honker", Class: "none", SeenAge: "3m0s"},
			},
		},
	}
}

// GET /api/mesh draws exactly the peers of status.mesh: one edge per peer in its class, a
// relayed one from its relay, every member with a web listener a link to its page by
// address and web port, a dead member dimmed; a device without a mesh gets 404.
// Invariant: I-331
func TestMesh_FromStatus(t *testing.T) {
	u := newUI(t, func(o *webui.Options) { o.Status = func() any { return meshReport() } })
	code, hdr, body := u.get("/api/mesh")
	if code != http.StatusOK || hdr.Get("Cache-Control") != "no-store" {
		t.Fatalf("%d %q: %s", code, hdr.Get("Cache-Control"), body)
	}
	var out struct {
		SVG string `json:"svg"`
	}
	if err := json.Unmarshal(body, &out); err != nil {
		t.Fatal(err)
	}
	svg := out.SVG
	edges := regexp.MustCompile(`<line class="mesh-edge ([a-z]+)"`).FindAllStringSubmatch(svg, -1)
	if len(edges) != 3 || edges[0][1] != "lan" || edges[1][1] != "relay" || edges[2][1] != "none" {
		t.Errorf("edges: %v", edges)
	}
	for _, want := range []string{
		`<text x="12" y="18" class="title">Paths from pike (this device)</text>`,
		`<title>pike: this device, 100.64.0.12, utun4, udp 20483, mapped at 203.0.113.9:20483 by pcp</title>`,
		`<a class="mesh-link" href="http://100.64.0.3:20482/#/" data-node="atlas">`, // the page sends the click through the hand-off (docs/21 §6)
		`<a class="mesh-link" href="http://100.64.0.5:20482/#/" data-node="lumen">`,
		`<title>atlas: leader, voter, 100.64.0.3; lan 192.168.1.20:20483, seen 1s ago, handshake 42s ago; open atlas&#39;s dashboard</title>`,
		`<title>lumen: voter, 100.64.0.5; relayed through atlas, seen 2s ago; open lumen&#39;s dashboard</title>`,
		`<g class="mesh-node off">`,
		`<title>honker: non-voter, draining, dead 3m0s, 100.64.0.31; no path, last seen 3m0s ago; no web listener</title>`,
	} {
		if !strings.Contains(svg, want) {
			t.Errorf("missing %s\nin %s", want, svg)
		}
	}
	if strings.Count(svg, "mesh-link") != 2 || strings.Contains(svg, "atlas/1") {
		t.Errorf("links %d, disk drawn %v", strings.Count(svg, "mesh-link"), strings.Contains(svg, "atlas/1"))
	}
	// the relayed edge starts at atlas's circle, not the centre
	atlas := regexp.MustCompile(`href="http://100.64.0.3:20482/#/" data-node="atlas"><circle cx="(\d+)" cy="(\d+)"`).FindStringSubmatch(svg)
	relay := regexp.MustCompile(`<line class="mesh-edge relay" x1="(\d+)" y1="(\d+)"`).FindStringSubmatch(svg)
	if atlas == nil || relay == nil || atlas[1] != relay[1] || atlas[2] != relay[2] {
		t.Errorf("relayed edge from %v, atlas at %v", relay, atlas)
	}

	plain := newUI(t) // the default status has no mesh
	if code, _, body := plain.get("/api/mesh"); code != http.StatusNotFound {
		t.Errorf("without a mesh: %d %s", code, body)
	}
}
