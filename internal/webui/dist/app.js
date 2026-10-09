// internal/webui/src/dom.ts
function h(tag, attrs, ...kids) {
  const el = document.createElement(tag);
  if (attrs) {
    for (const [k, v] of Object.entries(attrs)) {
      if (v == null || v === false) continue;
      if (k.startsWith("on") && typeof v === "function") el.addEventListener(k.slice(2).toLowerCase(), v);
      else if (k === "class") el.className = String(v);
      else if (k === "style") throw new Error("inline style is blocked by the CSP");
      else el.setAttribute(k, v === true ? "" : String(v));
    }
  }
  append(el, kids);
  return el;
}
function append(el, kids) {
  for (const c of kids) {
    if (c == null || c === false) continue;
    el.append(c instanceof Node ? c : String(c));
  }
}
var SVG = "http://www.w3.org/2000/svg";
function svg(tag, attrs, ...kids) {
  const el = document.createElementNS(SVG, tag);
  if (attrs) for (const [k, v] of Object.entries(attrs)) el.setAttribute(k, String(v));
  for (const c of kids) el.append(c);
  return el;
}
function clear(el) {
  while (el.firstChild) el.removeChild(el.firstChild);
}
function toast(msg, cls) {
  const box = document.getElementById("toasts");
  if (!box) return;
  const el = h("div", { class: "toast" + (cls ? " " + cls : ""), role: "status" }, msg);
  el.addEventListener("click", () => el.remove());
  box.append(el);
  setTimeout(() => el.remove(), 6e3);
}
var ICONS = {
  dir: "M1.5 4.5h4.5l1.5 1.5h7v7.5h-13z",
  file: "M4 1.5h5.5l3 3v10h-8.5z M9.5 1.5v3h3",
  link: "M6.5 9.5a2.5 2.5 0 0 0 3.5 0l2.5-2.5a2.5 2.5 0 0 0-3.5-3.5L8 4.5 M9.5 6.5a2.5 2.5 0 0 0-3.5 0L3.5 9a2.5 2.5 0 0 0 3.5 3.5L8 11.5",
  media: "M4 2.5l9 5.5-9 5.5z",
  image: "M1.5 2.5h13v11h-13z M1.5 11l4-4 3 3 2.5-2.5 3.5 3.5"
};
function icon(kind) {
  const el = svg("svg", { class: "icon", viewBox: "0 0 16 16" }, svg("path", { d: ICONS[kind] ?? ICONS.file }));
  if (kind === "image") el.append(svg("circle", { cx: 11, cy: 5.5, r: 1 }));
  return el;
}
function setWidth(el, frac) {
  el.style.width = (Math.max(0, Math.min(1, frac)) * 100).toFixed(2) + "%";
}

// internal/webui/src/api.ts
var ApiError = class extends Error {
  constructor(status2, message) {
    super(message);
    this.status = status2;
  }
  status;
};
var HEADER = "X-Tailraid-UI";
function encodePath(p) {
  return p.split("/").map(encodeURIComponent).join("/");
}
function fileURL(path, download = false) {
  return "/files" + encodePath(path) + (download ? "?download=1" : "");
}
async function call(url, init) {
  const r = await fetch(url, init);
  if (!r.ok) throw new ApiError(r.status, await errorText(r));
  if (r.status === 204) return void 0;
  return await r.json();
}
async function errorText(r) {
  try {
    const body = await r.json();
    if (body.error) return `${body.error} (${r.status})`;
  } catch {
  }
  return `${r.statusText || "request failed"} (${r.status})`;
}
var getStatus = () => call("/api/status");
var ls = (path) => call("/api/ls?path=" + encodeURIComponent(path));
var stat = (path) => call("/api/stat?path=" + encodeURIComponent(path));
var chunks = (path, offset, limit) => call(`/api/chunks?path=${encodeURIComponent(path)}&offset=${offset}&limit=${limit}`);
var mkdir = (path) => call("/api/mkdir", { method: "POST", headers: { [HEADER]: "1", "Content-Type": "application/json" }, body: JSON.stringify({ path }) });
async function text(path) {
  const r = await fetch(fileURL(path));
  if (!r.ok) throw new ApiError(r.status, await errorText(r));
  return r.text();
}
var PART = 32 << 20;
var RETRIES = 5;
function sendPart(path, blob, range, onProgress, track) {
  return new Promise((resolve, reject) => {
    const xhr = new XMLHttpRequest();
    track(xhr);
    xhr.open("PUT", fileURL(path));
    xhr.setRequestHeader(HEADER, "1");
    if (range) xhr.setRequestHeader("Content-Range", range);
    xhr.upload.onprogress = (e) => onProgress(e.loaded);
    xhr.onload = () => {
      const reply = { status: xhr.status };
      try {
        const body = JSON.parse(xhr.responseText);
        reply.error = body.error;
        reply.offset = body.offset;
      } catch {
      }
      resolve(reply);
    };
    xhr.onerror = () => reject(new ApiError(0, "network error"));
    xhr.onabort = () => reject(new ApiError(0, "cancelled"));
    xhr.send(blob);
  });
}
function failure(r) {
  return new ApiError(r.status, `${r.error ?? "upload failed"} (${r.status})`);
}
function wait(ms) {
  return new Promise((resolve) => setTimeout(resolve, ms));
}
async function uploadedSoFar(path) {
  const i = path.lastIndexOf("/");
  const temp = path.slice(0, i + 1) + "." + path.slice(i + 1) + ".tailraid-upload";
  try {
    return (await stat(temp)).size;
  } catch (e) {
    if (e instanceof ApiError && e.status === 404) return 0;
    throw e;
  }
}
function upload(path, file, onProgress, onNote) {
  let current = null;
  let aborted = false;
  const track = (x) => {
    current = x;
  };
  const promise = (async () => {
    if (file.size <= PART) {
      const r = await sendPart(path, file, null, onProgress, track);
      if (r.status < 200 || r.status >= 300) throw failure(r);
      return;
    }
    let offset = 0;
    let retries = 0;
    while (offset < file.size) {
      if (aborted) throw new ApiError(0, "cancelled");
      const end = Math.min(offset + PART, file.size);
      const base = offset;
      let r;
      try {
        r = await sendPart(path, file.slice(offset, end), `bytes ${offset}-${end - 1}/${file.size}`, (loaded) => onProgress(base + loaded), track);
      } catch (e) {
        if (aborted || e instanceof ApiError && e.status === 0 && e.message === "cancelled") throw e;
        r = { status: 0, error: e instanceof Error ? e.message : String(e) };
      }
      if (r.status >= 200 && r.status < 300) {
        offset = end;
        retries = 0;
        onNote?.("");
        continue;
      }
      if (r.status === 409 && typeof r.offset === "number" && r.offset !== offset) {
        offset = r.offset;
        continue;
      }
      if (r.status === 0 || r.status === 408 || r.status >= 500) {
        if (++retries > RETRIES) throw failure(r);
        onNote?.(`resuming ${retries}/${RETRIES}`);
        await wait(1e3 * Math.min(8, 2 ** (retries - 1)));
        offset = await uploadedSoFar(path);
        continue;
      }
      throw failure(r);
    }
  })();
  return {
    promise,
    abort: () => {
      aborted = true;
      current?.abort();
    }
  };
}

