# 03 — Erasure coding and placement

Each chunk is one stripe: `k` data shards and `m` parity shards computed with Reed-Solomon
over GF(2^8) (`klauspost/reedsolomon`). Shards are placed on `k+m` distinct targets (devices, their added disks, or S3 nodes)
chosen by weighted rendezvous hashing over the live, active candidate set at write time, and the
chosen placement is recorded in the chunk entry. Reads need any `k` shards. Every chunk
records its own `(k, m)`, which makes a later change of the volume policy a lazy migration
rather than a rewrite.

## `internal/ec`

```go
const Align = 64

type Codec struct { k, m int; rs reedsolomon.Encoder } // rs == nil when k == 1 or m == 0

func New(k, m int) (*Codec, error)        // 1 <= k, 0 <= m, k+m <= 256
func ShardLen(size, k int) int            // roundUp(ceil(size/k), Align); size 0 → Align
func (c *Codec) Encode(chunk []byte) ([][]byte, error)
func (c *Codec) Decode(shards [][]byte, size int) ([]byte, error)
func (c *Codec) Verify(shards [][]byte) (bool, error)

type Pool struct{ ... }                   // (k,m) → *Codec, because (k,m) is per chunk
func (p *Pool) Get(k, m uint8) (*Codec, error)
```

**Encode.** Returns `k+m` slices, each exactly `ShardLen(len(chunk), k)` bytes, freshly
allocated (never aliasing the input). The chunk is zero-padded to `k*ShardLen` and split into
`k` data shards; parity is computed over the padded data. Encoding is a pure function: the
same chunk bytes always produce the same shards, which is required for content addressing
(two gateways racing on the same new chunk PUT byte-identical shards).

**Decode.** `shards` has length `k+m`; `nil` marks a missing shard. With fewer than `k`
non-nil shards: `ErrTooFewShards`. If all `k` data shards are present they are concatenated
directly; otherwise `ReconstructData` is run first. The result is trimmed to `size`. Padding
never leaks (I-10).

**Verify.** Requires all `k+m` shards; returns whether parity is consistent. Used by scrub.

**Special cases.** `k == 1` is replication: every shard is the chunk padded to `ShardLen`
(`m+1` identical copies); no matrix is built. `m == 0` is plain splitting with no parity
(allowed, discouraged; `status` warns). Alignment to 64 bytes keeps shard lengths SIMD
friendly and bounds padding at `64*k` bytes per chunk.

**Policy selection per chunk** (in the flusher):

```
if size < ChunkSize and size <= 64 KiB (meta.SmallChunkMax):  k = 1, m = Vol.M
else:                                                          k = Vol.K, m = Vol.M
```

A full chunk is never "small", however small the chunk size (test harnesses use 64 KiB
chunks). `meta.IsSmallChunk` and `meta.TargetPolicy` are the only places this rule lives:
the flusher, the repair scanner and every report read them.

Small files thus cost `(m+1)×` their size with a single RPC per replica instead of `k+m`
RPCs for a few kilobytes. Space overhead of a full 1 MiB chunk at 2+1 is 1.5× plus ≤128 B.

## Placement

### Candidates

```go
type Candidate struct { Idx meta.NodeIdx; Weight uint64; Voter bool; Domain meta.NodeIdx }
// Weight = Node.Capacity; Voter = the host's Voter; Domain = Host for a disk, else Idx
```

A candidate is a **target**: one of a device's disks, or an S3 node (14); a device row
itself is never one (14 §9). Targets of one host share a **domain**; `PlaceKM` spreads a stripe across domains
first (14 §5).

At write time the flusher builds the candidate list from `State.Nodes` filtered to
`State == Active` **and** currently live (`Peers.Live()`), with `Weight = Capacity`. Nodes
that are `Draining` or `Removed`, dead, or that reported `ErrFull` (HTTP 507) on their last
PUT are excluded. S3 nodes (`Kind == KindS3`) are candidates when their bucket probe is
live. The candidate list is built by the gateway doing the flush from its *own* prober
view; no node is asked for permission.

### Voter-first slots

```go
func PlaceKM(h meta.Hash, cands []Candidate, k, m int) []meta.NodeIdx
```

Voters are the always-on devices; non-voters are the laptops that sleep (00 glossary).
`PlaceKM` draws the `k` **data** slots by rendezvous from the voter candidates when at
least `k` of them are present, otherwise from all candidates, and then the `m` **parity**
slots by rendezvous from the candidates not yet used, voters and non-voters alike. The
result is still a pure function of `(h, candidate set, k, m)` (I-8, I-9 hold), and when no
candidate is a voter it is a plain rendezvous over all candidates (the former `Place`, which only tests called, is gone: the tests prove `PlaceKM`). `FillPlacement(h, place, cands, k)`
refills a zero data slot from voters first and a zero parity slot from anyone.

