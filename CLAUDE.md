# tailraid

One mountable, erasure-coded, deduplicated volume across the devices of a Tailscale
tailnet. Go, single static binary, pure Go dependencies, `CGO_ENABLED=0`. Metadata in
hashicorp/raft, data as Reed-Solomon shards of content-addressed 1 MiB chunks, mounted via
NFSv3 on loopback on macOS and Linux and SMB2 on Windows (and later an S3 API). Design docs live in `docs/`; read `docs/README.md`
first. The approved plan is `~/.claude/plans/i-want-to-create-starry-kahn.md`.

**Milestone status:** M0–M4 built. The multi-device validation runs locally on three VMs (docs/vms.md: macOS, Linux and Windows guests on a throwaway headscale, `make vms-rounds`, each OS running `init` in turn, `verify --destructive --restarts` per round; results in docs/reports/vms/). Still manual: the Windows checklist on real hardware, the phone web UI run, an overnight endurance run and a throttled uplink. M5 (S3 gateway, S3 storage nodes) is deferred as of 2026-10-08: the volume is complete without it; docs/10 keeps the design and its invariants stay as stubs. The browser UI (docs/13: dashboard and file browser on `<tsIP>:20482`) is built; its lookdev in `docs/assets/lookdev/` is the signed-off markup the TypeScript renders. Pending: a run on the real tailnet from a phone (same-user rule) and the manual QA list in docs/13. `tailraid init` needs no options (`--name`, `--chunk`, `--token` and the hidden `--k`/`--m` are overrides for test rigs, docs/12 §7): budgets derive from each data disk (docs/01), data slots prefer voters and the leader picks the policy from the member table (docs/03); the soak suite's phase 11 checks all three. Large files stream (docs/04 §9): the flusher holds at most `ChunkWorkers` chunks, a WRITE at a cap waits for the flusher instead of failing, web uploads are atomic and resumable (docs/13 §4), and phase 12 writes a file many times the WAL cap, watches the heap and checks where its chunks landed; `status` carries the node's machine vitals (docs/12 §8) and the soak report charts them. Pending: `make soak` and `tailraid verify` on the real tailnet with phase 12, and the macOS and Windows soft-mount checks. Several disks per device (docs/14) is built: a disk is a placement target with its host as failure domain, the host cap spreads stripes across hosts first, the automatic policy counts disks and survives one host, a `disk_id` marker keeps an unmounted path closed, and `node add-disk` adds one live; `make soak` (4 hosts × 3 targets) and `make soak-single` (one host, four disks) pass phase 13 on Docker (2026-10-09). Pending: a second disk on a real tailnet host. Devices are never placement targets (docs/14 §9, 2026-10-09): `init` and `join` register only the disks the config lists, a host with none is a diskless host (a member, voter if configured, and gateway with no slot), a cluster with no disk fails writes with `ENOSPC`, `serve` refuses a pre-disks-only state with the re-create instructions, and `make soak` runs 4 hosts × 2 disks plus a diskless voter through phase 14. Pending: a diskless host on the real tailnet. The 2026-10 review (deps and vendoring, linters and hooks, then correctness) closed the Raft perimeter gap (the accept path now runs the WhoIs member check, I-92), resolved unknown-outcome proposals per command (I-95), made identity files atomic, bounded RPC bodies, cancelled losing hedged reads, fixed the sweep race and the WAL's failed-fsync and failed-reopen paths, added placement conflicts (`SetPlacement.Expect`, I-102), GC batches (I-103), streamed snapshots (I-104) and the codec check on join (I-105), fixed the boundary bugs it found (holes past the base list, GC on disks, k+m narrowing), and deleted the second placement implementation the tests had been proving. PR 3 (tests and the soak report) made the soak driver survive a phase that panics or hangs, restore what a phase changed, gate cluster mutations behind `--destructive`, report skipped checks and aborted runs honestly (I-111..I-114), bounded path length (I-115) and every request body (I-116), pinned the FSM's policy bounds (I-117), added the missing scenarios (concurrent writers on one inode, a thousand-level path, symlink loops, a handle across a gateway restart, a leader killed mid-flush, 100k-entry readdir), fuzz targets for handles, the WAL and the checksum file, and a gate rule against unconditional skips. PR 4 (hardening) confined the Linux unit and fixed the LaunchAgent restart rule (I-122), hands leadership over on shutdown (I-123), rotates the log file and rate-limits per-item warnings (I-124, `internal/logx`), reports readiness (I-125), validates the config and expands `~` (I-126), checks the web UI's Host and strips the join token (I-127), signs releases with minisign and verifies them in `update` with rollback and a downgrade guard (I-128, `cmd/tailraid-sign`; the public key is `internal/update.ReleaseKey`, the secret is the CI secret `MINISIGN_SECRET_KEY`), and adds `tailraid snapshot` and `tailraid recover` for quorum loss (I-129, docs/12 §9). README is prose and tables (`check-docs` allows two lists). Pending: a real release signed by CI, `install` on each OS with the confined unit, and `recover` once on real hardware. The SMB gateway for Windows (docs/15) is built: `internal/smbgw` adapts the vfs to `third_party/smb`, the in-tree copy of go-filesystems/smb with the credit-charge and timestamp fixes docs/15 §2 lists; Windows defaults to `listen.smb: 127.0.0.1:20445`, `init`/`join` generate the share password, `tailraid mount` runs `net use … /TCPPORT:20445` (Windows 11 24H2+), and the PMAP stub and the Windows NFS path are gone (its invariant number is intentionally unused). Pending: the Windows checklist on 24H2 hardware, and offering the two server fixes upstream.

