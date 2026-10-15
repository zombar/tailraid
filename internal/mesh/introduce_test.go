package mesh

import (
	"bytes"
	"context"
	"errors"
	"net/netip"
	"sync"
	"testing"
	"time"

	"github.com/zombar/tailraid/internal/clock"
)

type fakeIntroducer struct {
	mu    sync.Mutex
	addr  netip.Prefix
	err   error
	calls int
	keys  []Key
}

func (f *fakeIntroducer) Introduce(_ context.Context, key Key) (netip.Prefix, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	f.keys = append(f.keys, key)
	return f.addr, f.err
}

func (f *fakeIntroducer) last() Key {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.keys) == 0 {
		return Key{}
	}
	return f.keys[len(f.keys)-1]
}

// Invariant: I-319
func TestIntroduceServer_Table(t *testing.T) {
	token := NewToken()
	var cluster [16]byte
	cluster[3] = 9
	key := IntroduceKey(token, cluster)
	if len(key) != 32 || bytes.Equal(IntroduceKey(token, [16]byte{}), key) {
		t.Fatal("the introduce key must depend on the cluster")
	}
	clk := clock.NewFake(time.Unix(1_800_000_000, 0))
	addr := netip.MustParsePrefix("100.70.3.5/24")
	joiner, _ := GenerateKey()
	src := netip.MustParseAddrPort("203.0.113.9:4000")
	ctx := context.Background()
	fresh := func(f *fakeIntroducer) *introduceServer {
		return newIntroduceServer(cluster, func() []byte { return key }, f, clk)
	}
	req := introduceReq{cluster: cluster, key: joiner.Public(), at: clk.Now().Unix()}
	good := encodeIntroduce(req, key)
	if len(good) != introduceLen {
		t.Fatalf("request is %d bytes, want %d", len(good), introduceLen)
	}

	t.Run("good", func(t *testing.T) {
		f := &fakeIntroducer{addr: addr}
		reply := fresh(f).handle(ctx, good, src)
		if len(reply) != introduceReplyLen || len(reply) >= len(good) {
			t.Fatalf("reply is %d bytes (request %d)", len(reply), len(good))
		}
		got, mac, ok := decodeReply(reply)
		if !ok || got != addr || !bytes.Equal(mac, replyMAC(key, addr, req)) {
			t.Fatalf("reply %v %v", got, ok)
		}
		if f.calls != 1 || f.last() != joiner.Public() {
			t.Fatalf("introducer called %d times with %s", f.calls, f.last())
		}
	})
	t.Run("idempotent", func(t *testing.T) {
		f := &fakeIntroducer{addr: addr}
		s := fresh(f)
		r1, r2 := s.handle(ctx, good, src), s.handle(ctx, good, src)
		if !bytes.Equal(r1, r2) {
			t.Fatal("the same request must get the same answer")
		}
	})
	t.Run("bad mac", func(t *testing.T) {
		f := &fakeIntroducer{addr: addr}
		bad := append([]byte(nil), good...)
		bad[len(bad)-1] ^= 1
		if r := fresh(f).handle(ctx, bad, src); r != nil || f.calls != 0 {
			t.Fatalf("answered %v, introducer called %d", r, f.calls)
		}
	})
	t.Run("wrong token", func(t *testing.T) {
		f := &fakeIntroducer{addr: addr}
		other := encodeIntroduce(req, IntroduceKey(NewToken(), cluster))
		if r := fresh(f).handle(ctx, other, src); r != nil || f.calls != 0 {
			t.Fatal("a rotated token's request was answered")
		}
	})
	t.Run("wrong cluster", func(t *testing.T) {
		f := &fakeIntroducer{addr: addr}
		other := encodeIntroduce(introduceReq{key: joiner.Public(), at: req.at}, key)
		if r := fresh(f).handle(ctx, other, src); r != nil || f.calls != 0 {
			t.Fatal("another cluster's request was answered")
		}
	})
	t.Run("skew", func(t *testing.T) {
		for _, d := range []time.Duration{-IntroduceSkew - time.Second, IntroduceSkew + time.Second} {
			f := &fakeIntroducer{addr: addr}
			old := encodeIntroduce(introduceReq{cluster: cluster, key: joiner.Public(), at: clk.Now().Add(d).Unix()}, key)
			if r := fresh(f).handle(ctx, old, src); r != nil || f.calls != 0 {
				t.Fatalf("a request %s off was answered", d)
			}
		}
		for _, d := range []time.Duration{-IntroduceSkew + time.Second, IntroduceSkew - time.Second} {
			f := &fakeIntroducer{addr: addr}
			ok := encodeIntroduce(introduceReq{cluster: cluster, key: joiner.Public(), at: clk.Now().Add(d).Unix()}, key)
			if r := fresh(f).handle(ctx, ok, src); r == nil {
				t.Fatalf("a request %s off was refused", d)
			}
		}
	})
	t.Run("hold after a failure", func(t *testing.T) {
		f := &fakeIntroducer{addr: addr}
		s := fresh(f)
		bad := append([]byte(nil), good...)
		bad[20] ^= 1
		if s.handle(ctx, bad, src) != nil {
			t.Fatal("bad answered")
		}
		if s.handle(ctx, good, src) != nil {
			t.Fatal("a good request right after a failure must wait a second")
		}
		other := netip.MustParseAddrPort("203.0.113.10:4000")
		if s.handle(ctx, good, other) == nil {
			t.Fatal("another source is not held")
		}
		clk.Advance(introduceHold)
		if s.handle(ctx, good, src) == nil {
			t.Fatal("after the hold the source is answered")
		}
	})
	t.Run("sizes", func(t *testing.T) {
		f := &fakeIntroducer{addr: addr}
		s := fresh(f)
		for _, n := range []int{0, 1, introduceLen - 1, introduceLen + 1, 200} {
			b := make([]byte, n)
			if n > 0 {
				b[0] = frameIntroduce
			}
			if s.handle(ctx, b, src) != nil {
				t.Fatalf("a %d-byte frame was answered", n)
			}
		}
	})
	t.Run("introducer failure is silence without a hold", func(t *testing.T) {
		f := &fakeIntroducer{err: errors.New("no leader")}
		s := fresh(f)
		if s.handle(ctx, good, src) != nil {
			t.Fatal("answered")
		}
		f.err, f.addr = nil, addr
		if s.handle(ctx, good, src) == nil {
			t.Fatal("the source was held for the member's own failure")
		}
	})
	t.Run("no token no answer", func(t *testing.T) {
		f := &fakeIntroducer{addr: addr}
		s := newIntroduceServer(cluster, func() []byte { return nil }, f, clk)
		if s.handle(ctx, good, src) != nil || f.calls != 0 {
			t.Fatal("answered without a token")
		}
		var none *introduceServer
		if none.handle(ctx, good, src) != nil {
			t.Fatal("a nil server answered")
		}
	})
	t.Run("hold table is bounded", func(t *testing.T) {
		f := &fakeIntroducer{addr: addr}
		s := fresh(f)
		for i := range maxHeld + 10 {
			a := netip.AddrFrom4([4]byte{10, byte(i >> 16), byte(i >> 8), byte(i)})
			s.hold(a, clk.Now().Add(introduceHold))
		}
		if len(s.held) > maxHeld {
			t.Fatalf("%d held", len(s.held))
		}
	})
}

