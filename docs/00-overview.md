# 00 — Overview

`tailraid` turns the devices on a Tailscale tailnet into one shared volume that behaves like
a modern RAID array: file data is Reed-Solomon erasure-coded into shards spread across
devices, identical content is stored once, and the volume is mounted locally on Windows,
macOS and Linux through the operating system's own NFSv3 client. Metadata is kept
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
3. **Dedup**: identical 1 MiB chunks are stored once regardless of path or how often they
   are written.
4. **Native mounts** on Windows, macOS, Linux with no kernel drivers to install.
5. **Small, provable code**: about 8k production lines through M4 (about 10k with S3), with
   a test suite that would have caught every bug the predecessor shipped.
6. **Day-zero membership management**: add, drain, remove, promote nodes and change `k+m`
   at any time without downtime.

## Non-goals (out of scope)

S3 features beyond the implemented subset (versioning, ACLs, policies, lifecycle, tagging,
SSE, object lock, presigned POST); web UI; metrics stack (a `status` JSON instead);
encryption at rest (WireGuard covers transit); quotas; file versioning / recycle bin;
xattrs; NLM locking (mount with `nolock`); snapshots; hard links; NFSv4; multiple
independent volumes (S3 buckets are top-level directories of the one volume);
content-defined chunking; compression (hook left); tiering; fsck-from-shards metadata
reconstruction; WAL mirroring to a peer before ack (noted as a later option).

## Glossary

| Term | Meaning |
|---|---|
| **node** | A participant in the cluster. `Kind=device` runs `tailraid serve` (Raft, RPC, gateways, shard store). `Kind=s3` is a storage-only bucket: never a Raft member, never a gateway. |
| **member** | A node present in the replicated node table; devices are also Raft servers (voter or non-voter). |
| **voter** | A Raft server that counts toward quorum. Always-on devices, odd count. Laptops default to non-voter. |
| **volume** | The single filesystem tree managed by one cluster. Has a policy `Volume{ChunkSize, K, M, PolicyVersion}`. |
| **bucket** | A top-level directory of the volume as seen through the S3 API. |
| **inode** | A file, directory or symlink record in the FSM, identified by `Ino` (monotonic, never reused) plus `Gen`. |
| **chunk** | A fixed-size (default 1 MiB) span of a file at offset `idx*ChunkSize`, content-addressed by SHA-256 of its logical bytes. The unit of dedup and of erasure coding. |
| **stripe** | One chunk encoded into `k` data shards and `m` parity shards. Chunk and stripe are 1:1. |
| **shard** | One of the `k+m` encoded pieces of a chunk; stored on exactly one node; key `hex(hash).idx`. |
| **slot** | Position `idx` in a chunk's `Placement []NodeIdx`. Slot value 0 means "no node holds this shard yet" (degraded). |
| **placement** | The `k+m` node indices that hold a chunk's shards, chosen by rendezvous hashing at write time and **stored** in the chunk table. |
| **policy version** | `Volume.PolicyVersion`, bumped by `volume set-redundancy`. Chunks whose `(K,M)` differ from the current policy are migrated lazily by the repairer. |
| **gateway** | The NFS (and later S3) front end running on a device; any device is a gateway for its own user. |
| **live** | A node that answered `/health` within the last 15 s (probed every 5 s). Not stored in Raft. |
| **WAL** | Per-inode write-ahead log on the gateway's disk that makes NFS `FILE_SYNC` honest before the chunk is flushed to the cluster. |

## Architecture

```
 NFS client (OS)  ──127.0.0.1──▶  nfsgw (go-nfs Handler, handles = ino+gen)
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
      cluster (raft wiring, leader forwarding,   rpc (HTTP/1.1 on <tsIP>:20481:
      membership, prober/liveness, events)      GET/PUT/HEAD/DELETE /v1/shards/{key},
      raft TCP <tsIP>:20480 + cluster-id        POST /v1/apply, /v1/members, /v1/join,
      handshake; raft-boltdb; FileSnapshotStore GET /health; WhoIs authorizer)
                        ▲
      tailscale (CLI shell-out: status/whois JSON, binary finder, Fake)
      repair (leader only: GC of tombstones after grace; scanner refills missing/dead slots; scrub)
```

Ports: Raft `<tsIP>:20480`, node RPC `<tsIP>:20481`, S3 gateway `<tsIP>:20443`, NFS
`127.0.0.1:20490` (Windows: `127.0.0.1:2049` plus a portmapper stub on `127.0.0.1:111`).

