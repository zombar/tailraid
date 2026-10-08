<p align="center">
  <picture>
    <source media="(prefers-color-scheme: dark)" srcset="docs/assets/logo-dark.svg">
    <img src="docs/assets/logo-light.svg" alt="tailraid" width="300">
  </picture>
</p>

One shared volume across the devices on your tailnet, behaving like a modern RAID array:
every file is cut into chunks, each chunk is Reed-Solomon erasure-coded into `k` data and
`m` parity shards spread over different devices, so any `m` devices can be off, lost or
corrupted and the volume stays readable and writable; identical content is stored once; and
it mounts as a normal filesystem on macOS, Linux and Windows through the OS's own NFS
client. One static Go binary is the node daemon, the NFS gateway and the management CLI.
Metadata lives in a Raft-replicated log on your always-on devices; data never leaves your
tailnet.

**Status: the volume is built.** Single node, Raft cluster with join/leave/drain, erasure
coding with a redundancy policy you can change at any time, dedup, a self-fsyncing write
path, repair, scrub and garbage collection, a 600-step model checker and a nine-phase soak
suite that renders an HTML report, service install on all three OSes, verified
self-update, and release binaries. Validated on a real macOS NFS mount and on a four-node
docker-compose cluster. Still pending: the Windows mount path on real hardware and a
multi-device run on a real tailnet. The S3 gateway and S3 buckets as storage nodes are
designed (`docs/10`) and deferred. Design docs for every subsystem live in `docs/`.

> [!IMPORTANT]
> **Requirements & platform support.** Every device runs Tailscale (a headscale control
> server works: tailraid only shells out to the `tailscale` CLI for `status` and `whois`).
> Metadata needs an odd number of **always-on voters** — one is fine to start, three is
> the sensible shape; laptops join as non-voters and come and go. Chunks are `k+m` shards
> on distinct devices, so you need at least `k+m` live devices to write at full
> redundancy (writes degrade gracefully below that and are repaired later).
> - **macOS, Linux:** everything, mounted with the stock NFSv3 client (`soft`, `nolock`).
> - **Windows:** Pro/Enterprise/Education only (Home has no NFS client). The portmap
>   stub, mount helper and service installer are cross-compiled and unit-tested but
>   **not yet validated on hardware** — see `docs/windows-checklist.md`.
> - **Docker Desktop** (optional): runs the soak suite locally.

## Install

Grab a release binary for your OS/arch from the GitHub releases page (six static
binaries plus `sha256sums.txt`) and put it on your `PATH`, or build from source:

```sh
git clone https://github.com/zombar/tailraid.git
cd tailraid
make install        # build + install to ~/.local/bin/tailraid (INSTALL_DIR=… to change)
```

Requires **Go 1.27+**, `CGO_ENABLED=0`, no C toolchain. Later, the binary updates itself
from the releases (the repository is private, so give it a token):

```sh
tailraid update --check          # exit 2 when a newer release exists
tailraid update                  # download, verify sha256, run it, swap atomically
```

## Quickstart

On the always-on device that will hold the first metadata copy:

```sh
tailraid init --k 2 --m 1 --name studio --capacity 500G   # creates the volume; prints the join token
tailraid serve                                            # metadata, shard store, NFS gateway
tailraid mount                                            # ~/TailRAID (T: on Windows)
```

On every other device:

```sh
tailraid join 100.64.0.11 <token>        # --voter on other always-on boxes, --capacity 200G
tailraid serve
tailraid mount
```

Write on one device, read on any other. `tailraid status` shows the cluster; `tailraid
install` (below) keeps `serve` running after you log out.

`k` and `m` are the redundancy policy: `2+1` survives one lost device at 1.5× storage,
`3+2` survives two at 1.67×, `1+2` is three plain copies. Every chunk records its own
`(k,m)`, so `tailraid volume set-redundancy --k 3 --m 2` later is a background migration,
not a rebuild. Chunk size (default 1 MiB) is fixed at `init`.

## What a write does

```
NFS WRITE ──▶ gateway WAL (fsync) ──▶ ack
                 │ idle 2 s / 32 MiB dirty / 256 MiB total
                 ▼
          assemble chunks ─▶ sha256 ─▶ dedup? ─▶ RS encode k+m ─▶ PUT shards to k+m devices
                                                                        │ ≥ k acks
                                                                        ▼
                                                     Raft Commit {inode, chunk list, placement}
```

