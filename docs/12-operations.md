# 12 — Operations: service install, logs, the Windows mount, fault injection, releases

What a device needs beyond the binary so that `tailraid serve` runs unattended, logs land
somewhere, Windows can mount, a live cluster can be exercised by `tailraid verify
--destructive`, and releases are reproducible. Milestone M4 (docs/00).

## 1. Service install

`tailraid install [--print]` writes the per-OS unit and registers it; `uninstall`, `start`,
`stop` and `logs` complete the set. `--print` shows every file and command without
touching the system. The service runs the same binary with `serve --config <path>`, so
whatever `init`/`join` wrote is what the service reads; the installer prints the config
path and data directory it bound.

| OS | Unit | Manager commands | Restart | Logs |
|---|---|---|---|---|
| macOS | LaunchAgent `~/Library/LaunchAgents/app.tailraid.serve.plist` (per user: Tailscale's GUI session and the mount are per user) | `launchctl bootstrap gui/$UID …` / `bootout`; `kickstart` / `kill SIGTERM` then `print` until `not running` | `KeepAlive`, `ThrottleInterval 5` | `~/Library/Logs/tailraid/serve.log` (`StandardOutPath`/`StandardErrorPath`) |
| Linux | system unit `/etc/systemd/system/tailraid.service`, `User=<installing user>`, `Environment=TAILRAID_HOME=…`, `After=network-online.target tailscaled.service` | `systemctl daemon-reload`, `enable --now`, `stop`, `disable --now` (via `sudo` when not root) | `Restart=always`, `RestartSec=2` | journald: `tailraid logs` runs `journalctl -u tailraid` |
| Windows | SCM service `tailraid` (`sc.exe create … binPath= "<exe> serve --config … --service" start= delayed-auto depend= Tailscale`), `sc.exe failure … actions= restart/5000` | `sc.exe start|stop|delete` (elevated); `stop` then `query` until `STOPPED` | SCM failure actions | `%ProgramData%\tailraid\serve.log` (`log.file`): services have no stderr |

`log.file` in the config (or `TAILRAID_LOG_FILE`) makes `serve` append to a file in the
configured format instead of stderr; `install` sets it on macOS and Windows. Only `serve`
opens it: every other command logs to stderr, so a user's `status` or `join` never needs
the service's log directory (I-60). `tailraid logs [-f] [-n N]` prints or follows the
configured file, or journald on Linux.

On Windows `serve --service` runs under the service control manager (start/stop
handshake, `SERVICE_RUNNING`); without the flag it is an ordinary console process. The
service runs as LocalSystem, whose config home is `%ProgramData%\tailraid` — the same
`config.Home()` an elevated `join` used, so no copy step.

Mounting is a user action (`tailraid mount`) on macOS and Linux: the LaunchAgent and the
systemd unit only serve. On Windows `install` also registers a logon task that maps the
drive in the user's desktop session (§2). The mount survives service restarts because handles are stable
(I-16) and the mounts are `soft`. The three service managers, the three mounts and the
install, restart and recover paths are exercised on real guests by the three-VM rounds
([vms.md](vms.md)).

The service log prints the browser UI address (`http://<tsIP>:20482/`, docs/13) at start,
and `tailraid status` shows it on its `web` line; `tailraid dashboard` opens it in the
default browser (docs/13 §1); `listen.web: 0` turns the UI off.

**Address changes.** When the device's Tailscale IP changes, `serve` re-registers the new
address in the node table (every peer dials it from then on) and exits with a non-zero
status, because its Raft and RPC listeners are bound to the old address; the service unit's
restart policy brings it back on the new one within seconds. Without a service manager,
restart `tailraid serve` by hand.

**Start without a leader.** `serve` waits for the log only when its data directory holds
no snapshot with its own row and the volume (the first start after a join that ended
before the snapshot); otherwise it gives a leader five seconds (the usual case, so the
legacy check below sees the whole log before any listener opens) and then starts its
listeners, with or without one, serving its gateways stale (docs/02, I-39) and
answering `/health` with `ready: false` until a leader connects, so `status` exits 1
meanwhile (I-125); the legacy check runs again once the log has arrived. Raft
restores a table at open from a snapshot only, and its own snapshots come every 8192
entries, so `init`, `join`, a `serve` that has become ready and a clean shutdown each
take one: the node that ran `init` and was killed an hour later restarts with its state
like any other. A node
that waited a minute for a leader and then exited was what the three-VM rounds saw: the
service manager restarted it every minute and a killed node never came back within the
soak's settle time, because the leader stays out of reach for as long as the fault lasts
and the restart loop never let the node be reached either (I-135).

**Waiting for the log means waiting for its own row.** A follower knows the leader
from the first heartbeat, before the entries that add its row have arrived: a joiner
whose leader had a few hundred entries past its last snapshot replayed its local log,
counted itself ready, found no row of its own and exited, and each restart got one
batch further until the leader's replication backoff outlasted the process (node0's
first start, 2026-10-10). So the node without that state waits for its own row and the
volume to be applied, within the same bound, and exits only when they never arrive
(I-144).

