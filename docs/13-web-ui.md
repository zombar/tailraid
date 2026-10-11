# 13 — Web UI: dashboard and file browser

The web UI is another gateway next to the NFS and SMB gateways (docs/07, docs/15): one `http.Handler` over
the same vfs and FSM, on its own tailnet listener `<tsIP>:20482`. It shows the cluster the
way `tailraid status --json` reports it (data balance across nodes, chunk integrity, the
repairer's state) and lets a browser on any of the user's tailnet devices browse the volume,
stream media, upload files, make folders, rename, move and delete, restore what was deleted
(the trash of docs/02 §Apply rules), flag folders and keep a folder cached on the device
serving the page. It is read-only for cluster administration: every mutating admin
operation stays in the CLI.

## 1. Scope

- **Dashboard**: the `admin.Report` of docs/02 and docs/06 (volume policy and usage, per-node
  capacity, slots and liveness, degraded / repair-pending / unreadable / stale-policy counts,
  the repairer's last pass, the gateway's dirty bytes and WAL) polled every 5 s, plus a
  per-file drill-down from `admin.DescribeChunks`: each chunk's hash, (k,m), live slots and
  which node holds each shard. A **Disks** block under Volume draws the data spread
  across targets: each host a card with one row per disk, each row's slots against its
  expected share, and the volume's "survives N disks, M hosts" (docs/14 §8). Hosts with
  no added disk show one cell, their own store; the block is drawn whenever any host has
  more than one target.
- **Stats**: a third tab, `#/stats`, draws the soak report's charts (docs/09 §6) over the
  history every node keeps inside the volume (docs/17): stored against logical bytes,
  chunks, the repair backlog and the bytes a pass moves, liveness lanes, the data spread
  across disks and the slots per node for the cluster, then one member's client
  throughput, tailnet and total network rates, CPU, memory, pressure, data disk, disk
  throughput, WAL depth, process memory and readahead. A range picker in the Grafana
  style (1h, 6h, 24h, 7d, 30d, 1y, or a custom from and to) and the member choice live in
  the URL hash, so a link reproduces a view; a range ending now refreshes every 5 s
  while the tab is visible. The member dropdown lists every device member with its
  liveness and, since every member serves its own page, links each one's Stats page at
  the same range by its tailnet address and web port (which `/health` now carries,
  docs/08). The charts come from `GET /api/stats` as server-rendered SVG, the report's
  own drawing code, so nothing is lost between the report and the live page (§Design).
- **Theme switch**: a button at the right end of the header cycles system, light and
  dark; the choice stays in the browser (`localStorage`) and is applied before the first
  view renders (§Design, I-200).
- **File browser**: list, stat, stream (HTTP Range), download, upload (`PUT`), make
  folders, rename, move and delete any number of selected entries at once (one proposal
  per entry, every entry reported on its own, §4), and a tile view next to the list with a
  thumbnail per image, video and album (§5).
- **Trash**: a delete from anywhere (this UI, an NFS or SMB mount) moves the entry out of
  the namespace instead of dropping it (docs/02 §Apply rules, docs/06 §1). The share sees
  a plain delete; this UI still lists the entry in its old folder, dimmed and dated, and a
  banner on it restores it or deletes it for good. `trash.retention` (docs/12, 30 days)
  purges it otherwise.
- **Folder flags**: `protected` (the FSM refuses to delete, replace or move out anything
  under it, from every gateway) and `nsfw` (this UI blurs its tiles and previews until
  tapped; nothing else changes). Set from the folder menu, shown as badges.
- **Local cache**: "Keep on this device" pins a folder so the node serving the page keeps
  every chunk under it in its local chunk cache (docs/01 §Local chunk cache); the
  dashboard's Cache card shows what is pinned and how much of it has arrived.
- **Media**: audio, video and images play in the browser's own elements; the node serves
  files as stored, with no transcoding. Audio gets more (§6): a folder of tracks renders
  as an album with its cover and tags, playback continues in a bar at the bottom while
  the user browses anything else, with a queue, repeat and shuffle, and the lock screen
  shows title, artist and cover through the Media Session API.
- A single-page TypeScript app, no framework, bundled by esbuild into one committed file and
  embedded in the binary. Hash routing (`#/`, `#/browse/<path>`, `#/file/<path>`, `#/stats`).
- A separate listener, not a route on the RPC port: the caller set differs (same-user
  devices that are not members), a page load cannot send the `X-Tailraid-Cluster` header,
  and the RPC port's member-only rule must stay exactly as docs/08 states it.
- **`tailraid dashboard`** opens the UI from a terminal: it asks the node for its status,
  prints the `web_url` there (the address `status` shows on its `web` line), and hands it to
  the OS's opener: `open` on macOS, `xdg-open` on Linux, `rundll32
  url.dll,FileProtocolHandler` on Windows (no shell, so nothing in the URL is interpreted).
  `--print` prints it and opens nothing, for an SSH session; `--node` asks another node.
  With the UI off it fails, saying `listen.web` is 0 (I-143).

## 2. Who may use it

Every request is authorized from the TCP source address alone, before any body byte is
read, in this order:

1. `rpc.SourceAddr(r)`; no parseable source → 403.
2. `Authorizer.Allow(src)`: a cluster member whose WhoIs StableID matches its member record
   (docs/08 steps 2–4) → allowed. A WhoIs transport failure → 503.
3. Otherwise `Authorizer.User(src)`: the WhoIs `UserProfile.LoginName` of the source. It is
   allowed when it is non-empty and equals this node's own login name. A WhoIs failure → 503;
   anything else → 403, logged at warn with the source and the login it presented.

The node learns its own login once at `serve` start by `WhoIs`-ing its own Tailscale IP. If
that fails the UI runs members-only (`Login: ""`) and a warning is logged.

Tailscale reports `LoginName: "tagged-devices"` for every tagged node, so two tagged devices
would authorize each other. `(*tailscale.WhoIs).Login()` therefore returns `""` whenever
`Node.Tags` is non-empty, on the caller's side and on this node's own side, and an empty
login never matches. The authorizer caches the login next to the StableID with the same TTLs
and the same purge on membership change as the RPC rule.

What this gives: the user's phone, tablet or laptop that is not a storage node can open the
UI; a guest device on the tailnet, another user's device and a tagged server cannot.

The listener also answers only to its own name: a request whose `Host` is neither the
listener's `<tsIP>:port` nor the node's MagicDNS name with the port is a `421` before
anything else runs, so a page on another origin that points a name of its own at this
node's IP (DNS rebinding) gets nothing from it; the index page's CSP carries
`frame-ancestors 'none'`, so no other page can frame the UI; and `/api/status` is the
admin report without the join token, which belongs to the CLI on a member (I-127).

## 3. Endpoints

| Method and pattern | CSRF header | Response |
|---|---|---|
| `GET /` | – | `index.html`; `text/html; charset=utf-8`; `Cache-Control: no-cache`; CSP `default-src 'self'; img-src 'self' data:; media-src 'self'; frame-src 'self'; frame-ancestors 'none'` |
| `GET /assets/{file}` | – | `app.js` (`text/javascript`), `app.css` (`text/css`); `no-cache`; anything else 404 (no SPA fallback: routing is in the hash) |
| `GET /api/status` | – | the `admin.Report` JSON without `join_token`, cached 2 s; `Cache-Control: no-store` |
| `GET /api/ls?path=&in=` | – | `{"path":"/a","dir":{protected,nsfw,pinned,deleted_at?,purge_at?,cover?},"entries":[{name,type,size,mtime,mode,ino,version,dirty?,target?,protected?,nsfw?,deleted_at?,purge_at?,thumb?,cover?}]}`, live entries sorted by name, then the folder's deleted roots newest first, without macOS's `._<name>` and `.DS_Store` and without `/.tailraid` (I-140); `type` is `file`, `dir` or `symlink`; `version` is the file's commit count, the thumbnail key's last part (§5); `mtime`, `deleted_at` and `purge_at` RFC 3339 UTC; `dirty` only on files; `dir.protected` and `dir.nsfw` are effective (the folder or an ancestor carries the flag), an entry's own flags are its own; `cover` names the audio entry whose picture is the folder's tile (§5), on the listed folder and on each folder entry, once this node has resolved it: a listing reads no file, `GET /api/thumb` of the folder resolves it (I-165); `in=<ino>` addresses a deleted root and `path` is then relative to it (§4) |
| `GET /api/stats?from=&to=&node=` | – | `{"range":{from,to,live,tier,step},"node","nodes":[{name,addr,live,self,web?}],"charts":[{id,title,svg,caption}]}`: the charts of docs/17 §4 over `[from, to)` (Unix seconds; `to=now` or absent for the live edge), server-rendered SVG from the finest tier whose retention covers `from`, merged with every live member's unsealed rows when `to` is now (I-196); `node` names the member whose node charts are drawn (default: this node), `all` draws one series per member; `nodes` carries each device member's tailnet IP, liveness and web port for the member menu's links; a missing or malformed `from`, `from ≥ to` or a range longer than `stats.keep.coarse` → 400; no query wired (`listen.web` on a node without the sampler) → 503; `Cache-Control: no-store` |
| `GET /api/stat?path=&in=` | – | one entry plus `path`, with effective `protected` and `nsfw` |
| `GET /api/trash` | – | `{"entries":[{…entry, path, restorable}]}`: every deleted root, newest first, `path` its original place, `restorable` false while that place is itself deleted |
| `GET /api/chunks?path=&offset=&limit=` | – | `{"total":N,"chunks":[ChunkReport…]}`; default limit 64, max 256; the event ring is omitted; the shard checks of a page run concurrently and `slots` is always an array (I-161) |
| `GET`, `HEAD /files/{path}?in=` | – | the bytes through `http.ServeContent`; `?download=1` adds `Content-Disposition: attachment`; `in=<ino>` reads inside a deleted root with the same headers |
| `PUT /files/{path}` | required | the whole file (`Content-Length` required, 411 without) or one part (`Content-Range: bytes a-b/total`): the bytes stream into `.<name>.tailraid-upload` beside the target and the last byte renames it over the target; 201 for a new file, 204 for an overwrite (the previous version is now in the trash, §4), 202 `{"offset"}` for a part that is not the last; a part whose `a` is not the temp's size → 409 `{"offset"}`; `If-None-Match: *` on an existing file → 412; more than four uploads at once → 503 `Retry-After: 2`; a size the volume cannot hold → 507 (§4); an overwrite under a protected folder → 403 |
| `POST /api/mkdir` `{"path"}` | required | 201 `{"path"}`; parents are created; an existing entry → 409 |
| `POST /api/rename` `{"path","name"}` | required | 200 `{"path"}` (the new path); a taken name → 409; a protected folder, or a replace under one → 403; the root → 400 |
| `POST /api/move` `{"paths":[…],"to"}` | required | 200 `{"moved":[…],"failed":[{path,error,status}]}`; `to` must be a live directory (404/400 for the whole request otherwise); every path is tried on its own, one `Rename` each, never over an existing entry (409 for that item); at most `MaxBatch` (1000) paths, 413 above, 400 for none |
| `POST /api/delete` `{"paths":[…]}` | required | 200 `{"deleted":[…],"failed":[…]}`; each entry lands in the trash (a folder as one unit); 403 for an item under a protected folder; the same `MaxBatch` bound |
| `POST /api/restore` `{"ino","to"?,"name"?}` | required | 200 `{"path"}`; `to` and `name` default to the original place; a taken name → 409 `{"error","taken":true}`; an original parent that is itself deleted → 409 `{"error","in_trash":true}` (the page then asks for a destination); not a deleted root → 404 |
| `POST /api/purge` `{"ino"}` or `{"all":true}` | required | 204; permanent: the entry's references are dropped and GC reclaims its chunks after grace (docs/06); `all` proposes purges until the trash is empty |
| `POST /api/flags` `{"path","protected"?,"nsfw"?}` | required | 200 the entry; directories only (400), never the root (400), never inside the trash (409) |
| `GET /api/thumb?path=&in=` | – | `image/jpeg`, at most 320 px wide, from `/.tailraid/thumbs/<ino>-<gen>-<version>.jpg` (§5); an image whose thumbnail is missing is decoded and scaled by the node first (JPEG, PNG, GIF, WebP; two at a time; more than 64 megapixels → 415); audio answers its embedded picture or a `cover.*`/`folder.*` sibling; a folder answers its cover track's picture (§5), resolving which track that is on the first request and remembering it for the folder's version; a video, a folder without a cover or anything else without a stored thumbnail → 404 `{"error":"no thumbnail"}` |
| `PUT /api/thumb?path=&version=` | required | a JPEG body of at most 256 KiB, the frame the browser grabbed from the middle of a video (§5): 201; not a JPEG → 415; `version` not the file's current `Version` → 409 |
| `GET /api/tags?path=&in=` | – | audio only (400 otherwise): `{title,artist,album,album_artist,track,tracks,disc,year,genre,has_cover}` from the file's ID3v2, ID3v1, MP4, FLAC or Ogg tags (§6); a file without tags, or one whose tags do not parse, answers 200 with empty fields, never an error |
| `POST /api/pin` `{"path","pinned"}` | required | 200 `{"path","pinned","bytes"}`; directories only (400); local to the node serving the page (§4) |
| `GET /api/pins` | – | `{"size","used","pinned_bytes","pins":[{path,bytes,cached_bytes}]}`, the same object `/api/status` carries as `cache` |

- Every error is a non-2xx status with a JSON body `{"error":"…"}`. Mapping by `errors.Is`:
  `ErrNotExist` 404 · `ErrExist` 409 · `EISDIR`, `ENOTDIR`, `EINVAL`, `ENAMETOOLONG` 400 ·
  `EROFS` (stale gateway, docs/02) 503 with `Retry-After: 5` · `ENOSPC` 507 · `EIO` 503 ·
  `admin.ErrAmbiguous` 409 · `ErrProtected` 403 · `ErrInTrash` 409 · `ErrCodec` (a member
  too old for the command, docs/12 §6) 503 with `Retry-After: 30` · anything else 500,
  logged.
- The batch routes answer 200 whenever the request itself was valid and report every item:
  an item is moved or deleted whole or not at all, a failure of one item changes nothing
  about the others, and the page shows the failures by name (I-159).
- Every `/files/` response carries `Accept-Ranges: bytes`, `X-Content-Type-Options: nosniff`
  and `Content-Security-Policy: sandbox`: a stored HTML or SVG file opened from the volume
  renders inert and cannot call the API as the viewer.
- Mutating routes require the header `X-Tailraid-UI: 1`, checked before the body is read. No
  response carries a CORS header and there is no `OPTIONS` handler, so a page on another
  origin cannot make the browser send that header.
- Content types come from `mime.TypeByExtension` after an override table for what Go's table
  lacks or gets wrong: `.mkv video/x-matroska`, `.m4v video/mp4`, `.mov video/quicktime`,
  `.flac audio/flac`, `.m4a audio/mp4`, `.opus audio/ogg`, `.heic image/heic`,
  `.md text/markdown; charset=utf-8`, `.ts application/octet-stream`. Unknown extensions are
  `application/octet-stream`; nothing is sniffed.
- Paths are cleaned with `meta.SplitPath` (which resolves `.` and `..`, collapses repeated
  separators and maps `\` to `/`) and resolved only through the vfs, so a request can never
  name anything outside the volume tree and the package touches no host path. The canonical
  `/a/b` form is echoed back. File operations on the root or a directory are 400. The
  reserved folder `/.tailraid` (§5) is left out of every listing and refused as the target
  of every user operation with 400.
- Request bodies are bounded before they are decoded (I-116): 4 KiB for the routes that
  name one entry (`mkdir`, `rename`, `restore`, `purge`, `flags`, `pin`), 256 KiB for
  `move` and `delete` (a batch of `MaxBatch` paths of `MaxPathLen` bytes does not fit,
  which is the point: the page sends a larger selection in several requests), 256 KiB
  for a thumbnail.

## 4. File semantics

- **Reads** are `http.ServeContent` over a `vfs.File` (`Read`, `ReadAt`, `Seek`), so a
  Range request returns exactly the bytes a READ on this gateway returns at that offset
  right now: committed chunks with the overlay's dirty extents on top (docs/04 §6). Size is
  discovered with `Seek(0, SeekEnd)`, which the overlay answers while the file is dirty.
- **ETag** is `"<ino>.<gen>.<version>"` and is sent only while the file is clean. `Version`
  bumps on commit, not on a WAL write, so a dirty file sends no validator and a zero modtime;
  a conditional GET can therefore never 304 stale bytes.
- **Uploads** are atomic and resumable, and the vfs is their only state. The body streams
  through `OpenFile(O_WRONLY|O_CREATE)`, `Seek` and `Write` on a hidden sibling
  `.<name>.tailraid-upload` in the target's directory, the path an NFS WRITE takes: each
  chunk-sized read is one fsynced WAL record, the flusher commits chunks on the idle timer
  or the thresholds (a WRITE at a cap waits inside the vfs, docs/04 §3), and the bytes are
  readable through the vfs at once. Only when every byte announced by `Content-Length` (or
  the `Content-Range` total) has arrived does one `Rename` publish the temp over the target
  (replacing a file, never a directory), so the previous content is untouched until the
  upload is complete and a disconnect, a short body or any error leaves it as it was: a
  whole-file upload loses its temp, a part is rolled back to its start so the next attempt
  resumes there. A file larger than 32 MiB is sent by the browser as
  sequential parts with `Content-Range: bytes a-b/total`; a part is accepted only when `a`
  equals the temp's current size (0 when there is none), otherwise 409 with the offset the
  client should continue from, which is also how a dropped connection resumes (the browser
  asks `/api/stat` for the temp's size, or simply retries and reads the 409). Temps show in
  listings with `"upload": true`, are skipped by the player, and are removed by the node's
  hourly sweep once idle for 24 h (I-81).
- Before any body byte is read the gateway refuses what cannot succeed: no length (411),
  an announced size the volume cannot hold at the current policy (507, from the cached
  status), a fifth concurrent upload (503 with `Retry-After: 2`); and it drops an upload
  whose body sends nothing for `BodyTimeout` (60 s) together with its temp, so a vanished
  client never pins a slot or a buffer (I-82). Memory per upload is one chunk-sized buffer
  (docs/04 §9).
- A new file answers 201, an overwrite 204. `POST /api/mkdir` creates parents like
  `MkdirAll`. Both are refused with 503 on a stale gateway (`EROFS`) and change nothing.
- **Trash.** A delete is `OpTrash` (docs/02): the entry leaves its parent's name table,
  keeps its inode, its chunks and their references, and is indexed under the parent it
  came from. The NFS and SMB gateways see exactly a delete: LOOKUP fails, READDIR omits
  the name, the handle is stale, a new entry may take the name at once; only the free
  space does not move until the purge. This UI lists a folder's deleted roots after its
  live entries with `deleted_at` and `purge_at`, dimmed, and addresses them by inode:
  `in=<ino>` on `ls`, `stat`, `files`, `thumb` and `tags` reads inside the deleted root,
  so a deleted folder is browsed and a deleted video plays as before the delete, from the
  same bytes (I-158). A folder deleted from a mount with `rm -rf` arrives piece by piece
  and is folded into one entry when the folder itself is deleted (docs/02), so it restores
  with one tap; the mount that deleted the names shows them again once its attribute
  cache expires (an NFS client's `acdirmax`, up to 60 s, docs/07), the browser at once.
  Restore puts the entry back under its original name, or under the name or
  into the folder the page asks for after a 409, with the same `(ino, gen)`, so an NFS
  handle that went stale is valid again. "Delete now" purges one entry; the retention
  purge runs on the leader (docs/06 §2). An overwrite, whether a `PUT` of an existing
  name here or an editor's save-by-rename on a mount, puts the previous version in the
  trash under its old name; the upload's own temp and the other ephemeral names of
  docs/06 §1 never enter it.
- **Protected and nsfw** are the inode flags of docs/01, set with `OpSetAttr` on a
  directory. The FSM enforces `protected` for every gateway (docs/02 §Apply rules,
  I-154): this UI greys out Delete and Move for a selection that touches a protected
  folder and names it in the tooltip, and shows the server's 403 for anything it did not
  catch. `nsfw` has no meaning to the FSM; this UI blurs the tiles and the viewer of
  everything under the folder until the user taps "show" (I-163). Both flags are
  inherited for display: `ls` and `stat` carry the effective value next to the entry's
  own.
- **Pins** are local state of the node serving the page (`<data_dir>/pins.json`, never in
  Raft): a pinned folder's chunks are prefetched into the node's chunk cache and kept out
  of its eviction, renames keep the pin (it is stored by inode), and `/api/pins` and the
  Cache card report how much of each pin has arrived (docs/01 §Local chunk cache, I-162).
  A pin is "Keep on this device" in the folder menu, for the device whose URL is open.

## 5. Tiles and thumbnails

The tile view (`.ftiles`) shows one tile per entry with a thumbnail where one exists:
images, videos, audio files with a picture, and folders of music through their first
track's cover. Thumbnails live in the volume so every device shares them:
`/.tailraid/thumbs/<ino>-<gen>-<version>.jpg`, at most 320 px wide, written through the
same vfs path as an upload (temp beside the target, one `Rename`), so a thumbnail is a
file like any other: replicated, erasure-coded and deduplicated. `/.tailraid` is a plain
folder on the mount, left out of this UI's listings and refused as a target of user
operations (400). The key carries `Version`, which bumps on every commit, so a changed
file gets a new thumbnail and the hourly sweep (docs/06 §3) removes any whose
`(ino, gen, version)` no longer names a file (I-160).

- **Images** are made by the node on the first `GET /api/thumb` for the file: decoded
  with Go's own `image/jpeg`, `image/png`, `image/gif` and `golang.org/x/image/webp`,
  scaled with `x/image/draw`, encoded as JPEG at quality 80 and stored. Two decodes run at
  a time and a picture above 64 megapixels is refused (415) rather than decoded: the
  decode cost is bounded by the pixel count, which the header gives before any pixel.
  HEIC has no pure-Go decoder; those tiles stay generic.
- **Videos** have no pure-Go decoder either, so the browser makes the thumbnail with the
  decoder it already uses to play the file: the page loads the file into an off-screen
  `<video preload="metadata">`, seeks to half its duration, draws the frame on a 320-px
  canvas, encodes a JPEG and `PUT`s it with the file's `Version`. The node stores it like
  an image thumbnail and every later tile on every device reads it back. A video the
  browser cannot decode (mkv, avi where no decoder exists) keeps the generic tile.
- **Audio** answers its embedded picture (§6), else a `cover.*`/`folder.*` sibling, scaled
  like an image. A folder's own tile uses its first audio entry by name that has a
  picture (`dir.cover`), chosen the same way on every node (I-165). Finding it reads each
  candidate's tags, so the folder's tile asks `GET /api/thumb?path=<folder>` and the node
  resolves the cover then, once per folder version, and remembers it; a listing reports
  `cover` only when already known and reads no file (measured 2026-10-11: a cold listing
  of 99 album folders took 5.5 s while it resolved every cover in line).
- The page loads thumbnails lazily (three at a time as tiles scroll into view) and never
  more than one `PUT` per file per version; a tile under an nsfw folder draws its
  thumbnail blurred.

## 6. Audio

A folder whose audio files outnumber its other files renders as an album (`.album`): the
cover, the album and artist from the tags of its first track, the year, the track count
and the total length once the browser has reported each file's duration, then the tracks
in track order with their titles. Tags come from `GET /api/tags`, read by the node with
`github.com/dhowden/tag` (ID3v2 and ID3v1, MP4 atoms, FLAC and Ogg comments; pure Go) over
at most the first 10 MiB of the file (an ID3v2 tag with its picture) and its last 128 B
(ID3v1), so a tag read never streams a file; a file without tags, or with tags the parser
rejects, answers empty fields (I-164). The tile view and the list use the same tags for
titles where it helps and never hide the file name.

Playback belongs to a bar at the bottom of the page (`.nowbar`) that the router never
touches: a track started anywhere keeps playing while the user browses other folders,
the dashboard or a video page (which pauses it). A change of track or of play state
re-marks the listing's rows in place (the playing row, the play buttons): the listing is
neither fetched nor rebuilt, so nothing flickers. The selection bar, the now-playing bar
and the queue carry `hidden` while they have nothing to show, and the stylesheet lets the
attribute win over any display a class sets. The bar carries the cover, title and
artist — album, previous · play/pause · next, a seek slider with the elapsed and total
time, repeat (off, all, one) and shuffle, and opens into the queue. Playing a track queues
its folder in track order; "Play all" and "Shuffle" on an album, "Play" and "Add to queue"
on a selection do the same for any set of audio files or folders. The next track is
preloaded in a second hidden `<audio>` and swapped in when the first ends, so an album
plays through without a gap where the browser allows it. `navigator.mediaSession` carries
the title, artist, album and cover and the play, pause, previous, next, seek-to and 10-s
seek actions, which is what the lock screen and the headphone buttons use. The current
track and position survive a reload through `sessionStorage`; the queue lives in the tab.
Keys: space play/pause, `←`/`→` seek 10 s, shift with them previous/next, `q` the queue.

## 7. Status cache and chunk drill-down

`admin.Build` walks every chunk (`ScanHealth`); the node builds the report off the request
path and serves the latest build (docs/12 §8, I-205). The handler encodes it once and
serves that body for 2 s, single-flight under a mutex, so several tabs polling at 5 s cost
one encoding. `/api/chunks` issues one HEAD per live slot per chunk with a 3 s timeout
(`admin.DescribeChunk`), which is why it pages: 64 chunks per call by default. The HEADs of
one page run concurrently, sixteen at a time, so a page of 64 chunks at `3+2` costs the
tailnet round trip a few times over rather than 320 times in a row (the sequential form
took 5.6 s on a four-node tailnet, with nothing on screen meanwhile); the panel shows a
loading row at once, keeps the rows it has when a later page fails, and is built once per
file so the page's second draw (after the folder listing arrives) re-attaches it instead
of discarding a panel the user had opened (I-161).

## 8. Build and assets

The TypeScript lives in `internal/webui/src/`; `make web` runs esbuild (a Go tool pinned in
`tools.mod`, no node or npm anywhere) and writes `internal/webui/dist/app.js`, which is
committed and embedded. `make web-check` rebuilds into a temporary directory and fails when
the committed file is stale; it is part of `make check` and CI. The stylesheet
`src/app.css` and `static/index.html` are hand-written and embedded as they are. esbuild
strips types; a `tsconfig.json` is kept so editors type-check.

## 9. Non-goals

User management and per-user access control, mutating admin operations from the browser
(repair, GC, membership, policy), HTTPS of its own (the tailnet encrypts), serving on
loopback, transcoding, search, saved playlists, a queue shared between tabs or devices,
server-side video decoding, versions beyond the one the trash keeps.

## Design

The look is the brand sheet (`assets/brand-sheet.svg`) applied to an operations page: a
slate ink on white or on deep navy, one sky-blue accent, mono for anything a human will
compare or copy (names, paths, numbers, hashes), system sans for prose. A healthy cluster
reads as a quiet page; colour appears only where something needs attention. The stylesheet
is `internal/webui/src/app.css`; the pages in `assets/lookdev/` are the reference rendering
of every component and screen and the markup the TypeScript renders.

### Tokens

| Token | Light | Dark | Use |
|---|---|---|---|
| `--bg` | `#ffffff` | `#0f172a` | page |
| `--surface` | `#f8fafc` | `#1e293b` | raised blocks: pills, notes, inline forms, the viewer |
| `--line` | `#e2e8f0` | `#334155` | hairlines, bar tracks |
| `--text` | `#0f172a` | `#f8fafc` | ink |
| `--dim` | `#64748b` | `#94a3b8` | secondary ink (dark value lifted for 4.5:1 on navy) |
| `--accent` | `#2563eb` | `#38bdf8` | brand: wordmark, links, active tab, the `self` badge |
| `--ok` | `#16a34a` | same | live, present, healthy |
| `--warn` | `#d97706` | same | degraded, pending, stale policy, draining, absent-but-live, > 80 % full |
| `--bad` | `#d42035` | same | unreadable, dead, lost, errors, > 95 % full |
| `--fill` | ink at 55 % | same | bar fill when the value is within bounds |

The three status colours pass the data-viz validator on both surfaces (lightness band,
chroma, contrast ≥ 3:1, normal-vision ΔE ≥ 15). Green and amber sit in the colour-vision
warning band (protan ΔE 6.2), so a status colour never carries meaning alone: pills carry a
word, bars a label, slot cells a glyph (`!` absent, `×` dead, dashed = unplaced). Tints are
`color-mix(in srgb, <status> 12%, var(--bg))`; there are no separate tint tokens.

Dark mode is selected, not inverted: the same status steps, a lighter `--dim`, a lighter
accent. `color-scheme: light dark` lets native controls (video, inputs, scrollbars) follow.
The sheet honours `prefers-color-scheme` and, above it, `data-theme` on the document:
the **theme switch** at the right end of the header (`button.theme`) cycles system → light
→ dark → system, sets `data-theme` to `light` or `dark` (removes it for system) and keeps
the choice in `localStorage` under `theme`, a per-viewer convenience the server never
sees. The CSP forbids an inline script, so `src/theme.ts` applies the stored value as the
first statement of the module, before any view renders; a storage read or write that
throws (a private window, cleared site data) means system. The glyph is a half-filled disc
for system, a sun for light and a moon for dark, with the word beside it above 480 px;
`title` and `aria-label` read "Theme: dark (tap for system)" (I-200). The Stats page's
charts follow the theme through the chart tokens below (§Charts).

| Token | Light | Dark | Use |
|---|---|---|---|
| `--c0`…`--c7` | the soak report's eight hues (`#2563eb #dc2626 #16a34a #d97706 #7c3aed #0891b2 #be185d #4d7c0f`) | the same hues lifted for navy (`#60a5fa #f87171 #4ade80 #fbbf24 #a78bfa #22d3ee #f472b6 #a3e635`) | chart series, in order |
| `--under3` `--under2` `--under1` | `#1d4ed8` `#7895e4` `#c3cdee` | `#60a5fa` `#2563eb` `#1e3a8a` | the disk-spread lanes: a target under its expected share, strong to mild |
| `--even` | `#e5e7eb` | `#334155` | within ±5 % |
| `--over1` `--over2` `--over3` | `#e3c3e6` `#c272c9` `#a21caf` | `#581c87` `#a21caf` `#e879f9` | over the expected share, mild to strong |

A series hue is never a status: the lanes and the host-cap strip use `--ok`, `--warn` and
`--bad` as everywhere else.

### Type

- Sans: `system-ui, -apple-system, 'Segoe UI', Helvetica, Arial`. Body 15/1.45, small 13, meta 12.
- Mono: `'JetBrains Mono', 'Fira Code', 'Cascadia Code', Consolas, ui-monospace, Menlo`.
  Headings (h1 20 wordmark, h2 17, h3 15), the health pill, badges, tile values (22),
  node names, paths, hashes, sizes, times, counters. `font-variant-numeric: tabular-nums`
  wherever digits line up.
- No web fonts: the UI is served from the node over the tailnet and must not fetch from the
  internet; the index page's CSP is `default-src 'self'`.

### Space and shape

- Spacing steps 4 / 8 / 12 / 16 / 24; `--gutter` 16 px on every width; `main` max 1100 px.
- Radius 6 px; pills 999 px. Borders are 1 px `--line`. No shadows except the toast.
- Sections stack in one column with 24 px gaps; inside a section 12 px. Cards and tiles sit
  in `auto-fill` grids (`minmax(320px, 1fr)` cards, `minmax(140px, 1fr)` stat tiles,
  `minmax(196px, 1fr)` file tiles, five to a row at full width) so a phone gets one
  column without a media query.
  Breakpoints only hide or loosen: 480 px drops the mtime column and the header's node
  text and lets the file tiles pack two to a row.
- Touch targets ≥ 40 px (buttons, tabs, file rows 44 px). Focus is a 2 px accent outline.
- No horizontal page scroll at 360 px: every grid child has `min-width: 0`, names use
  `overflow-wrap: anywhere`, the file table is `table-layout: fixed`.

### Components

| Component | Markup | States |
|---|---|---|
| Health pill | `.pill` | `ok` healthy · `warn` degraded, stale · `bad` unreadable, no leader, unreachable; the word is the label, the dot repeats the colour |
| Badge | `.badge` | default (dim), `strong` leader/voter, `self` (accent), `warn` draining, `bad` lost |
| Live dot | `.dot` | `ok` live · `bad` dead · `off` dashed = unknown |
| Stat tile | `.tile` > `.label .value .sub` | zero: value in `--dim`; non-zero `warn`/`bad`: surface background, value in ink, a dot in the label |
| Bar | `.bar` > `i` (+ `b.tick`) | fill `--fill` within bounds, `warn`, `bad`; `off` hatched = unknown; the tick marks the expected share |
| Bar row | `.bar-row` > `.bar-label .bar .bar-val` | label left, value under the bar in mono |
| Compare bars | `.compare` of bar rows | two measures of one unit at one scale (logical vs stored); never stacked |
| Sparkline | `.spark-wrap` > `svg.spark` + `.spark-dot` | full width of its container (48 px tall) unless a layout manager places it; 2 px line in `--dim`, 12 % wash, the end dot is a positioned element coloured by the current value |
| Key/value grid | `dl.kv` | counters; a value is `warn`/`bad` only when non-zero and meaningful |
| Node card | `.card.node` | `self` outlined in accent; name row, two bar rows, mono meta footer; an S3 node's first bar row is labelled `stored`, its `stored_bytes` of its weight (a bucket reports no free space, docs/10 §4.1, I-178) and its footer names the bucket, prefix and region after the rtt where a device shows its version, applied index and address |
| File row | `table.files tr` | dir / file / symlink icon; `dirty` adds "syncing" in amber |
| Breadcrumb | `.crumbs` | mono, wraps; last segment plain |
| Upload row | `.upload` | queued (dim), uploading (bar), done `ok`, failed `bad` with the server's message |
| Inline form | `.inline-form` | the new-folder input; Enter submits, Escape cancels |
| Toast | `.toast` | `ok` / `bad`, bottom right, 6 s, click to dismiss, `aria-live="polite"` |
| Spread | `.spread` > `.spread-axis` + `.spread-plot` > `.spread-host` > `.spread-cols` > `.spread-col` | the Disks block's summary, the dashboard's still frame of the soak report's spread chart: one column per target grouped by host, the host's name running up beside its columns so a group is as wide as its disks, rising above or falling below a midline at the expected share, on a symmetric scale of ±25, ±50 or ±100 % chosen by the worst target; neutral within ±10 %, `warn` past 10 %, `bad` past 25 %; `dead` hatched, `drain` amber-tinted, `off` dashed; the host-cap note sits under it |
| Diskless host | `.host-row.diskless` + `.host-note` | a host card with no target rows and the note `gateway only · no disks`; its border dimmed |
| Warning icon | `.warn-icon` | the warnings strip's amber `!` disc at 14 px, inline after a name; the title says why (a target on the same physical disk as others: they fail together) |
| Disk rows | `.disks` > `.host-row` > `.disk` | one card per host (name, live dot, voter badge, target count and total budget), one row per target in it: name, a slots bar on one scale for the whole block with the expected-share tick (so a bigger budget reads as a tick further right), then the slot counts and used of budget; at ≤ 480 px the figures drop under the bar; `bad` dead (hatched track), `warn` draining (the word in amber before the slots to move), `off` closed (dashed outline, marker missing); `self` outlines the host card |
| Slot cell | `.cell` | present (green), `warn` `!` live but absent, `bad` `×` dead, `unplaced` dashed, `unverified` 50 % |
| Panel | `details.panel` | collapsed by default; summary names the count; a `.loading` row ("loading 64 of 1,946…") while a page is in flight, a "Retry" button in the error row |
| Empty / error | `.empty`, `.error` | "empty folder"; the error block quotes the server's `error` string and status |
| View switch | `.toolbar .seg` > two `button[aria-pressed]` | list / tiles; remembered in `sessionStorage` |
| Folder menu | `.toolbar details.menu` > `summary` + `.menu-body` of `label.menu-row` (checkbox + text + `.small.dim` note) | Protect · Mark nsfw · Keep on this device; a row is disabled with its reason on the root and inside the trash |
| Flag badges | `.badge.lock`, `.badge.nsfw`, `.badge.pin` | after the breadcrumb's last segment (effective flags) and in rows and tiles (own flags); mono words `protected`, `nsfw`, `kept here` |
| Select box | `td.sel` / `.ftile .sel` > `input[type=checkbox]` | the header box selects all; shift-click selects a range; a selected row is `tr.selected`, a selected tile `.ftile.selected` |
| Selection bar | `.selbar` > `.count` + buttons + `.spacer` + Clear | a raised surface (surface background, hairline border, an accent bar on the left, the count in accent mono) fixed above the bottom edge, shown while the selection is non-empty; "3 selected · Delete · Move to… · Rename · Play · Add to queue · Clear"; Rename only with one entry; Delete and Move disabled with the folder's name in the title when the selection touches a protected folder; Play and Add to queue only when every entry is audio or a folder |
| Folder picker | `dialog.picker` > `.crumbs` + `ul.picker-list` > `li` (+ `button`) + `.picker-actions` | the move destination; the current folder and the selection's own subtree are disabled; Enter moves, Escape closes |
| Rename form | `.inline-form.rename` | the new-folder form with the entry's name pre-filled and selected up to the extension |
| Deleted row / tile | `tr.deleted`, `.ftile.deleted` | dimmed to `--dim`, a `.badge` "deleted · 27 d" after the name, no dirty marker, links lead into the trash routes |
| Trash banner | `.banner.deleted` > text + `.actions` (Restore · Delete now) | above a deleted folder's listing and a deleted file's view: "Deleted 3 d ago · gone for good in 27 d"; after a 409 the text becomes "`Day 2` is taken in `Concerts 2024` · restore under another name" and the rename form takes the banner's next line (no nested box), or the picker opens for a parent that is itself deleted |
| File tile | `.ftiles` > `a.ftile` > `.thumb` (`img`, or the icon) + `.tname` + `.tmeta` | `auto-fill, minmax(196px, 1fr)` with a 16 px gap: five per row at the page's full width, four as it narrows, two on a phone (`minmax(140px, 1fr)` under 480 px); the thumb is 16:9, letterboxed on `--surface`; `dir`, `selected`, `dirty` (amber "syncing" in `.tmeta`), `deleted`, `nsfw` (blurred thumb), `protected` and `pinned` (glyph badges in the corner), `playing` (audio) |
| NSFW veil | `.thumb.nsfw`, `.viewer.nsfw` > `.veil` | `filter: blur(14px) scale(1.1)` inside `overflow: hidden`; the veil reads "nsfw · tap to show"; `.revealed` removes both for the session |
| Album | `section.album` > `.album-head` (`img.cover`, `h1`, `.artist`, `.meta`) + `table.tracks` (`.no`, `.title`, `.dur`) | the playing row is `tr.playing` with the equaliser glyph in `.no`; "Play all" and "Shuffle" are toolbar buttons |
| Now-playing bar | `aside.nowbar` > `.cover` + `.titles` (`.title`, `.sub`) + `.transport` + `.seek` (`input[type=range]` + `.time`) + `.modes` (repeat, shuffle with `aria-pressed`) + `.queue-btn` | fixed bottom, 56 px, hidden until something plays (`hidden`); `expanded` shows the queue panel above it; at ≤ 480 px the seek drops under the titles and the cover shrinks to 40 px; the selection bar sits above it when both show |
| Queue | `.queue` > `ol` > `li.qrow` (`.no`, `.title`, `.artist`, `.dur`, remove button) | the current item is `li.playing`; items can be removed; "Clear" empties it |
| Cache card | `.card.cache` > `.name-row` + bar rows + `.pins` > `.pin-row` | used and pinned of `cache.size`; one row per pin: path, a bar of cached over total, the figures; `warn` when the data disk is under 20 % free |
| Range picker | `.toolbar .seg` > `button[aria-pressed]` × 7 | 1h · 6h · 24h · 7d · 30d · 1y · custom; the pressed one is the range in the hash; custom opens the range form on the toolbar's next line |
| Range form | `.inline-form.range` > two `input[type=datetime-local]` + Apply + Cancel | a custom `from` and `to`; Enter applies, Escape closes; an empty or inverted range disables Apply |
| Live pill | `.pill.ok` "live" / `.pill` "paused" | at the toolbar's right: live while the range ends now and the tab is visible (a refresh every 5 s), paused for a custom range or a hidden tab; the title says when the last fetch landed |
| Member menu | `.toolbar details.menu` > `summary` ("Node: pike") + `.menu-body` of `a.menu-row` (`.dot` + name + `.small.dim` state) + `a.menu-link` (↗) | one row per device member with its live dot and state; choosing a row re-renders the node section from this gateway; the ↗ link opens that member's own Stats page at the same range (`http://<tsIP>:<web>/#/stats?…`); a member without a web listener has no link; "all nodes" is the first row |
| Chart card | `.charts` > `.chart-card` > `svg.chart` + `.small.dim` caption | the soak report's chart drawn in the page's tokens: text in `--text` and `--dim`, grid in `--line`, series in `--c0`…`--c7`, status cells in the status colours, the spread bins in the under/even/over tokens; one chart per row at every width; the caption names the unit, the window and the tier; hover shows the chart's `<title>` tooltips |
| Theme switch | `header.top button.theme` > `svg.icon` + `span` | system (half disc) · light (sun) · dark (moon); the word hides below 480 px; cycles on tap |

### Copy

Sentence case everywhere; no title case, no all-caps. Short labels: "no leader", "last scan
42 s ago", "empty folder", "37 missing". Relative time under 24 h, then `2026-10-08 14:03`.
Binary units with one decimal (1.5 GiB). Counts with thousands separators. Durations as the
server prints them (`3m0s`, `12ms`); track times as `m:ss`. Errors say what and why,
quoting the server: "mkdir photos: already exists (409)". Buttons state the action:
"Upload", "New folder", "Download", "Restore", "Delete now", "Move to…". The trash banner
counts days: "Deleted 3 d ago · gone for good in 27 d". A refusal names the cause:
"protected: unprotect Concerts 2024 to delete". Tooltips on the health pill quote the
degraded-operation table of docs/00.

### Charts

Bars are the only chart. A bar is ≤ 12 px thick on a `--line` track, square at the baseline,
rounded at the data end, 2 px of surface between adjacent bars. Fill is neutral ink unless a
threshold moves it to amber or red, so colour on a bar is always a status, never decoration.
The expected-share tick is 2 px of ink crossing the track. The sparkline carries no axis; its
caption says what it is and over what window. Every bar has a text value; nothing is
colour-alone. No dual axes, no stacked bars, no pie.

**The Stats page adds the soak report's charts** (docs/09 §6, `internal/chart`): lines
over a shared time axis with dashed event markers, liveness lanes, the disk-spread lanes
with their seven deviation bins and the hatched `dead` pattern, and bars. They keep the
report's information (every series, band, marker, bin and tooltip) and take this page's
look: the package emits no colour at all, only classes (`title`, `tick`, `grid`, `marker`,
`marklabel`, `band0`/`band1`, series `s0`…`s7`, status `chart-ok`/`chart-warn`/`chart-bad`,
`chart-bg` for the pattern's ground, bins `dev-3`…`dev3`), and `app.css` maps them to the
tokens above, so the charts follow light and dark like the rest of the page while the
report's own stylesheet maps the same classes to the report's fixed colours. The same SVG
bytes therefore draw the report's picture in the report and the page's picture here. One
chart per row at every width. The rules above stand around them: a series hue is never a
status, every lane and cell has a `<title>` with its value, the caption says what the chart
is, its unit and its window; the one thing the page asks of the drawing that the report
does not is a clock time axis in place of seconds since the run started (I-197).

## Manual checks (real tailnet, before a release)

1. Desktop Chrome, Safari and Firefox: the dashboard in light and dark (toggle the OS
   theme live); the pill through stopping a node (degraded), killing the leader (stale,
   then no leader) and stopping the daemon (unreachable).
2. Follower and leader: the integrity block links the leader from a follower and shows
   the repairer's counters on the leader; "last scan never" on a fresh cluster.
3. Polling pauses while the tab is hidden and resumes with one immediate fetch.
4. Balance: filling a device's disk lowers its budget and expected share within a minute;
   a draining node shows "slots to move"; after `node add-disk` of a large disk its host's
   bars move toward their ticks over the following repair passes (docs/14 §7); a bucket's
   card shows what it stores of its weight and its bucket, prefix and region, never a
   full red bar on an empty volume.
5. Browser: nested folders, every sort both ways, names with spaces, `#`, `%`, `?` and
   non-ASCII; a 409 toast on an existing folder name; an upload of a 100 MB file with
   progress, a cancel mid-way, five files queued in order, a 507 toast on a full volume.
   Then a 2 GB upload: `tailraid status` shows the heap flat and the WAL under its cap
   while it runs; drop Wi‑Fi mid-way and reconnect: the row reads "resuming" and the
   upload continues from the last part; the file's sha256 matches; cancel an overwrite
   mid-way: the old file is intact and the temp is gone after the hourly sweep.
6. A phone logged in as the same user: a 1 GB mp4 starts within a second and seeking
   works; the next track starts after the first tap; lock-screen controls show the name;
   an album of m4a plays through; image prev/next; no horizontal scroll on any route.
7. A tagged device and another user's device get 403; `tailraid status` prints the URL.
8. `.mkv`, `.flac` and `.avi` show the decoder note; a stored `.html` opens inert.
9. Tiles on the phone: a folder of 200 photos and videos fills as it scrolls, no
   horizontal scroll, the first video tiles appear within a few seconds of the page and
   are already there on the laptop afterwards; an nsfw folder's tiles are blurred until
   tapped; a protected folder shows the lock.
10. Trash round trip: `rm` a file on the mount, find it dimmed in the browser, restore it,
    `sha256` matches; `rm -rf` a folder on the mount and restore it with one tap; delete a
    folder in the browser and confirm it is gone from the mount at once, then "Delete now"
    and watch `tailraid status` reclaim after grace.
11. Protected: `rm` under a protected folder says "Permission denied" on macOS and
    Linux (`EACCES` from the wire's `NFS3ERR_ACCES`, docs/07) and "Access denied" on Windows; an upload of a new name into it succeeds; a
    re-upload of an existing name is refused with the reason.
12. Select ten entries across pages of the list, move them to another folder, rename one;
    a move onto a taken name reports that one entry and moves the rest.
13. Album: an album plays through with the screen locked; the lock screen shows title,
    artist and cover and its buttons skip tracks; the next track starts without a gap in
    Safari on iOS and Chrome on Android; browsing the dashboard while playing keeps the
    bar; a reload resumes at the same position; seek on a 1 GB flac.
14. Pin a folder on the laptop, wait for the Cache card to show it complete, stop every
    other node, and play a file from it.
15. Stats on the phone at 360 px: every chart fits the width with no horizontal scroll,
    the range picker's seven buttons wrap, a 24 h and a 30 d range draw within two
    seconds, the live pill turns to paused when the tab is hidden and back with one fetch.
16. The member menu: choose another member and the node charts change; the ↗ link
    opens that member's own page at the same range and node (its Host check lets it in);
    a diskless member has node charts and no spread lanes; a member with `listen.web: 0`
    shows no link.
17. The theme switch: set dark on the dashboard, reload, open Files and Stats: all dark
    with no flash of light on load; set system and change the OS theme live; the charts
    follow the theme, every series legible on navy.

## Invariants

- **I-68** — Every request to the web listener is authorized from the TCP source address
  alone, before any request body is read: the source is a cluster member whose WhoIs
  StableID matches its member record (I-18), or a tailnet device whose WhoIs login name
  equals this node's own login name; an empty login (unresolved own identity, or a tagged
  device on either side) never matches; a WhoIs failure is 503 and every other refusal 403.
- **I-69** — `GET /files/<path>`, with or without a `Range`, returns exactly the bytes a READ
  on the local NFS gateway returns at the same offset at that moment (committed chunks with
  the overlay on top), with `Accept-Ranges: bytes`, 206 for a satisfiable range, 416 for a
  start at or past the size, HEAD with the same headers and no body, and an ETag derived
  from (ino, gen, version) that is omitted while the file is dirty so no stale 304 can occur.
- **I-70** — A `PUT /files/<path>` body enters the volume through the same overlay and WAL as
  an NFS WRITE: when the response is sent every byte is fsynced in the WAL and readable with
  identical bytes through the vfs at once; a new file is 201, an overwrite 204 and truncates
  first; a PUT or `POST /api/mkdir` on a stale gateway is 503 and changes nothing; a WAL or
  dirty-byte cap waits for the flusher instead of failing.
- **I-81** — A web upload becomes visible under its name only when every byte announced by
  `Content-Length` or the `Content-Range` total has arrived: the bytes stream into
  `.<name>.tailraid-upload` beside the target and one `Rename` publishes them; a
  disconnect, a short body or an error leaves the previous content untouched, removes a
  whole-file upload's temp and rolls a part back to its start; a part whose offset is not
  the temp's current size is refused with 409 and that size; temps idle for 24 h or more
  are removed by the maintenance sweep.
- **I-82** — Before reading an upload body the web gateway refuses a missing length (411),
  an announced size the volume cannot hold (507) and more than four concurrent uploads
  (503 with `Retry-After`); a body that sends nothing for `BodyTimeout` is dropped together
  with its temp.
- **I-71** — Every mutating web endpoint requires the `X-Tailraid-UI` header and is refused
  with 403 before its body is read without it; no web response carries a CORS header; every
  `/files/` response carries `Content-Security-Policy: sandbox` and
  `X-Content-Type-Options: nosniff`.
- **I-72** — A web path is cleaned (`.`, `..`, repeated and backslash separators) and
  resolved only inside the volume tree through the vfs; the root and directories are 400 for
  file operations, a missing entry 404, an invalid name 400, and the web UI reads no host
  path.
- **I-73** — The web listener exists only when `listen.web` is non-zero and binds only the
  node's Tailscale IP; `webui.New` refuses to build without an authorizer; no web endpoint
  proposes a Raft command other than the file operations of I-70; `/api/status` is served
  from a cache so the chunk table is walked at most once per 2 s.
- **I-140** — `GET /api/ls` leaves out every entry named `._<name>` (at least one
  character after the prefix) or `.DS_Store`, the files macOS writes beside the user's
  over NFS for its extended attributes and Finder's view settings, and lists every other
  entry, every other dot file and an upload's temp included; a left-out file stays in the
  volume and `GET /files/<path>` serves it.
- **I-143** — `tailraid dashboard` opens exactly the `web_url` the node's status reports,
  with `open` (macOS), `xdg-open` (Linux) or `rundll32 url.dll,FileProtocolHandler`
  (Windows) and the URL as one argument, after printing it; with `--print` it launches
  nothing; a status without a `web_url` fails naming `listen.web` and launches nothing.
- **I-127** — The web listener answers a request only when its `Host` is the listener's
  `<tsIP>:port` or the node's MagicDNS name with that port (case-insensitively) and
  refuses every other with `421` before authorization; the index page's CSP contains
  `frame-ancestors 'none'`; `/api/status` never carries the join token.
- **I-158** — The web UI lists a folder's deleted roots in that folder with their
  `deleted_at` and `purge_at`, after the live entries; `ls`, `stat`, `files`, `thumb` and
  `tags` with `in=<ino>` read inside a deleted root and `GET /files` there returns exactly
  the bytes the file returned before its delete; `restore` returns the entry with the
  same `(ino, gen)` under its original name, or under the name or into the live folder
  the request gives, and answers 409 naming the cause (`taken`, `in_trash`) otherwise;
  `purge` of a deleted root drops every reference its files held.
- **I-159** — `move` and `delete` try every path of the request on its own and answer
  200 with each path under `moved`/`deleted` or under `failed` with its error and
  status; an entry is moved or deleted whole or not at all, a failing entry changes
  nothing about the others, `move` never replaces an existing entry, and a request with
  no path or more than `MaxBatch` paths is refused as a whole (400, 413) before any entry
  is touched.
- **I-160** — A thumbnail is stored only at `/.tailraid/thumbs/<ino>-<gen>-<version>.jpg`
  and served only from there; the node generates one itself only for an image it can
  decode within 64 megapixels, two at a time; a `PUT` is accepted only as JPEG of at most
  256 KiB for the file's current `Version`, else 415 or 409; `/.tailraid` is absent from
  every listing and refused as the target of every user operation with 400; and the
  sweep removes a thumbnail whose `(ino, gen, version)` no longer names a file.
- **I-161** — `/api/chunks` issues the shard checks of one page concurrently, at most
  sixteen in flight, in report order, and every report carries `slots` as an array
  (empty for a hole or an unknown hash), never `null`; the integrity panel is built once
  per file view and survives the view's second draw.
- **I-162** — A pin is local to the node that holds it (`<data_dir>/pins.json`, stored by
  inode so a rename keeps it) and survives its restart; a pinned folder's chunks are
  fetched into the node's chunk cache and never evicted from it while the pin stands,
  so with every other holder dead a read under the pin is served from the cache; an
  unpin returns the entries to the LRU; `/api/pins` and `status` report each pin's bytes
  and cached bytes.
- **I-163** — `ls` and `stat` carry a folder's effective `protected` and `nsfw` (its own
  or an ancestor's) next to each entry's own flags; `POST /api/flags` accepts directories
  only, never the root and never inside the trash; a mutation the FSM refuses with
  `ErrProtected` is 403 with the reason, and no request from the web UI can delete,
  replace or move out an entry under a protected folder.
- **I-164** — `/api/tags` reads at most the first 10 MiB and the last 128 B of an audio
  file, answers 200 with empty fields for a file without tags or with tags that do not
  parse, never 500, and reports `has_cover` true exactly when `/api/thumb` would serve
  the file's embedded picture.
- **I-165** — The thumbnail of an audio file is its embedded picture, else a `cover.*` or
  `folder.*` sibling in that order of names, scaled like an image; a folder's `cover` is
  its first audio entry by name that has a picture, the same on every node, and absent
  when none has one; a listing reports it only once this node has resolved it and reads
  no file to do so, and `GET /api/thumb` of the folder resolves it, serves that picture
  and answers 404 for a folder without one.
- **I-197** — One chart package renders the soak report and the Stats page: the same
  series render byte-identical SVG for both; it emits no colour and no inline `style`,
  only classes, and every class it emits is defined in `app.css` and in the report's
  stylesheet; every title, name and tooltip it emits is escaped; the page's time axis
  prints clock times and is the only difference the page asks of it.
- **I-198** — The Stats page's range and node are in the URL hash; a range ending now
  refreshes every 5 s while the tab is visible and pauses while it is hidden, a custom
  range never refreshes; the member dropdown lists every device member with its liveness
  and links each that has a web listener to its own Stats page at the same range and
  node by its tailnet address and web port, exactly the Host form that page admits.
- **I-200** — The theme switch cycles system, light and dark, sets `data-theme` on the
  document (absent for system), keeps the choice in the browser's `localStorage` only and
  applies it before the first view renders; a storage failure means system; the server
  never sees the choice.

## Tests

| Invariant | Tests |
|---|---|
| I-68 | `TestWebUI_Authorize_MemberOrSameUser`, `TestWebUI_Authorize_BeforeBody`, `TestWhoIs_User_CachesLogin`, `TestWhoIs_Login_TaggedHasNone` |
| I-69 | `TestWebUI_Get_RangeBoundaries`, `TestWebUI_Get_ETagAndHead`, `TestWebUI_Get_DirtyOverlayReadYourWrites`, `TestWebUI_Get_ContentTypes`, `TestVFS_StatReportsDirty` |
| I-70 | `TestWebUI_Put_NewAndOverwrite`, `TestWebUI_Put_SizeBoundaries`, `TestWebUI_Put_WaitsForWALCap`, `TestWebUI_Mkdir`, `TestWebUI_Mutations_StaleIs503`, `TestVFS_CapErrorsKeepSentinels` |
| I-71 | `TestWebUI_CSRFHeaderRequired`, `TestWebUI_Get_ContentTypes` |
| I-72 | `TestWebUI_Paths` |
| I-73 | `TestWebUI_New_RequiresAuthorizer`, `TestWebUI_StatusCached`, `TestWebUI_StaticAssets`, `TestConfig_ListenWeb` |
| I-81 | `TestWebUI_Put_AtomicOverwrite_DisconnectKeepsOld`, `TestWebUI_Put_ShortBody_400_NoTemp`, `TestWebUI_Put_Parts_Table`, `TestWebUI_Put_Resume_AfterFailedPart`, `TestWebUI_SweepUploads_AgeTable` |
| I-82 | `TestWebUI_Put_NoContentLength_411`, `TestWebUI_Put_FreeSpace_507`, `TestWebUI_Put_Concurrency_503`, `TestWebUI_Put_BodyStall_DropsTemp` |
| I-127 | `TestWebUI_HostCheckFramingAndToken` |
| I-143 | `TestBrowserCommand_PerOS`, `TestCLI_Dashboard` |
| I-140 | `TestWebUI_Ls_HidesOSMetadata` |
| I-158 | `TestWebUI_Delete_LandsInTrash_ListedInPlace`, `TestWebUI_TrashAddressing_LsStatGet`, `TestWebUI_Restore_TakenName_409_ThenAs`, `TestWebUI_Purge_FreesRefs`, `TestWebUI_Put_OverwriteKeepsPreviousInTrash`, `TestReport_TrashCacheChart`, `TestSoak_AllPhasesAgainstHarness` |
| I-159 | `TestWebUI_Rename_Table`, `TestWebUI_Move_PerItemResults`, `TestWebUI_Batch_SizeBoundaries`, `TestWebUI_BodyCaps_AllMutatingRoutes`, `TestSoak_AllPhasesAgainstHarness` |
| I-160 | `TestWebUI_Thumb_Image_Generated_Cached_Hidden`, `TestWebUI_Thumb_Put_Boundaries`, `TestWebUI_Thumb_SweptWhenFileChanges`, `TestSoak_AllPhasesAgainstHarness` |
| I-161 | `TestDescribeChunksPage_Concurrent`, `TestWebUI_Chunks_HolesHaveSlots`, `TestSoak_AllPhasesAgainstHarness` |
| I-162 | `TestCache_PinnedNeverEvicted`, `TestPins_PrefetchFillsThenReadsOffline`, `TestPins_RenameKeepsPin`, `TestPins_UnpinReleases`, `TestPins_SurviveRestart`, `TestPins_NewFileInPinnedDirIsFetched`, `TestConfig_CacheSize`, `TestLowDiskWarning_Boundary`, `TestReport_TrashAndCacheFields`, `TestCLI_Cache_PinUnpinLs`, `TestWebUI_Pins`, `TestReport_TrashCacheChart`, `TestSoak_AllPhasesAgainstHarness` |
| I-163 | `TestWebUI_Flags_DirsOnly_Effective`, `TestWebUI_Mutations_ProtectedIs403`, `TestSoak_AllPhasesAgainstHarness` |
| I-164 | `TestWebUI_Tags_Table`, `TestWebUI_Tags_ReadBounded`, `TestSoak_AllPhasesAgainstHarness` |
| I-165 | `TestWebUI_Thumb_AudioCoverFallbackOrder`, `TestWebUI_Ls_DirCoverDeterministic`, `TestWebUI_Ls_ReadsNoFile`, `TestSoak_AllPhasesAgainstHarness` |
| I-197 | `TestChart_ReportGolden_ByteIdenticalAfterMove`, `TestChart_NoInlineStyle_NoColour`, `TestChart_ClassesDefinedInAppCSS`, `TestReport_DefinesChartClasses`, `TestChart_EscapesHostileNames`, `TestChart_Points_Table`, `TestChart_LegendWrap_Table`, `TestChart_DropsNaNInf`, `TestChart_Spread_DeadAndDraining`, `TestChart_ClockAxis`, `TestChart_TruncateMarkerLabel`, `TestExpectedShares_WaterFill`, `TestChart_LookdevDataSetMatchesStatsPage` |
| I-198 | `TestWebUI_Stats_Route`, `TestStatic_StatsShellAndTheme`; the hash, the 5 s refresh and the member links are manual checks 15 and 16 (the TypeScript has no unit runner) |
| I-200 | `TestStatic_StatsShellAndTheme`; the cycle, the stored choice and the flash are manual check 17 |

`TestWebUI_Ls_Stat`, `TestWebUI_Ls_MarksUploads` and `TestWebUI_Chunks` cover the listing and paging shapes of §3.
