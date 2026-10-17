# Taking tailraid public

Written 2026-10-14. The plan for publishing the work: a 1.0 release with a launch post,
an experience article, and one workshop paper built around a single measured claim. The
system is further along than most published home-storage work, and `docs/reports/` holds
most of an evaluation section already, so the cost is writing, not engineering.

Decision of record: Tailscale is dropped (the mesh plan, docs/20). The launch waits for
the mesh stack to merge and for a rename, because launching as "tailraid, for your
tailnet" and renaming a quarter later would waste the one launch. The new name is open;
`<name>` stands for it below.

## 1. The order of moves

1. **Launch post** with the 1.0 release. Brings users and incident reports.
2. **Experience article** on the five real-cluster incidents. Its material is what a
   paper's motivation section wants.
3. **arXiv report**: the full design and evaluation, citable and dated, establishing
   priority on the placement idea.
4. **Workshop paper**: the report cut to six pages around the home-placement claim.

Each step reuses the one before it. The launch post and the arXiv report are one draft;
the post is its first third, the paper is the report cut down. Write once.

The one claim worth a paper is placement: every chunk has a home that is a pure function
of its hash and the membership (I-294), migration is gated by a movable-bytes threshold
(I-295), and the system was measured on a real fleet of laptops, a NAS, a Windows box and
a cloud bucket over Wi-Fi and WAN. Nothing else in the codebase is new enough on its own.

## 2. What we have to show

| Asset | Where | What it proves |
|---|---|---|
| Design of record, 20 documents on main, 21 with the mesh PR | `docs/00`–`docs/20` | Every component was specified before it was coded; goals and non-goals are explicit |
| 306 invariants and 220 lifecycle cells | `invariants.md`, `lifecycle.md` | Each has a citing test; the build fails when the mapping breaks |
| 24 soak phases on main, 27 with the mesh stack | `internal/soak/phases.go` | Faults, partitions, restarts, corruption, churn, multi-user races and the mesh, on a Docker rig and on the live cluster through `tailraid verify` |
| Three-OS VM rounds | `reports/vms/` | macOS, Linux and Windows guests each initiating a cluster, destructive verify per round |
| Open-media bench | `reports/bench-2026-10-open-media.txt` | Time to first byte of cold and warm reads on the real network, before and after readahead and cache changes |
| Hot-path micro-benchmarks | `reports/bench-2026-10-hotpath*.txt` | FSM commit, lookup, readdir and placement costs before and after the hot-path work |
| RPC transport study | `reports/rpc-transports-2026-10.md` | HTTP/1.1 against HTTP/2, gRPC, yamux and QUIC under modelled LAN, mesh and lossy links |
| Incident write-ups | `CLAUDE.md` status, this section | Five real-cluster failures found and fixed in the field |
| Production code | `internal/`, `cmd/` | About 24k lines of Go plus 5.8k of TypeScript, no CGO, vendored, one static binary |

The incidents are the strongest material, because a reader cannot get them from a design
paper. Each was found on a volume of about 720 GB across six devices and a cloud bucket,
and each produced invariants and a soak phase.

| Incident | What happened | Fix |
|---|---|---|
| Zero-filled copies | Explorer copies over SMB left files of holes. Four causes stacked: a mid-copy restart, a lagging codec view failing writes at once, the protected flag refusing Explorer's own cleanup, synchronous WRITE handling under load | I-217 to I-221, soak phase 22 |
| Repair that never converged | 98k chunks stayed pending because the voter preference outranked the host cap | I-173 to I-176, phase 18 |
| Idle churn | An idle volume moved about 30 MB/s because each scrub pass took 1 % of the chunks a minute after the last | I-215 |
| Slow first byte | The head of a video cost 0.7 s because four prefetches started alongside it | I-203 to I-205, phase 17 |
| Two clients in one name | Research for the first incident found two writers could truncate each other | I-222 to I-226, phase 23 |

Not yet in hand: a comparison against any other system, a run against a real S3 provider,
the phone and Windows-hardware checks the status notes list as pending, and the mesh
stack's own pending items (the NAT rig's mapping round, VM rounds, the bench against the
old baseline).

