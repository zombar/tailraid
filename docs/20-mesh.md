# 20 — The mesh: tailraid's own WireGuard network

Until this doc, tailraid lived on a Tailscale tailnet: a second daemon on every device,
a login per device, a control plane somewhere else, and `tailscale whois` as the
identity of every caller. The mesh replaces all of that with one WireGuard device per
node, driven by the member table the cluster already replicates. A device joins the
network and the cluster in one step, `tailraid join <code>`, and nothing but the
tailraid binary runs on it.

What Tailscale did for tailraid was never the data path: its data path is WireGuard
between peers, as ours is. It was the control plane (keys and endpoints exchanged
through its coordination server), the relays for pairs that cannot reach each other,
and the identity of a caller. Here the control plane is the member table (a row carries
the device's public key, its mesh address and its endpoint candidates), the identity of
a caller is the key that sealed its packet, and a relay is any member both sides can
reach, forwarding sealed datagrams it cannot read.

*Status (2026-10-14): designed; the PRs follow the order in `CLAUDE.md`. PR 1 built
`internal/mesh` (the keys, the join code, the introduce, the Bind with its fan-out, the
device and its peers, the path monitor, the interface tables and the NAT simulator of the
tests); PR 2 the identity in the FSM and the RPC (`Node.Key`, `Endpoints` and `MeshPort`,
`Meta.MeshSubnet`, the pending rows with ops 26–27 at codec 13, `cluster.Node.Introduce`,
the join from a pending row, `update_endpoints`,
the pending expiry, `rpc.TableAuthorizer` and `/health`'s `seen_from`); what each later PR
wires is a stub in `test/invariants/mesh_stubs_test.go`; PR 4 landed the privileges (§9:
the Linux unit's capability, the macOS LaunchDaemon with `mount.owner`, wintun.dll
embedded and written by `install` and `update` with the firewall rule, `uninstall
--forget-identity`, `giveBack` after a `join` under sudo on Linux); PR 3 put the node on it
(`internal/node/network.go`: the device opened before Raft, peers from the table, the
seed and the pending rows, the path monitor, the candidates published, the pending
expiry, `/health`'s `seen_from`, `status.mesh` and `node ls`'s PATH; `tailraid init`
prints the code, `join <code>` introduces and joins, `token` prints the code; the table
authorizer everywhere; `internal/tailscale` deleted; `dev.mesh_fake` for the CLI tests,
which cannot open a TUN). The release
that carries the mesh starts a new cluster: there is no migration from a Tailscale
cluster, by decision (the volume was being re-created anyway).*

## 1. The interface

Each node runs one wireguard-go device (`golang.zx2c4.com/wireguard`, pure Go, no
kernel module: the kernel's TUN driver is the only kernel part, and it ships with every
OS tailraid supports) on a real TUN interface, so the mesh addresses are ordinary
addresses of the host: Raft, the node RPC and the browser UI bind them exactly as they
bound the Tailscale address, the CLI dials its own node at that address, and the
compose rig's worker reaches its node the same way. Every connection a node or the CLI
opens leaves from the mesh address too: a dial made while the interface is absent (the
two seconds of a restart) fails at once instead of leaving by another interface with a
source no member admits, which a client's connection pool would otherwise keep reusing.

| | Linux | macOS | Windows |
|---|---|---|---|
| Device | `/dev/net/tun`, named `tailraid0` (`mesh.interface`) | `utun<n>`, the next free number | wintun, adapter "tailraid", GUID derived from the cluster ID so it is the same adapter across restarts |
| Address, MTU, route | `ip addr add`, `ip link set … mtu … up` | `ifconfig <utun> <addr> <addr> netmask …`, `ifconfig … mtu`, `route add -net <subnet> -interface` | `netsh interface ip set address`, `netsh interface ipv4 set subinterface … mtu`, `route add` |
| Privilege | `CAP_NET_ADMIN` (the unit grants it, §9) | root (the service is a LaunchDaemon, §9) | LocalSystem (the service already is) |
| The dll | — | — | `wintun.dll` beside the exe, written by `install`, `update` and whatever opens the adapter (a `join`, a `serve` by hand) when absent or different (§9) |

A packet the OS hands the tunnel for the device's own address goes straight back to the
kernel: macOS routes a host's traffic to its own utun address into the tunnel (the
point-to-point route's destination is the address itself), where WireGuard has no peer
for it, and a node could not reach its own listeners (the CLI's `status`, the browser,
the mount); Linux delivers such packets locally and never hands them over. Tailscale
loops them in userspace the same way.

The command sequences are lifted from tunnelmesh's `internal/tun/device.go` and live
in `internal/mesh/ifconfig_<os>.go` as command tables that `Apply` runs, tested like the
mount helpers of docs/07: the table is checked, the commands are not run in CI.

**MTU 1280.** The inner MTU is the IPv6 minimum: a relayed packet (§6) inside an IPv6
outer datagram is 40 + 8 + 33 + 32 + 1280 bytes and still fits a PPPoE or mobile path,
and neither wireguard-go nor the kernel discovers the outer path MTU. The cost is about
7 % more packets than 1420; on Linux the TUN is opened with its offloads (TSO/GRO), so
the per-packet cost stays off the CPU.

**One UDP socket**, dual-stack, on `mesh.port` (20483 by default), carries every
WireGuard packet (message types 1–4) and tailraid's three control frames, told apart by
the first byte: `0x10` the introduce (§4), `0x11` a relayed packet (§6), `0x12` a path
probe (§6). The socket is wireguard-go's own `conn.Bind` (`StdNetBind`) wrapped, so its
batching (`sendmmsg`/`recvmmsg`) and UDP GSO/GRO on Linux stay; its buffer sizes are the
OS's defaults until the bench of PR 5 says they should not be.

**The storage traffic on top is unchanged.** HTTP/1.1 for the node RPC and plain TCP
for Raft, as docs/00 and the transport study (`docs/reports/rpc-transports-2026-10.md`)
chose: the mesh encrypts and authenticates the link, as the tailnet did, and the
cluster-ID handshake, the `X-Tailraid-Cluster` header, the member check on a Raft
accept (I-92) and the member seed (I-134, I-281, I-282) stay as they are, with one
change: the seed now also feeds the WireGuard peer table (§3).

## 2. Identity

A device's identity is a Curve25519 key made once, at `init` or `join`. The private key
lives in the device's keychain (`internal/keychain`, docs/19 §2, account
`mesh-<cluster ID>`; on macOS the service is a root daemon with no session keychain, so
the key lands in the documented 0600 file fallback under the data directory). The public
key is the device's member row (`Node.Key`, 32 bytes), where the Tailscale stable ID
used to be. `reset` keeps the key as it keeps the config, so a reset device joins again
as itself and keeps its row, its index and its disks (§4); `uninstall --forget-identity`
removes it. The private key is never in the FSM, a snapshot, an export, a report, a log
line, an error or any request (I-316).

The cryptography is WireGuard's: Noise IKpsk2 over Curve25519, ChaCha20-Poly1305,
BLAKE2s, a handshake every two minutes, replay protection, cookies under load. tailraid
adds no cipher of its own to the data path.

## 3. Addresses and peers

**The subnet.** `Meta.MeshSubnet` is a /24 inside 100.64.0.0/10 (the shared address
space, never a home LAN) derived from the cluster ID: `100.(64 + id[0] mod 64).id[1].0/24`,
one of 16,384; `init --subnet` overrides it. A device that still runs Tailscale routes
100.64.0.0/10 to `tailscale0`; our /24 is more specific and wins, and a tailnet peer that
happens to sit inside it (about one chance in 16,384 per peer) is unreachable from that
device while the mesh is up, which is what the override is for.

**Allocation.** The leader gives each device an address at its introduce (§4): the
lowest free host of the subnet, hosts 1–254, `init`'s node taking .1. A host is free
while no device row holds it and no pending row does: a member's address is never reused
(as `Idx` is never reused, and a removed row keeps its address), but a pending row that
expired, was deleted or was ended by a token rotation frees its host, so a code holder
who introduces strangers' keys burns nothing for good. `Meta.NextMeshHost` is the
high-water mark `status` warns on at 200. `Node.Addr` is the mesh address
and never changes for the life of the row: the exception that let a non-member source
change an address (under Tailscale, an IP change) and the address watch in `serve` are
gone; `update_addr` is an admins' member op the leader refuses for any row that carries
a key (it serves the keyless test rigs alone). Inner
traffic is IPv4; the outer socket is dual-stack and a
global IPv6 address is an endpoint candidate (§5), often the best path there is.

**Peers.** A node's WireGuard peers are exactly the rows its authorizer admits (docs/08):
every device row that carries a key and is not `Removed`, every pending row (§4), and,
until the table has arrived from the log, the rows of the seed file (I-134, I-281,
I-282). Each peer has `AllowedIPs = Addr/32` and a 25 s persistent keepalive. The peer
set is diffed into the device the moment a node applies a membership entry (a row, its
state, a pending row, the token: a hook on the FSM's apply, coalesced into one refresh),
and again on the tick, so a removed member's next packet dies at the tunnel on every
node at once rather than at a 403 (I-317). Cryptokey routing is what makes the
authorizer simple: a packet from mesh address X was sealed by the key that owns X, so
"which row is at this source address" is the whole question; the table decides every
address it holds, and the seed file stands in only for one it does not hold yet, so a
row the table marks removed is refused whatever the seed still says (I-318).

**`mesh.json`.** `serve` writes `<home>/mesh.json` (`{addr, ifname, port}`, world-
readable) so the CLI finds its node without the tailnet status it used to ask; the
cluster ID comes from `/health`.

## 4. Joining: the code and the introduce

**The token** is 16 base32 characters (80 bits) rather than 8: under Tailscale a wrong
token could only come from a tailnet device; here the introduce is answered on an
internet-facing UDP port, and the token is the only thing that authenticates it.

**The join code** is one base32 string, printed by `init` and by `tailraid token`, of
`version ‖ cluster ID ‖ token ‖ bootstrap public key ‖ bootstrap mesh host byte ‖ mesh
port ‖ endpoints ‖ 4-byte check`. The endpoints are the bootstrap's candidates at the
moment of printing (its LAN addresses, its configured public endpoint, a mapped port,
§5 and §7), so `tailraid token` prints a fresh code whenever a device is about to join
from somewhere new; the token inside is stable until `token rotate`.

**The introduce.** `tailraid join <code> [--voter]` sends a 73-byte datagram to each
endpoint in the code, on the mesh socket: `0x10 ‖ cluster ID ‖ joiner public key ‖ unix
seconds ‖ HMAC-SHA256(k, the rest)` with `k = HKDF-SHA256(token, salt = cluster ID,
info = "tailraid introduce v1")`. A member that receives one verifies it (constant time,
the timestamp within 10 min of its clock, and after a failure one attempt per second per
source, as the join has always been throttled), forwards `POST /v1/members {op:
introduce, key}` to the leader, which allocates the address (§3) and proposes a
**pending row** (op 26, `PendingAdd{Key, Addr, At}`), waits for its own apply, and
answers 38 bytes: `0x10 ‖ addr ‖ prefix ‖ HMAC`. The reply is shorter than the request,
so the socket is not an amplifier; a bad HMAC gets silence; the same key introduced
again, with a pending or an active row, gets that row's address back (I-319). A source
gets one answered introduce a second as it gets one failed one. A pending row expires
after 10 min on the leader's tick (op 27, `PendingDelete`), and a token rotation ends
every pending row at once with the introduces that would follow: what the old code
bought, the old code loses.

Why not a shared "lobby" key derived from the token, so that the join needs no frame of
its own? Because wireguard-go's responder breaks it three ways: two joiners under one
static key clobber each other's session and endpoint, a later joiner whose clock is
behind an earlier one is refused by the TAI64N replay check until the peer is re-added,
and the lobby peer cannot be removed per join without killing the session the reply is
on. The introduce costs one frame type on a socket that needs frame types anyway (§6).

**Then the join is today's join.** With its address the joiner configures the interface,
adds the bootstrap as its first peer (the seed; under Tailscale this was the WhoIs of
the join target), opens its Raft listener at `addr:raft` (so the leader's first dial
succeeds, as before), and calls `POST /v1/join` through the tunnel from its pending
address with its name, ports, suffrage request, capacity and `seen_from`, the UDP source
the bootstrap saw its introduce from (its first reflexive endpoint, §5). No token in
the request: the introduce consumed it, and the pending row is the proof. The leader
adds the Raft server and proposes the row, in that order as before (the row's entry is
then the newest in the log, which is what lets the joiner's first snapshot follow its
apply); the row's upsert turns the pending row into the member row (same `Idx`, `Key`
and `Addr`) and removes the pending entry; the response carries the member rows, and the
joiner seeds its authorizer and peer table from them, writes `members.seed`, registers
its disks, waits for the log and snapshots, as docs/02 §Membership says (I-321). The
joiner holds the bootstrap as a peer before the call, so the leader's first dial is
accepted when the bootstrap leads; otherwise Raft's retry covers the seconds until the
response names the leader.

**A re-join after `reset`** is an introduce with the device's existing key: the leader
finds the active row by key and answers its address, and the `/v1/join` from that
address under the new node ID re-keys the row to the new ID and replaces the stale Raft
server (by the row's previous node ID, and by address for the keyless rigs: the address
is cryptographic, so no identity compare is needed), whatever Raft port the device comes
back with. The device keeps its index, its address and its disks; the FSM holds one
active device row per key, so a re-join never leaves two. A removed
row's key gets a fresh pending row and a fresh address, and every member takes the
device back as a peer: what a removal retires is the row (its index and its address),
never the key.

## 5. Paths: candidates, fan-out, pinning

WireGuard sends a peer's packets to one endpoint and learns a new one from any
authenticated packet (roaming). tailraid's `conn.Bind` adds the part a coordination
server used to do: finding that endpoint.

**Candidates.** Each row carries `Endpoints`, at most 8: every non-loopback,
non-link-local interface address of the device with its mesh port; `mesh.endpoint`, a
public `host:port` the operator knows (a port forward, the cloud voter's own address; a
name is resolved by the device that publishes it, each time it publishes, so a DDNS
name reaches the peers as the addresses it points at today, and a peer never resolves
a name);
the reflexive addresses it has learned (the `seen_from` of its introduce, and the
`seen_from` every `/health` reply carries: the UDP source the responder's device
currently sees for the caller, on a direct path only; a caller it reaches through a
relay or is still fanning out to gets none, since the relay's address is not the
caller's); and, with §7, the port a router mapped. A node publishes
its candidates with the member op `update_endpoints`, for its own row only, at most once
a minute and only when they changed (I-322).

**Fan-out.** A peer with no session has the endpoint `fanout:<idx>`: a packet to it is
sent to every candidate at once. Only handshake initiations and keepalives are ever sent
that way, because the first authenticated packet back pins the peer to its real source
(roaming) and every later `Send` carries that endpoint, so data is never fanned out.
Every node↔node pair keeps a 25 s keepalive, so both sides fan out at the same time,
which is what opens a cone NAT on each side: each side's first packet creates the
mapping the other side's packet then passes. A monitor reads each peer's received
bytes and `last_handshake_time` every 10 s and returns a peer to fan-out when nothing
has arrived from it for 75 s (three keepalives; a handshake alone is no sign of life,
WireGuard renews one only every two minutes), so a peer that moved, slept or lost its
mapping is found again within a tick (I-323).

**What fan-out cannot do**: two devices both behind symmetric NATs (each mapping is per
destination, so a reflexive address learned from one member is wrong for another), or
one behind a firewall that drops unsolicited UDP. Such a pair reaches each other through
a member both can reach (§6), at that member's cost; a pair with no member in common
is dead to each other exactly as a pair without a tailnet path was: Raft tolerates it
while a majority sees the leader, and shard traffic between the two fails until a path
exists. A direct path (a LAN, IPv6, a port forward, a mapped port, §7, or the always-on
cloud voter's `mesh.endpoint`) is the fast one, and the always-on voter's public
address is what makes every pair at least relayed.

**Path class.** `mesh.Path(idx)` reports `{Class, RTT, Since}` for every peer: `lan`
(the pinned endpoint is on one of this host's own subnets), `wan` (any other direct
endpoint), `relay` (§6) or `none`. The class is what the storage code uses (§8); the
prober's liveness is unchanged (a peer with no path fails its probe and is dead, as
today).

## 6. The relay and the path probe

**Relay.** For each live member M with a direct path, a peer gets the candidate
`via M`, tried once its direct candidates have failed for 10 s. A packet sent `via M`
goes to M's pinned endpoint as `0x11 ‖ sender key ‖ HMAC ‖ WireGuard packet`; M checks
the HMAC (below), looks the destination up among its own peers (a relay forwards only
between two devices it holds sessions with), and forwards the WireGuard packet to the
destination's pinned endpoint with the sender's key in front, so the receiver's reply
goes back `via M`. The relay never holds a key that opens the payload: a relayed packet
is end-to-end WireGuard between the two peers, as a DERP-relayed packet was. A relay
forwards at most `mesh.relay_rate` (20 MiB/s) and counts what it forwarded per pair
(`status.mesh.relayed_bytes`); a device on a relayed path retries its direct candidates
every 30 s and moves back the moment one answers (I-328).

**The HMAC** under the cluster key (docs/19 §3, made by the leader on its first tick and the key every member fetches from
another) with a timestamp authenticates relay frames and path probes: a relay forwards
nothing it cannot verify, so it is never an amplifier for a non-member, and a probe
cannot be forged. A device that has not fetched the cluster key yet (a fresh joiner
before its first fetch) uses direct paths only; the fetch itself runs over its first
direct path, to the bootstrap.

**The path probe.** A device pinned on a `wan` or `relay` path sends `0x12 ‖ kind ‖ its key ‖
the candidate's key ‖ unix seconds ‖ HMAC` to its better-class candidates every 60 s;
the device the probe names answers with the frame marked an echo under a MAC of its own,
any other member drops it, and a candidate whose echo comes back naming that device
becomes the endpoint (so a candidate that is really another member, a relay, a UDP echo
service or the prober's own address pins nothing: the probe reflected unchanged is no
echo), and the device sends
a keepalive on it at once, so the peer, still sending through the relay, learns the
direct address and moves too; without that the peer's next relayed packet would roam
the device back to the relay before anything had gone direct. A laptop that comes
home moves from the public path to the LAN within a minute and without a restart
(I-329).

## 7. Port mapping

On the node's tick the mesh probes the default gateway (the routing table on Linux,
`route -n get default` on macOS, `route print` on Windows), PCP first and NAT-PMP next,
asks it to map `mesh.port` for an hour, refreshes the mapping at half its lifetime,
publishes `<external address>:<external port>` as a candidate (an external address no
peer reaches, one that is unspecified, loopback, private, link-local, multicast or in
the shared space the mesh lives in, or a port of 0, is a gateway behind another NAT or
misconfigured: the mapping is given back, nothing is published, and it is tried again
every minute) (so it also lands in the
join code this device prints), and releases the mapping on shutdown. Once a minute it
asks the gateway's epoch: one that went back is a router that restarted and lost its
mappings (RFC 6886 §3.6), and the device maps again at once; a gateway that stops
answering drops the mapping and is probed again every minute. `status.mesh.portmap`
says `pcp`, `natpmp` or `none` and the external endpoint; `mesh.portmap: false` turns it
off (I-330). With it, a home node behind an ordinary router is reachable directly from
anywhere, which is what makes the join code enough for a device joining from outside.
The clients are written after tunnelmesh's `internal/portmap`; `internal/mesh/portmap`
also holds the responder the tests and the NAT rig's routers run (`soak-client natpmp`),
which turns every mapping into a DNAT rule there.

## 8. What the storage code does with the path

Placement is a pure function of the hash and the membership (docs/03, I-294) and the
path never enters it: where a chunk lives does not depend on who can reach whom today.
What the path changes is ranking and scheduling (I-327):

- **Holder ranking.** `Prober.Rank` orders holders by `(class, RTT)` after this host's
  own shards and own disks and before S3 nodes, so within the reader's tiers (own shards,
  then data, then parity, L-63) a LAN holder is asked before a WAN one and a relayed one
  last; a hedged GET skips relayed holders while k direct holders are live, because the
  relay is somebody's egress.
