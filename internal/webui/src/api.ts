// The JSON the node serves (docs/13 §3), typed field for field after the Go structs.

export type Health = "healthy" | "stale" | "no-leader";

export interface VolumeReport {
  k: number;
  m: number;
  auto: boolean;
  chunk_size: number;
  policy_version: number;
  inodes: number;
  chunks: number;
  tombstoned: number;
  logical_bytes: number;
  stored_bytes: number;
  dedup_ratio: number;
  capacity: number;
  usable_capacity: number;
  free: number;
  degraded_chunks: number;
  repair_pending: number;
  unreadable_chunks: number;
  stale_policy_chunks: number;
  domain_skew: number; // chunks above the host cap (docs/14 §7)
  tolerates_disks: number;
  tolerates_hosts: number;
  trash_inodes: number; // deleted entries waiting in the trash (docs/06 §1)
  trash_bytes: number;
}

export interface NodeReport {
  idx: number;
  id: string;
  name: string;
  kind: string; // device | disk | s3
  host?: number; // disks: the device that serves it (docs/14)
  diskless?: boolean; // a device with no disks: a gateway-only host (docs/14 §9)
  shared_with?: string[]; // targets of the same host on the same physical disk
  state: "active" | "draining" | "removed" | string;
  addr: string;
  rpc_port: number;
  voter: boolean;
  leader: boolean;
  self: boolean;
  live: boolean;
  dead_for?: string;
  lost?: boolean;
  rtt?: string;
  web?: number; // the member's browser UI port, absent when off (docs/13 §1)
  free: number; // from the probe; a bucket's is 0, never a measurement (docs/10 §4.1)
  capacity: number;
  stored_bytes: number; // shard bytes of the slots the placement table gives it (I-178)
  version: string;
  applied: number;
  slots: number;
  missing_slots: number;
  dev?: boolean;
  joined_at: string;
  s3?: S3Report; // S3 nodes: the bucket, never its credentials
}

export interface S3Report {
  endpoint: string;
  bucket: string;
  prefix?: string;
  region?: string;
  path_style?: boolean;
}

export interface RepairStats {
  scanned: number;
  repaired: number;
  partial: number;
  unrepairable: number;
  deferred: number;
  migrated: number;
  policy_stale: number;
  scrubbed: number;
  scrub_bad: number;
  bytes_moved: number;
  errors: number;
  homed?: number; // chunks moved home (docs/14 §7)
}

// The home gate (docs/14 §7): what is away from its home placement and whether it moves.
export interface HomeStatus {
  away_slots: number;
  away_bytes: number;
  movable_bytes: number;
  stored_bytes: number;
  start: number; // repair.home_start, a fraction
  open: boolean;
  forced?: boolean;
}

// The pass under way on the leader (docs/12 §8): absent while the repairer is idle.
export interface RepairProgress {
  pass: number;
  started: string;
  scanned: number;
  total: number;
  in_flight: number;
  workers: number;
  bytes_moved: number;
}

export interface RepairStatus {
  last_scan: string;
  stats: RepairStats;
  reported_bad_slots: number;
  in_progress?: RepairProgress | null;
  home?: HomeStatus | null;
}

export interface GatewayReport {
  dirty_inodes: number;
  dirty_bytes: number;
  wal_bytes: number;
  flushing: number;
}

export interface MachineReport {
  disk_free?: number;
  disk_total?: number;
}

/** PinEntry is one pinned folder of this node (docs/13 §4). */
export interface PinEntry {
  path: string;
  bytes: number;
  cached_bytes: number;
}

/** CacheReport is `cache` in the status report and the body of /api/pins. */
export interface CacheReport {
  size: number;
  used: number;
  pinned_bytes: number;
  pins: PinEntry[];
}

/** MeshPeer is this device's path to one member (docs/20 §10, status.mesh.peers). */
export interface MeshPeer {
  idx: number;
  name?: string;
  class: "lan" | "wan" | "relay" | "none" | string;
  endpoint?: string;
  via?: string;
  handshake_age?: string;
  seen_age?: string;
}

/** PortMapStatus is the gateway mapping of the mesh port (docs/20 §7). */
export interface PortMapStatus {
  protocol: string;
  external?: string;
}

/** MeshReport is `status.mesh`: this device and its paths (docs/20 §10). */
export interface MeshReport {
  addr: string;
  interface?: string;
  port: number;
  peers?: MeshPeer[];
  portmap?: PortMapStatus | null;
}

