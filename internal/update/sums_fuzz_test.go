package update

import (
	"strings"
	"testing"
)

// parseSums never panics and maps every "<hex>  <name>" line (with or without the binary
// marker "*", which is dropped once: a name that itself starts with "*" keeps it) to its
// sum.
//
// Invariant: I-61
func FuzzParseSums(f *testing.F) {
	f.Add("abc  tailraid_linux_amd64\n0123 *tailraid_windows_amd64.exe\n\nnot a sums line at all\n")
	f.Add("")
	f.Add("x")
	f.Add("\r\n\t\n")
	f.Add("00 **\n")
	f.Fuzz(func(t *testing.T, s string) {
		out := parseSums(s)
		if len(s) > 60_000 {
			return // the scanner's line limit: a longer line is dropped, which is fine
		}
		for line := range strings.SplitSeq(s, "\n") {
			if fields := strings.Fields(line); len(fields) == 2 {
				name := strings.TrimPrefix(fields[1], "*")
				if _, ok := out[name]; !ok {
					t.Fatalf("line %q not parsed: %v", line, out)
				}
			}
		}
	})
}
