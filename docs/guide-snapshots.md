# Snapshots and mirrors

Adding a mirror, the schedule, browsing and restoring, and reading a mirror without the cluster. Design: [16-snapshots.md](16-snapshots.md).

A **mirror** is a disk given to snapshots. It is attached to any device of the cluster, it never holds the live volume, and it may be offline most of the time: a USB drive you plug in on Sundays is a mirror. While a mirror is online its device snapshots the whole volume onto it every hour, and each snapshot after the first copies only what changed. Every mirror is a complete, independent copy of the history it took: two mirrors are two backups, and either one restores everything it holds, with or without the cluster.

## Add a mirror

On the device the disk is attached to:

```sh
tailraid mirror add /Volumes/Archive/tailraid    # an empty directory on the mirror disk
```

The mirror is named after its device, `pike/m1`, `pike/m2` and so on. The device opens it at once and takes the first snapshot, which copies the whole volume; later ones copy only the difference. On Linux, `mirror add` rewrites the confined service unit so it may write there, as `node add-disk` does.

Unplug the disk whenever you like. A mirror that comes back after missing a scheduled snapshot gets one on its next minute, and the snapshots it missed are simply not taken; nothing is held in the pool for it while it is away. A disk moved to another device keeps its name and history: `mirror add` the same path on the new device.

## The schedule

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

## Browse, restore and revert

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

`snapshot restore` hands the copy to the mirror's host as a job and returns at once, printing the job's ID and where it runs. The job goes on whatever happens to your terminal: it survives the host restarting, waits while the mirror is unplugged, and retries a failure that may pass. `tailraid status` shows each job's progress while it runs, and `tailraid snapshot restore --cancel <job>` stops one; what it restored stays.

In the browser, pick a snapshot from the *as of* menu, open the folder and choose **Restore folder to…**: it starts the same job, and the toast links the dashboard, whose Mirrors section shows a card per restore with its bytes and files, its speed and the time left, and a Cancel button for whoever started it and for admins. Closing or refreshing the page changes nothing. Past 20 snapshots, the dashboard's snapshot table pages with Newer and Older.

A restore never overwrites: an existing file is skipped and counted. Each file appears under its name only once its data is in, so a restore stopped half-way is finished by running it again. A revert is itself a change, so the content it replaced is in the next snapshot.

## Without the cluster

A mirror needs nothing from the cluster to be read. Plug it into any machine with the binary:

```sh
tailraid mirror ls /Volumes/Archive/tailraid                                   # the snapshots on the disk
tailraid mirror cat /Volumes/Archive/tailraid 2026-10-12-0900Z Documents/todo.txt
tailraid mirror export /Volumes/Archive/tailraid 2026-10-12-0900Z ~/rescue      # --path to export only part of it
```

`tailraid mirror remove pike/m1` forgets a mirror and leaves the disk and every snapshot on it untouched; added back, they return. A mirror holds your files in the clear, so keep the disk where the data may be. Design: [docs/16-snapshots.md](16-snapshots.md).
