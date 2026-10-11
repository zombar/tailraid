// File browser (docs/13 §4, §5): breadcrumb with the folder's flags, toolbar (upload,
// new folder, the folder menu, the list/tiles switch), the upload queue, the inline
// rename form, the listing as rows or tiles with selection, the folder's deleted roots
// in place, the trash banner inside a deleted root, the album view for a folder of
// music, and the selection bar. Uploads run one at a time through the NFS write path.

import { clear, h, icon, setWidth, toast, type IconKind } from "./dom.ts";
import { ApiError, del, ls, mkdir, move, rename, trashList, upload, type DirInfo, type Entry, type TrashEntry, type Upload } from "./api.ts";
import { browseHash, bytes, encodePathForHash, ext, fileHash, join, session, when, type Ctx } from "./util.ts";
import { mediaKind } from "./player.ts";
import { Selection } from "./select.ts";
import { lazy } from "./thumbs.ts";
import { pick } from "./picker.ts";
import { badge as deletedBadge, banner } from "./trash.ts";
import { menu } from "./flags.ts";
import { isAudioName, player, type Track } from "./audio.ts";
import * as album from "./album.ts";

type SortKey = "name" | "size" | "mtime";
type View = "list" | "tiles";

interface Sort {
  key: SortKey;
  desc: boolean;
}

// --- the upload queue (module state: it survives navigation between folders) ---------