export interface Report {
  node: string;
  cluster_id: string;
  health: Health;
  leader: string;
  last_contact?: string;
  voters: number;
  live_voters: number;
  warnings?: string[];
  volume: VolumeReport;
  nodes: NodeReport[];
  repair?: RepairStatus | null;
  gateway?: GatewayReport | null;
  machine?: MachineReport | null;
  cache?: CacheReport | null;
  web_url?: string;
  search?: SearchStatus | null;
  mesh?: MeshReport | null;
  generated_at: string;
}

/** SearchStatus is `status.search` (docs/12 §8, docs/18 §5). */
export interface SearchStatus {
  docs: number;
  segments: number;
  bytes: number;
  catalog: number;
  pending: number;
  last_pass?: string;
  in_progress?: boolean;
  capped?: boolean;
  index?: string;
}

/** SearchHit is one result of GET /api/search (docs/13 §3). */
export interface SearchHit {
  path: string;
  type: "file" | "dir" | "symlink";
  size: number;
  mtime: string;
  ino: number;
  match: "name" | "tags" | "content";
  title?: string;
  artist?: string;
  album?: string;
  terms: { term: string; off: number }[];
}

export interface SearchResult {
  query: string;
  hits: SearchHit[];
  index?: string;
}

export interface Entry {
  name: string;
  type: "file" | "dir" | "symlink";
  size: number;
  mtime: string;
  mode: number;
  ino: number;
  version: number;
  dirty?: boolean;
  upload?: boolean;
  target?: string;
  path?: string;
  protected?: boolean;
  nsfw?: boolean;
  library?: string;
  deleted_at?: string;
  purge_at?: string;
  thumb?: boolean;
  cover?: string;
  /** a folder resolved to no cover: the page asks for no thumbnail (I-165) */
  no_cover?: boolean;
  /** a folder under a library no art can come from: its title card's words (docs/13 §10, I-280) */
  card?: Card;
}

/** Card is a title card's words: the analysed title and the line under it. */
export interface Card {
  title: string;
  sub?: string;
}

/** MediaFile is one file of a game: a disc or another dump of the title (docs/13 §10). */
export interface MediaFile {
  path: string;
  name: string;
  size: number;
  disc?: number;
  regions?: string[];
  revision?: string;
  flags?: string[];
}

/** DirInfo describes the listed folder: effective flags, pin, place in the trash. */
export interface DirInfo {
  protected: boolean;
  nsfw: boolean;
  library?: string;
  pinned: boolean;
  deleted_at?: string;
  purge_at?: string;
  cover?: string;
  no_cover?: boolean;
  /** the snapshot the listing is read as of (at=, docs/16 §6) */
  snapshot?: SnapRef;
}

/** SnapRef names the snapshot a listing is read as of. */
export interface SnapRef {
  id: number;
  name: string;
  mirror: string;
  at: string;
}

export interface Ls {
  path: string;
  dir: DirInfo;
  entries: Entry[];
}

export interface TrashEntry {
  ino: number;
  name: string;
  type: "file" | "dir" | "symlink";
  size: number;
  path: string;
  deleted_at: string;
  purge_at?: string;
  restorable: boolean;
}

export interface Failure {
  path: string;
  error: string;
  status: number;
}

export interface MoveResult {
  moved: string[];
  failed: Failure[];
}

export interface DeleteResult {
  deleted: string[];
  failed: Failure[];
}

export interface Tags {
  title: string;
  artist: string;
  album: string;
  album_artist: string;
  track: number;
  tracks: number;
  disc: number;
  year: number;
  genre: string;
  has_cover: boolean;
}

export interface SlotReport {
  idx: number;
  node: number;
  name?: string;
  live: boolean;
  present?: boolean | null;
}

export interface ChunkReport {
  hash: string;
  found: boolean;
  state: "committed" | "degraded" | "unreadable" | "tombstoned" | "zero" | "unknown" | string;
  size: number;
  k: number;
  m: number;
  refs: number;
  policy_stale: boolean;
  tombstoned_at?: string;
  live_slots: number;
  slots: SlotReport[];
  files?: string[];
}

export interface ChunksPage {
  total: number;
  chunks: ChunkReport[];
}

