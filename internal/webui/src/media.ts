// The Media tab (docs/13 §10, I-212): its own search (§10d), the viewer's Continue
// watching row (§10c), one section per library folder, films and shows by poster and
// albums by cover, and a detail page with the backdrop, facts, plot, people and, for a
// show, its seasons and episodes. The node lists the structure live and fills in the
// catalogue's metadata; a film without sidecars shows its cleaned name and the
// browser-made frame. Routes: #/media, #/media?q=<query> and #/media/item/<path>; Play
// opens the viewer (viewer.ts) over the tab.

import { clear, h, titleCard, toast } from "./dom.ts";
import { icon } from "./icons.ts";
import { continueRow, fileURL, media, mediaItem, mediaSearch, report, suggest, thumbURL, type MediaFile, type MediaItem, type MediaLibrary, type Progress } from "./api.ts";
import { lazy, lazyURL } from "./thumbs.ts";
import { basename, browseHash, bytes, count, dirname, encodePathForHash, session } from "./util.ts";
import { albumHash, markWatched, playHash, searchHash, setLibraries } from "./viewer.ts";
import { left, resumeFrom } from "./watch.ts";
import { player } from "./audio.ts";

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
  return it.kind === "album" ? albumHash(it.path) : "#/media/item" + encodePathForHash(it.path);
}

/** progEl is a row's progress as a 4 px bar (docs/13 §10c). */
function progEl(p: Progress | undefined): HTMLElement {
  const el = h("span", { class: "prog" });
  if (p && p.dur > 0) el.style.setProperty("--p", `${Math.min(100, (p.pos / p.dur) * 100).toFixed(1)}%`);
  return el;
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

/** fact is the chip over the art's corner: a film's runtime, a show's seasons, a platform's games, an album's plays. */
function fact(it: MediaItem): string {
  if (it.kind === "album" && it.plays) return `${it.plays} ${it.plays === 1 ? "play" : "plays"}`;
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
    const img = h("img", { alt: "" }) as HTMLImageElement; // a thumbnail still being made answers 202: fetched as a listing tile's (I-374)
    lazyURL(img, src, missing);
    box.append(img);
  } else missing();
  if (chip && !box.contains(chip)) box.append(chip);
  // the viewer's state: watched a tick, started its progress along the foot (§10c)
  if (it.progress?.watched || it.watched) box.append(h("span", { class: "ov tr" }, icon("check")));
  else if (it.progress && it.progress.pos > 0) box.append(progEl(it.progress));
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
  const sorts = lib.kind === "music" ? ["title", "year", "added", "played"] : ["title", "year", "added"];
  let sort = session.get(`tr.media.sort.${lib.path}`, "title");
  let items: MediaItem[] = [];
  const buttons = sorts.map((s) => h("button", { type: "button", "aria-pressed": String(s === sort) }, s === "played" ? "Most played" : s[0]!.toUpperCase() + s.slice(1)));
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

// --- the tab's own search (§10d)

interface Chip {
  field: string;
  value: string;
}

/** splitQuery reads a query into its field chips and its free words. */
function splitQuery(q: string): { chips: Chip[]; words: string } {
  const chips: Chip[] = [];
  const words: string[] = [];
  for (const m of q.matchAll(/([a-z]+):"([^"]*)"|([a-z]+):(\S+)|(\S+)/gi)) {
    if (m[1]) chips.push({ field: m[1].toLowerCase(), value: m[2]! });
    else if (m[3]) chips.push({ field: m[3].toLowerCase(), value: m[4]! });
    else words.push(m[5]!);
  }
  return { chips, words: words.join(" ") };
}

function joinQuery(chips: Chip[], words: string): string {
  return [...chips.map((c) => (/\s/.test(c.value) || c.field !== "year" ? `${c.field}:"${c.value}"` : `${c.field}:${c.value}`)), words.trim()].filter(Boolean).join(" ");
}

function go(q: string): void {
  location.hash = "#/media" + (q ? "?q=" + encodeURIComponent(q) : "");
}

