// Package s3test is an in-memory S3 bucket for tests (docs/10 §5): path-style requests,
// every signature verified, every payload hash checked, and switches for the faults the
// storage-node tests need (a slow or unreachable bucket, objects deleted behind our back,
// an object's age).
package s3test

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/xml"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/zombar/tailraid/internal/s3"
)

// Bucket and credentials every Server accepts.
const (
	Name      = "shards"
	AccessKey = "AKIDTEST"
	SecretKey = "secret/TEST+key"
)

// Creds are the credentials the Server accepts.
var Creds = s3.Credentials{AccessKey: AccessKey, SecretKey: SecretKey}

type object struct {
	body []byte
	mod  time.Time
}

// Server is a running fake bucket.
type Server struct {
	*httptest.Server
	mu      sync.Mutex
	objects map[string]object
	now     func() time.Time
	delay   time.Duration
	down    bool
	fail    int // the next fail requests get 503
	counts  map[string]int
	block   chan struct{} // PUTs wait on it while set
}

// New starts a fake bucket; Close it when done.
func New() *Server {
	s := &Server{objects: map[string]object{}, now: time.Now, counts: map[string]int{}}
	s.Server = httptest.NewServer(http.HandlerFunc(s.serve))
	return s
}

// Target is the s3.Target of this bucket under prefix.
func (s *Server) Target(prefix string) s3.Target {
	return s3.Target{Endpoint: s.URL, Bucket: Name, Prefix: prefix, PathStyle: true}
}

// SetNow sets the clock that dates objects (LastModified) and checks signatures.
func (s *Server) SetNow(now func() time.Time) { s.mu.Lock(); s.now = now; s.mu.Unlock() }

// SetDelay makes every request wait d before it is answered.
func (s *Server) SetDelay(d time.Duration) { s.mu.Lock(); s.delay = d; s.mu.Unlock() }

// SetDown makes every request fail with 503 while down.
func (s *Server) SetDown(down bool) { s.mu.Lock(); s.down = down; s.mu.Unlock() }

// FailNext makes the next n requests fail with 503.
func (s *Server) FailNext(n int) { s.mu.Lock(); s.fail = n; s.mu.Unlock() }

// BlockPuts holds every PUT until the returned function is called.
func (s *Server) BlockPuts() (release func()) {
	ch := make(chan struct{})
	s.mu.Lock()
	s.block = ch
	s.mu.Unlock()
	var once sync.Once
	return func() {
		once.Do(func() {
			s.mu.Lock()
			s.block = nil
			s.mu.Unlock()
			close(ch)
		})
	}
}

// Count is how many requests of method reached the bucket (signed correctly).
func (s *Server) Count(method string) int { s.mu.Lock(); defer s.mu.Unlock(); return s.counts[method] }

// Keys lists the stored keys, sorted.
func (s *Server) Keys() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]string, 0, len(s.objects))
	for k := range s.objects {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// Object returns a stored object's bytes.
func (s *Server) Object(key string) ([]byte, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	o, ok := s.objects[key]
	return o.body, ok
}

// PutObject stores an object directly (an operator's upload, or a corrupted one).
func (s *Server) PutObject(key string, body []byte) {
	s.mu.Lock()
	s.objects[key] = object{body: bytes.Clone(body), mod: s.now()}
	s.mu.Unlock()
}

// DeleteObject removes an object directly (deleted behind the cluster's back).
func (s *Server) DeleteObject(key string) { s.mu.Lock(); delete(s.objects, key); s.mu.Unlock() }

// Age sets an object's LastModified to d before the bucket's clock.
func (s *Server) Age(key string, d time.Duration) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if o, ok := s.objects[key]; ok {
		o.mod = s.now().Add(-d)
		s.objects[key] = o
	}
}

