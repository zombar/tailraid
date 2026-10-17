// Audio playback (docs/13 §6): a queue, two <audio> elements (the next track preloads
// in the second and is swapped in on `ended`), the now-playing bar the router never
// touches, the queue panel, Media Session, keyboard, and the current track and position
// in sessionStorage so a reload resumes where it was.

import { h } from "./dom.ts";
import { icon, type IconName } from "./icons.ts";
import { fileURL, tags, thumbURL, type Src } from "./api.ts";
import { fetchThumb } from "./thumbs.ts";
import { basename, ext, mmss, session } from "./util.ts";

export interface Track {
  path: string;
  root?: Src;
  title?: string;
  artist?: string;
  album?: string;
  dur?: number;
  hasCover?: boolean;
}

type Repeat = "off" | "all" | "one";

interface Saved {
  queue: Track[];
  index: number;
  pos: number;
  repeat: Repeat;
  shuffle: boolean;
  seed: number;
}

const KEY = "tr.now";

class Player {
  queue: Track[] = [];
  index = -1;
  repeat: Repeat = "off";
  shuffle = false;
  private order: number[] = []; // the shuffle order over queue indexes
  private seed = 1;
  private a = document.createElement("audio");
  private b = document.createElement("audio");
  private preloaded = -1; // the queue index b holds
  private listeners = new Set<() => void>();
  private bar: HTMLElement | null = null;
  private queueEl: HTMLElement | null = null;
  private els: Record<string, HTMLElement> = {};
  private coverFor = ""; // the thumbnail URL the bar's cover shows or is fetching
  private coverSrc = ""; // its picture as an object URL, once it came
  private saveTimer = 0;

  current(): Track | null {
    return this.index >= 0 ? (this.queue[this.index] ?? null) : null;
  }

  get playing(): boolean {
    return !this.a.paused && !this.a.ended;
  }

  isCurrent(path: string, root?: Src): boolean {
    const c = this.current();
    return !!c && c.path === path && c.root === root;
  }

  subscribe(fn: () => void): () => void {
    this.listeners.add(fn);
    return () => this.listeners.delete(fn);
  }

