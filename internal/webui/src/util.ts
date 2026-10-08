// Re-exports shared by the views, plus the hash-route path encoding: every segment is
// encoded on its own so "#", "?" and "%" in names survive the URL fragment.

export { basename, bytes, count, dirname, ext, join, when } from "./fmt.ts";

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
