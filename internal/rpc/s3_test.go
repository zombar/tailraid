package rpc

import (
	"bytes"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/zombar/tailraid/internal/chunk"
	"github.com/zombar/tailraid/internal/meta"
	"github.com/zombar/tailraid/internal/s3"
	"github.com/zombar/tailraid/internal/s3/s3test"
	"github.com/zombar/tailraid/internal/sysfs"
)

// s3Client routes target 7 to the fake bucket under prefix "c1", target 8 to an S3 row
// this device holds no credentials for, and nothing else.
func s3Client(t *testing.T, srv *s3test.Server) *Client {
	t.Helper()
	b, err := s3.New(srv.Target("c1"), s3test.Creds, nil)
	if err != nil {
		t.Fatal(err)
	}
	b.SetRetry(time.Millisecond)
	c := NewClient([16]byte{1}, func(meta.NodeIdx) (string, bool, bool) { return "", false, false })
	c.SetS3(func(idx meta.NodeIdx) (*s3.Bucket, bool) {
		switch idx {
		case 7:
			return b, true
		case 8:
			return nil, true
		}
		return nil, false
	})
	return c
}

// A shard on a bucket is the bytes of the same shard on a disk, under the disk's
// relative path below the prefix (docs/10 §4.2).
// Invariant: I-52
func TestS3Node_ShardKeyLayoutMatchesDevice(t *testing.T) {
	srv := s3test.New()
	defer srv.Close()
	c := s3Client(t, srv)
	disk, err := chunk.Open(sysfs.OS{}, t.TempDir(), 1<<30)
	if err != nil {
		t.Fatal(err)
	}
	for _, n := range []int{0, 1, 4096, 1 << 16} {
		p := bytes.Repeat([]byte{byte(n), 9}, n/2+1)[:n]
		k := key(p, 2)
		if _, err := disk.Put(k, p); err != nil {
			t.Fatal(err)
		}
		if err := c.Put(ctx, 7, k, p); err != nil {
			t.Fatalf("put %d: %v", n, err)
		}
		file, err := os.ReadFile(disk.Path(k))
		if err != nil {
			t.Fatal(err)
		}
		rel, _ := filepath.Rel(disk.Root(), disk.Path(k))
		obj, ok := srv.Object("c1/" + filepath.ToSlash(rel))
		if !ok {
			t.Fatalf("no object c1/%s; bucket holds %v", rel, srv.Keys())
		}
		if !bytes.Equal(obj, file) {
			t.Fatalf("%d-byte shard: the object differs from the disk file", n)
		}
		if got, err := c.Get(ctx, 7, k); err != nil || !bytes.Equal(got, p) {
			t.Fatalf("get %d: %d bytes, %v", n, len(got), err)
		}
		if has, err := c.Has(ctx, 7, k); !has || err != nil {
			t.Fatalf("has %d: %v %v", n, has, err)
		}
	}
}

// An object that does not verify is ErrCorrupt and never returned; an absent one is
// ErrNotFound; a bucket without credentials here, or down, is unreachable.
// Invariant: I-52
func TestS3Node_GetVerifiesHash(t *testing.T) {
	srv := s3test.New()
	defer srv.Close()
	c := s3Client(t, srv)
	p := []byte("payload of a shard")
	k := key(p, 1)
	if err := c.Put(ctx, 7, k, p); err != nil {
		t.Fatal(err)
	}
	name := "c1/" + ShardObject(k)
	good, _ := srv.Object(name)
	for what, spoil := range map[string]func([]byte) []byte{
		"flipped payload": func(b []byte) []byte { b[len(b)-1] ^= 1; return b },
		"flipped hash":    func(b []byte) []byte { b[9] ^= 1; return b },
		"truncated":       func(b []byte) []byte { return b[:len(b)-1] },
		"bad magic":       func(b []byte) []byte { b[0] = 'X'; return b },
		"short":           func(b []byte) []byte { return b[:10] },
	} {
		srv.PutObject(name, spoil(bytes.Clone(good)))
		if got, err := c.Get(ctx, 7, k); !errors.Is(err, chunk.ErrCorrupt) || got != nil {
			t.Errorf("%s: %d bytes, %v", what, len(got), err)
		}
	}
	srv.DeleteObject(name)
	if _, err := c.Get(ctx, 7, k); !errors.Is(err, chunk.ErrNotFound) {
		t.Errorf("absent: %v", err)
	}
	if has, err := c.Has(ctx, 7, k); has || err != nil {
		t.Errorf("absent has: %v %v", has, err)
	}
	for _, call := range []func() error{
		func() error { return c.Put(ctx, 8, k, p) },
		func() error { _, err := c.Get(ctx, 8, k); return err },
		func() error { _, err := c.Has(ctx, 8, k); return err },
		func() error { return c.ProbeS3(ctx, 8) },
	} {
		if err := call(); !errors.Is(err, ErrUnavailable) || !errors.Is(err, ErrNoCredentials) {
			t.Errorf("no credentials: %v", err)
		}
	}
	srv.SetDown(true)
	if _, err := c.Get(ctx, 7, k); !errors.Is(err, ErrUnavailable) {
		t.Errorf("down: %v", err)
	}
}

// The shard client never sends a DELETE to a bucket; only the sweep's delete does.
// Invariant: I-138
func TestS3Node_DeleteRefused(t *testing.T) {
	srv := s3test.New()
	defer srv.Close()
	c := s3Client(t, srv)
	p := []byte("x")
	k := key(p, 1)
	if err := c.Put(ctx, 7, k, p); err != nil {
		t.Fatal(err)
	}
	for _, node := range []meta.NodeIdx{7, 8} {
		if err := c.Delete(ctx, node, k); !errors.Is(err, ErrFenced) {
			t.Errorf("delete on node %d: %v", node, err)
		}
		if err := c.DeleteFenced(ctx, node, k, 1<<40); !errors.Is(err, ErrFenced) {
			t.Errorf("fenced delete on node %d: %v", node, err)
		}
	}
	if n := srv.Count(http.MethodDelete); n != 0 || len(srv.Keys()) != 1 {
		t.Fatalf("%d DELETEs reached the bucket; it holds %v", n, srv.Keys())
	}
	srv.PutObject("c1/shards/README", []byte("not a shard"))
	srv.PutObject("c1/tailraid-check", nil)
	var listed []chunk.ShardKey
	if err := c.ListS3(ctx, 7, func(o S3Object) error { listed = append(listed, o.Key); return nil }); err != nil {
		t.Fatal(err)
	}
	if len(listed) != 1 || listed[0] != k {
		t.Fatalf("listed %v, want only %s", listed, k)
	}
	if err := c.SweepS3Delete(ctx, 7, k); err != nil {
		t.Fatal(err)
	}
	if n := srv.Count(http.MethodDelete); n != 1 || len(srv.Keys()) != 2 {
		t.Fatalf("after the sweep's delete: %d DELETEs, bucket %v", n, srv.Keys())
	}
}
