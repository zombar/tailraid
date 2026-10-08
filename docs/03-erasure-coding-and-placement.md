# 03 — Erasure coding and placement

Each chunk is one stripe: `k` data shards and `m` parity shards computed with Reed-Solomon
over GF(2^8) (`klauspost/reedsolomon`). Shards are placed on `k+m` distinct nodes chosen by
weighted rendezvous hashing over the live, active candidate set at write time, and the
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
type Candidate struct { Idx meta.NodeIdx; Weight uint64 }   // Weight = Node.Capacity
```

At write time the flusher builds the candidate list from `State.Nodes` filtered to
`State == Active` **and** currently live (`Peers.Live()`), with `Weight = Capacity`. Nodes
that are `Draining` or `Removed`, dead, or that reported `ErrFull` (HTTP 507) on their last
PUT are excluded. S3 nodes (`Kind == KindS3`) are candidates when their bucket probe is
live. The candidate list is built by the gateway doing the flush from its *own* prober
view; no node is asked for permission.

### Rendezvous (HRW) hashing

```go
func Place(h meta.Hash, cands []Candidate, n int) []meta.NodeIdx
```

For each candidate compute
`u = fmix64(fnv64a(h[:] ‖ u16le(idx))) / 2^64` mapped into the open interval `(0, 1)` (a
result of 0 or 1 is nudged by one ulp), and `score = -Weight / ln(u)` (weighted rendezvous
hashing; a node with twice the weight receives twice the shards in expectation). `fmix64` is
the MurmurHash3 64-bit finalizer; it is required because plain FNV-1a has no avalanche over
the two trailing `idx` bytes, so the `u` values of different candidates for the same hash
were nearly equal and the weights were effectively ignored (a 2:1 weighting measured
99984:16 before the mix). Sort candidates by `score` descending, ties broken by `idx` ascending, and take
the first `n`. Shard `i` goes to the `i`-th selected node. Properties:

- Pure function of `(h, candidate set, n)` (I-8): every node computes the same answer from
  the same inputs, which the predecessor got wrong by sorting its own name among peer IPs.
- Minimal disruption: adding a candidate moves only the shards that now hash highest onto
  it; removing one moves only that node's shards.
- With fewer than `n` candidates the result is shorter (`TestPlace_FewerCandidatesThanN`);
  the caller fills the remaining slots with 0 (degraded) or refuses, per the rules below.

The placement is **stored** in `Chunk.Placement` by `Commit`/`SetPlacement`. Membership or
liveness changes never move data implicitly; only the repairer, through `SetPlacement`,
changes where a shard lives. Reads therefore never guess.

### Slot semantics

`Placement[i]` is the `NodeIdx` holding shard `i`, or `0` when no node holds it. A non-zero
entry pointing at a `Removed` or `Draining` node is treated by the repairer as work to do
but is still a valid read source while the node is live. Non-zero entries within one chunk
are always distinct (I-9): the same node never holds two shards of one stripe, since that
would make a single node loss cost two shards.

## Writing a chunk

The flusher ([04](04-write-path-and-durability.md)) for a new chunk hash:

1. Choose `(k, m)`; `placement = Place(h, candidates, k+m)`; `shards = Encode(chunk)`.
2. PUT every slot in parallel (self via the local store). Each PUT carries
   `X-Shard-SHA256`; the receiver verifies before the shard becomes visible.
3. Wait until **at least `k` slots are acknowledged** and either all PUTs finished or a
   10 s deadline passed. Failed or late slots: retry once with `Place` over candidates minus
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
`Rank(live non-zero placement)` (dead holders are never dialled, L-131; slots are offered
in placement order so data shards come before parity at equal rank, L-63); fetch the
first `k` in parallel; on any failure or `ErrCorrupt`
pull the next slot until `k` good shards are in hand; `Decode(shards, c.Size)`; verify
`sha256 == h` (a mismatch is `EIO` and is logged with the slot list for scrub); cache.
Fewer than `k` reachable shards ⇒ `EIO` for that chunk only.

S3 nodes rank last so that bulk reads never touch a bucket while enough device shards are
reachable; a bucket is the shard source of last resort and the off-site copy.

## Changing the policy: `volume set-redundancy --k K --m M`

Proposes `SetVolume{K, M}`; refused by the CLI if `K+M` exceeds the number of **live
Active nodes** (devices or S3). The FSM increments `PolicyVersion` itself. From the next flush, new chunks use the new `(K, M)`. Existing
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

- **I-8** — `Place(h, cands, n)` is a pure function: for the same hash, the same candidate
  set (any order) and the same `n`, every node returns the identical sequence.
- **I-9** — The non-zero entries of any committed `Placement` are pairwise distinct node
  indices; a stripe never has two shards on one node. With `≥ k+m` candidates all `k+m`
  slots are non-zero.
- **I-10** — For any chunk and any subset of `k` of its `k+m` shards,
  `Decode(subset, Size)` returns exactly the original `Size` bytes; with fewer than `k`
  shards `Decode` fails rather than returning data.
- **I-30** — `Encode` is deterministic and never aliases its input; all `k+m` shards have
  length `ShardLen(size, k)`; `Verify(Encode(x))` is true.
- **I-32** — Policy migration and drain never reduce a chunk's reachable shard count below
  its recorded `k`: old shards are deleted only after the new placement has been committed
  and applied.
- **I-33** — Weighted placement is proportional: over many hashes a candidate receives
  shards in proportion to its weight within ±10 %, and removing a candidate relocates only
  the shards it held.

## Tests

| Invariant | Tests |
|---|---|
| I-8 | `TestPlace_DeterministicAcrossNodes`, `TestPlace_GoldenVectors`, `FuzzPlace_Determinism`, `TestCluster_PlacementAgreesOnAllNodes` |
| I-9 | `TestPlace_DistinctNodes`, `TestFSM_Commit_RejectsDuplicatePlacement`, `TestCluster_WriteReadAllNodes`, `TestModelCheck_Long` |
| I-10 | `TestDecode_EverySubsetOfKShards`, `TestDecode_FewerThanK_Fails`, `TestEncodeDecode_Sizes`, `TestTailPadding_NotLeaked`, `FuzzRoundTrip`, `TestCluster_KillMNodes_AllDataReadable` |
| I-30 | `TestShardSizesEqual_AndDeterministic`, `TestEncode_DoesNotAliasInput`, `TestVerify_EncodedIsConsistent` |
| I-32 | `TestRepair_SetRedundancy_MigratesAllChunks_NoWindowBelowK`, `TestRepair_DrainMovesAllSlotsBeforeRemove`, `TestRepair_DeletesOldShardsOnlyAfterCommit` |
| I-33 | `TestPlace_CapacityWeighting`, `TestPlace_StabilityOnRemove` |