/** searchField is the tab's search: chips and words in one field, suggestions under it. */
function searchField(q: string): HTMLElement {
  const { chips, words } = splitQuery(q);
  const input = h("input", { type: "search", value: words, placeholder: "title, actor, director, genre, year…", "aria-label": "search the media", autocomplete: "off" }) as HTMLInputElement;
  const field = h("label", { class: "mfield" }, icon("search"));
  chips.forEach((c, i) => {
    field.append(h("button", { type: "button", class: "qchip", "aria-label": `remove ${c.field} ${c.value}`, onClick: (ev: Event) => { ev.preventDefault(); go(joinQuery(chips.filter((_, j) => j !== i), input.value)); } }, h("b", null, c.field), c.value, icon("close")));
  });
  field.append(input);
  const list = h("ul", { class: "msuggest", role: "listbox", hidden: true });
  const box = h("div", { class: "msearch" }, field, list);
  let opts: { li: HTMLElement; chip: Chip }[] = [];
  let sel = -1;
  let timer = 0;
  let asked = "";
  const mark = () => opts.forEach((o, i) => o.li.setAttribute("aria-selected", String(i === sel)));
  const pick = (c: Chip) => {
    const rest = input.value.trim().split(/\s+/).slice(0, -1).join(" ");
    go(joinQuery([...chips, c], rest));
  };
  const ask = () => {
    const last = input.value.trim().split(/\s+/).pop() ?? "";
    const query = joinQuery(chips, input.value);
    if (last.length < 2 || /:/.test(last)) {
      list.hidden = true;
      return;
    }
    asked = query;
    suggest(query).then(
      (res) => {
        if (asked !== query) return;
        clear(list);
        opts = [];
        sel = -1;
        const lw = last.toLowerCase();
        for (const g of res.groups) {
          list.append(h("li", { class: "mgroup" }, g.field.charAt(0).toUpperCase() + g.field.slice(1) + "s"));
          for (const v of g.values) {
            const at = v.value.toLowerCase().indexOf(lw);
            const label = at >= 0 ? [v.value.slice(0, at), h("mark", null, v.value.slice(at, at + lw.length)), v.value.slice(at + lw.length)] : [v.value];
            const counts = Object.entries(v.counts).map(([k, n]) => `${n} ${k === "movie" ? (n === 1 ? "film" : "films") : n === 1 ? k : k + "s"}`).join(" · ");
            const li = h("li", { class: "mopt", role: "option", "aria-selected": "false" }, h("span", { class: "mkind" }, g.field), h("span", null, ...label), h("span", { class: "mcount" }, counts));
            const chip = { field: g.field, value: v.value };
            li.addEventListener("mousedown", (ev) => {
              ev.preventDefault();
              pick(chip);
            });
            opts.push({ li, chip });
            list.append(li);
          }
        }
        list.hidden = opts.length === 0;
      },
      () => (list.hidden = true),
    );
  };
  input.addEventListener("input", () => {
    window.clearTimeout(timer);
    timer = window.setTimeout(ask, 150);
  });
  input.addEventListener("blur", () => window.setTimeout(() => (list.hidden = true), 150));
  input.addEventListener("keydown", (ev) => {
    if (ev.key === "ArrowDown" || ev.key === "ArrowUp") {
      if (!opts.length) return;
      ev.preventDefault();
      sel = (sel + (ev.key === "ArrowDown" ? 1 : opts.length - 1)) % opts.length;
      mark();
    } else if (ev.key === "Enter") {
      ev.preventDefault();
      if (sel >= 0 && !list.hidden) pick(opts[sel]!.chip);
      else go(joinQuery(chips, input.value));
    } else if (ev.key === "Escape") {
      list.hidden = true;
    } else if (ev.key === "Backspace" && input.value === "" && chips.length) {
      go(joinQuery(chips.slice(0, -1), ""));
    }
  });
  return box;
}

const KIND_NOUN: Record<string, [string, string]> = { movie: ["film", "films"], show: ["show", "shows"], album: ["album", "albums"], game: ["game", "games"] };

