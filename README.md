<p align="center">
  <picture>
    <source media="(prefers-color-scheme: dark)" srcset="docs/assets/logo-dark.svg">
    <img src="docs/assets/logo-light.svg" alt="tailraid" width="300">
  </picture>
</p>

tailraid turns the devices on your tailnet into one shared volume that behaves like a
RAID array. Files are cut into chunks, each chunk is erasure-coded into `k` data and `m`
parity shards on different devices, so any `m` devices can be off, lost or corrupted and
the volume stays readable and writable. Identical content is stored once. The volume
mounts as a normal filesystem on macOS, Linux and Windows through the OS's own NFS client.
One static binary is the node daemon, the gateway and the management CLI; metadata is
Raft-replicated across your always-on devices, and data never leaves your tailnet.

> [!IMPORTANT]
> Every device runs Tailscale (headscale works; tailraid only shells out to the
> `tailscale` CLI). Give the metadata an odd number of always-on **voters** — one to start,
> three is the sensible shape; laptops join as non-voters and come and go. Windows needs
> Pro/Enterprise/Education (Home has no NFS client); the Windows mount path is
> experimental, see [docs/windows-checklist.md](docs/windows-checklist.md).

## Install

Download the binary for your OS/arch from the [releases](https://github.com/zombar/tailraid/releases)
(six static binaries plus `sha256sums.txt`) and put it on your `PATH`, or build it:

```sh
git clone https://github.com/zombar/tailraid.git && cd tailraid
make install                     # → ~/.local/bin/tailraid (Go 1.27+, no C toolchain)
```

`tailraid update` replaces the binary with the latest release after verifying its
checksum. The repository is private: set `GITHUB_TOKEN` or be logged in with `gh`.

## First use

On the always-on device that will hold the first metadata copy:

```sh
tailraid init --k 2 --m 1 --name studio --capacity 500G    # creates the volume, prints the join token
tailraid install                                           # runs `serve` under launchd / systemd / SCM
tailraid mount                                             # ~/TailRAID (T: on Windows)
```

On every other device:

```sh
tailraid join 100.64.0.11 <token> --capacity 200G          # --voter on other always-on boxes
tailraid install
tailraid mount
```

Write on one device, read on any other. `tailraid status` shows the cluster.

Choosing `k` and `m`: `2+1` survives one lost device at 1.5× storage, `3+2` survives two
at 1.67×, `1+2` is three plain copies. You need at least `k+m` live devices to write at
full redundancy; below that, writes still succeed and are brought back to full redundancy
when devices return. The policy can be changed any time with `tailraid volume
set-redundancy`; existing data migrates in the background. The chunk size (1 MiB by
default) is fixed at `init`.

## Configuration

`init` and `join` write the config file: `~/Library/Application Support/tailraid/config.yaml`
on macOS, `~/.config/tailraid/config.yaml` on Linux, `%ProgramData%\tailraid\config.yaml`
on Windows (`$TAILRAID_HOME` overrides the directory). The keys you may want to touch:

```yaml
node:
  capacity: 500G          # shard-byte budget on this device, also its placement weight
  voter: true             # always-on devices only
listen:
  raft: 20480             # tailnet TCP
  rpc: 20481              # tailnet TCP
  web: 20482              # tailnet TCP; the browser UI, 0 disables it
  nfs: 127.0.0.1:20490    # loopback only (2049 on Windows)
mount:
  path: ~/TailRAID
repair:
  grace: 10m              # deleted data is reclaimed after this
  fill_after: 10m         # a dead device's shards are re-placed after this (laptop lids!)
log:
  level: info             # debug | info | warn | error
  format: text            # json | text
  file: ""                # append to a file instead of stderr
```

Environment overrides: `TAILRAID_LOG_LEVEL`, `TAILRAID_LOG_FORMAT`, `TAILRAID_LOG_FILE`.

## Day to day

```sh
tailraid status                        # health, leader, members, usage, warnings
tailraid check                         # replication health; exit 1 if anything is degraded
tailraid repair                        # repair pass now (it also runs every minute on the leader)
tailraid chunk describe /path/or/hash  # state, placement and history of a file's chunks
tailraid volume describe               # k+m, chunk size, stored vs logical bytes, dedup, headroom
tailraid volume set-redundancy --k 3 --m 2
tailraid node drain oldbox && tailraid node remove oldbox
tailraid logs -f                       # the service log
```

Every command takes `--json` and `--node <ip>` to address another device.

## Browser UI

Every device that runs `serve` also serves a small web page on its Tailscale IP,
`http://<tsIP>:20482/` (`tailraid status` prints it): a dashboard of data balance and
integrity, and a file browser that streams audio, video and images, uploads files and makes
folders. It is open to the cluster's devices and to any tailnet device logged in as the same
user, so a phone works; other users' devices and tagged servers are refused. Cluster
administration stays in the CLI. Design in [docs/13](docs/13-web-ui.md).

## Verifying your cluster

```sh
tailraid verify --report out.html
```

runs the soak scenarios (transfer, cross-device integrity, random-access churn, dedup
and garbage collection, metadata agreement across every node) against your live cluster
through this device's mount and writes a self-contained HTML report with charts; add
`--destructive --restarts` to include corruption, node kills and partitions on devices
that run with `dev.enabled`. The same scenarios run nightly against a four-node Docker
cluster in CI, including parity loss, leader kills, redundancy changes and restart storms.

**[Latest report](docs/reports/latest.html)** (download and open; GitHub does not render
HTML).

## Releasing

A release is a tag on `main`; CI builds and publishes it. From an up-to-date `main`:

```sh
make tag VERSION=v0.2.0
```

The target refuses a dirty tree or an existing tag, runs `make check`, and regenerates
`CHANGELOG.md` from the commit subjects since the last tag (so write subjects as
`scope: what changed`). `main` only takes pull requests, so the first run commits the
changelog on `changelog/<version>`, opens that pull request and stops; merge it, `git
pull`, and run the same command again. With nothing left to commit it creates the
annotated tag and pushes it, which starts the `release` job: six static binaries
(`linux`, `darwin`, `windows` × `amd64`, `arm64`), `sha256sums.txt`, and a GitHub release
named after the tag with that version's changelog section as the notes. Every binary
answers `tailraid version` with the tag, and `tailraid update` on a device offers it.
Versions are semver with a `v` prefix; a build from a bare commit prints a short SHA and
is never offered as an update. Details in [docs/12-operations.md](docs/12-operations.md) §4.

## Documentation

- [docs/README.md](docs/README.md) — the design documents in reading order: data model,
  consistency and Raft, erasure coding and placement, the write path and durability
  contract, failure model, repair and GC, the NFS gateway, security, testing, operations.
- [docs/05-failure-model.md](docs/05-failure-model.md) — exactly what "no data loss"
  covers and what it does not.
- [docs/12-operations.md](docs/12-operations.md) — service install per OS, logs, the
  Windows portmap stub, fault injection, releases and self-update.
- [docs/invariants.md](docs/invariants.md) and [docs/lifecycle.md](docs/lifecycle.md) —
  every promise the system makes and the tests that prove it.
- [CLAUDE.md](CLAUDE.md) — how the code is written and tested.
