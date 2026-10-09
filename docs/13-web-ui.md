# 13 — Web UI: dashboard and file browser

The web UI is a second gateway next to the NFS gateway (docs/07): one `http.Handler` over
the same vfs and FSM, on its own tailnet listener `<tsIP>:20482`. It shows the cluster the
way `tailraid status --json` reports it (data balance across nodes, chunk integrity, the
repairer's state) and lets a browser on any of the user's tailnet devices browse the volume,
stream media, upload files and make folders. It is read-only for cluster administration:
every mutating admin operation stays in the CLI.

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
- **File browser**: list, stat, stream (HTTP Range), download, upload (`PUT`) and make
  folders. No rename, no delete: those stay on the mount.
- **Media**: audio, video and images play in the browser's own elements; the node serves
  files as stored, with no transcoding.
- A single-page TypeScript app, no framework, bundled by esbuild into one committed file and
  embedded in the binary. Hash routing (`#/`, `#/browse/<path>`, `#/file/<path>`).
- A separate listener, not a route on the RPC port: the caller set differs (same-user
  devices that are not members), a page load cannot send the `X-Tailraid-Cluster` header,
  and the RPC port's member-only rule must stay exactly as docs/08 states it.

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

## 3. Endpoints

| Method and pattern | CSRF header | Response |
|---|---|---|
| `GET /` | – | `index.html`; `text/html; charset=utf-8`; `Cache-Control: no-cache`; CSP `default-src 'self'; img-src 'self' data:; media-src 'self'; frame-src 'self'` |
| `GET /assets/{file}` | – | `app.js` (`text/javascript`), `app.css` (`text/css`); `no-cache`; anything else 404 (no SPA fallback: routing is in the hash) |
| `GET /api/status` | – | the `admin.Report` JSON, cached 2 s; `Cache-Control: no-store` |
| `GET /api/ls?path=` | – | `{"path":"/a","entries":[{name,type,size,mtime,mode,ino,dirty?,target?}]}`, sorted by name; `type` is `file`, `dir` or `symlink`; `mtime` RFC 3339 UTC; `dirty` only on files |
| `GET /api/stat?path=` | – | one entry plus `path` |
| `GET /api/chunks?path=&offset=&limit=` | – | `{"total":N,"chunks":[ChunkReport…]}`; default limit 64, max 256; the event ring is omitted |
| `GET`, `HEAD /files/{path}` | – | the bytes through `http.ServeContent`; `?download=1` adds `Content-Disposition: attachment` |
| `PUT /files/{path}` | required | the whole file (`Content-Length` required, 411 without) or one part (`Content-Range: bytes a-b/total`): the bytes stream into `.<name>.tailraid-upload` beside the target and the last byte renames it over the target; 201 for a new file, 204 for an overwrite, 202 `{"offset"}` for a part that is not the last; a part whose `a` is not the temp's size → 409 `{"offset"}`; `If-None-Match: *` on an existing file → 412; more than four uploads at once → 503 `Retry-After: 2`; a size the volume cannot hold → 507 (§4) |
| `POST /api/mkdir` `{"path"}` | required | 201 `{"path"}`; parents are created; an existing entry → 409 |

- Every error is a non-2xx status with a JSON body `{"error":"…"}`. Mapping by `errors.Is`:
  `ErrNotExist` 404 · `ErrExist` 409 · `EISDIR`, `ENOTDIR`, `EINVAL`, `ENAMETOOLONG` 400 ·
  `EROFS` (stale gateway, docs/02) 503 with `Retry-After: 5` · `ENOSPC` 507 · `EIO` 503 ·
  `admin.ErrAmbiguous` 409 · anything else 500, logged.
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
  `/a/b` form is echoed back. File operations on the root or a directory are 400.

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

## 5. Status cache and chunk drill-down

`admin.Build` walks every chunk (`ScanHealth`). The handler encodes the report once and
serves that body for 2 s, single-flight under a mutex, so several tabs polling at 5 s cost
one walk. `/api/chunks` issues one HEAD per live slot per chunk with a 3 s timeout
(`admin.DescribeChunk`), which is why it pages: 64 chunks per call by default.

## 6. Build and assets

The TypeScript lives in `internal/webui/src/`; `make web` runs esbuild (a Go tool pinned in
`tools.mod`, no node or npm anywhere) and writes `internal/webui/dist/app.js`, which is
committed and embedded. `make web-check` rebuilds into a temporary directory and fails when
the committed file is stale; it is part of `make check` and CI. The stylesheet
`src/app.css` and `static/index.html` are hand-written and embedded as they are. esbuild
strips types; a `tsconfig.json` is kept so editors type-check.

## 7. Non-goals

User management and per-user access control, mutating admin operations from the browser
(repair, GC, membership, policy), HTTPS of its own (the tailnet encrypts), serving on
loopback, transcoding, rename and delete (for now), thumbnails, search.

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
The sheet honours `prefers-color-scheme` and, for mockup viewers that stamp
`data-theme`, that attribute; the app itself has no theme toggle.

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
  in `auto-fill` grids (`minmax(320px, 1fr)` cards, `minmax(140px, 1fr)` tiles) so a phone
  gets one column without a media query. Breakpoints only hide: 480 px drops the mtime
  column and the header's node text.
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
| Node card | `.card.node` | `self` outlined in accent; name row, two bar rows, mono meta footer |
| File row | `table.files tr` | dir / file / symlink icon; `dirty` adds "syncing" in amber |
| Breadcrumb | `.crumbs` | mono, wraps; last segment plain |
| Upload row | `.upload` | queued (dim), uploading (bar), done `ok`, failed `bad` with the server's message |
| Inline form | `.inline-form` | the new-folder input; Enter submits, Escape cancels |
| Toast | `.toast` | `ok` / `bad`, bottom right, 6 s, click to dismiss, `aria-live="polite"` |
| Spread | `.spread` > `.spread-axis` + `.spread-plot` > `.spread-host` > `.spread-cols` > `.spread-col` | the Disks block's summary, the dashboard's still frame of the soak report's spread chart: one column per target grouped under its host, rising above or falling below a midline at the expected share, on a symmetric scale of ±25, ±50 or ±100 % chosen by the worst target; neutral within ±10 %, `warn` past 10 %, `bad` past 25 %; `dead` hatched, `drain` amber-tinted, `off` dashed; the host-cap note sits under it |
| Data-dir tag | `.disk-name .tag` | a dim mono `data dir` after the host's own store in the disk rows, only on a host with added disks |
| Warning icon | `.warn-icon` | the warnings strip's amber `!` disc at 14 px, inline after a name; the title says why (a target on the same physical disk as others: they fail together) |
| Disk rows | `.disks` > `.host-row` > `.disk` | one card per host (name, live dot, voter badge, target count and total budget), one row per target in it: name, a slots bar on one scale for the whole block with the expected-share tick (so a bigger budget reads as a tick further right), then the slot counts and used of budget; at ≤ 480 px the figures drop under the bar; `bad` dead (hatched track), `warn` draining (the word in amber before the slots to move), `off` closed (dashed outline, marker missing); `self` outlines the host card |
| Slot cell | `.cell` | present (green), `warn` `!` live but absent, `bad` `×` dead, `unplaced` dashed, `unverified` 50 % |
| Panel | `details.panel` | collapsed by default; summary names the count |
| Empty / error | `.empty`, `.error` | "empty folder"; the error block quotes the server's `error` string and status |

### Copy

Sentence case everywhere; no title case, no all-caps. Short labels: "no leader", "last scan
42 s ago", "empty folder", "37 missing". Relative time under 24 h, then `2026-10-08 14:03`.
Binary units with one decimal (1.5 GiB). Counts with thousands separators. Durations as the
server prints them (`3m0s`, `12ms`). Errors say what and why, quoting the server: "mkdir
photos: already exists (409)". Buttons state the action: "Upload", "New folder", "Download".
Tooltips on the health pill quote the degraded-operation table of docs/00.

### Charts

Bars are the only chart. A bar is ≤ 12 px thick on a `--line` track, square at the baseline,
rounded at the data end, 2 px of surface between adjacent bars. Fill is neutral ink unless a
threshold moves it to amber or red, so colour on a bar is always a status, never decoration.
The expected-share tick is 2 px of ink crossing the track. The sparkline carries no axis; its
caption says what it is and over what window. Every bar has a text value; nothing is
colour-alone. No dual axes, no stacked bars, no pie.

## Manual checks (real tailnet, before a release)

1. Desktop Chrome, Safari and Firefox: the dashboard in light and dark (toggle the OS
   theme live); the pill through stopping a node (degraded), killing the leader (stale,
   then no leader) and stopping the daemon (unreachable).
2. Follower and leader: the integrity block links the leader from a follower and shows
   the repairer's counters on the leader; "last scan never" on a fresh cluster.
3. Polling pauses while the tab is hidden and resumes with one immediate fetch.
4. Balance: filling a device's disk lowers its budget and expected share within a minute;
   a draining node shows "slots to move".
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

`TestWebUI_Ls_Stat`, `TestWebUI_Ls_MarksUploads` and `TestWebUI_Chunks` cover the listing and paging shapes of §3.
