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
- **Local OS**: the NFS and SMB gateways and all `127.0.0.1` listeners trust the local
  operating system to say *which* local user is calling (the AUTH_SYS uid, the SMB
  session account); docs/21 maps that credential to a tailraid user through the device's
  bindings and an unbound credential may do nothing. Until the first user exists the
  mount is that device's, the same trust as a local disk.
- **People**: a person is a user row with Ed25519 keys, one per device, created from an
  admin's one-time invite; every mutating operation is signed by a user's key, every
  gateway operation carries its actor into the FSM, and the browser signs a person in
  from a link their own CLI mints (docs/21). The FSM trusts the actor a member asserts;
  a compromised member is the non-goal below.

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
| `GET /health` | anyone on the tailnet | unauthenticated, no secrets in the body; it names the node's web port (`web`, 0 when off) so a dashboard can link a member's own pages (docs/13 §1) |
| `GET/HEAD/PUT/DELETE /v1/disks/{id}/shards/{key}` | members | `DELETE` additionally needs `X-Tailraid-Log-Index` and passes the fence of `docs/06` or gets `412`; `X-Tailraid-Priority: background` (the repairer's requests) holds at most half the node's slots (06 §4, I-175) |
| `POST /v1/apply`, `POST /v1/members`, `POST /v1/bad-slot` | members | `update_addr` per step 6 |
| `POST /v1/join` | any tailnet identity with the token | step 2 skipped, step 3 against the claimed StableID |
| `GET /v1/status` | members | |
| `GET /v1/members` | members | the device rows, for a restarting node's seed (I-282) |
| `GET /v1/restores`, `POST /v1/restores`, `POST /v1/restores/{id}/cancel` | members | this host's restore jobs, a job's start and its cancel, the last two forwarded to the mirror's host by the node a user asked, which checked the user's role first (docs/16 §6); the body names the actor, as a member's commands do (docs/21 §4), and the host checks the role again before the job runs and the FSM on every proposal; `/health` carries only the count of jobs, since a job names paths and a user |
| `GET /v1/stats?since=` | members | this node's unsealed stats rows newer than `since`, at most 720, never another member's (docs/17 §3, I-199) |
| `/dev/*` | members, only with `dev.enabled` | |
| web UI (`<tsIP>:20482`): `GET /`, `/assets/*`, `/api/*`, `GET`/`HEAD /files/*` | members, or any tailnet device logged in as this node's user | docs/13 §2; no cluster header |
| web UI: `PUT /files/*`, `POST /api/mkdir` | the same, plus the `X-Tailraid-UI` header | no CORS, so a cross-site page cannot send the header |

Authorization is checked before any request body is consumed (`http.MaxBytesReader` is
installed after the check). A rejected PUT therefore costs the attacker's bandwidth, not
the node's disk. Every body is bounded (I-116): a shard PUT by the chunk size, `/v1/apply`
at 4 MiB, the JSON of `/v1/join`, `/v1/members`, `/v1/admin`, `/v1/restores` and `/dev/fault` at 64 KiB,
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
membership change apply. With users (docs/21 §6) a session cookie minted by `tailraid web
login` is required on top of it: an admitted source without one reaches the sign-in page
only. Mutating routes additionally require a custom header that a cross-origin page cannot
send, and files served from the volume carry a sandboxing CSP so a stored HTML file cannot
act as the viewer. Invariants I-68 and I-71 in docs/13.

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
| Secrets store (docs/19): S3 node credentials and the TMDB key once its PR 3 lands | FSM, as AES-256-GCM ciphertext; the cluster key that opens them in each device's OS keychain (0600 file fallback), fetched from a member | the key in Raft, a value in a log, report or argv |
| User keys (docs/21 §1) | the person's own OS keychain (0600 file fallback under their config home), account `user-<cluster ID>`; the public half in the FSM | the private key in the FSM, a snapshot, a log line, a report or any request; the daemon's keychain |
| Invite codes (docs/21 §1) | printed once; the FSM holds the SHA-256 with a 24 h expiry | a log line, `status`, the audit record (which names the invite, not the code) |
| Browser sessions (docs/21 §6) | the FSM holds the token's SHA-256 with the user, the key and the expiry; the token itself is in the browser's cookie and the one-time link | the token in the FSM, a log line or a report |
| Cluster ID | `<data_dir>/cluster_id` and FSM `Meta` | — (not a secret) |
| Node ID | `<data_dir>/node_id` | — (not a secret) |

Shards are stored in plaintext on each node's disk. Encryption at rest is a non-goal
(see below); the section after next says what is done instead.

## Files on disk

What a node writes to its own disks is readable by nobody but the daemon's user, and the
daemon tells the operator when the disk under it is not encrypted.

- **Modes.** A shard is written through a temp file (0600) and renamed into place; the
  shard store, its quarantine and each prefix directory, the chunk cache and its
  directories, a mirror's tree and the WAL directory are created 0700, and the store and
  the cache tighten their own directories to 0700 at open (a tree created by an older
  release was 0755: another user of the device could list shard names, never read them).
  The config file and the keychain fallback are 0600 in 0700 directories. No umask is
  set: each path is created with its own mode (I-340).
- **Ownership and width.** `serve` warns at start, and `status` and `check` on that
  device carry the same lines, when the config file, the data directory, a disk or a
  mirror is wider than its mode above or owned by another user than the daemon (the
  `chmod` or `chown` is named in the line). Windows has no mode bits worth reading, so it
  reports nothing (I-126, I-341).
- **Full-disk encryption.** Root, and whoever holds the disk, read every shard: a data
  shard is a contiguous slice of its chunk. The at-rest control is the OS's full-disk
  encryption, so each device probes the filesystem under its data directory, each disk
  and each mirror once an hour, and `status --json` carries the answer in
  `machine.paths[].encrypted` as `on`, `off` or `unknown`: macOS asks `diskutil info`
  (`FileVault: Yes` or `Encrypted: Yes`), Linux follows the mount's source device
  through the device-mapper `slaves` chain to a `CRYPT-` uuid (dm-crypt under LVM
  counts, a plain block device is `off`, and ZFS, FUSE, network and memory filesystems
  are `unknown`), Windows asks `manage-bde -status` for the drive (`Protection On`;
  without the right to ask, `unknown`). `status` and `check` on that device warn for
  every path whose answer is `off`, never for `unknown` (I-341).
- **Tampering** is detected, not prevented: the receiver's SHA-256 in the shard header
  (`PutVerified`), the chunk hash on every read and the scrub turn an edited shard into a
  bad slot and a repair (docs/06). Prevention against root is not a software property:
  root holds the cluster key too. Encrypting the shards under the
  cluster key (as sealed S3 nodes do per object, docs/10 §4.7) would cover a lost disk
  without full-disk encryption; it is recorded as a later plan, not built.

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
| An admin op runs a pass longer than a shard call's header timeout | the client's control-plane transport has no response-header timeout, and a pass op (`repair_now`, `gc_now`, `check` with a scrub, `trash_purge` of the whole trash, and a restore, revert or copy from a mirror) is bounded by the caller's context alone; shard calls keep the 10 s header timeout (I-189) |
| LAN host reaches NFS, SMB or RPC | NFS and SMB bound to loopback; RPC bound to Tailscale IP only |
| `/dev/*` abuse | only with `dev.enabled`; member-authorized; flagged in `status` |
| Guest, other-user or tagged tailnet device opens the web UI | member-or-same-user WhoIs rule; tagged devices have no login (docs/13, I-68) |
| Cross-site page drives an upload through a logged-in browser | `X-Tailraid-UI` header required before the body is read; no CORS (I-71) |
| A stored HTML or SVG file scripts against the API when opened from `/files/` | `Content-Security-Policy: sandbox` and `nosniff` on every `/files/` response (I-71) |
| Join-token brute force | tailnet-only, member or not; 8 base32 chars; after a failure 1 attempt/s per source IP; rotate anytime |
| Another user of the device reads shards, the cache or the WAL | 0600 files in 0700 directories, tightened at open (I-340); `status` warns when a path is wider or owned by someone else (I-341) |
| A member asserts another user as the actor of a command | not defended: a member is trusted (the non-goal below); every command's actor is in the audit log, so the lie is at least attributable to the node (docs/21 §4) |
| A captured signature is replayed, on this node or another | the timestamp window, the per-node nonce cache and the target node ID inside the signed bytes (docs/21 §2, I-344) |
| An invite code is guessed | 50 bits, one attempt per second per source, 24 h expiry, checked on the receiving node before anything is proposed (I-343) |
| A session cookie is stolen | `HttpOnly`, `SameSite=Strict`, bound to a key the user can remove, 7-day lifetime, ended everywhere by `web logout` (I-354) |
| Every admin is disabled or every admin key is lost | `user bootstrap` by the OS root on a voter, the same trust as `recover` (I-343); the last admin cannot be removed (I-346) |
| A lost or discarded disk is read | the OS's full-disk encryption; every device probes it hourly and `status` and `check` warn while it is off (I-341); an edited shard is detected by its header sum and the chunk hash, never served |

## Non-goals (explicit)

- Defending against an attacker who controls a **member device** or the headscale control
  plane. Such an attacker can read all data and corrupt metadata; Raft and hashing detect
  accidents, not malice from an authorized member.
- Encryption at rest (§Files on disk: modes, ownership and the full-disk-encryption
  check are what is done instead; encrypting shards under the cluster key is a recorded
  later plan). This includes mirrors (docs/16): a mirror disk holds every byte of
  every snapshot it captured in the clear and is readable with no cluster and no
  Tailscale (`tailraid mirror export`), by design, so a USB mirror is as sensitive as the
  volume itself. The node RPC routes that serve a mirror's trees and objects
  (`/v1/mirrors/{id}/…`) are authorized exactly as the shard routes are.
- Enforcing POSIX modes: they are stored and returned, never consulted; access inside
  the volume is the grants of docs/21, and S3 keys are per cluster, not per path.
- Defending the actor a member asserts: a command's actor is what the member says it is
  (docs/21 §4); user-signed commands would close this and are a later hardening.
- An audit log that survives a member rewriting the volume: the log of docs/21 §8 is
  tamper-evident within a segment and lives in the volume, under the same trust.

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
- **I-281** — `serve` starts its authorizer's seed from the table as of the node's own
  Raft log (the restored table with the entries past its index replayed on a copy) plus
  the members of the seed file the log never mentions, writes that seed to the file, and
  refreshes the file from the table only while the node is healthy. A member that
  joined after the node's last snapshot, the leader among them, is accepted at once; a
  member the log removed is not. Before, the log was consulted only with no seed file
  and an empty table, and the file was refreshed once a minute from whatever table the
  node had: a node restarted within a minute of a join refused the new leader as "not a
  member" for good (soak phase 23.9 on the compose rig, 2026-10-13).
- **I-282** — Until a leader connects, `serve` asks the members its seed names, every
  `SeedFetchEvery` (10 s), for their device rows over the member route `GET /v1/members`,
  and adds every row it does not hold to the authorizer's seed and the seed file; a row it
  holds keeps its own record. A member that joined while this node was down, and so is in
  neither its log nor its seed, is accepted once one member it knows answers, the leader
  among them. The route answers members only, so the list never leaves the cluster; a row
  only ever widens the seed, and the leader's table replaces it once it arrives.
- **I-93** — `update_addr` from an address that is not in the member table is accepted
  only when the caller's WhoIs StableID is that of the member it names, and changes
  nothing but that member's address; every other op from a non-member is rejected with
  403.
- **I-97** — Reading a PUT or `apply` body and writing a GET body carry a deadline derived
  from the largest transfer (`BodyTimeout`, default `callTimeout(MaxShard)`), so a peer
  that stalls mid-transfer releases its slot when the deadline passes.
- **I-189** — The RPC client reaches a node's control plane (`/v1/*` JSON calls, `/health`)
  through a transport without a response-header timeout, and an admin op that runs a
  pass (`repair_now`, `gc_now`, `check` with a scrub, `trash_purge` of the whole trash)
  or copies out of a mirror (`snapshot_restore`, `versions_revert`, `versions_copy`)
  carries no client deadline beyond
  the caller's context: the node answers when the work is done, `tailraid repair`,
  `gc`, `check --scrub`, `snapshot restore` and `versions --revert|--copy` wait for it, and the soak's live environment gives a pass
  `PassTimeout` (30 min). Shard calls keep their size-scaled deadline and the 10 s
  header timeout (I-83), so a stalled holder still fails fast.
- **I-255** — The control transport keeps as many idle connections per host (32) as the
  shard transport: a burst of control calls to one node (proposals forwarded to the
  leader by every gateway's flush and repair workers) reuses its connections, and a
  second burst of the same size dials none. Before, it kept 4, and every call past the
  fourth dialed a fresh TCP connection and closed it after.
- **I-94** — A join records the caller's source address and rejects a request that names
  another with 403; a re-join under a known node ID keeps the record only when it presents
  that record's StableID; a join from the Raft address of a known server under a new node
  ID removes that server and re-keys its row to the new ID only when it presents the
  row's StableID, and is refused otherwise.
- **I-199** — `GET /v1/stats` is a member route (the cluster-ID and WhoIs member check of
  `/v1/status`); it answers at most 720 of this node's own unsealed rows, newer than
  `since` and oldest first, and never another member's rows or any credential; `/health`
  carries the node's web port and nothing else that is new.
- **I-340** — Every directory a node creates for shards, quarantine, the chunk cache, a
  mirror or the WAL is created 0700 and every shard, cache and WAL file 0600; the shard
  store and the cache tighten their own directories to 0700 when they open, and the
  process sets no umask (the config home stays readable by the CLI's user).
- **I-341** — `serve` at start, and `status` and `check` on that device, warn once per
  path when the config file, the data directory, a disk or a mirror is wider than 0600 or
  0700 or owned by another user than the daemon, naming the `chmod` or `chown`, and when
  the filesystem under the data directory, a disk or a mirror reports full-disk
  encryption `off`; `status --json` carries each path's `encrypted` as `on`, `off` or
  `unknown`, a probe that cannot answer reports `unknown` and warns nothing, and a path
  is probed at most once an hour.

## Tests

| Invariant | Tests |
|---|---|
| I-18 | `TestChunkRPC_ReadsAuthBeforeBody_AndClusterHeader`, `TestWhoIs_DeniesUnknownPeer_AndStableIDMismatch_AndPurge` |
| I-48 | `TestCluster_JoinDrainRemove`, `TestJoin_RateLimitedPerSource`, `TestJoin_TokenAndIdentity`, `TestToken_Rotate` |
| I-49 | `TestChunkRPC_ReadsAuthBeforeBody_AndClusterHeader`, `TestIDStream_RejectsForeignCluster` |
| I-50 | `TestListen_RefusesNonLoopbackWithoutAuthorizer`, `TestNFS_FSStatAndUnsupported`, `TestSMB_ListenRefusesNonLoopback` |
| I-92 | `TestIDStream_RefusesNonMemberBeforeHandshake`, `TestMemberSeed_RoundTrip`, `TestCLI_Join_NoFlags` |
| I-134 | `TestMembersFromLog_MatchesTable` |
| I-281 | `TestStartSeed_NamesMembersJoinedAfterTheSnapshot` |
| I-282 | `TestMemberList_MembersOnly_DeviceRows`, `TestSeedFetch_AddsMembersAPeerKnows` |
| I-93 | `TestMembers_UpdateAddrFromUnknownAddress` |
| I-94 | `TestJoin_AddrMustBeCaller`, `TestJoin_RejoinRequiresSameStableID`, `TestCluster_RejoinAfterReset_ReplacesOldServer` |
| I-97 | `TestPut_StalledBodyFreesSlot` |
| I-255 | `TestClient_ControlBurst_ReusesConnections` |
| I-189 | `TestClient_PassOpOutlivesHeaderTimeout`, `TestAdminOp_IsPass_Table`, `TestAdmin_PassOpsCarryDeadline` |
| I-116 | `TestRPC_BodyCaps_Table`, `TestDev_FaultBodyCap`, `TestWebUI_MkdirBodyCap`, `TestWebUI_BodyCaps_AllMutatingRoutes`, `TestWebUI_Thumb_Put_Boundaries` |
| I-199 | `TestRPC_Stats_MemberOnly_OwnRows`, `TestWriter_Live_SinceAndCap_Table` |
| I-340 | `TestStore_DirsPrivate`, `TestCache_DirsPrivate` |
| I-341 | `TestConfig_Warnings_PathsTable`, `TestOwnedBy`, `TestEncryption_ParseDiskutil_Table`, `TestEncryption_ParseManageBDE_Table`, `TestEncryption_Linux_Table`, `TestPaths_ProbeOncePerHour`, `TestPathWarnings_Table`, `TestStatus_CarriesPathsAndWarnings` |