// internal/webui/src/status.ts
var INTERVAL = 5e3;
var HISTORY = 120;
var Status = class {
  current = null;
  error = null;
  history = [];
  listeners = /* @__PURE__ */ new Set();
  timer;
  started = false;
  subscribe(fn) {
    this.listeners.add(fn);
    if (!this.started) this.start();
    return () => this.listeners.delete(fn);
  }
  async refresh() {
    try {
      const r = await getStatus();
      this.current = r;
      this.error = null;
      this.history.push({ t: Date.now(), degraded: r.volume.degraded_chunks, pending: r.volume.repair_pending });
      if (this.history.length > HISTORY) this.history.splice(0, this.history.length - HISTORY);
    } catch (e) {
      this.error = e instanceof Error ? e.message : String(e);
    }
    for (const fn of this.listeners) fn();
  }
  start() {
    this.started = true;
    document.addEventListener("visibilitychange", () => {
      if (document.visibilityState === "hidden") this.stopTimer();
      else void this.loop();
    });
    void this.loop();
  }
  async loop() {
    this.stopTimer();
    await this.refresh();
    if (document.visibilityState !== "hidden") this.timer = window.setTimeout(() => void this.loop(), INTERVAL);
  }
  stopTimer() {
    if (this.timer !== void 0) window.clearTimeout(this.timer);
    this.timer = void 0;
  }
};
var status = new Status();

// internal/webui/src/fmt.ts
var UNITS = ["B", "KiB", "MiB", "GiB", "TiB", "PiB"];
function bytes(n) {
  if (!(n > 0)) return "0 B";
  let i = 0;
  let v = n;
  while (v >= 1024 && i < UNITS.length - 1) {
    v /= 1024;
    i++;
  }
  return (i === 0 ? v.toFixed(0) : v.toFixed(1)) + " " + UNITS[i];
}
function count(n) {
  return n.toLocaleString("en-US");
}
function signedPct(frac) {
  const s = (Math.abs(frac) * 100).toFixed(1) + " %";
  return (frac < 0 ? "\u2212" : "+") + s;
}
var ZERO_TIME = "0001-01-01";
function ago(iso, now = Date.now()) {
  if (!iso || iso.startsWith(ZERO_TIME)) return "never";
  const t = Date.parse(iso);
  if (Number.isNaN(t)) return iso;
  const s = Math.max(0, Math.round((now - t) / 1e3));
  if (s < 60) return s + " s ago";
  const m = Math.round(s / 60);
  if (m < 60) return m + " min ago";
  const hrs = Math.round(m / 60);
  if (hrs < 24) return hrs + " h ago";
  return date(t);
}
function when(iso, now = Date.now()) {
  if (!iso || iso.startsWith(ZERO_TIME)) return "";
  const t = Date.parse(iso);
  if (Number.isNaN(t)) return iso;
  if (now - t < 24 * 3600 * 1e3 && now >= t) return ago(iso, now);
  return date(t);
}
function date(t) {
  const d = new Date(t);
  const p = (n) => String(n).padStart(2, "0");
  return `${d.getFullYear()}-${p(d.getMonth() + 1)}-${p(d.getDate())} ${p(d.getHours())}:${p(d.getMinutes())}`;
}
function shortDate(iso) {
  if (!iso || iso.startsWith(ZERO_TIME)) return "";
  const t = Date.parse(iso);
  if (Number.isNaN(t)) return iso;
  return date(t).slice(0, 10);
}
function dirname(p) {
  const i = p.lastIndexOf("/");
  return i <= 0 ? "/" : p.slice(0, i);
}
function basename(p) {
  return p.slice(p.lastIndexOf("/") + 1);
}
function join(dir, name) {
  return dir === "/" ? "/" + name : dir + "/" + name;
}
function ext(name) {
  const i = name.lastIndexOf(".");
  return i < 0 ? "" : name.slice(i + 1).toLowerCase();
}

// internal/webui/src/util.ts
function encodePathForHash(path) {
  return path.split("/").map((s) => encodeURIComponent(s)).join("/");
}
function decodeHashPath(encoded) {
  const p = encoded.split("/").map((s) => {
    try {
      return decodeURIComponent(s);
    } catch {
      return s;
    }
  }).join("/");
  return p.startsWith("/") ? p : "/" + p;
}

// internal/webui/src/chart.ts
function bar(frac, tone = "", tickFrac, tall = false) {
  const el = h("div", { class: "bar" + (tone ? " " + tone : "") + (tall ? " tall" : "") });
  if (tone === "off") return el;
  const fill = h("i");
  setWidth(fill, frac);
  el.append(fill);
  if (tickFrac !== void 0) {
    const tick = h("b", { class: "tick" });
    tick.style.left = (Math.max(0, Math.min(1, tickFrac)) * 100).toFixed(2) + "%";
    el.append(tick);
  }
  return el;
}
function barRow(label, b, value) {
  return h("div", { class: "bar-row" }, h("span", { class: "bar-label" }, label), b, h("span", { class: "bar-val" }, value));
}
function fillTone(frac) {
  return frac > 0.95 ? "bad" : frac > 0.8 ? "warn" : "";
}
function sparkline(values, lastTone = "") {
  const wrap = h("div", { class: "spark-wrap" });
  const W = 120;
  const H = 32;
  const n = values.length;
  const max = Math.max(1, ...values);
  const pts = values.map((v, i) => [n === 1 ? W : i / (n - 1) * W, 28 - v / max * 24]);
  const line = pts.map(([x, y], i) => (i ? "L" : "M") + x.toFixed(1) + " " + y.toFixed(1)).join(" ");
  const el = svg("svg", { class: "spark", viewBox: `0 0 ${W} ${H}`, preserveAspectRatio: "none", "aria-hidden": "true" });
  if (n >= 2) {
    el.append(svg("path", { class: "area", d: `${line} L${W} ${H} L0 ${H} Z` }));
    el.append(svg("path", { class: "line", d: line }));
  }
  wrap.append(el);
  if (n >= 1) {
    const dot = h("span", { class: "spark-dot" + (lastTone ? " " + lastTone : "") });
    const y = pts[n - 1][1];
    dot.style.top = (y / H * 48).toFixed(1) + "px";
    wrap.append(dot);
  }
  return wrap;
}
function slotStrip(slots) {
  const strip = h("div", { class: "cells" });
  for (const s of slots) {
    let cls = "cell";
    let title = `slot ${s.idx}`;
    if (s.node === 0) {
      cls += " unplaced";
      title += " \xB7 unplaced";
    } else {
      title += " \xB7 " + (s.name || "node " + s.node);
      if (!s.live) {
        cls += " bad";
        title += " \xB7 dead";
      } else if (s.present === false) {
        cls += " warn";
        title += " \xB7 live, shard absent";
      } else if (s.present == null) {
        cls += " unverified";
        title += " \xB7 not verified";
      }
    }
    strip.append(h("span", { class: cls, title }));
  }
  return strip;
}

