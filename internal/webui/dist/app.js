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
function titleCard(title, sub) {
  return h("span", { class: "tcard" }, h("span", { class: "ct" }, title), sub ? h("span", { class: "cy" }, sub) : null);
}
function setWidth(el, frac) {
  el.style.width = (Math.max(0, Math.min(1, frac)) * 100).toFixed(2) + "%";
}

// internal/webui/src/icons.ts
var PATHS = {
  folder: "M160-160q-33 0-56.5-23.5T80-240v-480q0-33 23.5-56.5T160-800h240l80 80h320q33 0 56.5 23.5T880-640v400q0 33-23.5 56.5T800-160H160Zm0-80h640v-400H447l-80-80H160v480Zm0 0v-480 480Z",
  draft: "M240-80q-33 0-56.5-23.5T160-160v-640q0-33 23.5-56.5T240-880h320l240 240v480q0 33-23.5 56.5T720-80H240Zm280-520v-200H240v640h480v-440H520ZM240-800v200-200 640-640Z",
  link: "M440-280H280q-83 0-141.5-58.5T80-480q0-83 58.5-141.5T280-680h160v80H280q-50 0-85 35t-35 85q0 50 35 85t85 35h160v80ZM320-440v-80h320v80H320Zm200 160v-80h160q50 0 85-35t35-85q0-50-35-85t-85-35H520v-80h160q83 0 141.5 58.5T880-480q0 83-58.5 141.5T680-280H520Z",
  image: "M200-120q-33 0-56.5-23.5T120-200v-560q0-33 23.5-56.5T200-840h560q33 0 56.5 23.5T840-760v560q0 33-23.5 56.5T760-120H200Zm0-80h560v-560H200v560Zm40-80h480L570-480 450-320l-90-120-120 160Zm-40 80v-560 560Z",
  movie: "m160-800 80 160h120l-80-160h80l80 160h120l-80-160h80l80 160h120l-80-160h120q33 0 56.5 23.5T880-720v480q0 33-23.5 56.5T800-160H160q-33 0-56.5-23.5T80-240v-480q0-33 23.5-56.5T160-800Zm0 240v320h640v-320H160Zm0 0v320-320Z",
  music_note: "M400-120q-66 0-113-47t-47-113q0-66 47-113t113-47q23 0 42.5 5.5T480-418v-422h240v160H560v400q0 66-47 113t-113 47Z",
  album: "M480-300q75 0 127.5-52.5T660-480q0-75-52.5-127.5T480-660q-75 0-127.5 52.5T300-480q0 75 52.5 127.5T480-300Zm0-140q-17 0-28.5-11.5T440-480q0-17 11.5-28.5T480-520q17 0 28.5 11.5T520-480q0 17-11.5 28.5T480-440Zm0 360q-83 0-156-31.5T197-197q-54-54-85.5-127T80-480q0-83 31.5-156T197-763q54-54 127-85.5T480-880q83 0 156 31.5T763-763q54 54 85.5 127T880-480q0 83-31.5 156T763-197q-54 54-127 85.5T480-80Zm0-80q134 0 227-93t93-227q0-134-93-227t-227-93q-134 0-227 93t-93 227q0 134 93 227t227 93Zm0-320Z",
  search: "M784-120 532-372q-30 24-69 38t-83 14q-109 0-184.5-75.5T120-580q0-109 75.5-184.5T380-840q109 0 184.5 75.5T640-580q0 44-14 83t-38 69l252 252-56 56ZM380-400q75 0 127.5-52.5T560-580q0-75-52.5-127.5T380-760q-75 0-127.5 52.5T200-580q0 75 52.5 127.5T380-400Z",
  contrast: "M480-80q-83 0-156-31.5T197-197q-54-54-85.5-127T80-480q0-83 31.5-156T197-763q54-54 127-85.5T480-880q83 0 156 31.5T763-763q54 54 85.5 127T880-480q0 83-31.5 156T763-197q-54 54-127 85.5T480-80Zm40-83q119-15 199.5-104.5T800-480q0-123-80.5-212.5T520-797v634Z",
  light_mode: "M480-360q50 0 85-35t35-85q0-50-35-85t-85-35q-50 0-85 35t-35 85q0 50 35 85t85 35Zm0 80q-83 0-141.5-58.5T280-480q0-83 58.5-141.5T480-680q83 0 141.5 58.5T680-480q0 83-58.5 141.5T480-280ZM200-440H40v-80h160v80Zm720 0H760v-80h160v80ZM440-760v-160h80v160h-80Zm0 720v-160h80v160h-80ZM256-650l-101-97 57-59 96 100-52 56Zm492 496-97-101 53-55 101 97-57 59Zm-98-550 97-101 59 57-100 96-56-52ZM154-212l101-97 55 53-97 101-59-57Zm326-268Z",
  dark_mode: "M480-120q-150 0-255-105T120-480q0-150 105-255t255-105q14 0 27.5 1t26.5 3q-41 29-65.5 75.5T444-660q0 90 63 153t153 63q55 0 101-24.5t75-65.5q2 13 3 26.5t1 27.5q0 150-105 255T480-120Zm0-80q88 0 158-48.5T740-375q-20 5-40 8t-40 3q-123 0-209.5-86.5T364-660q0-20 3-40t8-40q-78 32-126.5 102T200-480q0 116 82 198t198 82Zm-10-270Z",
  skip_previous_fill: "M220-240v-480h80v480h-80Zm520 0L380-480l360-240v480Z",
  play_arrow_fill: "M320-200v-560l440 280-440 280Z",
  pause_fill: "M560-200v-560h160v560H560Zm-320 0v-560h160v560H240Z",
  skip_next_fill: "M660-240v-480h80v480h-80Zm-440 0v-480l360 240-360 240Z",
  repeat: "M280-80 120-240l160-160 56 58-62 62h406v-160h80v240H274l62 62-56 58Zm-80-440v-240h486l-62-62 56-58 160 160-160 160-56-58 62-62H280v160h-80Z",
  repeat_one: "M460-360v-180h-60v-60h120v240h-60ZM280-80 120-240l160-160 56 58-62 62h406v-160h80v240H274l62 62-56 58Zm-80-440v-240h486l-62-62 56-58 160 160-160 160-56-58 62-62H280v160h-80Z",
  shuffle: "M560-160v-80h104L537-367l57-57 126 126v-102h80v240H560Zm-344 0-56-56 504-504H560v-80h240v240h-80v-104L216-160Zm151-377L160-744l56-56 207 207-56 56Z",
  queue_music: "M640-160q-50 0-85-35t-35-85q0-50 35-85t85-35q11 0 21 1.5t19 6.5v-328h200v80H760v360q0 50-35 85t-85 35ZM120-320v-80h320v80H120Zm0-160v-80h480v80H120Zm0-160v-80h480v80H120Z",
  equalizer: "M160-160v-320h160v320H160Zm240 0v-640h160v640H400Zm240 0v-440h160v440H640Z",
  close: "m256-200-56-56 224-224-224-224 56-56 224 224 224-224 56 56-224 224 224 224-56 56-224-224-224 224Z",
  person: "M480-480q-66 0-113-47t-47-113q0-66 47-113t113-47q66 0 113 47t47 113q0 66-47 113t-113 47ZM160-160v-112q0-34 17.5-62.5T224-378q62-31 126-46.5T480-440q66 0 130 15.5T736-378q29 15 46.5 43.5T800-272v112H160Zm80-80h480v-32q0-11-5.5-20T700-306q-54-27-109-40.5T480-360q-56 0-111 13.5T260-306q-9 5-14.5 14t-5.5 20v32Zm240-320q33 0 56.5-23.5T560-640q0-33-23.5-56.5T480-720q-33 0-56.5 23.5T400-640q0 33 23.5 56.5T480-560Zm0-80Zm0 400Z",
  logout: "M200-120q-33 0-56.5-23.5T120-200v-560q0-33 23.5-56.5T200-840h280v80H200v560h280v80H200Zm440-160-55-58 102-102H360v-80h327L585-622l55-58 200 200-200 200Z",
  open_in_new: "M200-120q-33 0-56.5-23.5T120-200v-560q0-33 23.5-56.5T200-840h280v80H200v560h560v-280h80v280q0 33-23.5 56.5T760-120H200Zm188-212-56-56 372-372H560v-80h280v280h-80v-144L388-332Z",
  chevron_left: "M560-240 320-480l240-240 56 56-184 184 184 184-56 56Z",
  chevron_right: "M504-480 320-664l56-56 240 240-240 240-56-56 184-184Z",
  delete: "M280-120q-33 0-56.5-23.5T200-200v-520h-40v-80h200v-40h240v40h200v80h-40v520q0 33-23.5 56.5T680-120H280Zm400-600H280v520h400v-520ZM360-280h80v-360h-80v360Zm160 0h80v-360h-80v360ZM280-720v520-520Z",
  drive_file_move: "m488-400-65 65 56 56 161-161-161-161-56 56 65 65H320v80h168ZM160-160q-33 0-56.5-23.5T80-240v-480q0-33 23.5-56.5T160-800h240l80 80h320q33 0 56.5 23.5T880-640v400q0 33-23.5 56.5T800-160H160Zm0-80h640v-400H447l-80-80H160v480Zm0 0v-480 480Z",
  edit: "M200-200h57l391-391-57-57-391 391v57Zm-80 80v-170l528-527q12-11 26.5-17t30.5-6q16 0 31 6t26 18l55 56q12 11 17.5 26t5.5 30q0 16-5.5 30.5T817-647L290-120H120Zm640-584-56-56 56 56Zm-141 85-28-29 57 57-29-28Z",
  playlist_add: "M120-320v-80h280v80H120Zm0-160v-80h440v80H120Zm0-160v-80h440v80H120Zm520 480v-160H480v-80h160v-160h80v160h160v80H720v160h-80Z"
};
function icon(name) {
  return svg("svg", { class: "icon", viewBox: "0 -960 960 960", "aria-hidden": "true" }, svg("path", { d: PATHS[name] }));
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
  system: () => icon("contrast"),
  light: () => icon("light_mode"),
  dark: () => icon("dark_mode")
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
  constructor(status2, message2, body) {
    super(message2);
    this.status = status2;
    this.body = body;
  }
  status;
  body;
};
function offline(e) {
  return e instanceof ApiError && e.body?.["offline"] === true;
}
var HEADER = "X-Tailraid-UI";
function encodePath(p) {
  return p.split("/").map(encodeURIComponent).join("/");
}
function srcParam(root) {
  if (root === void 0) return "";
  return typeof root === "string" ? "at=" + root.slice(1) : "in=" + root;
}
function inQuery(root, first = false) {
  return root === void 0 ? "" : (first ? "?" : "&") + srcParam(root);
}
function fileURL(path, download = false, root) {
  const q = [];
  if (download) q.push("download=1");
  if (root !== void 0) q.push(srcParam(root));
  return "/files" + encodePath(path) + (q.length ? "?" + q.join("&") : "");
}
function thumbURL(path, root, tag) {
  return "/api/thumb?path=" + encodeURIComponent(path) + inQuery(root) + (tag ? "&t=" + encodeURIComponent(tag) : "");
}
async function call(url, init) {
  const r = await fetch(url, init);
  if (r.status === 401) signInAgain();
  if (!r.ok) throw await apiError(r);
  if (r.status === 204) return void 0;
  return await r.json();
}
function signInAgain() {
  const to = location.pathname + location.search + location.hash;
  location.replace("/login?to=" + encodeURIComponent(to));
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
function media(lib, sort = "title", offset = 0) {
  if (lib === void 0) return call("/api/media");
  return call(`/api/media?lib=${encodeURIComponent(lib)}&sort=${sort}&offset=${offset}`);
}
function mediaItem(path) {
  return call("/api/media?item=" + encodeURIComponent(path));
}
function search(q, under) {
  return call("/api/search?q=" + encodeURIComponent(q) + (under ? "&under=" + encodeURIComponent(under) : ""));
}
async function meshPicture() {
  const r = await fetch("/api/mesh");
  if (r.status === 404) return "";
  if (!r.ok) throw await apiError(r);
  return (await r.json()).svg;
}
async function rendered(path, root) {
  const r = await fetch("/api/render?path=" + encodeURIComponent(path) + inQuery(root));
  if (!r.ok) throw await apiError(r);
  return r.text();
}
async function text(path, root) {
  const r = await fetch(fileURL(path, false, root));
  if (!r.ok) throw await apiError(r);
  return r.text();
}
var PART = 32 << 20;
var RETRIES = 5;
function sendPart(path, blob, range2, onProgress, track) {
  return new Promise((resolve, reject) => {
    const xhr = new XMLHttpRequest();
    track(xhr);
    xhr.open("PUT", fileURL(path));
    xhr.setRequestHeader(HEADER, "1");
    if (range2) xhr.setRequestHeader("Content-Range", range2);
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
async function streamStats(from, to, node, ids, onFirst, onLine) {
  let url = `/api/stats?from=${from}&to=${to}&node=${encodeURIComponent(node)}&stream=1`;
  if (ids) url += "&ids=" + encodeURIComponent(ids.join(","));
  const r = await fetch(url, { cache: "no-store" });
  if (!r.ok || !r.body) throw await apiError(r);
  const reader = r.body.getReader();
  const dec = new TextDecoder();
  let buf = "";
  let first = true;
  for (; ; ) {
    const { done, value } = await reader.read();
    if (value) buf += dec.decode(value, { stream: true });
    for (let i = buf.indexOf("\n"); i >= 0; i = buf.indexOf("\n")) {
      const line = buf.slice(0, i);
      buf = buf.slice(i + 1);
      if (!line) continue;
      if (first) {
        first = false;
        onFirst(JSON.parse(line));
      } else onLine(JSON.parse(line));
    }
    if (done) return;
  }
}
var snapshots = () => call("/api/snapshots");
var takeSnapshot = (opts = {}) => post("/api/snapshot", opts);
var versions = (path) => call("/api/versions?path=" + encodeURIComponent(path));
var revert = (path, snapshot) => post("/api/revert", { path, snapshot });
var copyVersion = (path, snapshot, name) => post("/api/copy", { path, snapshot, name });
var restoreTree = (snapshot, path, opts = {}) => post("/api/restore-tree", { snapshot, path, ...opts });

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
function src(ctx) {
  return ctx.at !== void 0 ? `@${ctx.at}` : ctx.root;
}
var atSuffix = (ctx) => ctx.at !== void 0 ? "?at=" + ctx.at : "";
function browseHash(ctx) {
  return (ctx.root === void 0 ? "#/browse" + encodePathForHash(ctx.path) : `#/trash/${ctx.root}` + encodePathForHash(ctx.path)) + atSuffix(ctx);
}
function fileHash(ctx) {
  return (ctx.root === void 0 ? "#/file" + encodePathForHash(ctx.path) : `#/trashfile/${ctx.root}` + encodePathForHash(ctx.path)) + atSuffix(ctx);
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
function devTone(dev) {
  return Number.isNaN(dev) ? "" : Math.abs(dev) > 0.5 ? "bad" : Math.abs(dev) > 0.25 ? "warn" : "";
}
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
  return nodes.filter((n) => n.kind !== "disk" && n.kind !== "mirror" && n.state !== "removed").map((n) => {
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
    const tone = devTone(dev);
    const val = h("span", null, `${count(n.slots)} \xB7 expected ${count(Math.round(share.expected))}`);
    if (!Number.isNaN(dev)) val.append(" \xB7 " + signedPct(dev));
    if (n.missing_slots > 0) {
      val.append(" \xB7 ");
      val.append(h("span", { class: "text-bad" }, `${count(n.missing_slots)} missing`));
    }
    slots = barRow("slots", bar(n.slots / scale, tone, share.expected / scale), val);
  }
  const meta2 = h("div", { class: "meta" }, h("span", null, "rtt " + (n.rtt ?? "\u2014")));
  if (n.s3) {
    meta2.append(h("span", null, n.s3.prefix ? `${n.s3.bucket}/${n.s3.prefix}` : n.s3.bucket));
    if (n.s3.region) meta2.append(h("span", null, n.s3.region));
  } else {
    meta2.append(h("span", null, n.version || "?"), h("span", null, "applied " + count(n.applied)), h("span", null, `${n.addr}:${n.rpc_port}`));
  }
  meta2.append(h("span", null, "joined " + shortDate(n.joined_at)));
  return h("div", { class: "card node" + (n.self ? " self" : "") }, name, capacity, slots, meta2);
}

// internal/webui/src/disks.ts
function hosts(nodes) {
  const of = (kind2, n) => nodes.filter((d) => d.kind === kind2 && d.host === n.idx && d.state !== "removed");
  return nodes.filter((n) => n.kind === "device" && n.state !== "removed").map((n) => ({ node: n, targets: of("disk", n), mirrors: of("mirror", n) }));
}
function buckets(nodes) {
  return nodes.filter((n) => n.kind === "s3" && n.state !== "removed");
}
function off(t) {
  return t.state === "active" && t.capacity === 0;
}
function plural(n, one) {
  return n === 1 ? `1 ${one}` : `${n} ${one}s`;
}
function render2(root, r, snaps) {
  if (!r.nodes.some((n2) => n2.kind === "disk")) return;
  const groups = hosts(r.nodes);
  const s3 = buckets(r.nodes);
  const targets = [...groups.flatMap((g) => g.targets), ...s3];
  const per = chunksPerTarget(r);
  const sh = shares(targets, () => per);
  const v = r.volume;
  const active = groups.filter((g) => g.targets.some((t) => t.state === "active" && t.capacity > 0));
  const disks = groups.flatMap((g) => g.targets).length;
  const perHostDisks = disks / Math.max(1, groups.length);
  let layout = Number.isInteger(perHostDisks) ? `${plural(groups.length, "host")} \xD7 ${plural(perHostDisks, "disk")}` : `${plural(groups.length, "host")}, ${plural(disks, "disk")}`;
  if (s3.length) layout += ` + ${plural(s3.length, "bucket")}`;
  const sub = `survives ${plural(v.tolerates_disks, "disk")}, ${plural(v.tolerates_hosts, "host")} \xB7 ${v.auto ? "auto" : "manual"} ${v.k}+${v.m} \xB7 ${layout}`;
  const n = v.k + v.m;
  const capPer = Math.ceil(n / Math.max(1, active.length));
  const capNote = v.domain_skew > 0 ? h("div", { class: "cap-note warn" }, `${count(v.domain_skew)} chunks above the host cap (${capPer} of ${n} slots per host) \xB7 repair is moving them`) : h("div", { class: "cap-note" }, active.length > 1 ? `no chunk above the host cap (${capPer} of ${n} slots per host)` : `one host: every slot is on ${active[0]?.node.name ?? "it"}`);
  const scale = Math.max(1, ...targets.map((t) => Math.max(t.slots, sh.get(t.idx)?.expected ?? 0))) * 1.1;
  const mirrors = new Map((snaps?.mirrors ?? []).map((m) => [m.id, m]));
  const block = h("div", { class: "disks" }, spread(groups, s3, sh, mirrors), capNote);
  for (const g of groups) block.append(hostCard(g, sh, scale, mirrors));
  if (s3.length) block.append(bucketCard(s3, sh, scale));
  root.append(h("section", null, h("h2", null, "Disks", h("span", { class: "h-sub" }, sub)), block));
}
function spread(groups, s3, sh, mirrors) {
  const all = [...groups.flatMap((g) => g.targets), ...s3];
  const devs = all.filter((t) => t.state === "active" && !off(t)).map((t) => Math.abs(sh.get(t.idx)?.deviation ?? 0));
  const worst = Math.max(0, ...devs.filter((d) => !Number.isNaN(d)));
  const lim = worst <= 0.25 ? 0.25 : worst <= 0.5 ? 0.5 : 1;
  const pctLabel = (x) => `${Math.round(x * 100)} %`;
  const plot = h("div", { class: "spread-plot" });
  const column = (t) => {
    const share = sh.get(t.idx);
    if (off(t)) return h("div", { class: "spread-col off", title: `${t.name}: no budget (capacity 0)` });
    if (t.state !== "active") return h("div", { class: "spread-col drain", title: `${t.name}: draining, ${count(t.slots)} slots to move` });
    const dev = share?.deviation ?? NaN;
    const d = Number.isNaN(dev) ? 0 : dev;
    const cls = !t.live ? "dead" : devTone(dev);
    const title = `${t.name}: ${Number.isNaN(dev) ? "no expected share" : signedPct(dev) + " of expected"} (${count(t.slots)} of ${count(Math.round(share?.expected ?? 0))} slots)` + (t.live ? "" : ", dead");
    const col = h("div", { class: "spread-col" + (cls ? " " + cls : ""), title });
    const fill = h("i", { class: d >= 0 ? "up" : "down" });
    fill.style.height = (Math.min(Math.abs(d), lim) / lim * 50).toFixed(1) + "%";
    col.append(fill);
    return col;
  };
  const group = (cols, live, name) => plot.append(h("div", { class: "spread-host" }, h("div", { class: "spread-cols" }, ...cols), h("div", { class: "spread-label" }, h("span", { class: "dot " + (live ? "ok" : "bad") }), name)));
  for (const g of groups) {
    if (g.targets.length === 0 && g.mirrors.length === 0) continue;
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
    h("div", { class: "spread-axis" }, h("span", null, "+" + pctLabel(lim)), h("span", null, "expected"), h("span", null, "\u2212" + pctLabel(lim))),
    plot
  );
}
function hostCard(g, sh, scale, mirrors) {
  const n = g.node;
  const badges = h("span", { class: "badges" });
  if (n.leader) badges.append(h("span", { class: "badge strong" }, "leader"));
  badges.append(n.voter ? h("span", { class: "badge strong" }, "voter") : h("span", { class: "badge" }, "non-voter"));
  if (n.self) badges.append(h("span", { class: "badge self" }, "self"));
  if (n.state === "draining") badges.append(h("span", { class: "badge warn" }, "draining"));
  const budget = g.targets.reduce((s, t) => s + t.capacity, 0);
  let sum = g.targets.length === 0 ? "no disks" : `${plural(g.targets.length, "disk")} \xB7 ${bytes(budget)}`;
  if (g.mirrors.length) sum += ` \xB7 ${plural(g.mirrors.length, "mirror")}`;
  if (!n.live) sum += " \xB7 dead " + (n.dead_for ?? "?");
  const head = h("div", { class: "name-row" }, h("span", { class: "dot " + (n.live ? "ok" : "bad") }), h("span", { class: "name" }, n.name), h("span", { class: "host-sum" }, sum), badges);
  if (g.targets.length === 0 && g.mirrors.length === 0) {
    return h("div", { class: "host-row diskless" + (n.self ? " self" : "") }, head, h("div", { class: "host-note" }, "gateway only \xB7 no disks"));
  }
  const strip = h("div", { class: "disk-strip" });
  for (const t of g.targets) strip.append(diskRow(t, sh.get(t.idx), scale));
  for (const m of g.mirrors) strip.append(mirrorRow(m, mirrors.get(m.idx)));
  const card3 = h("div", { class: "host-row" + (n.self ? " self" : "") }, head, strip);
  for (const group of sharedGroups(g.targets)) {
    card3.append(h("div", { class: "host-note" }, h("span", { class: "warn-icon" }), `${listOf(group)} are on one physical disk: they fail together, so data spread across them is not redundant`));
  }
  return card3;
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
function mirrorRow(n, m) {
  const online = !!m?.online;
  const name = h("div", { class: "disk-name" }, h("span", { class: "dot " + (online ? "ok" : "off") }), h("span", { class: "n" }, n.name));
  const frac = m && m.total > 0 ? m.used / m.total : 0;
  const state = online ? "archive \xB7 online" : "archive \xB7 offline" + (m?.last_seen ? ` since ${m.last_seen.slice(0, 10)}` : "");
  const usage = m && m.total > 0 ? h("div", { class: "disk-val" }, h("span", null, bytes(m.used)), h("span", null, "of " + bytes(m.total))) : h("div", { class: "disk-val" }, online ? "size unknown" : "attach it to read its size");
  const title = `${n.name}: archive mirror, snapshots only` + (m?.reason ? ` \xB7 ${m.reason}` : "");
  return h("div", { class: "disk" + (online ? "" : " off"), title }, name, bar(frac), h("div", { class: "disk-val dots" }, h("span", null, state)), usage);
}
function bucketCard(s3, sh, scale) {
  const live = s3.filter((b) => b.live).length;
  const head = h("div", { class: "name-row" }, h("span", { class: "dot " + (live === s3.length ? "ok" : live ? "warn" : "bad") }), h("span", { class: "name" }, "S3"), h("span", { class: "host-sum" }, `${plural(s3.length, "bucket")} \xB7 ${bytes(s3.reduce((s, b) => s + b.stored_bytes, 0))} stored`));
  const strip = h("div", { class: "disk-strip" });
  for (const b of s3) {
    const row3 = diskRow(b, sh.get(b.idx), scale);
    const where = b.s3 ? b.s3.prefix ? `${b.s3.bucket}/${b.s3.prefix}` : b.s3.bucket : "";
    row3.lastElementChild?.replaceWith(h("div", { class: "disk-val" }, h("span", null, bytes(b.stored_bytes) + " stored"), h("span", null, where)));
    strip.append(row3);
  }
  return h("div", { class: "host-row" }, head, strip);
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
  const last2 = history[history.length - 1];
  const lastTone = last2 && last2.degraded + last2.pending > 0 ? "warn" : "";
  const spark = h(
    "div",
    null,
    sparkline(
      history.map((s) => s.degraded + s.pending),
      lastTone
    ),
    h("div", { class: "small dim" }, "degraded + pending, last 10 min (this tab only)")
  );
  const section2 = h("section", null, h("h2", null, "Integrity"), tiles, spark, repair(r));
  if (r.gateway) {
    const g = r.gateway;
    section2.append(
      h("h3", null, "Gateway"),
      barRow("dirty bytes", bar(g.dirty_bytes / MAX_DIRTY, fillTone(g.dirty_bytes / MAX_DIRTY)), `${bytes(g.dirty_bytes)} in ${g.dirty_inodes} ${g.dirty_inodes === 1 ? "file" : "files"} of ${bytes(MAX_DIRTY)}` + (g.flushing ? " \xB7 flushing" : "")),
      barRow("WAL", bar(g.wal_bytes / WAL_CAP, fillTone(g.wal_bytes / WAL_CAP)), `${bytes(g.wal_bytes)} of ${bytes(WAL_CAP)}`),
      h("div", { class: "small dim" }, "acknowledged writes not yet committed to the cluster")
    );
  }
  root.append(section2);
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
  const row3 = (k, v, tone) => kv.append(h("div", null, h("dt", null, k), h("dd", { class: tone }, v)));
  row3("scanned", count(s.scanned));
  row3("repaired", count(s.repaired));
  row3("partial", count(s.partial));
  row3("unrepairable", count(s.unrepairable), s.unrepairable > 0 ? "bad" : void 0);
  row3("deferred", count(s.deferred));
  row3("migrated", count(s.migrated));
  row3("moved home", count(s.homed ?? 0));
  row3("policy stale", count(s.policy_stale));
  row3("scrubbed", count(s.scrubbed));
  row3("scrub bad", count(s.scrub_bad), s.scrub_bad > 0 ? "bad" : void 0);
  row3("bytes moved", bytes(s.bytes_moved));
  row3("errors", count(s.errors), s.errors > 0 ? "bad" : void 0);
  row3("reported bad slots", count(r.repair.reported_bad_slots), r.repair.reported_bad_slots > 0 ? "warn" : void 0);
  const p = r.repair.in_progress;
  const away = home(r.repair.home);
  if (!p) return h("div", null, h("h3", null, "Repair \xB7 last scan " + ago(r.repair.last_scan)), away, kv);
  const started = new Date(p.started).toLocaleTimeString([], { hour: "2-digit", minute: "2-digit" });
  const progress = barRow(
    "scanned",
    bar(p.total > 0 ? p.scanned / p.total : 0),
    `${count(p.scanned)} of ${count(p.total)} \xB7 ${p.in_flight} in flight of ${p.workers} workers \xB7 ${bytes(p.bytes_moved)} moved \xB7 started ${started}`
  );
  const note = h("div", { class: "small dim" }, `the pass under way; the counters below are the last completed pass, ${ago(r.repair.last_scan)}`);
  return h("div", null, h("h3", null, `Repair \xB7 pass ${p.pass} running`), progress, note, away, kv);
}
function home(st) {
  if (!st || st.away_slots === 0) return null;
  const share = st.stored_bytes > 0 ? st.movable_bytes / st.stored_bytes : 0;
  const pct = (f) => `${(f * 100).toFixed(f < 0.1 ? 1 : 0)} %`;
  const gate = st.forced ? "gate open (volume rebalance)" : st.open ? "gate open" : st.start > 0 ? "gate closed" : "gate closed (home_start 0)";
  const of = st.start > 0 ? ` (${pct(share)} of ${pct(st.start)})` : "";
  return barRow(
    "away from home",
    bar(st.open ? 1 : st.start > 0 ? Math.min(share / st.start, 1) : 0),
    `${count(st.away_slots)} slots \xB7 ${bytes(st.away_bytes)} \xB7 ${bytes(st.movable_bytes)} movable${of} \xB7 ${gate}`
  );
}

// internal/webui/src/account.ts
var session2 = null;
function toLogin() {
  const to = location.pathname + location.search + location.hash;
  location.replace("/login?to=" + encodeURIComponent(to));
}
async function mount(header2) {
  let r;
  try {
    r = await fetch("/api/session");
  } catch {
    return;
  }
  if (r.status === 401) {
    toLogin();
    return;
  }
  if (!r.ok) return;
  session2 = await r.json();
  const chip = h("span", { class: "account", title: `signed in as ${session2.user}` }, icon("person"), h("span", null, session2.user));
  const out = h("button", { type: "button", class: "signout", title: "Sign out", "aria-label": "Sign out" }, icon("logout"));
  out.addEventListener("click", async () => {
    out.disabled = true;
    try {
      await fetch("/api/logout", { method: "POST", headers: { [HEADER]: "1" } });
    } finally {
      location.replace("/login");
    }
  });
  chip.append(out);
  const theme = header2.querySelector("button.theme");
  if (theme) theme.before(chip);
  else header2.append(chip);
}
function memberLink(name, href, ...kids) {
  const a = h("a", { href, "data-node": name }, ...kids);
  return a;
}
function wire2(root) {
  root.addEventListener("click", (ev) => {
    const a = ev.target?.closest("a[data-node]");
    if (!a || !session2) return;
    ev.preventDefault();
    void follow(a);
  });
}
async function follow(a) {
  const to = new URL(a.href, location.href);
  try {
    const r = await fetch("/api/handoff?to=" + encodeURIComponent(a.dataset.node ?? ""));
    if (r.status === 401) {
      toLogin();
      return;
    }
    if (!r.ok) {
      location.assign(a.href);
      return;
    }
    const { url } = await r.json();
    location.assign(url + "&to=" + encodeURIComponent(to.pathname + to.search + to.hash));
  } catch {
    location.assign(a.href);
  }
}

// internal/webui/src/mesh.ts
var picture = "";
async function refresh() {
  let svg2 = "";
  try {
    svg2 = await meshPicture();
  } catch {
    svg2 = "";
  }
  if (svg2 === picture) return false;
  picture = svg2;
  return true;
}
function render4(root, r) {
  const m = r.mesh;
  if (!m) return;
  const peers = m.peers ?? [];
  const byIdx = /* @__PURE__ */ new Map();
  for (const n of r.nodes) byIdx.set(n.idx, n);
  const counts = { lan: 0, wan: 0, relay: 0, none: 0 };
  for (const p of peers) {
    const c = p.class === "lan" || p.class === "wan" || p.class === "relay" ? p.class : "none";
    counts[c]++;
  }
  const pm = m.portmap && m.portmap.external ? `portmap ${m.portmap.protocol} ${m.portmap.external}` : "portmap none";
  const sub = `${m.addr}${m.interface ? " on " + m.interface : ""} \xB7 udp ${m.port} \xB7 ${pm} \xB7 ${peers.length} ${peers.length === 1 ? "peer" : "peers"}: ${counts.lan} lan, ${counts.wan} wan, ${counts.relay} relayed, ${counts.none} none`;
  const card3 = h("div", { class: "chart-card" });
  const pic = h("div");
  pic.innerHTML = picture;
  if (picture) card3.append(...pic.childNodes);
  const self = r.nodes.find((n) => n.self);
  const rows = [
    h(
      "tr",
      { class: "self" },
      h("td", { class: "name" }, self?.name ?? r.node, h("span", { class: "badge self" }, "this device")),
      h("td", { class: "addr" }, m.addr),
      h("td", { class: "path" }, `${m.interface ? m.interface + " \xB7 " : ""}udp ${m.port}`),
      h("td", { class: "ep" }, pm),
      h("td", { class: "rtt" }),
      h("td", { class: "seen" })
    )
  ];
  for (const p of peers) rows.push(row(p, byIdx.get(p.idx)));
  card3.append(
    h(
      "table",
      { class: "mesh-peers" },
      h("thead", null, h("tr", null, h("th", null, "member"), h("th", null, "address"), h("th", null, "path"), h("th", null, "endpoint"), h("th", { class: "rtt" }, "rtt"), h("th", { class: "seen" }, "seen"))),
      h("tbody", null, ...rows)
    ),
    h("p", { class: "small dim" }, "this device's view: one edge per member in the class of the path to it, lan green, wan ink, relay amber through the member that carries it, none red; the table under it carries each member's facts; click a member for its own view")
  );
  root.append(h("section", null, h("h2", null, "Mesh", h("span", { class: "h-sub" }, sub)), card3));
}
function row(p, n) {
  const name = p.name || n?.name || `node ${p.idx}`;
  const host = n?.addr ? n.addr.replace(/:\d+$/, "") : "";
  const nameCell = h("td", { class: "name" }, n?.web && host ? memberLink(name, `http://${host}:${n.web}/#/`, name) : name);
  if (n?.leader) nameCell.append(h("span", { class: "badge strong" }, "leader"));
  else if (n?.state === "draining") nameCell.append(h("span", { class: "badge warn" }, "draining"));
  let dot = "";
  let path = p.class;
  let ep = p.endpoint || "\u2014";
  switch (p.class) {
    case "lan":
      dot = " ok";
      break;
    case "relay":
      dot = " warn";
      path = "relay via " + (p.via ?? "?");
      ep = "\u2014";
      break;
    case "wan":
      break;
    default:
      dot = " bad";
      path = "none";
      ep = p.endpoint && !p.endpoint.startsWith("fanout:") ? p.endpoint : "\u2014";
  }
  return h(
    "tr",
    { class: n && !n.live ? "off" : "" },
    nameCell,
    h("td", { class: "addr" }, host),
    h("td", { class: "path" }, h("span", { class: "dot" + dot }), path),
    h("td", { class: "ep" }, ep),
    h("td", { class: "rtt" }, n?.rtt || "\u2014"),
    h("td", { class: "seen" }, p.seen_age || "\u2014")
  );
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
      if (cur !== "/") list.append(h("li", { onClick: () => go(dirname(cur)) }, icon("folder"), ".."));
      try {
        const r = await ls(cur);
        for (const e of r.entries) {
          if (e.type !== "dir" || e.deleted_at) continue;
          const path = cur === "/" ? "/" + e.name : cur + "/" + e.name;
          const why = opts.disabled(path);
          const li = h("li", why ? { "aria-disabled": "true", title: why } : null, icon("folder"), e.name);
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

// internal/webui/src/snapshots.ts
var FRESH_MS = 1e4;
var cached = null;
var pending = null;
function catalogue(force = false) {
  if (!force && cached && Date.now() - cached.at < FRESH_MS) return Promise.resolve(cached.v);
  pending ??= snapshots().then(
    (v) => {
      cached = { at: Date.now(), v };
      pending = null;
      return v;
    },
    (e) => {
      pending = null;
      throw e;
    }
  );
  return pending;
}
function last() {
  return cached?.v ?? null;
}
var errText = (e) => e instanceof Error ? e.message : String(e);
function kind(sn, c) {
  const mine = c.snapshots.filter((s2) => s2.mirror === sn.mirror && s2.state !== "syncing");
  if (mine.length > 1 && mine[mine.length - 1]?.id === sn.id) return "first";
  if (!sn.auto) return "";
  const hours = (Date.now() - Date.parse(sn.at)) / 36e5;
  const s = c.schedule;
  if (hours < s.hourly) return "hourly";
  if (hours < s.daily * 24) return "daily";
  if (hours < s.weekly * 24 * 7) return "weekly";
  return "monthly";
}
function taken(sn, c) {
  const k = kind(sn, c);
  return when(sn.at) + (k ? " \xB7 " + k : "");
}
function offlineFor(m) {
  if (!m.last_seen) return "offline";
  const d = daysAgo(m.last_seen);
  return d === "today" ? "offline" : "offline " + d.replace(" ago", "");
}
function asofMenu(ctx, view3, current) {
  const body = h("div", { class: "menu-body", role: "radiogroup", "aria-label": "as of" });
  const details2 = h("details", { class: "menu asof" }, h("summary", null, current ? current.name : "Now"), body);
  const go = (at) => {
    const next2 = { path: ctx.path, root: ctx.root };
    if (at !== void 0) next2.at = at;
    location.hash = view3 === "browse" ? browseHash(next2) : fileHash(next2);
  };
  const row3 = (label, note, checked, disabled, onPick) => {
    const radio = h("input", { type: "radio", name: "asof", checked, disabled: !!disabled });
    const lab = h("label", { class: "menu-row" }, radio, h("span", null, label), h("span", { class: "small dim" }, disabled ? "offline: attach it to browse" : note));
    if (disabled) {
      lab.setAttribute("aria-disabled", "true");
      lab.title = disabled;
    }
    radio.addEventListener("change", () => {
      if (radio.checked) onPick();
    });
    return lab;
  };
  const fill = async () => {
    clear(body);
    body.append(row3("Now", "the live volume", !current, null, () => go()));
    try {
      const c = await catalogue();
      clear(body);
      body.append(row3("Now", "the live volume", !current, null, () => go()));
      let n = 0;
      for (const m of c.mirrors) {
        const mine = c.snapshots.filter((s) => s.mirror === m.name && s.state !== "syncing");
        if (!mine.length) continue;
        body.append(h("div", { class: "menu-group" }, h("span", { class: "dot " + (m.online ? "ok" : "off") }), `${m.name} \xB7 ${m.online ? "online" : offlineFor(m)}`));
        for (const s of mine) {
          n++;
          const label = h("span", null, s.name);
          if (!s.auto) label.append(h("span", { class: "badge" }, "manual"));
          body.append(row3(label, taken(s, c), current?.id === s.id, m.online ? null : `${m.name} is offline: attach it to browse`, () => go(s.id)));
        }
      }
      const mirrors = new Set(c.snapshots.map((s) => s.mirror)).size;
      body.append(h("div", { class: "menu-foot small dim" }, n ? `${n} snapshots on ${mirrors} ${mirrors === 1 ? "mirror" : "mirrors"} \xB7 ` : "no snapshot yet \xB7 ", h("a", { href: "#/" }, "manage on the dashboard")));
    } catch (e) {
      body.append(h("div", { class: "menu-foot small dim" }, "snapshots: " + errText(e)));
    }
  };
  details2.addEventListener("toggle", () => {
    if (details2.open) void fill();
  });
  return details2;
}
function banner(ctx, ref, summary, folder3, back) {
  const name = ref?.name ?? (ctx.at !== void 0 ? `#${ctx.at}` : "");
  const text2 = h("span", null, "As of ", h("span", { class: "mono" }, name));
  if (ref) text2.append(" on ", h("span", { class: "mono" }, ref.mirror), ` \xB7 taken ${ago(ref.at)}`);
  text2.append(" \xB7 read-only" + (summary ? " \xB7 " + summary : ""));
  const actions = h("span", { class: "actions" });
  if (folder3 && ref) actions.append(h("button", { type: "button", onClick: () => void restoreFolder(ctx, ref) }, "Restore folder to\u2026"));
  actions.append(h("button", { type: "button", onClick: () => location.hash = back }, "Back to now"));
  return h("div", { class: "banner snapshot" }, text2, actions);
}
function offlineView(ctx, e, back) {
  const c = last();
  const sn = c?.snapshots.find((s) => s.id === ctx.at);
  const m = c?.mirrors.find((x) => x.name === sn?.mirror);
  const text2 = h("span", null, "As of ", h("span", { class: "mono" }, sn?.name ?? `#${ctx.at}`));
  if (sn) text2.append(" on ", h("span", { class: "mono" }, sn.mirror), ` \xB7 taken ${ago(sn.at)}`);
  const why = m ? h("div", { class: "empty" }, h("span", { class: "mono" }, m.name), ` is offline${m.last_seen ? ` (last seen ${ago(m.last_seen)} on ${m.host})` : ""}: attach it to browse this snapshot`) : h("div", { class: "empty" }, errText(e));
  return [h("div", { class: "banner snapshot" }, text2, h("span", { class: "actions" }, h("button", { type: "button", onClick: () => location.hash = back }, "Back to now"))), why];
}
var isOffline = offline;
async function restoreFolder(ctx, ref) {
  const to = await pick({ start: dirname(ctx.path) || "/", disabled: () => null, action: (p) => `Restore here as ${basename(ctx.path) || ref.name}` + (p === "/" ? "" : "") });
  if (to === null) return;
  try {
    const r = await restoreTree(ref.id, ctx.path, { to });
    const made = r.files + r.dirs + r.links;
    toast(`restored ${made} ${made === 1 ? "entry" : "entries"} to ${r.path}` + (r.skipped_count ? ` \xB7 ${r.skipped_count} existing left alone` : "") + (r.uploaded ? ` \xB7 ${r.uploaded} chunks copied back from ${ref.mirror}` : ""), "ok");
    location.hash = browseHash({ path: r.path });
  } catch (e) {
    toast("restore: " + errText(e), "bad");
  }
}
function range(v) {
  const names = v.snapshots.map((s) => s.name);
  if (names.length <= 1) return names[0] ?? "";
  return `${names[names.length - 1]} \u2013 ${names[0]}`;
}
function holders(v) {
  const mirrors = [...new Set(v.snapshots.map((s) => s.mirror))].join(", ");
  const ends = /* @__PURE__ */ new Set([v.snapshots[0]?.name, v.snapshots[v.snapshots.length - 1]?.name]);
  const manual = v.snapshots.filter((s) => !s.auto && !ends.has(s.name)).map((s) => s.name).slice(0, 2);
  const n = v.snapshots.length;
  return [`${n} ${n === 1 ? "snapshot" : "snapshots"}`, ...manual, mirrors].filter(Boolean).join(" \xB7 ");
}
function copyName(name, snap) {
  const e = ext(name);
  const stem = e ? name.slice(0, name.length - e.length - 1) : name;
  return `${stem} (${snap})${e ? "." + e : ""}`;
}
function versionsPanel(path, onChange) {
  const summary = h("summary", null, "Versions");
  const body = h("div", { class: "body" });
  const panel2 = h("details", { class: "panel" }, summary, body);
  let loaded = false;
  const load2 = async () => {
    clear(body);
    body.append(h("div", { class: "small dim" }, "loading\u2026"));
    try {
      const r = await versions(path);
      loaded = true;
      draw(r.versions, r.offline);
    } catch (e) {
      clear(body);
      body.append(h("div", { class: "error" }, "versions: " + errText(e)));
    }
  };
  const draw = (vs, off2) => {
    clear(body);
    const held = vs.reduce((n, v) => n + v.snapshots.length, 0);
    const mirrors = [...new Set(vs.flatMap((v) => v.snapshots.map((s) => s.mirror)))];
    summary.textContent = `Versions (${vs.length} ${vs.length === 1 ? "content" : "contents"} \xB7 ${held} snapshots${mirrors.length ? " on " + mirrors.join(", ") : ""})`;
    const current = vs.find((v) => v.current);
    const tbody = h("tbody");
    for (const v of vs) {
      const asof = h("td", { class: "asof" }, v.current ? "now" : range(v), h("span", { class: "small dim" }, v.current ? v.snapshots.length ? "also in " + holders(v) : "in no snapshot yet" : holders(v)));
      const acts = h("td", { class: "acts" });
      const first = v.snapshots[0];
      if (v.current) acts.append(h("span", { class: "badge strong" }, "current"));
      else if (first) {
        acts.append(
          h("button", { type: "button", onClick: () => location.hash = fileHash({ path, at: first.id }) }, "View"),
          h("button", { type: "button", onClick: () => askRevert(first, !!current && current.snapshots.length === 0) }, "Revert"),
          h("button", { type: "button", onClick: () => void saveCopy(first) }, "Save a copy")
        );
      }
      tbody.append(h("tr", { class: v.current ? "current" : "" }, asof, h("td", { class: "size" }, bytes(v.size)), h("td", { class: "mtime", title: v.mtime }, when(v.mtime)), acts));
    }
    body.append(h("table", { class: "versions" }, h("thead", null, h("tr", null, h("th", null, "as of"), h("th", { class: "size" }, "size"), h("th", { class: "mtime" }, "modified"), h("th", { class: "acts" }))), tbody));
    if (!vs.some((v) => !v.current)) body.append(h("div", { class: "small dim" }, "no snapshot holds another version of this file"));
    for (const o of off2) body.append(h("div", { class: "small dim" }, `${o.count} more ${o.count === 1 ? "snapshot" : "snapshots"} on `, h("span", { class: "mono" }, o.mirror), ", offline: attach it to see them"));
  };
  const doRevert = async (s) => {
    try {
      const r = await revert(path, s.id);
      toast(`reverted to ${s.name}` + (r.uploaded ? ` \xB7 ${r.uploaded} chunks copied back from ${s.mirror}` : ""), "ok");
      onChange();
      void load2();
    } catch (e) {
      toast("revert: " + errText(e), "bad");
    }
  };
  const askRevert = (s, unsaved) => {
    body.querySelector(".banner")?.remove();
    if (!unsaved) {
      void doRevert(s);
      return;
    }
    const confirm = h(
      "div",
      { class: "banner" },
      h("span", null, "The current content is in no snapshot yet: reverting replaces it for good."),
      h(
        "span",
        { class: "actions" },
        h("button", { type: "button", onClick: () => void takeSnapshot().then(() => toast("snapshot started; revert once it is complete", "ok"), (e) => toast("snapshot: " + errText(e), "bad")).finally(() => confirm.remove()) }, "Take a snapshot first"),
        h("button", { type: "button", onClick: () => {
          confirm.remove();
          void doRevert(s);
        } }, "Revert anyway"),
        h("button", { type: "button", onClick: () => confirm.remove() }, "Cancel")
      )
    );
    body.append(confirm);
  };
  const saveCopy = async (s) => {
    const name = copyName(basename(path), s.name);
    try {
      const r = await copyVersion(path, s.id, name);
      toast(`saved a copy as ${name}` + (r.uploaded ? ` \xB7 ${r.uploaded} chunks copied back from ${s.mirror}` : ""), "ok");
    } catch (e) {
      toast("save a copy: " + (e instanceof ApiError && e.status === 409 ? `${name} exists` : errText(e)), "bad");
    }
  };
  panel2.addEventListener("toggle", () => {
    if (panel2.open && !loaded) void load2();
  });
  return panel2;
}
function tile2(c) {
  if (!c || !c.mirrors.length) return null;
  const off2 = c.mirrors.filter((m) => !m.online).length;
  const n = c.snapshots.filter((s) => s.state !== "syncing").length;
  return h(
    "div",
    { class: "tile" },
    h("div", { class: "label" }, "Snapshots"),
    h("div", { class: "value" }, String(n)),
    h("div", { class: "sub" }, `on ${c.mirrors.length} ${c.mirrors.length === 1 ? "mirror" : "mirrors"}` + (off2 ? ` \xB7 ${off2} offline` : ""))
  );
}
var WARN = { stale: "stale", reserve: "nearly full", incomplete: "incomplete" };
function card2(m, c) {
  const mine = c.snapshots.filter((s) => s.mirror === m.name && s.state !== "syncing");
  const badges = h("span", { class: "badges" }, h("span", { class: "badge" }, "on " + m.host));
  if (m.self) badges.append(h("span", { class: "badge self" }, "self"));
  if (m.warn) badges.append(h("span", { class: "badge warn" }, WARN[m.warn]));
  const el = h("div", { class: "card mirror" + (m.online ? "" : " off") + (m.warn && m.online ? " warn" : "") }, h("div", { class: "name-row" }, h("span", { class: "dot " + (m.online ? "ok" : "off") }), h("span", { class: "name" }, m.name), badges));
  const disk = m.used + m.free;
  if (m.online) {
    const frac = m.total > 0 ? (m.total - m.free) / m.total : disk > 0 ? m.used / disk : 0;
    const val = `${bytes(m.used)} used of ${bytes(m.total || disk)} \xB7 ${mine.length} ${mine.length === 1 ? "snapshot" : "snapshots"}` + (m.warn === "reserve" ? " \xB7 under its reserve: the oldest automatic snapshots go first" : "");
    el.append(barRow("disk", bar(frac, m.warn === "reserve" ? "warn" : fillTone(frac)), val));
  } else {
    el.append(barRow("disk", bar(0, "off"), `${offlineFor(m)} \xB7 ${mine.length} snapshots` + (m.used ? ` \xB7 ${bytes(m.used)} when last seen` : "")));
  }
  if (m.syncing) {
    const s = m.syncing;
    const frac = s.total > 0 ? s.read / s.total : 0;
    el.append(barRow("syncing", bar(frac), `${s.snapshot} \xB7 ${Math.floor(frac * 100)} % read \xB7 ${bytes(s.bytes_new)} new`));
  }
  const meta2 = h("div", { class: "meta" });
  const newest = mine[0];
  const first = mine[mine.length - 1];
  if (newest) meta2.append(h("span", null, "newest " + when(newest.at)));
  if (first && first !== newest) meta2.append(h("span", null, "first " + shortDate(first.at)));
  if (!newest) meta2.append(h("span", null, m.syncing ? "the first snapshot copies the whole volume" : "no snapshot yet"));
  const incomplete = mine.filter((s) => s.state === "incomplete");
  if (incomplete.length) meta2.append(h("span", null, `${incomplete.length} incomplete: ${incomplete.reduce((n, s) => n + s.missing, 0)} chunks unreadable`));
  if (!m.online) meta2.append(h("span", null, m.reason ? `${m.reason} \xB7 attach it to sync and browse` : "attach it to sync and browse"));
  if (m.path) meta2.append(h("span", null, m.path));
  el.append(meta2);
  return el;
}
function schedule(c) {
  const s = c.schedule;
  if (!s.every) return "no automatic snapshots (mirrors.every 0)";
  const parts = [`hourly ${s.hourly}`, `daily ${s.daily}`, `weekly ${s.weekly}`, `monthly ${s.monthly}`];
  if (s.next) {
    const mins = Math.max(0, Math.round((Date.parse(s.next) - Date.now()) / 6e4));
    parts.push(mins < 1 ? "next now" : mins < 120 ? `next in ${mins} m` : `next in ${Math.round(mins / 60)} h`);
  }
  return parts.join(" \xB7 ");
}
function section(c, onTaken) {
  if (!c || !c.mirrors.length) return null;
  const sec = h("section", null, h("h2", null, "Mirrors", h("span", { class: "h-sub" }, schedule(c))));
  sec.append(h("div", { class: "cards" }, ...c.mirrors.map((m) => card2(m, c))));
  const manual = c.snapshots.filter((s) => !s.auto).length;
  const online = c.mirrors.filter((m) => m.online).length;
  const take = h("button", { type: "button", disabled: online === 0, title: online ? "" : "no mirror is online" }, "Take a snapshot");
  take.addEventListener("click", () => void takeDialog(onTaken));
  sec.append(h("div", { class: "snap-head" }, take, h("span", { class: "small dim" }, `every online mirror \xB7 ${c.snapshots.length} snapshots \xB7 ${manual} manual`)));
  const byName = new Map(c.mirrors.map((m) => [m.name, m]));
  const tbody = h("tbody");
  for (const s of c.snapshots) {
    const m = byName.get(s.mirror);
    const on = !!m?.online;
    const name = h("td", { class: "name" }, s.name);
    if (!s.auto) name.append(h("span", { class: "badge" }, "manual"));
    const sync = m?.syncing && m.syncing.id === s.id ? m.syncing : void 0;
    if (s.state === "syncing") name.append(h("span", { class: "badge" }, sync && sync.total ? `syncing ${Math.floor(sync.read / sync.total * 100)} %` : "syncing"));
    if (s.state === "incomplete") name.append(h("span", { class: "badge warn" }, `${s.missing} missing`));
    const why = s.state === "syncing" ? "still syncing" : on ? "" : `${s.mirror} is offline: attach it to browse`;
    const browse = h("button", { type: "button", disabled: !!why, title: why, onClick: () => location.hash = browseHash({ path: "/", at: s.id }) }, "Browse");
    const added = s.state === "syncing" ? sync ? `${bytes(sync.bytes_new)} so far` : "" : bytes(s.new_bytes);
    tbody.append(
      h("tr", { class: on ? "" : "offline" }, name, h("td", { class: "mirror" }, s.mirror), h("td", { class: "taken", title: s.at }, taken(s, c)), h("td", { class: "holds" }, added), h("td", { class: "acts" }, browse))
    );
  }
  if (c.snapshots.length) {
    sec.append(h("table", { class: "snapshots" }, h("thead", null, h("tr", null, h("th", null, "name"), h("th", { class: "mirror" }, "mirror"), h("th", { class: "taken" }, "taken"), h("th", { class: "holds" }, "added"), h("th", { class: "acts" }))), tbody));
  } else sec.append(h("div", { class: "empty" }, "no snapshot yet: the first one copies the whole volume"));
  sec.append(h("div", { class: "small dim" }, "a snapshot copies only what changed since the last one on its mirror; \u201Cadded\u201D is what it wrote"));
  return sec;
}
function takeDialog(onTaken) {
  return new Promise((resolve) => {
    const dlg = h("dialog", { class: "picker", "aria-label": "take a snapshot" });
    const name = h("input", { type: "text", placeholder: "name (optional: the UTC minute)", "aria-label": "snapshot name", maxlength: 64 });
    const go = h("button", { type: "submit" }, "Take a snapshot");
    const cancel = h("button", { type: "button" }, "Cancel");
    const form = h("form", { class: "inline-form" }, name);
    dlg.append(
      h("div", { class: "small dim" }, "A manual snapshot is permanent: the schedule never thins it and no page removes it. This device's unsaved writes land first."),
      form,
      h("div", { class: "picker-actions" }, cancel, go)
    );
    const close = () => {
      dlg.close();
      dlg.remove();
      resolve();
    };
    const submit = (ev) => {
      ev?.preventDefault();
      const n = name.value.trim();
      takeSnapshot(n ? { name: n } : {}).then(
        (rows) => {
          toast(`snapshot ${rows[0]?.name ?? n} started on ${rows.map((r) => r.mirror).join(", ")}`, "ok");
          close();
          void catalogue(true).then(onTaken, onTaken);
        },
        (e) => toast("snapshot: " + (offline(e) ? "no mirror is online" : errText(e)), "bad")
      );
    };
    form.addEventListener("submit", submit);
    go.addEventListener("click", () => submit());
    cancel.addEventListener("click", close);
    dlg.addEventListener("cancel", (ev) => {
      ev.preventDefault();
      close();
    });
    document.body.append(dlg);
    dlg.showModal();
    queueMicrotask(() => name.focus());
  });
}

// internal/webui/src/dashboard.ts
function render5(root) {
  const draw = () => {
    clear(root);
    const r = status.current;
    if (!r) {
      root.append(h("div", { class: status.error ? "error" : "empty" }, status.error ? "Could not load the status: " + status.error : "loading\u2026"));
      return;
    }
    volume(root, r);
    render2(root, r, last());
    render(root, r);
    render4(root, r);
    render3(root, r, status.history);
    const mirrors = section(last(), draw);
    if (mirrors) root.append(mirrors);
  };
  let drawn = last();
  const unsub = status.subscribe(() => {
    draw();
    if (status.current?.mesh) refresh().then((changed) => changed && draw(), () => void 0);
    catalogue().then(
      (c) => {
        if (c !== drawn) {
          drawn = c;
          draw();
        }
      },
      () => void 0
    );
  });
  draw();
  return unsub;
}
function volume(root, r) {
  const v = r.volume;
  const tiles = h(
    "div",
    { class: "tiles" },
    tile3("Policy", `${v.k}+${v.m}`, `${v.auto ? "auto" : "manual"} \xB7 ${bytes(v.chunk_size)} chunks \xB7 policy v${v.policy_version}`),
    tile3("Files", count(v.inodes), `${count(v.chunks)} chunks \xB7 ${count(v.tombstoned)} tombstoned`),
    tile3("Dedup", v.dedup_ratio.toFixed(2) + "\xD7", bytes(v.logical_bytes) + " logical"),
    tile3("Trash", count(v.trash_inodes ?? 0), v.trash_inodes ? `${bytes(v.trash_bytes ?? 0)} \xB7 restore from its folder` : "nothing deleted")
  );
  const st = tile2(last());
  if (st) tiles.append(st);
  const s = r.search;
  if (s && (s.docs > 0 || s.pending > 0 || s.index)) {
    let sub = s.index ? "index unavailable on this node" : `${bytes(s.bytes)} in ${count(s.segments)} ${s.segments === 1 ? "segment" : "segments"}`;
    if (s.pending > 0) sub += ` \xB7 ${count(s.pending)} pending`;
    if (s.capped) sub += " \xB7 capped: contents no longer indexed";
    tiles.append(tile3("Index", count(s.docs), sub));
  }
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
function tile3(label, value, sub) {
  return h("div", { class: "tile" }, h("div", { class: "label" }, label), h("div", { class: "value" }, value), h("div", { class: "sub" }, sub));
}

// internal/webui/src/chunks.ts
var PAGE = 64;
function render6(path, expected) {
  const title = h("span");
  const summaryExtra = h("span", { class: "dim small" });
  const summary = h("summary", null, title, summaryExtra);
  const body = h("div", { class: "body" });
  const panel2 = h("details", { class: "panel" }, summary, body);
  let loaded = 0;
  let total = 0;
  let fetching = false;
  const counts = { committed: 0, degraded: 0, unreadable: 0, tombstoned: 0, stale: 0, holes: 0 };
  const rows = h("div", { class: "chunk-rows" });
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
    if (counts.holes) parts.push(`${count(counts.holes)} zero`);
    if (counts.stale) parts.push(`${count(counts.stale)} policy stale`);
    summaryExtra.textContent = parts.length ? " \xB7 " + parts.join(" \xB7 ") : "";
    caption.textContent = `a hash that appears more than once is the same stored chunk (dedup) \xB7 ${count(loaded)} of ${count(total)}`;
    more.textContent = `Load ${Math.min(PAGE, total - loaded)} more`;
    more.hidden = loaded >= total;
  };
  const load2 = async () => {
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
        rows.append(row2(loaded + i, c));
        if (c.state === "committed") counts.committed++;
        else if (c.state === "degraded") counts.degraded++;
        else if (c.state === "unreadable") counts.unreadable++;
        else if (c.state === "tombstoned") counts.tombstoned++;
        else if (c.state === "zero") counts.holes++;
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
  more.addEventListener("click", () => void load2());
  retry.addEventListener("click", () => void load2());
  panel2.addEventListener("toggle", () => {
    if (panel2.open && loaded === 0 && !fetching) void load2();
  });
  return {
    el: panel2,
    expect(n) {
      expected = n;
      setTitle(n);
    }
  };
}
function row2(i, c) {
  const km = c.k + c.m;
  const live = h("span", { class: "live" + (c.live_slots < c.k ? " bad" : c.live_slots < km ? " warn" : "") }, `${c.live_slots}/${km}`);
  const hash2 = h("span", { title: c.hash }, c.hash.slice(0, 10));
  if (c.policy_stale) hash2.append(" ", h("span", { class: "badge warn" }, "stale"));
  const hole = c.state === "zero";
  if (!c.found && !hole) hash2.append(" ", h("span", { class: "badge bad" }, c.state));
  const strip = hole ? h("span", null, h("span", { class: "badge" }, "zero")) : slotStrip(c.slots ?? []);
  return h("div", { class: "chunk-row" }, h("span", { class: "dim" }, "#" + i), hash2, h("span", { class: "km dim" }, `${c.k}+${c.m}`), live, strip);
}

// internal/webui/src/trash.ts
function banner2(info) {
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
    const prev = iconBtn("skip_previous_fill", "previous", () => this.prev());
    const playBtn = iconBtn("play_arrow_fill", "play", () => this.toggle());
    const next2 = iconBtn("skip_next_fill", "next", () => this.next());
    const seek = h("input", { type: "range", min: 0, max: 0, value: 0, "aria-label": "seek" });
    seek.addEventListener("input", () => this.seek(Number(seek.value)));
    const time = h("span", { class: "time" }, "0:00 / \u2014");
    const repeat = iconBtn("repeat", "repeat", () => this.cycleRepeat());
    repeat.setAttribute("aria-pressed", "false");
    const shuffle = iconBtn("shuffle", "shuffle", () => this.toggleShuffle());
    shuffle.setAttribute("aria-pressed", "false");
    const queueBtn = h("button", { type: "button", class: "queue-btn", "aria-pressed": "false", onClick: () => {
      this.bar?.classList.toggle("expanded");
      this.notify();
    } }, icon("queue_music"), h("span", null, "Queue"));
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
    const queue2 = h("div", { class: "queue", hidden: true }, h("div", { class: "toolbar" }, summary, h("span", { class: "spacer" }), h("button", { type: "button", onClick: () => this.clear() }, "Clear")), list);
    document.body.append(bar2, queue2);
    this.bar = bar2;
    this.queueEl = queue2;
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
    face(e.playBtn, this.playing ? "pause_fill" : "play_arrow_fill", this.playing ? "pause" : "play");
    e.repeat.setAttribute("aria-pressed", this.repeat === "off" ? "false" : "true");
    face(e.repeat, this.repeat === "one" ? "repeat_one" : "repeat", this.repeat === "one" ? "repeat one" : this.repeat === "all" ? "repeat all" : "repeat");
    e.shuffle.setAttribute("aria-pressed", this.shuffle ? "true" : "false");
    const expanded = this.bar.classList.contains("expanded");
    e.queueBtn.setAttribute("aria-pressed", expanded ? "true" : "false");
    e.queueBtn.lastElementChild.textContent = `Queue \xB7 ${this.queue.length}`;
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
        h("span", { class: "no" }, playing ? icon("equalizer") : String(n + 1)),
        h("span", { class: "title", onClick: () => this.load(qi, true) }, t.title ?? basename(t.path)),
        h("span", { class: "artist" }, t.artist ?? ""),
        h("span", { class: "dur" }, mmss(t.dur)),
        iconBtn("close", "remove", () => this.remove(qi))
      );
      list.append(li);
    });
  }
};
var player = new Player();
function isAudioName(name) {
  return ["mp3", "m4a", "aac", "ogg", "oga", "opus", "wav", "flac"].includes(ext(name));
}
function iconBtn(name, label, onClick) {
  const b = h("button", { type: "button", class: "icon-btn", onClick });
  face(b, name, label);
  return b;
}
function face(b, name, label) {
  b.setAttribute("aria-label", label);
  b.title = label;
  b.replaceChildren(icon(name));
}

// internal/webui/src/markdown.ts
function isMarkdown(name) {
  return /\.(md|markdown)$/i.test(name);
}
function message(e) {
  return e instanceof Error ? e.message : String(e);
}
function view(path, root) {
  const body = h("div");
  const asRendered = h("button", { type: "button" }, "Rendered");
  const asRaw = h("button", { type: "button" }, "Raw");
  const seg = h("span", { class: "seg", role: "group", "aria-label": "view" }, asRendered, asRaw);
  const show = (raw) => {
    session.set("tr.md.raw", raw);
    asRendered.setAttribute("aria-pressed", String(!raw));
    asRaw.setAttribute("aria-pressed", String(raw));
    clear(body);
    if (raw) {
      const pre = h("pre", null, "loading\u2026");
      body.append(pre);
      text(path, root).then(
        (t) => pre.textContent = t,
        (e) => pre.textContent = "could not load: " + message(e)
      );
      return;
    }
    const article = h("article", { class: "md md-doc" }, "loading\u2026");
    body.append(article);
    rendered(path, root).then(
      (html) => {
        article.innerHTML = html;
      },
      (e) => article.textContent = "could not render: " + message(e)
    );
  };
  body.addEventListener("click", (ev) => {
    const a = ev.target.closest("a");
    const href = a?.getAttribute("href") ?? "";
    if (!href.startsWith("#") || href.startsWith("#/")) return;
    ev.preventDefault();
    document.getElementById(decodeURIComponent(href.slice(1)))?.scrollIntoView({ behavior: "smooth", block: "start" });
  });
  asRendered.addEventListener("click", () => show(false));
  asRaw.addEventListener("click", () => show(true));
  show(session.get("tr.md.raw", false));
  return { seg, body };
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
var media2 = null;
var panel = null;
function render7(root, ctx) {
  const path = ctx.path;
  const dir = dirname(path);
  const name = basename(path) || "/";
  const kind2 = mediaKind(ext(name));
  const inTrash = ctx.root !== void 0;
  const asOf = ctx.at !== void 0;
  let snap;
  let offlineErr = null;
  let versionsPanel2 = null;
  let siblings = [];
  let entry;
  let trashRoot;
  let nsfw = false;
  let folder3 = { protected: false, pinned: false };
  let keyHandler = null;
  if (kind2 === "video") player.pause();
  const draw = () => {
    clear(root);
    const section2 = h("section", { class: "player" }, crumbsAll({ path: dir, root: ctx.root, at: ctx.at }, snap?.name));
    if (asOf && offlineErr) {
      section2.append(...offlineView(ctx, offlineErr, fileHash({ path })));
      root.append(section2);
      return;
    }
    if (asOf) section2.append(banner(ctx, snap, "", false, fileHash({ path })));
    if (inTrash && (trashRoot || entry?.deleted_at)) {
      const r = trashRoot ?? { ino: ctx.root, name, type: "file", size: entry?.size ?? 0, path: name, deleted_at: entry?.deleted_at ?? "", restorable: true };
      section2.append(banner2({ root: { ...r, purge_at: r.purge_at ?? entry?.purge_at }, bytes: entry?.size }));
    }
    const info = entry ? `${bytes(entry.size)} \xB7 ${when(entry.mtime)}` : "";
    const actions = h("span", { class: "actions" }, h("a", { class: "btn", href: fileURL(path, true, src(ctx)) }, "Download"));
    if (!inTrash) actions.prepend(asofMenu(ctx, "file", snap));
    const titleRow = h("div", { class: "title-row" }, h("h1", null, name), h("span", { class: "small dim mono" }, info), actions);
    section2.append(titleRow);
    const veiled = nsfw && !session.get("tr.revealed", false);
    const viewer = h("div", { class: "viewer" + (nsfw ? " nsfw" : "") + (nsfw && !veiled ? " revealed" : "") });
    section2.append(viewer);
    const group = siblings.filter((s) => sameGroup(mediaKind(ext(s.name)), kind2));
    const index = group.findIndex((s) => s.name === name);
    const prev = index > 0 ? group[index - 1] : void 0;
    const next2 = index >= 0 && index < group.length - 1 ? group[index + 1] : void 0;
    const go = (e) => {
      if (e) location.hash = fileHash({ path: join(dir, e.name), root: ctx.root, at: ctx.at });
    };
    if (kind2 === "video") {
      const mime = KINDS[ext(name)]?.[1] ?? "";
      let el;
      if (media2 && media2.dir === dir) {
        el = media2.el;
        el.src = fileURL(path, false, src(ctx));
        void el.play().catch(() => void 0);
      } else {
        media2?.el.pause();
        el = document.createElement("video");
        el.controls = true;
        el.preload = "metadata";
        el.playsInline = true;
        el.src = fileURL(path, false, src(ctx));
        media2 = { el, dir };
      }
      el.onended = () => go(next2);
      viewer.append(el);
      if (mime && el.canPlayType(mime) === "") {
        section2.append(
          h("div", { class: "note" }, "Your browser reports it cannot play ", h("span", { class: "mono" }, mime), ". tailraid serves files as stored, without transcoding; mkv, avi and flac play only where the browser has a native decoder. ", h("a", { href: fileURL(path, true, src(ctx)) }, "Download instead"), ".")
        );
      }
    } else if (kind2 === "audio") {
      viewer.append(audioCard(ctx, name, entry, group));
    } else if (kind2 === "image") {
      viewer.append(h("img", { src: fileURL(path, false, src(ctx)), alt: name }));
      if (next2) new Image().src = fileURL(join(dir, next2.name), false, src(ctx));
    } else if (kind2 === "text") {
      if (entry && entry.size > TEXT_MAX) {
        viewer.append(h("div", { class: "placeholder" }, "too large to preview", h("a", { class: "btn", href: fileURL(path, true, src(ctx)) }, "Download")));
      } else if (isMarkdown(name)) {
        const doc = view(path, src(ctx));
        titleRow.insertBefore(doc.seg, actions);
        if (nsfw) viewer.append(doc.body);
        else viewer.replaceWith(doc.body);
      } else {
        const pre = h("pre", null, "loading\u2026");
        viewer.append(pre);
        text(path, src(ctx)).then(
          (t) => pre.textContent = t,
          (e) => pre.textContent = "could not load: " + (e instanceof Error ? e.message : String(e))
        );
      }
    } else if (kind2 === "pdf") {
      viewer.append(h("iframe", { src: fileURL(path, false, src(ctx)), title: name }));
      section2.append(h("div", { class: "small dim" }, h("a", { href: fileURL(path, false, src(ctx)), target: "_blank", rel: "noopener" }, "Open in a new tab")));
    } else {
      viewer.append(h("div", { class: "placeholder" }, info || name, h("a", { class: "btn", href: fileURL(path, true, src(ctx)) }, "Download")));
    }
    if (nsfw) {
      viewer.append(h("span", { class: "veil" }));
      viewer.addEventListener("click", () => {
        viewer.classList.add("revealed");
        session.set("tr.revealed", true);
      });
    }
    if (group.length > 1 && kind2 !== "text" && kind2 !== "other" && kind2 !== "pdf") {
      const label = kind2 === "image" ? "image" : "track";
      section2.append(
        h(
          "div",
          { class: "controls" },
          h("button", { type: "button", disabled: !prev, onClick: () => go(prev) }, icon("chevron_left"), "Previous"),
          h("button", { type: "button", disabled: !next2, onClick: () => go(next2) }, "Next", icon("chevron_right")),
          h("span", { class: "track" }, `${label} ${index + 1} of ${group.length}` + (next2 ? ` \xB7 next: ${next2.name}` : ""))
        )
      );
    }
    if (entry) section2.append(details(path, entry, kind2, KINDS[ext(name)]?.[1] ?? "", folder3, nsfw, viewer));
    if (!inTrash && !asOf) {
      versionsPanel2 ??= versionsPanel(path, () => void reload());
      section2.append(versionsPanel2);
      const cs = status.current?.volume.chunk_size;
      const expected = entry && cs ? Math.ceil(entry.size / cs) : void 0;
      if (!panel || panel.path !== path) panel = { path, p: render6(path, expected) };
      else if (expected !== void 0) panel.p.expect(expected);
      section2.append(panel.p.el);
    }
    root.append(section2);
    if (keyHandler) document.removeEventListener("keydown", keyHandler);
    keyHandler = (ev) => {
      const t = ev.target;
      if (t && (t.tagName === "INPUT" || t.tagName === "TEXTAREA" || t.tagName === "VIDEO" || t.tagName === "AUDIO")) return;
      if (kind2 === "audio") return;
      if (ev.key === "ArrowRight") go(next2);
      else if (ev.key === "ArrowLeft") go(prev);
      else if (ev.key === "Escape") location.hash = inTrash ? `#/trash/${ctx.root}` + encodeForHash(dir) : browseHash({ path: dir, at: ctx.at });
      else if (ev.key === " " && media2) {
        ev.preventDefault();
        if (media2.el.paused) void media2.el.play().catch(() => void 0);
        else media2.el.pause();
      }
    };
    document.addEventListener("keydown", keyHandler);
  };
  draw();
  const reload = async () => {
    const r = await ls(dir, src(ctx)).catch(() => null);
    if (r) {
      siblings = r.entries.filter((e) => e.type === "file" && !e.upload && !e.deleted_at);
      entry = siblings.find((e) => e.name === name);
    }
    if (media2 && media2.dir === dir) media2.dir = "";
    draw();
  };
  const listing = inTrash && dir === "/" && name !== "/" ? ls("/", ctx.root) : ls(dir, src(ctx));
  listing.then(
    (r) => {
      siblings = r.entries.filter((e) => e.type === "file" && !e.upload && !e.deleted_at);
      entry = siblings.find((e) => e.name === name);
      nsfw = r.dir.nsfw || !!entry?.nsfw;
      folder3 = { protected: r.dir.protected, pinned: r.dir.pinned };
      snap = r.dir.snapshot;
      draw();
    },
    (e) => {
      if (asOf && isOffline(e)) {
        offlineErr = e;
        void catalogue().then(draw, draw);
        return;
      }
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
    if (leaving && media2) {
      media2.el.pause();
      media2.el.removeAttribute("src");
      media2.el.load();
      media2 = null;
    }
  };
}
function audioCard(ctx, name, entry, group) {
  const img = h("img", { class: "cover", alt: "" });
  img.src = thumbURL(ctx.path, src(ctx));
  img.addEventListener("error", () => img.removeAttribute("src"));
  const title = h("h1", null, name.replace(/\.[^.]+$/, ""));
  const artist = h("div", { class: "artist" });
  const meta2 = h("div", { class: "meta" });
  const dirPath = dirname(ctx.path);
  const tracks = group.filter((e) => mediaKind(ext(e.name)) === "audio").map((e) => ({ path: join(dirPath, e.name), root: src(ctx) }));
  const index = Math.max(0, tracks.findIndex((t) => t.path === ctx.path));
  const play = h("button", { type: "button", onClick: () => tracks.length ? player.play(tracks, index) : player.playNow({ path: ctx.path, root: src(ctx) }) }, ...player.isCurrent(ctx.path, src(ctx)) && player.playing ? [icon("pause_fill"), "Playing"] : [icon("play_arrow_fill"), "Play"]);
  const queueBtn = h("button", { type: "button", onClick: () => player.enqueue([{ path: ctx.path, root: src(ctx) }]) }, "Add to queue");
  const head = h("div", { class: "album-head" }, img, h("div", null, title, artist, meta2, h("div", { class: "controls" }, play, queueBtn)));
  tags(ctx.path, src(ctx)).then(
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
      meta2.replaceChildren(...parts.map((p) => h("span", null, p)));
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
var DETAILS_KEY = "tr.details";
function details(path, e, kind2, mime, f, nsfw, viewer) {
  const dl = h("dl", { class: "kv facts" });
  const row3 = (k, v2, title, full = false) => dl.append(h("div", full ? { class: "full" } : null, h("dt", null, k), h("dd", title ? { title } : null, v2)));
  row3("path", path, void 0, true);
  row3("type", mime ? `${kind2} \xB7 ${mime}` : kind2);
  row3("size", bytes(e.size), `${e.size.toLocaleString()} bytes`);
  row3("modified", date2(e.mtime), e.mtime);
  row3("mode", "0" + (e.mode & 511).toString(8));
  row3("inode", `${e.ino.toLocaleString()} \xB7 v${e.version}`);
  const v = status.current?.volume;
  if (v) row3("chunks", `${Math.ceil(e.size / v.chunk_size).toLocaleString()} \xB7 ${bytes(v.chunk_size)} \xB7 ${v.k}+${v.m}`);
  const flags = [f.protected ? "protected" : "", nsfw ? "nsfw" : "", f.pinned ? "kept here" : ""].filter(Boolean);
  if (flags.length) row3("folder", flags.join(" \xB7 "));
  const vid = viewer.querySelector("video");
  if (vid) {
    const add = () => {
      if (Number.isFinite(vid.duration)) row3("length", mmss(vid.duration));
      if (vid.videoWidth) row3("resolution", `${vid.videoWidth} \xD7 ${vid.videoHeight}`);
    };
    if (vid.readyState >= HTMLMediaElement.HAVE_METADATA) add();
    else vid.addEventListener("loadedmetadata", add, { once: true });
  }
  const img = viewer.querySelector("img");
  if (img) {
    const add = () => row3("resolution", `${img.naturalWidth} \xD7 ${img.naturalHeight}`);
    if (img.complete && img.naturalWidth) add();
    else img.addEventListener("load", add, { once: true });
  }
  const panel2 = h("details", { class: "panel" }, h("summary", null, "Details"), h("div", { class: "body" }, dl));
  panel2.open = session.get(DETAILS_KEY, false);
  panel2.addEventListener("toggle", () => session.set(DETAILS_KEY, panel2.open));
  return panel2;
}
function date2(iso) {
  const t = Date.parse(iso);
  if (Number.isNaN(t)) return iso;
  const d = new Date(t);
  const p = (n) => String(n).padStart(2, "0");
  return `${d.getFullYear()}-${p(d.getMonth() + 1)}-${p(d.getDate())} ${p(d.getHours())}:${p(d.getMinutes())}`;
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
var FAST = 4;
var GRABS = 1;
var WIDTH = 320;
var RECIPE = 2;
var POSITIONS = [0.5, 0.25, 0.75, 0.1, 0.9];
var MIN_LUMA = 28;
var MIN_SPREAD = 14;
var PROBE = 64;
var LOAD_TIMEOUT = 2e4;
var FRAME_TIMEOUT = 15e3;
var NO_FRAME = /* @__PURE__ */ new Set(["avi", "wmv", "mpg", "mpeg", "flv", "vob", "rm", "rmvb", "divx", "asf"]);
var Lane = class {
  constructor(max) {
    this.max = max;
  }
  max;
  queue = [];
  active = 0;
  push(job) {
    this.queue.push(job);
    this.pump();
  }
  /** take removes a job that has not started. */
  take(job) {
    const i = this.queue.indexOf(job);
    if (i >= 0) this.queue.splice(i, 1);
  }
  pump() {
    while (this.active < this.max && this.queue.length) {
      const job = this.queue.shift();
      this.active++;
      void job().catch(() => void 0).finally(() => {
        this.active--;
        this.pump();
      });
    }
  }
};
var fast = new Lane(FAST);
var grabs = new Lane(GRABS);
var pending2 = /* @__PURE__ */ new Map();
var observer = typeof IntersectionObserver === "undefined" ? null : new IntersectionObserver(
  (entries) => {
    for (const e of entries) {
      const p = pending2.get(e.target);
      if (!p) continue;
      if (e.isIntersecting && !p.queued) {
        p.queued = true;
        fast.push(p.job);
      } else if (!e.isIntersecting && p.queued) {
        p.queued = false;
        fast.take(p.job);
      }
    }
  },
  { rootMargin: "200px" }
);
function lazy(img, e, path, root, kind2, onMiss) {
  const miss = () => {
    if (img.isConnected) onMiss();
  };
  const show = async (url) => {
    const src2 = await load(url);
    if (!src2) return miss();
    if (!img.isConnected) return URL.revokeObjectURL(src2);
    return showBlob(img, src2, onMiss);
  };
  const tag = kind2 === "video" ? `${e.version}.f${RECIPE}` : void 0;
  const job = async () => {
    pending2.delete(img);
    observer?.unobserve(img);
    if (!img.isConnected) return;
    if (kind2 === "dir" && root === void 0) {
      const r = await folder(path);
      if (r.src) return showBlob(img, r.src, onMiss);
      if (!r.grab) return miss();
      const g = r.grab;
      grabs.push(async () => {
        if (!img.isConnected) return;
        if (!near(img)) return wait2();
        if (!await grab(join(path, g.path), { ino: g.ino, version: g.version, thumb: false })) return miss();
        const again = await folder(path);
        if (again.src) await showBlob(img, again.src, onMiss);
        else miss();
      });
      return;
    }
    if (e.thumb || kind2 !== "video" || root !== void 0) return show(thumbURL(path, root, tag));
    grabs.push(async () => {
      if (!img.isConnected) return;
      if (!near(img)) return wait2();
      if (await grab(path, e)) await show(thumbURL(path, void 0, tag));
      else miss();
    });
  };
  const p = { img, job, queued: false };
  const wait2 = () => {
    if (!observer) return;
    p.queued = false;
    pending2.set(img, p);
    observer.observe(img);
  };
  if (observer) wait2();
  else {
    p.queued = true;
    fast.push(job);
  }
}
function near(img) {
  const r = img.getBoundingClientRect();
  return r.bottom >= -200 && r.top <= window.innerHeight + 200;
}
async function load(url) {
  try {
    const r = await fetch(url, { signal: AbortSignal.timeout(LOAD_TIMEOUT) });
    if (!r.ok) return void 0;
    return URL.createObjectURL(await r.blob());
  } catch {
    return void 0;
  }
}
function showBlob(img, src2, onMiss) {
  return new Promise((resolve) => {
    img.onload = () => {
      URL.revokeObjectURL(src2);
      resolve();
    };
    img.onerror = () => {
      URL.revokeObjectURL(src2);
      onMiss();
      resolve();
    };
    img.src = src2;
  });
}
async function folder(path) {
  const r = await fetch(thumbURL(path), { signal: AbortSignal.timeout(LOAD_TIMEOUT) }).catch(() => void 0);
  if (!r) return {};
  if (r.ok) return { src: URL.createObjectURL(await r.blob()) };
  if (r.status !== 404) return {};
  const miss = await r.json().catch(() => ({}));
  return { grab: miss.grab };
}
async function grab(path, e) {
  if (NO_FRAME.has(ext(path))) return false;
  const key = `tr.vthumb:${e.ino}.${e.version}.f${RECIPE}`;
  if (session.get(key, false)) return false;
  session.set(key, true);
  const blob = await frame(fileURL(path));
  if (!blob) return false;
  await putThumb(path, e.version, blob);
  return true;
}
async function frame(src2) {
  const v = document.createElement("video");
  v.preload = "auto";
  v.muted = true;
  v.playsInline = true;
  v.crossOrigin = "anonymous";
  let timer = 0;
  const expired = new Promise((resolve) => {
    timer = window.setTimeout(() => resolve(null), FRAME_TIMEOUT);
  });
  const choose = async () => {
    v.src = src2;
    await event(v, "loadedmetadata");
    if (!Number.isFinite(v.duration) || v.duration <= 0) return null;
    const w = Math.min(WIDTH, v.videoWidth || WIDTH);
    const h2 = Math.max(1, Math.round((v.videoHeight || 9) * (w / (v.videoWidth || 16))));
    let best = null;
    let bestScore = -1;
    for (const at of POSITIONS) {
      v.currentTime = v.duration * at;
      await event(v, "seeked");
      await presented(v);
      const c = document.createElement("canvas");
      c.width = w;
      c.height = h2;
      const g = c.getContext("2d");
      if (!g) return null;
      g.drawImage(v, 0, 0, w, h2);
      const { mean, spread: spread2 } = luma(c);
      if (mean >= MIN_LUMA && spread2 >= MIN_SPREAD) {
        best = c;
        break;
      }
      const score = spread2 + mean / 4;
      if (score > bestScore) {
        best = c;
        bestScore = score;
      }
    }
    if (!best) return null;
    const chosen = best;
    return new Promise((resolve) => chosen.toBlob((b) => resolve(b), "image/jpeg", 0.8));
  };
  try {
    return await Promise.race([choose().catch(() => null), expired]);
  } finally {
    clearTimeout(timer);
    v.removeAttribute("src");
    v.load();
  }
}
function event(v, name) {
  return new Promise((resolve, reject) => {
    const ok = () => {
      v.removeEventListener("error", bad);
      resolve();
    };
    const bad = () => {
      v.removeEventListener(name, ok);
      reject(new Error("video error"));
    };
    v.addEventListener(name, ok, { once: true });
    v.addEventListener("error", bad, { once: true });
  });
}
function presented(v) {
  return new Promise((resolve) => {
    const t = window.setTimeout(resolve, 1e3);
    const done = () => {
      clearTimeout(t);
      resolve();
    };
    if (typeof v.requestVideoFrameCallback === "function") {
      v.requestVideoFrameCallback(() => done());
    } else if (v.readyState >= HTMLMediaElement.HAVE_CURRENT_DATA) {
      done();
    } else {
      v.addEventListener("loadeddata", done, { once: true });
    }
  });
}
function luma(c) {
  const p = document.createElement("canvas");
  p.width = PROBE;
  p.height = Math.max(1, Math.round(c.height * PROBE / c.width));
  const g = p.getContext("2d", { willReadFrequently: true });
  if (!g) return { mean: 0, spread: 0 };
  g.drawImage(c, 0, 0, p.width, p.height);
  const d = g.getImageData(0, 0, p.width, p.height).data;
  let sum = 0;
  let sq = 0;
  const n = d.length / 4;
  for (let i = 0; i < d.length; i += 4) {
    const y = 0.2126 * d[i] + 0.7152 * d[i + 1] + 0.0722 * d[i + 2];
    sum += y;
    sq += y * y;
  }
  const mean = sum / n;
  return { mean, spread: Math.sqrt(Math.max(0, sq / n - mean * mean)) };
}

// internal/webui/src/flags.ts
function menu(ctx) {
  const body = h("div", { class: "menu-body" });
  const details2 = h("details", { class: "menu" }, h("summary", null, "Folder"), body);
  const isRoot = ctx.path === "/";
  const row3 = (label, note, checked, disabled, onToggle) => {
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
  const libraryRow = (current, why) => {
    const select = h("select", { "aria-label": "library kind" });
    for (const word of ["none", "movies", "tv", "music", "roms"]) select.append(h("option", { value: word, selected: (current || "none") === word ? "" : void 0 }, word));
    const note = h("span", { class: "small dim" }, why ?? "the Media tab lists this folder as films, shows, music or games");
    const lab = h("label", { class: "menu-row library" }, h("span", null, "Library"), select, note);
    if (why) {
      select.disabled = true;
      lab.setAttribute("aria-disabled", "true");
    }
    select.addEventListener("change", () => {
      select.disabled = true;
      setFlags(ctx.path, { library: select.value }).then(
        () => ctx.onChange(),
        (e) => {
          select.value = current || "none";
          select.disabled = false;
          toast("Library: " + (e instanceof Error ? e.message : String(e)), "bad");
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
      row3("Protect", "nothing in it can be deleted, replaced or moved out, from any device", own?.protected ?? ctx.dir.protected, why ?? inherited(ctx.dir.protected, own?.protected), (on) => setFlags(ctx.path, { protected: on })),
      row3("Mark nsfw", "thumbnails and previews are blurred until tapped", own?.nsfw ?? ctx.dir.nsfw, why ?? inherited(ctx.dir.nsfw, own?.nsfw), (on) => setFlags(ctx.path, { nsfw: on })),
      row3(
        "Keep on this device",
        pinned ? `${self} holds every chunk in its cache \xB7 ${bytes(pinned.cached_bytes)} of ${bytes(pinned.bytes)} here` : `${self} fetches every chunk under it into its cache and keeps it there`,
        ctx.dir.pinned,
        why,
        (on) => pin(ctx.path, on)
      ),
      libraryRow(own?.library ?? "", why ?? (ctx.dir.library && !own?.library ? `inside a ${ctx.dir.library} library: this folder is listed as part of it` : null))
    );
  };
  details2.addEventListener("toggle", () => {
    if (details2.open) void fill();
  });
  return details2;
}

// internal/webui/src/album.ts
function isAlbum(entries) {
  const live = entries.filter((e) => !e.deleted_at && !e.upload && e.type === "file");
  const audio = live.filter((e) => isAudioName(e.name)).length;
  return audio > 0 && audio > live.length - audio;
}
var PROBES = 4;
function render8(ctx, cover, entries) {
  const audio = entries.filter((e) => !e.deleted_at && !e.upload && e.type === "file" && isAudioName(e.name)).sort((a, b) => trackNo(a.name) - trackNo(b.name) || a.name.localeCompare(b.name, void 0, { numeric: true }));
  const tracks = audio.map((e) => ({ path: join(ctx.path, e.name), root: src(ctx) }));
  const img = h("img", { class: "cover", alt: "" });
  if (cover !== null) img.src = thumbURL(cover ? join(ctx.path, cover) : ctx.path, src(ctx));
  img.addEventListener("error", () => img.removeAttribute("src"));
  const title = h("h1", null, basename(ctx.path) || "/");
  const artist = h("div", { class: "artist" });
  const meta2 = h("div", { class: "meta" });
  const head = h("div", { class: "album-head" }, img, h("div", null, title, artist, meta2));
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
    meta2.replaceChildren(...parts.map((p) => h("span", null, p)));
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
    tr.replaceChildren(h("td", { class: "no" }, playing ? icon("equalizer") : String(no)), titleCell, h("td", { class: "dur" }, mmss(durs.get(i))));
  };
  audio.forEach((_, i) => {
    const tr = h("tr");
    tr.addEventListener("click", () => player.play(tracks, i));
    rows.push(tr);
    body.append(tr);
    drawRow(i);
  });
  describe();
  let active = 0;
  const pending3 = [];
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
  const pump = () => {
    while (active < PROBES && pending3.length) {
      const i = pending3.shift();
      active++;
      void probe(i).finally(() => {
        active--;
        pump();
      });
    }
  };
  if (typeof IntersectionObserver !== "undefined") {
    const io = new IntersectionObserver(
      (es) => {
        for (const x of es) {
          if (!x.isIntersecting) continue;
          io.unobserve(x.target);
          pending3.push(rows.indexOf(x.target));
        }
        pump();
      },
      { rootMargin: "300px" }
    );
    for (const tr of rows) io.observe(tr);
  } else {
    rows.forEach((_, i) => pending3.push(i));
    pump();
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
function duration(src2) {
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
    a.src = src2;
  });
}

// internal/webui/src/browser.ts
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
function render9(root, ctx) {
  const dir = ctx.path;
  const inTrash = ctx.root !== void 0;
  const asOf = ctx.at !== void 0;
  const source = src(ctx);
  let sort = session.get("tr.sort", { key: "name", desc: false });
  let view3 = session.get("tr.view", "list");
  let showDeleted = session.get("tr.deleted", false);
  let entries = [];
  let info = { protected: false, nsfw: false, pinned: false };
  let trashRoot;
  let formOpen = false;
  let renaming = null;
  const sel = new Selection();
  let alb = null;
  const live = () => entries.filter((e) => !e.deleted_at);
  const pathOf = (e) => join(dir, e.name);
  const refresh2 = async () => {
    try {
      const r = await ls(dir, source);
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
      if (asOf && isOffline(e)) {
        await catalogue().catch(() => null);
        root.append(h("section", null, crumbs(ctx), ...offlineView(ctx, e, browseHash({ path: dir }))));
        return;
      }
      const msg = e instanceof Error ? e.message : String(e);
      root.append(h("section", null, crumbs(ctx), h("div", { class: "error" }, `Could not list ${dir}: ${msg}. `, h("a", { href: "#", onClick: (ev) => {
        ev.preventDefault();
        void refresh2();
      } }, "Retry"))));
    }
  };
  const draw = () => {
    clear(root);
    const section2 = h("section", null, crumbs(ctx, info, inTrash));
    if (inTrash) {
      const files = entries.filter((e) => e.type === "file");
      const rootEntry = trashRoot ?? { ino: ctx.root, name: "", type: "dir", size: 0, path: "", deleted_at: info.deleted_at ?? "", restorable: true };
      if (info.purge_at && !trashRoot) rootEntry.purge_at = info.purge_at;
      section2.append(banner2({ root: { ...rootEntry, purge_at: rootEntry.purge_at ?? info.purge_at }, items: entries.length, bytes: files.reduce((s, e) => s + e.size, 0) }));
    }
    const input = h("input", { type: "file", multiple: true, "aria-label": "upload files" });
    input.addEventListener("change", () => {
      if (input.files?.length) enqueue(dir, input.files);
      input.value = "";
    });
    const total = live().reduce((s, e) => s + (e.type === "file" ? e.size : 0), 0);
    const deleted = entries.length - live().length;
    if (asOf) section2.append(banner(ctx, info.snapshot, `${live().length} ${live().length === 1 ? "item" : "items"} \xB7 ${bytes(total)}`, true, browseHash({ path: dir })));
    const toolbar = h("div", { class: "toolbar" });
    const asAlbum = view3 === "list" && !isAlbum(entries) ? false : view3 === "list";
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
    if (!inTrash && !asOf) {
      toolbar.append(
        h("label", { class: "btn" }, "Upload", input),
        h("button", { type: "button", "aria-pressed": formOpen ? "true" : "false", onClick: () => {
          formOpen = !formOpen;
          renaming = null;
          draw();
        } }, "New folder")
      );
    }
    if (!asOf) toolbar.append(menu({ path: dir, inTrash, dir: info, onChange: () => void refresh2() }));
    if (!inTrash) toolbar.append(asofMenu(ctx, "browse", info.snapshot));
    const seg = h(
      "span",
      { class: "seg", role: "group", "aria-label": "view" },
      h("button", { type: "button", "aria-pressed": view3 === "list" ? "true" : "false", onClick: () => setView("list") }, "List"),
      h("button", { type: "button", "aria-pressed": view3 === "tiles" ? "true" : "false", onClick: () => setView("tiles") }, "Tiles")
    );
    toolbar.append(seg);
    if (!inTrash && !asOf) {
      const box = h("input", { type: "checkbox" });
      box.checked = showDeleted;
      box.addEventListener("change", () => {
        showDeleted = box.checked;
        session.set("tr.deleted", showDeleted);
        draw();
      });
      toolbar.append(h("label", { class: "toggle" }, box, "show deleted"));
    }
    toolbar.append(h("span", { class: "spacer" }), h("span", { class: "small dim" }, `${live().length} ${live().length === 1 ? "item" : "items"} \xB7 ${bytes(total)}` + (deleted ? ` \xB7 ${deleted} deleted` : "")));
    section2.append(toolbar);
    const shown = queue.filter((j) => j.state !== "done");
    if (shown.length) {
      const list = h("div", { class: "uploads" });
      for (const j of shown) list.append(jobRow(j));
      section2.append(list);
    }
    if (formOpen) section2.append(folderForm());
    if (renaming) section2.append(renameForm(renaming));
    alb = null;
    if (sorted(entries).length === 0) section2.append(h("div", { class: "empty" }, deleted ? "empty folder \xB7 tick show deleted to list what was deleted here" : "empty folder"));
    else if (asAlbum) {
      alb = render8(ctx, info.no_cover ? null : info.cover, entries);
      section2.append(alb.el);
      const rest = sorted(entries).filter((e) => !(e.type === "file" && isAudioName(e.name) && !e.deleted_at));
      if (rest.length) section2.append(table(rest));
    } else if (view3 === "tiles") section2.append(tiles());
    else section2.append(table(entries));
    section2.append(selbar());
    root.append(section2);
  };
  const setView = (v) => {
    view3 = v;
    session.set("tr.view", v);
    draw();
  };
  const folderForm = () => {
    const name = h("input", { type: "text", placeholder: "folder name", "aria-label": "folder name", autofocus: true });
    const form = h("form", { class: "inline-form" }, icon("folder"), name, h("button", { type: "submit" }, "Create"), h("button", { type: "button", onClick: () => {
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
          void refresh2();
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
          void refresh2();
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
    const gone = list.filter((e) => e.deleted_at && (showDeleted || inTrash)).sort((a, b) => (b.deleted_at ?? "").localeCompare(a.deleted_at ?? "") || a.name.localeCompare(b.name));
    return [...livePart, ...gone];
  };
  const order = () => sorted(entries).filter((e) => !e.deleted_at).map((e) => e.name);
  const onSelect = (e, ev) => {
    ev.stopPropagation();
    sel.click(e.name, ev.shiftKey, order());
  };
  const selectBox = (e, cls) => e.deleted_at || asOf ? h("span", { class: cls }) : (() => {
    const box = h("input", { type: "checkbox", "aria-label": "select" });
    box.checked = sel.has(e.name);
    box.addEventListener("click", (ev) => onSelect(e, ev));
    return h("span", { class: cls }, box);
  })();
  const linkOf = (e) => {
    const path = pathOf(e);
    if (e.deleted_at) return e.type === "dir" ? { href: `#/trash/${e.ino}/`, kind: "folder" } : { href: `#/trashfile/${e.ino}/`, kind: iconFor(e.name) };
    if (e.type === "dir") return { href: browseHash({ path, root: ctx.root, at: ctx.at }), kind: "folder" };
    return { href: fileHash({ path, root: ctx.root, at: ctx.at }), kind: iconFor(e.name) };
  };
  const playBtn = (e, tile5 = false) => {
    const t = { path: pathOf(e), root: e.deleted_at ? e.ino : source };
    if (e.deleted_at) t.path = "/";
    const now = player.isCurrent(t.path, t.root) && player.playing;
    const b = h("button", { type: "button", class: tile5 ? "play icon-btn" : "play", onClick: (ev) => {
      ev.preventDefault();
      ev.stopPropagation();
      if (player.isCurrent(t.path, t.root)) player.toggle();
      else playFrom(e);
    } });
    playFace(b, now);
    return b;
  };
  const playFrom = (e) => {
    const tracks = sorted(entries).filter((x) => !x.deleted_at && x.type === "file" && isAudioName(x.name)).map((x) => ({ path: pathOf(x), root: source }));
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
    if (e.library) b.append(h("span", { class: "badge lib" }, e.library));
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
    const head = h("tr", null, th("name", "name"), th("size", "size", "size"), th("mtime", "modified", "mtime"));
    if (!asOf) head.prepend(h("th", { class: "sel" }, all));
    const t = h("table", { class: "files" }, h("thead", null, head));
    const body = h("tbody");
    for (const e of sorted(list)) body.append(row3(e));
    t.append(body);
    return t;
  };
  const row3 = (e) => {
    const { href, kind: kind2 } = linkOf(e);
    let link;
    if (e.type === "symlink") link = h("span", { class: "linkname" }, icon("link"), e.name + " ", h("span", { class: "target" }, "\u2192 " + (e.target ?? "")));
    else if (e.upload) link = h("span", { class: "dim", title: "an upload in progress, or one that was not resumed" }, icon("draft"), e.name);
    else {
      link = h("a", { href }, icon(kind2), e.name);
      if (e.protected) link.append(h("span", { class: "badge lock" }, "protected"));
      if (e.nsfw) link.append(h("span", { class: "badge nsfw" }, "nsfw"));
      if (e.library) link.append(h("span", { class: "badge lib" }, e.library));
      if (e.deleted_at) link.append(badge(e.purge_at));
      if (e.type === "file" && isAudioName(e.name)) link.append(playBtn(e));
    }
    const cls = [e.upload ? "dim" : e.dirty ? "dirty" : "", e.deleted_at ? "deleted" : "", sel.has(e.name) ? "selected" : "", e.type === "file" && player.isCurrent(pathOf(e), source) ? "playing" : ""].filter(Boolean).join(" ");
    const tr = h("tr", { class: cls, "data-name": e.name }, h("td", { class: "name" }, link), h("td", { class: "size" }, e.type === "file" ? bytes(e.size) : ""), h("td", { class: "mtime", title: e.mtime }, when(e.mtime)));
    if (!asOf) tr.prepend(h("td", { class: "sel" }, selectBox(e, "sel").firstChild ?? ""));
    return tr;
  };
  const tiles = () => {
    const grid = h("div", { class: "ftiles" });
    for (const e of sorted(entries)) grid.append(tile4(e));
    return grid;
  };
  const tile4 = (e) => {
    const { href, kind: kind2 } = linkOf(e);
    const kindOf = mediaKind(ext(e.name));
    const thumb = h("span", { class: "thumb" + (info.nsfw || e.nsfw ? " nsfw" : "") });
    const showIcon = () => thumb.replaceChildren(e.type === "dir" && e.card ? titleCard(e.card.title, e.card.sub) : icon(kind2));
    const path = pathOf(e);
    const thumbPath = e.type === "dir" && e.cover ? join(path, e.cover) : path;
    const canThumb = e.type === "dir" ? !e.no_cover && (!e.deleted_at || !!e.cover) : e.type === "file" && !e.upload && (kindOf === "image" || kindOf === "video" || kindOf === "audio");
    if (canThumb) {
      const img = h("img", { alt: "" });
      thumb.append(img);
      const root2 = e.deleted_at ? e.ino : source;
      const tp = e.deleted_at ? e.type === "dir" && e.cover ? "/" + e.cover : "/" : thumbPath;
      lazy(img, e, tp, root2, e.type === "dir" ? e.cover ? "audio" : "dir" : kindOf, showIcon);
      if (e.type === "dir") {
        const words = e.card ?? { title: e.name };
        img.addEventListener("load", () => thumb.append(h("span", { class: "tover" }, h("span", { class: "ct" }, words.title), words.sub ? h("span", { class: "cy" }, words.sub) : null)), { once: true });
      }
    } else showIcon();
    if (e.type === "file") thumb.append(h("span", { class: "ov br" }, bytes(e.size)));
    const kindWord = e.type === "dir" ? "folder" : e.type === "symlink" ? "link" : ext(e.name).toLowerCase() || "file";
    const meta2 = h("span", { class: "tmeta" }, h("span", null, kindWord), h("span", { title: e.mtime }, when(e.mtime)));
    const cls = ["ftile", e.type === "dir" ? "dir" : "", sel.has(e.name) ? "selected" : "", e.dirty ? "dirty" : "", e.deleted_at ? "deleted" : "", e.type === "file" && player.isCurrent(path, source) ? "playing" : ""].filter(Boolean).join(" ");
    const a = h("a", { class: cls, href, "data-name": e.name }, selectBox(e, "sel"), badges(e), thumb, h("span", { class: "tname", title: e.name }, e.name), meta2);
    if (e.type === "file" && isAudioName(e.name) && !e.upload) thumb.append(playBtn(e, true));
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
    const delBtn = action("delete", "Delete", () => void doDelete(chosen));
    const mvBtn = action("drive_file_move", "Move to\u2026", () => void doMove(chosen));
    const rnBtn = action("edit", "Rename", () => {
      renaming = chosen[0].name;
      formOpen = false;
      draw();
    });
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
      bar2.append(action("play_arrow_fill", "Play", () => void playSelection(chosen, false)), action("playlist_add", "Add to queue", () => void playSelection(chosen, true)));
    }
    bar2.append(h("span", { class: "spacer" }), action("close", "Clear", () => sel.clear()));
    return bar2;
  };
  const playSelection = async (chosen, append2) => {
    const tracks = [];
    for (const e of chosen) {
      if (e.type === "dir") {
        try {
          const r = await ls(pathOf(e), source);
          for (const x of r.entries) if (x.type === "file" && isAudioName(x.name) && !x.deleted_at && !x.upload) tracks.push({ path: join(pathOf(e), x.name), root: source });
        } catch {
        }
      } else tracks.push({ path: pathOf(e), root: source });
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
      void refresh2();
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
      void refresh2();
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
    if (inTrash || asOf) return;
    ev.preventDefault();
    root.querySelector("table.files")?.classList.add("drop");
  };
  const onDragLeave = () => root.querySelector("table.files")?.classList.remove("drop");
  const onDrop = (ev) => {
    if (inTrash || asOf) return;
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
    } else if (ev.key === "Delete" && chosen.length && !info.protected && !inTrash && !asOf && !chosen.some((e) => e.protected)) void doDelete(chosen);
    else if (ev.key === "F2" && chosen.length === 1 && !inTrash && !asOf) {
      renaming = chosen[0].name;
      draw();
    }
  };
  document.addEventListener("keydown", onKey);
  const unsubSel = sel.subscribe(draw);
  const unsubPlayer = player.subscribe(() => {
    if (alb) alb.refresh();
    root.querySelectorAll("tr.playing, .ftile.playing").forEach((el) => el.classList.remove("playing"));
    const c = player.current();
    const here = c && c.root === source && c.path.startsWith(dir === "/" ? "/" : dir + "/") ? c.path.slice(dir === "/" ? 1 : dir.length + 1) : "";
    for (const el of root.querySelectorAll("tr[data-name], .ftile[data-name]")) {
      const mine = here !== "" && el.dataset["name"] === here;
      el.classList.toggle("playing", mine);
      const b = el.querySelector("button.play");
      if (b) playFace(b, mine && player.playing);
    }
  });
  onQueueChange = draw;
  onJobDone = (j) => {
    if (j.dir === dir) void refresh2();
  };
  void refresh2();
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
  const el = h("div", { class: "crumbs" }, h("a", { href: browseHash({ path: "/", at: ctx.at }) }, "/"));
  const parts = ctx.path.split("/").filter(Boolean);
  let acc = "";
  parts.forEach((p, i) => {
    acc += "/" + p;
    if (i > 0 || inTrash) el.append(h("span", { class: "sep" }, "/"));
    if (i === parts.length - 1) el.append(h("span", null, p));
    else el.append(h("a", { href: browseHash({ path: acc, root: ctx.root, at: ctx.at }) }, p));
  });
  if (inTrash) el.append(h("span", { class: "badge" }, "deleted"));
  if (info?.snapshot) el.append(h("span", { class: "badge" }, "as of " + info.snapshot.name));
  if (info?.protected) el.append(h("span", { class: "badge lock" }, "protected"));
  if (info?.nsfw) el.append(h("span", { class: "badge nsfw" }, "nsfw"));
  if (info?.library) el.append(h("span", { class: "badge lib" }, info.library));
  if (info?.pinned) el.append(h("span", { class: "badge pin" }, "kept here"));
  return el;
}
function crumbsAll(ctx, asOf) {
  const el = h("div", { class: "crumbs" }, h("a", { href: browseHash({ path: "/", at: ctx.at }) }, "/"));
  const parts = ctx.path.split("/").filter(Boolean);
  let acc = "";
  parts.forEach((p, i) => {
    acc += "/" + p;
    if (i > 0) el.append(h("span", { class: "sep" }, "/"));
    el.append(h("a", { href: browseHash({ path: acc, root: ctx.root, at: ctx.at }) }, p));
  });
  if (ctx.root !== void 0) el.append(h("span", { class: "badge" }, "deleted"));
  if (asOf) el.append(h("span", { class: "badge" }, "as of " + asOf));
  return el;
}
function playFace(b, now) {
  const label = now ? "Playing" : "Play";
  b.title = label;
  b.setAttribute("aria-label", label);
  b.replaceChildren(icon(now ? "pause_fill" : "play_arrow_fill"));
  if (!b.classList.contains("icon-btn")) b.append(h("span", null, label));
}
function action(name, label, onClick) {
  return h("button", { type: "button", "aria-label": label, title: label, onClick }, icon(name), h("span", null, label));
}
function iconFor(name, dir = false) {
  if (dir) return "folder";
  const k = mediaKind(ext(name));
  if (k === "image") return "image";
  if (k === "video") return "movie";
  if (k === "audio") return "music_note";
  return "draft";
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
  const range2 = RANGES.some(([r]) => r === q.get("range")) ? q.get("range") : "6h";
  return { range: range2, from: 0, to: 0, node: q.get("node") ?? "" };
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
function render10(root, query) {
  const state = parse(query);
  let timer;
  let disposed = false;
  let last2 = null;
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
    const row3 = (name, label, dot, note, m) => {
      const el = h("div", { class: "menu-row" + (m?.web ? " link" : ""), "aria-current": String(name === current) }, h("span", { class: "dot " + dot }), h("a", { href: hash({ ...state, node: name }) }, label), h("span", { class: "small dim" }, note));
      if (m?.web) {
        const link = memberLink(m.name, `http://${m.addr}:${m.web}/${hash({ ...state, node: name })}`, icon("open_in_new"));
        link.className = "menu-link";
        link.title = "open on " + m.name;
        link.setAttribute("aria-label", "open on " + m.name);
        el.append(link);
      }
      body.append(el);
    };
    row3("all", "all nodes", "off", "one series per member");
    for (const m of nodes) row3(m.name, m.name, m.live ? "ok" : "bad", (m.self ? "this device \xB7 " : "") + (m.live ? "live" : "dead") + (m.web ? "" : " \xB7 no web listener"), m);
  };
  const cards = /* @__PURE__ */ new Map();
  let order = [];
  const titles = /* @__PURE__ */ new Map();
  const empty = /* @__PURE__ */ new Set();
  const header2 = (f) => {
    last2 = f;
    lastAt = Date.now();
    const nodeName = state.node === "" ? f.node : state.node;
    summary.textContent = "Node: " + (nodeName === "all" ? "all nodes" : nodeName);
    drawMembers(f.nodes, nodeName);
    const span = f.range.to - f.range.from;
    sub.textContent = `${f.range.tier} rows \xB7 ${span >= 2 * 86400 ? Math.round(span / 86400) + " d" : Math.round(span / 3600) + " h"}` + (f.range.live ? " \xB7 refreshes every 5 s while live" : "");
  };
  const frames = (list) => {
    cards.clear();
    empty.clear();
    titles.clear();
    order = list.map((fr) => fr.id);
    clear(charts);
    for (const fr of list) {
      titles.set(fr.id, fr.title);
      const card3 = h("div", { class: "chart-card" });
      card3.innerHTML = fr.frame;
      card3.append(h("div", { class: "small dim" }, "loading"));
      cards.set(fr.id, card3);
      charts.append(card3);
    }
  };
  const fill = (l) => {
    if (!l.id) {
      if (l.error) fail(l.error);
      return;
    }
    let card3 = cards.get(l.id);
    if (l.empty) {
      card3?.remove();
      cards.delete(l.id);
      empty.add(l.id);
      return;
    }
    if (!l.svg) return;
    if (!card3) {
      charts.querySelector(":scope > .empty")?.remove();
      card3 = h("div", { class: "chart-card" });
      const next2 = order.slice(order.indexOf(l.id) + 1).find((id) => cards.has(id));
      charts.insertBefore(card3, next2 ? cards.get(next2) : null);
      cards.set(l.id, card3);
    }
    empty.delete(l.id);
    card3.innerHTML = l.svg;
    card3.append(h("div", { class: "small dim" }, (l.caption ?? "") + " \xB7 " + (titles.get(l.id) ?? l.id)));
  };
  const fail = (reason) => {
    for (const card3 of cards.values()) {
      const svg2 = card3.querySelector("svg.chart.pending");
      if (!svg2) continue;
      svg2.classList.add("failed");
      const note = svg2.querySelector(".note");
      if (note) note.textContent = "not drawn: " + reason;
      const cap = card3.querySelector("div.small");
      if (cap) cap.textContent = reason;
    }
  };
  const onScreen = () => {
    const ids = [];
    for (const [id, card3] of cards) {
      const r = card3.getBoundingClientRect();
      if (r.bottom > 0 && r.top < window.innerHeight) ids.push(id);
    }
    return [...ids, ...empty];
  };
  const setPill = (mode, err) => {
    pill2.className = "pill" + (mode === "live" ? " ok" : mode === "error" ? " bad" : "");
    pill2.textContent = mode === "error" ? "unreachable" : mode;
    pill2.title = mode === "error" ? err ?? "" : mode === "live" ? "last fetch " + new Date(lastAt).toLocaleTimeString() + " \xB7 next in 5 s" : "a custom range never refreshes";
  };
  const fetchOnce = async () => {
    const b = bounds(state, Date.now());
    const refresh2 = last2 !== null && cards.size + empty.size > 0;
    if (!refresh2) {
      pill2.className = "pill";
      pill2.textContent = "loading";
    }
    try {
      await streamStats(
        b.from,
        b.to,
        state.node,
        refresh2 ? onScreen() : null,
        (f) => {
          if (disposed) return;
          header2(f);
          if (!refresh2) frames(f.charts);
        },
        (l) => {
          if (!disposed) fill(l);
        }
      );
      if (disposed) return;
      if (cards.size === 0) charts.replaceChildren(h("div", { class: "empty" }, "no rows in this range yet"));
      setPill(last2?.range.live ? "live" : "paused");
    } catch (e) {
      if (disposed) return;
      const msg = e instanceof Error ? e.message : String(e);
      setPill("error", msg);
      if (cards.size > 0) fail(msg);
      else charts.replaceChildren(h("div", { class: "error" }, "Could not load the stats: " + msg));
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
      if (last2) setPill("paused");
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

// internal/webui/src/search.ts
var folder2 = "/";
function setFolder(p) {
  folder2 = p || "/";
}
function searchHash(q, under) {
  return "#/search/" + encodeURIComponent(q) + (under ? "?under=" + encodeURIComponent(under) : "");
}
function typing(t) {
  return t instanceof HTMLElement && (t.isContentEditable || t.tagName === "INPUT" || t.tagName === "TEXTAREA" || t.tagName === "SELECT");
}
function wire3(form) {
  const input = form.querySelector("input");
  let timer = 0;
  const go = () => {
    const q = input.value.trim();
    if (!q) return;
    const on = location.hash.startsWith("#/search/");
    const under = on ? new URLSearchParams(location.hash.split("?")[1] ?? "").get("under") ?? "" : "";
    const target = searchHash(q, under);
    if (target === location.hash) return;
    if (on) location.replace(target);
    else location.hash = target;
  };
  form.addEventListener("submit", (ev) => {
    ev.preventDefault();
    clearTimeout(timer);
    go();
  });
  input.addEventListener("input", () => {
    clearTimeout(timer);
    timer = window.setTimeout(() => {
      if (input.value.trim().length >= 2) go();
    }, 150);
  });
  input.addEventListener("keydown", (ev) => {
    if (ev.key === "Escape") {
      input.value = "";
      input.blur();
    }
  });
  document.addEventListener("keydown", (ev) => {
    if (ev.key === "/" && !ev.metaKey && !ev.ctrlKey && !typing(ev.target)) {
      ev.preventDefault();
      input.focus();
      input.select();
    }
  });
}
function fold(s) {
  return s.normalize("NFKD").replace(/\p{M}/gu, "").toLowerCase();
}
function marked(text2, terms) {
  const out = [];
  for (const part of text2.split(/([\p{L}\p{N}]+)/u)) {
    if (!part) continue;
    const f = fold(part);
    out.push(/[\p{L}\p{N}]/u.test(part) && terms.some((t) => f.startsWith(t)) ? h("mark", null, part) : part);
  }
  return out;
}
var SNIP_BEFORE = 100;
var SNIP_AFTER = 140;
var SNIP_AT_ONCE = 3;
async function snippet(hit, signal) {
  const off2 = Math.min(...hit.terms.map((t) => t.off));
  const start = Math.max(0, off2 - SNIP_BEFORE);
  const end = Math.min(hit.size, off2 + SNIP_AFTER) - 1;
  const r = await fetch(fileURL(hit.path), { headers: { Range: `bytes=${start}-${Math.max(end, start)}` }, signal });
  if (!r.ok) throw new Error(`${r.status}`);
  let text2 = new TextDecoder("utf-8").decode(await r.arrayBuffer()).replace(/�/g, "");
  if (start > 0) text2 = "\u2026" + text2.replace(/^[\p{L}\p{N}]*\s*/u, "");
  if (end < hit.size - 1) text2 = text2.replace(/\s*[\p{L}\p{N}]*$/u, "") + "\u2026";
  return marked(text2.replace(/\s+/g, " "), hit.terms.map((t) => t.term));
}
function hitRow(hit, want) {
  const href = hit.type === "dir" ? browseHash({ path: hit.path }) : fileHash({ path: hit.path });
  let where = dirname(hit.path);
  if (hit.match === "tags" && hit.title) where += " \xB7 " + [hit.title, hit.artist, hit.album].filter(Boolean).join(" \u2014 ");
  const li = h("li", { class: "hit" }, h("a", { class: "hname", href }, basename(hit.path) || "/"), h("span", { class: "hpath" }, where), h("span", { class: "hwhy badge" }, hit.match));
  if (hit.match === "content" && hit.terms.length > 0 && hit.size > 0) {
    const snip = h("span", { class: "hsnip loading" }, "loading the snippet\u2026");
    li.append(snip);
    want(snip, hit);
  }
  return li;
}
function render11(root, q, params) {
  const under = new URLSearchParams(params).get("under") ?? "";
  const input = document.querySelector("form.search input");
  if (input && document.activeElement !== input) input.value = q;
  const abort = new AbortController();
  const section2 = h("section", { class: "results" });
  root.append(section2);
  const scope = under || folder2;
  const sub = h("span", { class: "h-sub" }, "searching\u2026");
  const bar2 = h("div", { class: "toolbar" }, h("h2", null, "Search", sub), h("span", { class: "spacer" }));
  if (scope !== "/") {
    const toggle = h("input", { type: "checkbox", checked: under !== "" });
    toggle.addEventListener("change", () => location.replace(searchHash(q, toggle.checked ? scope : "")));
    bar2.append(h("label", { class: "toggle" }, toggle, " in ", h("code", null, scope), " only"));
  }
  section2.append(bar2);
  const queue2 = [];
  let running2 = 0;
  const pump = () => {
    while (running2 < SNIP_AT_ONCE && queue2.length > 0) {
      const [el, hit] = queue2.shift();
      running2++;
      snippet(hit, abort.signal).then(
        (parts) => {
          clear(el);
          el.classList.remove("loading");
          el.append(...parts);
        },
        () => el.remove()
      ).finally(() => {
        running2--;
        pump();
      });
    }
  };
  const pending3 = /* @__PURE__ */ new Map();
  const seen = new IntersectionObserver((entries) => {
    for (const e of entries) {
      const hit = pending3.get(e.target);
      if (!e.isIntersecting || !hit) continue;
      pending3.delete(e.target);
      seen.unobserve(e.target);
      queue2.push([e.target, hit]);
    }
    pump();
  });
  const want = (el, hit) => {
    pending3.set(el, hit);
    seen.observe(el);
  };
  search(q, under).then(
    (res) => {
      if (abort.signal.aborted) return;
      clear(sub);
      sub.append(`${res.hits.length} ${res.hits.length === 1 ? "hit" : "hits"} for `, h("code", null, q));
      if (res.index) section2.append(h("div", { class: "note" }, "index unavailable on this node: names only until the leader's next pass"));
      if (res.hits.length === 0) {
        const empty = h("div", { class: "empty" }, "no matches for ", h("code", null, q));
        if (under) empty.append(" under ", h("code", null, under));
        section2.append(empty);
        return;
      }
      const ol = h("ol");
      for (const hit of res.hits) ol.append(hitRow(hit, want));
      section2.append(ol);
    },
    (e) => {
      if (!abort.signal.aborted) sub.textContent = "search failed: " + (e instanceof Error ? e.message : String(e));
    }
  );
  return () => {
    abort.abort();
    seen.disconnect();
  };
}

// internal/webui/src/media.ts
var NOUN = { movies: ["film", "films"], tv: ["show", "shows"], music: ["album", "albums"], roms: ["platform", "platforms"] };
var WARN_FLAGS = /* @__PURE__ */ new Set(["proto", "hack", "beta", "demo", "bad", "unl", "pirate", "dlc", "update"]);
function chips(it, more = false) {
  const el = h("span", { class: "chips" });
  for (const r of it.regions ?? []) el.append(h("span", { class: "badge" }, r));
  if (more) for (const l of it.languages ?? []) el.append(h("span", { class: "badge" }, l));
  if (it.revision) el.append(h("span", { class: "badge" }, it.revision));
  for (const f of it.flags ?? []) if (f !== "base") el.append(h("span", { class: "badge" + (WARN_FLAGS.has(f) ? " warn" : "") }, f));
  if (more && it.discs && it.discs > 1) el.append(h("span", { class: "badge" }, `${it.discs} discs`));
  if (it.variants) el.append(h("span", { class: "badge" }, `+${it.variants}`));
  return el;
}
function cardSub(it) {
  if (it.kind === "platform") return basename(it.path) !== it.title ? basename(it.path) : void 0;
  if (it.kind === "game") return it.platform;
  return it.year ? String(it.year) : void 0;
}
function glyph(kind2) {
  return icon(kind2 === "album" ? "album" : "movie");
}
function duration2(min) {
  if (!min) return "";
  return min >= 60 ? `${Math.floor(min / 60)} h ${String(min % 60).padStart(2, "0")}` : `${min} min`;
}
function itemHash(it) {
  return it.kind === "album" ? browseHash({ path: it.path }) : "#/media/item" + encodePathForHash(it.path);
}
function art(it) {
  if (it.poster) return thumbURL(it.poster);
  if (it.kind === "album") return thumbURL(it.path);
  if (it.video) return thumbURL(it.video);
  return void 0;
}
function frameOf(img, it, onMiss) {
  if (it.poster || it.thumb || !it.video || it.video_ino === void 0) return false;
  lazy(img, { ino: it.video_ino, version: it.video_version ?? 0, thumb: it.video_thumb }, it.video, void 0, "video", onMiss);
  return true;
}
function meta(it) {
  if (it.kind === "album") return [it.artist ?? "", it.year ? String(it.year) : ""];
  return [it.year ? String(it.year) : "", it.kind === "movie" && !it.runtime && !it.plot ? "no sidecar" : ""];
}
function fact(it) {
  if (it.kind === "movie" && it.runtime) return duration2(it.runtime);
  if (it.kind === "show" && it.children) return `${it.children} ${it.children === 1 ? "season" : "seasons"}`;
  if (it.kind === "platform") return `${it.children ?? 0} ${it.children === 1 ? "game" : "games"}`;
  return "";
}
var metaSpans = (it) => meta(it).map((t) => h("span", null, t));
function posterTile(it) {
  const veiled = it.nsfw && !session.get("tr.revealed", false);
  const box = h("span", { class: "art" });
  const chip = fact(it) ? h("span", { class: "ov br" }, fact(it)) : null;
  const below = it.kind === "game" && !veiled ? chips(it) : h("span", { class: "pyear" }, ...metaSpans(it));
  const a = h("a", { class: "poster" + (veiled ? " nsfw" : ""), href: itemHash(it) }, box, h("span", { class: "ptitle", title: it.title }, it.title), below);
  const missing = () => {
    clear(box);
    box.append(it.kind === "album" ? glyph(it.kind) : titleCard(it.title, cardSub(it)));
    if (chip) box.append(chip);
    if (it.kind !== "platform") a.classList.add("missing");
  };
  const src2 = art(it);
  const frame2 = h("img", { alt: "" });
  if (frameOf(frame2, it, missing)) box.append(frame2);
  else if (src2) {
    const img = h("img", { alt: "", loading: "lazy", src: src2 });
    img.addEventListener("error", missing);
    box.append(img);
  } else missing();
  if (chip && !box.contains(chip)) box.append(chip);
  if (veiled) {
    a.addEventListener("click", (ev) => {
      if (!a.classList.contains("nsfw")) return;
      ev.preventDefault();
      session.set("tr.revealed", true);
      a.classList.remove("nsfw");
    });
  }
  return a;
}
function stored2(key) {
  try {
    return localStorage.getItem(key) === "1";
  } catch {
    return false;
  }
}
function remember(key, on) {
  try {
    if (on) localStorage.setItem(key, "1");
    else localStorage.removeItem(key);
  } catch {
  }
}
function librarySection(lib) {
  const [one, many] = NOUN[lib.kind] ?? ["item", "items"];
  const countEl = h("span", { class: "count" }, "\u2026");
  const key = `tr.media.closed.${lib.path}`;
  const fold2 = h("details", null, h("summary", null, h("h2", null, basename(lib.path) || "/", " ", h("span", { class: "badge lib" }, lib.kind), countEl)));
  fold2.open = !stored2(key);
  const section2 = h("section", { class: "lib " + lib.kind }, fold2);
  const grid = h("div", { class: "posters" });
  const sorts = ["title", "year", "added"];
  let sort = session.get(`tr.media.sort.${lib.path}`, "title");
  let items = [];
  const buttons = sorts.map((s) => h("button", { type: "button", "aria-pressed": String(s === sort) }, s[0].toUpperCase() + s.slice(1)));
  const draw = () => {
    clear(grid);
    for (const it of items) grid.append(posterTile(it));
  };
  const load2 = () => {
    media(lib.path, sort).then(
      (res) => {
        items = res.items;
        clear(countEl);
        countEl.append(`${count(res.lib.count)} ${res.lib.count === 1 ? one : many}${res.lib.count > res.items.length ? ` (first ${res.items.length})` : ""} \xB7 `, h("a", { href: browseHash({ path: lib.path }) }, "open folder"));
        draw();
      },
      (e) => {
        countEl.textContent = "could not list: " + (e instanceof Error ? e.message : String(e));
      }
    );
  };
  buttons.forEach(
    (b, i) => b.addEventListener("click", () => {
      sort = sorts[i];
      session.set(`tr.media.sort.${lib.path}`, sort);
      buttons.forEach((o, j) => o.setAttribute("aria-pressed", String(i === j)));
      load2();
    })
  );
  fold2.append(h("div", { class: "toolbar" }, h("span", { class: "seg", role: "group", "aria-label": "sort" }, ...buttons)), grid);
  let loaded = false;
  const open = () => {
    if (fold2.open && !loaded) {
      loaded = true;
      load2();
    }
  };
  fold2.addEventListener("toggle", () => {
    remember(key, !fold2.open);
    open();
  });
  open();
  return section2;
}
function overview(root) {
  media().then(
    (res) => {
      if (res.libs.length === 0) {
        root.append(
          h(
            "section",
            null,
            h("h2", null, "Media"),
            h("div", { class: "empty" }, "No library yet: open a folder in Files, choose its menu and set ", h("b", null, "Library"), " to movies, tv, music or roms.")
          )
        );
        return;
      }
      for (const lib of res.libs) root.append(librarySection(lib));
    },
    (e) => root.append(h("div", { class: "note" }, "could not list the libraries: " + (e instanceof Error ? e.message : String(e))))
  );
}
function episodeNo(e) {
  const two = (n) => String(n).padStart(2, "0");
  if (e.part) return `Part ${e.part}`;
  if (!e.episode && e.date) return e.date;
  if (!e.episode && e.absolute) return "#" + String(e.absolute).padStart(3, "0");
  return `S${two(e.season ?? 0)}E${two(e.episode ?? 0)}` + (e.episode_to && e.episode_to !== e.episode ? `E${two(e.episode_to)}` : "");
}
function platformPage(root, it) {
  const grid = h("div", { class: "posters games" });
  for (const g of it.games ?? []) grid.append(posterTile(g));
  const n = it.games?.length ?? 0;
  root.append(
    h(
      "section",
      { class: "lib roms" },
      h("h2", null, it.title, " ", h("span", { class: "badge lib" }, "roms"), h("span", { class: "count" }, `${count(n)} ${n === 1 ? "game" : "games"} \xB7 `, h("a", { href: browseHash({ path: it.path }) }, "open folder"))),
      grid
    )
  );
}
function gamePage(root, it) {
  const section2 = h("section", { class: it.poster ? "hero poster-only" : "hero no-art" });
  let poster = titleCard(it.title, it.platform);
  poster.classList.add("hero-poster");
  if (it.poster) {
    section2.append(h("img", { class: "backdrop", alt: "", src: fileURL(it.poster) }));
    const img = h("img", { class: "hero-poster", alt: "", src: fileURL(it.poster) });
    const card3 = poster;
    img.addEventListener("error", () => img.replaceWith(card3));
    poster = img;
  }
  const facts = h("div", { class: "facts" });
  for (const f of [it.platform ?? "", it.year ? String(it.year) : "", it.developer ?? "", it.publisher ?? "", it.players ? `${it.players} ${it.players === "1" ? "player" : "players"}` : "", (it.genres ?? []).join(" \xB7 ").toLowerCase()])
    if (f) facts.append(h("span", null, f));
  facts.append(h("span", { class: "dim" }, dirname(it.path)));
  const body = h("div", null, h("h1", null, it.title), facts, chips(it, true));
  if (it.plot) body.append(h("p", { class: "plot" }, it.plot));
  const files = it.files ?? [];
  const isFolder = files.some((f) => f.path.startsWith(it.path + "/"));
  const folder3 = isFolder ? it.path : dirname(it.path);
  body.append(h("div", { class: "actions" }, h("a", { class: "btn", href: browseHash({ path: folder3 }) }, "Open folder")));
  if (files.length > 0) {
    const tbody = h("tbody");
    for (const f of files) tbody.append(fileRow(f, files.length));
    body.append(h("table", { class: "episodes discs" }, tbody));
  }
  section2.append(h("div", { class: "hero-body" }, poster, body));
  root.append(section2);
}
function fileRow(f, n) {
  const label = f.disc ? `Disc ${f.disc}` : n > 1 ? [...f.regions ?? [], f.revision ?? ""].filter(Boolean).join(" ") || "\u2014" : "File";
  return h("tr", null, h("td", { class: "no" }, label), h("td", { class: "title" }, h("a", { href: fileURL(f.path, true) }, f.name)), h("td", { class: "dur" }, bytes(f.size)));
}
function detail(root, path) {
  mediaItem(path).then(
    (it) => {
      if (it.kind === "platform") return platformPage(root, it);
      if (it.kind === "game") return gamePage(root, it);
      const backdrop = it.fanart ? fileURL(it.fanart) : it.poster ? fileURL(it.poster) : "";
      const cls = "hero" + (it.fanart ? "" : it.poster ? " poster-only" : " no-art");
      const section2 = h("section", { class: cls });
      if (backdrop) section2.append(h("img", { class: "backdrop", alt: "", src: backdrop }));
      const posterSrc = it.poster ? fileURL(it.poster) : it.video ? thumbURL(it.video) : it.kind === "album" ? thumbURL(it.path) : "";
      const poster = h("img", { class: "hero-poster", alt: "" });
      const hide = () => {
        if (it.kind === "album") poster.style.visibility = "hidden";
        else {
          const card3 = titleCard(it.title, it.year ? String(it.year) : void 0);
          card3.classList.add("hero-poster");
          poster.replaceWith(card3);
        }
      };
      if (!frameOf(poster, it, hide)) {
        if (posterSrc) poster.src = posterSrc;
        poster.addEventListener("error", hide);
      }
      const noArt = !posterSrc && !it.video;
      const facts = h("div", { class: "facts" });
      const seasons = it.seasons ?? [];
      const episodes = seasons.reduce((n, s) => n + s.episodes.length, 0);
      for (const f of [
        it.year ? String(it.year) : "",
        it.kind === "show" ? `${seasons.length} ${seasons.length === 1 ? "season" : "seasons"} \xB7 ${episodes} ${episodes === 1 ? "episode" : "episodes"}` : duration2(it.runtime),
        it.rating ? `\u2605 ${it.rating}` : "",
        (it.genres ?? []).join(" \xB7 ").toLowerCase(),
        it.mpaa ?? ""
      ])
        if (f) facts.append(h("span", null, f));
      facts.append(h("span", { class: "dim" }, it.path));
      const body = h("div", null, h("h1", null, it.title), facts);
      if (it.plot) body.append(h("p", { class: "plot" }, it.plot));
      else {
        const sidecar = it.kind === "show" ? "tvshow.nfo" : it.path.includes(".") && it.video === it.path ? basename(it.path).replace(/\.[^.]+$/, "") + ".nfo" : "movie.nfo";
        body.append(h("p", { class: "plot dim" }, "No description: add a ", h("code", null, sidecar), it.kind === "show" ? " to the show's folder." : " beside the file."));
      }
      const first = seasons.find((s) => s.n > 0)?.episodes[0] ?? seasons[0]?.episodes[0];
      const play = h("button", { type: "button" }, icon("play_arrow_fill"), it.kind === "show" && first ? `Play ${episodeNo(first)}` : "Play");
      const target = it.kind === "show" ? first?.path : it.video;
      if (target) play.addEventListener("click", () => location.hash = fileHash({ path: target }));
      else play.disabled = true;
      const folder3 = it.video === it.path ? dirname(it.path) : it.path;
      body.append(h("div", { class: "actions" }, play, h("a", { class: "btn", href: browseHash({ path: folder3 }) }, "Open folder")));
      if (it.people?.length) {
        const people = h("div", { class: "people" });
        it.people.forEach((p, i) => people.append(h("span", { class: "person" }, i === 0 && it.kind === "movie" && it.people.length > 1 ? `${p} \xB7 director` : p)));
        body.append(people);
      }
      if (seasons.length > 0) {
        const strip = h("div", { class: "seasons" });
        const table = h("table", { class: "episodes" });
        const tbody = h("tbody");
        table.append(tbody);
        const show = (i) => {
          strip.querySelectorAll("button").forEach((b, j) => b.setAttribute("aria-pressed", String(i === j)));
          clear(tbody);
          for (const e of seasons[i].episodes) {
            const thumb = h("td", { class: "thumb" });
            const img = h("img", { alt: "" });
            const miss = () => img.replaceWith(glyph("episode"));
            if (!frameOf(img, e, miss)) {
              img.loading = "lazy";
              img.src = thumbURL(e.thumb || e.path);
              img.addEventListener("error", miss);
            }
            thumb.append(img);
            const title = h("td", { class: "title" }, e.title);
            if (e.plot) title.append(h("div", { class: "plot small dim" }, e.plot));
            const row3 = h("tr", null, h("td", { class: "no" }, episodeNo(e)), thumb, title, h("td", { class: "dur" }, duration2(e.runtime)));
            row3.addEventListener("click", () => location.hash = fileHash({ path: e.path }));
            tbody.append(row3);
          }
        };
        seasons.forEach((s, i) => {
          const b = h("button", { type: "button", "aria-pressed": "false" }, s.title);
          b.addEventListener("click", () => show(i));
          strip.append(b);
        });
        body.append(strip, table);
        show(Math.max(0, seasons.findIndex((s) => s.n > 0)));
      }
      section2.append(h("div", { class: "hero-body" }, poster, body));
      if (noArt) hide();
      root.append(section2);
    },
    (e) => root.append(h("div", { class: "note" }, "could not open: " + (e instanceof Error ? e.message : String(e))))
  );
}
function render12(root, rest) {
  if (rest.startsWith("/item/")) detail(root, rest.slice("/item".length));
  else overview(root);
  return () => void 0;
}

// internal/webui/src/main.ts
var view2 = document.getElementById("view");
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
  const ctx = { path: decodeHashPath(rest.join("/")) };
  const at = Number(new URLSearchParams(query).get("at") ?? "");
  if ((head === "browse" || head === "file") && Number.isInteger(at) && at > 0) ctx.at = at;
  return { head, ctx, query };
}
function rawRest(hash2) {
  const where = hash2.replace(/^#\/?/, "").split("?")[0] ?? "";
  const i = where.indexOf("/");
  return i < 0 ? "" : where.slice(i + 1);
}
function route() {
  const { head, ctx, query } = parse2(location.hash || "#/");
  dispose?.();
  dispose = null;
  clear(view2);
  let tab = "dash";
  if (head === "browse" || head === "trash" && ctx.root !== void 0) {
    tab = "files";
    if (head === "browse") setFolder(ctx.path);
    dispose = render9(view2, ctx);
  } else if (head === "media") {
    tab = "media";
    dispose = render12(view2, decodeHashPath(rawRest(location.hash)));
  } else if (head === "search") {
    tab = "";
    dispose = render11(view2, decodeHashPath(rawRest(location.hash)).slice(1), query);
  } else if (head === "file" || head === "trashfile" && ctx.root !== void 0) {
    tab = "files";
    dispose = render7(view2, ctx);
  } else if (head === "stats") {
    tab = "stats";
    dispose = render10(view2, query);
  } else {
    dispose = render5(view2);
  }
  for (const a of tabs) a.classList.toggle("active", a.dataset.tab === tab);
  window.scrollTo(0, 0);
}
player.mount();
var searchForm = document.querySelector("form.search");
if (searchForm) wire3(searchForm);
var themeButton = document.querySelector("button.theme");
if (themeButton) wire(themeButton);
var header = document.querySelector("header.top");
if (header) void mount(header);
wire2(document);
status.subscribe(drawShell);
window.addEventListener("hashchange", route);
route();
export {
  healthPill,
  parse2 as parse
};
