// Balance: one card per node, capacity used (capacity − free) and slots held against the
// capacity-weighted expected share (rendezvous placement, docs/03). A device's card
// totals its added disks; the Disks block shows them one by one (docs/14 §8).

import { h } from "./dom.ts";
import { bar, barRow, fillTone, type Tone } from "./chart.ts";
import { bytes, count, shortDate, signedPct } from "./fmt.ts";
import type { NodeReport, Report } from "./api.ts";

interface Share {
  expected: number;
  deviation: number; // NaN when no expectation (not active)
}

/**
 * devTone is the balance rule: neutral within the repairer's RebalanceSkew (25 %) of the
 * expected share, amber past it (a pass will move slots), red past 50 %.
 */
export function devTone(dev: number): Tone {
  return Number.isNaN(dev) ? "" : Math.abs(dev) > 0.5 ? "bad" : Math.abs(dev) > 0.25 ? "warn" : "";
}

/**
 * shares computes each node's expected slot count: the total slots spread over the active
 * nodes by capacity, except that a node never expects more than maxSlots(n) — one slot per
 * chunk for a single target (I-9) — and what a capped node cannot take is spread over the
 * rest by capacity (water-filling). Without a cap it is total × capacity / Σ capacity.
 */
export function shares(nodes: NodeReport[], maxSlots: (n: NodeReport) => number = () => Infinity): Map<number, Share> {
  const total = nodes.reduce((s, n) => s + n.slots, 0);
  const expected = new Map<number, number>();
  let open = nodes.filter((n) => n.state === "active" && n.capacity > 0);
  let left = total;
  while (open.length > 0) {
    const sumCap = open.reduce((s, n) => s + n.capacity, 0);
    const capped = open.filter((n) => (left * n.capacity) / sumCap > maxSlots(n));
    if (capped.length === 0) {
      for (const n of open) expected.set(n.idx, (left * n.capacity) / sumCap);
      break;
    }
    for (const n of capped) {
      expected.set(n.idx, maxSlots(n));
      left -= maxSlots(n);
    }
    open = open.filter((n) => !capped.includes(n));
  }
  const out = new Map<number, Share>();
  for (const n of nodes) {
    const e = expected.get(n.idx) ?? 0;
    out.set(n.idx, { expected: e, deviation: e > 0 ? (n.slots - e) / e : NaN });
  }
  return out;
}

/**
 * chunksPerTarget is the most slots one target can hold: one per live chunk (I-9). The
 * report's `chunks` already leaves the tombstoned ones out, as the slot counts do.
 */
export function chunksPerTarget(r: Report): number {
  return r.volume.chunks;
}

/**
 * withDisks folds every disk into its host: capacity, free, slots and missing slots add up.
 * A removed member keeps its row in the table for good (its index is never reused) and is
 * not shown, like `status` and `node ls` (I-132).
 */
export function withDisks(nodes: NodeReport[]): NodeReport[] {
  return nodes
    .filter((n) => n.kind !== "disk" && n.kind !== "mirror" && n.state !== "removed") // a mirror holds snapshots, never slots (docs/16)
    .map((n) => {
      const ds = nodes.filter((d) => d.kind === "disk" && d.host === n.idx && d.state !== "removed");
      if (ds.length === 0) return n;
      const sum = (f: (x: NodeReport) => number) => f(n) + ds.reduce((s, d) => s + f(d), 0);
      return { ...n, capacity: sum((x) => x.capacity), free: sum((x) => x.free), slots: sum((x) => x.slots), missing_slots: sum((x) => x.missing_slots) };
    });
}

export function render(root: HTMLElement, r: Report): void {
  const nodes = withDisks(r.nodes);
  const per = chunksPerTarget(r);
  // a device row is never a target (docs/14 §9): a host's expected share is its disks'
  const targetsOf = (n: NodeReport) => (n.kind === "s3" ? 1 : r.nodes.filter((d) => d.kind === "disk" && d.host === n.idx && d.state === "active" && d.capacity > 0).length);
  const sh = shares(nodes, (n) => per * targetsOf(n));
  const scale = Math.max(1, ...nodes.map((n) => Math.max(n.slots, sh.get(n.idx)?.expected ?? 0)));
  const cards = h("div", { class: "cards" });
  for (const n of nodes) cards.append(card(n, sh.get(n.idx) ?? { expected: 0, deviation: NaN }, scale));
  root.append(h("section", null, h("h2", null, "Balance"), cards));
}

