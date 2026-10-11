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
- **Local OS**: the NFS and SMB gateways and all `127.0.0.1` listeners trust the local operating
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
4. Cache: positive results for 5 minutes, negative results for 30 seconds; concurrent
   misses for one address share a single WhoIs call. The cache is
   purged entirely on every membership change event, so a removed member is rejected on
   its very next request even with a warm cache.
5. The `X-Tailraid-Cluster` header must equal this node's cluster ID → otherwise **409**.
   This stops two clusters that were separately `init`ed on one tailnet from exchanging
   shards or metadata. `409` is reserved for this; a refused shard `DELETE` (fence, `docs/06`)
   is `412`.
6. Exception for address changes: `POST /v1/members {update_addr}` arrives from an IP that
   is, by definition, not yet in the member table. When step 2 fails, the body (64 KiB at
   most) is read to learn the op; `update_addr` is then authorized by
   `WhoIs(srcIP).StableID == TSStableID` of the member named in the body (step 3 without
   step 2) and may change nothing but that member's `Addr`; any other op from a non-member
   is `403` (I-93).

Endpoints and who may call them:

| Endpoint | Caller | Note |
|---|---|---|
| `GET /health` | anyone on the tailnet | unauthenticated, no secrets in the body |
| `GET/HEAD/PUT/DELETE /v1/disks/{id}/shards/{key}` | members | `DELETE` additionally needs `X-Tailraid-Log-Index` and passes the fence of `docs/06` or gets `412`; `X-Tailraid-Priority: background` (the repairer's requests) holds at most half the node's slots (06 §4, I-175) |
| `POST /v1/apply`, `POST /v1/members`, `POST /v1/bad-slot` | members | `update_addr` per step 6 |
| `POST /v1/join` | any tailnet identity with the token | step 2 skipped, step 3 against the claimed StableID |
| `GET /v1/status` | members | |
| `/dev/*` | members, only with `dev.enabled` | |
| web UI (`<tsIP>:20482`): `GET /`, `/assets/*`, `/api/*`, `GET`/`HEAD /files/*` | members, or any tailnet device logged in as this node's user | docs/13 §2; no cluster header |
| web UI: `PUT /files/*`, `POST /api/mkdir` | the same, plus the `X-Tailraid-UI` header | no CORS, so a cross-site page cannot send the header |

Authorization is checked before any request body is consumed (`http.MaxBytesReader` is
installed after the check). A rejected PUT therefore costs the attacker's bandwidth, not
the node's disk. Every body is bounded (I-116): a shard PUT by the chunk size, `/v1/apply`
at 4 MiB, the JSON of `/v1/join`, `/v1/members`, `/v1/admin` and `/dev/fault` at 64 KiB,
`/v1/bad-slot` and the web UI's single-entry routes (`mkdir`, `rename`, `restore`,
`purge`, `flags`, `pin`) at 4 KiB, its batch routes (`move`, `delete`) and a thumbnail
`PUT` at 256 KiB (docs/13 §3); a body past its bound is a `400` before anything is
decoded.

## Join

`tailraid join <host> <token>` sends `POST /v1/join` with the joiner's claimed `StableID`,
name, address, ports, suffrage request and capacity. The receiving node:

- requires `WhoIs(src).StableID == claimed StableID` (the joiner is who it says it is),
- after a failed attempt accepts one more from that source IP per second (`429` otherwise); successful joins are never throttled,
- requires `token == Meta.JoinToken` from the FSM (constant-time compare),
- records the caller's source address: a request that names another address is `403`
  (I-94), so a token holder cannot point a member record at a different device,
- forwards a member-add to the Raft leader; the leader adds the server to the Raft
  configuration (voter or non-voter) and upserts the member record. A re-join under a
  known node ID keeps its index only when it presents that record's StableID; node IDs
  are public (`/health`), the StableID is what WhoIs vouched for (I-94).

