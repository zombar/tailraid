# 17 — Stats: time series kept in the volume

`tailraid status` is a photograph. The soak report (09 §6) is the only film: a run's samples
charted as throughput, repair backlog, stored against logical bytes, the WAL depth, the
process, the machine and the data spread across disks, drawn in Go as inline SVG. This
document gives the cluster that film permanently: every node samples itself, the leader
samples the cluster, the rows are kept **inside the volume** as small immutable files so a
lost device loses no history, and the browser's **Stats** page (13 §1) draws the soak
report's charts over any range of it, live.

The user asked for a SQLite file in the volume. It is not a fit: SQLite needs page-level
random writes, locks and fsync on a real file, while a byte changed anywhere inside a
chunk re-reads, re-encodes and re-commits the whole 4 MiB chunk (04 §4) and the vfs has no
lock across gateways (07). What SQLite would have given, rollups and retention, this
design gives with files that are only ever written whole.

## 1. The series

Package `internal/stats`. Every node runs a **sampler** that every `stats.every` (10 s)
produces one **node row** from what is already cheap to read: `admin.Vitals` (12 §8), the
gateway snapshot (`GatewayFromDirty(...).WithFlusher(...).WithReader(...)`) and the process
stats. It never calls `admin.Build`: that walks every chunk under the FSM lock
(`ScanHealth`) and `ReadMemStats` stops the world, and once every 10 s forever on a
Raspberry Pi over Wi‑Fi would be a new steady load.

```json
{"t":1760184000,"n":"jockey","m":{"cpu":0.12,"load1":0.8,"rss":412000000,"mem_avail":2100000000,
 "mem_pressure":0.4,"disk_free":512000000000,"disk_w":1200000,"disk_r":80000,
 "net_rx":2400000,"net_tx":900000,"tailnet_rx":2100000,"tailnet_tx":880000,
 "heap":96000000,"sys":180000000,"goroutines":212,"dirty":0,"wal":0,"plan":0,"flush":0,
 "ra_fetched":3.2,"ra_joined":2.9,"prefetching":2,"bytes_r":1900000,"bytes_w":0}}
```

| Key | Unit | Kind | Source |
|---|---|---|---|
| `t` | Unix seconds | — | the sampler's wall clock, whole seconds |
| `n` | — | — | the member name |
| `cpu` | share of one core | rate | `machine.cpu_seconds` |
| `load1` | — | gauge | `machine.load1` |
| `rss` | bytes | gauge | `machine.rss` (Linux; the peak on macOS) |
| `mem_avail`, `mem_pressure` | bytes, percent | gauge | `machine.mem_available`, `machine.mem_pressure` |
| `disk_free` | bytes | gauge | `machine.disk_free` |
| `disk_w`, `disk_r` | bytes/s | rate | `machine.disk_written_bytes`, `machine.disk_read_bytes` |
| `net_rx`, `net_tx` | bytes/s | rate | `machine.net_rx_bytes`, `machine.net_tx_bytes` (12 §8, I-190) |
| `tailnet_rx`, `tailnet_tx` | bytes/s | rate | `machine.tailnet_rx_bytes`, `machine.tailnet_tx_bytes` |
| `heap`, `sys`, `goroutines` | bytes, bytes, count | gauge | `process.*` |
| `dirty`, `wal`, `plan`, `flush` | bytes | gauge | `gateway.dirty_bytes`, `wal_bytes`, `plan_bytes`, `flush_bytes` |
| `ra_fetched`, `ra_joined` | chunks/s | rate | `gateway.readahead_fetched`, `readahead_joined` |
| `prefetching` | count | gauge | `gateway.prefetching` |
| `bytes_r`, `bytes_w` | bytes/s | rate | `gateway.bytes_read`, `gateway.bytes_written`: every byte a client read or wrote through NFS, SMB or the web, new cumulative counters kept by the vfs |

A **rate** is the counter's difference against the previous sample divided by the seconds
between them, as the soak's `vitalsSample` computes it; the first sample after a start
carries no rate. A vital the OS does not expose is absent from the report (I-85) and
therefore absent from the row: nothing is written as zero.

