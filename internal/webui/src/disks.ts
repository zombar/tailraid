// Disks: the data spread across every disk (docs/14 §8), drawn when any host has a disk.
// A spread summary — one column per disk above or below its expected share, grouped by
// host — then one card per host with a row per disk; a host with no disks is a card with
// the note "gateway only · no disks" (docs/14 §9). The markup is
// docs/assets/lookdev/disks.html.

import { h } from "./dom.ts";
import { bar, type Tone } from "./chart.ts";
import { bytes, count, signedPct } from "./fmt.ts";
import { chunksPerTarget, shares } from "./balance.ts";
import type { NodeReport, Report } from "./api.ts";

interface Host {
  node: NodeReport;
  targets: NodeReport[]; // its disks; a device row itself is never a target (docs/14 §9)
}

/** hosts groups the devices with their disks, in node-table order. */
export function hosts(nodes: NodeReport[]): Host[] {
  return nodes
    .filter((n) => n.kind === "device" && n.state !== "removed")
    .map((n) => ({ node: n, targets: nodes.filter((d) => d.kind === "disk" && d.host === n.idx && d.state !== "removed") }));
}

/** devTone is the balance rule: amber past 10 % off the expected share, red past 25 %. */
function devTone(dev: number): Tone {
  return Number.isNaN(dev) ? "" : Math.abs(dev) > 0.25 ? "bad" : Math.abs(dev) > 0.1 ? "warn" : "";
}

/** off reports a disk that holds no budget: one pinned to 0. */
function off(t: NodeReport): boolean {
  return t.state === "active" && t.capacity === 0;
}

function plural(n: number, one: string): string {
  return n === 1 ? `1 ${one}` : `${n} ${one}s`;
}

export function render(root: HTMLElement, r: Report): void {
  if (!r.nodes.some((n) => n.kind === "disk")) return;
  const groups = hosts(r.nodes);
  const targets = groups.flatMap((g) => g.targets);
  const per = chunksPerTarget(r);
  const sh = shares(targets, () => per);
  const v = r.volume;
  const active = groups.filter((g) => g.targets.some((t) => t.state === "active" && t.capacity > 0));
  const perHost = targets.length / Math.max(1, groups.length);
  const layout = Number.isInteger(perHost) ? `${plural(groups.length, "host")} × ${plural(perHost, "target")}` : `${plural(groups.length, "host")}, ${plural(targets.length, "target")}`;
  const sub = `survives ${plural(v.tolerates_disks, "disk")}, ${plural(v.tolerates_hosts, "host")} · ${v.auto ? "auto" : "manual"} ${v.k}+${v.m} · ${layout}`;

  const n = v.k + v.m;
  const capPer = Math.ceil(n / Math.max(1, active.length));
  const capNote =
    v.domain_skew > 0
      ? h("div", { class: "cap-note warn" }, `${count(v.domain_skew)} chunks above the host cap (${capPer} of ${n} slots per host) · repair is moving them`)
      : h("div", { class: "cap-note" }, active.length > 1 ? `no chunk above the host cap (${capPer} of ${n} slots per host)` : `one host: every slot is on ${active[0]?.node.name ?? "it"}`);

  const scale = Math.max(1, ...targets.map((t) => Math.max(t.slots, sh.get(t.idx)?.expected ?? 0))) * 1.1;
  const block = h("div", { class: "disks" }, spread(groups, sh), capNote);
  for (const g of groups) block.append(hostCard(g, sh, scale));
  root.append(h("section", null, h("h2", null, "Disks", h("span", { class: "h-sub" }, sub)), block));
}

/** spread is the summary: each target's column rises above or falls below the midline by its deviation from the expected share, on a symmetric scale of ±25, ±50 or ±100 %. */
function spread(groups: Host[], sh: ReturnType<typeof shares>): HTMLElement {
  const devs = groups.flatMap((g) => g.targets.filter((t) => t.state === "active" && !off(t)).map((t) => Math.abs(sh.get(t.idx)?.deviation ?? 0)));
  const worst = Math.max(0, ...devs.filter((d) => !Number.isNaN(d)));
  const lim = worst <= 0.25 ? 0.25 : worst <= 0.5 ? 0.5 : 1;
  const pctLabel = (x: number) => `${Math.round(x * 100)} %`;
  const plot = h("div", { class: "spread-plot" });
  for (const g of groups) {
    if (g.targets.length === 0) continue; // a diskless host has no column (docs/14 §9)
    const cols = h("div", { class: "spread-cols" });
    for (const t of g.targets) {
      const share = sh.get(t.idx);
      if (off(t)) {
        cols.append(h("div", { class: "spread-col off", title: `${t.name}: no budget (capacity 0)` }));
        continue;
      }
      if (t.state !== "active") {
        cols.append(h("div", { class: "spread-col drain", title: `${t.name}: draining, ${count(t.slots)} slots to move` }));
        continue;
      }
      const dev = share?.deviation ?? NaN;
      const d = Number.isNaN(dev) ? 0 : dev;
      const cls = !t.live ? "dead" : devTone(dev);
      const title = `${t.name}: ${Number.isNaN(dev) ? "no expected share" : signedPct(dev) + " of expected"} (${count(t.slots)} of ${count(Math.round(share?.expected ?? 0))} slots)` + (t.live ? "" : ", dead");
      const col = h("div", { class: "spread-col" + (cls ? " " + cls : ""), title });
      const fill = h("i", { class: d >= 0 ? "up" : "down" });
      fill.style.height = ((Math.min(Math.abs(d), lim) / lim) * 50).toFixed(1) + "%";
      col.append(fill);
      cols.append(col);
    }
    const label = h("div", { class: "spread-label" }, h("span", { class: "dot " + (g.node.live ? "ok" : "bad") }), g.node.name);
    plot.append(h("div", { class: "spread-host" }, cols, label));
  }
  return h(
    "div",
    { class: "spread", role: "img", "aria-label": "slot share of each target against its expected share" },
    h("div", { class: "spread-axis" }, h("span", null, "+" + pctLabel(lim)), h("span", null, "expected"), h("span", null, "−" + pctLabel(lim))),
    plot,
  );
}