export class ApiError extends Error {
  constructor(
    public status: number,
    message: string,
    public body?: Record<string, unknown>,
  ) {
    super(message);
  }
}

/** offline reports whether an error is a snapshot whose mirror is offline (409 offline, docs/13 §3). */
export function offline(e: unknown): boolean {
  return e instanceof ApiError && e.body?.["offline"] === true;
}

/** HEADER is the CSRF header every mutating request carries (docs/13 §3). */
export const HEADER = "X-Tailraid-UI";

export function encodePath(p: string): string {
  return p.split("/").map(encodeURIComponent).join("/");
}

/**
 * Src is where a read looks besides the live volume: a deleted root's inode (`in=`,
 * docs/13 §4), or a snapshot as "@<id>" (`at=`, docs/16 §6). A string keeps two
 * sources comparable with ===, which the player's queue relies on.
 */
export type Src = number | `@${number}` | undefined;

/** srcParam is the query parameter of a source, without its separator. */
export function srcParam(root: Src): string {
  if (root === undefined) return "";
  return typeof root === "string" ? "at=" + root.slice(1) : "in=" + root;
}

/** inQuery is the `in=<ino>` or `at=<id>` parameter of a source. */
function inQuery(root: Src, first = false): string {
  return root === undefined ? "" : (first ? "?" : "&") + srcParam(root);
}

export function fileURL(path: string, download = false, root?: Src): string {
  const q: string[] = [];
  if (download) q.push("download=1");
  if (root !== undefined) q.push(srcParam(root));
  return "/files" + encodePath(path) + (q.length ? "?" + q.join("&") : "");
}

/** thumbURL is the thumbnail of path; tag, when given, keys the browser's cache (a video's version and frame recipe, docs/13 §5). */
export function thumbURL(path: string, root?: Src, tag?: string): string {
  return "/api/thumb?path=" + encodeURIComponent(path) + inQuery(root) + (tag ? "&t=" + encodeURIComponent(tag) : "");
}

async function call<T>(url: string, init?: RequestInit): Promise<T> {
  const r = await fetch(url, init);
  if (!r.ok) throw await apiError(r);
  if (r.status === 204) return undefined as T;
  return (await r.json()) as T;
}

async function apiError(r: Response): Promise<ApiError> {
  try {
    const body = (await r.json()) as Record<string, unknown>;
    if (typeof body.error === "string") return new ApiError(r.status, `${body.error} (${r.status})`, body);
  } catch {
    // not JSON
  }
  return new ApiError(r.status, `${r.statusText || "request failed"} (${r.status})`);
}

function post<T>(route: string, body: unknown): Promise<T> {
  return call(route, { method: "POST", headers: { [HEADER]: "1", "Content-Type": "application/json" }, body: JSON.stringify(body) });
}

export const getStatus = (): Promise<Report> => call("/api/status");
export const ls = (path: string, root?: Src): Promise<Ls> => call("/api/ls?path=" + encodeURIComponent(path) + inQuery(root));
export const stat = (path: string, root?: Src): Promise<Entry> => call("/api/stat?path=" + encodeURIComponent(path) + inQuery(root));
export const trashList = (): Promise<{ entries: TrashEntry[] }> => call("/api/trash");
export const chunks = (path: string, offset: number, limit: number): Promise<ChunksPage> =>
  call(`/api/chunks?path=${encodeURIComponent(path)}&offset=${offset}&limit=${limit}`);
export const tags = (path: string, root?: Src): Promise<Tags> => call("/api/tags?path=" + encodeURIComponent(path) + inQuery(root));
export const getPins = (): Promise<CacheReport> => call("/api/pins");
export const mkdir = (path: string): Promise<{ path: string }> => post("/api/mkdir", { path });
export const rename = (path: string, name: string): Promise<{ path: string }> => post("/api/rename", { path, name });
export const move = (paths: string[], to: string): Promise<MoveResult> => post("/api/move", { paths, to });
export const del = (paths: string[]): Promise<DeleteResult> => post("/api/delete", { paths });
export const restore = (ino: number, opts: { to?: string; name?: string } = {}): Promise<{ path: string }> => post("/api/restore", { ino, ...opts });
export const purge = (sel: { ino: number } | { all: true }): Promise<void> => post("/api/purge", sel);
export const setFlags = (path: string, flags: { protected?: boolean; nsfw?: boolean; library?: string }): Promise<Entry> => post("/api/flags", { path, ...flags });
export const pin = (path: string, pinned: boolean): Promise<{ path: string; pinned: boolean; bytes: number }> => post("/api/pin", { path, pinned });

