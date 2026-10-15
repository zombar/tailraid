package rpc

import (
	"errors"
	"testing"
	"time"
)

// Invariant: I-68
func TestWhoIs_User_CachesLogin(t *testing.T) {
	e := newEnv(t)
	e.ts.SetUser(memberIP, "jim@example.com")
	id, login, err := e.auth.User(ctx, memberIP)
	if err != nil || id != "self-1" || login != "jim@example.com" {
		t.Fatalf("User = %q %q %v", id, login, err)
	}
	// the login is cached with the StableID: a change is not seen until a purge
	e.ts.SetUser(memberIP, "other@example.com")
	if _, login, _ := e.auth.User(ctx, memberIP); login != "jim@example.com" {
		t.Fatalf("cached login changed without a purge: %q", login)
	}
	e.auth.Purge()
	if _, login, _ := e.auth.User(ctx, memberIP); login != "other@example.com" {
		t.Fatalf("after purge: %q", login)
	}
	// a tagged device has no login
	e.ts.SetUser(memberIP, "tagged-devices", "tag:server")
	e.auth.Purge()
	if _, login, err := e.auth.User(ctx, memberIP); err != nil || login != "" {
		t.Fatalf("tagged: %q %v", login, err)
	}
	// not on the tailnet → forbidden, negative-cached; WhoIs down → unavailable
	e.ts.Remove(memberIP)
	e.auth.Purge()
	if _, _, err := e.auth.User(ctx, memberIP); !errors.Is(err, ErrForbidden) {
		t.Fatalf("off tailnet: %v", err)
	}
	e.ts.Add(memberIP, "self-1", "self")
	if _, _, err := e.auth.User(ctx, memberIP); !errors.Is(err, ErrForbidden) {
		t.Fatal("negative cache not honoured")
	}
	e.clk.Advance(time.Minute)
	if _, login, err := e.auth.User(ctx, memberIP); err != nil || login != "" {
		t.Fatalf("after negative TTL: %q %v", login, err)
	}
	e.ts.Err = errors.New("tailscaled down")
	e.auth.Purge()
	if _, _, err := e.auth.User(ctx, memberIP); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("whois down: %v", err)
	}
	// AllowAll (loopback/dev) answers its node with no login
	if id, login, err := (AllowAll{Node: e.app.fsm.Nodes()[0]}).User(ctx, memberIP); err != nil || id != "self-1" || login != "" {
		t.Fatalf("AllowAll.User = %q %q %v", id, login, err)
	}
}
