// The viewer (docs/13 §10a, §10b, I-414): a modal over the Media tab, on the media ground
// in the dark tokens. #/media/play/<path> is a film or an episode: Play asks for
// fullscreen in the click's task and leaving fullscreen lands here, the facts folded,
// back to the page it came from. #/media/album/<path> is an album, never its folder;
// #/media/now is the now-playing view. The page under the modal is never re-rendered, so
// back returns to it as it was.

import { clear, h } from "./dom.ts";
import { icon, type IconName } from "./icons.ts";
import { fileURL, media, mediaItem, report, tags, thumbURL, type MediaItem, type Progress } from "./api.ts";
import { duration, trackNo } from "./album.ts";
import { player, type Track } from "./audio.ts";
import { basename, decodeHashPath, dirname, encodePathForHash, mmss } from "./util.ts";
import { hms, resumeFrom, watch } from "./watch.ts";

/** isOverlay reports a route the viewer draws over the page. */
export function isOverlay(hash: string): boolean {
  return /^#\/media\/(play|album)\/|^#\/media\/now$/.test(hash.split("?")[0] ?? "");
}

export function playHash(path: string): string {
  return "#/media/play" + encodePathForHash(path);
}

export function albumHash(path: string): string {
  return "#/media/album" + encodePathForHash(path);
}

export function searchHash(field: string, value: string): string {
  return "#/media?q=" + encodeURIComponent(`${field}:"${value}"`);
}

let libs: { path: string; kind: string }[] | null = null;

/** setLibraries hands the viewer the Media tab's libraries, for the back button's name. */
export function setLibraries(l: { path: string; kind: string }[]): void {
  libs = l;
}

/** libName is the library a path lies under, as the back button names it. */
async function libName(p: string): Promise<string> {
  if (!libs) {
    try {
      libs = (await media()).libs;
    } catch {
      libs = [];
    }
  }
  const lib = libs.filter((l) => p === l.path || p.startsWith(l.path + "/")).sort((a, b) => b.path.length - a.path.length)[0];
  const n = lib ? basename(lib.path) : "Media";
  return n.charAt(0).toUpperCase() + n.slice(1);
}

interface Open {
  hash: string;
  el: HTMLElement;
  close: () => void;
}

let current: Open | null = null;
let backTo = "#/media";

