# 00 — Overview

`tailraid` turns a set of devices into one shared volume that behaves like
a modern RAID array: file data is Reed-Solomon erasure-coded into shards spread across
devices, identical content is stored once, the devices reach each other over a WireGuard
mesh the binary runs itself ([20](20-mesh.md)), and the volume is mounted locally on macOS
and Linux through the operating system's own NFSv3 client and on Windows through its SMB
client. Metadata is kept
consistent by a Raft-replicated state machine, so there is one coherent volume and no
split-brain.

This document set is the design of record. Code is written against it, test-first; every
numbered invariant (`I-n`) in these documents is enforced by at least one named test, and
`scripts/check-invariants.sh` fails the build when that mapping breaks.

## Goals

1. **No data loss** within the durability contract below. This dominates every other goal.
2. **RAID semantics across devices**: any `m` devices may be lost or asleep and the volume
   remains readable; writes continue (degraded) while at least `k` storage nodes and a Raft
   quorum are reachable.
3. **Dedup**: identical chunks (4 MiB by default) are stored once regardless of path or how often they
   are written.
4. **Native mounts** on Windows, macOS, Linux with no kernel drivers to install; the mesh
   uses the OS's own TUN driver (wintun on Windows, a signed dll the binary carries).
5. **Small, provable code**: a single static binary of about 18.1k production lines through
   M4 (the original 8k budget was optimistic, see the size table), with a test suite that
   would have caught every bug the predecessor shipped.
6. **Day-zero membership management**: add, drain, remove, promote nodes and change `k+m`
   at any time without downtime.

## Non-goals (out of scope)