**Confinement (Linux).** The unit runs with `NoNewPrivileges`, `ProtectSystem=strict`,
`ProtectHome=read-only` and `PrivateTmp`, and `ReadWritePaths` names exactly the data
directory, every added disk and the config directory (plus the log file's directory when
`log.file` is set), so the service can write nowhere else; every path in the unit is
quoted; the installer stages the unit in a directory it just created, never at a
predictable path in `/tmp` (I-122). The LaunchAgent's `KeepAlive` is
`{SuccessfulExit: false}`: `tailraid stop` keeps the service down, while the address-change
exit (non-zero) still restarts it.

**Shutdown.** On SIGTERM or SIGINT a leader first transfers leadership to another voter
(bounded by 5 s) and then stops Raft, so the rest of the cluster keeps committing
without an election timeout; the NFS and SMB listeners close, the flusher's final flush runs
(capped at 30 s), then the stores close. A second signal ends the process at once. The
Windows handler reports a stop wait hint of 45 s and the unit's `TimeoutStopSec` is the
same figure (I-123).

**`stop` then `start`.** `launchctl kill` and `sc.exe stop` return as soon as the signal
is delivered, while the process is still handing over leadership and flushing, and a
`launchctl kickstart` issued before it exits does nothing (the job is still running), after
which the clean exit leaves it down. `tailraid stop` therefore returns only once the
manager reports no process (`launchctl print` says `not running`, `sc.exe query` says
`STOPPED`; `systemctl stop` blocks on its own), bounded by 60 s, and succeeds when there
was no process to stop (launchctl's "No process to signal", SCM's error 1062), so
`tailraid stop && tailraid start` and `update --restart` always start the new binary; a
service that is not installed is still an error (I-131).

**Log file.** `log.file` rotates itself at 64 MiB keeping five generations
(`serve.log.1` … `.5`), reopens on SIGHUP (so logrotate or newsyslog can rename it), and
`tailraid logs -f` prints every appended line once. When `serve` stops on an error it
logs the error as `serve stopped` before exiting, since a service has no terminal to
print it to (and on Windows no stderr). Warnings that repeat per item (a flush
that fails for every dirty inode, a repair that fails for every chunk, a refused web
request per probe) are rate-limited to one line a minute per site with a `suppressed`
count on the next line (I-124).

## 2. Windows mount

Windows mounts the SMB gateway (docs/15): `tailraid mount T:` runs `New-SmbMapping` with the
gateway's port, which needs Windows 11 24H2 or newer. There is no portmapper and no NFS
client to enable.

Explorer cannot open the share by path: `\\host\…` always dials port 445, which is
Windows' own file sharing, so the drive letter is the way in. `install` registers the
scheduled task `tailraid-mount` for the installing user, which maps the drive at every
logon (and once at install), so it is under This PC after a reboot without a command
(docs/15 §6, I-142). A drive is mapped per logon session, and an elevated prompt is a
different session from Explorer's: `tailraid mount` run as administrator maps a drive
Explorer does not show, and says so.

