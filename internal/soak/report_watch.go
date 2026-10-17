package soak

import (
	"maps"
	"slices"
	"strings"
)

// queryView is one row of phase 35's query table.
type queryView struct {
	Query, Times string
}

// queryRows lists phase 35's media queries with each gateway's answer time, in run order.
func queryRows(s Summary) []queryView {
	var out []queryView
	for i := range s.Records {
		rec := &s.Records[i]
		if rec.Kind != "event" || rec.Name != watchQuery {
			continue
		}
		var parts []string
		for _, n := range slices.Sorted(maps.Keys(rec.Attrs)) {
			parts = append(parts, n+" "+rec.Attrs[n]+" ms")
		}
		out = append(out, queryView{Query: rec.Text, Times: strings.Join(parts, " · ")})
	}
	return out
}
