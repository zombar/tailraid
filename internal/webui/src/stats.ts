// The Stats page (docs/13 §1, docs/17 §4): the soak report's charts over any range of the
// history kept in the volume, a Grafana-style range picker, a member dropdown, and a live
// refresh while the range ends now (I-198).

import { clear, h } from "./dom.ts";
import { getStats, type StatsMember, type StatsResponse } from "./api.ts";

const RANGES: [string, number][] = [
  ["1h", 3600],
  ["6h", 6 * 3600],
  ["24h", 86400],
  ["7d", 7 * 86400],
  ["30d", 30 * 86400],
  ["1y", 365 * 86400],
];
const INTERVAL = 5000;

interface State {
  range: string; // a quick range's label, or "custom"
  from: number; // custom: Unix seconds
  to: number;
  node: string; // "" = this node
}

/** parse reads the state from the hash's query ("range=6h&node=pike" or "from=&to=&node="). */
export function parse(query: string): State {
  const q = new URLSearchParams(query);
  const from = Number(q.get("from"));
  const to = Number(q.get("to"));
  if (from > 0 && to > from) return { range: "custom", from, to, node: q.get("node") ?? "" };
  const range = RANGES.some(([r]) => r === q.get("range")) ? (q.get("range") as string) : "6h";
  return { range, from: 0, to: 0, node: q.get("node") ?? "" };
}

/** hash writes the state back: a link reproduces the view. */
export function hash(s: State): string {
  const q = new URLSearchParams();
  if (s.range === "custom") {
    q.set("from", String(s.from));
    q.set("to", String(s.to));
  } else q.set("range", s.range);
  if (s.node) q.set("node", s.node);
  return "#/stats?" + q.toString();
}

function bounds(s: State, now: number): { from: number; to: number | "now" } {
  if (s.range === "custom") return { from: s.from, to: s.to };
  const secs = RANGES.find(([r]) => r === s.range)?.[1] ?? 6 * 3600;
  return { from: Math.floor(now / 1000) - secs, to: "now" };
}

function local(t: number): string {
  const d = new Date(t * 1000);
  const p = (n: number) => String(n).padStart(2, "0");
  return `${d.getFullYear()}-${p(d.getMonth() + 1)}-${p(d.getDate())}T${p(d.getHours())}:${p(d.getMinutes())}`;
}

