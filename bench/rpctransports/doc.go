// Package rpctransports benchmarks transports for tailraid's node-to-node traffic: the
// node RPC's HTTP/1.1, cleartext HTTP/2, gRPC, a yamux stream multiplexer with a
// length-prefixed framing, and QUIC, on the same workloads over the same simulated links
// (docs/reports/rpc-transports-2026-10.md). It is a module of its own, so its
// dependencies stay out of tailraid's go.mod and vendor/; nothing in the node uses it.
//
//	cd bench/rpctransports && go test -run '^$' -bench . -benchtime 20x
package rpctransports
