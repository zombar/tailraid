# Changelog

Generated from the commit log by `scripts/changelog.sh`; do not edit by hand.

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