## 3. Positioning

**One sentence.** `<name>` turns the devices you already own into one erasure-coded,
deduplicated volume that mounts as an ordinary folder, joined by a code, with no server to
run and nothing leaving your own WireGuard mesh.

**Who it is for.** A person or household with several machines and a pile of media, who
wants RAID-grade durability without buying a NAS or trusting a cloud with the plaintext.
The secondary audience is the self-hosting community, which is where the first users and
bug reports come from.

**What it is not.** Not a sync tool (one volume, not copies), not a backup product
(snapshots live on mirror disks you plug in), not a cluster filesystem for a datacentre
(it expects laptops that sleep and Wi-Fi that drops), and not an S3 service (buckets are
storage targets, not an API).

**Against the neighbours.** The row no neighbour fills is erasure coding across
intermittently connected consumer devices, a native OS mount, its own mesh, and placement
that tolerates sleep and churn.

| System | Model | Redundancy | Mount | Devices that sleep |
|---|---|---|---|---|
| `<name>` | one volume, Raft metadata, content-addressed chunks, own WireGuard mesh | Reed-Solomon k+m across hosts and disks, automatic policy | NFSv3 and SMB2 by the OS client, no driver | yes: dead, suspect and live states, fill-after refill, home placement |
| Syncthing | n-way sync of folders | a full copy per device | native folder | yes, but every device holds everything |
| Tahoe-LAFS | capability-addressed grid | Reed-Solomon across storage servers | FUSE or web gateway | partly: servers expected online, no OS mount |
| Garage | S3 object store | 3-way replication across zones | S3 only | no: a cluster of servers |
| SeaweedFS, MinIO | object and file stores | erasure coding across servers | FUSE, S3, NFS gateway | no: datacentre deployment |
| Perkeep | personal content-addressed store | per-blobserver replication | web UI, FUSE | partly: personal, but no erasure coding or OS mount |
| Storj | paid decentralised storage | Reed-Solomon across strangers' nodes | S3 and CLI | yes, but the data leaves your network for a fee |
| Ceph | datacentre cluster | CRUSH placement, replication or EC | CephFS kernel client | no |

The launch line follows from the table: "the erasure coding of Ceph, on the devices
Syncthing runs on, mounted like a USB drive."

## 4. Venues

USENIX ;login: concluded publication in 2025, so the experience article goes to a guest
piece or our own long-form post. Every 2026 academic deadline has passed and the 2027
calls are not out; their dates below follow the 2026 pattern and are marked expected.

| Venue | Fits | Deadline | Length | Review | Effort | Verdict |
|---|---|---|---|---|---|---|
| Launch post; Hacker News, Lobsters, r/selfhosted, r/DataHoarder | the product | any time after 1.0 | 1,500 words | none | 1 week | first move |
| arXiv, cs.DC | the full design and evaluation as a technical report | any time | 10 to 15 pages | none, but citable and dated | 3 weeks | second move |
| LWN guest article | the experience piece: five incidents and what the model checker missed | rolling, pitch first | 2,500 words | editor | 1 week | replaces ;login: |
| HotStorage '27 (co-located with SOSP; 2026 closed June 12) | position and early-results papers; the home-placement claim | expected June 2027 | short | peer, workshop | 6 weeks on top of the report | the paper target |
| SYSTOR 2027 (2026 closed May 21; short papers 5 pages; operational experience welcome; virtual) | the same manuscript | expected May 2027 | 5 or 12 pages | peer | same material | fallback |
| FAST '28, Deployed System category (FAST '27 closed March 17 and September 15, 2026; 6-page short papers; artifact evaluation) | real operational systems | expected March 2027 | 6 or 12 pages | peer, top tier | 3 months, needs the comparison figures | stretch, only with the headline chart in hand |

Recommendation: the first three in that order, then HotStorage or SYSTOR with one
manuscript. FAST is realistic only if the headline chart in §5 shows a clear win, and a
FAST submission is double-blind, so it would mean holding the arXiv report until the
decision.

## 5. The paper

**Research question.** Can a deterministic home for every chunk, computed from its hash
and the membership alone, keep an erasure-coded volume balanced across a fleet of consumer
devices that sleep, roam and get replaced, while moving fewer bytes than a liveness-driven
placer?