// internal/webui/src/balance.ts
function shares(nodes, maxSlots = () => Infinity) {
  const total = nodes.reduce((s, n) => s + n.slots, 0);
  const expected = /* @__PURE__ */ new Map();
  let open = nodes.filter((n) => n.state === "active" && n.capacity > 0);
  let left = total;
  while (open.length > 0) {
    const sumCap = open.reduce((s, n) => s + n.capacity, 0);
    const capped = open.filter((n) => left * n.capacity / sumCap > maxSlots(n));
    if (capped.length === 0) {
      for (const n of open) expected.set(n.idx, left * n.capacity / sumCap);
      break;
    }
    for (const n of capped) {
      expected.set(n.idx, maxSlots(n));
      left -= maxSlots(n);
    }
    open = open.filter((n) => !capped.includes(n));
  }
  const out = /* @__PURE__ */ new Map();
  for (const n of nodes) {
    const e = expected.get(n.idx) ?? 0;
    out.set(n.idx, { expected: e, deviation: e > 0 ? (n.slots - e) / e : NaN });
  }
  return out;
}
function chunksPerTarget(r) {
  return Math.max(0, r.volume.chunks - r.volume.tombstoned);
}
function withDisks(nodes) {
  return nodes.filter((n) => n.kind !== "disk").map((n) => {
    const ds = nodes.filter((d) => d.kind === "disk" && d.host === n.idx && d.state !== "removed");
    if (ds.length === 0) return n;
    const sum = (f) => f(n) + ds.reduce((s, d) => s + f(d), 0);
    return { ...n, capacity: sum((x) => x.capacity), free: sum((x) => x.free), slots: sum((x) => x.slots), missing_slots: sum((x) => x.missing_slots) };
  });
}
function render(root, r) {
  const nodes = withDisks(r.nodes);
  const per = chunksPerTarget(r);
  const targetsOf = (n) => n.kind === "s3" ? 1 : r.nodes.filter((d) => d.kind === "disk" && d.host === n.idx && d.state === "active" && d.capacity > 0).length;
  const sh = shares(nodes, (n) => per * targetsOf(n));
  const scale = Math.max(1, ...nodes.map((n) => Math.max(n.slots, sh.get(n.idx)?.expected ?? 0)));
  const cards = h("div", { class: "cards" });
  for (const n of nodes) cards.append(card(n, sh.get(n.idx) ?? { expected: 0, deviation: NaN }, scale));
  root.append(h("section", null, h("h2", null, "Balance"), cards));
}
function card(n, share, scale) {
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
  let capacity;
  if (!n.live) {
    capacity = barRow("capacity", bar(0, "off"), "unknown (dead " + (n.dead_for ?? "?") + ")");
  } else if (n.capacity === 0) {
    capacity = barRow("capacity", bar(0), n.kind === "device" ? "no disks" : "no capacity");
  } else {
    const used = Math.max(0, n.capacity - n.free);
    const frac = used / n.capacity;
    capacity = barRow("capacity", bar(frac, fillTone(frac)), `${bytes(used)} of ${bytes(n.capacity)}`);
  }
  let slots;
  if (n.state !== "active") {
    slots = barRow("slots", bar(n.slots / scale, n.slots > 0 ? "warn" : ""), `${count(n.slots)} slots to move`);
  } else {
    const dev = share.deviation;
    const tone = Number.isNaN(dev) ? "" : Math.abs(dev) > 0.25 ? "bad" : Math.abs(dev) > 0.1 ? "warn" : "";
    const val = h("span", null, `${count(n.slots)} \xB7 expected ${count(Math.round(share.expected))}`);
    if (!Number.isNaN(dev)) val.append(" \xB7 " + signedPct(dev));
    if (n.missing_slots > 0) {
      val.append(" \xB7 ");
      val.append(h("span", { class: "text-bad" }, `${count(n.missing_slots)} missing`));
    }
    slots = barRow("slots", bar(n.slots / scale, tone, share.expected / scale), val);
  }
  const meta = h(
    "div",
    { class: "meta" },
    h("span", null, "rtt " + (n.rtt ?? "\u2014")),
    h("span", null, n.version || "?"),
    h("span", null, "applied " + count(n.applied)),
    h("span", null, `${n.addr}:${n.rpc_port}`),
    h("span", null, "joined " + shortDate(n.joined_at))
  );
  return h("div", { class: "card node" + (n.self ? " self" : "") }, name, capacity, slots, meta);
}

