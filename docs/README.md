# tailraid design documents

Read in order. Each design doc ends with numbered invariants (`I-N`) and the tests that
prove them; `invariants.md` is generated from those sections by `scripts/check-invariants.sh`.

| Doc | Contents |
|---|---|
| [00-overview.md](00-overview.md) | Goals, non-goals, glossary, one-page architecture, size budget |
| [01-data-model.md](01-data-model.md) | Inodes, directories, chunks, stripes, shards, handles; FSM state and memory |
| [02-consistency-and-raft.md](02-consistency-and-raft.md) | What lives in Raft, the command set, leader forwarding, stale reads, snapshots, bootstrap/join |
| [03-erasure-coding-and-placement.md](03-erasure-coding-and-placement.md) | k+m per chunk, shard layout and padding, rendezvous placement, policy changes |
| [04-write-path-and-durability.md](04-write-path-and-durability.md) | NFS WRITE → WAL → flush → Commit; ack semantics; the durability contract |
| [05-failure-model.md](05-failure-model.md) | Survived vs. not-survived failures, crash points, partitions, clocks |
| [06-gc-and-repair.md](06-gc-and-repair.md) | Refcounts, tombstones, GC with grace, sweep, repair scanner, scrub, drain and re-encode |
| [07-nfs-gateway.md](07-nfs-gateway.md) | go-nfs behaviour, handles, mount commands for macOS and Linux, POSIX gaps |
| [08-security.md](08-security.md) | Trust model, WhoIs authorization, join token, cluster-ID handshake, secrets, threats, non-goals |
| [09-testing-strategy.md](09-testing-strategy.md) | Invariant discipline, TDD workflow, seams, test pyramid, soak suite, CI |
| [10-s3.md](10-s3.md) | *Deferred.* Our S3 implementation: API gateway over the vfs and remote S3 buckets as storage nodes |
| [11-chunk-lifecycle.md](11-chunk-lifecycle.md) | Chunk and shard state machines, the event catalogue, transition cells `L-n` |
| [12-operations.md](12-operations.md) | Service install per OS, log files, the Windows portmap stub, /dev fault injection for live soak runs, releases |
| [13-web-ui.md](13-web-ui.md) | Browser dashboard (balance, integrity) and file browser with media playback on a tailnet listener; same-user authorization; the style guide and lookdev |
| [14-disks.md](14-disks.md) | Several disks per device: disks as placement targets, the host as failure domain, the host cap, host-aware policy |
| [15-smb-gateway.md](15-smb-gateway.md) | SMB2 gateway for Windows: library, adapter over the vfs, case-insensitive names, password and `net use` mount, known gaps |
| [invariants.md](invariants.md) | Generated table: every invariant, its doc, statement and tests |
| [windows-checklist.md](windows-checklist.md) | Manual hardware validation of the Windows mount path (M4) |

Status of each milestone is tracked in the repository `CLAUDE.md`.
