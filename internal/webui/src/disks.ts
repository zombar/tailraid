// Disks: every drive of the volume (docs/14 §8), drawn when any host has a disk.
// A spread summary — one column per disk above or below its expected share, grouped by
// host, the S3 buckets as a group of their own and a host's archive mirror as a dashed
// column — then one card per host with a row per disk and per mirror (docs/16: it holds
// snapshots, never slots), and a card for the buckets; a host with no disks is a card
// with the note "gateway only · no disks" (docs/14 §9). The markup is
// docs/assets/lookdev/disks.html.

import { h } from "./dom.ts";
import { bar } from "./chart.ts";
import { bytes, count, signedPct } from "./fmt.ts";
import { chunksPerTarget, devTone, shares } from "./balance.ts";
import type { Mirror, NodeReport, Report, Snapshots } from "./api.ts";

interface Host {
  node: NodeReport;
  targets: NodeReport[]; // its disks; a device row itself is never a target (docs/14 §9)
  mirrors: NodeReport[]; // its archive disks: snapshots only, never a target (docs/16)
}

/** hosts groups the devices with their disks and mirrors, in node-table order. */
export function hosts(nodes: NodeReport[]): Host[] {
  const of = (kind: string, n: NodeReport) => nodes.filter((d) => d.kind === kind && d.host === n.idx && d.state !== "removed");
  return nodes.filter((n) => n.kind === "device" && n.state !== "removed").map((n) => ({ node: n, targets: of("disk", n), mirrors: of("mirror", n) }));
}

/** buckets are the S3 nodes: targets of their own, outside any host (docs/10 §4). */
function buckets(nodes: NodeReport[]): NodeReport[] {
  return nodes.filter((n) => n.kind === "s3" && n.state !== "removed");
}

/** off reports a disk that holds no budget: one pinned to 0. */
function off(t: NodeReport): boolean {
  return t.state === "active" && t.capacity === 0;
}

function plural(n: number, one: string): string {
  return n === 1 ? `1 ${one}` : `${n} ${one}s`;
}

export function render(root: HTMLElement, r: Report, snaps: Snapshots | null): void {
  if (!r.nodes.some((n) => n.kind === "disk")) return;
  const groups = hosts(r.nodes);
  const s3 = buckets(r.nodes);
  const targets = [...groups.flatMap((g) => g.targets), ...s3];
  const per = chunksPerTarget(r);
  const sh = shares(targets, () => per);
  const v = r.volume;
  const active = groups.filter((g) => g.targets.some((t) => t.state === "active" && t.capacity > 0));
  const disks = groups.flatMap((g) => g.targets).length;
  const perHostDisks = disks / Math.max(1, groups.length);
  let layout = Number.isInteger(perHostDisks) ? `${plural(groups.length, "host")} × ${plural(perHostDisks, "disk")}` : `${plural(groups.length, "host")}, ${plural(disks, "disk")}`;
  if (s3.length) layout += ` + ${plural(s3.length, "bucket")}`;
  const sub = `survives ${plural(v.tolerates_disks, "disk")}, ${plural(v.tolerates_hosts, "host")} · ${v.auto ? "auto" : "manual"} ${v.k}+${v.m} · ${layout}`;

  const n = v.k + v.m;
  const capPer = Math.ceil(n / Math.max(1, active.length));
  const capNote =
    v.domain_skew > 0
      ? h("div", { class: "cap-note warn" }, `${count(v.domain_skew)} chunks above the host cap (${capPer} of ${n} slots per host) · repair is moving them`)
      : h("div", { class: "cap-note" }, active.length > 1 ? `no chunk above the host cap (${capPer} of ${n} slots per host)` : `one host: every slot is on ${active[0]?.node.name ?? "it"}`);

  const scale = Math.max(1, ...targets.map((t) => Math.max(t.slots, sh.get(t.idx)?.expected ?? 0))) * 1.1;
  const mirrors = new Map((snaps?.mirrors ?? []).map((m) => [m.id, m]));
  const block = h("div", { class: "disks" }, spread(groups, s3, sh, mirrors), capNote);
  for (const g of groups) block.append(hostCard(g, sh, scale, mirrors));
  if (s3.length) block.append(bucketCard(s3, sh, scale));
  root.append(h("section", null, h("h2", null, "Disks", h("span", { class: "h-sub" }, sub)), block));
}

