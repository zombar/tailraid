package cluster

import (
	"errors"
	"fmt"
	"io/fs"
	"maps"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"github.com/goccy/go-yaml"

	"github.com/zombar/tailraid/internal/meta"
	"github.com/zombar/tailraid/internal/s3"
)

// S3KeysFile holds this device's credentials for S3 nodes, keyed by node name (docs/10
// §4.1), from before the secrets store: read for the names the store cannot serve, and
// imported into it and removed (docs/19 §6). It never leaves the device (I-139).
const S3KeysFile = "s3nodes.yaml"

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

// MergeS3Keys is the credentials a node serves its buckets with: the store's pairs over
// its file's for the names the store cannot serve (I-308).
func MergeS3Keys(opened map[string]string, file map[string]s3.Credentials) map[string]s3.Credentials {
	out := maps.Clone(file)
	if out == nil {
		out = map[string]s3.Credentials{}
	}
	maps.Copy(out, S3KeysFromSecrets(opened))
	return out
}

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
// modification time changes (a file restored by hand takes effect at the next
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
