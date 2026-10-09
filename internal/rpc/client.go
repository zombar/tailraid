package rpc

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"strconv"
	"time"

	"github.com/zombar/tailraid/internal/chunk"
	"github.com/zombar/tailraid/internal/meta"
)

// Resolver maps a node index to its RPC address; self reports whether it is this node.
type Resolver func(idx meta.NodeIdx) (addr string, self bool, ok bool)

// Client talks to other nodes. It implements chunk.ShardClient (self traffic goes to
// the local store) and the leader-forwarding and join calls.
type Client struct {
	http    *http.Client
	clusID  string
	resolve Resolver
	local   ShardStore
	grace   time.Duration // age guard for self-targeted deletes (same rule as the fence)
	// added disks (docs/14 §4): diskOf names a target's disk ID (false for disk 0 and
	// other kinds), localDisk opens one of this host's disks (nil when closed)
	diskOf    func(meta.NodeIdx) (string, bool)
	localDisk func(id string) ShardStore
}

// SetDisks wires added-disk routing: the disk ID of a target, and this host's open disks.
func (c *Client) SetDisks(diskOf func(meta.NodeIdx) (string, bool), local func(id string) ShardStore) {
	c.diskOf, c.localDisk = diskOf, local
}

var _ chunk.ShardClient = (*Client)(nil)

// NewClient builds a client; local may be nil when this node stores no shards.
func NewClient(clusterID [16]byte, resolve Resolver, local ShardStore) *Client {
	tr := &http.Transport{
		DialContext:           (&net.Dialer{Timeout: 3 * time.Second}).DialContext,
		MaxIdleConnsPerHost:   32, // a flush fans out up to k+m PUTs per chunk on Config.Workers chunks at once
		IdleConnTimeout:       90 * time.Second,
		ResponseHeaderTimeout: 10 * time.Second,
	}
	// no whole-request timeout on the client: a shard call's deadline scales with its
	// size (callTimeout), the rest get callTimeout(0); ResponseHeaderTimeout bounds a
	// dead peer either way (I-83)
	return &Client{http: &http.Client{Transport: tr}, clusID: hex.EncodeToString(clusterID[:]), resolve: resolve, local: local, grace: 10 * time.Minute}
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

// SetTransport replaces the HTTP transport (tests inject FaultNet).
func (c *Client) SetTransport(rt http.RoundTripper) { c.http.Transport = rt }

// WrapTransport replaces the transport with wrap(current) (dev fault rules).
func (c *Client) WrapTransport(wrap func(http.RoundTripper) http.RoundTripper) {
	c.http.Transport = wrap(c.http.Transport)
}

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
	if body != nil {
		req.ContentLength = int64(len(body))
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("%w: %s %s: %w", ErrUnavailable, method, addr, err) // a cancelled context stays matchable
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
	t := shardTarget{addr: addr, prefix: "/v1/shards/", self: self, store: c.local}
	if c.diskOf != nil {
		if id, isDisk := c.diskOf(node); isDisk {
			t.prefix = "/v1/disks/" + id + "/shards/"
			t.store = nil
			if self && c.localDisk != nil {
				t.store = c.localDisk(id)
			}
			if self && t.store == nil {
				return shardTarget{}, fmt.Errorf("%w: disk %s is not open on this host", ErrUnavailable, id)
			}
		}
	}
	return t, nil
}

func (c *Client) Put(ctx context.Context, node meta.NodeIdx, key chunk.ShardKey, body []byte) error {
	t, err := c.target(node)
	if err != nil {
		return err
	}
	sum := sha256.Sum256(body)
	if t.self && t.store != nil {
		_, err := t.store.PutVerified(key, body, sum)
		return err
	}
	ctx, cancel := bound(ctx, int64(len(body)))
	defer cancel()
	resp, err := c.do(ctx, http.MethodPut, t.addr, t.prefix+string(key), body, map[string]string{HeaderShardSum: hex.EncodeToString(sum[:])})
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return errFromStatus(resp)
	}
	return nil
}

func (c *Client) Get(ctx context.Context, node meta.NodeIdx, key chunk.ShardKey) ([]byte, error) {
	t, err := c.target(node)
	if err != nil {
		return nil, err
	}
	ctx, cancel := bound(ctx, chunk.MaxPayload) // the reply is up to a shard
	defer cancel()
	if t.self && t.store != nil {
		rc, _, err := t.store.Open(key)
		if err != nil {
			return nil, err
		}
		defer rc.Close()
		return io.ReadAll(rc)
	}
	resp, err := c.do(ctx, http.MethodGet, t.addr, t.prefix+string(key), nil, nil)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, errFromStatus(resp)
	}
	if n := resp.ContentLength; n >= 0 && n <= chunk.MaxPayload+chunk.HeaderSize {
		b := make([]byte, n) // sized by Content-Length: one allocation, no growth copies
		_, err := io.ReadFull(resp.Body, b)
		return b, err
	}
	return io.ReadAll(resp.Body)
}

// Delete issues an unfenced local delete for self, and is otherwise DeleteFenced with
// index 0 (which every node has applied) — callers doing GC use DeleteFenced.
func (c *Client) Delete(ctx context.Context, node meta.NodeIdx, key chunk.ShardKey) error {
	return c.DeleteFenced(ctx, node, key, 0)
}

// DeleteFenced deletes a shard on node with the docs/06 fence (GCDelete/SetPlacement index).
func (c *Client) DeleteFenced(ctx context.Context, node meta.NodeIdx, key chunk.ShardKey, index uint64) error {
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
	ctx, cancel := bound(ctx, 0)
	defer cancel()
	resp, err := c.do(ctx, http.MethodDelete, t.addr, t.prefix+string(key), nil, map[string]string{HeaderLogIndex: strconv.FormatUint(index, 10)})
	if err != nil {
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
	t, err := c.target(node)
	if err != nil {
		return false, err
	}
	if t.self && t.store != nil {
		return t.store.Has(key), nil
	}
	ctx, cancel := bound(ctx, 0)
	defer cancel()
	resp, err := c.do(ctx, http.MethodHead, t.addr, t.prefix+string(key), nil, nil)
	if err != nil {
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
	err := c.postJSON(ctx, addr, "/v1/admin", op, &raw, http.StatusOK)
	return raw, err
}
