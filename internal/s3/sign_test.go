package s3

import (
	"net/http"
	"strings"
	"testing"
	"time"
)

// The worked examples of the S3 SigV4 documentation ("Signature calculations for the
// Authorization header: transferring payload in a single chunk") and get-vanilla of the
// SigV4 test suite: the signature is byte-exact or no provider accepts it.
func TestSign_AWSExamples(t *testing.T) {
	s3creds := Credentials{AccessKey: "AKIAIOSFODNN7EXAMPLE", SecretKey: "wJalrXUtnFEMI/K7MDENG/bPxRfiCYEXAMPLEKEY"}
	at := time.Date(2013, 5, 24, 0, 0, 0, 0, time.UTC)
	for _, c := range []struct {
		name, method, url, sum string
		hdr                    map[string]string
		signed, sig            string
	}{
		{
			"get object", "GET", "https://examplebucket.s3.amazonaws.com/test.txt", EmptySum,
			map[string]string{"Range": "bytes=0-9"},
			"host;range;x-amz-content-sha256;x-amz-date", "f0e8bdb87c964420e857bd35b5d6ed310bd44f0170aba48dd91039c6036bdb41",
		},
		{
			"put object", "PUT", "https://examplebucket.s3.amazonaws.com/test$file.text", "44ce7dd67c959e0d3524ffac1771dfbba87d2b6b4b4e99e42034a8b803f8b072",
			map[string]string{"Date": "Fri, 24 May 2013 00:00:00 GMT", "X-Amz-Storage-Class": "REDUCED_REDUNDANCY"},
			"date;host;x-amz-content-sha256;x-amz-date;x-amz-storage-class", "98ad721746da40c64f1a55b78f14c238d841ea1380cd77a1b5971af0ece108bd",
		},
		{
			"get lifecycle", "GET", "https://examplebucket.s3.amazonaws.com/?lifecycle", EmptySum, nil,
			"host;x-amz-content-sha256;x-amz-date", "fea454ca298b7da1c68078a5d1bdbfbbe0d65c699e0f91ac7a200a0136783543",
		},
		{
			"list objects", "GET", "https://examplebucket.s3.amazonaws.com/?max-keys=2&prefix=J", EmptySum, nil,
			"host;x-amz-content-sha256;x-amz-date", "34b48302e7b5fa45bde8084f4b7868a86f0a534bc59db6670ed5711ef69dc6f7",
		},
	} {
		req, err := http.NewRequest(c.method, c.url, http.NoBody)
		if err != nil {
			t.Fatal(err)
		}
		for k, v := range c.hdr {
			req.Header.Set(k, v)
		}
		Sign(req, s3creds, "us-east-1", c.sum, at)
		want := "AWS4-HMAC-SHA256 Credential=AKIAIOSFODNN7EXAMPLE/20130524/us-east-1/s3/aws4_request, SignedHeaders=" + c.signed + ", Signature=" + c.sig
		if got := req.Header.Get("Authorization"); got != want {
			t.Errorf("%s:\n got %s\nwant %s", c.name, got, want)
		}
	}
	req, _ := http.NewRequest("GET", "https://example.amazonaws.com/", http.NoBody)
	sign(req, Credentials{AccessKey: "AKIDEXAMPLE", SecretKey: "wJalrXUtnFEMI/K7MDENG+bPxRfiCYEXAMPLEKEY"}, "us-east-1", "service", EmptySum,
		time.Date(2015, 8, 30, 12, 36, 0, 0, time.UTC))
	if got := req.Header.Get("Authorization"); !strings.HasSuffix(got, "SignedHeaders=host;x-amz-date, Signature=5fa00fa31553b73ebf1942676e86291e8372ff2a2260956d9b8aae1d763fbf31") {
		t.Errorf("get-vanilla: %s", got)
	}
}

// Verify accepts what Sign produced and nothing that was changed after signing.
func TestVerify_SignRoundTrip(t *testing.T) {
	c := Credentials{AccessKey: "AK", SecretKey: "SK"}
	now := time.Date(2026, 10, 10, 12, 0, 0, 0, time.UTC)
	lookup := func(k string) (string, bool) { return "SK", k == "AK" }
	mk := func() *http.Request {
		req, _ := http.NewRequest("PUT", "http://127.0.0.1:9000/b/p/shards/ab/x.2-1.0?x=a%20b", http.NoBody)
		Sign(req, c, "us-east-1", EmptySum, now)
		return req
	}
	if k, err := Verify(mk(), lookup, now.Add(time.Minute), 15*time.Minute); err != nil || k != "AK" {
		t.Fatalf("round trip: %q %v", k, err)
	}
	for name, spoil := range map[string]func(*http.Request){
		"path":   func(r *http.Request) { r.URL.Path = "/b/other" },
		"query":  func(r *http.Request) { r.URL.RawQuery = "x=b" },
		"method": func(r *http.Request) { r.Method = "DELETE" },
		"sum":    func(r *http.Request) { r.Header.Set(headerSum, "00") },
		"host":   func(r *http.Request) { r.Host = "127.0.0.1:9001" },
		"key": func(r *http.Request) {
			r.Header.Set("Authorization", strings.Replace(r.Header.Get("Authorization"), "AK/", "XX/", 1))
		},
		"unsigned": nil,
	} {
		req := mk()
		if spoil == nil {
			req.Header.Set("User-Agent", "added after signing") // not signed: ignored
			if _, err := Verify(req, lookup, now, time.Minute); err != nil {
				t.Errorf("unsigned header changed the verdict: %v", err)
			}
			continue
		}
		spoil(req)
		if _, err := Verify(req, lookup, now, time.Minute); err == nil {
			t.Errorf("%s changed after signing still verifies", name)
		}
	}
	if _, err := Verify(mk(), lookup, now.Add(20*time.Minute), 15*time.Minute); err == nil {
		t.Error("a request outside the skew window verifies")
	}
}
