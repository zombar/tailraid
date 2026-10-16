// Snapshots on mirror disks in the browser (docs/13 §4, §7, docs/16 §6, I-187): the
// as-of menu over a listing or a file, the banner of a view read as of a snapshot with
// its folder restore, a file's Versions panel with View, Revert and Save a copy, and the
// dashboard's Snapshots tile and Mirrors section. Taking a snapshot is a user's file
// operation here; no page removes one (I-183), and adding or removing a mirror stays in
// the CLI.

import { clear, h, setWidth, toast } from "./dom.ts";
import { ApiError, cancelRestore, copyVersion, offline, restoreTree, revert, snapshots, takeSnapshot, versions, type Mirror, type RestoreJob, type SnapRef, type Snapshot, type Snapshots, type Version } from "./api.ts";
import { bar, barRow, fillTone } from "./chart.ts";
import { basename, browseHash, bytes, dirname, ext, fileHash, when, type Ctx } from "./util.ts";
import { ago, daysAgo, shortDate } from "./fmt.ts";
import { pick } from "./picker.ts";
import { status } from "./status.ts";
import * as account from "./account.ts";

// --- the catalogue, fetched at most every few seconds ------------------------------------

const FRESH_MS = 10_000;
let cached: { at: number; v: Snapshots } | null = null;
let pending: Promise<Snapshots> | null = null;

/** catalogue is GET /api/snapshots, shared by every view for FRESH_MS. */
export function catalogue(force = false): Promise<Snapshots> {
  if (!force && cached && Date.now() - cached.at < FRESH_MS) return Promise.resolve(cached.v);
  pending ??= snapshots().then(
    (v) => {
      cached = { at: Date.now(), v };
      pending = null;
      return v;
    },
    (e: unknown) => {
      pending = null;
      throw e;
    },
  );
  return pending;
}

/** last is the catalogue as last fetched, for a synchronous draw. */
export function last(): Snapshots | null {
  return cached?.v ?? null;
}

const errText = (e: unknown) => (e instanceof Error ? e.message : String(e));

/**
 * kind says why a snapshot is kept: "first" for a mirror's oldest, else an automatic one's
 * schedule bucket by its age (hourly, daily, weekly, monthly, docs/16 §4); "" for a manual
 * one, which its badge names.
 */
function kind(sn: Snapshot, c: Snapshots): string {
  const mine = c.snapshots.filter((s) => s.mirror === sn.mirror && s.state !== "syncing");
  if (mine.length > 1 && mine[mine.length - 1]?.id === sn.id) return "first";
  if (!sn.auto) return "";
  const hours = (Date.now() - Date.parse(sn.at)) / 3_600_000;
  const s = c.schedule;
  if (hours < s.hourly) return "hourly";
  if (hours < s.daily * 24) return "daily";
  if (hours < s.weekly * 24 * 7) return "weekly";
  return "monthly";
}

/** taken is "12 min ago · hourly", or the time alone for a manual snapshot. */
function taken(sn: Snapshot, c: Snapshots): string {
  const k = kind(sn, c);
  return when(sn.at) + (k ? " · " + k : "");
}

/** offlineFor is "offline 3 d" for a mirror last seen at last_seen. */
function offlineFor(m: Mirror): string {
  if (!m.last_seen) return "offline";
  const d = daysAgo(m.last_seen);
  return d === "today" ? "offline" : "offline " + d.replace(" ago", "");
}

// --- the as-of menu ------------------------------------------------------------------------

/**
 * asofMenu is the toolbar's as-of menu (docs/13 §4): "Now", then each mirror's snapshots
 * newest first, an offline mirror's greyed with the reason; choosing one opens the same
 * place as of it.
 */
