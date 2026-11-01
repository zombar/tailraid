# 14 — Disks and failure domains

A device may give the volume more than one disk. Each disk is its own **placement target**
with its own shard store, budget, liveness and state, and the device that serves it is its
**host**, the failure domain. A single machine with N disks is then a real `(k, m)` volume
that survives the loss of any `m` disks; a tailnet of several machines keeps spreading
every stripe across machines first, so it still survives the loss of a whole machine when
the policy allows.

## 1. Model

```go
const KindDisk NodeKind = 3

type Node struct {
    ...
    Host NodeIdx // KindDisk: the device row that serves this disk; 0 for devices and S3 nodes
}
```

A disk is a `Node` row of `Kind == KindDisk`, like an S3 node a placement target that is
not a Raft member: it has its own `Idx` (never reused), `Name` (`<host>/<n>`, n from 1),
`ID` (16 random hex chars, the disk ID), `Capacity`, `State` and `JoinedAt`; `Voter` is
false and `Addr`, `RaftPort`, `RPCPort` are empty. Because it is a row, drain, remove, the
remove guard (`ErrNodeNotEmpty`, I-42) and the repair cases for draining and removed
holders work on a disk unchanged. A `Placement` entry still holds a `NodeIdx`; the chunk
codec does not change.

A device stores nothing itself: its `data_dir` holds Raft, `node_id`, `cluster_id`, the
read cache and the overlay WAL, never a shard. Only disks are placement targets; a device
row's `Capacity` stays 0 and is read by nothing (I-118). A device with no disks is a
**diskless host**: a full Raft member (voter if configured) and gateway whose writes land
on other hosts' disks (§9). `NodeUpsert` refuses a `KindDisk` row whose `Host` is not an Active device
(`ErrBadCommand`); removing a device is refused while any of its disks is not `Removed`.
A disk is never more active than its host: while the host is Draining or Removed its disks
count as such for placement and repair, so `node drain <host>` empties the host's disks
too.

The **domain** of a target is its `Host` for a disk and its own `Idx` for a device or an S3
node. A device and its disks form one domain.

## 2. Configuration and `node add-disk`

```yaml
node:
  data_dir: /var/lib/tailraid/data   # Raft, WAL, cache; no shards
  disks:
    - path: /mnt/a                   # one entry per disk
      capacity: ""                   # "" derives from the disk's filesystem, "500G" pins
```

`tailraid node add-disk <path> [--capacity SIZE]`, run on the host:

1. refuses a path that holds anything other than a `lost+found` directory;
2. writes the **disk marker** `<path>/disk_id`: `cluster_id`, a fresh disk ID and the
   host's node ID, fsynced with its directory;
3. appends the entry to the config;
4. proposes `NodeUpsert{Kind: KindDisk, Host: self, ID: diskID, Capacity: budget}` through
   the leader (the `member` op `add_disk`, forwarded like `join`);
5. the running daemon opens the store on its next maintenance tick (no restart needed),
   except under the systemd unit `install` writes on Linux: its sandbox lets `serve` write
   only the paths `ReadWritePaths` names, so `add-disk` there rewrites the unit from the
   config, reloads systemd and restarts the service, and waits for the node to come back
   and open the disk (I-147). Without that the disk opened read-only for `serve`, and
   every shard placed on it failed.

`init` and `join` take the disks already listed in the config: each is marked and
registered with the device row, so both stay option-free (12 §7, I-121); with no disks
listed both print that the device stores no data and how to add a disk. A marker that
already names this cluster and host is reused, so a retried `init` or `join` is idempotent. A disk is named
`<host>/<n>` in `node ls`, `node describe`, `node drain` and `node remove`; `ls` groups
disks under their host.

A disk leaves the way a node does: `node drain <host>/<n>` moves its slots off (in auto mode
the policy follows, §6), then `node remove <host>/<n>` takes it out of the node table, which
is refused while it still holds a slot (I-42). Its directory and marker are left as they are:
added back on the same path, the disk takes a new row under its marker's ID and the host
opens it under that row, the removed row standing for its history (I-90).

A path that sits on the same filesystem as another target is accepted with a warning: the
two share one budget (§3) and one failure.

## 3. Stores and budgets

