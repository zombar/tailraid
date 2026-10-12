// The file view: a native player for video (the browser issues the Range requests), the
// album-style page for audio (the now-playing bar plays it), images with prev/next, small
// text in a <pre>, PDF in a frame, otherwise a download link; the trash banner for a
// deleted file; the nsfw veil; the chunk integrity panel.

import { clear, h, toast } from "./dom.ts";
import { fileURL, ls, stat, tags, text, thumbURL, trashList, type Entry, type SnapRef, type TrashEntry } from "./api.ts";
import { basename, browseHash, bytes, dirname, ext, fileHash, join, mmss, session, src, when, type Ctx } from "./util.ts";
import * as snaps from "./snapshots.ts";
import { crumbsAll } from "./browser.ts";
import * as chunksPanel from "./chunks.ts";
import { status } from "./status.ts";
import { banner } from "./trash.ts";
import { player as audio } from "./audio.ts";
import * as markdown from "./markdown.ts";

export type Kind = "video" | "audio" | "image" | "text" | "pdf" | "other";

const KINDS: Record<string, [Kind, string]> = {
  mp4: ["video", "video/mp4"], m4v: ["video", "video/mp4"], webm: ["video", "video/webm"], mov: ["video", "video/quicktime"], ogv: ["video", "video/ogg"],
  mkv: ["video", "video/x-matroska"], avi: ["video", "video/x-msvideo"],
  mp3: ["audio", "audio/mpeg"], m4a: ["audio", "audio/mp4"], aac: ["audio", "audio/aac"], ogg: ["audio", "audio/ogg"], oga: ["audio", "audio/ogg"],
  opus: ["audio", "audio/ogg"], wav: ["audio", "audio/wav"], flac: ["audio", "audio/flac"],
  jpg: ["image", ""], jpeg: ["image", ""], png: ["image", ""], gif: ["image", ""], webp: ["image", ""], avif: ["image", ""], svg: ["image", ""], bmp: ["image", ""],
  txt: ["text", ""], md: ["text", ""], markdown: ["text", ""], json: ["text", ""], log: ["text", ""], yaml: ["text", ""], yml: ["text", ""], toml: ["text", ""],
  csv: ["text", ""], ini: ["text", ""], conf: ["text", ""], go: ["text", ""], ts: ["text", ""], js: ["text", ""], py: ["text", ""], sh: ["text", ""], html: ["text", ""],
  pdf: ["pdf", ""],
};

const TEXT_MAX = 1024 * 1024;

export function mediaKind(e: string): Kind {
  return KINDS[e]?.[0] ?? "other";
}

// the video element is kept across sibling navigation so a user gesture keeps autoplay allowed
let media: { el: HTMLMediaElement; dir: string } | null = null;
// the integrity panel is built once per file and re-attached on the view's second draw
// (after the folder listing arrives), so a panel the user opened is not thrown away (I-161)
let panel: { path: string; p: chunksPanel.Panel } | null = null;

