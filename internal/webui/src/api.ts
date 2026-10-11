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
  web_url?: string;
  generated_at: string;
}

export interface Entry {
  name: string;
  type: "file" | "dir" | "symlink";
  size: number;
  mtime: string;
  mode: number;
  ino: number;
  dirty?: boolean;
  upload?: boolean;
  target?: string;
  path?: string;
}

export interface Ls {
  path: string;
  entries: Entry[];
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
  constructor(public status: number, message: string) {
    super(message);
  }
}

/** HEADER is the CSRF header every mutating request carries (docs/13 §3). */
export const HEADER = "X-Tailraid-UI";

export function encodePath(p: string): string {
  return p.split("/").map(encodeURIComponent).join("/");
}

export function fileURL(path: string, download = false): string {
  return "/files" + encodePath(path) + (download ? "?download=1" : "");
}

async function call<T>(url: string, init?: RequestInit): Promise<T> {
  const r = await fetch(url, init);
  if (!r.ok) throw new ApiError(r.status, await errorText(r));
  if (r.status === 204) return undefined as T;
  return (await r.json()) as T;
}

async function errorText(r: Response): Promise<string> {
  try {
    const body = (await r.json()) as { error?: string };
    if (body.error) return `${body.error} (${r.status})`;
  } catch {
    // not JSON
  }
  return `${r.statusText || "request failed"} (${r.status})`;
}

export const getStatus = (): Promise<Report> => call("/api/status");
export const ls = (path: string): Promise<Ls> => call("/api/ls?path=" + encodeURIComponent(path));
export const stat = (path: string): Promise<Entry> => call("/api/stat?path=" + encodeURIComponent(path));
export const chunks = (path: string, offset: number, limit: number): Promise<ChunksPage> =>
  call(`/api/chunks?path=${encodeURIComponent(path)}&offset=${offset}&limit=${limit}`);
export const mkdir = (path: string): Promise<{ path: string }> =>
  call("/api/mkdir", { method: "POST", headers: { [HEADER]: "1", "Content-Type": "application/json" }, body: JSON.stringify({ path }) });

/** text fetches a small file as text (previews). */
export async function text(path: string): Promise<string> {
  const r = await fetch(fileURL(path));
  if (!r.ok) throw new ApiError(r.status, await errorText(r));
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
