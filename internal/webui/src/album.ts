// The album view (docs/13 §6): a folder whose audio files outnumber its other files
// shows its cover, the album and artist from the first track's tags, the year, the
// track count and total length, then the tracks in track order.

import { h } from "./dom.ts";
import { icon } from "./icons.ts";
import { tags, thumbURL, fileURL, type Entry, type Tags } from "./api.ts";
import { basename, ext, join, mmss, src, type Ctx } from "./util.ts";
import { isAudioName, player, type Track } from "./audio.ts";

export interface Album {
  el: HTMLElement;
  tracks: Track[];
  /** refresh re-marks the playing row. */
  refresh(): void;
}

/** isAlbum reports whether the listing renders as an album. */
export function isAlbum(entries: Entry[]): boolean {
  const live = entries.filter((e) => !e.deleted_at && !e.upload && e.type === "file");
  const audio = live.filter((e) => isAudioName(e.name)).length;
  return audio > 0 && audio > live.length - audio;
}

const PROBES = 4;

/** render draws the album; cover is null for a folder known to have none (I-165). */
export function render(ctx: Ctx, cover: string | null | undefined, entries: Entry[]): Album {
  const audio = entries.filter((e) => !e.deleted_at && !e.upload && e.type === "file" && isAudioName(e.name)).sort((a, b) => trackNo(a.name) - trackNo(b.name) || a.name.localeCompare(b.name, undefined, { numeric: true }));
  const tracks: Track[] = audio.map((e) => ({ path: join(ctx.path, e.name), root: src(ctx) }));
  const img = h("img", { class: "cover", alt: "" }) as HTMLImageElement;
  if (cover !== null) img.src = thumbURL(cover ? join(ctx.path, cover) : ctx.path, src(ctx)); // the folder itself resolves its cover when the listing did not carry one
  img.addEventListener("error", () => img.removeAttribute("src"));
  const title = h("h1", null, basename(ctx.path) || "/");
  const artist = h("div", { class: "artist" });
  const meta = h("div", { class: "meta" });
  const head = h("div", { class: "album-head" }, img, h("div", null, title, artist, meta));
  const body = h("tbody");
  const table = h("table", { class: "tracks" }, body);
  const el = h("section", { class: "album" }, head, table);
  const info = new Map<number, Tags>();
  const durs = new Map<number, number>();
  const rows: HTMLTableRowElement[] = [];

  const describe = () => {
    const first = info.get(0) ?? Array.from(info.values())[0];
    if (first) {
      if (first.album) title.textContent = first.album;
      artist.textContent = first.album_artist || first.artist || "";
    }
    const parts: string[] = [];
    if (first?.year) parts.push(String(first.year));
    parts.push(`${audio.length} ${audio.length === 1 ? "track" : "tracks"}`);
    if (durs.size === audio.length && audio.length) parts.push(mmss(Array.from(durs.values()).reduce((s, d) => s + d, 0)));
    const exts = Array.from(new Set(audio.map((e) => ext(e.name))));
    if (exts.length) parts.push(exts.join(", "));
    meta.replaceChildren(...parts.map((p) => h("span", null, p)));
  };

  const drawRow = (i: number) => {
    const e = audio[i]!;
    const t = tracks[i]!;
    const g = info.get(i);
    const stem = e.name.replace(/\.[^.]+$/, "");
    const no = g?.track || trackNo(e.name) || i + 1;
    const playing = player.isCurrent(t.path, t.root);
    const tr = rows[i]!;
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

  // tags and durations arrive lazily, four probes at a time, rows near the viewport first
  let active = 0;
  const pending: number[] = [];
  const probe = async (i: number) => {
    const t = tracks[i]!;
    try {
      const g = await tags(t.path, t.root);
      info.set(i, g);
      t.title = g.title || audio[i]!.name.replace(/\.[^.]+$/, "");
      t.artist = g.artist;
      t.album = g.album;
      t.hasCover = g.has_cover;
    } catch {
      // a file whose tags cannot be read keeps its name
    }
    const d = await duration(fileURL(t.path, false, t.root));
    if (d !== undefined) {
      durs.set(i, d);
      t.dur = d;
    }
    drawRow(i);
    describe();
  };
  const pump = () => {
    while (active < PROBES && pending.length) {
      const i = pending.shift()!;
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
          pending.push(rows.indexOf(x.target as HTMLTableRowElement));
        }
        pump();
      },
      { rootMargin: "300px" },
    );
    for (const tr of rows) io.observe(tr);
  } else {
    rows.forEach((_, i) => pending.push(i));
    pump();
  }

  return {
    el,
    tracks,
    refresh: () => rows.forEach((_, i) => drawRow(i)),
  };
}

/** trackNo is the leading number of a file name ("03 - Jam.flac" → 3), 0 without one. */
export function trackNo(name: string): number {
  const m = /^\s*(\d{1,3})\b/.exec(name);
  return m ? Number(m[1]) : 0;
}

/** duration asks the browser for a file's length through a metadata-only load. */
function duration(src: string): Promise<number | undefined> {
  return new Promise((resolve) => {
    const a = document.createElement("audio");
    a.preload = "metadata";
    const done = (d: number | undefined) => {
      a.removeAttribute("src");
      a.load();
      resolve(d);
    };
    const timer = setTimeout(() => done(undefined), 15_000);
    a.onloadedmetadata = () => {
      clearTimeout(timer);
      done(Number.isFinite(a.duration) ? a.duration : undefined);
    };
    a.onerror = () => {
      clearTimeout(timer);
      done(undefined);
    };
    a.src = src;
  });
}
