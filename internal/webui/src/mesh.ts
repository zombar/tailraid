// The dashboard's Mesh section (docs/13 §7, docs/20 §10, I-331): the node's picture of
// its paths (GET /api/mesh, fetched beside every status poll) over a table of the facts,
// one row per node: address, path, endpoint, probe round trip, seen age.

import { memberLink } from "./account.ts";
import { h } from "./dom.ts";
import { meshPicture } from "./api.ts";
import type { MeshPeer, NodeReport, Report } from "./api.ts";

let picture = "";

/** refresh fetches the picture; true when it changed (the caller redraws). */
export async function refresh(): Promise<boolean> {
  let svg = "";
  try {
    svg = await meshPicture();
  } catch {
    svg = "";
  }
  if (svg === picture) return false;
  picture = svg;
  return true;
}

export function render(root: HTMLElement, r: Report): void {
  const m = r.mesh;
  if (!m) return;
  const peers = m.peers ?? [];
  const byIdx = new Map<number, NodeReport>();
  for (const n of r.nodes) byIdx.set(n.idx, n);
  const counts = { lan: 0, wan: 0, relay: 0, none: 0 };
  for (const p of peers) {
    const c = p.class === "lan" || p.class === "wan" || p.class === "relay" ? p.class : "none";
    counts[c]++;
  }
  const pm = m.portmap && m.portmap.external ? `portmap ${m.portmap.protocol} ${m.portmap.external}` : "portmap none";
  const sub = `${m.addr}${m.interface ? " on " + m.interface : ""} · udp ${m.port} · ${pm} · ${peers.length} ${peers.length === 1 ? "peer" : "peers"}: ${counts.lan} lan, ${counts.wan} wan, ${counts.relay} relayed, ${counts.none} none`;
  const card = h("div", { class: "chart-card" });
  const pic = h("div");
  pic.innerHTML = picture; // the node renders the SVG and escapes every name in it (I-197)
  if (picture) card.append(...pic.childNodes);
  const self = r.nodes.find((n) => n.self);
  const rows: HTMLElement[] = [
    h(
      "tr",
      { class: "self" },
      h("td", { class: "name" }, self?.name ?? r.node, h("span", { class: "badge self" }, "this device")),
      h("td", { class: "addr" }, m.addr),
      h("td", { class: "path" }, `${m.interface ? m.interface + " · " : ""}udp ${m.port}`),
      h("td", { class: "ep" }, pm),
      h("td", { class: "rtt" }),
      h("td", { class: "seen" }),
    ),
  ];
  for (const p of peers) rows.push(row(p, byIdx.get(p.idx)));
  card.append(
    h(
      "table",
      { class: "mesh-peers" },
      h("thead", null, h("tr", null, h("th", null, "member"), h("th", null, "address"), h("th", null, "path"), h("th", null, "endpoint"), h("th", { class: "rtt" }, "rtt"), h("th", { class: "seen" }, "seen"))),
      h("tbody", null, ...rows),
    ),
    h("p", { class: "small dim" }, "this device's view: one edge per member in the class of the path to it, lan green, wan ink, relay amber through the member that carries it, none red; the table under it carries each member's facts; click a member for its own view"),
  );
  root.append(h("section", null, h("h2", null, "Mesh", h("span", { class: "h-sub" }, sub)), card));
}

function row(p: MeshPeer, n: NodeReport | undefined): HTMLElement {
  const name = p.name || n?.name || `node ${p.idx}`;
  const host = n?.addr ? n.addr.replace(/:\d+$/, "") : "";
  const nameCell = h("td", { class: "name" }, n?.web && host ? memberLink(name, `http://${host}:${n.web}/#/`, name) : name);
  if (n?.leader) nameCell.append(h("span", { class: "badge strong" }, "leader"));
  else if (n?.state === "draining") nameCell.append(h("span", { class: "badge warn" }, "draining"));
  let dot = "";
  let path = p.class;
  let ep = p.endpoint || "—";
  switch (p.class) {
    case "lan":
      dot = " ok";
      break;
    case "relay":
      dot = " warn";
      path = "relay via " + (p.via ?? "?");
      ep = "—";
      break;
    case "wan":
      break;
    default:
      dot = " bad";
      path = "none";
      ep = p.endpoint && !p.endpoint.startsWith("fanout:") ? p.endpoint : "—";
  }
  return h(
    "tr",
    { class: n && !n.live ? "off" : "" },
    nameCell,
    h("td", { class: "addr" }, host),
    h("td", { class: "path" }, h("span", { class: "dot" + dot }), path),
    h("td", { class: "ep" }, ep),
    h("td", { class: "rtt" }, n?.rtt || "—"),
    h("td", { class: "seen" }, p.seen_age || "—"),
  );
}