## 3. Fault injection on a live cluster (`dev.enabled`)

Five endpoints (docs/08) turn a real node into a soak target:

- `POST /dev/exit` replies `204` and exits with status 3 after 100 ms. With a service
  manager that restarts the process this is "kill a node".
- `POST /dev/fault` with `{"drop":["ip",…]}` installs drop rules for the listed peers
  on that node: the Raft stream layer refuses to dial them, fails the connections it
  already has with them (a leader's replication pipelines, which a rule on one side
  alone must cut) and closes their incoming
  connections, the node RPC client refuses to send to them, and inbound RPC from them is
  answered `503`. An empty list heals. `GET /dev/fault` returns the rules. Rules live in
  memory only; a restart heals.
- `GET /dev/goroutines` returns every goroutine's stack. `verify` and the compose driver
  take it from every node after a failed phase and attach it to the report, so a node
  that hangs is diagnosed from the report rather than by sending it SIGQUIT.
- `GET /dev/disk` returns every open disk as its budget sees it (`{"disks": [{id, name,
  used, free, total, pinned}]}`); `POST /dev/fill?bytes=N&disk=<id>` writes a scratch file
  of N bytes on that disk (0 removes it), so the soak suite's automation phase can move a
  disk's free space and watch its budget follow (docs/01 §Node budget).

`tailraid verify --destructive --restarts` declares that every target runs with
`dev.enabled` under a restarting service manager: the parity, consensus, restart-storm
and partition steps then run against the real cluster through those endpoints. Because
the service manager restarts an exited node within seconds, `verify` holds a killed node
down with drop rules on it and on its peers until the phase restarts it (docs/09 §6).

## 4. Releases

A release is a git tag `vMAJOR.MINOR.PATCH` on `main`:

1. `make tag VERSION=v0.1.0`, on `main`, refuses a dirty tree or a tag that exists, runs
   `make check` and regenerates `CHANGELOG.md` from the commit log (`scripts/changelog.sh`:
   one section per tag, entries grouped into Changes / Documentation / Tooling by the
   subject's `scope:` prefix, so commit subjects are the changelog). `main` only takes
   pull requests, so when the changelog changed the target commits it on
   `changelog/<version>`, opens the pull request and stops; merge it, pull, and run the
   same command again: with nothing left to commit it creates the annotated tag and
   pushes it.
2. CI (`release` job, after `checks`, `lint`, `vuln` and `test`) runs `make release
   VERSION=<tag>`: six static binaries `tailraid_{linux,darwin,windows}_{amd64,arm64}[.exe]`
   built with `-trimpath -mod=vendor -ldflags "-s -w -X main.version=<tag>"` from the
   committed `vendor/` tree (no module download at release time), plus `sha256sums.txt`
   over them, attached to a GitHub release named after the tag whose notes are that tag's
   `CHANGELOG.md` section, together with `sha256sums.txt.minisig`: the release job signs
   the sums file with `cmd/tailraid-sign` under the key in the `MINISIGN_SECRET_KEY`
   secret (a minisign unencrypted secret key file, the output of `tailraid-sign -gen`).
   A release without the secret fails; nothing unsigned is ever published.
3. Every artifact answers `tailraid version` with the tag, which is what the updater and
   `status` (`VERSION` column) show.

**Signing (I-128).** The signature is minisign's prehashed form: ed25519 over the
BLAKE2b-512 of `sha256sums.txt`, with a trusted comment (`timestamp:<unix>\tfile:<name>`)
bound by a global signature over signature and comment. `cmd/tailraid-sign` makes the
key pair (`-gen`), signs (`-sign`) and verifies (`-verify`); the formats are minisign's
own, so `minisign -Vm sha256sums.txt -P <key>` verifies the same file. The public key is
the constant `internal/update.ReleaseKey`:

```
RWQZh3kca5j1N8FXgFtm3kxOE8RBttUOd0MlVyZcg3Va4XXCkDWxw+E+
```

To rotate it: generate a new pair, replace the constant and the CI secret in one release,
and update every node once more under the old key first (an old binary trusts only the
old key).

Versions are plain semver with a `v` prefix; a build from a commit prints
`<short-sha>[-dirty]` and is never offered as an update target.

## 5. Self-update

`tailraid update [--check] [--version vX.Y.Z] [--allow-downgrade] [--yes] [--restart]
[--rollback]` replaces the running binary with a release artifact (`internal/update`,
I-128):

1. Resolve the release: `GET /repos/zombar/tailraid/releases/latest` (or `/tags/<v>`),
   anonymously (the repository is public). A token from `TAILRAID_GITHUB_TOKEN`, else
   `GITHUB_TOKEN`, else `gh auth token` when the `gh` CLI is installed, is sent when
   present: a private fork needs one, and with one the asset is fetched through the API
   URL instead of the browser URL. The asset is `tailraid_<GOOS>_<GOARCH>[.exe]`.
2. Compare: a release older than or equal to the running semver is not applied unless
   `--version` names it explicitly, and a release older than the running one also needs
   `--allow-downgrade` (an older node cannot read a newer snapshot, §6); a non-semver
   build (`dev`, `<sha>`) always updates.
3. Fetch `sha256sums.txt` and `sha256sums.txt.minisig` from the same release and verify
   the signature under `ReleaseKey` (§4); a release without a signature, or a build
   without an embedded key, is refused before anything is downloaded.
4. Download the asset into a temp file in the executable's directory (same filesystem,
   so the swap is a rename), require its size to equal the asset's announced size and its
   SHA-256 to equal the entry for that asset name, and fsync it. A mismatch deletes the
   download and fails.
