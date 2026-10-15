# Operating a cluster

Secrets, the command reference, the configuration file, backup and recovery, upgrading, verifying a download and checking a live cluster. Design: [12-operations.md](12-operations.md), [19-secrets.md](19-secrets.md).

## Secrets

The volume needs a few credentials: an S3 bucket's key and the TMDB key. They live in one encrypted store inside the cluster state, so you enter each once, on any device, and every device has it. `secret set <name> --system` is the one way in (an admin's, once the volume has users; without `--system` a user's secret is their own, [guide-users.md](guide-users.md#secrets-and-the-audit-log)), and whatever uses a secret checks it when it does: `node add-s3` refuses a bucket its pair does not open, `media scrape` asks TMDB once before a run and a device's probe shows a bucket its key no longer opens as not live. A value comes from stdin only, never from a flag, and no command prints one back. The key that opens the store sits in each device's keychain (the macOS Keychain, the Windows Credential Manager, the Linux Secret Service, or a 0600 file under the data directory where there is none), and a device that joins fetches it from another member over the tailnet.

```sh
tailraid secret ls                     # s3/offsite/access_key, s3/offsite/secret_key, tmdb: when, by which device, readable here
tailraid secret set tmdb --system < key.txt   # from a file, or typed without echo at a terminal
tailraid secret rm --all               # if every device lost the key: start over, then enter each again
```

Details: [docs/19-secrets.md](19-secrets.md).

## Users

A person gets an identity from an admin's one-time invite: `tailraid user invite kim`
prints a code, and Kim runs `tailraid user create kim <code>` on their own device, which
makes a key in their keychain. No password anywhere. Another device of Kim's is another
key (`user key invite`, then `user key add <code>` there). Folders are shared with
`grant <path> kim read|write|admin` or to a group (`@family`), nothing is shared until
it is, and the browser signs a person in from the link `tailraid web login` prints.
Every change that matters is signed by the person who made it and written to an audit
log you read with `audit ls`. The setup step by step, and two users for one person, are
in [guide-users.md](guide-users.md); the design is [docs/21-users.md](21-users.md).

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
| Secrets | `secret set <name> [--system]` · `secret get <name>` · `secret ls [--system]` · `secret rm <name>\|--all` |
| Users (docs/21) | `user invite\|create\|use\|ls\|key\|disable\|enable\|rm\|bind\|bootstrap` · `group create\|rm\|add\|remove\|ls` · `grant <path> <user\|@group> read\|write\|admin` · `revoke` · `access ls` · `audit ls` · `web login\|logout` |
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
| A file you deleted | The [trash](guide-using.md#trash), for 30 days |
| A file you overwrote, or one deleted longer ago | A [snapshot](guide-snapshots.md#browse-restore-and-revert) on a mirror |
| More voters than Raft can spare, for good | `tailraid recover` on one survivor (below) |
| Everything but a mirror | `tailraid mirror export` on any machine |

The metadata is replicated on the voters. `tailraid export <dir>` writes a fresh copy of it (a Raft snapshot) to a directory, which is small and worth keeping somewhere outside the cluster now and then.

Losing more voters than Raft can spare, permanently, is the one failure the cluster cannot heal on its own ([docs/05-failure-model.md](05-failure-model.md) §3). On one surviving voter, with its service stopped, `tailraid recover --voters <its node id>` makes it continue alone with everything it knew; every file whose shards survive is readable again. Start it, then `tailraid reset` and re-join the other devices.

## Upgrading

Run `tailraid update` on every device, voters first, then `tailraid stop && tailraid start` on each (or `tailraid update --restart`). Do the whole cluster in one go: the first snapshot an upgraded node writes may be one an older node cannot read ([docs/12-operations.md](12-operations.md) §6). The previous binary stays beside the new one as `tailraid.old` for a minute after a clean start; `tailraid update --rollback` puts it back. A release older than the running one needs `--version` and `--allow-downgrade`.

## Verifying a download

Every release carries `sha256sums.txt` and `sha256sums.txt.minisig`, a [minisign](https://jedisct1.github.io/minisign/) signature over the sums by the release key. `tailraid update` checks both before it swaps a binary. To check a download by hand, verify the sums file against the public key printed in [docs/12-operations.md](12-operations.md) §4, then the binary against the sums:

```sh
minisign -Vm sha256sums.txt -P <public key>
sha256sum -c --ignore-missing sha256sums.txt
```

## Check your cluster

```sh
tailraid verify --report out.html
```

This runs the soak scenarios against your live cluster and writes an HTML report: transfer, integrity, churn, dedup, repair and the disk checks. Without `--destructive` it touches nothing but its own files. Add `--destructive --restarts` to also test corruption, policy changes, drains, kills and partitions on devices running with `dev.enabled`; Ctrl-C stops after the current phase, restores what it changed and still writes the report.

CI runs the same scenarios against Docker clusters: four hosts with two disks each plus a diskless voter, and one host with four disks. See the **[latest report](reports/latest.html)** (download it; GitHub doesn't render HTML).