export function asofMenu(ctx: Ctx, view: "browse" | "file", current?: SnapRef): HTMLElement {
  const body = h("div", { class: "menu-body", role: "radiogroup", "aria-label": "as of" });
  const details = h("details", { class: "menu asof" }, h("summary", null, current ? current.name : "Now"), body);
  const go = (at?: number) => {
    const next: Ctx = { path: ctx.path, root: ctx.root };
    if (at !== undefined) next.at = at;
    location.hash = view === "browse" ? browseHash(next) : fileHash(next);
  };
  const row = (label: Node | string, note: string, checked: boolean, disabled: string | null, onPick: () => void) => {
    const radio = h("input", { type: "radio", name: "asof", checked, disabled: !!disabled }) as HTMLInputElement;
    const lab = h("label", { class: "menu-row" }, radio, h("span", null, label), h("span", { class: "small dim" }, disabled ? "offline: attach it to browse" : note));
    if (disabled) {
      lab.setAttribute("aria-disabled", "true");
      lab.title = disabled;
    }
    radio.addEventListener("change", () => {
      if (radio.checked) onPick();
    });
    return lab;
  };
  const fill = async () => {
    clear(body);
    body.append(row("Now", "the live volume", !current, null, () => go()));
    try {
      const c = await catalogue();
      clear(body);
      body.append(row("Now", "the live volume", !current, null, () => go()));
      let n = 0;
      for (const m of c.mirrors) {
        const mine = c.snapshots.filter((s) => s.mirror === m.name && s.state !== "syncing");
        if (!mine.length) continue;
        body.append(h("div", { class: "menu-group" }, h("span", { class: "dot " + (m.online ? "ok" : "off") }), `${m.name} · ${m.online ? "online" : offlineFor(m)}`));
        for (const s of mine) {
          n++;
          const label = h("span", null, s.name);
          if (!s.auto) label.append(h("span", { class: "badge" }, "manual"));
          body.append(row(label, taken(s, c), current?.id === s.id, m.online ? null : `${m.name} is offline: attach it to browse`, () => go(s.id)));
        }
      }
      const mirrors = new Set(c.snapshots.map((s) => s.mirror)).size;
      body.append(h("div", { class: "menu-foot small dim" }, n ? `${n} snapshots on ${mirrors} ${mirrors === 1 ? "mirror" : "mirrors"} · ` : "no snapshot yet · ", h("a", { href: "#/" }, "manage on the dashboard")));
    } catch (e) {
      body.append(h("div", { class: "menu-foot small dim" }, "snapshots: " + errText(e)));
    }
  };
  details.addEventListener("toggle", () => {
    if (details.open) void fill();
  });
  return details;
}

// --- the banner of a view as of a snapshot ---------------------------------------------------

/**
 * banner says what the view is read as of (docs/13 §4): the snapshot and its mirror, when
 * it was taken, read-only; a folder offers "Restore folder to…"; every view "Back to now".
 */
export function banner(ctx: Ctx, ref: SnapRef | undefined, summary: string, folder: boolean, back: string): HTMLElement {
  const name = ref?.name ?? (ctx.at !== undefined ? `#${ctx.at}` : "");
  const text = h("span", null, "As of ", h("span", { class: "mono" }, name));
  if (ref) text.append(" on ", h("span", { class: "mono" }, ref.mirror), ` · taken ${ago(ref.at)}`);
  text.append(" · read-only" + (summary ? " · " + summary : ""));
  const actions = h("span", { class: "actions" });
  if (folder && ref) actions.append(h("button", { type: "button", onClick: () => void restoreFolder(ctx, ref) }, "Restore folder to…"));
  actions.append(h("button", { type: "button", onClick: () => (location.hash = back) }, "Back to now"));
  return h("div", { class: "banner snapshot" }, text, actions);
}

/** offlineView is the banner and the reason for a snapshot whose mirror is offline. */
export function offlineView(ctx: Ctx, e: unknown, back: string): HTMLElement[] {
  const c = last();
  const sn = c?.snapshots.find((s) => s.id === ctx.at);
  const m = c?.mirrors.find((x) => x.name === sn?.mirror);
  const text = h("span", null, "As of ", h("span", { class: "mono" }, sn?.name ?? `#${ctx.at}`));
  if (sn) text.append(" on ", h("span", { class: "mono" }, sn.mirror), ` · taken ${ago(sn.at)}`);
  const why = m
    ? h("div", { class: "empty" }, h("span", { class: "mono" }, m.name), ` is offline${m.last_seen ? ` (last seen ${ago(m.last_seen)} on ${m.host})` : ""}: attach it to browse this snapshot`)
    : h("div", { class: "empty" }, errText(e));
  return [h("div", { class: "banner snapshot" }, text, h("span", { class: "actions" }, h("button", { type: "button", onClick: () => (location.hash = back) }, "Back to now"))), why];
}

