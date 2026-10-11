// Re-exports shared by the views, plus the hash-route path encoding: every segment is
// encoded on its own so "#", "?" and "%" in names survive the URL fragment.

export { basename, bytes, count, daysAgo, daysUntil, dirname, ext, join, mmss, when } from "./fmt.ts";

/** Ctx is where a view looks: a live folder, or a path inside the deleted root `root` (docs/13 §4). */
export interface Ctx {
  path: string;
  root?: number;
}

/** browseHash is the route to a folder, live or inside a deleted root. */
export function browseHash(ctx: Ctx): string {
  return ctx.root === undefined ? "#/browse" + encodePathForHash(ctx.path) : `#/trash/${ctx.root}` + encodePathForHash(ctx.path);
}

/** fileHash is the route to a file view, live or inside a deleted root. */
export function fileHash(ctx: Ctx): string {
  return ctx.root === undefined ? "#/file" + encodePathForHash(ctx.path) : `#/trashfile/${ctx.root}` + encodePathForHash(ctx.path);
}

/** session reads and writes one sessionStorage key, silently when storage is unavailable. */
export const session = {
  get<T>(key: string, fallback: T): T {
    try {
      const s = sessionStorage.getItem(key);
      if (s) return JSON.parse(s) as T;
    } catch {
      // storage may be unavailable
    }
    return fallback;
  },
  set(key: string, v: unknown): void {
    try {
      sessionStorage.setItem(key, JSON.stringify(v));
    } catch {
      // ignore
    }
  },
};

export function encodePathForHash(path: string): string {
  return path
    .split("/")
    .map((s) => encodeURIComponent(s))
    .join("/");
}

export function decodeHashPath(encoded: string): string {
  const p = encoded
    .split("/")
    .map((s) => {
      try {
        return decodeURIComponent(s);
      } catch {
        return s;
      }
    })
    .join("/");
  return p.startsWith("/") ? p : "/" + p;
}