The response lists the current device members. The joiner seeds its authorizer with
them so that it accepts the leader's Raft connection while its own member table is still
empty (the table replaces the seed as soon as the log arrives; §Raft transport); before
the request it seeds the target itself from WhoIs, so the leader's first connection is
not refused, and it persists the response's members as `<data_dir>/members.seed`, which
`serve` loads at start and keeps current from its table once a minute (`reset` removes
it). The file matters on every restart, not only the first: Raft restores a follower's
table at open from a snapshot only, so a node restarted before its first snapshot has an
empty table until the leader connects, and the leader was accepted only by that table;
`join` also takes a snapshot once the log has arrived. A node with neither a table nor a
seed file (it joined before the file existed, or the file was lost) replays the member
entries its own log already holds into a scratch table and seeds from that: the log was
sent by the leader the join accepted, so it names only members that leader vouched for,
and without it the node would refuse the leader forever and never receive its table
(I-134). A join from the Raft address of a known server under a new node ID (a device that
was `reset`) removes that server and re-keys the device's row to the new ID, so its disks
and their shards stay registered under it, when the caller presents the row's StableID; a
different tailnet device at a known address is refused (I-94).

The token is 8 random base32 characters, created at `init`, shown by `tailraid token`, and
rotated by `tailraid token rotate` (a Raft `MetaSet`). It stops accidental joins to the
wrong cluster and joins by tailnet devices that are not meant to hold data. It is not a
secret of high value: anyone with it is still confined to the tailnet.

## Raft transport: member check and cluster-ID handshake

hashicorp/raft's TCP transport has no authentication: whoever can send AppendEntries or
InstallSnapshot to a follower rewrites its metadata. `internal/cluster.idStream` wraps the
`raft.StreamLayer` with two checks.

1. **Membership on accept (I-92).** Before a byte is read, an accepted connection's source
   IP goes through the same rule as an RPC (steps 2–3 above, the one `WhoIsAuthorizer`):
   a member record at that address whose `TSStableID` matches `WhoIs(src)`. Anything else is
   closed. This is what keeps a shared, guest or unrelated tailnet device out of Raft; the
   perimeter claim of the trust model holds for Raft exactly as for RPC. A joiner's table is
   empty until the log arrives, so `join` seeds the authorizer with the members the join
   response listed (§Join); the leader that dials it is one of them.
2. **Cluster ID.** Each side then sends its 16-byte cluster ID and reads the peer's; on
   mismatch the connection is closed and the event logged (I-49). The dialer writes first
   and the acceptor reads first, so an accepted peer learns nothing before it has shown its
   own ID. This stops two clusters that share a tailnet from exchanging traffic; it is not
   a secret (`/health` publishes it), the member check is the gate.

Raft traffic is confined to Tailscale IPs; an attacker who controls a member device is a
non-goal (below).

## Loopback-only listeners

- NFS `127.0.0.1:20490` (macOS, Linux) and SMB `127.0.0.1:20445` (Windows, docs/15). Never
  bound to a Tailscale or LAN address. NFS accepts `AUTH_NULL` because the OS is the
  boundary; SMB requires the generated share password because Windows refuses guest
  sessions, not because the password adds a boundary: any local process can read the 0600
  config that holds it.
- `internal/rpc.Listen` refuses to bind a non-loopback address when no `Authorizer` is
  configured (errol rule), so a misconfiguration cannot expose unauthenticated RPC.
- The web UI listener binds the node's Tailscale IP only, like RPC, and `webui.New` refuses
  to build without an authorizer (docs/13, I-73).

## Web UI authorization (same-user rule)

The web UI (docs/13) serves people, not nodes: the user's phone or laptop is on the tailnet
but is usually not a storage node. Its listener therefore accepts a source that is either a
cluster member under the RPC rule above, or a tailnet device whose WhoIs
`UserProfile.LoginName` equals the login name of this node's own identity, resolved once at
start by `WhoIs`-ing the node's own Tailscale IP. A tagged device reports the pseudo-login
`tagged-devices`, so a login is treated as empty whenever the WhoIs answer carries tags, and an
empty login never matches on either side. The same identity cache, TTLs and purge on
membership change apply. Mutating routes additionally require a custom header that a
cross-origin page cannot send, and files served from the volume carry a sandboxing CSP so a
stored HTML file cannot act as the viewer. Invariants I-68 and I-71 in docs/13.