/** putThumb stores the frame the browser grabbed from a video (docs/13 §5). */
export async function putThumb(path: string, version: number, jpeg: Blob): Promise<void> {
  const r = await fetch(`/api/thumb?path=${encodeURIComponent(path)}&version=${version}`, { method: "PUT", headers: { [HEADER]: "1", "Content-Type": "image/jpeg" }, body: jpeg });
  if (!r.ok) throw await apiError(r);
}

/** MediaItem is a film, show, season, episode or album of the Media tab (docs/13 §3). */
export interface MediaItem {
  ino: number;
  path: string;
  kind: "movie" | "show" | "season" | "episode" | "album" | "platform" | "game";
  title: string;
  sort_title: string;
  year?: number;
  genres?: string[];
  rating?: string;
  runtime?: number;
  mpaa?: string;
  artist?: string;
  poster?: string;
  fanart?: string;
  thumb?: string;
  video?: string;
  /** the video's inode and version and whether its frame is stored: the page grabs a missing one (docs/13 §10) */
  video_ino?: number;
  video_version?: number;
  video_thumb?: boolean;
  size?: number;
  added: string;
  nsfw: boolean;
  children?: number;
  plot?: string;
  people?: string[];
  season?: number;
  episode?: number;
  episode_to?: number;
  aired?: string;
  seasons?: { n: number; title: string; poster?: string; episodes: MediaItem[] }[];
  tracks?: string[];
  catalogued: boolean;
  /** the name analyser's fields (docs/18 §8a) */
  date?: string;
  absolute?: number;
  part?: number;
  edition?: string;
  quality?: string[];
  languages?: string[];
  discs?: number;
  /** a platform's or a game's (docs/13 §10) */
  platform?: string;
  regions?: string[];
  revision?: string;
  flags?: string[];
  developer?: string;
  publisher?: string;
  players?: string;
  variants?: number;
  games?: MediaItem[];
  files?: MediaFile[];
}

export interface MediaLibrary {
  path: string;
  kind: string;
  count: number;
}

/** media lists the libraries, or one library's items (docs/13 §10). */
export function media(): Promise<{ libs: MediaLibrary[] }>;
export function media(lib: string, sort: string, offset?: number): Promise<{ lib: MediaLibrary; items: MediaItem[] }>;
export function media(lib?: string, sort = "title", offset = 0): Promise<unknown> {
  if (lib === undefined) return call("/api/media");
  return call(`/api/media?lib=${encodeURIComponent(lib)}&sort=${sort}&offset=${offset}`);
}

export function mediaItem(path: string): Promise<MediaItem> {
  return call<MediaItem>("/api/media?item=" + encodeURIComponent(path));
}

/** search asks the node's index (docs/18 §6). */
export function search(q: string, under: string): Promise<SearchResult> {
  return call<SearchResult>("/api/search?q=" + encodeURIComponent(q) + (under ? "&under=" + encodeURIComponent(under) : ""));
}

/** rendered fetches a markdown file as the node's HTML fragment (docs/13 §4, I-209). */
/** meshPicture is the Mesh section's SVG from GET /api/mesh, "" on a device without a mesh (docs/13 §7). */
export async function meshPicture(): Promise<string> {
  const r = await fetch("/api/mesh");
  if (r.status === 404) return "";
  if (!r.ok) throw await apiError(r);
  return ((await r.json()) as { svg: string }).svg;
}

export async function rendered(path: string, root?: Src): Promise<string> {
  const r = await fetch("/api/render?path=" + encodeURIComponent(path) + inQuery(root));
  if (!r.ok) throw await apiError(r);
  return r.text();
}

/** text fetches a small file as text (previews). */
export async function text(path: string, root?: Src): Promise<string> {
  const r = await fetch(fileURL(path, false, root));
  if (!r.ok) throw await apiError(r);
  return r.text();
}

export interface Upload {
  promise: Promise<void>;
  abort(): void;
}

/** PART is the size of one upload request for a large file (docs/13 §4). */
export const PART = 32 << 20;
const RETRIES = 5;

interface PartReply {
  status: number;
  error?: string;
  offset?: number;
}