`serve` opens one `chunk.Store` per configured disk, at `<path>/shards`. Every store keeps
the existing layout, file format and sweep (01 §Shards, 06 §3). A disk must also take a
new file: `serve` writes and removes a check file in the store when it opens the disk and
on every maintenance tick, and a disk that refuses it (remounted read-only after errors,
or outside what the service may write) is closed with the reason `not writable: …`,
reported not OK like an unmounted one (§4), and reopened by the tick that finds it
writable again (I-146). A disk that answered reads but refused writes would otherwise
stay a placement target and fail every shard sent to it.

Before opening an added disk, `serve` reads its marker. A missing marker, a different
`cluster_id`, or a disk ID that is not a row of this host leaves the disk **closed**: it is
reported not OK and never written. This is what keeps an unmounted mount point (an empty
directory on the root filesystem) from silently filling the system disk and reporting every
shard absent. `Store.Open` never writes a marker; only `add-disk` and `init` do.

Each store derives its own budget (01 §Node budget) from the filesystem holding its root,
and the maintenance tick proposes `set_capacity` for each target separately, past the same
hysteresis. When several targets of one host share a filesystem, its `used + free − reserve`
is divided equally among them. A pinned `capacity` is never re-derived.

`tailraid volume grow <disk> <size>` pins a disk's budget without hand-editing the config:
it rewrites that disk's `capacity:` (the disk named as `node ls` shows it, by its number
or by its path), validates and saves the config, and restarts the service so the budget
takes effect; `auto` as the size removes the pin so the budget follows free space again;
`--no-restart` writes the config only. An unknown disk or an unparsable size changes
nothing (I-130).

## 4. Transport and liveness

The shard RPC addresses a target by index: the resolver maps a disk to its host's
`Addr:RPCPort` plus the disk ID: `/v1/disks/{diskID}/shards/{key}`, the only shard route,
with the methods, headers, fence and status codes of 04 §3 and 06 §2. A request for a
device index is refused by the client as `ErrUnavailable` (a device is not a target). A request for a disk ID that is not open on that host is `404` with
`X-Tailraid-Disk: closed`, which the client maps to `ErrUnavailable` (never to "shard
absent"). On the host itself the client goes straight to the store.

`/health` reports each target: `{idx, used, free, capacity, ok}`, and each mirror of the
host (docs/16 §1). The prober still probes
hosts only. A disk is **live** iff its host is live and the host's last `/health` reported
it `ok`; it is dead from the probe that first reports it not OK. A disk that fails while
its host stays up is therefore dead to every node within one probe interval, and the
repairer re-places its slots after `fill_after` exactly as for a dead node (06 §4 case 3).
I/O errors other than `ErrCorrupt` and `ErrFull` on a store mark that disk not OK until it
answers a probe read again.

## 5. Placement with domains

```go
type Candidate struct { Idx NodeIdx; Weight uint64; Voter bool; Domain NodeIdx }
```

`Candidates` sets `Domain` as in §1 and `Voter` to the host's voter flag, so the data
shards of a chunk still prefer always-on machines (I-77) and a disk on a laptop is a
parity target.

`Place(h, cands, n)` keeps the scores of 03 §Rendezvous, then picks in score order with a
**host cap** `c = ceil(n / D)`, D the number of distinct domains among the candidates: a
candidate is skipped while its domain already holds `c` picks. When the first walk leaves
slots unfilled (some domains have too few targets), a second walk in the same order fills
them from the skipped candidates. The result is still a pure function of
`(h, candidate set, n)` (I-8). With one target per domain `c = 1` and the result equals the
plain top-`n`, so placements of clusters without disks are unchanged; on a single host
`c = n` and placement is weighted HRW over its disks.

`PlaceKM` and `FillPlacement` count the domains of slots already held: a refilled slot
skips any domain at the cap given the slots kept, so repair, migration and drain never put
a host above the cap when another placement exists (I-89). The voter preference for data
slots ([03](03-erasure-coding-and-placement.md)) yields to the cap the same way: a voter
host's third disk never takes a data slot while a non-voter under the cap is free
(I-173).

## 6. Policy

`AutoPolicy` (03 §Choosing the policy) counts **targets**, not devices: every Active disk
of an Active device; a device with no disks counts for nothing, and an Active S3 node
counts as one target on a host of its own that is never a voter (10 §4.1): a bucket is an
off-site host, so the policy that survives one host survives losing the bucket or any one
device. `v` counts the targets on voter hosts. The thresholds for `m` and the cap `k ≤ 6` are unchanged. One step
is added when there are `H ≥ 2` hosts with a target: `k` is lowered (never below 1) until
no host would hold more than `m` slots of a stripe under the host cap (so `ceil((k+m)/H) ≤
m` when every host has the targets), and an automatic policy survives the loss of one whole
host.

| hosts × targets | v | rule before the host step | policy | survives |
|---|---|---|---|---|
| 1 × 1 | 1 | 1+0 | 1+0 | 0 disks |
| 1 × 4 | 4 | 2+2 | 2+2 | 2 disks |
| 2 × 1 | 2 | 1+1 | 1+1 | 1 disk, 1 host |
| 2 × 3 | 6 | 4+2 | 2+2 | 2 disks, 1 host |
| 3 × 1 | 3 | 2+1 | 2+1 | 1 disk, 1 host |
| 4 × 3 | 12 | 6+2 | 6+2 | 2 disks, 1 host |

With one target per host the step never changes the rule's answer (`ceil((k+m)/H) = 1 ≤ m`
whenever `m ≥ 1`), so existing clusters keep their policy.

