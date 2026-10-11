package admin

import (
	"strconv"
	"strings"
)

// parseNetDev sums the receive and transmit bytes of every interface in a /proc/net/dev
// listing but loopback, and picks out the interface named tailnet ("" for none). ok is
// false when no interface parsed (I-190: absent, never zero).
func parseNetDev(text, tailnet string) (rx, tx, trx, ttx uint64, ok bool) {
	for line := range strings.SplitSeq(text, "\n") {
		name, rest, found := strings.Cut(line, ":")
		if !found {
			continue
		}
		name = strings.TrimSpace(name)
		f := strings.Fields(rest)
		if name == "" || name == "lo" || len(f) < 9 {
			continue
		}
		in, err1 := strconv.ParseUint(f[0], 10, 64)
		out, err2 := strconv.ParseUint(f[8], 10, 64)
		if err1 != nil || err2 != nil {
			continue
		}
		rx, tx, ok = rx+in, tx+out, true
		if name == tailnet {
			trx, ttx = in, out
		}
	}
	return rx, tx, trx, ttx, ok
}
