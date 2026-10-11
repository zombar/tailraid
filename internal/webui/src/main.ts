// tailraid web UI entry (docs/13): the shell (health pill, warnings, tabs) and the hash
// router over the dashboard, the file browser and the file view.

import * as theme from "./theme.ts"; // first: applies the stored theme before any view renders (I-200)
import { clear, h } from "./dom.ts";
import { status } from "./status.ts";
import { decodeHashPath } from "./util.ts";
import * as dashboard from "./dashboard.ts";
import * as browser from "./browser.ts";
import * as player from "./player.ts";
import * as stats from "./stats.ts";
import type { Report } from "./api.ts";

const view = document.getElementById("view") as HTMLElement;
const pill = document.getElementById("health") as HTMLElement;
const who = document.getElementById("who") as HTMLElement;
const warnings = document.getElementById("warnings") as HTMLElement;
const tabs = Array.from(document.querySelectorAll<HTMLAnchorElement>("nav.tabs a"));

// docs/00 "Degraded operation" rows, quoted by the pill's tooltip.
const TIPS = {
  healthy: "leader reachable, live ≥ k+m: metadata, reads and writes normal; full placement",
  degraded: "leader reachable, k ≤ live < k+m: reads OK if k holders live; writes degraded (≥ k shards); 0 slots filled as soon as a spare is live, dead-node slots re-placed after fill_after",
  unreadable: "leader reachable, live < k: EIO for affected chunks; flush fails; WAL buffers up to 1 GiB then EIO",
  stale: "no leader contact for more than 30 s: metadata served from the stale local copy; data writes buffer in the WAL until the caps, then EIO; metadata mutations EROFS",
  "no-leader": "no quorum: metadata served from the stale local copy; data writes buffer in the WAL until the caps, then EIO; metadata mutations EROFS",
  unreachable: "the node did not answer /api/status",
};

interface Pill {
  label: string;
  cls: "ok" | "warn" | "bad";
  title: string;
}

export function healthPill(r: Report | null, error: string | null): Pill {
  if (!r) return { label: "unreachable", cls: "bad", title: TIPS.unreachable + (error ? ": " + error : "") };
  if (r.health === "no-leader") return { label: "no leader", cls: "bad", title: TIPS["no-leader"] };
  if (r.health === "stale") return { label: "stale", cls: "warn", title: TIPS.stale + (r.last_contact ? " · last contact " + r.last_contact : "") };
  const live = r.nodes.filter((n) => n.state === "active" && n.live).length;
  const { k, m } = r.volume;
  if (r.volume.unreadable_chunks > 0 || live < k) return { label: "unreadable", cls: "bad", title: TIPS.unreadable };
  if (r.volume.degraded_chunks > 0 || live < k + m) return { label: "degraded", cls: "warn", title: TIPS.degraded };
  return { label: "healthy", cls: "ok", title: TIPS.healthy };
}

function drawShell(): void {
  const r = status.current;
  const p = healthPill(r, status.error);
  pill.className = "pill " + p.cls;
  pill.textContent = p.label;
  pill.title = p.title;
  clear(who);
  if (r) {
    // the report names nodes by ID; people know them by name
    const self = r.nodes.find((n) => n.self)?.name ?? r.node;
    const leader = r.nodes.find((n) => n.leader)?.name ?? r.leader;
    who.append(`${self} · leader ${leader || "—"} · ${r.live_voters}/${r.voters} voters live`);
  }
  clear(warnings);
  for (const w of r?.warnings ?? []) warnings.append(h("div", { class: "warn-strip" }, w));
}

let dispose: (() => void) | null = null;

function route(): void {
  const hash = location.hash || "#/";
  const [where = "", query = ""] = hash.replace(/^#\/?/, "").split("?");
  const [head = "", ...rest] = where.split("/");
  const tail = rest.join("/");
  dispose?.();
  dispose = null;
  clear(view);
  let tab = "dash";
  if (head === "browse") {
    tab = "files";
    dispose = browser.render(view, decodeHashPath(tail));
  } else if (head === "file") {
    tab = "files";
    dispose = player.render(view, decodeHashPath(tail));
  } else if (head === "stats") {
    tab = "stats";
    dispose = stats.render(view, query);
  } else {
    dispose = dashboard.render(view);
  }
  for (const a of tabs) a.classList.toggle("active", a.dataset.tab === tab);
  window.scrollTo(0, 0);
}

const themeButton = document.querySelector<HTMLButtonElement>("button.theme");
if (themeButton) theme.wire(themeButton);
status.subscribe(drawShell);
window.addEventListener("hashchange", route);
route();
