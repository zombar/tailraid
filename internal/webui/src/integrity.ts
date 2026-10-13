// Integrity: the chunk health tiles, the sparkline of this tab's polls, the repairer's
// last pass (leader) or a pointer to the leader (follower), and the gateway's overlay.

import { h } from "./dom.ts";
import { bar, barRow, fillTone, sparkline } from "./chart.ts";
import { ago, bytes, count } from "./fmt.ts";
import type { HomeStatus, Report } from "./api.ts";
import type { Sample } from "./status.ts";

// docs/00 durability contract: the gateway's dirty-byte and WAL caps.
const MAX_DIRTY = 256 * 1024 * 1024;
const WAL_CAP = 128 * 1024 * 1024;

export function render(root: HTMLElement, r: Report, history: Sample[]): void {
  const v = r.volume;
  const tiles = h(
    "div",
    { class: "tiles" },
    tile("Degraded", v.degraded_chunks, `chunks with fewer than ${v.k + v.m} good slots`, "warn"),
    tile("Repair pending", v.repair_pending, "waiting for fill_after", "warn"),
    tile("Unreadable", v.unreadable_chunks, `chunks with fewer than ${v.k} live slots`, "bad"),
    tile("Stale policy", v.stale_policy_chunks, `chunks not yet at ${v.k}+${v.m}`, "warn"),
    tile("Host skew", v.domain_skew, "chunks above the host cap", "warn"),
  );
  const last = history[history.length - 1];
  const lastTone = last && last.degraded + last.pending > 0 ? "warn" : "";
  const spark = h(
    "div",
    null,
    sparkline(
      history.map((s) => s.degraded + s.pending),
      lastTone,
    ),
    h("div", { class: "small dim" }, "degraded + pending, last 10 min (this tab only)"),
  );
  const section = h("section", null, h("h2", null, "Integrity"), tiles, spark, repair(r));
  if (r.gateway) {
    const g = r.gateway;
    section.append(
      h("h3", null, "Gateway"),
      barRow("dirty bytes", bar(g.dirty_bytes / MAX_DIRTY, fillTone(g.dirty_bytes / MAX_DIRTY)), `${bytes(g.dirty_bytes)} in ${g.dirty_inodes} ${g.dirty_inodes === 1 ? "file" : "files"} of ${bytes(MAX_DIRTY)}` + (g.flushing ? " · flushing" : "")),
      barRow("WAL", bar(g.wal_bytes / WAL_CAP, fillTone(g.wal_bytes / WAL_CAP)), `${bytes(g.wal_bytes)} of ${bytes(WAL_CAP)}`),
      h("div", { class: "small dim" }, "acknowledged writes not yet committed to the cluster"),
    );
  }
  root.append(section);
}

function tile(label: string, value: number, sub: string, tone: "warn" | "bad"): HTMLElement {
  const zero = value === 0;
  const lab = h("div", { class: "label" });
  if (!zero) lab.append(h("span", { class: "dot " + tone }));
  lab.append(label);
  return h("div", { class: "tile" + (zero ? " zero" : " " + tone) }, lab, h("div", { class: "value" }, count(value)), h("div", { class: "sub" }, sub));
}

function repair(r: Report): HTMLElement {
  if (!r.repair) {
    const self = r.nodes.find((n) => n.self)?.name ?? r.node;
    const note = h("div", { class: "note" }, `Repair runs on the leader. This node (${self}) is a follower. `);
    const leader = r.nodes.find((n) => n.leader || n.id === r.leader);
    if (!r.leader) note.append("No leader is elected.");
    else if (leader) note.append(h("a", { href: `http://${leader.addr}:${location.port || "20482"}/` }, `Open ${leader.name}'s dashboard`), " for the repairer's counters.");
    return note;
  }
  const s = r.repair.stats;
  const kv = h("dl", { class: "kv" });
  const row = (k: string, v: string, tone?: "warn" | "bad") => kv.append(h("div", null, h("dt", null, k), h("dd", { class: tone }, v)));
  row("scanned", count(s.scanned));
  row("repaired", count(s.repaired));
  row("partial", count(s.partial));
  row("unrepairable", count(s.unrepairable), s.unrepairable > 0 ? "bad" : undefined);
  row("deferred", count(s.deferred));
  row("migrated", count(s.migrated));
  row("moved home", count(s.homed ?? 0));
  row("policy stale", count(s.policy_stale));
  row("scrubbed", count(s.scrubbed));
  row("scrub bad", count(s.scrub_bad), s.scrub_bad > 0 ? "bad" : undefined);
  row("bytes moved", bytes(s.bytes_moved));
  row("errors", count(s.errors), s.errors > 0 ? "bad" : undefined);
  row("reported bad slots", count(r.repair.reported_bad_slots), r.repair.reported_bad_slots > 0 ? "warn" : undefined);
  // the pass under way (docs/12 §8) above the last completed pass's counters
  const p = r.repair.in_progress;
  const away = home(r.repair.home);
  if (!p) return h("div", null, h("h3", null, "Repair · last scan " + ago(r.repair.last_scan)), away, kv);
  const started = new Date(p.started).toLocaleTimeString([], { hour: "2-digit", minute: "2-digit" });
  const progress = barRow(
    "scanned",
    bar(p.total > 0 ? p.scanned / p.total : 0),
    `${count(p.scanned)} of ${count(p.total)} · ${p.in_flight} in flight of ${p.workers} workers · ${bytes(p.bytes_moved)} moved · started ${started}`,
  );
  const note = h("div", { class: "small dim" }, `the pass under way; the counters below are the last completed pass, ${ago(r.repair.last_scan)}`);
  return h("div", null, h("h3", null, `Repair · pass ${p.pass} running`), progress, note, away, kv);
}

// home is the "away from home" row (docs/14 §7): shown while anything is away, the bar the
// movable share against repair.home_start.
function home(st: HomeStatus | null | undefined): HTMLElement | null {
  if (!st || st.away_slots === 0) return null;
  const share = st.stored_bytes > 0 ? st.movable_bytes / st.stored_bytes : 0;
  const pct = (f: number) => `${(f * 100).toFixed(f < 0.1 ? 1 : 0)} %`;
  const gate = st.forced ? "gate open (volume rebalance)" : st.open ? "gate open" : st.start > 0 ? "gate closed" : "gate closed (home_start 0)";
  const of = st.start > 0 ? ` (${pct(share)} of ${pct(st.start)})` : "";
  return barRow(
    "away from home",
    bar(st.open ? 1 : st.start > 0 ? Math.min(share / st.start, 1) : 0),
    `${count(st.away_slots)} slots · ${bytes(st.away_bytes)} · ${bytes(st.movable_bytes)} movable${of} · ${gate}`,
  );
}
