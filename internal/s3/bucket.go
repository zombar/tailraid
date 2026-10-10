package s3

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"math/rand/v2"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// Target names a bucket and the prefix under which a cluster keeps its objects.
type Target struct {
	Endpoint  string // http(s)://host[:port]
	Bucket    string
	Prefix    string
	Region    string // us-east-1 when empty
	PathStyle bool   // endpoint/bucket/key instead of bucket.endpoint/key
}

// Errors of a bucket call. ErrNotFound is an absent object; the rest are matched with
// errors.Is by the shard client (docs/10 §4.2).
var (
	ErrNotFound    = errors.New("s3: no such key")
	ErrNoBucket    = errors.New("s3: no such bucket")
	ErrForbidden   = errors.New("s3: access denied")
	ErrUnavailable = errors.New("s3: unavailable")
	ErrBadTarget   = errors.New("s3: bad endpoint")
)

// Bucket is a client for one Target with one set of credentials.
type Bucket struct {
	t     Target
	c     Credentials
	base  *url.URL // scheme://host[:port] and, path style, /bucket
	http  *http.Client
	now   func() time.Time
	retry time.Duration // the first backoff; doubles per attempt
}

// Attempts is how many times a call is sent before its error is returned (docs/10 §4.5).
const Attempts = 4

// transferRate is the slowest path a body is given; requestBase the fixed allowance.
const (
	transferRate = 256 << 10
	requestBase  = 30 * time.Second
)

// ParseEndpoint checks an endpoint URL: http or https, a host, no path, query or user.
func ParseEndpoint(endpoint string) (*url.URL, error) {
	u, err := url.Parse(endpoint)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" || u.User != nil || u.RawQuery != "" || strings.Trim(u.Path, "/") != "" {
		return nil, fmt.Errorf("%w: %q is not an http(s)://host[:port] URL", ErrBadTarget, endpoint)
	}
	return u, nil
}

// New builds a client; rt nil uses a keep-alive transport of 8 connections per host.
func New(t Target, c Credentials, rt http.RoundTripper) (*Bucket, error) {
	u, err := ParseEndpoint(t.Endpoint)
	if err != nil {
		return nil, err
	}
	if t.Bucket == "" {
		return nil, fmt.Errorf("%w: empty bucket", ErrBadTarget)
	}
	if t.Region == "" {
		t.Region = "us-east-1"
	}
	t.Prefix = strings.Trim(t.Prefix, "/")
	base := &url.URL{Scheme: u.Scheme, Host: u.Host}
	if t.PathStyle {
		base.Path = "/" + t.Bucket
	} else {
		base.Host = t.Bucket + "." + u.Host
	}
	if rt == nil {
		rt = &http.Transport{
			Proxy:                 http.ProxyFromEnvironment,
			DialContext:           (&net.Dialer{Timeout: 10 * time.Second}).DialContext,
			MaxConnsPerHost:       8,
			MaxIdleConnsPerHost:   8,
			IdleConnTimeout:       90 * time.Second,
			TLSHandshakeTimeout:   10 * time.Second,
			ResponseHeaderTimeout: requestBase,
		}
	}
	return &Bucket{t: t, c: c, base: base, http: &http.Client{Transport: rt}, now: time.Now, retry: 200 * time.Millisecond}, nil
}

// Target is the bucket's target, with the defaults applied.
func (b *Bucket) Target() Target { return b.t }

// SetRetry sets the first backoff (tests make it short).
func (b *Bucket) SetRetry(d time.Duration) { b.retry = d }

// Key is the full object key of a key relative to the prefix.
func (b *Bucket) Key(rel string) string {
	if b.t.Prefix == "" {
		return rel
	}
	return b.t.Prefix + "/" + rel
}

func (b *Bucket) objectURL(rel string) *url.URL {
	u := *b.base
	u.Path += "/" + b.Key(rel)
	return &u
}

func (b *Bucket) bucketURL(q url.Values) *url.URL {
	u := *b.base
	if u.Path == "" {
		u.Path = "/"
	}
	u.RawQuery = q.Encode()
	return &u
}

