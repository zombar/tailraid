// Dashboard: the volume summary (with the trash), Disks (every drive: disks, archive
// mirrors and S3 buckets), Balance, Integrity and the mirrors. Re-rendered on every poll.

import { clear, h } from "./dom.ts";
import { bar, barRow, fillTone } from "./chart.ts";
import { bytes, count } from "./fmt.ts";
import { status } from "./status.ts";
import * as balance from "./balance.ts";
import * as disks from "./disks.ts";
import * as integrity from "./integrity.ts";
import * as snaps from "./snapshots.ts";
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
    disks.render(root, r, snaps.last());
    balance.render(root, r);
    integrity.render(root, r, status.history);
    const mirrors = snaps.section(snaps.last(), draw); // docs/13 §7: after Integrity
    if (mirrors) root.append(mirrors);
  };
  // the catalogue is fetched beside the status, at most every few seconds; a fetch that
  // brings it draws again
  let drawn = snaps.last();
  const unsub = status.subscribe(() => {
    draw();
    snaps.catalogue().then(
      (c) => {
        if (c !== drawn) {
          drawn = c;
          draw();
        }
      },
      () => undefined,
    );
  });
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
  const st = snaps.tile(snaps.last()); // docs/16: the count, the mirrors and how many are offline
  if (st) tiles.append(st);
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
