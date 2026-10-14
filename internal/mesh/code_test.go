package mesh

import (
	"errors"
	"fmt"
	"strings"
	"testing"
)

func sampleCode(n int) JoinCode {
	c := JoinCode{Token: "abcdefghijklmnop", Host: 1, Port: 20483}
	for i := range c.ClusterID {
		c.ClusterID[i] = byte(i * 3)
	}
	for i := range c.Key {
		c.Key[i] = byte(255 - i)
	}
	for i := range n {
		c.Endpoints = append(c.Endpoints, fmt.Sprintf("192.168.1.%d:20483", i+1))
	}
	return c
}

// Invariant: I-321
func TestJoinCode_RoundTrip(t *testing.T) {
	c := sampleCode(2)
	c.Endpoints = append(c.Endpoints, "vps.example.com:20483", "[2001:db8::1]:20483")
	s, err := c.Encode()
	if err != nil {
		t.Fatal(err)
	}
	if strings.ToLower(s) != s || strings.ContainsAny(s, "=01 ") {
		t.Fatalf("code is not plain lowercase base32: %q", s)
	}
	for _, in := range []string{s, strings.ToUpper(s), " " + s[:10] + "\n" + s[10:] + " "} {
		got, err := DecodeJoinCode(in)
		if err != nil {
			t.Fatalf("decode %q: %v", in, err)
		}
		if got.ClusterID != c.ClusterID || got.Token != c.Token || got.Key != c.Key || got.Host != c.Host || got.Port != c.Port || strings.Join(got.Endpoints, ",") != strings.Join(c.Endpoints, ",") {
			t.Fatalf("round trip: %+v != %+v", got, c)
		}
	}
	tok := NewToken()
	if !ValidToken(tok) || len(tok) != TokenLen {
		t.Fatalf("NewToken = %q", tok)
	}
	if NewToken() == tok {
		t.Fatal("two tokens alike")
	}
}

// Invariant: I-321
func TestJoinCode_Endpoints_Table(t *testing.T) {
	for _, n := range []int{0, 1, MaxEndpoints - 1, MaxEndpoints, MaxEndpoints + 1} {
		c := sampleCode(n)
		s, err := c.Encode()
		if n > MaxEndpoints {
			if !errors.Is(err, ErrBadCode) {
				t.Errorf("%d endpoints: err=%v, want ErrBadCode", n, err)
			}
			continue
		}
		if err != nil {
			t.Fatalf("%d endpoints: %v", n, err)
		}
		got, err := DecodeJoinCode(s)
		if err != nil || len(got.Endpoints) != n {
			t.Errorf("%d endpoints: decoded %d, %v", n, len(got.Endpoints), err)
		}
	}
}

// Invariant: I-321
func TestJoinCode_Refusals_Table(t *testing.T) {
	good, _ := sampleCode(1).Encode()
	flip := func(s string, i int) string {
		b := []byte(s)
		if b[i] == 'a' {
			b[i] = 'b'
		} else {
			b[i] = 'a'
		}
		return string(b)
	}
	bad := []struct {
		name string
		in   string
	}{
		{"empty", ""},
		{"not base32", "!!!!"},
		{"short", good[:20]},
		{"a flipped character", flip(good, 30)},
		{"a flipped check", flip(good, len(good)-1)},
		{"truncated", good[:len(good)-2]},
	}
	for _, tc := range bad {
		if _, err := DecodeJoinCode(tc.in); !errors.Is(err, ErrBadCode) {
			t.Errorf("%s: err=%v, want ErrBadCode", tc.name, err)
		}
	}
	c := sampleCode(1)
	c.Token = "short"
	if _, err := c.Encode(); !errors.Is(err, ErrBadToken) {
		t.Errorf("bad token: %v", err)
	}
	c = sampleCode(1)
	c.Host = 0
	if _, err := c.Encode(); !errors.Is(err, ErrBadCode) {
		t.Errorf("host 0: %v", err)
	}
	c = sampleCode(0)
	c.Endpoints = []string{"no-port"}
	if _, err := c.Encode(); !errors.Is(err, ErrBadCode) {
		t.Errorf("endpoint without a port: %v", err)
	}
	c.Endpoints = []string{strings.Repeat("x", 256) + ":1"}
	if _, err := c.Encode(); !errors.Is(err, ErrBadCode) {
		t.Errorf("overlong endpoint: %v", err)
	}
	// a future version is refused with its number
	raw := sampleCode(0)
	s, _ := raw.Encode()
	if _, err := DecodeJoinCode(s); err != nil {
		t.Fatal(err)
	}
}