/** isOffline re-exports the 409-offline test for the views. */
export const isOffline = offline;

/**
 * restoreFolder starts a restore job of the folder on the mirror's host (docs/16 §6): the
 * answer comes once the job is recorded, and the toast links the dashboard's Restores
 * block, where the job's progress is (docs/13 §4, I-369).
 */
async function restoreFolder(ctx: Ctx, ref: SnapRef): Promise<void> {
  const to = await pick({ start: dirname(ctx.path) || "/", disabled: () => null, action: (p) => `Restore here as ${basename(ctx.path) || ref.name}` + (p === "/" ? "" : "") });
  if (to === null) return;
  try {
    const r = await restoreTree(ref.id, ctx.path, { to });
    const follow = h("a", { href: "#/", onClick: () => (scrollToRestores = true) }, "follow it on the dashboard");
    toast(h("span", null, `restoring ${basename(r.path) || r.path} from ${ref.name} on ${r.host} · `, follow), "ok");
    void status.refresh();
  } catch (e) {
    toast("restore: " + errText(e), "bad");
  }
}

/** scrollToRestores is set by the toast's link: the dashboard scrolls to its Restores block once drawn. */
let scrollToRestores = false;

// --- a file's Versions panel ------------------------------------------------------------------

/** range folds the snapshots holding a version into "first – last". */
function range(v: Version): string {
  const names = v.snapshots.map((s) => s.name);
  if (names.length <= 1) return names[0] ?? "";
  return `${names[names.length - 1]} – ${names[0]}`;
}

/** holders says how many snapshots hold a version and on which mirrors. */
function holders(v: Version): string {
  const mirrors = [...new Set(v.snapshots.map((s) => s.mirror))].join(", ");
  const ends = new Set([v.snapshots[0]?.name, v.snapshots[v.snapshots.length - 1]?.name]);
  const manual = v.snapshots.filter((s) => !s.auto && !ends.has(s.name)).map((s) => s.name).slice(0, 2); // the range already names its ends
  const n = v.snapshots.length;
  return [`${n} ${n === 1 ? "snapshot" : "snapshots"}`, ...manual, mirrors].filter(Boolean).join(" · ");
}

/** copyName is "<stem> (<snapshot>)<ext>" beside the file. */
function copyName(name: string, snap: string): string {
  const e = ext(name);
  const stem = e ? name.slice(0, name.length - e.length - 1) : name;
  return `${stem} (${snap})${e ? "." + e : ""}`;
}

/**
 * versionsPanel lists a live file's distinct contents across the online mirrors (docs/13
 * §4), loaded when it is opened: the current row first with what holds it, then each
 * older one with View, Revert and Save a copy; Revert asks first when the current content
 * is in no snapshot.
 */
