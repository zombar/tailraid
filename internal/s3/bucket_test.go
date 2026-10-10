package s3_test

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net/http"
	"testing"
	"time"

	"github.com/zombar/tailraid/internal/s3"
	"github.com/zombar/tailraid/internal/s3/s3test"
)

var ctx = context.Background()

func open(t *testing.T, srv *s3test.Server, prefix string, c s3.Credentials) *s3.Bucket {
	t.Helper()
	b, err := s3.New(srv.Target(prefix), c, nil)
	if err != nil {
		t.Fatal(err)
	}
	b.SetRetry(time.Millisecond)
	return b
}

// Every call against a bucket that verifies each signature and payload hash.
func TestBucket_RoundTrip(t *testing.T) {
	srv := s3test.New()
	defer srv.Close()
	b := open(t, srv, "/clusterA/", s3test.Creds)
	if err := b.Probe(ctx); err != nil {
		t.Fatal(err)
	}
	body := bytes.Repeat([]byte("tailraid "), 1000)
	if err := b.Put(ctx, "shards/ab/key 1+x", body); err != nil {
		t.Fatal(err)
	}
	if got, ok := srv.Object("clusterA/shards/ab/key 1+x"); !ok || !bytes.Equal(got, body) {
		t.Fatalf("stored under %v", srv.Keys())
	}
	if got, err := b.Get(ctx, "shards/ab/key 1+x", int64(len(body))); err != nil || !bytes.Equal(got, body) {
		t.Fatalf("get: %d bytes, %v", len(got), err)
	}
	if _, err := b.Get(ctx, "shards/ab/key 1+x", int64(len(body))-1); err == nil {
		t.Fatal("a body over the limit was returned")
	}
	if has, err := b.Has(ctx, "shards/ab/key 1+x"); !has || err != nil {
		t.Fatalf("has: %v %v", has, err)
	}
	if _, err := b.Get(ctx, "shards/ab/absent", 10); !errors.Is(err, s3.ErrNotFound) {
		t.Fatalf("absent get: %v", err)
	}
	if has, err := b.Has(ctx, "shards/ab/absent"); has || err != nil {
		t.Fatalf("absent has: %v %v", has, err)
	}
	if err := b.Delete(ctx, "shards/ab/key 1+x"); err != nil {
		t.Fatal(err)
	}
	if err := b.Delete(ctx, "shards/ab/key 1+x"); err != nil {
		t.Fatalf("deleting an absent key: %v", err)
	}
	if len(srv.Keys()) != 0 {
		t.Fatalf("left %v", srv.Keys())
	}
	wrong := open(t, srv, "clusterA", s3.Credentials{AccessKey: s3test.AccessKey, SecretKey: "wrong"})
	if err := wrong.Probe(ctx); !errors.Is(err, s3.ErrForbidden) {
		t.Fatalf("wrong secret: %v", err)
	}
	other, err := s3.New(s3.Target{Endpoint: srv.URL, Bucket: "nope", PathStyle: true}, s3test.Creds, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := other.Probe(ctx); !errors.Is(err, s3.ErrNoBucket) {
		t.Fatalf("missing bucket: %v", err)
	}
}

// A listing pages through every key under the prefix exactly once, in order, and never
// returns a key of another prefix.
func TestBucket_ListPaging(t *testing.T) {
	srv := s3test.New()
	defer srv.Close()
	b := open(t, srv, "a", s3test.Creds)
	const n = s3.ListPage*2 + 7
	for i := range n {
		srv.PutObject(fmt.Sprintf("a/shards/%05d", i), []byte{byte(i)})
	}
	srv.PutObject("ab/shards/00001", nil) // a sibling prefix that shares the first letter
	srv.PutObject("a/other", nil)
	var got []string
	if err := b.List(ctx, "shards/", func(o s3.Object) error {
		got = append(got, o.Key)
		if o.LastModified.IsZero() || o.Size != 1 {
			return fmt.Errorf("entry %+v", o)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if len(got) != n {
		t.Fatalf("%d keys, want %d", len(got), n)
	}
	for i, k := range got {
		if want := fmt.Sprintf("shards/%05d", i); k != want {
			t.Fatalf("key %d is %q, want %q", i, k, want)
		}
	}
	if pages := srv.Count(http.MethodGet); pages != 3 {
		t.Fatalf("%d list requests, want 3", pages)
	}
}

// 5xx and 429 are retried up to Attempts times; another 4xx is returned at once.
func TestBucket_RetryPolicy(t *testing.T) {
	srv := s3test.New()
	defer srv.Close()
	b := open(t, srv, "", s3test.Creds)
	srv.FailNext(s3.Attempts - 1)
	if err := b.Put(ctx, "k", []byte("x")); err != nil {
		t.Fatalf("put after %d failures: %v", s3.Attempts-1, err)
	}
	srv.FailNext(s3.Attempts)
	if err := b.Put(ctx, "k", []byte("x")); !errors.Is(err, s3.ErrUnavailable) {
		t.Fatalf("put after %d failures: %v", s3.Attempts, err)
	}
	wrong := open(t, srv, "", s3.Credentials{AccessKey: "nobody", SecretKey: "x"})
	before := srv.Count(http.MethodPut)
	if err := wrong.Put(ctx, "k", []byte("x")); !errors.Is(err, s3.ErrForbidden) {
		t.Fatalf("forbidden: %v", err)
	}
	srv.SetDown(true)
	cctx, cancel := context.WithCancel(ctx)
	cancel()
	if err := b.Put(cctx, "k", []byte("x")); err == nil {
		t.Fatal("a cancelled put succeeded")
	}
	srv.SetDown(false)
	if srv.Count(http.MethodPut) != before {
		t.Fatal("a refused request was counted as accepted")
	}
}

// New refuses endpoints that are not http(s)://host[:port], and virtual-host style puts
// the bucket in the host name.
func TestBucket_Targets(t *testing.T) {
	for _, ep := range []string{"", "s3.example.com", "ftp://s3.example.com", "https://", "https://s3.example.com/path", "https://u:p@s3.example.com", "https://s3.example.com?x=1"} {
		if _, err := s3.New(s3.Target{Endpoint: ep, Bucket: "b"}, s3.Credentials{}, nil); !errors.Is(err, s3.ErrBadTarget) {
			t.Errorf("endpoint %q: %v", ep, err)
		}
	}
	if _, err := s3.New(s3.Target{Endpoint: "https://s3.example.com"}, s3.Credentials{}, nil); !errors.Is(err, s3.ErrBadTarget) {
		t.Errorf("empty bucket: %v", err)
	}
	var seen string
	rt := roundTrip(func(r *http.Request) (*http.Response, error) {
		seen = r.URL.String()
		return nil, errors.New("stop")
	})
	b, err := s3.New(s3.Target{Endpoint: "https://s3.example.com/", Bucket: "bk", Prefix: "p"}, s3.Credentials{}, rt)
	if err != nil {
		t.Fatal(err)
	}
	b.SetRetry(time.Millisecond)
	_ = b.Put(ctx, "shards/ab/k", nil)
	if seen != "https://bk.s3.example.com/p/shards/ab/k" {
		t.Fatalf("virtual-host URL %q", seen)
	}
}

type roundTrip func(*http.Request) (*http.Response, error)

func (f roundTrip) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }
