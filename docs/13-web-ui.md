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
  the repairer's last pass, the gateway's staged bytes with their time to drain, docs/04 §3, and its WAL) polled every 5 s, plus a
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
- **Snapshots and versions** (docs/16): the file browser shows any folder *as of* a
  snapshot on an online mirror (read-only, with a folder restore), a file's view lists its
  versions across the online mirrors (view, revert, save a copy), and the dashboard's
  Mirrors section shows every mirror, online or not, its snapshots and the sync under way,
  and takes snapshots. Taking and restoring a snapshot are the
  user's file operations; no page removes a snapshot (only the schedule's thinning does, docs/16 §4), and adding or removing a mirror stays in the CLI.
- **Local cache**: "Keep on this device" pins a folder so the node serving the page keeps
  every chunk under it in its local chunk cache (docs/01 §Local chunk cache); `tailraid
  cache ls` shows what is pinned and how much of it has arrived.
- **Media**: audio, video and images play in the browser's own elements; the node serves
  files as stored, with no transcoding. Audio gets more (§6): a folder of tracks renders
  as an album with its cover and tags, playback continues in a bar at the bottom while
  the user browses anything else, with a queue, repeat and shuffle, and the lock screen
  shows title, artist and cover through the Media Session API.
- **Search**: a box in the header finds files and folders by name across the volume and
  text files and audio tracks by what is in them, from the index the leader keeps in the
  volume (docs/18); a hit opens the file or the folder, a content hit with a snippet.
- **Markdown**: a `.md` file renders as a document on the node, escape-first (§4), with
  the raw text a tap away.
- **Media**: a fourth tab, `#/media`, browses the folders flagged as libraries (docs/18
  §9) as films, shows and music by poster and cover, with the descriptions and background
  art the volume holds (§10).
- A single-page TypeScript app, no framework, bundled by esbuild into one committed file and
  embedded in the binary. Hash routing (`#/`, `#/browse/<path>`, `#/file/<path>`, `#/stats`,
  `#/search/<q>`, `#/media`, `#/media/<lib>`, `#/media/item/<path>`).
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

With users (docs/21 §6): a session of the volume on top of the rule above. `tailraid web
login` proposes a session row in the FSM and prints
`http://<tailscale IP>:20482/login?code=…` (`--node` picks which member's address);
opening it sets an `HttpOnly`, `SameSite=Lax` cookie for that origin holding the session
token (Lax, because a hand-off is a navigation one origin starts to another and a Strict
cookie is withheld for that navigation's whole redirect chain; every mutating route is a
POST or PUT with the UI header, which Lax keeps cross-site requests off), and every node
validates a token against its replica. A cookie is per origin and each node's page
is its own, so a link to another node's page (the Stats member menu) carries a one-time
hand-off code from `GET /api/handoff` and the other node's `/login` sets its own cookie
for the same session. A request without a session gets the sign-in page (`/login`;
`/assets/*` is the only other route without one), whatever its source: an admitted
address alone admits nothing more. Every request re-reads the session's user row, so a
disabled user, a removed key or `web logout` is refused at the next request on every
node. The session's user is the actor of every request: listings show what they hold
`Read` on, mutations carry them into the FSM (I-354, I-355). Before the first user exists
the UI admits as above alone, and `/login` sends a visitor to the root. `GET /api/session`
tells the page who it is signed in as, so the header can draw the account chip.

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
| `GET /` | – | `index.html`; `text/html; charset=utf-8`; `Cache-Control: no-cache`; CSP `default-src 'self'; img-src 'self' data: blob:; media-src 'self'; frame-src 'self'; frame-ancestors 'none'` |
| `GET /assets/{file}` | – | `app.js` (`text/javascript`), `app.css` (`text/css`); `no-cache`; anything else 404 (no SPA fallback: routing is in the hash) |
| `GET /login?code=&to=` | – | with a valid one-time code (from `web login` or a hand-off): sets the session cookie for this origin and redirects to `to` (a path under `/`, default `/`); without one, or with a used or expired code: the sign-in page, `text/html`, the one command to run and, after a bad code, one line saying so (docs/21 §6, I-354); the only route besides `/assets/*` that needs no session |
| `GET /api/handoff?to=` | – | `{"url"}`: the `to` member's `/login` with a one-time code (five minutes, single use) that signs that origin into this session; the page uses it for every link to another node's page |
| `POST /api/logout` | required | removes the session row: ends it on every node; 204 |
| `GET /api/session` | – | `{"user","admin"}`: who this page is signed in as; 404 before the cluster has users |
| `GET /api/status` | – | the `admin.Report` JSON without `join_token`, cached 2 s; `Cache-Control: no-store` |
| `GET /api/ls?path=&in=` | – | `{"path":"/a","dir":{protected,nsfw,pinned,deleted_at?,purge_at?,cover?,no_cover?},"entries":[{name,type,size,mtime,mode,ino,version,dirty?,target?,protected?,nsfw?,deleted_at?,purge_at?,thumb?,cover?,no_cover?}]}`, live entries sorted by name, then the folder's deleted roots newest first, without macOS's `._<name>` and `.DS_Store` and without `/.tailraid` (I-140); `type` is `file`, `dir` or `symlink`; `version` is the file's commit count, the thumbnail key's last part (§5); `mtime`, `deleted_at` and `purge_at` RFC 3339 UTC; `dirty` only on files; `dir.protected` and `dir.nsfw` are effective (the folder or an ancestor carries the flag), an entry's own flags are its own; `cover` names the audio entry whose picture is the folder's tile (§5), `<subfolder>/<entry>` when it comes from a subfolder, and `no_cover` is true for a folder known to have none and waiting for no video's frame (I-275), on the listed folder and on each folder entry, once this node has resolved it: a listing reads no file, `GET /api/thumb` of the folder resolves it (I-165); `in=<ino>` addresses a deleted root and `path` is then relative to it (§4) |
| `GET /api/stats?from=&to=&node=` | – | `{"range":{from,to,live,tier,step},"node","nodes":[{name,addr,live,self,web?}],"charts":[{id,title,svg,caption}]}`: the charts of docs/17 §4 over `[from, to)` (Unix seconds; `to=now` or absent for the live edge), server-rendered SVG from the finest tier whose retention covers `from`, merged with every live member's unsealed rows when `to` is now (I-196); `node` names the member whose node charts are drawn (default: this node), `all` draws one series per member; `nodes` carries each device member's tailnet IP, liveness and web port for the member menu's links; a missing or malformed `from`, `from ≥ to` or a range longer than `stats.keep.coarse` → 400; no query wired (`listen.web` on a node without the sampler) → 503; `Cache-Control: no-store` |
| `GET /api/stat?path=&in=` | – | one entry plus `path`, with effective `protected` and `nsfw` |
| `GET /api/trash` | – | `{"entries":[{…entry, path, restorable}]}`: every deleted root, newest first, `path` its original place, `restorable` false while that place is itself deleted |
| `GET /api/chunks?path=&offset=&limit=` | – | `{"total":N,"chunks":[ChunkReport…]}`; default limit 64, max 256; the event ring is omitted; the shard checks of a page run concurrently and `slots` is always an array (I-161) |
| `GET`, `HEAD /files/{path}?in=` | – | the bytes through `http.ServeContent`; `?download=1` adds `Content-Disposition: attachment`; `in=<ino>` reads inside a deleted root with the same headers |
| `PUT /files/{path}` | required | the whole file (`Content-Length` required, 411 without) or one part (`Content-Range: bytes a-b/total`): the bytes stream into `.<name>.<node>.tailraid-upload` beside the target and the last byte renames it over the target; 201 for a new file, 204 for an overwrite (the previous version is now in the trash, §4), 202 `{"offset"}` for a part that is not the last; a part whose `a` is not the temp's size → 409 `{"offset"}`; `If-None-Match: *` on an existing file, or on a name another gateway publishes first, → 412 (I-226); more than four uploads at once → 503 `Retry-After: 2`; a size the volume cannot hold → 507 (§4); an overwrite under a protected folder → 403 |
| `POST /api/mkdir` `{"path"}` | required | 201 `{"path"}`; parents are created; an existing entry → 409 |
| `POST /api/rename` `{"path","name"}` | required | 200 `{"path"}` (the new path); a taken name → 409; a protected folder, or a replace under one → 403; the root → 400 |
| `POST /api/move` `{"paths":[…],"to"}` | required | 200 `{"moved":[…],"failed":[{path,error,status}]}`; `to` must be a live directory (404/400 for the whole request otherwise); every path is tried on its own, one `Rename` each, never over an existing entry (409 for that item); at most `MaxBatch` (1000) paths, 413 above, 400 for none |
| `POST /api/delete` `{"paths":[…]}` | required | 200 `{"deleted":[…],"failed":[…]}`; each entry lands in the trash (a folder as one unit); 403 for an item under a protected folder; the same `MaxBatch` bound |
| `POST /api/restore` `{"ino","to"?,"name"?}` | required | 200 `{"path"}`; `to` and `name` default to the original place; a taken name → 409 `{"error","taken":true}`; an original parent that is itself deleted → 409 `{"error","in_trash":true}` (the page then asks for a destination); not a deleted root → 404 |
| `POST /api/purge` `{"ino"}` or `{"all":true}` | required | 204; permanent: the entry's references are dropped and GC reclaims its chunks after grace (docs/06); `all` proposes purges until the trash is empty |
| `POST /api/flags` `{"path","protected"?,"nsfw"?,"library"?}` | required | 200 the entry; directories only (400), never the root (400), never inside the trash (409); `library` is one of `none`, `movies`, `tv`, `music` and needs every member on codec 6 (docs/18 §9, 503 `Retry-After: 30` otherwise) |
| `GET /api/thumb?path=&in=` | – | `image/jpeg`, at most 320 px wide, from `/.tailraid/thumbs/<ino>-<gen>-<version>.jpg`, or `….f2.jpg` for a file that is neither an image nor audio (§5, I-216); an image whose thumbnail is missing is decoded and scaled by the node first (JPEG, PNG, GIF, WebP; two at a time; more than 64 megapixels → 415); audio answers its embedded picture or a `cover.*`/`folder.*` sibling; a folder answers its cover track's picture (§5), resolving which track that is on the first request and remembering it for the folder's version; a video, a folder without a cover or anything else without a stored thumbnail → 404 `{"error":"no thumbnail"}`, for a folder whose art waits for a video's frame with `"grab":{path,ino,version}`, the video to frame, its path below the folder (I-275); one the node must make (an image, an audio file's picture or sibling cover, a folder's cover) is made in the background and waited for at most `ThumbWait` (1.5 s): not ready by then → 202 `{"later":true}` with `Retry-After: 2`, the making going on and its result stored, so the next request answers it (§5, I-374) |
| `PUT /api/thumb?path=&version=` | required | a JPEG body of at most 256 KiB, the frame the browser chose from a video (§5), stored as `<ino>-<gen>-<version>.f2.jpg`: 201; not a JPEG → 415; `version` not the file's current `Version` → 409 |
| `GET /api/tags?path=&in=` | – | audio only (400 otherwise): `{title,artist,album,album_artist,track,tracks,disc,year,genre,has_cover}` from the file's ID3v2, ID3v1, MP4, FLAC or Ogg tags (§6); a file without tags, or one whose tags do not parse, answers 200 with empty fields, never an error |
| `POST /api/pin` `{"path","pinned"}` | required | 200 `{"path","pinned","bytes"}`; directories only (400); local to the node serving the page (§4); an admin's once users exist (403 for anyone else, docs/21 §3) |
| `GET /api/pins` | – | `{"size","used","pinned_bytes","pins":[{path,bytes,cached_bytes}]}`, the same object `/api/status` carries as `cache` |
| `GET /api/search?q=&under=&limit=` | – | `{"query","hits":[{path,type,size,mtime,ino,match,title?,artist?,album?,terms:[{term,off}]}],"index"?}`: the words of `q` ANDed, the last as a prefix, names from the FSM and tags and contents from the index (docs/18 §6); `under` limits to a live folder; `limit` 50 by default, 200 at most, 400 outside `1..200`; `index: "unavailable"` when the node has no loadable index and only names answered; never an entry in the trash or under `/.tailraid` |
| `GET /api/render?path=&in=` | – | `text/html; charset=utf-8`: the file rendered as a document by the node's own markdown renderer (§4); `.md` and `.markdown` only, else 400; a source over `TEXT_MAX` (1 MiB) → 413; the ETag rule of `/files/`; `Cache-Control: no-cache` |
| `GET /api/media` | – | `{"libs":[{path,kind}]}`: every live flagged folder in kind-then-path order (docs/18 §9), the Media tab's sections |
| `GET /api/media?lib=&offset=&sort=` | – | `{"lib":{path,kind,count},"items":[{ino,path,kind,title,sort_title,year?,genres?,rating?,runtime?,poster?,fanart?,thumb?,video?,video_ino?,video_version?,video_thumb?,nsfw,children?}]}`: a library folder's items, structure live from the FSM and metadata from the catalogue (§10, docs/18 §8); `sort` is `title` (default), `year` or `added`; at most `MediaPage` (500) items from `offset`; a path that is not a flagged folder → 404 |
| `GET /api/media?item=&in=` | – | one item with `plot`, `people`, `seasons:[{n,poster?,episodes:[…]}]` for a show or `tracks` (through the tags of §6) for an album; `in=` reads a deleted root as `ls` does |
| `GET /api/snapshots` | – | `{"mirrors":[{id,name,host,self,path?,online,reason?,used,free,total,last_seen?,warn?,syncing?:{id,snapshot,read,total,bytes_new}}],"snapshots":[{id,mirror,name,at,auto,state,files,bytes,new_bytes,missing}],"schedule":{every,hourly,daily,weekly,monthly,next?}}`, snapshots newest first, from the catalogue: offline mirrors' snapshots included (docs/16 §5); a mirror's `name` is its directory under `/.snapshots` (`pike-m1`), its state from the probes (docs/16 §1); `warn` says why its card warns (`stale`, `reserve`, `incomplete`); `schedule` is this node's `mirrors` config, `next` when the newest automatic snapshot on an online mirror falls due |
| `POST /api/snapshot` `{"name"?,"mirror"?}` | required | 201 the begun rows (one per online mirror, or the one named); the serving gateway flushes its dirty files first; a taken name or a mirror already syncing → 409; an offline mirror → 409 `{"error","offline":true}`; a member too old for codec 7 → 503 |
| `GET /api/versions?path=` | – | `{"versions":[{key,size,mtime,snapshots:[{id,mirror,name}],current}],"offline":[{mirror,count}]}`: the file's distinct contents on the online mirrors, newest first, the live one `current` with the snapshots holding it (possibly none); `offline` counts what the offline mirrors would add |
| `POST /api/revert` `{"path","snapshot"}` | required | 200 `{"uploaded":n}`: the content as of that snapshot becomes current (docs/16 §6), `n` the chunks copied back from the mirror because the pool no longer held them; a dirty file is flushed first; an offline mirror → 409 `offline` |
| `POST /api/copy` `{"path","snapshot","name"}` | required | 201 the new entry beside the file; a taken name → 409 |
| `POST /api/restore-tree` `{"snapshot","path","to"?,"name"?}` | required | 202 `{"job","host","path"}`: a restore job of the snapshot's tree at `path` into `to` (its parent unless given) as `name` is recorded on the mirror's host, which runs it (docs/16 §6); the node asked checks `Admin` where it lands and forwards the start, so the answer comes once the job is recorded, not when it ends; the job's progress is in `/api/status` (`restores`); a mirror host on an older version → 501 with its version |
| `POST /api/restore/cancel` `{"job"}` | required | 204: the job stops before its next chunk, and what it restored stays; only its starter or an admin (403), forwarded to the job's host; an unknown or finished job → 404 |

- `ls`, `stat`, `files`, `thumb` and `tags` take `at=<snapid>` and then read as of that
  snapshot from its mirror (docs/16 §6): no `dirty`, `deleted_at` or `upload` fields, and
  `dir` carries `snapshot` (`{id, name, mirror, at}`); a mutating route given `at` is 400;
  an unknown snapshot 404; an offline mirror 409 `{"error","offline":true}`; `in=` with
  `at=` is 400. A thumbnail as of a snapshot is made for an image or an audio file on each
  request and never stored; a video has none (404), so its tile shows its icon.
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
- **Dropping a folder** onto a listing uploads every file in it, keeping its tree below
  the folder dropped on (I-404): the page walks the drop's entries (`webkitGetAsEntry`,
  read before the handler returns) and queues each file for its own directory, which the
  PUT makes as it creates the temp; the desktop's bookkeeping (`.DS_Store`, `._*`,
  `Thumbs.db`, `desktop.ini`) is left out and an empty folder makes nothing. The queue
  shows each file by its path below the drop and sends them one after another. Until
  2026-10-17 the page took `dataTransfer.files`, where a folder is one unreadable entry
  (with a non-zero size on macOS), so a dropped folder failed as "network error".
- **Uploads** are atomic and resumable, and the vfs is their only state. The body streams
  through `OpenFile(O_WRONLY|O_CREATE)`, `Seek` and `Write` on a hidden sibling
  `.<name>.<node>.tailraid-upload` in the target's directory (the gateway's node ID in
  it, unique where host names need not be, so uploads of one name through two gateways never share a temp, I-226), the path an NFS WRITE takes: each
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
  sends the part again from where it was and adopts the 409's offset: the temp is the
  node's own, so its name is not the browser's to know). Temps show in
  listings with `"upload": true`, are skipped by the player, and are removed by the node's
  hourly sweep once idle for 24 h (I-81), as the user whose upload made the temp: its
  name ends `.<node>.u<user>.tailraid-upload` once the cluster has users (docs/21 §4).
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
  live entries with `deleted_at` and `purge_at`, dimmed, once the viewer ticks the
  toolbar's **show deleted** (off by default, kept for the session; the toolbar's count
  says how many are hidden), and addresses them by inode:
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
- **Library** is the third folder flag (docs/18 §9): "Library: none · movies · tv ·
  music" in the folder menu, inherited for display like the other two, and what the
  Media tab (§10) lists; it needs every member on codec 6 and is refused with 503 and
  `Retry-After: 30` until then, like the trash was at codec 4.
- **Markdown (I-209).** `GET /api/render` returns a `.md` file as a document fragment the
  page inserts into `article.md.md-doc`. A table cell's alignment is the `align`
  attribute (the CSP forbids inline style), and an `#anchor` link scrolls to its heading
  without reaching the hash router. The node renders it with its own escape-first
  renderer (`internal/webui/md.go`, in the manner of errol's): the source is HTML-escaped
  line by line *before* any construct is recognised, so no tag, attribute or entity of the
  file reaches the output unescaped, and the output's tags are only the renderer's own.
  Constructs: ATX headings (the file's `#` becomes the page's `h2`, down to `h6`, each
  with an `id` from its slug), paragraphs, `*`/`_` emphasis, `**` strong, `~~` strike,
  backtick code, fenced and indented code blocks (the info string becomes `data-lang`; no
  highlighting), block quotes, ordered, unordered and task lists nested by indentation,
  pipe tables with alignment, thematic breaks, hard breaks and autolinks. **Links:**
  `http:`, `https:`, `mailto:` and `#anchor` pass, external ones with `rel="noopener
  noreferrer"`; a relative link to a `.md` becomes the UI route `#/file/<resolved path>`
  (the `in=` of a deleted root carried along), to any other volume file `/files/<resolved
  path>`; every other scheme is text. **Images:** relative paths only, served from
  `/files/`; an absolute URL becomes its alt text (the CSP forbids it anyway). A raw HTML
  block renders as escaped text. The viewer offers "Rendered | Raw" (the `<pre>` of
  today), remembered in `sessionStorage`.
- **Pins** are local state of the node serving the page (`<data_dir>/pins.json`, never in
  Raft): a pinned folder's chunks are prefetched into the node's chunk cache and kept out
  of its eviction, renames keep the pin (it is stored by inode), and `/api/pins` and
  `tailraid cache ls` report how much of each pin has arrived (docs/01 §Local chunk cache, I-162).
  A pin is "Keep on this device" in the folder menu, for the device whose URL is open.
- **Snapshots and versions** (docs/16). The toolbar's *as-of* menu lists the snapshots by
  mirror, newest first, "Now" at the top; an offline mirror's snapshots are listed greyed
  with "offline: attach it to browse". Choosing one puts `at=<id>` on the route and on
  every read, and the listing becomes read-only: no select boxes, no upload, no folder
  menu, no dirty or deleted rows; the breadcrumb ends in the snapshot's badge, and a
  banner says "As of `2026-10-12-0900Z` on `pike-m1` · taken 3 h ago · read-only" with
  "Restore folder to…" (the picker, then `restore-tree`, which starts a restore job on
  the mirror's host and answers at once; the toast links the dashboard's Restores block)
  and "Back to now". A file's view
  carries a **Versions** panel: one row per distinct content, the snapshots holding it
  folded into a range ("2026-10-12-0600Z – 0900Z · 4 hourly · pike-m1"), its size and
  modified time, and View (the file view with `at=`), Revert and Save a copy; the live
  content is the `current` row, saying which snapshots hold it or "in no snapshot yet",
  and Revert asks first when the current content is in no snapshot. Offline mirrors add
  one dim line ("12 more snapshots on `atlas-m1`, offline"). The toasts of a revert and a
  copy say how many chunks came back from the mirror. The dashboard's Mirrors section (§7) is where
  snapshots are taken (a manual one is permanent, and the dialog says so); "Take a snapshot" flushes the serving gateway's dirty
  files first so the user's own last writes are in it.

## 5. Tiles and thumbnails

The tile view (`.ftiles`) shows one tile per entry with a thumbnail where one exists:
images, videos, audio files with a picture, and folders of music through their first
track's cover. Thumbnails live in the volume so every device shares them:
`/.tailraid/thumbs/<ino>-<gen>-<version>.jpg`, at most 320 px wide, written through the
same vfs path as an upload (temp beside the target, one `Rename`), so a thumbnail is a
file like any other: replicated, erasure-coded and deduplicated, and kept in every node's
chunk cache by the system pin (01 §Local chunk cache, I-373), so a stored cover is served
from the node's disk when the network is busy. `/.tailraid` is a plain
folder on the mount, left out of this UI's listings and refused as a target of user
operations (400). The key carries `Version`, which bumps on every commit, so a changed
file gets a new thumbnail and the hourly sweep (docs/06 §3) removes any whose
`(ino, gen, version)` no longer names a file (I-160). A thumbnail the browser grabbed
from a video carries the frame recipe that chose it, `<ino>-<gen>-<version>.f2.jpg`: when
the recipe changes, every video's stored frame stops counting as its thumbnail, the
sweep removes it and the page grabs a new one as the tile comes into view (I-216).

- **Made in the background.** A thumbnail the node makes itself (an image, an audio
  file's picture or a cover beside it, a folder's cover resolved by reading its tracks)
  reads the volume, which on a busy cluster took 3–9 s a file (2026-10-16), and the
  browser loads tiles four at a time in order, so a few covers being made held every
  tile behind them. The node now makes each in the background, one job per key however
  many tiles ask, at most `ThumbJobs` (16) at once and still at most two decodes, and a
  request waits for its job at most `ThumbWait` (1.5 s). A job not done by then answers
  `202` with `Retry-After: 2` while it goes on; the browser frees the tile's place in the
  fast lane at once and asks again from a slow lane of its own (two at a time, after the
  `Retry-After`, at most five times, then the generic tile), so a tile whose thumbnail
  exists is never held behind one being made. A finished job's answer (the picture, or
  why there is none) is kept for `ThumbKeep` (1 min) for that retry; the picture is
  stored as before. With 16 jobs running a request starts nothing and answers `202`:
  the retry finds a free place. A job runs to its end, at most 2 min, whether or not
  anyone still waits: the stored thumbnail serves the next view.
- **Images** are made by the node on the first `GET /api/thumb` for the file: decoded
  with Go's own `image/jpeg`, `image/png`, `image/gif` and `golang.org/x/image/webp`,
  scaled with `x/image/draw`, encoded as JPEG at quality 80 and stored. Two decodes run at
  a time and a picture above 64 megapixels is refused (415) rather than decoded: the
  decode cost is bounded by the pixel count, which the header gives before any pixel.
  HEIC has no pure-Go decoder; those tiles stay generic.
- **Videos** have no pure-Go decoder either, so the browser makes the thumbnail with the
  decoder it already uses to play the file: the page loads the file into an off-screen
  `<video preload="auto">` and tries the frames at 50 %, 25 %, 75 %, 10 % and 90 % of its
  duration in that order. After each seek it waits for the frame to be presented
  (`requestVideoFrameCallback`, else `readyState ≥ HAVE_CURRENT_DATA`) before reading it,
  since a frame drawn at `seeked` alone is often still black. It scores the frame on a
  64-px probe: mean luma at least 28 of 255 and luma spread (standard deviation) at least
  14. It keeps the first frame that passes, else the one with the highest
  `spread + mean ÷ 4`, so a fade, a title card or a black opening is never chosen while
  the clip holds a better frame. It draws the chosen frame on a 320-px canvas, encodes a
  JPEG and `PUT`s it with the file's `Version`. This is recipe 2 (recipe 1, the frame at
  `seeked` at half the duration, drew black tiles). The node stores it like an image
  thumbnail and every later tile on every device reads it back. A video the browser
  cannot decode (mkv, avi where no decoder exists) keeps the generic tile. The tile asks
  for the thumbnail with the file's version and recipe in the URL, so a browser that
  cached a recipe-1 frame for a day fetches the new one.
- **Audio** answers its embedded picture (§6), else a `cover.*`/`folder.*` sibling, scaled
  like an image. A folder's own tile uses its first audio entry by name that has a
  thumbnail (`dir.cover`): an embedded picture, or, when the folder holds a `cover.*` or
  `folder.*` image, its first audio entry outright. Without a track's picture, a folder's
  own art is an artwork image (`cover.*`, `folder.*`, then `poster.*`: a movie's or a
  show's poster), else its first other image by name that is not a tracker's advert
  (`www.YTS.MX.jpg`, `YTS.BZ - Official site.jpg`: a name with `www.`, `YTS`, `YIFY`,
  `RARBG`, `EZTV`, `torrent` or `official site`), so a folder of photos has a tile, else
  the first other file by name whose thumbnail is stored (a video whose frame the browser
  grabbed). A folder with no art of its own takes the cover of its first subfolder by name
  that has one, first wins, looking into at most `CoverSubdirs` (16) subfolders a level,
  never into `Subs`, `Subtitles`, `Sample` or `Samples`, and at most `CoverDepth` (4)
  levels down, so an album kept as editions or discs (`Fugazi/CBR Original/…`,
  `Artist/1 Studio/1983 90125/CD1`), an artist's folder of albums, a Movies folder and a
  TV folder of shows and seasons all get a tile (`cover` is then the path below the
  folder, `<subfolder>/…/<entry>`; I-257, I-275). The choice is the same on every node
  (I-165).
  A folder whose only media is video nobody has framed yet is not settled: its `GET
  /api/thumb` answers 404 naming the first such video it saw (`grab`, with its path below
  the folder, inode and version; an mp4, m4v, webm or mov before an mkv or avi, which a
  browser may not decode), its listing does not say `no_cover`, and its tile frames that
  video as a video's own tile would (once per file, version and recipe) and asks again.
  The resolved none waits for at most eight such videos: once any of them has a frame,
  every node resolves the folder again. A track whose tags were read counts like a folder
  it read, so a track whose bytes land after its create (a copy through a mount creates
  the names first) is read again once they do (I-275).
  Finding it reads each candidate's tags, so the folder's tile asks
  `GET /api/thumb?path=<folder>` and the node resolves the cover then and remembers it
  until the folder or a subfolder it looked into changes. The choice is stored in the
  volume beside the thumbnails, as `<ino>-<gen>.cover` under `/.tailraid/thumbs` (the
  chosen entry, or none, and the version of every folder it read), so the first node to
  resolve a folder resolves it for all: every other node, and this one after a restart,
  serves the record with one small read while those folders are unchanged, and resolves
  and stores it again once one changed; the sweep removes the record of a folder that no
  longer exists (I-256); a listing reports `cover`, or
  `no_cover` for a folder resolved to none, only when already known and reads no file
  (measured 2026-10-11: a cold listing of 99 album folders took 5.5 s while it resolved
  every cover in line). The page asks for no thumbnail of a folder marked `no_cover`.
- The page loads thumbnails lazily, in two lanes: the node's thumbnails six at a time
  as tiles come within 200 px of the viewport, and the browser's own video frames one at
  a time in a lane of their own, so a slow video never holds up the tiles behind it. A
  tile that leaves the viewport before its turn waits again, and one a redraw replaced is
  dropped. No frame is attempted for a container browsers do not decode (`avi`, `wmv`,
  `mpg`, `mpeg`, `flv`, `vob`, `rm`, `rmvb`, `divx`, `asf`), and one video's frame is
  given at most 15 s. Never more than one `PUT` per file per version; a tile under an
  nsfw folder draws its thumbnail blurred. (Measured 2026-10-13 on the real volume: the
  node answered every folder thumbnail in 2–100 ms; the waits were the page's queue,
  three slots that a frame grab could hold for 30 s and that tiles scrolled past kept.)

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
time, repeat (off, all, one) and shuffle, opens into the queue, and ends in a close button
that stops the track and empties the queue, as the queue's Clear does, so the bar goes
away. The cover is the track's thumbnail, its own picture or the `cover.*`/`folder.*`
beside it, asked for whatever the tags say (`has_cover` is only the embedded picture)
and fetched as the tiles fetch theirs: a 202 is asked again after its `Retry-After`
(I-374), and the picture, once it came, is also the media session's artwork. A track
whose cover will not load (no picture in it and none beside it: the thumbnail answers
404) leaves the cover's box empty, never a broken image, and the bar does not ask again
until another track plays. Playing a track queues
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

The dashboard's **Mirrors** section (docs/16) sits after Integrity: a Snapshots tile under
Volume (the count, the mirrors and how many are offline), then one card per mirror (name
and host, an online dot, used and free on its disk, the newest snapshot, the sync under
way as a progress bar with the bytes it is adding, `warn` when it is offline past
`stale_after`, under its reserve or whose newest snapshot is incomplete), "Take a snapshot",
then a **Restores** block (`#restores`, present while `status.restores` lists a job), and a
table of every snapshot newest first (name with a `manual` badge, mirror, taken,
state, what it added to the mirror, and Browse, disabled
with the reason for an offline mirror's rows; there is no Delete), 20 rows a page under a
pager (Newer, "21–40 of 57", Older) once there are more than 20; the page is the
browser's own and starts at the newest on every load. The Restores block holds one card
per job of every host, newest first: the destination path, the host and the state as
badges; a bar of bytes with the percentage, the rate and the time left; a bar of files with
how many were already there; the snapshot it reads, who started it and when, and a resume
after a restart; Cancel on an unfinished job for its starter or an admin. Counting shows
an empty bar and the files counted so far; queued names the job ahead; waiting says the
mirror is offline and the job goes on when it is back; a running job that yields to
clients (docs/16 §6) says "yielding to clients" after its rate; retrying says when and why; failed
says why and that a rerun goes on from there; done says what was restored and how many
chunks came back from the mirror, and leaves after 7 days. The section's heading carries the schedule
(`hourly 24 · daily 30 · weekly 8 · monthly 12 · next in 23 m`).

## 8. Build and assets

The lookdev pages are `dashboard.html`, `disks.html`, `files.html`, `audio.html`,
`player.html`, `search.html`, `markdown.html`, `media.html` (and their `-phone` variants),
`stats.html`, `snapshots.html`, `signin.html` (docs/21 §6) and `components.html`; the
soak report's are `soak-*.html`.

The TypeScript lives in `internal/webui/src/`; `make web` runs esbuild (a Go tool pinned in
`tools.mod`, no node or npm anywhere) and writes `internal/webui/dist/app.js`, which is
committed and embedded. `make web-check` rebuilds into a temporary directory and fails when
the committed file is stale; it is part of `make check` and CI. The stylesheet
`src/app.css` and `static/index.html` are hand-written and embedded as they are. esbuild
strips types; a `tsconfig.json` is kept so editors type-check.

## 9. Non-goals

User management (users, groups and grants are the CLI's, docs/21 §9; the browser shows
what the signed-in user may see), mutating admin operations from the browser
(repair, GC, membership, policy), HTTPS of its own (the tailnet encrypts), serving on
loopback, transcoding, saved playlists, a queue shared between tabs or devices,
server-side video decoding, adding or removing a mirror, a diff between two versions or
two snapshots, the Windows Previous Versions tab (docs/15 §8), online metadata
providers for the Media tab (docs/18 §10). Search was a non-goal until docs/18.

## 10. The Media tab (I-212)

A fourth tab, `#/media`, with one section per folder whose effective library kind is set
(docs/18 §9), in the order movies, tv, music, then by path. `GET /api/media?lib=<path>`
lists a library's items: the **structure is live** (folders and files from the FSM, so a
film copied a second ago is listed at once, by its cleaned name) and the **metadata comes
from the catalogue** (docs/18 §8), so an item without an entry yet shows its name and the
generic or browser-made thumbnail and fills in after the next pass. `GET
/api/media?item=<path>` returns one item with its plot, people and, for a show, its seasons
and episodes, or, for an album, its tracks through the tags of §6. Nothing under a deleted
root appears; an item under an nsfw folder carries `nsfw` and its art is blurred until
tapped (I-163). Art comes through the routes that exist: a poster or cover tile is `GET
/api/thumb?path=<poster>` (at most 320 px, generated once and shared, I-160), the detail
page's poster and the background are `GET /files/<fanart>` as stored, and a film or an episode without a sidecar frame shows its browser-made thumbnail,
grabbed and stored by the page exactly as the file browser's tiles do (§5) when none is
stored yet: an item carries its video's `video_ino`, `video_version` and `video_thumb`. A show kept as one folder per season, as release packs are
(`Invasion.2021.S01.WEBRip`, `Invasion.2021.S02.1080p`), is one item: a TV library's child
folders whose names clean to the same title and year (docs/18 §8, compared folded) are
one show, listed once under the first of them by name with that folder's metadata (its
poster, else the first folder's with one), `children` the number of distinct seasons
across them, and `item=` any of the folders answers the whole show, its seasons the union
of every folder's (two copies of one episode are both listed, by path). The grouping uses
the names only, so it is the same before and after a pass. A section lists at most
`MediaPage` (500) items a page from `offset`, sorted by `sort_title` then path, the same on
every node. An item is listed only while its structure says so: a film folder with a video in it, a show folder, an album folder by the rule of §6; an episode file without `SnnEnn`, `nnxnn` or `Episode nn` in its name is not an episode, and one named only by its show and number is titled "Episode N". Below 480 px the season strip and the episode list run under the poster column. "Play" opens the file route for a film or an episode and hands an album to the
now-playing bar (§6); "Open folder" goes to the file browser.

A **ROMs** library (docs/18 §8b, I-279) lists its platforms as cards (4:3, the platform's
display name and its folder name, the game count; the art is the folder's logo or poster, else
its first game's box art, whole inside the card, I-400), and `#/media/item/<platform>` its
games by title, three to four abreast: one card per title, its region, revision and flags
(`proto`, `hack`, `beta`, `dlc`, in the warn colour) as chips under the name, `+n` for
the other variants. The tiles take the shape of the platform's box art, since no one ratio
fits every platform (SNES and N64 boxes are wide, NES and Mega Drive tall, Game Boy and
PlayStation square): the median width-to-height ratio of the art loaded so far sets the
grid's `--box` (3:4 until the first one loads), and a box of another shape sits whole
inside its tile, never cropped. Every poster, cover and box-art tile of the Media tab
fetches its thumbnail as a listing tile does, once near the viewport and asked again
while the node makes it (a 202, I-374); only a miss turns it into the title card. A game's page shows its box art at its own shape, or a
title card, its facts (platform,
year, developer, publisher, players), its region and language chips, the description of
its `gamelist.xml` when there is one, and its files (one row per disc or variant, the
size, the file route as the download). It has no Play and no launch route: launching is
not built, and the server has no route that runs anything.

**Title cards** (I-280). Where art cannot exist (a platform with no art and no game with box art, a game without box art, a
film or show folder whose video no browser has framed and no sidecar names a poster),
the page draws a title card instead of the generic icon: the analysed title (docs/18 §8a)
on the surface colour, the year or the platform under it in mono, an accent rule on top.
It is markup, not an image, so it follows the theme; the title comes from the item's
catalogue entry, or, in the file browser's tiles, from a folder's `card` (`title`, `sub`) in
`/api/ls`, which the node fills from the name alone for every folder under a library
whose cover is not known (a folder of video no browser can frame never settles), reading
no file (I-165); the tile draws it when no art arrives.

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
for system, a sun for light and a moon for dark, with the word beside it from 1200 px;
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

The spacing follows Material 3's rules without its libraries (I-286):

- **One 4 px grid.** The steps are tokens: `--sp-1` 4, `--sp-2` 8, `--sp-3` 12,
  `--sp-4` 16, `--sp-6` 24, `--sp-8` 32. Every padding, margin, gap and offset is a
  step or a multiple of 4 px. The exceptions are 1 and 2 px hairlines (borders, a focus
  offset, the inner step of a dense row), and the end dot of a sparkline, which is
  centred on the line's last point by half its own size.
- **The page margin follows the window.** `--gutter` is 16 px on a phone and 24 px from
  600 px. A container's padding, `--pad`, is 12 px and 16 px at the same widths.
  `main` is at most `--page` (1100 px) wide, and the selection bar and the toasts line
  up with its column.
- **Padding by role:**
  - Containers (card, host card, stat tile, note, banner, chart card) take `--pad`.
  - Rows (menu row, disk row, upload, search hit, picker row, queue row) take 8 px
    vertical and 12 px horizontal.
  - A text button is 16 px either side, a segment or a dense button 12 px.
  - An icon button is square, with a 24 px glyph.
  - The dialog takes 24 px.
- **Inside a section 12 px, between sections 24 px.** A section's `h2` has no margin of
  its own: the section's gap is the space under its rule.
- **Grids.** Cards and tiles sit in `auto-fill` grids whose minimum is clamped to the
  container, so a phone gets one column without a media query (I-290):
  - cards: `minmax(min(320px, 100%), 1fr)`
  - stat tiles: `minmax(min(140px, 100%), 1fr)`
  - file tiles: `minmax(min(196px, 100%), 1fr)`, five to a row at full width
  - posters: `minmax(min(140px, 100%), 1fr)`
- **Shape.** The radius is 6 px; pills are 999 px, and a phone's bottom sheet is 12 px at
  the top. Borders are 1 px `--line`.
- **Elevation.** Only floating surfaces cast a shadow: the toast, a menu, the selection
  bar, the dialog and the hero's poster. They share one token, `--shadow` (Material's
  level 2; stronger on navy). A dialog's backdrop is `--scrim`.
- **Ink on a status colour** (the slot cell's glyph) is `--on-status`, and the ground
  behind a video is `--media-bg`. Both are the same in either theme. No rule outside the
  token blocks names a colour, so every rule follows the theme (I-283). The two dark
  blocks, the one under `prefers-color-scheme` and the one under `data-theme`, hold the
  same values (I-284).
- **No horizontal page scroll at 320 px.** Every grid child has `min-width: 0`, names use
  `overflow-wrap: anywhere`, the file table is `table-layout: fixed`, a segmented button
  scrolls inside itself, and a menu is never wider than the window.

### Layout

**Window classes.** The UI asks the width only through Material's window classes:
compact (< 600 px), medium (600–839), expanded (840–1199) and large (≥ 1200). The
queries are `(max-width: 599px)`, `(min-width: 600px)`, `(max-width: 839px)`,
`(min-width: 840px)`, `(max-width: 1199px)` and `(min-width: 1200px)` (I-287). A class
only hides, stacks or loosens; the unqueried rules are the widest layout that still
fits.

**Targets.** The base styles are touch-sized: buttons, tabs, menu and picker rows,
inputs and summaries are at least 48 px, and file, track and snapshot rows are 48 px
tall. Under `@media (pointer: fine)` a mouse gets the denser sizes: 40 px controls, 44 px
file rows, and 32 px for the small buttons in rows (I-288). A control that hover reveals
(the play button of a row or a tile) is always shown under `@media (hover: none)`
(I-289).

**Layers.** The z-index takes one of four values (I-291):
- 1: a control over its tile
- 3: the bottom bars and the toasts
- 4: the menus and a phone's bottom sheet
- 5: the sticky header

**Type.** Font sizes come from the scale of *Type*: 11, 12, 13, 14, 15, 17, 18, 20 and
22 px, plus 9 and 10 px for chart labels and the warning glyph (I-292).

How each part responds:

| Part | Compact (< 600) | Medium (600–839) | Expanded (≥ 840) |
|---|---|---|---|
| Header | row 1: brand, health pill, theme icon; row 2: the tabs spread across the width; row 3: the search | row 1: brand, pill, theme; row 2: tabs and search | one row: brand, tabs, search (160–320 px), pill, theme; the node's name and the theme's word from 1200 |
| Now-playing bar (`--nowbar-h`) | 120 px, two rows: cover, titles, transport; then seek, repeat and shuffle, queue | 120 px, two rows: cover, titles, transport, modes, queue; then the seek bar | 64 px, one row |
| Queue | sits on top of the bar (`bottom: var(--nowbar-h)`), the artist column hidden | on top of the bar | on top of the bar |
| Selection bar | icon buttons (the label in `aria-label` and `title`) | icon and label | icon and label, aligned to `main` |
| Menus | a bottom sheet over the page and the bars | under the summary, end-aligned after the toolbar's spacer, capped at the window | the same |
| File table | modified column hidden | all columns | all columns |
| Disk rows | the figures drop under the bar | one line per target | one line per target |
| Hero | 96 px poster beside the title and facts; plot, actions, people, seasons and episodes run the full width | 160 px poster column | 160 px poster column |
| Markdown | 32 px gutter, smaller heading markers | 48 px gutter | 48 px gutter |

`main`'s bottom padding and the offsets of the selection bar and the toasts follow
`--nowbar-h`, so nothing is measured in the page's script.

### Icons

The icons are **Material Symbols Outlined** (Apache-2.0, from
`github.com/google/material-design-icons` at a pinned commit; the licence and the list
are in `third_party/material-symbols/`): weight 400, grade 0, the 24 px optical size,
`viewBox="0 -960 960 960"`, drawn with `fill: currentColor`. A glyph is 20 px in text and
rows and 24 px in an icon button (`.icon-btn`). The transport and the playing marker use
the filled variants, as music players do. No text glyph stands in for an icon: `⏮ ⏭ ⏸ ▶
⟳ ⤮ ↗` render as colour emoji on iOS (I-293).

| Use | Symbol |
|---|---|
| folder, file, link, image | `folder`, `draft`, `link`, `image` |
| video file, audio file, album, movie or show without art | `movie`, `music_note`, `album`, `movie` |
| search, theme system/light/dark | `search`, `contrast`, `light_mode`, `dark_mode` |
| previous, play, pause, next (filled) | `skip_previous`, `play_arrow`, `pause`, `skip_next` |
| repeat, repeat one, shuffle, queue, playing row | `repeat`, `repeat_one`, `shuffle`, `queue_music`, `equalizer` |
| remove, open a member's page, next file | `close`, `open_in_new`, `chevron_right` |
| selection: delete, move, rename, download, restore | `delete`, `drive_file_move`, `edit`, `download`, `restore_from_trash` |
| disclosure and menu markers (CSS masks) | `chevron_right` turned down when open, `arrow_drop_down` |

The status glyphs stay text: the slot cell's `!` and `×`, the warning disc's `!` and the
sort marks `▴ ▾`. None of them is drawn as emoji.

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
| Spread | `.spread` > `.spread-axis` + `.spread-plot` > `.spread-host` > `.spread-cols` > `.spread-col` | the Disks block's summary, the dashboard's still frame of the soak report's spread chart: one column per target grouped by host, the host's name running up beside its columns so a group is as wide as its disks, rising above or falling below a midline at the expected share, on a symmetric scale of ±25, ±50 or ±100 % chosen by the worst target; neutral within ±25 % (the spread weighted rendezvous leaves at home; docs/14 §7: moves follow home placement, not this band), `warn` past 25 %, `bad` past 50 %; `dead` hatched, `drain` amber-tinted, `off` dashed; the host-cap note sits under it |
| Diskless host | `.host-row.diskless` + `.host-note` | a host card with no target rows and the note `gateway only · no disks`; its border dimmed |
| Warning icon | `.warn-icon` | the warnings strip's amber `!` disc at 14 px, inline after a name; the title says why (a target on the same physical disk as others: they fail together) |
| Disk rows | `.disks` > `.host-row` > `.disk` | one card per host (name, live dot, voter badge, target count and total budget), one row per target in it: name, a slots bar on one scale for the whole block with the expected-share tick (so a bigger budget reads as a tick further right), toned as the spread (neutral within ±25 %), then the slot counts and used of budget; below 600 px the figures drop under the bar; `bad` dead (hatched track), `warn` draining (the word in amber before the slots to move), `off` closed (dashed outline, marker missing); `self` outlines the host card; a host's archive mirror (16) is a row after its disks: used of the disk, `archive · online`, or `off` (dashed) with `archive · offline since <date>`, and no tick, since it holds no slot; the S3 buckets are a card of their own named `S3`, a row per bucket with its slots against its share and its stored bytes and `bucket/prefix` in place of used of budget |
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
| File tile | `.ftiles` > `a.ftile` > `.thumb` (`img` or the icon, `.ov.br` a file's size, `.ov.bl` the playing marker, the play button in the middle) + `.tname` + `.tmeta` (kind left, modified right) | a media card: the picture edge to edge with an 8 px radius, no frame or padding, one line of title (ellipsis), one line of meta; `auto-fill, minmax(min(196px, 100%), 1fr)` with 24 px between rows and 16 px between columns (`minmax(min(140px, 100%), 1fr)`, 16/12 px below 600 px); the thumb is 16:9, cropped to fill; hover lifts it with `--shadow`; `selected` rings it in the accent; `dir`, `dirty` (amber "· syncing" after the meta), `deleted`, `nsfw` (blurred thumb), `protected` and `pinned` (badges on `--overlay` in the top corner), `playing` (audio); a folder's cover carries `.tover`, its title card's words (a library folder's analysed title and year, else its name) centred in sans over the cover dimmed by `--scrim` |
| NSFW blur | `.thumb.nsfw`, `.viewer.nsfw` (+ an empty `.veil` over the viewer, which takes the tap a video would swallow) | the picture is blurred (`blur(16px)`, scaled 1.1 inside `overflow: hidden`) and nothing more: no text; a tap reveals it, and `.revealed` keeps every nsfw picture clear for the session |
| Album | `section.album` > `.album-head` (`img.cover`, `h1`, `.artist`, `.meta`) + `table.tracks` (`.no`, `.title`, `.dur`) | the playing row is `tr.playing` with the equaliser glyph in `.no`; "Play all" and "Shuffle" are toolbar buttons |
| Now-playing bar | `aside.nowbar` > `.cover` + `.titles` (`.title`, `.sub`) + `.transport` + `.seek` (`input[type=range]` + `.time`) + `.modes` (repeat, shuffle with `aria-pressed`) + `.queue-btn` | fixed bottom, `--nowbar-h` tall (64 px on one row from 840 px, 120 px on two rows below; see *Layout*), hidden until something plays (`hidden`); the transport, the modes and the queue's remove are icon buttons; `expanded` shows the queue panel above it; below 600 px the cover shrinks to 40 px; the selection bar sits above it when both show |
| Queue | `.queue` > `ol` > `li.qrow` (`.no`, `.title`, `.artist`, `.dur`, remove button) | the current item is `li.playing`; items can be removed; "Clear" empties it |
| Range picker | `.toolbar .seg` > `button[aria-pressed]` × 7 | 1h · 6h · 24h · 7d · 30d · 1y · custom; the pressed one is the range in the hash; custom opens the range form on the toolbar's next line |
| Range form | `.inline-form.range` > two `input[type=datetime-local]` + Apply + Cancel | a custom `from` and `to`; Enter applies, Escape closes; an empty or inverted range disables Apply |
| Live pill | `.pill.ok` "live" / `.pill` "paused" | at the toolbar's right: live while the range ends now and the tab is visible (a refresh every 5 s), paused for a custom range or a hidden tab; the title says when the last fetch landed |
| Member menu | `.toolbar details.menu` > `summary` ("Node: pike") + `.menu-body` of `a.menu-row` (`.dot` + name + `.small.dim` state) + `a.menu-link` (↗) | one row per device member with its live dot and state; choosing a row re-renders the node section from this gateway; the ↗ link opens that member's own Stats page at the same range (`http://<tsIP>:<web>/#/stats?…`); a member without a web listener has no link; "all nodes" is the first row |
| Chart card | `.charts` > `.chart-card` > `svg.chart` + `.small.dim` caption | the soak report's chart drawn in the page's tokens: text in `--text` and `--dim`, grid in `--line`, series in `--c0`…`--c7`, status cells in the status colours, the spread bins in the under/even/over tokens; one chart per row at every width; the caption names the unit, the window and the tier; hover shows the chart's `<title>` tooltips |
| Search box | `header.top form.search` > `input[type=search]` + `button` | in the header after the tabs; `/` focuses it, Escape clears it, Enter opens `#/search/<q>`; one focus ring around input and button; below 840 px it shares the tabs' row, and takes a row of its own on a phone |
| Search results | `section.results` > `.toolbar` (the count, an "in this folder" `label.toggle`) + `ol` > `li.hit` > `a.hname` (mono) + `.hpath` (dim) + `.hwhy` (`.badge` `name`, `tags` or `content`) + `.hsnip` (the snippet with `mark`) | `.empty` "no matches for `…`"; a `.note` "index unavailable: names only" when the node has no index; a `.hsnip.loading` row while a snippet is fetched |
| Markdown document | `article.md.md-doc` | a 760 px measure with a gutter hairline on the left and faint `##`, `###` markers in the margin before `h3`–`h6` (the file's `#` is the page's `h2`); headings in mono as the UI's; code on `--surface`; tables with `--line` hairlines; `mark` in the accent tint; "Rendered \| Raw" as a `.seg` in the toolbar |
| Library section | `section.lib` > `details` > `summary` > `h2` (▾/▸, title + `.badge.lib` kind + `.count`) + `.toolbar` (`.seg` title \| year \| added) + `.posters` | one per flagged folder; `movies`, `tv`, `music` set the poster shape; a section folds away on its heading, the choice kept per viewer in `localStorage`, and a folded library is listed only once opened |
| Poster grid | `.posters` > `a.poster` > `.art` (`img`, or the kind glyph; `.ov.br` the runtime or the season count) + `.ptitle` + `.pyear` (year left, a note right) | the file tile's card: `auto-fill, minmax(min(140px, 100%), 1fr)` (120 px under 600 px), 24/16 px gaps; `.art` is 2:3 cropped to fill on `--surface` with an 8 px radius and no frame, square for music; `nsfw` blurs the art; `missing` shows the title card in a dashed outline; loaded lazily like tiles; a film without a poster draws its video's frame, and a show without one its first regular episode's (the lowest season above the specials; `video` in `/api/media`) |
| Hero | `section.hero` > `img.backdrop` + `.hero-body` (`img.poster`, `h1`, `.facts`, `.plot`, `.actions`, `.people`) | the fanart as a full-width background 32 vh tall, `object-fit: cover`, darkened to 55 % and faded into `--bg` with a gradient so the ink stays legible in both themes; the poster overlaps its bottom edge; `.facts` year · runtime · rating · genres · mpaa in `--dim`; `.plot` sans 15/1.55 on a 65-ch measure; `.actions` "Play" and "Open folder" buttons; `.people` director and up to six actors as text chips; `.hero.no-art` is a plain `--surface` band, `.hero.poster-only` uses the poster's blurred copy at low opacity |
| Season strip | `.seasons` > `button[aria-pressed]` per season | the selected season's episodes follow; "Specials" last |
| Episode list | `table.episodes` > `tr` > `.no` (`SnnEnn` mono) + `.thumb` (16:9 at 96 px) + `.title` + `.dur` + `.plot.small.dim` (clamped to two lines) | a row opens the player; `playing` on the one in the bar |
| Library badge | `.badge.lib` | the mono word `movies`, `tv` or `music` after the breadcrumb's last segment (effective) and in rows and tiles (own) |
| Library menu row | `.toolbar details.menu` > `label.menu-row.library` > `select` | "Library" with none · movies · tv · music; disabled with the reason on the root, in the trash and while a member reads a codec older than 6 |
| Theme switch | `header.top button.theme` > `svg.icon` + `span` | system (half disc) · light (sun) · dark (moon); the word shows from 1200 px; an icon button below; cycles on tap |
| As-of menu | `.toolbar details.menu.asof` > `summary` (a dim "as of", then "Now" or the snapshot's name) + `.menu-body` of `.menu-group` (the mirror's name, its online dot) and `label.menu-row` (radio + name (+ `.badge` manual) + `.small.dim` when and kind) + `.menu-foot` | floats over the listing (the folder menu's z-order); newest first per mirror after "Now"; an offline mirror's rows `aria-disabled` with "offline: attach it to browse"; the foot counts snapshots and links the dashboard |
| Snapshot banner | `.banner.snapshot` > text + `.actions` (Restore folder to… · Back to now) | above a listing or a file view as of a snapshot: "As of `2026-10-12-0900Z` on `pike-m1` · taken 3 h ago · read-only · 11 items · 2.3 GiB"; accent border and tint (information, not a warning) |
| Details | `details.panel` > `dl.kv.facts` (a `div.full` row for the path) | three columns at the page's width as the Integrity rows, one on a phone; above Versions on a file's page, closed until opened (the choice kept for the session): path, type and MIME, size (the exact bytes in the title), modified, mode, inode and version, chunks with the chunk size and the volume's policy, the folder's flags, and a video's length and resolution or an image's size once the viewer has loaded them |
| Versions list | `details.panel` > `table.versions` > `tr` (`td.asof`, `td.size`, `td.mtime`, `td.acts`) + `.small.dim` offline line | one row per distinct content, newest first; `td.asof` the snapshot range in mono with the count and mirror under it; the live content `tr.current` with a `.badge.strong` "current" and its holders or "in no snapshot yet"; others View · Revert · Save a copy; the table sizes to its content, the actions never wrap; below 600 px the modified column drops |
| Mirror card | `.cards` > `.card.mirror` > `.name-row` (dot, name, host, badges) + bar rows (used of the disk; the sync's progress) + `.meta` | `ok` online, `off` dashed offline with "offline 3 d"; `warn` past `stale_after`, under its reserve, or whose newest snapshot is incomplete (the word says which) |
| Snapshots table | `section` > `h2` + `.h-sub` (the schedule) + `.snap-head` (Take a snapshot · counts) + `table.snapshots` > `tr` (`td.name` (+ `.badge` manual), `td.mirror`, `td.taken`, `td.holds`, `td.acts` Browse; no delete) | newest first; a syncing row carries `.badge` "syncing 41 %", an incomplete one `.badge.warn` "3 missing"; an offline mirror's actions disabled with the reason; below 600 px mirror, taken and holds drop under the name |
| Restore card | `.cards` > `.card.restore` (+ `.warn` queued, waiting or retrying, `.bad` failed, `.done` done or cancelled) > `.name-row` (dot, destination path, `.badges`: on host, state) + `.bar-row` bytes + `.bar-row` files + `.meta` (from mirror · snapshot, by user, started, resumed) + `.card-acts` (Cancel restore) | in the Mirrors section under `h3#restores`, newest first; a done or cancelled job for an hour after it ends, a failed one while its host lists it (docs/16 §6, I-390); counting shows an empty bar; the card's border follows its state (warn, bad, dashed when finished); Cancel sits in the card's bottom right corner, level across a row of cards of different heights |
| Pager | `.pager` > `button` Newer + `.small.dim` "21–40 of 57" + `button` Older | under the Snapshots table past 20 snapshots; Newer disabled on the first page, Older on the last |

### Copy

Sentence case everywhere; no title case, no all-caps. Short labels: "no leader", "last scan
42 s ago", "empty folder", "37 missing". Relative time under 24 h, then `2026-10-08 14:03`.
Binary units with one decimal (1.5 GiB). Counts with thousands separators. Durations as the
server prints them (`3m0s`, `12ms`); track times as `m:ss`. Errors say what and why,
quoting the server: "mkdir photos: already exists (409)". Buttons state the action:
"Upload", "New folder", "Download", "Restore", "Delete now", "Move to…". The trash banner
counts days: "Deleted 3 d ago · gone for good in 27 d". A refusal names the cause:
"protected: unprotect Concerts 2024 to delete", "pike-m1 is offline: attach it to
browse". A snapshot is named as the server names it, in mono (`2026-10-12-0900Z`, or the
user's own name), with its mirror after it (`on pike-m1`), never reformatted; "as of"
introduces it everywhere; a revert's or a copy's toast says what came back from the mirror ("3
chunks copied back from pike-m1"), a tree restore's toast says where it runs and links the
dashboard ("restoring Concerts 2024 from 2026-10-12-0900Z on pike · follow it on the
dashboard"). Tooltips on the health pill quote the
degraded-operation table of docs/00.

### Charts

Bars are the only chart. A bar is ≤ 12 px thick on a `--line` track, square at the baseline,
rounded at the data end, 2 px of surface between adjacent bars. Fill is neutral ink unless a
threshold moves it to amber or red, so colour on a bar is always a status, never decoration.
The expected-share tick is 2 px of ink crossing the track. The sparkline carries no axis; its
caption says what it is and over what window. Every bar has a text value; nothing is
colour-alone. No dual axes, no stacked bars, no pie.

**The Stats page adds the soak report's charts** (docs/09 §6, `internal/chart`): lines
over a shared time axis with dashed event markers (their labels in a lane under the axis, below), liveness lanes, the disk-spread lanes
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

**Event labels** (I-297). A marker is a dashed line over the plot; its label never
covers the plot. The labels sit in a lane of three rows under the time axis, in 10 px
text, always reserved so a chart is as tall with events as without (the Stats page draws
its frame at `chart.LineHeight` before the events arrive). A label starts at its marker,
or slides right by at most 64 units (of the chart's 760) to clear the label before it in
its row, with a thin leader from the axis to it. A label that fits in no row joins the one
before it as "repair pass end +4", and its tooltip names every event it carries. Exact
placement gives way to legible text: the dashed line keeps the event's true time.

## Manual checks (real tailnet, before a release)

0. Sign-in (docs/21 §6): a plain visit shows `/login` with the command; `tailraid
   web login` prints a link that signs the browser in and the header shows the account
   chip with the name; a second open of the same link is refused with the one line; a
   folder the user holds nothing on is absent from the listing; `web logout` returns the
   next request to `/login`; the lookdev `signin.html` matches in light and dark.
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
14. Pin a folder on the laptop, wait for `tailraid cache ls` to show it complete, stop every
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
18. Search from a phone: type three letters of an artist, the hits appear within a
    keystroke; a renamed file is found under its new name at once; a trashed file is gone
    from the hits; a content hit shows its snippet as it scrolls into view; "in this
    folder" narrows to the open folder; a word with an accent finds its unaccented spelling.
19. A README with a table, a task list, an image beside it and a link to another `.md`
    renders and the link navigates inside the UI; a file containing `<script>` renders as
    text; "Raw" shows the source and the choice survives a reload.
20. Flag `/music` as music and a `movies` folder holding one Kodi-style film (`poster.jpg`,
    `fanart.jpg`, `movie.nfo`) and one bare `.mkv`: after `tailraid search index` the Media
    tab shows the album covers, the film with its poster, plot and background, and the bare
    file by its cleaned name with the browser's frame; "Play" plays; an nsfw library is
    blurred; the phone layout has no horizontal scroll and the hero keeps the title legible
    in light and dark.
21. A show folder with `Season 01` and `Season 02` and `tvshow.nfo`: the season strip, the
    episode list with numbers, titles and plots, the episode's frame after it played once.
22. Mirrors: plug a USB disk into the laptop, `tailraid mirror add` it, and watch the
    dashboard's card sync the first snapshot; overwrite a file on the mount, take a
    snapshot, open the file in the browser and see two versions; View the old one, Save a
    copy, Revert, and read the reverted bytes on the mount; `ls ~/TailRAID/.snapshots/`
    shows the mirror and its snapshots, and `touch` there says "Read-only file system".
23. Unplug the disk: the card goes offline, the as-of menu greys its snapshots, the file
    view says how many versions are on it; plug it back: a catch-up snapshot starts at
    once. Move the disk to the desktop and `mirror add` it there: same name, same
    history.
24. No page offers to delete a snapshot; lower `keep.hourly` on the leader and watch the mirror's used
    space fall as the next tick thins it; on a machine with no cluster, `tailraid mirror ls <path>` and
    `mirror export` read the disk.
25. Resize sweep: every page (dashboard, files in list and tiles with a selection, an
    album with the bar playing and the queue open, Media, a hero, Stats, a markdown file)
    at 320, 360, 599, 600, 839, 840, 1100 and 1440 px, in light and dark. No horizontal
    scroll; the header, the bar and the selection bar take the layout of the Layout
    table; an open menu stays inside the window (a bottom sheet on a phone).
26. On an iPhone: the transport, the play buttons and the playing row show Material
    icons, never colour emoji; a row's and a tile's Play shows without hover; every
    control is easy to hit.
27. Restore jobs: from a snapshot on another device's mirror, Restore folder to… a folder
    of a few gigabytes; the toast links the dashboard, where the card counts, then runs
    with its rate and time left. Refresh the page and close the tab: the card goes on.
    Restart the mirror's host (`tailraid stop && tailraid start` there): the card says it
    resumed, and the restored files match the snapshot's (`sha256`). Unplug the mirror:
    the card waits; plug it back: it goes on. Start a second restore: it queues behind the
    first. Cancel it: no `.tailraid-restore-` file is left. Past 20 snapshots, the table
    pages.

## Invariants

- **I-297** — A line chart draws no event label over its plot: the labels sit in a
  three-row lane under the time axis, reserved whatever the events; in the lane no two
  overlap or leave the chart, a label starts at most 64 units right of its marker (or
  is pulled back over it at the right edge), and every event is named once, as a label
  or in the tooltip of the "+N" label before it.
- **I-283** — No rule of `app.css` outside its token blocks names a colour (hex, a colour
  function or a colour keyword); a token block sets only custom properties and
  `color-scheme`. The one literal is the hero's alpha mask.
- **I-284** — The dark token block under `prefers-color-scheme: dark` and the one under
  `data-theme="dark"` set the same tokens to the same values; every colour token of the
  light block has a dark value unless it is listed as the same in both (`--ok`, `--warn`,
  `--bad`, `--on-status`, `--media-bg`, `--scrim`, `--overlay`).
- **I-285** — Every `var(--x)` in `app.css`, the lookdev and the TypeScript names a defined
  custom property, and every custom property `app.css` defines is used.
- **I-286** — Every padding, margin, gap and offset in `app.css` and the lookdev's own
  styles is 0, a 1–2 px hairline or a multiple of 4 px, except the listed geometry; the
  spacing tokens are 4 px steps.
- **I-287** — Every width query in `app.css` and the lookdev is one of the window classes
  of *Layout*.
- **I-288** — A control's `min-height` under 48 px appears only inside
  `@media (pointer: fine)`.
- **I-289** — Every selector hidden with `visibility: hidden` until hover is made visible
  under `@media (hover: none)`.
- **I-290** — Every `auto-fill` or `auto-fit` grid clamps its minimum with
  `min(…, 100%)`.
- **I-291** — Every z-index is a layer of *Layout* (1, 3, 4, 5).
- **I-292** — Every font size in `app.css` is on the scale of *Layout*.
- **I-293** — Every icon is a Material Symbols Outlined path under the `0 -960 960 960`
  viewBox, and no emoji-prone glyph (`⏮ ⏭ ⏸ ▶ ⟳ ⤮ ↗` and the emoji planes) appears in the
  TypeScript, `index.html` or the lookdev.
- **I-68** — Every request to the web listener is authorized from the TCP source address
  alone, before any request body is read: the source is a cluster member whose WhoIs
  StableID matches its member record (I-18), or a tailnet device whose WhoIs login name
  equals this node's own login name; an empty login (unresolved own identity, or a tagged
  device on either side) never matches; a WhoIs failure is 503 and every other refusal 403.
  Once users exist (docs/21, I-354) a session cookie is required on top of it and an
  admitted source alone reaches the sign-in page only.
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
- **I-404** — A folder dropped onto a listing uploads each file it holds, at any depth, to
  its path below the folder dropped on, making the folders on the way; the desktop's
  bookkeeping files are not uploaded.
- **I-226** — Uploads of one name through two gateways never mix: each gateway writes its
  own temp (`.<name>.<node>.tailraid-upload`), one gateway refuses a second concurrent
  upload of a name with 409, and the publish is one `Rename`, so the name ends up holding
  exactly one upload's bytes, the last to publish; with `If-None-Match: *` the publish is
  a `Rename` with `NoReplace` (codec 9, docs/02), so of two exclusive uploads through two
  gateways one gets 2xx and the other 412, and its temp is removed; `<node>` is the
  gateway's node ID, since two devices may share a host name; while a member reads a
  codec before 9 the gateway looks the name up itself, which only covers uploads through
  that gateway.
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
- **I-315** — The index page's CSP admits every source the TypeScript gives an image:
  `blob:` for a thumbnail or folder cover fetched and shown through an object URL, `data:`
  for an inline picture; a test holds the two together (in v0.8.1 every thumbnail went
  through an object URL under an `img-src` without `blob:`, and the browser drew none).
- **I-158** — The web UI lists a folder's deleted roots in that folder with their
  `deleted_at` and `purge_at`, after the live entries, when show deleted is ticked; `ls`, `stat`, `files`, `thumb` and
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
  (with the frame recipe, `….f<recipe>.jpg`, for a frame the browser grabbed, I-216)
  and served only from there; the node generates one itself only for an image it can
  decode within 64 megapixels, two at a time; a `PUT` is accepted only as JPEG of at most
  256 KiB for the file's current `Version`, else 415 or 409; `/.tailraid` is absent from
  every listing and refused as the target of every user operation with 400; and the
  sweep removes a thumbnail whose `(ino, gen, version)` no longer names a file.
- **I-216** — A thumbnail `PUT` for a file that is neither an image nor audio is stored
  as `<ino>-<gen>-<version>.f<FrameRecipe>.jpg`, and for such a file only that key is
  listed (`thumb`) and served. The hourly sweep removes the plain key of such a file and
  any key of another recipe. The page asks for a video's thumbnail with its version and
  recipe in the URL, and for each file and version it grabs at most once per recipe and
  session.
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
  its own art (I-257), else `<subfolder>/<cover>` for the first of at most
  `CoverSubdirs` subfolders by name whose cover resolves, at most `CoverDepth` levels
  down, the same on every node, and absent when none has one; a listing reports
  it, or `no_cover` for a folder resolved to none, only once this node has resolved it
  for the current versions of the folder and of every subfolder it looked into, and
  reads no file to do so; `GET /api/thumb` of the folder resolves it, serves that
  picture and answers 404 for a folder without one.
- **I-257** — A folder's own art is the first audio entry by name that has a thumbnail
  (I-165), else the first of `cover.*`, `folder.*` and `poster.*` it holds, else the
  first other file by name whose thumbnail is stored; a folder without its own takes the
  cover of its first subfolder by name that has one, first wins, looking into at most
  `CoverSubdirs` subfolders a level, skipping hidden folders, and at most `CoverDepth`
  levels down (4 since I-275). Before, only audio counted and only one level down, so a
  Movies or TV folder and a Music folder of artists had no tile.
- **I-275** — A folder's own art includes its first image by name that is not a tracker's
  advert; `Subs`, `Subtitles`, `Sample` and `Samples` folders are not looked into;
  `CoverDepth` is 4. A folder resolved to none while a video below it had no frame is not
  settled: `GET /api/thumb` answers 404 with `grab` naming one such video (path below the
  folder, inode, version; mp4, m4v, webm and mov first), the listing does not report
  `no_cover`, the tile frames that video and asks again, and once any of the at most eight
  videos the none waits for has a stored frame every node resolves the folder again, from
  memory or from its record. A track whose tags were read is a dependency like a folder,
  so its bytes written after the resolve make it resolve again. A none record written
  before it (no `v`, so no waits) is resolved again; a stored choice of a file is still
  served. Before, only `cover.*`, `folder.*` and `poster.*` images counted, depth stopped
  at three, and a none was stored for every node with only folder versions as its
  dependencies, so a frame stored later or a track's bytes written after its create never
  cleared it: 12 of 14 TV shows of a real volume stayed `no_cover`.
- **I-256** — A folder's resolved cover is stored in the volume as a record beside the
  thumbnails (the chosen entry or none, and the version of every folder it read); a node
  without the cover in memory serves the record when those folders are unchanged, reading
  no track's tags, and otherwise resolves the cover and stores the new record; the
  thumbnail sweep keeps a record while its folder exists, live or in the trash, and
  removes it otherwise. Before, the choice lived in each node's memory only, so every
  node, after every restart, read the tags of every folder's tracks again.
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
- **I-209** — `GET /api/render` renders only `.md` and `.markdown` files of at most
  `TEXT_MAX` bytes (400 and 413 otherwise) with the node's escape-first renderer: the
  source is HTML-escaped before any construct is parsed, every tag in the output is one the
  renderer emits and every attribute value is one it built, so no markup of the file
  reaches the page; a link keeps only an `http`, `https`, `mailto` or `#` target, a relative
  `.md` link becomes the file route, a relative link to anything else `/files/`, an image
  only a relative volume path; the fragment's bytes are a pure function of the source.
- **I-279** — Under a `roms` library `/api/media` lists platforms, a platform's games
  (one per title, variants carried with it) and a game's fields, discs and files, the
  same on every node; the only routes a game links to are the file route and the file
  browser; no route launches, emulates or reads a ROM.
- **I-280** — A tile or a poster whose art does not exist is a title card: the analysed
  title and year or platform as markup, never an image the node makes or stores; `/api/ls`
  carries `card` (`title`, `sub`) for every folder under a library, from its name alone;
  the tile draws it when no art arrives and lays it over the folder's cover when art does.
- **I-212** — `/api/media` lists a flagged folder's items from the live FSM with the
  catalogue's metadata where an entry exists and the item's name otherwise, a TV
  library's folders whose names clean to the same title and year as one show with the
  union of their seasons, never an
  entry under a deleted root or `/.tailraid`, carries `nsfw` for an item under an nsfw
  folder, pages at most `MediaPage` items sorted by `sort_title` then path, answers 404
  for a folder that is not a library and 400 for `offset` or `sort` outside their bounds,
  and two nodes with the same chain and FSM return byte-identical bodies.
- **I-200** — The theme switch cycles system, light and dark, sets `data-theme` on the
  document (absent for system), keeps the choice in the browser's `localStorage` only and
  applies it before the first view renders; a storage failure means system; the server
  never sees the choice.
- **I-369** — A tree restore from the browser answers `202` once its job is recorded on the
  mirror's host and the page shows a toast linking `#restores`; the dashboard lists every
  job of `status.restores` with its state, its bytes and files done of the totals, and
  Cancel on an unfinished job for its starter or an admin, whose cancel the job's host
  answers and every other user's is `403`; and the Snapshots table shows at most 20 rows
  a page, newest first, with Newer and Older.
- **I-374** — `GET /api/thumb` answers a stored thumbnail at once; one the node must make
  is made by one background job per key, at most `ThumbJobs` at once, and the request
  waits for it at most `ThumbWait`, answering `202` with `Retry-After` while it runs (or
  while every job place is taken, starting nothing); a job runs to its end whether the
  request waits or not and its result answers the next request (the stored picture, or
  for `ThumbKeep` the reason there is none); the page loads a `202` tile again from a
  lane of its own, so it holds no place in the lane of thumbnails that exist.
- **I-187** — The web UI lists every snapshot of every mirror with its state, online or
  not, reads as of a snapshot on an online mirror with `at=` and refuses a mutation given
  `at` with 400, lists a file's distinct contents across the online mirrors with the
  snapshots holding each and counts the offline mirrors' snapshots, takes
  snapshots, and reverts, copies and restores trees through the endpoints of §3 with the
  bytes of the snapshot and the count of chunks copied back from the mirror; an offline
  mirror's snapshot is answered 409 `offline`, never with stale or partial bytes; no
  route removes a snapshot; and the page flushes the serving gateway's dirty files
  before a snapshot it takes.

## Tests

| Invariant | Tests |
|---|---|
| I-68 | `TestWebUI_Authorize_MemberOrSameUser`, `TestWebUI_Authorize_BeforeBody`, `TestWhoIs_User_CachesLogin`, `TestWhoIs_Login_TaggedHasNone` |
| I-69 | `TestWebUI_Get_RangeBoundaries`, `TestWebUI_Get_ETagAndHead`, `TestWebUI_Get_DirtyOverlayReadYourWrites`, `TestWebUI_Get_ContentTypes`, `TestVFS_StatReportsDirty` |
| I-70 | `TestWebUI_Put_NewAndOverwrite`, `TestWebUI_Put_SizeBoundaries`, `TestWebUI_Put_WaitsForWALCap`, `TestWebUI_Mkdir`, `TestWebUI_Mutations_StaleIs503`, `TestVFS_CapErrorsKeepSentinels` |
| I-404 | `TestWebUI_Put_MakesParents` |
| I-71 | `TestWebUI_CSRFHeaderRequired`, `TestWebUI_Get_ContentTypes` |
| I-72 | `TestWebUI_Paths` |
| I-73 | `TestWebUI_New_RequiresAuthorizer`, `TestWebUI_StatusCached`, `TestWebUI_StaticAssets`, `TestConfig_ListenWeb` |
| I-226 | `TestFSM_Rename_NoReplace_Exist`, `TestWebUI_Put_TwoGateways_OneName_OneWhole`, `TestWebUI_Put_TwoGateways_IfNoneMatch_One412`, `TestSoak_MultiUserAgainstHarness` |
| I-81 | `TestWebUI_Put_AtomicOverwrite_DisconnectKeepsOld`, `TestWebUI_Put_ShortBody_400_NoTemp`, `TestWebUI_Put_Parts_Table`, `TestWebUI_Put_Resume_AfterFailedPart`, `TestWebUI_SweepUploads_AgeTable`, `TestWebUI_SweepUploads_AsTheUploader` |
| I-82 | `TestWebUI_Put_NoContentLength_411`, `TestWebUI_Put_FreeSpace_507`, `TestWebUI_Put_Concurrency_503`, `TestWebUI_Put_BodyStall_DropsTemp` |
| I-315 | `TestStatic_CSPAdmitsEveryImageSource` |
| I-127 | `TestWebUI_HostCheckFramingAndToken` |
| I-143 | `TestBrowserCommand_PerOS`, `TestCLI_Dashboard` |
| I-140 | `TestWebUI_Ls_HidesOSMetadata` |
| I-158 | `TestWebUI_Delete_LandsInTrash_ListedInPlace`, `TestWebUI_TrashAddressing_LsStatGet`, `TestWebUI_Restore_TakenName_409_ThenAs`, `TestWebUI_Purge_FreesRefs`, `TestWebUI_Put_OverwriteKeepsPreviousInTrash`, `TestReport_TrashCacheChart`, `TestSoak_AllPhasesAgainstHarness` |
| I-159 | `TestWebUI_Rename_Table`, `TestWebUI_Move_PerItemResults`, `TestWebUI_Batch_SizeBoundaries`, `TestWebUI_BodyCaps_AllMutatingRoutes`, `TestSoak_AllPhasesAgainstHarness` |
| I-160 | `TestWebUI_Thumb_Image_Generated_Cached_Hidden`, `TestWebUI_Thumb_Put_Boundaries`, `TestWebUI_Thumb_SweptWhenFileChanges`, `TestSoak_AllPhasesAgainstHarness` |
| I-216 | `TestWebUI_Thumb_FrameRecipe_ReplacesOldVideoThumbs` |
| I-161 | `TestDescribeChunksPage_Concurrent`, `TestWebUI_Chunks_HolesHaveSlots`, `TestSoak_AllPhasesAgainstHarness` |
| I-162 | `TestCache_PinnedNeverEvicted`, `TestPins_PrefetchFillsThenReadsOffline`, `TestPins_RenameKeepsPin`, `TestPins_UnpinReleases`, `TestPins_SurviveRestart`, `TestPins_NewFileInPinnedDirIsFetched`, `TestConfig_CacheSize`, `TestLowDiskWarning_Boundary`, `TestReport_TrashAndCacheFields`, `TestCLI_Cache_PinUnpinLs`, `TestWebUI_Pins`, `TestReport_TrashCacheChart`, `TestSoak_AllPhasesAgainstHarness` |
| I-163 | `TestWebUI_Flags_DirsOnly_Effective`, `TestWebUI_Mutations_ProtectedIs403`, `TestSoak_AllPhasesAgainstHarness` |
| I-164 | `TestWebUI_Tags_Table`, `TestWebUI_Tags_ReadBounded`, `TestSoak_AllPhasesAgainstHarness` |
| I-165 | `TestWebUI_Thumb_AudioCoverFallbackOrder`, `TestWebUI_Ls_DirCoverDeterministic`, `TestWebUI_Ls_DirCoverFromSubfolder`, `TestWebUI_Ls_ReadsNoFile`, `TestSoak_AllPhasesAgainstHarness` |
| I-257 | `TestWebUI_DirCover_SurfacesArtUpward`, `TestWebUI_Ls_DirCoverFromSubfolder` |
| I-256 | `TestWebUI_DirCover_StoredAndShared`, `TestSweepThumbs_CoverRecords` |
| I-275 | `TestWebUI_DirCover_PhotosAndAdverts`, `TestWebUI_DirCover_NoneClears`, `TestWebUI_DirCover_LegacyNoneResolvedAgain` |
| I-197 | `TestChart_ReportGolden_ByteIdenticalAfterMove`, `TestChart_NoInlineStyle_NoColour`, `TestChart_ClassesDefinedInAppCSS`, `TestReport_DefinesChartClasses`, `TestChart_EscapesHostileNames`, `TestChart_Points_Table`, `TestChart_LegendWrap_Table`, `TestChart_DropsNaNInf`, `TestChart_Spread_DeadAndDraining`, `TestChart_ClockAxis`, `TestChart_TruncateMarkerLabel`, `TestExpectedShares_WaterFill`, `TestChart_LookdevDataSetMatchesStatsPage` |
| I-198 | `TestWebUI_Stats_Route`, `TestStatic_StatsShellAndTheme`; the hash, the 5 s refresh and the member links are manual checks 15 and 16 (the TypeScript has no unit runner) |
| I-200 | `TestStatic_StatsShellAndTheme`; the cycle, the stored choice and the flash are manual check 17 |
| I-209 | `TestMarkdown_Table`, `TestMarkdown_LinksAndImages_Policy`, `TestMarkdown_RawHTMLIsText`, `FuzzMarkdown`, `TestWebUI_Render_Table`, `TestSoak_AllPhasesAgainstHarness` (the Rendered/Raw switch is manual check 19) |
| I-187 | `TestWebUI_Snapshots_ListTake_NoDeleteRoute`, `TestWebUI_LsAt_ReadOnly`, `TestWebUI_OfflineMirror_409`, `TestWebUI_Versions_DistinctContents`, `TestWebUI_Revert_Copy_RestoreTree`, `TestSoak_AllPhasesAgainstHarness` |
| I-374 | `TestWebUI_Thumb_LaterWhileMade`, `TestWebUI_Thumb_OneJobPerKey`, `TestWebUI_Thumb_FullJobsStartNothing`, `TestWebUI_FolderCover_LaterWhileMade` |
| I-369 | `TestWebUI_RestoreTree_JobAndCancel`, `TestWebUI_Revert_Copy_RestoreTree`, `TestWebUI_AdminRoutes_ByRole`, `TestStatic_ClassesAgreeAcrossCSSLookdevAndTypeScript` |
| I-279 | `TestWebUI_Media_Roms_Table`, `TestWebUI_Media_NoLaunchRoute`, `TestSoak_AllPhasesAgainstHarness` |
| I-280 | `TestWebUI_Ls_TitleCardFields`, `TestStatic_ClassesAgreeAcrossCSSLookdevAndTypeScript` |
| I-212 | `TestWebUI_Media_Library_Table`, `TestWebUI_Media_VideoFrameFields`, `TestWebUI_Media_ShowStill`, `TestWebUI_Media_ShowSplitAcrossFolders`, `TestWebUI_Media_Item_ShowSeasonsEpisodes`, `TestWebUI_Media_TrashedAndReservedHidden`, `TestWebUI_Media_NSFWFlagCarried`, `TestWebUI_Media_DeterministicAcrossNodes`, `TestSoak_AllPhasesAgainstHarness` (the hero and the grids are manual checks 20 and 21) |
| I-297 | `TestChart_MarkLabels_Lane_Table`, `TestChart_Line_MarkLabelsBelowAxis_HeightFixed` |
| I-283 | `TestCSS_ColoursOnlyInTokens`, `TestCSS_HasColour_Table` |
| I-284 | `TestCSS_ThemesDefineTheSameTokens` |
| I-285 | `TestCSS_VarsDefinedAndUsed` |
| I-286 | `TestCSS_SpacingOnGrid`, `TestCSS_OnGrid_Table` |
| I-287 | `TestCSS_BreakpointsAreWindowClasses` |
| I-288 | `TestCSS_DenseTargetsOnlyForFinePointers`, `TestCSS_Interactive_Table` (manual check 26) |
| I-289 | `TestCSS_HoverRevealHasTouchFallback` |
| I-290 | `TestCSS_AutoFillMinimumsClamp` (manual check 25) |
| I-291 | `TestCSS_ZLayers` |
| I-292 | `TestCSS_FontScale` |
| I-293 | `TestIcons_MaterialSymbolsOnly`, `TestIcons_EmojiProne_Table` (manual check 26) |

`TestWebUI_Ls_Stat`, `TestWebUI_Ls_MarksUploads` and `TestWebUI_Chunks` cover the listing and paging shapes of §3.
