// Playback reports (docs/13 §10c): a media element's position every 15 s while it plays
// and at pause, a settled seek, the end and leaving; a track's play once it passes half
// its length or four minutes. The node keeps one row per viewer and file (op 34).

import { report, type Progress } from "./api.ts";

export const REPORT_EVERY = 15_000; // ms
export const RESUME_MIN = 60_000; // ms: the least position worth resuming from
export const LONG_TRACK = 20 * 60_000; // ms: a track this long keeps a position

/** watchedAt is the node's rule (docs/02 §Apply rules): 90 %, or the last five minutes of anything over thirty. */
export function watchedAt(pos: number, dur: number): boolean {
  if (dur <= 0) return false;
  return pos >= dur - Math.floor(dur / 10) || (dur > 30 * 60_000 && pos >= dur - 5 * 60_000);
}

/** resumeFrom is where a row offers to resume, in seconds; 0 when it does not. */
export function resumeFrom(p: Progress | undefined | null): number {
  if (!p || p.watched || p.pos < RESUME_MIN || watchedAt(p.pos, p.dur)) return 0;
  return p.pos / 1000;
}

/** left is what remains of a row, as people read it: "26 min left", "1 h 40 left". */
export function left(p: Progress): string {
  const min = Math.max(0, Math.round((p.dur - p.pos) / 60_000));
  return min >= 60 ? `${Math.floor(min / 60)} h ${String(min % 60).padStart(2, "0")} left` : `${min} min left`;
}

/** hms is a position as h:mm:ss, or m:ss under an hour. */
export function hms(sec: number): string {
  const s = Math.max(0, Math.floor(sec));
  const h = Math.floor(s / 3600);
  const m = Math.floor((s % 3600) / 60);
  const ss = String(s % 60).padStart(2, "0");
  return h ? `${h}:${String(m).padStart(2, "0")}:${ss}` : `${m}:${ss}`;
}

export interface WatchOpts {
  /** positions reports where el is; for audio only once the track is known to be long. */
  positions: () => boolean;
  /** plays reports a play once el passes half its length or four minutes. */
  plays: boolean;
}

/** watch reports el's playback of path until the returned function ends it with a final report. */
export function watch(el: HTMLMediaElement, path: string, opts: WatchOpts): () => void {
  let last = 0;
  let played = false;
  let ended = false;
  let seekTimer = 0;
  const send = (final: boolean) => {
    const dur = el.duration;
    if (!opts.positions() || !Number.isFinite(dur) || dur <= 0) return;
    last = Date.now();
    void report({ path, pos: Math.round(el.currentTime * 1000), dur: Math.round(dur * 1000), final }, final).catch(() => undefined);
  };
  const onTime = () => {
    if (!el.paused && Date.now() - last >= REPORT_EVERY) send(false);
    if (opts.plays && !played && Number.isFinite(el.duration) && el.currentTime >= Math.min(el.duration / 2, 240)) {
      played = true;
      void report({ path, played: true }).catch(() => undefined);
    }
  };
  const onPause = () => {
    if (!el.ended) send(true);
  };
  const onSeeked = () => {
    window.clearTimeout(seekTimer);
    seekTimer = window.setTimeout(() => send(true), 1000); // once the seek settles
  };
  const onEnded = () => {
    ended = true;
    const dur = el.duration;
    if (opts.positions() && Number.isFinite(dur) && dur > 0) void report({ path, pos: Math.round(dur * 1000), dur: Math.round(dur * 1000), final: true }, true).catch(() => undefined);
  };
  const onHide = () => send(true);
  el.addEventListener("timeupdate", onTime);
  el.addEventListener("pause", onPause);
  el.addEventListener("seeked", onSeeked);
  el.addEventListener("ended", onEnded);
  window.addEventListener("pagehide", onHide);
  return () => {
    el.removeEventListener("timeupdate", onTime);
    el.removeEventListener("pause", onPause);
    el.removeEventListener("seeked", onSeeked);
    el.removeEventListener("ended", onEnded);
    window.removeEventListener("pagehide", onHide);
    window.clearTimeout(seekTimer);
    if (!ended && el.currentTime > 0) send(true);
  };
}
