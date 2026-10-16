# Changelog

Generated from the commit log by `scripts/changelog.sh`; do not edit by hand.

## v0.10.3 (2026-10-16)

### Changes

- cluster, vfs, repair: a host whose PUT is cut at the straggler grace is held slow, 1 to 10 min, and takes no new shards (I-371, L-222) ([786db72](https://github.com/zombar/tailraid/commit/786db72))
- pins, vfs: every node keeps /.tailraid's thumbnails, index and stats in its cache; the stats and search readers consult it (I-373) ([b605b38](https://github.com/zombar/tailraid/commit/b605b38))
- webui: a thumbnail the node must make is made in the background, and the page asks for it again later (I-374) ([655c593](https://github.com/zombar/tailraid/commit/655c593))
- restore, vfs, rpc: a running restore job narrows to one file and two chunks while clients are busy (I-372) ([33c1fe2](https://github.com/zombar/tailraid/commit/33c1fe2))
- nfs: WRITE and COMMIT hold at most 8 of a connection's 16 slots and queue in order, so reads go on during a copy (I-370) ([0321b73](https://github.com/zombar/tailraid/commit/0321b73))
- cluster, rpc: a suspect host keeps its mirrors online, and the scrape's plan is a pass op (I-188, I-189) ([64a9f75](https://github.com/zombar/tailraid/commit/64a9f75))

## v0.10.2 (2026-10-16)

### Changes

- soak: phase 32 restore jobs, its report section, and the runner in internal/restore so the harness runs it (I-363..I-368) ([025d71b](https://github.com/zombar/tailraid/commit/025d71b))
- webui: a folder restore is a job, the dashboard's Restores block, the Snapshots pager (I-369) ([bffeb73](https://github.com/zombar/tailraid/commit/bffeb73))
- node, rpc, cli: a tree restore is a job on the mirror's host, resumed, queued, cancellable, in status (I-363..I-368) ([f0d3b0d](https://github.com/zombar/tailraid/commit/f0d3b0d))
- webui: a target's expected share caps at the live chunks, not live minus tombstoned ([e600fe1](https://github.com/zombar/tailraid/commit/e600fe1))
- vfs: gofmt the restore tests' gate ([d1383f9](https://github.com/zombar/tailraid/commit/d1383f9))
- vfs: the restore tests' gate closes its channels once, however often the reads in flight reach the count ([0d740a6](https://github.com/zombar/tailraid/commit/0d740a6))
- vfs: a tree restore writes four files and eight chunks at once, reports its progress and counts its totals first (I-365, I-366, I-368) ([5f7ed1e](https://github.com/zombar/tailraid/commit/5f7ed1e))
- lookdev: a restore card's Cancel sits in its bottom right corner, level across a row ([038d812](https://github.com/zombar/tailraid/commit/038d812))
- docs, lookdev: restore jobs (docs/16 §6), the dashboard's Restores block and the Snapshots pager, I-363..I-369, stubs ([d2d1549](https://github.com/zombar/tailraid/commit/d2d1549))

## v0.10.1 (2026-10-16)

### Changes

- soak: the users-first harness run takes the writing phases under -short (I-362) ([b4833d4](https://github.com/zombar/tailraid/commit/b4833d4))
- soak: users first, so every phase runs with the FSM checking actors (I-362) ([76b19d2](https://github.com/zombar/tailraid/commit/76b19d2))
- webui, smbgw, vfs, cmd: the writers that broke once users existed act as their user (I-81, I-229, I-299, I-352) ([c7995fc](https://github.com/zombar/tailraid/commit/c7995fc))
- node, repair: restores write as the signer, no stamp before the first user, every admin op run as an admin under users (I-348, I-349) ([d606f8e](https://github.com/zombar/tailraid/commit/d606f8e))
- mount: the macOS mount sets timeo=600,retrans=2, so a WRITE waiting at a cap is never timed out by the client (I-361) ([021ab84](https://github.com/zombar/tailraid/commit/021ab84))
- node, repair: trash restore and purge propose as the signer, the retention sweep as System (I-349) ([48aec08](https://github.com/zombar/tailraid/commit/48aec08))
- users: several identities per OS account, user use and --identity (I-360) ([cbe33f6](https://github.com/zombar/tailraid/commit/cbe33f6))
- vfs: a restored file lands under a temporary name and is renamed into place, so a rerun finishes an interrupted restore (I-186) ([deddd97](https://github.com/zombar/tailraid/commit/deddd97))

### Documentation

- docs: a users guide, linked from the README; system secrets need --system ([cf51ebf](https://github.com/zombar/tailraid/commit/cf51ebf))
- docs: the move to 0.10 needs 0.10.0, which carries the restore fix ([f7ff891](https://github.com/zombar/tailraid/commit/f7ff891))

## v0.10.0 (2026-10-15)

### Changes

- docs, rpc: the move to 0.10 per OS; a restore from a mirror waits as a pass (I-189) ([2ce5b06](https://github.com/zombar/tailraid/commit/2ce5b06))
- vms: the Windows image keeps C:\tailraid on the machine Path after the Tailscale MSI ([549c15c](https://github.com/zombar/tailraid/commit/549c15c))
- CLAUDE.md: tailraid on Tailscale again, the mesh withdrawn in 0.10.0, users kept ([2a70a00](https://github.com/zombar/tailraid/commit/2a70a00))
- meta: the users state is snapshot frame 15; 0.9.x's frames 13 and 14 are refused by name (I-349) ([47b1dac](https://github.com/zombar/tailraid/commit/47b1dac))
- soak: a live run sets its secret through a node it can kill, and judges a live kill by the leader's rows ([82979b6](https://github.com/zombar/tailraid/commit/82979b6))
- dev: /dev/fsm-hash labels a hash with its snapshot's index (I-4) ([472e738](https://github.com/zombar/tailraid/commit/472e738))
- vms: the clean slate sets every guest's clock from the host when it drifted ([b0dde0b](https://github.com/zombar/tailraid/commit/b0dde0b))
- vms: the Windows data check writes its small file, and a failed write fails the step ([9bca5c4](https://github.com/zombar/tailraid/commit/9bca5c4))
- smbgw: the admin reads a file under another user's Read grant (I-352) ([b4a631f](https://github.com/zombar/tailraid/commit/b4a631f))
- vfs, nfsgw: ACCESS grants READ on a directory on the way to a role (I-351) ([905453d](https://github.com/zombar/tailraid/commit/905453d))
- soak: phase 29 compares the root listing as a set of names ([3ed7878](https://github.com/zombar/tailraid/commit/3ed7878))
- make: the soak target changes into test/soak once ([b33f023](https://github.com/zombar/tailraid/commit/b33f023))
- users, webui: a sign-in code seals its token, and the session cookie is SameSite=Lax (I-354) ([fe9d59d](https://github.com/zombar/tailraid/commit/fe9d59d))
- smbgw, nfsgw, vfs, webui: a Read-only user's handle, ACCESS by role, and nothing told before the role is checked (I-351, I-352, I-353, I-355) ([7b130f6](https://github.com/zombar/tailraid/commit/7b130f6))
- node: the status report's ready bit is read live, not from the refresher's cached build (I-125) ([029d7a2](https://github.com/zombar/tailraid/commit/029d7a2))
- meta, vfs: every Commit needs Write, and a refused Commit drops the inode's dirty data (I-349, I-353) ([d4130e7](https://github.com/zombar/tailraid/commit/d4130e7))
- vfs, webui: snapshot reverts, restores, the whole trash and snapshot creation check the session's role (I-348, I-355) ([e63c950](https://github.com/zombar/tailraid/commit/e63c950))
- meta, rpc: the trash sweep, a restore's two parents and the self-only member ops check their actor (I-349, I-345, I-322) ([5f72670](https://github.com/zombar/tailraid/commit/5f72670))
- node: a re-added disk opens under its active row, and only a refused invite code throttles its source (I-90, I-343) ([cab1b25](https://github.com/zombar/tailraid/commit/cab1b25))
- audit: Record waits for no flush; the leader's apply hook no longer stalls Raft (I-333) ([7fb2328](https://github.com/zombar/tailraid/commit/7fb2328))
- soak: phase 29 probes the gateways once the grants reached their replicas ([25d9714](https://github.com/zombar/tailraid/commit/25d9714))
- vfs: a READ within ReadAhead chunks of a stream's frontier stays in the sequence (I-203) ([5d83545](https://github.com/zombar/tailraid/commit/5d83545))
- cluster: export falls back to the newest stored snapshot when Raft has nothing new (I-129) ([21129d1](https://github.com/zombar/tailraid/commit/21129d1))
- repo: the built tailraid binary leaves the tree; the ignore file names every build output ([d959ab6](https://github.com/zombar/tailraid/commit/d959ab6))
- soak: phases 28–31, users and keys, grants, secrets per user, audit and sessions ([766fde4](https://github.com/zombar/tailraid/commit/766fde4))
- audit: the chained audit log, its records, status.users and check's warnings (I-357, I-358) ([9a7b21b](https://github.com/zombar/tailraid/commit/9a7b21b))
- web: sign in to the volume, sessions on every node and routes by role (I-354, I-355) ([087ad37](https://github.com/zombar/tailraid/commit/087ad37))
- gateways: the actor of every mount operation (I-351..I-353) ([51c36dc](https://github.com/zombar/tailraid/commit/51c36dc))
- users: signed requests, the needs table, the CLI and personal secrets (I-343..I-345, I-350, I-359) ([d5d531c](https://github.com/zombar/tailraid/commit/d5d531c))
- cmd: the paths test hands init the token as the other CLI tests do ([fbec6ec](https://github.com/zombar/tailraid/commit/fbec6ec))
- meta: users, groups, grants, invites and sessions in the FSM (codec 14, I-342..I-349) ([637c960](https://github.com/zombar/tailraid/commit/637c960))
- docs, lookdev: users, groups and permissions (docs/21, I-342..I-359) ([01b71fd](https://github.com/zombar/tailraid/commit/01b71fd))
- soak: check 23.5 reads before its first poll of the copy ([61a128d](https://github.com/zombar/tailraid/commit/61a128d))
- admin: the Linux encryption table runs on Linux only ([60fa06a](https://github.com/zombar/tailraid/commit/60fa06a))
- security: files on disk are 0700 and status checks their encryption (I-340, I-341) ([3eefa15](https://github.com/zombar/tailraid/commit/3eefa15))
- rpc: the drain test waits for its connection instead of racing the pool ([6452ec3](https://github.com/zombar/tailraid/commit/6452ec3))
- secrets: one key make at a time per node, the fetch outside the store's lock (I-304, I-305) ([13fb12d](https://github.com/zombar/tailraid/commit/13fb12d))
- secrets: the leader makes the cluster key on its first tick (I-303, I-305) ([880aaa4](https://github.com/zombar/tailraid/commit/880aaa4))
- cmd: the trash and cache CLI tests wait for the node as the others do ([8235816](https://github.com/zombar/tailraid/commit/8235816))

### Documentation

- docs: tailraid runs on Tailscale again; the mesh of 0.9.x is withdrawn, users stay ([b19ab51](https://github.com/zombar/tailraid/commit/b19ab51))
- docs: regenerate the invariants table ([421f6ee](https://github.com/zombar/tailraid/commit/421f6ee))
- docs: a person signs in to the volume, not a node (sessions in the FSM, op 32) ([5697bea](https://github.com/zombar/tailraid/commit/5697bea))
- docs: the README is a quick start; the guides move to docs/guide-*.md ([33dcd1f](https://github.com/zombar/tailraid/commit/33dcd1f))
- docs: regenerate the invariants table for the key-make tests ([59b7dd4](https://github.com/zombar/tailraid/commit/59b7dd4))

### Tooling

- ci: one make tag releases (restore #321, discarded by the rollback merge) ([75dd316](https://github.com/zombar/tailraid/commit/75dd316))
- ci: one make tag releases ([bf93468](https://github.com/zombar/tailraid/commit/bf93468))

## v0.9.1 (2026-10-15)

### Changes

- mesh: never carry the mesh over a Tailscale route ([6192402](https://github.com/zombar/tailraid/commit/6192402))
- smbgw: the admin reads a file under another user's Read grant (I-352) ([99386e4](https://github.com/zombar/tailraid/commit/99386e4))
- mesh: keep the node's chain first in the host firewall ([c3748da](https://github.com/zombar/tailraid/commit/c3748da))
- vfs, nfsgw: ACCESS grants READ on a directory on the way to a role (I-351) ([a7b17f9](https://github.com/zombar/tailraid/commit/a7b17f9))

## v0.9.0 (2026-10-15)

### Changes

- vms: the clean slate sets every guest's clock from the host when it drifted ([3d64999](https://github.com/zombar/tailraid/commit/3d64999))
- soak: phase 29 compares the root listing as a set of names ([311bee9](https://github.com/zombar/tailraid/commit/311bee9))
- mesh: `mesh.relay_rate: "0"` lifts the limit as documented, and a small rate refills (I-328) ([9d9ab86](https://github.com/zombar/tailraid/commit/9d9ab86))
- make: the soak target changes into test/soak once ([521e2ee](https://github.com/zombar/tailraid/commit/521e2ee))
- mesh: a pending row's host is allocated again once it is free, and a token rotation ends every pending row (I-319, I-320) ([716debf](https://github.com/zombar/tailraid/commit/716debf))
- mesh: a removed device joins again, a re-join keeps one row, and the mesh errors cross the wire (I-317, I-318, I-319, I-321) ([fb9fee2](https://github.com/zombar/tailraid/commit/fb9fee2))
- users, webui: a sign-in code seals its token, and the session cookie is SameSite=Lax (I-354) ([e946857](https://github.com/zombar/tailraid/commit/e946857))
- mesh: a configured endpoint name is resolved when published, and an unusable mapped address is refused (I-322, I-330) ([88c1f7d](https://github.com/zombar/tailraid/commit/88c1f7d))
- mesh, rpc: the table refuses a removed row whatever the seed says, and the peers follow every membership entry (I-317, I-318) ([75b36a6](https://github.com/zombar/tailraid/commit/75b36a6))
- vms: the Windows data check writes its small file, and a failed write fails the step ([8966abf](https://github.com/zombar/tailraid/commit/8966abf))
- mesh: a path probe's echo carries its own MAC, so a reflected probe pins nothing (I-329) ([1372b14](https://github.com/zombar/tailraid/commit/1372b14))
- smbgw, nfsgw, vfs, webui: a Read-only user's handle, ACCESS by role, and nothing told before the role is checked (I-351, I-352, I-353, I-355) ([b6e7249](https://github.com/zombar/tailraid/commit/b6e7249))
- node: the status report's ready bit is read live, not from the refresher's cached build (I-125) ([e84dc6e](https://github.com/zombar/tailraid/commit/e84dc6e))
- meta, vfs: every Commit needs Write, and a refused Commit drops the inode's dirty data (I-349, I-353) ([279e5ca](https://github.com/zombar/tailraid/commit/279e5ca))
- vfs, webui: snapshot reverts, restores, the whole trash and snapshot creation check the session's role (I-348, I-355) ([09b1edf](https://github.com/zombar/tailraid/commit/09b1edf))
- meta, rpc: the trash sweep, a restore's two parents and the self-only member ops check their actor (I-349, I-345, I-322) ([554e8ce](https://github.com/zombar/tailraid/commit/554e8ce))
- node: a re-added disk opens under its active row, and only a refused invite code throttles its source (I-90, I-343) ([ecab562](https://github.com/zombar/tailraid/commit/ecab562))
- audit: Record waits for no flush; the leader's apply hook no longer stalls Raft (I-333) ([6d4a215](https://github.com/zombar/tailraid/commit/6d4a215))
- soak: phase 29 probes the gateways once the grants reached their replicas ([58b7b4b](https://github.com/zombar/tailraid/commit/58b7b4b))
- vms: the rig's state lives under the main checkout's test/vms for every worktree ([8bd9ef5](https://github.com/zombar/tailraid/commit/8bd9ef5))
- vfs: a READ within ReadAhead chunks of a stream's frontier stays in the sequence (I-203) ([b497b29](https://github.com/zombar/tailraid/commit/b497b29))
- cluster: export falls back to the newest stored snapshot when Raft has nothing new (I-129) ([1060712](https://github.com/zombar/tailraid/commit/1060712))
- repo: the built tailraid binary leaves the tree; the ignore file names every build output ([9426dee](https://github.com/zombar/tailraid/commit/9426dee))
- soak: phases 28–31, users and keys, grants, secrets per user, audit and sessions ([68e68cf](https://github.com/zombar/tailraid/commit/68e68cf))
- audit: the chained audit log, its records, status.users and check's warnings (I-357, I-358) ([6f0fe77](https://github.com/zombar/tailraid/commit/6f0fe77))
- web: sign in to the volume, sessions on every node and routes by role (I-354, I-355) ([2cc1b3c](https://github.com/zombar/tailraid/commit/2cc1b3c))
- gateways: the actor of every mount operation (I-351..I-353) ([e3f5a14](https://github.com/zombar/tailraid/commit/e3f5a14))
- users: signed requests, the needs table, the CLI and personal secrets (I-343..I-345, I-350, I-359) ([b4a1992](https://github.com/zombar/tailraid/commit/b4a1992))
- cli: add-disk, mirror add and verify take the cluster ID from the node, not the data directory (I-324) ([c977014](https://github.com/zombar/tailraid/commit/c977014))
- meta: users, groups, grants, invites and sessions in the FSM (codec 14, I-342..I-349) ([6379858](https://github.com/zombar/tailraid/commit/6379858))
- docs, lookdev: users, groups and permissions (docs/21, I-342..I-359) ([a661154](https://github.com/zombar/tailraid/commit/a661154))
- vms: step 8b records a pre-mesh release's refusal as the hard cut ([c545d06](https://github.com/zombar/tailraid/commit/c545d06))
- soak: a live run sets its secret through a node it can kill, and judges a live kill by the leader's rows ([102ab4d](https://github.com/zombar/tailraid/commit/102ab4d))
- rpc, cluster: every connection to a member leaves from the mesh address (I-317) ([76ffee4](https://github.com/zombar/tailraid/commit/76ffee4))
- svc: the Windows install opens an inbound TCP rule for the node's listeners (I-324) ([b032f27](https://github.com/zombar/tailraid/commit/b032f27))
- config, cli: the sudo paths tests run on a Windows runner (I-324) ([5ac6a3a](https://github.com/zombar/tailraid/commit/5ac6a3a))
- soak: check 23.5 reads before its first poll of the copy ([490e5db](https://github.com/zombar/tailraid/commit/490e5db))
- config: the POSIX homes are joined as paths on every host (I-324) ([06f85ba](https://github.com/zombar/tailraid/commit/06f85ba))
- node: serve waits for its address to take a bind; a sudo join gives the disk directories back too (I-324) ([b8d8c09](https://github.com/zombar/tailraid/commit/b8d8c09))
- mesh: opening the Windows adapter writes wintun.dll beside the exe first (I-324) ([d98fc13](https://github.com/zombar/tailraid/commit/d98fc13))
- config: a join under sudo on Linux keeps the invoking user's config home (I-324) ([04acf79](https://github.com/zombar/tailraid/commit/04acf79))
- mesh: a packet for the device's own address loops back to the kernel; the VM rig bridges onto the LAN (I-317) ([e8ef483](https://github.com/zombar/tailraid/commit/e8ef483))
- admin: the Linux encryption table runs on Linux only ([920aae6](https://github.com/zombar/tailraid/commit/920aae6))
- security: files on disk are 0700 and status checks their encryption (I-340, I-341) ([e0af6ec](https://github.com/zombar/tailraid/commit/e0af6ec))
- soak: the driver waits for the mesh to converge; /dev/fsm-hash labels a hash with its snapshot's index (I-4) ([499bd4d](https://github.com/zombar/tailraid/commit/499bd4d))
- mesh: a pin sends a keepalive on the new path; the NAT rig's router hairpins mapped-in flows (I-329) ([8de9b30](https://github.com/zombar/tailraid/commit/8de9b30))
- rpc: the drain test waits for its connection instead of racing the pool ([1fae2a2](https://github.com/zombar/tailraid/commit/1fae2a2))
- mesh: a probe names its target and a member reports a direct source only (I-322, I-329) ([505cd50](https://github.com/zombar/tailraid/commit/505cd50))
- web: the dashboard's Mesh section, chart.Mesh and GET /api/mesh (I-331) ([9327272](https://github.com/zombar/tailraid/commit/9327272))
- secrets: one key make at a time per node, the fetch outside the store's lock (I-304, I-305) ([56b11d4](https://github.com/zombar/tailraid/commit/56b11d4))
- mesh chart design: names on a flattened ring, the facts in a table under it (I-331) ([44d797d](https://github.com/zombar/tailraid/commit/44d797d))
- docs, lookdev: the dashboard's Mesh section (PR 8 design, I-331 stub) ([a1becb3](https://github.com/zombar/tailraid/commit/a1becb3))
- mesh: port mapping through the default gateway, PCP then NAT-PMP (I-330) ([1137258](https://github.com/zombar/tailraid/commit/1137258))
- cmd: the trash and cache CLI tests wait for the node as the others do ([b2526fc](https://github.com/zombar/tailraid/commit/b2526fc))
- soak: the relay round of phase 26, the NAT rig and phase 27; the cluster key on the leader's tick ([c25fb22](https://github.com/zombar/tailraid/commit/c25fb22))
- mesh: the relay through a member and the path probe (I-328, I-329) ([ddc9c7c](https://github.com/zombar/tailraid/commit/ddc9c7c))
- soak: a passing check of phase 25 carries no reason, the restart check's text no leading space ([d2273af](https://github.com/zombar/tailraid/commit/d2273af))
- mesh, soak: what the compose run of phases 25 and 26 found ([5daa874](https://github.com/zombar/tailraid/commit/5daa874))
- vms: the three-guest rig on the mesh, no headscale ([5f640a2](https://github.com/zombar/tailraid/commit/5f640a2))
- soak: the compose rig on the mesh, phases 25 and 26, the report's mesh section ([8782073](https://github.com/zombar/tailraid/commit/8782073))
- mesh: the path class ranks holders and schedules PUTs and GETs (I-327) ([c82f488](https://github.com/zombar/tailraid/commit/c82f488))
- mesh: the wintun size check counts bytes; the svc test takes the host's separators ([eff043d](https://github.com/zombar/tailraid/commit/eff043d))
- mesh: the service runs with the mesh's privileges on each OS (I-324) ([d38f881](https://github.com/zombar/tailraid/commit/d38f881))
- node, cmd: the node runs on the mesh; Tailscale deleted (I-317, I-318, I-321, I-322, I-325, I-326) ([b9544ab](https://github.com/zombar/tailraid/commit/b9544ab))
- meta, cluster, rpc: the mesh identity in the FSM and the RPC (I-319, I-320, I-326) ([1592620](https://github.com/zombar/tailraid/commit/1592620))
- mesh: internal/mesh, the device, the join code, the introduce and the fan-out (I-316, I-319, I-320, I-323) ([6823532](https://github.com/zombar/tailraid/commit/6823532))

### Documentation

- docs: the three-VM rounds of 2026-10-14 on the mesh (mac and linux PASS, win FAIL in phase 17) ([d18070a](https://github.com/zombar/tailraid/commit/d18070a))
- docs: a person signs in to the volume, not a node (sessions in the FSM, op 32) ([e435fb7](https://github.com/zombar/tailraid/commit/e435fb7))
- docs: the README is a quick start; the guides move to docs/guide-*.md ([d8b2029](https://github.com/zombar/tailraid/commit/d8b2029))
- docs: the mesh's history on the Stats page (I-332 stub, lookdev) ([bbd7b57](https://github.com/zombar/tailraid/commit/bbd7b57))
- docs: regenerate the invariants table for the key-make tests ([90c8adc](https://github.com/zombar/tailraid/commit/90c8adc))
- docs: the mesh, tailraid's own WireGuard network in place of Tailscale (I-316..I-330) ([6155d6a](https://github.com/zombar/tailraid/commit/6155d6a))

## v0.8.3 (2026-10-14)

### Changes

- secrets: no carry-over of the old key files; the volume is re-created with the store ([b565d5c](https://github.com/zombar/tailraid/commit/b565d5c))
- secrets: `secret set` is the one way in; `node keys` and `media key` go (I-308) ([8f48be3](https://github.com/zombar/tailraid/commit/8f48be3))

## v0.8.2 (2026-10-14)

### Changes

- secrets: the S3 and TMDB keys move into the store, the old files are imported (I-308) ([4936dcb](https://github.com/zombar/tailraid/commit/4936dcb))
- webui: the index CSP admits blob: images (I-315) ([d2d6661](https://github.com/zombar/tailraid/commit/d2d6661))

### Documentation

- README: a Secrets section and the secret commands in the reference ([ffe76e1](https://github.com/zombar/tailraid/commit/ffe76e1))

## v0.8.1 (2026-10-14)

### Changes

- vfs, webui: a seek cuts the fetch nobody waits on; two readahead streams per file (I-314, I-203) ([bfe0a97](https://github.com/zombar/tailraid/commit/bfe0a97))
- cluster, webui: one missed probe keeps a node live; thumbnails never pin the browser (I-313) ([c465235](https://github.com/zombar/tailraid/commit/c465235))
- vfs: a soon retry of a flush reuses the shards it placed (I-312) ([218d632](https://github.com/zombar/tailraid/commit/218d632))
- repair: a home move never names a target twice; failed moves shut the gate (I-311) ([b2bccde](https://github.com/zombar/tailraid/commit/b2bccde))
- vfs: drop a finished prefetch from the readahead table (I-310) ([7475e2f](https://github.com/zombar/tailraid/commit/7475e2f))
- secrets: the replicated encrypted store, the member key fetch, secret set|ls|rm (I-303..I-307) ([de51410](https://github.com/zombar/tailraid/commit/de51410))
- go: tools.sum and actionlint.sum carry the keyring modules' sums ([2c551ed](https://github.com/zombar/tailraid/commit/2c551ed))
- keychain: the device's keychain over go-keyring, 0600 file fallback (I-309) ([848cb09](https://github.com/zombar/tailraid/commit/848cb09))
- webui: a show without a poster draws its first episode's frame ([1765eca](https://github.com/zombar/tailraid/commit/1765eca))
- webui: thumbnails in two lanes, a plain nsfw blur, titles over folder covers, file details ([9972474](https://github.com/zombar/tailraid/commit/9972474))

### Documentation

- docs: one encrypted secrets store for the cluster (docs/19, I-303..I-309) ([9b91244](https://github.com/zombar/tailraid/commit/9b91244))

### Tooling

- tests: Windows has no permission bits and checks out CRLF ([7cae06a](https://github.com/zombar/tailraid/commit/7cae06a))

## v0.8.0 (2026-10-13)

### Changes

- media scrape: run the keyless groups without a TMDB key ([356f5d0](https://github.com/zombar/tailraid/commit/356f5d0))
- media scrape: game box art from the libretro thumbnails ([0551020](https://github.com/zombar/tailraid/commit/0551020))
- media scrape: fill missing sidecars and art from TMDB and MusicBrainz ([b6224ad](https://github.com/zombar/tailraid/commit/b6224ad))
- webui: every drive in Disks (mirrors, S3 buckets); no Cache section ([79ad3dd](https://github.com/zombar/tailraid/commit/79ad3dd))
- repair: home placement folds domain skew and the capacity rebalance (I-294..I-296) ([81ca2d1](https://github.com/zombar/tailraid/commit/81ca2d1))
- chart: event labels in a lane under the time axis, legible over exact ([cfc5656](https://github.com/zombar/tailraid/commit/cfc5656))
- webui: Material Symbols in the TypeScript, media-card tiles and posters ([643d134](https://github.com/zombar/tailraid/commit/643d134))
- webui: space a library fold's toolbar from its posters ([96e8e91](https://github.com/zombar/tailraid/commit/96e8e91))
- webui: Material spacing, window classes, media cards and tested CSS (docs) ([c904fba](https://github.com/zombar/tailraid/commit/c904fba))
- cluster: a restarting node asks the members it knows for late joiners (I-282) ([f1907e1](https://github.com/zombar/tailraid/commit/f1907e1))
- webui: ROMs in the Media tab, browse only, and title cards (I-279, I-280) ([d8faefe](https://github.com/zombar/tailraid/commit/d8faefe))
- search: platforms and games under a roms library (I-278) ([101fb84](https://github.com/zombar/tailraid/commit/101fb84))
- meta: the roms library kind, a third library bit at codec 10 (I-277) ([de54904](https://github.com/zombar/tailraid/commit/de54904))
- search: a media name analyser built from a real volume's names (I-276) ([31018c6](https://github.com/zombar/tailraid/commit/31018c6))
- lookdev: tighter media rows, true 3:4 title cards, spacing under the actions ([fc203c5](https://github.com/zombar/tailraid/commit/fc203c5))

### Documentation

- docs: tailraid media scrape, sidecars from TMDB and MusicBrainz (docs/18 §11) ([bf04857](https://github.com/zombar/tailraid/commit/bf04857))
- docs: home placement folds domain skew and the capacity rebalance (I-294..I-296) ([8f2ff36](https://github.com/zombar/tailraid/commit/8f2ff36))
- docs: media names, the ROMs library and title cards (I-276..I-280), lookdev first ([858f412](https://github.com/zombar/tailraid/commit/858f412))

## v0.7.4 (2026-10-13)

### Changes

- soak: 23.4 judges the replaced file once both writers have flushed ([7c67f49](https://github.com/zombar/tailraid/commit/7c67f49))
- cluster: a restarted node accepts members that joined after its snapshot (I-281) ([f5a9a9f](https://github.com/zombar/tailraid/commit/f5a9a9f))
- smb: change notification belongs to the handle; CLOSE answers what waits (I-264) ([cb8d7a1](https://github.com/zombar/tailraid/commit/cb8d7a1))
- soak: the harness drops a node's SMB connections when it stops the node ([2f56486](https://github.com/zombar/tailraid/commit/2f56486))
- rpc: TestClient_CancelledGet_DrainsAShortRest no longer races or hangs ([bd049c9](https://github.com/zombar/tailraid/commit/bd049c9))
- soak: the evidence test decodes the results (Windows paths are escaped) ([ec1bb03](https://github.com/zombar/tailraid/commit/ec1bb03))
- vendor: refresh after the conflict resolution's gofmt ([53d5a50](https://github.com/zombar/tailraid/commit/53d5a50))
- vendor: refresh after merging the stack's base ([76719a8](https://github.com/zombar/tailraid/commit/76719a8))
- vendor: refresh after merging the stack's base ([6817c14](https://github.com/zombar/tailraid/commit/6817c14))
- vendor: refresh after merging the stack's base ([51e158c](https://github.com/zombar/tailraid/commit/51e158c))
- vendor: refresh after merging the stack's base ([bf49cf7](https://github.com/zombar/tailraid/commit/bf49cf7))
- vendor: refresh after merging the stack's base ([44bef6e](https://github.com/zombar/tailraid/commit/44bef6e))
- soak: a failing phase saves each node's log and status before the rig goes (I-270) ([394f8cf](https://github.com/zombar/tailraid/commit/394f8cf))
- webui: folder covers from photos, and a none that waits for a frame clears (I-275) ([c6e4656](https://github.com/zombar/tailraid/commit/c6e4656))
- nfs: a reply the socket refuses closes the connection (I-269) ([65fba9d](https://github.com/zombar/tailraid/commit/65fba9d))
- smb: change notification belongs to the handle; CLOSE answers what waits (I-264) ([b65877b](https://github.com/zombar/tailraid/commit/b65877b))
- smb: an end-of-file change and a FLUSH take no share lock on a concurrent driver (I-268) ([9108baa](https://github.com/zombar/tailraid/commit/9108baa))
- smb: a caseless lookup asks the backend instead of listing the folder (I-267) ([e3f9e78](https://github.com/zombar/tailraid/commit/e3f9e78))
- nfs: every refusal carries its RFC 1813 failure body (I-266) ([b7a25ee](https://github.com/zombar/tailraid/commit/b7a25ee))
- smb: share access is enforced across a share's opens (I-265) ([363b1c2](https://github.com/zombar/tailraid/commit/363b1c2))
- smb: change notification belongs to the handle; CLOSE answers what waits (I-264) ([ed407e6](https://github.com/zombar/tailraid/commit/ed407e6))

### Tooling

- ci: test only what changed, Linux-only pull requests, shellcheck ([e738f92](https://github.com/zombar/tailraid/commit/e738f92))

## v0.7.3 (2026-10-13)

### Changes

- rpc: a cancelled GET drains a short rest; bulk sockets keep little unsent (I-262, I-263) ([9a7b35f](https://github.com/zombar/tailraid/commit/9a7b35f))
- testharness: Settle's catch-up waits 30 s and names the laggard at the timeout ([9b431e1](https://github.com/zombar/tailraid/commit/9b431e1))
- vfs: a dirty file reports its write time, as it reports its size (I-261) ([d3e02ea](https://github.com/zombar/tailraid/commit/d3e02ea))
- bench: compare HTTP/1.1, h2c, gRPC, yamux and QUIC for the node RPC; report ([25551c2](https://github.com/zombar/tailraid/commit/25551c2))
- nfs: a duplicate request cache, so a retransmission runs once (I-260) ([bdfb936](https://github.com/zombar/tailraid/commit/bdfb936))
- rpc: benchmark HTTP/1.1 against cleartext HTTP/2 for the node RPC; report ([4ea8a80](https://github.com/zombar/tailraid/commit/4ea8a80))
- webui: the Stats page draws its frames at once and streams the charts in (I-258, I-259) ([67075a8](https://github.com/zombar/tailraid/commit/67075a8))
- webui: album, show and movie art surfaces to parent folders, first wins (I-257) ([0f7b68d](https://github.com/zombar/tailraid/commit/0f7b68d))
- webui: a folder's cover is stored in the volume and shared by every node (I-256) ([a8ebfd6](https://github.com/zombar/tailraid/commit/a8ebfd6))
- rpc: the control transport keeps 32 idle connections per host (I-255) ([db132ad](https://github.com/zombar/tailraid/commit/db132ad))
- smb: READs off the read loop and, for a concurrent driver, outside the share lock (I-254) ([71c2379](https://github.com/zombar/tailraid/commit/71c2379))

### Documentation

- docs: the Stats page streams its charts in (I-258, I-259), lookdev first ([cf80f6c](https://github.com/zombar/tailraid/commit/cf80f6c))
- docs: sealed S3 nodes renumbered to I-249..I-253 ([fd9920d](https://github.com/zombar/tailraid/commit/fd9920d))
- docs: sealed S3 nodes, client-side encryption of bucket objects (I-246..I-250) ([59e252b](https://github.com/zombar/tailraid/commit/59e252b))

## v0.7.2 (2026-10-13)

### Changes

- nfs: listings filled by real entry sizes, dtpref 1 MiB (I-248) ([ceef273](https://github.com/zombar/tailraid/commit/ceef273))
- smb: a CANCEL never takes back a WRITE; a warning does not stop a chain (I-247) ([53b8521](https://github.com/zombar/tailraid/commit/53b8521))
- soak: phase 12 explains each new orphan instead of demanding none ([772de8d](https://github.com/zombar/tailraid/commit/772de8d))
- rpc: a full node answers busy after SlotWait instead of looking dead (I-246) ([d090c5e](https://github.com/zombar/tailraid/commit/d090c5e))
- nfs: bounded XDR lengths, RFC 5531 reply codes, version checks, LINK args (I-244, I-245) ([9fa1efe](https://github.com/zombar/tailraid/commit/9fa1efe))
- mirror: a sync cut by a restart resumes; the sweep runs only for a deletion (I-243) ([47969df](https://github.com/zombar/tailraid/commit/47969df))
- nfs, soak: an EXIST refusal carries the parent's attributes; the soak's mkdir is mkdir -p (I-242) ([924ba60](https://github.com/zombar/tailraid/commit/924ba60))
- rpc: a mirror's objects and trees travel on the bounded transport (I-241) ([bced835](https://github.com/zombar/tailraid/commit/bced835))
- smb: an open left by a dropped connection, a logoff or a tree disconnect is closed, delete-on-close included (I-240) ([7b42bb4](https://github.com/zombar/tailraid/commit/7b42bb4))
- mirror: a snapshot sync reads and writes 8 chunks at once (I-239) ([b3e4124](https://github.com/zombar/tailraid/commit/b3e4124))
- nfs, vfs: RFC 1813 statuses for namespace errors, exclusive MKDIR, self-rename keeps data (I-238) ([e79eb16](https://github.com/zombar/tailraid/commit/e79eb16))
- smb: allocation is not end of file, non-empty folder delete, MS-FSA wildcards (I-235..I-237) ([870d1d7](https://github.com/zombar/tailraid/commit/870d1d7))
- admin, cli: trash ls pages its reply (I-234) ([d9b7ade](https://github.com/zombar/tailraid/commit/d9b7ade))

## v0.7.1 (2026-10-13)

### Changes

- smb: a wrong-case path reaches the existing folders, no case twins (I-233) ([665cb1a](https://github.com/zombar/tailraid/commit/665cb1a))
- nfs: a listing paged while its folder changes goes on, no BAD_COOKIE (I-232) ([f9334e0](https://github.com/zombar/tailraid/commit/f9334e0))
- nfs: CREATE EXCLUSIVE with a verifier, per RFC 1813 3.3.8 (I-231) ([95a5cdc](https://github.com/zombar/tailraid/commit/95a5cdc))
- vfs: a client's file times outlive the flush; out-of-range times clamp (I-230) ([cc5810c](https://github.com/zombar/tailraid/commit/cc5810c))
- vfs: a created file deleted with no data is purged, not trashed (I-229) ([44b35f3](https://github.com/zombar/tailraid/commit/44b35f3))
- smb, vfs: answer FSCTL_QUERY_FILE_REGIONS, IOCTL statuses per MS-SMB2 (I-227, I-228) ([0778a42](https://github.com/zombar/tailraid/commit/0778a42))
- smb: a request trace at log.level debug ([5e12140](https://github.com/zombar/tailraid/commit/5e12140))
- vfs, soak: a fetch looks in the cache once registered; the harness seal waits for a sample ([9ec874e](https://github.com/zombar/tailraid/commit/9ec874e))
- soak: round 23.5 keeps the poll that saw the end, faults end through the undo (I-112, I-36) ([961898e](https://github.com/zombar/tailraid/commit/961898e))
- webui: an upload's temp carries the node ID, not the host name (I-226) ([0eb52e2](https://github.com/zombar/tailraid/commit/0eb52e2))
- smb, vfs: open-if never truncates, locks follow a rename, exclusive mkdir (I-222, I-224, I-225) ([2ab2872](https://github.com/zombar/tailraid/commit/2ab2872))
- vfs: a plain flush ends a pending rewrite, a dirty claim never lapses (I-223, I-225) ([946cf65](https://github.com/zombar/tailraid/commit/946cf65))

## v0.7.0 (2026-10-12)

### Changes

- soak: phase 23, many users at once (I-222..I-226) ([df9bbf9](https://github.com/zombar/tailraid/commit/df9bbf9))
- webui: uploads of one name through two gateways never mix (I-226) ([8cefb46](https://github.com/zombar/tailraid/commit/8cefb46))
- smb, vfs: rename and delete-on-close judged by the file, not its old name (I-224, I-225) ([1532843](https://github.com/zombar/tailraid/commit/1532843))
- meta, vfs: two clients replacing one file leave one whole version (I-223, codec 9) ([0a73a46](https://github.com/zombar/tailraid/commit/0a73a46))
- vfs, smb, nfs: an exclusive create succeeds once across gateways (I-222) ([6485c85](https://github.com/zombar/tailraid/commit/6485c85))
- soak: phase 19 retries the read of a mirror back online for a probe interval ([e66e864](https://github.com/zombar/tailraid/commit/e66e864))
- soak: a mirror is never a down node; phase 19 unplugs its mirror when it ends ([08e7114](https://github.com/zombar/tailraid/commit/08e7114))
- vfs: an unknown outcome during a leader failover is retried until stale (I-95) ([8085416](https://github.com/zombar/tailraid/commit/8085416))
- soak: phase 22 fixes from its Docker runs, and a harness Kill/Restart race ([43e52a3](https://github.com/zombar/tailraid/commit/43e52a3))
- chore: remove the soak binary #143 committed at the root, and ignore it ([eebfe0f](https://github.com/zombar/tailraid/commit/eebfe0f))
- meta, smb: restore the own cleanup that #143 reverted (I-220) ([7d411a2](https://github.com/zombar/tailraid/commit/7d411a2))
- mirror: a write to an unplugged mirror fails instead of recreating its path ([8269194](https://github.com/zombar/tailraid/commit/8269194))
- soak: phase 22, copies under faults ([8ef0441](https://github.com/zombar/tailraid/commit/8ef0441))
- soak: phase 19 keeps every snapshot it saw for the report (the schedule thins automatic ones within the hour) ([a1ebda1](https://github.com/zombar/tailraid/commit/a1ebda1))
- meta, smb: a failed copy cleans up after itself under protection (I-220) ([345c455](https://github.com/zombar/tailraid/commit/345c455))
- vfs: a flush cancelled before every chunk started is an error, not nil works ([16da26c](https://github.com/zombar/tailraid/commit/16da26c))
- smb: keep the share lock for drivers that never said they can skip it (I-217) ([6ad1e9d](https://github.com/zombar/tailraid/commit/6ad1e9d))
- smb: answer slow WRITEs later, log write-path refusals (I-217, I-218) ([acd4c7d](https://github.com/zombar/tailraid/commit/acd4c7d))
- vfs, repair: a peer's restart or upgrade never fails a client WRITE (I-219, L-219) ([5ef0f16](https://github.com/zombar/tailraid/commit/5ef0f16))
- mirror: adopt, import rowless manifests, open an added mirror at once, fix the hook deadlock ([c919c9b](https://github.com/zombar/tailraid/commit/c919c9b))
- webui: snapshots and versions in the browser, every mirror's state on every node (I-187, I-188) ([40d336d](https://github.com/zombar/tailraid/commit/40d336d))
- soak: phase 19 snapshots on the compose rig's mirror and its report figures ([828c3ef](https://github.com/zombar/tailraid/commit/828c3ef))
- update: restart only once the gateway is idle (I-221) ([5436ffe](https://github.com/zombar/tailraid/commit/5436ffe))
- config: TestConfig_Mirrors builds its paths under a temp dir (Windows CI) ([8196a16](https://github.com/zombar/tailraid/commit/8196a16))
- vfs, node, cli: versions and restore from a snapshot (I-186, L-216, L-217) ([729408c](https://github.com/zombar/tailraid/commit/729408c))
- vfs, nfsgw, smbgw, rpc: /.snapshots on the mounts (I-185) ([58c51aa](https://github.com/zombar/tailraid/commit/58c51aa))
- vfs: pin the readahead order by holding the client's GETs ([175fb4b](https://github.com/zombar/tailraid/commit/175fb4b))
- webui: one show for its season-pack folders ([57cc0fd](https://github.com/zombar/tailraid/commit/57cc0fd))
- node, config, cli: mirrors on their hosts, the sync worker and the schedule (I-179, I-181..I-183, I-188) ([eb667a4](https://github.com/zombar/tailraid/commit/eb667a4))
- meta, mirror: the snapshot catalogue, the sync hold and the mirror store (I-179..I-181, I-183, I-184) ([701dc81](https://github.com/zombar/tailraid/commit/701dc81))

### Documentation

- docs: many users at once, exclusive create, whole rewrites, SMB handles by inode (I-222..I-226) ([561936b](https://github.com/zombar/tailraid/commit/561936b))
- docs: soak report on main with the snapshot stack (22 phases, 0 failed) ([3d21ac3](https://github.com/zombar/tailraid/commit/3d21ac3))
- docs: soak report on the snapshot stack (21 phases, 251 checks, 0 failed) ([c199c69](https://github.com/zombar/tailraid/commit/c199c69))
- docs: README with a feature list, snapshots and mirrors guide, and a command reference ([fbedf86](https://github.com/zombar/tailraid/commit/fbedf86))
- docs: no user removal of snapshots (I-183, I-187, I-188) ([08aaefb](https://github.com/zombar/tailraid/commit/08aaefb))
- docs: snapshots on mirror disks, design, lookdev and stubs (I-179..I-188) ([ad13bf6](https://github.com/zombar/tailraid/commit/ad13bf6))

### Tooling

- test: snapshots on the harness cluster and in the model checker; report a detached mirror by its row ([640290a](https://github.com/zombar/tailraid/commit/640290a))

## v0.6.1 (2026-10-12)

### Changes

- webui: the Media tab grabs missing video frames ([b863ab6](https://github.com/zombar/tailraid/commit/b863ab6))
- webui: fold media sections, drop genre chips, hide deleted by default ([9c51051](https://github.com/zombar/tailraid/commit/9c51051))
- search: cut scene names at season markers and a mid-name year ([b6c0eb5](https://github.com/zombar/tailraid/commit/b6c0eb5))
- webui: folder covers from a subfolder, and no_cover in listings ([06a2904](https://github.com/zombar/tailraid/commit/06a2904))
- webui: disk balance tones follow the rebalance skew ([abf98dd](https://github.com/zombar/tailraid/commit/abf98dd))
- webui: integrity rows in three columns, zero chunks grey ([cf144b8](https://github.com/zombar/tailraid/commit/cf144b8))
- vfs: Reader.Close cuts and waits for detached fetches before the cache drains (I-204) ([edf1e41](https://github.com/zombar/tailraid/commit/edf1e41))

## v0.6.0 (2026-10-12)

### Changes

- soak: phase 21 search and media (I-206..I-212) ([0644c4c](https://github.com/zombar/tailraid/commit/0644c4c))
- webui: the Media tab (I-212) ([20a6d22](https://github.com/zombar/tailraid/commit/20a6d22))
- webui: search, the markdown document and the Index tile (I-208, I-209) ([3a521ec](https://github.com/zombar/tailraid/commit/3a521ec))
- search: the index in the volume, the leader's pass and the media catalogue (I-206, I-207, I-208, I-211) ([c8a689a](https://github.com/zombar/tailraid/commit/c8a689a))
- meta, webui: the library folder flag, two bits of Flags at codec 6 (I-210) ([cfc1695](https://github.com/zombar/tailraid/commit/cfc1695))
- node: the stats seal test's rows lie in the past whatever the hour ([e2e3dbe](https://github.com/zombar/tailraid/commit/e2e3dbe))
- lookdev: the search box on its own row below 900 px with one focus ring, real 360 px frames, the Library row stacked ([3640603](https://github.com/zombar/tailraid/commit/3640603))
- docs, lookdev: search, the index in the volume, the media catalogue and the Media tab (I-206..I-212, stubs) ([deb1d5d](https://github.com/zombar/tailraid/commit/deb1d5d))

### Tooling

- tests: CLI ports below the ephemeral range; the harness's MinCodec keeps its node ([1b1adb2](https://github.com/zombar/tailraid/commit/1b1adb2))

## v0.5.3 (2026-10-12)

### Changes

- webui: a video's tile frame is a presented, non-dark frame; re-grab old ones (I-216) ([92eec24](https://github.com/zombar/tailraid/commit/92eec24))
- repair, config: the background scrub verifies the volume once per scrub_period (I-215) ([1e96e56](https://github.com/zombar/tailraid/commit/1e96e56))

## v0.5.2 (2026-10-11)

### Changes

- soak: the harness wait lets a flush under way finish before flushing (phase 2) ([42743e3](https://github.com/zombar/tailraid/commit/42743e3))
- vfs, soak: readahead retries skipped candidates and counts them (I-166) ([a01f485](https://github.com/zombar/tailraid/commit/a01f485))
- vfs: a READ that joined a failed prefetch fetches for itself (I-203) ([57eb46c](https://github.com/zombar/tailraid/commit/57eb46c))
- soak: phase 17 polls for the suspect mark to clear before repairing (I-170) ([93a8852](https://github.com/zombar/tailraid/commit/93a8852))
- vfs: a READ in the chunk of the previous one keeps the readahead (I-203) ([0eb4b2a](https://github.com/zombar/tailraid/commit/0eb4b2a))
- admin, rpc: a status reply follows the request that asked for it (I-214) ([a34593f](https://github.com/zombar/tailraid/commit/a34593f))
- smb: a QUERY_INFO answer is bounded by the client's buffer (I-213) ([5c13502](https://github.com/zombar/tailraid/commit/5c13502))
- pins, testharness: drain the chunk cache before its directory goes ([79a9a17](https://github.com/zombar/tailraid/commit/79a9a17))
- webui: hidden wins over a class's display; a player change re-marks rows in place ([ecfcfaf](https://github.com/zombar/tailraid/commit/ecfcfaf))
- CLAUDE.md: one status paragraph after the merge of main ([14989ab](https://github.com/zombar/tailraid/commit/14989ab))
- Early serve: data-shard sums in Raft, a range served from a verified shard before the chunk is decoded (I-201, I-202, codec 5) ([10d4a75](https://github.com/zombar/tailraid/commit/10d4a75))
- Open media fast: readahead after the read, cache writes behind the Put, status off the request path (I-203..I-205, I-165) ([a8cc721](https://github.com/zombar/tailraid/commit/a8cc721))

## v0.5.1 (2026-10-11)

### Changes

- soak: phase 20 compares the rollup against the raw rows as sealed by the rollup seal ([45b2331](https://github.com/zombar/tailraid/commit/45b2331))
- stats: a dead member's row says dead, whatever its suspect mark (I-191) ([c79adc3](https://github.com/zombar/tailraid/commit/c79adc3))
- soak: phase 20 judges the newest cluster row after the kill from any live member ([b56dd7d](https://github.com/zombar/tailraid/commit/b56dd7d))
- soak: gofmt the report after the merge ([5708bad](https://github.com/zombar/tailraid/commit/5708bad))
- rpc, meta: a join with an unknown row outcome is retryable; the FSM generator keeps one folder protected ([905d984](https://github.com/zombar/tailraid/commit/905d984))
- soak, stats: phase 20 and the seal's rollup mode (docs/17 §7) ([ef386fd](https://github.com/zombar/tailraid/commit/ef386fd))
- webui, stats: the Stats page, /api/stats and the theme switch (I-196, I-198, I-200) ([bc450de](https://github.com/zombar/tailraid/commit/bc450de))
- stats: the history kept in the volume (I-191..I-194, I-199, the hiding half of I-195) ([d386c90](https://github.com/zombar/tailraid/commit/d386c90))
- admin, soak: network rx/tx vitals on three OSes and the report's network chart (I-190) ([8339a14](https://github.com/zombar/tailraid/commit/8339a14))
- chart: one package draws the soak report and the Stats page (I-197) ([ce7b6c2](https://github.com/zombar/tailraid/commit/ce7b6c2))
- docs, lookdev: the Stats page (docs/17), I-190..I-200, stubs ([be5b8c3](https://github.com/zombar/tailraid/commit/be5b8c3))

## v0.5.0 (2026-10-11)

### Changes

- vms: the downgrade step accepts a release's refusal of a newer snapshot codec ([d84889a](https://github.com/zombar/tailraid/commit/d84889a))
- config: trash.retention is written only when set (I-61) ([ad11d3a](https://github.com/zombar/tailraid/commit/ad11d3a))
- soak: phase 16 gives the mount its attribute-cache window to show a restored tree ([a15a30b](https://github.com/zombar/tailraid/commit/a15a30b))
- rpc, soak: an admin op that runs a pass is answered when the pass is done (I-189) ([1082911](https://github.com/zombar/tailraid/commit/1082911))
- soak: phase 16 judges the reclaim as phase 8 does ([7e42d24](https://github.com/zombar/tailraid/commit/7e42d24))
- testharness: a fabric reader's readahead drains before the test's temp dirs go ([438f15a](https://github.com/zombar/tailraid/commit/438f15a))
- meta: a fold touches the directory it folds into ([f9226a4](https://github.com/zombar/tailraid/commit/f9226a4))
- soak: phase 12 lets a migration from the previous phase's restore finish before it counts orphans ([b06f656](https://github.com/zombar/tailraid/commit/b06f656))
- soak: the compose rig admits the driver to the browser UI; a refused remove is a permission error ([25bbec0](https://github.com/zombar/tailraid/commit/25bbec0))
- pins: a walk's shard requests carry the background mark (I-175) ([3382dae](https://github.com/zombar/tailraid/commit/3382dae))
- admin, webui: a bucket's card shows what it stores, not a full disk (I-178) ([fa44d1c](https://github.com/zombar/tailraid/commit/fa44d1c))
- dev, soak: a corruption hits every copy of the shard the host holds ([f5f9ef5](https://github.com/zombar/tailraid/commit/f5f9ef5))
- build: tidy tools.mod and actionlint.mod for x/image and dhowden/tag ([78dfdab](https://github.com/zombar/tailraid/commit/78dfdab))
- soak: phase 16 drives the browser UI; the report charts the trash and the pinned cache ([e44caec](https://github.com/zombar/tailraid/commit/e44caec))
- webui: list and tile views, selection, trash, flags, pins, thumbnails, the album view and the now-playing bar ([0a39622](https://github.com/zombar/tailraid/commit/0a39622))
- webui: the trash, batches, flags, thumbnails, tags and pins over HTTP ([9537a70](https://github.com/zombar/tailraid/commit/9537a70))
- webui: the integrity panel loads a page sixteen checks at a time and survives the redraw ([4c6da9b](https://github.com/zombar/tailraid/commit/4c6da9b))
- cache: pinned folders kept on a device, cache.size, `tailraid cache pin|unpin|ls` ([aa57001](https://github.com/zombar/tailraid/commit/aa57001))
- status: no "unreachable" warning for a dead diskless host ([584827f](https://github.com/zombar/tailraid/commit/584827f))
- vfs, gateways, gc: a delete is a trash; the retention purge, trash admin ops and CLI ([42d6da1](https://github.com/zombar/tailraid/commit/42d6da1))
- meta: the trash, restore and purge, protected and nsfw flags, codec 4 ([08ec386](https://github.com/zombar/tailraid/commit/08ec386))

### Documentation

- docs: soak report on the final web UI v2 stack (18 phases, 202 checks, 0 failed) ([2078287](https://github.com/zombar/tailraid/commit/2078287))
- docs: soak report on the web UI v2 stack (18 phases, 202 checks, 0 failed) ([658891e](https://github.com/zombar/tailraid/commit/658891e))

### Tooling

- tests: the command generator keeps protection short-lived; the idle-flush test moves the clock inside its condition ([fe6511c](https://github.com/zombar/tailraid/commit/fe6511c))
- test: the model checker trashes, restores and purges; a trash survives a snapshot install ([bbade8f](https://github.com/zombar/tailraid/commit/bbade8f))

## v0.4.8 (2026-10-11)

### Changes

- dev, soak: a corruption reaches every copy of the shard on the host ([ecf1834](https://github.com/zombar/tailraid/commit/ecf1834))
- cmd/tailraid: the CLI tests wait a minute for a real Raft to become ready ([198393f](https://github.com/zombar/tailraid/commit/198393f))
- cluster, node: a member op reads the row behind a Raft barrier (I-177) ([af28107](https://github.com/zombar/tailraid/commit/af28107))
- cli: default chunk size 4 MiB ([3742d49](https://github.com/zombar/tailraid/commit/3742d49))

## v0.4.7 (2026-10-11)

### Changes

- rpc: the share test asserts order, not a wall-clock bound ([007ed72](https://github.com/zombar/tailraid/commit/007ed72))
- config: repair.workers and repair.rate are written only when set ([1c5573a](https://github.com/zombar/tailraid/commit/1c5573a))
- soak, rpc, vms: phase 18 reports its backlog, one gateway skips its read check, the share test waits for its slots ([41a7ce4](https://github.com/zombar/tailraid/commit/41a7ce4))
- node: the voter-file test's unwritable step works on Windows ([3709c71](https://github.com/zombar/tailraid/commit/3709c71))
- node: the config's vote follows the member table (I-176) ([1875183](https://github.com/zombar/tailraid/commit/1875183))
- repair: a bounded fan-out that stays in the background (I-174, I-175) ([77e2dc7](https://github.com/zombar/tailraid/commit/77e2dc7))
- placement: the host cap outranks the voter preference (I-173) ([d2b7114](https://github.com/zombar/tailraid/commit/d2b7114))

### Documentation

- docs: soak and three-VM reports on repair-converges (18 phases, 0 failed; mac and win rounds pass) ([77b8f04](https://github.com/zombar/tailraid/commit/77b8f04))

## v0.4.6 (2026-10-11)

### Changes

- repair: capacity rebalance within a host after add-disk (I-172) ([c70079b](https://github.com/zombar/tailraid/commit/c70079b))
- vfs, cluster, rpc: straggler grace, suspect marks, GET body deadline (I-169..I-171) ([75ee95d](https://github.com/zombar/tailraid/commit/75ee95d))
- vfs: WAL group commit; a record's effect applies after its fsync, in log order (I-168) ([69717d6](https://github.com/zombar/tailraid/commit/69717d6))
- nfsgw: in-tree go-nfs serves a connection's requests concurrently; macOS mounts at 1 MiB (I-167) ([d9d9a4f](https://github.com/zombar/tailraid/commit/d9d9a4f))
- vfs: server-side readahead, own and data shards first, soak phase 17 (I-166) ([5758ea8](https://github.com/zombar/tailraid/commit/5758ea8))

### Documentation

- docs: soak report on the hot-path stack (16 phases, 0 failed) ([cfe506f](https://github.com/zombar/tailraid/commit/cfe506f))
- docs: web UI v2 design, lookdev and stubs (trash, flags, tiles, audio, pins) ([c839884](https://github.com/zombar/tailraid/commit/c839884))

## v0.4.5 (2026-10-10)

### Changes

- s3: prove the failover to the bucket (I-149, I-150, I-151, L-198) ([d313ac0](https://github.com/zombar/tailraid/commit/d313ac0))
- s3: the leader judges a bucket by its own probe (I-148) ([8deb2d5](https://github.com/zombar/tailraid/commit/8deb2d5))
- soak: phase 15, an S3 bucket as a storage node ([1376e63](https://github.com/zombar/tailraid/commit/1376e63))
- s3: buckets as storage nodes (docs/10 §4) ([64c6d2e](https://github.com/zombar/tailraid/commit/64c6d2e))

## v0.4.4 (2026-10-10)

### Changes

- svc: the Linux unit plan stages at a slash path on every OS ([fc5eb8a](https://github.com/zombar/tailraid/commit/fc5eb8a))
- webui: spread host names run up beside their columns ([33c3c66](https://github.com/zombar/tailraid/commit/33c3c66))
- cluster, node: demote takes the vote, and a disk serve cannot write closes ([8d9b5a4](https://github.com/zombar/tailraid/commit/8d9b5a4))
- serve: a node waiting for the log waits for its own row ([7863b77](https://github.com/zombar/tailraid/commit/7863b77))
- cli: tailraid dashboard opens the browser UI ([4d112af](https://github.com/zombar/tailraid/commit/4d112af))
- mount: warn when a Windows mount runs as administrator ([b492a23](https://github.com/zombar/tailraid/commit/b492a23))
- windows: install maps the drive at every logon, in the desktop session ([ab7a66a](https://github.com/zombar/tailraid/commit/ab7a66a))
- node: register a disk's budget as serve starts, not a tick later ([6d9cf10](https://github.com/zombar/tailraid/commit/6d9cf10))
- webui, smb: macOS's ._ and .DS_Store files stop cluttering listings ([9b9976d](https://github.com/zombar/tailraid/commit/9b9976d))
- cluster: a name names the live row, and a removed row refuses member ops ([5e3c337](https://github.com/zombar/tailraid/commit/5e3c337))

### Tooling

- tests: freePorts keeps every listener open until the whole set is chosen ([80dd197](https://github.com/zombar/tailraid/commit/80dd197))

## v0.4.3 (2026-10-10)

### Changes

- soak: the membership phase hands an auto volume back in auto mode ([7ac9e21](https://github.com/zombar/tailraid/commit/7ac9e21))
- The Windows node (honker) has restarted every minute since it joined: `serve` waits 60 s for its member table, the table never arrives because the authorizer refuses the leader ("raft connection refused: not a member"), serve exits, and the SCM starts it again. The RPC port never opens, so `status` and the web UI are refused. ([fbc397b](https://github.com/zombar/tailraid/commit/fbc397b))
- vms: the update step restarts the restored binary when a release's rollback restart fails ([1f1cd8b](https://github.com/zombar/tailraid/commit/1f1cd8b))
- smb: every reply of a compounded response is signed over what is sent ([6bfefb7](https://github.com/zombar/tailraid/commit/6bfefb7))
- vms: the rig's sources live under scripts/vms and test/vms is state only, ignored as a whole ([4cf746b](https://github.com/zombar/tailraid/commit/4cf746b))
- cli: update --restart resolves the service before the binary swap ([1066769](https://github.com/zombar/tailraid/commit/1066769))
- cluster: a forward that never left is retried like a missing leader ([51b80a0](https://github.com/zombar/tailraid/commit/51b80a0))
- vms: guest binaries under ~/.local/bin, and every guest's service log kept per round ([73c5469](https://github.com/zombar/tailraid/commit/73c5469))
- node: a gateway that was the leader dates a leaderless spell from when it noticed ([4ba1f65](https://github.com/zombar/tailraid/commit/4ba1f65))
- soak: verify on Windows rides out a gateway restart, and the drain guard counts targets ([83f9c7b](https://github.com/zombar/tailraid/commit/83f9c7b))
- cluster: a drop rule fails the Raft connections already open with the peer ([9b80ebb](https://github.com/zombar/tailraid/commit/9b80ebb))
- soak: a live run's drop rules never name the device running verify ([ae8b9e6](https://github.com/zombar/tailraid/commit/ae8b9e6))
- vms: a round updates every node to the latest release with --restart and rolls it back ([26ad66b](https://github.com/zombar/tailraid/commit/26ad66b))
- node: init, a ready serve and a clean shutdown snapshot, so every restart has its state ([52789da](https://github.com/zombar/tailraid/commit/52789da))
- vms: Windows by password-authenticated SSH, liveness polled on every guest, the parity skip allowed ([9fc8c7b](https://github.com/zombar/tailraid/commit/9fc8c7b))
- node: serve starts without a leader when its snapshot holds its row and the volume ([656855d](https://github.com/zombar/tailraid/commit/656855d))
- smb: Windows mounts with New-SmbMapping, and a failed READ is an I/O error on the wire ([19109e3](https://github.com/zombar/tailraid/commit/19109e3))
- vms: the Windows image installs unattended from a no-prompt copy of the ISO ([f982d52](https://github.com/zombar/tailraid/commit/f982d52))
- vms: three local VMs (macOS, Linux, Windows) forming one volume, each OS first in turn ([4b4f296](https://github.com/zombar/tailraid/commit/4b4f296))
- node: retry the joiner's first snapshot until Raft has passed its configuration entry ([5c5e933](https://github.com/zombar/tailraid/commit/5c5e933))
- soak: one Full profile for cmd/soak --profile full and tailraid verify --full ([5f2903f](https://github.com/zombar/tailraid/commit/5f2903f))

### Documentation

- docs: README explains the Docker soak and the three-VM rig ([259f177](https://github.com/zombar/tailraid/commit/259f177))
- docs: the license holder is FORGE3D CYF. ([9aaa6de](https://github.com/zombar/tailraid/commit/9aaa6de))
- docs: AGPL-3.0 license file and the README notice ([6658659](https://github.com/zombar/tailraid/commit/6658659))

### Tooling

- tests: the shutdown hand-over test waits for the new leader's vote ([4ad0863](https://github.com/zombar/tailraid/commit/4ad0863))

## v0.4.2 (2026-10-10)

### Changes

- soak: the fake tailnet lists the other nodes as peers ([7f89da7](https://github.com/zombar/tailraid/commit/7f89da7))
- webui: the Balance block hides removed members like status and node ls ([387d2c5](https://github.com/zombar/tailraid/commit/387d2c5))
- The repository went public so a device without a GitHub token (the Windows machine) can update. The updater already handled that: without a token it resolves the release anonymously and downloads the browser URL, and the anonymous API answers 200 for v0.4.1 with every asset. What changed is the wording and one error: README, docs/12 §5 and the `update` help no longer say the repository is private, and an anonymous 404 now reads "release not found" with the token hint as a parenthesis, instead of "repository needs a token". No invariant changes. ([74162d3](https://github.com/zombar/tailraid/commit/74162d3))
- cluster: a node reports the version it runs into its own row at start ([74ac12e](https://github.com/zombar/tailraid/commit/74ac12e))
- svc: stop waits for the process to exit and succeeds when none runs; node ls hides removed rows ([a0fa6d5](https://github.com/zombar/tailraid/commit/a0fa6d5))

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

