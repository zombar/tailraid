// Thumbnails for the tile view (docs/13 §5): loaded lazily as tiles come near the viewport,
// in three lanes. Thumbnails the node serves load FAST at a time; one the node is still
// making answers 202 and is asked again from a lane of its own, SLOW at a time after the
// reply's Retry-After (I-374); a video without a stored thumbnail has a frame chosen by the
// browser, one GRABS at a time in a lane of its own. So a cover being made, or a slow or
// undecodable video, never holds up the tiles behind it. A tile that leaves the
// viewport before its turn (in either lane) waits again, and one a redraw removed is
// dropped. FAST stays below the browser's six connections to the node, so playback and
// /api requests always find one, and a thumbnail that has not arrived by LOAD_TIMEOUT is
// cancelled, freeing its slot and its connection.

import { fileURL, putThumb, thumbURL, type Entry, type Src } from "./api.ts";
import { ext, join, session } from "./util.ts";

const FAST = 4;
const SLOW = 2;
const GRABS = 1;
/** TRIES is how often a thumbnail the node is still making is asked for before the tile keeps its icon. */
const TRIES = 5;
const WIDTH = 320;
/** RECIPE is the frame choice below; the node keys a grabbed frame by it (webui.FrameRecipe, I-216). */
const RECIPE = 2;
/** POSITIONS are the fractions of the duration tried in order. */
const POSITIONS = [0.5, 0.25, 0.75, 0.1, 0.9];
/** A frame passes with a mean luma of at least MIN_LUMA and a luma spread of at least MIN_SPREAD (of 255). */
const MIN_LUMA = 28;
const MIN_SPREAD = 14;
const PROBE = 64;
/** LOAD_TIMEOUT bounds one thumbnail or folder tile request. */
const LOAD_TIMEOUT = 20_000;
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
const slow = new Lane(SLOW);
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
  const miss = () => {
    if (img.isConnected) onMiss();
  };
  const settle = async (f: Fetched): Promise<void> => {
    if (!f.src) return miss();
    if (!img.isConnected) return URL.revokeObjectURL(f.src);
    return showBlob(img, f.src, onMiss);
  };
  const show = async (url: string): Promise<void> => {
    const f = await load(url);
    if (f.later !== undefined) return later(img, () => load(url), f.later, TRIES, settle);
    return settle(f);
  };
  const tag = kind === "video" ? `${e.version}.f${RECIPE}` : undefined; // a cached frame of another recipe is not reused
  const job = async (): Promise<void> => {
    pending.delete(img);
    observer?.unobserve(img);
    if (!img.isConnected) return; // a redraw replaced the tile
    if (kind === "dir" && root === undefined) {
      const done = async (r: Fetched): Promise<void> => {
        if (r.src) return showBlob(img, r.src, onMiss);
        if (!r.grab) return miss();
        const g = r.grab;
        // the folder's art is a video nobody framed: frame it in the grab lane, then ask again
        grabs.push(async () => {
          if (!img.isConnected) return;
          if (!near(img)) return wait(); // scrolled away while queued: framed when it is back
          if (!(await grab(join(path, g.path), { ino: g.ino, version: g.version, thumb: false }))) return miss();
          const again = await folder(path);
          if (again.later !== undefined) return later(img, () => folder(path), again.later, TRIES, settle);
          if (again.src) await showBlob(img, again.src, onMiss);
          else miss();
        });
      };
      const r = await folder(path);
      if (r.later !== undefined) return later(img, () => folder(path), r.later, TRIES, done);
      return done(r);
    }
    if (e.thumb || kind !== "video" || root !== undefined) return show(thumbURL(path, root, tag));
    grabs.push(async () => {
      if (!img.isConnected) return;
      if (!near(img)) return wait();
      if (await grab(path, e)) await show(thumbURL(path, undefined, tag));
      else miss();
    });
  };
  const p: Pending = { img, job, queued: false };
  const wait = () => {
    if (!observer) return;
    p.queued = false;
    pending.set(img, p);
    observer.observe(img);
  };
  if (observer) wait();
  else {
    p.queued = true;
    fast.push(job);
  }
}

/** near reports whether img is within the observer's margin of the viewport. */
function near(img: HTMLImageElement): boolean {
  const r = img.getBoundingClientRect();
  return r.bottom >= -200 && r.top <= window.innerHeight + 200;
}

/** Fetched is what a thumbnail request came back with: the picture as an object URL, the seconds to wait while the node makes it (202), the video a folder needs framed, or nothing. */
interface Fetched {
  src?: string;
  later?: number;
  grab?: Grab;
}

/** retryAfter is a 202's Retry-After in seconds (2 when absent, at most 30). */
function retryAfter(r: Response): number {
  const n = Number(r.headers.get("Retry-After"));
  return Number.isFinite(n) && n > 0 ? Math.min(n, 30) : 2;
}

/** later asks again, from the slow lane after wait seconds, for a thumbnail the node was still making, at most tries times in all, then hands what came back to done (docs/13 §5, I-374). */
function later(img: HTMLImageElement, ask: () => Promise<Fetched>, wait: number, tries: number, done: (f: Fetched) => Promise<void>): void {
  window.setTimeout(() => {
    slow.push(async () => {
      if (!img.isConnected) return; // a redraw replaced the tile
      const f = await ask();
      if (f.later !== undefined) {
        if (tries > 1) return later(img, ask, f.later, tries - 1, done);
        return done({}); // still not made: the tile keeps its icon
      }
      return done(f);
    });
  }, wait * 1000);
}

/** load fetches a thumbnail as an object URL, a "later" while the node makes it, or nothing on a miss, an error or LOAD_TIMEOUT. */
async function load(url: string): Promise<Fetched> {
  try {
    const r = await fetch(url, { signal: AbortSignal.timeout(LOAD_TIMEOUT) });
    if (r.status === 202) return { later: retryAfter(r) };
    if (!r.ok) return {};
    return { src: URL.createObjectURL(await r.blob()) };
  } catch {
    return {};
  }
}

/** showBlob sets img to an object URL and settles once it decoded or failed, revoking the URL. */
function showBlob(img: HTMLImageElement, src: string, onMiss: () => void): Promise<void> {
  return new Promise<void>((resolve) => {
    img.onload = () => {
      URL.revokeObjectURL(src);
      resolve();
    };
    img.onerror = () => {
      URL.revokeObjectURL(src);
      onMiss();
      resolve();
    };
    img.src = src;
  });
}

/** Grab is the video a folder without art names for the page to frame (webui.Grab). */
interface Grab {
  path: string;
  ino: number;
  version: number;
}

/** folder fetches a folder's tile as an object URL. A folder whose art would be a video nobody framed yet answers 404 naming it: the caller frames that video in the grab lane, which settles the folder, and asks once more (docs/13 §5, I-275). */
async function folder(path: string): Promise<Fetched> {
  const r = await fetch(thumbURL(path), { signal: AbortSignal.timeout(LOAD_TIMEOUT) }).catch(() => undefined);
  if (!r) return {};
  if (r.status === 202) return { later: retryAfter(r) }; // its cover is still being made (I-374)
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
