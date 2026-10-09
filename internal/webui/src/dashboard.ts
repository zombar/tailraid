// Dashboard: the volume summary, Disks (when a host has added disks), Balance and
// Integrity. Re-rendered on every poll.

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
  );
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