/** sendPart PUTs one slice; it resolves with any HTTP status and rejects only on a network error or an abort. */
function sendPart(path: string, blob: Blob, range: string | null, onProgress: (loaded: number) => void, track: (x: XMLHttpRequest) => void): Promise<PartReply> {
  return new Promise((resolve, reject) => {
    const xhr = new XMLHttpRequest();
    track(xhr);
    xhr.open("PUT", fileURL(path));
    xhr.setRequestHeader(HEADER, "1");
    if (range) xhr.setRequestHeader("Content-Range", range);
    xhr.upload.onprogress = (e) => onProgress(e.loaded);
    xhr.onload = () => {
      const reply: PartReply = { status: xhr.status };
      try {
        const body = JSON.parse(xhr.responseText) as { error?: string; offset?: number };
        reply.error = body.error;
        reply.offset = body.offset;
      } catch {
        // not JSON
      }
      resolve(reply);
    };
    xhr.onerror = () => reject(new ApiError(0, "network error"));
    xhr.onabort = () => reject(new ApiError(0, "cancelled"));
    xhr.send(blob);
  });
}

function failure(r: PartReply): ApiError {
  return new ApiError(r.status, `${r.error ?? "upload failed"} (${r.status})`);
}

function wait(ms: number): Promise<void> {
  return new Promise((resolve) => setTimeout(resolve, ms));
}

/**
 * upload stores a File atomically and resumably (docs/13 §4): a file up to PART goes in
 * one PUT; a larger one in sequential parts with Content-Range, each a request of its
 * own, so a server refusal reaches the browser as a status and a dropped connection
 * resumes from what the server already holds. onNote reports retries ("resuming 2/5").
 */
export function upload(path: string, file: File, onProgress: (sent: number) => void, onNote?: (note: string) => void): Upload {
  let current: XMLHttpRequest | null = null;
  let aborted = false;
  const track = (x: XMLHttpRequest) => {
    current = x;
  };
  const promise = (async () => {
    if (file.size <= PART) {
      const r = await sendPart(path, file, null, onProgress, track);
      if (r.status < 200 || r.status >= 300) throw failure(r);
      return;
    }
    let offset = 0;
    let retries = 0;
    while (offset < file.size) {
      if (aborted) throw new ApiError(0, "cancelled");
      const end = Math.min(offset + PART, file.size);
      const base = offset;
      let r: PartReply;
      try {
        r = await sendPart(path, file.slice(offset, end), `bytes ${offset}-${end - 1}/${file.size}`, (loaded) => onProgress(base + loaded), track);
      } catch (e) {
        if (aborted || (e instanceof ApiError && e.status === 0 && e.message === "cancelled")) throw e;
        r = { status: 0, error: e instanceof Error ? e.message : String(e) };
      }
      if (r.status >= 200 && r.status < 300) {
        offset = end;
        retries = 0;
        onNote?.("");
        continue;
      }
      if (r.status === 409 && typeof r.offset === "number" && r.offset !== offset) {
        offset = r.offset; // the server says where the upload really is
        continue;
      }
      if (r.status === 0 || r.status === 408 || r.status >= 500) {
        if (++retries > RETRIES) throw failure(r);
        onNote?.(`resuming ${retries}/${RETRIES}`);
        await wait(1000 * Math.min(8, 2 ** (retries - 1)));
        // the part is sent again from where this upload was; a server that holds more
        // (the reply was lost after the part landed) answers 409 with its offset, which
        // the loop adopts: the temp is the node's own, its name not the browser's to know
        continue;
      }
      throw failure(r);
    }
  })();
  return {
    promise,
    abort: () => {
      aborted = true;
      current?.abort();
    },
  };
}

// The Stats page (docs/13 §3, docs/17 §3): the charts of a range as server-rendered SVG.
export interface StatsMember {
  name: string;
  addr: string; // the tailnet IP
  live: boolean;
  self: boolean;
  web?: number; // the member's browser UI port, absent when off
}

export interface StatsChart {
  id: string;
  title: string;
  svg: string;
  caption: string;
}

export interface StatsResponse {
  range: { from: number; to: number; live: boolean; tier: string; step: number };
  node: string;
  nodes: StatsMember[];
  charts: StatsChart[];
}

/** a chart before its rows are read: the empty SVG the page draws at once (docs/17 §3) */
export interface StatsFrame {
  id: string;
  title: string;
  frame: string;
}