/** spread is the summary: each target's column rises above or falls below the midline by its deviation from the expected share, on a symmetric scale of ±25, ±50 or ±100 %. */
function spread(groups: Host[], s3: NodeReport[], sh: ReturnType<typeof shares>, mirrors: Map<number, Mirror>): HTMLElement {
  const all = [...groups.flatMap((g) => g.targets), ...s3];
  const devs = all.filter((t) => t.state === "active" && !off(t)).map((t) => Math.abs(sh.get(t.idx)?.deviation ?? 0));
  const worst = Math.max(0, ...devs.filter((d) => !Number.isNaN(d)));
  const lim = worst <= 0.25 ? 0.25 : worst <= 0.5 ? 0.5 : 1;
  const pctLabel = (x: number) => `${Math.round(x * 100)} %`;
  const plot = h("div", { class: "spread-plot" });
  const column = (t: NodeReport): HTMLElement => {
      const share = sh.get(t.idx);
      if (off(t)) return h("div", { class: "spread-col off", title: `${t.name}: no budget (capacity 0)` });
      if (t.state !== "active") return h("div", { class: "spread-col drain", title: `${t.name}: draining, ${count(t.slots)} slots to move` });
      const dev = share?.deviation ?? NaN;
      const d = Number.isNaN(dev) ? 0 : dev;
      const cls = !t.live ? "dead" : devTone(dev);
      const title = `${t.name}: ${Number.isNaN(dev) ? "no expected share" : signedPct(dev) + " of expected"} (${count(t.slots)} of ${count(Math.round(share?.expected ?? 0))} slots)` + (t.live ? "" : ", dead");
      const col = h("div", { class: "spread-col" + (cls ? " " + cls : ""), title });
      const fill = h("i", { class: d >= 0 ? "up" : "down" });
      fill.style.height = ((Math.min(Math.abs(d), lim) / lim) * 50).toFixed(1) + "%";
      col.append(fill);
      return col;
  };
  const group = (cols: HTMLElement[], live: boolean, name: string) =>
    plot.append(h("div", { class: "spread-host" }, h("div", { class: "spread-cols" }, ...cols), h("div", { class: "spread-label" }, h("span", { class: "dot " + (live ? "ok" : "bad") }), name)));
  for (const g of groups) {
    if (g.targets.length === 0 && g.mirrors.length === 0) continue; // a diskless host has no column (docs/14 §9)
    // an archive mirror holds no slots, so no share: its column fills from the bottom by how
    // full the disk is, dashed while it is not attached
    const archive = g.mirrors.map((n) => {
      const m = mirrors.get(n.idx);
      const frac = m && m.total > 0 ? Math.min(1, m.used / m.total) : 0;
      const col = h("div", { class: "spread-col archive" + (m?.online ? "" : " off"), title: `${n.name}: archive mirror, ${Math.round(frac * 100)} % full` + (m && m.total > 0 ? ` (${bytes(m.used)} of ${bytes(m.total)})` : "") + (m?.online ? "" : ", offline") });
      const level = h("i");
      level.style.height = (frac * 100).toFixed(1) + "%";
      col.append(level);
      return col;
    });
    group([...g.targets.map(column), ...archive], g.node.live, g.node.name);
  }
  if (s3.length) group(s3.map(column), s3.some((b) => b.live), "s3");
  return h(
    "div",
    { class: "spread", role: "img", "aria-label": "slot share of each target against its expected share" },
    h("div", { class: "spread-axis" }, h("span", null, "+" + pctLabel(lim)), h("span", null, "expected"), h("span", null, "−" + pctLabel(lim))),
    plot,
  );
}

