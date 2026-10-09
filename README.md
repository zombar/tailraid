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

`tailraid update` installs the latest release. The repository is private: set `GITHUB_TOKEN` or log in with `gh`. See [Verifying a download](#verifying-a-download) before trusting a binary you fetched by hand.

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

## Everyday commands

```sh
tailraid status                    # health, leader, members, usage, warnings (exit 1 until the node is ready)
tailraid node ls                   # devices and their disks
tailraid check                     # exit 1 if anything is degraded
tailraid repair                    # repair now (it also runs every minute)
tailraid volume describe           # policy, usage, dedup, free space
tailraid chunk describe <path>     # where a file's chunks live
tailraid logs -f                   # the service log
```

Every command takes `--json`, and `--node <ip>` to talk to another device.

## Browser UI

Each device serves a web page at `http://<tailscale-ip>:20482/`, and `tailraid status` prints the address. The dashboard shows balance across devices and disks and the volume's integrity; the file browser streams audio, video and images, uploads files and makes folders. Cluster members and your own devices (same Tailscale user) can open it, so your phone works. Changing the cluster stays in the CLI.

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
| [docs/invariants.md](docs/invariants.md) | every promise the system makes, and the tests that prove it |
| [CLAUDE.md](CLAUDE.md) | how the code is written and tested |