## S3 gateway (M5a, deferred)

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
(flips a byte of the shard's payload on every disk of the host that holds a copy: a rebalance within the host leaves the old copy until the sweep), `/dev/exit` (process exit 3 after the reply, for
a service manager to restart), `/dev/fault` (per-peer drop rules on the Raft stream and
the node RPC, both directions; docs/12 §3), `/dev/goroutines` (the stack of every
goroutine, as a crash dump prints it, so a hung node is diagnosed without killing it),
`/dev/disk` and `/dev/fill` (the data disk as the budget sees it, and a scratch file that
consumes it; docs/12 §3) exist for the soak suite (`docs/09`); metrics come from
`/v1/status`. They are
registered only when `dev.enabled: true` is set in the node's config; otherwise the routes
do not exist (404). When enabled they go through the same WhoIs member authorization as
every other endpoint, every node's `/health` carries `dev: true`, and `tailraid status`
prints a warning line for every node that has them enabled.

## Credentials and secrets

| Secret | Where it lives | Never |
|---|---|---|
| Join token | FSM `Meta` (replicated to all members) | printed in logs |
| SMB share password | node config (0600), generated by `init`/`join` (docs/15) | in logs, in `status`, on a command line (`net use … *` reads it from stdin) |
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
| Unrelated tailnet device speaks Raft to a node | member check on accept, before any byte is read (I-92) |
| Two clusters on one tailnet cross-talk | cluster-ID handshake on Raft, `X-Tailraid-Cluster` header on RPC |
| Tailscale IP reassigned to another device | WhoIs StableID mismatch → 403 + cache purge |
| Removed member keeps using a warm authorizer cache | cache purged on membership change |
| Accidental join to the wrong cluster | join token |
| Token holder points a member record at another device, or takes over a known node ID | join records the caller's address; a re-join must present the record's StableID (I-94) |
| Rogue "leader" overwrites a follower's log | member check on accept (I-92) and the handshake (I-49); Raft traffic only on Tailscale IPs |
| Corrupt or forged shard body | receiver hashes the body once, verifies `X-Shard-SHA256` and stores that sum in the shard header (`PutVerified`); reader verifies decoded chunk hash |
| Oversized body exhausts memory/disk | `MaxBytesReader(shardMax+1)`, PUT semaphore, 507 on no space |
| A peer stalls mid-body and holds a PUT or GET slot | per-request body deadline derived from the transfer size (I-97) |
| An admin op runs a pass longer than a shard call's header timeout | the client's control-plane transport has no response-header timeout, and a pass op (`repair_now`, `gc_now`, `check` with a scrub) is bounded by the caller's context alone; shard calls keep the 10 s header timeout (I-189) |
| LAN host reaches NFS, SMB or RPC | NFS and SMB bound to loopback; RPC bound to Tailscale IP only |
| `/dev/*` abuse | only with `dev.enabled`; member-authorized; flagged in `status` |
| Guest, other-user or tagged tailnet device opens the web UI | member-or-same-user WhoIs rule; tagged devices have no login (docs/13, I-68) |
| Cross-site page drives an upload through a logged-in browser | `X-Tailraid-UI` header required before the body is read; no CORS (I-71) |
| A stored HTML or SVG file scripts against the API when opened from `/files/` | `Content-Security-Policy: sandbox` and `nosniff` on every `/files/` response (I-71) |
| Join-token brute force | tailnet-only, member or not; 8 base32 chars; after a failure 1 attempt/s per source IP; rotate anytime |

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
- **I-116** — Every request body a node reads is bounded before it is decoded: 4 MiB for
  `/v1/apply`, 64 KiB for `/v1/join`, `/v1/members`, `/v1/admin` and `/dev/fault`, 4 KiB
  for `/v1/bad-slot` and the web UI's single-entry routes, 256 KiB for its batch routes
  and a thumbnail `PUT`, the chunk size for a shard PUT; a body one byte past its bound
  is refused with `400` and nothing of it is applied.

- **I-18** — An RPC whose source IP is not a cluster member with a matching Tailscale
  StableID is rejected with 403 before any request body is read; a request whose
  `X-Tailraid-Cluster` header does not match this node's cluster ID is rejected with 409.
- **I-48** — A join succeeds only if the caller's WhoIs StableID equals the StableID it
  claims and the presented token equals the current join token; a failed join changes no
  Raft state.