func (s *Server) serve(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	delay, down, now := s.delay, s.down, s.now
	failing := s.fail > 0
	if failing {
		s.fail--
	}
	block := s.block
	s.mu.Unlock()
	if delay > 0 {
		select {
		case <-time.After(delay):
		case <-r.Context().Done():
			return
		}
	}
	if down || failing {
		writeErr(w, http.StatusServiceUnavailable, "SlowDown", "unavailable")
		return
	}
	if _, err := s3.Verify(r, func(k string) (string, bool) { return SecretKey, k == AccessKey }, now(), 15*time.Minute); err != nil {
		writeErr(w, http.StatusForbidden, "SignatureDoesNotMatch", err.Error())
		return
	}
	rest, ok := strings.CutPrefix(r.URL.Path, "/"+Name)
	if !ok || (rest != "" && rest[0] != '/') {
		writeErr(w, http.StatusNotFound, "NoSuchBucket", "no such bucket")
		return
	}
	key := strings.TrimPrefix(rest, "/")
	s.mu.Lock()
	s.counts[r.Method]++
	s.mu.Unlock()
	if key == "" {
		switch r.Method {
		case http.MethodHead:
			w.WriteHeader(http.StatusOK)
		case http.MethodGet:
			s.list(w, r)
		default:
			writeErr(w, http.StatusMethodNotAllowed, "MethodNotAllowed", r.Method)
		}
		return
	}
	switch r.Method {
	case http.MethodPut:
		if block != nil {
			select {
			case <-block:
			case <-r.Context().Done():
				return
			}
		}
		body, err := io.ReadAll(r.Body)
		if err != nil {
			writeErr(w, http.StatusBadRequest, "IncompleteBody", err.Error())
			return
		}
		sum := sha256.Sum256(body)
		if r.Header.Get("X-Amz-Content-Sha256") != hex.EncodeToString(sum[:]) {
			writeErr(w, http.StatusBadRequest, "XAmzContentSHA256Mismatch", "payload hash")
			return
		}
		s.PutObject(key, body)
		w.WriteHeader(http.StatusOK)
	case http.MethodGet, http.MethodHead:
		s.mu.Lock()
		o, ok := s.objects[key]
		s.mu.Unlock()
		if !ok {
			writeErr(w, http.StatusNotFound, "NoSuchKey", key)
			return
		}
		w.Header().Set("Content-Length", strconv.Itoa(len(o.body)))
		w.Header().Set("Last-Modified", o.mod.UTC().Format(http.TimeFormat))
		w.WriteHeader(http.StatusOK)
		if r.Method == http.MethodGet {
			_, _ = w.Write(o.body)
		}
	case http.MethodDelete:
		s.DeleteObject(key)
		w.WriteHeader(http.StatusNoContent)
	default:
		writeErr(w, http.StatusMethodNotAllowed, "MethodNotAllowed", r.Method)
	}
}

type listXML struct {
	XMLName               xml.Name `xml:"ListBucketResult"`
	Contents              []entry  `xml:"Contents"`
	IsTruncated           bool     `xml:"IsTruncated"`
	NextContinuationToken string   `xml:"NextContinuationToken,omitempty"`
}

type entry struct {
	Key          string `xml:"Key"`
	Size         int    `xml:"Size"`
	LastModified string `xml:"LastModified"`
}

// list answers ListObjectsV2: keys under prefix after the continuation token (the last
// key of the previous page), max-keys at a time.
func (s *Server) list(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	if q.Get("list-type") != "2" {
		writeErr(w, http.StatusBadRequest, "InvalidArgument", "list-type")
		return
	}
	limit := 1000
	if v, err := strconv.Atoi(q.Get("max-keys")); err == nil && v > 0 && v < limit {
		limit = v
	}
	prefix, after := q.Get("prefix"), q.Get("continuation-token")
	var res listXML
	for _, k := range s.Keys() {
		if !strings.HasPrefix(k, prefix) || k <= after {
			continue
		}
		if len(res.Contents) == limit {
			res.IsTruncated = true
			res.NextContinuationToken = res.Contents[limit-1].Key
			break
		}
		s.mu.Lock()
		o := s.objects[k]
		s.mu.Unlock()
		res.Contents = append(res.Contents, entry{Key: k, Size: len(o.body), LastModified: o.mod.UTC().Format(time.RFC3339)})
	}
	w.Header().Set("Content-Type", "application/xml")
	_ = xml.NewEncoder(w).Encode(res)
}

func writeErr(w http.ResponseWriter, code int, name, msg string) {
	w.Header().Set("Content-Type", "application/xml")
	w.WriteHeader(code)
	_, _ = fmt.Fprintf(w, "<Error><Code>%s</Code><Message>%s</Message></Error>", name, xmlEscape(msg))
}

func xmlEscape(s string) string {
	var b strings.Builder
	_ = xml.EscapeText(&b, []byte(s))
	return b.String()
}
