// Thumbnails for the tile view (docs/13 §5): loaded lazily as tiles scroll into view,
// three at a time; a video without a stored thumbnail has its middle frame grabbed by
// the browser and PUT once per file and version.

import { fileURL, putThumb, thumbURL, type Entry } from "./api.ts";
import { session } from "./util.ts";

const CONCURRENT = 3;
const WIDTH = 320;

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

/** lazy schedules img to show the thumbnail of path once it is near the viewport; onMiss replaces it with the generic icon. */
export function lazy(img: HTMLImageElement, e: Entry, path: string, root: number | undefined, kind: "image" | "video" | "audio", onMiss: () => void): void {
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
      if (e.thumb || kind !== "video" || root !== undefined) {
        show(thumbURL(path, root));
        return;
      }
      void grab(path, e)
        .then((ok) => (ok ? show(thumbURL(path)) : (onMiss(), done())))
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

/** grab draws the frame at half the video's duration on a canvas and stores it; one attempt per file and version per session. */
async function grab(path: string, e: Entry): Promise<boolean> {
  const key = `tr.vthumb:${e.ino}.${e.version}`;
  if (session.get(key, false)) return false;
  session.set(key, true);
  const blob = await frame(fileURL(path));
  if (!blob) return false;
  await putThumb(path, e.version, blob);
  return true;
}

function frame(src: string): Promise<Blob | null> {
  return new Promise((resolve) => {
    const v = document.createElement("video");
    v.preload = "metadata";
    v.muted = true;
    v.playsInline = true;
    v.crossOrigin = "anonymous";
    const finish = (b: Blob | null) => {
      v.removeAttribute("src");
      v.load();
      resolve(b);
    };
    const timer = setTimeout(() => finish(null), 20_000);
    v.onerror = () => {
      clearTimeout(timer);
      finish(null);
    };
    v.onloadedmetadata = () => {
      if (!Number.isFinite(v.duration) || v.duration <= 0) {
        clearTimeout(timer);
        finish(null);
        return;
      }
      v.currentTime = v.duration / 2;
    };
    v.onseeked = () => {
      clearTimeout(timer);
      const w = Math.min(WIDTH, v.videoWidth || WIDTH);
      const h = Math.max(1, Math.round((v.videoHeight || 9) * (w / (v.videoWidth || 16))));
      const c = document.createElement("canvas");
      c.width = w;
      c.height = h;
      const g = c.getContext("2d");
      if (!g) {
        finish(null);
        return;
      }
      g.drawImage(v, 0, 0, w, h);
      c.toBlob((b) => finish(b), "image/jpeg", 0.8);
    };
    v.src = src;
  });
}
