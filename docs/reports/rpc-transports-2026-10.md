# Transports for the node RPC

Measured 2026-10-13 to decide whether tailraid's node-to-node traffic should stay on
HTTP/1.1 or move to cleartext HTTP/2, gRPC, a stream multiplexer with its own framing, or
QUIC. The benchmarks are the module `bench/rpctransports` (its own `go.mod`, so none of
the candidates enters tailraid's `go.mod` or `vendor/`); the raw output, two runs of each,
is `bench-2026-10-rpc-transports.txt` beside this file. Nothing in the node changed.

## Candidates

| Name | What it is |
|---|---|
| `h1` | today's node RPC: HTTP/1.1, `MaxIdleConnsPerHost` 32 (I-255), `ResponseHeaderTimeout` 10 s, 3 s dial |
| `h2` | the same `http.Transport` speaking only cleartext HTTP/2 with prior knowledge (Go 1.27 `http.Protocols`), Go's default windows |
| `grpc` | `google.golang.org/grpc` 1.84 with a raw `[]byte` codec and a hand-written `ServiceDesc` (no protobuf code, no protoc): a shard PUT and GET as unary calls of one 1.4 MiB message, default windows (64 KiB grown by the BDP estimator), 8 MiB message limits |
| `grpc-tuned` | `grpc` with static windows of 4 MiB per stream and 32 MiB per connection on both ends |
| `grpc-stream` | `grpc` with the shard as a client stream (PUT) or server stream (GET) of 256 KiB messages |
| `mux` | `hashicorp/yamux` 0.1.2 over one TCP connection per host, one stream per request, a length-prefixed framing (an op byte, a length, the body; a length and the reply), yamux's 256 KiB stream window |
| `mux-4MiB` | `mux` with a 4 MiB stream window |
| `quic` | `quic-go` 0.63 over UDP, one connection per host and one stream per request, the same framing, TLS 1.3 with a self-signed certificate, quic-go's auto-tuned windows (up to 6 MiB per stream, 15 MiB per connection) |

All of them build with `CGO_ENABLED=0`.

## Method

Each host is a real server on loopback. Every connection (or, for QUIC, every datagram)
to a host goes through a link that delays it by a one-way latency and paces it through a
bandwidth shared by every connection to that host, per direction, behind a 1 MiB send
buffer per connection, so one connection and many share the same pipe as they would
between two tailnet devices. Three paths:

| Profile | One-way delay | RTT | Bandwidth per host and direction | Loss |
|---|---|---|---|---|
| `lan` | 0.25 ms | 0.5 ms | 100 MiB/s | none |
| `tailnet` | 7.5 ms | 15 ms | 25 MiB/s | none |
| `lossy` | 7.5 ms | 15 ms | 25 MiB/s | 0.2 % of 1,280-byte packets |

Loss is modelled two ways, and the difference matters for reading the `lossy` rows.
QUIC's datagrams are really dropped, so quic-go's loss detection and congestion control
react as they would. A TCP connection cannot be made to lose a segment from user space, so
the TCP link charges a lost packet as a stall of everything behind it on that connection:
one RTT when fast retransmit recovers it, the 200 ms minimum RTO for a tenth of losses.
It models head-of-line blocking but not TCP halving its congestion window, which makes
the TCP rows optimistic against the QUIC rows under loss.

The workloads, a shard being 1,468,006 bytes (a 4 MiB chunk at 3+2):

| Workload | What it does | Stands for |
|---|---|---|
| PutFanout | 8 workers, each PUTting one chunk's 5 shards to 5 hosts at once | the flusher, `ChunkWorkers` 8 (docs/04 §9) |
| HedgedGet | 8 workers, each GETting 4 shards and cancelling the last once 3 arrived; host 3 has a quarter of the bandwidth | the hedged read |
| ReadaheadSeek | 2 workers, each reading 1 shard with 3 prefetches started beside it and cancelled when the read ends | readahead and a seek (I-203) |
| ControlBurst | 32 apply calls at once (200 B in, 100 B out) to one host | proposals forwarded to the leader (I-255) |
| MixedRepairAndControl | 4 shard GETs streaming from a host while single apply calls run against it | a repair pass beside a gateway's control traffic |

Each workload runs its workers twice untimed to open the connections a steady state keeps;
`conns/op` counts the TCP or QUIC connections opened after that. Environment: Apple M2
Max, macOS, Go 1.27.2, `-benchtime 20x`, `-count 2`; the tables give the mean of the two.

## Results

Bulk transfer, MiB/s of shard bodies moved:

| PutFanout | h1 | h2 | grpc | grpc-tuned | grpc-stream | mux | mux-4MiB | quic |
|---|---|---|---|---|---|---|---|---|
| lan | 274 | 274 | 273 | 272 | 275 | 276 | 253 | 185 |
| tailnet | 67.8 | 66.7 | 65.3 | 66.7 | 65.5 | 67.4 | 51.1 | 64.7 |
| lossy | 55.8 | 29.5 | 32.8 | 30.8 | 30.0 | 27.4 | 21.7 | 7.0 |

Read latency, p99 of a hedged read (3 of 4 shards):

| HedgedGet p99 | h1 | h2 | grpc | grpc-tuned | grpc-stream | mux | mux-4MiB | quic |
|---|---|---|---|---|---|---|---|---|
| lan | 113 ms | 117 ms | 121 ms | 121 ms | 121 ms | 117 ms | 120 ms | 177 ms |
| tailnet | 464 ms | 476 ms | 500 ms | 491 ms | 493 ms | 504 ms | 483 ms | 474 ms |
| lossy | 683 ms | 1.25 s | 1.10 s | 1.15 s | 977 ms | 1.29 s | 986 ms | 4.40 s |

New connections per operation, steady state (every other candidate opens none):

| | h1 lan | h1 tailnet | h1 lossy |
|---|---|---|---|
| HedgedGet | 1.0 | 1.0 | 1.0 |
| ReadaheadSeek | 2.65 | 2.23 | 1.90 |

Control calls, p99 of one apply call:

| | h1 | h2 | grpc | grpc-tuned | grpc-stream | mux | mux-4MiB | quic |
|---|---|---|---|---|---|---|---|---|
| ControlBurst lan | 0.94 ms | 1.54 ms | 1.11 ms | 1.14 ms | 1.13 ms | 2.2 ms | 1.84 ms | 0.88 ms |
| ControlBurst tailnet | 17.3 ms | 17.2 ms | 17.5 ms | 21.8 ms | 24.6 ms | 30.1 ms | 20.0 ms | 27.8 ms |
| ControlBurst lossy | 21.5 ms | 32.6 ms | 24.7 ms | 32.6 ms | 17.4 ms | 133 ms | 224 ms | 17.1 ms |
| Mixed lan | 33.5 ms | 12.8 ms | 12.4 ms | 11.9 ms | 12.4 ms | 7.4 ms | 13.8 ms | 7.7 ms |
| Mixed tailnet | 132 ms | 61.1 ms | 58.6 ms | 54.2 ms | 58.5 ms | 36.9 ms | 61.4 ms | 33.3 ms |
| Mixed lossy | 135 ms | 233 ms | 240 ms | 243 ms | 235 ms | 225 ms | 238 ms | 38.5 ms |

The p50 of a mixed call tells the same story: 103 ms on `h1` on the tailnet path, 54–58 ms
on the HTTP/2 family, 21 ms on `mux` and 33 ms on `quic`.

## Reading

**Without loss, bulk throughput is a wash.** Every TCP candidate moves 272–276 MiB/s on
the LAN path and 65–68 MiB/s on the tailnet path; the link is the limit. Two lose: `quic`
on the LAN path (185 MiB/s: quic-go does its encryption, pacing and acknowledgements in
user space, on the CPU this box shares with the test) and `mux-4MiB` (51 MiB/s on the
tailnet path: with 4 MiB windows five 1.4 MiB streams on one connection fill the send
buffer in turn instead of interleaving, and a chunk waits for its slowest shard).
Larger windows bought nothing anywhere: `grpc-tuned` matches `grpc` and `mux-4MiB` is
worse than `mux`.

**Connections are HTTP/1.1's cost, everywhere else they are free.** A hedged read opens
one new TCP connection per read on HTTP/1.1 and a readahead cancelled by a seek two to
three, because Go retires a connection whose response it stopped reading. Every
multiplexed candidate cancels a stream (`RST_STREAM`, a yamux close, a QUIC
`STOP_SENDING`) and keeps the connection. In this rig the handshakes are cheap enough
that the latency effect is a few percent; on the real tailnet each is a round trip plus a
cold congestion window.

**One connection is what makes the mixed workload faster.** With four shard GETs
streaming from a host, a control call waits 103 ms (p50) on HTTP/1.1 on the tailnet path,
57 ms on the HTTP/2 family and 21 ms on `mux`. On HTTP/1.1 the call rides its own
connection, which the link serves after the bulk connections' queued bytes; on one
multiplexed connection the call's frame is interleaved ahead of the bulk data still in
the sender's queue. yamux, with no priority logic but small default windows, keeps that
queue shortest.

**Under loss, one connection is what makes everything slower.** On the `lossy` path every
single-connection TCP candidate roughly halves its bulk throughput (27–33 MiB/s against
`h1`'s 56) and doubles a hedged read's p99 (1–1.3 s against 683 ms): a lost packet stalls
every stream of that host behind it, while HTTP/1.1's other connections keep moving. And
these TCP rows are optimistic, since the model leaves out the congestion window halving.

**QUIC is the outlier both ways.** It is the only candidate whose streams do not block
each other on loss, and the mixed workload shows it: a control call's p99 under loss is
38.5 ms on `quic` against 225–243 ms on every TCP multiplexer and 135 ms on `h1`. But
quic-go's real congestion control on a 0.2 % loss path cut bulk throughput to 7 MiB/s
and a hedged read's p99 to 4.4 s; the TCP rows, without congestion control, are not a fair
comparison here, but the gap is too large to be only the model's. Two further costs are
specific to tailraid: QUIC would run inside WireGuard, so two congestion controllers (the
QUIC one and whatever the underlay does) and two encryption layers stack; and
tailscale0's MTU is 1,280, QUIC's minimum, which leaves no room for path MTU discovery to
grow packets and costs header overhead on every datagram.

## Costs

| | h1 | h2 | grpc | mux (yamux) | quic |
|---|---|---|---|---|---|
| New modules in `vendor/` | none | none | `grpc`, `protobuf`, `genproto/googleapis/rpc` (3, about 3.1 MB of source) | `yamux` (1, 88 KB) | `quic-go` (1, 1.3 MB; its `x/crypto`, `x/net`, `x/sys` are already vendored) |
| Static linux/amd64 binary, `-s -w` (probe programs) | 3.54 MB baseline | +0 | +7.3 MB | +0.14 MB | +3.0 MB |
| On tailraid's 20.9 MB binary | — | +0 % | about +35 % | under +1 % | about +14 % |
| Rolling upgrade | today's | a node must know its peer serves h2c: the member row's `Version`, or a release that enables the server side before any client uses it | a second listener or a gate on the version; old nodes only speak HTTP/1.1 | the same as gRPC | the same, plus a UDP port to open beside the TCP one |
| Cancellation | closes the TCP connection | `RST_STREAM`, connection kept | `RST_STREAM` | a close, reset after `StreamCloseTimeout`; no immediate reset | `STOP_SENDING` / `RESET_STREAM`, connection kept |
| Head-of-line under loss | per connection | every stream of a host | every stream of a host | every stream of a host | none between streams |
| Flow-control knobs | TCP buffers only | `HTTP2Config` windows (Go caps a stream at 4 MiB) | static or BDP-grown windows, message size limits | one stream window | stream and connection windows, auto-tuned |

The sizes come from four probe programs built the same way (`CGO_ENABLED=0`, linux/amd64,
`-trimpath -ldflags "-s -w"`): `net/http` with a TLS config as the baseline, then the same
plus a gRPC server and client, a yamux session pair, and a QUIC listener and dialer. They
are a lower bound on what tailraid would add; tailraid itself is 20.9 MB built that way.

How each fits the RPC's seams:

- **`rpc.Client` and `rpc.Server`.** `h2` is a transport setting: the handlers, status
  codes, headers and `FaultNet` stay. gRPC replaces the HTTP surface with a service
  definition and status codes; `mux` and `quic` replace it with a framing of tailraid's
  own, every route re-expressed as an op.
- **`FaultNet`** wraps an `http.RoundTripper`. `h2` keeps it. gRPC would need a dialer or
  interceptor of its own; `mux` a wrapper over the session; `quic` a `net.PacketConn`
  wrapper.
- **The WhoIs authorizer** keys on the peer's tailnet source address. Every TCP candidate
  has `RemoteAddr` per connection (per stream on yamux, from its session). QUIC has it per
  connection, but a connection survives a change of the peer's address (migration), so the
  check would have to run again on migration.
- **The busy reply (I-246)** is a 503 with a header. `h2` keeps it as is; gRPC would map it
  to `RESOURCE_EXHAUSTED`; the framings need a status byte.
- **Size-scaled deadlines (I-83)** are per request contexts with `ResponseHeaderTimeout`
  as the dead-peer bound. `h2` keeps both, but a stalled connection now fails every stream
  on it at once, and needs `ReadIdleTimeout` and `PingTimeout` so a dead peer is noticed.
  gRPC has keepalive pings and per-call deadlines. yamux and QUIC have stream deadlines and
  their own keepalives.

## Recommendation

Ranked, best first:

1. **Stay on HTTP/1.1.** It ties every candidate on bulk throughput without loss, and
   under loss, the case Wi-Fi and relayed tailnet paths produce, it is the only TCP option
   that does not put a host's whole traffic behind one connection: 56 MiB/s against 27–33,
   a hedged read's p99 of 683 ms against 1–1.3 s. Its costs are connection churn (one to
   three handshakes per cancelled read) and control calls queued behind bulk transfers
   (132 ms p99 against 55–61 ms on the tailnet path). Both have cheaper fixes than a new
   transport: a separate connection pool for control calls, which is half done since the
   control transport is its own `http.Transport` (I-255) and needs only to stop sharing a
   link-level queue with shard traffic, and reading a cancelled GET's remaining bytes when
   they are few instead of closing the connection.
2. **Cleartext HTTP/2, behind a version gate, if the churn shows up on the real cluster.**
   No new dependency, no binary growth, the same handlers and seams. It removes the churn
   and halves control-call latency beside bulk traffic, at the price of the loss behaviour
   above, a rolling-upgrade gate, and the need for `ReadIdleTimeout`. Measure it with real
   loss on the real tailnet first.
3. **yamux with tailraid's own framing.** The best control-call latency beside bulk
   traffic (37 ms p99 on the tailnet path) for under 1 % of binary size, but every route
   would be rewritten into a framing, `FaultNet` and the authorizer rebuilt around
   sessions, and it shares HTTP/2's loss behaviour and lacks an immediate stream reset.
4. **gRPC.** It matches `h2` on every number while adding three modules and about 35 % to
   the binary, and its service definitions and status codes replace the HTTP surface. It
   offers nothing tailraid lacks.
5. **QUIC.** The only option without head-of-line blocking between streams, and the best
   control-call latency under loss (38.5 ms p99), but its bulk throughput collapsed under
   the same loss (7 MiB/s), it is slowest on a fast LAN (185 MiB/s against 274), it adds
   about 14 % to the binary and a UDP port, and inside WireGuard it stacks a second
   congestion controller and encryption layer on a 1,280-byte MTU.