export function versionsPanel(path: string, onChange: () => void): HTMLElement {
  const summary = h("summary", null, "Versions");
  const body = h("div", { class: "body" });
  const panel = h("details", { class: "panel" }, summary, body);
  let loaded = false;
  const load = async () => {
    clear(body);
    body.append(h("div", { class: "small dim" }, "loading…"));
    try {
      const r = await versions(path);
      loaded = true;
      draw(r.versions, r.offline);
    } catch (e) {
      clear(body);
      body.append(h("div", { class: "error" }, "versions: " + errText(e)));
    }
  };
  const draw = (vs: Version[], off: { mirror: string; count: number }[]) => {
    clear(body);
    const held = vs.reduce((n, v) => n + v.snapshots.length, 0);
    const mirrors = [...new Set(vs.flatMap((v) => v.snapshots.map((s) => s.mirror)))];
    summary.textContent = `Versions (${vs.length} ${vs.length === 1 ? "content" : "contents"} · ${held} snapshots${mirrors.length ? " on " + mirrors.join(", ") : ""})`;
    const current = vs.find((v) => v.current);
    const tbody = h("tbody");
    for (const v of vs) {
      const asof = h("td", { class: "asof" }, v.current ? "now" : range(v), h("span", { class: "small dim" }, v.current ? (v.snapshots.length ? "also in " + holders(v) : "in no snapshot yet") : holders(v)));
      const acts = h("td", { class: "acts" });
      const first = v.snapshots[0];
      if (v.current) acts.append(h("span", { class: "badge strong" }, "current"));
      else if (first) {
        acts.append(
          h("button", { type: "button", onClick: () => (location.hash = fileHash({ path, at: first.id })) }, "View"),
          h("button", { type: "button", onClick: () => askRevert(first, !!current && current.snapshots.length === 0) }, "Revert"),
          h("button", { type: "button", onClick: () => void saveCopy(first) }, "Save a copy"),
        );
      }
      tbody.append(h("tr", { class: v.current ? "current" : "" }, asof, h("td", { class: "size" }, bytes(v.size)), h("td", { class: "mtime", title: v.mtime }, when(v.mtime)), acts));
    }
    body.append(h("table", { class: "versions" }, h("thead", null, h("tr", null, h("th", null, "as of"), h("th", { class: "size" }, "size"), h("th", { class: "mtime" }, "modified"), h("th", { class: "acts" }))), tbody));
    if (!vs.some((v) => !v.current)) body.append(h("div", { class: "small dim" }, "no snapshot holds another version of this file"));
    for (const o of off) body.append(h("div", { class: "small dim" }, `${o.count} more ${o.count === 1 ? "snapshot" : "snapshots"} on `, h("span", { class: "mono" }, o.mirror), ", offline: attach it to see them"));
  };
  const doRevert = async (s: { id: number; name: string; mirror: string }) => {
    try {
      const r = await revert(path, s.id);
      toast(`reverted to ${s.name}` + (r.uploaded ? ` · ${r.uploaded} chunks copied back from ${s.mirror}` : ""), "ok");
      onChange();
      void load();
    } catch (e) {
      toast("revert: " + errText(e), "bad");
    }
  };
  const askRevert = (s: { id: number; name: string; mirror: string }, unsaved: boolean) => {
    body.querySelector(".banner")?.remove();
    if (!unsaved) {
      void doRevert(s);
      return;
    }
    const confirm = h(
      "div",
      { class: "banner" },
      h("span", null, "The current content is in no snapshot yet: reverting replaces it for good."),
      h(
        "span",
        { class: "actions" },
        h("button", { type: "button", onClick: () => void takeSnapshot().then(() => toast("snapshot started; revert once it is complete", "ok"), (e: unknown) => toast("snapshot: " + errText(e), "bad")).finally(() => confirm.remove()) }, "Take a snapshot first"),
        h("button", { type: "button", onClick: () => { confirm.remove(); void doRevert(s); } }, "Revert anyway"),
        h("button", { type: "button", onClick: () => confirm.remove() }, "Cancel"),
      ),
    );
    body.append(confirm);
  };
  const saveCopy = async (s: { id: number; name: string; mirror: string }) => {
    const name = copyName(basename(path), s.name);
    try {
      const r = await copyVersion(path, s.id, name);
      toast(`saved a copy as ${name}` + (r.uploaded ? ` · ${r.uploaded} chunks copied back from ${s.mirror}` : ""), "ok");
    } catch (e) {
      toast("save a copy: " + (e instanceof ApiError && e.status === 409 ? `${name} exists` : errText(e)), "bad");
    }
  };
  panel.addEventListener("toggle", () => {
    if (panel.open && !loaded) void load();
  });
  return panel;
}

// --- the dashboard -----------------------------------------------------------------------------

/** tile is the Snapshots tile under Volume, or null without mirrors. */
export function tile(c: Snapshots | null): HTMLElement | null {
  if (!c || !c.mirrors.length) return null;
  const off = c.mirrors.filter((m) => !m.online).length;
  const n = c.snapshots.filter((s) => s.state !== "syncing").length;
  return h(
    "div",
    { class: "tile" },
    h("div", { class: "label" }, "Snapshots"),
    h("div", { class: "value" }, String(n)),
    h("div", { class: "sub" }, `on ${c.mirrors.length} ${c.mirrors.length === 1 ? "mirror" : "mirrors"}` + (off ? ` · ${off} offline` : "")),
  );
}