5. Run the downloaded file with `version` and require it to print the release tag.
6. Swap atomically: rename the running binary to `<exe>.old` (Windows cannot overwrite a
   running image; Unix could but the same path keeps the code identical), rename the
   download into place. `.old` stays: `serve` removes it a minute into a clean start, and
   `tailraid update --rollback` puts it back before then (or until the next update).
7. Print how to restart the service (`tailraid stop && tailraid start`, or `sc.exe`), or
   do it when `--restart` is given.

`--check` stops after step 2 and exits 2 when an update is available.

## 6. Mixed versions

`node ls` and `status` show in VERSION what each node's `serve` runs: a node reports its
version into its own row at start (02 §Lifecycle commands, I-133).

Log entries carry codec version 1, 2 or 3 and snapshots version 3 (02 §Codec versions;
every older version is still read). `/health` reports the newest version a node writes
(`meta.CodecVersion`), `join` refuses a cluster whose node reports a newer one, and a
node that meets an entry it cannot decode counts it (`status` shows
`incompatible_entries` and a warning) and logs it once a minute (I-105). A release that adds a rule the FSM enforces (a name longer than 255 bytes, a
chunk size above 64 MiB, a Commit past `MaxFileSize`), an op (`SetVolumeMode`, 02) or a
snapshot field can reject what an older node wrote or sends, so upgrade every node
(`tailraid update`, §5) before relying on the new feature; every such rule is also checked
by the gateway before it proposes, so a cluster running one version never produces an
entry another node of that version rejects. The first snapshot an upgraded node writes is
version 2, which an un-upgraded node cannot restore: upgrade the whole cluster in one go.

## 7. Day one: `tailraid init`

