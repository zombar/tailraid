# 08 — Security

tailraid is a home/small-team system that lives entirely inside one tailnet. The security
design leans on what the tailnet already provides and adds only what is needed to stop
accidents and casual misuse. It is explicit about what it does not defend against.

## Trust model

- **Transport**: every node-to-node byte travels over WireGuard tunnels managed by
  tailscaled (here a headscale control plane). Confidentiality, integrity and peer
  authentication of the link are Tailscale's job. tailraid uses no TLS of its own.
- **Identity**: a tailnet node is identified by its Tailscale `StableID`, obtained via
  `tailscale whois --json <ip>` for the source address of an incoming connection. tailraid
  trusts the local tailscaled's answer.
- **Perimeter**: anything that can reach a node's Tailscale IP is already a tailnet member.
  tailraid narrows that to *cluster members* so that shared, guest or unrelated devices on
  the same tailnet cannot read or write shards or metadata.
- **Local OS**: the NFS gateway and all `127.0.0.1` listeners trust the local operating
  system. A user who can run processes on a device can access that device's mount; that
  is the same trust as a local disk.

## Node RPC authorization (WhoIs authorizer)

All RPC endpoints except `GET /health` require authorization. `internal/rpc.WhoIsAuthorizer`
implements:

1. Extract the source IP from the TCP connection (never from headers).
2. Look up the member table (replicated in the FSM) by IP. No member → **403** before the
   body is read.
3. Call `WhoIs(ip)`. The returned `Node.StableID` must equal the member's recorded
   `TSStableID`. Mismatch → **403** and the cache entry for that IP is purged. This catches
   an IP that was reassigned to another device and a device that re-authenticated under a
   different identity.
4. Cache: positive results for 5 minutes, negative results for 30 seconds. The cache is
   purged entirely on every membership change event, so a removed member is rejected on
   its very next request even with a warm cache.
5. The `X-Tailraid-Cluster` header must equal this node's cluster ID → otherwise **409**.
   This stops two clusters that were separately `init`ed on one tailnet from exchanging
   shards or metadata. `409` is reserved for this; a refused shard `DELETE` (fence, `docs/06`)
   is `412`.
6. Exception for address changes: `POST /v1/members {update_addr}` arrives from an IP that
   is, by definition, not yet in the member table. It is authorized by `WhoIs(srcIP).StableID
   == TSStableID` of the member named in the body (step 3 without step 2), and may change
   nothing but that member's `Addr`.

Endpoints and who may call them:

| Endpoint | Caller | Note |
|---|---|---|
| `GET /health` | anyone on the tailnet | unauthenticated, no secrets in the body |
| `GET/HEAD/PUT/DELETE /v1/shards/{key}` | members | `DELETE` additionally needs `X-Tailraid-Log-Index` and passes the fence of `docs/06` or gets `412` |
| `POST /v1/apply`, `POST /v1/members`, `POST /v1/bad-slot` | members | `update_addr` per step 6 |
| `POST /v1/join` | any tailnet identity with the token | step 2 skipped, step 3 against the claimed StableID |
| `GET /v1/status` | members | |
| `/dev/*` | members, only with `dev.enabled` | |

Authorization is checked before any request body is consumed (`http.MaxBytesReader` is
installed after the check). A rejected PUT therefore costs the attacker's bandwidth, not
the node's disk.

## Join

`tailraid join <host> <token>` sends `POST /v1/join` with the joiner's claimed `StableID`,
name, address, ports, suffrage request and capacity. The receiving node:

- requires `WhoIs(src).StableID == claimed StableID` (the joiner is who it says it is),
- requires `token == Meta.JoinToken` from the FSM (constant-time compare),
- forwards a member-add to the Raft leader; the leader adds the server to the Raft
  configuration (voter or non-voter) and upserts the member record.

The token is 8 random base32 characters, created at `init`, shown by `tailraid token`, and
rotated by `tailraid token rotate` (a Raft `MetaSet`). It stops accidental joins to the
wrong cluster and joins by tailnet devices that are not meant to hold data. It is not a
secret of high value: anyone with it is still confined to the tailnet.

## Raft transport: cluster-ID handshake

hashicorp/raft's TCP transport has no authentication. A node from a different cluster
that dials a follower could, as "leader", overwrite its log. `internal/cluster.idStream`
wraps the `raft.StreamLayer`: on `Dial` and `Accept` each side sends its 16-byte cluster
ID and reads the peer's; on mismatch the connection is closed and the event logged. Raft
traffic is confined to Tailscale IPs, so the handshake only needs to defeat mistakes, not
an active attacker inside the tailnet (that attacker already owns a member device).

## Loopback-only listeners

- NFS: `127.0.0.1:20490` (Windows: `127.0.0.1:2049` and the PMAP stub on `127.0.0.1:111`).
  Never bound to a Tailscale or LAN address. `AUTH_NULL` is accepted because the OS is the
  boundary.
- `internal/rpc.Listen` refuses to bind a non-loopback address when no `Authorizer` is
  configured (errol rule), so a misconfiguration cannot expose unauthenticated RPC.

## S3 gateway (M5a)

The S3 API server listens on the node's Tailscale IP (and loopback). It requires **both**:

- a valid SigV4 signature with an access key created by `tailraid s3 keys create`
  (keys are stored only in the local config file with mode 0600 or the OS keyring; the
  secret never enters Raft), and