## Layout

```
cmd/tailraid/       the binary: node daemon, gateways, management CLI (cobra from M1)
cmd/soak/           docker-compose soak driver (test tooling)
internal/clock      Clock seam            internal/sysfs    FS seam (shard store + WAL)
internal/ec         Reed-Solomon codec    internal/chunk    shard store, cache, sweep
internal/meta       FSM, commands, codec, placement, read API
internal/vfs        overlay+WAL, flusher, reader, billy.Filesystem
internal/nfsgw      go-nfs handler        internal/repair   GC, repair scanner, scrub
internal/smbgw      SMB2 gateway (Windows): adapter over vfs for third_party/smb, the in-tree copy of go-filesystems/smb with the fixes docs/15 §2 lists
internal/cluster    raft wiring, membership, liveness, forwarding
internal/rpc        node HTTP RPC + WhoIs authorizer   internal/tailscale  CLI shell-out + Fake
internal/config     YAML config           internal/mount    per-OS mount helpers (NFS; SMB via net use on Windows)
internal/svc        service install       internal/admin    status/check/describe reports
internal/dev        /dev/* endpoints, only with dev.enabled
internal/webui      browser UI: Go handler over vfs + admin; src/ TypeScript (no framework), src/app.css, dist/app.js (esbuild, committed)
internal/soak       Env interface, scenarios, HTML report (shared by cmd/soak and `tailraid verify`)
internal/testharness  in-process Cluster, FaultNet, Hooks (test only)
test/invariants     skipped stubs that cite not-yet-implemented I-n / L-n (S3 only now)
test/cluster        multi-node tests on the in-process harness: cluster, durability, repair, model checker
test/mount, test/soak
docs/               whitepaper 00–11, generated invariants.md + lifecycle.md, windows-checklist.md
scripts/check-invariants.sh   enforces docs ↔ tests mapping
```

## Commands

```
make build        static binary → dist/tailraid        make test        -short -cover
make test-race    -short -race (pre-commit)             make test-long   model checker etc. (minutes)
make lint         golangci-lint + check-invariants      make invariants  regenerate docs/invariants.md, lifecycle.md
make fmt-check    gofmt                                 make vuln        govulncheck
make check        fmt-check + vendor-check + lint + web-check + web-typecheck + test-race
make vendor-check vendor/ matches go.mod                make deadcode    deadcode + unparam (part of lint)
make lint-cross   golangci-lint under GOOS=linux, darwin and windows (the per-OS files; CI runs it)
make web-typecheck typescript-go over internal/webui    make spell       misspell over docs, TS, CSS, lookdev
make docs-check   scripts/check-docs.sh (links, anchors, I-n/L-n mentions, fences, README lists)
make actionlint / yaml-check / yamlfmt / shell-check    workflow, YAML and shell files
make hooks        install the lefthook hooks (pre-commit = check, pre-push = vuln, commit-msg = scope)
make release      6 static binaries + sha256sums
make test-mount   real NFS mount (sudo -E)              make soak        docker-compose soak → report.html
make web          esbuild → internal/webui/dist/app.js  make web-check   fails when dist is stale (part of check)
```