**The leader alone** also produces a **cluster row** each tick: the FSM's volume counters
(O(1)), the pass under way (`repair.Progress`, 06 §4), the prober's view of every device
and the member table, plus the chunk walk's counters (degraded, pending, unreadable, stale,
the host and disk skew, the slots per target) from `ScanHealth`, which the leader's
sampler runs **at most once a minute** and reuses for the rows in between: one walk a
minute on one node, against the walk the dashboard's status cache makes every 2 s while a
browser polls, and none on a follower.

```json
{"t":1760184000,"term":12,"by":"jockey","c":{"degraded":37,"pending":37,"stale":0,"unreadable":0,
 "stored":60000000000,"logical":44000000000,"chunks":48211,"domain_skew":0,"disk_skew":0,
 "repair_moved":5200000,"live":{"jockey":1,"honker":0,"node0":2},
 "targets":[{"name":"jockey/1","host":"jockey","slots":15020,"cap":500000000000,"live":true,"active":true}]}}
```

`term` is the Raft term the writer held; `by` its name; `live` maps every device member to
0 (dead), 1 (live) or 2 (suspect, 05 §6); `repair_moved` is the running pass's bytes moved
as a rate; `targets` is what the disk-spread chart needs (09 §6). Two leaders can write
rows for one instant around an election; the reader keeps the row with the higher term
(§3), so a handover is one continuous series with no leader logic anywhere else.

## 2. Segments: the volume is the database

A node writes **only** under `/.tailraid/stats/<tier>/<name>/`, where `<name>` is its member
name, or `cluster/` for the leader's cluster rows. Every segment is **gzip-compressed JSON
lines** (`.jsonl.gz`, Go's `compress/gzip`): the keys repeat on every row, so a segment is
about eight times smaller than its rows, which keeps a raw hour under the small-chunk
threshold (`meta.SmallChunkMax`, 64 KiB: replicated, one Commit, no stripe) and the archive
of a five-node cluster near 150 MB stored over two years (§6).

A segment is written whole to a temp name beside its target, **flushed** (`FS.Flush`, a vfs
method over the flusher's `FlushInode`, so the Commit lands before the name is visible; the
web upload path renames before its flush, and another gateway reading the name at that
moment would see a zero-length file) and then renamed. **One goroutine per node owns every
stats write** (seal, rollup, catch-up, spill), so no two writers ever touch one path. The
writer's name is part of every file name, so two leaders, or a node renamed by a re-join,
never collide.

| Tier | Row interval | One file per | Name | Written |
|---|---|---|---|---|
| `10s` | `stats.every` | UTC hour | `2026-10-11T14-jockey.jsonl.gz`; a partial hour (a restart, a late start, a seal) is named by its first row's minute, `2026-10-11T1423-jockey.jsonl.gz`, and a second seal within one minute by its second, `2026-10-11T142315-jockey.jsonl.gz`; a reader takes the period from the name either way | at the hour boundary, on `serve` stop and on `tailraid stats seal`; never rewritten |
| `1m` | 60 s | UTC day | `2026-10-11-jockey.jsonl.gz` | in the first hour of the next day, from the day's sealed raw files **read back from the volume**, never from memory, so a restart loses nothing; never rewritten |
| `10m` | 600 s | UTC month | `2026-10-jockey.jsonl.gz` | **rebuilt daily**: the month file's rows plus the new day's rows derived from its raw files; so a year is twelve files per node, not 365 (a one-year page would otherwise open 1 825 one-chunk files at about 66 ms each) |

- A **rollup row** is a pure function of the raw rows of its interval: the mean of every
  rate and gauge, the minimum of `live`, the last `targets` of the interval, the highest
  `term`; its `t` is the interval's start; an interval with no raw rows produces no row (a
  gap stays a gap). The bytes are therefore identical on every node (I-193).
- **Catch-up.** On every hourly tick a node checks the last `keep.raw` days for a day whose
  `1m` file is missing while its raw files exist and rolls it up then. A node gone for good
  never rolls its last day up; that raw ages out and the day is a gap at the coarser tiers.
- **Retention** runs on the leader hourly (the `maintenance` tick of 06 §3): a segment whose
  period ended more than its tier's `stats.keep` ago is removed, whoever wrote it, so a
  removed member's files age out like everyone else's (I-194).
