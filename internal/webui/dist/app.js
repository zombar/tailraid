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

// internal/webui/src/theme.ts
var KEY = "theme";
var NEXT = { system: "light", light: "dark", dark: "system" };
function stored() {
  try {
    const v = localStorage.getItem(KEY);
    return v === "light" || v === "dark" ? v : "system";
  } catch {
    return "system";
  }
}
function apply(t) {
  if (t === "system") document.documentElement.removeAttribute("data-theme");
  else document.documentElement.setAttribute("data-theme", t);
  try {
    if (t === "system") localStorage.removeItem(KEY);
    else localStorage.setItem(KEY, t);
  } catch {
  }
}
function next(t) {
  return NEXT[t];
}
var GLYPHS = {
  system: () => svg("svg", { class: "icon", viewBox: "0 0 16 16", "aria-hidden": "true" }, svg("circle", { cx: 8, cy: 8, r: 6 }), svg("path", { d: "M8 2a6 6 0 0 1 0 12z", fill: "currentColor", stroke: "none" })),
  light: () => svg("svg", { class: "icon", viewBox: "0 0 16 16", "aria-hidden": "true" }, svg("circle", { cx: 8, cy: 8, r: 3 }), svg("path", { d: "M8 1.5v2M8 12.5v2M1.5 8h2M12.5 8h2M3.4 3.4l1.4 1.4M11.2 11.2l1.4 1.4M3.4 12.6l1.4-1.4M11.2 4.8l1.4-1.4" })),
  dark: () => svg("svg", { class: "icon", viewBox: "0 0 16 16", "aria-hidden": "true" }, svg("path", { d: "M13.5 9.8A6 6 0 0 1 6.2 2.5a6 6 0 1 0 7.3 7.3z" }))
};
function wire(button) {
  const draw = (t) => {
    const label = `Theme: ${t} (tap for ${next(t)})`;
    button.title = label;
    button.setAttribute("aria-label", label);
    button.replaceChildren(GLYPHS[t](), Object.assign(document.createElement("span"), { textContent: t }));
  };
  let current = stored();
  draw(current);
  button.addEventListener("click", () => {
    current = next(current);
    apply(current);
    draw(current);
  });
}
apply(stored());