// Invariant: I-319
func TestIntroduce_ReplyDecode_Table(t *testing.T) {
	key := IntroduceKey(NewToken(), [16]byte{})
	req := introduceReq{at: 5}
	addr := netip.MustParsePrefix("100.64.9.9/24")
	good := encodeReply(key, addr, req)
	if got, mac, ok := decodeReply(good); !ok || got != addr || len(mac) != 32 {
		t.Fatal("decode")
	}
	for _, n := range []int{0, introduceReplyLen - 1, introduceReplyLen + 1} {
		b := make([]byte, n)
		if n > 0 {
			b[0] = frameIntroduce
		}
		if _, _, ok := decodeReply(b); ok {
			t.Fatalf("a %d-byte reply decoded", n)
		}
	}
	bad := append([]byte(nil), good...)
	bad[0] = frameRelay
	if _, _, ok := decodeReply(bad); ok {
		t.Fatal("a relay frame decoded as a reply")
	}
	if _, ok := decodeIntroduce(good); ok {
		t.Fatal("a reply decoded as a request")
	}
}

// An answered introduce holds its source for a second as a failed one does: a code
// holder mints pending rows no faster than one a second, and a joiner, which resends
// every IntroduceRetry, never waits (I-319).
func TestIntroduceServer_SuccessRateLimit(t *testing.T) {
	token := NewToken()
	var cluster [16]byte
	key := IntroduceKey(token, cluster)
	clk := clock.NewFake(time.Unix(1_800_000_000, 0))
	f := &fakeIntroducer{addr: netip.MustParsePrefix("100.70.3.5/24")}
	s := newIntroduceServer(cluster, func() []byte { return key }, f, clk)
	src := netip.MustParseAddrPort("203.0.113.9:4000")
	ctx := context.Background()
	frame := func() []byte {
		k, _ := GenerateKey()
		return encodeIntroduce(introduceReq{cluster: cluster, key: k.Public(), at: clk.Now().Unix()}, key)
	}
	first := frame()
	if s.handle(ctx, first, src) == nil {
		t.Fatal("the first introduce was not answered")
	}
	if s.handle(ctx, first, src) == nil {
		t.Fatal("the same key again (a joiner's resend) was not answered")
	}
	if s.handle(ctx, frame(), src) != nil {
		t.Fatal("another key within the second was answered")
	}
	if s.handle(ctx, frame(), netip.MustParseAddrPort("203.0.113.10:4000")) == nil {
		t.Fatal("another source was held")
	}
	clk.Advance(introduceHold)
	if s.handle(ctx, frame(), src) == nil {
		t.Fatal("the introduce after the hold was not answered")
	}
}
