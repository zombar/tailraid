// The file view: a native player for audio and video (the browser issues the Range
// requests), images with prev/next, small text in a <pre>, PDF in a frame, otherwise a
// download link; a folder playlist; the chunk integrity panel.

import { clear, h, toast } from "./dom.ts";
import { fileURL, ls, text, type Entry } from "./api.ts";
import { basename, bytes, dirname, ext, join, when } from "./util.ts";
import { crumbsAll } from "./browser.ts";
import * as chunksPanel from "./chunks.ts";
import { status } from "./status.ts";

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

// the media element is kept across sibling navigation so a user gesture keeps autoplay allowed
let media: { el: HTMLMediaElement; dir: string } | null = null;

export function render(root: HTMLElement, path: string): () => void {
  const dir = dirname(path);
  const name = basename(path);
  const kind = mediaKind(ext(name));
  let siblings: Entry[] = [];
  let entry: Entry | undefined;
  let keyHandler: ((ev: KeyboardEvent) => void) | null = null;

  const draw = () => {
    clear(root);
    const section = h("section", { class: "player" }, crumbsAll(dir));
    const info = entry ? `${bytes(entry.size)} · ${when(entry.mtime)}` : "";
    section.append(
      h(
        "div",
        { class: "title-row" },
        h("h1", null, name),
        h("span", { class: "small dim mono" }, info),
        h("span", { class: "actions" }, h("a", { class: "btn", href: fileURL(path, true) }, "Download")),
      ),
    );
    const viewer = h("div", { class: "viewer" });
    section.append(viewer);
    const group = siblings.filter((s) => sameGroup(mediaKind(ext(s.name)), kind));
    const index = group.findIndex((s) => s.name === name);
    const prev = index > 0 ? group[index - 1] : undefined;
    const next = index >= 0 && index < group.length - 1 ? group[index + 1] : undefined;
    const go = (e: Entry | undefined) => {
      if (e) location.hash = "#/file" + encodeForHash(join(dir, e.name));
    };

    if (kind === "video" || kind === "audio") {
      const mime = KINDS[ext(name)]?.[1] ?? "";
      let el: HTMLMediaElement;
      if (media && media.dir === dir && media.el.tagName.toLowerCase() === kind) {
        el = media.el;
        el.src = fileURL(path);
        void el.play().catch(() => undefined);
      } else {
        media?.el.pause();
        el = document.createElement(kind);
        el.controls = true;
        el.preload = "metadata";
        if (kind === "video") (el as HTMLVideoElement).playsInline = true;
        el.src = fileURL(path);
        media = { el, dir };
      }
      el.onended = () => go(next);
      viewer.append(el);
      if (mime && el.canPlayType(mime) === "") {
        section.append(
          h("div", { class: "note" }, "Your browser reports it cannot play ", h("span", { class: "mono" }, mime), ". tailraid serves files as stored, without transcoding; mkv, avi and flac play only where the browser has a native decoder. ", h("a", { href: fileURL(path, true) }, "Download instead"), "."),
        );
      }
    } else if (kind === "image") {
      viewer.append(h("img", { src: fileURL(path), alt: name }));
      if (next) new Image().src = fileURL(join(dir, next.name)); // prefetch
    } else if (kind === "text") {
      if (entry && entry.size > TEXT_MAX) {
        viewer.append(h("div", { class: "placeholder" }, "too large to preview", h("a", { class: "btn", href: fileURL(path, true) }, "Download")));
      } else {
        const pre = h("pre", null, "loading…");
        viewer.append(pre);
        text(path).then(
          (t) => (pre.textContent = t),
          (e: unknown) => (pre.textContent = "could not load: " + (e instanceof Error ? e.message : String(e))),
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
          h("button", { type: "button", disabled: !prev, onClick: () => go(prev) }, "◀ Previous"),
          h("button", { type: "button", disabled: !next, onClick: () => go(next) }, "Next ▶"),
          h("span", { class: "track" }, `${label} ${index + 1} of ${group.length}` + (next ? ` · next: ${next.name}` : "")),
        ),
      );
    }

    const cs = status.current?.volume.chunk_size;
    section.append(chunksPanel.render(path, entry && cs ? Math.ceil(entry.size / cs) : undefined));
    root.append(section);

    if (keyHandler) document.removeEventListener("keydown", keyHandler);
    keyHandler = (ev: KeyboardEvent) => {
      const t = ev.target as HTMLElement | null;
      if (t && (t.tagName === "INPUT" || t.tagName === "TEXTAREA" || t.tagName === "VIDEO" || t.tagName === "AUDIO")) return;
      if (ev.key === "ArrowRight") go(next);
      else if (ev.key === "ArrowLeft") go(prev);
      else if (ev.key === "Escape") location.hash = "#/browse" + encodeForHash(dir);
      else if (ev.key === " " && media) {
        ev.preventDefault();
        if (media.el.paused) void media.el.play().catch(() => undefined);
        else media.el.pause();
      }
    };
    document.addEventListener("keydown", keyHandler);
  };

  draw();
  ls(dir).then(
    (r) => {
      siblings = r.entries.filter((e) => e.type === "file");
      entry = siblings.find((e) => e.name === name);
      draw();
    },
    (e: unknown) => toast("Could not list the folder: " + (e instanceof Error ? e.message : String(e)), "bad"),
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

function sameGroup(a: Kind, b: Kind): boolean {
  if (a === b) return true;
  return (a === "video" || a === "audio") && (b === "video" || b === "audio");
}

function encodeForHash(path: string): string {
  return path.split("/").map(encodeURIComponent).join("/");
}