Runtime dependencies are vendored (`vendor/`, refreshed by `make tidy`, checked by
`make vendor-check`); builds use `-mod=vendor`. Dev tools are pinned in `tools.mod`, so the
main `go.mod` holds only runtime dependencies. Because the vendor tree matches `go.mod` and
not `tools.mod`, tools run as `go run -mod=readonly -modfile=tools.mod <import path>` (the
Makefile's `TOOL`), never `go tool -modfile` (it refuses with "inconsistent vendoring") and
never `go run … @latest` (it edits the module file). `make deadcode` (x/tools `deadcode`
and `unparam`) is part of `make lint`. actionlint alone lives in `actionlint.mod`: its YAML
library version conflicts with gosec's. The TypeScript is type-checked by typescript-go
(`make web-typecheck`); CSS and the lookdev are checked by `internal/webui/static_test.go`
(every class the TypeScript emits exists in `app.css` or the lookdev, every class the CSS
defines is used); the prose by `scripts/check-docs.sh` and misspell (`make docs-check`,
`make spell`). `make hooks` installs the lefthook hooks (lefthook is in `tools.mod`).

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
  (`NFS3ERR_IO`) in tables and errno names (`EROFS`) in prose, `(k,m)` for a policy.
- The only seams: `clock.Clock`, `sysfs.FS` (shard store and WAL only), `FaultNet`,
  `vfs.Flusher.Hooks` (crash points), `tailscale.Client`, and `cluster.Options.Stores`/`Timing`
  (Raft stores and timeouts; the harness passes in-memory stores). Raft is not wrapped.
- No `time.Sleep` for synchronization (forbidigo). Wait on channels, futures or the
  `testharness.Eventually` helper (it polls on the wall clock; tests that wait on a
  tailraid timer advance the fake clock inside the condition).
- Review checklist for every PR: every write-path test writes at a **non-zero offset**;
  every size-bounded path has a `{0, 1, max−1, max, max+1}` table; every "same answer on every
  node" function has an **N-node determinism** test; every randomized test takes `-seed` and
  prints it on failure; `goleak.VerifyTestMain` in packages that start goroutines.
- Prefer deleting over abstracting. Production code is ≈ 23.5k lines through the SMB gateway (measured 2026-10-09; 23.2k through the correctness review, 22.4k through several disks per device, 20.5k through streaming large files and machine vitals, 19.4k through init-without-flags, 18.1k through M4; the plan's 8k was optimistic,
  see docs/00), plus 1.7k lines of TypeScript. If a feature needs far more than its row in docs/00, the design is wrong.
- Logging with `log/slog`; no metrics library (`tailraid status --json` instead).
- Browser UI: TypeScript in `internal/webui/src`, no framework, no node/npm; esbuild is a Go
  tool in `tools.mod`. Run `make web` after touching `src/`; `make web-check` fails the commit
  otherwise. The markup is `docs/assets/lookdev/`; a visual change edits `src/app.css` and the
  lookdev page together, never the TS alone.

## CLI (target surface; see the plan for details)

`init` (no options: derived budget, auto redundancy, docs/12 §7), `join`, `serve`, `token`, `reset` (wipes local Raft state only, never shards) ·
`status` (also prints the web UI URL; exits 1 until the node is ready), `node ls|describe|promote|demote|
drain|remove|add-disk` (docs/14: disks are targets named `<host>/<n>`) · `check`, `repair`, `gc`, `fsck` · `volume
describe|grow <disk> <size>|auto|set-redundancy` · `chunk describe <hash>`
(lifecycle state, placement, refs, history; prints "(event ring disabled)" without `dev.enabled`) · `verify` (live soak + HTML report) · `update [--check|--rollback|--restart|--allow-downgrade]` · `snapshot <dir>`, `recover --voters` (docs/12 §9)
· deferred with M5: `node add-s3|keys`, `bucket ls|describe|create|rm`, `s3 keys`
· `mount`, `umount`, `install`, `uninstall`, `start`, `stop`, `logs` · `version`.

## Out of scope (keep it small)

S3 beyond the implemented subset (versioning, ACLs, policies, lifecycle, tagging, SSE,
object lock, presigned POST), web UI beyond docs/13 (user management, mutating admin ops
from the browser), metrics stack, encryption at rest, quotas,
versioning/recycle bin, xattrs, NLM locking, snapshots, hard links, NFSv4, SMB encryption,
3.1.1, oplocks, leases and security descriptors, Windows before 11 24H2, multiple
independent volumes, content-defined chunking, compression (hook left), tiering,
fsck-from-shards metadata reconstruction, WAL mirroring.
