package chart

import (
	"fmt"
	"html"
	"math"
	"strings"
)

// MeshNode is one node of the mesh chart (docs/13 §7, docs/20 §10): its name on the
// ring, its tooltip, and the link to its own dashboard ("" draws plain text).
type MeshNode struct {
	Name  string
	Title string
	Href  string
	Off   bool // a dead member: dimmed
}

// MeshEdge is this device's path to a member, in the class of the path (lan, wan, relay
// or none); a relayed path names the member it goes through, and is drawn from there.
type MeshEdge struct {
	To    string
	Via   string
	Class string
	Title string
}

// The mesh chart's frame: this device at the centre, the members on an ellipse around
// it, offset by half a step so no member sits straight above or below the centre (four
// read as a flattened X); names beside the nodes on the outer side, this device's under
// its dot; more than meshNamesMax members drop the names, which the table beside the
// chart still carries.
const (
	MeshHeight   = 250
	meshCX       = Width / 2
	meshCY       = MeshHeight/2 + 5 // room for the title
	meshRX       = 300
	meshRY       = 90
	meshR        = 11
	meshGap      = 18 // from a node's centre to its name
	meshNamesMax = 12
)

// Mesh draws this device's paths to every member: one edge per member in its class,
// every node with a title, every linked node an <a> around its circle and name (I-331).
// Classes only (I-197).
func Mesh(title string, self MeshNode, members []MeshNode, edges []MeshEdge) string {
	type pos struct{ x, y int }
	at := map[string]pos{self.Name: {meshCX, meshCY}}
	ps := make([]pos, len(members))
	for i := range members {
		a := -math.Pi/2 - math.Pi/float64(len(members)) + 2*math.Pi*float64(i)/float64(len(members))
		ps[i] = pos{int(math.Round(meshCX + meshRX*math.Cos(a))), int(math.Round(meshCY + meshRY*math.Sin(a)))}
		at[members[i].Name] = ps[i]
	}
	var b strings.Builder
	label := make([]string, 0, len(edges))
	for _, e := range edges {
		switch e.Class {
		case "relay":
			label = append(label, e.To+" relayed through "+e.Via)
		default:
			label = append(label, e.To+" "+e.Class)
		}
	}
	fmt.Fprintf(&b, `<svg class="chart mesh" viewBox="0 0 %d %d" xmlns="http://www.w3.org/2000/svg" role="img" aria-label="%s">`,
		Width, MeshHeight, html.EscapeString("this device's paths to every member: "+strings.Join(label, ", ")))
	fmt.Fprintf(&b, `<text x="12" y="18" class="title">%s</text>`, html.EscapeString(title))
	for _, e := range edges {
		from, to := at[self.Name], at[e.To]
		if e.Class == "relay" {
			if p, ok := at[e.Via]; ok {
				from = p
			}
		}
		fmt.Fprintf(&b, `<line class="mesh-edge %s" x1="%d" y1="%d" x2="%d" y2="%d"><title>%s</title></line>`,
			edgeClass(e.Class), from.x, from.y, to.x, to.y, html.EscapeString(e.Title))
	}
	names := len(members) <= meshNamesMax
	node := func(n MeshNode, p pos, cls string, nameX, nameY int, anchor string) {
		fmt.Fprintf(&b, `<g class="mesh-node%s">`, cls)
		if n.Href != "" {
			fmt.Fprintf(&b, `<a class="mesh-link" href="%s">`, html.EscapeString(n.Href))
		}
		fmt.Fprintf(&b, `<circle cx="%d" cy="%d" r="%d"/>`, p.x, p.y, meshR)
		if names {
			fmt.Fprintf(&b, `<text x="%d" y="%d" text-anchor="%s" class="mesh-name">%s</text>`, nameX, nameY, anchor, html.EscapeString(n.Name))
		}
		if n.Href != "" {
			b.WriteString(`</a>`)
		}
		fmt.Fprintf(&b, `<title>%s</title></g>`, html.EscapeString(n.Title))
	}
	node(self, at[self.Name], " self", meshCX, meshCY+meshR+15, "middle")
	for i, m := range members {
		cls := ""
		if m.Off {
			cls = " off"
		}
		p := ps[i]
		switch {
		case p.x < meshCX:
			node(m, p, cls, p.x-meshGap, p.y+5, "end")
		case p.x > meshCX:
			node(m, p, cls, p.x+meshGap, p.y+5, "start")
		case p.y < meshCY:
			node(m, p, cls, p.x, p.y-meshGap, "middle")
		default:
			node(m, p, cls, p.x, p.y+meshGap+10, "middle")
		}
	}
	b.WriteString(`</svg>`)
	return b.String()
}

// edgeClass keeps an edge's class to the four the sheet knows.
func edgeClass(c string) string {
	switch c {
	case "lan", "wan", "relay":
		return c
	}
	return "none"
}