A WRITE is acknowledged once its bytes are fsynced to the issuing gateway's write-ahead
log; it is readable there immediately and from every other gateway once flushed, which
happens within about 2 s after writes to that file pause. Placement is a pure function
of the chunk hash and the live device set, stored with the chunk, so devices joining or
leaving never move data implicitly: only the repairer re-places shards, after a hold-down
(`repair.fill_after`, 10 min) so a laptop closing its lid does not trigger a rebuild.

**Durability contract** (the full text is `docs/05`): every acknowledged byte and every
metadata operation that returned OK is returned unchanged by any later read on any live
gateway under the loss of any `m` devices' disks, a gateway crash or power loss at any
instant, a leader crash or partition at any point, corruption of up to `m` shards per
chunk, and a crash at any fsync step of a shard write. Not covered: more than `m` devices
lost before repair completes, permanent loss of the Raft majority, storage that lies about
fsync, and the gateway's own disk dying inside the ≤ 2 s flush window.

**Degraded operation**, as the volume sees it:

| Condition | Metadata | Reads | Writes |
|---|---|---|---|
| leader reachable, live devices ≥ k+m | normal | normal | full placement |
| leader reachable, k ≤ live < k+m | normal | OK while k holders are live | degraded (≥ k shards), filled after `fill_after` |
| leader reachable, live < k | normal | EIO for affected chunks | flush waits in the WAL (up to 1 GiB), then EIO |
| no leader for > 30 s / no quorum | stale local copy | served | EROFS for metadata; WRITEs to open files still land in the WAL |

## Command tour

Every management command talks to the local `serve` over its tailnet address (`--node
<ip>` for another one) and takes `--json`. A week with tailraid looks like:

```sh
# the cluster
tailraid status                            # health, leader, voters, usage, warnings, join command
tailraid node ls                           # id, name, voter, live, slots, free, version
tailraid node describe honker              # probe history, slots held, raft lag
tailraid node promote honker               # non-voter → voter (always-on devices only)
tailraid node set-capacity skel 1T         # placement weight and shard budget

# replication health
tailraid check                             # degraded / unreadable chunks, last repair pass; exit 1 on issues
tailraid check --scrub --files             # verify every shard; list the files touching bad chunks
tailraid repair                            # run a repair pass on the leader now
tailraid chunk describe /photos/2024.zip   # every chunk: state, (k,m), refs, slots, shard HEADs, history
tailraid fsck                              # metadata invariants, orphan shards on this node

# changing the shape of the volume
tailraid volume describe                   # k+m, chunk size, logical vs stored bytes, dedup, headroom
tailraid volume set-redundancy --k 3 --m 2 # new chunks now; old ones migrate in the background
tailraid node drain oldbox                 # repairer moves its slots off …
tailraid node remove oldbox                # … then it leaves the cluster
tailraid gc                                # collect tombstoned chunks past the grace period

# proving it
tailraid verify --report out.html          # the soak scenarios against this cluster, HTML report
tailraid verify --destructive --restarts   # + corruption, kills and partitions (dev.enabled targets)

# running it
tailraid install                           # launchd / systemd / Windows SCM; --print shows the unit
tailraid logs -f                           # follow the service log (journald on Linux)
tailraid stop && tailraid start
tailraid update                            # verified self-update, then restart the service
```

## Configuration

`~/Library/Application Support/tailraid/config.yaml` on macOS,
`~/.config/tailraid/config.yaml` on Linux, `%ProgramData%\tailraid\config.yaml` on
Windows (`$TAILRAID_HOME` overrides the directory). `init` and `join` write it; every key
has a default:

```yaml
node:
  name: studio
  data_dir: …/tailraid/data        # raft log, shards, staging WAL, cache
  capacity: 500G                   # shard-byte budget and placement weight
  voter: true
listen:
  raft: 20480                      # tailnet TCP
  rpc: 20481                       # tailnet TCP, WhoIs-authorized
  nfs: 127.0.0.1:20490             # loopback only (2049 on Windows, plus the portmap stub on 111)
mount:
  path: ~/TailRAID                 # T: on Windows
repair:
  grace: 10m                       # tombstoned chunks are collected after this
  fill_after: 10m                  # a dead device's slots are re-placed after this
log:
  level: info                      # TAILRAID_LOG_LEVEL
  format: text                     # json | text, TAILRAID_LOG_FORMAT
  file: ""                         # append here instead of stderr, TAILRAID_LOG_FILE
dev:
  enabled: false                   # /dev/* endpoints and the chunk event ring: test rigs only
```