- a WhoIs check that the source IP is a tailnet node (not necessarily a cluster member, so
  phones and Windows Home machines can use rclone). The config key `s3.members_only: true`
  tightens this to cluster members through the same `rpc.Authorizer`.

Clock skew tolerance is 15 minutes, as in AWS. `UNSIGNED-PAYLOAD` is accepted because the
transport is already authenticated and encrypted; when a payload hash is supplied it is
verified.

## Development endpoints

`/dev/fsm-hash` (applied index and SHA-256 of the encoded FSM state), `/dev/corrupt?key=`
(flips a byte of a shard file's payload), `/dev/exit` (process exit 3 after the reply, for
a service manager to restart) and `/dev/fault` (per-peer drop rules on the Raft stream and
the node RPC, both directions; docs/12 §3) exist for the soak suite (`docs/09`); metrics come from `/v1/status`. They are
registered only when `dev.enabled: true` is set in the node's config; otherwise the routes
do not exist (404). When enabled they go through the same WhoIs member authorization as
every other endpoint, every node's `/health` carries `dev: true`, and `tailraid status`
prints a warning line for every node that has them enabled.

## Credentials and secrets

| Secret | Where it lives | Never |
|---|---|---|
| Join token | FSM `Meta` (replicated to all members) | printed in logs |
| S3 gateway access keys | node config (0600) or OS keyring | in Raft, in `status` output |
| Remote S3 node credentials (M5b) | node config (0600) or OS keyring on each device | in Raft |
| Cluster ID | `<data_dir>/cluster_id` and FSM `Meta` | — (not a secret) |
| Node ID | `<data_dir>/node_id` | — (not a secret) |

Shards are stored in plaintext on each node's disk. Encryption at rest is a non-goal
(see below); users who need it put the data directory on an encrypted volume.

## Threats and mitigations

| Threat | Mitigation |
|---|---|
| Unrelated tailnet device reads or writes shards | member table + WhoIs StableID check (I-18) |
| Stale or replayed shard `DELETE` removes a live shard | DELETE fence: log index, placement and file age checked by the receiver (`docs/06`) |
| Two clusters on one tailnet cross-talk | cluster-ID handshake on Raft, `X-Tailraid-Cluster` header on RPC |
| Tailscale IP reassigned to another device | WhoIs StableID mismatch → 403 + cache purge |
| Removed member keeps using a warm authorizer cache | cache purged on membership change |
| Accidental join to the wrong cluster | join token |
| Rogue "leader" overwrites a follower's log | handshake; Raft traffic only on Tailscale IPs |
| Corrupt or forged shard body | receiver verifies `X-Shard-SHA256`; reader verifies decoded chunk hash |
| Oversized body exhausts memory/disk | `MaxBytesReader(shardMax+1)`, PUT semaphore, 507 on no space |
| LAN host reaches NFS or RPC | NFS bound to loopback; RPC bound to Tailscale IP only |
| `/dev/*` abuse | only with `dev.enabled`; member-authorized; flagged in `status` |
| Join-token brute force | tailnet-only, member or not; 8 base32 chars; rate limit 1 attempt/s per source IP; rotate anytime |

## Non-goals (explicit)

- Defending against an attacker who controls a **member device** or the headscale control
  plane. Such an attacker can read all data and corrupt metadata; Raft and hashing detect
  accidents, not malice from an authorized member.
- Encryption at rest.
- Per-user access control inside the volume (POSIX modes are stored, not enforced across
  devices; S3 keys are per cluster, not per path).
- Audit logging.
- Protecting the local mount from other users of the same device.

## Invariants

- **I-18** — An RPC whose source IP is not a cluster member with a matching Tailscale
  StableID is rejected with 403 before any request body is read; a request whose
  `X-Tailraid-Cluster` header does not match this node's cluster ID is rejected with 409.
- **I-48** — A join succeeds only if the caller's WhoIs StableID equals the StableID it
  claims and the presented token equals the current join token; a failed join changes no
  Raft state.
- **I-49** — A Raft stream whose peer presents a different cluster ID is closed before any
  Raft message is processed.
- **I-50** — NFS, PMAP and all `127.0.0.1` listeners bind only loopback addresses; the
  RPC and S3 listeners bind only the node's Tailscale IP (and optionally loopback), and
  `rpc.Listen` refuses a non-loopback bind without an authorizer.

## Tests

| Invariant | Tests |
|---|---|
| I-18 | `TestWhoIs_DeniesUnknownPeer`, `TestWhoIs_DeniesStableIDMismatch`, `TestWhoIs_AllowsMember`, `TestWhoIs_PurgeOnMembershipChange`, `TestChunkRPC_ReadsAuthBeforeBody`, `TestRPC_ClusterIDMismatch409` |
| I-48 | `TestJoin_WrongToken403_NoRaftChange`, `TestJoin_StableIDMismatch403`, `TestJoin_Succeeds_AddsNonvoter` |
| I-49 | `TestIDStream_RejectsForeignCluster`, `TestCluster_TwoInitsDoNotMerge` |
| I-50 | `TestRPC_ListenRefusesNonLoopbackWithoutAuth`, `TestNFSGW_BindsLoopbackOnly`, `TestServe_ListenerAddresses` |