/** the stream's first line: the range, the members and every chart's frame */
export interface StatsFirst {
  range: StatsResponse["range"];
  node: string;
  nodes: StatsMember[];
  charts: StatsFrame[];
}

/** a later line: one chart drawn, empty (no rows in the range), or the error that ended the stream */
export interface StatsLine {
  id?: string;
  svg?: string;
  caption?: string;
  empty?: boolean;
  error?: string;
}

/**
 * streamStats reads /api/stats?stream=1 (docs/17 §3, I-258): onFirst gets the frames as
 * soon as the node writes them, before it reads any row, and onLine each chart as it
 * arrives. ids limits the charts drawn (a live refresh asks for those on screen).
 */
export async function streamStats(
  from: number,
  to: number | "now",
  node: string,
  ids: string[] | null,
  onFirst: (f: StatsFirst) => void,
  onLine: (l: StatsLine) => void,
): Promise<void> {
  let url = `/api/stats?from=${from}&to=${to}&node=${encodeURIComponent(node)}&stream=1`;
  if (ids) url += "&ids=" + encodeURIComponent(ids.join(","));
  const r = await fetch(url, { cache: "no-store" });
  if (!r.ok || !r.body) throw await apiError(r);
  const reader = r.body.getReader();
  const dec = new TextDecoder();
  let buf = "";
  let first = true;
  for (;;) {
    const { done, value } = await reader.read();
    if (value) buf += dec.decode(value, { stream: true });
    for (let i = buf.indexOf("\n"); i >= 0; i = buf.indexOf("\n")) {
      const line = buf.slice(0, i);
      buf = buf.slice(i + 1);
      if (!line) continue;
      if (first) {
        first = false;
        onFirst(JSON.parse(line) as StatsFirst);
      } else onLine(JSON.parse(line) as StatsLine);
    }
    if (done) return;
  }
}

// --- snapshots on mirror disks (docs/13 §3, docs/16) -------------------------------------

export interface MirrorSync {
  id: number;
  snapshot: string;
  read: number;
  total: number;
  bytes_new: number;
}

export interface Mirror {
  id: number;
  /** its directory under /.snapshots: pike-m1 */
  name: string;
  host: string;
  self?: boolean;
  path?: string;
  online: boolean;
  reason?: string;
  used: number;
  free: number;
  total: number;
  last_seen?: string;
  warn?: "stale" | "reserve" | "incomplete";
  syncing?: MirrorSync;
}

export interface Snapshot {
  id: number;
  mirror: string;
  name: string;
  at: string;
  auto: boolean;
  state: "syncing" | "complete" | "incomplete";
  files: number;
  bytes: number;
  new_bytes: number;
  missing: number;
}

export interface Schedule {
  every: number;
  hourly: number;
  daily: number;
  weekly: number;
  monthly: number;
  next?: string;
}

export interface Snapshots {
  mirrors: Mirror[];
  snapshots: Snapshot[];
  schedule: Schedule;
}

export interface VersionSnap {
  id: number;
  mirror: string;
  name: string;
  at: string;
  auto: boolean;
}

export interface Version {
  key: string;
  size: number;
  mtime: string;
  current?: boolean;
  snapshots: VersionSnap[];
}

export interface Versions {
  path: string;
  versions: Version[];
  offline: { mirror: string; count: number }[];
}

export interface TreeRestore {
  path: string;
  files: number;
  dirs: number;
  links: number;
  skipped: string[];
  skipped_count: number;
  uploaded: number;
}

export const snapshots = (): Promise<Snapshots> => call("/api/snapshots");
export const takeSnapshot = (opts: { name?: string; mirror?: string } = {}): Promise<{ mirror: string; name: string }[]> => post("/api/snapshot", opts);
export const versions = (path: string): Promise<Versions> => call("/api/versions?path=" + encodeURIComponent(path));
export const revert = (path: string, snapshot: number): Promise<{ path: string; uploaded: number }> => post("/api/revert", { path, snapshot });
export const copyVersion = (path: string, snapshot: number, name: string): Promise<{ entry: Entry; uploaded: number }> => post("/api/copy", { path, snapshot, name });
export const restoreTree = (snapshot: number, path: string, opts: { to?: string; name?: string } = {}): Promise<TreeRestore> => post("/api/restore-tree", { snapshot, path, ...opts });
