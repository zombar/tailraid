package admin

import (
	"context"
	"encoding/json"
	"net"
	"net/http"
	"strconv"
)

// Caller is one raw member RPC to a node by address: rpc.Client over the tailnet, or a
// proxy through a sidecar in the soak driver.
type Caller interface {
	Call(ctx context.Context, addr, method, path string, body []byte) ([]byte, error)
}

// FetchStatus asks a node for its status report (GET /v1/status).
func FetchStatus(ctx context.Context, c Caller, addr string) (Report, error) {
	var rep Report
	raw, err := c.Call(ctx, addr, http.MethodGet, "/v1/status", nil)
	if err != nil {
		return rep, err
	}
	err = json.Unmarshal(raw, &rep)
	return rep, err
}

// RPCAddr is the node's host:port for member RPC.
func (n *NodeReport) RPCAddr() string {
	return net.JoinHostPort(n.Addr, strconv.Itoa(int(n.RPCPort)))
}

// LeaderRPC is the RPC address of the leader as this report sees it, or "" without one.
func (r *Report) LeaderRPC() string {
	for i := range r.Nodes {
		if n := &r.Nodes[i]; n.Leader && n.Addr != "" {
			return n.RPCAddr()
		}
	}
	return ""
}
