package rpc

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/zombar/tailraid/internal/chunk"
	"github.com/zombar/tailraid/internal/clock"
	"github.com/zombar/tailraid/internal/meta"
	"github.com/zombar/tailraid/internal/s3"
)

// Resolver maps a node index to its RPC address; self reports whether it is this node.
type Resolver func(idx meta.NodeIdx) (addr string, self bool, ok bool)

// Client talks to other nodes. It implements chunk.ShardClient (traffic to this host's
// own disks goes straight to their stores) and the leader-forwarding and join calls.
type Client struct {
	http    *http.Client // shard calls: size-scaled deadlines and a header timeout (I-83)
	ctl     *http.Client // the control plane: no header timeout, a pass answers when done (I-189)
	clusID  string
	resolve Resolver
	grace   time.Duration // age guard for self-targeted deletes (same rule as the fence)
	// disks (docs/14 §4): diskOf names a target's disk ID (false for any other row),
	// localDisk opens one of this host's disks (nil when closed)
	diskOf    func(meta.NodeIdx) (string, bool)
	localDisk func(id string) ShardStore
	s3Of      func(meta.NodeIdx) (*s3.Bucket, bool) // S3 targets (docs/10 §4.2)
	clk       clock.Clock                           // the GET body deadline's timer (I-171)
	// OnUnreachable, when set, is called with the node of a shard RPC that failed at the
	// transport level (dial, header timeout, the call's own deadline), never for an HTTP
	// status reply or a request the caller cancelled (docs/05 §6, I-170). Must not block.
	OnUnreachable func(meta.NodeIdx)
}

// errTransport marks a request that got no HTTP reply at all.
var errTransport = errors.New("rpc: no reply")

// SetClock sets the clock the GET body deadline runs on (tests use a fake).
func (c *Client) SetClock(clk clock.Clock) { c.clk = clk }

// unreachable reports a transport-level failure of a shard RPC to node, unless the
// caller itself cancelled the request (parent is its context before the call's own
// deadline was added).
func (c *Client) unreachable(parent context.Context, node meta.NodeIdx, err error) {
	if c.OnUnreachable != nil && errors.Is(err, errTransport) && parent.Err() == nil {
		c.OnUnreachable(node)
	}
}

// SetDisks wires disk routing: the disk ID of a target, and this host's open disks.
func (c *Client) SetDisks(diskOf func(meta.NodeIdx) (string, bool), local func(id string) ShardStore) {
	c.diskOf, c.localDisk = diskOf, local
}

var _ chunk.ShardClient = (*Client)(nil)

// NewClient builds a client; resolve may be nil for a client that only joins or probes.
func NewClient(clusterID [16]byte, resolve Resolver) *Client {
	dial := (&net.Dialer{Timeout: 3 * time.Second}).DialContext
	tr := &http.Transport{
		DialContext:           dial,
		MaxIdleConnsPerHost:   32, // a flush fans out up to k+m PUTs per chunk on Config.Workers chunks at once
		IdleConnTimeout:       90 * time.Second,
		ResponseHeaderTimeout: 10 * time.Second,
	}
	// no whole-request timeout on the client: a shard call's deadline scales with its
	// size (callTimeout), the rest get callTimeout(0); ResponseHeaderTimeout bounds a
	// dead peer either way (I-83). The control plane has no header timeout: an admin op
	// that runs a pass is answered when the pass is done, and the dial timeout plus the
	// caller's context bound a dead peer (I-189).
	ctl := &http.Transport{DialContext: dial, MaxIdleConnsPerHost: 4, IdleConnTimeout: 90 * time.Second}
	return &Client{http: &http.Client{Transport: tr}, ctl: &http.Client{Transport: ctl}, clusID: hex.EncodeToString(clusterID[:]), resolve: resolve, grace: 10 * time.Minute, clk: clock.Real()}
}

// transferRate is the slowest tailnet path a shard transfer is given: a DERP relay at
// 256 KiB/s moves a 64 MiB shard in about four minutes.
const transferRate = 256 << 10

// callTimeout is the deadline of one RPC that carries n body bytes: a fixed allowance
// plus the time to move n bytes at transferRate (docs/04 §4 step 9, I-83).
func callTimeout(n int64) time.Duration {
	return 10*time.Second + time.Duration(n)*time.Second/transferRate
}