/** results draws a media search's hits, grouped by library (§10d). */
function results(root: HTMLElement, q: string): void {
  const note = h("div", { class: "mhits" }, "…");
  root.append(note);
  mediaSearch(q).then(
    (res) => {
      const byKind = new Map<string, number>();
      let n = 0;
      for (const lib of res.libs) for (const it of lib.items) {
        byKind.set(it.kind, (byKind.get(it.kind) ?? 0) + 1);
        n++;
      }
      if (n === 0) {
        note.remove();
        const { chips, words } = splitQuery(q);
        const drop = chips.length ? h("a", { href: "#/media?q=" + encodeURIComponent(joinQuery(chips.slice(0, -1), words)) }, "Drop " + chips[chips.length - 1]!.field) : null;
        root.append(h("div", { class: "empty" }, "No films, shows or albums match ", h("b", null, q), ". ", drop));
        return;
      }
      note.textContent = [`${n} ${n === 1 ? "match" : "matches"}`, ...[...byKind].map(([k, c]) => `${c} ${(KIND_NOUN[k] ?? [k, k + "s"])[c === 1 ? 0 : 1]}`)].join(" · ");
      for (const lib of res.libs) {
        const grid = h("div", { class: "posters" });
        for (const it of lib.items) grid.append(posterTile(it));
        root.append(h("section", { class: "lib " + lib.kind }, h("h2", null, basename(lib.path) || "/", " ", h("span", { class: "badge lib" }, lib.kind), h("span", { class: "count" }, String(lib.items.length))), grid));
      }
    },
    (e: unknown) => (note.textContent = "could not search: " + (e instanceof Error ? e.message : String(e))),
  );
}

// --- Continue watching (§10c)

function continueTile(it: MediaItem, drop: () => void): HTMLElement {
  const img = h("img", { alt: "" }) as HTMLImageElement;
  const cart = h("span", { class: "cart" }, img);
  const miss = () => (img.style.visibility = "hidden");
  if (it.kind === "track") lazyURL(img, thumbURL(dirname(it.path)), miss);
  else if (!frameOf(img, it, miss)) lazyURL(img, thumbURL(it.thumb || it.video || it.path), miss); // a frame the page grabs when none is stored
  if (it.next_up) cart.append(h("span", { class: "ov bl" }, "next up"));
  if (it.progress) cart.append(h("span", { class: "ov br" }, left(it.progress)), progEl(it.progress));
  let title = it.title;
  let sub = it.year ? String(it.year) : "";
  if (it.kind === "episode") {
    title = it.show ?? it.title;
    sub = `${episodeNo(it)} · ${it.next_up ? "next episode" : it.title}`;
  } else if (it.kind === "track") {
    title = it.album || it.title;
    sub = [it.artist, it.title].filter(Boolean).join(" · ");
  }
  const href = it.kind === "track" ? "#/media/now" : playHash(it.kind === "movie" && it.video ? it.video : it.path);
  const a = h("a", { href }, cart, h("span", { class: "ctitle", title }, title), h("span", { class: "csub" }, sub));
  if (it.kind === "track")
    a.addEventListener("click", () => player.play([{ path: it.path }], 0, resumeFrom(it.progress)));
  const file = it.kind === "movie" && it.video ? it.video : it.path;
  const act = (action: "watched" | "hide") => {
    const done = action === "watched" ? markWatched([file], true) : report({ path: file, action: "hide" });
    done.then(drop, (e: unknown) => toast(String(e instanceof Error ? e.message : e), "bad"));
  };
  const menu = h(
    "details",
    { class: "menu" },
    h("summary", { "aria-label": "more", title: "more" }, icon("more_vert")),
    h("div", { class: "menu-body" }, h("button", { type: "button", class: "menu-row", onClick: () => act("watched") }, h("span", null, "Mark watched")), h("button", { type: "button", class: "menu-row", onClick: () => act("hide") }, h("span", null, "Remove from Continue watching"))),
  );
  return h("div", { class: "ctile" }, a, menu);
}

function continueSection(root: HTMLElement): void {
  const row = h("div", { class: "crow" });
  const countEl = h("span", { class: "count" });
  const section = h("section", { class: "continue", hidden: true }, h("h2", null, "Continue watching", countEl), row);
  root.append(section);
  continueRow().then(
    (res) => {
      const draw = (items: MediaItem[]) => {
        clear(row);
        countEl.textContent = String(items.length);
        section.hidden = items.length === 0; // absent while empty
        items.forEach((it, i) => row.append(continueTile(it, () => draw(items.filter((_, j) => j !== i)))));
      };
      draw(res.items);
    },
    () => undefined, // no viewer, or an older node: the tab without the row
  );
}

