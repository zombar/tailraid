package rpctransports

import (
	"context"
	"errors"
	"io"
	"net"
	"testing"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
)

// rawCodec carries []byte messages as they are: no protobuf, no protoc.
type rawCodec struct{}

func (rawCodec) Name() string { return "raw" }
func (rawCodec) Marshal(v any) ([]byte, error) {
	return *v.(*[]byte), nil
}

func (rawCodec) Unmarshal(data []byte, v any) error {
	*v.(*[]byte) = append((*v.(*[]byte))[:0], data...)
	return nil
}

const (
	grpcUnary  = false
	grpcStream = true
	grpcChunk  = 256 << 10 // a streamed shard's message size
	grpcMaxMsg = 8 << 20
)

// grpcService is hand-written: unary Put, Get and Apply, and streamed PutStream (client
// stream of chunks) and GetStream (server stream of chunks).
var grpcService = grpc.ServiceDesc{
	ServiceName: "tr.Node",
	HandlerType: (*any)(nil),
	Methods: []grpc.MethodDesc{
		{MethodName: "Put", Handler: func(_ any, _ context.Context, dec func(any) error, _ grpc.UnaryServerInterceptor) (any, error) {
			var in []byte
			if err := dec(&in); err != nil {
				return nil, err
			}
			out := []byte{}
			return &out, nil
		}},
		{MethodName: "Get", Handler: func(_ any, _ context.Context, dec func(any) error, _ grpc.UnaryServerInterceptor) (any, error) {
			var in []byte
			if err := dec(&in); err != nil {
				return nil, err
			}
			out := make([]byte, shardSize)
			return &out, nil
		}},
		{MethodName: "Apply", Handler: func(_ any, _ context.Context, dec func(any) error, _ grpc.UnaryServerInterceptor) (any, error) {
			var in []byte
			if err := dec(&in); err != nil {
				return nil, err
			}
			out := applyReply
			return &out, nil
		}},
	},
	Streams: []grpc.StreamDesc{
		{StreamName: "PutStream", ClientStreams: true, Handler: func(_ any, st grpc.ServerStream) error {
			for {
				var m []byte
				if err := st.RecvMsg(&m); errors.Is(err, io.EOF) {
					out := []byte{}
					return st.SendMsg(&out)
				} else if err != nil {
					return err
				}
			}
		}},
		{StreamName: "GetStream", ServerStreams: true, Handler: func(_ any, st grpc.ServerStream) error {
			var in []byte
			if err := st.RecvMsg(&in); err != nil {
				return err
			}
			chunk := make([]byte, grpcChunk)
			for left := shardSize; left > 0; left -= grpcChunk {
				m := chunk[:min(left, grpcChunk)]
				if err := st.SendMsg(&m); err != nil {
					return err
				}
			}
			return nil
		}},
	},
}

type grpcClient struct {
	conns  []*grpc.ClientConn
	stream bool
}

var callOpts = []grpc.CallOption{grpc.ForceCodec(rawCodec{}), grpc.MaxCallRecvMsgSize(grpcMaxMsg), grpc.MaxCallSendMsgSize(grpcMaxMsg)}

func (g *grpcClient) put(ctx context.Context, h int, body []byte) error {
	if !g.stream {
		var out []byte
		return g.conns[h].Invoke(ctx, "/tr.Node/Put", &body, &out, callOpts...)
	}
	st, err := g.conns[h].NewStream(ctx, &grpcService.Streams[0], "/tr.Node/PutStream", callOpts...)
	if err != nil {
		return err
	}
	for off := 0; off < len(body); off += grpcChunk {
		m := body[off:min(off+grpcChunk, len(body))]
		if err := st.SendMsg(&m); err != nil {
			return err
		}
	}
	if err := st.CloseSend(); err != nil {
		return err
	}
	var out []byte
	return st.RecvMsg(&out)
}

func (g *grpcClient) get(ctx context.Context, h int) error {
	in := []byte{}
	if !g.stream {
		var out []byte
		return g.conns[h].Invoke(ctx, "/tr.Node/Get", &in, &out, callOpts...)
	}
	st, err := g.conns[h].NewStream(ctx, &grpcService.Streams[1], "/tr.Node/GetStream", callOpts...)
	if err != nil {
		return err
	}
	if err := st.SendMsg(&in); err != nil {
		return err
	}
	if err := st.CloseSend(); err != nil {
		return err
	}
	for {
		var m []byte
		if err := st.RecvMsg(&m); errors.Is(err, io.EOF) {
			return nil
		} else if err != nil {
			return err
		}
	}
}

func (g *grpcClient) apply(ctx context.Context, h int, body []byte) error {
	var out []byte
	return g.conns[h].Invoke(ctx, "/tr.Node/Apply", &body, &out, callOpts...)
}

// startGRPC runs gRPC over the simulated links: one ClientConn (one TCP connection) per
// host, as gRPC does. tuned fixes the windows at 4 MiB per stream and 32 MiB per
// connection on both ends; otherwise gRPC's defaults apply (64 KiB windows grown by its
// BDP estimator).
func startGRPC(stream, tuned bool) func(b *testing.B, p profile, hosts int, slow map[int]float64) rig {
	return func(b *testing.B, p profile, hosts int, slow map[int]float64) rig {
		count := &countdown{}
		gc := &grpcClient{stream: stream}
		sopts := []grpc.ServerOption{grpc.ForceServerCodec(rawCodec{}), grpc.MaxRecvMsgSize(grpcMaxMsg), grpc.MaxSendMsgSize(grpcMaxMsg)}
		dopts := []grpc.DialOption{grpc.WithTransportCredentials(insecure.NewCredentials())}
		if tuned {
			sopts = append(sopts, grpc.StaticStreamWindowSize(4<<20), grpc.InitialConnWindowSize(32<<20))
			dopts = append(dopts, grpc.WithStaticStreamWindowSize(4<<20), grpc.WithInitialConnWindowSize(32<<20))
		}
		for i := range hosts {
			ln, err := net.Listen("tcp", "127.0.0.1:0")
			if err != nil {
				b.Fatal(err)
			}
			up, down := hostLinks(p, slow[i])
			srv := grpc.NewServer(sopts...)
			srv.RegisterService(&grpcService, nil)
			done := make(chan struct{})
			go func() { _ = srv.Serve(countingListener{Listener: ln, p: p, link: down, count: count}); close(done) }()
			b.Cleanup(func() { srv.Stop(); <-done })
			addr := ln.Addr().String()
			cc, err := grpc.NewClient("passthrough:///"+addr, append(dopts, grpc.WithContextDialer(func(ctx context.Context, a string) (net.Conn, error) {
				c, err := (&net.Dialer{}).DialContext(ctx, "tcp", a)
				if err != nil {
					return nil, err
				}
				return newSlowConn(c, p, up), nil
			}))...)
			if err != nil {
				b.Fatal(err)
			}
			b.Cleanup(func() { _ = cc.Close() })
			gc.conns = append(gc.conns, cc)
		}
		return rig{c: gc, conns: count.get}
	}
}
