# 08 — Security

tailraid is a home/small-team system that lives on a network of its own making: the
mesh of [20](20-mesh.md), one WireGuard device per node driven by the member table. The
security design leans on what WireGuard provides and adds only what is needed to stop
accidents and casual misuse. It is explicit about what it does not defend against.

## Trust model

- **Transport**: every node-to-node byte travels inside a WireGuard tunnel that tailraid
  itself runs (20 §1). Confidentiality, integrity and peer authentication of the link
  are WireGuard's job. tailraid uses no TLS of its own.
- **Identity**: a device is identified by its WireGuard key (20 §2). Cryptokey routing
  binds a source address to a key: a packet that arrives from mesh address X was sealed
  by the key that owns X, so "which row is at this address" is the whole identity check.
- **Perimeter**: the set of keys a node holds as peers. It is exactly the member table
  (every device row with a key that is not `Removed`, the pending rows of a join in
  progress, and the seed rows until the table arrives, 20 §3); a packet from any other
  key never reaches a listener. A shared, guest or unrelated device on the same LAN or
  the same internet is not a peer and sees a UDP port that answers nothing.
- **Local OS**: the NFS and SMB gateways and all `127.0.0.1` listeners trust the local
  operating system. A user who can run processes on a device can access that device's
  mount; that is the same trust as a local disk.

## Node RPC authorization (the table authorizer)

All RPC endpoints except `GET /health` require authorization. `internal/rpc.TableAuthorizer`
implements:

1. Extract the source IP from the TCP connection (never from headers).
2. Look up the member table (replicated in the FSM, or the seed until it arrives) by
   address. An active device row → allowed. A pending row (20 §4) → allowed for
   `POST /v1/join` and `GET /health` only. Anything else → **403** before the body is
   read (I-318).
3. The `X-Tailraid-Cluster` header must equal this node's cluster ID → otherwise **409**.
   This stops two clusters whose join codes were mixed up from exchanging shards or
   metadata. `409` is reserved for this; a refused shard `DELETE` (fence, `docs/06`)
   is `412`.

There is no identity cache and nothing to purge: a removed member is dropped from the
WireGuard peers on the membership event, before the table changes, so its next packet
dies at the tunnel (I-317). A member's address never changes (20 §3), so no request from
an unknown source is ever accepted.

Endpoints and who may call them:

| Endpoint | Caller | Note |
|---|---|---|
| `GET /health` | members and pending joiners | unauthenticated, no secrets in the body; it names the node's web port (`web`, 0 when off) so a dashboard can link a member's own pages (docs/13 §1) and the UDP source it sees for the caller (`seen_from`, 20 §5) |
| `GET/HEAD/PUT/DELETE /v1/disks/{id}/shards/{key}` | members | `DELETE` additionally needs `X-Tailraid-Log-Index` and passes the fence of `docs/06` or gets `412`; `X-Tailraid-Priority: background` (the repairer's requests) holds at most half the node's slots (06 §4, I-175) |
| `POST /v1/apply`, `POST /v1/members`, `POST /v1/bad-slot` | members | `introduce` is proposed by the member that verified the datagram, `update_endpoints` only for the caller's own row (20 §5, I-322) |
| `POST /v1/join` | a pending joiner | step 2's pending rule; no token in the request, the introduce consumed it (20 §4) |
| `GET /v1/status` | members | |
| `GET /v1/members` | members | the device rows, for a restarting node's seed (I-282) |
| `GET /v1/stats?since=` | members | this node's unsealed stats rows newer than `since`, at most 720, never another member's (docs/17 §3, I-199) |
| `GET /v1/secrets/key` | members | the cluster key, only to a current member (docs/19 §3, I-304) |
| `/dev/*` | members, only with `dev.enabled` | |
| web UI (`<mesh addr>:20482`): `GET /`, `/assets/*`, `/api/*`, `GET`/`HEAD /files/*` | members | docs/13 §2; no cluster header |
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

`tailraid join <code>` is two steps (20 §4):

1. **The introduce**, a 73-byte datagram on the mesh UDP port, authenticated by an HMAC
   under a key derived from the join token and the cluster ID, with a timestamp. A member
   that verifies it asks the leader for an address; the leader proposes a *pending row*
   (the joiner's key and address) and the member answers the address, in a reply shorter
   than the request. A bad HMAC gets silence; after a failed attempt a source gets one
   more per second (as before: successful joins are never throttled); a timestamp more
   than 10 minutes off is a failure, and the joiner's error says so. The same key
   introduced twice gets the same address. A pending row expires after 10 minutes, so an
   introduce that is never followed by a join leaves nothing behind (I-319).
2. **`POST /v1/join`** through the tunnel, from the pending address, with the joiner's
   name, ports, suffrage request, capacity and the UDP source the member saw. The leader
   turns the pending row into the member row (same index, key and address), adds the
   server to the Raft configuration (voter or non-voter) and proposes the row before the
   add, so every node holds the peer before the leader dials it. A re-join under a known
   node ID keeps its index only when it comes from that row's address, which only the
   row's key can use; a re-join from a device that was `reset` presents the same key,
   so the leader answers the row's existing address and the join re-keys the row to the
   new node ID and removes the stale Raft server (I-94, I-321).

The response lists the current device members. The joiner seeds its authorizer and its
WireGuard peers with them, so that it accepts the leader's Raft connection while its own
member table is still empty (the table replaces the seed as soon as the log arrives;
§Raft transport); before the request it seeds the bootstrap from the join code, so the
leader's first connection is not refused, and it persists the response's members as
`<data_dir>/members.seed`, which `serve` loads at start and keeps current from its table
once a minute (`reset` removes it). The file matters on every restart, not only the
first: Raft restores a follower's table at open from a snapshot only, so a node
restarted before its first snapshot has an empty table until the leader connects, and
the leader was accepted only by that table; the WireGuard device has the same need, since
it cannot even decrypt the leader's handshake without the leader's key. A node with
neither a table nor a seed file replays the member entries its own log already holds
into a scratch table and seeds from that (I-134).

The token is 16 random base32 characters, created at `init`, carried inside the join
code that `tailraid token` prints, and rotated by `tailraid token rotate` (a Raft
`MetaSet`), which changes the HMAC key of every later introduce and never touches a
pending row. It is the only thing that authenticates an introduce, and the introduce is
answered on a UDP port that may face the internet, hence the length: 80 bits against an
attacker who can send one datagram per second per source.

## Raft transport: member check and cluster-ID handshake

hashicorp/raft's TCP transport has no authentication: whoever can send AppendEntries or
InstallSnapshot to a follower rewrites its metadata. `internal/cluster.idStream` wraps the
`raft.StreamLayer` with two checks.

1. **Membership on accept (I-92).** Before a byte is read, an accepted connection's source
   address goes through the same rule as an RPC (step 2 above): an active device row at
   that address. Anything else is closed. The tunnel already guarantees the source is a
   peer; this check keeps a pending joiner, and a removed member whose peer entry is a
   tick from being dropped, out of Raft, and it is the defence that holds if the peer
   table and the member table ever disagree. A joiner's table is empty until the log
   arrives, so `join` seeds the authorizer with the members the join response listed
   (§Join); the leader that dials it is one of them.
2. **Cluster ID.** Each side then sends its 16-byte cluster ID and reads the peer's; on
   mismatch the connection is closed and the event logged (I-49). The dialer writes first
   and the acceptor reads first, so an accepted peer learns nothing before it has shown its
   own ID. It is not a secret (`/health` publishes it), the member check is the gate.

Raft traffic is confined to mesh addresses; an attacker who controls a member device is a
non-goal (below).

## Loopback-only listeners

- NFS `127.0.0.1:20490` (macOS, Linux) and SMB `127.0.0.1:20445` (Windows, docs/15). Never
  bound to a mesh or LAN address. NFS accepts `AUTH_NULL` because the OS is the
  boundary; SMB requires the generated share password because Windows refuses guest
  sessions, not because the password adds a boundary: any local process can read the 0600
  config that holds it.
- `internal/rpc.Listen` refuses to bind a non-loopback address when no `Authorizer` is
  configured (errol rule), so a misconfiguration cannot expose unauthenticated RPC.
- The web UI listener binds the node's mesh address only, like RPC, and `webui.New` refuses
  to build without an authorizer (docs/13, I-73).

## Web UI authorization

The web UI (docs/13) is served to cluster members under the RPC rule above: the source
is an active device row's address, else 403. Under Tailscale a device of the same tailnet
user could open it too (the phone); the mesh has no users, and a phone that is not a
node is a later feature (20 §11: client peers with a QR configuration). Mutating routes
additionally require a custom header that a cross-origin page cannot send, and files
served from the volume carry a sandboxing CSP so a stored HTML file cannot act as the
viewer. Invariants I-68 and I-71 in docs/13.

## The UDP port

The mesh port is the one listener a device may expose to the internet (a port forward,
a mapped port, the cloud voter's public address, 20 §5 and §7). What it answers:

| Frame | Who can make it answer | Answer |
|---|---|---|
| WireGuard handshake (types 1–4) | a configured peer; anyone else gets nothing, and under load WireGuard's cookie mechanism makes an initiator prove its address | a handshake response |
| `0x10` introduce | a holder of the current join token | 38 bytes to the source, after a Raft proposal; one attempt per second per source after a failure |
| `0x11` relay | a member (HMAC under the cluster key) | a forwarded packet to another member's pinned endpoint, within `mesh.relay_rate`; nothing to a non-member, so the port is never an amplifier |
| `0x12` path probe | a member (HMAC under the cluster key) | the frame back to the source |

## S3 gateway (M5a, deferred)

The S3 API server listens on the node's mesh address (and loopback). It requires **both**:

- a valid SigV4 signature with an access key created by `tailraid s3 keys create`
  (keys are stored only in the local config file with mode 0600 or the OS keyring; the
  secret never enters Raft), and
- a source that is a cluster member, through the same `rpc.Authorizer`. Devices that are
  not nodes (phones, rclone on a laptop) wait for client peers (20 §11).

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
do not exist (404). When enabled they go through the same member authorization as
every other endpoint, every node's `/health` carries `dev: true`, and `tailraid status`
prints a warning line for every node that has them enabled.

## Credentials and secrets

| Secret | Where it lives | Never |
|---|---|---|
| Mesh private key (20 §2) | each device's OS keychain (0600 file fallback), account `mesh-<cluster ID>`; kept by `reset` | in the FSM, a snapshot, an export, a report, a log line, an error or any request |
| Join token | FSM `Meta` (replicated to all members); inside the join code | printed in logs |
| Cluster key (docs/19) | each device's OS keychain (0600 file fallback), fetched from a member; it also authenticates relay frames and path probes (20 §6) | the key in Raft, a value in a log, report or argv |
| SMB share password | node config (0600), generated by `init`/`join` (docs/15) | in logs, in `status`, on a command line (`net use … *` reads it from stdin) |
| S3 gateway access keys | node config (0600) or OS keyring | in Raft, in `status` output |
| Secrets store (docs/19): S3 node credentials and the TMDB key | FSM, as AES-256-GCM ciphertext under the cluster key | a value in a log, report or argv |
| Cluster ID | `<data_dir>/cluster_id` and FSM `Meta` | — (not a secret) |
| Node ID | `<data_dir>/node_id` | — (not a secret) |

Shards are stored in plaintext on each node's disk. Encryption at rest is a non-goal
(see below); users who need it put the data directory on an encrypted volume.

## Threats and mitigations

| Threat | Mitigation |
|---|---|
| A device on the same LAN or the internet reads or writes shards | it is not a peer: its packets never leave the tunnel's receive path; a member check on every request besides (I-317, I-318) |
| Stale or replayed shard `DELETE` removes a live shard | DELETE fence: log index, placement and file age checked by the receiver (`docs/06`) |
| An unrelated device speaks Raft to a node | not a peer; the member check on accept besides, before any byte is read (I-92) |
| Two clusters' join codes mixed up | the introduce's HMAC is keyed by the cluster ID and the code carries it; the cluster-ID handshake on Raft and the `X-Tailraid-Cluster` header on RPC catch the rest |
| A removed member keeps talking | dropped from the peers on the membership event; its next packet dies at the tunnel (I-317) |
| A leaked join code | an attacker who can reach a member's UDP port can introduce a device until `token rotate`; it then holds a pending address and may call `/v1/join`, which gives it a member row: a leaked code is a leaked membership, rotate it as soon as a device is lost. The code is never in a log |
| An introduce flood at an internet-facing port | one attempt per second per source after a failure; HMAC checked before any proposal; the reply is shorter than the request (I-319) |
| A replayed introduce | the timestamp window and the idempotent answer: a replay gets the same address the key already has, and never a second row |
| A relay used to amplify or to reach a non-member | a relay forwards only between two of its own peers and only frames whose HMAC under the cluster key verifies, within a rate (I-328) |
| Corrupt or forged shard body | receiver hashes the body once, verifies `X-Shard-SHA256` and stores that sum in the shard header (`PutVerified`); reader verifies decoded chunk hash |
| Oversized body exhausts memory/disk | `MaxBytesReader(shardMax+1)`, PUT semaphore, 507 on no space |
| A peer stalls mid-body and holds a PUT or GET slot | per-request body deadline derived from the transfer size (I-97) |
| An admin op runs a pass longer than a shard call's header timeout | the client's control-plane transport has no response-header timeout, and a pass op (`repair_now`, `gc_now`, `check` with a scrub, `trash_purge` of the whole trash) is bounded by the caller's context alone; shard calls keep the 10 s header timeout (I-189) |
| LAN host reaches NFS, SMB or RPC | NFS and SMB bound to loopback; RPC bound to the mesh address only |
| `/dev/*` abuse | only with `dev.enabled`; member-authorized; flagged in `status` |
| A device that is not a node opens the web UI | not a peer, and the UI admits members only (docs/13, I-68) |
| Cross-site page drives an upload through a logged-in browser | `X-Tailraid-UI` header required before the body is read; no CORS (I-71) |
| A stored HTML or SVG file scripts against the API when opened from `/files/` | `Content-Security-Policy: sandbox` and `nosniff` on every `/files/` response (I-71) |
| A device's private key is read from its disk | the keychain where the OS has one; the file fallback is 0600 under a 0700 directory; a stolen key is the device, `node remove` it and rotate the token |

## Non-goals (explicit)

- Defending against an attacker who controls a **member device**. Such an attacker can
  read all data and corrupt metadata; Raft and hashing detect accidents, not malice from
  an authorized member.
- Encryption at rest. This includes mirrors (docs/16): a mirror disk holds every byte of
  every snapshot it captured in the clear and is readable with no cluster and no
  network (`tailraid mirror export`), by design, so a USB mirror is as sensitive as the
  volume itself. The node RPC routes that serve a mirror's trees and objects
  (`/v1/mirrors/{id}/…`) are authorized exactly as the shard routes are.
- Per-user access control inside the volume (POSIX modes are stored, not enforced across
  devices; S3 keys are per cluster, not per path).
- Hiding the cluster: the UDP port answers a handshake initiation from a configured peer
  only, but its existence is observable.
- Audit logging.
- Protecting the local mount from other users of the same device.

## Invariants
- **I-116** — Every request body a node reads is bounded before it is decoded: 4 MiB for
  `/v1/apply`, 64 KiB for `/v1/join`, `/v1/members`, `/v1/admin` and `/dev/fault`, 4 KiB
  for `/v1/bad-slot` and the web UI's single-entry routes, 256 KiB for its batch routes
  and a thumbnail `PUT`, the chunk size for a shard PUT; a body one byte past its bound
  is refused with `400` and nothing of it is applied.

- **I-18** — An RPC whose source address is not an active cluster member's (a pending
  joiner's only on `/health` and `/v1/join`) is rejected with 403 before any request
  body is read; a request whose `X-Tailraid-Cluster` header does not match this node's
  cluster ID is rejected with 409.
- **I-48** — A join succeeds only from the pending address an accepted introduce gave the
  caller's key, and an introduce is accepted only with a valid HMAC under the current
  token; a failed introduce or join changes no Raft state beyond a pending row that
  expires.
- **I-49** — A Raft stream whose peer presents a different cluster ID is closed before any
  Raft message is processed.
- **I-50** — NFS, SMB and all `127.0.0.1` listeners bind only loopback addresses; the
  RPC and S3 listeners bind only the node's mesh address (and optionally loopback), and
  `rpc.Listen` refuses a non-loopback bind without an authorizer.
- **I-92** — An accepted Raft connection is closed before any byte is read unless its
  source address is an active cluster member's (from the member table, or until the
  table has arrived from the seed: the bootstrap as the join code names it, then the
  device members `serve` keeps persisted from its table, starting with those the join
  response listed); the cluster-ID handshake runs after that check, and the acceptor
  reads the peer's ID before sending its own.
- **I-134** — A node whose member table is empty and that has no seed file seeds its
  authorizer and its WireGuard peers from the device members its local Raft log names,
  replayed through the FSM's own apply: exactly the rows with an address and a key that
  are not `Removed`.
- **I-281** — `serve` starts its authorizer's seed, and its WireGuard peers, from the
  table as of the node's own Raft log (the restored table with the entries past its
  index replayed on a copy) plus the members of the seed file the log never mentions,
  writes that seed to the file, and refreshes the file from the table only while the
  node is healthy. A member that joined after the node's last snapshot, the leader among
  them, is accepted at once; a member the log removed is not. Before, the log was
  consulted only with no seed file and an empty table, and the file was refreshed once a
  minute from whatever table the node had: a node restarted within a minute of a join
  refused the new leader as "not a member" for good (soak phase 23.9 on the compose rig,
  2026-10-13).
- **I-282** — Until a leader connects, `serve` asks the members its seed names, every
  `SeedFetchEvery` (10 s), for their device rows over the member route `GET /v1/members`,
  and adds every row it does not hold to the authorizer's seed, the WireGuard peers and
  the seed file; a row it holds keeps its own record. A member that joined while this
  node was down, and so is in neither its log nor its seed, is accepted once one member
  it knows answers, the leader among them. The route answers members only, so the list
  never leaves the cluster; a row only ever widens the seed, and the leader's table
  replaces it once it arrives.
- **I-93** — *Retired with the mesh (20 §3): a member's address never changes, so no
  request from a non-member source is ever accepted; the number stays with the Tailscale
  rule it named until its tests go with the code.* Under Tailscale: `update_addr` from an
  address that is not in the member table is accepted only when the caller's WhoIs
  StableID is that of the member it names, and changes nothing but that member's
  address; every other op from a non-member is rejected with 403.
- **I-97** — Reading a PUT or `apply` body and writing a GET body carry a deadline derived
  from the largest transfer (`BodyTimeout`, default `callTimeout(MaxShard)`), so a peer
  that stalls mid-transfer releases its slot when the deadline passes.
- **I-189** — The RPC client reaches a node's control plane (`/v1/*` JSON calls, `/health`)
  through a transport without a response-header timeout, and an admin op that runs a
  pass (`repair_now`, `gc_now`, `check` with a scrub, `trash_purge` of the whole trash)
  carries no client deadline beyond
  the caller's context: the leader answers when the pass is done, `tailraid repair`,
  `gc` and `check --scrub` wait for it, and the soak's live environment gives a pass
  `PassTimeout` (30 min). Shard calls keep their size-scaled deadline and the 10 s
  header timeout (I-83), so a stalled holder still fails fast.
- **I-255** — The control transport keeps as many idle connections per host (32) as the
  shard transport: a burst of control calls to one node (proposals forwarded to the
  leader by every gateway's flush and repair workers) reuses its connections, and a
  second burst of the same size dials none. Before, it kept 4, and every call past the
  fourth dialed a fresh TCP connection and closed it after.
- **I-94** — A join from a pending address records that address as the row's and
  rejects a request that names another with 403; a re-join under a known node ID keeps
  the record only when it comes from that record's address, which only the record's key
  can send from; a join from the address of a known server under a new node ID (a device
  that was `reset`, introduced again with its key) removes that server and re-keys its
  row to the new ID.
- **I-199** — `GET /v1/stats` is a member route (the cluster-ID and member check of
  `/v1/status`); it answers at most 720 of this node's own unsealed rows, newer than
  `since` and oldest first, and never another member's rows or any credential; `/health`
  carries the node's web port and nothing else that is new.

## Tests

| Invariant | Tests |
|---|---|
| I-18 | `TestChunkRPC_ReadsAuthBeforeBody_AndClusterHeader`, `TestWhoIs_DeniesUnknownPeer_AndStableIDMismatch_AndPurge` (replaced by the table authorizer's tests with docs/20's PR 2) |
| I-48 | `TestCluster_JoinDrainRemove`, `TestJoin_RateLimitedPerSource`, `TestJoin_TokenAndIdentity`, `TestToken_Rotate` |
| I-49 | `TestChunkRPC_ReadsAuthBeforeBody_AndClusterHeader`, `TestIDStream_RejectsForeignCluster` |
| I-50 | `TestListen_RefusesNonLoopbackWithoutAuthorizer`, `TestNFS_FSStatAndUnsupported`, `TestSMB_ListenRefusesNonLoopback` |
| I-92 | `TestIDStream_RefusesNonMemberBeforeHandshake`, `TestMemberSeed_RoundTrip`, `TestCLI_Join_NoFlags` |
| I-134 | `TestMembersFromLog_MatchesTable` |
| I-281 | `TestStartSeed_NamesMembersJoinedAfterTheSnapshot` |
| I-282 | `TestMemberList_MembersOnly_DeviceRows`, `TestSeedFetch_AddsMembersAPeerKnows` |
| I-93 | `TestMembers_UpdateAddrFromUnknownAddress` (goes with the op) |
| I-94 | `TestJoin_AddrMustBeCaller`, `TestJoin_RejoinRequiresSameStableID`, `TestCluster_RejoinAfterReset_ReplacesOldServer` |
| I-97 | `TestPut_StalledBodyFreesSlot` |
| I-255 | `TestClient_ControlBurst_ReusesConnections` |
| I-189 | `TestClient_PassOpOutlivesHeaderTimeout`, `TestAdminOp_IsPass_Table`, `TestAdmin_PassOpsCarryDeadline` |
| I-116 | `TestRPC_BodyCaps_Table`, `TestDev_FaultBodyCap`, `TestWebUI_MkdirBodyCap`, `TestWebUI_BodyCaps_AllMutatingRoutes`, `TestWebUI_Thumb_Put_Boundaries` |
| I-199 | `TestRPC_Stats_MemberOnly_OwnRows`, `TestWriter_Live_SinceAndCap_Table` |