// SetGrace sets the minimum age of a shard this node will delete locally.
func (c *Client) SetGrace(d time.Duration) { c.grace = d }

// SetTransport replaces both HTTP transports (tests inject FaultNet).
func (c *Client) SetTransport(rt http.RoundTripper) { c.http.Transport, c.ctl.Transport = rt, rt }

// WrapTransport replaces each transport with wrap(current) (dev fault rules).
func (c *Client) WrapTransport(wrap func(http.RoundTripper) http.RoundTripper) {
	c.http.Transport = wrap(c.http.Transport)
	c.ctl.Transport = wrap(c.ctl.Transport)
}

// shardPrefix opens every shard path (shardTarget.prefix) and mirrorPrefix every mirror
// object or tree path; both go on the transport whose response headers are bounded, so a
// silent host fails the call within ResponseHeaderTimeout. Other paths are the control
// plane.
const (
	shardPrefix  = "/v1/disks/"
	mirrorPrefix = "/v1/mirrors/"
)

// bound gives a call without a deadline one of callTimeout(n); the caller cancels it once
// the reply, body included, has been consumed.
func bound(ctx context.Context, n int64) (context.Context, context.CancelFunc) {
	if _, has := ctx.Deadline(); has {
		return ctx, func() {}
	}
	return context.WithTimeout(ctx, callTimeout(n))
}

func (c *Client) do(ctx context.Context, method, addr, path string, body []byte, hdr map[string]string) (*http.Response, error) {
	var rd io.Reader
	if body != nil {
		rd = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, "http://"+addr+path, rd)
	if err != nil {
		return nil, err
	}
	req.Header.Set(HeaderCluster, c.clusID)
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	if chunk.IsBackground(ctx) {
		req.Header.Set(HeaderPriority, PriorityBackground) // the repairer's work yields slots (I-175)
	}
	if body != nil {
		req.ContentLength = int64(len(body))
	}
	hc := c.ctl
	if strings.HasPrefix(path, shardPrefix) || strings.HasPrefix(path, mirrorPrefix) {
		hc = c.http // a shard or a mirror's object or tree: response headers bounded (I-83, I-241)
	}
	resp, err := hc.Do(req)
	if err != nil {
		return nil, fmt.Errorf("%w (%w): %s %s: %w", ErrUnavailable, errTransport, method, addr, err) // a cancelled context stays matchable
	}
	return resp, nil
}

func errFromStatus(resp *http.Response) error {
	var eb ErrorBody
	_ = json.NewDecoder(io.LimitReader(resp.Body, 4<<10)).Decode(&eb)
	msg := eb.Error
	switch resp.StatusCode {
	case http.StatusForbidden:
		return fmt.Errorf("%w: %s", ErrForbidden, msg)
	case http.StatusTooManyRequests:
		return fmt.Errorf("%w: %s", ErrRateLimited, msg)
	case http.StatusConflict:
		return fmt.Errorf("%w: %s", ErrWrongCluster, msg)
	case http.StatusMisdirectedRequest:
		return fmt.Errorf("%w (leader %s)", ErrNotLeader, resp.Header.Get(HeaderLeader))
	case http.StatusServiceUnavailable:
		if resp.Header.Get(HeaderBusy) != "" { // slots full, not a leader question (I-246)
			return fmt.Errorf("%w: %w: %s", ErrUnavailable, ErrBusy, msg)
		}
		return fmt.Errorf("%w: %s", ErrNoLeader, msg)
	case http.StatusPreconditionFailed:
		return fmt.Errorf("%w: %s", ErrFenced, msg)
	case http.StatusRequestEntityTooLarge:
		return ErrTooLarge
	case http.StatusInsufficientStorage:
		return chunk.ErrFull
	case http.StatusNotFound:
		if resp.Header.Get(HeaderDisk) != "" {
			return fmt.Errorf("%w: %s", ErrUnavailable, msg) // the disk is closed, not the shard absent (I-90)
		}
		return chunk.ErrNotFound
	case http.StatusUnprocessableEntity:
		return chunk.ErrCorrupt
	case http.StatusBadRequest:
		return fmt.Errorf("%w: %s", ErrBadRequest, msg)
	case http.StatusInternalServerError:
		return fmt.Errorf("%w: %s", ErrApplyFailed, msg)
	}
	return fmt.Errorf("rpc: unexpected status %d: %s", resp.StatusCode, msg)
}

