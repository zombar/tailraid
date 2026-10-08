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
}

var _ chunk.ShardClient = (*Client)(nil)

// NewClient builds a client; local may be nil when this node stores no shards.
func NewClient(clusterID [16]byte, resolve Resolver, local ShardStore) *Client {
	tr := &http.Transport{
		DialContext:           (&net.Dialer{Timeout: 3 * time.Second}).DialContext,
		MaxIdleConnsPerHost:   8,
		IdleConnTimeout:       90 * time.Second,
		ResponseHeaderTimeout: 10 * time.Second,
	}
	return &Client{http: &http.Client{Transport: tr, Timeout: 60 * time.Second}, clusID: hex.EncodeToString(clusterID[:]), resolve: resolve, local: local, grace: 10 * time.Minute}
}

// SetGrace sets the minimum age of a shard this node will delete locally.
func (c *Client) SetGrace(d time.Duration) { c.grace = d }

// SetTransport replaces the HTTP transport (tests inject FaultNet).
func (c *Client) SetTransport(rt http.RoundTripper) { c.http.Transport = rt }

// WrapTransport replaces the transport with wrap(current) (dev fault rules).
func (c *Client) WrapTransport(wrap func(http.RoundTripper) http.RoundTripper) {
	c.http.Transport = wrap(c.http.Transport)
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
		return nil, fmt.Errorf("%w: %s %s: %v", ErrUnavailable, method, addr, err) //nolint:errorlint // transport error, wrapped by sentinel
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
		return chunk.ErrNotFound
	case http.StatusUnprocessableEntity:
		return chunk.ErrCorrupt
	case http.StatusBadRequest:
		return fmt.Errorf("%w: %s", ErrBadRequest, msg)
	}
	return fmt.Errorf("rpc: unexpected status %d: %s", resp.StatusCode, msg)
}

// --- chunk.ShardClient --------------------------------------------------------------

func (c *Client) target(node meta.NodeIdx) (string, bool, error) {
	addr, self, ok := c.resolve(node)
	if !ok {
		return "", false, fmt.Errorf("%w: node %d has no address", ErrUnavailable, node)
	}
	return addr, self, nil
}

func (c *Client) Put(ctx context.Context, node meta.NodeIdx, key chunk.ShardKey, body []byte) error {
	addr, self, err := c.target(node)
	if err != nil {
		return err
	}
	if self && c.local != nil {
		_, err := c.local.Put(key, body)
		return err
	}
	sum := sha256.Sum256(body)
	resp, err := c.do(ctx, http.MethodPut, addr, "/v1/shards/"+string(key), body, map[string]string{HeaderShardSum: hex.EncodeToString(sum[:])})
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
	addr, self, err := c.target(node)
	if err != nil {
		return nil, err
	}
	if self && c.local != nil {
		rc, _, err := c.local.Open(key)
		if err != nil {
			return nil, err
		}
		defer rc.Close()
		return io.ReadAll(rc)
	}
	resp, err := c.do(ctx, http.MethodGet, addr, "/v1/shards/"+string(key), nil, nil)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, errFromStatus(resp)
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
	addr, self, err := c.target(node)
	if err != nil {
		return err
	}
	if self && c.local != nil {
		// the same age guard as the remote fence (I-14); placement is the caller's check
		_, _, err := c.local.DeleteIfOlder(key, c.grace, time.Now())
		return err
	}
	resp, err := c.do(ctx, http.MethodDelete, addr, "/v1/shards/"+string(key), nil, map[string]string{HeaderLogIndex: strconv.FormatUint(index, 10)})
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
	addr, self, err := c.target(node)
	if err != nil {
		return false, err
	}
	if self && c.local != nil {
		return c.local.Has(key), nil
	}
	resp, err := c.do(ctx, http.MethodHead, addr, "/v1/shards/"+string(key), nil, nil)
	if err != nil {
		return false, err
	}
	defer resp.Body.Close()
	switch resp.StatusCode {
	case http.StatusOK:
		return true, nil
	case http.StatusNotFound:
		return false, nil
	}
	return false, errFromStatus(resp)
}

// --- control calls -----------------------------------------------------------------

// ApplyAt proposes a command through the node at addr (the leader, or a node that
// redirects with 421).
func (c *Client) ApplyAt(ctx context.Context, addr string, cmd meta.Command) (meta.Result, uint64, error) {
	resp, err := c.do(ctx, http.MethodPost, addr, "/v1/apply", meta.EncodeCommand(cmd), map[string]string{"Content-Type": "application/octet-stream"})
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

// Join sends a join request to a node of the cluster.
func (c *Client) Join(ctx context.Context, addr string, req JoinRequest) (JoinResponse, error) {
	body, _ := json.Marshal(req)
	resp, err := c.do(ctx, http.MethodPost, addr, "/v1/join", body, map[string]string{"Content-Type": "application/json"})
	if err != nil {
		return JoinResponse{}, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return JoinResponse{}, errFromStatus(resp)
	}
	var jr JoinResponse
	if err := json.NewDecoder(io.LimitReader(resp.Body, 4<<20)).Decode(&jr); err != nil {
		return JoinResponse{}, fmt.Errorf("%w: bad join reply: %v", ErrBadRequest, err) //nolint:errorlint // transport
	}
	return jr, nil
}

// Members sends a member operation.
func (c *Client) Members(ctx context.Context, addr string, op MemberOp) error {
	body, _ := json.Marshal(op)
	resp, err := c.do(ctx, http.MethodPost, addr, "/v1/members", body, map[string]string{"Content-Type": "application/json"})
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return errFromStatus(resp)
	}
	return nil
}

// Health probes a node.
func (c *Client) Health(ctx context.Context, addr string) (Health, error) {
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
	body, _ := json.Marshal(BadSlot{Key: string(key), Node: node})
	resp, err := c.do(ctx, http.MethodPost, addr, "/v1/bad-slot", body, map[string]string{"Content-Type": "application/json"})
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusAccepted {
		return errFromStatus(resp)
	}
	return nil
}

// Admin sends a maintenance operation and returns the raw JSON reply.
// Call sends an arbitrary member request (status, admin, /dev/*) and returns the body of a
// 2xx reply; other statuses become errors as for every other method.
func (c *Client) Call(ctx context.Context, addr, method, path string, body []byte) ([]byte, error) {
	hdr := map[string]string{}
	if body != nil {
		hdr["Content-Type"] = "application/json"
	}
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
	body, _ := json.Marshal(op)
	resp, err := c.do(ctx, http.MethodPost, addr, "/v1/admin", body, map[string]string{"Content-Type": "application/json"})
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, errFromStatus(resp)
	}
	return io.ReadAll(io.LimitReader(resp.Body, 16<<20))
}