// internal/webui/src/disks.ts
function hosts(nodes) {
  return nodes.filter((n) => n.kind === "device" && n.state !== "removed").map((n) => ({ node: n, targets: nodes.filter((d) => d.kind === "disk" && d.host === n.idx && d.state !== "removed") }));
}
function devTone(dev) {
  return Number.isNaN(dev) ? "" : Math.abs(dev) > 0.25 ? "bad" : Math.abs(dev) > 0.1 ? "warn" : "";
}
function off(t) {
  return t.state === "active" && t.capacity === 0;
}
function plural(n, one) {
  return n === 1 ? `1 ${one}` : `${n} ${one}s`;
}
function render2(root, r) {
  if (!r.nodes.some((n2) => n2.kind === "disk")) return;
  const groups = hosts(r.nodes);
  const targets = groups.flatMap((g) => g.targets);
  const per = chunksPerTarget(r);
  const sh = shares(targets, () => per);
  const v = r.volume;
  const active = groups.filter((g) => g.targets.some((t) => t.state === "active" && t.capacity > 0));
  const perHost = targets.length / Math.max(1, groups.length);
  const layout = Number.isInteger(perHost) ? `${plural(groups.length, "host")} \xD7 ${plural(perHost, "target")}` : `${plural(groups.length, "host")}, ${plural(targets.length, "target")}`;
  const sub = `survives ${plural(v.tolerates_disks, "disk")}, ${plural(v.tolerates_hosts, "host")} \xB7 ${v.auto ? "auto" : "manual"} ${v.k}+${v.m} \xB7 ${layout}`;
  const n = v.k + v.m;
  const capPer = Math.ceil(n / Math.max(1, active.length));
  const capNote = v.domain_skew > 0 ? h("div", { class: "cap-note warn" }, `${count(v.domain_skew)} chunks above the host cap (${capPer} of ${n} slots per host) \xB7 repair is moving them`) : h("div", { class: "cap-note" }, active.length > 1 ? `no chunk above the host cap (${capPer} of ${n} slots per host)` : `one host: every slot is on ${active[0]?.node.name ?? "it"}`);
  const scale = Math.max(1, ...targets.map((t) => Math.max(t.slots, sh.get(t.idx)?.expected ?? 0))) * 1.1;
  const block = h("div", { class: "disks" }, spread(groups, sh), capNote);
  for (const g of groups) block.append(hostCard(g, sh, scale));
  root.append(h("section", null, h("h2", null, "Disks", h("span", { class: "h-sub" }, sub)), block));
}
function spread(groups, sh) {
  const devs = groups.flatMap((g) => g.targets.filter((t) => t.state === "active" && !off(t)).map((t) => Math.abs(sh.get(t.idx)?.deviation ?? 0)));
  const worst = Math.max(0, ...devs.filter((d) => !Number.isNaN(d)));
  const lim = worst <= 0.25 ? 0.25 : worst <= 0.5 ? 0.5 : 1;
  const pctLabel = (x) => `${Math.round(x * 100)} %`;
  const plot = h("div", { class: "spread-plot" });
  for (const g of groups) {
    if (g.targets.length === 0) continue;
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
      fill.style.height = (Math.min(Math.abs(d), lim) / lim * 50).toFixed(1) + "%";
      col.append(fill);
      cols.append(col);
    }
    const label = h("div", { class: "spread-label" }, h("span", { class: "dot " + (g.node.live ? "ok" : "bad") }), g.node.name);
    plot.append(h("div", { class: "spread-host" }, cols, label));
  }
  return h(
    "div",
    { class: "spread", role: "img", "aria-label": "slot share of each target against its expected share" },
    h("div", { class: "spread-axis" }, h("span", null, "+" + pctLabel(lim)), h("span", null, "expected"), h("span", null, "\u2212" + pctLabel(lim))),
    plot
  );
}
function hostCard(g, sh, scale) {
  const n = g.node;
  const badges = h("span", { class: "badges" });
  if (n.leader) badges.append(h("span", { class: "badge strong" }, "leader"));
  badges.append(n.voter ? h("span", { class: "badge strong" }, "voter") : h("span", { class: "badge" }, "non-voter"));
  if (n.self) badges.append(h("span", { class: "badge self" }, "self"));
  if (n.state === "draining") badges.append(h("span", { class: "badge warn" }, "draining"));
  const budget = g.targets.reduce((s, t) => s + t.capacity, 0);
  let sum = g.targets.length === 0 ? "no disks" : `${plural(g.targets.length, "disk")} \xB7 ${bytes(budget)}`;
  if (!n.live) sum += " \xB7 dead " + (n.dead_for ?? "?");
  const head = h("div", { class: "name-row" }, h("span", { class: "dot " + (n.live ? "ok" : "bad") }), h("span", { class: "name" }, n.name), h("span", { class: "host-sum" }, sum), badges);
  if (g.targets.length === 0) {
    return h("div", { class: "host-row diskless" + (n.self ? " self" : "") }, head, h("div", { class: "host-note" }, "gateway only \xB7 no disks"));
  }
  const strip = h("div", { class: "disk-strip" });
  for (const t of g.targets) strip.append(diskRow(t, sh.get(t.idx), scale));
  const card2 = h("div", { class: "host-row" + (n.self ? " self" : "") }, head, strip);
  for (const group of sharedGroups(g.targets)) {
    card2.append(h("div", { class: "host-note" }, h("span", { class: "warn-icon" }), `${listOf(group)} are on one physical disk: they fail together, so data spread across them is not redundant`));
  }
  return card2;
}
function listOf(names) {
  return names.length < 2 ? names.join("") : `${names.slice(0, -1).join(", ")} and ${names[names.length - 1]}`;
}
function sharedGroups(targets) {
  const seen = /* @__PURE__ */ new Set();
  const out = [];
  for (const t of targets) {
    if (!t.shared_with?.length || seen.has(t.name)) continue;
    const names = [t.name, ...t.shared_with].sort();
    for (const n of names) seen.add(n);
    out.push(names);
  }
  return out;
}
function diskRow(t, share, scale) {
  const dot = off(t) ? "off" : t.live ? "ok" : "bad";
  const name = h("div", { class: "disk-name" }, h("span", { class: "dot " + dot }), h("span", { class: "n" }, t.name));
  if (t.shared_with?.length) name.append(h("span", { class: "warn-icon", title: `on the same physical disk as ${t.shared_with.join(", ")}: they fail together` }));
  if (off(t)) {
    return h("div", { class: "disk off", title: `${t.name}: no budget (capacity 0)` }, name, h("div", { class: "bar" }), h("div", { class: "disk-val" }, "no budget (capacity 0)"));
  }
  const used = Math.max(0, t.capacity - t.free);
  const usage = t.live ? h("div", { class: "disk-val" }, h("span", null, bytes(used)), h("span", null, "of " + bytes(t.capacity))) : h("div", { class: "disk-val" }, `unknown (dead ${t.dead_for ?? "?"})`);
  if (t.state !== "active") {
    const b2 = bar(t.slots / scale);
    return h(
      "div",
      { class: "disk warn", title: `${t.name}: draining, ${count(t.slots)} slots to move` },
      name,
      b2,
      h("div", { class: "disk-val dots" }, h("span", { class: "text-warn" }, "draining"), h("span", null, `${count(t.slots)} slots to move`)),
      usage
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

// internal/webui/src/integrity.ts
var MAX_DIRTY = 256 * 1024 * 1024;
var WAL_CAP = 128 * 1024 * 1024;
function render3(root, r, history) {
  const v = r.volume;
  const tiles = h(
    "div",
    { class: "tiles" },
    tile("Degraded", v.degraded_chunks, `chunks with fewer than ${v.k + v.m} good slots`, "warn"),
    tile("Repair pending", v.repair_pending, "waiting for fill_after", "warn"),
    tile("Unreadable", v.unreadable_chunks, `chunks with fewer than ${v.k} live slots`, "bad"),
    tile("Stale policy", v.stale_policy_chunks, `chunks not yet at ${v.k}+${v.m}`, "warn"),
    tile("Host skew", v.domain_skew, "chunks above the host cap", "warn")
  );
  const last = history[history.length - 1];
  const lastTone = last && last.degraded + last.pending > 0 ? "warn" : "";
  const spark = h(
    "div",
    null,
    sparkline(
      history.map((s) => s.degraded + s.pending),
      lastTone
    ),
    h("div", { class: "small dim" }, "degraded + pending, last 10 min (this tab only)")
  );
  const section = h("section", null, h("h2", null, "Integrity"), tiles, spark, repair(r));
  if (r.gateway) {
    const g = r.gateway;
    section.append(
      h("h3", null, "Gateway"),
      barRow("dirty bytes", bar(g.dirty_bytes / MAX_DIRTY, fillTone(g.dirty_bytes / MAX_DIRTY)), `${bytes(g.dirty_bytes)} in ${g.dirty_inodes} ${g.dirty_inodes === 1 ? "file" : "files"} of ${bytes(MAX_DIRTY)}` + (g.flushing ? " \xB7 flushing" : "")),
      barRow("WAL", bar(g.wal_bytes / WAL_CAP, fillTone(g.wal_bytes / WAL_CAP)), `${bytes(g.wal_bytes)} of ${bytes(WAL_CAP)}`),
      h("div", { class: "small dim" }, "acknowledged writes not yet committed to the cluster")
    );
  }
  root.append(section);
}
function tile(label, value, sub, tone) {
  const zero = value === 0;
  const lab = h("div", { class: "label" });
  if (!zero) lab.append(h("span", { class: "dot " + tone }));
  lab.append(label);
  return h("div", { class: "tile" + (zero ? " zero" : " " + tone) }, lab, h("div", { class: "value" }, count(value)), h("div", { class: "sub" }, sub));
}
function repair(r) {
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
  const row2 = (k, v, tone) => kv.append(h("div", null, h("dt", null, k), h("dd", { class: tone }, v)));
  row2("scanned", count(s.scanned));
  row2("repaired", count(s.repaired));
  row2("partial", count(s.partial));
  row2("unrepairable", count(s.unrepairable), s.unrepairable > 0 ? "bad" : void 0);
  row2("deferred", count(s.deferred));
  row2("migrated", count(s.migrated));
  row2("policy stale", count(s.policy_stale));
  row2("scrubbed", count(s.scrubbed));
  row2("scrub bad", count(s.scrub_bad), s.scrub_bad > 0 ? "bad" : void 0);
  row2("bytes moved", bytes(s.bytes_moved));
  row2("errors", count(s.errors), s.errors > 0 ? "bad" : void 0);
  row2("reported bad slots", count(r.repair.reported_bad_slots), r.repair.reported_bad_slots > 0 ? "warn" : void 0);
  return h("div", null, h("h3", null, "Repair \xB7 last scan " + ago(r.repair.last_scan)), kv);
}

// internal/webui/src/dashboard.ts
function render4(root) {
  const draw = () => {
    clear(root);
    const r = status.current;
    if (!r) {
      root.append(h("div", { class: status.error ? "error" : "empty" }, status.error ? "Could not load the status: " + status.error : "loading\u2026"));
      return;
    }
    volume(root, r);
    render2(root, r);
    render(root, r);
    render3(root, r, status.history);
  };
  const unsub = status.subscribe(draw);
  draw();
  return unsub;
}
function volume(root, r) {
  const v = r.volume;
  const tiles = h(
    "div",
    { class: "tiles" },
    tile2("Policy", `${v.k}+${v.m}`, `${v.auto ? "auto" : "manual"} \xB7 ${bytes(v.chunk_size)} chunks \xB7 policy v${v.policy_version}`),
    tile2("Files", count(v.inodes), `${count(v.chunks)} chunks \xB7 ${count(v.tombstoned)} tombstoned`),
    tile2("Dedup", v.dedup_ratio.toFixed(2) + "\xD7", bytes(v.logical_bytes) + " logical")
  );
  const scale = Math.max(1, v.logical_bytes, v.stored_bytes);
  const used = Math.max(0, v.usable_capacity - v.free);
  const usedFrac = v.usable_capacity > 0 ? used / v.usable_capacity : 0;
  const compare2 = h(
    "div",
    { class: "compare" },
    barRow("logical", bar(v.logical_bytes / scale, "", void 0, true), bytes(v.logical_bytes)),
    barRow("stored", bar(v.stored_bytes / scale, "", void 0, true), bytes(v.stored_bytes) + " with parity"),
    barRow("usable", bar(usedFrac, fillTone(usedFrac), void 0, true), `${bytes(used)} used of ${bytes(v.usable_capacity)} \xB7 ${bytes(v.free)} free`)
  );
  root.append(h("section", null, h("h2", null, "Volume"), tiles, compare2));
}
function tile2(label, value, sub) {
  return h("div", { class: "tile" }, h("div", { class: "label" }, label), h("div", { class: "value" }, value), h("div", { class: "sub" }, sub));
}

// internal/webui/src/chunks.ts
var PAGE = 64;
function render5(path, expected) {
  const summary = h("summary", null, "Integrity" + (expected !== void 0 ? ` (${count(expected)} ${expected === 1 ? "chunk" : "chunks"})` : ""));
  const body = h("div", { class: "body" });
  const panel = h("details", { class: "panel" }, summary, body);
  let loaded = 0;
  let total = 0;
  let fetching = false;
  const counts = { committed: 0, degraded: 0, unreadable: 0, tombstoned: 0, stale: 0 };
  const rows = h("div");
  const more = h("button", { type: "button" }, "Load more");
  const caption = h("div", { class: "small dim" });
  const summaryExtra = h("span", { class: "dim small" });
  const describe = () => {
    const parts = [];
    if (counts.committed) parts.push(`${count(counts.committed)} committed`);
    if (counts.degraded) parts.push(`${count(counts.degraded)} degraded`);
    if (counts.unreadable) parts.push(`${count(counts.unreadable)} unreadable`);
    if (counts.tombstoned) parts.push(`${count(counts.tombstoned)} tombstoned`);
    if (counts.stale) parts.push(`${count(counts.stale)} policy stale`);
    summaryExtra.textContent = parts.length ? " \xB7 " + parts.join(" \xB7 ") : "";
    caption.textContent = `a hash that appears more than once is the same stored chunk (dedup) \xB7 ${count(loaded)} of ${count(total)}`;
    more.textContent = `Load ${Math.min(PAGE, total - loaded)} more`;
    more.hidden = loaded >= total;
  };
  const load = async () => {
    if (fetching) return;
    fetching = true;
    more.disabled = true;
    try {
      const page = await chunks(path, loaded, PAGE);
      total = page.total;
      page.chunks.forEach((c, i) => {
        rows.append(row(loaded + i, c));
        if (c.state === "committed") counts.committed++;
        else if (c.state === "degraded") counts.degraded++;
        else if (c.state === "unreadable") counts.unreadable++;
        else if (c.state === "tombstoned") counts.tombstoned++;
        if (c.policy_stale) counts.stale++;
      });
      loaded += page.chunks.length;
      if (!summary.contains(summaryExtra)) summary.append(summaryExtra);
      if (!body.contains(rows)) body.append(rows, caption, more);
      describe();
      if (total === 0) body.append(h("div", { class: "empty" }, "no chunks (empty file)"));
    } catch (e) {
      body.append(h("div", { class: "error" }, "Could not load the chunk reports: " + (e instanceof Error ? e.message : String(e))));
    } finally {
      fetching = false;
      more.disabled = false;
    }
  };
  more.addEventListener("click", () => void load());
  panel.addEventListener("toggle", () => {
    if (panel.open && loaded === 0 && !fetching) void load();
  });
  return panel;
}
function row(i, c) {
  const km = c.k + c.m;
  const live = h("span", { class: "live" + (c.live_slots < c.k ? " bad" : c.live_slots < km ? " warn" : "") }, `${c.live_slots}/${km}`);
  const hash = h("span", { title: c.hash }, c.hash.slice(0, 10));
  if (c.policy_stale) hash.append(" ", h("span", { class: "badge warn" }, "stale"));
  if (!c.found) hash.append(" ", h("span", { class: "badge bad" }, c.state));
  return h("div", { class: "chunk-row" }, h("span", { class: "dim" }, "#" + i), hash, h("span", { class: "km dim" }, `${c.k}+${c.m}`), live, slotStrip(c.slots));
}

// internal/webui/src/player.ts
var KINDS = {
  mp4: ["video", "video/mp4"],
  m4v: ["video", "video/mp4"],
  webm: ["video", "video/webm"],
  mov: ["video", "video/quicktime"],
  ogv: ["video", "video/ogg"],
  mkv: ["video", "video/x-matroska"],
  avi: ["video", "video/x-msvideo"],
  mp3: ["audio", "audio/mpeg"],
  m4a: ["audio", "audio/mp4"],
  aac: ["audio", "audio/aac"],
  ogg: ["audio", "audio/ogg"],
  oga: ["audio", "audio/ogg"],
  opus: ["audio", "audio/ogg"],
  wav: ["audio", "audio/wav"],
  flac: ["audio", "audio/flac"],
  jpg: ["image", ""],
  jpeg: ["image", ""],
  png: ["image", ""],
  gif: ["image", ""],
  webp: ["image", ""],
  avif: ["image", ""],
  svg: ["image", ""],
  bmp: ["image", ""],
  txt: ["text", ""],
  md: ["text", ""],
  markdown: ["text", ""],
  json: ["text", ""],
  log: ["text", ""],
  yaml: ["text", ""],
  yml: ["text", ""],
  toml: ["text", ""],
  csv: ["text", ""],
  ini: ["text", ""],
  conf: ["text", ""],
  go: ["text", ""],
  ts: ["text", ""],
  js: ["text", ""],
  py: ["text", ""],
  sh: ["text", ""],
  html: ["text", ""],
  pdf: ["pdf", ""]
};
var TEXT_MAX = 1024 * 1024;
function mediaKind(e) {
  return KINDS[e]?.[0] ?? "other";
}
var media = null;
function render6(root, path) {
  const dir = dirname(path);
  const name = basename(path);
  const kind = mediaKind(ext(name));
  let siblings = [];
  let entry;
  let keyHandler = null;
  const draw = () => {
    clear(root);
    const section = h("section", { class: "player" }, crumbsAll(dir));
    const info = entry ? `${bytes(entry.size)} \xB7 ${when(entry.mtime)}` : "";
    section.append(
      h(
        "div",
        { class: "title-row" },
        h("h1", null, name),
        h("span", { class: "small dim mono" }, info),
        h("span", { class: "actions" }, h("a", { class: "btn", href: fileURL(path, true) }, "Download"))
      )
    );
    const viewer = h("div", { class: "viewer" });
    section.append(viewer);
    const group = siblings.filter((s) => sameGroup(mediaKind(ext(s.name)), kind));
    const index = group.findIndex((s) => s.name === name);
    const prev = index > 0 ? group[index - 1] : void 0;
    const next = index >= 0 && index < group.length - 1 ? group[index + 1] : void 0;
    const go = (e) => {
      if (e) location.hash = "#/file" + encodeForHash(join(dir, e.name));
    };
    if (kind === "video" || kind === "audio") {
      const mime = KINDS[ext(name)]?.[1] ?? "";
      let el;
      if (media && media.dir === dir && media.el.tagName.toLowerCase() === kind) {
        el = media.el;
        el.src = fileURL(path);
        void el.play().catch(() => void 0);
      } else {
        media?.el.pause();
        el = document.createElement(kind);
        el.controls = true;
        el.preload = "metadata";
        if (kind === "video") el.playsInline = true;
        el.src = fileURL(path);
        media = { el, dir };
      }
      el.onended = () => go(next);
      viewer.append(el);
      if (mime && el.canPlayType(mime) === "") {
        section.append(
          h("div", { class: "note" }, "Your browser reports it cannot play ", h("span", { class: "mono" }, mime), ". tailraid serves files as stored, without transcoding; mkv, avi and flac play only where the browser has a native decoder. ", h("a", { href: fileURL(path, true) }, "Download instead"), ".")
        );
      }
    } else if (kind === "image") {
      viewer.append(h("img", { src: fileURL(path), alt: name }));
      if (next) new Image().src = fileURL(join(dir, next.name));
    } else if (kind === "text") {
      if (entry && entry.size > TEXT_MAX) {
        viewer.append(h("div", { class: "placeholder" }, "too large to preview", h("a", { class: "btn", href: fileURL(path, true) }, "Download")));
      } else {
        const pre = h("pre", null, "loading\u2026");
        viewer.append(pre);
        text(path).then(
          (t) => pre.textContent = t,
          (e) => pre.textContent = "could not load: " + (e instanceof Error ? e.message : String(e))
        );
      }
    } else if (kind === "pdf") {
      viewer.append(h("iframe", { src: fileURL(path), title: name }));
      section.append(h("div", { class: "small dim" }, h("a", { href: fileURL(path), target: "_blank", rel: "noopener" }, "Open in a new tab")));
    } else {
      viewer.append(h("div", { class: "placeholder" }, info || name, h("a", { class: "btn", href: fileURL(path, true) }, "Download")));
    }
    if (group.length > 1 && kind !== "text" && kind !== "other" && kind !== "pdf") {
      const label = kind === "image" ? "image" : "track";
      section.append(
        h(
          "div",
          { class: "controls" },
          h("button", { type: "button", disabled: !prev, onClick: () => go(prev) }, "\u25C0 Previous"),
          h("button", { type: "button", disabled: !next, onClick: () => go(next) }, "Next \u25B6"),
          h("span", { class: "track" }, `${label} ${index + 1} of ${group.length}` + (next ? ` \xB7 next: ${next.name}` : ""))
        )
      );
    }
    const cs = status.current?.volume.chunk_size;
    section.append(render5(path, entry && cs ? Math.ceil(entry.size / cs) : void 0));
    root.append(section);
    if (keyHandler) document.removeEventListener("keydown", keyHandler);
    keyHandler = (ev) => {
      const t = ev.target;
      if (t && (t.tagName === "INPUT" || t.tagName === "TEXTAREA" || t.tagName === "VIDEO" || t.tagName === "AUDIO")) return;
      if (ev.key === "ArrowRight") go(next);
      else if (ev.key === "ArrowLeft") go(prev);
      else if (ev.key === "Escape") location.hash = "#/browse" + encodeForHash(dir);
      else if (ev.key === " " && media) {
        ev.preventDefault();
        if (media.el.paused) void media.el.play().catch(() => void 0);
        else media.el.pause();
      }
    };
    document.addEventListener("keydown", keyHandler);
  };
  draw();
  ls(dir).then(
    (r) => {
      siblings = r.entries.filter((e) => e.type === "file" && !e.upload);
      entry = siblings.find((e) => e.name === name);
      draw();
    },
    (e) => toast("Could not list the folder: " + (e instanceof Error ? e.message : String(e)), "bad")
  );
  return () => {
    if (keyHandler) document.removeEventListener("keydown", keyHandler);
    const leaving = !location.hash.startsWith("#/file");
    if (leaving && media) {
      media.el.pause();
      media.el.removeAttribute("src");
      media.el.load();
      media = null;
    }
  };
}
function sameGroup(a, b) {
  if (a === b) return true;
  return (a === "video" || a === "audio") && (b === "video" || b === "audio");
}
function encodeForHash(path) {
  return path.split("/").map(encodeURIComponent).join("/");
}

// internal/webui/src/browser.ts
function loadSort() {
  try {
    const s = sessionStorage.getItem("tr.sort");
    if (s) return JSON.parse(s);
  } catch {
  }
  return { key: "name", desc: false };
}
function saveSort(s) {
  try {
    sessionStorage.setItem("tr.sort", JSON.stringify(s));
  } catch {
  }
}
var queue = [];
var running = false;
var onQueueChange = null;
var onJobDone = null;
function enqueue(dir, files) {
  for (const f of Array.from(files)) queue.push({ dir, file: f, sent: 0, state: "queued" });
  onQueueChange?.();
  void runQueue();
}
async function runQueue() {
  if (running) return;
  running = true;
  for (; ; ) {
    const job = queue.find((j) => j.state === "queued");
    if (!job) break;
    job.state = "uploading";
    onQueueChange?.();
    job.handle = upload(
      join(job.dir, job.file.name),
      job.file,
      (sent) => {
        job.sent = sent;
        onQueueChange?.();
      },
      (note) => {
        job.note = note;
        onQueueChange?.();
      }
    );
    try {
      await job.handle.promise;
      job.state = "done";
      toast("Uploaded " + job.file.name, "ok");
      onJobDone?.(job);
    } catch (e) {
      const msg = e instanceof Error ? e.message : String(e);
      job.state = msg === "cancelled" ? "cancelled" : "failed";
      job.message = msg;
      if (job.state === "failed") toast(job.file.name + ": " + msg, "bad");
    }
    onQueueChange?.();
  }
  running = false;
}
function render7(root, dir) {
  let sort = loadSort();
  let entries = [];
  let formOpen = false;
  const refresh = async () => {
    try {
      const r = await ls(dir);
      entries = r.entries;
      draw();
    } catch (e) {
      clear(root);
      const msg = e instanceof Error ? e.message : String(e);
      root.append(h("section", null, crumbs(dir), h("div", { class: "error" }, `Could not list ${dir}: ${msg}. `, h("a", { href: "#", onClick: (ev) => {
        ev.preventDefault();
        void refresh();
      } }, "Retry"))));
    }
  };
  const draw = () => {
    clear(root);
    const section = h("section", null, crumbs(dir));
    const input = h("input", { type: "file", multiple: true, "aria-label": "upload files" });
    input.addEventListener("change", () => {
      if (input.files?.length) enqueue(dir, input.files);
      input.value = "";
    });
    const total = entries.reduce((s, e) => s + (e.type === "file" ? e.size : 0), 0);
    const toolbar = h(
      "div",
      { class: "toolbar" },
      h("label", { class: "btn" }, "Upload", input),
      h("button", { type: "button", "aria-pressed": formOpen ? "true" : "false", onClick: () => {
        formOpen = !formOpen;
        draw();
      } }, "New folder"),
      h("span", { class: "spacer" }),
      h("span", { class: "small dim" }, `${entries.length} ${entries.length === 1 ? "item" : "items"} \xB7 ${bytes(total)}`)
    );
    section.append(toolbar);
    const shown = queue.filter((j) => j.state !== "done");
    if (shown.length) {
      const list = h("div", { class: "uploads" });
      for (const j of shown) list.append(jobRow(j));
      section.append(list);
    }
    if (formOpen) section.append(folderForm());
    if (entries.length === 0) section.append(h("div", { class: "empty" }, "empty folder"));
    else section.append(table());
    root.append(section);
  };
  const folderForm = () => {
    const name = h("input", { type: "text", placeholder: "folder name", "aria-label": "folder name", autofocus: true });
    const form = h("form", { class: "inline-form" }, icon("dir"), name, h("button", { type: "submit" }, "Create"), h("button", { type: "button", onClick: () => {
      formOpen = false;
      draw();
    } }, "Cancel"));
    form.addEventListener("submit", (ev) => {
      ev.preventDefault();
      const n = name.value.trim();
      if (!n || n.includes("/") || n === "." || n === "..") {
        toast("A folder name cannot be empty or contain /", "bad");
        return;
      }
      mkdir(join(dir, n)).then(
        () => {
          toast("Created " + n, "ok");
          formOpen = false;
          void refresh();
        },
        (e) => toast("mkdir " + n + ": " + (e instanceof ApiError ? e.message : String(e)), "bad")
      );
    });
    form.addEventListener("keydown", (ev) => {
      if (ev.key === "Escape") {
        formOpen = false;
        draw();
      }
    });
    queueMicrotask(() => name.focus());
    return form;
  };
  const table = () => {
    const sorted = [...entries].sort((a, b) => compare(a, b, sort));
    const th = (key, label, cls) => {
      const attrs = { class: cls ?? "" };
      if (sort.key === key) attrs["aria-sort"] = sort.desc ? "descending" : "ascending";
      const el = h("th", attrs, label);
      el.addEventListener("click", () => {
        sort = sort.key === key ? { key, desc: !sort.desc } : { key, desc: key !== "name" };
        saveSort(sort);
        draw();
      });
      return el;
    };
    const t = h("table", { class: "files" }, h("thead", null, h("tr", null, th("name", "name"), th("size", "size", "size"), th("mtime", "modified", "mtime"))));
    const body = h("tbody");
    for (const e of sorted) body.append(row2(e));
    t.append(body);
    return t;
  };
  const row2 = (e) => {
    const path = join(dir, e.name);
    let link;
    if (e.type === "dir") link = h("a", { href: "#/browse" + encodePathForHash(path) }, icon("dir"), e.name);
    else if (e.type === "symlink") link = h("span", { class: "linkname" }, icon("link"), e.name + " ", h("span", { class: "target" }, "\u2192 " + (e.target ?? "")));
    else if (e.upload) link = h("span", { class: "dim", title: "an upload in progress, or one that was not resumed" }, icon("file"), e.name);
    else link = h("a", { href: "#/file" + encodePathForHash(path) }, icon(iconFor(e.name)), e.name);
    return h(
      "tr",
      { class: e.upload ? "dim" : e.dirty ? "dirty" : "" },
      h("td", { class: "name" }, link),
      h("td", { class: "size" }, e.type === "file" ? bytes(e.size) : ""),
      h("td", { class: "mtime", title: e.mtime }, when(e.mtime))
    );
  };
  const jobRow = (j) => {
    const state = h("span", { class: "state" + (j.state === "failed" ? " bad" : j.state === "done" ? " ok" : "") });
    if (j.state === "uploading") state.append(`${bytes(j.sent)} / ${bytes(j.file.size)} `, j.note ? j.note + " " : "", h("button", { type: "button", onClick: () => j.handle?.abort() }, "cancel"));
    else if (j.state === "queued") state.append("queued ", h("button", { type: "button", onClick: () => {
      j.state = "cancelled";
      onQueueChange?.();
    } }, "cancel"));
    else if (j.state === "failed") state.append("failed: " + (j.message ?? ""));
    else state.append(j.state);
    const el = h("div", { class: "upload" }, h("span", { class: "name" }, j.file.name), state);
    if (j.state === "uploading") {
      const b = h("div", { class: "bar" });
      const fill = h("i");
      setWidth(fill, j.file.size ? j.sent / j.file.size : 1);
      b.append(fill);
      el.append(b);
    }
    return el;
  };
  const onDragOver = (ev) => {
    ev.preventDefault();
    root.querySelector("table.files")?.classList.add("drop");
  };
  const onDragLeave = () => root.querySelector("table.files")?.classList.remove("drop");
  const onDrop = (ev) => {
    ev.preventDefault();
    onDragLeave();
    const files = Array.from(ev.dataTransfer?.files ?? []);
    const plain = files.filter((f) => f.type !== "" || f.size > 0);
    if (plain.length < files.length) toast("Folders are not supported; use New folder", "bad");
    if (plain.length) enqueue(dir, plain);
  };
  root.addEventListener("dragover", onDragOver);
  root.addEventListener("dragleave", onDragLeave);
  root.addEventListener("drop", onDrop);
  onQueueChange = draw;
  onJobDone = (j) => {
    if (j.dir === dir) void refresh();
  };
  void refresh();
  return () => {
    onQueueChange = null;
    onJobDone = null;
    root.removeEventListener("dragover", onDragOver);
    root.removeEventListener("dragleave", onDragLeave);
    root.removeEventListener("drop", onDrop);
  };
}
function crumbs(path) {
  const el = h("div", { class: "crumbs" }, h("a", { href: "#/browse/" }, "/"));
  const parts = path.split("/").filter(Boolean);
  let acc = "";
  parts.forEach((p, i) => {
    acc += "/" + p;
    if (i > 0) el.append(h("span", { class: "sep" }, "/"));
    if (i === parts.length - 1) el.append(h("span", null, p));
    else el.append(h("a", { href: "#/browse" + encodePathForHash(acc) }, p));
  });
  return el;
}
function crumbsAll(path) {
  const el = h("div", { class: "crumbs" }, h("a", { href: "#/browse/" }, "/"));
  const parts = path.split("/").filter(Boolean);
  let acc = "";
  parts.forEach((p, i) => {
    acc += "/" + p;
    if (i > 0) el.append(h("span", { class: "sep" }, "/"));
    el.append(h("a", { href: "#/browse" + encodePathForHash(acc) }, p));
  });
  return el;
}
function iconFor(name) {
  const k = mediaKind(ext(name));
  if (k === "image") return "image";
  if (k === "video" || k === "audio") return "media";
  return "file";
}
function compare(a, b, s) {
  const da = a.type === "dir" ? 0 : 1;
  const db = b.type === "dir" ? 0 : 1;
  if (da !== db) return da - db;
  let c = 0;
  if (s.key === "size") c = a.size - b.size;
  else if (s.key === "mtime") c = a.mtime.localeCompare(b.mtime);
  if (c === 0) c = a.name.localeCompare(b.name, void 0, { numeric: true, sensitivity: "base" });
  return s.desc ? -c : c;
}

// internal/webui/src/main.ts
var view = document.getElementById("view");
var pill = document.getElementById("health");
var who = document.getElementById("who");
var warnings = document.getElementById("warnings");
var tabs = Array.from(document.querySelectorAll("nav.tabs a"));
var TIPS = {
  healthy: "leader reachable, live \u2265 k+m: metadata, reads and writes normal; full placement",
  degraded: "leader reachable, k \u2264 live < k+m: reads OK if k holders live; writes degraded (\u2265 k shards); 0 slots filled as soon as a spare is live, dead-node slots re-placed after fill_after",
  unreadable: "leader reachable, live < k: EIO for affected chunks; flush fails; WAL buffers up to 1 GiB then EIO",
  stale: "no leader contact for more than 30 s: metadata served from the stale local copy; data writes buffer in the WAL until the caps, then EIO; metadata mutations EROFS",
  "no-leader": "no quorum: metadata served from the stale local copy; data writes buffer in the WAL until the caps, then EIO; metadata mutations EROFS",
  unreachable: "the node did not answer /api/status"
};
function healthPill(r, error) {
  if (!r) return { label: "unreachable", cls: "bad", title: TIPS.unreachable + (error ? ": " + error : "") };
  if (r.health === "no-leader") return { label: "no leader", cls: "bad", title: TIPS["no-leader"] };
  if (r.health === "stale") return { label: "stale", cls: "warn", title: TIPS.stale + (r.last_contact ? " \xB7 last contact " + r.last_contact : "") };
  const live = r.nodes.filter((n) => n.state === "active" && n.live).length;
  const { k, m } = r.volume;
  if (r.volume.unreadable_chunks > 0 || live < k) return { label: "unreadable", cls: "bad", title: TIPS.unreadable };
  if (r.volume.degraded_chunks > 0 || live < k + m) return { label: "degraded", cls: "warn", title: TIPS.degraded };
  return { label: "healthy", cls: "ok", title: TIPS.healthy };
}
function drawShell() {
  const r = status.current;
  const p = healthPill(r, status.error);
  pill.className = "pill " + p.cls;
  pill.textContent = p.label;
  pill.title = p.title;
  clear(who);
  if (r) {
    const self = r.nodes.find((n) => n.self)?.name ?? r.node;
    const leader = r.nodes.find((n) => n.leader)?.name ?? r.leader;
    who.append(`${self} \xB7 leader ${leader || "\u2014"} \xB7 ${r.live_voters}/${r.voters} voters live`);
  }
  clear(warnings);
  for (const w of r?.warnings ?? []) warnings.append(h("div", { class: "warn-strip" }, w));
}
var dispose = null;
function route() {
  const hash = location.hash || "#/";
  const [head = "", ...rest] = hash.replace(/^#\/?/, "").split("/");
  const tail = rest.join("/");
  dispose?.();
  dispose = null;
  clear(view);
  let tab = "dash";
  if (head === "browse") {
    tab = "files";
    dispose = render7(view, decodeHashPath(tail));
  } else if (head === "file") {
    tab = "files";
    dispose = render6(view, decodeHashPath(tail));
  } else {
    dispose = render4(view);
  }
  for (const a of tabs) a.classList.toggle("active", a.dataset.tab === tab);
  window.scrollTo(0, 0);
}
status.subscribe(drawShell);
window.addEventListener("hashchange", route);
route();
export {
  healthPill
};