const WARN: Record<NonNullable<Mirror["warn"]>, string> = { stale: "stale", reserve: "nearly full", incomplete: "incomplete" };

function card(m: Mirror, c: Snapshots): HTMLElement {
  const mine = c.snapshots.filter((s) => s.mirror === m.name && s.state !== "syncing");
  const badges = h("span", { class: "badges" }, h("span", { class: "badge" }, "on " + m.host));
  if (m.self) badges.append(h("span", { class: "badge self" }, "self"));
  if (m.warn) badges.append(h("span", { class: "badge warn" }, WARN[m.warn]));
  const el = h("div", { class: "card mirror" + (m.online ? "" : " off") + (m.warn && m.online ? " warn" : "") }, h("div", { class: "name-row" }, h("span", { class: "dot " + (m.online ? "ok" : "off") }), h("span", { class: "name" }, m.name), badges));
  const disk = m.used + m.free;
  if (m.online) {
    const frac = m.total > 0 ? (m.total - m.free) / m.total : disk > 0 ? m.used / disk : 0;
    const val = `${bytes(m.used)} used of ${bytes(m.total || disk)} · ${mine.length} ${mine.length === 1 ? "snapshot" : "snapshots"}` + (m.warn === "reserve" ? " · under its reserve: the oldest automatic snapshots go first" : "");
    el.append(barRow("disk", bar(frac, m.warn === "reserve" ? "warn" : fillTone(frac)), val));
  } else {
    el.append(barRow("disk", bar(0, "off"), `${offlineFor(m)} · ${mine.length} snapshots` + (m.used ? ` · ${bytes(m.used)} when last seen` : "")));
  }
  if (m.syncing) {
    const s = m.syncing;
    const frac = s.total > 0 ? s.read / s.total : 0;
    el.append(barRow("syncing", bar(frac), `${s.snapshot} · ${Math.floor(frac * 100)} % read · ${bytes(s.bytes_new)} new`));
  }
  const meta = h("div", { class: "meta" });
  const newest = mine[0];
  const first = mine[mine.length - 1];
  if (newest) meta.append(h("span", null, "newest " + when(newest.at)));
  if (first && first !== newest) meta.append(h("span", null, "first " + shortDate(first.at)));
  if (!newest) meta.append(h("span", null, m.syncing ? "the first snapshot copies the whole volume" : "no snapshot yet"));
  const incomplete = mine.filter((s) => s.state === "incomplete");
  if (incomplete.length) meta.append(h("span", null, `${incomplete.length} incomplete: ${incomplete.reduce((n, s) => n + s.missing, 0)} chunks unreadable`));
  if (!m.online) meta.append(h("span", null, m.reason ? `${m.reason} · attach it to sync and browse` : "attach it to sync and browse"));
  if (m.path) meta.append(h("span", null, m.path));
  el.append(meta);
  return el;
}

function schedule(c: Snapshots): string {
  const s = c.schedule;
  if (!s.every) return "no automatic snapshots (mirrors.every 0)";
  const parts = [`hourly ${s.hourly}`, `daily ${s.daily}`, `weekly ${s.weekly}`, `monthly ${s.monthly}`];
  if (s.next) {
    const mins = Math.max(0, Math.round((Date.parse(s.next) - Date.now()) / 60_000));
    parts.push(mins < 1 ? "next now" : mins < 120 ? `next in ${mins} m` : `next in ${Math.round(mins / 60)} h`);
  }
  return parts.join(" · ");
}

/**
 * section is the dashboard's Mirrors section (docs/13 §7): one card per mirror, "Take a
 * snapshot" (a manual one is permanent, and the dialog says so), and every snapshot newest
 * first with Browse; there is no Delete (I-183).
 */
