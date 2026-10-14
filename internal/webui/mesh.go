package webui

import (
	"fmt"
	"net"
	"net/http"
	"strings"

	"github.com/zombar/tailraid/internal/admin"
	"github.com/zombar/tailraid/internal/chart"
	"github.com/zombar/tailraid/internal/meta"
)

// mesh answers GET /api/mesh: the Mesh section's picture (docs/13 §7, docs/20 §10),
// drawn from this node's status; 404 on a device without a mesh (I-331).
func (s *Server) mesh(w http.ResponseWriter, _ *http.Request) {
	if s.o.Status == nil {
		writeErr(w, http.StatusServiceUnavailable, "status is not wired on this node")
		return
	}
	rep, ok := s.report().(admin.Report)
	if !ok || rep.Mesh == nil {
		writeErr(w, http.StatusNotFound, "this device has no mesh")
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"svg": meshChart(rep)})
}

// meshChart is the report's mesh as the dashboard's picture: this device at the centre,
// exactly the peers of status.mesh on the ring, one edge per peer in its class, every
// member with a web listener a link to its own dashboard, a dead member dimmed (I-331).
func meshChart(rep admin.Report) string {
	m := rep.Mesh
	byIdx := map[meta.NodeIdx]*admin.NodeReport{}
	for i := range rep.Nodes {
		byIdx[rep.Nodes[i].Idx] = &rep.Nodes[i]
	}
	self := chart.MeshNode{Name: rep.Node, Title: selfTitle(rep)}
	members := make([]chart.MeshNode, 0, len(m.Peers))
	edges := make([]chart.MeshEdge, 0, len(m.Peers))
	for i := range m.Peers {
		p := &m.Peers[i]
		n := byIdx[p.Idx]
		if n == nil {
			n = &admin.NodeReport{}
		}
		name := p.Name
		if name == "" {
			name = n.Name
		}
		if name == "" {
			name = fmt.Sprintf("node %d", p.Idx)
		}
		path := pathText(p)
		node := chart.MeshNode{Name: name, Off: !n.Live, Href: pageURL(n), Title: name + ": " + strings.Join(roles(n), ", ") + "; " + path}
		if node.Href != "" {
			node.Title += "; open " + name + "'s dashboard"
		} else {
			node.Title += "; no web listener"
		}
		members = append(members, node)
		edges = append(edges, chart.MeshEdge{To: name, Via: p.Via, Class: p.Class, Title: name + ": " + path})
	}
	return chart.Mesh("Paths from "+rep.Node+" (this device)", self, members, edges)
}

// selfTitle is this device's tooltip: address, interface, port and the gateway mapping.
func selfTitle(rep admin.Report) string {
	m := rep.Mesh
	t := fmt.Sprintf("%s: this device, %s", rep.Node, m.Addr)
	if m.Interface != "" {
		t += ", " + m.Interface
	}
	t += fmt.Sprintf(", udp %d", m.Port)
	if pm := m.PortMap; pm != nil && pm.External != "" {
		t += fmt.Sprintf(", mapped at %s by %s", pm.External, pm.Protocol)
	}
	return t
}

// pathText words a peer's path as the tooltips say it.
func pathText(p *admin.MeshPeer) string {
	var t string
	switch p.Class {
	case "relay":
		t = "relayed through " + p.Via
	case "lan", "wan":
		t = p.Class + " " + p.Endpoint
	default:
		t = "no path"
		if p.SeenAge != "" {
			return t + ", last seen " + p.SeenAge + " ago"
		}
		return t
	}
	if p.SeenAge != "" {
		t += ", seen " + p.SeenAge + " ago"
	}
	if p.HandshakeAge != "" {
		t += ", handshake " + p.HandshakeAge + " ago"
	}
	return t
}

// roles words a member's standing: leader, voter or non-voter, draining, dead.
func roles(n *admin.NodeReport) []string {
	var r []string
	if n.Leader {
		r = append(r, "leader")
	}
	if n.Voter {
		r = append(r, "voter")
	} else {
		r = append(r, "non-voter")
	}
	if n.State == "draining" {
		r = append(r, "draining")
	}
	if !n.Live {
		if n.DeadFor != "" {
			r = append(r, "dead "+n.DeadFor)
		} else {
			r = append(r, "dead")
		}
	}
	host := n.Addr
	if h, _, err := net.SplitHostPort(n.Addr); err == nil {
		host = h
	}
	if host != "" {
		r = append(r, host)
	}
	return r
}

// pageURL is a member's own dashboard, the target the Stats page's member menu uses:
// its mesh address and web port; "" for a member without a web listener.
func pageURL(n *admin.NodeReport) string {
	if n.Web == 0 || n.Addr == "" {
		return ""
	}
	host := n.Addr
	if h, _, err := net.SplitHostPort(n.Addr); err == nil {
		host = h
	}
	return fmt.Sprintf("http://%s/#/", net.JoinHostPort(host, fmt.Sprint(n.Web)))
}