`PolicyAllowed` counts the same targets that are live (a disk is live per §4, and only
while its host is), plus live Active S3 nodes. `meta.Tolerance(nodes,
k, m)` returns the disks and hosts a policy survives: `m` disks; `floor(m / w)` hosts, `w` being the
most slots one host receives when each host is filled up to `ceil((k+m)/H)` and hosts with
spare targets take the rest, or 0 with a single host. `status`, `volume describe` and
`set-redundancy` print it ("survives 2 disks, 1 host"); pinning a policy that survives no
host on a cluster of two or more hosts prints a warning but is allowed.

Only a volume in auto mode follows its disks: adding one grows the policy on the leader's
next repair pass, and draining or removing one shrinks it (a draining target no longer
counts, so draining one of two targets moves the volume to `1+0` and migrates every chunk
off it). A pinned policy (`set-redundancy --k --m`, or `init --k --m`) is never changed;
`set-redundancy --auto` hands it back to the rule.

## 7. Home placement and skew

Every chunk has a **home**: `Home(h, nodes, k, m) = PlaceKM(h, HomeCandidates(nodes), k,
m)`, where `HomeCandidates` is `Candidates` with every Active target counted live. The home
depends on the hash and the membership (which targets are Active, their hosts, voter bits
and capacities) and on nothing else: never on liveness, suspect marks or fullness. A chunk
written while every target was up lands at its home already; a chunk written or repaired
while a target was dead, suspect (05 §6) or full, while a host was missing, or before a
disk or host joined, lands wherever `FillPlacement` found room among the live targets, and
stays there until the repairer moves it. **One rule decides where a slot belongs**: the
domain skew of earlier releases (the host cap) and the capacity rebalance within a host
(I-172) are both cases of it, and nothing else moves a good slot.

A slot is **away** when its target is not in its home for its class: a data slot (index
< k) when its target is not one of the home's data targets, a parity slot when its target
is not one of the home's parity targets. An away slot is **movable** when a home target of
its class holds no slot of the chunk and is live, not suspect and has room for the shard;
it then moves there, keeping its index (shard `i` stays shard `i`). Every away slot of a
chunk moves in one `SetPlacement`, so a home target that held a slot of the other class is
freed and taken in the same step, and the host cap is checked against the placement the
step commits, never against a half-moved one: a chunk is home after one move once its
targets are live. Slots on an S3 node are never moved and an S3 node is never the
destination of a move (egress, and only the leader's daily sweep deletes in a bucket,
I-138), though S3 rows count in the home as they do for writes. A chunk whose `(k,m)`
differs from the volume's is not judged (the migration re-places every slot, and lands it
at home, 06 §7), nor is a tombstone. Every move is the repair move sequence: read, PUT to the home target,
`SetPlacement` with `Expect`, then the fenced DELETE, so the chunk never has fewer readable
slots than before (I-65) and never two slots on one target (I-9).

Away slots are of two kinds, and keep the urgency each had before the fold:

- **Cap-breaking** (domain skew): a host holds more than `ceil((k+m)/D)` of the chunk's
  slots although the live candidates admit a placement within the cap. The chunk survives
  fewer hosts than the policy promises, so `meta.ChunkState` reports it as `DomainSkew` with
  the excess slots (the highest indices on the over-cap host) as bad, the repairer moves
  them like draining slots (06 §4 case 6) without any gate, and `status` counts such chunks
  as `domain_skew`. Each excess slot goes to its home target when that target is movable,
  otherwise to whatever `FillPlacement` finds under the cap: restoring the host tolerance
  outranks reaching home, and a later pass brings it home. Every other refill (a missing
  slot, a dead or draining target) aims at home the same way (I-296), so repair does not
  itself create away slots that the gate must undo. Skew no placement can
  fix (too few hosts or live targets) is not reported and does not count as degraded
  (L-194).
- **Placement-only**: every other away slot. The chunk has the tolerance the policy
  promises; it sits on a non-voter where a voter is home for a data slot, on the wrong disk
  of a host, or on a host that is not its home since a host or disk joined. These move only
  through the **gate** (I-295), so liveness churn never moves data and a small residue of
  ad hoc slots does not keep the pool busy:
  - the leader's repair sweep (06 §4a) counts the movable bytes of placement-only slots
    and queues their chunks; when they reach `repair.home_start` (default 5 %) of the
    stored bytes, the gate opens, and it stays open until a sweep finds none left or the
    home candidate set changes (the gate then re-evaluates against the new homes);
  - while open, each pass moves the placement-only slots of at most `HomeBatch` chunks
    from the queue: `HomePerWorker` (16) for each of `repair.workers` (64 at the default
    4, 1024 at 64), so the fan-out an operator raises for a refill also hurries a
    rebalance; and none when the pass before found a bad slot or a pending migration, at
    background priority and under `repair.rate` and `repair.workers` (06 §4); a pass that
    finds the queue empty sweeps again;
  - `repair.home_start: 0` closes it for good; `tailraid volume rebalance` opens it once
    whatever the threshold, until the movable bytes reach 0 (`--dry-run` lists the bytes
    each target would send and receive and moves nothing);
  - an away slot whose home target is dead, suspect or full stays where it is (L-220): it
    is counted as away but not movable, and moves once that target can take it.

Since the home is weighted rendezvous by capacity, a host whose slots are home holds each
of its disks' capacity share of them (what the web UI's Disks block draws as the tick), so
capacity within a host follows from home placement and needs no rule of its own. After
`node add-disk` or a joined host, the fraction of slots whose home moved is about the new
target's weight over the total, the least any placement keyed by capacity can move, and
the gate moves them over the following passes. A budget re-proposed past I-74's hysteresis
changes the homes of a few percent of one disk's slots, which count toward the gate like
any other placement-only slots.

`status` carries `domain_skew` (chunks with cap-breaking slots), `disk_skew` (placement-only
away slots whose home is another disk of the same host, what the Disks block summarises),
and `repair.home{away_slots, away_bytes, movable_bytes, open}` for the whole volume;
`tailraid status` prints it as one line (`home  12,345 slots away (48.2 GiB, 31.0 GiB
movable), gate closed below 5 %`), and the dashboard's integrity panel shows the same line.

## 8. What the user sees

- `node ls` groups disks under their host; `status` and the web UI show per-target used,
  budget, stored shard bytes (`stored_bytes`, 10 §4.1), slots and liveness, and the
  volume's "survives N disks, M hosts".
- The web UI's **Disks** block (13 §Design) lists every drive of the volume. It opens
  with a spread summary (one column per target, grouped by host, above or below its
  expected share; the S3 buckets as a group named `s3`; a host's archive mirror, which
  holds snapshots and never a slot (16), as a column filled from the bottom by how full
  its disk is, dashed while it is offline) and then draws each host as
  a card with one row per target (its slots on one shared scale against the
  expected-share tick, and its used bytes of budget) and one per mirror (its used bytes
  of the disk, `archive · online` or `archive · offline since <date>`), and the buckets
  as a card named `S3` with one row per bucket (its slots against its share, its stored
  bytes and `bucket/prefix`). The Balance block lists the members that hold slots: a
  mirror is never there.
- The soak report's **data spread across disks** chart (09 §soak) draws one lane per disk
  over time, coloured by each disk's slot share against its expected share, with a host-cap
  strip underneath.