- **I-49** — A Raft stream whose peer presents a different cluster ID is closed before any
  Raft message is processed.
- **I-50** — NFS, SMB and all `127.0.0.1` listeners bind only loopback addresses; the
  RPC and S3 listeners bind only the node's Tailscale IP (and optionally loopback), and
  `rpc.Listen` refuses a non-loopback bind without an authorizer.
- **I-92** — An accepted Raft connection is closed before any byte is read unless its
  source IP is a cluster member (from the member table, or until the table has arrived
  from the seed: the join target as WhoIs names it, then the device members `serve` keeps
  persisted from its table, starting with those the join response listed) whose WhoIs
  StableID matches the record; the cluster-ID
  handshake runs after that check, and the acceptor reads the peer's ID before sending
  its own.
- **I-134** — A node whose member table is empty and that has no seed file seeds its
  authorizer from the device members its local Raft log names, replayed through the FSM's
  own apply: exactly the rows with an address that are not `Removed`.
- **I-93** — `update_addr` from an address that is not in the member table is accepted
  only when the caller's WhoIs StableID is that of the member it names, and changes
  nothing but that member's address; every other op from a non-member is rejected with
  403.
- **I-97** — Reading a PUT or `apply` body and writing a GET body carry a deadline derived
  from the largest transfer (`BodyTimeout`, default `callTimeout(MaxShard)`), so a peer
  that stalls mid-transfer releases its slot when the deadline passes.
- **I-189** — The RPC client reaches a node's control plane (`/v1/*` JSON calls, `/health`)
  through a transport without a response-header timeout, and an admin op that runs a
  pass (`repair_now`, `gc_now`, `check` with a scrub) carries no client deadline beyond
  the caller's context: the leader answers when the pass is done, `tailraid repair`,
  `gc` and `check --scrub` wait for it, and the soak's live environment gives a pass
  `PassTimeout` (30 min). Shard calls keep their size-scaled deadline and the 10 s
  header timeout (I-83), so a stalled holder still fails fast.
- **I-94** — A join records the caller's source address and rejects a request that names
  another with 403; a re-join under a known node ID keeps the record only when it presents
  that record's StableID; a join from the Raft address of a known server under a new node
  ID removes that server and re-keys its row to the new ID only when it presents the
  row's StableID, and is refused otherwise.

## Tests

| Invariant | Tests |
|---|---|
| I-18 | `TestChunkRPC_ReadsAuthBeforeBody_AndClusterHeader`, `TestWhoIs_DeniesUnknownPeer_AndStableIDMismatch_AndPurge` |
| I-48 | `TestCluster_JoinDrainRemove`, `TestJoin_RateLimitedPerSource`, `TestJoin_TokenAndIdentity`, `TestToken_Rotate` |
| I-49 | `TestChunkRPC_ReadsAuthBeforeBody_AndClusterHeader`, `TestIDStream_RejectsForeignCluster` |
| I-50 | `TestListen_RefusesNonLoopbackWithoutAuthorizer`, `TestNFS_FSStatAndUnsupported`, `TestSMB_ListenRefusesNonLoopback` |
| I-92 | `TestIDStream_RefusesNonMemberBeforeHandshake`, `TestMemberSeed_RoundTrip`, `TestCLI_Join_NoFlags` |
| I-134 | `TestMembersFromLog_MatchesTable` |
| I-93 | `TestMembers_UpdateAddrFromUnknownAddress` |
| I-94 | `TestJoin_AddrMustBeCaller`, `TestJoin_RejoinRequiresSameStableID`, `TestCluster_RejoinAfterReset_ReplacesOldServer` |
| I-97 | `TestPut_StalledBodyFreesSlot` |
| I-189 | `TestClient_PassOpOutlivesHeaderTimeout`, `TestAdminOp_IsPass_Table`, `TestAdmin_PassOpsCarryDeadline` |
| I-116 | `TestRPC_BodyCaps_Table`, `TestDev_FaultBodyCap`, `TestWebUI_MkdirBodyCap`, `TestWebUI_BodyCaps_AllMutatingRoutes`, `TestWebUI_Thumb_Put_Boundaries` |
