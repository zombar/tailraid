<p align="center">
  <picture>
    <source media="(prefers-color-scheme: dark)" srcset="docs/assets/logo-dark.svg">
    <img src="docs/assets/logo-light.svg" alt="tailraid" width="300">
  </picture>
</p>

tailraid turns the devices on your tailnet into one shared volume that behaves like a RAID array.

- **Redundant.** Files are split into chunks, and each chunk is erasure-coded across devices and disks. Any `m` of them can fail and the volume keeps working.
- **Deduplicated.** Identical content is stored once.
- **Mountable.** It mounts as a normal folder on macOS, Linux and Windows, using the OS's own NFS client.
- **One binary.** The same static binary is the daemon, the gateway and the CLI. Data never leaves your tailnet.

> [!IMPORTANT]
> - Every device needs Tailscale (headscale works).
> - Windows needs Pro, Enterprise or Education (Home has no NFS client). The Windows mount is experimental: see [docs/windows-checklist.md](docs/windows-checklist.md).

## Install

Download a binary from the [releases](https://github.com/zombar/tailraid/releases) and put it on your `PATH`, or build it:

```sh
git clone https://github.com/zombar/tailraid.git && cd tailraid
make install          # → ~/.local/bin/tailraid (needs Go 1.27+)
```

`tailraid update` installs the latest release. The repository is private: set `GITHUB_TOKEN` or log in with `gh`.

## Set up a volume

On the first always-on device:

```sh
tailraid init         # creates the volume and prints a join token
tailraid install      # runs tailraid as a service (launchd / systemd / Windows service)
tailraid mount        # ~/TailRAID (T: on Windows)
```

On each other device:

```sh
tailraid join 100.64.0.11 <token>     # add --voter on other always-on devices
tailraid install
tailraid mount
```

Write a file on one device and read it on any other. `tailraid status` shows the whole cluster.

## Grow the volume

There are three ways to add capacity:

| To add capacity | Do this |
|---|---|
| Add a device | `tailraid join <ip> <token>` on the new device |
| Add a disk to a device | `tailraid node add-disk /path/to/empty/dir` on that device |
| Give a device more room | Free space on its disk. Budgets follow free space automatically. |

`tailraid volume describe` shows the volume's raw, usable and free capacity.

New data spreads over the new space straight away. Existing data stays where it is.

### Adding a disk

```sh
tailraid node add-disk /Volumes/Big/tailraid              # budget follows the disk's free space
tailraid node add-disk /Volumes/Big/tailraid --capacity 500G
```

- The directory must be empty and should be on a different physical disk.
- The disk joins as `<device>/1`, `<device>/2` and so on, listed under its device.
- A single machine with several disks makes a complete volume. Redundancy is then across its disks.
- If a disk is unplugged or not mounted, tailraid notices. It stops using that disk and repairs from the other copies.

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

- A pinned policy is never changed automatically, even when you add disks.
- Existing data migrates to a new policy in the background.
- `tailraid status` prints what the current policy survives.

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
tailraid status                    # health, leader, members, usage, warnings
tailraid node ls                   # devices and their disks
tailraid check                     # exit 1 if anything is degraded
tailraid repair                    # repair now (it also runs every minute)
tailraid volume describe           # policy, usage, dedup, free space
tailraid chunk describe <path>     # where a file's chunks live
tailraid logs -f                   # the service log
```

Every command takes `--json`, and `--node <ip>` to talk to another device.

## Browser UI

Each device serves a web page at `http://<tailscale-ip>:20482/`. `tailraid status` prints the address. It has:

- a dashboard: balance across devices and disks, and integrity;
- a file browser: stream audio, video and images, upload files, make folders.

Cluster members and your own devices (same Tailscale user) can open it, so your phone works. Changing the cluster stays in the CLI.

## Configuration

`init` and `join` write the config file. It lives at:

- **macOS:** `~/Library/Application Support/tailraid/config.yaml`
- **Linux:** `~/.config/tailraid/config.yaml`
- **Windows:** `%ProgramData%\tailraid\config.yaml`

`$TAILRAID_HOME` overrides the directory. The keys you might change:

```yaml
node:
  capacity: 500G        # pin this device's budget (omit to follow free space; "0" stores nothing here)
  voter: true           # always-on devices only
  disks:                # disks listed here before `init` are added by it;
    - path: /Volumes/Big/tailraid   # later ones need: tailraid node add-disk
listen:
  web: 20482            # browser UI port; 0 turns it off
mount:
  path: ~/TailRAID
repair:
  fill_after: 10m       # how long a device may be away before its data is re-copied
log:
  level: info           # debug | info | warn | error
```

## Check your cluster

```sh
tailraid verify --report out.html
```

This runs the soak scenarios against your live cluster and writes an HTML report: transfer, integrity, churn, dedup, repair and the disk checks. Add `--destructive --restarts` to also test corruption, kills and partitions on devices running with `dev.enabled`.

CI runs the same scenarios against Docker clusters: four hosts with three disks each, and one host with four disks. See the **[latest report](docs/reports/latest.html)** (download it; GitHub doesn't render HTML).

## Releasing

```sh
make tag VERSION=v0.3.0
```

1. The first run opens a pull request with the changelog. Merge it and `git pull`.
2. Run the same command again. It tags the release, and CI publishes six binaries.

Details: [docs/12-operations.md](docs/12-operations.md) §4.

## Documentation

- [docs/README.md](docs/README.md): the design, in reading order.
- [docs/05-failure-model.md](docs/05-failure-model.md): exactly what "no data loss" covers.
- [docs/14-disks.md](docs/14-disks.md): several disks per device.
- [docs/12-operations.md](docs/12-operations.md): services, logs, releases, self-update.
- [docs/invariants.md](docs/invariants.md): every promise the system makes, and the tests that prove it.
- [CLAUDE.md](CLAUDE.md): how the code is written and tested.
