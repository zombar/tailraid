<p align="center">
  <picture>
    <source media="(prefers-color-scheme: dark)" srcset="docs/assets/logo-dark.svg">
    <img src="docs/assets/logo-light.svg" alt="tailraid" width="300">
  </picture>
</p>

<p align="center"><b>One RAID-like volume across every device you own.</b><br>
Erasure-coded, deduplicated, snapshotted, mounted by the OS's own client. One static binary.</p>

<p align="center">
  <a href="#install">Install</a> ·
  <a href="#quick-start">Quick start</a> ·
  <a href="#guides">Guides</a> ·
  <a href="docs/README.md">Design</a>
</p>

tailraid pools the disks of your laptops, desktops, NAS boxes and cloud VMs into a single volume. Files are cut into content-addressed chunks, each chunk is Reed-Solomon coded into `k` data and `m` parity shards spread over different devices and disks, and the metadata is replicated by Raft on the devices you trust to stay on. Any `m` disks or devices can fail and the volume keeps working, then repairs itself. It appears as an ordinary folder on macOS, Linux and Windows, the devices talk over a WireGuard mesh the binary runs itself, and nothing ever leaves your devices.

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
| **Private by construction** | Every byte between devices travels inside WireGuard, every device is a key, and only a device the cluster admitted can reach a node. No account, no login, no VPN to install: a join code is the whole of it. |
| **One binary** | The same static executable is the daemon, the gateways and the CLI. Signed releases, `tailraid update` with rollback, and a service installer for launchd, systemd and Windows. |

> [!IMPORTANT]
> Two devices that cannot reach each other directly (both behind strict NATs) talk through a member both can reach, so give one always-on device a public address (`mesh.endpoint`) or a mapped port; a direct path (a LAN, IPv6, a port forward or the router's own port mapping) is the fast one ([docs/20](docs/20-mesh.md) §5–§7). On macOS the service runs as root and `init`, `join`, `install` and `reset` need `sudo`; on Linux `join` does. Windows needs Windows 11 24H2 or newer, whose SMB client can be told the port tailraid listens on; the Windows mount is experimental, see [docs/windows-checklist.md](docs/windows-checklist.md). tailraid does not encrypt data at rest: whoever holds a disk, a bucket or a mirror can read what is on it.

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
tailraid init         # creates the volume and prints a join code
tailraid install      # runs tailraid as a service (launchd / systemd / Windows service)
tailraid mount        # ~/TailRAID (T: on Windows)
```

On each other device:

```sh
tailraid join <code>     # the code init printed, or `tailraid token` on any member; --voter on other always-on devices
tailraid install
tailraid mount
```

The code carries the cluster's identity, the join token and where to find the device
that printed it; the joiner gets its address from the cluster and its first tunnel in
the same step, and nothing else runs on the device ([docs/20](docs/20-mesh.md)).

Write a file on one device and read it on any other. `tailraid status` shows the whole cluster and prints the browser UI's address; `tailraid dashboard` opens it.

`init` needs no options: each disk's budget follows its free space, and the redundancy follows the devices and disks the cluster has ([redundancy](docs/guide-capacity.md#redundancy)). To give tailraid a disk other than the data directory, list it under `node.disks` in the config before `init`, or add it later with `tailraid node add-disk`.

## Guides

| Guide | What it covers |
|---|---|
| [Using the volume](docs/guide-using.md) | the browser UI, the trash, folder flags, pinned folders, search, posters for the Media tab |
| [Snapshots and mirrors](docs/guide-snapshots.md) | adding a mirror, the schedule, browse, restore and revert, reading a mirror without the cluster |
| [Capacity, redundancy and voters](docs/guide-capacity.md) | adding devices, disks and S3 buckets, draining, the policy, voters and a cheap always-on voter |
| [Operating a cluster](docs/guide-operating.md) | secrets, every command, the configuration file, backup and recovery, upgrading, verifying a download, `tailraid verify` |
| [Testing a change and releasing](docs/guide-contributing.md) | the Docker soak, the three-VM rig, cutting a release |

The design lives in [docs/](docs/README.md), in reading order, each document ending in the promises it makes and the tests that prove them ([docs/invariants.md](docs/invariants.md)); [docs/05-failure-model.md](docs/05-failure-model.md) says exactly what "no data loss" covers. [CLAUDE.md](CLAUDE.md) is how the code is written and tested.

## License

Copyright (C) 2026 FORGE3D CYF. tailraid is free software under the [GNU Affero General Public License, version 3](LICENSE): anyone who runs a modified version as a network service must offer its source to its users. The in-tree copies of go-filesystems/smb in `third_party/smb` and go-nfs in `third_party/nfs` keep their own licenses ([BSD 3-Clause](third_party/smb/LICENSE), [Apache 2.0](third_party/nfs/LICENSE)).