  /** mount builds the bar once and restores the last session's track, paused. */
  mount(): void {
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
    const saved = session.get<Saved | null>(KEY, null);
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
  play(tracks: Track[], index = 0): void {
    if (!tracks.length) return;
    this.queue = tracks.slice();
    this.index = Math.max(0, Math.min(index, tracks.length - 1));
    this.seed = (Date.now() % 2147483647) || 1;
    this.reshuffle();
    this.load(this.index, true);
  }

  /** enqueue appends tracks; with nothing playing it starts them. */
  enqueue(tracks: Track[]): void {
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
  playNow(t: Track): void {
    const i = this.queue.findIndex((q) => q.path === t.path && q.root === t.root);
    if (i >= 0) this.load(i, true);
    else this.play([t]);
  }

  toggle(): void {
    if (this.index < 0) return;
    if (this.a.paused) void this.a.play().catch(() => undefined);
    else this.a.pause();
  }

  pause(): void {
    this.a.pause();
  }

  next(auto = false): void {
    if (this.index < 0) return;
    if (auto && this.repeat === "one") {
      this.a.currentTime = 0;
      void this.a.play().catch(() => undefined);
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
    this.load(this.order[nextPos]!, true);
  }

  prev(): void {
    if (this.index < 0) return;
    if (this.a.currentTime > 3) {
      this.a.currentTime = 0;
      return;
    }
    const pos = this.order.indexOf(this.index);
    this.load(this.order[(pos - 1 + this.order.length) % this.order.length]!, true);
  }

  seek(sec: number): void {
    if (!Number.isFinite(sec)) return;
    this.a.currentTime = Math.max(0, sec);
  }

  cycleRepeat(): void {
    this.repeat = this.repeat === "off" ? "all" : this.repeat === "all" ? "one" : "off";
    this.notify();
    this.save();
  }

  toggleShuffle(): void {
    this.shuffle = !this.shuffle;
    this.reshuffle();
    this.notify();
    this.save();
  }

  remove(i: number): void {
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

  clear(): void {
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

  private reshuffle(): void {
    this.order = this.queue.map((_, i) => i);
    if (!this.shuffle) return;
    // a seeded Fisher–Yates so a reload keeps the same order (the seed is stored)
    let s = this.seed;
    const rnd = () => {
      s = (s * 48271) % 2147483647;
      return s / 2147483647;
    };
    for (let i = this.order.length - 1; i > 0; i--) {
      const j = Math.floor(rnd() * (i + 1));
      [this.order[i], this.order[j]] = [this.order[j]!, this.order[i]!];
    }
    if (this.index >= 0) {
      // the current track leads the order so "next" is the first unplayed one
      const p = this.order.indexOf(this.index);
      if (p > 0) this.order.unshift(...this.order.splice(p, 1));
    }
  }

  private url(t: Track): string {
    return fileURL(t.path, false, t.root);
  }

  private load(i: number, start: boolean, pos = 0): void {
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
    if (start) void this.a.play().catch(() => undefined);
    void this.describe(t);
    this.notify();
    this.save();
  }

  /** rebind moves the element listeners to the element that is now `a` (after a swap). */
  private rebind(): void {
    const fresh = this.a.cloneNode(false) as HTMLAudioElement; // listeners live on the old a; re-add on the new one
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

  private async describe(t: Track): Promise<void> {
    if (t.title === undefined) {
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

  private onTime(): void {
    const c = this.current();
    if (!c) return;
    const left = (this.a.duration || 0) - this.a.currentTime;
    // preload the next track shortly before the end so the swap is seamless
    if (Number.isFinite(left) && left < 15 && this.repeat !== "one") {
      const pos = this.order.indexOf(this.index);
      const n = pos + 1 < this.order.length ? this.order[pos + 1]! : this.repeat === "all" ? this.order[0]! : -1;
      if (n >= 0 && this.preloaded !== n) {
        this.preloaded = n;
        this.b.preload = "auto";
        this.b.src = this.url(this.queue[n]!);
        this.b.load();
      }
    }
    if (this.els.seek) {
      const seek = this.els.seek as HTMLInputElement;
      if (document.activeElement !== seek) {
        seek.max = String(Math.round(this.a.duration || c.dur || 0));
        seek.value = String(Math.round(this.a.currentTime));
      }
      this.els.time!.textContent = `${mmss(this.a.currentTime)} / ${mmss(this.a.duration || c.dur)}`;
    }
    if (!this.saveTimer) this.saveTimer = window.setTimeout(() => this.save(), 2000);
  }

  private save(): void {
    if (this.saveTimer) {
      window.clearTimeout(this.saveTimer);
      this.saveTimer = 0;
    }
    if (this.index < 0) {
      session.set(KEY, null);
      return;
    }
    const s: Saved = { queue: this.queue.map((t) => ({ path: t.path, root: t.root, title: t.title, artist: t.artist, album: t.album, dur: t.dur, hasCover: t.hasCover })), index: this.index, pos: this.a.currentTime || 0, repeat: this.repeat, shuffle: this.shuffle, seed: this.seed };
    session.set(KEY, s);
  }

  /**
   * drawCover shows the thumbnail at want: the track's picture or the cover beside it,
   * whatever has_cover says (only the embedded picture), fetched once per track and asked
   * again while the node makes it (a 202, I-374); a miss leaves the box empty until
   * another track plays (docs/13 §6). The picture is also the media session's artwork.
   */
  private drawCover(cover: HTMLImageElement, want: string): void {
    if (want === this.coverFor) return;
    this.coverFor = want;
    if (this.coverSrc) URL.revokeObjectURL(this.coverSrc);
    this.coverSrc = "";
    cover.removeAttribute("src");
    void fetchThumb(want).then((src) => {
      if (this.coverFor !== want) { // another track since
        if (src) URL.revokeObjectURL(src);
        return;
      }
      if (!src) return;
      this.coverSrc = src;
      cover.src = src;
      this.mediaSession();
    });
  }

  private mediaSession(): void {
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
      artwork: this.coverSrc && this.coverFor === thumbURL(c.path, c.root) ? [{ src: this.coverSrc, sizes: "320x320", type: "image/jpeg" }] : [],
    });
    ms.setActionHandler("play", () => this.toggle());
    ms.setActionHandler("pause", () => this.pause());
    ms.setActionHandler("previoustrack", () => this.prev());
    ms.setActionHandler("nexttrack", () => this.next());
    ms.setActionHandler("seekto", (d) => this.seek(d.seekTime ?? 0));
    ms.setActionHandler("seekbackward", () => this.seek(this.a.currentTime - 10));
    ms.setActionHandler("seekforward", () => this.seek(this.a.currentTime + 10));
  }

  private keys(ev: KeyboardEvent): void {
    if (this.index < 0) return;
    const t = ev.target as HTMLElement | null;
    if (t && (t.tagName === "INPUT" || t.tagName === "TEXTAREA" || t.tagName === "SELECT" || t.isContentEditable)) return;
    if (document.querySelector("section.player video, section.player img")) return; // the file view owns the keys there
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

  private buildBar(): void {
    const cover = h("img", { class: "cover", alt: "" }) as HTMLImageElement;
    cover.addEventListener("error", () => cover.removeAttribute("src")); // never the broken-image glyph
    const title = h("div", { class: "title" });
    const sub = h("div", { class: "sub" });
    const prev = iconBtn("skip_previous_fill", "previous", () => this.prev());
    const playBtn = iconBtn("play_arrow_fill", "play", () => this.toggle());
    const next = iconBtn("skip_next_fill", "next", () => this.next());
    const seek = h("input", { type: "range", min: 0, max: 0, value: 0, "aria-label": "seek" }) as HTMLInputElement;
    seek.addEventListener("input", () => this.seek(Number(seek.value)));
    const time = h("span", { class: "time" }, "0:00 / —");
    const repeat = iconBtn("repeat", "repeat", () => this.cycleRepeat());
    repeat.setAttribute("aria-pressed", "false");
    const shuffle = iconBtn("shuffle", "shuffle", () => this.toggleShuffle());
    shuffle.setAttribute("aria-pressed", "false");
    const close = iconBtn("close", "close the player", () => this.clear()); // stop and empty the queue, as Clear does
    close.classList.add("close");
    const queueBtn = h("button", { type: "button", class: "queue-btn", "aria-pressed": "false", onClick: () => { this.bar?.classList.toggle("expanded"); this.notify(); } }, icon("queue_music"), h("span", null, "Queue"));
    const bar = h(
      "aside",
      { class: "nowbar", hidden: true },
      cover,
      h("div", { class: "titles" }, title, sub),
      h("div", { class: "transport" }, prev, playBtn, next),
      h("div", { class: "seek" }, seek, time),
      h("div", { class: "modes" }, repeat, shuffle),
      queueBtn,
      close,
    );
    const list = h("ol");
    const summary = h("span", { class: "small dim" });
    const queue = h("div", { class: "queue", hidden: true }, h("div", { class: "toolbar" }, summary, h("span", { class: "spacer" }), h("button", { type: "button", onClick: () => this.clear() }, "Clear")), list);
    document.body.append(bar, queue);
    this.bar = bar;
    this.queueEl = queue;
    this.els = { cover, title, sub, playBtn, seek, time, repeat, shuffle, queueBtn, list, summary };
  }

  private notify(): void {
    this.drawBar();
    for (const fn of this.listeners) fn();
  }

  private drawBar(): void {
    if (!this.bar) return;
    const c = this.current();
    this.bar.hidden = !c;
    if (this.queueEl) this.queueEl.hidden = !c || !this.bar.classList.contains("expanded");
    if (!c) return;
    const e = this.els;
    this.drawCover(e.cover as HTMLImageElement, thumbURL(c.path, c.root));
    e.title!.textContent = c.title ?? basename(c.path);
    e.sub!.textContent = [c.artist, c.album].filter(Boolean).join(" — ") || ext(c.path);
    face(e.playBtn!, this.playing ? "pause_fill" : "play_arrow_fill", this.playing ? "pause" : "play");
    e.repeat!.setAttribute("aria-pressed", this.repeat === "off" ? "false" : "true");
    face(e.repeat!, this.repeat === "one" ? "repeat_one" : "repeat", this.repeat === "one" ? "repeat one" : this.repeat === "all" ? "repeat all" : "repeat");
    e.shuffle!.setAttribute("aria-pressed", this.shuffle ? "true" : "false");
    const expanded = this.bar.classList.contains("expanded");
    e.queueBtn!.setAttribute("aria-pressed", expanded ? "true" : "false");
    e.queueBtn!.lastElementChild!.textContent = `Queue · ${this.queue.length}`;
    e.time!.textContent = `${mmss(this.a.currentTime)} / ${mmss(this.a.duration || c.dur)}`;
    e.summary!.textContent = `${this.queue.length} in the queue` + (this.repeat !== "off" ? ` · repeat ${this.repeat}` : "") + (this.shuffle ? " · shuffle" : "");
    if (expanded) this.drawQueue();
  }

  private drawQueue(): void {
    const list = this.els.list!;
    list.replaceChildren();
    this.order.forEach((qi, n) => {
      const t = this.queue[qi]!;
      const playing = qi === this.index;
      const li = h(
        "li",
        { class: "qrow" + (playing ? " playing" : "") },
        h("span", { class: "no" }, playing ? icon("equalizer") : String(n + 1)),
        h("span", { class: "title", onClick: () => this.load(qi, true) }, t.title ?? basename(t.path)),
        h("span", { class: "artist" }, t.artist ?? ""),
        h("span", { class: "dur" }, mmss(t.dur)),
        iconBtn("close", "remove", () => this.remove(qi)),
      );
      list.append(li);
    });
  }
}

export const player = new Player();

/** isAudioName reports a file the player handles. */
export function isAudioName(name: string): boolean {
  return ["mp3", "m4a", "aac", "ogg", "oga", "opus", "wav", "flac"].includes(ext(name));
}

/** iconBtn is a square button carrying one Material symbol; label names it for a screen reader and in its tooltip. */
function iconBtn(name: IconName, label: string, onClick: () => void): HTMLButtonElement {
  const b = h("button", { type: "button", class: "icon-btn", onClick });
  face(b, name, label);
  return b;
}

/** face redraws an icon button's symbol and its name. */
function face(b: HTMLElement, name: IconName, label: string): void {
  b.setAttribute("aria-label", label);
  b.title = label;
  b.replaceChildren(icon(name));
}
