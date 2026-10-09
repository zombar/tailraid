# Changelog

Generated from the commit log by `scripts/changelog.sh`; do not edit by hand.

## v0.4.1 (2026-10-09)

### Changes

- node: keep the member seed current while serving and snapshot after the join: a fresh follower restarts with an empty table ([8191d4a](https://github.com/zombar/tailraid/commit/8191d4a))
- cluster: a joiner keeps the leader's seed for serve, and a reset device re-joins from its old address ([0c92d84](https://github.com/zombar/tailraid/commit/0c92d84))

## v0.4.0 (2026-10-09)

### Changes

- cli: join resolves a device name through the tailnet, and only serve opens log.file ([0e611b5](https://github.com/zombar/tailraid/commit/0e611b5))
- cli: volume grow <disk> <size>|auto pins a disk's budget in the config and restarts the service ([2905a51](https://github.com/zombar/tailraid/commit/2905a51))

## v0.3.2 (2026-10-09)

### Changes

- testharness: hold a node's RPC port from allocation to its first start ([473566d](https://github.com/zombar/tailraid/commit/473566d))
- config: an off gateway listener is "" and takes no port; SMB's port joins the distinct-port check ([80e8709](https://github.com/zombar/tailraid/commit/80e8709))
- config, node, mount, cli: Windows mounts the SMB gateway; the portmap stub goes ([07c73da](https://github.com/zombar/tailraid/commit/07c73da))
- smbgw: SMB2 gateway over the vfs, with an in-tree copy of go-filesystems/smb ([ad9d9f0](https://github.com/zombar/tailraid/commit/ad9d9f0))
- vfs: File.WriteAt; the file info exposes Ino and Times ([1a7fb16](https://github.com/zombar/tailraid/commit/1a7fb16))
- soak: a diskless voter in the compose rig, join registers config disks, phase 14 (docs/14 §9) ([b97a79d](https://github.com/zombar/tailraid/commit/b97a79d))
- webui: a host's targets are its disks; a diskless host is a card with a note (docs/14 §9) ([8b84171](https://github.com/zombar/tailraid/commit/8b84171))
- node: devices are never placement targets; diskless hosts; serve refuses legacy state (docs/14 §9) ([4b8ec89](https://github.com/zombar/tailraid/commit/4b8ec89))
- update: minisign-signed releases with rollback and a downgrade guard; snapshot export and offline recovery ([3674559](https://github.com/zombar/tailraid/commit/3674559))
- ops: confined units, leadership hand-over on shutdown, rotating logs, readiness, config validation, web UI host check ([0eae3e0](https://github.com/zombar/tailraid/commit/0eae3e0))
- soak: a timed-out phase's late return is not recorded into the next phase ([f3cdc9d](https://github.com/zombar/tailraid/commit/f3cdc9d))
- soak: phases survive panics and timeouts, restore what they changed, --destructive gates mutations, skipped checks and aborted runs render ([cd0c557](https://github.com/zombar/tailraid/commit/cd0c557))
- cluster: a closing leader drains committed entries into the FSM before Raft stops, so no apply waiter is left behind ([41ded3e](https://github.com/zombar/tailraid/commit/41ded3e))
- cluster: identity files sync their directory through the sysfs seam, so Windows can write them ([ce469f9](https://github.com/zombar/tailraid/commit/ce469f9))
- lint: gocritic, gofumpt and an unused helper in the review's new code ([8edddf8](https://github.com/zombar/tailraid/commit/8edddf8))
- meta: SetPlacement names the placement it planned from, GCDelete runs in batches, snapshots stream, undecodable entries are loud ([c7a5ef9](https://github.com/zombar/tailraid/commit/c7a5ef9))
- meta: off-by-one boundaries, half-wired features, one implementation per helper, dead code ([8233dbe](https://github.com/zombar/tailraid/commit/8233dbe))
- chunk: sweep re-checks age at delete time, WAL survives fsync and reopen failures, scrub fetches once, NFS closes its connections ([dbcb7b7](https://github.com/zombar/tailraid/commit/dbcb7b7))
- cluster: atomic identity files, startup failures free the port, body deadlines, cancelled hedges, cache races ([3335816](https://github.com/zombar/tailraid/commit/3335816))
- vfs: a proposal with an unknown outcome is resolved per command, never retried blindly ([97e9f54](https://github.com/zombar/tailraid/commit/97e9f54))
- cluster: the Raft transport accepts members only; update_addr and join are bound to the caller ([5e5c8a1](https://github.com/zombar/tailraid/commit/5e5c8a1))
- lint: fix what the new linters found ([3d77ee0](https://github.com/zombar/tailraid/commit/3d77ee0))
- cleanup: drop dead code found by deadcode and unparam ([5b10899](https://github.com/zombar/tailraid/commit/5b10899))
- build: Go 1.27.2, bbolt 1.5.0, vendored dependencies, tools via go run, CI pinned to SHAs ([feb722d](https://github.com/zombar/tailraid/commit/feb722d))

### Documentation

- docs: Windows checklist for the SMB mount; size table through the SMB gateway ([5604d26](https://github.com/zombar/tailraid/commit/5604d26))
- docs: SMB gateway for Windows (docs/15); NFS stays on macOS and Linux ([c6a4266](https://github.com/zombar/tailraid/commit/c6a4266))
- docs: devices are never placement targets; diskless hosts (docs/14 §9) ([3120186](https://github.com/zombar/tailraid/commit/3120186))

### Tooling

- test: the model checker never adds a disk to the host it is draining ([dfdc0f1](https://github.com/zombar/tailraid/commit/dfdc0f1))
- ci: tidy the tool module files; the Windows and scheduling failures of the review's new tests ([b110e49](https://github.com/zombar/tailraid/commit/b110e49))
- test: the review's new tests on a disks-only tree ([d180697](https://github.com/zombar/tailraid/commit/d180697))
- tests: the review checklist across the suite, size tables, new scenarios, fuzz targets, no unconditional skips ([f5b6355](https://github.com/zombar/tailraid/commit/f5b6355))
- ci: quote the release job's tag variable (shellcheck SC2086 through actionlint on the runner) ([f2aa388](https://github.com/zombar/tailraid/commit/f2aa388))
- ci: lint-cross covers GOOS=linux too; the Linux vitals reads are marked for gosec ([b621339](https://github.com/zombar/tailraid/commit/b621339))
- ci: hooks call a lefthook built to dist/tools; the Windows service switch has a default ([38b8a4e](https://github.com/zombar/tailraid/commit/38b8a4e))
- ci: linters and hooks for Go, TypeScript, CSS, the docs, YAML and the workflow ([7a2abf6](https://github.com/zombar/tailraid/commit/7a2abf6))
- ci: build the linter for the host before linting under GOOS=darwin and GOOS=windows ([0c0a36d](https://github.com/zombar/tailraid/commit/0c0a36d))

## v0.3.1 (2026-10-09)

### Changes

- webui: one line per disk row — dot, name and warning together, the data-dir tag under the name ([98754e0](https://github.com/zombar/tailraid/commit/98754e0))

## v0.3.0 (2026-10-09)

### Changes

- build: make tag names the changelog branch after the commit and stops on any failed step ([5568caf](https://github.com/zombar/tailraid/commit/5568caf))
- soak: judge the data-slot spread only where it can be judged ([06660f7](https://github.com/zombar/tailraid/commit/06660f7))
- admin, node: gofmt and range by index — make check green ([a7196e2](https://github.com/zombar/tailraid/commit/a7196e2))
- admin, cli, webui: the data-dir store labelled, targets on one physical disk flagged ([01648df](https://github.com/zombar/tailraid/commit/01648df))
- lookdev: the data-dir store labelled, targets on one physical disk flagged ([a717ce5](https://github.com/zombar/tailraid/commit/a717ce5))
- node: a pinned budget reaches the cluster when it changes — growing a device in place ([50cf821](https://github.com/zombar/tailraid/commit/50cf821))
- node: open_disk waits for the disk's row instead of leaving it closed for a tick ([425e61f](https://github.com/zombar/tailraid/commit/425e61f))
- soak: phase 13 adds, drains and removes a disk in auto mode; the policy follows each step ([162fa8f](https://github.com/zombar/tailraid/commit/162fa8f))
- build: make install renames into place — overwriting a binary in place gets it killed on macOS ([7c2ca65](https://github.com/zombar/tailraid/commit/7c2ca65))
- tailscale: give the CLI a TERM — the macOS app's binary needs one under launchd ([6f95ea3](https://github.com/zombar/tailraid/commit/6f95ea3))
- build: ignore the single-host soak's report and results ([7baf28b](https://github.com/zombar/tailraid/commit/7baf28b))
- soak: the single-host rig runs clean — no drain without a second node; data-slot spread capped per chunk ([43740a4](https://github.com/zombar/tailraid/commit/43740a4))
- cluster: the leader names added disks, so two quick add-disks never share a name ([cd3071b](https://github.com/zombar/tailraid/commit/cd3071b))
- soak: phase 13 disks, a 4 hosts × 3 targets rig and a single-host one, the disk-spread chart ([2015650](https://github.com/zombar/tailraid/commit/2015650))
- meta: a disk is never more active than its host — draining a host drains its disks ([08bc675](https://github.com/zombar/tailraid/commit/08bc675))
- webui: the Disks block — spread summary and a row per target; balance totals each host's disks ([5377f8f](https://github.com/zombar/tailraid/commit/5377f8f))
- cli, admin: node add-disk, init takes configured disks, status shows disks, tolerance and host skew ([6fca256](https://github.com/zombar/tailraid/commit/6fca256))
- repair: host-cap skew moves; harness disks; the model checker adds and fails disks ([0c4d2b7](https://github.com/zombar/tailraid/commit/0c4d2b7))
- node, rpc, cluster: a store per disk — marker, per-target budget, disk RPC path, disk liveness ([eb4005c](https://github.com/zombar/tailraid/commit/eb4005c))
- meta: disk targets, domain-aware placement, host-aware policy ([3656c52](https://github.com/zombar/tailraid/commit/3656c52))
- lookdev: spread groups packed from the left, fixed-width columns ([5b214ee](https://github.com/zombar/tailraid/commit/5b214ee))
- lookdev: a spread summary on top of the Disks block ([27caf09](https://github.com/zombar/tailraid/commit/27caf09))
- lookdev: disks as rows inside each host card ([04d8881](https://github.com/zombar/tailraid/commit/04d8881))
- lookdev: the Disks block and the soak report's disk-spread chart ([10ecf1e](https://github.com/zombar/tailraid/commit/10ecf1e))

### Documentation

- docs: growing a device or disk in place, in the README and volume grow ([0a2f836](https://github.com/zombar/tailraid/commit/0a2f836))
- docs: a shorter README with how to grow and shrink the volume, add disks, and change voters ([4757d76](https://github.com/zombar/tailraid/commit/4757d76))
- docs: several disks per device — disk targets, host cap, host-aware policy, domain skew (docs/14) ([48097be](https://github.com/zombar/tailraid/commit/48097be))

## v0.2.1 (2026-10-09)

### Changes

- vfs: the two PUT-deadline tests stop advancing the clock once the straggler is cancelled ([f20dbd3](https://github.com/zombar/tailraid/commit/f20dbd3))
- build: the format check and the invariant script enumerate files through ls-files, skipping nested worktrees ([4d8a22c](https://github.com/zombar/tailraid/commit/4d8a22c))
- gitignore: the .claude directory (linked worktrees), so make tag sees a clean tree ([ef7d16e](https://github.com/zombar/tailraid/commit/ef7d16e))
- vfs: the total-dirty trigger test waits for the entries to leave the overlay ([d6d8db4](https://github.com/zombar/tailraid/commit/d6d8db4))
- soak: legends below the charts; rates carried across intervals too short to divide by ([6f681c9](https://github.com/zombar/tailraid/commit/6f681c9))
- admin, soak: machine vitals in status and the report; where the large file's chunks landed ([ef4d758](https://github.com/zombar/tailraid/commit/ef4d758))
- soak: phase 12 counts orphans before and after, and judges the flusher's own peak ([3e8f241](https://github.com/zombar/tailraid/commit/3e8f241))
- admin, soak: process memory and flush bounds in status; phase 12 writes a file many times the WAL cap ([33ebd8e](https://github.com/zombar/tailraid/commit/33ebd8e))
- webui: atomic, resumable uploads — a temp beside the target, parts with Content-Range, limits ([0796ca5](https://github.com/zombar/tailraid/commit/0796ca5))
- rpc, node: shard call deadlines scale with size; server slots within a byte budget ([0c2607f](https://github.com/zombar/tailraid/commit/0c2607f))
- vfs, chunk: streamed compaction; the WAL survives a failed step after the rename; cache temps swept ([c43657f](https://github.com/zombar/tailraid/commit/c43657f))
- vfs: a WRITE at a cap waits for the flusher; ENOSPC when the cluster is full ([ef6a7c7](https://github.com/zombar/tailraid/commit/ef6a7c7))
- vfs: chunk pipeline — bounded flush memory, batch commits, cancelled stragglers ([5c12686](https://github.com/zombar/tailraid/commit/5c12686))
- soak: phase 11 automation — budgets, voter-first placement and the automatic policy in the report ([936f718](https://github.com/zombar/tailraid/commit/936f718))
- cmd, admin, webui: set-redundancy --auto; the mode in describe, status and the dashboard; CLI tests ([6d61b51](https://github.com/zombar/tailraid/commit/6d61b51))
- meta, repair: automatic redundancy — the leader picks the policy from the member table ([96a6291](https://github.com/zombar/tailraid/commit/96a6291))
- node, config, cmd: the budget is derived from the data disk; init and join take no --capacity ([c5d4e14](https://github.com/zombar/tailraid/commit/c5d4e14))
- meta: Volume.Auto, the SetVolumeMode op, snapshot codec v2 that still reads v1 ([9aa223f](https://github.com/zombar/tailraid/commit/9aa223f))
- sysfs, chunk: free-space probe, the derived budget and a store cap that follows it ([1a3a80d](https://github.com/zombar/tailraid/commit/1a3a80d))
- meta: voter-first placement — data slots on always-on devices ([b95e34e](https://github.com/zombar/tailraid/commit/b95e34e))
- webui: the TypeScript app — shell, dashboard (balance, integrity), file browser, player, chunk panel ([028479b](https://github.com/zombar/tailraid/commit/028479b))
- build: esbuild as a Go tool; make web and web-check; the bundle is committed and embedded ([1bcec14](https://github.com/zombar/tailraid/commit/1bcec14))
- node, cmd: web UI listener on <tsIP>:listen.web, own login via WhoIs, web_url in status ([c3723f6](https://github.com/zombar/tailraid/commit/c3723f6))
- webui: the handler — member-or-same-user auth, CSRF header, paths, ls/stat, Range reads, uploads, mkdir, status cache, chunk paging, static shell ([6363dbe](https://github.com/zombar/tailraid/commit/6363dbe))
- admin, vfs, config: chunk report paging, dirty file info, WAL sentinels, listen.web ([c0f07de](https://github.com/zombar/tailraid/commit/c0f07de))
- tailscale, rpc: WhoIs login behind the identity cache; SourceAddr and ServeHTTP exported ([a11dbc5](https://github.com/zombar/tailraid/commit/a11dbc5))

### Documentation

- docs: measured size through streaming large files ([b3e6ccb](https://github.com/zombar/tailraid/commit/b3e6ccb))
- docs: streaming large files — memory model, cap backpressure, atomic resumable uploads ([dbd8c6b](https://github.com/zombar/tailraid/commit/dbd8c6b))
- docs: measured size through init-without-flags ([be2dd7f](https://github.com/zombar/tailraid/commit/be2dd7f))
- docs: init without flags — voter-first placement, derived capacity, automatic redundancy ([66505d1](https://github.com/zombar/tailraid/commit/66505d1))
- docs: measured size through the web UI; manual checks for the real tailnet ([93c4a10](https://github.com/zombar/tailraid/commit/93c4a10))
- docs: web UI design (docs/13), style guide and lookdev, invariants I-68..I-73 ([3ba0313](https://github.com/zombar/tailraid/commit/3ba0313))
- docs: README explains how to cut a release ([535fdab](https://github.com/zombar/tailraid/commit/535fdab))

### Tooling

- test: a crash at any point leaves nothing behind ([c2d3ade](https://github.com/zombar/tailraid/commit/c2d3ade))

## v0.2.0 (2026-10-08)

### Changes

- testharness: a released PUT stall waits for the stalled PUT to finish ([295990e](https://github.com/zombar/tailraid/commit/295990e))
- soak: compose node indices come from the cluster, not the container number ([abda021](https://github.com/zombar/tailraid/commit/abda021))
- dev, soak: goroutine dumps from a hung node; corruption phase reports an absent slot ([f8da445](https://github.com/zombar/tailraid/commit/f8da445))
- meta: ForEachNode runs its callback with the FSM lock released ([2c487ca](https://github.com/zombar/tailraid/commit/2c487ca))
- soak: the worker survives a gateway operation that outruns the NFS client's timeout ([49b0238](https://github.com/zombar/tailraid/commit/49b0238))
- vfs: a PUT deadline that passes before the k-th ack still ends the wait; stragglers re-placed ([5e0b7eb](https://github.com/zombar/tailraid/commit/5e0b7eb))
- meta, vfs, repair, chunk, rpc, admin: one copy of each shared helper ([ee3a8c9](https://github.com/zombar/tailraid/commit/ee3a8c9))
- meta, cluster, rpc: indexed node lookups, one WhoIs per miss, context-aware shard slots ([9a092f7](https://github.com/zombar/tailraid/commit/9a092f7))
- vfs, rpc, chunk, cluster, meta: hedged shard fetch, one-allocation bodies, smaller nits ([d26aa92](https://github.com/zombar/tailraid/commit/d26aa92))
- meta, cluster, nfsgw: commit wait without a goroutine per waiter; READDIR pages reuse one listing ([6bd7f17](https://github.com/zombar/tailraid/commit/6bd7f17))
- vfs: extent insert and lookups by binary search ([3d1b55d](https://github.com/zombar/tailraid/commit/3d1b55d))
- meta: volume stats kept per apply, one usage formula, chunk walk sorts once ([412fab5](https://github.com/zombar/tailraid/commit/412fab5))
- chunk, rpc: one hash per hop into the shard store, per-key locks, prefix dirs made once ([9e670b2](https://github.com/zombar/tailraid/commit/9e670b2))
- chunk, vfs: in-memory cache tier, throttled mtime refresh, one fetch per chunk ([f07df85](https://github.com/zombar/tailraid/commit/f07df85))
- meta: Commit refs only for changed indices; inode copies carry no Children map ([a6d5526](https://github.com/zombar/tailraid/commit/a6d5526))
- vfs: no RMW fetch for covered chunks, base fetched outside the inode lock, parallel flush ([24210a5](https://github.com/zombar/tailraid/commit/24210a5))
- bench: hot-path benchmarks and the pre-optimisation baseline ([0080bfc](https://github.com/zombar/tailraid/commit/0080bfc))
- soak: report carries seed, policy, versions, citations, percentiles, totals and liveness ([86842d4](https://github.com/zombar/tailraid/commit/86842d4))
- soak: partition phase; live kills held down with drop rules ([510b36b](https://github.com/zombar/tailraid/commit/510b36b))
- soak: records under the lock, one Admin surface, honest corruption and storage checks ([085a421](https://github.com/zombar/tailraid/commit/085a421))
- nfsgw, cluster: directory paging under change; the 1+0 default policy ([6f0d8fd](https://github.com/zombar/tailraid/commit/6f0d8fd))
- meta, repair, cluster: refcounts per index, stale readers, PUT failures, resumed migration ([cc929cd](https://github.com/zombar/tailraid/commit/cc929cd))
- chunk, repair: atomic capacity reservation; repair output and sweep gates pinned ([0f41bd6](https://github.com/zombar/tailraid/commit/0f41bd6))
- vfs, cluster: flusher trigger, boundary and crash edge cases ([eed4320](https://github.com/zombar/tailraid/commit/eed4320))
- vfs: edge cases around opens, renames, empty files and WAL limits ([72a6b99](https://github.com/zombar/tailraid/commit/72a6b99))
- rpc: constant-time join token compare, throttle after a failed join, token rotate ([095c56f](https://github.com/zombar/tailraid/commit/095c56f))
- cluster: no skip-fsync switch in production options; tests inject Raft stores ([2c966ab](https://github.com/zombar/tailraid/commit/2c966ab))
- nfsgw: pin the wire statuses of a stale gateway; SETATTR errors get a status ([14cc038](https://github.com/zombar/tailraid/commit/14cc038))
- vfs: metadata mutations retry until the gateway is stale, then EROFS ([ad3527a](https://github.com/zombar/tailraid/commit/ad3527a))
- cmd: add the documented `tailraid reset` ([1558f99](https://github.com/zombar/tailraid/commit/1558f99))
- node: check --scrub runs through the repairer; fsck tested; verify and shutdown fixes ([6800c45](https://github.com/zombar/tailraid/commit/6800c45))
- cluster: the FSM enforces node removal; --force zeroes slots; status marks lost nodes ([eb88d4b](https://github.com/zombar/tailraid/commit/eb88d4b))
- meta: bound directory entry names and the chunk size in one place each ([6266c60](https://github.com/zombar/tailraid/commit/6266c60))
- meta: one ChunkState derivation shared by status, check, describe and the repairer ([8449bf4](https://github.com/zombar/tailraid/commit/8449bf4))
- vfs: the reader never dials a holder the prober reports dead ([d18eb42](https://github.com/zombar/tailraid/commit/d18eb42))
- vfs: enforce the gateway-wide dirty cap; flush an inode in full at its WAL cap ([7e79b2a](https://github.com/zombar/tailraid/commit/7e79b2a))
- repair: measure GC grace on the FSM clock, as documented ([7b3a18a](https://github.com/zombar/tailraid/commit/7b3a18a))
- repair: never lower a chunk's readable slots; propose nothing when nothing is placeable ([c732c3f](https://github.com/zombar/tailraid/commit/c732c3f))
- vfs: a dedup hit checks shard presence, not just holder liveness ([859c2a8](https://github.com/zombar/tailraid/commit/859c2a8))
- meta, vfs: bound file size so a Commit can never allocate per a hostile NewSize ([986688a](https://github.com/zombar/tailraid/commit/986688a))
- meta: a Commit without HasTruncate never shrinks a file ([25c682c](https://github.com/zombar/tailraid/commit/25c682c))
- vfs: fsync the staging directory when a WAL is created; cut back un-synced records ([4a09cab](https://github.com/zombar/tailraid/commit/4a09cab))
- vfs: keep a raised size across WAL compaction; hide plan bytes cut by a truncate ([5d551a8](https://github.com/zombar/tailraid/commit/5d551a8))
- vfs: a partial flush keeps every dirty byte outside the committed chunks ([e3dd883](https://github.com/zombar/tailraid/commit/e3dd883))
- vfs: run the WAL on the sysfs.FS seam ([bd48257](https://github.com/zombar/tailraid/commit/bd48257))
- changelog: only skip the tag auto-commits; a tag with no other commits still gets a body ([c97777f](https://github.com/zombar/tailraid/commit/c97777f))

### Documentation

- docs: benchmark run after the hot-path work ([6a74a35](https://github.com/zombar/tailraid/commit/6a74a35))
- docs: every test the docs name exists, and the script enforces it; layout and size drift ([e5b5fe4](https://github.com/zombar/tailraid/commit/e5b5fe4))

### Tooling

- ci: make tag opens the changelog as a pull request, since main only takes pull requests ([69c6dde](https://github.com/zombar/tailraid/commit/69c6dde))
- test: cite the real tests behind 32 leftover stubs and delete them ([8fa2fc6](https://github.com/zombar/tailraid/commit/8fa2fc6))
- test: one -seed helper, goleak in every goroutine-starting package, placement golden vectors ([2aef634](https://github.com/zombar/tailraid/commit/2aef634))

## v0.1.2 (2026-10-08)

### Changes

- changelog: generated from the commit log; make tag commits it and CI uses it as release notes ([d714a37](https://github.com/zombar/tailraid/commit/d714a37))

## v0.1.1 (2026-10-08)

### Changes

- update: fetch private release assets through the API; fsm: dedupe only real IDs ([4a6fdca](https://github.com/zombar/tailraid/commit/4a6fdca))
- Fix the CI timing flakes at the root: NoSync raft log in the harness, idempotent join ([eb1bcb1](https://github.com/zombar/tailraid/commit/eb1bcb1))

## v0.1.0 (2026-10-08)

### Changes

- update test: Windows keeps the .old image ([b494a04](https://github.com/zombar/tailraid/commit/b494a04))
- serve: check the log closer error (errcheck) ([d702f68](https://github.com/zombar/tailraid/commit/d702f68))
- release process and self-update: make tag, tailraid update, docs/12 §4–5 ([bc14e0a](https://github.com/zombar/tailraid/commit/bc14e0a))
- install: refuse go-run binaries ([f301d88](https://github.com/zombar/tailraid/commit/f301d88))
- M4: service install, log file, Windows portmap stub, /dev/fault, verify --restarts ([0cde8ae](https://github.com/zombar/tailraid/commit/0cde8ae))
- report: keep the wordmark as one flex item ([d2008a7](https://github.com/zombar/tailraid/commit/d2008a7))
- brand: logo mark and combination logos; README header and status; mark in the soak report ([8e4fd71](https://github.com/zombar/tailraid/commit/8e4fd71))
- soak: follow leader redirects, read placement from the leader, nil-safe LeaderFromErr ([f302bdb](https://github.com/zombar/tailraid/commit/f302bdb))
- M3b: soak suite — scenario engine, compose driver with worker sidecars, HTML report, tailraid verify ([060f7dd](https://github.com/zombar/tailraid/commit/060f7dd))
- model checker: mid-run policy changes and node drains ([ff8863d](https://github.com/zombar/tailraid/commit/ff8863d))
- cluster: WaitApplied returns ErrClosed when the node shuts down ([d5310d1](https://github.com/zombar/tailraid/commit/d5310d1))
- M3: chunk describe with the dev-mode event ring; mount test without a tailscale CLI ([66f66b5](https://github.com/zombar/tailraid/commit/66f66b5))
- M3: model checker, stale-overlay fix (L-178), unknown-outcome modelling ([09882b5](https://github.com/zombar/tailraid/commit/09882b5))
- M3: repair, GC, sweep, maintenance CLI, crash and durability tests ([aed57e1](https://github.com/zombar/tailraid/commit/aed57e1))
- soak-client: wait with time.After (lint) ([b00248d](https://github.com/zombar/tailraid/commit/b00248d))
- soak-client: preallocate the manifest slice (lint) ([f40bbcb](https://github.com/zombar/tailraid/commit/f40bbcb))
- Compose cluster: three nodes with fake tailnet identities, Go NFS client workload ([54b5b3d](https://github.com/zombar/tailraid/commit/54b5b3d))
- M2: node wiring, admin reports and management CLI ([3c61d83](https://github.com/zombar/tailraid/commit/3c61d83))
- M2: cluster membership, forwarding, liveness, multi-node harness and scenarios ([772748d](https://github.com/zombar/tailraid/commit/772748d))
- M2: node RPC over the tailnet, test-first ([1af5e77](https://github.com/zombar/tailraid/commit/1af5e77))
- M2: tailscale client via the CLI, with headscale fixtures and a Fake ([95f4cfb](https://github.com/zombar/tailraid/commit/95f4cfb))
- cluster: readiness waits for the FSM to apply the last command entry ([8763cfc](https://github.com/zombar/tailraid/commit/8763cfc))
- Windows: release file handles before remove/rename; test fixes from CI ([dee9107](https://github.com/zombar/tailraid/commit/dee9107))
- check-invariants: force LC_ALL=C so generated tables sort identically on macOS and Linux ([ce7fb83](https://github.com/zombar/tailraid/commit/ce7fb83))
- Module path: github.com/zombar/tailraid ([9411ed9](https://github.com/zombar/tailraid/commit/9411ed9))
- M1 validated: real macOS NFS mount workload passes ([c367004](https://github.com/zombar/tailraid/commit/c367004))
- cluster: WaitReady waits for log replay before serve reads the FSM ([6a2a1d6](https://github.com/zombar/tailraid/commit/6a2a1d6))
- M1: single-node serve, config, mount helpers, CLI, real-mount test ([565cad7](https://github.com/zombar/tailraid/commit/565cad7))
- M1: NFSv3 gateway handler over the billy adapter, test-first ([3aa3396](https://github.com/zombar/tailraid/commit/3aa3396))
- Test harness: promote the vfs fabric to internal/testharness ([baf17f4](https://github.com/zombar/tailraid/commit/baf17f4))
- M1: billy filesystem adapter over overlay, FSM and reader ([0e8edaf](https://github.com/zombar/tailraid/commit/0e8edaf))
- M1: vfs Reader and Flusher, consumer interfaces, test-first ([5d81e05](https://github.com/zombar/tailraid/commit/5d81e05))
- M1: vfs overlay, per-inode WAL, dirty extents, test-first ([e0aa939](https://github.com/zombar/tailraid/commit/e0aa939))
- M1: meta FSM apply rules, snapshot/restore, read API, invariant checker ([757f79f](https://github.com/zombar/tailraid/commit/757f79f))
- Docs: apply whitepaper review decisions ([65aac41](https://github.com/zombar/tailraid/commit/65aac41))
- M1: clock, sysfs seam, shard store, meta types/codec/placement, test-first ([2852808](https://github.com/zombar/tailraid/commit/2852808))
- Fix prune-stubs block matching; prune I-10/I-30 stubs ([37587d1](https://github.com/zombar/tailraid/commit/37587d1))
- M1: internal/ec Reed-Solomon codec, test-first ([6ce9a8b](https://github.com/zombar/tailraid/commit/6ce9a8b))
- Add M1 library dependencies (reedsolomon, raft, go-billy, go-nfs) ([002cee6](https://github.com/zombar/tailraid/commit/002cee6))
- M0: design docs, invariants and lifecycle matrix, tooling skeleton ([e047f67](https://github.com/zombar/tailraid/commit/e047f67))

### Documentation

- README: product voice, first use and configuration, latest report linked; docs carry the detail ([a7c2b81](https://github.com/zombar/tailraid/commit/a7c2b81))
- README in the style of errol: pitch, status, platform note, install, quickstart, write path, command tour, config, soak, Windows, hacking; make install ([0ca2070](https://github.com/zombar/tailraid/commit/0ca2070))
- docs: M5 deferred; measured code size; status lines ([ad9dccb](https://github.com/zombar/tailraid/commit/ad9dccb))
- docs: M3b done in the status line ([3b1af17](https://github.com/zombar/tailraid/commit/3b1af17))

### Tooling

- ci: Windows leg skips the harness-based packages ([937207f](https://github.com/zombar/tailraid/commit/937207f))
- ci: Windows tests without the race detector; soak waits for the first election ([e8f6b19](https://github.com/zombar/tailraid/commit/e8f6b19))
- ci: Windows-neutral tests for svc and update, log file closer, tidy tools.mod ([54febf1](https://github.com/zombar/tailraid/commit/54febf1))
- tests: trim the short suite (crash test on one cluster, Fast raft in cluster tests, smaller oracle runs) ([1f61f25](https://github.com/zombar/tailraid/commit/1f61f25))
- Tidy tools.mod: -modfile tidies against the repo's packages, so it carries the project deps too ([85bd8d9](https://github.com/zombar/tailraid/commit/85bd8d9))

