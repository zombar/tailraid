package rpctransports

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"math/big"
	"net"
	"sync"
	"testing"
	"time"

	"github.com/quic-go/quic-go"
)

var (
	certOnce sync.Once
	cert     tls.Certificate
)

func selfSigned() tls.Certificate {
	certOnce.Do(func() {
		key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
		if err != nil {
			panic(err)
		}
		tmpl := &x509.Certificate{SerialNumber: big.NewInt(1), NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour), IPAddresses: []net.IP{net.IPv4(127, 0, 0, 1)}}
		der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
		if err != nil {
			panic(err)
		}
		cert = tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key}
	})
	return cert
}

type quicClient struct {
	conns []*quic.Conn
}

// call runs one request on a new stream; a cancelled ctx resets both directions.
func (q *quicClient) call(ctx context.Context, h int, op byte, body []byte) error {
	st, err := q.conns[h].OpenStreamSync(ctx)
	if err != nil {
		return err
	}
	stop := context.AfterFunc(ctx, func() { st.CancelRead(1); st.CancelWrite(1) })
	defer stop()
	if err := writeRequest(st, op, body); err != nil {
		return err
	}
	if err := st.Close(); err != nil { // the request is complete: FIN the send side
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

func (q *quicClient) put(ctx context.Context, h int, b []byte) error {
	return q.call(ctx, h, opPut, b)
}
func (q *quicClient) get(ctx context.Context, h int) error { return q.call(ctx, h, opGet, nil) }
func (q *quicClient) apply(ctx context.Context, h int, b []byte) error {
	return q.call(ctx, h, opApply, b)
}

// startQUIC runs quic-go over the simulated datagram links, one connection per host and
// one stream per request, with quic-go's defaults (stream windows starting at 512 KiB and
// auto-tuned up to 6 MiB, connection windows up to 15 MiB).
func startQUIC() func(b *testing.B, p profile, hosts int, slow map[int]float64) rig {
	return func(b *testing.B, p profile, hosts int, slow map[int]float64) rig {
		count := &countdown{}
		qc := &quicClient{}
		conf := &quic.Config{MaxIdleTimeout: time.Minute, MaxIncomingStreams: 1 << 12}
		for i := range hosts {
			up, down := hostLinks(p, slow[i])
			spc, err := net.ListenPacket("udp", "127.0.0.1:0")
			if err != nil {
				b.Fatal(err)
			}
			st := &quic.Transport{Conn: newSlowPacketConn(spc, p, down)}
			ln, err := st.Listen(&tls.Config{Certificates: []tls.Certificate{selfSigned()}, NextProtos: []string{"tr"}, MinVersion: tls.VersionTLS13}, conf)
			if err != nil {
				b.Fatal(err)
			}
			var wg sync.WaitGroup
			wg.Go(func() {
				for {
					c, err := ln.Accept(context.Background())
					if err != nil {
						return
					}
					count.add()
					wg.Go(func() {
						for {
							s, err := c.AcceptStream(context.Background())
							if err != nil {
								return
							}
							go func() {
								if err := serveFrame(s); err != nil {
									s.CancelWrite(1)
									return
								}
								_ = s.Close()
							}()
						}
					})
				}
			})
			cpc, err := net.ListenPacket("udp", "127.0.0.1:0")
			if err != nil {
				b.Fatal(err)
			}
			ct := &quic.Transport{Conn: newSlowPacketConn(cpc, p, up)}
			c, err := ct.Dial(context.Background(), spc.LocalAddr(), &tls.Config{InsecureSkipVerify: true, NextProtos: []string{"tr"}, MinVersion: tls.VersionTLS13}, conf) //nolint:gosec // a benchmark's self-signed peer
			if err != nil {
				b.Fatal(err)
			}
			b.Cleanup(func() {
				_ = c.CloseWithError(0, "")
				_ = ln.Close()
				_ = ct.Close()
				_ = st.Close()
				wg.Wait()
			})
			qc.conns = append(qc.conns, c)
		}
		return rig{c: qc, conns: count.get}
	}
}
