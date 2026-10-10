package s3_test

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"strings"
	"testing"

	"github.com/zombar/tailraid/internal/s3"
)

// RealEnv names a real bucket for TestBucket_RealProvider:
// endpoint,bucket,access-key,secret-key[,region[,path]] (docs/10 §5). The test writes and
// deletes objects under the prefix tailraid-test/.
const RealEnv = "TAILRAID_S3_TEST"

// The client against a real provider (B2, R2, MinIO, rclone serve s3): the fake bucket
// shares our signing code, so only a real server proves the signatures and the listing.
func TestBucket_RealProvider(t *testing.T) {
	spec := os.Getenv(RealEnv)
	if spec == "" {
		t.Skip("set " + RealEnv + "=endpoint,bucket,access-key,secret-key[,region[,path]] to run against a real bucket")
	}
	f := strings.Split(spec, ",")
	if len(f) < 4 {
		t.Fatalf("%s: want endpoint,bucket,access-key,secret-key[,region[,path]]", RealEnv)
	}
	tgt := s3.Target{Endpoint: f[0], Bucket: f[1], Prefix: "tailraid-test"}
	if len(f) > 4 {
		tgt.Region = f[4]
	}
	tgt.PathStyle = len(f) > 5 && f[5] == "path"
	creds := s3.Credentials{AccessKey: f[2], SecretKey: f[3]}
	b, err := s3.New(tgt, creds, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := b.Probe(ctx); err != nil {
		t.Fatalf("probe: %v", err)
	}
	keys := []string{"shards/ab/one", "shards/ab/two three", "shards/cd/x+y=z", "other"}
	for i, k := range keys {
		if err := b.Put(ctx, k, bytes.Repeat([]byte{byte(i)}, 1000*(i+1))); err != nil {
			t.Fatalf("put %q: %v", k, err)
		}
	}
	defer func() {
		for _, k := range keys {
			_ = b.Delete(ctx, k)
		}
	}()
	for i, k := range keys {
		got, err := b.Get(ctx, k, 1<<20)
		if err != nil || !bytes.Equal(got, bytes.Repeat([]byte{byte(i)}, 1000*(i+1))) {
			t.Fatalf("get %q: %d bytes, %v", k, len(got), err)
		}
	}
	var listed []string
	if err := b.List(ctx, "shards/", func(o s3.Object) error {
		if o.LastModified.IsZero() {
			return fmt.Errorf("%s has no LastModified", o.Key)
		}
		listed = append(listed, o.Key)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if strings.Join(listed, "|") != strings.Join(keys[:3], "|") {
		t.Fatalf("listed %q", listed)
	}
	if _, err := b.Get(ctx, "shards/absent", 10); !errors.Is(err, s3.ErrNotFound) {
		t.Fatalf("absent: %v", err)
	}
	if has, err := b.Has(ctx, "shards/ab/one"); !has || err != nil {
		t.Fatalf("has: %v %v", has, err)
	}
	if err := b.Delete(ctx, "shards/ab/one"); err != nil {
		t.Fatal(err)
	}
	if has, err := b.Has(ctx, "shards/ab/one"); has || err != nil {
		t.Fatalf("has after delete: %v %v", has, err)
	}
	wrong, err := s3.New(tgt, s3.Credentials{AccessKey: f[2], SecretKey: f[3] + "x"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := wrong.Probe(ctx); !errors.Is(err, s3.ErrForbidden) {
		t.Fatalf("wrong secret: %v", err)
	}
}
