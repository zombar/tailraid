// The Media tab (docs/13 §10, I-212): one section per library folder, films and shows by
// poster and albums by cover, and a detail page with the backdrop, facts, plot, people
// and, for a show, its seasons and episodes. The node lists the structure live and
// fills in the catalogue's metadata; a film without sidecars shows its cleaned name and
// the browser-made frame. Routes: #/media and #/media/item/<path>.

import { clear, h, titleCard } from "./dom.ts";
import { icon } from "./icons.ts";
import { fileURL, media, mediaItem, thumbURL, type MediaFile, type MediaItem, type MediaLibrary } from "./api.ts";
import { lazy } from "./thumbs.ts";
import { basename, browseHash, bytes, count, dirname, encodePathForHash, fileHash, session } from "./util.ts";

const NOUN: Record<string, [string, string]> = { movies: ["film", "films"], tv: ["show", "shows"], music: ["album", "albums"], roms: ["platform", "platforms"] };

/** WARN_FLAGS are a dump's flags drawn in the warn colour (docs/13 §10). */
const WARN_FLAGS = new Set(["proto", "hack", "beta", "demo", "bad", "unl", "pirate", "dlc", "update"]);

/** chips are a game's region, revision and flag badges, "+n" for its other dumps. */
function chips(it: { regions?: string[]; revision?: string; flags?: string[]; languages?: string[]; variants?: number; discs?: number }, more = false): HTMLElement {
  const el = h("span", { class: "chips" });
  for (const r of it.regions ?? []) el.append(h("span", { class: "badge" }, r));
  if (more) for (const l of it.languages ?? []) el.append(h("span", { class: "badge" }, l));
  if (it.revision) el.append(h("span", { class: "badge" }, it.revision));
  for (const f of it.flags ?? []) if (f !== "base") el.append(h("span", { class: "badge" + (WARN_FLAGS.has(f) ? " warn" : "") }, f));
  if (more && it.discs && it.discs > 1) el.append(h("span", { class: "badge" }, `${it.discs} discs`));
  if (it.variants) el.append(h("span", { class: "badge" }, `+${it.variants}`));
  return el;
}

/** cardSub is the line under an item's title card: a platform's folder name, a game's platform, else the year. */
function cardSub(it: MediaItem): string | undefined {
  if (it.kind === "platform") return basename(it.path) !== it.title ? basename(it.path) : undefined;
  if (it.kind === "game") return it.platform;
  return it.year ? String(it.year) : undefined;
}

function glyph(kind: string): SVGElement {
  return icon(kind === "album" ? "album" : "movie");
}

function duration(min: number | undefined): string {
  if (!min) return "";
  return min >= 60 ? `${Math.floor(min / 60)} h ${String(min % 60).padStart(2, "0")}` : `${min} min`;
}

function itemHash(it: MediaItem): string { // a platform's and a game's pages are items like a film's
  return it.kind === "album" ? browseHash({ path: it.path }) : "#/media/item" + encodePathForHash(it.path);
}

/** art is the poster tile's picture, or the kind glyph when there is none. */
function art(it: MediaItem): string | undefined {
  if (it.poster) return thumbURL(it.poster);
  if (it.kind === "album") return thumbURL(it.path); // the folder resolves its cover (I-165)
  if (it.video) return thumbURL(it.video); // the browser-made frame (I-160)
  return undefined;
}

/**
 * frameOf shows a film's or an episode's video frame in img, grabbing and storing it
 * first when none is stored, as the file browser's tiles do (docs/13 §5); false when
 * the item has no video to grab from.
 */
function frameOf(img: HTMLImageElement, it: MediaItem, onMiss: () => void): boolean {
  if (it.poster || it.thumb || !it.video || it.video_ino === undefined) return false;
  lazy(img, { ino: it.video_ino, version: it.video_version ?? 0, thumb: it.video_thumb }, it.video, undefined, "video", onMiss);
  return true;
}

/** meta is the card's line under the title: the year (an album's artist) left, a note right. */
function meta(it: MediaItem): string[] {
  if (it.kind === "album") return [it.artist ?? "", it.year ? String(it.year) : ""];
  return [it.year ? String(it.year) : "", it.kind === "movie" && !it.runtime && !it.plot ? "no sidecar" : ""];
}

