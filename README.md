# tailraid

**Status: M0 — design docs and tooling. No working code yet.**

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

## Building

```
make build          # dist/tailraid for this machine (Go 1.27, CGO_ENABLED=0)
make check          # gofmt, lint, invariant check, go test -short -race
```

Reference for what not to do: the previous attempt,
[tunnelmesh](https://github.com/tunnelmesh/tunnelmesh), whose useful parts informed this
design and whose bugs informed its tests.
