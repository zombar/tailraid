package dev

import (
	"errors"
	"net"
	"net/http"
	"sort"
	"sync"
)

// Faults holds the /dev/fault drop rules of one node (docs/12 §3): peers whose traffic
// this node refuses in both directions on both the Raft stream and the node RPC. A nil
// *Faults blocks nothing.
type Faults struct {
	mu   sync.RWMutex
	drop map[string]bool // IP (no port)
}

// NewFaults returns an empty rule set.
func NewFaults() *Faults { return &Faults{drop: map[string]bool{}} }

// Blocked reports whether addr (host:port or bare host) is under a drop rule.
func (f *Faults) Blocked(addr string) bool {
	if f == nil {
		return false
	}
	host := addr
	if h, _, err := net.SplitHostPort(addr); err == nil {
		host = h
	}
	f.mu.RLock()
	defer f.mu.RUnlock()
	return f.drop[host]
}

// Set replaces the rules (an empty list heals).
func (f *Faults) Set(ips []string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.drop = map[string]bool{}
	for _, ip := range ips {
		if ip != "" {
			f.drop[ip] = true
		}
	}
}

// List returns the rules, sorted.
func (f *Faults) List() []string {
	if f == nil {
		return nil
	}
	f.mu.RLock()
	defer f.mu.RUnlock()
	out := make([]string, 0, len(f.drop))
	for ip := range f.drop {
		out = append(out, ip)
	}
	sort.Strings(out)
	return out
}

// ErrDropped is what a blocked outbound request fails with.
var ErrDropped = errors.New("dev: request dropped by a fault rule")

// Transport wraps an RPC transport so requests to blocked peers fail without a dial.
func (f *Faults) Transport(base http.RoundTripper) http.RoundTripper {
	return faultTransport{base: base, f: f}
}

type faultTransport struct {
	base http.RoundTripper
	f    *Faults
}

func (t faultTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	if t.f.Blocked(r.URL.Host) {
		// a refused dial: the request never left, which a proposer may retry (I-95)
		return nil, &net.OpError{Op: "dial", Net: "tcp", Err: ErrDropped}
	}
	return t.base.RoundTrip(r)
}
