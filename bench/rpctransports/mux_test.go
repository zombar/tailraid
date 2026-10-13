package rpctransports

import (
	"context"
	"io"
	"net"
	"sync"
	"testing"
	"time"

	"github.com/hashicorp/yamux"
)

// muxConfig is yamux's, quiet; big raises the stream window from 256 KiB to 4 MiB. yamux
// has no stream reset: an abort is a close, and the stream is reset once the peer has not
// closed within StreamCloseTimeout. The client sets it to 100 ms, so a cancelled GET stops
// its sender then; the server keeps yamux's 5 minutes, since it closes as soon as it has
// written a reply the client may still be reading.
func muxConfig(big, client bool) *yamux.Config {
	c := yamux.DefaultConfig()
	c.LogOutput = io.Discard
	c.EnableKeepAlive = false
	if client {
		c.StreamCloseTimeout = 100 * time.Millisecond
	}
	if big {
		c.MaxStreamWindowSize = 4 << 20
	}
	return c
}

type muxClient struct {
	sessions []*yamux.Session
}

// call runs one request on a new stream; a cancelled ctx closes the stream.
func (m *muxClient) call(ctx context.Context, h int, op byte, body []byte) error {
	st, err := m.sessions[h].OpenStream()
	if err != nil {
		return err
	}
	defer st.Close()
	stop := context.AfterFunc(ctx, func() { _ = st.Close(); _ = st.SetReadDeadline(time.Now()) })
	defer stop()
	if err := writeRequest(st, op, body); err != nil {
		return err
	}
	if err := readReply(st); err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return err
	}
	return nil
}

func (m *muxClient) put(ctx context.Context, h int, b []byte) error {
	return m.call(ctx, h, opPut, b)
}
func (m *muxClient) get(ctx context.Context, h int) error { return m.call(ctx, h, opGet, nil) }
func (m *muxClient) apply(ctx context.Context, h int, b []byte) error {
	return m.call(ctx, h, opApply, b)
}

// startMux runs yamux over one TCP connection per host, one stream per request.
func startMux(big bool) func(b *testing.B, p profile, hosts int, slow map[int]float64) rig {
	return func(b *testing.B, p profile, hosts int, slow map[int]float64) rig {
		count := &countdown{}
		mc := &muxClient{}
		for i := range hosts {
			ln, err := net.Listen("tcp", "127.0.0.1:0")
			if err != nil {
				b.Fatal(err)
			}
			up, down := hostLinks(p, slow[i])
			cl := countingListener{Listener: ln, p: p, link: down, count: count}
			var wg sync.WaitGroup
			wg.Go(func() {
				for {
					c, err := cl.Accept()
					if err != nil {
						return
					}
					sess, err := yamux.Server(c, muxConfig(big, false))
					if err != nil {
						return
					}
					wg.Go(func() {
						for {
							st, err := sess.AcceptStream()
							if err != nil {
								return
							}
							go func() {
								_ = st.SetWriteDeadline(time.Now().Add(time.Minute))
								_ = serveFrame(st)
								_ = st.Close()
							}()
						}
					})
				}
			})
			c, err := net.Dial("tcp", ln.Addr().String())
			if err != nil {
				b.Fatal(err)
			}
			sess, err := yamux.Client(newSlowConn(c, p, up), muxConfig(big, true))
			if err != nil {
				b.Fatal(err)
			}
			b.Cleanup(func() { _ = sess.Close(); _ = ln.Close(); wg.Wait() })
			mc.sessions = append(mc.sessions, sess)
		}
		return rig{c: mc, conns: count.get}
	}
}
