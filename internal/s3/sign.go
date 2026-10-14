// Package s3 is the S3 client of the storage nodes (docs/10 §4): AWS Signature Version 4
// and the few calls a bucket that holds shards needs. It is our own, not an SDK (docs/10 §1).
package s3

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"time"
)

// Credentials are an access key pair. They never leave the device (I-139).
type Credentials struct {
	AccessKey string `yaml:"access_key" json:"access_key"`
	SecretKey string `yaml:"secret_key" json:"secret_key"`
}

const (
	algorithm  = "AWS4-HMAC-SHA256"
	amzDate    = "20060102T150405Z"
	headerDate = "X-Amz-Date"
	headerSum  = "X-Amz-Content-Sha256"
	// EmptySum is the payload hash of a request without a body.
	EmptySum = "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855"
)

// Sign adds X-Amz-Date, X-Amz-Content-Sha256 and the Authorization header for service s3
// to req. Every header already set on req is signed, and so is the host; payloadHash is
// the hex SHA-256 of the body (docs/10 §2).
func Sign(req *http.Request, c Credentials, region, payloadHash string, now time.Time) {
	req.Header.Set(headerSum, payloadHash)
	sign(req, c, region, "s3", payloadHash, now)
}

func sign(req *http.Request, c Credentials, region, service, payloadHash string, now time.Time) {
	now = now.UTC()
	req.Header.Set(headerDate, now.Format(amzDate))
	names, canonHeaders := canonicalHeaders(req)
	signed := strings.Join(names, ";")
	canon := strings.Join([]string{req.Method, canonicalURI(req.URL), canonicalQuery(req.URL), canonHeaders, signed, payloadHash}, "\n")
	day := now.Format("20060102")
	scope := day + "/" + region + "/" + service + "/aws4_request"
	sum := sha256.Sum256([]byte(canon))
	toSign := algorithm + "\n" + now.Format(amzDate) + "\n" + scope + "\n" + hex.EncodeToString(sum[:])
	key := mac([]byte("AWS4"+c.SecretKey), day)
	key = mac(key, region)
	key = mac(key, service)
	key = mac(key, "aws4_request")
	sig := hex.EncodeToString(mac(key, toSign))
	req.Header.Set("Authorization", algorithm+" Credential="+c.AccessKey+"/"+scope+", SignedHeaders="+signed+", Signature="+sig)
}

func mac(key []byte, s string) []byte {
	h := hmac.New(sha256.New, key)
	h.Write([]byte(s))
	return h.Sum(nil)
}

// canonicalHeaders lists the signed header names (lower case, sorted) and their canonical
// block: host plus every header set on the request but Authorization.
func canonicalHeaders(req *http.Request) ([]string, string) {
	vals := map[string]string{"host": hostOf(req)}
	for k, v := range req.Header {
		lk := strings.ToLower(k)
		if lk == "authorization" {
			continue
		}
		trimmed := make([]string, len(v))
		for i := range v {
			trimmed[i] = strings.Join(strings.Fields(v[i]), " ")
		}
		vals[lk] = strings.Join(trimmed, ",")
	}
	names := make([]string, 0, len(vals))
	for k := range vals {
		names = append(names, k)
	}
	sort.Strings(names)
	var b strings.Builder
	for _, k := range names {
		b.WriteString(k + ":" + vals[k] + "\n")
	}
	return names, b.String()
}

func hostOf(req *http.Request) string {
	if req.Host != "" {
		return req.Host
	}
	return req.URL.Host
}

// canonicalURI is the path with each segment URI-encoded once (S3 does not double-encode).
func canonicalURI(u *url.URL) string {
	p := u.EscapedPath()
	if p == "" {
		return "/"
	}
	raw, err := url.PathUnescape(p)
	if err != nil {
		raw = u.Path
	}
	segs := strings.Split(raw, "/")
	for i := range segs {
		segs[i] = uriEncode(segs[i])
	}
	return strings.Join(segs, "/")
}

// canonicalQuery sorts the parameters by name, then value, each URI-encoded.
func canonicalQuery(u *url.URL) string {
	q := u.Query()
	pairs := make([]string, 0, len(q))
	for k, vs := range q {
		for _, v := range vs {
			pairs = append(pairs, uriEncode(k)+"="+uriEncode(v))
		}
	}
	sort.Strings(pairs)
	return strings.Join(pairs, "&")
}

// uriEncode percent-encodes every byte but the unreserved characters, in upper-case hex.
func uriEncode(s string) string {
	const hexd = "0123456789ABCDEF"
	var b strings.Builder
	for i := range len(s) {
		c := s[i]
		if 'A' <= c && c <= 'Z' || 'a' <= c && c <= 'z' || '0' <= c && c <= '9' || c == '-' || c == '_' || c == '.' || c == '~' {
			b.WriteByte(c)
			continue
		}
		b.WriteByte('%')
		b.WriteByte(hexd[c>>4])
		b.WriteByte(hexd[c&15])
	}
	return b.String()
}

// ErrBadSignature is a request whose signature does not verify.
var ErrBadSignature = errors.New("s3: signature does not match")

// Verify checks a request signed by Sign against lookup's secret for its access key and
// returns the access key. Only the headers the request lists as signed are used. It is
// the check of the in-memory bucket the tests run against (s3test) and of the gateway to
// come (docs/10 §2).
func Verify(req *http.Request, lookup func(accessKey string) (secret string, ok bool), now time.Time, skew time.Duration) (string, error) {
	auth := req.Header.Get("Authorization")
	rest, ok := strings.CutPrefix(auth, algorithm+" ")
	if !ok {
		return "", ErrBadSignature
	}
	var cred, signedList string
	for part := range strings.SplitSeq(rest, ",") {
		k, v, _ := strings.Cut(strings.TrimSpace(part), "=")
		switch k {
		case "Credential":
			cred = v
		case "SignedHeaders":
			signedList = v
		}
	}
	fields := strings.Split(cred, "/")
	if len(fields) != 5 || fields[4] != "aws4_request" {
		return "", ErrBadSignature
	}
	secret, ok := lookup(fields[0])
	if !ok {
		return "", ErrBadSignature
	}
	t, err := time.Parse(amzDate, req.Header.Get(headerDate))
	if err != nil || t.Format("20060102") != fields[1] || now.Sub(t) > skew || t.Sub(now) > skew {
		return "", ErrBadSignature
	}
	check := &http.Request{Method: req.Method, URL: req.URL, Host: req.Host, Header: http.Header{}}
	for name := range strings.SplitSeq(signedList, ";") {
		if name != "host" {
			check.Header[http.CanonicalHeaderKey(name)] = req.Header.Values(name)
		}
	}
	sign(check, Credentials{AccessKey: fields[0], SecretKey: secret}, fields[2], fields[3], req.Header.Get(headerSum), t)
	if !hmac.Equal([]byte(check.Header.Get("Authorization")), []byte(auth)) {
		return "", ErrBadSignature
	}
	return fields[0], nil
}
