package webui

import (
	"net"
	"net/http"
	"strconv"
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
	rows, rg, err := s.o.Stats.Rows(r.Context(), fromT, toT, live)
	if err != nil {
		s.fail(w, err)
		return
	}
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