/** fact is the chip over the art's corner: a film's runtime, a show's seasons, a platform's games. */
function fact(it: MediaItem): string {
  if (it.kind === "movie" && it.runtime) return duration(it.runtime);
  if (it.kind === "show" && it.children) return `${it.children} ${it.children === 1 ? "season" : "seasons"}`;
  if (it.kind === "platform") return `${it.children ?? 0} ${it.children === 1 ? "game" : "games"}`;
  return "";
}

const metaSpans = (it: MediaItem) => meta(it).map((t) => h("span", null, t));

function posterTile(it: MediaItem): HTMLElement {
  const veiled = it.nsfw && !session.get("tr.revealed", false);
  const box = h("span", { class: "art" });
  const chip = fact(it) ? h("span", { class: "ov br" }, fact(it)) : null;
  const below = it.kind === "game" && !veiled ? chips(it) : h("span", { class: "pyear" }, ...metaSpans(it));
  const a = h("a", { class: "poster" + (veiled ? " nsfw" : ""), href: itemHash(it) }, box, h("span", { class: "ptitle", title: it.title }, it.title), below);
  const missing = () => {
    clear(box);
    // art that does not exist is a title card (I-280); an album keeps its note glyph
    box.append(it.kind === "album" ? glyph(it.kind) : titleCard(it.title, cardSub(it)));
    if (chip) box.append(chip);
    if (it.kind !== "platform") a.classList.add("missing");
  };
  const src = art(it);
  const frame = h("img", { alt: "" }) as HTMLImageElement;
  if (frameOf(frame, it, missing)) box.append(frame);
  else if (src) {
    const img = h("img", { alt: "", loading: "lazy", src }) as HTMLImageElement;
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

/** stored reads a per-viewer flag from localStorage, false when storage is unavailable. */
function stored(key: string): boolean {
  try {
    return localStorage.getItem(key) === "1";
  } catch {
    return false;
  }
}

function remember(key: string, on: boolean): void {
  try {
    if (on) localStorage.setItem(key, "1");
    else localStorage.removeItem(key);
  } catch {
    // storage may be unavailable
  }
}

function librarySection(lib: MediaLibrary): HTMLElement {
  const [one, many] = NOUN[lib.kind] ?? ["item", "items"];
  const countEl = h("span", { class: "count" }, "…");
  // a section folds away; the choice is a per-viewer convenience, and a folded library
  // is listed only once it is opened
  const key = `tr.media.closed.${lib.path}`;
  const fold = h("details", null, h("summary", null, h("h2", null, basename(lib.path) || "/", " ", h("span", { class: "badge lib" }, lib.kind), countEl))) as HTMLDetailsElement;
  fold.open = !stored(key);
  const section = h("section", { class: "lib " + lib.kind }, fold);
  const grid = h("div", { class: "posters" });
  const sorts = ["title", "year", "added"];
  let sort = session.get(`tr.media.sort.${lib.path}`, "title");
  let items: MediaItem[] = [];
  const buttons = sorts.map((s) => h("button", { type: "button", "aria-pressed": String(s === sort) }, s[0]!.toUpperCase() + s.slice(1)));
  const draw = () => {
    clear(grid);
    for (const it of items) grid.append(posterTile(it));
  };
  const load = () => {
    media(lib.path, sort).then(
      (res) => {
        items = res.items;
        clear(countEl);
        countEl.append(`${count(res.lib.count)} ${res.lib.count === 1 ? one : many}${res.lib.count > res.items.length ? ` (first ${res.items.length})` : ""} · `, h("a", { href: browseHash({ path: lib.path }) }, "open folder"));
        draw();
      },
      (e: unknown) => {
        countEl.textContent = "could not list: " + (e instanceof Error ? e.message : String(e));
      },
    );
  };
  buttons.forEach((b, i) =>
    b.addEventListener("click", () => {
      sort = sorts[i]!;
      session.set(`tr.media.sort.${lib.path}`, sort);
      buttons.forEach((o, j) => o.setAttribute("aria-pressed", String(i === j)));
      load();
    }),
  );
  fold.append(h("div", { class: "toolbar" }, h("span", { class: "seg", role: "group", "aria-label": "sort" }, ...buttons)), grid);
  let loaded = false;
  const open = () => {
    if (fold.open && !loaded) {
      loaded = true;
      load();
    }
  };
  fold.addEventListener("toggle", () => {
    remember(key, !fold.open);
    open();
  });
  open();
  return section;
}

function overview(root: HTMLElement): void {
  media().then(
    (res) => {
      if (res.libs.length === 0) {
        root.append(
          h(
            "section",
            null,
            h("h2", null, "Media"),
            h("div", { class: "empty" }, "No library yet: open a folder in Files, choose its menu and set ", h("b", null, "Library"), " to movies, tv, music or roms."),
          ),
        );
        return;
      }
      for (const lib of res.libs) root.append(librarySection(lib));
    },
    (e: unknown) => root.append(h("div", { class: "note" }, "could not list the libraries: " + (e instanceof Error ? e.message : String(e)))),
  );
}

function episodeNo(e: MediaItem): string {
  const two = (n: number) => String(n).padStart(2, "0");
  if (e.part) return `Part ${e.part}`; // a miniseries (docs/18 §8a)
  if (!e.episode && e.date) return e.date;
  if (!e.episode && e.absolute) return "#" + String(e.absolute).padStart(3, "0");
  return `S${two(e.season ?? 0)}E${two(e.episode ?? 0)}` + (e.episode_to && e.episode_to !== e.episode ? `E${two(e.episode_to)}` : "");
}

/** platformPage lists a platform's games, one card per title (docs/13 §10). */
function platformPage(root: HTMLElement, it: MediaItem): void {
  const grid = h("div", { class: "posters games" });
  for (const g of it.games ?? []) grid.append(posterTile(g));
  boxShape(grid);
  const n = it.games?.length ?? 0;
  root.append(
    h(
      "section",
      { class: "lib roms" },
      h("h2", null, it.title, " ", h("span", { class: "badge lib" }, "roms"), h("span", { class: "count" }, `${count(n)} ${n === 1 ? "game" : "games"} · `, h("a", { href: browseHash({ path: it.path }) }, "open folder"))),
      grid,
    ),
  );
}

/**
 * boxShape sizes a platform's game tiles to its box art: a platform's boxes share a shape
 * (SNES and N64 wide, NES and Mega Drive tall, Game Boy and PlayStation square) that no
 * one ratio fits, so the tiles take the median of the art loaded so far (docs/13 §10),
 * and the odd box out sits whole inside its tile instead of cropped.
 */
function boxShape(grid: HTMLElement): void {
  const ratios: number[] = [];
  grid.addEventListener(
    "load",
    (ev) => {
      const img = ev.target as HTMLImageElement;
      if (!(img instanceof HTMLImageElement) || !img.naturalWidth || !img.naturalHeight) return;
      ratios.push(img.naturalWidth / img.naturalHeight);
      const sorted = [...ratios].sort((a, b) => a - b);
      grid.style.setProperty("--box", String(sorted[sorted.length >> 1]));
    },
    true, // load does not bubble: catch it on the way down
  );
}

/** gamePage is a game: its art or a title card, its facts and chips, its files to download; it has no Play (I-279). */
function gamePage(root: HTMLElement, it: MediaItem): void {
  const section = h("section", { class: it.poster ? "hero poster-only game" : "hero no-art" });
  let poster: HTMLElement = titleCard(it.title, it.platform);
  poster.classList.add("hero-poster");
  if (it.poster) {
    section.append(h("img", { class: "backdrop", alt: "", src: fileURL(it.poster) }));
    const img = h("img", { class: "hero-poster", alt: "", src: fileURL(it.poster) }) as HTMLImageElement;
    const card = poster;
    img.addEventListener("error", () => img.replaceWith(card));
    poster = img;
  }
  const facts = h("div", { class: "facts" });
  for (const f of [it.platform ?? "", it.year ? String(it.year) : "", it.developer ?? "", it.publisher ?? "", it.players ? `${it.players} ${it.players === "1" ? "player" : "players"}` : "", (it.genres ?? []).join(" · ").toLowerCase()])
    if (f) facts.append(h("span", null, f));
  facts.append(h("span", { class: "dim" }, dirname(it.path)));
  const body = h("div", null, h("h1", null, it.title), facts, chips(it, true));
  if (it.plot) body.append(h("p", { class: "plot" }, it.plot));
  const files = it.files ?? [];
  const isFolder = files.some((f) => f.path.startsWith(it.path + "/")); // a disc set's folder
  const folder = isFolder ? it.path : dirname(it.path);
  body.append(h("div", { class: "actions" }, h("a", { class: "btn", href: browseHash({ path: folder }) }, "Open folder")));
  if (files.length > 0) {
    const tbody = h("tbody");
    for (const f of files) tbody.append(fileRow(f, files.length));
    body.append(h("table", { class: "episodes discs" }, tbody));
  }
  section.append(h("div", { class: "hero-body" }, poster, body));
  root.append(section);
}

/** fileRow is one of a game's files: its disc or its dump's tags, the name as the download. */
function fileRow(f: MediaFile, n: number): HTMLElement {
  const label = f.disc ? `Disc ${f.disc}` : n > 1 ? [...(f.regions ?? []), f.revision ?? ""].filter(Boolean).join(" ") || "—" : "File";
  return h("tr", null, h("td", { class: "no" }, label), h("td", { class: "title" }, h("a", { href: fileURL(f.path, true) }, f.name)), h("td", { class: "dur" }, bytes(f.size)));
}

function detail(root: HTMLElement, path: string): void {
  mediaItem(path).then(
    (it) => {
      if (it.kind === "platform") return platformPage(root, it);
      if (it.kind === "game") return gamePage(root, it);
      const backdrop = it.fanart ? fileURL(it.fanart) : it.poster ? fileURL(it.poster) : "";
      const cls = "hero" + (it.fanart ? "" : it.poster ? " poster-only" : " no-art");
      const section = h("section", { class: cls });
      if (backdrop) section.append(h("img", { class: "backdrop", alt: "", src: backdrop }));
      const posterSrc = it.poster ? fileURL(it.poster) : it.video ? thumbURL(it.video) : it.kind === "album" ? thumbURL(it.path) : "";
      const poster = h("img", { class: "hero-poster", alt: "" }) as HTMLImageElement;
      // no art and no frame: the title card stands in (docs/13 §10, I-280); an album keeps the gap
      const hide = () => {
        if (it.kind === "album") poster.style.visibility = "hidden";
        else {
          const card = titleCard(it.title, it.year ? String(it.year) : undefined);
          card.classList.add("hero-poster");
          poster.replaceWith(card);
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
        it.kind === "show" ? `${seasons.length} ${seasons.length === 1 ? "season" : "seasons"} · ${episodes} ${episodes === 1 ? "episode" : "episodes"}` : duration(it.runtime),
        it.rating ? `★ ${it.rating}` : "",
        (it.genres ?? []).join(" · ").toLowerCase(),
        it.mpaa ?? "",
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
      if (target) play.addEventListener("click", () => (location.hash = fileHash({ path: target })));
      else (play as HTMLButtonElement).disabled = true;
      const folder = it.video === it.path ? dirname(it.path) : it.path;
      body.append(h("div", { class: "actions" }, play, h("a", { class: "btn", href: browseHash({ path: folder }) }, "Open folder")));
      if (it.people?.length) {
        const people = h("div", { class: "people" });
        it.people.forEach((p, i) => people.append(h("span", { class: "person" }, i === 0 && it.kind === "movie" && it.people!.length > 1 ? `${p} · director` : p)));
        body.append(people);
      }
      if (seasons.length > 0) {
        const strip = h("div", { class: "seasons" });
        const table = h("table", { class: "episodes" });
        const tbody = h("tbody");
        table.append(tbody);
        const show = (i: number) => {
          strip.querySelectorAll("button").forEach((b, j) => b.setAttribute("aria-pressed", String(i === j)));
          clear(tbody);
          for (const e of seasons[i]!.episodes) {
            const thumb = h("td", { class: "thumb" });
            const img = h("img", { alt: "" }) as HTMLImageElement;
            const miss = () => img.replaceWith(glyph("episode"));
            if (!frameOf(img, e, miss)) {
              img.loading = "lazy";
              img.src = thumbURL(e.thumb || e.path);
              img.addEventListener("error", miss);
            }
            thumb.append(img);
            const title = h("td", { class: "title" }, e.title);
            if (e.plot) title.append(h("div", { class: "plot small dim" }, e.plot));
            const row = h("tr", null, h("td", { class: "no" }, episodeNo(e)), thumb, title, h("td", { class: "dur" }, duration(e.runtime)));
            row.addEventListener("click", () => (location.hash = fileHash({ path: e.path })));
            tbody.append(row);
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
      section.append(h("div", { class: "hero-body" }, poster, body));
      if (noArt) hide();
      root.append(section);
    },
    (e: unknown) => root.append(h("div", { class: "note" }, "could not open: " + (e instanceof Error ? e.message : String(e)))),
  );
}

export function render(root: HTMLElement, rest: string): () => void {
  if (rest.startsWith("/item/")) detail(root, rest.slice("/item".length));
  else overview(root);
  return () => undefined;
}