What this buys: a chunk is readable from the live voters alone whenever `k ≤` live
voters (I-77), and a flush collects its `k` durable acks (I-7) from always-on devices, so
a closed laptop costs a parity slot, never a write; the repairer fills that slot when the
laptop wakes (06). Voters receive the data shards and fill faster; the derived capacity
weight (01) leans new placements toward the emptier disks. Existing placements never
move because the rule changed.

### Rendezvous (HRW) hashing

```go
func PlaceKM(h meta.Hash, cands []Candidate, k, m int) []meta.NodeIdx          // a fresh k+m stripe
func FillPlacement(h meta.Hash, place []meta.NodeIdx, cands []Candidate, k int, dom Domains) // the zero slots of one
```

For each candidate compute
`u = fmix64(fnv64a(h[:] ‖ u16le(idx))) / 2^64` mapped into the open interval `(0, 1)` (a
result of 0 or 1 is nudged by one ulp), and `score = -Weight / ln(u)` (weighted rendezvous
hashing; a node with twice the weight receives twice the shards in expectation). `fmix64` is
the MurmurHash3 64-bit finalizer; it is required because plain FNV-1a has no avalanche over
the two trailing `idx` bytes, so the `u` values of different candidates for the same hash
were nearly equal and the weights were effectively ignored (a 2:1 weighting measured
99984:16 before the mix). Sort candidates by `score` descending, ties broken by `idx` ascending, and take
the first `n`, skipping a candidate whose domain already holds `ceil(n/D)` picks and
filling any slot left over from the skipped ones in the same order (the host cap, 14 §5;
with one target per domain this is the plain top-`n`). Shard `i` goes to the `i`-th selected node. Properties:

- Pure function of `(h, candidate set, n)` (I-8): every node computes the same answer from
  the same inputs, which the predecessor got wrong by sorting its own name among peer IPs.
- Minimal disruption: adding a candidate moves only the shards that now hash highest onto
  it; removing one moves only that node's shards.
- With fewer than `n` candidates the result is shorter (`TestPlace_FewerCandidatesThanN`);
  the caller fills the remaining slots with 0 (degraded) or refuses, per the rules below.

The placement is **stored** in `Chunk.Placement` by `Commit`/`SetPlacement`. Membership or
liveness changes never move data implicitly; only the repairer, through `SetPlacement`,
changes where a shard lives. Reads therefore never guess. Every `SetPlacement` names the
placement it was planned from (`Expect`), and the FSM applies it only if that is still the
current one ([02](02-consistency-and-raft.md), I-102): the flusher filling a dedup'd
chunk's slots, a repair pass and a forced removal zeroing slots can run at once, and the
one that lost re-plans instead of orphaning the other's shards.

### Slot semantics

`Placement[i]` is the `NodeIdx` holding shard `i`, or `0` when no node holds it. A non-zero
entry pointing at a `Removed` or `Draining` node is treated by the repairer as work to do
but is still a valid read source while the node is live. Non-zero entries within one chunk
are always distinct (I-9): the same target never holds two shards of one stripe, since that
would make a single disk loss cost two shards. A host with several disks may hold several
shards of a stripe, never more than the host cap while another placement exists (I-87).

## Writing a chunk

The flusher ([04](04-write-path-and-durability.md)) for a new chunk hash:

1. Choose `(k, m)`; `placement = PlaceKM(h, candidates, k, m)`; `shards = Encode(chunk)`.
2. PUT every slot in parallel (self via the local store). Each PUT carries
   `X-Shard-SHA256`; the receiver verifies before the shard becomes visible.
3. Wait until **at least `k` slots are acknowledged** and either all PUTs finished or a
   10 s deadline passed. Failed or late slots: retry once with `FillPlacement` over candidates minus
   the failed nodes **and minus every node already holding an acknowledged slot of this
   stripe** (I-9); slot `i` keeps its index and takes the `i`-th entry of the new list that
   is not already used; still failed ⇒ slot `0`. An ack is a `200` from `PUT`, which the
   receiver returns only after the shard file and its directory are fsynced (I-7).
4. Fewer than `k` acknowledged slots ⇒ the flush fails (`ErrDegraded`), nothing is committed,
   the data remains in the WAL, and the flusher backs off and retries.
5. Otherwise `Commit` carries `ChunkDesc{Hash, Size, K, M, Placement}`.

A chunk committed with any slot `0` is **degraded** (fewer than `k+m` good slots).
`status`/`check` count these; the repairer fills a `0` slot as soon as any live spare node
exists — no hold-down applies to `0` slots, only to slots on dead nodes
([06](06-gc-and-repair.md)). This is the
"degraded writes allowed" decision: the volume stays writable while `k ≤ live < k+m`, and
freshly written chunks carry less than `m` parity until repair completes.