The claim has a clean shape. Ceph's CRUSH and consistent hashing give determinism but
assume servers that stay up. Liveness-driven placers, which is what tailraid was before
the home rule, react to every absence and churn data. The home rule separates the two:
placement is pure, liveness only decides what to serve from and when to refill, and
migration is gated so a device that sleeps nightly costs nothing.

**Working title.** "A home for every chunk: erasure-coded storage across devices that
sleep."

**Outline, six pages.**

1. The setting: a household fleet, Wi-Fi and WAN, devices that sleep, a cloud bucket as
   a last resort. Why RAID semantics are wanted here and why nobody offers them.
2. The design in one page: content-addressed chunks, Reed-Solomon over hosts and disks,
   Raft for metadata, native mounts, the mesh. Only what the placement claim needs.
3. Home placement: the pure function, the host cap, the away classes, the gate, the
   refill-at-home rule.
4. Evaluation: balance and bytes moved under a replayed membership trace; the
   repair-convergence incident as the before case; read tail latency on the real network
   with the suspect and straggler rules.
5. Experience: what the field found that the model checker did not, in a page.
6. Related work and what is left out.

**Figures we have.** The disk-spread chart from the soak report; the time-to-first-byte
table from the open-media bench, cold and warm, local and one hop; repair fan-out and
foreground throughput from phase 18; the transport study's throughput by link profile.

**Figures we need.**

- Bytes moved per membership event, the home rule against the pre-home placer, on the
  same trace. This is the headline chart and does not exist yet.
- Balance over time as devices join, sleep and leave, from a replayed trace of the real
  cluster's member table.
- Read tail latency with and without suspect marking, measured on the real network.

The first two need a trace replayer over the in-process harness, about a week of work in
`test/cluster`. The third is a bench script against the live volume.

## 6. Evaluation plan

The soak suite and the bench files map onto an evaluation section almost one to one. What
a reviewer will miss is a baseline and a workload description.

| Evaluation section | Source | Status |
|---|---|---|
| Durability under faults: parity loss, corruption, leader kill, partition, restart storm | phases 4, 5, 6, 9, 10 | exists; re-run at 1.0 on the Docker rig and the live cluster |
| Membership churn and repair convergence | phases 7, 13, 14, 18 | exists; add the pre-home placer as the before case |
| Balance and bytes moved | phase 13's disk-spread chart, `status.repair.home` | needs the trace replayer |
| Read latency, cold and warm, local and one hop | the open-media bench, phase 17 | exists; re-run at 1.0 |
| Write path and group commit | phase 12, the hot-path bench | exists |
| Multi-user correctness | phases 22, 23 | exists; the nine race rounds make a good table |
| Transport choice | the transport study | exists; one paragraph and one chart |
| The mesh: join, paths, NAT | phases 25, 26, 27 | exists on the rigs; the real-network bench against the Tailscale baseline is pending |
| Portability | the three-OS VM rounds | exists; the Linux round must pass again on the mesh |
| Workload description | none | a paragraph: file-size histogram and dedup ratio of the real volume from `volume describe` |

Rules for the re-run so the numbers survive review: one tagged version on every node,
printed in each report header (the open-media bench mixed v0.5.0 with a copy-back still
running); each live measurement five times, median and spread; say what the links are
(the node on the 2.4 GHz Wi-Fi dongle is a feature of the evaluation, not a footnote); keep
the raw output beside the prose in `reports/` and link it from the artifact appendix.

## 7. Marketing and release

The launch is a 1.0 tag, a signed release built by CI, a landing page and one post. The
README already reads as a landing page, so the page is the README rendered by GitHub
Pages with the lookdev screenshots added.

