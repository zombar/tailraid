// Formatting per docs/13 §Design "Copy": binary units with one decimal, thousands
// separators, relative times under 24 h, durations as the server prints them.

const UNITS = ["B", "KiB", "MiB", "GiB", "TiB", "PiB"];

export function bytes(n: number): string {
  if (!(n > 0)) return "0 B";
  let i = 0;
  let v = n;
  while (v >= 1024 && i < UNITS.length - 1) {
    v /= 1024;
    i++;
  }
  return (i === 0 ? v.toFixed(0) : v.toFixed(1)) + " " + UNITS[i];
}

export function count(n: number): string {
  return n.toLocaleString("en-US");
}

export function pct(frac: number, digits = 0): string {
  return (frac * 100).toFixed(digits) + " %";
}

/** signedPct renders a deviation as "+2.2 %" / "−1.5 %". */
export function signedPct(frac: number): string {
  const s = (Math.abs(frac) * 100).toFixed(1) + " %";
  return (frac < 0 ? "−" : "+") + s;
}

const ZERO_TIME = "0001-01-01";

/** ago renders a timestamp relative to now ("never" for Go's zero time). */
export function ago(iso: string | undefined, now = Date.now()): string {
  if (!iso || iso.startsWith(ZERO_TIME)) return "never";
  const t = Date.parse(iso);
  if (Number.isNaN(t)) return iso;
  const s = Math.max(0, Math.round((now - t) / 1000));
  if (s < 60) return s + " s ago";
  const m = Math.round(s / 60);
  if (m < 60) return m + " min ago";
  const hrs = Math.round(m / 60);
  if (hrs < 24) return hrs + " h ago";
  return date(t);
}

/** when is relative under 24 h, else "2026-10-08 14:03". */
export function when(iso: string | undefined, now = Date.now()): string {
  if (!iso || iso.startsWith(ZERO_TIME)) return "";
  const t = Date.parse(iso);
  if (Number.isNaN(t)) return iso;
  if (now - t < 24 * 3600 * 1000 && now >= t) return ago(iso, now);
  return date(t);
}

export function date(t: number): string {
  const d = new Date(t);
  const p = (n: number) => String(n).padStart(2, "0");
  return `${d.getFullYear()}-${p(d.getMonth() + 1)}-${p(d.getDate())} ${p(d.getHours())}:${p(d.getMinutes())}`;
}

export function shortDate(iso: string | undefined): string {
  if (!iso || iso.startsWith(ZERO_TIME)) return "";
  const t = Date.parse(iso);
  if (Number.isNaN(t)) return iso;
  return date(t).slice(0, 10);
}

// --- paths ------------------------------------------------------------------------

export function dirname(p: string): string {
  const i = p.lastIndexOf("/");
  return i <= 0 ? "/" : p.slice(0, i);
}

export function basename(p: string): string {
  return p.slice(p.lastIndexOf("/") + 1);
}

export function join(dir: string, name: string): string {
  return dir === "/" ? "/" + name : dir + "/" + name;
}

export function ext(name: string): string {
  const i = name.lastIndexOf(".");
  return i < 0 ? "" : name.slice(i + 1).toLowerCase();
}