function hostCard(g: Host, sh: ReturnType<typeof shares>, scale: number, mirrors: Map<number, Mirror>): HTMLElement {
  const n = g.node;
  const badges = h("span", { class: "badges" });
  if (n.leader) badges.append(h("span", { class: "badge strong" }, "leader"));
  badges.append(n.voter ? h("span", { class: "badge strong" }, "voter") : h("span", { class: "badge" }, "non-voter"));
  if (n.self) badges.append(h("span", { class: "badge self" }, "self"));
  if (n.state === "draining") badges.append(h("span", { class: "badge warn" }, "draining"));
  const budget = g.targets.reduce((s, t) => s + t.capacity, 0);
  let sum = g.targets.length === 0 ? "no disks" : `${plural(g.targets.length, "disk")} · ${bytes(budget)}`;
  if (g.mirrors.length) sum += ` · ${plural(g.mirrors.length, "mirror")}`;
  if (!n.live) sum += " · dead " + (n.dead_for ?? "?");
  const head = h("div", { class: "name-row" }, h("span", { class: "dot " + (n.live ? "ok" : "bad") }), h("span", { class: "name" }, n.name), h("span", { class: "host-sum" }, sum), badges);
  if (g.targets.length === 0 && g.mirrors.length === 0) {
    // a diskless host: a member and gateway that stores nothing (docs/14 §9)
    return h("div", { class: "host-row diskless" + (n.self ? " self" : "") }, head, h("div", { class: "host-note" }, "gateway only · no disks"));
  }
  const strip = h("div", { class: "disk-strip" });
  for (const t of g.targets) strip.append(diskRow(t, sh.get(t.idx), scale));
  for (const m of g.mirrors) strip.append(mirrorRow(m, mirrors.get(m.idx)));
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

/** mirrorRow is a host's archive disk (docs/16): how full it is, and whether it is attached now. */
function mirrorRow(n: NodeReport, m: Mirror | undefined): HTMLElement {
  const online = !!m?.online;
  const name = h("div", { class: "disk-name" }, h("span", { class: "dot " + (online ? "ok" : "off") }), h("span", { class: "n" }, n.name));
  const frac = m && m.total > 0 ? m.used / m.total : 0;
  const state = online ? "archive · online" : "archive · offline" + (m?.last_seen ? ` since ${m.last_seen.slice(0, 10)}` : "");
  const usage = m && m.total > 0 ? h("div", { class: "disk-val" }, h("span", null, bytes(m.used)), h("span", null, "of " + bytes(m.total))) : h("div", { class: "disk-val" }, online ? "size unknown" : "attach it to read its size");
  const title = `${n.name}: archive mirror, snapshots only` + (m?.reason ? ` · ${m.reason}` : "");
  return h("div", { class: "disk" + (online ? "" : " off"), title }, name, bar(frac), h("div", { class: "disk-val dots" }, h("span", null, state)), usage);
}

/** bucketCard lists the S3 nodes: each a target with its slots against its expected share, its stored bytes and its bucket. */
function bucketCard(s3: NodeReport[], sh: ReturnType<typeof shares>, scale: number): HTMLElement {
  const live = s3.filter((b) => b.live).length;
  const head = h("div", { class: "name-row" }, h("span", { class: "dot " + (live === s3.length ? "ok" : live ? "warn" : "bad") }), h("span", { class: "name" }, "S3"), h("span", { class: "host-sum" }, `${plural(s3.length, "bucket")} · ${bytes(s3.reduce((s, b) => s + b.stored_bytes, 0))} stored`));
  const strip = h("div", { class: "disk-strip" });
  for (const b of s3) {
    const row = diskRow(b, sh.get(b.idx), scale);
    // a bucket reports no free space (docs/10 §4.1): its usage is what it stores
    const where = b.s3 ? (b.s3.prefix ? `${b.s3.bucket}/${b.s3.prefix}` : b.s3.bucket) : "";
    row.lastElementChild?.replaceWith(h("div", { class: "disk-val" }, h("span", null, bytes(b.stored_bytes) + " stored"), h("span", null, where)));
    strip.append(row);
  }
  return h("div", { class: "host-row" }, head, strip);
}
