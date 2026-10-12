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

/** HEADER is the CSRF header every mutating request carries (docs/13 §3). */
export const HEADER = "X-Tailraid-UI";

export function encodePath(p: string): string {
  return p.split("/").map(encodeURIComponent).join("/");
}

/** inQuery is the `in=<ino>` parameter that addresses a deleted root (docs/13 §4). */
function inQuery(root: number | undefined, first = false): string {
  return root === undefined ? "" : (first ? "?" : "&") + "in=" + root;
}

export function fileURL(path: string, download = false, root?: number): string {
  const q: string[] = [];
  if (download) q.push("download=1");
  if (root !== undefined) q.push("in=" + root);
  return "/files" + encodePath(path) + (q.length ? "?" + q.join("&") : "");
}

/** thumbURL is the thumbnail of path; tag, when given, keys the browser's cache (a video's version and frame recipe, docs/13 §5). */
export function thumbURL(path: string, root?: number, tag?: string): string {
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
export const ls = (path: string, root?: number): Promise<Ls> => call("/api/ls?path=" + encodeURIComponent(path) + inQuery(root));
export const stat = (path: string, root?: number): Promise<Entry> => call("/api/stat?path=" + encodeURIComponent(path) + inQuery(root));
export const trashList = (): Promise<{ entries: TrashEntry[] }> => call("/api/trash");
export const chunks = (path: string, offset: number, limit: number): Promise<ChunksPage> =>
  call(`/api/chunks?path=${encodeURIComponent(path)}&offset=${offset}&limit=${limit}`);
export const tags = (path: string, root?: number): Promise<Tags> => call("/api/tags?path=" + encodeURIComponent(path) + inQuery(root));
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
  kind: "movie" | "show" | "season" | "episode" | "album";
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
export async function rendered(path: string, root?: number): Promise<string> {
  const r = await fetch("/api/render?path=" + encodeURIComponent(path) + inQuery(root));
  if (!r.ok) throw await apiError(r);
  return r.text();
}

/** text fetches a small file as text (previews). */
export async function text(path: string, root?: number): Promise<string> {
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

/** uploadedSoFar asks the server how much of an upload's temp already exists (0 when none). */
async function uploadedSoFar(path: string): Promise<number> {
  const i = path.lastIndexOf("/");
  const temp = path.slice(0, i + 1) + "." + path.slice(i + 1) + ".tailraid-upload";
  try {
    return (await stat(temp)).size;
  } catch (e) {
    if (e instanceof ApiError && e.status === 404) return 0;
    throw e;
  }
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
        offset = await uploadedSoFar(path);
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

export const getStats = (from: number, to: number | "now", node: string): Promise<StatsResponse> =>
  call(`/api/stats?from=${from}&to=${to}&node=${encodeURIComponent(node)}`);
