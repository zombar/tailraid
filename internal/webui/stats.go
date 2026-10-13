package webui

import (
	"encoding/json"
	"net"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/zombar/tailraid/internal/stats"
)

// GET /api/stats?from=&to=&node= (docs/13 §3, docs/17 §3, I-196): the charts of the range
// from the finest tier whose retention covers it, merged with every live member's unsealed
// rows when to is now. from and to are Unix seconds; to may be "now" or empty. node names
// the member whose node charts are drawn (this node by default) or "all".
func (s *Server) stats(w http.ResponseWriter, r *http.Request) {
	if s.o.Stats == nil {
		writeErr(w, http.StatusServiceUnavailable, "stats are not wired on this node")
		return
	}
	q := r.URL.Query()
	now := s.o.Clk.Now()
	from, err := strconv.ParseInt(q.Get("from"), 10, 64)
	if err != nil {
		writeErr(w, http.StatusBadRequest, "from: want Unix seconds")
		return
	}
	live := q.Get("to") == "" || q.Get("to") == "now"
	to := now.Unix()
	if !live {
		if to, err = strconv.ParseInt(q.Get("to"), 10, 64); err != nil {
			writeErr(w, http.StatusBadRequest, "to: want Unix seconds or now")
			return
		}
	}
	if from >= to {
		writeErr(w, http.StatusBadRequest, "from must be before to")
		return
	}
	if time.Duration(to-from)*time.Second > s.o.Stats.Keep.Coarse {
		writeErr(w, http.StatusBadRequest, "the range is longer than the history kept")
		return
	}
	node := q.Get("node")
	members := s.members()
	if node == "" {
		for _, m := range members {
			if m.Self {
				node = m.Name
			}
		}
	}
	fromT, toT := time.Unix(from, 0).UTC(), time.Unix(to, 0).UTC()
	type memberOut struct {
		Name string `json:"name"`
		Addr string `json:"addr"`
		Live bool   `json:"live"`
		Self bool   `json:"self"`
		Web  uint16 `json:"web,omitempty"`
	}
	nodes := make([]memberOut, 0, len(members))
	for _, m := range members {
		host := m.Addr
		if h, _, err := net.SplitHostPort(m.Addr); err == nil {
			host = h
		}
		nodes = append(nodes, memberOut{Name: m.Name, Addr: host, Live: m.Live, Self: m.Self, Web: m.Web})
	}
	if q.Get("stream") == "1" {
		names := make([]string, 0, len(members))
		for _, m := range members {
			names = append(names, m.Name)
		}
		s.statsStream(w, r, streamArgs{from: fromT, to: toT, live: live, node: node, nodes: nodes, names: names, ids: q.Get("ids")})
		return
	}
	rows, rg, err := s.o.Stats.Rows(r.Context(), fromT, toT, live)
	if err != nil {
		s.fail(w, err)
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, http.StatusOK, map[string]any{
		"range":  map[string]any{"from": from, "to": to, "live": live, "tier": rg.Tier.Dir(), "step": int64(rg.Tier.Step() / time.Second)},
		"node":   node,
		"nodes":  nodes,
		"charts": stats.Charts(rows, fromT, toT, node),
	})
}

// members is the query's member view, or none.
func (s *Server) members() []stats.Member {
	if s.o.Stats == nil || s.o.Stats.Members == nil {
		return nil
	}
	return s.o.Stats.Members()
}

type streamArgs struct {
	from, to time.Time
	live     bool
	node     string
	nodes    any
	names    []string // the members, for the frames' heights
	ids      string   // "a,b": draw only these charts
}

// streamLine is one chart of the stream after the first line: drawn, empty (no rows in
// the range) or not drawn.
type streamLine struct {
	ID      string `json:"id,omitempty"`
	SVG     string `json:"svg,omitempty"`
	Caption string `json:"caption,omitempty"`
	Empty   bool   `json:"empty,omitempty"`
	Error   string `json:"error,omitempty"`
}

// statsStream answers /api/stats?stream=1 as NDJSON (docs/17 §3, I-258): the range, the
// members and every chart's frame first, written and flushed before any row is read or
// member asked; then the rows, read once, and one line per chart in the first line's
// order (only those ids= names, when given); a failure to read the rows is a last line
// carrying only the error, since the status went out with the first line.
func (s *Server) statsStream(w http.ResponseWriter, r *http.Request, a streamArgs) {
	tier := s.o.Stats.TierFor(a.from, s.o.Clk.Now())
	cat := stats.Catalogue(a.node, a.names)
	w.Header().Set("Content-Type", "application/x-ndjson")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(http.StatusOK)
	rc := http.NewResponseController(w)
	enc := json.NewEncoder(w)
	enc.SetEscapeHTML(false) // SVG text in JSON strings: < and > need no escaping there
	_ = enc.Encode(map[string]any{
		"range":  map[string]any{"from": a.from.Unix(), "to": a.to.Unix(), "live": a.live, "tier": tier.Dir(), "step": int64(tier.Step() / time.Second)},
		"node":   a.node,
		"nodes":  a.nodes,
		"charts": cat,
	})
	_ = rc.Flush()
	rows, _, err := s.o.Stats.Rows(r.Context(), a.from, a.to, a.live)
	if err != nil {
		_ = enc.Encode(streamLine{Error: err.Error()})
		return
	}
	want := map[string]bool{}
	for id := range strings.SplitSeq(a.ids, ",") {
		if id != "" {
			want[id] = true
		}
	}
	drawn := map[string]stats.Chart{}
	for _, c := range stats.Charts(rows, a.from, a.to, a.node) {
		drawn[c.ID] = c
	}
	for _, f := range cat {
		if len(want) > 0 && !want[f.ID] {
			continue
		}
		line := streamLine{ID: f.ID, Empty: true}
		if c, ok := drawn[f.ID]; ok {
			line = streamLine{ID: f.ID, SVG: c.SVG, Caption: c.Caption}
		}
		if err := enc.Encode(line); err != nil {
			return // the browser went away
		}
		_ = rc.Flush()
	}
}
