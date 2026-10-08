<p align="center">
  <picture>
    <source media="(prefers-color-scheme: dark)" srcset="docs/assets/logo-dark.svg">
    <img src="docs/assets/logo-light.svg" alt="tailraid" width="300">
  </picture>
</p>

**Status:** M0–M3b complete: single node, Raft cluster, repair/GC, model checker, soak suite.
Validated on a real macOS NFS mount and a four-node docker-compose cluster; multi-device
validation on a real tailnet, service install (M4) and the S3 gateway (M5) are next.

tailraid turns the devices on your Tailscale tailnet into one shared volume that behaves
like a modern RAID array: data is Reed-Solomon erasure-coded across devices so any `m` of
them can be off, lost or corrupted and the volume stays readable and writable; identical
content is stored once; and it mounts as a normal filesystem on macOS, Linux and Windows
through the OS's own NFS client. A single static Go binary is the node daemon, the NFS (and
later S3) gateway and the management CLI.

## Durability contract (excerpt from `docs/05-failure-model.md`)

"No data loss" means: every byte whose write was acknowledged is durable at once (fsynced
to the issuing gateway's write-ahead log) and readable there immediately; it is readable
from every live gateway once flushed to the cluster, which happens within 2 s after writes
to that file pause (or at the dirty-byte thresholds while they continue). This holds under
the loss of any m nodes' disks, a gateway crash or power loss at any instant, a leader
crash or partition at any point, corruption of up to m shards per stripe, and a crash at any
fsync step of a shard write.

Not covered: more than m nodes lost before repair completes; permanent loss of the Raft
majority; storage that lies about fsync; more than m corrupt shards in one stripe;
destruction of the issuing gateway's own disk before its write-ahead log has flushed.

## Where to look

- `docs/README.md` — the design documents, in reading order.
- `docs/invariants.md`, `docs/lifecycle.md` — every promise the system makes and the tests
  that prove it (generated; `make lint` fails if they drift).
- `CLAUDE.md` — how the code is written: docs first, test first, conventions, scope.
- `Makefile` — `make help`.

## Quick start

```
tailraid init --k 2 --m 1 --name studio        # first (always-on) device: creates the volume
tailraid serve                                 # on every device; prints the mount command
tailraid join <ip-of-first-device> <token>     # other devices, then `tailraid serve`
tailraid mount                                 # mounts via the OS NFS client (~/TailRAID)
tailraid status                                # health, members, slots, warnings
tailraid check / repair / fsck / gc            # replication health and maintenance
tailraid verify --report out.html              # soak scenarios against the live cluster
```

## Building

```
make build          # dist/tailraid for this machine (Go 1.27, CGO_ENABLED=0)
make check          # gofmt, lint, invariant check, go test -short -race
make soak           # four-node docker-compose soak → report.html (needs Docker)
```

Reference for what not to do: the previous attempt,
[tunnelmesh](https://github.com/tunnelmesh/tunnelmesh), whose useful parts informed this
design and whose bugs informed its tests.
