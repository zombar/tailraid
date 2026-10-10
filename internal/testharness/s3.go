package testharness

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"maps"
	"sync"

	"github.com/zombar/tailraid/internal/meta"
	"github.com/zombar/tailraid/internal/rpc"
	"github.com/zombar/tailraid/internal/s3"
	"github.com/zombar/tailraid/internal/s3/s3test"
)

// nodeS3Keys is one node's S3 credentials by node name: its s3nodes.yaml.
type nodeS3Keys struct {
	mu   sync.Mutex
	keys map[string]s3.Credentials
}

func (k *nodeS3Keys) get() map[string]s3.Credentials {
	k.mu.Lock()
	defer k.mu.Unlock()
	return maps.Clone(k.keys)
}

// SetS3Key gives node i the credentials of S3 node name, or takes them away (nil).
func (c *Cluster) SetS3Key(i int, name string, creds *s3.Credentials) {
	k := &c.Nodes[i].s3
	k.mu.Lock()
	defer k.mu.Unlock()
	if k.keys == nil {
		k.keys = map[string]s3.Credentials{}
	}
	if creds == nil {
		delete(k.keys, name)
		return
	}
	k.keys[name] = *creds
}

// AddS3 registers srv under prefix as S3 node name through the leader's add_s3 (docs/10
// §4.1), gives the credentials to the listed nodes (every node when none is listed), and
// probes once so the bucket is live to them. It returns the bucket's target index.
func (c *Cluster) AddS3(srv *s3test.Server, name, prefix string, withKeys ...int) meta.NodeIdx {
	c.T.Helper()
	if len(withKeys) == 0 {
		for i := range c.Nodes {
			withKeys = append(withKeys, i)
		}
	}
	for _, i := range withKeys {
		c.SetS3Key(i, name, &s3test.Creds)
	}
	t := srv.Target(prefix)
	sum := sha256.Sum256([]byte(name))
	id := hex.EncodeToString(sum[:8])
	op := rpc.MemberOp{
		Op: "add_s3", Name: name, Capacity: 1 << 40,
		S3: &rpc.S3Spec{ID: id, Endpoint: t.Endpoint, Bucket: t.Bucket, Prefix: t.Prefix, PathStyle: t.PathStyle},
	}
	if err := c.Leader().Node.MemberOp(context.Background(), op); err != nil {
		c.T.Fatalf("add s3 %s: %v", name, err)
	}
	c.Settle()
	row, ok := c.Leader().Node.FSM.NodeByID(id)
	if !ok {
		c.T.Fatalf("s3 node %s not in the node table", name)
	}
	c.Probe()
	return row.Idx
}
