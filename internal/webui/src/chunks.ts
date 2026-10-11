// The per-file integrity panel: paged chunk reports with a slot strip per chunk. A page
// shows a loading row the moment it is asked for, a failed page keeps the rows already
// there and offers a retry, and the panel is built once per file view (docs/13 §7, I-161).

import { h } from "./dom.ts";
import { slotStrip } from "./chart.ts";
import { chunks, type ChunkReport } from "./api.ts";
import { count } from "./fmt.ts";

const PAGE = 64;

export interface Panel {
  el: HTMLElement;
  /** expect sets the chunk count in the summary once the file's size is known. */
  expect(n: number): void;
}

export function render(path: string, expected?: number): Panel {
  const title = h("span");
  const summaryExtra = h("span", { class: "dim small" });
  const summary = h("summary", null, title, summaryExtra);
  const body = h("div", { class: "body" });
  const panel = h("details", { class: "panel" }, summary, body);
  let loaded = 0;
  let total = 0;
  let fetching = false;
  const counts = { committed: 0, degraded: 0, unreadable: 0, tombstoned: 0, stale: 0 };
  const rows = h("div");
  const more = h("button", { type: "button" }, "Load more");
  const caption = h("div", { class: "small dim" });
  const loading = h("div", { class: "loading" });
  const retry = h("button", { type: "button" }, "Retry");
  const failure = h("div", { class: "error" });

  const setTitle = (n?: number) => {
    title.textContent = "Integrity" + (n !== undefined ? ` (${count(n)} ${n === 1 ? "chunk" : "chunks"})` : "");
  };
  setTitle(expected);

  const describe = () => {
    const parts: string[] = [];
    if (counts.committed) parts.push(`${count(counts.committed)} committed`);
    if (counts.degraded) parts.push(`${count(counts.degraded)} degraded`);
    if (counts.unreadable) parts.push(`${count(counts.unreadable)} unreadable`);
    if (counts.tombstoned) parts.push(`${count(counts.tombstoned)} tombstoned`);
    if (counts.stale) parts.push(`${count(counts.stale)} policy stale`);
    summaryExtra.textContent = parts.length ? " · " + parts.join(" · ") : "";
    caption.textContent = `a hash that appears more than once is the same stored chunk (dedup) · ${count(loaded)} of ${count(total)}`;
    more.textContent = `Load ${Math.min(PAGE, total - loaded)} more`;
    more.hidden = loaded >= total;
  };

  const load = async () => {
    if (fetching) return;
    fetching = true;
    more.disabled = true;
    failure.remove();
    const known = total || expected;
    loading.textContent = `loading ${known !== undefined ? Math.min(PAGE, Math.max(known - loaded, 0)) : PAGE} of ${known !== undefined ? count(known) : "?"}…`;
    body.append(loading);
    try {
      const page = await chunks(path, loaded, PAGE);
      total = page.total;
      if (expected === undefined) setTitle(total);
      page.chunks.forEach((c, i) => {
        rows.append(row(loaded + i, c));
        if (c.state === "committed") counts.committed++;
        else if (c.state === "degraded") counts.degraded++;
        else if (c.state === "unreadable") counts.unreadable++;
        else if (c.state === "tombstoned") counts.tombstoned++;
        if (c.policy_stale) counts.stale++;
      });
      loaded += page.chunks.length;
      loading.remove();
      if (!body.contains(rows)) body.append(rows, caption, more);
      describe();
      if (total === 0 && !body.querySelector(".empty")) body.append(h("div", { class: "empty" }, "no chunks (empty file)"));
    } catch (e) {
      loading.remove();
      failure.textContent = "Could not load the chunk reports: " + (e instanceof Error ? e.message : String(e)) + " ";
      failure.append(retry);
      body.append(failure);
    } finally {
      fetching = false;
      more.disabled = false;
    }
  };
  more.addEventListener("click", () => void load());
  retry.addEventListener("click", () => void load());
  panel.addEventListener("toggle", () => {
    if (panel.open && loaded === 0 && !fetching) void load();
  });
  return {
    el: panel,
    expect(n) {
      expected = n;
      setTitle(n);
    },
  };
}

function row(i: number, c: ChunkReport): HTMLElement {
  const km = c.k + c.m;
  const live = h("span", { class: "live" + (c.live_slots < c.k ? " bad" : c.live_slots < km ? " warn" : "") }, `${c.live_slots}/${km}`);
  const hash = h("span", { title: c.hash }, c.hash.slice(0, 10));
  if (c.policy_stale) hash.append(" ", h("span", { class: "badge warn" }, "stale"));
  if (!c.found) hash.append(" ", h("span", { class: "badge bad" }, c.state));
  return h("div", { class: "chunk-row" }, h("span", { class: "dim" }, "#" + i), hash, h("span", { class: "km dim" }, `${c.k}+${c.m}`), live, slotStrip(c.slots ?? []));
}
