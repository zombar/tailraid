<p align="center">
  <picture>
    <source media="(prefers-color-scheme: dark)" srcset="docs/assets/logo-dark.svg">
    <img src="docs/assets/logo-light.svg" alt="tailraid" width="300">
  </picture>
</p>

<p align="center"><b>One RAID-like volume across every device on your tailnet.</b><br>
Erasure-coded, deduplicated, snapshotted, mounted by the OS's own client. One static binary.</p>

<p align="center">
  <a href="#install">Install</a> ·
  <a href="#quick-start">Quick start</a> ·
  <a href="#snapshots-and-mirrors">Snapshots</a> ·
  <a href="#grow-the-volume">Grow</a> ·
  <a href="#command-reference">Commands</a> ·
  <a href="#configuration">Configuration</a> ·
  <a href="docs/README.md">Design</a>
</p>

tailraid pools the disks of your laptops, desktops, NAS boxes and cloud VMs into a single volume. Files are cut into content-addressed chunks, each chunk is Reed-Solomon coded into `k` data and `m` parity shards spread over different devices and disks, and the metadata is replicated by Raft on the devices you trust to stay on. Any `m` disks or devices can fail and the volume keeps working, then repairs itself. It appears as an ordinary folder on macOS, Linux and Windows, and nothing ever leaves your tailnet.

## Features

| | |
|---|---|
| **Redundant** | Any `m` of the `k+m` shards of a chunk can be lost. The cluster picks the policy itself as devices and disks come and go, and never puts more of a stripe on one host than it can lose. |
| **Self-healing** | A device away longer than `fill_after` has its data re-copied elsewhere; a weekly scrub re-reads and verifies every chunk; a disk that fills up hands slots to its siblings. |
| **Deduplicated** | Identical chunks are stored once, across files, folders and devices. |
| **Native mounts** | NFSv3 on loopback for macOS and Linux, SMB2 for Windows: no FUSE, no kernel extension, no driver. |
| **Elastic** | Add a device, a disk or an off-site S3 bucket while the volume runs; drain and remove one the same way. Budgets follow free space by default. |
| **Snapshots** | Hourly, differential snapshots on *mirror* disks (a USB drive plugged in now and then is enough). Each mirror is a complete, self-describing copy that reads without the cluster. |
| **Trash** | Every delete, from any mount or the browser, waits 30 days before it is purged. |
| **Browser UI** | A dashboard, a file browser that streams audio and video with tiles and an album view, a Media tab for films, shows and music, full-text search, rendered markdown and a Stats page with the cluster's history. Works from your phone. |
| **Search** | Names, audio tags and text contents, from the browser or `tailraid search`. The index lives in the volume, so every device has it. |
| **Offline folders** | Pin a folder to a device and it reads from that device with every other node away. |
| **Private by construction** | Only members of your tailnet can reach a node; every request is checked against Tailscale's identity of the caller. |
| **One binary** | The same static executable is the daemon, the gateways and the CLI. Signed releases, `tailraid update` with rollback, and a service installer for launchd, systemd and Windows. |

> [!IMPORTANT]
> Every device needs Tailscale (headscale works). Windows needs Windows 11 24H2 or newer, whose SMB client can be told the port tailraid listens on; the Windows mount is experimental, see [docs/windows-checklist.md](docs/windows-checklist.md). tailraid does not encrypt data at rest: whoever holds a disk, a bucket or a mirror can read what is on it.

## Install