Identity and transport security come from Tailscale: every RPC connection is authorized by
resolving the source IP with `tailscale whois` and matching the Tailscale stable ID recorded
for a member. No TLS of our own.

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
| `cmd/tailraid`, `internal/admin` | ~1,400 |
| `internal/{clock,sysfs,ec,chunk}` | ~840 |
| `internal/meta` (FSM, codec, placement) | ~1,250 |
| `internal/vfs` (overlay, WAL, flusher, reader, billy) | ~1,500 |
| `internal/nfsgw`, `internal/repair` | ~630 |
| `internal/{cluster,rpc,tailscale,config}` | ~1,760 |
| `internal/{mount,svc,dev}` | ~720 |
| **Through M4** | **≈ 7.9k** |
| `internal/s3`, `internal/s3srv` (M5) | ~1,700 |
| `internal/soak` (shipped part of `tailraid verify`) | ~400 |

Tests are not budget-limited but each must cite the invariant it defends.

## Milestones

| M | Scope | Acceptance |
|---|---|---|
| M0 | These docs, `invariants.md`, `check-invariants.sh`, Makefile, lint, CI skeleton, CLAUDE.md, skipped test stubs for every I-n | `make lint` green with every invariant mapped |
| M1 | Single node: clock, sysfs, ec, chunk, meta (in-proc), vfs, nfsgw, `serve`+`mount` | billy suites, `TestVFS_OracleRandomOps`, `TestNFS_WriteSequence_MultiRPC`, `make test-mount` |
| M2 | Cluster: tailscale, config, cluster (raft), rpc, placement over live set, testharness, admin reports, node/volume CLI | all cluster scenarios, `TestE2E_ThreeNodes…`, `TestCrash_AtEveryPoint`, 3 real devices |
| M3 | Durability: repair, GC, scrub, sweep, `check`/`fsck`, `volume set-redundancy` | GC/torn-shard/ack-durability tests, `TestModelCheck_Long` 20 min × 3 seeds |
| M3b | Soak suite (compose + live), `/dev/*` endpoints, HTML report | `make soak` green, `tailraid verify` green on the real cluster |
| M4 | Ops: service install, mount helpers, Windows portmap stub, `make release` | 6 release artifacts; Windows checklist executed |
| M5a | S3 API gateway (own SigV4 server over vfs) | I-19/I-20/I-21, NFS↔S3 oracle, rclone conformance |
| M5b | Remote S3 storage nodes (own client) | mixed-kind cluster tests, live bucket test |

## Document map

| Doc | Defines |
|---|---|
| [01-data-model](01-data-model.md) | Types, chunking, hashing, on-disk formats, handles. I-1..I-3, I-22 |
| [02-consistency-and-raft](02-consistency-and-raft.md) | FSM commands and apply rules, Raft wiring, membership, forwarding, reads. I-4..I-6, I-26..I-29 |
| [03-erasure-coding-and-placement](03-erasure-coding-and-placement.md) | `ec.Codec`, rendezvous placement, degraded writes, policy migration. I-8..I-10, I-30, I-32, I-33 |
| [04-write-path-and-durability](04-write-path-and-durability.md) | Overlay/WAL, flush protocol, acks, fsync policy. I-7, I-11, I-34..I-37 |
| [05-failure-model](05-failure-model.md) | Crash points, partitions, what is and is not survived. I-12, I-38..I-40 |
| [06-gc-and-repair](06-gc-and-repair.md) | Tombstones, GC grace, DELETE fence, sweep, repair scanner, scrub, drain, migration. I-13..I-15, I-41..I-44 |
| [07-nfs-gateway](07-nfs-gateway.md) | go-nfs handler, handles, op mapping, per-OS mount. I-16, I-17, I-45..I-47 |
| [08-security](08-security.md) | WhoIs authorization, join token, cluster ID, S3 keys. I-18, I-48..I-50 |
| [09-testing-strategy](09-testing-strategy.md) | TDD workflow, seams, harness, soak suite (no invariants) |
| [10-s3](10-s3.md) | Own S3 server and client, S3 storage nodes. I-19..I-21, I-51..I-55 |
| [11-chunk-lifecycle](11-chunk-lifecycle.md) | Chunk and shard state machines, event catalogue, transition cells L-n. I-56, I-57 |
| [invariants.md](invariants.md), [lifecycle.md](lifecycle.md) | Generated tables of every I-n / L-n with its tests |

Numbers I-23, I-24, I-25 and I-31 are intentionally unused (their statements were merged
into I-11 and I-7 during review); numbers are never reused.
