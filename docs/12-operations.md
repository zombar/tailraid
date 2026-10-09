# 12 — Operations: service install, logs, Windows portmap, fault injection, releases

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
| macOS | LaunchAgent `~/Library/LaunchAgents/app.tailraid.serve.plist` (per user: Tailscale's GUI session and the mount are per user) | `launchctl bootstrap gui/$UID …` / `bootout`; `kickstart -k` / `kill SIGTERM` | `KeepAlive`, `ThrottleInterval 5` | `~/Library/Logs/tailraid/serve.log` (`StandardOutPath`/`StandardErrorPath`) |
| Linux | system unit `/etc/systemd/system/tailraid.service`, `User=<installing user>`, `Environment=TAILRAID_HOME=…`, `After=network-online.target tailscaled.service` | `systemctl daemon-reload`, `enable --now`, `stop`, `disable --now` (via `sudo` when not root) | `Restart=always`, `RestartSec=2` | journald: `tailraid logs` runs `journalctl -u tailraid` |
| Windows | SCM service `tailraid` (`sc.exe create … binPath= "<exe> serve --config … --service" start= delayed-auto depend= Tailscale`), `sc.exe failure … actions= restart/5000` | `sc.exe start|stop|delete` (elevated) | SCM failure actions | `%ProgramData%\tailraid\serve.log` (`log.file`): services have no stderr |

`log.file` in the config (or `TAILRAID_LOG_FILE`) makes `serve` append to a file in the
configured format instead of stderr; `install` sets it on macOS and Windows. `tailraid logs
[-f] [-n N]` prints or follows the configured file, or journald on Linux.

On Windows `serve --service` runs under the service control manager (start/stop
handshake, `SERVICE_RUNNING`); without the flag it is an ordinary console process. The
service runs as LocalSystem, whose config home is `%ProgramData%\tailraid` — the same
`config.Home()` an elevated `join` used, so no copy step.

Mounting stays a user action (`tailraid mount`) on every OS: the LaunchAgent and the
systemd unit only serve. The mount survives service restarts because handles are stable
(I-16) and the mounts are `soft`.

The service log prints the browser UI address (`http://<tsIP>:20482/`, docs/13) at start,
and `tailraid status` shows it on its `web` line; `listen.web: 0` turns the UI off.

**Address changes.** When the device's Tailscale IP changes, `serve` re-registers the new
address in the node table (every peer dials it from then on) and exits with a non-zero
status, because its Raft and RPC listeners are bound to the old address; the service unit's
restart policy brings it back on the new one within seconds. Without a service manager,
restart `tailraid serve` by hand.

## 2. Windows portmap stub

`mount.exe` has no port option: it asks the portmapper on 111 where MOUNT (100005 v3)
and NFS (100003 v3) live. On Windows `serve` therefore binds the gateway on
`127.0.0.1:2049` and runs `internal/mount.Portmap` on `127.0.0.1:111`, TCP (record
marking) and UDP, answering ONC RPC v2 program 100000 version 2:

| Proc | Reply |
|---|---|
| 0 `NULL` | void |
| 3 `GETPORT` | `2049` for prog ∈ {100003, 100005}, vers 3, prot TCP (6); `0` otherwise |
| 4 `DUMP` | the two mappings |
| other | `PROC_UNAVAIL` |

Nothing else is implemented (no SET/UNSET, no v3/v4 rpcbind). The stub is loopback-only
and refuses any other bind address. It is compiled on every OS so its tests run everywhere;
`serve` starts it only on Windows.

## 3. Fault injection on a live cluster (`dev.enabled`)

Five endpoints (docs/08) turn a real node into a soak target:

- `POST /dev/exit` replies `204` and exits with status 3 after 100 ms. With a service
  manager that restarts the process this is "kill a node".
- `POST /dev/fault` with `{"drop":["ip",…]}` installs drop rules for the listed peers
  on that node: the Raft stream layer refuses to dial them and closes their incoming
  connections, the node RPC client refuses to send to them, and inbound RPC from them is
  answered `503`. An empty list heals. `GET /dev/fault` returns the rules. Rules live in
  memory only; a restart heals.
- `GET /dev/goroutines` returns every goroutine's stack. `verify` and the compose driver
  take it from every node after a failed phase and attach it to the report, so a node
  that hangs is diagnosed from the report rather than by sending it SIGQUIT.
- `GET /dev/disk` returns the data disk as the budget sees it (`{used, free, total}`);
  `POST /dev/fill?bytes=N` writes a scratch file of N bytes in the data directory (0
  removes it), so the soak suite's automation phase can move a node's free space and
  watch its budget follow (docs/01 §Node budget).

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
   `CHANGELOG.md` section.
3. Every artifact answers `tailraid version` with the tag, which is what the updater and
   `status` (`VERSION` column) show.

Versions are plain semver with a `v` prefix; a build from a commit prints
`<short-sha>[-dirty]` and is never offered as an update target.

## 5. Self-update

`tailraid update [--check] [--version vX.Y.Z] [--yes]` replaces the running binary with a
release artifact (`internal/update`):

1. Resolve the release: `GET /repos/zombar/tailraid/releases/latest` (or `/tags/<v>`),
   authenticated with `TAILRAID_GITHUB_TOKEN`, else `GITHUB_TOKEN`, else `gh auth token`
   when the `gh` CLI is installed (the repository is private; a public repository needs
   none). The asset is `tailraid_<GOOS>_<GOARCH>[.exe]`.
2. Compare: a release older than or equal to the running semver is not applied unless
   `--version` names it explicitly; a non-semver build (`dev`, `<sha>`) always updates.
3. Download the asset and `sha256sums.txt` from the same release, into a temp file in the
   executable's directory (same filesystem, so the swap is a rename), and verify the SHA-256
   against the entry for that asset name. A mismatch deletes the download and fails.
4. Run the downloaded file with `version` and require it to print the release tag.
5. Swap atomically: rename the running binary to `<exe>.old` (Windows cannot overwrite a
   running image; Unix could but the same path keeps the code identical), rename the
   download into place, remove `.old` (left behind on Windows until the next run).
6. Print how to restart the service (`tailraid stop && tailraid start`, or `sc.exe`); the
   running process is not restarted by `update`.

`--check` stops after step 2 and exits 2 when an update is available.

## 6. Mixed versions

Log entries carry codec version 1 and snapshots version 2 (01; a version-1 snapshot is
still read). A release that adds a rule the FSM enforces (a name longer than 255 bytes, a
chunk size above 64 MiB, a Commit past `MaxFileSize`), an op (`SetVolumeMode`, 02) or a
snapshot field can reject what an older node wrote or sends, so upgrade every node
(`tailraid update`, §5) before relying on the new feature; every such rule is also checked
by the gateway before it proposes, so a cluster running one version never produces an
entry another node of that version rejects. The first snapshot an upgraded node writes is
version 2, which an un-upgraded node cannot restore: upgrade the whole cluster in one go.

## 7. Day one: `tailraid init`

`tailraid init` takes no required options: the node is named after the host, its budget
is derived from the data directory's free space (01) and the volume starts at `1+0` in
automatic redundancy mode (03), growing as devices join. `tailraid join <host> <token>`
likewise derives the budget; `--voter` stays explicit because only the operator knows
which devices are always on. `--name`, `--chunk` and `--token` remain for overrides and
test rigs; `--k`/`--m` on `init` pin a manual policy for test rigs and are hidden. Disks
listed under `node.disks` in the config are marked and registered by `init`; later ones
are added with `tailraid node add-disk <path>` (14 §2), and the automatic policy counts
them (a single machine with four disks runs 2+2).

## 8. What `status` reports about the process

Besides the cluster view (docs/06 §7), `tailraid status --json` carries what a long write
needs watched: `process.heap_inuse`, `process.sys` and `process.goroutines` from the Go
runtime, and on the gateway `gateway.plan_bytes` (dirty bytes in flushes under way),
`gateway.flush_bytes` (chunk and shard buffers held by the flusher, docs/04 §9),
`gateway.wal_cap` and `gateway.last_flush_err`. The soak sampler records them every second
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

## Invariants

- **I-84** — `status` reports the process's heap in use, system memory and goroutine count, and the gateway's plan bytes, flush buffer bytes, WAL cap and last flush error; the soak sampler records them and the report charts the heap.
- **I-85** — Every machine vital in `status` is a measurement the operating system exposes to a pure-Go binary (process CPU seconds and resident set, load average, total and available memory, data-disk free and total, and on Linux machine CPU ticks, memory pressure and disk bytes); a vital the OS does not expose is absent from the report, never zero, and cumulative counters never decrease between two reports from one process.
- **I-58** — The portmap stub answers `GETPORT` for programs 100003 and 100005 version 3 over TCP with the NFS gateway port and `0` for any other mapping, serves `NULL` and `DUMP`, answers `PROC_UNAVAIL` for other procedures, and binds loopback only.
- **I-59** — A `/dev/fault` drop rule for a peer stops Raft dials to it, incoming Raft connections from it, outbound RPC to it and inbound RPC from it on the node that holds the rule; an empty rule set restores all four paths.
- **I-60** — With `log.file` set, every line `serve` logs is appended to that file in the configured format and nothing is written to stderr; the service units route stdout and stderr to the same file or to journald, never to nowhere.
- **I-61** — `update` replaces the binary only with an artifact whose SHA-256 matches the `sha256sums.txt` of the same release and which reports that release's version when run; on any failure the running binary is untouched and no partial file is left in its directory; a release that is not newer than a semver build is applied only when named explicitly.

- **I-76** — `tailraid init` with no options bootstraps a cluster whose node is named
  after the host, whose budget is derived (I-74) and whose volume is `1+0` with `Auto`
  set; `tailraid join <host> <token>` with no options joins as a non-voter with a derived
  budget.

## Tests

| ID | Tests |
|---|---|
| I-58 | `TestPortmap_GetportDumpNull`, `TestPortmap_RefusesNonLoopback` |
| I-59 | `TestFaults_DropRulesCutRaftAndRPC` |
| I-60 | `TestConfig_LogFile`, `TestSvc_UnitsRouteLogs` |
| I-61 | `TestUpdate_AppliesVerifiedRelease`, `TestUpdate_RefusesBadChecksumAndOlder` |
| I-76 | `TestCLI_Init_NoFlags`, `TestCLI_Join_NoFlags` |
| I-84 | `TestReport_ProcessAndFlushFields`, `TestSoak_AllPhasesAgainstHarness` |
| I-85 | `TestVitals_RealMeasurementsOrAbsent`, `TestSoak_AllPhasesAgainstHarness` |