| Step | Detail |
|---|---|
| Merge the mesh stack | PRs #250 to #263, then `make soak-nat`, the VM rounds and the bench against the old baseline |
| Rename | One PR: module path, binary, docs, lookdev, the README's logo. The old name stays as a redirect on GitHub |
| Close the pending list | A release signed by CI (the key exists, the CI secret does not), `install` on each OS with the confined unit, `recover` once on real hardware, the Windows checklist on 24H2, a phone run of the web UI, a run against a real S3 provider |
| Demo video, 90 seconds | Three machines, `init` and two `join <code>`s, a folder appears on each, copy a film in, pull a disk, the film still plays, `status` shows the refill. Recorded on the VM rig with the dashboard open |
| Launch post, 1,500 words | The problem (a household's disks and a cloud bill), the one sentence, the demo, the design in five paragraphs, the honest limits (beta, one maintainer, the volume was re-created during development), the ask (try it, report the first thing that breaks). Link the design docs, not the code |
| Install story per OS | A Homebrew tap, a `.deb` and the static binary, a signed `.exe` with the `net use` step. `install` covers the service; the download is the gap |
| Repository hygiene | Issue templates asking for `status --json` and the log, `SECURITY.md`, `CONTRIBUTING.md` pointing at docs-first and the invariant rule, Discussions on |
| Where to post | Show HN on a Tuesday to Thursday morning US time, Lobsters, r/selfhosted, r/DataHoarder, Mastodon. One day, then answer every comment for 48 hours |

Leave the Media tab, search and the Stats page out of the launch. They dilute the pitch;
a second post a month later can cover the browser side.

## 8. Risks and disclosures

| Risk | Why it matters | What to do |
|---|---|---|
| AI-assisted authorship | Most of the code was written with Claude. ACM and USENIX require disclosure of generative AI use, and a reviewer who finds out later discounts the work | State it in the acknowledgements of every paper and in the launch post: the design decisions, the invariants and the field work are the author's, the code was pair-written with an AI, and the test suite is what makes that defensible. Lead with it |
| Novelty | Every primitive is known; a reviewer will cite Tahoe-LAFS, Ceph and Garage | Claim only the home-placement result and the field measurements, never the system |
| The name | The current name ties the project to a dependency it no longer has | Rename in its own PR before the tag |
| Licence | AGPL-3.0 keeps a hosted fork honest and deters some corporate users; for a self-hosted tool that is the right trade | Keep it; say why in the README |
| Support load | One maintainer and a storage system people will trust with their only copy. The first data-loss report arrives within a month of a successful launch | Label it beta. Put the durability contract and the "not a backup" line above the install section. Ship snapshots and mirrors as the answer |
| Numbers that will not reproduce | The live measurements depend on one Wi-Fi dongle and one household | Publish the Docker rig results as the reproducible set and the live ones as the field set, labelled |
| Anonymity | HotStorage and FAST are double-blind; the repo, the README and an arXiv report name the author | arXiv first forfeits FAST in practice: either skip FAST or hold the report |
| Time | Four moves alongside the mesh and the pending list | One draft for the post and the report; the paper is the report cut down |

## 9. Timeline

Dates from 2026-10-14, assuming the mesh stack merges within two weeks. The paper is not
on it: HotStorage '27 and SYSTOR 2027 are expected to close in May and June 2027, and the
manuscript is the arXiv report cut to six pages once the headline chart exists.

| When | What |
|---|---|
| by 2026-10-28 | Mesh stack merged, NAT rig and VM rounds green, bench against the old baseline in `reports/` |
| by 2026-10-30 | Name chosen; rename PR merged; CI secret for release signing added and a signed test release cut |
| 2026-10-19 to 2026-11-13 | Close the pending list (§7) |
| 2026-11-02 to 2026-11-20 | Trace replayer in `test/cluster`; replay the real member table; chart bytes moved for the home rule against the pre-home placer |
| 2026-11-04 to 2026-11-20 | Launch post and arXiv report as one draft |
| 2026-11-16 to 2026-11-20 | Demo video on the VM rig |
| 2026-11-24 | `make soak` and `tailraid verify` at the 1.0 candidate, one version on every node; tag 1.0 |
| 2026-12-01 | Launch: page up, Show HN and the rest, 48 hours of answering |
| 2026-12-02 | Pitch LWN with the five incidents |
| by 2026-12-23 | arXiv report submitted, with the AI-assistance acknowledgement |
| early 2027 | Watch for the HotStorage '27 and SYSTOR 2027 calls; pick one |