Logs are `log/slog` lines in one format for the whole process, including the Raft and
NFS libraries. There is no metrics stack; `tailraid status --json` is the machine-readable
view.

## Security model

Every node RPC is authorized by asking the local `tailscaled` **who** the source IP is
(`tailscale whois`) and checking that stable identity against the replicated member
table, before a request body is read. Raft connections carry a 16-byte cluster-ID
handshake so two clusters on one tailnet can never mix. Joining needs the join token plus a
WhoIs-verified identity. The NFS gateway listens on loopback only. Transport encryption is
WireGuard's; there is no encryption at rest. `docs/08` has the threat table.

## Soak suite & report

`make soak` builds the Linux binaries, brings up four nodes on a private Docker network
(three voters and one laptop-like non-voter, fake tailnet identities, 20 s repair timers),
and drives nine phases through worker sidecars: baseline transfer, cross-view integrity,
random-access churn, parity (lose `m`, then `m+1`), corruption (`m` shards, then `m+1`),
consensus (kill the leader under writes), membership (`set-redundancy` both ways, drain),
dedup and GC, and a restart storm. It writes `results.jsonl` and a self-contained
`report.html` with the phase × check matrix and charts drawn in Go: throughput with
event markers, repair backlog, stored vs logical bytes, slots per node, latency
histograms, and an FSM-hash agreement strip across nodes. `tailraid verify` runs the same
engine against your real cluster from any device's mount. The in-process variant runs in
22 s as a unit test.

## Windows (experimental)

`mount.exe` has no port option and finds NFS through the portmapper, so on Windows the
gateway binds `127.0.0.1:2049` and `serve` runs a tiny portmap v2 stub on `127.0.0.1:111`.
`tailraid mount T:` prints the three prerequisites when the NFS client is missing (enable
*Client for NFS*, switch it to TCP, set `AnonymousUid`/`AnonymousGid`). `tailraid install`
registers an SCM service that depends on Tailscale and restarts on failure; `serve
--service` speaks the service-control protocol. All of it awaits a pass through
`docs/windows-checklist.md` on a real Pro machine.

## Hacking

Docs first, tests first. Every component starts as a section in `docs/` that ends with
numbered invariants (`I-n`), and every event in the life of a chunk is a cell (`L-n`) in
the transition tables of `docs/11`. A test cites what it proves (`// Invariant: I-12`,
`// Lifecycle: L-178`) and `scripts/check-invariants.sh` fails the build if any ID has no
test, any citation is undefined, or the generated `docs/invariants.md` and
`docs/lifecycle.md` are stale: 57 invariants, 178 cells. No production code path skips
fsync and there is no test-mode switch; speed comes from in-memory stores.

```sh
make check          # gofmt, lint, invariant check, go test -short -race  (≈ 2 min)
make test-long      # 600-step model checker, long durability runs, oracles
make test-mount     # real NFS mount workload (sudo -E)
make soak           # the compose suite → report.html
make release        # six static binaries + sha256sums
make tag VERSION=v0.2.0   # green check, annotated tag; CI publishes the release
```

The model checker (`test/cluster/modelcheck_test.go`) drives a five-node 3+2 cluster
through random writes, truncates, renames, kills, restarts, partitions, clock jumps,
policy changes and drains, and after every quiescence compares cold reads on every gateway
with a model that keeps every candidate outcome of a failed metadata operation (a proposal
that returned EROFS may still commit after a partition heals). It found the two bugs that
reached `main` so far: a stale overlay base that zeroed a committed chunk, and a flusher
that waited forever on a node that was shutting down.

`docs/README.md` is the reading order; `CLAUDE.md` is how the code is written. The
previous attempt, [tunnelmesh](https://github.com/tunnelmesh/tunnelmesh), informed this
design with its useful parts and this test suite with its bugs.