export function render(root: HTMLElement, ctx: Ctx): () => void {
  const path = ctx.path;
  const dir = dirname(path);
  const name = basename(path) || "/";
  const kind = mediaKind(ext(name));
  const inTrash = ctx.root !== undefined;
  const asOf = ctx.at !== undefined;
  let snap: SnapRef | undefined; // the snapshot the file is read as of (docs/16 §6)
  let offlineErr: unknown = null; // its mirror is offline: nothing to show but why
  let versionsPanel: HTMLElement | null = null;
  let siblings: Entry[] = [];
  let entry: Entry | undefined;
  let trashRoot: TrashEntry | undefined;
  let nsfw = false;
  let keyHandler: ((ev: KeyboardEvent) => void) | null = null;

  if (kind === "video") audio.pause(); // a video page pauses the music (docs/13 §6)

  const draw = () => {
    clear(root);
    const section = h("section", { class: "player" }, crumbsAll({ path: dir, root: ctx.root, at: ctx.at }, snap?.name));
    if (asOf && offlineErr) {
      section.append(...snaps.offlineView(ctx, offlineErr, fileHash({ path })));
      root.append(section);
      return;
    }
    if (asOf) section.append(snaps.banner(ctx, snap, "", false, fileHash({ path })));
    if (inTrash && (trashRoot || entry?.deleted_at)) {
      const r: TrashEntry = trashRoot ?? { ino: ctx.root!, name, type: "file", size: entry?.size ?? 0, path: name, deleted_at: entry?.deleted_at ?? "", restorable: true };
      section.append(banner({ root: { ...r, purge_at: r.purge_at ?? entry?.purge_at }, bytes: entry?.size }));
    }
    const info = entry ? `${bytes(entry.size)} · ${when(entry.mtime)}` : "";
    const actions = h("span", { class: "actions" }, h("a", { class: "btn", href: fileURL(path, true, src(ctx)) }, "Download"));
    if (!inTrash) actions.prepend(snaps.asofMenu(ctx, "file", snap));
    const titleRow = h("div", { class: "title-row" }, h("h1", null, name), h("span", { class: "small dim mono" }, info), actions);
    section.append(titleRow);
    const veiled = nsfw && !session.get("tr.revealed", false);
    const viewer = h("div", { class: "viewer" + (nsfw ? " nsfw" : "") + (nsfw && !veiled ? " revealed" : "") });
    section.append(viewer);
    const group = siblings.filter((s) => sameGroup(mediaKind(ext(s.name)), kind));
    const index = group.findIndex((s) => s.name === name);
    const prev = index > 0 ? group[index - 1] : undefined;
    const next = index >= 0 && index < group.length - 1 ? group[index + 1] : undefined;
    const go = (e: Entry | undefined) => {
      if (e) location.hash = fileHash({ path: join(dir, e.name), root: ctx.root, at: ctx.at });
    };

    if (kind === "video") {
      const mime = KINDS[ext(name)]?.[1] ?? "";
      let el: HTMLMediaElement;
      if (media && media.dir === dir) {
        el = media.el;
        el.src = fileURL(path, false, src(ctx));
        void el.play().catch(() => undefined);
      } else {
        media?.el.pause();
        el = document.createElement("video");
        el.controls = true;
        el.preload = "metadata";
        (el as HTMLVideoElement).playsInline = true;
        el.src = fileURL(path, false, src(ctx));
        media = { el, dir };
      }
      el.onended = () => go(next);
      viewer.append(el);
      if (mime && el.canPlayType(mime) === "") {
        section.append(
          h("div", { class: "note" }, "Your browser reports it cannot play ", h("span", { class: "mono" }, mime), ". tailraid serves files as stored, without transcoding; mkv, avi and flac play only where the browser has a native decoder. ", h("a", { href: fileURL(path, true, src(ctx)) }, "Download instead"), "."),
        );
      }
    } else if (kind === "audio") {
      viewer.append(audioCard(ctx, name, entry, group));
    } else if (kind === "image") {
      viewer.append(h("img", { src: fileURL(path, false, src(ctx)), alt: name }));
      if (next) new Image().src = fileURL(join(dir, next.name), false, src(ctx)); // prefetch
    } else if (kind === "text") {
      if (entry && entry.size > TEXT_MAX) {
        viewer.append(h("div", { class: "placeholder" }, "too large to preview", h("a", { class: "btn", href: fileURL(path, true, src(ctx)) }, "Download")));
      } else if (markdown.isMarkdown(name)) {
        const doc = markdown.view(path, src(ctx));
        titleRow.insertBefore(doc.seg, actions);
        if (nsfw) viewer.append(doc.body);
        else viewer.replaceWith(doc.body); // a document reads on the page, not in the media frame
      } else {
        const pre = h("pre", null, "loading…");
        viewer.append(pre);
        text(path, src(ctx)).then(
          (t) => (pre.textContent = t),
          (e: unknown) => (pre.textContent = "could not load: " + (e instanceof Error ? e.message : String(e))),
        );
      }
    } else if (kind === "pdf") {
      viewer.append(h("iframe", { src: fileURL(path, false, src(ctx)), title: name }));
      section.append(h("div", { class: "small dim" }, h("a", { href: fileURL(path, false, src(ctx)), target: "_blank", rel: "noopener" }, "Open in a new tab")));
    } else {
      viewer.append(h("div", { class: "placeholder" }, info || name, h("a", { class: "btn", href: fileURL(path, true, src(ctx)) }, "Download")));
    }
    if (nsfw) {
      viewer.append(h("span", { class: "veil" }, "nsfw · tap to show"));
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
          h("button", { type: "button", disabled: !prev, onClick: () => go(prev) }, "◀ Previous"),
          h("button", { type: "button", disabled: !next, onClick: () => go(next) }, "Next ▶"),
          h("span", { class: "track" }, `${label} ${index + 1} of ${group.length}` + (next ? ` · next: ${next.name}` : "")),
        ),
      );
    }

    if (!inTrash && !asOf) {
      versionsPanel ??= snaps.versionsPanel(path, () => void reload());
      section.append(versionsPanel);
      const cs = status.current?.volume.chunk_size;
      const expected = entry && cs ? Math.ceil(entry.size / cs) : undefined;
      if (!panel || panel.path !== path) panel = { path, p: chunksPanel.render(path, expected) };
      else if (expected !== undefined) panel.p.expect(expected);
      section.append(panel.p.el);
    }
    root.append(section);

    if (keyHandler) document.removeEventListener("keydown", keyHandler);
    keyHandler = (ev: KeyboardEvent) => {
      const t = ev.target as HTMLElement | null;
      if (t && (t.tagName === "INPUT" || t.tagName === "TEXTAREA" || t.tagName === "VIDEO" || t.tagName === "AUDIO")) return;
      if (kind === "audio") return; // the now-playing bar owns the keys
      if (ev.key === "ArrowRight") go(next);
      else if (ev.key === "ArrowLeft") go(prev);
      else if (ev.key === "Escape") location.hash = inTrash ? `#/trash/${ctx.root}` + encodeForHash(dir) : browseHash({ path: dir, at: ctx.at });
      else if (ev.key === " " && media) {
        ev.preventDefault();
        if (media.el.paused) void media.el.play().catch(() => undefined);
        else media.el.pause();
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
    if (media && media.dir === dir) media.dir = ""; // the bytes changed: the element loads them again
    draw();
  };
  const listing = inTrash && dir === "/" && name !== "/" ? ls("/", ctx.root) : ls(dir, src(ctx));
  listing.then(
    (r) => {
      siblings = r.entries.filter((e) => e.type === "file" && !e.upload && !e.deleted_at);
      entry = siblings.find((e) => e.name === name);
      nsfw = r.dir.nsfw || !!entry?.nsfw;
      snap = r.dir.snapshot;
      draw();
    },
    (e: unknown) => {
      if (asOf && snaps.isOffline(e)) {
        offlineErr = e;
        void snaps.catalogue().then(draw, draw);
        return;
      }
      if (inTrash) {
        // the file itself is the deleted root: stat it through the root
        stat("/", ctx.root).then(
          (st) => {
            entry = st;
            draw();
          },
          () => toast("Could not read the deleted file: " + (e instanceof Error ? e.message : String(e)), "bad"),
        );
      } else toast("Could not list the folder: " + (e instanceof Error ? e.message : String(e)), "bad");
    },
  );
  if (inTrash) {
    trashList().then(
      (r) => {
        trashRoot = r.entries.find((t) => t.ino === ctx.root);
        draw();
      },
      () => undefined,
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

/** audioCard is the audio file's page: a large cover, its tags and a Play button; the now-playing bar carries the transport. */
function audioCard(ctx: Ctx, name: string, entry: Entry | undefined, group: Entry[]): HTMLElement {
  const img = h("img", { class: "cover", alt: "" }) as HTMLImageElement;
  img.src = thumbURL(ctx.path, src(ctx));
  img.addEventListener("error", () => img.removeAttribute("src"));
  const title = h("h1", null, name.replace(/\.[^.]+$/, ""));
  const artist = h("div", { class: "artist" });
  const meta = h("div", { class: "meta" });
  const dirPath = dirname(ctx.path);
  const tracks = group.filter((e) => mediaKind(ext(e.name)) === "audio").map((e) => ({ path: join(dirPath, e.name), root: src(ctx) }));
  const index = Math.max(0, tracks.findIndex((t) => t.path === ctx.path));
  const play = h("button", { type: "button", onClick: () => (tracks.length ? audio.play(tracks, index) : audio.playNow({ path: ctx.path, root: src(ctx) })) }, audio.isCurrent(ctx.path, src(ctx)) && audio.playing ? "⏸ Playing" : "▶ Play");
  const queueBtn = h("button", { type: "button", onClick: () => audio.enqueue([{ path: ctx.path, root: src(ctx) }]) }, "Add to queue");
  const head = h("div", { class: "album-head" }, img, h("div", null, title, artist, meta, h("div", { class: "controls" }, play, queueBtn)));
  tags(ctx.path, src(ctx)).then(
    (g) => {
      if (g.title) title.textContent = g.title;
      artist.textContent = [g.artist, g.album].filter(Boolean).join(" — ");
      const parts: string[] = [];
      if (g.track) parts.push(`track ${g.track}` + (g.tracks ? ` of ${g.tracks}` : ""));
      if (g.year) parts.push(String(g.year));
      if (g.genre) parts.push(g.genre);
      if (entry) parts.push(ext(name), bytes(entry.size));
      const c = audio.current();
      if (c && c.path === ctx.path && c.dur) parts.push(mmss(c.dur));
      meta.replaceChildren(...parts.map((p) => h("span", null, p)));
    },
    () => undefined,
  );
  return h("section", { class: "album" }, head);
}

function sameGroup(a: Kind, b: Kind): boolean {
  if (a === b) return true;
  return (a === "video" || a === "audio") && (b === "video" || b === "audio");
}

function encodeForHash(path: string): string {
  return path.split("/").map(encodeURIComponent).join("/");
}
