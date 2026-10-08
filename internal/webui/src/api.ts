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
}

export interface NodeReport {
  idx: number;
  id: string;
  name: string;
  kind: string;
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
  free: number;
  capacity: number;
  version: string;
  applied: number;
  slots: number;
  missing_slots: number;
  dev?: boolean;
  joined_at: string;
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

export interface RepairStatus {
  last_scan: string;
  stats: RepairStats;
  reported_bad_slots: number;
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

/** upload PUTs a File with progress; XMLHttpRequest streams the file and reports progress everywhere, including iOS. */
export function upload(path: string, file: File, onProgress: (sent: number) => void): Upload {
  const xhr = new XMLHttpRequest();
  const promise = new Promise<void>((resolve, reject) => {
    xhr.open("PUT", fileURL(path));
    xhr.setRequestHeader(HEADER, "1");
    xhr.upload.onprogress = (e) => onProgress(e.loaded);
    xhr.onload = () => {
      if (xhr.status >= 200 && xhr.status < 300) resolve();
      else {
        let msg = xhr.statusText || "upload failed";
        try {
          const body = JSON.parse(xhr.responseText) as { error?: string };
          if (body.error) msg = body.error;
        } catch {
          // not JSON
        }
        reject(new ApiError(xhr.status, `${msg} (${xhr.status})`));
      }
    };
    xhr.onerror = () => reject(new ApiError(0, "network error"));
    xhr.onabort = () => reject(new ApiError(0, "cancelled"));
    xhr.send(file);
  });
  return { promise, abort: () => xhr.abort() };
}
