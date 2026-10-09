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

/** chunksPerTarget is the most slots one target can hold: one per live chunk (I-9). */
export function chunksPerTarget(r: Report): number {
  return Math.max(0, r.volume.chunks - r.volume.tombstoned);
}

/** withDisks folds every disk into its host: capacity, free, slots and missing slots add up. */
export function withDisks(nodes: NodeReport[]): NodeReport[] {
  return nodes
    .filter((n) => n.kind !== "disk")
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
  const targetsOf = (n: NodeReport) => 1 + r.nodes.filter((d) => d.kind === "disk" && d.host === n.idx && d.state === "active" && d.capacity > 0).length;
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
  if (n.state === "removed") badges.append(h("span", { class: "badge" }, "removed"));
  if (n.lost) badges.append(h("span", { class: "badge bad", title: "dead longer than evict_after: remove it" }, "lost"));

  const dot = h("span", { class: "dot " + (n.live ? "ok" : "bad"), title: n.live ? "live" : "dead for " + (n.dead_for ?? "?") });
  const name = h("div", { class: "name-row" }, dot, h("span", { class: "name" }, n.name), badges);

  let capacity: HTMLElement;
  if (!n.live) {
    capacity = barRow("capacity", bar(0, "off"), "unknown (dead " + (n.dead_for ?? "?") + ")");
  } else if (n.capacity === 0) {
    capacity = barRow("capacity", bar(0), "no capacity");
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
    const tone: Tone = Number.isNaN(dev) ? "" : Math.abs(dev) > 0.25 ? "bad" : Math.abs(dev) > 0.1 ? "warn" : "";
    const val = h("span", null, `${count(n.slots)} · expected ${count(Math.round(share.expected))}`);
    if (!Number.isNaN(dev)) val.append(" · " + signedPct(dev));
    if (n.missing_slots > 0) {
      val.append(" · ");
      val.append(h("span", { class: "bad-text" }, `${count(n.missing_slots)} missing`));
    }
    slots = barRow("slots", bar(n.slots / scale, tone, share.expected / scale), val);
  }

  const meta = h(
    "div",
    { class: "meta" },
    h("span", null, "rtt " + (n.rtt ?? "—")),
    h("span", null, n.version || "?"),
    h("span", null, "applied " + count(n.applied)),
    h("span", null, `${n.addr}:${n.rpc_port}`),
    h("span", null, "joined " + shortDate(n.joined_at)),
  );
  return h("div", { class: "card node" + (n.self ? " self" : "") }, name, capacity, slots, meta);
}