`tailraid init` takes no required options: the node is named after the host and the
volume starts at `1+0` in automatic redundancy mode (03), growing as disks join.
`tailraid join <host> <token>` is the same for a second device: `<host>` is the first
device's Tailscale IP, or its name as `tailscale status` shows it (host name or MagicDNS
name), which `join` looks up in the tailnet status because a plain DNS lookup gives a LAN
address no node listens on (I-50); an address off the tailnet is refused with the hint.
`--voter` stays explicit
because only the operator knows which devices are always on. `--name`, `--chunk` and
`--token` remain for overrides and test rigs; `--k`/`--m` on `init` pin a manual policy
for test rigs and are hidden. Disks listed under `node.disks` in the config are marked and
registered by `init` and by `join` (I-121), each with its derived budget (01); later ones
are added with `tailraid node add-disk <path>` (14 §2), and the automatic policy counts
them (a single machine with four disks runs 2+2). A device with no disks is told so
(`this device stores no data: tailraid node add-disk <path>`) and serves the volume as a
diskless host (14 §9).

**Validation.** `Load` expands a leading `~` in every path (`node.data_dir`, the disks,
`mount.path`, `log.file`) and `Validate` refuses a listener port outside 1..65535 or
shared between `listen.raft`, `listen.rpc`, `listen.nfs` and `listen.web`, an unknown
`log.level` or `log.format`, and a relative `mount.path`; `serve` warns at start when the
config file or the data directory is readable by other users of the machine (I-126).

## 8. What `status` reports about the process

Besides the cluster view (docs/06 §7), `tailraid status --json` carries what a long write
needs watched: `process.heap_inuse`, `process.sys` and `process.goroutines` from the Go
runtime, and on the gateway `gateway.plan_bytes` (dirty bytes in flushes under way),
`gateway.flush_bytes` (chunk and shard buffers held by the flusher, docs/04 §9),
`gateway.wal_cap` and `gateway.last_flush_err`, and the reader's readahead (docs/04 §5):
`gateway.readahead_fetched` (chunks a prefetch fetched), `gateway.readahead_joined`
(client READs served by a chunk a prefetch fetched or was fetching) and
`gateway.prefetching` (prefetches in flight right now). The soak sampler records them every second
and the report charts the heap next to the WAL depth, so a memory regression under a
large copy is a visible line, not a guess (I-84).

**Machine vitals.** `status --json` also carries `machine`, what the operating system
exposes about the node without a C library (the binary stays pure Go): the process's CPU
seconds (every OS) and its resident set (Linux; its peak on macOS), the machine's load
average, total and available memory, the data disk's free and total bytes, and on Linux
the machine's busy and total CPU ticks, the memory-pressure stall share
(`/proc/pressure/memory`, `some avg10`) and the cumulative bytes written and read on
every disk. Every field is a real measurement or absent: a value the OS does not expose
is omitted from the JSON, never reported as zero (I-85). The sampler turns the cumulative
counters into rates between its ticks, and the report draws CPU, memory, memory pressure
and disk charts for the sampled node, with peaks in the totals table.

**Readiness.** `/health` and `status` carry `ready`: the node knows a leader and Raft has
handed its FSM every entry it has committed. A freshly restarted node is not ready
until its replay is done, an isolated follower stops being ready when it loses its
leader, and `tailraid status` exits 1 while the node it asked is not ready, so a
supervisor or a script can wait on it (I-125).

## 9. Backup, restore and quorum loss

The metadata (file names, chunk hashes, placements, the member table) is replicated on
the voters and lives in each one's Raft log and snapshots under `<data_dir>/raft`; the
data is the shards on every device and disk. Losing any `m` targets is survived (05 §3)
and needs nothing from this section.

**Backup.** `tailraid snapshot <dir>` asks the local node for a fresh Raft snapshot
(`/v1/admin` op `snapshot`, which the node writes under `<data_dir>/export/<id>/` with
`state.bin` and `meta.json`, each fsynced) and copies it to `<dir>/<id>/`. It is small
(the metadata only) and a copy outside the cluster is worth keeping; `state.bin` is the
`DecodeState` format of 02, so a future `fsck-from-snapshot` can read it. Shards are not
part of it.