/** open draws the route's view over the page; under is the page's route. */
export function open(hash: string, under: string): void {
  if (current?.hash === hash) return;
  current?.close();
  current = null;
  backTo = under;
  const where = (hash.split("?")[0] ?? "").replace(/^#\/media\//, "");
  const slash = where.indexOf("/");
  const kind = slash < 0 ? where : where.slice(0, slash);
  const path = decodeHashPath(slash < 0 ? "" : where.slice(slash));
  const el = h("div", { class: "mviewer theme-dark", role: "dialog", "aria-modal": "true" });
  document.body.append(el);
  document.body.classList.add("modal-open");
  const o: Open = { hash, el, close: () => undefined };
  current = o;
  const onKey = (ev: KeyboardEvent) => {
    if (ev.key === "Escape" && !document.fullscreenElement) back();
  };
  document.addEventListener("keydown", onKey);
  let inner: () => void = () => undefined;
  o.close = () => {
    inner();
    document.removeEventListener("keydown", onKey);
    if (document.fullscreenElement && el.contains(document.fullscreenElement)) void document.exitFullscreen().catch(() => undefined);
    el.remove();
    document.body.classList.remove("modal-open", "now-open");
  };
  if (kind === "play") inner = video(el, path);
  else if (kind === "album") inner = album(el, path);
  else inner = nowPlaying(el);
}

/** close takes the viewer down, leaving the page under it. */
export function close(): void {
  current?.close();
  current = null;
}

function back(): void {
  location.hash = backTo;
}

function iconBtn(name: IconName, label: string, onClick: () => void, cls = ""): HTMLButtonElement {
  return h("button", { type: "button", class: "icon-btn" + (cls ? " " + cls : ""), "aria-label": label, title: label, onClick }, icon(name)) as HTMLButtonElement;
}

function bar(label: Promise<string>, title: string, extra?: HTMLElement): HTMLElement {
  const backLink = h("a", { class: "back", href: backTo }, icon("arrow_back"), "Back");
  void label.then((n) => backLink.replaceChildren(icon("arrow_back"), n));
  return h("div", { class: "mbar" }, backLink, h("span", { class: "mtitle" }, title), extra ?? iconBtn("close", "close", back));
}

function prog(p: Progress | undefined): HTMLElement {
  const el = h("span", { class: "prog" });
  if (p && p.dur > 0) el.style.setProperty("--p", `${Math.min(100, (p.pos / p.dur) * 100).toFixed(1)}%`);
  return el;
}

/** links are genres and people as links into the tab's search (docs/13 §10d). */
function links(it: MediaItem): HTMLElement[] {
  const out: HTMLElement[] = [];
  if (it.genres?.length) out.push(h("div", { class: "mlinks" }, ...it.genres.map((g) => h("a", { class: "genre", href: searchHash("genre", g) }, g.toLowerCase()))));
  const dirs = it.directors ?? 0;
  if (it.people?.length)
    out.push(
      h(
        "div",
        { class: "mlinks" },
        ...it.people.map((p, i) => (i < dirs ? h("a", { class: "person", href: searchHash("director", p) }, p, h("span", { class: "small dim" }, "director")) : h("a", { class: "person", href: searchHash("actor", p) }, p))),
      ),
    );
  return out;
}

function episodeNo(e: MediaItem): string {
  const two = (n: number) => String(n).padStart(2, "0");
  return `S${two(e.season ?? 0)}E${two(e.episode ?? 0)}`;
}

function minutes(min: number | undefined): string {
  if (!min) return "";
  return min >= 60 ? `${Math.floor(min / 60)} h ${String(min % 60).padStart(2, "0")}` : `${min} min`;
}

// --- a film or an episode (§10a)

function video(root: HTMLElement, path: string): () => void {
  let disposed = false;
  let unwatch: (() => void) | null = null;
  const v = h("video", { controls: true, playsinline: true, preload: "metadata" }) as HTMLVideoElement;
  const screen = h("div", { class: "mscreen" }, v);
  let idle = 0;
  const wake = () => {
    screen.classList.remove("idle");
    window.clearTimeout(idle);
    idle = window.setTimeout(() => screen.classList.add("idle"), 3000);
  };
  screen.addEventListener("pointermove", wake);
  const onFs = () => {
    screen.classList.toggle("fs", document.fullscreenElement === screen);
    wake();
  };
  document.addEventListener("fullscreenchange", onFs);
  const fullscreen = () => {
    if (screen.requestFullscreen) void screen.requestFullscreen().catch(() => undefined); // refused: plays in the viewer
    else (v as HTMLVideoElement & { webkitEnterFullscreen?: () => void }).webkitEnterFullscreen?.();
  };
  const fsName = h("span", { class: "fs-name" });
  const fsTop = h("div", { class: "fs-top" }, iconBtn("arrow_back", "leave fullscreen", () => void document.exitFullscreen().catch(() => undefined)), fsName);
  screen.append(fsTop, iconBtn("fullscreen", "fullscreen", fullscreen, "fs-btn"));
  const facts = h("details", { class: "mfacts" }) as HTMLDetailsElement;
  try {
    facts.open = localStorage.getItem("tr.viewer.facts") === "1";
  } catch {
    // storage may be unavailable
  }
  facts.addEventListener("toggle", () => {
    try {
      localStorage.setItem("tr.viewer.facts", facts.open ? "1" : "0");
    } catch {
      // ignore
    }
  });
  const body = h("div", { class: "mbody" }, screen, facts);
  const backdrop = h("img", { class: "mbackdrop", alt: "" }) as HTMLImageElement;
  backdrop.addEventListener("error", () => backdrop.remove());
  const title = h("span");
  root.append(backdrop, bar(libName(path), "", undefined), body);
  root.querySelector(".mtitle")?.append(title);
  player.pause(); // a video pauses the music, as the file view does

  let eps: MediaItem[] = [];
  let item: MediaItem | null = null;
  let cur: { path: string; video: string; ep?: MediaItem } | null = null;
  let upnext: HTMLElement | null = null;
  let cancelled = false;
  let prompt: HTMLElement | null = null;
  const nextEp = () => (cur?.ep ? eps[eps.findIndex((e) => e.path === cur!.ep!.path) + 1] : undefined);
  const prevEp = () => (cur?.ep ? eps[eps.findIndex((e) => e.path === cur!.ep!.path) - 1] : undefined);

  const ask = (...kids: (Node | string)[]) => {
    prompt?.remove();
    prompt = h("div", { class: "mprompt" }, ...kids);
    screen.append(prompt);
  };
  const dismiss = () => {
    prompt?.remove();
    prompt = null;
  };
  const start = (from: number) => {
    dismiss();
    v.currentTime = from;
    fullscreen();
    void v.play().catch(() => ask(h("span", { class: "mq" }, cur?.ep?.title ?? item?.title ?? ""), h("span", { class: "acts" }, h("button", { type: "button", class: "primary", onClick: () => start(v.currentTime) }, icon("play_arrow_fill"), "Play"))));
  };

  const drawFacts = (it: MediaItem, ep?: MediaItem) => {
    clear(facts);
    const sum = h("summary");
    if (ep) {
      sum.append(h("h1", null, ep.title), h("span", null, it.title), h("span", null, episodeNo(ep)));
      if (ep.runtime) sum.append(h("span", null, minutes(ep.runtime)));
      if (ep.aired) sum.append(h("span", null, "aired " + ep.aired));
    } else {
      sum.append(h("h1", null, it.title));
      for (const f of [it.year ? String(it.year) : "", minutes(it.runtime), it.rating ? `★ ${it.rating}` : "", it.mpaa ?? ""]) if (f) sum.append(h("span", null, f));
    }
    const more = h("div", { class: "mmore" });
    const plot = ep?.plot ?? it.plot;
    if (plot) more.append(h("p", { class: "plot" }, plot));
    more.append(...links(ep?.people?.length ? { ...it, people: ep.people, directors: ep.directors } : it));
    const file = cur?.video ?? "";
    more.append(h("div", { class: "small dim" }, file, " · ", h("a", { href: "#/browse" + encodePathForHash(dirname(file)) }, "open folder")));
    facts.append(sum, more);
    fsName.replaceChildren(ep ? it.title : it.title, h("span", { class: "small" }, ep ? `${episodeNo(ep)} · ${ep.title}` : [it.year, minutes(it.runtime)].filter(Boolean).join(" · ")));
    title.textContent = ep ? `${it.title} · ${episodeNo(ep)}` : [it.title, it.year].filter(Boolean).join(" · ");
  };

  const episodesUI = h("div");
  const drawEpisodes = () => {
    clear(episodesUI);
    const ep = cur?.ep;
    if (!item || !ep) return;
    const next = nextEp();
    const prev = prevEp();
    if (next || prev) {
      const nimg = h("img", { alt: "" }) as HTMLImageElement;
      if (next) nimg.src = thumbURL(next.thumb || next.path);
      nimg.addEventListener("error", () => (nimg.style.visibility = "hidden"));
      const nacts = h("span", { class: "nacts" });
      if (prev) nacts.append(iconBtn("skip_previous_fill", "previous episode", () => switchTo(prev, 0)));
      if (next) nacts.append(h("button", { type: "button", class: "primary", onClick: () => switchTo(next, 0) }, icon("skip_next_fill"), "Next"));
      episodesUI.append(h("div", { class: "mnext" }, nimg, h("span", { class: "nname" }, h("span", null, next ? `Next · ${episodeNo(next)}` : "Last episode"), h("span", { class: "small" }, next ? [next.title, minutes(next.runtime)].filter(Boolean).join(" · ") : "")), nacts));
    }
    const season = (item.seasons ?? []).find((s) => s.episodes.some((e) => e.path === ep.path));
    if (!season) return;
    const watched = season.episodes.filter((e) => e.progress?.watched).length;
    const tbody = h("tbody");
    for (const e of season.episodes) {
      const t = h("td", { class: "title" });
      if (e.progress?.watched) t.append(icon("check"));
      t.append(e.title);
      if (e.progress && !e.progress.watched && e.progress.pos > 0) t.append(prog(e.progress));
      const img = h("img", { alt: "", loading: "lazy", src: thumbURL(e.thumb || e.path) }) as HTMLImageElement;
      img.addEventListener("error", () => (img.style.visibility = "hidden"));
      const row = h("tr", { class: [e.path === ep.path ? "playing" : "", e.progress?.watched ? "watched" : ""].filter(Boolean).join(" ") }, h("td", { class: "no" }, episodeNo(e)), h("td", { class: "thumb" }, img), t, h("td", { class: "dur" }, minutes(e.runtime)));
      row.addEventListener("click", () => switchTo(e, resumeFrom(e.progress)));
      tbody.append(row);
    }
    episodesUI.append(h("details", { class: "mfold" }, h("summary", null, "Episodes", h("span", { class: "small" }, `${season.title} · ${watched} of ${season.episodes.length} watched`)), h("table", { class: "episodes" }, tbody)));
  };
  body.append(episodesUI);

  /** load puts a file on the screen; from is where it starts (0: the beginning). */
  const load = (file: string, ep: MediaItem | undefined, resume: number, autoplay: boolean) => {
    unwatch?.();
    cancelled = false;
    upnext?.remove();
    upnext = null;
    cur = { path: ep?.path ?? path, video: file, ep };
    v.src = fileURL(file);
    unwatch = watch(v, file, { positions: () => true, plays: false });
    drawFacts(item!, ep);
    drawEpisodes();
    if (resume > 0) {
      v.currentTime = resume;
      ask(
        h("span", { class: "mq" }, `Resume from ${hms(resume)}?`),
        prog(ep?.progress ?? item?.progress),
        h("span", { class: "acts" }, h("button", { type: "button", class: "primary", onClick: () => start(resume) }, icon("play_arrow_fill"), "Resume"), h("button", { type: "button", onClick: () => start(0) }, icon("replay"), "Start from the beginning")),
      );
      (prompt?.querySelector("button.primary") as HTMLButtonElement | null)?.focus();
    } else if (autoplay) start(0);
  };
  const switchTo = (e: MediaItem, resume: number) => {
    history.replaceState(null, "", playHash(e.path)); // in place: the screen keeps fullscreen
    if (current) current.hash = playHash(e.path);
    load(e.path, e, resume, true);
  };

  v.addEventListener("timeupdate", () => {
    const next = nextEp();
    if (!next || cancelled || upnext || !Number.isFinite(v.duration) || v.duration - v.currentTime > 30) return;
    let n = 10;
    const label = h("span", { class: "ulabel" }, `Up next in ${n} s`);
    const nimg = h("img", { alt: "", src: thumbURL(next.thumb || next.path) });
    upnext = h("div", { class: "upnext" }, nimg, label, h("span", { class: "uname" }, `${episodeNo(next)} · ${next.title}`), h("span", { class: "uacts" }, h("button", { type: "button", class: "primary", onClick: () => switchTo(next, 0) }, icon("play_arrow_fill"), "Play now"), h("button", { type: "button", onClick: () => { cancelled = true; upnext?.remove(); } }, "Cancel")));
    screen.append(upnext);
    const tick = window.setInterval(() => {
      if (disposed || cancelled || !upnext?.isConnected) return window.clearInterval(tick);
      if (--n <= 0) {
        window.clearInterval(tick);
        switchTo(next, 0);
        return;
      }
      label.textContent = `Up next in ${n} s`;
    }, 1000);
  });
  v.addEventListener("ended", () => {
    const next = nextEp();
    if (next && !cancelled) return switchTo(next, 0);
    if (document.fullscreenElement === screen) void document.exitFullscreen().catch(() => undefined);
    const backLabel = item?.kind === "show" ? `Back to ${item.title}` : "Back";
    ask(icon("check"), h("span", { class: "mq" }, "Watched"), h("span", { class: "acts" }, h("button", { type: "button", onClick: () => start(0) }, icon("replay"), "Play again"), h("a", { class: "btn primary", href: backTo }, backLabel)));
  });
  v.addEventListener("error", () => {
    const file = cur?.video ?? "";
    ask(icon("error"), h("span", { class: "mq" }, "This browser cannot play this file"), h("span", { class: "small" }, basename(file)), h("span", { class: "acts" }, h("a", { class: "btn", href: fileURL(file, true) }, "Download")));
  });

  mediaItem(path).then(
    (it) => {
      if (disposed) return;
      item = it;
      if (it.fanart || it.poster) backdrop.src = fileURL(it.fanart || it.poster!);
      else backdrop.remove();
      if (it.kind === "show") {
        eps = (it.seasons ?? []).filter((s) => s.n > 0).flatMap((s) => s.episodes);
        const all = (it.seasons ?? []).flatMap((s) => s.episodes);
        const ep = all.find((e) => e.path === path) ?? all.find((e) => e.path === it.next) ?? eps[0];
        if (!ep) return ask(h("span", { class: "mq" }, "No episode to play"));
        load(ep.path, ep, resumeFrom(ep.progress), true);
      } else {
        const file = it.video ?? path;
        load(file, undefined, resumeFrom(it.progress), true);
      }
    },
    (e: unknown) => ask(h("span", { class: "mq" }, "could not open: " + (e instanceof Error ? e.message : String(e)))),
  );
  return () => {
    disposed = true;
    unwatch?.();
    document.removeEventListener("fullscreenchange", onFs);
    window.clearTimeout(idle);
    v.pause();
    v.removeAttribute("src");
    v.load();
  };
}

// --- an album (§10b)

function album(root: HTMLElement, path: string): () => void {
  let disposed = false;
  const backdrop = h("img", { class: "mbackdrop", alt: "", src: thumbURL(path) }) as HTMLImageElement;
  backdrop.addEventListener("error", () => backdrop.remove());
  const cover = h("img", { class: "cover", alt: "", src: thumbURL(path) }) as HTMLImageElement;
  cover.addEventListener("error", () => (cover.style.visibility = "hidden"));
  const head = h("div", null, h("h1", null, basename(path)));
  const tbody = h("tbody");
  const body = h("div", { class: "mbody" }, h("div", { class: "malbum" }, cover, head), h("table", { class: "tracks" }, tbody));
  root.append(backdrop, bar(libName(path), ""), body);
  let unsub: () => void = () => undefined;
  mediaItem(path).then(
    (it) => {
      if (disposed) return;
      const names = (it.tracks ?? []).slice().sort((a, b) => trackNo(a) - trackNo(b) || a.localeCompare(b, undefined, { numeric: true }));
      const tracks: Track[] = names.map((n) => ({ path: path + "/" + n }));
      const tp = new Map((it.track_play ?? []).map((t) => [t.name, t]));
      const durs = new Map<number, number>();
      const titles = new Map<number, string>();
      const rows: HTMLTableRowElement[] = [];
      const first = names.findIndex((n) => resumeFrom(tp.get(n)?.progress) > 0);
      const playAt = (i: number) => player.play(tracks, i, resumeFrom(tp.get(names[i]!)?.progress));
      const meta = h("div", { class: "meta" });
      const drawMeta = () => {
        const parts = [it.year ? String(it.year) : "", (it.genres ?? []).join(" · ").toLowerCase(), `${names.length} ${names.length === 1 ? "track" : "tracks"}` + (durs.size === names.length && names.length ? ` · ${mmss([...durs.values()].reduce((s, d) => s + d, 0))}` : ""), it.plays ? `played ${it.plays} ${it.plays === 1 ? "time" : "times"}` : ""];
        meta.replaceChildren(...parts.filter(Boolean).map((p) => h("span", null, p)));
      };
      const actions = h(
        "div",
        { class: "actions" },
        h("button", { type: "button", class: "primary", onClick: () => playAt(first >= 0 ? first : 0) }, icon("play_arrow_fill"), first >= 0 ? `Resume ${titles.get(first) ?? names[first]!.replace(/\.[^.]+$/, "")} · ${hms(resumeFrom(tp.get(names[first]!)?.progress))}` : "Play"),
        h("button", { type: "button", onClick: () => { if (!player.shuffle) player.toggleShuffle(); player.play(tracks, Math.floor(Math.random() * tracks.length)); } }, icon("shuffle"), "Shuffle"),
        h("a", { class: "btn", href: "#/browse" + encodePathForHash(path) }, "Open folder"),
      );
      const artist = it.artist ? h("a", { class: "artist", href: searchHash("artist", it.artist) }, it.artist) : h("span", { class: "artist" });
      head.replaceChildren(h("h1", null, it.title), artist, meta, actions);
      if (it.plot) body.insertBefore(h("details", { class: "mfacts" }, h("summary", null, h("span", null, `${it.artist ?? it.title} · about`)), h("div", { class: "mmore" }, h("p", { class: "plot" }, it.plot))), body.lastElementChild);
      const drawRow = (i: number) => {
        const n = names[i]!;
        const t = tp.get(n);
        const playing = player.isCurrent(tracks[i]!.path);
        const tc = h("td", { class: "title" }, titles.get(i) ?? n.replace(/\.[^.]+$/, ""));
        const from = resumeFrom(t?.progress);
        if (from > 0) tc.append(h("span", { class: "small dim" }, ` Resume from ${hms(from)}`), prog(t?.progress));
        rows[i]!.className = playing ? "playing" : "";
        rows[i]!.replaceChildren(h("td", { class: "no" }, playing ? icon("equalizer") : String(trackNo(n) || i + 1)), tc, h("td", { class: "plays" }, t?.plays ? String(t.plays) : ""), h("td", { class: "dur" }, mmss(durs.get(i))));
      };
      names.forEach((_, i) => {
        const tr = h("tr") as HTMLTableRowElement;
        tr.addEventListener("click", () => playAt(i));
        rows.push(tr);
        tbody.append(tr);
        drawRow(i);
      });
      drawMeta();
      unsub = player.subscribe(() => rows.forEach((_, i) => drawRow(i)));
      // titles and lengths arrive one track at a time
      void (async () => {
        for (let i = 0; i < names.length && !disposed; i++) {
          try {
            const g = await tags(tracks[i]!.path);
            if (g.title) titles.set(i, g.title);
            tracks[i]!.title = g.title || undefined;
            tracks[i]!.artist = g.artist;
            tracks[i]!.album = g.album;
          } catch {
            // a track whose tags cannot be read keeps its name
          }
          const d = await duration(fileURL(tracks[i]!.path));
          if (d !== undefined) {
            durs.set(i, d);
            tracks[i]!.dur = d;
          }
          if (!disposed) {
            drawRow(i);
            drawMeta();
          }
        }
      })();
    },
    (e: unknown) => head.append(h("p", { class: "plot" }, "could not open: " + (e instanceof Error ? e.message : String(e)))),
  );
  return () => {
    disposed = true;
    unsub();
  };
}

// --- the now-playing view (§10b)

function nowPlaying(root: HTMLElement): () => void {
  document.body.classList.add("now-open");
  const cover = h("img", { class: "cover", alt: "" }) as HTMLImageElement;
  cover.addEventListener("error", () => (cover.style.visibility = "hidden"));
  const backdrop = h("img", { class: "mbackdrop", alt: "" }) as HTMLImageElement;
  const ntitle = h("div", { class: "ntitle" });
  const nsub = h("div", { class: "nsub" });
  const cur = h("span", { class: "time" }, "0:00");
  const end = h("span", { class: "time" }, "—");
  const seek = h("input", { type: "range", min: 0, max: 0, value: 0, "aria-label": "seek" }) as HTMLInputElement;
  seek.addEventListener("input", () => player.seek(Number(seek.value)));
  const playBtn = iconBtn("play_arrow_fill", "play", () => player.toggle(), "big");
  const shuffle = iconBtn("shuffle", "shuffle", () => player.toggleShuffle());
  const repeat = iconBtn("repeat", "repeat", () => player.cycleRepeat());
  const queue = h("ol");
  const queueSum = h("span", { class: "small" });
  const albumLink = h("a", { class: "btn" }, "Album");
  const label = h("span", { class: "mtitle" });
  const mbar = h("div", { class: "mbar" }, h("a", { class: "back", href: backTo }, icon("arrow_back"), "Back"), label, albumLink);
  root.append(
    backdrop,
    mbar,
    h(
      "div",
      { class: "mbody" },
      h(
        "div",
        { class: "mnow" },
        cover,
        h("div", null, ntitle, nsub),
        h("div", { class: "seek" }, cur, seek, end),
        h("div", { class: "transport" }, iconBtn("skip_previous_fill", "previous", () => player.prev()), playBtn, iconBtn("skip_next_fill", "next", () => player.next())),
        h("div", { class: "modes" }, shuffle, repeat),
        h("details", { class: "mfold" }, h("summary", null, "Up next", queueSum), queue),
      ),
    ),
  );
  let drawnFor = "";
  const draw = () => {
    const t = player.current();
    if (!t) {
      back();
      return;
    }
    if (drawnFor !== t.path) {
      drawnFor = t.path;
      cover.style.visibility = "";
      cover.src = backdrop.src = thumbURL(t.path, t.root);
      albumLink.setAttribute("href", albumHash(dirname(t.path)));
      void libName(t.path).then((n) => mbar.querySelector("a.back")?.replaceChildren(icon("arrow_back"), n));
    }
    ntitle.textContent = t.title ?? basename(t.path);
    nsub.textContent = [t.artist, t.album].filter(Boolean).join(" · ");
    label.textContent = t.album ? `playing from ${t.album}` : "";
    playBtn.setAttribute("aria-label", player.playing ? "pause" : "play");
    playBtn.replaceChildren(icon(player.playing ? "pause_fill" : "play_arrow_fill"));
    shuffle.setAttribute("aria-pressed", String(player.shuffle));
    repeat.setAttribute("aria-pressed", String(player.repeat !== "off"));
    const rest = player.queue.slice(player.index + 1);
    queueSum.textContent = `${rest.length} ${rest.length === 1 ? "track" : "tracks"}`;
    queue.replaceChildren(...rest.map((q) => h("li", null, q.title ?? basename(q.path))));
  };
  const tick = window.setInterval(() => {
    const { cur: c, dur } = player.position();
    if (document.activeElement !== seek) {
      seek.max = String(Math.round(dur));
      seek.value = String(Math.round(c));
    }
    cur.textContent = mmss(c);
    end.textContent = mmss(dur);
  }, 500);
  const unsub = player.subscribe(draw);
  draw();
  return () => {
    unsub();
    window.clearInterval(tick);
  };
}

/** markWatched marks files watched or unwatched for the viewer (a row's or a season's menu). */
export function markWatched(paths: string[], watched: boolean): Promise<void> {
  return report({ paths, action: watched ? "watched" : "unwatched" });
}

