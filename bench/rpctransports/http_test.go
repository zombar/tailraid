package rpctransports

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"testing"
	"time"
)

func httpHandler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("PUT /shard", func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		w.WriteHeader(http.StatusNoContent)
	})
	mux.HandleFunc("GET /shard", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Length", fmt.Sprint(shardSize))
		for left := shardSize; left > 0; {
			n := min(left, len(getPayload))
			if _, err := w.Write(getPayload[:n]); err != nil {
				return
			}
			left -= n
		}
	})
	mux.HandleFunc("POST /apply", func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		_, _ = w.Write(applyReply)
	})
	return mux
}

type httpClient struct {
	c     *http.Client
	addrs []string
}

func (h *httpClient) do(ctx context.Context, method string, i int, path string, body []byte) error {
	var rd io.Reader = http.NoBody
	if body != nil {
		rd = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, "http://"+h.addrs[i]+path, rd)
	if err != nil {
		return err
	}
	resp, err := h.c.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	_, err = io.Copy(io.Discard, resp.Body)
	return err
}

func (h *httpClient) put(ctx context.Context, i int, b []byte) error {
	return h.do(ctx, http.MethodPut, i, "/shard", b)
}
func (h *httpClient) get(ctx context.Context, i int) error {
	return h.do(ctx, http.MethodGet, i, "/shard", nil)
}
func (h *httpClient) apply(ctx context.Context, i int, b []byte) error {
	return h.do(ctx, http.MethodPost, i, "/apply", b)
}

// startHTTP runs the node RPC's settings (internal/rpc/client.go): HTTP/1.1 with 32 idle
// connections per host, a 10 s header timeout and a 3 s dial; or the same transport
// speaking only cleartext HTTP/2 with prior knowledge, Go's default windows.
func startHTTP(h2 bool) func(b *testing.B, p profile, hosts int, slow map[int]float64) rig {
	return func(b *testing.B, p profile, hosts int, slow map[int]float64) rig {
		var sp http.Protocols
		sp.SetHTTP1(true)
		sp.SetUnencryptedHTTP2(true)
		count := &countdown{}
		hc := &httpClient{}
		ups := map[string]*link{}
		for i := range hosts {
			ln, err := net.Listen("tcp", "127.0.0.1:0")
			if err != nil {
				b.Fatal(err)
			}
			up, down := hostLinks(p, slow[i])
			srv := &http.Server{Handler: httpHandler(), Protocols: &sp, ReadHeaderTimeout: 5 * time.Second, IdleTimeout: 2 * time.Minute}
			done := make(chan struct{})
			go func() { _ = srv.Serve(countingListener{Listener: ln, p: p, link: down, count: count}); close(done) }()
			b.Cleanup(func() { _ = srv.Close(); <-done })
			hc.addrs = append(hc.addrs, ln.Addr().String())
			ups[ln.Addr().String()] = up
		}
		dialer := &net.Dialer{Timeout: 3 * time.Second}
		tr := &http.Transport{
			MaxIdleConnsPerHost: 32, IdleConnTimeout: 90 * time.Second, ResponseHeaderTimeout: 10 * time.Second,
			DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
				c, err := dialer.DialContext(ctx, network, addr)
				if err != nil {
					return nil, err
				}
				return newSlowConn(c, p, ups[addr]), nil
			},
		}
		if h2 {
			var cp http.Protocols
			cp.SetUnencryptedHTTP2(true)
			tr.Protocols = &cp
		}
		b.Cleanup(tr.CloseIdleConnections)
		hc.c = &http.Client{Transport: tr}
		return rig{c: hc, conns: count.get}
	}
}
