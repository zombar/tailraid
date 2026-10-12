// Dashboard: the volume summary (with the trash), Disks (when a host has added disks),
// Balance, Integrity and this node's cache card. Re-rendered on every poll.

import { clear, h } from "./dom.ts";
import { bar, barRow, fillTone } from "./chart.ts";
import { bytes, count } from "./fmt.ts";
import { status } from "./status.ts";
import * as balance from "./balance.ts";
import * as disks from "./disks.ts";
import * as integrity from "./integrity.ts";
import type { Report } from "./api.ts";

export function render(root: HTMLElement): () => void {
  const draw = () => {
    clear(root);
    const r = status.current;
    if (!r) {
      root.append(h("div", { class: status.error ? "error" : "empty" }, status.error ? "Could not load the status: " + status.error : "loading…"));
      return;
    }
    volume(root, r);
    disks.render(root, r);
    balance.render(root, r);
    integrity.render(root, r, status.history);
    cache(root, r);
  };
  const unsub = status.subscribe(draw);
  draw();
  return unsub;
}

function volume(root: HTMLElement, r: Report): void {
  const v = r.volume;
  const tiles = h(
    "div",
    { class: "tiles" },
    tile("Policy", `${v.k}+${v.m}`, `${v.auto ? "auto" : "manual"} · ${bytes(v.chunk_size)} chunks · policy v${v.policy_version}`),
    tile("Files", count(v.inodes), `${count(v.chunks)} chunks · ${count(v.tombstoned)} tombstoned`),
    tile("Dedup", v.dedup_ratio.toFixed(2) + "×", bytes(v.logical_bytes) + " logical"),
    tile("Trash", count(v.trash_inodes ?? 0), v.trash_inodes ? `${bytes(v.trash_bytes ?? 0)} · restore from its folder` : "nothing deleted"),
  );
  const s = r.search;
  if (s && (s.docs > 0 || s.pending > 0 || s.index)) {
    let sub = s.index ? "index unavailable on this node" : `${bytes(s.bytes)} in ${count(s.segments)} ${s.segments === 1 ? "segment" : "segments"}`;
    if (s.pending > 0) sub += ` · ${count(s.pending)} pending`;
    if (s.capped) sub += " · capped: contents no longer indexed";
    tiles.append(tile("Index", count(s.docs), sub)); // docs/18 §5: files whose contents or tags are searchable
  }
  const scale = Math.max(1, v.logical_bytes, v.stored_bytes);
  const used = Math.max(0, v.usable_capacity - v.free);
  const usedFrac = v.usable_capacity > 0 ? used / v.usable_capacity : 0;
  const compare = h(
    "div",
    { class: "compare" },
    barRow("logical", bar(v.logical_bytes / scale, "", undefined, true), bytes(v.logical_bytes)),
    barRow("stored", bar(v.stored_bytes / scale, "", undefined, true), bytes(v.stored_bytes) + " with parity"),
    barRow("usable", bar(usedFrac, fillTone(usedFrac), undefined, true), `${bytes(used)} used of ${bytes(v.usable_capacity)} · ${bytes(v.free)} free`),
  );
  root.append(h("section", null, h("h2", null, "Volume"), tiles, compare));
}

function tile(label: string, value: string, sub: string): HTMLElement {
  return h("div", { class: "tile" }, h("div", { class: "label" }, label), h("div", { class: "value" }, value), h("div", { class: "sub" }, sub));
}

/** cache is this node's chunk cache and the folders it keeps (docs/13 §4, I-162). */
function cache(root: HTMLElement, r: Report): void {
  const c = r.cache;
  if (!c) return;
  const self = r.nodes.find((n) => n.self)?.name ?? r.node;
  const low = !!r.machine?.disk_total && (r.machine.disk_free ?? 0) * 5 < r.machine.disk_total;
  const usedFrac = c.size > 0 ? Math.min(1, (c.used - c.pinned_bytes) / c.size) : 0;
  const card = h(
    "div",
    { class: "card cache" + (low ? " warn" : "") },
    h("div", { class: "name-row" }, h("span", { class: "name" }, self), h("span", { class: "badges" }, h("span", { class: "badge self" }, "self"))),
    barRow("used", bar(usedFrac, fillTone(usedFrac)), `${bytes(Math.max(0, c.used - c.pinned_bytes))} of ${bytes(c.size)}`),
    barRow("pinned", bar(c.pinned_bytes > 0 ? 1 : 0, low ? "warn" : ""), `${bytes(c.pinned_bytes)} kept here` + (r.machine?.disk_free ? ` · ${bytes(r.machine.disk_free)} free on the data disk` : "")),
  );
  if (c.pins.length) {
    const pins = h("div", { class: "pins" });
    for (const p of c.pins) {
      const frac = p.bytes > 0 ? p.cached_bytes / p.bytes : 1;
      pins.append(h("div", { class: "pin-row" }, h("span", { class: "n", title: p.path }, p.path), bar(frac), h("span", { class: "v" }, `${bytes(p.cached_bytes)} of ${bytes(p.bytes)}` + (frac < 1 ? " · fetching" : ""))));
    }
    card.append(pins);
  } else card.append(h("div", { class: "small dim" }, "no folder is kept on this device · the folder menu has “Keep on this device”"));
  root.append(h("section", null, h("h2", null, "Cache"), h("div", { class: "cards" }, card)));
}