// --- chunk.ShardClient --------------------------------------------------------------

// shardTarget is where one target's shards live: the host's address, the URL prefix of
// the target's shard endpoint, and, on this host, its store (nil when it is closed).
type shardTarget struct {
	addr, prefix string
	self         bool
	store        ShardStore
}

func (c *Client) target(node meta.NodeIdx) (shardTarget, error) {
	addr, self, ok := c.resolve(node)
	if !ok {
		return shardTarget{}, fmt.Errorf("%w: node %d has no address", ErrUnavailable, node)
	}
	// only a disk holds shards (docs/14 §9): a device or unknown index is unavailable,
	// never "absent", so a caller counts it as a slot it cannot reach
	var id string
	if c.diskOf != nil {
		var isDisk bool
		if id, isDisk = c.diskOf(node); !isDisk {
			return shardTarget{}, fmt.Errorf("%w: node %d is not a placement target", ErrUnavailable, node)
		}
	} else {
		return shardTarget{}, fmt.Errorf("%w: node %d is not a placement target", ErrUnavailable, node)
	}
	t := shardTarget{addr: addr, prefix: "/v1/disks/" + id + "/shards/", self: self}
	if self {
		if c.localDisk != nil {
			t.store = c.localDisk(id)
		}
		if t.store == nil {
			return shardTarget{}, fmt.Errorf("%w: disk %s is not open on this host", ErrUnavailable, id)
		}
	}
	return t, nil
}

func (c *Client) Put(ctx context.Context, node meta.NodeIdx, key chunk.ShardKey, body []byte) error {
	if b, isS3, err := c.bucket(node); isS3 {
		if err != nil {
			return err
		}
		return s3Put(ctx, b, key, body)
	}
	t, err := c.target(node)
	if err != nil {
		return err
	}
	sum := sha256.Sum256(body)
	if t.self && t.store != nil {
		_, err := t.store.PutVerified(key, body, sum)
		return err
	}
	parent := ctx
	ctx, cancel := bound(ctx, int64(len(body)))
	defer cancel()
	resp, err := c.do(ctx, http.MethodPut, t.addr, t.prefix+string(key), body, map[string]string{HeaderShardSum: hex.EncodeToString(sum[:])})
	if err != nil {
		c.unreachable(parent, node, err)
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return errFromStatus(resp)
	}
	return nil
}

func (c *Client) Get(ctx context.Context, node meta.NodeIdx, key chunk.ShardKey) ([]byte, error) {
	if b, isS3, err := c.bucket(node); isS3 {
		if err != nil {
			return nil, err
		}
		return s3Get(ctx, b, key)
	}
	t, err := c.target(node)
	if err != nil {
		return nil, err
	}
	if t.self && t.store != nil {
		return t.store.Get(key) // the verified payload, no second copy
	}
	// The headers are bounded by the dial and response-header timeouts; the body by
	// callTimeout of its own announced length, never the MaxPayload allowance (I-171).
	parent := ctx
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	resp, err := c.do(ctx, http.MethodGet, t.addr, t.prefix+string(key), nil, nil)
	if err != nil {
		c.unreachable(parent, node, err)
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, errFromStatus(resp)
	}
	n := resp.ContentLength
	switch {
	case n < 0:
		return nil, fmt.Errorf("%w: reply without a length", ErrUnavailable)
	case n > chunk.MaxPayload+chunk.HeaderSize:
		return nil, ErrTooLarge // refused before any of it is read
	}
	t2 := c.clk.NewTimer(callTimeout(n))
	defer t2.Stop()
	done := make(chan struct{})
	defer close(done)
	go func() {
		select {
		case <-t2.C():
			cancel()
		case <-done:
		}
	}()
	b := make([]byte, n) // sized by Content-Length: one allocation, no growth copies
	if _, err := io.ReadFull(resp.Body, b); err != nil {
		c.unreachable(parent, node, fmt.Errorf("%w: body: %w", errTransport, err))
		return nil, fmt.Errorf("%w: GET %s body: %w", ErrUnavailable, t.addr, err)
	}
	return b, nil
}

