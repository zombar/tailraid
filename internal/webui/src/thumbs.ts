// Thumbnails for the tile view (docs/13 §5): loaded lazily as tiles scroll into view,
// three at a time; a video without a stored thumbnail has a frame chosen by the browser
// and PUT once per file, version and recipe.

import { fileURL, putThumb, thumbURL, type Entry, type Src } from "./api.ts";
import { session } from "./util.ts";

const CONCURRENT = 3;
const WIDTH = 320;
/** RECIPE is the frame choice below; the node keys a grabbed frame by it (webui.FrameRecipe, I-216). */
const RECIPE = 2;
/** POSITIONS are the fractions of the duration tried in order. */
const POSITIONS = [0.5, 0.25, 0.75, 0.1, 0.9];
/** A frame passes with a mean luma of at least MIN_LUMA and a luma spread of at least MIN_SPREAD (of 255). */
const MIN_LUMA = 28;
const MIN_SPREAD = 14;
const PROBE = 64;

interface Pending {
  img: HTMLImageElement;
  load: () => Promise<void>;
}

const queue: Pending[] = [];
let active = 0;

const observer =
  typeof IntersectionObserver === "undefined"
    ? null
    : new IntersectionObserver(
        (entries) => {
          for (const e of entries) {
            if (!e.isIntersecting) continue;
            observer?.unobserve(e.target);
            const p = waiting.get(e.target as HTMLImageElement);
            if (p) {
              waiting.delete(e.target as HTMLImageElement);
              queue.push(p);
            }
          }
          pump();
        },
        { rootMargin: "200px" },
      );
const waiting = new Map<HTMLImageElement, Pending>();

function pump(): void {
  while (active < CONCURRENT && queue.length) {
    const p = queue.shift()!;
    active++;
    void p.load().finally(() => {
      active--;
      pump();
    });
  }
}

/** Thumbable is what a thumbnail needs of a file: a listing entry, or a media item's video (docs/13 §10). */
export type Thumbable = Pick<Entry, "ino" | "version" | "thumb">;

/** lazy schedules img to show the thumbnail of path once it is near the viewport; onMiss replaces it with the generic icon. */
export function lazy(img: HTMLImageElement, e: Thumbable, path: string, root: Src, kind: "image" | "video" | "audio", onMiss: () => void): void {
  const load = () =>
    new Promise<void>((resolve) => {
      const done = () => resolve();
      const show = (src: string) => {
        img.onload = done;
        img.onerror = () => {
          onMiss();
          done();
        };
        img.src = src;
      };
      const tag = kind === "video" ? `${e.version}.f${RECIPE}` : undefined; // a cached frame of another recipe is not reused
      if (e.thumb || kind !== "video" || root !== undefined) {
        show(thumbURL(path, root, tag));
        return;
      }
      void grab(path, e)
        .then((ok) => (ok ? show(thumbURL(path, undefined, tag)) : (onMiss(), done())))
        .catch(() => {
          onMiss();
          done();
        });
    });
  const p: Pending = { img, load };
  if (observer) {
    waiting.set(img, p);
    observer.observe(img);
  } else {
    queue.push(p);
    pump();
  }
}

/** grab chooses a frame of the video and stores it; one attempt per file, version and recipe per session. */
async function grab(path: string, e: Thumbable): Promise<boolean> {
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
    timer = window.setTimeout(() => resolve(null), 30_000);
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
