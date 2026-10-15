# Capacity, redundancy and voters

Growing and shrinking the volume (devices, disks, S3 buckets), the redundancy policy, and which devices should vote. Design: [03-erasure-coding-and-placement.md](03-erasure-coding-and-placement.md), [14-disks.md](14-disks.md), [10-s3.md](10-s3.md).

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

The directory must be empty and should be on a different physical disk. The disk joins as `<device>/1`, `<device>/2` and so on, listed under its device. A single machine with several disks makes a complete volume, with redundancy across its disks. If a disk is unplugged or not mounted, tailraid notices, stops using it and repairs from the other copies. Details: [docs/14-disks.md](14-disks.md).

### Growing in place

Each disk has a **budget**: how much of it tailraid may use. When the budget follows free space, which is the default, free up space on the disk or enlarge its partition and the budget grows within a minute with no restart. When the budget is pinned, raise it with one command, which rewrites the config and restarts the service:

```sh
tailraid volume grow jockey/1 500G     # pin the budget of disk 1
tailraid volume grow jockey/1 auto     # follow free space again
```

`tailraid node describe <name>` shows a budget (`capacity`).

### Adding an S3 bucket

A bucket at any S3 provider (Backblaze B2, Cloudflare R2, Hetzner, a MinIO on one of your devices) can hold shards like a disk on a host of its own, which gives the volume an off-site copy. Devices read and write it directly; reads use it only when a device that holds a shard is down.

```sh
tailraid secret set s3/offsite/access_key   # each value typed without echo, or piped in: never a flag
tailraid secret set s3/offsite/secret_key
tailraid node add-s3 offsite --endpoint https://s3.eu-central-003.backblazeb2.com \
  --bucket my-bucket --prefix tailraid --capacity 1T
```

`add-s3` takes the two secrets from the store, then writes, reads back and deletes a test object, so a key without those rights is refused before anything is registered. Every device has the key through the store, and each one needs it: a device that leads without it treats the bucket as dead and after `fill_after` copies the bucket's shards back onto disks. After a rotation, `secret set` the two again; every device's probe picks the new pair up within a probe interval. Choose a provider with cheap or free egress: repairs read from the bucket when a device is down. Use a key the provider scopes to that one bucket, since every device can open it.

A bucket holds one shard of every stripe, so it stores 1/k of the volume (half at 2+1, a quarter at 4+2). It is written when you write, probed with a cheap HEAD every 30 s by each device, listed once a day by the leader, and read only while a device holding a needed shard is down. A bucket never votes: it keeps your data safe while a device is away, not the volume writable. For that, pair it with a [diskless voter](#a-cheap-always-on-voter). Details: [docs/10-s3.md](10-s3.md) §4.

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
