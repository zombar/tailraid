package cluster

import (
	"net/http"
	"strings"
	"sync"

	"github.com/zombar/tailraid/internal/meta"
	"github.com/zombar/tailraid/internal/s3"
)

// S3SecretPrefix begins the secrets store's names of S3 node credentials (docs/19 §6).
const S3SecretPrefix = "s3/"

// S3SecretNames are the store's names of S3 node name's credentials,
// `s3/<name>/access_key` and `s3/<name>/secret_key` (docs/19 §6), and false for a node
// name off the secret pattern or holding a slash (its credentials stay in the file).
func S3SecretNames(name string) (access, secret string, ok bool) {
	access, secret = S3SecretPrefix+name+"/access_key", S3SecretPrefix+name+"/secret_key"
	return access, secret, name != "" && !strings.Contains(name, "/") && meta.SecretName.MatchString(secret)
}

// S3KeysFromSecrets pairs the opened secrets under s3/ into credentials by node name; a
// node with only one of its two names has none.
func S3KeysFromSecrets(opened map[string]string) map[string]s3.Credentials {
	out := map[string]s3.Credentials{}
	for name, v := range opened {
		rest, ok := strings.CutPrefix(name, S3SecretPrefix)
		i := strings.LastIndex(rest, "/")
		if !ok || i <= 0 || v == "" {
			continue
		}
		node, field := rest[:i], rest[i+1:]
		c := out[node]
		switch field {
		case "access_key":
			c.AccessKey = v
		case "secret_key":
			c.SecretKey = v
		default:
			continue
		}
		out[node] = c
	}
	for node, c := range out {
		if c.AccessKey == "" || c.SecretKey == "" {
			delete(out, node)
		}
	}
	return out
}

// S3Buckets holds a client for each S3 row this device has credentials for, rebuilt when
// the row or the credentials change. Its Bucket is rpc.Client's S3 routing (docs/10 §4.2).
type S3Buckets struct {
	node *Node
	keys func() map[string]s3.Credentials
	rt   http.RoundTripper

	mu    sync.Mutex
	cache map[meta.NodeIdx]cachedBucket
}

type cachedBucket struct {
	row   meta.Node
	creds s3.Credentials
	b     *s3.Bucket
}

// NewS3Buckets resolves S3 rows of node's table with the credentials keys returns (called
// on every lookup, so it should be cheap: the store's Opened is). rt nil is the default transport.
func NewS3Buckets(node *Node, keys func() map[string]s3.Credentials, rt http.RoundTripper) *S3Buckets {
	return &S3Buckets{node: node, keys: keys, rt: rt, cache: map[meta.NodeIdx]cachedBucket{}}
}

// Bucket returns the client of S3 row idx (nil without credentials) and whether idx is an
// S3 row at all.
func (s *S3Buckets) Bucket(idx meta.NodeIdx) (*s3.Bucket, bool) {
	row, ok := s.node.nodeByIdx(idx)
	if !ok || row.Kind != meta.KindS3 {
		return nil, false
	}
	creds, ok := s.keys()[row.Name]
	if !ok {
		return nil, true
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if c, ok := s.cache[idx]; ok && c.row == row && c.creds == creds {
		return c.b, true
	}
	b, err := s3.New(s3.Target{Endpoint: row.S3Endpoint, Bucket: row.S3Bucket, Prefix: row.S3Prefix, Region: row.S3Region, PathStyle: row.S3PathStyle}, creds, s.rt)
	if err != nil {
		return nil, true // the leader checked the row (I-137); a bad one is unreachable
	}
	s.cache[idx] = cachedBucket{row: row, creds: creds, b: b}
	return b, true
}