export function section(c: Snapshots | null, onTaken: () => void, restores: RestoreJob[] = []): HTMLElement | null {
  if (!c || !c.mirrors.length) return null;
  const sec = h("section", null, h("h2", null, "Mirrors", h("span", { class: "h-sub" }, schedule(c))));
  sec.append(h("div", { class: "cards" }, ...c.mirrors.map((m) => card(m, c))));
  if (restores.length) {
    const head = h("h3", { id: "restores" }, "Restores");
    sec.append(head, h("div", { class: "cards" }, ...restores.map((j) => restoreCard(j, restores))));
    if (scrollToRestores) {
      scrollToRestores = false;
      requestAnimationFrame(() => head.scrollIntoView({ block: "start" }));
    }
  }
  const manual = c.snapshots.filter((s) => !s.auto).length;
  const online = c.mirrors.filter((m) => m.online).length;
  const take = h("button", { type: "button", disabled: online === 0, title: online ? "" : "no mirror is online" }, "Take a snapshot") as HTMLButtonElement;
  take.addEventListener("click", () => void takeDialog(onTaken));
  sec.append(h("div", { class: "snap-head" }, take, h("span", { class: "small dim" }, `every online mirror · ${c.snapshots.length} snapshots · ${manual} manual`)));
  const byName = new Map(c.mirrors.map((m) => [m.name, m]));
  const tbody = h("tbody");
  const pages = Math.max(1, Math.ceil(c.snapshots.length / PAGE));
  page = Math.min(page, pages - 1);
  const shown = c.snapshots.slice(page * PAGE, (page + 1) * PAGE);
  for (const s of shown) {
    const m = byName.get(s.mirror);
    const on = !!m?.online;
    const name = h("td", { class: "name" }, s.name);
    if (!s.auto) name.append(h("span", { class: "badge" }, "manual"));
    const sync = m?.syncing && m.syncing.id === s.id ? m.syncing : undefined;
    if (s.state === "syncing") name.append(h("span", { class: "badge" }, sync && sync.total ? `syncing ${Math.floor((sync.read / sync.total) * 100)} %` : "syncing"));
    if (s.state === "incomplete") name.append(h("span", { class: "badge warn" }, `${s.missing} missing`));
    const why = s.state === "syncing" ? "still syncing" : on ? "" : `${s.mirror} is offline: attach it to browse`;
    const browse = h("button", { type: "button", disabled: !!why, title: why, onClick: () => (location.hash = browseHash({ path: "/", at: s.id })) }, "Browse");
    const added = s.state === "syncing" ? (sync ? `${bytes(sync.bytes_new)} so far` : "") : bytes(s.new_bytes);
    tbody.append(
      h("tr", { class: on ? "" : "offline" }, name, h("td", { class: "mirror" }, s.mirror), h("td", { class: "taken", title: s.at }, taken(s, c)), h("td", { class: "holds" }, added), h("td", { class: "acts" }, browse)),
    );
  }
  if (c.snapshots.length) {
    sec.append(h("table", { class: "snapshots" }, h("thead", null, h("tr", null, h("th", null, "name"), h("th", { class: "mirror" }, "mirror"), h("th", { class: "taken" }, "taken"), h("th", { class: "holds" }, "added"), h("th", { class: "acts" }))), tbody));
  } else sec.append(h("div", { class: "empty" }, "no snapshot yet: the first one copies the whole volume"));
  if (c.snapshots.length > PAGE) {
    const go = (p: number) => () => {
      page = p;
      onTaken();
    };
    const first = page * PAGE + 1;
    const lastRow = Math.min((page + 1) * PAGE, c.snapshots.length);
    sec.append(
      h(
        "div",
        { class: "pager" },
        h("button", { type: "button", disabled: page === 0, onClick: go(page - 1) }, "Newer"),
        h("span", { class: "small dim" }, `${first}–${lastRow} of ${c.snapshots.length}`),
        h("button", { type: "button", disabled: page >= pages - 1, onClick: go(page + 1) }, "Older"),
      ),
    );
  }
  sec.append(h("div", { class: "small dim" }, "a snapshot copies only what changed since the last one on its mirror; “added” is what it wrote"));
  return sec;
}

/** PAGE is how many snapshots the table shows a page; page is this page load's, newest first (I-369). */
const PAGE = 20;
let page = 0;