- **A removed member is not listed.** Its row stays in the node table for good (an index
  is never reused, so old placements stay interpretable, 02), but `status`, `node ls` and
  the web UI's Balance and Disks blocks leave `removed` rows out: a device that was removed
  and joined again shows once, under its new index. `node ls --all` lists them, `--json`
  and `node describe` always carry them (I-132).
- **A diskless host is shown as such.** `node ls` prints a device row with `-` for FREE and
  the flag `no disks` when it has no disk rows; `status` reports it `diskless`, and the web
  UI's Disks block draws its card with no target rows and the note `gateway only · no
  disks`. When it is dead, `status` prints no "unreachable for" warning for it, before or
  after `evict_after`: it holds no slot, so nothing waits on it and there is nothing to
  re-place (a dead voter still shows in the quorum line).
- **Targets on one physical disk are flagged.** Each host reports the filesystem ID of
  every target it serves (`fs` in `/health`, from the device number on Unix and the volume
  on Windows); `status` marks targets of one host that share a filesystem with
  `shared_with`, the names of the others. They fail together, so redundancy between them
  is illusory: `node ls` shows `shares disk with <names>` in its flags, `status` adds a
  warning line, and the web UI puts the warning icon (the amber `!` of the warnings strip)
  beside each such target, its title naming the others.

## 9. Diskless hosts and legacy state

A host with no disks is a member like any other: it votes if configured, it mounts the
volume, its reads fetch shards from the hosts that have them and its writes are placed on
them. Removing a host's last disk (`node remove <host>/<n>`, refused while the disk holds
a slot, I-42) leaves such a host; nothing else changes about it. A volume whose node table
holds no Active disk or S3 row has nowhere to put a chunk: every flush fails with
`ErrFull`, so the client sees `ENOSPC` (I-120), not the `EIO` of disks that exist but are
dead.

Clusters created when the device row was a target hold slots on device indices and a
`Capacity` on device rows. Nothing migrates them: before any listener opens, `serve`
inspects the restored state and refuses to run when a device row has `Capacity > 0` or a
chunk slot names a device index, printing that the cluster must be re-created (`tailraid
reset --yes` on every device, then `init` and `join`, and the data copied back; I-119).
The FSM itself still applies such entries, so a replayed log restores before the check
runs.

**Mirrors are not targets.** A host may also carry mirrors (docs/16): disks given to
snapshots, `KindMirror` rows with the host as their `Host`. A mirror is never a placement
candidate, never counted by the automatic policy, the host cap, the budget or the skew,
and never repaired; a host whose only disks are mirrors is a diskless host for this
document. It shares the disk machinery that makes sense for it: a marker that keeps an
unmounted path closed, liveness through its host, and `node ls` listing it under its host
as `<host>/m<n>`.

## Invariants

- **I-86** — A disk that is not marked for this cluster and this host (`disk_id` missing,
  another `cluster_id`, or a disk ID that is not a row of this host) is never opened: no
  shard is written to it, none is served from it, the sweep never runs on it, and the host
  reports it not OK so it is dead to placement and reads.
- **I-87** — `Place(h, cands, n)` gives no domain more than `ceil(n/D)` of its picks
  whenever the candidates admit such a choice, and with one candidate per domain equals the
  top-`n` by score; `FillPlacement` respects the same cap counting the slots already held.
- **I-88** — When the Active targets span two or more hosts, `AutoPolicy` returns a policy
  with `ceil((k+m)/H) ≤ m`, so it survives the loss of any one host; with one target per
  host it equals the device-count rule of I-75.
- **I-89** — Repair, migration and drain never raise a host's slot count of a chunk above
  the cap while a placement within it exists, and a skewed chunk is moved to within the cap
  without ever having fewer than `k` readable slots.
- **I-90** — A disk is live iff its host is live and the host's last `/health` reported it
  OK; a closed or failing disk is dead to every node within one probe interval and its
  slots are re-placed after `fill_after`, while the host's other targets keep serving.
- **I-91** — Each target's budget is derived from the filesystem holding its own store and
  proposed separately past the hysteresis of I-74; targets sharing a filesystem divide it,
  so their budgets never sum to more than that filesystem's `used + free − reserve`.
- **I-118** — A device row is never a placement target: `Candidates`, `AutoPolicy`,
  `PolicyAllowed`, `Tolerance`, `Usage` and the reports count only disk and S3 rows, no
  chunk slot names a device index, `set_capacity` is refused for a device, and a device
  with no disks is a full member (voter if configured) and gateway that reads and writes
  the volume while holding no slot; removing a host's last disk leaves it such a diskless
  host.
- **I-119** — `serve` refuses, before any listener opens, a restored state in which a device
  row has `Capacity > 0` or a chunk slot names a device index, and prints that the cluster
  must be re-created with `reset` and `init`/`join`; the check runs again once the node
  is ready and its FSM holds every entry of its log, for a snapshot older than the
  legacy entries.
- **I-120** — A volume whose node table holds no Active disk or S3 row fails every flush
  with `ErrFull`, so the client sees `ENOSPC`, never `EIO`.
- **I-121** — `join` marks and registers the disks listed in its config exactly as `init`
  does, named `<host>/<n>` from 1; `init` and `join` with no disks print that the device
  stores no data and how to add a disk; neither registers a budget on the device row.
- **I-130** — `volume grow <disk> <size>` rewrites exactly that disk's `capacity` in the
  config (`auto` clears it) for a disk named by `<host>/<n>`, its number or its path,
  saves a config that validates, and restarts the service unless `--no-restart`; an
  unknown disk, a device without disks or an unparsable size leaves the config untouched.
- **I-132** — `status`, `node ls` and the web UI's Balance and Disks blocks omit `removed`
  rows, which stay in the node table because an index is never reused; `node ls --all`
  lists them and `--json` carries them.
- **I-146** — A disk whose store does not take a new file is closed with the reason when
  it opens and at every maintenance tick, so it is never a live target, and it reopens at
  the first tick that finds it writable.
- **I-147** — On Linux with the systemd unit installed, `node add-disk` rewrites the unit
  from the saved config, so `ReadWritePaths` names the new disk, and restarts the service
  before asking the node to open it.
- **I-172** — Capacity within a host follows from home placement (§7): when every slot of
  a host is home, each of its Active disks holds its capacity share of the host's slots
  within the spread of weighted rendezvous, and after `node add-disk` the gate moves the
  slots whose home is the new disk onto it, never two slots of a chunk on one target (I-9),
  never above the host cap, the old shard deleted only after the new placement applied
  (I-65). No other rule moves slots between the disks of a host.
- **I-294** — `Home(h, nodes, k, m)` is a pure function of the hash and the Active
  membership (targets, hosts, voter bits, capacities), the same on every node, and equal to
  the placement a write gets when every Active target is live; `HomeMoves` sends an away
  slot only to a home target of its class that holds no slot of the chunk and is live, not
  suspect and has room, keeps the slot's index, never moves a slot on or onto an S3 node,
  and returns nothing for a chunk whose slots are home, so a second pass after convergence
  moves nothing and liveness alone never changes a home.
- **I-295** — Placement-only moves run only while the gate is open: it opens when the
  movable placement-only bytes reach `repair.home_start` of the stored bytes (or once by
  `volume rebalance`), closes when a pass finds none movable, and re-evaluates when the
  home candidate set changes; an open pass moves at most `HomeBatch` chunks
  (`HomePerWorker` 16 for each of `repair.workers`), none when the pass before found a
  bad slot or a pending migration, at background priority and under `repair.rate`; `repair.home_start: 0` never opens it on its own, and `volume
  rebalance --dry-run` moves nothing.
- **I-296** — Every slot the repairer refills (missing, on a dead, draining or removed
  target, reported, absent, or cap-breaking) goes to its home target when that target is
  movable, and otherwise to a target `FillPlacement` finds among the live candidates under
  the cap; cap-breaking slots move without the gate, so restoring the host tolerance never
  waits on a dead, suspect or full home, and a refill never makes a chunk away that had a
  movable home.
- **I-311** — A repair never plans or proposes a placement that names a target twice: a
  holder may move to another slot of its chunk (a swap home), but a slot that keeps its
  holder (no home could take it) wins over that move, which is planned again without it,
  and a still-served slot whose PUT failed gets its holder back only when no other slot
  now names it; a planned placement the FSM would refuse is dropped before any PUT. A pass whose home moves all committed nothing shuts the gate for
  `HomeHold` (30 min), so a move that cannot succeed is not retried, bytes and all, every
  pass (on the real cluster after v0.8.0 every home move failed as a malformed
  `SetPlacement` and the open gate re-PUT the same chunks each minute).

## Tests

| Invariant | Tests |
|---|---|
| I-86 | `TestChunkRPC_DiskPath_RoundTrip`, `TestClient_LocalDiskRouting`, `TestDiskMarker_Table`, `TestNode_ClosedDisk_NotServedNotSwept` |
| I-87 | `TestCandidates_DiskDomainAndVoter`, `TestFillPlacement_HostCap`, `TestPlace_HostCap`, `TestPlace_HostCap_NNodeDeterminism`, `TestPlace_OneTargetPerHost_Unchanged` |
| I-88 | `TestAutoPolicy_Disks_Table`, `TestAutoPolicy_OneTargetPerHost_Unchanged`, `TestAutoPolicy_SurvivesOneHost`, `TestCluster_AutoPolicy_SingleHostDisks`, `TestPolicyAllowed_CountsTargets`, `TestTolerance_Table`, `TestAutoPolicy_CountsS3` |
| I-89 | `TestChunkState_DomainSkew`, `TestModelCheck_Long`, `TestRepair_DeadDisk_RePlacesWithinCap`, `TestRepair_DomainSkew_MovesWithinCap`, `TestRepair_DomainSkew_UnfixableIgnored`, `TestRepair_Migration_WithinCap_VoterHostDisks` |
| I-90 | `TestChunkRPC_DiskPath_RoundTrip`, `TestCluster_SingleHostDisks_DiskFails_ReadsAndRepair`, `TestMember_AddDisk_ResolvesToHost`, `TestProber_DiskLiveness` |
| I-91 | `TestBudget_PerDisk_SharedFilesystem`, `TestNode_Rebudget_PerTarget` |
| I-118 | `TestAutoPolicy_DisklessHostNotCounted`, `TestCandidates_DevicesNeverTargets`, `TestCluster_DisklessVoter_WritesLandOnOtherHosts`, `TestCluster_RemoveLastDisk_HostBecomesDiskless`, `TestMemberOp_SetCapacity_RefusesDevice`, `TestPrintNodes_DisklessAndSharedDisk`, `TestReport_DisklessAndSharedDisk`, `TestStatus_NoUnreachableWarningForDisklessHost`, `TestUsage_DisksOnly` |
| I-119 | `TestCLI_Serve_RefusesLegacyState`, `TestLegacyDeviceTargets_Table`, `TestServe_RefusesLegacyState` |
| I-120 | `TestCluster_Diskless_WriteIsENOSPC`, `TestFlusher_NoTargets_ErrorWrapsErrFull`, `TestHasTargets_Table` |
| I-121 | `TestCLI_Init_NoDisks_PrintsHint`, `TestCLI_Join_RegistersConfiguredDisks` |
| I-130 | `TestCLI_VolumeGrow_EditsConfig` |
| I-132 | `TestPrintNodes_HidesRemoved` |
| I-146 | `TestDiskSet_UnwritableDiskClosed` |
| I-147 | `TestSvc_RefreshUnit_NamesNewDiskAndRestarts` |
| I-172 | `TestHomeMoves_Table`, `TestRepair_Home_AddDiskFillsWithinHost` |
| I-294 | `TestHome_EqualsWritePlacement`, `TestHome_NNodeDeterminism`, `TestHomeMoves_Idempotent`, `TestHomeMoves_Table`, `TestCluster_DegradedWrite_DataSlotsComeHome` |
| I-295 | `TestRepair_Home_Gate_Table`, `TestRepair_Home_AddDiskFillsWithinHost`, `TestCluster_DegradedWrite_DataSlotsComeHome`, `TestCLI_VolumeRebalance_DryRun`, `TestConfig_HomeStart`, `TestRepair_HomePerPass_FollowsWorkers` |
| I-311 | `TestPlanSlots_HomeMoveNeverNamesATargetTwice`, `TestRepair_Home_FailedMovesShutTheGate` |
| I-296 | `TestChunkState_DomainSkew`, `TestFillHome_RefillsFromHome`, `TestRepair_RefillGoesHome_Table`, `TestRepair_DomainSkew_HomeDeadFillsUnderCap` |