- **Straggler grace.** The grace a chunk's PUTs get past k acks (4 × the p95 of recent
  PUTs, I-169) keeps its p95 per class, so a slow WAN member does not stretch the grace
  for LAN PUTs and a LAN straggler is cut sooner.
- **Background work.** The repairer and the scrub defer a move, fill or scrub read whose
  only source or target is relayed, unless the slot is bad or unreadable
  (`repair.relay: avoid`, the default; `allow` for a cluster that is mostly relayed);
  readahead to a relayed holder is capped at one chunk.
- **Later, measured first:** readahead depth from the holder's bandwidth-delay product,
  and a PUT concurrency per class.

## 9. Privileges and the service

A TUN needs a privilege on every OS, and that is the one cost of a real interface over
a userspace stack (which would have needed none, and would have hidden the mesh from the
OS, the CLI and the rigs).

- **Linux.** The unit keeps `User=<installing user>` and gains
  `AmbientCapabilities=CAP_NET_ADMIN` and `CapabilityBoundingSet=CAP_NET_ADMIN`
  (`NoNewPrivileges` keeps ambient capabilities; `/dev/net/tun` is reachable, since the
  unit sets no `PrivateDevices`); `After=network-online.target` alone, no tailscaled. A
  `join` run by hand needs the capability too, so it runs under `sudo`, writes into the
  invoking user's config home (`SUDO_USER`'s `~/.config/tailraid`, the one the user's
  unit names: root's own home only when nothing says who invoked it) and gives what it
  wrote back to `SUDO_UID:SUDO_GID`, the disk and mirror directories included (their
  markers and shard directories are root's otherwise); `init` needs no TUN.
- **macOS.** A utun needs root, so `serve` runs as a LaunchDaemon
  (`/Library/LaunchDaemons/app.tailraid.serve.plist`, the `system` domain: it starts at
  boot, not at login) and `config.Home()` as root is `/Library/Application
  Support/tailraid`, with the log at `/Library/Logs/tailraid/serve.log`. `init`, `join`,
  `install`, `start`, `stop`, `update --restart`, `export`, `recover` and `reset` run
  under `sudo`; `status`, `dashboard`, `trash`, `cache`, `search`, `node add-disk`,
  `mirror add`, `verify` and the browser do not (they take the cluster ID from the node's
  status or `/health`, never from the data directory, which is root's)
  (the CLI finds the node through `mesh.json` in the system home and the cluster ID
  through `/health`). `install` records `mount.owner` from `SUDO_UID` and `SUDO_GID`, so
  the user's NFS mount shows their ownership. (A
  privileged helper that only creates the utun and passes its descriptor to an
  unprivileged `serve` was considered and not chosen: 250 lines of privileged code, a
  socket protocol and a second plist, for a per-user data directory.)
- **Windows.** The service already runs as LocalSystem; `depend= Tailscale` goes.
  `wintun.dll` (signed, redistributable under its prebuilt-binaries licence, which ships
  in the release) is embedded in the Windows binary and written beside the exe by
  `install` and by `update` when absent or different; `install` adds two `netsh
  advfirewall` inbound rules, one for the UDP port and one for the program's TCP
  listeners (the wintun adapter is an unidentified network, which Windows files under
  the public profile and blocks unsolicited inbound TCP on: without the rule the
  leader's Raft dial to the node timed out), and `uninstall` removes them and the dll
  (I-324).

`tailraid update --restart` recreates the interface with the same address and port, so
a router's mapping and the peers' pinned endpoints usually survive; peers re-handshake
within a retry (docs/12 §5 unchanged).

## 10. Status, config and the soak

`tailraid status` gains a `mesh` block: the device's address, interface, port and
port-mapping state, and for every device member its path class, endpoint, handshake age, how long since it was last heard from
and bytes each way; `node ls` gains a `PATH` column; the machine vitals' network
counters (docs/12 §8) are the mesh interface's, `mesh_rx_bytes` and `mesh_tx_bytes`
(I-325). The dashboard draws the same block as a picture, the Mesh section of docs/13
§7: this device at the centre, the members on a ring, the edges in their path classes,
the facts in a table under it, each member a link to its own dashboard (I-331). The
Stats page keeps the history (docs/17 §1 and §4): every node's rows carry its path class
and round trip to each peer and the bytes it relayed, drawn as the paths lanes and the
round-trip line of its node section (I-332).

| Key | Default | Meaning |
|---|---|---|
| `mesh.port` | 20483 | the UDP port of the device |
| `mesh.endpoint` | — | a public `host:port` the operator knows reaches this device |
| `mesh.subnet` | derived | `init` only: the cluster's /24 |
| `mesh.address` | written by `init`/`join` | the device's address; survives `reset` |
| `mesh.interface` | `tailraid0` / `tailraid` | the interface name on Linux and Windows |
| `mesh.portmap` | `true` | ask the router for a mapping (§7) |
| `mesh.relay_rate` | 20 MiB/s | what this device forwards for others (§6); `"0"` lifts the limit |
| `repair.relay` | `avoid` | background work on relayed paths (§8) |
| `mount.owner` | from `SUDO_UID:SUDO_GID` at `install` | macOS: `uid:gid` the gateways give files, so the user's mount shows their ownership under the root daemon |
| `dev.mesh_fake` | — | with `dev.enabled`: a JSON file `{"addr": "127.0.0.1"}` replacing the device for the CLI tests, which cannot open a TUN; the node binds that address and joins by the code's token without an introduce (docs/09 §3) |

Gone: `tailscale.cli`, `dev.tailscale_fake` (`Validate` names them with the hint).

**Codec 13.** `Node.Key`, `Node.Endpoints`, `Node.MeshPort`, `Meta.MeshSubnet`,
`Meta.NextMeshHost`, the pending rows and ops 26–27 are codec 13 (12 is sealed S3's,
docs/10 §4.7) and snapshot frame 13; a reader decodes every lower version and discards
the Tailscale stable ID. `serve` refuses a state whose device rows carry no key with the
re-create instructions, as it refuses a pre-disks state (I-326).

**The soak** (docs/09 §6) gets three phases of its own: 25 `mesh join` (a late joiner
by a code printed on a non-bootstrap node, a rotated token, a re-join after reset, a
removal), 26 `mesh paths` (every pair direct; a UDP cut and its recovery; a restart; the
relay round) and 27 `mesh nat` on a rig of its own, `test/soak/docker-compose.nat.yml`,
with two router containers (one cone, one symmetric) and the fake NAT-PMP/PCP responder
(hole punching, the relay where it is forced, the mapping); the cone router hairpins a
flow its mapping lets in to its own inner address, since the Docker bridge drops a new
flow from an address off its subnet, so the cone side sees the mapped-in peer at its
router. The report gains a paths
table, a lanes chart of each pair's path over the run, the mesh counters and the join
round-trip table.

## 11. Later

- **Client peers.** A phone or laptop that is not a node: `tailraid client add <name>`
  would make a row with a key and an address and no Raft, no disks and no placement, and
  print a standard WireGuard configuration (a QR code for the official app) peering with
  every node that has a public endpoint; the browser UI would admit members and clients.
  Until then the browser UI admits cluster members only (docs/13 §2).
- **Users and groups** (after 1.0; designed in docs/21). The mesh makes a device's key
  its identity; a permissions system needs a second layer, who a key belongs to. The client rows above
  are where it goes (a row carries an owner and groups), the check runs in the FSM's
  apply path as the protected flag does (docs/13), so every mount and the browser agree,
  and the authorizers already answer with an `rpc.Principal` (a member row today, a
  user with groups then) rather than a bare row, so the rows are not reshaped later.
- **Re-keying** a device without a reset, and rotating the cluster subnet.

## Invariants

- **I-316** — A device's identity is a Curve25519 key made once at `init` or `join`:
  the private key is kept only in the device's keychain or its 0600 file fallback, kept
  by `reset` and removed only by `uninstall --forget-identity`, and is never in the FSM,
  a snapshot, an export, a report, a log line, an error or any request; the public key
  is the device's member row's `Key`.
- **I-317** — Raft, the node RPC and the browser UI bind only the device's mesh address
  (and the loopback listeners of I-50), and every connection the node or the CLI opens
  to a member leaves from it; the device's WireGuard peers are exactly the
  device rows with a key that are not `Removed`, the pending rows and, until the table
  has arrived, the seed rows, each with `AllowedIPs` of its address alone; a packet from
  any other key is dropped by the tunnel, a packet the OS hands the tunnel for the
  device's own address is handed back to the kernel, and a membership change reconfigures the peers
  before the authorizer sees it, so a removed member's next packet dies at the tunnel.
- **I-318** — A request is admitted when its source address is that of an active device
  row (from the table, or the seed until the table arrives) and nothing else is checked;
  a pending row's source may call only `GET /health` and `POST /v1/join`; every other
  source is refused with 403 before any body byte is read; there is no identity cache to
  purge.
- **I-319** — An introduce is accepted only with a valid HMAC under the current token
  and a timestamp within 10 minutes; it yields a pending row with an address the leader
  allocated, the same address again for a key that already has a pending or active row,
  and silence otherwise; a source gets one attempt per second, answered or failed; the
  reply is shorter than the request; a token rotation invalidates later introduces and
  ends every pending row, a pending row expires after 10 minutes, and a host a pending
  row held is allocated again while a member's address never is.
- **I-320** — The mesh subnet is derived from the cluster ID by a pure function, the same
  on every node, unless set at `init`; addresses are allocated by the leader from
  `Meta.NextMeshHost`, never reused, `init`'s node taking .1; a row's address never
  changes.
- **I-321** — `tailraid join <code>` with no other argument joins the network and the
  cluster: the joiner's listeners open on the address the introduce gave before
  `/v1/join`, the join completes the key's pending row into its member row, and the
  joiner's first handshake with every member follows within 10 s; a re-join after `reset`
  presents the same key, keeps the row's index, address and disks and replaces the
  stale Raft server; a code whose cluster ID is not the cluster's is refused at once.
- **I-322** — A device's endpoint candidates are its non-loopback, non-link-local
  interface addresses with its mesh port, `mesh.endpoint`, the reflexive addresses it
  learned from `seen_from` (its introduce, every `/health` reply, which reports a
  direct path's source only) and a mapped port, deduplicated and at most 8; `update_endpoints` (as `set_version`, the other unsigned member op) is accepted for the caller's own row
  only, and published at most once a minute and only when the set changed.
- **I-323** — A peer without a session is sent to every candidate at once and pinned to
  the source of the first authenticated packet; data is never fanned out; a peer not
  heard from (no packet, no handshake) for 75 s returns to fan-out within 10 s; two devices behind
  cone NATs, and one behind a cone NAT and one behind a symmetric NAT, reach each other
  directly.
- **I-324** — The Linux unit runs as the installing user with `CAP_NET_ADMIN` as its
  only ambient and bounding capability; the macOS service is a LaunchDaemon in the
  `system` domain with its home under `/Library/Application Support/tailraid`; the
  Windows service runs with `wintun.dll` beside the exe, written by `install`, by
  `update` and by whatever opens the adapter (a `join` runs before any `install`) when
  absent or different, and two inbound firewall rules, one for the UDP port and one
  for the program's TCP listeners, all removed by `uninstall`; a `join` under sudo on Linux writes into the invoking
  user's config home, the one that user's unit names, and gives the data, config, disk
  and mirror directories back to that user; `serve` listens only once the interface's
  address takes a bind (Windows holds a fresh address tentative for a moment); `init`
  creates no interface.
- **I-325** — `status` reports, for every device member, its path class, endpoint,
  handshake age and bytes each way, and the device's own address, interface, port and
  port-mapping state; `node ls` shows the path; the vitals' network counters are the
  mesh interface's, cumulative and never decreasing between two reports from one process.
- **I-326** — `Node.Key`, `Node.Endpoints`, `Node.MeshPort`, `Meta.MeshSubnet`,
  `Meta.NextMeshHost`, the pending rows and ops 26–27 are codec 13 and snapshot frame
  13; a reader decodes every lower version and discards the Tailscale stable ID; `serve`
  refuses a state whose device rows carry no key, with the re-create instructions.
- **I-327** — `Prober.Rank` orders holders by `(path class, RTT)` after this host's own
  shards and own disks and before S3 nodes, the same on every node for the same path
  table; a hedged GET skips relayed holders while k direct holders are live; the
  straggler p95 is kept per class; placement never depends on the path.
- **I-328** — A relayed packet is forwarded only by a member that holds sessions with
  both ends, only with a valid HMAC under the cluster key, size-preserving and within
  `mesh.relay_rate`; the relay never holds a key that opens the payload; a relayed path
  is tried only after the direct candidates failed for 10 s and is left for a direct
  path within 30 s of one answering; two devices behind symmetric NATs reach each other
  through a member both reach.
- **I-329** — A device pinned on a `wan` or `relay` path probes its better-class
  candidates every 60 s with an authenticated frame naming the candidate's device, which
  that device alone answers with an echo under its own MAC (the probe reflected
  unchanged is no answer), moves to a candidate that answers within two probe
  intervals and sends a keepalive on it at once, so the pin holds against a peer still
  sending through the relay; the repairer and the scrub defer background work
  whose only source or target is relayed unless the slot is bad, and readahead to a
  relayed holder fetches at most one chunk.
- **I-330** — With `mesh.portmap` on, a device behind a gateway that answers PCP or
  NAT-PMP holds one mapping for its mesh port, refreshed at half its lifetime and
  released at shutdown, and publishes the external endpoint once as a candidate; with it
  off it asks nothing; a gateway that stops answering restarts the probe at the next
  tick.

## Tests

| Invariant | Tests |
|---|---|
| I-316 | `TestKey_GeneratePublicParse`, `TestKey_KeychainLoadCreateForget` |
| I-317 | `TestMesh_PeersExchangePackets`, `TestMeshPeers_FromRowsAndPending`, `TestMembershipOp_KicksPeerRefresh`, `TestMeshPeers_RemovedKeyReturnsAsPending`; stub `TestStub_I_317` for the listeners on a real interface (the VM rig) |
| I-318 | `TestTableAuthorizer_Table`, `TestTableAuthorizer_RemovedRowBeatsSeed`, `TestTable_DeniesStrangerPendingAndRemoved`, `TestWebUI_Authorize_MembersOnly`, `TestMemberOp_UpdateAddrRefusedOnMesh`, `TestNodeUpsert_RefusesDuplicateKeyOrAddr` |
| I-319 | `TestIntroduceServer_Table`, `TestIntroduce_ReplyDecode_Table`, `TestIntroduce_EndToEnd`, `TestPendingApply_Table`, `TestMesh_IntroduceJoinExpire`, `TestErrorCode_MeshSentinels`, `TestIntroduceServer_SuccessRateLimit`, `TestPendingApply_HostsReusedAndRotationEndsPending` |
| I-320 | `TestSubnet_FromClusterID_Table`, `TestSubnet_HostAddr_Bounds`, `TestPendingApply_Table`, `TestMesh_IntroduceJoinExpire`, `TestPendingApply_HostsReusedAndRotationEndsPending` |
| I-321 | `TestJoinCode_RoundTrip`, `TestJoinCode_Endpoints_Table`, `TestJoinCode_Refusals_Table`, `TestIntroduce_EndToEnd`, `TestMesh_IntroduceJoinExpire`, `TestJoin_TokenAndKey`, `TestCLI_Join_NoFlags`, `TestJoin_RejoinNewRaftPortKeepsOneRow`; stub `TestStub_I_321` for the join over a real interface (the VM rig) |
| I-322 | `TestLocalEndpoints_NeverLoopbackOrMesh`, `TestPendingApply_Table`, `TestMembers_UpdateEndpointsOwnRowOnly`, `TestMesh_IntroduceJoinExpire`, `TestCandidates_OrderDedupCap`, `TestCandidates_ConfiguredNameResolvedAtPublish`; stub `TestStub_I_322` for the publishing on a real interface |
| I-323 | `TestBind_FanoutPinsOnFirstAuthenticatedPacket`, `TestBind_FanoutWithoutCandidates`, `TestMesh_DeadPathReturnsToFanout`, `TestMesh_KeepaliveIsOnByDefault`, `TestNAT_ConeToCone_Punches`, `TestNAT_ConeToSymmetric_Punches`, `TestNAT_SymmetricToSymmetric_NeedsARelay` |
| I-324 | `TestCommands_PerOS`, `TestApply_OptionalStepsMayFail`, `TestCreateTUN_NeedsAPrivilege`, `TestExecRunner_ReportsTheCommand`, `TestSvc_MeshPrivileges`, `TestEnsureDLL_WritesOnceAndReplaces`, `TestEnsureDLL_NothingOffWindows`, `TestMountOwner_Table`, `TestPrivilege_GiveBackAndSudoOwner` |
| I-325 | `TestClassify_Table`, `TestLocalAddr_MeshFileThenConfig`; stub `TestStub_I_325` for `status` on a real interface |
| I-326 | `TestMeshOps_Codec13`, `TestMeshState_Snapshot`, `TestKeylessCheck` |
| I-327 | `TestProber_RankByClass_Determinism`, `TestReader_HedgeSkipsRelayedHolders_Table`, `TestFlusher_StragglerGrace_PerClass` |
| I-328 | `TestNAT_SymmetricToSymmetric_NeedsARelay` (the need), `TestNAT_SymmetricToSymmetric_ThroughARelay`, `TestRelay_Codecs_Table`, `TestBind_RelayForwardRules`, `TestRelay_LeavesForADirectPath` |
| I-329 | `TestRelay_Codecs_Table`, `TestBind_RelayForwardRules`, `TestRelay_LeavesForADirectPath`, `TestProbeTargets_Table`, `TestRepair_RelayedPathsDeferred`, `TestReader_Readahead_RelayedHolderOneChunk` |
| I-330 | `TestClients_MapRefreshRelease`, `TestMapper_Lifecycle`, `TestMapper_RejectsUnusableExternal_Table`, `TestParseProcRoute_Table`, `TestCandidates_OrderDedupCap` |