export function render(root: HTMLElement, query: string): () => void {
  const state = parse(query);
  let timer: number | undefined;
  let disposed = false;
  let last: StatsResponse | null = null;
  let lastAt = 0;

  const go = (next: Partial<State>) => {
    location.hash = hash({ ...state, ...next });
  };

  // the toolbar: the range picker, the custom form, the member menu, the live pill
  const seg = h("span", { class: "seg", role: "group", "aria-label": "range" });
  for (const [r] of RANGES) seg.append(h("button", { type: "button", "aria-pressed": String(state.range === r), onclick: () => go({ range: r, from: 0, to: 0 }) }, r));
  const form = h("form", { class: "inline-form range" });
  const customBtn = h("button", { type: "button", "aria-pressed": String(state.range === "custom"), onclick: () => form.toggleAttribute("hidden") }, "custom");
  seg.append(customBtn);
  const now = Math.floor(Date.now() / 1000);
  const fromIn = h("input", { type: "datetime-local", "aria-label": "from", value: local(state.range === "custom" ? state.from : now - 6 * 3600) }) as HTMLInputElement;
  const toIn = h("input", { type: "datetime-local", "aria-label": "to", value: local(state.range === "custom" ? state.to : now) }) as HTMLInputElement;
  const apply = h("button", { type: "submit" }, "Apply") as HTMLButtonElement;
  const check = () => {
    const f = Date.parse(fromIn.value) / 1000;
    const t = Date.parse(toIn.value) / 1000;
    apply.disabled = !(f > 0 && t > f);
  };
  fromIn.addEventListener("input", check);
  toIn.addEventListener("input", check);
  check();
  form.append(h("label", null, "from ", fromIn), h("label", null, "to ", toIn), apply, h("button", { type: "button", onclick: () => form.setAttribute("hidden", "") }, "Cancel"));
  form.addEventListener("submit", (e) => {
    e.preventDefault();
    go({ range: "custom", from: Math.floor(Date.parse(fromIn.value) / 1000), to: Math.floor(Date.parse(toIn.value) / 1000) });
  });
  form.addEventListener("keydown", (e) => {
    if ((e as KeyboardEvent).key === "Escape") form.setAttribute("hidden", "");
  });
  if (state.range !== "custom") form.setAttribute("hidden", "");
  const menu = h("details", { class: "menu" });
  const summary = h("summary", null, "Node: " + (state.node || "this device"));
  const body = h("div", { class: "menu-body" });
  menu.append(summary, body);
  const pill = h("span", { class: "pill", title: "waiting for the first fetch" }, "loading");
  const toolbar = h("div", { class: "toolbar" }, seg, menu, h("span", { class: "spacer" }), pill);
  const charts = h("div", { class: "charts" });
  const head = h("section", null, h("h2", null, "Stats", h("span", { class: "h-sub" }, "")), toolbar, form);
  root.append(head, charts);
  const sub = head.querySelector(".h-sub") as HTMLElement;

  const drawMembers = (nodes: StatsMember[], current: string) => {
    clear(body);
    const row = (name: string, label: string, dot: string, note: string, m?: StatsMember) => {
      const el = h("div", { class: "menu-row" + (m?.web ? " link" : ""), "aria-current": String(name === current) }, h("span", { class: "dot " + dot }), h("a", { href: hash({ ...state, node: name }) }, label), h("span", { class: "small dim" }, note));
      if (m?.web) el.append(h("a", { class: "menu-link", href: `http://${m.addr}:${m.web}/${hash({ ...state, node: name })}`, title: "open on " + m.name }, "↗"));
      body.append(el);
    };
    row("all", "all nodes", "off", "one series per member");
    for (const m of nodes) row(m.name, m.name, m.live ? "ok" : "bad", (m.self ? "this device · " : "") + (m.live ? "live" : "dead") + (m.web ? "" : " · no web listener"), m);
  };

  const draw = (r: StatsResponse) => {
    last = r;
    lastAt = Date.now();
    const nodeName = state.node === "" ? r.node : state.node;
    summary.textContent = "Node: " + (nodeName === "all" ? "all nodes" : nodeName);
    drawMembers(r.nodes, nodeName);
    const span = r.range.to - r.range.from;
    sub.textContent = `${r.range.tier} rows · ${span >= 2 * 86400 ? Math.round(span / 86400) + " d" : Math.round(span / 3600) + " h"}` + (r.range.live ? " · refreshes every 5 s while live" : "");
    clear(charts);
    if (r.charts.length === 0) charts.append(h("div", { class: "empty" }, "no rows in this range yet"));
    for (const c of r.charts) {
      const card = h("div", { class: "chart-card" });
      card.innerHTML = c.svg; // the node renders the SVG and escapes every name in it (I-197)
      card.append(h("div", { class: "small dim" }, c.caption + " · " + c.title));
      charts.append(card);
    }
    setPill(r.range.live ? "live" : "paused");
  };

  const setPill = (mode: "live" | "paused" | "error", err?: string) => {
    pill.className = "pill" + (mode === "live" ? " ok" : mode === "error" ? " bad" : "");
    pill.textContent = mode === "error" ? "unreachable" : mode;
    pill.title = mode === "error" ? err ?? "" : mode === "live" ? "last fetch " + new Date(lastAt).toLocaleTimeString() + " · next in 5 s" : "a custom range never refreshes";
  };

  const fetchOnce = async () => {
    const b = bounds(state, Date.now());
    try {
      const r = await getStats(b.from, b.to, state.node);
      if (!disposed) draw(r);
    } catch (e) {
      if (disposed) return;
      setPill("error", e instanceof Error ? e.message : String(e));
      if (!last) charts.replaceChildren(h("div", { class: "error" }, "Could not load the stats: " + (e instanceof Error ? e.message : String(e))));
    }
  };
  const loop = async () => {
    stop();
    await fetchOnce();
    if (!disposed && state.range !== "custom" && document.visibilityState !== "hidden") timer = window.setTimeout(() => void loop(), INTERVAL);
  };
  const stop = () => {
    if (timer !== undefined) window.clearTimeout(timer);
    timer = undefined;
  };
  const onVisible = () => {
    if (document.visibilityState === "hidden") {
      stop();
      if (last) setPill("paused");
    } else void loop();
  };
  document.addEventListener("visibilitychange", onVisible);
  void loop();
  return () => {
    disposed = true;
    stop();
    document.removeEventListener("visibilitychange", onVisible);
  };
}