S3 features beyond the implemented subset (versioning, ACLs, policies, lifecycle, tagging,
SSE, object lock, presigned POST); web UI beyond the dashboard and file browser of docs/13
(user management is the CLI's, docs/21; no mutating admin operations from the browser);
metrics stack (a `status` JSON instead);
encryption at rest beyond sealed S3 nodes (docs/10 §4.7: the mesh's WireGuard covers transit,
devices rely on the OS's full-disk encryption, and a mirror disk is plaintext); quotas;
history in the live pool (the trash keeps deletes for a while; snapshots live on mirror
disks, docs/16, with no per-save history, no writable clone, no snapshot of one folder,
no diff and not the Windows Previous Versions tab);
xattrs; NLM locking (mount with `nolock`); hard links; NFSv4; SMB encryption,
dialect 3.1.1, oplocks and leases, SMB security descriptors; Windows before 11 24H2; multiple
independent volumes (S3 buckets are top-level directories of the one volume);
content-defined chunking; compression (hook left); tiering; fsck-from-shards metadata
reconstruction; WAL mirroring to a peer before ack (noted as a later option).

## Glossary

| Term | Meaning |
|---|---|
| **node** | A participant in the cluster. `Kind=device` runs `tailraid serve` (Raft, RPC, gateways) and stores nothing itself; `Kind=disk` is one of a device's disks, the only place shards live (docs/14). `Kind=s3` is a storage-only bucket: never a Raft member, never a gateway. |
| **member** | A node present in the replicated node table; devices are also Raft servers (voter or non-voter). |
| **voter** | A Raft server that counts toward quorum. Always-on devices, odd count. Laptops default to non-voter. |
| **volume** | The single filesystem tree managed by one cluster. Has a policy `Volume{ChunkSize, K, M, PolicyVersion}`. |
| **bucket** | A top-level directory of the volume as seen through the S3 API. |
| **inode** | A file, directory or symlink record in the FSM, identified by `Ino` (monotonic, never reused) plus `Gen`. |
| **chunk** | A fixed-size (default 4 MiB) span of a file at offset `idx*ChunkSize`, content-addressed by SHA-256 of its logical bytes. The unit of dedup and of erasure coding. |
| **stripe** | One chunk encoded into `k` data shards and `m` parity shards. Chunk and stripe are 1:1. |
| **shard** | One of the `k+m` encoded pieces of a chunk; stored on exactly one disk; key `hex(hash).k-m.idx` (the encoding is part of the key, so a migration's new shards coexist with the old). |
| **slot** | Position `idx` in a chunk's `Placement []NodeIdx`. Slot value 0 means "no node holds this shard yet" (degraded). |
| **placement** | The `k+m` node indices that hold a chunk's shards, chosen by rendezvous hashing at write time and **stored** in the chunk table. |
| **policy version** | `Volume.PolicyVersion`, bumped by `volume set-redundancy`. Chunks whose `(K,M)` differ from the current policy are migrated lazily by the repairer. |
| **gateway** | The NFS or SMB (and later S3) front end running on a device; any device is a gateway for its own user. |
| **mirror** | A disk given to snapshots (docs/16): `Kind=mirror`, attached to a device, never a placement target, often offline; an independent, self-describing, differential copy of every snapshot it captured. |
| **diskless host** | A device with no disks: a member, voter if configured, and gateway whose reads and writes go to other hosts' disks (docs/14 §9). |
| **live** | A node that answered `/health` within the last 25 s (probed every 5 s, docs/05 §6). Not stored in Raft. |
| **WAL** | Per-inode write-ahead log on the gateway's disk that makes NFS `FILE_SYNC` honest before the chunk is flushed to the cluster. |

## Architecture

```
 NFS client (OS)  ──127.0.0.1──▶  nfsgw (go-nfs Handler, handles = ino+gen)
 SMB client (Win) ──127.0.0.1──▶  smbgw (go-filesystems/smb over a vfs adapter, docs/15)
                                      │ billy.Filesystem
                                      ▼
                                   vfs: Overlay (dirty extents + per-inode WAL, fsync)
                                       Flusher (RMW chunk → sha256 → dedup → EC → PUT → Raft Commit)
                                       Reader  (chunk → k shards from ranked live nodes → decode → LRU cache)
                        ┌─────────────┴──────────────┐
                        ▼                             ▼
             meta.FSM (in-memory State,        chunk.Store (local shard files
             Raft-replicated; inodes, dirs,    "TRS1" header + sha256, fsync+rename)
             chunk table w/ placement+refs,    chunk.Cache (decoded chunks, LRU)
             node table, tombstones)
                        ▲                             ▲
      cluster (raft wiring, leader forwarding,   rpc (HTTP/1.1 on <mesh addr>:20481:
      membership, prober/liveness, events)      GET/PUT/HEAD/DELETE /v1/disks/{id}/shards/{key},
      raft TCP <mesh addr>:20480 + cluster-id   POST /v1/apply, /v1/members, /v1/join,
      handshake; raft-boltdb; FileSnapshotStore GET /health; table authorizer)
      webui (HTTP on <mesh addr>:20482: dashboard over the status report, file browser and
      media over the vfs; members only; docs/13)
                        ▲
      mesh (wireguard-go on a TUN; peers from the member table; the introduce, fan-out,
      relay and path probe frames on one UDP socket, <any>:20483; docs/20)
      repair (leader only: GC of tombstones after grace; scanner refills missing/dead slots; scrub)
```

Ports: Raft `<mesh addr>:20480`, node RPC `<mesh addr>:20481`, web UI `<mesh addr>:20482`,
S3 gateway `<mesh addr>:20443`, the mesh's UDP socket `20483` on every address, NFS
`127.0.0.1:20490` (macOS, Linux), SMB `127.0.0.1:20445` (Windows).

Identity and transport security come from the mesh (docs/20): a packet from a mesh
address was sealed by the key that owns it, so every RPC connection is authorized by
looking its source address up in the member table. No TLS of our own.

## Durability contract

> "No data loss" means: every byte whose WRITE was acknowledged is durable at once (fsynced
> to the issuing gateway's WAL) and is returned unchanged by any later READ on the issuing
> gateway immediately; it is returned unchanged by any later READ on **every** live gateway
> once it has been flushed to the cluster, which happens within the idle timer (2 s) after
> writes to that file pause, and otherwise within the dirty-byte thresholds (32 MiB per file,
> 256 MiB total, or a 128 MiB WAL) while writes continue. Every metadata op that returned OK
> is visible on every live gateway once its Raft entry is applied there. This holds under:
> (a) permanent loss of any **m** nodes' disks; (b) gateway process crash or power loss at any
> instant, including right after an ack (WAL replay); (c) leader crash or partition at any
> point in Apply; (d) corruption of up to m shards per stripe (detected by hash, repaired);
> (e) kill -9 or power loss at any fsync step of a shard write (temp files swept, never
> served; an acknowledged shard is fsynced with its directory); (f) loss of a Raft minority.
> **Not covered:** > m nodes lost before repair; permanent loss of Raft majority; storage that
> lies about fsync; > m corrupt shards in one stripe; destruction of the issuing gateway's
> disk before its WAL has flushed (option for later: WAL mirroring to one peer before ack).
> Degraded writes (user's choice) hold < m parity until repair.
> fsync policy: WAL per record; shard file `write → Sync → Close → Rename → fsync(dir)`;
> raft-boltdb `NoSync=false`; snapshots fsync on close. **No production path skips fsync; no
> test-mode env var.** Tests needing speed use in-memory stores; durability tests use real
> temp dirs.

## Degraded operation (user-visible)

| Condition | Metadata | Reads | Writes |
|---|---|---|---|
| leader reachable, live ≥ k+m | normal | normal | full placement |
| leader reachable, k ≤ live < k+m | normal | OK if k holders live | degraded (≥ k shards); `0` slots filled as soon as a spare is live, slots on dead nodes re-placed after `fill_after` |
| leader reachable, live < k | normal | EIO for affected chunks | flush fails; WAL buffers up to 1 GiB then EIO |
| no leader contact > 30 s / no quorum | stale local copy | served | data WRITEs buffered in the WAL until the caps, then EIO; metadata mutations EROFS |

## Size budget

| Area | Lines (production) |
|---|---|
| `cmd/tailraid`, `internal/admin` | ~1,850 |
| `internal/{clock,sysfs,ec,chunk}` | ~1,550 |
| `internal/meta` (FSM, codec, placement, chunk state, event ring) | ~2,700 |
| `internal/vfs` (overlay, WAL, flusher, reader, billy) | ~2,550 |
| `internal/nfsgw`, `internal/repair` | ~900 |
| `internal/{cluster,rpc,tailscale,config,node}` | ~3,600 |
| The mesh (docs/20, planned 2026-10-14): `internal/mesh` (keys, the join code and the introduce, the Bind with fan-out and the frame demux, the device and its peer diff, the path monitor, the per-OS interface commands, 0.9k), identity in the FSM and the RPC (`Node.Key`, pending rows, codec 13, the table authorizer, +0.3k −0.3k), the node on the mesh with Tailscale deleted (+0.4k −0.5k), the path class in the prober, reader and flusher (0.15k), the service units (0.15k), the rigs (0.1k); the relay (0.25k) and port mapping (0.5k, clients lifted from tunnelmesh) follow | +0.8k planned, then +0.75k |
| `internal/{mount,svc,dev}` | ~700 |
| `internal/soak` (engine, live env, report; shipped for `tailraid verify`) | ~2,300 |
| `internal/update` | ~350 |
| `internal/webui` (handler over the vfs; its TypeScript app is 1.4k lines and 0.2k of CSS, not Go) | ~570 |
| **Through M4 (measured 2026-10-08, after the review fixes and the hot-path work)** | **≈ 18.1k** |
| **Through the web UI (measured 2026-10-08: +0.7k, the handler plus ~130 lines of wiring in rpc, tailscale, admin, vfs, config, node and cmd)** | **≈ 18.8k** |
| `init` without flags (voter-first placement, derived budget, automatic redundancy, codec v2: ~0.4k) and the soak automation phase with `/dev/disk`, `/dev/fill` (~0.2k) | +0.6k |
| **Through init-without-flags (measured 2026-10-08)** | **≈ 19.4k** |
| Streaming large files: bounded flush memory, cap backpressure, streamed compaction, atomic resumable web upload, process memory in status, soak phase 12 (measured 2026-10-09: webui 0.27k, soak 0.23k, vfs 0.22k, rpc and admin 0.1k; the plan said 0.6k) | +0.8k |
| Machine vitals in `status` and the soak report (per-OS probes 0.15k) and the large file's placement checks and figure (0.1k) | +0.25k |
| **Through streaming large files (measured 2026-10-09)** | **≈ 20.5k** |
| Several disks per device (docs/14, measured 2026-10-09: node disk set, marker and per-target budgets 0.44k, meta placement, policy, skew and codec 0.37k, CLI 0.15k, rpc and prober 0.2k, soak phase 13, targets and the spread chart 0.7k; the TypeScript Disks block 0.2k, not Go; the plan said 0.8k: the soak and the node's disk lifecycle were sized optimistically) | +1.9k |
| **Through several disks per device (measured 2026-10-09)** | **≈ 22.4k** |
| The 2026-10 review, correctness (PR 2, measured 2026-10-09 as the net diff): the Raft accept check and join binding 0.15k, unknown-outcome resolution 0.1k, identity files, body deadlines and cache races 0.15k, WAL failure state, streamed snapshots and the tracked NFS listener 0.2k, placement conflicts, GC batches and codec version 3 0.15k; minus the removed `Place`, wrappers and dead code | +0.8k |
| **Through the correctness review (measured 2026-10-09)** | **≈ 23.2k** |
| Tests and the soak report (PR 3, measured 2026-10-09): the soak driver's phase runner with recovery and timeout, the undo list, skipped checks, the abort path and the Mutate gate 0.3k, the FSM path bound and the Windows directory sync under 0.05k; the rest of the PR is test code (new scenarios, size tables, fuzz targets, the skip rule) | +0.35k |
| **Through the test review (measured 2026-10-09)** | **≈ 23.55k** |
| Hardening (PR 4, measured 2026-10-09): minisign verification and the signing tool 0.35k, update hardening (size, fsync, rollback, downgrade guard) 0.1k, snapshot export and offline recovery 0.2k, the rotating log file, config validation and the rate limiter 0.25k, unit confinement, shutdown hand-over, readiness and the web UI host check 0.15k | +1.05k |
| **Through the 2026-10 review (measured 2026-10-09)** | **≈ 24.6k** |
| Disks only and diskless hosts (docs/14 §9, measured 2026-10-09 as the net diff): the device store, its budget, the `/v1/shards` route and the capacity special cases go (−0.3k); join registering config disks, the legacy-state check, per-disk `/dev/disk` and the soak's diskless voter and phase 14 come (+0.35k); the TypeScript loses its data-dir label | +0.05k |
| **Through disks only (measured 2026-10-09)** | **≈ 24.65k** |
| SMB gateway for Windows (docs/15, measured 2026-10-09: the adapter and server wiring in `internal/smbgw` 0.33k, `vfs.File.WriteAt` and the file info's times 0.02k, config, node, CLI and mount helpers 0.19k; minus the portmap stub and the Windows NFS path 0.25k; the server copy in `third_party/smb` is upstream code and not counted) | +0.3k |
| **Through the SMB gateway (measured 2026-10-09)** | **≈ 24.95k** |
| Web UI v2 (docs/13, planned 2026-10-10): the trash, folder flags and protection in the FSM, vfs and GC tick (meta 0.35k, vfs 0.08k, repair, node and config 0.08k), the pinned local cache (0.3k), the web handler's batch, trash, flag, thumbnail, tag and pin routes with the in-volume thumbnails (0.9k + 0.4k), the `trash` and `cache` CLI (0.15k), soak phase 16 with the report's trash and cache chart (0.4k); the TypeScript grows by about 1.8k (tiles, selection, trash, flags, the album view and the now-playing bar), not Go | +2.6k planned |
| Open media fast (PR A1 `open-fast`, measured 2026-10-11 as the net diff): the cache's background writers and pending entries 0.09k, readahead depth in bytes, background priority and seek cancellation 0.07k, the status reporter 0.13k, folder covers off the listing 0.03k; measured first on the real cluster (`docs/reports/bench-2026-10-open-media.txt`) | +0.32k |
| Early serve (PR A2 `early-serve`, measured 2026-10-11 as the net diff): data-shard sums in the chunk table, codec 5 and `SetShardSums` (meta 0.12k), the flusher's sums and codec gate 0.02k, the reader's per-shard publish and `ReadAtEarly` 0.22k, the repair backfill 0.06k, the test fabric's GET stall 0.05k | +0.47k |
| Stats page (docs/17, planned 2026-10-11): the sampler, rows, gzip segments, rollups, retention and the live-edge RPC in `internal/stats` (1.0k), the chart helpers moved to `internal/chart` with a clock time axis (net +0.15k), the network vitals on three OSes (0.15k), the web port in the probe and `/v1/stats` (0.15k), `/api/stats` with its cache and fan-out (0.25k), `vfs.FS.Flush` and the client byte counters (0.05k), config, node wiring and `stats seal` (0.15k); the TypeScript grows by about 0.45k (the page, the range picker, the member menu, the theme switch), not Go | +1.9k planned |
| Search, markdown and the Media tab (docs/18, docs/13 §4 and §10, planned 2026-10-11): the library flag and codec 6 (0.12k), `internal/search` with its tokenizer, segment codec, leader pass, catalogue rules and query (0.95k + 0.3k), the markdown renderer (0.45k), the search, render and media routes with the status and CLI (0.25k), soak phase 21 (0.3k); the TypeScript grows by about 1.0k (search box and results, the markdown view, the Media tab), not Go | +2.4k planned |
| Snapshots on mirror disks (docs/16, planned 2026-10-12): the mirror row, the catalogue, ops 19–21, the sync hold in `GCDelete` and codec 7 (meta 0.25k); `internal/mirror`, the disk format, the object and tree store, the sync walker, reconciliation and the standalone readers (0.75k); the tick's schedule and thinning (0.15k); `/.snapshots`, the snapshot handle index, the mirror RPC routes, the ROFS mapping, revert and tree restore (vfs, rpc and gateways 0.45k); `mirror`, `snapshot`, `versions`, the `export` rename, status and check (0.35k); the web handler's routes (0.3k); soak phase 19 and the report's charts and table (0.35k); config (0.05k); the TypeScript grows by about 0.6k, not Go. PR 1 measured 2026-10-12, comments included: meta 0.48k, `internal/mirror` 1.13k (the plan said 0.25k and 0.75k: the snapshot writer, the crash-safe store and the standalone export came out larger) | +2.65k planned, +1.6k of it landed |
| Files on disk (docs/08 §Files on disk, planned 2026-10-14, the first PR of the users plan): `sysfs.FS.Chmod`, 0700 directories tightened at open in the store and the cache (0.03k), the disk and mirror mode and owner warnings in `config.Warnings` (0.04k), the full-disk-encryption probes on three OSes with their parsers and the hourly cache in `internal/admin` (0.15k), the wiring into `status` and `check` (0.02k). Measured 2026-10-14, comments included: 0.41k; the Linux device chain with its mountinfo parser and the three OS probe files are the difference | +0.25k planned, +0.41k measured |
| Users, groups and permissions (docs/21, planned 2026-10-14, after 1.0): the rows, ops 28–31, codec 14, `Access` and the FSM checks (meta 0.6k); signing, the needs table, bindings and the CLI (0.9k); `vfs.FS.As`, the WAL actor, the NFS and SMB actors (0.5k); the browser's sessions and filters (0.4k Go, 0.3k TypeScript); the audit writer and `audit ls` (0.4k); soak phases 28–31 are test code | +2.8k planned |
| `internal/s3srv` (the S3 gateway, deferred; not in the tree) | ~900 planned |

The original budget was ≈ 7.9k through M4; the measured count is more than twice that. The
difference is explained by the parts the plan sized optimistically rather than by scope
creep: the repair scanner's partial fills and migrations, the admin reports and CLI, and
the soak engine. Keep the rule in `CLAUDE.md`: prefer deleting over abstracting, and a
feature that needs far more than its row here means the design is wrong.

Tests are not budget-limited but each must cite the invariant it defends.

## Milestones

| M | Scope | Acceptance |
|---|---|---|
| M0 | These docs, `invariants.md`, `check-invariants.sh`, Makefile, lint, CI skeleton, CLAUDE.md, skipped test stubs for every I-n | `make lint` green with every invariant mapped |
| M1 | Single node: clock, sysfs, ec, chunk, meta (in-proc), vfs, nfsgw, `serve`+`mount` | billy suites, `TestVFS_OracleRandomOps`, `TestNFS_WriteSequence_MultiRPC`, `make test-mount` |
| M2 | Cluster: tailscale, config, cluster (raft), rpc, placement over live set, testharness, admin reports, node/volume CLI | all cluster scenarios, `TestCluster_WriteOnA_ReadOnB_ShardsOnDistinctNodes`, `TestCrash_AtEveryPoint`, 3 real devices |
| M3 | Durability: repair, GC, scrub, sweep, `check`/`fsck`, `volume set-redundancy` | GC/torn-shard/ack-durability tests, `TestModelCheck_Long` 20 min × 3 seeds |
| M3b | Soak suite (compose + live), `/dev/*` endpoints, HTML report | `make soak` green, `tailraid verify` green on the real cluster |
| M4 | Ops: service install, mount helpers, Windows portmap stub, `make release` | 6 release artifacts; Windows checklist executed |
| M5a | *Deferred (2026-10-08).* S3 API gateway (own SigV4 server over vfs) | I-19/I-20/I-21, NFS↔S3 oracle, rclone conformance |
| M5b | *Deferred (2026-10-08).* Remote S3 storage nodes (own client) | mixed-kind cluster tests, live bucket test |

M5 is deferred, not dropped: the volume is complete without it. What it would add is
S3-protocol access for devices without an NFS client (Windows Home, rclone, backup tools)
and buckets as off-site storage nodes with Raft snapshot backup, which is the planned fix
for the "permanent loss of the Raft majority" gap in docs/05. The seams it needs
(`Node.Kind`, the per-node shard client, `Peers.Rank` ordering) are in place, so taking it
up later is additive. docs/10 keeps the design; its invariants stay as stubs.

## Document map

| Doc | Defines |
|---|---|
| [01-data-model](01-data-model.md) | Types, chunking, hashing, on-disk formats, handles, the trash index, the local cache and its pins. I-1..I-3, I-22, I-156 |
| [02-consistency-and-raft](02-consistency-and-raft.md) | FSM commands and apply rules (the trash, restore, purge, protection), Raft wiring, membership, forwarding, reads. I-4..I-6, I-26..I-29, I-152..I-155, I-222 |
| [03-erasure-coding-and-placement](03-erasure-coding-and-placement.md) | `ec.Codec`, rendezvous placement, degraded writes, policy migration. I-8..I-10, I-30, I-32, I-33 |
| [04-write-path-and-durability](04-write-path-and-durability.md) | Overlay/WAL, flush protocol, acks, fsync policy, the write path's memory model, readahead, concurrent writers and whole rewrites. I-7, I-11, I-34..I-37, I-63, I-78..I-80, I-83, I-166, I-168, I-169, I-171, I-223 |
| [05-failure-model](05-failure-model.md) | Crash points, partitions, what is and is not survived, suspect marks. I-12, I-38..I-40, I-170 |
| [06-gc-and-repair](06-gc-and-repair.md) | Tombstones, GC grace, the retention purge, DELETE fence, sweep, repair scanner, scrub, drain, migration, the ephemeral `/.tailraid` tree, the sync hold and the mirror schedule. I-13..I-15, I-41..I-44, I-157, I-182, I-195 |
| [07-nfs-gateway](07-nfs-gateway.md) | go-nfs handler, handles, op mapping, macOS and Linux mounts, the in-tree go-nfs copy. I-16, I-17, I-45..I-47, I-100, I-167 |
| [08-security](08-security.md) | The trust model on the mesh, the table authorizer, the join code and the introduce, cluster ID, S3 keys, the stats route. I-18, I-48..I-50, I-199 |
| [09-testing-strategy](09-testing-strategy.md) | TDD workflow, seams, harness, soak suite (no invariants) |
| [10-s3](10-s3.md) | Own S3 server and client, S3 storage nodes. I-19..I-21, I-51..I-55 |
| [11-chunk-lifecycle](11-chunk-lifecycle.md) | Chunk and shard state machines, event catalogue, transition cells L-n. I-56, I-57 |
| [12-operations](12-operations.md) | Service install per OS, log files, the Windows mount, `/dev/*` fault injection, releases, self-update, `init` without options, machine and network vitals. I-59..I-61, I-76, I-84, I-85, I-190 |
| [13-web-ui](13-web-ui.md) | Browser dashboard and file browser on the mesh listener: members-only authorization, Range reads, atomic resumable uploads, batch rename/move/delete, the trash view, folder flags, tiles and thumbnails, the album view and now-playing bar, pins, the Stats page and the theme switch, the style guide. I-68..I-73, I-81, I-82, I-158..I-165, I-197, I-198, I-200, I-226 |
| [14-disks](14-disks.md) | Several disks per device: disk rows as placement targets, host domains and the host cap, marker, per-disk budget and liveness, host-aware policy, home placement (domain skew and capacity within a host as one rule, behind a gate). I-86..I-91, I-172, I-294..I-296 |
| [15-smb-gateway](15-smb-gateway.md) | SMB2 gateway for Windows: the library and its backend contract, the adapter over the vfs, case-insensitive names, password and `net use` mount, caps, known gaps, handles judged by inode. I-106..I-110, I-224, I-225 |
| [16-snapshots](16-snapshots.md) | Snapshots on mirror disks: mirrors, the self-describing differential format, the sync and its GC hold, the schedule and thinning, the catalogue and reconciliation, reading, versions and restore, disaster recovery. I-179..I-181, I-183, I-186 (I-182 in 06; I-184 in 01; I-185 in 07; I-187 in 13; I-188 in 12) |
| [17-stats](17-stats.md) | Time series kept in the volume: the sampler and its rows, gzip segments per node and hour, daily and monthly rollups, retention, the live-edge RPC and the Stats page's query, the storage budget. I-191..I-194, I-196 |
| [18-search](18-search.md) | Search and the index kept in the volume: what is indexed, segments and the leader's pass, queries, the media catalogue from sidecars and tags, the library folder flag |
| [19-secrets](19-secrets.md) | One encrypted secrets store: ciphertexts in the FSM, the cluster key in each device's OS keychain and fetched from a member, the keychain layer, `secret set\|ls\|rm`, the S3 and TMDB keys moved in. I-303..I-309 |
| [20-mesh](20-mesh.md) | tailraid's own WireGuard mesh in place of Tailscale: the TUN and its privileges, the key as identity, the subnet and addresses, the join code and the introduce, endpoint candidates and fan-out, the relay through a member, port mapping, the path class in the storage code. I-316..I-330 |
| [21-users](21-users.md) | Users, groups and permissions: identities with a key per device from an invite, signed CLI requests, grants with three roles enforced in the FSM, the gateways' actors, the browser sign-in, personal secrets, the audit log, the standards mapping. I-342..I-359 |
| [invariants.md](invariants.md), [lifecycle.md](lifecycle.md) | Generated tables of every I-n / L-n with its tests |

Numbers I-23, I-24, I-25 and I-31 are intentionally unused (their statements were merged
into I-11 and I-7 during review); I-93 is intentionally unused since the mesh (docs/20:
a member's address never changes, so no request from a non-member source is accepted);
I-58 is intentionally unused since the Windows portmap
stub went with the SMB gateway of docs/15. Numbers are never reused.