function hostCard(g: Host, sh: ReturnType<typeof shares>, scale: number): HTMLElement {
  const n = g.node;
  const badges = h("span", { class: "badges" });
  if (n.leader) badges.append(h("span", { class: "badge strong" }, "leader"));
  badges.append(n.voter ? h("span", { class: "badge strong" }, "voter") : h("span", { class: "badge" }, "non-voter"));
  if (n.self) badges.append(h("span", { class: "badge self" }, "self"));
  if (n.state === "draining") badges.append(h("span", { class: "badge warn" }, "draining"));
  const budget = g.targets.reduce((s, t) => s + t.capacity, 0);
  let sum = g.targets.length === 0 ? "no disks" : `${plural(g.targets.length, "disk")} · ${bytes(budget)}`;
  if (!n.live) sum += " · dead " + (n.dead_for ?? "?");
  const head = h("div", { class: "name-row" }, h("span", { class: "dot " + (n.live ? "ok" : "bad") }), h("span", { class: "name" }, n.name), h("span", { class: "host-sum" }, sum), badges);
  if (g.targets.length === 0) {
    // a diskless host: a member and gateway that stores nothing (docs/14 §9)
    return h("div", { class: "host-row diskless" + (n.self ? " self" : "") }, head, h("div", { class: "host-note" }, "gateway only · no disks"));
  }
  const strip = h("div", { class: "disk-strip" });
  for (const t of g.targets) strip.append(diskRow(t, sh.get(t.idx), scale));
  const card = h("div", { class: "host-row" + (n.self ? " self" : "") }, head, strip);
  for (const group of sharedGroups(g.targets)) {
    card.append(h("div", { class: "host-note" }, h("span", { class: "warn-icon" }), `${listOf(group)} are on one physical disk: they fail together, so data spread across them is not redundant`));
  }
  return card;
}

function listOf(names: string[]): string {
  return names.length < 2 ? names.join("") : `${names.slice(0, -1).join(", ")} and ${names[names.length - 1]}`;
}

/** sharedGroups are the host's disks that sit on one physical disk, as name groups. */
function sharedGroups(targets: NodeReport[]): string[][] {
  const seen = new Set<string>();
  const out: string[][] = [];
  for (const t of targets) {
    if (!t.shared_with?.length || seen.has(t.name)) continue;
    const names = [t.name, ...t.shared_with].sort();
    for (const n of names) seen.add(n);
    out.push(names);
  }
  return out;
}

function diskRow(t: NodeReport, share: { expected: number; deviation: number } | undefined, scale: number): HTMLElement {
  const dot = off(t) ? "off" : t.live ? "ok" : "bad";
  const name = h("div", { class: "disk-name" }, h("span", { class: "dot " + dot }), h("span", { class: "n" }, t.name));
  if (t.shared_with?.length) name.append(h("span", { class: "warn-icon", title: `on the same physical disk as ${t.shared_with.join(", ")}: they fail together` }));
  if (off(t)) {
    return h("div", { class: "disk off", title: `${t.name}: no budget (capacity 0)` }, name, h("div", { class: "bar" }), h("div", { class: "disk-val" }, "no budget (capacity 0)"));
  }
  const used = Math.max(0, t.capacity - t.free);
  const usage = t.live ? h("div", { class: "disk-val" }, h("span", null, bytes(used)), h("span", null, "of " + bytes(t.capacity))) : h("div", { class: "disk-val" }, `unknown (dead ${t.dead_for ?? "?"})`);
  if (t.state !== "active") {
    const b = bar(t.slots / scale);
    return h(
      "div",
      { class: "disk warn", title: `${t.name}: draining, ${count(t.slots)} slots to move` },
      name,
      b,
      h("div", { class: "disk-val dots" }, h("span", { class: "text-warn" }, "draining"), h("span", null, `${count(t.slots)} slots to move`)),
      usage,
    );
  }
  const exp = share?.expected ?? 0;
  const dev = share?.deviation ?? NaN;
  const b = bar(t.slots / scale, t.live ? devTone(dev) : "", exp / scale);
  const vals = h("div", { class: "disk-val dots" }, h("span", null, `${count(t.slots)} slots`), h("span", null, `exp ${count(Math.round(exp))}`));
  if (!Number.isNaN(dev)) vals.append(h("span", null, signedPct(dev)));
  const title = `${t.name}: ${count(t.slots)} slots, expected ${count(Math.round(exp))}; ${bytes(used)} used of ${bytes(t.capacity)}`;
  return h("div", { class: "disk" + (t.live ? "" : " bad"), title }, name, b, vals, usage);
}
