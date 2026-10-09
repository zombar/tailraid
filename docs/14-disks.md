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

A device's own store at `<data_dir>/shards` is its **disk 0**: the device row itself, with
the device's `Capacity`. Clusters created before disks existed are therefore unchanged,
and a device that should not store shards on its system disk pins `node.capacity: 0`.
`NodeUpsert` refuses a `KindDisk` row whose `Host` is not an Active device
(`ErrBadCommand`); removing a device is refused while any of its disks is not `Removed`.

The **domain** of a target is its `Host` for a disk and its own `Idx` for a device or an S3
node. A device and its disks form one domain.

## 2. Configuration and `node add-disk`

```yaml
node:
  data_dir: /var/lib/tailraid/data   # Raft, WAL, cache, disk 0
  capacity: ""                       # disk 0's budget: "" derives, "500G" pins, "0" opts out
  disks:
    - path: /mnt/a                   # one entry per added disk
      capacity: ""                   # "" derives from the disk's filesystem
```

`tailraid node add-disk <path> [--capacity SIZE]`, run on the host:

1. refuses a path that holds anything other than a `lost+found` directory;
2. writes the **disk marker** `<path>/disk_id`: `cluster_id`, a fresh disk ID and the
   host's node ID, fsynced with its directory;
3. appends the entry to the config;
4. proposes `NodeUpsert{Kind: KindDisk, Host: self, ID: diskID, Capacity: budget}` through
   the leader (the `member` op `add_disk`, forwarded like `join`);
5. the running daemon opens the store on its next maintenance tick (no restart needed).

`init` takes the disks already listed in the config: each is marked and registered in the
same bootstrap as the device row, so `init` stays option-free (12 §7). A disk is named
`<host>/<n>` in `node ls`, `node describe`, `node drain` and `node remove`; `ls` groups
disks under their host.

A path that sits on the same filesystem as another target is accepted with a warning: the
two share one budget (§3) and one failure.

## 3. Stores and budgets

`serve` opens one `chunk.Store` per target it hosts: disk 0 at `<data_dir>/shards` and
each configured disk at `<path>/shards`. Every store keeps the existing layout, file
format and sweep (01 §Shards, 06 §3).

Before opening an added disk, `serve` reads its marker. A missing marker, a different
`cluster_id`, or a disk ID that is not a row of this host leaves the disk **closed**: it is
reported not OK and never written. This is what keeps an unmounted mount point (an empty
directory on the root filesystem) from silently filling the system disk and reporting every
shard absent. `Store.Open` never writes a marker; only `add-disk` and `init` do.

Each store derives its own budget (01 §Node budget) from the filesystem holding its root,
and the maintenance tick proposes `set_capacity` for each target separately, past the same
hysteresis. When several targets of one host share a filesystem, its `used + free − reserve`
is divided equally among them. A pinned `capacity` is never re-derived.

## 4. Transport and liveness

The shard RPC addresses a target by index: the resolver maps a disk to its host's
`Addr:RPCPort` plus the disk ID. Disk 0 keeps `/v1/shards/{key}`; an added disk is
`/v1/disks/{diskID}/shards/{key}` with the same methods, headers, fence and status codes
(04 §3, 06 §2). A request for a disk ID that is not open on that host is `404` with
`X-Tailraid-Disk: closed`, which the client maps to `ErrUnavailable` (never to "shard
absent"). On the host itself the client goes straight to the store, as it does today for
disk 0.

`/health` reports each target: `{idx, used, free, capacity, ok}`. The prober still probes
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
a host above the cap when another placement exists (I-89).

## 6. Policy

`AutoPolicy` (03 §Choosing the policy) counts **targets**, not devices: every Active
device with `Capacity > 0`, every Active disk, and S3 nodes as before; `v` counts the
targets on voter hosts. The thresholds for `m` and the cap `k ≤ 6` are unchanged. One step
is added when there are `H ≥ 2` hosts with a target: `k` is lowered (never below 1) until
`ceil((k+m)/H) ≤ m`, so an automatic policy always survives the loss of one whole host.

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

`PolicyAllowed` counts live Active targets (a disk is live per §4). `meta.Tolerance(nodes,
k, m)` returns the disks and hosts a policy survives: `m` disks; `floor(m / c)` hosts with
`c = ceil((k+m)/H)`, or 0 with a single host. `status`, `volume describe` and
`set-redundancy` print it ("survives 2 disks, 1 host"); pinning a policy that survives no
host on a cluster of two or more hosts prints a warning but is allowed.

## 7. Domain skew

A chunk is **skewed** when one host holds more than `ceil((k+m)/D)` of its slots although
the current candidates admit a placement within the cap. Skew arises from chunks written
while the cluster had one host, or before a disk was added on another host, or while hosts
were draining. `meta.ChunkState` reports it as `DomainSkew` with the excess slots (the
highest slot indices on the over-cap host) as bad; the repairer moves them like draining
slots (06 §4 case 6): read, PUT to a host under the cap, `SetPlacement`, then the fenced
DELETE, so a chunk never has fewer than `k` readable slots (I-32, I-65). Skew that no
placement can fix (too few hosts or targets) is not reported and does not count as
degraded. `status` counts skewed chunks as `domain_skew`.

There is no capacity rebalancing: a disk that is emptier than its share receives new
writes and repairs only (06 §6).

## 8. What the user sees

- `node ls` groups disks under their host; `status` and the web UI show per-target used,
  budget, slots and liveness, and the volume's "survives N disks, M hosts".
- The web UI's **Disks** block (13 §Design) opens with a spread summary (one column per
  target, grouped by host, above or below its expected share) and then draws each host as a card with one row per
  target: its slots on one shared scale against the expected-share tick, and its used
  bytes of budget.
- The soak report's **data spread across disks** chart (09 §soak) draws one lane per disk
  over time, coloured by each disk's slot share against its expected share, with a host-cap
  strip underneath.

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

## Tests

| Invariant | Tests |
|---|---|
| I-86 | `TestStub_I_86` |
| I-87 | `TestStub_I_87` |
| I-88 | `TestStub_I_88` |
| I-89 | `TestStub_I_89` |
| I-90 | `TestStub_I_90` |
| I-91 | `TestStub_I_91` |