**Quorum loss (05 (h)).** When more voters are gone for good than Raft can lose, the
survivors can never elect a leader: every gateway goes stale and read-only. On one
surviving voter, with its service stopped, `tailraid recover --voters <id>[,<id>…]`
rewrites that node's Raft membership to the voters named (this node must be one of them;
IDs as `node ls` shows them) using hashicorp/raft's `RecoverCluster`: the node's newest
snapshot and the log after it are replayed into a fresh FSM, a snapshot carrying the new
configuration is written and the log truncated, so the next `serve` starts as the leader
of that set with every file it knew. Files whose shards survived on the remaining targets
read exactly; a file whose only shards died with the lost nodes is `EIO`, never garbage;
the member table still lists the lost nodes until they are removed or `reset` and re-join.
`recover` refuses to run while `serve` holds the data directory, on an uninitialised
directory, and when the list leaves this node out (I-129). Run it on one node only: two
recovered nodes are two clusters.

## Invariants

- **I-84** — `status` reports the process's heap in use, system memory and goroutine count, and the gateway's plan bytes, flush buffer bytes, WAL cap and last flush error; the soak sampler records them and the report charts the heap.
- **I-85** — Every machine vital in `status` is a measurement the operating system exposes to a pure-Go binary (process CPU seconds and resident set, load average, total and available memory, data-disk free and total, and on Linux machine CPU ticks, memory pressure and disk bytes); a vital the OS does not expose is absent from the report, never zero, and cumulative counters never decrease between two reports from one process.
- **I-59** — A `/dev/fault` drop rule for a peer stops Raft dials to it, fails the Raft connections already open with it, stops incoming Raft connections from it, outbound RPC to it and inbound RPC from it on the node that holds the rule; an empty rule set restores all four paths.
- **I-60** — With `log.file` set, every line `serve` logs is appended to that file in the configured format and nothing is written to stderr; the service units route stdout and stderr to the same file or to journald, never to nowhere; every command other than `serve` logs to stderr and never opens the file.
- **I-61** — `update` replaces the binary only with an artifact whose SHA-256 matches the `sha256sums.txt` of the same release and which reports that release's version when run; on any failure the running binary is untouched and no partial file is left in its directory; a release that is not newer than a semver build is applied only when named explicitly.

- **I-76** — `tailraid init` with no options bootstraps a cluster whose node is named
  after the host, whose device row carries no budget and whose volume is `1+0` with `Auto`
  set; `tailraid join <host> <token>` with no options joins as a non-voter, resolving a
  device name through the tailnet status and refusing an address off the tailnet; both register
  the configured disks with derived budgets (I-74, I-121).
- **I-122** — The Linux unit runs with `NoNewPrivileges`, `ProtectSystem=strict`,
  `ProtectHome=read-only` and `PrivateTmp`, and its `ReadWritePaths` names exactly the
  data directory, every added disk, the config directory and the log file's directory,
  with every path quoted; the installer stages the unit only in a directory it created;
  the LaunchAgent restarts the service only after a non-zero exit.
- **I-123** — On the first interrupt a leader transfers leadership before its Raft
  stops, bounded by 5 s, so another node leads the moment the shutdown proceeds; a
  second interrupt ends the process at once; the Windows service handler reports a stop
  wait hint of 45 s and the unit's `TimeoutStopSec` is 45.
- **I-124** — With `log.file` set the file rotates before a write would push it past
  64 MiB, keeping five generations and dropping the oldest, and reopens on SIGHUP;
  `logs -f` prints every appended line exactly once; a per-item warning site emits at
  most one line a minute and reports how many it suppressed.
- **I-125** — `/health` and `status` report `ready` iff the node knows a leader and
  Raft's applied index has reached its commit index; a closed node is never ready;
  `tailraid status` exits 1 when the node it asked is not ready.
- **I-126** — `Load` expands a leading `~` in `node.data_dir`, the disks' paths,
  `mount.path` and `log.file`; `Validate` refuses a listener port outside 1..65535 or
  shared by two listeners, an unknown `log.level` or `log.format` and a relative
  `mount.path`; `serve` warns when the config file or the data directory is readable by
  group or others.