// --- the dashboard's Restores block (docs/13 §7, docs/16 §6, I-369) ----------------------------

const UNFINISHED = new Set<RestoreJob["state"]>(["counting", "queued", "running", "waiting", "retrying"]);

/** iso is a job's Unix-nanosecond time as the formatters take it. */
function iso(ns: number | undefined): string | undefined {
  return ns ? new Date(ns / 1e6).toISOString() : undefined;
}

/** span says how long secs is: "45 s", "4 m", "3 h 35 m". */
function span(secs: number): string {
  const s = Math.max(0, Math.round(secs));
  if (s < 60) return `${s} s`;
  const m = Math.round(s / 60);
  if (m < 60) return `${m} m`;
  return `${Math.floor(m / 60)} h ${m % 60} m`;
}

/** mirrorDir is a mirror row's directory under /.snapshots: pike/m1 is pike-m1. */
function mirrorDir(row: string): string {
  return row.replace(/\//g, "-");
}

/** restoreCard is one restore job, as the lookdev draws it: the destination and its state, a bar of bytes, a bar of files while it copies, where it reads from and who started it, Cancel while it is unfinished. */
function restoreCard(j: RestoreJob, all: RestoreJob[]): HTMLElement {
  const warn = j.state === "queued" || j.state === "waiting" || j.state === "retrying";
  const finished = j.state === "done" || j.state === "cancelled";
  const tone = warn ? "warn" : j.state === "failed" ? "bad" : "";
  const dot = j.state === "cancelled" ? "off" : tone || "ok";
  const badges = h("span", { class: "badges" }, h("span", { class: "badge" }, "on " + j.host), h("span", { class: "badge" + (tone ? " " + tone : "") }, j.state));
  const el = h("div", { class: "card restore" + (tone ? " " + tone : "") + (finished ? " done" : "") }, h("div", { class: "name-row" }, h("span", { class: "dot " + dot }), h("span", { class: "name" }, j.dst), badges));
  const frac = j.bytes_total > 0 ? j.bytes_done / j.bytes_total : 0;
  const so = `${bytes(j.bytes_done)} of ${bytes(j.bytes_total)} · ${Math.floor(frac * 100)} %`;
  const empty = () => h("div", { class: "bar" });
  switch (j.state) {
    case "counting":
      el.append(barRow("bytes", empty(), "counting what to restore" + (j.files_total ? ` · ${j.files_total.toLocaleString("en")} files so far` : "")));
      break;
    case "queued": {
      const ahead = all.find((o) => o !== j && o.host === j.host && o.state !== "queued" && UNFINISHED.has(o.state));
      el.append(barRow("bytes", empty(), ahead ? `after the restore of ${ahead.dst} on ${j.host}` : `waiting its turn on ${j.host}`));
      break;
    }
    case "running": {
      const left = j.rate > 0 ? ` · ${span((j.bytes_total - j.bytes_done) / j.rate)} left` : "";
      el.append(barRow("bytes", bar(frac), `${so} · ${bytes(j.rate)}/s${left}`));
      break;
    }
    case "waiting":
      el.append(barRow("bytes", bar(frac, "warn"), `${so} · ${mirrorDir(j.mirror)} is offline: attach it and the restore goes on`));
      break;
    case "retrying": {
      const next = j.next_try ? ` · again in ${span(j.next_try / 1e9 - Date.now() / 1000)}` : "";
      el.append(barRow("bytes", bar(frac, "warn"), `${so}${next}${j.error ? ": " + j.error : ""}`));
      break;
    }
    case "failed":
      el.append(barRow("bytes", bar(frac, "bad"), `${bytes(j.bytes_done)} of ${bytes(j.bytes_total)} · ${j.error || "failed"}: a rerun goes on from here`));
      break;
    case "cancelled": {
      const fill = h("i");
      setWidth(fill, frac);
      const off = h("div", { class: "bar off" }, fill); // hatched, with what was restored
      el.append(barRow("bytes", off, `${bytes(j.bytes_done)} of ${bytes(j.bytes_total)} · what was restored stays`));
      break;
    }
    case "done":
      el.append(barRow("bytes", bar(1), `${bytes(j.bytes_total)} · ${j.files_total.toLocaleString("en")} files` + (j.existing ? ` · ${j.existing.toLocaleString("en")} already there` : "")));
      break;
  }
  if (j.state === "running" || j.state === "waiting") {
    const ff = j.files_total > 0 ? j.files_done / j.files_total : 0;
    el.append(barRow("files", bar(ff, j.state === "waiting" ? "warn" : ""), `${j.files_done.toLocaleString("en")} of ${j.files_total.toLocaleString("en")}` + (j.existing ? ` · ${j.existing.toLocaleString("en")} already there` : "")));
  }
  const meta = h("div", { class: "meta" }, h("span", null, `from ${mirrorDir(j.mirror)} · ${j.snapshot}`));
  if (j.user) meta.append(h("span", null, "by " + j.user));
  if (j.state === "done") meta.append(h("span", null, `finished ${when(iso(j.ended))} · took ${span(((j.ended ?? j.updated) - j.started) / 1e9)}`));
  else if (j.state === "failed") meta.append(h("span", null, "failed " + when(iso(j.ended ?? j.updated))));
  else if (j.state === "cancelled") meta.append(h("span", null, "cancelled " + when(iso(j.ended ?? j.updated))));
  else meta.append(h("span", null, `started ${when(iso(j.started))}` + (j.resumed ? ` · resumed after a restart${j.resumed > 1 ? ` ${j.resumed}×` : ""}` : "") + (j.state === "retrying" && j.tries ? ` · ${j.tries} ${j.tries === 1 ? "try" : "tries"}` : "")));
  el.append(meta);
  const se = account.current();
  if (UNFINISHED.has(j.state) && (!se || se.admin || se.user === j.user)) {
    const btn = h("button", { type: "button" }, "Cancel restore") as HTMLButtonElement;
    btn.addEventListener("click", () => {
      btn.disabled = true;
      cancelRestore(j.id).then(
        () => {
          toast(`cancelled the restore of ${j.dst}: what was restored stays`, "ok");
          void status.refresh();
        },
        (e) => {
          btn.disabled = false;
          toast("cancel: " + errText(e), "bad");
        },
      );
    });
    el.append(h("div", { class: "card-acts" }, btn));
  }
  return el;
}

/** takeDialog asks for an optional name and says a manual snapshot is permanent. */
function takeDialog(onTaken: () => void): Promise<void> {
  return new Promise((resolve) => {
    const dlg = h("dialog", { class: "picker", "aria-label": "take a snapshot" });
    const name = h("input", { type: "text", placeholder: "name (optional: the UTC minute)", "aria-label": "snapshot name", maxlength: 64 }) as HTMLInputElement;
    const go = h("button", { type: "submit" }, "Take a snapshot");
    const cancel = h("button", { type: "button" }, "Cancel");
    const form = h("form", { class: "inline-form" }, name);
    dlg.append(
      h("div", { class: "small dim" }, "A manual snapshot is permanent: the schedule never thins it and no page removes it. This device's unsaved writes land first."),
      form,
      h("div", { class: "picker-actions" }, cancel, go),
    );
    const close = () => {
      dlg.close();
      dlg.remove();
      resolve();
    };
    const submit = (ev?: Event) => {
      ev?.preventDefault();
      const n = name.value.trim();
      takeSnapshot(n ? { name: n } : {}).then(
        (rows) => {
          toast(`snapshot ${rows[0]?.name ?? n} started on ${rows.map((r) => r.mirror).join(", ")}`, "ok");
          close();
          void catalogue(true).then(onTaken, onTaken);
        },
        (e: unknown) => toast("snapshot: " + (offline(e) ? "no mirror is online" : errText(e)), "bad"),
      );
    };
    form.addEventListener("submit", submit);
    go.addEventListener("click", () => submit());
    cancel.addEventListener("click", close);
    dlg.addEventListener("cancel", (ev) => {
      ev.preventDefault();
      close();
    });
    document.body.append(dlg);
    dlg.showModal();
    queueMicrotask(() => name.focus());
  });
}