## Reading a chunk

```go
type Peers interface {
    Self() meta.NodeIdx
    Live() []meta.NodeIdx
    Rank(nodes []meta.NodeIdx) []meta.NodeIdx   // Self first; then devices by RTT ascending; S3 nodes last
}
```

`Reader.ReadChunk(h)`: cache hit → return. Otherwise `c = GetChunk(h)`; order
`Rank(live non-zero placement)` (dead holders are never dialled, L-131), then stably
partitioned: this host's own shards first, then data slots, then parity, whatever their
RTT, so a decode runs only when a data holder is dead or hedged or this host holds parity
(L-63); fetch the
first `k` in parallel; on any failure or `ErrCorrupt`
pull the next slot until `k` good shards are in hand; `Decode(shards, c.Size)`; verify
`sha256 == h` (a mismatch is `EIO` and is logged with the slot list for scrub); cache.
Fewer than `k` reachable shards ⇒ `EIO` for that chunk only.

S3 nodes rank last so that bulk reads never touch a bucket while enough device shards are
reachable; a bucket is the shard source of last resort and the off-site copy.

## Choosing the policy: automatic, or `volume set-redundancy --k K --m M`

`tailraid init` creates the volume with `Vol.Auto` set and `1+0`. In auto mode the leader
derives the policy from the member table with `meta.AutoPolicy(nodes)`, a pure function
of the counts of Active targets on voter hosts (`v`) and on non-voter hosts (`s`),
`n = v + s` (a target is a disk or an S3 node, 14 §6):

- `m = 0` for `n = 1`, `1` for `n` of 2 or 3, `2` for `n ≥ 4`
- `k = clamp(v, 1, n − m)`, capped at 6

| voters | non-voters | policy | readable with every laptop asleep |
|---|---|---|---|
| 1 | 0 | 1+0 | yes |
| 1 | 1 | 1+1 | yes |
| 1 | 3 | 1+2 | yes |
| 2 | 1 | 2+1 | yes |
| 3 | 0 | 2+1 | yes |
| 3 | 2 | 3+2 | yes |
| 2 | 4 | 2+2 | yes |

When the targets span `H ≥ 2` hosts, `k` is then lowered until `ceil((k+m)/H) ≤ m` so the
policy survives the loss of a whole host (I-88; 14 §6 has the table for disks).

`k` follows the voters so the data shards fit on always-on devices (voter-first slots
above); `m` follows the cluster size. The leader evaluates the rule at the start of every
repair pass (06, leader only, once a minute) and proposes `SetVolume{K, M}` when `Auto` is
set, the rule's answer differs from `Vol.(K, M)`, and `K+M ≤` live Active targets; if a
join makes a larger policy desirable while a device is asleep the change waits for a
later pass, never forced. Membership is the only input, never liveness: the policy grows
after a join or promotion and shrinks only after a removal or demotion, so a dead laptop
never shrinks it. A node's budget plays no part.

`volume set-redundancy --k K --m M` clears `Auto` (`SetVolumeMode{Auto: false}`) and
proposes `SetVolume{K, M}`; `volume set-redundancy --auto` sets `Auto` again and the next
pass applies the rule. Either way the CLI refuses a policy whose `K+M` exceeds the number
of **live Active targets** (devices, disks or S3). The FSM increments `PolicyVersion` itself. From the next flush, new chunks use the new `(K, M)`. Existing
chunks keep their recorded `(K, M)` and remain fully readable. The repairer treats
`chunk.(K,M) != Vol.(K,M)` as repair work: read `k_old` shards, decode, re-encode with the
new policy, PUT to a fresh placement under the new keys (`hex.K-M.idx`, which never collide
with the old `hex.k-m.idx` files), `SetPlacement{Hash, Placement, K, M}`, and only then
delete the old shards (after the commit is applied, with the DELETE fence of 06). At no instant does a chunk have fewer
than `k_old` reachable shards (I-32). `volume describe` shows `migrated / total`.

The same mechanism handles `node drain`: a draining node is excluded from candidates, the
repairer moves its slots to other nodes, and `node remove` is allowed once the node holds
no sole copies.

The ≥ k durable-acks rule for committing a flush is **I-7** (`04-write-path-and-durability.md`).

## Invariants
- **I-117** — The FSM accepts exactly the policies the codec accepts: `SetVolume` and
  `validPlacement` require `1 ≤ k` and `k+m ≤ ec.MaxShards` (256), so `255+1` is applied
  and `255+2` is `ErrPolicy`; the CLI checks the same bounds before narrowing `--k` and
  `--m` to a byte.