// Put stores body under rel; a key that exists is overwritten.
func (b *Bucket) Put(ctx context.Context, rel string, body []byte) error {
	sum := sha256.Sum256(body)
	resp, err := b.do(ctx, http.MethodPut, b.objectURL(rel), body, hex.EncodeToString(sum[:]))
	if err != nil {
		return err
	}
	return drain(resp)
}

// Get returns the object under rel, at most limit bytes; a longer one is an error.
func (b *Bucket) Get(ctx context.Context, rel string, limit int64) ([]byte, error) {
	resp, err := b.do(ctx, http.MethodGet, b.objectURL(rel), nil, EmptySum)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.ContentLength > limit {
		return nil, fmt.Errorf("s3: %s is %d bytes, more than %d", rel, resp.ContentLength, limit)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, limit+1))
	if err != nil {
		return nil, fmt.Errorf("%w: read %s: %w", ErrUnavailable, rel, err)
	}
	if int64(len(body)) > limit {
		return nil, fmt.Errorf("s3: %s is more than %d bytes", rel, limit)
	}
	return body, nil
}

// Has reports whether rel exists (HEAD).
func (b *Bucket) Has(ctx context.Context, rel string) (bool, error) {
	resp, err := b.do(ctx, http.MethodHead, b.objectURL(rel), nil, EmptySum)
	if errors.Is(err, ErrNotFound) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return true, drain(resp)
}

// Delete removes rel; an absent key is not an error.
func (b *Bucket) Delete(ctx context.Context, rel string) error {
	resp, err := b.do(ctx, http.MethodDelete, b.objectURL(rel), nil, EmptySum)
	if errors.Is(err, ErrNotFound) {
		return nil
	}
	if err != nil {
		return err
	}
	return drain(resp)
}

// Probe checks that the bucket answers and the credentials are accepted (HEAD bucket).
// It is sent once: liveness already spans several probes (docs/10 §4.3).
func (b *Bucket) Probe(ctx context.Context) error {
	resp, err := b.once(ctx, http.MethodHead, b.bucketURL(nil), nil, EmptySum)
	if errors.Is(err, ErrNotFound) {
		return ErrNoBucket
	}
	if err != nil {
		return err
	}
	return drain(resp)
}

// Object is one entry of a listing; Key is relative to the prefix.
type Object struct {
	Key          string
	Size         int64
	LastModified time.Time
}

type listResult struct {
	Contents []struct {
		Key          string `xml:"Key"`
		Size         int64  `xml:"Size"`
		LastModified string `xml:"LastModified"`
	} `xml:"Contents"`
	IsTruncated           bool   `xml:"IsTruncated"`
	NextContinuationToken string `xml:"NextContinuationToken"`
}

// ListPage is the number of keys ListObjectsV2 is asked for per page.
const ListPage = 1000

// List calls fn for every object under rel (relative to the prefix), a page at a time.
func (b *Bucket) List(ctx context.Context, rel string, fn func(Object) error) error {
	full := b.Key(rel)
	strip := ""
	if b.t.Prefix != "" {
		strip = b.t.Prefix + "/"
	}
	token := ""
	for {
		q := url.Values{"list-type": {"2"}, "prefix": {full}, "max-keys": {strconv.Itoa(ListPage)}}
		if token != "" {
			q.Set("continuation-token", token)
		}
		resp, err := b.do(ctx, http.MethodGet, b.bucketURL(q), nil, EmptySum)
		if err != nil {
			return err
		}
		var res listResult
		err = xml.NewDecoder(io.LimitReader(resp.Body, 16<<20)).Decode(&res)
		_ = resp.Body.Close()
		if err != nil {
			return fmt.Errorf("%w: list %s: %w", ErrUnavailable, full, err)
		}
		for _, o := range res.Contents {
			mod, err := time.Parse(time.RFC3339, o.LastModified)
			if err != nil {
				return fmt.Errorf("s3: list %s: LastModified of %s: %w", full, o.Key, err)
			}
			if err := fn(Object{Key: strings.TrimPrefix(o.Key, strip), Size: o.Size, LastModified: mod}); err != nil {
				return err
			}
		}
		if !res.IsTruncated {
			return nil
		}
		if res.NextContinuationToken == "" {
			return fmt.Errorf("%w: list %s: truncated page without a continuation token", ErrUnavailable, full)
		}
		token = res.NextContinuationToken
	}
}

