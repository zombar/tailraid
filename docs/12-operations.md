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

Two endpoints (docs/08) turn a real node into a soak target:

- `POST /dev/exit` replies `204` and exits with status 3 after 100 ms. With a service
  manager that restarts the process this is "kill a node".
- `POST /dev/fault` with `{"drop":["ip",…]}` installs drop rules for the listed peers
  on that node: the Raft stream layer refuses to dial them and closes their incoming
  connections, the node RPC client refuses to send to them, and inbound RPC from them is
  answered `503`. An empty list heals. `GET /dev/fault` returns the rules. Rules live in
  memory only; a restart heals.

`tailraid verify --destructive --restarts` declares that every target runs with
`dev.enabled` under a restarting service manager: the parity, consensus, restart-storm
and partition steps then run against the real cluster through those endpoints.

## 4. Releases

`make release` cross-compiles `tailraid_{linux,darwin,windows}_{amd64,arm64}` with
`-trimpath -ldflags "-s -w -X main.version=<tag>"` and writes `sha256sums.txt`; CI runs
it on tags `v*` and attaches the seven files to the GitHub release. Every artifact prints
its version with `tailraid version`.

## Invariants

- **I-58** — The portmap stub answers `GETPORT` for programs 100003 and 100005 version 3 over TCP with the NFS gateway port and `0` for any other mapping, serves `NULL` and `DUMP`, answers `PROC_UNAVAIL` for other procedures, and binds loopback only.
- **I-59** — A `/dev/fault` drop rule for a peer stops Raft dials to it, incoming Raft connections from it, outbound RPC to it and inbound RPC from it on the node that holds the rule; an empty rule set restores all four paths.
- **I-60** — With `log.file` set, every line `serve` logs is appended to that file in the configured format and nothing is written to stderr; the service units route stdout and stderr to the same file or to journald, never to nowhere.

## Tests

| ID | Tests |
|---|---|
| I-58 | `TestPortmap_GetportDumpNull`, `TestPortmap_RefusesNonLoopback` |
| I-59 | `TestFaults_DropRulesCutRaftAndRPC` |
| I-60 | `TestConfig_LogFile`, `TestSvc_UnitsRouteLogs` |
