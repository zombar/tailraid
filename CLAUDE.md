# tailraid

One mountable, erasure-coded, deduplicated volume across the devices of a Tailscale
tailnet. Go, single static binary, pure Go dependencies, `CGO_ENABLED=0`. Metadata in
hashicorp/raft, data as Reed-Solomon shards of content-addressed 1 MiB chunks, mounted via
NFSv3 on loopback (and later an S3 API). Design docs live in `docs/`; read `docs/README.md`
first. The approved plan is `~/.claude/plans/i-want-to-create-starry-kahn.md`.

**Milestone status:** M0–M4 built. Pending validation outside this machine: the Windows checklist on real hardware and the multi-device run on the real tailnet (`install`, then `verify --destructive --restarts`). M5 (S3 gateway, S3 storage nodes) is deferred as of 2026-10-08: the volume is complete without it; docs/10 keeps the design and its invariants stay as stubs.

## Layout

```
cmd/tailraid/       the binary: node daemon, gateways, management CLI (cobra from M1)
cmd/soak/           docker-compose soak driver (test tooling)
internal/clock      Clock seam            internal/sysfs    FS seam (shard store only)
internal/ec         Reed-Solomon codec    internal/chunk    shard store, cache, sweep
internal/meta       FSM, commands, codec, placement, read API
internal/vfs        overlay+WAL, flusher, reader, billy.Filesystem
internal/nfsgw      go-nfs handler        internal/repair   GC, repair scanner, scrub
internal/cluster    raft wiring, membership, liveness, forwarding
internal/rpc        node HTTP RPC + WhoIs authorizer   internal/tailscale  CLI shell-out + Fake
internal/config     YAML config           internal/mount    per-OS mount helpers, PMAP stub
internal/svc        service install       internal/admin    status/check/describe reports
internal/s3, s3srv  (M5, deferred) SigV4 + lightweight S3 server/client
internal/dev        /dev/* endpoints, only with dev.enabled
internal/soak       Env interface, scenarios, HTML report (shared by cmd/soak and `tailraid verify`)
internal/testharness  in-process Cluster, FaultNet, Hooks (test only)
test/invariants     skipped stubs that cite not-yet-implemented I-n / L-n
test/e2e, test/mount, test/soak
docs/               whitepaper 00–11, generated invariants.md + lifecycle.md, windows-checklist.md
scripts/check-invariants.sh   enforces docs ↔ tests mapping
```

## Commands

```
make build        static binary → dist/tailraid        make test        -short -cover
make test-race    -short -race (pre-commit)             make test-long   model checker etc. (minutes)
make lint         golangci-lint + check-invariants      make invariants  regenerate docs/invariants.md, lifecycle.md
make fmt-check    gofmt                                 make vuln        govulncheck
make check        fmt-check + lint + test-race          make release     6 static binaries + sha256sums
make test-mount   real NFS mount (sudo -E)              make soak        docker-compose soak → report.html
```

Lint and vuln tools are Go tools pinned in `tools.mod` (`go tool -modfile=tools.mod …`), so
the main `go.mod` holds only runtime dependencies. `make tidy` tidies both.

## How we work: docs first, then test first

1. **Docs before code.** Every component starts as a section in `docs/` ending with numbered
   invariants (`I-n`) and a test table. Chunk state transitions are cells (`L-n`) in the
   transition tables of `docs/11-chunk-lifecycle.md`. Numbers are never reused.
2. **Red test first.** Write the test that proves the invariant/cell, cite it above the func
   (`// Invariant: I-7, I-12`, `// Lifecycle: L-3`, or `// Regression: #<issue> L-3`), watch
   it fail, then write the minimal code, then refactor with `-race` green. The `_test.go`
   lands in the same commit as or before the implementation.
3. **Stubs.** Until a real test exists, a skipped stub in `test/invariants/` cites the ID.
   Delete the stub in the commit that lands the real test.
4. **`make check` before every commit** (lefthook runs it). `scripts/check-invariants.sh`
   fails if any I-n/L-n has no citation, any citation is undefined, or the generated tables
   are stale.

## Conventions

- Errors: wrap with `fmt.Errorf("…: %w", err)`; typed sentinel errors asserted with `errors.Is`.
- `ctx` is the first argument and is never stored in a struct.
- Small consumer-defined interfaces (`Proposer`, `Peers`, `ShardClient`, `Cluster`,
  `tailscale.Client`, `Authorizer`); hand-written fakes, never `testify/mock`.
- Table-driven tests; stdlib `testing` plus small local helpers, no assertion frameworks.
- **No test-mode switches in production code** (no `TAILRAID_TEST`, no skip-fsync flags, no
  `sync bool` on the WAL). Speed comes from in-memory stores; durability tests use real temp dirs.
- Terminology: "shard store" (`internal/chunk`), "dead"/"live" for liveness, "degraded" for a
  chunk with < k+m good slots, "orphan shard" for an uploaded-but-uncommitted or unplaced
  shard, "re-place" for `SetPlacement` work, `ErrFull` for the 507 condition, wire names
  (`NFS3ERR_ROFS`) in tables and errno names (`EROFS`) in prose, `(k,m)` for a policy.
- The only seams: `clock.Clock`, `sysfs.FS` (shard store only), `FaultNet`, `cluster.Hooks`,
  `tailscale.Client`. Raft is not wrapped.
- No `time.Sleep` for synchronization (forbidigo). Wait on channels, futures or an
  `Eventually` helper driven by the fake clock.
- Review checklist for every PR: every write-path test writes at a **non-zero offset**;
  every size-bounded path has a `{0, 1, max−1, max, max+1}` table; every "same answer on every
  node" function has an **N-node determinism** test; every randomized test takes `-seed` and
  prints it on failure; `goleak.VerifyTestMain` in packages that start goroutines.
- Prefer deleting over abstracting. Production code is ≈ 15k lines through M4 (measured; the plan's 8k was optimistic,
  see docs/00). If a feature needs far more than its row in docs/00, the design is wrong.
- Logging with `log/slog`; no metrics library (`tailraid status --json` instead).

## CLI (target surface; see the plan for details)

`init`, `join`, `serve`, `token`, `reset` (wipes local Raft state only, never shards) ·
`status`, `node ls|describe|promote|demote|
set-capacity|drain|remove` · `check`, `repair`, `gc`, `fsck` · `volume
describe|grow|set-redundancy` · `chunk describe <hash>`
(lifecycle state, placement, refs, history; prints "(event ring disabled)" without `dev.enabled`) · `verify` (live soak + HTML report) · `update [--check]`
· deferred with M5: `node add-s3|keys`, `bucket ls|describe|create|rm`, `s3 keys`
· `mount`, `umount`, `install`, `uninstall`, `start`, `stop`, `logs` · `version`.

## Out of scope (keep it small)

S3 beyond the implemented subset (versioning, ACLs, policies, lifecycle, tagging, SSE,
object lock, presigned POST), web UI, metrics stack, encryption at rest, quotas,
versioning/recycle bin, xattrs, NLM locking, snapshots, hard links, NFSv4, multiple
independent volumes, content-defined chunking, compression (hook left), tiering,
fsck-from-shards metadata reconstruction, WAL mirroring.
