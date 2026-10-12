package dev

import (
	"errors"
	"net"
	"net/http"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/zombar/tailraid/internal/meta"
)

// Faults holds the /dev/fault drop rules of one node (docs/12 §3): peers whose traffic
// this node refuses in both directions on both the Raft stream and the node RPC. A nil
// *Faults blocks nothing.
type Faults struct {
	mu   sync.RWMutex
	drop map[string]bool // IP (no port)

	// View, when set, is this node's prober: /dev/codec makes it believe a peer reads an
	// older codec than it reports, the leader-only view of a rolling upgrade (soak phase
	// 22, docs/09 §6)
	View CodecViewer
	// putDelay holds every outbound shard PUT this long first, so the flusher falls
	// behind its clients and the caps are met (soak phase 22)
	putDelay atomic.Int64
}

// CodecViewer is a prober whose view of a peer's codec can be overridden.
type CodecViewer interface {
	SetCodecView(idx meta.NodeIdx, v uint8)
}

// SetPutDelay holds every outbound shard PUT for d first (0: none).
func (f *Faults) SetPutDelay(d time.Duration) { f.putDelay.Store(int64(d)) }

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
	if d := time.Duration(t.f.putDelay.Load()); d > 0 && r.Method == http.MethodPut && strings.Contains(r.URL.Path, "/shards/") {
		timer := time.NewTimer(d)
		select {
		case <-timer.C:
		case <-r.Context().Done():
			timer.Stop()
			return nil, r.Context().Err()
		}
	}
	return t.base.RoundTrip(r)
}