function overview(root: HTMLElement, query: string): void {
  const q = new URLSearchParams(query).get("q") ?? "";
  root.append(h("section", null, searchField(q)));
  if (q) {
    results(root, q);
    return;
  }
  continueSection(root);
  media().then(
    (res) => {
      setLibraries(res.libs);
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
      const all = seasons.flatMap((s) => s.episodes);
      const first = seasons.find((s) => s.n > 0)?.episodes[0] ?? seasons[0]?.episodes[0];
      // Play is the viewer's next (§10c): the episode in progress or after the last watched
      const next = it.kind === "show" ? (all.find((e) => e.path === it.next) ?? first) : undefined;
      const prog = it.kind === "show" ? next?.progress : it.progress;
      const from = resumeFrom(prog);
      const target = it.kind === "show" ? next?.path : it.video ? it.path : undefined;
      const label = it.kind === "show" && next ? (from ? `Resume ${episodeNo(next)} · ${left(prog!)}` : `Play ${episodeNo(next)}`) : from ? `Resume · ${left(prog!)}` : prog?.watched ? "Play again" : "Play";
      const play = h("button", { type: "button", class: from ? "primary" : "" }, icon("play_arrow_fill"), label);
      if (target) play.addEventListener("click", () => (location.hash = playHash(target)));
      else (play as HTMLButtonElement).disabled = true;
      const folder = it.video === it.path ? dirname(it.path) : it.path;
      const actions = h("div", { class: "actions" }, play);
      if (from && target && it.kind !== "show") {
        const file = it.video!;
        actions.append(
          h("button", { type: "button", onClick: () => { void markWatched([file], false).then(() => (location.hash = playHash(target))); } }, icon("replay"), "Start over"),
        );
      }
      actions.append(h("a", { class: "btn", href: browseHash({ path: folder }) }, "Open folder"));
      body.append(actions);
      if (from && it.kind !== "show") body.append(progEl(prog));
      if (it.people?.length) {
        const people = h("div", { class: "people" });
        const dirs = it.directors ?? 0;
        it.people.forEach((p, i) =>
          people.append(i < dirs ? h("a", { class: "person", href: searchHash("director", p) }, p, h("span", { class: "small dim" }, "director")) : h("a", { class: "person", href: searchHash("actor", p) }, p)),
        );
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
            const title = h("td", { class: "title" });
            if (e.progress?.watched) title.append(icon("check"));
            title.append(e.title);
            if (e.plot) title.append(h("div", { class: "plot small dim" }, e.plot));
            if (e.progress && !e.progress.watched && e.progress.pos > 0) title.append(progEl(e.progress));
            const row = h("tr", { class: e.progress?.watched ? "watched" : "" }, h("td", { class: "no" }, episodeNo(e)), thumb, title, h("td", { class: "dur" }, duration(e.runtime)));
            row.addEventListener("click", () => (location.hash = playHash(e.path)));
            tbody.append(row);
          }
        };
        let shown = 0;
        seasons.forEach((s, i) => {
          const b = h("button", { type: "button", "aria-pressed": "false" }, s.title);
          b.addEventListener("click", () => {
            shown = i;
            show(i);
            seasonLabel.textContent = seasons[i]!.title;
          });
          strip.append(b);
        });
        // the season menu marks the shown season watched or unwatched (§10c)
        const seasonLabel = h("span", null, "");
        const mark = (watched: boolean) => {
          void markWatched(seasons[shown]!.episodes.map((e) => e.path), watched).then(
            () => {
              clear(root);
              detail(root, path);
            },
            (e: unknown) => toast(String(e instanceof Error ? e.message : e), "bad"),
          );
        };
        const menu = h("details", { class: "menu" }, h("summary", null, seasonLabel), h("div", { class: "menu-body" }, h("button", { type: "button", class: "menu-row", onClick: () => mark(true) }, h("span", null, "Mark season watched")), h("button", { type: "button", class: "menu-row", onClick: () => mark(false) }, h("span", null, "Mark season unwatched"))));
        actions.insertBefore(menu, actions.lastElementChild);
        body.append(strip, table);
        shown = Math.max(0, next ? seasons.findIndex((s) => s.episodes.some((e) => e.path === next.path)) : seasons.findIndex((s) => s.n > 0));
        show(shown);
        seasonLabel.textContent = seasons[shown]?.title ?? "";
      }
      section.append(h("div", { class: "hero-body" }, poster, body));
      if (noArt) hide();
      root.append(section);
    },
    (e: unknown) => root.append(h("div", { class: "note" }, "could not open: " + (e instanceof Error ? e.message : String(e)))),
  );
}

export function render(root: HTMLElement, rest: string, query = ""): () => void {
  if (rest.startsWith("/item/")) detail(root, rest.slice("/item".length));
  else overview(root, query);
  return () => undefined;
}