Download a binary from the [releases](https://github.com/zombar/tailraid/releases) and put it on your `PATH`, or build it:

```sh
git clone https://github.com/zombar/tailraid.git && cd tailraid
make install          # → ~/.local/bin/tailraid (needs Go 1.27.2+)
```

Once installed, `tailraid update` fetches the latest release; no token or login is needed. See [Verifying a download](#verifying-a-download) before trusting a binary you fetched by hand.

## Quick start

On the first always-on device:

```sh
tailraid init         # creates the volume and prints a join token
tailraid install      # runs tailraid as a service (launchd / systemd / Windows service)
tailraid mount        # ~/TailRAID (T: on Windows)
```

On each other device:

```sh
tailraid join 100.64.0.11 <token>     # the first device's Tailscale IP or name; --voter on other always-on devices
tailraid install
tailraid mount
```

Write a file on one device and read it on any other. `tailraid status` shows the whole cluster and prints the browser UI's address; `tailraid dashboard` opens it.

`init` needs no options: each disk's budget follows its free space, and the redundancy follows the devices and disks the cluster has ([Redundancy](#redundancy)). To give tailraid a disk other than the data directory, list it under `node.disks` in the config before `init`, or add it later with `tailraid node add-disk`.

## Using the volume

### The browser UI

Every device serves a web page at `http://<tailscale-ip>:20482/`. Cluster members and your own devices (the same Tailscale user) can open it, so your phone works.

| Tab | What it does |
|---|---|
| Dashboard | Health, balance across devices and disks, repair progress, the integrity panel |
| Files | Browse, upload (resumable), make folders, select many to move, rename or delete; list or tile view with thumbnails; an album view and a now-playing bar for audio; markdown files rendered as documents |
| Media | One section per folder flagged as a library (movies, TV or music), with posters, fan art and descriptions from the Kodi/Jellyfin sidecars beside the files. Nothing is fetched from the internet |
| Stats | The cluster's history over any range: throughput, repair, data spread, and each device's CPU, memory, disk and network. Kept inside the volume, so a lost device loses none of it |

The search box in the header finds files and folders by name, audio tracks by title, artist and album, and text files by what they say. Changing the cluster stays in the CLI.

### Trash

Every delete lands in the trash. On the mount a deleted file looks deleted; the browser lists it in its old folder with a banner that restores it. The leader purges an entry 30 days after the delete (`trash.retention`).

```sh
tailraid trash ls [path]                     # deleted entries, with inode, original path, size and age
tailraid trash restore <ino> [--to dir] [--as name]
tailraid trash purge --older-than 0          # reclaim space now (or --ino <n>, --all)
```

Free space comes back with the purge, not with the delete: after an `rm -rf` to make room, purge the trash.

### Folder flags

The folder menu in the browser sets three flags on a folder and everything under it. **Protected** refuses every delete, overwrite and move out of the folder, from every mount and the browser alike. **nsfw** blurs its tiles and thumbnails in the browser. **Library** (movies, TV or music) puts the folder on the Media tab.

### Keep a folder on this device

```sh
tailraid cache pin ~/TailRAID/Music      # fetch every chunk under it into this device's cache and keep it there
tailraid cache ls                        # each pin and how much of it has arrived
tailraid cache unpin ~/TailRAID/Music
```

A pinned folder reads from the device even with every other node away, which suits a laptop on a plane. The browser's "Keep on this device" is the same switch. Pinned bytes live outside `cache.size`, the budget for ordinary caching (1 GiB).

### Search

```sh
tailraid search agnes river --under /Music     # the words ANDed, the last one a prefix
tailraid search index                          # run the leader's index pass now
```

Names are answered live from the metadata. Contents and tags are indexed by the leader every 10 minutes (`search.interval`) into `/.tailraid/index`, inside the volume.

### Posters and descriptions for the Media tab

```sh
tailraid secret set tmdb               # once per cluster: a free TMDB API key, typed or piped in
tailraid media scrape --dry-run        # what would be written
tailraid media scrape /Films           # write the missing .nfo, posters, backdrops, stills, album covers and game box art
```

The Media tab reads Kodi sidecars. `media scrape` fills in the missing ones from TMDB (films, shows, seasons, episodes), MusicBrainz with the Cover Art Archive (album covers) and the libretro thumbnails (game box art, by the No-Intro name), and never replaces a file that exists. A clear match is used at once; for anything else it shows the top five on a terminal, or skips with `--yes` not given. Album covers and game box art need no key: without one, `media scrape` does those and skips films and TV. Only this command talks to the internet, from the device you run it on. This product uses the TMDB API but is not endorsed or certified by TMDB.

## Snapshots and mirrors

A **mirror** is a disk given to snapshots. It is attached to any device of the cluster, it never holds the live volume, and it may be offline most of the time: a USB drive you plug in on Sundays is a mirror. While a mirror is online its device snapshots the whole volume onto it every hour, and each snapshot after the first copies only what changed. Every mirror is a complete, independent copy of the history it took: two mirrors are two backups, and either one restores everything it holds, with or without the cluster.

### Add a mirror

On the device the disk is attached to:

```sh
tailraid mirror add /Volumes/Archive/tailraid    # an empty directory on the mirror disk
```

The mirror is named after its device, `pike/m1`, `pike/m2` and so on. The device opens it at once and takes the first snapshot, which copies the whole volume; later ones copy only the difference. On Linux, `mirror add` rewrites the confined service unit so it may write there, as `node add-disk` does.

Unplug the disk whenever you like. A mirror that comes back after missing a scheduled snapshot gets one on its next minute, and the snapshots it missed are simply not taken; nothing is held in the pool for it while it is away. A disk moved to another device keeps its name and history: `mirror add` the same path on the new device.

### The schedule

| Kept | By default |
|---|---|
| Hourly | the newest of each of the last 24 hours |
| Daily | the newest of each of the last 30 days |
| Weekly | the newest of each of the last 8 ISO weeks |
| Monthly | the newest of each of the last 12 months |

Automatic snapshots are named by their UTC minute (`2026-10-12-0900Z`) and thinned to the table above (`mirrors.keep`). When a mirror's free space falls under its reserve (5 % of the disk or 4 GiB, whichever is larger), the oldest automatic snapshot goes first. Take a snapshot by hand before something risky:

```sh
tailraid snapshot create before-reorg            # on every online mirror (or --mirror pike/m1)
tailraid snapshot ls                             # every snapshot on every mirror, online or not
```

A manual snapshot is permanent: it is never thinned and never deleted for space. No command removes a snapshot by hand. A mirror full of manual snapshots stops syncing with a warning in `status` and `check`; give it a larger disk (copy the mirror directory over and `mirror add` the new path) or add another mirror.

### Browse, restore and revert

Each mount carries a hidden, read-only `.snapshots` folder at its root, with one folder per mirror and one per snapshot inside it:

```sh
ls ~/TailRAID/.snapshots/pike-m1/
cp ~/TailRAID/.snapshots/pike-m1/2026-10-12-0900Z/notes.md ~/Desktop/
```

Reads come from the live pool when the chunk is still there and from the mirror otherwise. An offline mirror's snapshots are still listed, and reading inside one fails until the disk is back. To put something back into the live volume:

```sh
tailraid versions Documents/thesis.tex                                   # the file's distinct versions across the online mirrors
tailraid versions Documents/thesis.tex --revert pike/m1/2026-10-12-0900Z  # make that version the current content
tailraid versions Documents/thesis.tex --copy pike/m1/before-reorg --as thesis-old.tex
tailraid snapshot restore pike/m1/before-reorg Photos/2024 --to Restored  # a folder, merged into the live tree
```

A restore never overwrites: an existing file is skipped and reported. A revert is itself a change, so the content it replaced is in the next snapshot.

### Without the cluster

A mirror needs nothing from the cluster to be read. Plug it into any machine with the binary:

```sh
tailraid mirror ls /Volumes/Archive/tailraid                                   # the snapshots on the disk
tailraid mirror cat /Volumes/Archive/tailraid 2026-10-12-0900Z Documents/todo.txt
tailraid mirror export /Volumes/Archive/tailraid 2026-10-12-0900Z ~/rescue      # --path to export only part of it
```

`tailraid mirror remove pike/m1` forgets a mirror and leaves the disk and every snapshot on it untouched; added back, they return. A mirror holds your files in the clear, so keep the disk where the data may be. Design: [docs/16-snapshots.md](docs/16-snapshots.md).

## Grow the volume

| To add capacity | Do this |
|---|---|
| Add a device | `tailraid join <ip> <token>` on the new device |
| Add a disk to a device | `tailraid node add-disk /path/to/empty/dir` on that device |
| Add an S3 bucket off-site | `tailraid node add-s3 <name> …` on one device |
| Grow a device or disk in place | [Growing in place](#growing-in-place) |

`tailraid volume describe` shows the volume's raw, usable and free capacity. New data spreads over new space straight away. Existing data moves only when enough of it sits away from where placement would put it today (5 % of the stored bytes by default, `repair.home_start`), and then a little each repair pass at background priority. `tailraid volume rebalance --dry-run` shows what would move, and `tailraid volume rebalance` starts it now.

### Adding a disk

```sh
tailraid node add-disk /Volumes/Big/tailraid              # budget follows the disk's free space
tailraid node add-disk /Volumes/Big/tailraid --capacity 500G
```

The directory must be empty and should be on a different physical disk. The disk joins as `<device>/1`, `<device>/2` and so on, listed under its device. A single machine with several disks makes a complete volume, with redundancy across its disks. If a disk is unplugged or not mounted, tailraid notices, stops using it and repairs from the other copies. Details: [docs/14-disks.md](docs/14-disks.md).

### Growing in place

Each disk has a **budget**: how much of it tailraid may use. When the budget follows free space, which is the default, free up space on the disk or enlarge its partition and the budget grows within a minute with no restart. When the budget is pinned, raise it with one command, which rewrites the config and restarts the service:

```sh
tailraid volume grow jockey/1 500G     # pin the budget of disk 1
tailraid volume grow jockey/1 auto     # follow free space again
```

`tailraid node describe <name>` shows a budget (`capacity`).

### Adding an S3 bucket

A bucket at any S3 provider (Backblaze B2, Cloudflare R2, Hetzner, a MinIO on the tailnet) can hold shards like a disk on a host of its own, which gives the volume an off-site copy. Devices read and write it directly; reads use it only when a device that holds a shard is down.

```sh
tailraid secret set s3/offsite/access_key   # each value typed without echo, or piped in: never a flag
tailraid secret set s3/offsite/secret_key
tailraid node add-s3 offsite --endpoint https://s3.eu-central-003.backblazeb2.com \
  --bucket my-bucket --prefix tailraid --capacity 1T
```

`add-s3` takes the two secrets from the store, then writes, reads back and deletes a test object, so a key without those rights is refused before anything is registered. Every device has the key through the store, and each one needs it: a device that leads without it treats the bucket as dead and after `fill_after` copies the bucket's shards back onto disks. After a rotation, `secret set` the two again; every device's probe picks the new pair up within a probe interval. Choose a provider with cheap or free egress: repairs read from the bucket when a device is down. Use a key the provider scopes to that one bucket, since every device can open it.

A bucket holds one shard of every stripe, so it stores 1/k of the volume (half at 2+1, a quarter at 4+2). It is written when you write, probed with a cheap HEAD every 30 s by each device, listed once a day by the leader, and read only while a device holding a needed shard is down. A bucket never votes: it keeps your data safe while a device is away, not the volume writable. For that, pair it with a [diskless voter](#a-cheap-always-on-voter). Details: [docs/10-s3.md](docs/10-s3.md) §4.

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

The metadata lives on **voters**. Pick always-on devices for them, in an odd number: 1 to start, 3 is a good size. Laptops should be **non-voters**: they store data and serve files, but a closed lid never stalls the cluster.

```sh
tailraid node demote laptop       # make a member a non-voter
tailraid node promote nas         # make a member a voter
```

Run these from any member. `tailraid node ls` shows who votes.

### A cheap always-on voter

A voter needs no disk. A small cloud VM on the tailnet, joined with `--voter` and no `disks:` in its config, is a full member and gateway that stores nothing: it keeps the metadata available while your laptops are closed, and its `node ls` row says `no disks`. Two laptops plus such a VM are three voters, so the volume stays writable with the VM and either laptop up. It fetches the cluster key from another member and with it the S3 key, which matters because the VM is the device most likely to lead.

```sh
tailraid join 100.64.0.11 <token> --voter    # on the VM; nothing under disks: in its config
tailraid install
```

Together, a diskless voter and a bucket give an always-on cluster of rented parts: the VM holds the votes and no data, the bucket holds the data and no votes.

## Secrets

The volume needs a few credentials: an S3 bucket's key and the TMDB key. They live in one encrypted store inside the cluster state, so you enter each once, on any device, and every device has it. `secret set <name>` is the one way in, and whatever uses a secret checks it when it does: `node add-s3` refuses a bucket its pair does not open, `media scrape` asks TMDB once before a run and a device's probe shows a bucket its key no longer opens as not live. A value comes from stdin only, never from a flag, and no command prints one back. The key that opens the store sits in each device's keychain (the macOS Keychain, the Windows Credential Manager, the Linux Secret Service, or a 0600 file under the data directory where there is none), and a device that joins fetches it from another member over the tailnet.

```sh
tailraid secret ls                     # s3/offsite/access_key, s3/offsite/secret_key, tmdb: when, by which device, readable here
tailraid secret set tmdb < key.txt     # from a file, or typed without echo at a terminal
tailraid secret rm --all               # if every device lost the key: start over, then enter each again
```

Details: [docs/19-secrets.md](docs/19-secrets.md).

## Command reference

Every command takes `--json`, and `--node <ip>` to talk to another device. `tailraid <command> --help` has the details.

| Area | Commands |
|---|---|
| Set up | `init` · `join <host> <token>` · `token` · `install` · `uninstall` · `start` · `stop` · `mount` · `umount` · `reset` |
| Health | `status` (exit 1 until the node is ready) · `dashboard` · `check` (exit 1 if anything is degraded; `--scrub` verifies every chunk) · `verify` · `logs -f` |
| Members and disks | `node ls` · `node describe <name>` · `node promote` · `node demote` · `node add-disk <path>` · `node add-s3 <name>` · `node drain` · `node remove` |
| Volume | `volume describe` · `volume grow <disk> <size>\|auto` · `volume set-redundancy --k --m\|--auto` · `chunk describe <path\|hash>` |
| Maintenance | `repair` (also runs every minute) · `gc` · `fsck` |
| Files | `trash ls\|restore\|purge` · `cache pin\|unpin\|ls` · `search <words>` · `search index` · `media scrape [path]` |
| Snapshots | `mirror add\|ls\|remove` · `mirror ls\|cat\|export <mirror path> …` · `snapshot ls\|create\|restore` · `versions <path> [--revert\|--copy]` |
| Secrets | `secret set <name>` · `secret ls` · `secret rm <name>\|--all` |
| Recovery | `export <dir>` · `recover --voters <id>` · `stats seal` |
| Releases | `update [--check\|--rollback\|--restart]` · `version` |

## Configuration

`init` and `join` write the config file. Its location depends on the OS:

| OS | Config file |
|---|---|
| macOS | `~/Library/Application Support/tailraid/config.yaml` |
| Linux | `~/.config/tailraid/config.yaml` |
| Windows | `%ProgramData%\tailraid\config.yaml` |

`$TAILRAID_HOME` overrides the directory. The keys you might change, with their defaults:

```yaml
node:
  capacity: 500G        # pin this device's budget (omit to follow free space; "0" stores nothing here)
  voter: true           # always-on devices only; follows `node promote|demote` while the node serves
  disks:                # disks listed here before `init` are added by it;
    - path: /Volumes/Big/tailraid   # later ones need: tailraid node add-disk
  mirrors:              # written by `tailraid mirror add`
    - path: /Volumes/Archive/tailraid
listen:
  web: 20482            # browser UI port; 0 turns it off
  nfs: 127.0.0.1:20490  # NFS gateway (macOS, Linux); Windows serves SMB on 127.0.0.1:20445 instead
mount:
  path: ~/TailRAID      # ~ is expanded
repair:
  fill_after: 10m       # how long a device may be away before its data is re-copied
  s3_scrub: 1G          # bucket bytes the scrub may download per day to verify objects ("0": HEAD only)
  workers: 4            # chunks a repair pass works on at once (1..64)
  rate: 50M             # shard bytes per second repair may move ("0": no limit)
  scrub_period: 168h    # how long the background scrub takes to re-read and verify everything ("0": off)
trash:
  retention: 720h       # how long a deleted entry waits before it is purged ("0": never)
cache:
  size: 1GiB            # this device's chunk cache, pinned folders not counted
search:
  interval: 10m         # how often the leader indexes changed files ("0": off)
  max_file: 1MiB        # how much of a text file is indexed
mirrors:
  every: 1h             # an automatic snapshot of every online mirror this often ("0": none)
  keep: {hourly: 24, daily: 30, weekly: 8, monthly: 12}
  rate: 50M             # bytes per second a snapshot reads from the pool ("0": no limit)
  hold_max: 24h         # a snapshot still copying after this long is ended as failed
  stale_after: 168h     # check warns when a mirror's newest snapshot is this old
stats:
  every: 10s            # how often each device samples itself for the Stats page
  keep:
    raw: 48h            # 10 s samples; then 1-minute rows for 30 d and 10-minute rows for 2 y
log:
  level: info           # debug | info | warn | error
  file: ~/tailraid.log  # optional; rotates at 64 MiB keeping five generations
```

`serve` refuses a config whose ports collide or whose log level it does not know, and warns when the file or the data directory is readable by other users of the machine.

## Backup and recovery

| What you lose | What saves you |
|---|---|
| Any `m` devices or disks | Nothing to do: the volume keeps working and repairs onto what is left |
| A file you deleted | The [trash](#trash), for 30 days |
| A file you overwrote, or one deleted longer ago | A [snapshot](#browse-restore-and-revert) on a mirror |
| More voters than Raft can spare, for good | `tailraid recover` on one survivor (below) |
| Everything but a mirror | `tailraid mirror export` on any machine |

The metadata is replicated on the voters. `tailraid export <dir>` writes a fresh copy of it (a Raft snapshot) to a directory, which is small and worth keeping somewhere outside the cluster now and then.

Losing more voters than Raft can spare, permanently, is the one failure the cluster cannot heal on its own ([docs/05-failure-model.md](docs/05-failure-model.md) §3). On one surviving voter, with its service stopped, `tailraid recover --voters <its node id>` makes it continue alone with everything it knew; every file whose shards survive is readable again. Start it, then `tailraid reset` and re-join the other devices.

## Upgrading

Run `tailraid update` on every device, voters first, then `tailraid stop && tailraid start` on each (or `tailraid update --restart`). Do the whole cluster in one go: the first snapshot an upgraded node writes may be one an older node cannot read ([docs/12-operations.md](docs/12-operations.md) §6). The previous binary stays beside the new one as `tailraid.old` for a minute after a clean start; `tailraid update --rollback` puts it back. A release older than the running one needs `--version` and `--allow-downgrade`.

## Verifying a download

Every release carries `sha256sums.txt` and `sha256sums.txt.minisig`, a [minisign](https://jedisct1.github.io/minisign/) signature over the sums by the release key. `tailraid update` checks both before it swaps a binary. To check a download by hand, verify the sums file against the public key printed in [docs/12-operations.md](docs/12-operations.md) §4, then the binary against the sums:

```sh
minisign -Vm sha256sums.txt -P <public key>
sha256sum -c --ignore-missing sha256sums.txt
```

## Check your cluster

```sh
tailraid verify --report out.html
```

This runs the soak scenarios against your live cluster and writes an HTML report: transfer, integrity, churn, dedup, repair and the disk checks. Without `--destructive` it touches nothing but its own files. Add `--destructive --restarts` to also test corruption, policy changes, drains, kills and partitions on devices running with `dev.enabled`; Ctrl-C stops after the current phase, restores what it changed and still writes the report.

CI runs the same scenarios against Docker clusters: four hosts with two disks each plus a diskless voter, and one host with four disks. See the **[latest report](docs/reports/latest.html)** (download it; GitHub doesn't render HTML).

## Testing a change before you ship it

Two rigs run the soak scenarios without touching your cluster. The Docker one is quick and runs anywhere Docker runs; the VM one is slower but real: three operating systems, their service managers and their mount clients.

### The Docker soak

```sh
make soak          # five nodes (four with two disks, one diskless voter), the quick profile, ~10 min
make soak-single   # one node with four disks
make soak-full     # the long profile, ~1 h
```

Each target cross-compiles the binary for the Docker engine's architecture, brings up `test/soak/docker-compose.yml` (fake tailnet identities, `dev.enabled`, 20 s repair knobs, a worker sidecar per node that reaches the node's loopback NFS gateway, an S3 server for the bucket phase), drives every phase from the host with `cmd/soak`, writes `report.html` and `results.jsonl` in the repository root and tears the cluster down. It needs Docker Desktop (or any engine with Compose) and nothing else. Run it on its own: `make check` alongside it turns ten minutes into an hour.

### Three local VMs

On an Apple Silicon Mac, `make vms-rounds` forms a volume from a macOS, a Linux and a Windows guest on a throwaway tailnet, each OS running `tailraid init` in turn while the other two join it by name, then crashes and restarts the joiners through launchd, systemd and the SCM, moves data across the three mounts, runs `tailraid verify --destructive --restarts` from the next OS, updates every node to the latest release with `tailraid update --restart` and rolls it back, adds a disk, takes a metadata export and recovers a one-voter cluster. It is the tier that would have caught the v0.3.2 join defects.

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
| [docs/03-erasure-coding-and-placement.md](docs/03-erasure-coding-and-placement.md) | how the policy is chosen and where shards go |
| [docs/14-disks.md](docs/14-disks.md) | several disks per device, diskless hosts |
| [docs/10-s3.md](docs/10-s3.md) | S3 buckets as storage nodes |
| [docs/16-snapshots.md](docs/16-snapshots.md) | snapshots on mirror disks |
| [docs/13-web-ui.md](docs/13-web-ui.md) | the browser UI, the trash, folder flags and the Media tab |
| [docs/17-stats.md](docs/17-stats.md) | the Stats page and its history in the volume |
| [docs/18-search.md](docs/18-search.md) | search and the media catalogue |
| [docs/15-smb-gateway.md](docs/15-smb-gateway.md) | the Windows SMB gateway |
| [docs/12-operations.md](docs/12-operations.md) | services, logs, releases, self-update, backup and recovery |
| [docs/09-testing-strategy.md](docs/09-testing-strategy.md) | the test tiers, the soak suite and its report |
| [docs/vms.md](docs/vms.md) | the three-VM rig: setup, what a round proves, results |
| [docs/invariants.md](docs/invariants.md) | every promise the system makes, and the tests that prove it |
| [CLAUDE.md](CLAUDE.md) | how the code is written and tested |

## License

Copyright (C) 2026 FORGE3D CYF. tailraid is free software under the [GNU Affero General Public License, version 3](LICENSE): anyone who runs a modified version as a network service must offer its source to its users. The in-tree copies of go-filesystems/smb in `third_party/smb` and go-nfs in `third_party/nfs` keep their own licenses ([BSD 3-Clause](third_party/smb/LICENSE), [Apache 2.0](third_party/nfs/LICENSE)).
