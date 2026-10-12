// The Media tab (docs/13 §10, I-212): one section per library folder, films and shows by
// poster and albums by cover, and a detail page with the backdrop, facts, plot, people
// and, for a show, its seasons and episodes. The node lists the structure live and
// fills in the catalogue's metadata; a film without sidecars shows its cleaned name and
// the browser-made frame. Routes: #/media and #/media/item/<path>.

import { clear, h, svg } from "./dom.ts";
import { fileURL, media, mediaItem, thumbURL, type MediaItem, type MediaLibrary } from "./api.ts";
import { lazy } from "./thumbs.ts";
import { basename, browseHash, count, dirname, encodePathForHash, fileHash, session } from "./util.ts";

const NOUN: Record<string, [string, string]> = { movies: ["film", "films"], tv: ["show", "shows"], music: ["album", "albums"] };

function glyph(kind: string): SVGElement {
  const d =
    kind === "album"
      ? "M6 12.5V3.5l7-1.5v9 M2 12.5a2 2 0 1 0 4 0a2 2 0 1 0 -4 0 M9 11a2 2 0 1 0 4 0a2 2 0 1 0 -4 0"
      : kind === "episode"
        ? "M4 2.5l9 5.5-9 5.5z"
        : "M1.5 3.5h13v9h-13z M4 3.5v9 M12 3.5v9 M1.5 6h2.5 M1.5 10h2.5 M12 6h2.5 M12 10h2.5";
  return svg("svg", { class: "icon", viewBox: "0 0 16 16", "aria-hidden": "true" }, svg("path", { d }));
}

function duration(min: number | undefined): string {
  if (!min) return "";
  return min >= 60 ? `${Math.floor(min / 60)} h ${String(min % 60).padStart(2, "0")}` : `${min} min`;
}

function itemHash(it: MediaItem): string {
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

function sub(it: MediaItem): string {
  if (it.kind === "album") return [it.artist, it.year].filter(Boolean).join(" · ");
  const parts: string[] = [];
  if (it.year) parts.push(String(it.year));
  if (it.kind === "show" && it.children) parts.push(`${it.children} ${it.children === 1 ? "season" : "seasons"}`);
  if (it.kind === "movie") parts.push(it.runtime ? duration(it.runtime) : it.plot ? "" : "no sidecar");
  return parts.filter(Boolean).join(" · ");
}

function posterTile(it: MediaItem): HTMLElement {
  const veiled = it.nsfw && !session.get("tr.revealed", false);
  const box = h("span", { class: "art" });
  const a = h("a", { class: "poster" + (veiled ? " nsfw" : ""), href: itemHash(it) }, box, h("span", { class: "ptitle", title: it.title }, it.title), h("span", { class: "pyear" }, veiled ? "nsfw · tap to show" : sub(it)));
  const missing = () => {
    clear(box);
    box.append(glyph(it.kind));
    a.classList.add("missing");
  };
  const src = art(it);
  const frame = h("img", { alt: "" }) as HTMLImageElement;
  if (frameOf(frame, it, missing)) box.append(frame);
  else if (src) {
    const img = h("img", { alt: "", loading: "lazy", src }) as HTMLImageElement;
    img.addEventListener("error", missing);
    box.append(img);
  } else missing();
  if (veiled) {
    a.addEventListener("click", (ev) => {
      if (!a.classList.contains("nsfw")) return;
      ev.preventDefault();
      session.set("tr.revealed", true);
      a.classList.remove("nsfw");
      (a.querySelector(".pyear") as HTMLElement).textContent = sub(it);
    });
  }
  return a;
}

function librarySection(lib: MediaLibrary): HTMLElement {
  const [one, many] = NOUN[lib.kind] ?? ["item", "items"];
  const countEl = h("span", { class: "count" }, "…");
  const section = h("section", { class: "lib " + lib.kind }, h("h2", null, basename(lib.path) || "/", " ", h("span", { class: "badge lib" }, lib.kind), countEl));
  const grid = h("div", { class: "posters" });
  const sorts = ["title", "year", "added"];
  let sort = session.get(`tr.media.sort.${lib.path}`, "title");
  const pressed = new Set<string>();
  let items: MediaItem[] = [];
  const buttons = sorts.map((s) => h("button", { type: "button", "aria-pressed": String(s === sort) }, s[0]!.toUpperCase() + s.slice(1)));
  const chips = h("div", { class: "chips" });
  const draw = () => {
    clear(grid);
    const shown = pressed.size === 0 ? items : items.filter((it) => (it.genres ?? []).some((g) => pressed.has(g.toLowerCase())));
    for (const it of shown) grid.append(posterTile(it));
  };
  const load = () => {
    media(lib.path, sort).then(
      (res) => {
        items = res.items;
        clear(countEl);
        countEl.append(`${count(res.lib.count)} ${res.lib.count === 1 ? one : many}${res.lib.count > res.items.length ? ` (first ${res.items.length})` : ""} · `, h("a", { href: browseHash({ path: lib.path }) }, "open folder"));
        clear(chips);
        for (const g of res.lib.genres ?? []) {
          const chip = h("button", { type: "button", class: "chip", "aria-pressed": String(pressed.has(g)) }, g);
          chip.addEventListener("click", () => {
            if (pressed.has(g)) pressed.delete(g);
            else pressed.add(g);
            chip.setAttribute("aria-pressed", String(pressed.has(g)));
            draw();
          });
          chips.append(chip);
        }
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
  section.append(h("div", { class: "toolbar" }, h("span", { class: "seg", role: "group", "aria-label": "sort" }, ...buttons), chips), grid);
  load();
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
            h("div", { class: "empty" }, "No library yet: open a folder in Files, choose its menu and set ", h("b", null, "Library"), " to movies, tv or music."),
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
  return `S${two(e.season ?? 0)}E${two(e.episode ?? 0)}` + (e.episode_to && e.episode_to !== e.episode ? `E${two(e.episode_to)}` : "");
}

function detail(root: HTMLElement, path: string): void {
  mediaItem(path).then(
    (it) => {
      const backdrop = it.fanart ? fileURL(it.fanart) : it.poster ? fileURL(it.poster) : "";
      const cls = "hero" + (it.fanart ? "" : it.poster ? " poster-only" : " no-art");
      const section = h("section", { class: cls });
      if (backdrop) section.append(h("img", { class: "backdrop", alt: "", src: backdrop }));
      const posterSrc = it.poster ? fileURL(it.poster) : it.video ? thumbURL(it.video) : it.kind === "album" ? thumbURL(it.path) : "";
      const poster = h("img", { class: "hero-poster", alt: "" }) as HTMLImageElement;
      const hide = () => (poster.style.visibility = "hidden");
      if (!frameOf(poster, it, hide)) {
        if (posterSrc) poster.src = posterSrc;
        poster.addEventListener("error", hide);
      }
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
      const play = h("button", { type: "button" }, it.kind === "show" ? (first ? `▶ Play ${episodeNo(first)}` : "▶ Play") : "▶ Play");
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