interface Job {
  dir: string;
  file: File;
  sent: number;
  state: "queued" | "uploading" | "done" | "failed" | "cancelled";
  message?: string;
  note?: string; // upload's onNote: "resuming 2/5" while it retries, "" once it is back
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

export function render(root: HTMLElement, ctx: Ctx): () => void {
  const dir = ctx.path;
  const inTrash = ctx.root !== undefined;
  let sort = session.get<Sort>("tr.sort", { key: "name", desc: false });
  let view = session.get<View>("tr.view", "list");
  let entries: Entry[] = [];
  let info: DirInfo = { protected: false, nsfw: false, pinned: false };
  let trashRoot: TrashEntry | undefined; // the deleted root we are inside
  let formOpen = false;
  let renaming: string | null = null;
  const sel = new Selection();
  let alb: album.Album | null = null;
  const live = () => entries.filter((e) => !e.deleted_at);
  const pathOf = (e: Entry) => join(dir, e.name);

  const refresh = async () => {
    try {
      const r = await ls(dir, ctx.root);
      entries = r.entries;
      info = r.dir;
      if (inTrash && !trashRoot) {
        try {
          trashRoot = (await trashList()).entries.find((t) => t.ino === ctx.root);
        } catch {
          // the banner falls back to what the listing says
        }
      }
      sel.keep(new Set(entries.filter((e) => !e.deleted_at).map((e) => e.name)));
      draw();
    } catch (e) {
      clear(root);
      const msg = e instanceof Error ? e.message : String(e);
      root.append(h("section", null, crumbs(ctx), h("div", { class: "error" }, `Could not list ${dir}: ${msg}. `, h("a", { href: "#", onClick: (ev: Event) => { ev.preventDefault(); void refresh(); } }, "Retry"))));
    }
  };

  const draw = () => {
    clear(root);
    const section = h("section", null, crumbs(ctx, info, inTrash));
    if (inTrash) {
      const files = entries.filter((e) => e.type === "file");
      const rootEntry: TrashEntry = trashRoot ?? { ino: ctx.root!, name: "", type: "dir", size: 0, path: "", deleted_at: info.deleted_at ?? "", restorable: true };
      if (info.purge_at && !trashRoot) rootEntry.purge_at = info.purge_at;
      section.append(banner({ root: { ...rootEntry, purge_at: rootEntry.purge_at ?? info.purge_at }, items: entries.length, bytes: files.reduce((s, e) => s + e.size, 0) }));
    }
    // toolbar
    const input = h("input", { type: "file", multiple: true, "aria-label": "upload files" }) as HTMLInputElement;
    input.addEventListener("change", () => {
      if (input.files?.length) enqueue(dir, input.files);
      input.value = "";
    });
    const total = live().reduce((s, e) => s + (e.type === "file" ? e.size : 0), 0);
    const deleted = entries.length - live().length;
    const toolbar = h("div", { class: "toolbar" });
    const asAlbum = view === "list" && !album.isAlbum(entries) ? false : view === "list";
    if (asAlbum) {
      toolbar.append(
        h("button", { type: "button", onClick: () => alb && player.play(alb.tracks, 0) }, "Play all"),
        h("button", { type: "button", onClick: () => { if (alb) { if (!player.shuffle) player.toggleShuffle(); player.play(alb.tracks, 0); } } }, "Shuffle"),
      );
    }
    if (!inTrash) {
      toolbar.append(
        h("label", { class: "btn" }, "Upload", input),
        h("button", { type: "button", "aria-pressed": formOpen ? "true" : "false", onClick: () => { formOpen = !formOpen; renaming = null; draw(); } }, "New folder"),
      );
    }
    toolbar.append(menu({ path: dir, inTrash, dir: info, onChange: () => void refresh() }));
    const seg = h(
      "span",
      { class: "seg", role: "group", "aria-label": "view" },
      h("button", { type: "button", "aria-pressed": view === "list" ? "true" : "false", onClick: () => setView("list") }, "List"),
      h("button", { type: "button", "aria-pressed": view === "tiles" ? "true" : "false", onClick: () => setView("tiles") }, "Tiles"),
    );
    toolbar.append(seg, h("span", { class: "spacer" }), h("span", { class: "small dim" }, `${live().length} ${live().length === 1 ? "item" : "items"} · ${bytes(total)}` + (deleted ? ` · ${deleted} deleted` : "")));
    section.append(toolbar);
    // upload queue: finished jobs leave the list, failures stay until the next navigation
    const shown = queue.filter((j) => j.state !== "done");
    if (shown.length) {
      const list = h("div", { class: "uploads" });
      for (const j of shown) list.append(jobRow(j));
      section.append(list);
    }
    if (formOpen) section.append(folderForm());
    if (renaming) section.append(renameForm(renaming));
    // listing
    alb = null;
    if (entries.length === 0) section.append(h("div", { class: "empty" }, "empty folder"));
    else if (asAlbum) {
      alb = album.render(ctx, info.cover, entries);
      section.append(alb.el);
      const rest = entries.filter((e) => !(e.type === "file" && isAudioName(e.name) && !e.deleted_at));
      if (rest.length) section.append(table(rest));
    } else if (view === "tiles") section.append(tiles());
    else section.append(table(entries));
    section.append(selbar());
    root.append(section);
  };

  const setView = (v: View) => {
    view = v;
    session.set("tr.view", v);
    draw();
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

  const renameForm = (old: string) => {
    const name = h("input", { type: "text", value: old, "aria-label": "new name" }) as HTMLInputElement;
    const form = h("form", { class: "inline-form rename" }, icon(iconFor(old, entries.find((e) => e.name === old)?.type === "dir")), name, h("button", { type: "submit" }, "Rename"), h("button", { type: "button", onClick: () => { renaming = null; draw(); } }, "Cancel"));
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
        (e: unknown) => toast("rename: " + (e instanceof Error ? e.message : String(e)), "bad"),
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

  const sorted = (list: Entry[]) => {
    const livePart = list.filter((e) => !e.deleted_at).sort((a, b) => compare(a, b, sort));
    const gone = list.filter((e) => e.deleted_at).sort((a, b) => (b.deleted_at ?? "").localeCompare(a.deleted_at ?? "") || a.name.localeCompare(b.name));
    return [...livePart, ...gone];
  };
  const order = () => sorted(entries).filter((e) => !e.deleted_at).map((e) => e.name);
  const onSelect = (e: Entry, ev: Event) => {
    ev.stopPropagation();
    sel.click(e.name, (ev as MouseEvent).shiftKey, order());
  };
  const selectBox = (e: Entry, cls: "sel") =>
    e.deleted_at
      ? h("span", { class: cls })
      : (() => {
          const box = h("input", { type: "checkbox", "aria-label": "select" }) as HTMLInputElement;
          box.checked = sel.has(e.name);
          box.addEventListener("click", (ev) => onSelect(e, ev));
          return h("span", { class: cls }, box);
        })();

  const linkOf = (e: Entry): { href: string; kind: IconKind } => {
    const path = pathOf(e);
    if (e.deleted_at) return e.type === "dir" ? { href: `#/trash/${e.ino}/`, kind: "dir" } : { href: `#/trashfile/${e.ino}/`, kind: iconFor(e.name) };
    if (e.type === "dir") return { href: browseHash({ path, root: ctx.root }), kind: "dir" };
    return { href: fileHash({ path, root: ctx.root }), kind: iconFor(e.name) };
  };

  const playBtn = (e: Entry) => {
    const t: Track = { path: pathOf(e), root: e.deleted_at ? e.ino : ctx.root };
    if (e.deleted_at) t.path = "/";
    const now = player.isCurrent(t.path, t.root) && player.playing;
    return h("button", { type: "button", class: "play", onClick: (ev: Event) => { ev.preventDefault(); ev.stopPropagation(); if (player.isCurrent(t.path, t.root)) player.toggle(); else playFrom(e); } }, now ? "⏸ Playing" : "▶ Play");
  };

  /** playFrom plays the folder's audio in listing order, starting at e. */
  const playFrom = (e: Entry) => {
    const tracks = sorted(entries).filter((x) => !x.deleted_at && x.type === "file" && isAudioName(x.name)).map((x): Track => ({ path: pathOf(x), root: ctx.root }));
    if (e.deleted_at) {
      player.play([{ path: "/", root: e.ino }]);
      return;
    }
    const i = tracks.findIndex((t) => t.path === pathOf(e));
    player.play(tracks, Math.max(0, i));
  };

  const badges = (e: Entry) => {
    const b = h("span", { class: "badges" });
    if (e.protected) b.append(h("span", { class: "badge lock" }, "protected"));
    if (e.nsfw) b.append(h("span", { class: "badge nsfw" }, "nsfw"));
    if (e.deleted_at) b.append(deletedBadge(e.purge_at));
    return b;
  };

  const table = (list: Entry[]) => {
    const th = (key: SortKey, label: string, cls?: string) => {
      const attrs: Record<string, string> = { class: cls ?? "" };
      if (sort.key === key) attrs["aria-sort"] = sort.desc ? "descending" : "ascending";
      const el = h("th", attrs, label);
      el.addEventListener("click", () => {
        sort = sort.key === key ? { key, desc: !sort.desc } : { key, desc: key !== "name" };
        session.set("tr.sort", sort);
        draw();
      });
      return el;
    };
    const all = h("input", { type: "checkbox", "aria-label": "select all" }) as HTMLInputElement;
    const names = live().map((e) => e.name);
    all.checked = names.length > 0 && names.every((n) => sel.has(n));
    all.addEventListener("click", () => (all.checked ? sel.all(names) : sel.clear()));
    const t = h("table", { class: "files" }, h("thead", null, h("tr", null, h("th", { class: "sel" }, all), th("name", "name"), th("size", "size", "size"), th("mtime", "modified", "mtime"))));
    const body = h("tbody");
    for (const e of sorted(list)) body.append(row(e));
    t.append(body);
    return t;
  };

  const row = (e: Entry) => {
    const { href, kind } = linkOf(e);
    let link: HTMLElement;
    if (e.type === "symlink") link = h("span", { class: "linkname" }, icon("link"), e.name + " ", h("span", { class: "target" }, "→ " + (e.target ?? "")));
    else if (e.upload) link = h("span", { class: "dim", title: "an upload in progress, or one that was not resumed" }, icon("file"), e.name);
    else {
      link = h("a", { href }, icon(kind), e.name);
      if (e.protected) link.append(h("span", { class: "badge lock" }, "protected"));
      if (e.nsfw) link.append(h("span", { class: "badge nsfw" }, "nsfw"));
      if (e.deleted_at) link.append(deletedBadge(e.purge_at));
      if (e.type === "file" && isAudioName(e.name)) link.append(playBtn(e));
    }
    const cls = [e.upload ? "dim" : e.dirty ? "dirty" : "", e.deleted_at ? "deleted" : "", sel.has(e.name) ? "selected" : "", e.type === "file" && player.isCurrent(pathOf(e), ctx.root) ? "playing" : ""].filter(Boolean).join(" ");
    return h("tr", { class: cls, "data-name": e.name }, h("td", { class: "sel" }, selectBox(e, "sel").firstChild ?? ""), h("td", { class: "name" }, link), h("td", { class: "size" }, e.type === "file" ? bytes(e.size) : ""), h("td", { class: "mtime", title: e.mtime }, when(e.mtime)));
  };

  const tiles = () => {
    const grid = h("div", { class: "ftiles" });
    for (const e of sorted(entries)) grid.append(tile(e));
    return grid;
  };

  const tile = (e: Entry) => {
    const { href, kind } = linkOf(e);
    const kindOf = mediaKind(ext(e.name));
    const thumb = h("span", { class: "thumb" + (info.nsfw || e.nsfw ? " nsfw" : "") });
    const showIcon = () => thumb.replaceChildren(icon(kind));
    const path = pathOf(e);
    const thumbPath = e.type === "dir" && e.cover ? join(path, e.cover) : path;
    const canThumb = e.type === "dir" ? !!e.cover : e.type === "file" && !e.upload && (kindOf === "image" || kindOf === "video" || kindOf === "audio");
    if (canThumb) {
      const img = h("img", { alt: "" }) as HTMLImageElement;
      thumb.append(img);
      if (thumb.classList.contains("nsfw")) thumb.append(h("span", { class: "veil" }, "nsfw · tap to show"));
      const root = e.deleted_at ? e.ino : ctx.root;
      const tp = e.deleted_at ? (e.type === "dir" && e.cover ? "/" + e.cover : "/") : thumbPath;
      lazy(img, e, tp, root, e.type === "dir" ? "audio" : (kindOf as "image" | "video" | "audio"), showIcon);
    } else showIcon();
    const meta = h("span", { class: "tmeta" });
    if (e.type === "file") meta.append(h("span", null, bytes(e.size)));
    const cls = ["ftile", e.type === "dir" ? "dir" : "", sel.has(e.name) ? "selected" : "", e.dirty ? "dirty" : "", e.deleted_at ? "deleted" : "", e.type === "file" && player.isCurrent(path, ctx.root) ? "playing" : ""].filter(Boolean).join(" ");
    const a = h("a", { class: cls, href, "data-name": e.name }, selectBox(e, "sel"), badges(e), thumb, h("span", { class: "tname", title: e.name }, e.name), meta);
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
    const bar = h("div", { class: "selbar" });
    if (!names.length) {
      bar.hidden = true;
      return bar;
    }
    const chosen = entries.filter((e) => names.includes(e.name) && !e.deleted_at);
    const protectedBy = info.protected ? dir.split("/").filter(Boolean).pop() ?? "/" : chosen.some((e) => e.protected) ? chosen.find((e) => e.protected)!.name : null;
    const delBtn = h("button", { type: "button", onClick: () => void doDelete(chosen) }, "Delete");
    const mvBtn = h("button", { type: "button", onClick: () => void doMove(chosen) }, "Move to…");
    const rnBtn = h("button", { type: "button", onClick: () => { renaming = chosen[0]!.name; formOpen = false; draw(); } }, "Rename");
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
    bar.append(h("span", { class: "count" }, `${chosen.length} selected`), delBtn, mvBtn, rnBtn);
    if (chosen.length && chosen.every((e) => e.type === "dir" || isAudioName(e.name))) {
      bar.append(h("button", { type: "button", onClick: () => void playSelection(chosen, false) }, "Play"), h("button", { type: "button", onClick: () => void playSelection(chosen, true) }, "Add to queue"));
    }
    bar.append(h("span", { class: "spacer" }), h("button", { type: "button", onClick: () => sel.clear() }, "Clear"));
    return bar;
  };

  const playSelection = async (chosen: Entry[], append: boolean) => {
    const tracks: Track[] = [];
    for (const e of chosen) {
      if (e.type === "dir") {
        try {
          const r = await ls(pathOf(e), ctx.root);
          for (const x of r.entries) if (x.type === "file" && isAudioName(x.name) && !x.deleted_at && !x.upload) tracks.push({ path: join(pathOf(e), x.name), root: ctx.root });
        } catch {
          // a folder that cannot be listed adds nothing
        }
      } else tracks.push({ path: pathOf(e), root: ctx.root });
    }
    if (!tracks.length) {
      toast("Nothing to play in the selection", "bad");
      return;
    }
    if (append) player.enqueue(tracks);
    else player.play(tracks, 0);
    sel.clear();
  };

  const doDelete = async (chosen: Entry[]) => {
    try {
      const r = await del(chosen.map(pathOf));
      if (r.deleted.length) toast(`Deleted ${r.deleted.length} ${r.deleted.length === 1 ? "entry" : "entries"} · in the trash` + (info.purge_at || chosen[0]?.purge_at ? "" : " for 30 d"), "ok");
      for (const f of r.failed) toast(`${f.path.split("/").pop()}: ${f.error}`, "bad");
      sel.clear();
      void refresh();
    } catch (e) {
      toast("delete: " + (e instanceof Error ? e.message : String(e)), "bad");
    }
  };

  const doMove = async (chosen: Entry[]) => {
    const subtree = chosen.filter((e) => e.type === "dir").map(pathOf);
    const to = await pick({
      start: dir,
      disabled: (p) => (p === dir ? "the selection is here" : subtree.some((s) => p === s || p.startsWith(s + "/")) ? "inside the selection" : null),
      action: (p) => `Move ${chosen.length} here` + (p === "/" ? "" : ""),
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
    if (inTrash) return;
    ev.preventDefault();
    root.querySelector("table.files")?.classList.add("drop");
  };
  const onDragLeave = () => root.querySelector("table.files")?.classList.remove("drop");
  const onDrop = (ev: DragEvent) => {
    if (inTrash) return;
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

  // keys: Delete, F2, Escape
  const onKey = (ev: KeyboardEvent) => {
    const t = ev.target as HTMLElement | null;
    if (t && (t.tagName === "INPUT" || t.tagName === "TEXTAREA" || t.tagName === "SELECT")) return;
    const chosen = entries.filter((e) => sel.has(e.name) && !e.deleted_at);
    if (ev.key === "Escape") {
      sel.clear();
      renaming = null;
      formOpen = false;
      draw();
    } else if (ev.key === "Delete" && chosen.length && !info.protected && !inTrash && !chosen.some((e) => e.protected)) void doDelete(chosen);
    else if (ev.key === "F2" && chosen.length === 1 && !inTrash) {
      renaming = chosen[0]!.name;
      draw();
    }
  };
  document.addEventListener("keydown", onKey);

  const unsubSel = sel.subscribe(draw);
  // a change in the player re-marks the rows in place (the playing row, the play
  // buttons): the listing is neither fetched nor rebuilt, so nothing flickers (docs/13 §6)
  const unsubPlayer = player.subscribe(() => {
    if (alb) alb.refresh();
    root.querySelectorAll("tr.playing, .ftile.playing").forEach((el) => el.classList.remove("playing"));
    const c = player.current();
    const here = c && c.root === ctx.root && c.path.startsWith(dir === "/" ? "/" : dir + "/") ? c.path.slice(dir === "/" ? 1 : dir.length + 1) : "";
    for (const el of root.querySelectorAll<HTMLElement>("tr[data-name], .ftile[data-name]")) {
      const mine = here !== "" && el.dataset["name"] === here;
      el.classList.toggle("playing", mine);
      const b = el.querySelector<HTMLButtonElement>("button.play");
      if (b) b.textContent = mine && player.playing ? "⏸ Playing" : "▶ Play";
    }
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

/** crumbs is the breadcrumb of a folder, with the folder's effective flags after the last segment. */
export function crumbs(ctx: Ctx, info?: DirInfo, inTrash = false): HTMLElement {
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

/** crumbsAll links every segment, for the file view's breadcrumb. */
export function crumbsAll(ctx: Ctx): HTMLElement {
  const el = h("div", { class: "crumbs" }, h("a", { href: "#/browse/" }, "/"));
  const parts = ctx.path.split("/").filter(Boolean);
  let acc = "";
  parts.forEach((p, i) => {
    acc += "/" + p;
    if (i > 0) el.append(h("span", { class: "sep" }, "/"));
    el.append(h("a", { href: browseHash({ path: acc, root: ctx.root }) }, p));
  });
  if (ctx.root !== undefined) el.append(h("span", { class: "badge" }, "deleted"));
  return el;
}

function iconFor(name: string, dir = false): IconKind {
  if (dir) return "dir";
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

export { encodePathForHash };
