# Using the volume

The mount is an ordinary folder; this guide covers what the browser adds and the commands behind the trash, folder flags, pinned folders, search and the Media tab. Design: [13-web-ui.md](13-web-ui.md), [18-search.md](18-search.md).

## The browser UI

Every device serves a web page at `http://<tailscale-ip>:20482/`. Cluster members and your own devices (the same Tailscale user) can open it, so your phone works. Once the volume has users, each person signs in from the link `tailraid web login` prints on their own device, and sees what they have been granted ([docs/21](21-users.md) §6).

| Tab | What it does |
|---|---|
| Dashboard | Health, balance across devices and disks, repair progress, the integrity panel |
| Files | Browse, upload (resumable), make folders, select many to move, rename or delete; list or tile view with thumbnails; an album view and a now-playing bar for audio; markdown files rendered as documents |
| Media | One section per folder flagged as a library (movies, TV or music), with posters, fan art and descriptions from the Kodi/Jellyfin sidecars beside the files. Nothing is fetched from the internet |
| Stats | The cluster's history over any range: throughput, repair, data spread, and each device's CPU, memory, disk and network. Kept inside the volume, so a lost device loses none of it |

The search box in the header finds files and folders by name, audio tracks by title, artist and album, and text files by what they say. Changing the cluster stays in the CLI.

## Trash

Every delete lands in the trash. On the mount a deleted file looks deleted; the browser lists it in its old folder with a banner that restores it. The leader purges an entry 30 days after the delete (`trash.retention`).

```sh
tailraid trash ls [path]                     # deleted entries, with inode, original path, size and age
tailraid trash restore <ino> [--to dir] [--as name]
tailraid trash purge --older-than 0          # reclaim space now (or --ino <n>, --all)
```

Free space comes back with the purge, not with the delete: after an `rm -rf` to make room, purge the trash.

## Folder flags

The folder menu in the browser sets three flags on a folder and everything under it. **Protected** refuses every delete, overwrite and move out of the folder, from every mount and the browser alike. **nsfw** blurs its tiles and thumbnails in the browser. **Library** (movies, TV or music) puts the folder on the Media tab.

## Keep a folder on this device

```sh
tailraid cache pin ~/TailRAID/Music      # fetch every chunk under it into this device's cache and keep it there
tailraid cache ls                        # each pin and how much of it has arrived
tailraid cache unpin ~/TailRAID/Music
```

A pinned folder reads from the device even with every other node away, which suits a laptop on a plane. The browser's "Keep on this device" is the same switch. Pinned bytes live outside `cache.size`, the budget for ordinary caching (1 GiB).

## Search

```sh
tailraid search agnes river --under /Music     # the words ANDed, the last one a prefix
tailraid search index                          # run the leader's index pass now
```

Names are answered live from the metadata. Contents and tags are indexed by the leader every 10 minutes (`search.interval`) into `/.tailraid/index`, inside the volume.

## Posters and descriptions for the Media tab

```sh
tailraid secret set tmdb --system      # once per cluster, an admin: a free TMDB API key, typed or piped in
tailraid media scrape --dry-run        # what would be written
tailraid media scrape /Films           # write the missing .nfo, posters, backdrops, stills, album covers and game box art
```

The Media tab reads Kodi sidecars. `media scrape` fills in the missing ones from TMDB (films, shows, seasons, episodes), MusicBrainz with the Cover Art Archive (album covers) and the libretro thumbnails (game box art, by the No-Intro name), and never replaces a file that exists. A clear match is used at once; for anything else it shows the top five on a terminal, or skips with `--yes` not given. Album covers and game box art need no key: without one, `media scrape` does those and skips films and TV. Only this command talks to the internet, from the device you run it on. This product uses the TMDB API but is not endorsed or certified by TMDB.
