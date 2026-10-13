// Thumbnails for the tile view (docs/13 §5): loaded lazily as tiles come near the viewport,
// in two lanes. Thumbnails the node serves load FAST at a time; a video without a stored
// thumbnail has a frame chosen by the browser, one GRABS at a time in a lane of its own, so
// a slow or undecodable video never holds up the tiles behind it. A tile that leaves the
// viewport before its turn waits again, and one a redraw removed is dropped.

import { fileURL, putThumb, thumbURL, type Entry, type Src } from "./api.ts";
import { ext, join, session } from "./util.ts";

const FAST = 6;
const GRABS = 1;
const WIDTH = 320;
/** RECIPE is the frame choice below; the node keys a grabbed frame by it (webui.FrameRecipe, I-216). */
const RECIPE = 2;
/** POSITIONS are the fractions of the duration tried in order. */
const POSITIONS = [0.5, 0.25, 0.75, 0.1, 0.9];
/** A frame passes with a mean luma of at least MIN_LUMA and a luma spread of at least MIN_SPREAD (of 255). */
const MIN_LUMA = 28;
const MIN_SPREAD = 14;
const PROBE = 64;
/** FRAME_TIMEOUT bounds one video's frame choice. */
const FRAME_TIMEOUT = 15_000;
/** NO_FRAME are containers a browser does not decode: no frame is attempted, the tile keeps its icon. */
const NO_FRAME = new Set(["avi", "wmv", "mpg", "mpeg", "flv", "vob", "rm", "rmvb", "divx", "asf"]);

/** A lane runs at most `max` jobs at once, in the order they were queued. */
class Lane {
  private queue: (() => Promise<void>)[] = [];
  private active = 0;
  constructor(private readonly max: number) {}
  push(job: () => Promise<void>): void {
    this.queue.push(job);
    this.pump();
  }
  /** take removes a job that has not started. */
  take(job: () => Promise<void>): void {
    const i = this.queue.indexOf(job);
    if (i >= 0) this.queue.splice(i, 1);
  }
  private pump(): void {
    while (this.active < this.max && this.queue.length) {
      const job = this.queue.shift()!;
      this.active++;
      void job()
        .catch(() => undefined)
        .finally(() => {
          this.active--;
          this.pump();
        });
    }
  }
}

const fast = new Lane(FAST);
const grabs = new Lane(GRABS);

interface Pending {
  img: HTMLImageElement;
  job: () => Promise<void>;
  queued: boolean;
}

const pending = new Map<HTMLImageElement, Pending>();

const observer =
  typeof IntersectionObserver === "undefined"
    ? null
    : new IntersectionObserver(
        (entries) => {
          for (const e of entries) {
            const p = pending.get(e.target as HTMLImageElement);
            if (!p) continue;
            if (e.isIntersecting && !p.queued) {
              p.queued = true;
              fast.push(p.job);
            } else if (!e.isIntersecting && p.queued) {
              p.queued = false; // scrolled away before its turn: it waits again
              fast.take(p.job);
            }
          }
        },
        { rootMargin: "200px" },
      );

/** Thumbable is what a thumbnail needs of a file: a listing entry, or a media item's video (docs/13 §10). */
export type Thumbable = Pick<Entry, "ino" | "version" | "thumb">;

/** lazy schedules img to show the thumbnail of path once it is near the viewport; onMiss replaces it with the generic icon. A "dir" is a folder whose cover is not known yet. */
export function lazy(img: HTMLImageElement, e: Thumbable, path: string, root: Src, kind: "image" | "video" | "audio" | "dir", onMiss: () => void): void {
  const show = (src: string) =>
    new Promise<void>((resolve) => {
      img.onload = () => resolve();
      img.onerror = () => {
        onMiss();
        resolve();
      };
      img.src = src;
    });
  const miss = () => {
    if (img.isConnected) onMiss();
  };
  const tag = kind === "video" ? `${e.version}.f${RECIPE}` : undefined; // a cached frame of another recipe is not reused
  const job = async (): Promise<void> => {
    pending.delete(img);
    observer?.unobserve(img);
    if (!img.isConnected) return; // a redraw replaced the tile
    if (kind === "dir" && root === undefined) {
      const r = await folder(path);
      if (r.src) {
        img.addEventListener("load", () => URL.revokeObjectURL(r.src!), { once: true });
        return show(r.src);
      }
      if (!r.grab) return miss();
      const g = r.grab;
      // the folder's art is a video nobody framed: frame it in the grab lane, then ask again
      grabs.push(async () => {
        if (!img.isConnected) return;
        if (!(await grab(join(path, g.path), { ino: g.ino, version: g.version, thumb: false }))) return miss();
        const again = await folder(path);
        if (again.src) {
          img.addEventListener("load", () => URL.revokeObjectURL(again.src!), { once: true });
          await show(again.src);
        } else miss();
      });
      return;
    }
    if (e.thumb || kind !== "video" || root !== undefined) return show(thumbURL(path, root, tag));
    grabs.push(async () => {
      if (!img.isConnected) return;
      if (await grab(path, e)) await show(thumbURL(path, undefined, tag));
      else miss();
    });
  };
  const p: Pending = { img, job, queued: false };
  if (observer) {
    pending.set(img, p);
    observer.observe(img);
  } else {
    p.queued = true;
    fast.push(job);
  }
}

/** Grab is the video a folder without art names for the page to frame (webui.Grab). */
interface Grab {
  path: string;
  ino: number;
  version: number;
}

/** folder fetches a folder's tile as an object URL. A folder whose art would be a video nobody framed yet answers 404 naming it: the caller frames that video in the grab lane, which settles the folder, and asks once more (docs/13 §5, I-275). */
async function folder(path: string): Promise<{ src?: string; grab?: Grab }> {
  const r = await fetch(thumbURL(path));
  if (r.ok) return { src: URL.createObjectURL(await r.blob()) };
  if (r.status !== 404) return {};
  const miss = (await r.json().catch(() => ({}))) as { grab?: Grab };
  return { grab: miss.grab };
}

/** grab chooses a frame of the video and stores it; one attempt per file, version and recipe per session. */
async function grab(path: string, e: Thumbable): Promise<boolean> {
  if (NO_FRAME.has(ext(path))) return false; // the browser cannot decode it
  const key = `tr.vthumb:${e.ino}.${e.version}.f${RECIPE}`;
  if (session.get(key, false)) return false;
  session.set(key, true);
  const blob = await frame(fileURL(path));
  if (!blob) return false;
  await putThumb(path, e.version, blob);
  return true;
}

/** frame tries POSITIONS in order and encodes the first frame that is neither dark nor flat, else the best scored one (docs/13 §5). */
async function frame(src: string): Promise<Blob | null> {
  const v = document.createElement("video");
  v.preload = "auto";
  v.muted = true;
  v.playsInline = true;
  v.crossOrigin = "anonymous";
  let timer = 0;
  const expired = new Promise<null>((resolve) => {
    timer = window.setTimeout(() => resolve(null), FRAME_TIMEOUT);
  });
  const choose = async (): Promise<Blob | null> => {
    v.src = src;
    await event(v, "loadedmetadata");
    if (!Number.isFinite(v.duration) || v.duration <= 0) return null;
    const w = Math.min(WIDTH, v.videoWidth || WIDTH);
    const h = Math.max(1, Math.round((v.videoHeight || 9) * (w / (v.videoWidth || 16))));
    let best: HTMLCanvasElement | null = null;
    let bestScore = -1;
    for (const at of POSITIONS) {
      v.currentTime = v.duration * at;
      await event(v, "seeked");
      await presented(v);
      const c = document.createElement("canvas");
      c.width = w;
      c.height = h;
      const g = c.getContext("2d");
      if (!g) return null;
      g.drawImage(v, 0, 0, w, h);
      const { mean, spread } = luma(c);
      if (mean >= MIN_LUMA && spread >= MIN_SPREAD) {
        best = c;
        break;
      }
      const score = spread + mean / 4;
      if (score > bestScore) {
        best = c;
        bestScore = score;
      }
    }
    if (!best) return null;
    const chosen = best;
    return new Promise<Blob | null>((resolve) => chosen.toBlob((b) => resolve(b), "image/jpeg", 0.8));
  };
  try {
    return await Promise.race([choose().catch(() => null), expired]);
  } finally {
    clearTimeout(timer);
    v.removeAttribute("src");
    v.load();
  }
}

/** event resolves on the element's next name event and rejects on an error. */
function event(v: HTMLVideoElement, name: string): Promise<void> {
  return new Promise((resolve, reject) => {
    const ok = () => {
      v.removeEventListener("error", bad);
      resolve();
    };
    const bad = () => {
      v.removeEventListener(name, ok);
      reject(new Error("video error"));
    };
    v.addEventListener(name, ok, { once: true });
    v.addEventListener("error", bad, { once: true });
  });
}

/** presented waits until the sought frame is decoded and presented, at most a second: a frame read at seeked alone is often still black. */
function presented(v: HTMLVideoElement): Promise<void> {
  return new Promise((resolve) => {
    const t = window.setTimeout(resolve, 1000);
    const done = () => {
      clearTimeout(t);
      resolve();
    };
    if (typeof v.requestVideoFrameCallback === "function") { // Firefox before 132 has none
      v.requestVideoFrameCallback(() => done());
    } else if (v.readyState >= HTMLMediaElement.HAVE_CURRENT_DATA) {
      done();
    } else {
      v.addEventListener("loadeddata", done, { once: true });
    }
  });
}

/** luma is the mean and standard deviation of the frame's luma on a PROBE-wide copy. */
function luma(c: HTMLCanvasElement): { mean: number; spread: number } {
  const p = document.createElement("canvas");
  p.width = PROBE;
  p.height = Math.max(1, Math.round((c.height * PROBE) / c.width));
  const g = p.getContext("2d", { willReadFrequently: true });
  if (!g) return { mean: 0, spread: 0 };
  g.drawImage(c, 0, 0, p.width, p.height);
  const d = g.getImageData(0, 0, p.width, p.height).data;
  let sum = 0;
  let sq = 0;
  const n = d.length / 4;
  for (let i = 0; i < d.length; i += 4) {
    const y = 0.2126 * d[i]! + 0.7152 * d[i + 1]! + 0.0722 * d[i + 2]!;
    sum += y;
    sq += y * y;
  }
  const mean = sum / n;
  return { mean, spread: Math.sqrt(Math.max(0, sq / n - mean * mean)) };
}