- **Ephemeral and hidden.** Every path under `/.tailraid/` is ephemeral (06 §1: a replace or
  delete unlinks, it never trashes; otherwise two days of raw would sit in the trash for
  thirty) and absent from the web file browser (13 §5, I-160); on the mounts the files are
  plain and an operator may read them with any tool (I-195).
- **Clock skew.** `t` is the writer's clock and a file is named from it. A reader opens the
  files whose names fall within an hour of the range and filters by row `t`, never trusting
  a name. A member whose clock is off plots off; nothing corrects it.
- **Shutdown.** The seal runs before `FlushAll` and before the leader hands over (12, I-123),
  with a 10 s deadline. When the cluster cannot take it (`EROFS`, no leader) the partial
  hour is spilled to `<data_dir>/stats/pending/` and written at the next start. A crash
  loses the unsealed hour.
- **Memory.** The sampler keeps the unsealed hour only (at most 360 rows); sealed hours are
  read back from the volume like any other range.

## 3. Query and the live edge

Two routes carry the series out:

- **`GET /v1/stats?since=<t>`** on the RPC port, a member route (08: cluster-ID and WhoIs
  member check as `/v1/status`): this node's unsealed rows with `t > since`, oldest first,
  at most 720 rows (far under the client's 4 MiB body cap), its cluster rows included while
  it leads. It never carries another member's rows or any credential (I-199).
- **`GET /api/stats?from=<unix>&to=<unix|now>&node=<name|all>`** on the web listener (13 §3), a
  read route behind the Host check and the member rule, no CSRF header. The handler:
  1. picks the finest tier whose retention covers `from` (`10s` within `keep.raw`, else
     `1m`, else `10m`), lists the tier's directories and reads the files named within an
     hour of the range through a **second `vfs.Reader` that never fills the cache**
     (`Reader.NoFill`: it consults the node's chunk cache, never adds to it and never
     prefetches), so the stats never push anything out of the 1 GiB chunk cache or start
     the readahead of 04 §5, while the segments the system pin keeps on every node (01
     §Local chunk cache, I-373) read from the local disk even when the network is busy; parsed files are cached in memory by `(path, inode version)` up to
     `stats.cache`, since a sealed file never changes and the month file's version changes
     with its daily rebuild;
  2. when `to` is now, asks every live member for its rows since the newest `t` **that
     member** reported (a per-member `since`, never the gateway's clock), in parallel with a
     2 s timeout; one fan-out serves every browser tab (the single-flight of 13 §7) and
     runs only while a browser asked within the last 10 s; a member that misses the timeout
     leaves a gap, never a zero or a carried-forward value, and is not asked again for 30 s;
  3. keeps the highest-term cluster row per `t`, deduplicates node rows by `(n, t)`,
     downsamples each series to at most `chart.Points` (760, the chart's width in its own
     units) by the mean per bucket, and renders the charts with `internal/chart` (§4);
  4. answers `{"range":{"from","to","tier","step"},"nodes":[…],"charts":[{"id","title","svg"}]}`
     (I-196).

   With `stream=1` (what the page asks for; measured 2026-10-13 on the real cluster: the
   whole reply was 0.6 MB for an hour and 1.9 MB for a day, 0.4–2.3 s before the page
   showed anything) the same reply comes as NDJSON, one JSON object a line, flushed as
   each is ready (I-258):
   - the **first line** is `{"range","nodes","charts":[{"id","title","frame"}]}`, written
     before any row is read or any member asked: the tier follows from the range and the
     configuration, the chart list from the node selection, and each `frame` is the
     chart's empty SVG from `internal/chart` (`class="chart pending"`, the chart's own
     `viewBox`, its title, a dashed `frame` rect and a `note` saying "loading");
   - then the rows are read and merged once, as in steps 1–3, and each chart is rendered
     and written as its own line `{"id","svg","caption"}` in the order of the first line;
   - a chart the range has no rows for is a line `{"id","empty":true}` (the page drops its
     frame, and asks for it again on the next refresh, so one that gets rows appears in its
     place); a failure to read the rows is a last line `{"error"}`, and the page then marks
     every frame still waiting failed with that reason; the reply's status is 200 once the
     first line went out.
   `ids=<id,…>` limits the charts rendered to those named (the first line still lists
   every chart), so a live refresh redraws only what is on screen.

No CLI reads the series: the rule stays `status --json` for the photograph and the page
for the film. The one command is `tailraid stats seal` (§7).

## 4. The charts and the page

The charts are the soak report's, drawn by the same code: `internal/chart` holds the line,
bars, lanes, strip and disk-spread helpers that `internal/soak/svg.go` and `svg_disks.go`
held. The one change in the move is that the helpers emit **classes instead of colours**
(series `s0`…`s7`, status, the spread bins, bands, text and grid; 13 §Design Charts): the
report's stylesheet maps them to the report's fixed colours and `app.css` maps them to the
page's tokens, so the same SVG bytes draw the report's picture in the report and a chart
that follows light and dark in the page (I-197). The page's other addition is the time
axis: the report prints seconds since the run started, the page prints clock times
(`HH:MM` within a day, `MM-DD HH:MM` beyond), an option of the shared helpers. Markers come
from the cluster rows: a leader change (`term`), a device going dead or live, a repair
pass starting and ending.

The page asks for the stream (§3): it draws every chart's frame from the first line at
once, each at the size its chart will have, and replaces a frame with its chart as that
chart's line arrives, so the page is laid out before any row is read and nothing moves
as the charts come in; the range pill says `loading` until the last line, and a chart
that could not be drawn keeps its frame with the reason (`class="chart pending failed"`,
the note in the bad colour). A live refresh, every 5 s, streams again with `ids=` the
charts at least partly on screen, and leaves the others as they were drawn (I-259). The
lookdev's first Stats section shows the page part-way in.

The page itself, its range picker, refresh rule, member dropdown and theme switch, is
13 §1 and 13 §Design (I-198, I-200). Its sections and charts:

| Section | Charts | Rows |
|---|---|---|
| Volume | stored vs logical bytes · chunks | cluster |
| Integrity | repair backlog (degraded, pending, stale, unreadable) · repair bytes moved · liveness lanes | cluster |
| Spread | data spread across disks (the lanes with the host-cap strip) · slots per node | cluster |
| *node* | client throughput · tailnet rx/tx · all interfaces rx/tx · CPU and load · memory · memory pressure · data disk free · disk read/write · WAL depth (dirty, WAL, plan, flush) · process (heap, sys) · readahead | the chosen member, or one series per member with `node=all` |

## 5. Configuration

```yaml
stats:
  every: 10s       # sample interval, 5s..1m
  keep:
    raw: 48h       # the 10 s tier; at least 26h, so a day is whole when it is rolled up
    minute: 720h   # the 1-min tier, 30 d; at least 48h
    coarse: 17520h # the 10-min tier, 2 y
  cache: 32MiB     # the gateway's parsed-segment cache
```

Every key is written only when set (12 §8, I-61), so a release from before them still
starts after a rollback; `Validate` refuses the ranges (I-194, 12 §7).

## 6. Storage and memory budget

A node row with the 24 metrics of §1 is about 0.55 KB as JSON and 70 B gzipped; a cluster
row with five devices and ten targets about 0.9 KB and 110 B.

| What | Per node | Cluster rows (the leader) | Five nodes, 3+2 |
|---|---|---|---|
| One raw hour file | 360 rows ≈ 25 KB gz, a replicated small chunk (3 copies) | ≈ 40 KB gz | — |
| `10s` tier, 48 h | 1.2 MB | 1.9 MB | ≈ 8 MB logical, ≈ 24 MB stored |
| `1m` tier, 30 d | 100 KB a day → 3 MB | 160 KB a day → 4.8 MB | ≈ 20 MB logical, ≈ 40 MB stored |
| `10m` tier, 2 y | 300 KB a month → 7.3 MB | 480 KB a month → 12 MB | ≈ 48 MB logical, ≈ 80 MB stored |
| **Archive** | **≈ 12 MB** | **≈ 19 MB** | **≈ 76 MB logical, ≈ 145 MB stored** |
| Files in the FSM | 48 + 30 + 24 ≈ 100 | ≈ 100 | ≈ 600 inodes at ~200 B: 0.12 MB of FSM memory |
| Commits a day | 24 seals + 1 rollup + 1 month rebuild = 26 | 26 | ≈ 160 small Commits; a seal is four Raft entries (Create, Commit, Rename, the temp's Unlink); retention's deletes are Unlinks whose tombstones GC reclaims after grace |
| Write traffic a day | 24 × 25 KB × 3 + 0.4 MB × 1.67 ≈ 2.5 MB | ≈ 4 MB | ≈ 17 MB for the whole cluster |
| Sampler RAM | 360 rows × ~250 B ≈ 90 KB | ≈ 0.5 MB | — |
| Gateway cache | parsed rows of the files a query touched, LRU, `stats.cache` 32 MiB (a 48 h `10s` query over five nodes parses ≈ 5.5 MB, a one-year query ≈ 10 MB) | — | per gateway that serves the page |
| CPU | one vitals probe and one gateway snapshot per 10 s, one gzip of 25 KB an hour, one rollup pass a day over ≈ 2 MB | a prober and member-table walk per 10 s | — |
| Chunk cache | none: the stats reader has no cache and no readahead | — | — |

Without gzip the archive would be about 640 MB stored and every raw hour a striped chunk;
the compression is part of the design, not an option. The soak phase (§7) measures the real
numbers and the report prints them next to this table's estimate.

## 7. Sealing, the soak phase and `verify`

`AdminOp` gains `stats_seal` next to `repair_now` and `gc_now` (12 §3): seal the partial
hour, roll up every day whose raw files exist without their rollup and, with `rollup`,
today's rows so far (a partial minute and coarse segment that the day's end rewrites
whole), and on the leader prune the archive as the hourly tick does. It is the shutdown
path's own function exposed as **`tailraid stats seal [--rollup]`**, so the soak needs no
test-mode switch, no period override and no fake clock on real nodes (09 §3).

Soak **phase 20 "stats"**, with `stats.every: 5s` on the rig (1 s on the in-process
harness), checks: (a) after `stats seal` every node's raw segment is under its own
directory, decodes, and is below `SmallChunkMax`, and the leader's cluster segment is
there; (b) `stats seal --rollup` writes today's `1m` and `10m` segments and the minute
rows equal the pure rollup of the raw rows the driver read back (I-193); (c) a segment
planted with a 2010 name is gone after the leader's seal; (d) the page's charts render for
a 6 h range from the rows read and the live edge every node answers over `/v1/stats` (the
same code `/api/stats` runs; the page itself is 13's manual checks); (e) a member killed
mid-phase answers no live edge and the leader's rows mark it dead, never a zero; (f)
`stored_bytes` grows by no more than §6's estimate for one sealed hour and the rollups per
node, printed measured against estimated. `verify` runs (a) to (d) and (f) on a live
cluster and (e) with `--destructive --restarts`; the three-VM rounds (vms.md) pick the
phase up unchanged.

## 8. Non-goals

Alerting, a query language, per-file or per-user series, exporting to Prometheus or
anything else, a CLI that prints the series, correcting a member's clock, keeping the
unsealed hour anywhere but memory and the shutdown spill, and sampling faster than 5 s.

## Invariants

- **I-191** — Every node samples its node series every `stats.every` from its vitals,
  gateway and process snapshots without walking the FSM; the leader alone samples the
  cluster series, from the volume counters, the pass under way, the prober, the member
  table and a chunk walk it runs at most once a minute, stamped with its Raft term; a
  cumulative counter is stored as a rate against the previous sample and the first sample
  after a start carries no rate; a vital absent from the report is absent from the row,
  never zero.
- **I-192** — A node writes only under `/.tailraid/stats/<tier>/<its name or cluster>/` with
  its name in every file name, from one goroutine; a segment is gzip-compressed JSON lines
  written whole to a temp name, flushed and then renamed; raw and minute segments are never
  rewritten, except that a forced rollup writes today's partial minute segment which the
  day's end rewrites whole, and the month segment is rebuilt daily to identical bytes for
  identical input; the raw tier seals at every UTC hour boundary, on `serve` stop and on
  `stats seal`, a partial hour named by its first row's minute, and a seal the cluster
  cannot take is spilled locally and written at the next start.
- **I-193** — The `1m` and `10m` rows of a UTC day are a pure function of its sealed raw
  rows read from the volume (the mean of rates and gauges, the minimum of liveness, the last
  targets and the highest term of the interval; no row for an empty interval), byte-identical
  on every node; the writer produces them in the first hour of the next day and catches up
  any day whose rollup is missing while its raw rows exist.
- **I-194** — The leader removes a segment whose period ended more than its tier's
  `stats.keep` ago, whoever wrote it, a removed member's included; `keep.raw` under 26 h,
  `keep.minute` under 48 h and `every` outside 5 s…1 min are refused by `Validate`.
- **I-196** — `/api/stats` answers from the finest tier whose retention covers `from`,
  reading the files named within an hour of the range and filtering by row time,
  downsampled to at most `chart.Points` per series, merged with each live member's rows
  since the newest instant that member reported; one fan-out serves every tab and runs only
  while a browser is asking; a member that does not answer within 2 s leaves a gap, never a
  zero or a carried-forward value, and is not asked again for 30 s; node rows are
  deduplicated by `(node, t)` and cluster rows keep the highest term per `t`; the parsed
  cache never exceeds `stats.cache` and the stats reader fills no chunk cache.
- **I-258** — With `stream=1`, `/api/stats` writes and flushes its first line (the range,
  the members and every chart's id, title and empty frame) before reading any row or
  asking any member, then one line per chart in that order, each the chart I-196 renders
  for the same range or, when the range has no rows for it, `empty`; a failure to read the
  rows is a last line carrying the error; `ids=` limits the charts rendered, never the
  first line; a chart's frame has its chart's own `viewBox` (the disk spread's estimated as
  one disk a member).
- **I-259** — The Stats page draws every frame of the first line before any chart arrives,
  replaces each frame with its chart as its line arrives, keeps a failed chart's frame
  with its reason, and on a live refresh asks only for the charts at least partly on
  screen.

## Tests

| Invariant | Tests |
|---|---|
| I-191 | `TestSampler_NodeRow_RatesAndAbsence`, `TestSampler_ClusterRow`, `TestService_Run_SealsAtHour`, `TestFS_ClientBytes_Counted` |
| I-192 | `TestRowCodec_RoundTrip`, `TestRowCodec_Empty_And_Garbage`, `FuzzRowCodec`, `TestSegmentName_RoundTrip_Table`, `TestWriter_SealsAtBoundary_NamesPartialHours`, `TestWriter_SpillAndRecover`, `TestService_Run_SealsAtHour`, `TestService_Seal_RollupAndPrune`, `TestStats_SealVisibleFromSecondGateway`, `TestAdminOp_StatsSeal`, `TestCLI_StatsSeal`, `TestSoak_AllPhasesAgainstHarness` (phase 20), `TestPhaseStats_SkipsWithoutSampler` |
| I-193 | `TestTier_PeriodOf`, `TestRollup_DeterministicAcrossNodes`, `TestRollup_Rules`, `TestWriter_RollupDay_MonthRebuild_CatchUp`, `TestService_Seal_RollupAndPrune`, `TestSoak_AllPhasesAgainstHarness` (phase 20) |
| I-194 | `TestSegment_Expired_Table`, `TestWriter_Retain_AnyWriter_Table`, `TestStats_Retention_RemovedMemberSegment`, `TestConfig_StatsKeys_Table`, `TestService_Seal_RollupAndPrune`, `TestSoak_AllPhasesAgainstHarness` (phase 20) |
| I-258 | `TestChart_Frame`, `TestCatalogue_MatchesCharts`, `TestWebUI_Stats_Stream_FramesFirst` |
| I-259 | `TestStatic_StatsStreamsFramesFirst` |
| I-196 | `TestStatsQuery_TierChoice_Table`, `TestStatsQuery_NameWindowAndRowFilter`, `TestStatsQuery_CacheBound_Table`, `TestStatsQuery_FanOut_TimeoutIsGap_BackOff`, `TestStatsQuery_Dedup_TermRule_Seeded`, `TestStatsQuery_Downsample_Points_Table`, `TestStatsQuery_Charts`, `TestWebUI_Stats_Route`, `TestStats_SealVisibleFromSecondGateway` (the cache-less reader) |

The stubs live in `test/invariants/` until the PR that lands each feature replaces them with the tests named after them (09 §2).
