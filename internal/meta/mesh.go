package meta

import (
	"errors"
	"fmt"
	"net/netip"
	"slices"
	"time"
)

// The mesh's share of the state (docs/20 §3, §4): a device row's key, mesh port and
// endpoint candidates, the cluster subnet with the next host to allocate, and the pending
// rows of joins in progress.

// Bounds of the mesh fields.
const (
	// MaxEndpoints a row publishes (docs/20 §5, I-322).
	MaxEndpoints = 8
	// MaxEndpoint is the longest endpoint string a row carries.
	MaxEndpoint = 255
	// MeshFirstHost and MeshMaxHost bound a /24's host numbers; init's node takes the first.
	MeshFirstHost = 1
	MeshMaxHost   = 254
	// PendingTTL is how long a pending row lives without its join (I-319); a token
	// rotation ends every pending row at once.
	PendingTTL = 10 * time.Minute
	// MaxPending bounds the table a flood of introduces could grow.
	MaxPending = 512
)

// Mesh errors.
var (
	ErrMeshFull   = errors.New("meta: the cluster subnet has no host number left")
	ErrNoSubnet   = errors.New("meta: the cluster has no mesh subnet")
	ErrNoPending  = errors.New("meta: no pending row for that key")
	ErrPendingFul = errors.New("meta: too many pending joins")
)

// MeshKey is a device's WireGuard public key (docs/20 §2).
type MeshKey [32]byte

// IsZero reports an unset key.
func (k MeshKey) IsZero() bool { return k == MeshKey{} }

// Pending is a join in progress (docs/20 §4): the key the introduce carried and the
// address the leader gave it. The join turns it into the member row; PendingDelete
// removes one the join never followed.
type Pending struct {
	Key  MeshKey
	Addr string // the mesh address, canonical text
	At   int64  // unix nanos of the introduce
}

// PendingAdd allocates an address for a key, or answers the one it has (op 26).
type PendingAdd struct {
	Key MeshKey
	Now int64
}

// PendingDelete removes a pending row (op 27): the leader's expiry.
type PendingDelete struct {
	Key MeshKey
	Now int64
}

// MeshSubnet is the cluster's subnet as the FSM holds it, or the zero prefix.
func (m Meta) MeshPrefix() netip.Prefix {
	p, err := netip.ParsePrefix(m.MeshSubnet)
	if err != nil {
		return netip.Prefix{}
	}
	return p
}

// pendingAdd answers a key with its address: the active device row that holds the key,
// the pending row it already has, or a fresh pending row at the lowest free host
// (I-319, I-320). A host is free when no device row but a removed one and no pending row
// holds it: a pending row that expired or was deleted frees its host, a member's address
// is retired for good (docs/20 §3). The address comes back in Result.Addr.
func (a *applier) pendingAdd(c *PendingAdd) (Result, error) {
	if c == nil || c.Key.IsZero() {
		return Result{}, ErrBadCommand
	}
	st := a.st
	for _, n := range st.Nodes {
		if n.Kind == KindDevice && n.State != Removed && n.Key == c.Key && n.Addr != "" {
			return Result{Addr: n.Addr}, nil
		}
	}
	if p, ok := st.Pending[c.Key]; ok {
		return Result{Addr: p.Addr}, nil
	}
	sub := st.Meta.MeshPrefix()
	if !sub.IsValid() {
		return Result{}, ErrNoSubnet
	}
	if len(st.Pending) >= MaxPending {
		return Result{}, ErrPendingFul
	}
	host, ok := st.freeMeshHost(sub)
	if !ok {
		return Result{}, ErrMeshFull
	}
	b := sub.Masked().Addr().As4()
	b[3] = host
	addr := netip.AddrFrom4(b).String()
	if host >= st.Meta.NextMeshHost {
		st.Meta.NextMeshHost = host + 1 // the high-water mark status warns on
	}
	if st.Pending == nil {
		st.Pending = map[MeshKey]*Pending{}
	}
	st.Pending[c.Key] = &Pending{Key: c.Key, Addr: addr, At: a.clock(c.Now)}
	return Result{Addr: addr}, nil
}

// freeMeshHost is the lowest host of sub above init's no device row (removed ones
// included: a member's address is never reused) and no pending row holds; false when
// none is left.
func (st *State) freeMeshHost(sub netip.Prefix) (uint8, bool) {
	var used [MeshMaxHost + 1]bool
	mark := func(addr string) {
		a, err := netip.ParseAddr(addr)
		if err != nil || !sub.Contains(a) {
			return
		}
		used[a.As4()[3]] = true
	}
	for _, n := range st.Nodes {
		if n.Kind == KindDevice {
			mark(n.Addr)
		}
	}
	for _, p := range st.Pending {
		mark(p.Addr)
	}
	for h := MeshFirstHost + 1; h <= MeshMaxHost; h++ { // .1 is init's node (docs/20 §3)
		if !used[h] {
			return uint8(h), true
		}
	}
	return 0, false
}

