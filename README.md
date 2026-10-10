<p align="center">
  <picture>
    <source media="(prefers-color-scheme: dark)" srcset="docs/assets/logo-dark.svg">
    <img src="docs/assets/logo-light.svg" alt="tailraid" width="300">
  </picture>
</p>

tailraid turns the devices on your tailnet into one shared volume that behaves like a RAID array.

It is redundant: files are split into chunks, each chunk is erasure-coded across devices and disks, and any `m` of them can fail while the volume keeps working. It is deduplicated: identical content is stored once. It is mountable: the volume appears as a normal folder on macOS and Linux through the OS's own NFS client, and on Windows through its SMB client. And it is one binary: the same static executable is the daemon, the gateway and the CLI, and data never leaves your tailnet.

> [!IMPORTANT]
> Every device needs Tailscale (headscale works). Windows needs Windows 11 24H2 or newer (its SMB client can be told the port tailraid listens on); the Windows mount is experimental, see [docs/windows-checklist.md](docs/windows-checklist.md).

## Install

Download a binary from the [releases](https://github.com/zombar/tailraid/releases) and put it on your `PATH`, or build it:

```sh
git clone https://github.com/zombar/tailraid.git && cd tailraid
make install          # → ~/.local/bin/tailraid (needs Go 1.27.2+)
```

`tailraid update` installs the latest release; no token or login is needed. See [Verifying a download](#verifying-a-download) before trusting a binary you fetched by hand.

## Set up a volume

On the first always-on device:

```sh
tailraid init         # creates the volume and prints a join token
tailraid install      # runs tailraid as a service (launchd / systemd / Windows service)
tailraid mount        # ~/TailRAID (T: on Windows)
```

On each other device:

```sh
tailraid join 100.64.0.11 <token>     # the first device's Tailscale IP or name; add --voter on other always-on devices
tailraid install
tailraid mount
```

Write a file on one device and read it on any other. `tailraid status` shows the whole cluster.

## Grow the volume

| To add capacity | Do this |
|---|---|
| Add a device | `tailraid join <ip> <token>` on the new device |
| Add a disk to a device | `tailraid node add-disk /path/to/empty/dir` on that device |
| Add an S3 bucket off-site | `tailraid node add-s3 <name> …` on one device, `tailraid node keys <name> …` on the others |
| Grow a device or disk in place | See below |

`tailraid volume describe` shows the volume's raw, usable and free capacity.

New data spreads over the new space straight away. Existing data stays where it is.

### Growing in place

Each disk has a **budget**: how much of it tailraid may use. When the budget follows free space, which is the default, free up space on the disk or enlarge its partition and the budget grows within a minute with no restart. When the budget is pinned, raise it with one command, which rewrites the config and restarts the service:

```sh
tailraid volume grow jockey/1 500G     # pin the budget of disk 1
tailraid volume grow jockey/1 auto     # follow free space again
```

`tailraid node describe <name>` shows a budget (`capacity`).

### Adding a disk

```sh
tailraid node add-disk /Volumes/Big/tailraid              # budget follows the disk's free space
tailraid node add-disk /Volumes/Big/tailraid --capacity 500G
```

The directory must be empty and should be on a different physical disk. The disk joins as `<device>/1`, `<device>/2` and so on, listed under its device. A single machine with several disks makes a complete volume, with redundancy across its disks. If a disk is unplugged or not mounted, tailraid notices, stops using it and repairs from the other copies.

Details: [docs/14-disks.md](docs/14-disks.md).

### Adding an S3 bucket

A bucket at any S3 provider (Backblaze B2, Cloudflare R2, Hetzner, a MinIO on the tailnet) can hold shards like a disk on a host of its own, which gives the volume an off-site copy. Devices read and write it directly; reads use it only when a device that holds a shard is down.

```sh
export TAILRAID_S3_SECRET_KEY=…            # or type it when asked: never a flag
tailraid node add-s3 offsite --endpoint https://s3.eu-central-003.backblazeb2.com \
  --bucket my-bucket --prefix tailraid --capacity 1T --access-key 003a…
tailraid node keys offsite --access-key 003a…   # on every other device
```

`add-s3` writes, reads back and deletes a test object first, so a key without those rights is refused before anything is registered. The keys stay in `s3nodes.yaml` in each device's data directory and never reach the cluster state. tailraid does not encrypt data at rest, so the provider can read what lands in the bucket. Choose a provider with cheap or free egress: repairs read from the bucket when a device is down.

Give the key to every device, voters above all. A device without it cannot write to the bucket, and a device that leads without it treats the bucket as dead: after `fill_after` it copies the bucket's shards back onto disks, as it would for a disk that went away. Use a key the provider scopes to that one bucket, since it sits on every device.

A bucket holds one shard of every stripe, so it stores 1/k of the volume (half at 2+1, a quarter at 4+2). It is written when you write, probed with a cheap HEAD every 30 s by each device, listed once a day by the leader, and read only while a device holding a needed shard is down. A bucket never votes: it keeps your data safe while a device is away, not the volume writable. For that, pair it with a diskless voter (next section).

Details: [docs/10-s3.md](docs/10-s3.md) §4.

## Shrink the volume

Drain first so the data moves off, then remove:

```sh
tailraid node drain jockey/1      # a disk, or a whole device: node drain oldbox
tailraid node remove jockey/1     # refused while it still holds data
```

Draining a device drains its disks too.

## Redundancy

By default the cluster picks the redundancy (`k+m`) itself and adjusts it as devices and disks come and go:

| Setup | Policy | Survives |
|---|---|---|
| 1 device, no extra disks | 1+0 | nothing |
| 1 device + 3 disks | 2+2 | 2 disks |
| 3 devices | 2+1 | 1 device |
| 4 hosts × 3 disks | 6+2 | 2 disks, or 1 whole host |

To pin it by hand, or to hand it back:

```sh
tailraid volume set-redundancy --k 3 --m 2    # pin: 3 data + 2 parity
tailraid volume set-redundancy --auto         # let the cluster choose again
```

A pinned policy is never changed automatically, even when you add disks. Existing data migrates to a new policy in the background, and `tailraid status` prints what the current policy survives.

## Voters and non-voters

The metadata lives on **voters**. Pick always-on devices for them, in an odd number: 1 to start, 3 is a good size.

Laptops should be **non-voters**. They store data and serve files, but a closed lid never stalls the cluster.

```sh
tailraid node demote laptop       # make a member a non-voter
tailraid node promote nas         # make a member a voter
```

Run these from any member. `tailraid node ls` shows who votes.

### A cheap always-on voter

A voter needs no disk. A small cloud VM on the tailnet, joined with `--voter` and no `disks:` in its config, is a full member and gateway that stores nothing: it keeps the metadata available while your laptops are closed, and its `node ls` row says `no disks`. Two laptops plus such a VM are three voters, so the volume stays writable with the VM and either laptop up. Give it the S3 key when you have a bucket, because the VM is the device most likely to lead.

```sh
tailraid join 100.64.0.11 <token> --voter    # on the VM; nothing under disks: in its config
tailraid install
tailraid node keys offsite --access-key 003a…   # if the volume has a bucket
```

Together, a diskless voter and a bucket give an always-on cluster of rented parts: the VM holds the votes and no data, the bucket holds the data and no votes.

## Everyday commands

```sh
tailraid status                    # health, leader, members, usage, warnings (exit 1 until the node is ready)
tailraid dashboard                 # open the browser UI (--print: just the address)
tailraid node ls                   # devices, their disks and S3 buckets
tailraid check                     # exit 1 if anything is degraded
tailraid repair                    # repair now (it also runs every minute)
tailraid volume describe           # policy, usage, dedup, free space
tailraid chunk describe <path>     # where a file's chunks live
tailraid logs -f                   # the service log
```

Every command takes `--json`, and `--node <ip>` to talk to another device.

## Browser UI

Each device serves a web page at `http://<tailscale-ip>:20482/`, `tailraid status` prints the address, and `tailraid dashboard` opens it. The dashboard shows balance across devices and disks and the volume's integrity; the file browser streams audio, video and images, uploads files and makes folders. Cluster members and your own devices (same Tailscale user) can open it, so your phone works. Changing the cluster stays in the CLI.

## Configuration

`init` and `join` write the config file. Its location depends on the OS:

| OS | Config file |
|---|---|
| macOS | `~/Library/Application Support/tailraid/config.yaml` |
| Linux | `~/.config/tailraid/config.yaml` |
| Windows | `%ProgramData%\tailraid\config.yaml` |

`$TAILRAID_HOME` overrides the directory. The keys you might change:

```yaml
node:
  capacity: 500G        # pin this device's budget (omit to follow free space; "0" stores nothing here)
  voter: true           # always-on devices only
  disks:                # disks listed here before `init` are added by it;
    - path: /Volumes/Big/tailraid   # later ones need: tailraid node add-disk
listen:
  web: 20482            # browser UI port; 0 turns it off
  nfs: 127.0.0.1:20490  # NFS gateway (macOS, Linux); Windows serves SMB on 127.0.0.1:20445 instead
mount:
  path: ~/TailRAID      # ~ is expanded
repair:
  fill_after: 10m       # how long a device may be away before its data is re-copied
log:
  level: info           # debug | info | warn | error
  file: ~/tailraid.log  # optional; rotates at 64 MiB keeping five generations
```

`serve` refuses a config whose ports collide or whose log level it does not know, and warns when the file or the data directory is readable by other users of the machine.

## Check your cluster

```sh
tailraid verify --report out.html
```

This runs the soak scenarios against your live cluster and writes an HTML report: transfer, integrity, churn, dedup, repair and the disk checks. Without `--destructive` it touches nothing but its own files. Add `--destructive --restarts` to also test corruption, policy changes, drains, kills and partitions on devices running with `dev.enabled`; Ctrl-C stops after the current phase, restores what it changed and still writes the report.

CI runs the same scenarios against Docker clusters: four hosts with three disks each, and one host with four disks. See the **[latest report](docs/reports/latest.html)** (download it; GitHub doesn't render HTML).

## Testing a change before you ship it

Two rigs run the soak scenarios without touching your cluster. The Docker one is quick and runs anywhere Docker runs; the VM one is slower but real: three operating systems, their service managers and their mount clients.

### The Docker soak

```sh
make soak          # five nodes (four with two disks, one diskless voter), the quick profile, ~10 min
make soak-single   # one node with four disks
make soak-full     # the long profile, ~1 h
```

Each target cross-compiles the binary for the Docker engine's architecture, brings up `test/soak/docker-compose.yml` (fake tailnet identities, `dev.enabled`, 20 s repair knobs, a worker sidecar per node that reaches the node's loopback NFS gateway), drives the fourteen phases from the host with `cmd/soak`, writes `report.html` and `results.jsonl` in the repository root and tears the cluster down. It needs Docker Desktop (or any engine with Compose) and nothing else. Run it on its own: `make check` alongside it turns ten minutes into an hour.

### Three local VMs

On an Apple Silicon Mac, `make vms-rounds` forms a volume from a macOS, a Linux and a Windows guest on a throwaway tailnet, each OS running `tailraid init` in turn while the other two join it by name, then crashes and restarts the joiners through launchd, systemd and the SCM, moves data across the three mounts, runs `tailraid verify --destructive --restarts` from the next OS, updates every node to the latest release with `tailraid update --restart` and rolls it back, adds a disk, takes a snapshot and recovers a one-voter cluster. It is the tier that would have caught the v0.3.2 join defects.

| Step | Command | Notes |
|---|---|---|
| once | `make vms-setup` | Tart, Packer, QEMU and the rig's SSH key; no sudo. Docker Desktop must be running (it hosts the control server) |
| once | download the Windows 11 ARM64 ISO into `test/vms/iso/` | from [microsoft.com/software-download/windows11arm64](https://www.microsoft.com/software-download/windows11arm64); the page needs a browser |
| once | `make vms-images` | the golden images: a minute each for macOS and Linux, 30–60 minutes for the unattended Windows install |
| each session | `make vms-up` | starts headscale and the three guests, registering fresh ones |
| | `make vms-rounds` | three rounds, about an hour and a quarter; `FULL=1` runs the long verify profile; summary and reports in `docs/reports/vms/<date>/` |
| | `make vms-round FIRST=linux` | one round; `OSES="mac linux"` runs it on two guests while the Windows image is not built yet |
| | `make vms-down` | stops everything and keeps every state, so the next `vms-up` registers nothing |
| | `make vms-destroy` | removes the instances and the control server's state; the images stay |

The guests take about 120 GB of disk. [docs/vms.md](docs/vms.md) explains what each step proves, how the networking is wired (the Tart guests' NAT, the Windows guest behind QEMU's user-mode NAT, headscale over TLS from a CA the rig generates) and what to do when something does not come up.

## Verifying a download

Every release carries `sha256sums.txt` and `sha256sums.txt.minisig`, a [minisign](https://jedisct1.github.io/minisign/) signature over the sums by the release key. `tailraid update` checks both before it swaps a binary. To check a download by hand, verify the sums file against the public key printed in [docs/12-operations.md](docs/12-operations.md) §4, then the binary against the sums:

```sh
minisign -Vm sha256sums.txt -P <public key>
sha256sum -c --ignore-missing sha256sums.txt
```

## Upgrading

Run `tailraid update` on every device, voters first, then `tailraid stop && tailraid start` on each (or `tailraid update --restart`). Do the whole cluster in one go: the first snapshot an upgraded node writes may be one an older node cannot read ([docs/12-operations.md](docs/12-operations.md) §6). The previous binary stays beside the new one as `tailraid.old` for a minute after a clean start; `tailraid update --rollback` puts it back. A release older than the running one needs `--version` and `--allow-downgrade`.

## Backup and recovery

The volume's metadata is replicated on the voters; the data is on the devices and disks. `tailraid snapshot <dir>` exports the metadata (a fresh Raft snapshot) to a directory, which is small and worth copying somewhere outside the cluster now and then. Losing any `m` devices or disks is survived without any of this.

Losing more voters than Raft can spare, permanently, is the one failure the cluster cannot heal on its own ([docs/05-failure-model.md](docs/05-failure-model.md) §3). On one surviving voter, with its service stopped, `tailraid recover --voters <its node id>` makes it continue alone with everything it knew; every file whose shards survive is readable again. Start it, then `tailraid reset` and re-join the other devices.

## Releasing

```sh
make tag VERSION=v0.3.0
```

1. The first run opens a pull request with the changelog. Merge it and `git pull`.
2. Run the same command again. It tags the release, and CI publishes six binaries, the sums file and its signature (the `MINISIGN_SECRET_KEY` secret holds the key).

Details: [docs/12-operations.md](docs/12-operations.md) §4.

## Documentation

| Document | What it covers |
|---|---|
| [docs/README.md](docs/README.md) | the design, in reading order |
| [docs/05-failure-model.md](docs/05-failure-model.md) | exactly what "no data loss" covers |
| [docs/14-disks.md](docs/14-disks.md) | several disks per device |
| [docs/12-operations.md](docs/12-operations.md) | services, logs, releases, self-update, backup and recovery |
| [docs/09-testing-strategy.md](docs/09-testing-strategy.md) | the test tiers, the soak suite and its report |
| [docs/vms.md](docs/vms.md) | the three-VM rig: setup, what a round proves, results |
| [docs/invariants.md](docs/invariants.md) | every promise the system makes, and the tests that prove it |
| [CLAUDE.md](CLAUDE.md) | how the code is written and tested |

## License

Copyright (C) 2026 FORGE3D CYF. tailraid is free software under the [GNU Affero General Public License, version 3](LICENSE): anyone who runs a modified version as a network service must offer its source to its users. The in-tree copy of go-filesystems/smb in `third_party/smb` keeps its own [BSD 3-Clause license](third_party/smb/LICENSE).