// Delete issues an unfenced local delete for self, and is otherwise DeleteFenced with
// index 0 (which every node has applied) — callers doing GC use DeleteFenced.
func (c *Client) Delete(ctx context.Context, node meta.NodeIdx, key chunk.ShardKey) error {
	return c.DeleteFenced(ctx, node, key, 0)
}

// DeleteFenced deletes a shard on node with the docs/06 fence (GCDelete/SetPlacement index).
func (c *Client) DeleteFenced(ctx context.Context, node meta.NodeIdx, key chunk.ShardKey, index uint64) error {
	if _, isS3, _ := c.bucket(node); isS3 {
		return errS3Delete // no request: the S3 sweep owns deletes on a bucket (I-138)
	}
	t, err := c.target(node)
	if err != nil {
		return err
	}
	if t.self && t.store != nil {
		// the same age guard as the remote fence (I-14); placement is the caller's check
		deleted, _, err := t.store.DeleteIfOlder(key, c.grace, time.Now())
		if err == nil && !deleted && t.store.Has(key) {
			return fmt.Errorf("%w: shard younger than the grace period", ErrFenced) // counted as refused, as a remote 412 is
		}
		return err
	}
	parent := ctx
	ctx, cancel := bound(ctx, 0)
	defer cancel()
	resp, err := c.do(ctx, http.MethodDelete, t.addr, t.prefix+string(key), nil, map[string]string{HeaderLogIndex: strconv.FormatUint(index, 10)})
	if err != nil {
		c.unreachable(parent, node, err)
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return errFromStatus(resp)
	}
	return nil
}

// Has checks shard presence on a node.
func (c *Client) Has(ctx context.Context, node meta.NodeIdx, key chunk.ShardKey) (bool, error) {
	if b, isS3, err := c.bucket(node); isS3 {
		if err != nil {
			return false, err
		}
		return s3Has(ctx, b, key)
	}
	t, err := c.target(node)
	if err != nil {
		return false, err
	}
	if t.self && t.store != nil {
		return t.store.Has(key), nil
	}
	parent := ctx
	ctx, cancel := bound(ctx, 0)
	defer cancel()
	resp, err := c.do(ctx, http.MethodHead, t.addr, t.prefix+string(key), nil, nil)
	if err != nil {
		c.unreachable(parent, node, err)
		return false, err
	}
	defer resp.Body.Close()
	switch resp.StatusCode {
	case http.StatusOK:
		return true, nil
	case http.StatusNotFound:
		if resp.Header.Get(HeaderDisk) != "" {
			return false, fmt.Errorf("%w: disk closed on its host", ErrUnavailable)
		}
		return false, nil
	}
	return false, errFromStatus(resp)
}

// --- control calls -----------------------------------------------------------------

// ApplyAt proposes a command through the node at addr (the leader, or a node that
// redirects with 421).
func (c *Client) ApplyAt(ctx context.Context, addr string, cmd meta.Command) (meta.Result, uint64, error) {
	cmdBytes, err := meta.EncodeCommand(cmd)
	if err != nil {
		return meta.Result{}, 0, err
	}
	ctx, cancel := bound(ctx, int64(len(cmdBytes)))
	defer cancel()
	resp, err := c.do(ctx, http.MethodPost, addr, "/v1/apply", cmdBytes, map[string]string{"Content-Type": "application/octet-stream"})
	if err != nil {
		return meta.Result{}, 0, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return meta.Result{}, 0, errFromStatus(resp)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return meta.Result{}, 0, fmt.Errorf("%w: read apply reply: %v", ErrUnavailable, err) //nolint:errorlint // transport
	}
	res, err := meta.DecodeResult(body)
	if err != nil {
		return meta.Result{}, 0, fmt.Errorf("%w: bad apply reply: %v", ErrBadRequest, err) //nolint:errorlint // transport
	}
	index, _ := strconv.ParseUint(resp.Header.Get(HeaderIndex), 10, 64)
	return res, index, nil
}

