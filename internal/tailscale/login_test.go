package tailscale

import (
	"errors"
	"net/netip"
	"testing"
)

// Invariant: I-68
func TestWhoIs_Login_TaggedHasNone(t *testing.T) {
	cases := []struct {
		name  string
		login string
		tags  []string
		want  string
	}{
		{"user device", "jim@example.com", nil, "jim@example.com"},
		{"tagged device reports the pseudo-login", "tagged-devices", []string{"tag:server"}, ""},
		{"tagged device with a real-looking login", "jim@example.com", []string{"tag:nas"}, ""},
		{"no login", "", nil, ""},
	}
	for _, c := range cases {
		var w WhoIs
		w.UserProfile.LoginName = c.login
		w.Node.Tags = c.tags
		if got := w.Login(); got != c.want {
			t.Errorf("%s: Login() = %q, want %q", c.name, got, c.want)
		}
	}
	// SetUser round-trips through WhoIs, tags included
	self := netip.MustParseAddr("100.64.0.1")
	phone := netip.MustParseAddr("100.64.0.7")
	f := NewFake(self, "1", map[netip.Addr]string{phone: "7"})
	f.SetUser(phone, "jim@example.com")
	if w, err := f.WhoIs(ctx, phone); err != nil || w.Login() != "jim@example.com" {
		t.Fatalf("after SetUser: %+v %v", w, err)
	}
	f.SetUser(phone, "tagged-devices", "tag:server")
	if w, err := f.WhoIs(ctx, phone); err != nil || w.Login() != "" || len(w.Node.Tags) != 1 {
		t.Fatalf("after tagged SetUser: %+v %v", w, err)
	}
	f.SetUser(netip.MustParseAddr("100.64.0.99"), "nobody") // unknown address: no-op
	if _, err := f.WhoIs(ctx, netip.MustParseAddr("100.64.0.99")); !errors.Is(err, ErrNotFound) {
		t.Fatal("SetUser on an unknown address must not create a node")
	}
}
