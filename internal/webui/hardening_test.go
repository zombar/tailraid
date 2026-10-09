package webui_test

import (
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/zombar/tailraid/internal/admin"
	"github.com/zombar/tailraid/internal/webui"
)

// The UI answers only its own Host values (the listener address and the MagicDNS name
// with the port), the index page forbids framing, and the status JSON never carries the
// join token.
//
// Invariant: I-127
func TestWebUI_HostCheckFramingAndToken(t *testing.T) {
	u := newUI(t, func(o *webui.Options) {
		o.Status = func() any { return admin.Report{Node: "one", JoinToken: "sekret"} }
	})
	// the node's listener would be <tsIP>:port; the test fixture listens on loopback, so
	// the loopback address itself is one of the refused hosts below
	u2 := newUI(t, func(o *webui.Options) {
		o.Hosts = webui.Hosts("100.64.0.9:20482", "node.tail.ts.net")
		o.Status = func() any { return admin.Report{Node: "one", JoinToken: "sekret"} }
	})
	get := func(ui *ui, host string) (int, http.Header, string) {
		t.Helper()
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, ui.hs.URL+"/api/status", http.NoBody)
		must(t, err)
		if host != "" {
			req.Host = host
		}
		resp, err := ui.client.Do(req)
		must(t, err)
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, resp.Header, string(b)
	}
	// without a Hosts list (tests) every Host is accepted; with one, only the listed
	if code, _, _ := get(u, "evil.example"); code != 200 {
		t.Fatalf("no host list: %d", code)
	}
	for _, c := range []struct {
		host string
		code int
	}{
		{"100.64.0.9:20482", 200},
		{"node.tail.ts.net:20482", 200},
		{"NODE.tail.ts.net:20482", 200},
		{"", 421}, // the loopback address the fixture listens on: not in the list
		{"evil.example", 421},
		{"node.tail.ts.net", 421},
		{"100.64.0.9", 421},
	} {
		code, _, body := get(u2, c.host)
		if code != c.code {
			t.Fatalf("Host %q: %d %s, want %d", c.host, code, body, c.code)
		}
		if code == 200 && (strings.Contains(body, "sekret") || !strings.Contains(body, `"node":"one"`)) {
			t.Fatalf("status body with Host %q: %s", c.host, body)
		}
	}
	code, hdr, _ := u.get("/")
	if code != 200 || !strings.Contains(hdr.Get("Content-Security-Policy"), "frame-ancestors 'none'") {
		t.Fatalf("index: %d CSP %q", code, hdr.Get("Content-Security-Policy"))
	}
	if got := webui.Hosts("100.64.0.9:20482", ""); len(got) != 1 || got[0] != "100.64.0.9:20482" {
		t.Fatalf("Hosts without a DNS name: %v", got)
	}
}
