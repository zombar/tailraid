// File browser: breadcrumb, toolbar, the upload queue, the inline new-folder form and the
// listing. Uploads run one at a time through the NFS write path (docs/13 §4).

import { clear, h, icon, setWidth, toast, type IconKind } from "./dom.ts";
import { ApiError, ls, mkdir, upload, type Entry, type Upload } from "./api.ts";
import { bytes, encodePathForHash, ext, join, when } from "./util.ts";
import { mediaKind } from "./player.ts";

type SortKey = "name" | "size" | "mtime";

interface Sort {
  key: SortKey;
  desc: boolean;
}

function loadSort(): Sort {
  try {
    const s = sessionStorage.getItem("tr.sort");
    if (s) return JSON.parse(s) as Sort;
  } catch {
    // storage may be unavailable
  }
  return { key: "name", desc: false };
}

function saveSort(s: Sort): void {
  try {
    sessionStorage.setItem("tr.sort", JSON.stringify(s));
  } catch {
    // ignore
  }
}

// --- the upload queue (module state: it survives navigation between folders) ---------

interface Job {
  dir: string;
  file: File;
  sent: number;
  state: "queued" | "uploading" | "done" | "failed" | "cancelled";
  message?: string;
  handle?: Upload;
}

const queue: Job[] = [];
let running = false;
let onQueueChange: (() => void) | null = null;
let onJobDone: ((j: Job) => void) | null = null;

function enqueue(dir: string, files: FileList | File[]): void {
  for (const f of Array.from(files)) queue.push({ dir, file: f, sent: 0, state: "queued" });
  onQueueChange?.();
  void runQueue();
}

async function runQueue(): Promise<void> {
  if (running) return;
  running = true;
  for (;;) {
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
      },
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

// --- the view --------------------------------------------------------------------------

export function render(root: HTMLElement, dir: string): () => void {
  let sort = loadSort();
  let entries: Entry[] = [];
  let formOpen = false;

  const refresh = async () => {
    try {
      const r = await ls(dir);
      entries = r.entries;
      draw();
    } catch (e) {
      clear(root);
      const msg = e instanceof Error ? e.message : String(e);
      root.append(h("section", null, crumbs(dir), h("div", { class: "error" }, `Could not list ${dir}: ${msg}. `, h("a", { href: "#", onClick: (ev: Event) => { ev.preventDefault(); void refresh(); } }, "Retry"))));
    }
  };

  const draw = () => {
    clear(root);
    const section = h("section", null, crumbs(dir));
    // toolbar
    const input = h("input", { type: "file", multiple: true, "aria-label": "upload files" }) as HTMLInputElement;
    input.addEventListener("change", () => {
      if (input.files?.length) enqueue(dir, input.files);
      input.value = "";
    });
    const total = entries.reduce((s, e) => s + (e.type === "file" ? e.size : 0), 0);
    const toolbar = h(
      "div",
      { class: "toolbar" },
      h("label", { class: "btn" }, "Upload", input),
      h("button", { type: "button", "aria-pressed": formOpen ? "true" : "false", onClick: () => { formOpen = !formOpen; draw(); } }, "New folder"),
      h("span", { class: "spacer" }),
      h("span", { class: "small dim" }, `${entries.length} ${entries.length === 1 ? "item" : "items"} · ${bytes(total)}`),
    );
    section.append(toolbar);
    // upload queue: finished jobs leave the list, failures stay until the next navigation
    const shown = queue.filter((j) => j.state !== "done");
    if (shown.length) {
      const list = h("div", { class: "uploads" });
      for (const j of shown) list.append(jobRow(j));
      section.append(list);
    }
    // new folder
    if (formOpen) section.append(folderForm());
    // listing
    if (entries.length === 0) section.append(h("div", { class: "empty" }, "empty folder"));
    else section.append(table());
    root.append(section);
  };

  const folderForm = () => {
    const name = h("input", { type: "text", placeholder: "folder name", "aria-label": "folder name", autofocus: true }) as HTMLInputElement;
    const form = h("form", { class: "inline-form" }, icon("dir"), name, h("button", { type: "submit" }, "Create"), h("button", { type: "button", onClick: () => { formOpen = false; draw(); } }, "Cancel"));
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
        (e: unknown) => toast("mkdir " + n + ": " + (e instanceof ApiError ? e.message : String(e)), "bad"),
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
    const th = (key: SortKey, label: string, cls?: string) => {
      const attrs: Record<string, string> = { class: cls ?? "" };
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
    for (const e of sorted) body.append(row(e));
    t.append(body);
    return t;
  };

  const row = (e: Entry) => {
    const path = join(dir, e.name);
    let link: HTMLElement;
    if (e.type === "dir") link = h("a", { href: "#/browse" + encodePathForHash(path) }, icon("dir"), e.name);
    else if (e.type === "symlink") link = h("span", { class: "linkname" }, icon("link"), e.name + " ", h("span", { class: "target" }, "→ " + (e.target ?? "")));
    else if (e.upload) link = h("span", { class: "dim", title: "an upload in progress, or one that was not resumed" }, icon("file"), e.name);
    else link = h("a", { href: "#/file" + encodePathForHash(path) }, icon(iconFor(e.name)), e.name);
    return h(
      "tr",
      { class: e.upload ? "dim" : e.dirty ? "dirty" : "" },
      h("td", { class: "name" }, link),
      h("td", { class: "size" }, e.type === "file" ? bytes(e.size) : ""),
      h("td", { class: "mtime", title: e.mtime }, when(e.mtime)),
    );
  };

  const jobRow = (j: Job) => {
    const state = h("span", { class: "state" + (j.state === "failed" ? " bad" : j.state === "done" ? " ok" : "") });
    if (j.state === "uploading") state.append(`${bytes(j.sent)} / ${bytes(j.file.size)} `, j.note ? j.note + " " : "", h("button", { type: "button", onClick: () => j.handle?.abort() }, "cancel"));
    else if (j.state === "queued") state.append("queued ", h("button", { type: "button", onClick: () => { j.state = "cancelled"; onQueueChange?.(); } }, "cancel"));
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

  // drag and drop onto the view
  const onDragOver = (ev: DragEvent) => {
    ev.preventDefault();
    root.querySelector("table.files")?.classList.add("drop");
  };
  const onDragLeave = () => root.querySelector("table.files")?.classList.remove("drop");
  const onDrop = (ev: DragEvent) => {
    ev.preventDefault();
    onDragLeave();
    const files = Array.from(ev.dataTransfer?.files ?? []);
    const plain = files.filter((f) => f.type !== "" || f.size > 0); // folders arrive as empty typeless entries
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

export function crumbs(path: string): HTMLElement {
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

/** crumbsWithLast links every segment, for the player's breadcrumb. */
export function crumbsAll(path: string): HTMLElement {
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

function iconFor(name: string): IconKind {
  const k = mediaKind(ext(name));
  if (k === "image") return "image";
  if (k === "video" || k === "audio") return "media";
  return "file";
}

function compare(a: Entry, b: Entry, s: Sort): number {
  const da = a.type === "dir" ? 0 : 1;
  const db = b.type === "dir" ? 0 : 1;
  if (da !== db) return da - db; // folders first
  let c = 0;
  if (s.key === "size") c = a.size - b.size;
  else if (s.key === "mtime") c = a.mtime.localeCompare(b.mtime);
  if (c === 0) c = a.name.localeCompare(b.name, undefined, { numeric: true, sensitivity: "base" });
  return s.desc ? -c : c;
}