// internal/webui/src/api.ts
var ApiError = class extends Error {
  constructor(status2, message, body) {
    super(message);
    this.status = status2;
    this.body = body;
  }
  status;
  body;
};
var HEADER = "X-Tailraid-UI";
function encodePath(p) {
  return p.split("/").map(encodeURIComponent).join("/");
}
function inQuery(root, first = false) {
  return root === void 0 ? "" : (first ? "?" : "&") + "in=" + root;
}
function fileURL(path, download = false, root) {
  const q = [];
  if (download) q.push("download=1");
  if (root !== void 0) q.push("in=" + root);
  return "/files" + encodePath(path) + (q.length ? "?" + q.join("&") : "");
}
function thumbURL(path, root) {
  return "/api/thumb?path=" + encodeURIComponent(path) + inQuery(root);
}
async function call(url, init) {
  const r = await fetch(url, init);
  if (!r.ok) throw await apiError(r);
  if (r.status === 204) return void 0;
  return await r.json();
}
async function apiError(r) {
  try {
    const body = await r.json();
    if (typeof body.error === "string") return new ApiError(r.status, `${body.error} (${r.status})`, body);
  } catch {
  }
  return new ApiError(r.status, `${r.statusText || "request failed"} (${r.status})`);
}
function post(route2, body) {
  return call(route2, { method: "POST", headers: { [HEADER]: "1", "Content-Type": "application/json" }, body: JSON.stringify(body) });
}
var getStatus = () => call("/api/status");
var ls = (path, root) => call("/api/ls?path=" + encodeURIComponent(path) + inQuery(root));
var stat = (path, root) => call("/api/stat?path=" + encodeURIComponent(path) + inQuery(root));
var trashList = () => call("/api/trash");
var chunks = (path, offset, limit) => call(`/api/chunks?path=${encodeURIComponent(path)}&offset=${offset}&limit=${limit}`);
var tags = (path, root) => call("/api/tags?path=" + encodeURIComponent(path) + inQuery(root));
var getPins = () => call("/api/pins");
var mkdir = (path) => post("/api/mkdir", { path });
var rename = (path, name) => post("/api/rename", { path, name });
var move = (paths, to) => post("/api/move", { paths, to });
var del = (paths) => post("/api/delete", { paths });
var restore = (ino, opts = {}) => post("/api/restore", { ino, ...opts });
var purge = (sel) => post("/api/purge", sel);
var setFlags = (path, flags) => post("/api/flags", { path, ...flags });
var pin = (path, pinned) => post("/api/pin", { path, pinned });
async function putThumb(path, version, jpeg) {
  const r = await fetch(`/api/thumb?path=${encodeURIComponent(path)}&version=${version}`, { method: "PUT", headers: { [HEADER]: "1", "Content-Type": "image/jpeg" }, body: jpeg });
  if (!r.ok) throw await apiError(r);
}
async function text(path, root) {
  const r = await fetch(fileURL(path, false, root));
  if (!r.ok) throw await apiError(r);
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
var getStats = (from, to, node) => call(`/api/stats?from=${from}&to=${to}&node=${encodeURIComponent(node)}`);

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
function mmss(sec) {
  if (sec === void 0 || !Number.isFinite(sec) || sec < 0) return "\u2014";
  const s = Math.round(sec);
  const h2 = Math.floor(s / 3600);
  const m = Math.floor(s % 3600 / 60);
  const r = String(s % 60).padStart(2, "0");
  return h2 > 0 ? `${h2}:${String(m).padStart(2, "0")}:${r}` : `${m}:${r}`;
}
function days(ms) {
  const d = Math.round(Math.abs(ms) / 864e5);
  return d < 1 ? "today" : `${d} d`;
}
function daysAgo(iso, now = Date.now()) {
  if (!iso) return "";
  const t = Date.parse(iso);
  if (Number.isNaN(t)) return iso;
  const d = days(now - t);
  return d === "today" ? d : d + " ago";
}
function daysUntil(iso, now = Date.now()) {
  if (!iso) return "";
  const t = Date.parse(iso);
  if (Number.isNaN(t)) return iso;
  const d = days(t - now);
  return d === "today" ? d : "in " + d;
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
function browseHash(ctx) {
  return ctx.root === void 0 ? "#/browse" + encodePathForHash(ctx.path) : `#/trash/${ctx.root}` + encodePathForHash(ctx.path);
}
function fileHash(ctx) {
  return ctx.root === void 0 ? "#/file" + encodePathForHash(ctx.path) : `#/trashfile/${ctx.root}` + encodePathForHash(ctx.path);
}
var session = {
  get(key, fallback) {
    try {
      const s = sessionStorage.getItem(key);
      if (s) return JSON.parse(s);
    } catch {
    }
    return fallback;
  },
  set(key, v) {
    try {
      sessionStorage.setItem(key, JSON.stringify(v));
    } catch {
    }
  }
};
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
  return nodes.filter((n) => n.kind !== "disk" && n.state !== "removed").map((n) => {
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
  if (n.lost) badges.append(h("span", { class: "badge bad", title: "dead longer than evict_after: remove it" }, "lost"));
  const dot = h("span", { class: "dot " + (n.live ? "ok" : "bad"), title: n.live ? "live" : "dead for " + (n.dead_for ?? "?") });
  const name = h("div", { class: "name-row" }, dot, h("span", { class: "name" }, n.name), badges);
  let capacity;
  if (n.s3) {
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
  const meta = h("div", { class: "meta" }, h("span", null, "rtt " + (n.rtt ?? "\u2014")));
  if (n.s3) {
    meta.append(h("span", null, n.s3.prefix ? `${n.s3.bucket}/${n.s3.prefix}` : n.s3.bucket));
    if (n.s3.region) meta.append(h("span", null, n.s3.region));
  } else {
    meta.append(h("span", null, n.version || "?"), h("span", null, "applied " + count(n.applied)), h("span", null, `${n.addr}:${n.rpc_port}`));
  }
  meta.append(h("span", null, "joined " + shortDate(n.joined_at)));
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
  const active2 = groups.filter((g) => g.targets.some((t) => t.state === "active" && t.capacity > 0));
  const perHost = targets.length / Math.max(1, groups.length);
  const layout = Number.isInteger(perHost) ? `${plural(groups.length, "host")} \xD7 ${plural(perHost, "target")}` : `${plural(groups.length, "host")}, ${plural(targets.length, "target")}`;
  const sub = `survives ${plural(v.tolerates_disks, "disk")}, ${plural(v.tolerates_hosts, "host")} \xB7 ${v.auto ? "auto" : "manual"} ${v.k}+${v.m} \xB7 ${layout}`;
  const n = v.k + v.m;
  const capPer = Math.ceil(n / Math.max(1, active2.length));
  const capNote = v.domain_skew > 0 ? h("div", { class: "cap-note warn" }, `${count(v.domain_skew)} chunks above the host cap (${capPer} of ${n} slots per host) \xB7 repair is moving them`) : h("div", { class: "cap-note" }, active2.length > 1 ? `no chunk above the host cap (${capPer} of ${n} slots per host)` : `one host: every slot is on ${active2[0]?.node.name ?? "it"}`);
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
    const note2 = h("div", { class: "note" }, `Repair runs on the leader. This node (${self}) is a follower. `);
    const leader = r.nodes.find((n) => n.leader || n.id === r.leader);
    if (!r.leader) note2.append("No leader is elected.");
    else if (leader) note2.append(h("a", { href: `http://${leader.addr}:${location.port || "20482"}/` }, `Open ${leader.name}'s dashboard`), " for the repairer's counters.");
    return note2;
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
  const p = r.repair.in_progress;
  if (!p) return h("div", null, h("h3", null, "Repair \xB7 last scan " + ago(r.repair.last_scan)), kv);
  const started = new Date(p.started).toLocaleTimeString([], { hour: "2-digit", minute: "2-digit" });
  const progress = barRow(
    "scanned",
    bar(p.total > 0 ? p.scanned / p.total : 0),
    `${count(p.scanned)} of ${count(p.total)} \xB7 ${p.in_flight} in flight of ${p.workers} workers \xB7 ${bytes(p.bytes_moved)} moved \xB7 started ${started}`
  );
  const note = h("div", { class: "small dim" }, `the pass under way; the counters below are the last completed pass, ${ago(r.repair.last_scan)}`);
  return h("div", null, h("h3", null, `Repair \xB7 pass ${p.pass} running`), progress, note, kv);
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
    cache(root, r);
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
    tile2("Dedup", v.dedup_ratio.toFixed(2) + "\xD7", bytes(v.logical_bytes) + " logical"),
    tile2("Trash", count(v.trash_inodes ?? 0), v.trash_inodes ? `${bytes(v.trash_bytes ?? 0)} \xB7 restore from its folder` : "nothing deleted")
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
function cache(root, r) {
  const c = r.cache;
  if (!c) return;
  const self = r.nodes.find((n) => n.self)?.name ?? r.node;
  const low = !!r.machine?.disk_total && (r.machine.disk_free ?? 0) * 5 < r.machine.disk_total;
  const usedFrac = c.size > 0 ? Math.min(1, (c.used - c.pinned_bytes) / c.size) : 0;
  const card2 = h(
    "div",
    { class: "card cache" + (low ? " warn" : "") },
    h("div", { class: "name-row" }, h("span", { class: "name" }, self), h("span", { class: "badges" }, h("span", { class: "badge self" }, "self"))),
    barRow("used", bar(usedFrac, fillTone(usedFrac)), `${bytes(Math.max(0, c.used - c.pinned_bytes))} of ${bytes(c.size)}`),
    barRow("pinned", bar(c.pinned_bytes > 0 ? 1 : 0, low ? "warn" : ""), `${bytes(c.pinned_bytes)} kept here` + (r.machine?.disk_free ? ` \xB7 ${bytes(r.machine.disk_free)} free on the data disk` : ""))
  );
  if (c.pins.length) {
    const pins = h("div", { class: "pins" });
    for (const p of c.pins) {
      const frac = p.bytes > 0 ? p.cached_bytes / p.bytes : 1;
      pins.append(h("div", { class: "pin-row" }, h("span", { class: "n", title: p.path }, p.path), bar(frac), h("span", { class: "v" }, `${bytes(p.cached_bytes)} of ${bytes(p.bytes)}` + (frac < 1 ? " \xB7 fetching" : ""))));
    }
    card2.append(pins);
  } else card2.append(h("div", { class: "small dim" }, "no folder is kept on this device \xB7 the folder menu has \u201CKeep on this device\u201D"));
  root.append(h("section", null, h("h2", null, "Cache"), h("div", { class: "cards" }, card2)));
}

// internal/webui/src/chunks.ts
var PAGE = 64;
function render5(path, expected) {
  const title = h("span");
  const summaryExtra = h("span", { class: "dim small" });
  const summary = h("summary", null, title, summaryExtra);
  const body = h("div", { class: "body" });
  const panel2 = h("details", { class: "panel" }, summary, body);
  let loaded = 0;
  let total = 0;
  let fetching = false;
  const counts = { committed: 0, degraded: 0, unreadable: 0, tombstoned: 0, stale: 0 };
  const rows = h("div");
  const more = h("button", { type: "button" }, "Load more");
  const caption = h("div", { class: "small dim" });
  const loading = h("div", { class: "loading" });
  const retry = h("button", { type: "button" }, "Retry");
  const failure2 = h("div", { class: "error" });
  const setTitle = (n) => {
    title.textContent = "Integrity" + (n !== void 0 ? ` (${count(n)} ${n === 1 ? "chunk" : "chunks"})` : "");
  };
  setTitle(expected);
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
    failure2.remove();
    const known = total || expected;
    loading.textContent = `loading ${known !== void 0 ? Math.min(PAGE, Math.max(known - loaded, 0)) : PAGE} of ${known !== void 0 ? count(known) : "?"}\u2026`;
    body.append(loading);
    try {
      const page = await chunks(path, loaded, PAGE);
      total = page.total;
      if (expected === void 0) setTitle(total);
      page.chunks.forEach((c, i) => {
        rows.append(row(loaded + i, c));
        if (c.state === "committed") counts.committed++;
        else if (c.state === "degraded") counts.degraded++;
        else if (c.state === "unreadable") counts.unreadable++;
        else if (c.state === "tombstoned") counts.tombstoned++;
        if (c.policy_stale) counts.stale++;
      });
      loaded += page.chunks.length;
      loading.remove();
      if (!body.contains(rows)) body.append(rows, caption, more);
      describe();
      if (total === 0 && !body.querySelector(".empty")) body.append(h("div", { class: "empty" }, "no chunks (empty file)"));
    } catch (e) {
      loading.remove();
      failure2.textContent = "Could not load the chunk reports: " + (e instanceof Error ? e.message : String(e)) + " ";
      failure2.append(retry);
      body.append(failure2);
    } finally {
      fetching = false;
      more.disabled = false;
    }
  };
  more.addEventListener("click", () => void load());
  retry.addEventListener("click", () => void load());
  panel2.addEventListener("toggle", () => {
    if (panel2.open && loaded === 0 && !fetching) void load();
  });
  return {
    el: panel2,
    expect(n) {
      expected = n;
      setTitle(n);
    }
  };
}
function row(i, c) {
  const km = c.k + c.m;
  const live = h("span", { class: "live" + (c.live_slots < c.k ? " bad" : c.live_slots < km ? " warn" : "") }, `${c.live_slots}/${km}`);
  const hash2 = h("span", { title: c.hash }, c.hash.slice(0, 10));
  if (c.policy_stale) hash2.append(" ", h("span", { class: "badge warn" }, "stale"));
  if (!c.found) hash2.append(" ", h("span", { class: "badge bad" }, c.state));
  return h("div", { class: "chunk-row" }, h("span", { class: "dim" }, "#" + i), hash2, h("span", { class: "km dim" }, `${c.k}+${c.m}`), live, slotStrip(c.slots ?? []));
}

// internal/webui/src/picker.ts
function pick(opts) {
  return new Promise((resolve) => {
    const dlg = h("dialog", { class: "picker", "aria-label": "move to" });
    let cur = opts.start;
    const crumbs2 = h("div", { class: "crumbs" });
    const list = h("ul", { class: "picker-list" });
    const confirm = h("button", { type: "button" }, "");
    const cancel = h("button", { type: "button" }, "Cancel");
    dlg.append(crumbs2, list, h("div", { class: "picker-actions" }, cancel, confirm));
    const close = (v) => {
      dlg.close();
      dlg.remove();
      resolve(v);
    };
    cancel.addEventListener("click", () => close(null));
    confirm.addEventListener("click", () => {
      if (!opts.disabled(cur)) close(cur);
    });
    dlg.addEventListener("cancel", (ev) => {
      ev.preventDefault();
      close(null);
    });
    dlg.addEventListener("keydown", (ev) => {
      if (ev.key === "Enter" && !(ev.target instanceof HTMLButtonElement)) {
        ev.preventDefault();
        if (!opts.disabled(cur)) close(cur);
      }
    });
    const draw = async () => {
      crumbs2.replaceChildren(h("a", { href: "#", onClick: (ev) => {
        ev.preventDefault();
        go("/");
      } }, "/"));
      const parts = cur.split("/").filter(Boolean);
      let acc = "";
      parts.forEach((p, i) => {
        acc += "/" + p;
        if (i > 0) crumbs2.append(h("span", { class: "sep" }, "/"));
        const target = acc;
        if (i === parts.length - 1) crumbs2.append(h("span", null, p));
        else crumbs2.append(h("a", { href: "#", onClick: (ev) => {
          ev.preventDefault();
          go(target);
        } }, p));
      });
      const reason = opts.disabled(cur);
      confirm.textContent = opts.action(cur);
      confirm.disabled = reason !== null;
      confirm.title = reason ?? "";
      list.replaceChildren();
      if (cur !== "/") list.append(h("li", { onClick: () => go(dirname(cur)) }, icon("dir"), ".."));
      try {
        const r = await ls(cur);
        for (const e of r.entries) {
          if (e.type !== "dir" || e.deleted_at) continue;
          const path = cur === "/" ? "/" + e.name : cur + "/" + e.name;
          const why = opts.disabled(path);
          const li = h("li", why ? { "aria-disabled": "true", title: why } : null, icon("dir"), e.name);
          if (!why) li.addEventListener("click", () => go(path));
          list.append(li);
        }
      } catch (e) {
        list.append(h("li", { "aria-disabled": "true" }, "could not list: " + (e instanceof Error ? e.message : String(e))));
      }
    };
    const go = (path) => {
      cur = path;
      void draw();
    };
    document.body.append(dlg);
    dlg.showModal();
    void draw();
    void basename;
  });
}

// internal/webui/src/trash.ts
function banner(info) {
  const r = info.root;
  const text2 = h("span");
  const parts = [`Deleted ${daysAgo(r.deleted_at)}`];
  if (r.purge_at) parts.push(`gone for good ${daysUntil(r.purge_at)}`);
  if (info.items !== void 0) parts.push(`${count(info.items)} ${info.items === 1 ? "item" : "items"}`);
  if (info.bytes !== void 0 && info.bytes > 0) parts.push(bytes(info.bytes));
  text2.textContent = parts.join(" \xB7 ");
  const restoreBtn = h("button", { type: "button" }, "Restore");
  const purgeBtn = h("button", { type: "button" }, "Delete now");
  const el = h("div", { class: "banner deleted" }, text2, h("span", { class: "actions" }, restoreBtn, purgeBtn));
  const landed = (path) => {
    toast(`Restored ${basename(path)}`, "ok");
    location.hash = "#/browse" + encodePathForHash(dirname(path));
  };
  const fail = (e) => toast("Restore: " + (e instanceof Error ? e.message : String(e)), "bad");
  const asForm = () => {
    text2.replaceChildren(h("span", { class: "mono" }, r.name), " is taken in ", h("span", { class: "mono" }, basename(dirname(r.path)) || "/"), " \xB7 restore under another name");
    const dot = r.name.lastIndexOf(".");
    const stem = dot > 0 ? r.name.slice(0, dot) : r.name;
    const extn = dot > 0 ? r.name.slice(dot) : "";
    const input = h("input", { type: "text", value: `${stem} (restored)${extn}`, "aria-label": "new name" });
    const form = h("form", { class: "inline-form rename" }, input, h("button", { type: "submit" }, "Restore as"), h("button", { type: "button", onClick: () => el.remove() }, "Cancel"));
    form.addEventListener("submit", (ev) => {
      ev.preventDefault();
      const name = input.value.trim();
      if (!name) return;
      restore(r.ino, { name }).then((res) => landed(res.path), fail);
    });
    el.append(form);
    queueMicrotask(() => {
      input.focus();
      input.setSelectionRange(0, stem.length);
    });
  };
  const elsewhere = async () => {
    const to = await pick({ start: "/", disabled: () => null, action: (p) => `Restore into ${basename(p) || "/"}` });
    if (to === null) return;
    restore(r.ino, { to }).then((res) => landed(res.path), fail);
  };
  restoreBtn.addEventListener("click", () => {
    restoreBtn.disabled = true;
    restore(r.ino).then(
      (res) => landed(res.path),
      (e) => {
        restoreBtn.disabled = false;
        if (e instanceof ApiError && e.status === 409 && e.body?.taken) asForm();
        else if (e instanceof ApiError && e.status === 409 && e.body?.in_trash) void elsewhere();
        else fail(e);
      }
    );
  });
  purgeBtn.addEventListener("click", () => {
    purgeBtn.disabled = true;
    purge({ ino: r.ino }).then(
      () => {
        toast(`Deleted ${r.name} for good`, "ok");
        location.hash = "#/browse" + encodePathForHash(dirname(r.path));
      },
      (e) => {
        purgeBtn.disabled = false;
        toast("Delete now: " + (e instanceof Error ? e.message : String(e)), "bad");
      }
    );
  });
  return el;
}
function badge(purgeAt) {
  const left = purgeAt ? daysUntil(purgeAt).replace(/^in /, "") : "";
  return h("span", { class: "badge" }, left ? `deleted \xB7 ${left}` : "deleted");
}

// internal/webui/src/audio.ts
var KEY2 = "tr.now";
var Player = class {
  queue = [];
  index = -1;
  repeat = "off";
  shuffle = false;
  order = [];
  // the shuffle order over queue indexes
  seed = 1;
  a = document.createElement("audio");
  b = document.createElement("audio");
  preloaded = -1;
  // the queue index b holds
  listeners = /* @__PURE__ */ new Set();
  bar = null;
  queueEl = null;
  els = {};
  saveTimer = 0;
  current() {
    return this.index >= 0 ? this.queue[this.index] ?? null : null;
  }
  get playing() {
    return !this.a.paused && !this.a.ended;
  }
  isCurrent(path, root) {
    const c = this.current();
    return !!c && c.path === path && c.root === root;
  }
  subscribe(fn) {
    this.listeners.add(fn);
    return () => this.listeners.delete(fn);
  }
  /** mount builds the bar once and restores the last session's track, paused. */
  mount() {
    if (this.bar) return;
    for (const el of [this.a, this.b]) {
      el.preload = "metadata";
    }
    this.a.addEventListener("ended", () => this.next(true));
    this.a.addEventListener("timeupdate", () => this.onTime());
    this.a.addEventListener("play", () => this.notify());
    this.a.addEventListener("pause", () => this.notify());
    this.a.addEventListener("loadedmetadata", () => {
      const c = this.current();
      if (c && Number.isFinite(this.a.duration)) c.dur = this.a.duration;
      this.notify();
    });
    this.buildBar();
    const saved = session.get(KEY2, null);
    if (saved && saved.queue.length && saved.index >= 0 && saved.index < saved.queue.length) {
      this.queue = saved.queue;
      this.index = saved.index;
      this.repeat = saved.repeat;
      this.shuffle = saved.shuffle;
      this.seed = saved.seed;
      this.reshuffle();
      this.load(this.index, false, saved.pos);
    }
    document.addEventListener("keydown", (ev) => this.keys(ev));
    this.mediaSession();
    this.notify();
  }
  /** play starts the queue at index. */
  play(tracks, index = 0) {
    if (!tracks.length) return;
    this.queue = tracks.slice();
    this.index = Math.max(0, Math.min(index, tracks.length - 1));
    this.seed = Date.now() % 2147483647 || 1;
    this.reshuffle();
    this.load(this.index, true);
  }
  /** enqueue appends tracks; with nothing playing it starts them. */
  enqueue(tracks) {
    if (!tracks.length) return;
    if (this.index < 0) {
      this.play(tracks);
      return;
    }
    this.queue.push(...tracks);
    this.reshuffle();
    this.notify();
  }
  /** playNow plays one track: in its place when it is queued, else as a queue of one. */
  playNow(t) {
    const i = this.queue.findIndex((q) => q.path === t.path && q.root === t.root);
    if (i >= 0) this.load(i, true);
    else this.play([t]);
  }
  toggle() {
    if (this.index < 0) return;
    if (this.a.paused) void this.a.play().catch(() => void 0);
    else this.a.pause();
  }
  pause() {
    this.a.pause();
  }
  next(auto = false) {
    if (this.index < 0) return;
    if (auto && this.repeat === "one") {
      this.a.currentTime = 0;
      void this.a.play().catch(() => void 0);
      return;
    }
    const pos = this.order.indexOf(this.index);
    let nextPos = pos + 1;
    if (nextPos >= this.order.length) {
      if (this.repeat === "all" || !auto) nextPos = 0;
      else {
        this.notify();
        return;
      }
    }
    this.load(this.order[nextPos], true);
  }
  prev() {
    if (this.index < 0) return;
    if (this.a.currentTime > 3) {
      this.a.currentTime = 0;
      return;
    }
    const pos = this.order.indexOf(this.index);
    this.load(this.order[(pos - 1 + this.order.length) % this.order.length], true);
  }
  seek(sec) {
    if (!Number.isFinite(sec)) return;
    this.a.currentTime = Math.max(0, sec);
  }
  cycleRepeat() {
    this.repeat = this.repeat === "off" ? "all" : this.repeat === "all" ? "one" : "off";
    this.notify();
    this.save();
  }
  toggleShuffle() {
    this.shuffle = !this.shuffle;
    this.reshuffle();
    this.notify();
    this.save();
  }
  remove(i) {
    if (i < 0 || i >= this.queue.length) return;
    this.queue.splice(i, 1);
    if (i < this.index) this.index--;
    else if (i === this.index) {
      if (!this.queue.length) this.clear();
      else this.load(Math.min(this.index, this.queue.length - 1), this.playing);
    }
    this.reshuffle();
    this.notify();
  }
  clear() {
    this.a.pause();
    this.a.removeAttribute("src");
    this.a.load();
    this.queue = [];
    this.index = -1;
    this.order = [];
    this.preloaded = -1;
    this.notify();
    this.save();
  }
  reshuffle() {
    this.order = this.queue.map((_, i) => i);
    if (!this.shuffle) return;
    let s = this.seed;
    const rnd = () => {
      s = s * 48271 % 2147483647;
      return s / 2147483647;
    };
    for (let i = this.order.length - 1; i > 0; i--) {
      const j = Math.floor(rnd() * (i + 1));
      [this.order[i], this.order[j]] = [this.order[j], this.order[i]];
    }
    if (this.index >= 0) {
      const p = this.order.indexOf(this.index);
      if (p > 0) this.order.unshift(...this.order.splice(p, 1));
    }
  }
  url(t) {
    return fileURL(t.path, false, t.root);
  }
  load(i, start, pos = 0) {
    const t = this.queue[i];
    if (!t) return;
    this.index = i;
    if (this.preloaded === i && this.b.src) {
      [this.a, this.b] = [this.b, this.a];
      this.rebind();
    } else {
      this.a.src = this.url(t);
    }
    this.preloaded = -1;
    this.b.removeAttribute("src");
    if (pos > 0) this.a.currentTime = pos;
    if (start) void this.a.play().catch(() => void 0);
    void this.describe(t);
    this.notify();
    this.save();
  }
  /** rebind moves the element listeners to the element that is now `a` (after a swap). */
  rebind() {
    const fresh = this.a.cloneNode(false);
    void fresh;
    this.a.onended = () => this.next(true);
    this.a.ontimeupdate = () => this.onTime();
    this.a.onplay = () => this.notify();
    this.a.onpause = () => this.notify();
    this.a.onloadedmetadata = () => {
      const c = this.current();
      if (c && Number.isFinite(this.a.duration)) c.dur = this.a.duration;
      this.notify();
    };
    this.b.onended = null;
    this.b.ontimeupdate = null;
    this.b.onplay = null;
    this.b.onpause = null;
    this.b.onloadedmetadata = null;
  }
  async describe(t) {
    if (t.title === void 0) {
      try {
        const g = await tags(t.path, t.root);
        t.title = g.title || basename(t.path).replace(/\.[^.]+$/, "");
        t.artist = g.artist;
        t.album = g.album;
        t.hasCover = g.has_cover;
      } catch {
        t.title = basename(t.path).replace(/\.[^.]+$/, "");
      }
    }
    if (this.current() === t) {
      this.notify();
      this.mediaSession();
    }
  }
  onTime() {
    const c = this.current();
    if (!c) return;
    const left = (this.a.duration || 0) - this.a.currentTime;
    if (Number.isFinite(left) && left < 15 && this.repeat !== "one") {
      const pos = this.order.indexOf(this.index);
      const n = pos + 1 < this.order.length ? this.order[pos + 1] : this.repeat === "all" ? this.order[0] : -1;
      if (n >= 0 && this.preloaded !== n) {
        this.preloaded = n;
        this.b.preload = "auto";
        this.b.src = this.url(this.queue[n]);
        this.b.load();
      }
    }
    if (this.els.seek) {
      const seek = this.els.seek;
      if (document.activeElement !== seek) {
        seek.max = String(Math.round(this.a.duration || c.dur || 0));
        seek.value = String(Math.round(this.a.currentTime));
      }
      this.els.time.textContent = `${mmss(this.a.currentTime)} / ${mmss(this.a.duration || c.dur)}`;
    }
    if (!this.saveTimer) this.saveTimer = window.setTimeout(() => this.save(), 2e3);
  }
  save() {
    if (this.saveTimer) {
      window.clearTimeout(this.saveTimer);
      this.saveTimer = 0;
    }
    if (this.index < 0) {
      session.set(KEY2, null);
      return;
    }
    const s = { queue: this.queue.map((t) => ({ path: t.path, root: t.root, title: t.title, artist: t.artist, album: t.album, dur: t.dur, hasCover: t.hasCover })), index: this.index, pos: this.a.currentTime || 0, repeat: this.repeat, shuffle: this.shuffle, seed: this.seed };
    session.set(KEY2, s);
  }
  mediaSession() {
    if (!("mediaSession" in navigator)) return;
    const ms = navigator.mediaSession;
    const c = this.current();
    if (!c) {
      ms.metadata = null;
      return;
    }
    ms.metadata = new MediaMetadata({
      title: c.title ?? basename(c.path),
      artist: c.artist ?? "",
      album: c.album ?? "",
      artwork: c.hasCover === false ? [] : [{ src: thumbURL(c.path, c.root), sizes: "320x320", type: "image/jpeg" }]
    });
    ms.setActionHandler("play", () => this.toggle());
    ms.setActionHandler("pause", () => this.pause());
    ms.setActionHandler("previoustrack", () => this.prev());
    ms.setActionHandler("nexttrack", () => this.next());
    ms.setActionHandler("seekto", (d) => this.seek(d.seekTime ?? 0));
    ms.setActionHandler("seekbackward", () => this.seek(this.a.currentTime - 10));
    ms.setActionHandler("seekforward", () => this.seek(this.a.currentTime + 10));
  }
  keys(ev) {
    if (this.index < 0) return;
    const t = ev.target;
    if (t && (t.tagName === "INPUT" || t.tagName === "TEXTAREA" || t.tagName === "SELECT" || t.isContentEditable)) return;
    if (document.querySelector("section.player video, section.player img")) return;
    switch (ev.key) {
      case " ":
        ev.preventDefault();
        this.toggle();
        break;
      case "ArrowLeft":
        ev.preventDefault();
        if (ev.shiftKey) this.prev();
        else this.seek(this.a.currentTime - 10);
        break;
      case "ArrowRight":
        ev.preventDefault();
        if (ev.shiftKey) this.next();
        else this.seek(this.a.currentTime + 10);
        break;
      case "q":
        this.bar?.classList.toggle("expanded");
        this.notify();
        break;
    }
  }
  buildBar() {
    const cover = h("img", { class: "cover", alt: "" });
    cover.addEventListener("error", () => cover.removeAttribute("src"));
    const title = h("div", { class: "title" });
    const sub = h("div", { class: "sub" });
    const prev = h("button", { type: "button", "aria-label": "previous", onClick: () => this.prev() }, "\u23EE");
    const playBtn = h("button", { type: "button", "aria-label": "play", onClick: () => this.toggle() }, "\u25B6");
    const next2 = h("button", { type: "button", "aria-label": "next", onClick: () => this.next() }, "\u23ED");
    const seek = h("input", { type: "range", min: 0, max: 0, value: 0, "aria-label": "seek" });
    seek.addEventListener("input", () => this.seek(Number(seek.value)));
    const time = h("span", { class: "time" }, "0:00 / \u2014");
    const repeat = h("button", { type: "button", "aria-pressed": "false", "aria-label": "repeat", onClick: () => this.cycleRepeat() }, "\u27F3");
    const shuffle = h("button", { type: "button", "aria-pressed": "false", "aria-label": "shuffle", onClick: () => this.toggleShuffle() }, "\u292E");
    const queueBtn = h("button", { type: "button", class: "queue-btn", "aria-pressed": "false", onClick: () => {
      this.bar?.classList.toggle("expanded");
      this.notify();
    } }, "Queue");
    const bar2 = h(
      "aside",
      { class: "nowbar", hidden: true },
      cover,
      h("div", { class: "titles" }, title, sub),
      h("div", { class: "transport" }, prev, playBtn, next2),
      h("div", { class: "seek" }, seek, time),
      h("div", { class: "modes" }, repeat, shuffle),
      queueBtn
    );
    const list = h("ol");
    const summary = h("span", { class: "small dim" });
    const queue3 = h("div", { class: "queue", hidden: true }, h("div", { class: "toolbar" }, summary, h("span", { class: "spacer" }), h("button", { type: "button", onClick: () => this.clear() }, "Clear")), list);
    document.body.append(bar2, queue3);
    this.bar = bar2;
    this.queueEl = queue3;
    this.els = { cover, title, sub, playBtn, seek, time, repeat, shuffle, queueBtn, list, summary };
  }
  notify() {
    this.drawBar();
    for (const fn of this.listeners) fn();
  }
  drawBar() {
    if (!this.bar) return;
    const c = this.current();
    this.bar.hidden = !c;
    if (this.queueEl) this.queueEl.hidden = !c || !this.bar.classList.contains("expanded");
    if (!c) return;
    const e = this.els;
    const cover = e.cover;
    const want = c.hasCover === false ? "" : thumbURL(c.path, c.root);
    if (cover.getAttribute("src") !== want) {
      if (want) cover.src = want;
      else cover.removeAttribute("src");
    }
    e.title.textContent = c.title ?? basename(c.path);
    e.sub.textContent = [c.artist, c.album].filter(Boolean).join(" \u2014 ") || ext(c.path);
    e.playBtn.textContent = this.playing ? "\u23F8" : "\u25B6";
    e.playBtn.setAttribute("aria-label", this.playing ? "pause" : "play");
    e.repeat.setAttribute("aria-pressed", this.repeat === "off" ? "false" : "true");
    e.repeat.setAttribute("aria-label", this.repeat === "one" ? "repeat one" : this.repeat === "all" ? "repeat all" : "repeat");
    e.repeat.textContent = this.repeat === "one" ? "\u27F31" : "\u27F3";
    e.shuffle.setAttribute("aria-pressed", this.shuffle ? "true" : "false");
    const expanded = this.bar.classList.contains("expanded");
    e.queueBtn.setAttribute("aria-pressed", expanded ? "true" : "false");
    e.queueBtn.textContent = `Queue \xB7 ${this.queue.length}`;
    e.time.textContent = `${mmss(this.a.currentTime)} / ${mmss(this.a.duration || c.dur)}`;
    e.summary.textContent = `${this.queue.length} in the queue` + (this.repeat !== "off" ? ` \xB7 repeat ${this.repeat}` : "") + (this.shuffle ? " \xB7 shuffle" : "");
    if (expanded) this.drawQueue();
  }
  drawQueue() {
    const list = this.els.list;
    list.replaceChildren();
    this.order.forEach((qi, n) => {
      const t = this.queue[qi];
      const playing = qi === this.index;
      const li = h(
        "li",
        { class: "qrow" + (playing ? " playing" : "") },
        h("span", { class: "no" }, playing ? "\u25B6" : String(n + 1)),
        h("span", { class: "title", onClick: () => this.load(qi, true) }, t.title ?? basename(t.path)),
        h("span", { class: "artist" }, t.artist ?? ""),
        h("span", { class: "dur" }, mmss(t.dur)),
        h("button", { type: "button", "aria-label": "remove", onClick: () => this.remove(qi) }, "\xD7")
      );
      list.append(li);
    });
  }
};
var player = new Player();
function isAudioName(name) {
  return ["mp3", "m4a", "aac", "ogg", "oga", "opus", "wav", "flac"].includes(ext(name));
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
var panel = null;
function render6(root, ctx) {
  const path = ctx.path;
  const dir = dirname(path);
  const name = basename(path) || "/";
  const kind = mediaKind(ext(name));
  const inTrash = ctx.root !== void 0;
  let siblings = [];
  let entry;
  let trashRoot;
  let nsfw = false;
  let keyHandler = null;
  if (kind === "video") player.pause();
  const draw = () => {
    clear(root);
    const section = h("section", { class: "player" }, crumbsAll({ path: dir, root: ctx.root }));
    if (inTrash && (trashRoot || entry?.deleted_at)) {
      const r = trashRoot ?? { ino: ctx.root, name, type: "file", size: entry?.size ?? 0, path: name, deleted_at: entry?.deleted_at ?? "", restorable: true };
      section.append(banner({ root: { ...r, purge_at: r.purge_at ?? entry?.purge_at }, bytes: entry?.size }));
    }
    const info = entry ? `${bytes(entry.size)} \xB7 ${when(entry.mtime)}` : "";
    section.append(
      h(
        "div",
        { class: "title-row" },
        h("h1", null, name),
        h("span", { class: "small dim mono" }, info),
        h("span", { class: "actions" }, h("a", { class: "btn", href: fileURL(path, true, ctx.root) }, "Download"))
      )
    );
    const veiled = nsfw && !session.get("tr.revealed", false);
    const viewer = h("div", { class: "viewer" + (nsfw ? " nsfw" : "") + (nsfw && !veiled ? " revealed" : "") });
    section.append(viewer);
    const group = siblings.filter((s) => sameGroup(mediaKind(ext(s.name)), kind));
    const index = group.findIndex((s) => s.name === name);
    const prev = index > 0 ? group[index - 1] : void 0;
    const next2 = index >= 0 && index < group.length - 1 ? group[index + 1] : void 0;
    const go = (e) => {
      if (e) location.hash = fileHash({ path: join(dir, e.name), root: ctx.root });
    };
    if (kind === "video") {
      const mime = KINDS[ext(name)]?.[1] ?? "";
      let el;
      if (media && media.dir === dir) {
        el = media.el;
        el.src = fileURL(path, false, ctx.root);
        void el.play().catch(() => void 0);
      } else {
        media?.el.pause();
        el = document.createElement("video");
        el.controls = true;
        el.preload = "metadata";
        el.playsInline = true;
        el.src = fileURL(path, false, ctx.root);
        media = { el, dir };
      }
      el.onended = () => go(next2);
      viewer.append(el);
      if (mime && el.canPlayType(mime) === "") {
        section.append(
          h("div", { class: "note" }, "Your browser reports it cannot play ", h("span", { class: "mono" }, mime), ". tailraid serves files as stored, without transcoding; mkv, avi and flac play only where the browser has a native decoder. ", h("a", { href: fileURL(path, true, ctx.root) }, "Download instead"), ".")
        );
      }
    } else if (kind === "audio") {
      viewer.append(audioCard(ctx, name, entry, group));
    } else if (kind === "image") {
      viewer.append(h("img", { src: fileURL(path, false, ctx.root), alt: name }));
      if (next2) new Image().src = fileURL(join(dir, next2.name), false, ctx.root);
    } else if (kind === "text") {
      if (entry && entry.size > TEXT_MAX) {
        viewer.append(h("div", { class: "placeholder" }, "too large to preview", h("a", { class: "btn", href: fileURL(path, true, ctx.root) }, "Download")));
      } else {
        const pre = h("pre", null, "loading\u2026");
        viewer.append(pre);
        text(path, ctx.root).then(
          (t) => pre.textContent = t,
          (e) => pre.textContent = "could not load: " + (e instanceof Error ? e.message : String(e))
        );
      }
    } else if (kind === "pdf") {
      viewer.append(h("iframe", { src: fileURL(path, false, ctx.root), title: name }));
      section.append(h("div", { class: "small dim" }, h("a", { href: fileURL(path, false, ctx.root), target: "_blank", rel: "noopener" }, "Open in a new tab")));
    } else {
      viewer.append(h("div", { class: "placeholder" }, info || name, h("a", { class: "btn", href: fileURL(path, true, ctx.root) }, "Download")));
    }
    if (nsfw) {
      viewer.append(h("span", { class: "veil" }, "nsfw \xB7 tap to show"));
      viewer.addEventListener("click", () => {
        viewer.classList.add("revealed");
        session.set("tr.revealed", true);
      });
    }
    if (group.length > 1 && kind !== "text" && kind !== "other" && kind !== "pdf") {
      const label = kind === "image" ? "image" : "track";
      section.append(
        h(
          "div",
          { class: "controls" },
          h("button", { type: "button", disabled: !prev, onClick: () => go(prev) }, "\u25C0 Previous"),
          h("button", { type: "button", disabled: !next2, onClick: () => go(next2) }, "Next \u25B6"),
          h("span", { class: "track" }, `${label} ${index + 1} of ${group.length}` + (next2 ? ` \xB7 next: ${next2.name}` : ""))
        )
      );
    }
    if (!inTrash) {
      const cs = status.current?.volume.chunk_size;
      const expected = entry && cs ? Math.ceil(entry.size / cs) : void 0;
      if (!panel || panel.path !== path) panel = { path, p: render5(path, expected) };
      else if (expected !== void 0) panel.p.expect(expected);
      section.append(panel.p.el);
    }
    root.append(section);
    if (keyHandler) document.removeEventListener("keydown", keyHandler);
    keyHandler = (ev) => {
      const t = ev.target;
      if (t && (t.tagName === "INPUT" || t.tagName === "TEXTAREA" || t.tagName === "VIDEO" || t.tagName === "AUDIO")) return;
      if (kind === "audio") return;
      if (ev.key === "ArrowRight") go(next2);
      else if (ev.key === "ArrowLeft") go(prev);
      else if (ev.key === "Escape") location.hash = inTrash ? `#/trash/${ctx.root}` + encodeForHash(dir) : "#/browse" + encodeForHash(dir);
      else if (ev.key === " " && media) {
        ev.preventDefault();
        if (media.el.paused) void media.el.play().catch(() => void 0);
        else media.el.pause();
      }
    };
    document.addEventListener("keydown", keyHandler);
  };
  draw();
  const listing = inTrash && dir === "/" && name !== "/" ? ls("/", ctx.root) : ls(dir, ctx.root);
  listing.then(
    (r) => {
      siblings = r.entries.filter((e) => e.type === "file" && !e.upload && !e.deleted_at);
      entry = siblings.find((e) => e.name === name);
      nsfw = r.dir.nsfw || !!entry?.nsfw;
      draw();
    },
    (e) => {
      if (inTrash) {
        stat("/", ctx.root).then(
          (st) => {
            entry = st;
            draw();
          },
          () => toast("Could not read the deleted file: " + (e instanceof Error ? e.message : String(e)), "bad")
        );
      } else toast("Could not list the folder: " + (e instanceof Error ? e.message : String(e)), "bad");
    }
  );
  if (inTrash) {
    trashList().then(
      (r) => {
        trashRoot = r.entries.find((t) => t.ino === ctx.root);
        draw();
      },
      () => void 0
    );
  }
  return () => {
    if (keyHandler) document.removeEventListener("keydown", keyHandler);
    panel = null;
    const leaving = !location.hash.startsWith("#/file") && !location.hash.startsWith("#/trashfile");
    if (leaving && media) {
      media.el.pause();
      media.el.removeAttribute("src");
      media.el.load();
      media = null;
    }
  };
}
function audioCard(ctx, name, entry, group) {
  const img = h("img", { class: "cover", alt: "" });
  img.src = thumbURL(ctx.path, ctx.root);
  img.addEventListener("error", () => img.removeAttribute("src"));
  const title = h("h1", null, name.replace(/\.[^.]+$/, ""));
  const artist = h("div", { class: "artist" });
  const meta = h("div", { class: "meta" });
  const dirPath = dirname(ctx.path);
  const tracks = group.filter((e) => mediaKind(ext(e.name)) === "audio").map((e) => ({ path: join(dirPath, e.name), root: ctx.root }));
  const index = Math.max(0, tracks.findIndex((t) => t.path === ctx.path));
  const play = h("button", { type: "button", onClick: () => tracks.length ? player.play(tracks, index) : player.playNow({ path: ctx.path, root: ctx.root }) }, player.isCurrent(ctx.path, ctx.root) && player.playing ? "\u23F8 Playing" : "\u25B6 Play");
  const queueBtn = h("button", { type: "button", onClick: () => player.enqueue([{ path: ctx.path, root: ctx.root }]) }, "Add to queue");
  const head = h("div", { class: "album-head" }, img, h("div", null, title, artist, meta, h("div", { class: "controls" }, play, queueBtn)));
  tags(ctx.path, ctx.root).then(
    (g) => {
      if (g.title) title.textContent = g.title;
      artist.textContent = [g.artist, g.album].filter(Boolean).join(" \u2014 ");
      const parts = [];
      if (g.track) parts.push(`track ${g.track}` + (g.tracks ? ` of ${g.tracks}` : ""));
      if (g.year) parts.push(String(g.year));
      if (g.genre) parts.push(g.genre);
      if (entry) parts.push(ext(name), bytes(entry.size));
      const c = player.current();
      if (c && c.path === ctx.path && c.dur) parts.push(mmss(c.dur));
      meta.replaceChildren(...parts.map((p) => h("span", null, p)));
    },
    () => void 0
  );
  return h("section", { class: "album" }, head);
}
function sameGroup(a, b) {
  if (a === b) return true;
  return (a === "video" || a === "audio") && (b === "video" || b === "audio");
}
function encodeForHash(path) {
  return path.split("/").map(encodeURIComponent).join("/");
}

// internal/webui/src/select.ts
var Selection = class {
  set = /* @__PURE__ */ new Set();
  anchor = null;
  listeners = /* @__PURE__ */ new Set();
  get size() {
    return this.set.size;
  }
  has(name) {
    return this.set.has(name);
  }
  names() {
    return Array.from(this.set);
  }
  /** click toggles one name; with shift it selects the range from the last click in order. */
  click(name, shift, order) {
    if (shift && this.anchor !== null) {
      const a = order.indexOf(this.anchor);
      const b = order.indexOf(name);
      if (a >= 0 && b >= 0) {
        for (const n of order.slice(Math.min(a, b), Math.max(a, b) + 1)) this.set.add(n);
        this.notify();
        return;
      }
    }
    if (this.set.has(name)) this.set.delete(name);
    else this.set.add(name);
    this.anchor = name;
    this.notify();
  }
  all(names) {
    for (const n of names) this.set.add(n);
    this.notify();
  }
  clear() {
    this.set.clear();
    this.anchor = null;
    this.notify();
  }
  /** keep drops names that are no longer listed (after a refresh). */
  keep(names) {
    let changed = false;
    for (const n of this.set) {
      if (!names.has(n)) {
        this.set.delete(n);
        changed = true;
      }
    }
    if (changed) this.notify();
  }
  subscribe(fn) {
    this.listeners.add(fn);
    return () => this.listeners.delete(fn);
  }
  notify() {
    for (const fn of this.listeners) fn();
  }
};

// internal/webui/src/thumbs.ts
var CONCURRENT = 3;
var WIDTH = 320;
var queue = [];
var active = 0;
var observer = typeof IntersectionObserver === "undefined" ? null : new IntersectionObserver(
  (entries) => {
    for (const e of entries) {
      if (!e.isIntersecting) continue;
      observer?.unobserve(e.target);
      const p = waiting.get(e.target);
      if (p) {
        waiting.delete(e.target);
        queue.push(p);
      }
    }
    pump();
  },
  { rootMargin: "200px" }
);
var waiting = /* @__PURE__ */ new Map();
function pump() {
  while (active < CONCURRENT && queue.length) {
    const p = queue.shift();
    active++;
    void p.load().finally(() => {
      active--;
      pump();
    });
  }
}
function lazy(img, e, path, root, kind, onMiss) {
  const load = () => new Promise((resolve) => {
    const done = () => resolve();
    const show = (src) => {
      img.onload = done;
      img.onerror = () => {
        onMiss();
        done();
      };
      img.src = src;
    };
    if (e.thumb || kind !== "video" || root !== void 0) {
      show(thumbURL(path, root));
      return;
    }
    void grab(path, e).then((ok) => ok ? show(thumbURL(path)) : (onMiss(), done())).catch(() => {
      onMiss();
      done();
    });
  });
  const p = { img, load };
  if (observer) {
    waiting.set(img, p);
    observer.observe(img);
  } else {
    queue.push(p);
    pump();
  }
}
async function grab(path, e) {
  const key = `tr.vthumb:${e.ino}.${e.version}`;
  if (session.get(key, false)) return false;
  session.set(key, true);
  const blob = await frame(fileURL(path));
  if (!blob) return false;
  await putThumb(path, e.version, blob);
  return true;
}
function frame(src) {
  return new Promise((resolve) => {
    const v = document.createElement("video");
    v.preload = "metadata";
    v.muted = true;
    v.playsInline = true;
    v.crossOrigin = "anonymous";
    const finish = (b) => {
      v.removeAttribute("src");
      v.load();
      resolve(b);
    };
    const timer = setTimeout(() => finish(null), 2e4);
    v.onerror = () => {
      clearTimeout(timer);
      finish(null);
    };
    v.onloadedmetadata = () => {
      if (!Number.isFinite(v.duration) || v.duration <= 0) {
        clearTimeout(timer);
        finish(null);
        return;
      }
      v.currentTime = v.duration / 2;
    };
    v.onseeked = () => {
      clearTimeout(timer);
      const w = Math.min(WIDTH, v.videoWidth || WIDTH);
      const h2 = Math.max(1, Math.round((v.videoHeight || 9) * (w / (v.videoWidth || 16))));
      const c = document.createElement("canvas");
      c.width = w;
      c.height = h2;
      const g = c.getContext("2d");
      if (!g) {
        finish(null);
        return;
      }
      g.drawImage(v, 0, 0, w, h2);
      c.toBlob((b) => finish(b), "image/jpeg", 0.8);
    };
    v.src = src;
  });
}

// internal/webui/src/flags.ts
function menu(ctx) {
  const body = h("div", { class: "menu-body" });
  const details = h("details", { class: "menu" }, h("summary", null, "Folder"), body);
  const isRoot = ctx.path === "/";
  const row2 = (label, note, checked, disabled, onToggle) => {
    const box = h("input", { type: "checkbox" });
    box.checked = checked;
    const noteEl = h("span", { class: "small dim" }, disabled ?? note);
    const lab = h("label", { class: "menu-row" }, box, h("span", null, label), noteEl);
    if (disabled) {
      box.disabled = true;
      lab.setAttribute("aria-disabled", "true");
    }
    box.addEventListener("change", () => {
      box.disabled = true;
      onToggle(box.checked).then(
        () => ctx.onChange(),
        (e) => {
          box.checked = !box.checked;
          box.disabled = false;
          toast(label + ": " + (e instanceof Error ? e.message : String(e)), "bad");
        }
      );
    });
    return lab;
  };
  const fill = async () => {
    body.replaceChildren(h("div", { class: "loading" }, "loading"));
    let own;
    let pinned;
    if (!isRoot && !ctx.inTrash) {
      try {
        own = (await ls(dirname(ctx.path))).entries.find((e) => e.name === basename(ctx.path) && !e.deleted_at);
      } catch {
      }
      try {
        pinned = (await getPins()).pins.find((p) => p.path === ctx.path);
      } catch {
      }
    }
    const inherited = (eff, mine) => eff && !mine ? "inherited from a parent folder: change it there" : null;
    const why = isRoot ? "not for the root" : ctx.inTrash ? "not for a deleted folder" : null;
    const self = status.current?.nodes.find((n) => n.self)?.name ?? "this device";
    body.replaceChildren(
      row2("Protect", "nothing in it can be deleted, replaced or moved out, from any device", own?.protected ?? ctx.dir.protected, why ?? inherited(ctx.dir.protected, own?.protected), (on) => setFlags(ctx.path, { protected: on })),
      row2("Mark nsfw", "thumbnails and previews are blurred until tapped", own?.nsfw ?? ctx.dir.nsfw, why ?? inherited(ctx.dir.nsfw, own?.nsfw), (on) => setFlags(ctx.path, { nsfw: on })),
      row2(
        "Keep on this device",
        pinned ? `${self} holds every chunk in its cache \xB7 ${bytes(pinned.cached_bytes)} of ${bytes(pinned.bytes)} here` : `${self} fetches every chunk under it into its cache and keeps it there`,
        ctx.dir.pinned,
        why,
        (on) => pin(ctx.path, on)
      )
    );
  };
  details.addEventListener("toggle", () => {
    if (details.open) void fill();
  });
  return details;
}

// internal/webui/src/album.ts
function isAlbum(entries) {
  const live = entries.filter((e) => !e.deleted_at && !e.upload && e.type === "file");
  const audio = live.filter((e) => isAudioName(e.name)).length;
  return audio > 0 && audio > live.length - audio;
}
var PROBES = 4;
function render7(ctx, cover, entries) {
  const audio = entries.filter((e) => !e.deleted_at && !e.upload && e.type === "file" && isAudioName(e.name)).sort((a, b) => trackNo(a.name) - trackNo(b.name) || a.name.localeCompare(b.name, void 0, { numeric: true }));
  const tracks = audio.map((e) => ({ path: join(ctx.path, e.name), root: ctx.root }));
  const img = h("img", { class: "cover", alt: "" });
  img.src = thumbURL(cover ? join(ctx.path, cover) : ctx.path, ctx.root);
  img.addEventListener("error", () => img.removeAttribute("src"));
  const title = h("h1", null, basename(ctx.path) || "/");
  const artist = h("div", { class: "artist" });
  const meta = h("div", { class: "meta" });
  const head = h("div", { class: "album-head" }, img, h("div", null, title, artist, meta));
  const body = h("tbody");
  const table = h("table", { class: "tracks" }, body);
  const el = h("section", { class: "album" }, head, table);
  const info = /* @__PURE__ */ new Map();
  const durs = /* @__PURE__ */ new Map();
  const rows = [];
  const describe = () => {
    const first = info.get(0) ?? Array.from(info.values())[0];
    if (first) {
      if (first.album) title.textContent = first.album;
      artist.textContent = first.album_artist || first.artist || "";
    }
    const parts = [];
    if (first?.year) parts.push(String(first.year));
    parts.push(`${audio.length} ${audio.length === 1 ? "track" : "tracks"}`);
    if (durs.size === audio.length && audio.length) parts.push(mmss(Array.from(durs.values()).reduce((s, d) => s + d, 0)));
    const exts = Array.from(new Set(audio.map((e) => ext(e.name))));
    if (exts.length) parts.push(exts.join(", "));
    meta.replaceChildren(...parts.map((p) => h("span", null, p)));
  };
  const drawRow = (i) => {
    const e = audio[i];
    const t = tracks[i];
    const g = info.get(i);
    const stem = e.name.replace(/\.[^.]+$/, "");
    const no = g?.track || trackNo(e.name) || i + 1;
    const playing = player.isCurrent(t.path, t.root);
    const tr = rows[i];
    tr.className = playing ? "playing" : "";
    const titleCell = h("td", { class: "title" }, g?.title || stem);
    if (g?.title && g.title !== stem) titleCell.append(" ", h("span", { class: "small dim" }, e.name));
    tr.replaceChildren(h("td", { class: "no" }, playing ? "\u25B6" : String(no)), titleCell, h("td", { class: "dur" }, mmss(durs.get(i))));
  };
  audio.forEach((_, i) => {
    const tr = h("tr");
    tr.addEventListener("click", () => player.play(tracks, i));
    rows.push(tr);
    body.append(tr);
    drawRow(i);
  });
  describe();
  let active2 = 0;
  const pending = [];
  const probe = async (i) => {
    const t = tracks[i];
    try {
      const g = await tags(t.path, t.root);
      info.set(i, g);
      t.title = g.title || audio[i].name.replace(/\.[^.]+$/, "");
      t.artist = g.artist;
      t.album = g.album;
      t.hasCover = g.has_cover;
    } catch {
    }
    const d = await duration(fileURL(t.path, false, t.root));
    if (d !== void 0) {
      durs.set(i, d);
      t.dur = d;
    }
    drawRow(i);
    describe();
  };
  const pump2 = () => {
    while (active2 < PROBES && pending.length) {
      const i = pending.shift();
      active2++;
      void probe(i).finally(() => {
        active2--;
        pump2();
      });
    }
  };
  if (typeof IntersectionObserver !== "undefined") {
    const io = new IntersectionObserver(
      (es) => {
        for (const x of es) {
          if (!x.isIntersecting) continue;
          io.unobserve(x.target);
          pending.push(rows.indexOf(x.target));
        }
        pump2();
      },
      { rootMargin: "300px" }
    );
    for (const tr of rows) io.observe(tr);
  } else {
    rows.forEach((_, i) => pending.push(i));
    pump2();
  }
  return {
    el,
    tracks,
    refresh: () => rows.forEach((_, i) => drawRow(i))
  };
}
function trackNo(name) {
  const m = /^\s*(\d{1,3})\b/.exec(name);
  return m ? Number(m[1]) : 0;
}
function duration(src) {
  return new Promise((resolve) => {
    const a = document.createElement("audio");
    a.preload = "metadata";
    const done = (d) => {
      a.removeAttribute("src");
      a.load();
      resolve(d);
    };
    const timer = setTimeout(() => done(void 0), 15e3);
    a.onloadedmetadata = () => {
      clearTimeout(timer);
      done(Number.isFinite(a.duration) ? a.duration : void 0);
    };
    a.onerror = () => {
      clearTimeout(timer);
      done(void 0);
    };
    a.src = src;
  });
}

// internal/webui/src/browser.ts
var queue2 = [];
var running = false;
var onQueueChange = null;
var onJobDone = null;
function enqueue(dir, files) {
  for (const f of Array.from(files)) queue2.push({ dir, file: f, sent: 0, state: "queued" });
  onQueueChange?.();
  void runQueue();
}
async function runQueue() {
  if (running) return;
  running = true;
  for (; ; ) {
    const job = queue2.find((j) => j.state === "queued");
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
function render8(root, ctx) {
  const dir = ctx.path;
  const inTrash = ctx.root !== void 0;
  let sort = session.get("tr.sort", { key: "name", desc: false });
  let view2 = session.get("tr.view", "list");
  let entries = [];
  let info = { protected: false, nsfw: false, pinned: false };
  let trashRoot;
  let formOpen = false;
  let renaming = null;
  const sel = new Selection();
  let alb = null;
  const live = () => entries.filter((e) => !e.deleted_at);
  const pathOf = (e) => join(dir, e.name);
  const refresh = async () => {
    try {
      const r = await ls(dir, ctx.root);
      entries = r.entries;
      info = r.dir;
      if (inTrash && !trashRoot) {
        try {
          trashRoot = (await trashList()).entries.find((t) => t.ino === ctx.root);
        } catch {
        }
      }
      sel.keep(new Set(entries.filter((e) => !e.deleted_at).map((e) => e.name)));
      draw();
    } catch (e) {
      clear(root);
      const msg = e instanceof Error ? e.message : String(e);
      root.append(h("section", null, crumbs(ctx), h("div", { class: "error" }, `Could not list ${dir}: ${msg}. `, h("a", { href: "#", onClick: (ev) => {
        ev.preventDefault();
        void refresh();
      } }, "Retry"))));
    }
  };
  const draw = () => {
    clear(root);
    const section = h("section", null, crumbs(ctx, info, inTrash));
    if (inTrash) {
      const files = entries.filter((e) => e.type === "file");
      const rootEntry = trashRoot ?? { ino: ctx.root, name: "", type: "dir", size: 0, path: "", deleted_at: info.deleted_at ?? "", restorable: true };
      if (info.purge_at && !trashRoot) rootEntry.purge_at = info.purge_at;
      section.append(banner({ root: { ...rootEntry, purge_at: rootEntry.purge_at ?? info.purge_at }, items: entries.length, bytes: files.reduce((s, e) => s + e.size, 0) }));
    }
    const input = h("input", { type: "file", multiple: true, "aria-label": "upload files" });
    input.addEventListener("change", () => {
      if (input.files?.length) enqueue(dir, input.files);
      input.value = "";
    });
    const total = live().reduce((s, e) => s + (e.type === "file" ? e.size : 0), 0);
    const deleted = entries.length - live().length;
    const toolbar = h("div", { class: "toolbar" });
    const asAlbum = view2 === "list" && !isAlbum(entries) ? false : view2 === "list";
    if (asAlbum) {
      toolbar.append(
        h("button", { type: "button", onClick: () => alb && player.play(alb.tracks, 0) }, "Play all"),
        h("button", { type: "button", onClick: () => {
          if (alb) {
            if (!player.shuffle) player.toggleShuffle();
            player.play(alb.tracks, 0);
          }
        } }, "Shuffle")
      );
    }
    if (!inTrash) {
      toolbar.append(
        h("label", { class: "btn" }, "Upload", input),
        h("button", { type: "button", "aria-pressed": formOpen ? "true" : "false", onClick: () => {
          formOpen = !formOpen;
          renaming = null;
          draw();
        } }, "New folder")
      );
    }
    toolbar.append(menu({ path: dir, inTrash, dir: info, onChange: () => void refresh() }));
    const seg = h(
      "span",
      { class: "seg", role: "group", "aria-label": "view" },
      h("button", { type: "button", "aria-pressed": view2 === "list" ? "true" : "false", onClick: () => setView("list") }, "List"),
      h("button", { type: "button", "aria-pressed": view2 === "tiles" ? "true" : "false", onClick: () => setView("tiles") }, "Tiles")
    );
    toolbar.append(seg, h("span", { class: "spacer" }), h("span", { class: "small dim" }, `${live().length} ${live().length === 1 ? "item" : "items"} \xB7 ${bytes(total)}` + (deleted ? ` \xB7 ${deleted} deleted` : "")));
    section.append(toolbar);
    const shown = queue2.filter((j) => j.state !== "done");
    if (shown.length) {
      const list = h("div", { class: "uploads" });
      for (const j of shown) list.append(jobRow(j));
      section.append(list);
    }
    if (formOpen) section.append(folderForm());
    if (renaming) section.append(renameForm(renaming));
    alb = null;
    if (entries.length === 0) section.append(h("div", { class: "empty" }, "empty folder"));
    else if (asAlbum) {
      alb = render7(ctx, info.cover, entries);
      section.append(alb.el);
      const rest = entries.filter((e) => !(e.type === "file" && isAudioName(e.name) && !e.deleted_at));
      if (rest.length) section.append(table(rest));
    } else if (view2 === "tiles") section.append(tiles());
    else section.append(table(entries));
    section.append(selbar());
    root.append(section);
  };
  const setView = (v) => {
    view2 = v;
    session.set("tr.view", v);
    draw();
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
  const renameForm = (old) => {
    const name = h("input", { type: "text", value: old, "aria-label": "new name" });
    const form = h("form", { class: "inline-form rename" }, icon(iconFor(old, entries.find((e) => e.name === old)?.type === "dir")), name, h("button", { type: "submit" }, "Rename"), h("button", { type: "button", onClick: () => {
      renaming = null;
      draw();
    } }, "Cancel"));
    form.addEventListener("submit", (ev) => {
      ev.preventDefault();
      const n = name.value.trim();
      if (!n || n === old) {
        renaming = null;
        draw();
        return;
      }
      rename(join(dir, old), n).then(
        () => {
          toast(`Renamed to ${n}`, "ok");
          renaming = null;
          sel.clear();
          void refresh();
        },
        (e) => toast("rename: " + (e instanceof Error ? e.message : String(e)), "bad")
      );
    });
    form.addEventListener("keydown", (ev) => {
      if (ev.key === "Escape") {
        renaming = null;
        draw();
      }
    });
    queueMicrotask(() => {
      name.focus();
      const dot = old.lastIndexOf(".");
      name.setSelectionRange(0, dot > 0 ? dot : old.length);
    });
    return form;
  };
  const sorted = (list) => {
    const livePart = list.filter((e) => !e.deleted_at).sort((a, b) => compare(a, b, sort));
    const gone = list.filter((e) => e.deleted_at).sort((a, b) => (b.deleted_at ?? "").localeCompare(a.deleted_at ?? "") || a.name.localeCompare(b.name));
    return [...livePart, ...gone];
  };
  const order = () => sorted(entries).filter((e) => !e.deleted_at).map((e) => e.name);
  const onSelect = (e, ev) => {
    ev.stopPropagation();
    sel.click(e.name, ev.shiftKey, order());
  };
  const selectBox = (e, cls) => e.deleted_at ? h("span", { class: cls }) : (() => {
    const box = h("input", { type: "checkbox", "aria-label": "select" });
    box.checked = sel.has(e.name);
    box.addEventListener("click", (ev) => onSelect(e, ev));
    return h("span", { class: cls }, box);
  })();
  const linkOf = (e) => {
    const path = pathOf(e);
    if (e.deleted_at) return e.type === "dir" ? { href: `#/trash/${e.ino}/`, kind: "dir" } : { href: `#/trashfile/${e.ino}/`, kind: iconFor(e.name) };
    if (e.type === "dir") return { href: browseHash({ path, root: ctx.root }), kind: "dir" };
    return { href: fileHash({ path, root: ctx.root }), kind: iconFor(e.name) };
  };
  const playBtn = (e) => {
    const t = { path: pathOf(e), root: e.deleted_at ? e.ino : ctx.root };
    if (e.deleted_at) t.path = "/";
    const now = player.isCurrent(t.path, t.root) && player.playing;
    return h("button", { type: "button", class: "play", onClick: (ev) => {
      ev.preventDefault();
      ev.stopPropagation();
      if (player.isCurrent(t.path, t.root)) player.toggle();
      else playFrom(e);
    } }, now ? "\u23F8 Playing" : "\u25B6 Play");
  };
  const playFrom = (e) => {
    const tracks = sorted(entries).filter((x) => !x.deleted_at && x.type === "file" && isAudioName(x.name)).map((x) => ({ path: pathOf(x), root: ctx.root }));
    if (e.deleted_at) {
      player.play([{ path: "/", root: e.ino }]);
      return;
    }
    const i = tracks.findIndex((t) => t.path === pathOf(e));
    player.play(tracks, Math.max(0, i));
  };
  const badges = (e) => {
    const b = h("span", { class: "badges" });
    if (e.protected) b.append(h("span", { class: "badge lock" }, "protected"));
    if (e.nsfw) b.append(h("span", { class: "badge nsfw" }, "nsfw"));
    if (e.deleted_at) b.append(badge(e.purge_at));
    return b;
  };
  const table = (list) => {
    const th = (key, label, cls) => {
      const attrs = { class: cls ?? "" };
      if (sort.key === key) attrs["aria-sort"] = sort.desc ? "descending" : "ascending";
      const el = h("th", attrs, label);
      el.addEventListener("click", () => {
        sort = sort.key === key ? { key, desc: !sort.desc } : { key, desc: key !== "name" };
        session.set("tr.sort", sort);
        draw();
      });
      return el;
    };
    const all = h("input", { type: "checkbox", "aria-label": "select all" });
    const names = live().map((e) => e.name);
    all.checked = names.length > 0 && names.every((n) => sel.has(n));
    all.addEventListener("click", () => all.checked ? sel.all(names) : sel.clear());
    const t = h("table", { class: "files" }, h("thead", null, h("tr", null, h("th", { class: "sel" }, all), th("name", "name"), th("size", "size", "size"), th("mtime", "modified", "mtime"))));
    const body = h("tbody");
    for (const e of sorted(list)) body.append(row2(e));
    t.append(body);
    return t;
  };
  const row2 = (e) => {
    const { href, kind } = linkOf(e);
    let link;
    if (e.type === "symlink") link = h("span", { class: "linkname" }, icon("link"), e.name + " ", h("span", { class: "target" }, "\u2192 " + (e.target ?? "")));
    else if (e.upload) link = h("span", { class: "dim", title: "an upload in progress, or one that was not resumed" }, icon("file"), e.name);
    else {
      link = h("a", { href }, icon(kind), e.name);
      if (e.protected) link.append(h("span", { class: "badge lock" }, "protected"));
      if (e.nsfw) link.append(h("span", { class: "badge nsfw" }, "nsfw"));
      if (e.deleted_at) link.append(badge(e.purge_at));
      if (e.type === "file" && isAudioName(e.name)) link.append(playBtn(e));
    }
    const cls = [e.upload ? "dim" : e.dirty ? "dirty" : "", e.deleted_at ? "deleted" : "", sel.has(e.name) ? "selected" : "", e.type === "file" && player.isCurrent(pathOf(e), ctx.root) ? "playing" : ""].filter(Boolean).join(" ");
    return h("tr", { class: cls }, h("td", { class: "sel" }, selectBox(e, "sel").firstChild ?? ""), h("td", { class: "name" }, link), h("td", { class: "size" }, e.type === "file" ? bytes(e.size) : ""), h("td", { class: "mtime", title: e.mtime }, when(e.mtime)));
  };
  const tiles = () => {
    const grid = h("div", { class: "ftiles" });
    for (const e of sorted(entries)) grid.append(tile3(e));
    return grid;
  };
  const tile3 = (e) => {
    const { href, kind } = linkOf(e);
    const kindOf = mediaKind(ext(e.name));
    const thumb = h("span", { class: "thumb" + (info.nsfw || e.nsfw ? " nsfw" : "") });
    const showIcon = () => thumb.replaceChildren(icon(kind));
    const path = pathOf(e);
    const thumbPath = e.type === "dir" && e.cover ? join(path, e.cover) : path;
    const canThumb = e.type === "dir" ? !e.deleted_at || !!e.cover : e.type === "file" && !e.upload && (kindOf === "image" || kindOf === "video" || kindOf === "audio");
    if (canThumb) {
      const img = h("img", { alt: "" });
      thumb.append(img);
      if (thumb.classList.contains("nsfw")) thumb.append(h("span", { class: "veil" }, "nsfw \xB7 tap to show"));
      const root2 = e.deleted_at ? e.ino : ctx.root;
      const tp = e.deleted_at ? e.type === "dir" && e.cover ? "/" + e.cover : "/" : thumbPath;
      lazy(img, e, tp, root2, e.type === "dir" ? "audio" : kindOf, showIcon);
    } else showIcon();
    const meta = h("span", { class: "tmeta" });
    if (e.type === "file") meta.append(h("span", null, bytes(e.size)));
    const cls = ["ftile", e.type === "dir" ? "dir" : "", sel.has(e.name) ? "selected" : "", e.dirty ? "dirty" : "", e.deleted_at ? "deleted" : "", e.type === "file" && player.isCurrent(path, ctx.root) ? "playing" : ""].filter(Boolean).join(" ");
    const a = h("a", { class: cls, href }, selectBox(e, "sel"), badges(e), thumb, h("span", { class: "tname", title: e.name }, e.name), meta);
    if (e.type === "file" && isAudioName(e.name) && !e.upload) a.insertBefore(playBtn(e), a.querySelector(".tname"));
    if (thumb.classList.contains("nsfw")) {
      thumb.addEventListener("click", (ev) => {
        if (thumb.classList.contains("revealed")) return;
        ev.preventDefault();
        ev.stopPropagation();
        thumb.classList.add("revealed");
      });
    }
    return a;
  };
  const selbar = () => {
    const names = sel.names();
    const bar2 = h("div", { class: "selbar" });
    if (!names.length) {
      bar2.hidden = true;
      return bar2;
    }
    const chosen = entries.filter((e) => names.includes(e.name) && !e.deleted_at);
    const protectedBy = info.protected ? dir.split("/").filter(Boolean).pop() ?? "/" : chosen.some((e) => e.protected) ? chosen.find((e) => e.protected).name : null;
    const delBtn = h("button", { type: "button", onClick: () => void doDelete(chosen) }, "Delete");
    const mvBtn = h("button", { type: "button", onClick: () => void doMove(chosen) }, "Move to\u2026");
    const rnBtn = h("button", { type: "button", onClick: () => {
      renaming = chosen[0].name;
      formOpen = false;
      draw();
    } }, "Rename");
    if (protectedBy || inTrash) {
      delBtn.disabled = mvBtn.disabled = true;
      const why = inTrash ? "restore the folder first" : `protected: unprotect ${protectedBy} to`;
      delBtn.title = inTrash ? why : why + " delete";
      mvBtn.title = inTrash ? why : why + " move out";
    }
    if (chosen.length !== 1 || inTrash) {
      rnBtn.disabled = true;
      rnBtn.title = inTrash ? "restore the folder first" : "select one entry to rename";
    }
    bar2.append(h("span", { class: "count" }, `${chosen.length} selected`), delBtn, mvBtn, rnBtn);
    if (chosen.length && chosen.every((e) => e.type === "dir" || isAudioName(e.name))) {
      bar2.append(h("button", { type: "button", onClick: () => void playSelection(chosen, false) }, "Play"), h("button", { type: "button", onClick: () => void playSelection(chosen, true) }, "Add to queue"));
    }
    bar2.append(h("span", { class: "spacer" }), h("button", { type: "button", onClick: () => sel.clear() }, "Clear"));
    return bar2;
  };
  const playSelection = async (chosen, append2) => {
    const tracks = [];
    for (const e of chosen) {
      if (e.type === "dir") {
        try {
          const r = await ls(pathOf(e), ctx.root);
          for (const x of r.entries) if (x.type === "file" && isAudioName(x.name) && !x.deleted_at && !x.upload) tracks.push({ path: join(pathOf(e), x.name), root: ctx.root });
        } catch {
        }
      } else tracks.push({ path: pathOf(e), root: ctx.root });
    }
    if (!tracks.length) {
      toast("Nothing to play in the selection", "bad");
      return;
    }
    if (append2) player.enqueue(tracks);
    else player.play(tracks, 0);
    sel.clear();
  };
  const doDelete = async (chosen) => {
    try {
      const r = await del(chosen.map(pathOf));
      if (r.deleted.length) toast(`Deleted ${r.deleted.length} ${r.deleted.length === 1 ? "entry" : "entries"} \xB7 in the trash` + (info.purge_at || chosen[0]?.purge_at ? "" : " for 30 d"), "ok");
      for (const f of r.failed) toast(`${f.path.split("/").pop()}: ${f.error}`, "bad");
      sel.clear();
      void refresh();
    } catch (e) {
      toast("delete: " + (e instanceof Error ? e.message : String(e)), "bad");
    }
  };
  const doMove = async (chosen) => {
    const subtree = chosen.filter((e) => e.type === "dir").map(pathOf);
    const to = await pick({
      start: dir,
      disabled: (p) => p === dir ? "the selection is here" : subtree.some((s) => p === s || p.startsWith(s + "/")) ? "inside the selection" : null,
      action: (p) => `Move ${chosen.length} here` + (p === "/" ? "" : "")
    });
    if (to === null) return;
    try {
      const r = await move(chosen.map(pathOf), to);
      if (r.moved.length) toast(`Moved ${r.moved.length} to ${to}`, "ok");
      for (const f of r.failed) toast(`${f.path.split("/").pop()}: ${f.error}`, "bad");
      sel.clear();
      void refresh();
    } catch (e) {
      toast("move: " + (e instanceof Error ? e.message : String(e)), "bad");
    }
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
    if (inTrash) return;
    ev.preventDefault();
    root.querySelector("table.files")?.classList.add("drop");
  };
  const onDragLeave = () => root.querySelector("table.files")?.classList.remove("drop");
  const onDrop = (ev) => {
    if (inTrash) return;
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
  const onKey = (ev) => {
    const t = ev.target;
    if (t && (t.tagName === "INPUT" || t.tagName === "TEXTAREA" || t.tagName === "SELECT")) return;
    const chosen = entries.filter((e) => sel.has(e.name) && !e.deleted_at);
    if (ev.key === "Escape") {
      sel.clear();
      renaming = null;
      formOpen = false;
      draw();
    } else if (ev.key === "Delete" && chosen.length && !info.protected && !inTrash && !chosen.some((e) => e.protected)) void doDelete(chosen);
    else if (ev.key === "F2" && chosen.length === 1 && !inTrash) {
      renaming = chosen[0].name;
      draw();
    }
  };
  document.addEventListener("keydown", onKey);
  const unsubSel = sel.subscribe(draw);
  const unsubPlayer = player.subscribe(() => {
    if (alb) alb.refresh();
    for (const b of root.querySelectorAll("button.play")) b.textContent = b.textContent?.startsWith("\u23F8") ? "\u25B6 Play" : b.textContent ?? "";
    root.querySelectorAll("tr.playing, .ftile.playing").forEach((el) => el.classList.remove("playing"));
    const c = player.current();
    if (c && c.root === ctx.root && c.path.startsWith(dir === "/" ? "/" : dir + "/")) draw();
  });
  onQueueChange = draw;
  onJobDone = (j) => {
    if (j.dir === dir) void refresh();
  };
  void refresh();
  return () => {
    onQueueChange = null;
    onJobDone = null;
    unsubSel();
    unsubPlayer();
    document.removeEventListener("keydown", onKey);
    root.removeEventListener("dragover", onDragOver);
    root.removeEventListener("dragleave", onDragLeave);
    root.removeEventListener("drop", onDrop);
  };
}
function crumbs(ctx, info, inTrash = false) {
  const el = h("div", { class: "crumbs" }, h("a", { href: "#/browse/" }, "/"));
  const parts = ctx.path.split("/").filter(Boolean);
  let acc = "";
  parts.forEach((p, i) => {
    acc += "/" + p;
    if (i > 0 || inTrash) el.append(h("span", { class: "sep" }, "/"));
    if (i === parts.length - 1) el.append(h("span", null, p));
    else el.append(h("a", { href: browseHash({ path: acc, root: ctx.root }) }, p));
  });
  if (inTrash) el.append(h("span", { class: "badge" }, "deleted"));
  if (info?.protected) el.append(h("span", { class: "badge lock" }, "protected"));
  if (info?.nsfw) el.append(h("span", { class: "badge nsfw" }, "nsfw"));
  if (info?.pinned) el.append(h("span", { class: "badge pin" }, "kept here"));
  return el;
}
function crumbsAll(ctx) {
  const el = h("div", { class: "crumbs" }, h("a", { href: "#/browse/" }, "/"));
  const parts = ctx.path.split("/").filter(Boolean);
  let acc = "";
  parts.forEach((p, i) => {
    acc += "/" + p;
    if (i > 0) el.append(h("span", { class: "sep" }, "/"));
    el.append(h("a", { href: browseHash({ path: acc, root: ctx.root }) }, p));
  });
  if (ctx.root !== void 0) el.append(h("span", { class: "badge" }, "deleted"));
  return el;
}
function iconFor(name, dir = false) {
  if (dir) return "dir";
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

// internal/webui/src/stats.ts
var RANGES = [
  ["1h", 3600],
  ["6h", 6 * 3600],
  ["24h", 86400],
  ["7d", 7 * 86400],
  ["30d", 30 * 86400],
  ["1y", 365 * 86400]
];
var INTERVAL2 = 5e3;
function parse(query) {
  const q = new URLSearchParams(query);
  const from = Number(q.get("from"));
  const to = Number(q.get("to"));
  if (from > 0 && to > from) return { range: "custom", from, to, node: q.get("node") ?? "" };
  const range = RANGES.some(([r]) => r === q.get("range")) ? q.get("range") : "6h";
  return { range, from: 0, to: 0, node: q.get("node") ?? "" };
}
function hash(s) {
  const q = new URLSearchParams();
  if (s.range === "custom") {
    q.set("from", String(s.from));
    q.set("to", String(s.to));
  } else q.set("range", s.range);
  if (s.node) q.set("node", s.node);
  return "#/stats?" + q.toString();
}
function bounds(s, now) {
  if (s.range === "custom") return { from: s.from, to: s.to };
  const secs = RANGES.find(([r]) => r === s.range)?.[1] ?? 6 * 3600;
  return { from: Math.floor(now / 1e3) - secs, to: "now" };
}
function local(t) {
  const d = new Date(t * 1e3);
  const p = (n) => String(n).padStart(2, "0");
  return `${d.getFullYear()}-${p(d.getMonth() + 1)}-${p(d.getDate())}T${p(d.getHours())}:${p(d.getMinutes())}`;
}
function render9(root, query) {
  const state = parse(query);
  let timer;
  let disposed = false;
  let last = null;
  let lastAt = 0;
  const go = (next2) => {
    location.hash = hash({ ...state, ...next2 });
  };
  const seg = h("span", { class: "seg", role: "group", "aria-label": "range" });
  for (const [r] of RANGES) seg.append(h("button", { type: "button", "aria-pressed": String(state.range === r), onclick: () => go({ range: r, from: 0, to: 0 }) }, r));
  const form = h("form", { class: "inline-form range" });
  const customBtn = h("button", { type: "button", "aria-pressed": String(state.range === "custom"), onclick: () => form.toggleAttribute("hidden") }, "custom");
  seg.append(customBtn);
  const now = Math.floor(Date.now() / 1e3);
  const fromIn = h("input", { type: "datetime-local", "aria-label": "from", value: local(state.range === "custom" ? state.from : now - 6 * 3600) });
  const toIn = h("input", { type: "datetime-local", "aria-label": "to", value: local(state.range === "custom" ? state.to : now) });
  const apply2 = h("button", { type: "submit" }, "Apply");
  const check = () => {
    const f = Date.parse(fromIn.value) / 1e3;
    const t = Date.parse(toIn.value) / 1e3;
    apply2.disabled = !(f > 0 && t > f);
  };
  fromIn.addEventListener("input", check);
  toIn.addEventListener("input", check);
  check();
  form.append(h("label", null, "from ", fromIn), h("label", null, "to ", toIn), apply2, h("button", { type: "button", onclick: () => form.setAttribute("hidden", "") }, "Cancel"));
  form.addEventListener("submit", (e) => {
    e.preventDefault();
    go({ range: "custom", from: Math.floor(Date.parse(fromIn.value) / 1e3), to: Math.floor(Date.parse(toIn.value) / 1e3) });
  });
  form.addEventListener("keydown", (e) => {
    if (e.key === "Escape") form.setAttribute("hidden", "");
  });
  if (state.range !== "custom") form.setAttribute("hidden", "");
  const menu2 = h("details", { class: "menu" });
  const summary = h("summary", null, "Node: " + (state.node || "this device"));
  const body = h("div", { class: "menu-body" });
  menu2.append(summary, body);
  const pill2 = h("span", { class: "pill", title: "waiting for the first fetch" }, "loading");
  const toolbar = h("div", { class: "toolbar" }, seg, menu2, h("span", { class: "spacer" }), pill2);
  const charts = h("div", { class: "charts" });
  const head = h("section", null, h("h2", null, "Stats", h("span", { class: "h-sub" }, "")), toolbar, form);
  root.append(head, charts);
  const sub = head.querySelector(".h-sub");
  const drawMembers = (nodes, current) => {
    clear(body);
    const row2 = (name, label, dot, note, m) => {
      const el = h("div", { class: "menu-row" + (m?.web ? " link" : ""), "aria-current": String(name === current) }, h("span", { class: "dot " + dot }), h("a", { href: hash({ ...state, node: name }) }, label), h("span", { class: "small dim" }, note));
      if (m?.web) el.append(h("a", { class: "menu-link", href: `http://${m.addr}:${m.web}/${hash({ ...state, node: name })}`, title: "open on " + m.name }, "\u2197"));
      body.append(el);
    };
    row2("all", "all nodes", "off", "one series per member");
    for (const m of nodes) row2(m.name, m.name, m.live ? "ok" : "bad", (m.self ? "this device \xB7 " : "") + (m.live ? "live" : "dead") + (m.web ? "" : " \xB7 no web listener"), m);
  };
  const draw = (r) => {
    last = r;
    lastAt = Date.now();
    const nodeName = state.node === "" ? r.node : state.node;
    summary.textContent = "Node: " + (nodeName === "all" ? "all nodes" : nodeName);
    drawMembers(r.nodes, nodeName);
    const span = r.range.to - r.range.from;
    sub.textContent = `${r.range.tier} rows \xB7 ${span >= 2 * 86400 ? Math.round(span / 86400) + " d" : Math.round(span / 3600) + " h"}` + (r.range.live ? " \xB7 refreshes every 5 s while live" : "");
    clear(charts);
    if (r.charts.length === 0) charts.append(h("div", { class: "empty" }, "no rows in this range yet"));
    for (const c of r.charts) {
      const card2 = h("div", { class: "chart-card" });
      card2.innerHTML = c.svg;
      card2.append(h("div", { class: "small dim" }, c.caption + " \xB7 " + c.title));
      charts.append(card2);
    }
    setPill(r.range.live ? "live" : "paused");
  };
  const setPill = (mode, err) => {
    pill2.className = "pill" + (mode === "live" ? " ok" : mode === "error" ? " bad" : "");
    pill2.textContent = mode === "error" ? "unreachable" : mode;
    pill2.title = mode === "error" ? err ?? "" : mode === "live" ? "last fetch " + new Date(lastAt).toLocaleTimeString() + " \xB7 next in 5 s" : "a custom range never refreshes";
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
    if (!disposed && state.range !== "custom" && document.visibilityState !== "hidden") timer = window.setTimeout(() => void loop(), INTERVAL2);
  };
  const stop = () => {
    if (timer !== void 0) window.clearTimeout(timer);
    timer = void 0;
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
function parse2(hash2) {
  const [where = "", query = ""] = hash2.replace(/^#\/?/, "").split("?");
  const [head = "", ...rest] = where.split("/");
  if (head === "trash" || head === "trashfile") {
    const ino = Number(rest.shift() ?? "");
    return { head, ctx: { path: decodeHashPath(rest.join("/")), root: Number.isFinite(ino) && ino > 0 ? ino : void 0 }, query };
  }
  return { head, ctx: { path: decodeHashPath(rest.join("/")) }, query };
}
function route() {
  const { head, ctx, query } = parse2(location.hash || "#/");
  dispose?.();
  dispose = null;
  clear(view);
  let tab = "dash";
  if (head === "browse" || head === "trash" && ctx.root !== void 0) {
    tab = "files";
    dispose = render8(view, ctx);
  } else if (head === "file" || head === "trashfile" && ctx.root !== void 0) {
    tab = "files";
    dispose = render6(view, ctx);
  } else if (head === "stats") {
    tab = "stats";
    dispose = render9(view, query);
  } else {
    dispose = render4(view);
  }
  for (const a of tabs) a.classList.toggle("active", a.dataset.tab === tab);
  window.scrollTo(0, 0);
}
player.mount();
var themeButton = document.querySelector("button.theme");
if (themeButton) wire(themeButton);
status.subscribe(drawShell);
window.addEventListener("hashchange", route);
route();
export {
  healthPill,
  parse2 as parse
};