function card(n: NodeReport, share: Share, scale: number): HTMLElement {
  const badges = h("span", { class: "badges" });
  if (n.leader) badges.append(h("span", { class: "badge strong" }, "leader"));
  if (n.voter) badges.append(h("span", { class: "badge strong" }, "voter"));
  else if (n.kind !== "s3") badges.append(h("span", { class: "badge" }, "non-voter"));
  if (n.kind === "s3") badges.append(h("span", { class: "badge" }, "s3"));
  if (n.self) badges.append(h("span", { class: "badge self" }, "self"));
  if (n.state === "draining") badges.append(h("span", { class: "badge warn" }, "draining"));
  if (n.lost) badges.append(h("span", { class: "badge bad", title: "dead longer than evict_after: remove it" }, "lost"));

  const dot = h("span", { class: "dot " + (n.live ? "ok" : "bad"), title: n.live ? "live" : "dead for " + (n.dead_for ?? "?") });
  const name = h("div", { class: "name-row" }, dot, h("span", { class: "name" }, n.name), badges);

  let capacity: HTMLElement;
  if (n.s3) {
    // a bucket reports no free space (its probe is a HEAD): what it stores, by the
    // placement table, against its weight (docs/10 §4.1, I-178)
    const frac = n.capacity > 0 ? n.stored_bytes / n.capacity : 0;
    capacity = barRow("stored", bar(frac, fillTone(frac)), `${bytes(n.stored_bytes)} of ${bytes(n.capacity)} weight`);
  } else if (!n.live) {
    capacity = barRow("capacity", bar(0, "off"), "unknown (dead " + (n.dead_for ?? "?") + ")");
  } else if (n.capacity === 0) {
    capacity = barRow("capacity", bar(0), n.kind === "device" ? "no disks" : "no capacity");
  } else {
    const used = Math.max(0, n.capacity - n.free);
    const frac = used / n.capacity;
    capacity = barRow("capacity", bar(frac, fillTone(frac)), `${bytes(used)} of ${bytes(n.capacity)}`);
  }

  let slots: HTMLElement;
  if (n.state !== "active") {
    slots = barRow("slots", bar(n.slots / scale, n.slots > 0 ? "warn" : ""), `${count(n.slots)} slots to move`);
  } else {
    const dev = share.deviation;
    const tone = devTone(dev);
    const val = h("span", null, `${count(n.slots)} · expected ${count(Math.round(share.expected))}`);
    if (!Number.isNaN(dev)) val.append(" · " + signedPct(dev));
    if (n.missing_slots > 0) {
      val.append(" · ");
      val.append(h("span", { class: "text-bad" }, `${count(n.missing_slots)} missing`));
    }
    slots = barRow("slots", bar(n.slots / scale, tone, share.expected / scale), val);
  }

  const meta = h("div", { class: "meta" }, h("span", null, "rtt " + (n.rtt ?? "—")));
  if (n.s3) {
    // a bucket runs no serve: its bucket, prefix and region stand where a device's version,
    // applied index and address go (I-178)
    meta.append(h("span", null, n.s3.prefix ? `${n.s3.bucket}/${n.s3.prefix}` : n.s3.bucket));
    if (n.s3.region) meta.append(h("span", null, n.s3.region));
  } else {
    meta.append(h("span", null, n.version || "?"), h("span", null, "applied " + count(n.applied)), h("span", null, `${n.addr}:${n.rpc_port}`));
  }
  meta.append(h("span", null, "joined " + shortDate(n.joined_at)));
  return h("div", { class: "card node" + (n.self ? " self" : "") }, name, capacity, slots, meta);
}
