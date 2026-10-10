package cluster

import (
	"errors"
	"fmt"
	"io/fs"
	"net/http"
	"os"
	"path/filepath"
	"sync"

	"github.com/goccy/go-yaml"

	"github.com/zombar/tailraid/internal/meta"
	"github.com/zombar/tailraid/internal/s3"
)

// S3KeysFile holds this device's credentials for S3 nodes, keyed by node name (docs/10
// §4.1). It never leaves the device (I-139).
const S3KeysFile = "s3nodes.yaml"

// LoadS3Keys reads <data_dir>/s3nodes.yaml; a missing file is no keys.
func LoadS3Keys(dataDir string) (map[string]s3.Credentials, error) {
	b, err := os.ReadFile(filepath.Join(dataDir, S3KeysFile)) //nolint:gosec // the keys file in this node's data directory
	if errors.Is(err, fs.ErrNotExist) {
		return map[string]s3.Credentials{}, nil
	}
	if err != nil {
		return nil, err
	}
	keys := map[string]s3.Credentials{}
	if err := yaml.Unmarshal(b, &keys); err != nil {
		return nil, fmt.Errorf("cluster: %s: %w", S3KeysFile, err)
	}
	return keys, nil
}

// SaveS3Key sets the credentials of one S3 node, keeping the others, in a file only this
// user can read, replaced atomically.
func SaveS3Key(dataDir, name string, c s3.Credentials) error {
	if name == "" || c.AccessKey == "" || c.SecretKey == "" {
		return errors.New("cluster: an S3 key needs a node name, an access key and a secret key")
	}
	keys, err := LoadS3Keys(dataDir)
	if err != nil {
		return err
	}
	keys[name] = c
	b, err := yaml.Marshal(keys)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(dataDir, 0o700); err != nil {
		return err
	}
	return writeFileSync(filepath.Join(dataDir, S3KeysFile), b)
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
// on every lookup, so it should be cheap: see KeysFromFile). rt nil is the default transport.
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

// KeysFromFile serves the credentials file of dataDir, read again whenever its size or
// modification time changes (a `node keys` on a running device takes effect at the next
// lookup). A file that does not parse keeps the last good keys and is reported to warn
// once per change.
func KeysFromFile(dataDir string, warn func(error)) func() map[string]s3.Credentials {
	var (
		mu   sync.Mutex
		seen fs.FileInfo
		keys = map[string]s3.Credentials{}
	)
	path := filepath.Join(dataDir, S3KeysFile)
	return func() map[string]s3.Credentials {
		mu.Lock()
		defer mu.Unlock()
		st, err := os.Stat(path)
		if err != nil {
			if seen != nil {
				keys, seen = map[string]s3.Credentials{}, nil // the file was removed
			}
			return keys
		}
		if seen != nil && st.Size() == seen.Size() && st.ModTime().Equal(seen.ModTime()) {
			return keys
		}
		seen = st
		next, err := LoadS3Keys(dataDir)
		if err != nil {
			warn(err)
			return keys
		}
		keys = next
		return keys
	}
}
