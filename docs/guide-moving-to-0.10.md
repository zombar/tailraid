# Moving from 0.9.x to 0.10

Taking a 0.9.x cluster down on macOS, Linux and Windows, starting a 0.10 cluster on Tailscale, and bringing the files back from a mirror. Design: [12-operations.md](12-operations.md) §6, [16-snapshots.md](16-snapshots.md) §7.

0.9.x ran on its own WireGuard mesh; 0.10 withdrew it and runs on Tailscale again. A 0.9.x volume does not upgrade: its member rows carry mesh keys and its snapshots are a format 0.10 refuses, so the cluster is created anew. A mirror is the bridge. It holds the files, not the cluster, so the new cluster adopts it and restores from it. Without a mirror, copy the files off through a mount before step 2 and back through a mount after step 4.

## 1. Before you take it down

While the old cluster still runs, take one last snapshot by hand. A manual snapshot is never thinned, so it is still there however long the move takes:

```sh
tailraid snapshot create before-0.10
tailraid snapshot ls        # wait until before-0.10 shows complete on the mirror
```

Write down what the new cluster will not inherit:

| What | How to see it now | Why |
|---|---|---|
| The disks and the mirror of each device | `node.disks` and `node.mirrors` in the device's config | You empty the disks and must not touch the mirror |
| The S3 key pair, the TMDB key | your password manager | No command prints a stored secret back |
| Users, groups and grants | `tailraid user ls`, `tailraid group ls`, `tailraid access ls` | They are made again with the CLI |
| Which devices were voters | `tailraid node ls` | `join --voter` asks for it again |

## 2. Take each device down

Do this on every device. Each block shows the config first: note the disk paths it lists, and the mirror path, which you leave alone. `--forget-identity` removes the device's mesh key from the keychain, so 0.9.1's own binary must run it, before you update.

**macOS.** 0.9.x ran as a root daemon with its home under `/Library/Application Support/tailraid`:

```sh
sudo grep -A3 -E 'disks:|mirrors:' "/Library/Application Support/tailraid/config.yaml"
tailraid umount
sudo tailraid uninstall --forget-identity
sudo rm -rf "/Library/Application Support/tailraid" ~/Library/Application\ Support/tailraid
sudo find ~/.tailraid -mindepth 1 -delete      # each disk path the config listed; keep the directory
```

**Linux.** `uninstall` asks for sudo itself:

```sh
grep -A3 -E 'disks:|mirrors:' ~/.config/tailraid/config.yaml
tailraid umount
tailraid uninstall --forget-identity
sudo rm -rf ~/.config/tailraid
sudo find /srv/tailraid -mindepth 1 -delete    # each disk path the config listed; keep the directory
sudo iptables -S tailraid-input                # should fail: the chain goes when serve stops
```

If the chain is still listed (a node killed rather than stopped), remove it with `sudo iptables -D INPUT -j tailraid-input`, `sudo iptables -F tailraid-input` and `sudo iptables -X tailraid-input`.

**Windows.** In an elevated PowerShell:

```powershell
Select-String -Context 0,3 -Pattern 'disks:|mirrors:' "$env:ProgramData\tailraid\config.yaml"
tailraid umount
tailraid uninstall --forget-identity
Remove-Item -Recurse -Force "$env:ProgramData\tailraid"
Get-ChildItem -Force C:\.TailRAID | Remove-Item -Recurse -Force    # each disk path the config listed; never the mirror's
Remove-Item (Join-Path (Split-Path (Get-Command tailraid).Source) wintun.dll)
```

The old cluster's keys for its secrets store and its users stay in each keychain under names carrying the old cluster ID. Nothing reads them again; delete them from Keychain Access, the Secret Service or the Credential Manager if you like.

## 3. Install Tailscale and 0.10

Install Tailscale on every device and sign it in to the same tailnet (`tailscale up`, or `tailscale up --login-server <url>` for headscale), then check that each device sees the others in `tailscale status`. Then update the binary; on Windows from an elevated prompt:

```sh
tailraid update
tailraid version            # v0.10.1 or later
```

Use 0.10.1 or later: 0.10.0's CLI gave a restore ten seconds before it gave up, which a whole volume does not fit in.

## 4. Create the new cluster

On the first always-on device:

```sh
tailraid init               # prints the join line and the admin invite
tailraid install
tailraid node add-disk /srv/tailraid
tailraid mount
```

On every other device, with the first device's Tailscale name or IP and the token `init` printed (`tailraid token` prints it again), and `--voter` on the devices that are always on:

| OS | Commands |
|---|---|
| macOS | `tailraid join pike <token>` · `tailraid install` · `tailraid node add-disk ~/.tailraid` · `tailraid mount` (no sudo: 0.10 runs as you, a LaunchAgent) |
| Linux | `tailraid join pike <token> --voter` · `tailraid install` (asks for sudo) · `tailraid node add-disk /srv/tailraid` · `tailraid mount` |
| Windows | elevated: `tailraid join pike <token> --voter` · `tailraid install` · `tailraid node add-disk C:\.TailRAID` · then as yourself: `tailraid mount T:` |

Then, on the device you administer from, create yourself from the invite `init` printed (within 24 hours; `tailraid user bootstrap` on a voter mints another):

```sh
tailraid user create kim <code>
tailraid status             # every device live, the policy grown with the disks
```

If the cluster had an S3 bucket or a TMDB key, enter them again (values from stdin) and add the bucket under its old name, with the flags of [guide-capacity.md](guide-capacity.md):

```sh
tailraid secret set s3/b2/access_key
tailraid secret set s3/b2/secret_key
tailraid node add-s3 b2 --endpoint <url> --region <region> --bucket <bucket> --prefix <prefix> --capacity 1T
tailraid secret set tmdb
```

The old bucket objects under the prefix belong to no chunk of the new cluster, and the leader's daily S3 sweep deletes them.

## 5. Attach the mirror

Plug the mirror disk into a device of the new cluster and adopt it there. `--adopt` rewrites the disk's marker to the new cluster and this device; its snapshots stay and become this cluster's on the device's next minute:

```sh
tailraid mirror add /Volumes/Archive/tailraid --adopt     # macOS, Linux
tailraid mirror add E:\tailraid --adopt                   # Windows, elevated
tailraid snapshot ls                                       # the old snapshots, before-0.10 among them
```

The mirror is named after its new device (`pike/m1`). Its schedule starts at once, so a snapshot of the still empty volume is taken first; it costs nothing, and the imported snapshots are thinned by the same table as before.

## 6. Restore

The pool needs room for what the snapshot holds (`BYTES` in `tailraid snapshot ls`) times the policy's overhead (`tailraid volume describe` shows the policy and the free space). Once a user exists, a restore is an admin's operation, so run it as yourself. It is fastest on the mirror's own device, which reads the disk directly: give your user a key there first (`tailraid user key invite` on your device, `tailraid user key add <code>` on the mirror's).

```sh
tailraid snapshot restore pike/m1/before-0.10 /
```

The command waits until the whole tree is back, which for a large volume is hours, and prints what it restored. A restore never overwrites, and each file lands whole or not at all. So if it stops (Ctrl-C, a restart, a dropped link), run the same command again: everything already there is skipped. To bring the volume back a folder at a time, name the folder instead of `/`. The browser's as-of menu restores a folder too ([guide-snapshots.md](guide-snapshots.md#browse-restore-and-revert)).

Then make the users, groups and grants again from the notes of step 1 ([guide-operating.md](guide-operating.md#users)), and finish with:

```sh
tailraid check              # exit 0: nothing degraded
```