// LeaderFromErr extracts the leader address from an ErrNotLeader produced by ApplyAt.
func LeaderFromErr(err error) string {
	if err == nil {
		return ""
	}
	s := err.Error()
	const tag = "(leader "
	i := bytes.Index([]byte(s), []byte(tag))
	if i < 0 {
		return ""
	}
	rest := s[i+len(tag):]
	if j := bytes.IndexByte([]byte(rest), ')'); j >= 0 {
		return rest[:j]
	}
	return ""
}

// postJSON sends in as JSON and decodes a reply with the expected status into out (nil:
// the body is ignored). Every other status becomes the sentinel errFromStatus maps it to.
func (c *Client) postJSON(ctx context.Context, addr, path string, in, out any, want int) error {
	body, _ := json.Marshal(in)
	ctx, cancel := bound(ctx, int64(len(body)))
	defer cancel()
	return c.postJSONRaw(ctx, addr, path, body, out, want)
}

// postJSONRaw is postJSON under the caller's context alone (a pass op, I-189).
func (c *Client) postJSONRaw(ctx context.Context, addr, path string, body []byte, out any, want int) error {
	resp, err := c.do(ctx, http.MethodPost, addr, path, body, map[string]string{"Content-Type": "application/json"})
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != want {
		return errFromStatus(resp)
	}
	if out == nil {
		return nil
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 4<<20)).Decode(out); err != nil {
		return fmt.Errorf("%w: bad %s reply: %v", ErrBadRequest, path, err) //nolint:errorlint // transport
	}
	return nil
}

// Join sends a join request to a node of the cluster.
func (c *Client) Join(ctx context.Context, addr string, req JoinRequest) (JoinResponse, error) {
	var jr JoinResponse
	err := c.postJSON(ctx, addr, "/v1/join", req, &jr, http.StatusOK)
	return jr, err
}

// Members sends a member operation.
func (c *Client) Members(ctx context.Context, addr string, op MemberOp) error {
	return c.postJSON(ctx, addr, "/v1/members", op, nil, http.StatusOK)
}

// Health probes a node.
func (c *Client) Health(ctx context.Context, addr string) (Health, error) {
	ctx, cancel := bound(ctx, 0)
	defer cancel()
	resp, err := c.do(ctx, http.MethodGet, addr, "/health", nil, nil)
	if err != nil {
		return Health{}, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return Health{}, errFromStatus(resp)
	}
	var h Health
	if err := json.NewDecoder(io.LimitReader(resp.Body, 64<<10)).Decode(&h); err != nil {
		return Health{}, fmt.Errorf("%w: bad health: %v", ErrBadRequest, err) //nolint:errorlint // transport
	}
	return h, nil
}

// ReportBadSlot tells the leader a slot was unreadable.
func (c *Client) ReportBadSlot(ctx context.Context, addr string, key chunk.ShardKey, node meta.NodeIdx) error {
	return c.postJSON(ctx, addr, "/v1/bad-slot", BadSlot{Key: string(key), Node: node}, nil, http.StatusAccepted)
}

// Admin sends a maintenance operation and returns the raw JSON reply.
// Call sends an arbitrary member request (status, admin, /dev/*) and returns the body of a
// 2xx reply; other statuses become errors as for every other method.
func (c *Client) Call(ctx context.Context, addr, method, path string, body []byte) ([]byte, error) {
	hdr := map[string]string{}
	if body != nil {
		hdr["Content-Type"] = "application/json"
	}
	ctx, cancel := bound(ctx, int64(len(body))+16<<20) // replies up to the 16 MiB read limit
	defer cancel()
	resp, err := c.do(ctx, method, addr, path, body, hdr)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		return nil, errFromStatus(resp)
	}
	return io.ReadAll(io.LimitReader(resp.Body, 16<<20))
}

func (c *Client) Admin(ctx context.Context, addr string, op AdminOp) (json.RawMessage, error) {
	var raw json.RawMessage
	if op.IsPass() { // answered when the pass is done: the caller's context bounds it (I-189)
		body, _ := json.Marshal(op)
		err := c.postJSONRaw(ctx, addr, "/v1/admin", body, &raw, http.StatusOK)
		return raw, err
	}
	err := c.postJSON(ctx, addr, "/v1/admin", op, &raw, http.StatusOK)
	return raw, err
}
