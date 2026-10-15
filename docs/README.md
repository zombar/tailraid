# tailraid documentation

The guides are for running a cluster; the design documents below them are for
understanding and changing it.

| Guide | Contents |
|---|---|
| [guide-using.md](guide-using.md) | The browser UI, the trash, folder flags, pinned folders, search, posters for the Media tab |
| [guide-snapshots.md](guide-snapshots.md) | Mirrors, the snapshot schedule, browse, restore and revert, reading a mirror without the cluster |
| [guide-capacity.md](guide-capacity.md) | Devices, disks and S3 buckets, draining, the redundancy policy, voters |
| [guide-operating.md](guide-operating.md) | Secrets, the command reference, configuration, backup and recovery, upgrading, verifying a download, `tailraid verify` |
| [guide-contributing.md](guide-contributing.md) | The Docker soak, the three-VM rig, releasing |

## Design documents

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
| [12-operations.md](12-operations.md) | Service install per OS, log files, the Windows mount, /dev fault injection for live soak runs, releases |
| [13-web-ui.md](13-web-ui.md) | Browser dashboard (balance, integrity) and file browser with media playback on a tailnet listener; same-user authorization; the style guide and lookdev |
| [14-disks.md](14-disks.md) | Several disks per device: disks as placement targets, the host as failure domain, the host cap, host-aware policy |
| [15-smb-gateway.md](15-smb-gateway.md) | SMB2 gateway for Windows: library, adapter over the vfs, case-insensitive names, password and `net use` mount, known gaps |
| [16-snapshots.md](16-snapshots.md) | Snapshots on mirror disks: independent, offline-capable, self-describing and differential copies of the volume; the sync, the schedule, reading, versions and restore |
| [18-search.md](18-search.md) | Search and the index kept in the volume: what is indexed, segments and the leader's pass, queries, the media catalogue from sidecars and tags, the library folder flag |
| [17-stats.md](17-stats.md) | Time series kept in the volume: per-node samples, gzip segments and rollups, retention, the live edge, the Stats page's charts, the storage budget |
| [19-secrets.md](19-secrets.md) | One encrypted secrets store for the cluster: ciphertexts in the FSM, the cluster key in each device's OS keychain, fetched from a member; the S3 and TMDB keys move in |
| [21-users.md](21-users.md) | Users, groups and permissions: a key per device from an admin's invite, signed CLI requests, grants with three roles enforced in the FSM, actors on every gateway, the browser sign-in, personal secrets, the audit log, the standards mapping |
| [invariants.md](invariants.md) | Generated table: every invariant, its doc, statement and tests |
| [windows-checklist.md](windows-checklist.md) | Manual hardware validation of the Windows SMB mount path |
| [vms.md](vms.md) | Three local VMs (macOS, Linux, Windows) on a throwaway tailnet forming one volume, each OS first in turn; the rounds, the soak and the results |

Status of each milestone is tracked in the repository `CLAUDE.md`.
