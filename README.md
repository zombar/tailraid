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
  <a href="#guides">Guides</a> ·
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
| **Users and permissions** | People get their own keys from an admin's invite; read, write and admin grants on folders hold on every mount and in the browser, and every change is in a tamper-evident audit log. |
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

Once installed, `tailraid update` fetches the latest release; no token or login is needed. See [verifying a download](docs/guide-operating.md#verifying-a-download) before trusting a binary you fetched by hand.

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

`init` also prints an admin invite. Until someone uses it the volume has no users and every mount sees everything; once the first user exists, each device's mount needs a binding to a user before it shows anything, so follow [the first admin](docs/guide-users.md#the-first-admin) when you are ready to share the volume with other people.

Write a file on one device and read it on any other. `tailraid status` shows the whole cluster and prints the browser UI's address; `tailraid dashboard` opens it.

`init` needs no options: each disk's budget follows its free space, and the redundancy follows the devices and disks the cluster has ([examples below](#redundancy-by-example), [the guide](docs/guide-capacity.md#redundancy)). To give tailraid a disk other than the data directory, list it under `node.disks` in the config before `init`, or add it later with `tailraid node add-disk`.

## Redundancy by example

A policy `k+m` cuts every chunk into `k` data shards and `m` parity shards, each on a different disk or bucket, and any `k` of the `k+m` can rebuild it. So a volume needs at least `k+m` targets, stores `(k+m)/k` bytes per byte of data, and loses nothing until more than `m` shards of one chunk are gone at once. What that means in whole devices depends on how the slots spread: a host with several disks holds several shards of a stripe, so losing it costs more than one. Data shards prefer the disks of voters (the always-on devices) and a read fetches data shards before parity, so when `k` is larger than the number of voter disks, one data shard of every stripe lives on a laptop or a bucket and a cold read goes there. The policy is automatic unless pinned with `tailraid volume set-redundancy`; `tailraid status` prints what the current one survives.

| Setup | Policy | Survives | Stored per byte | Notes |
|---|---|---|---|---|
| One device, one disk | 1+0 | nothing | 1.00x | One copy. Add a disk or a device before you rely on it. |
| One device with four disks | 2+2 | 2 disks, no host | 2.00x | Every stripe is on the one host, so its loss is the volume's. |
| Two always-on devices | 1+1 | 1 disk, 1 host | 2.00x | Mirroring: each chunk whole on both. |
| A laptop, a diskless cloud voter and a bucket | 1+1 | 1 disk, 1 host | 2.00x | The cheapest always-on setup: the voter holds the metadata and no data, the bucket a copy of everything, so the volume reads with the laptop closed. |
| Three always-on devices, one disk each | 2+1 | 1 disk, 1 host | 1.50x | The smallest setup that survives a whole device without doubling the bytes. |
| Three always-on devices and two laptops | 3+2 | 2 disks, 2 hosts | 1.67x | Every data shard is on an always-on device, so the volume reads with both laptops asleep. |
| Three always-on devices, one with two disks, a laptop and a bucket | 4+2 | 2 disks, 1 host | 1.50x | `k` follows the four voter disks; the device with two of them holds two shards of every stripe, so it is the one host the policy survives. Pinning 3+2 here spreads five slots over five hosts and survives two of them, at 1.67x. |
| The same with a second bucket | 4+2 | 2 disks, 2 hosts | 1.50x | Six slots over six hosts, one each. The buckets and the laptop hold parity only, so a cold read never touches a bucket while the voter disks are up. |
| The same, pinned to 5+2 | 5+2 | 2 disks, 1 host | 1.40x | Every target holds a slot of every chunk: nothing is spare, so one target away makes every new write degraded, and the fifth data shard lands on a bucket for most chunks, so most cold reads fetch from it. Not worth the 0.1x. |
| The same, pinned to 5+3 | 5+3 | 3 disks, 1 host | 1.60x | Refused until there are eight targets. With them it survives any three, but the two-disk host still caps it at one whole host, and it stores more than 4+2. |
| Four always-on devices with three disks each | 6+2 | 2 disks, 1 host | 1.33x | `k` stops at 6. The most compact policy the rule picks: eight slots over four hosts, two each, so losing a host costs exactly the parity. |

## Guides

| Guide | What it covers |
|---|---|
| [Using the volume](docs/guide-using.md) | the browser UI, the trash, folder flags, pinned folders, search, posters for the Media tab |
| [Users and permissions](docs/guide-users.md) | the first admin, binding each device, inviting people, groups and grants, two users for one person, browser sign-in, personal and system secrets |
| [Snapshots and mirrors](docs/guide-snapshots.md) | adding a mirror, the schedule, browse, restore and revert, reading a mirror without the cluster |
| [Capacity, redundancy and voters](docs/guide-capacity.md) | adding devices, disks and S3 buckets, draining, the policy, voters and a cheap always-on voter |
| [Moving from 0.9.x to 0.10](docs/guide-moving-to-0.10.md) | taking a 0.9.x cluster down on each OS, a new cluster on Tailscale, adopting the mirror and restoring from it |
| [Operating a cluster](docs/guide-operating.md) | secrets, every command, the configuration file, backup and recovery, upgrading, verifying a download, `tailraid verify` |
| [Testing a change and releasing](docs/guide-contributing.md) | the Docker soak, the three-VM rig, cutting a release |

The design lives in [docs/](docs/README.md), in reading order, each document ending in the promises it makes and the tests that prove them ([docs/invariants.md](docs/invariants.md)); [docs/05-failure-model.md](docs/05-failure-model.md) says exactly what "no data loss" covers. [CLAUDE.md](CLAUDE.md) is how the code is written and tested.

## License

Copyright (C) 2026 FORGE3D CYF. tailraid is free software under the [GNU Affero General Public License, version 3](LICENSE): anyone who runs a modified version as a network service must offer its source to its users. The in-tree copies of go-filesystems/smb in `third_party/smb` and go-nfs in `third_party/nfs` keep their own licenses ([BSD 3-Clause](third_party/smb/LICENSE), [Apache 2.0](third_party/nfs/LICENSE)).