- **I-128** — `update` applies an artifact only when the release's `sha256sums.txt`
  carries a `sha256sums.txt.minisig` that verifies under `ReleaseKey` (minisign's
  prehashed ed25519 over BLAKE2b-512, the trusted comment bound by the global
  signature), the download's size equals the asset's announced size and its SHA-256 the
  sums entry, and the temp file is fsynced before the swap; a release without a
  signature or a build without a key is refused before any download; the previous
  binary stays as `<exe>.old` until a minute into a clean `serve` or until
  `update --rollback` restores it; a release older than the running one needs
  `--version` and `--allow-downgrade`.
- **I-129** — `tailraid snapshot` exports a fresh Raft snapshot whose `state.bin`
  decodes to the node's current metadata; `tailraid recover --voters` on a stopped node
  rewrites its Raft membership to the voters named so it restarts as their leader with
  every entry it had, every file whose shards survive readable and the rest `EIO`; it
  refuses a running node, an uninitialised directory and a list without this node.
- **I-135** — A `serve` whose data directory holds a snapshot with its own row and the
  volume starts serving without a leader: after giving one five seconds its RPC answers
  with `ready: false`, its gateways serve stale, and it is ready once a leader connects;
  only a node without that state waits for the log, bounded, and exits when none arrives.
  `init`, `join`, every `serve` once ready and every clean shutdown take a snapshot, so
  the state is there from a node's first ready start on, leader or not.
- **I-144** — A node waiting for the log at start waits until its FSM holds its own
  row and the volume, not only until it knows a leader and has applied its local log;
  it fails with `ErrNotMember` when they have not arrived within the bound.
- **I-131** — `tailraid stop` returns only once the service manager reports no process
  (`launchctl print` not running, `sc.exe query` STOPPED, `systemctl stop` returned),
  bounded by 60 s, and succeeds when there was no process to stop; `update --restart` and
  `stop && start` therefore start the new binary.

## Tests

| ID | Tests |
|---|---|
| I-59 | `TestFaults_DropRulesCutRaftAndRPC`, `TestFaults_RuleFailsOpenRaftConnection` |
| I-60 | `TestConfig_LogFile`, `TestSvc_UnitsRouteLogs` |
| I-61 | `FuzzParseSums`, `TestUpdate_AppliesVerifiedRelease`, `TestUpdate_RefusesBadChecksumAndOlder` |
| I-76 | `TestCLI_Init_NoFlags`, `TestCLI_Join_NoFlags`, `TestResolveJoinTarget_Table` |
| I-84 | `TestReport_ProcessAndFlushFields`, `TestSoak_AllPhasesAgainstHarness` |
| I-85 | `TestVitals_RealMeasurementsOrAbsent`, `TestSoak_AllPhasesAgainstHarness` |
| I-122 | `TestSvc_SystemdHardeningAndQuoting`, `TestSvc_UnitsRouteLogs` |
| I-123 | `TestCluster_ShutdownTransfersLeadership` |
| I-124 | `TestLogFile_RotatesAndReopens`, `TestLimiter_OneLinePerInterval` |
| I-125 | `TestHealth_Ready`, `TestCluster_Ready_FollowsLeaderContact` |
| I-126 | `TestConfig_Validate_Table`, `TestConfig_ExpandHome`, `TestConfig_Warnings_Permissions` |
| I-128 | `TestMinisign_SignVerifyAndTamper`, `TestMinisign_FileShape`, `TestUpdate_AppliesVerifiedRelease`, `TestUpdate_RefusesBadChecksumAndOlder` |
| I-129 | `TestRecover_RewritesVotersOffline`, `TestCluster_RecoverAfterLostMajority` |
| I-131 | `TestSvc_Stopped_Table`, `TestSvc_StopWaitsForExit` |
| I-135 | `TestCLI_Serve_WithoutLeader_StartsStale`, `TestCLI_Serve_InitNode_WithoutQuorum_StartsStale` |
| I-144 | `TestWaitMember_WaitsForOwnRow` |