// do sends a signed request, retrying 5xx, 429 and transport errors with backoff and
// jitter; any other status above 299 is returned as an error at once (docs/10 §4.5).
func (b *Bucket) do(ctx context.Context, method string, u *url.URL, body []byte, sum string) (*http.Response, error) {
	var last error
	wait := b.retry
	for attempt := range Attempts {
		if attempt > 0 {
			d := wait/2 + rand.N(wait) //nolint:gosec // jitter, not a secret
			wait *= 2
			// a real-time backoff against a remote service, as the GC's (the clock seam
			// is for tailraid's own timers; a fake clock nobody advances would hang here)
			select {
			case <-time.After(d):
			case <-ctx.Done():
				return nil, ctx.Err()
			}
		}
		resp, err := b.once(ctx, method, u, body, sum)
		if err == nil {
			return resp, nil
		}
		last = err
		if !retryable(err) || ctx.Err() != nil {
			return nil, err
		}
	}
	return nil, last
}

type statusError struct {
	code int
	msg  string
	kind error
}

func (e *statusError) Error() string { return fmt.Sprintf("%v: HTTP %d %s", e.kind, e.code, e.msg) }
func (e *statusError) Unwrap() error { return e.kind }

func retryable(err error) bool {
	if se, ok := errors.AsType[*statusError](err); ok {
		return se.code >= 500 || se.code == http.StatusTooManyRequests
	}
	return errors.Is(err, ErrUnavailable)
}

func (b *Bucket) once(ctx context.Context, method string, u *url.URL, body []byte, sum string) (*http.Response, error) {
	ctx, cancel := context.WithTimeout(ctx, requestBase+time.Duration(len(body))*time.Second/transferRate)
	var rd io.Reader
	if body != nil {
		rd = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, u.String(), rd)
	if err != nil {
		cancel()
		return nil, fmt.Errorf("%w: %w", ErrBadTarget, err)
	}
	req.ContentLength = int64(len(body))
	Sign(req, b.c, b.t.Region, sum, b.now())
	resp, err := b.http.Do(req)
	if err != nil {
		cancel()
		return nil, fmt.Errorf("%w: %s %s: %w", ErrUnavailable, method, u.Redacted(), err)
	}
	resp.Body = &cancelBody{ReadCloser: resp.Body, cancel: cancel}
	if resp.StatusCode < 300 {
		return resp, nil
	}
	defer resp.Body.Close()
	var eb struct {
		Code    string `xml:"Code"`
		Message string `xml:"Message"`
	}
	if method != http.MethodHead {
		_ = xml.NewDecoder(io.LimitReader(resp.Body, 4<<10)).Decode(&eb)
	}
	se := &statusError{code: resp.StatusCode, msg: strings.TrimSpace(eb.Code + " " + eb.Message)}
	switch {
	case resp.StatusCode == http.StatusNotFound && eb.Code == "NoSuchBucket":
		se.kind = ErrNoBucket
	case resp.StatusCode == http.StatusNotFound:
		se.kind = ErrNotFound
	case resp.StatusCode == http.StatusForbidden || resp.StatusCode == http.StatusUnauthorized:
		se.kind = ErrForbidden
	default:
		se.kind = ErrUnavailable
	}
	return nil, se
}

// cancelBody ends a request's deadline once its body is closed.
type cancelBody struct {
	io.ReadCloser
	cancel context.CancelFunc
}

func (c *cancelBody) Close() error {
	err := c.ReadCloser.Close()
	c.cancel()
	return err
}

func drain(resp *http.Response) error {
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 64<<10))
	return resp.Body.Close()
}