func (a *applier) pendingDelete(c *PendingDelete) error {
	if c == nil || c.Key.IsZero() {
		return ErrBadCommand
	}
	if _, ok := a.st.Pending[c.Key]; !ok {
		return ErrNoPending
	}
	delete(a.st.Pending, c.Key)
	return nil
}

// consumePending drops the pending row a device row's upsert completes.
func (st *State) consumePending(n *Node) {
	if n.Kind != KindDevice || n.Key.IsZero() {
		return
	}
	delete(st.Pending, n.Key)
}

// checkMeshFields refuses a row whose mesh fields are out of bounds (I-322).
func checkMeshFields(n *Node) error {
	if len(n.Endpoints) > MaxEndpoints {
		return fmt.Errorf("%w: %d endpoints", ErrBadCommand, len(n.Endpoints))
	}
	for _, e := range n.Endpoints {
		if e == "" || len(e) > MaxEndpoint {
			return fmt.Errorf("%w: endpoint %q", ErrBadCommand, e)
		}
	}
	return nil
}

// hasMeshFields reports whether a row needs codec 13 to be written.
func hasMeshFields(n *Node) bool {
	return !n.Key.IsZero() || len(n.Endpoints) > 0 || n.MeshPort != 0
}

func writeMeshFields(w *writer, n *Node) {
	w.b = append(w.b, n.Key[:]...)
	w.u16(n.MeshPort)
	w.uvarint(uint64(len(n.Endpoints)))
	for _, e := range n.Endpoints {
		w.str(e)
	}
}

func readMeshFields(r *reader, n *Node) {
	copy(n.Key[:], r.take(len(n.Key)))
	n.MeshPort = r.u16()
	if cnt := r.length(MaxEndpoints); cnt > 0 {
		n.Endpoints = make([]string, cnt)
		for i := range n.Endpoints {
			n.Endpoints[i] = r.str()
		}
	}
}

func writePending(w *writer, st *State) {
	keys := make([]MeshKey, 0, len(st.Pending))
	for k := range st.Pending {
		keys = append(keys, k)
	}
	slices.SortFunc(keys, func(a, b MeshKey) int { return slices.Compare(a[:], b[:]) })
	w.uvarint(uint64(len(keys)))
	for _, k := range keys {
		p := st.Pending[k]
		w.b = append(w.b, p.Key[:]...)
		w.str(p.Addr)
		w.i64(p.At)
	}
}

func readPending(r *reader, st *State) {
	n := r.length(MaxPending)
	var prev MeshKey
	for i := 0; i < n && r.err == nil; i++ {
		var p Pending
		copy(p.Key[:], r.take(len(p.Key)))
		p.Addr = r.str()
		p.At = r.i64()
		if r.err == nil && i > 0 && slices.Compare(p.Key[:], prev[:]) <= 0 {
			r.fail(errors.New("meta: pending rows not in canonical order"))
			return
		}
		prev = p.Key
		st.Pending[p.Key] = &p
	}
}

// Pending lists the joins in progress, oldest first.
func (f *FSM) Pending() []Pending {
	f.mu.RLock()
	defer f.mu.RUnlock()
	out := make([]Pending, 0, len(f.st.Pending))
	for _, p := range f.st.Pending {
		out = append(out, *p)
	}
	slices.SortFunc(out, func(a, b Pending) int {
		if a.At != b.At {
			return int(a.At - b.At)
		}
		return slices.Compare(a.Key[:], b.Key[:])
	})
	return out
}

// PendingByAddr is the pending row at a mesh address, if any (the join's authorizer).
func (f *FSM) PendingByAddr(addr string) (Pending, bool) {
	f.mu.RLock()
	defer f.mu.RUnlock()
	for _, p := range f.st.Pending {
		if p.Addr == addr {
			return *p, true
		}
	}
	return Pending{}, false
}

// PendingByKey is the pending row of a key, if any.
func (f *FSM) PendingByKey(key MeshKey) (Pending, bool) {
	f.mu.RLock()
	defer f.mu.RUnlock()
	p, ok := f.st.Pending[key]
	if !ok {
		return Pending{}, false
	}
	return *p, true
}

// NodeByKey is the device row that holds a mesh key, removed rows excluded.
func (f *FSM) NodeByKey(key MeshKey) (Node, bool) {
	if key.IsZero() {
		return Node{}, false
	}
	return f.findNode(func(n *Node) bool { return n.Kind == KindDevice && n.State != Removed && n.Key == key })
}