- **I-8** — `PlaceKM(h, cands, k, m)` is a pure function: for the same hash, the same candidate
  set (any order) and the same `(k, m)`, every node returns the identical sequence.
- **I-9** — The non-zero entries of any committed `Placement` are pairwise distinct target
  indices; a stripe never has two shards on one target (device store, disk or S3 node). With `≥ k+m` candidates all `k+m`
  slots are non-zero.
- **I-10** — For any chunk and any subset of `k` of its `k+m` shards,
  `Decode(subset, Size)` returns exactly the original `Size` bytes; with fewer than `k`
  shards `Decode` fails rather than returning data.
- **I-30** — `Encode` is deterministic and never aliases its input; all `k+m` shards have
  length `ShardLen(size, k)`; `Verify(Encode(x))` is true.
- **I-32** — Policy migration and drain never reduce a chunk's reachable shard count below
  its recorded `k`: old shards are deleted only after the new placement has been committed
  and applied.
- **I-67** — The chunk size is bounded once: `init`, `SetVolume` and the shard store all
  enforce `ChunkSize ≤ meta.MaxChunkSize` (64 MiB), so a k=1 shard of the largest chunk
  always fits `chunk.MaxPayload` and no shard is stored that cannot be read back.
- **I-75** — `AutoPolicy(nodes)` is a pure function of the Active target counts on voter and
  non-voter hosts and the host count (the table above, 14 §6), identical on every node; the leader proposes it
  only while `Vol.Auto` is set, when it differs from the current policy and `K+M` does not
  exceed the live Active targets; it grows after a join or promotion and shrinks only after
  a removal or demotion, never because of liveness.
- **I-77** — `PlaceKM(h, cands, k, m)` is a pure function of `(h, candidate set, k, m)`
  whose data slots are drawn from voter candidates whenever at least `k` voters are
  candidates, so a chunk is readable from live voters alone when `k ≤` live voters; with
  no voter candidate it equals `Place(h, cands, k+m)`.
- **I-33** — Weighted placement is proportional: over many hashes a candidate receives
  shards in proportion to its weight within ±10 %, and removing a candidate relocates only
  the shards it held.

## Tests

| Invariant | Tests |
|---|---|
| I-8 | `FuzzPlace_Determinism`, `TestPlace_DeterministicAcrossNodes`, `TestPlace_GoldenVectors`, `TestPlaceKM_VoterFirst_NNodeDeterminism` |
| I-9 | `TestCluster_WriteOnA_ReadOnB_ShardsOnDistinctNodes`, `TestExcludeIdx`, `TestFSM_NodeRemove_RefusedWhileHoldingSlots`, `TestModelCheck_Long`, `TestPlace_DistinctNodes` |
| I-10 | `FuzzRoundTrip`, `TestDecode_EverySubsetOfKShards`, `TestDecode_FewerThanK_Fails`, `TestEncodeDecode_Sizes`, `TestRoundTrip_Property`, `TestTailPadding_NotLeaked` |
| I-30 | `FuzzRoundTrip`, `TestEncodeDecode_Sizes`, `TestEncode_DoesNotAliasInput`, `TestRoundTrip_Property`, `TestShardSizesEqual_AndDeterministic`, `TestVerify_DetectsCorruption`, `TestVerify_EncodedIsConsistent` |
| I-32 | `TestRepair_DrainMovesAllSlotsBeforeRemove`, `TestRepair_MigrationCrash_Resumes`, `TestRepair_SetRedundancy_MigratesAllChunks_NoWindowBelowK` |
| I-33 | `TestPlace_CapacityWeighting`, `TestPlace_StabilityOnAdd`, `TestPlace_StabilityOnMembershipChange`, `TestPlace_StabilityOnRemove` |
| I-67 | `TestFSM_SetVolume_ChunkSizeBounds`, `TestStore_PayloadBoundaries` |
| I-75 | `TestAutoPolicy_Table`, `TestAutoPolicy_NNodeDeterminism`, `TestCluster_AutoPolicy_GrowsOnJoin_DefersWhileAsleep`, `TestCluster_AutoPolicy_ShrinksOnRemoveNotOnDeath`, `TestAutoPolicy_ManualPins`, `TestFSM_SetVolumeMode` |
| I-77 | `TestPlaceKM_VoterFirst_DataSlots`, `TestPlaceKM_VoterFirst_NNodeDeterminism`, `TestFillPlacement_KeepsSlotClass`, `TestCandidates_VoterBit`, `TestCluster_ReadableWithNonVotersAsleep` |
| I-117 | `TestFSM_SetVolume_ShardCountBoundaries`, `TestPolicyFlags_Bounds` |
