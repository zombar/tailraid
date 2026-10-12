// Search (docs/13 §4, docs/18 §6–§7): the box in the header and the results view at
// #/search/<q>[?under=<folder>]. Names come from the node's live tree, tags and
// contents from the index; a content hit's snippet is one small Range read of the file,
// fetched three at a time as hits scroll into view.

import { clear, h } from "./dom.ts";
import { fileURL, search as query, type SearchHit } from "./api.ts";
import { basename, browseHash, dirname, fileHash } from "./util.ts";

/** the folder last browsed: what "in this folder only" means */
let folder = "/";

export function setFolder(p: string): void {
  folder = p || "/";
}

export function searchHash(q: string, under: string): string {
  return "#/search/" + encodeURIComponent(q) + (under ? "?under=" + encodeURIComponent(under) : "");
}

function typing(t: EventTarget | null): boolean {
  return t instanceof HTMLElement && (t.isContentEditable || t.tagName === "INPUT" || t.tagName === "TEXTAREA" || t.tagName === "SELECT");
}

/** wire makes the header form live: `/` focuses it, Escape clears it, typing searches. */
export function wire(form: HTMLFormElement): void {
  const input = form.querySelector("input") as HTMLInputElement;
  let timer = 0;
  const go = () => {
    const q = input.value.trim();
    if (!q) return;
    const on = location.hash.startsWith("#/search/");
    const under = on ? (new URLSearchParams(location.hash.split("?")[1] ?? "").get("under") ?? "") : "";
    const target = searchHash(q, under);
    if (target === location.hash) return;
    if (on) location.replace(target); // typeahead refines the results in place, not in history
    else location.hash = target;
  };
  form.addEventListener("submit", (ev) => {
    ev.preventDefault();
    clearTimeout(timer);
    go();
  });
  input.addEventListener("input", () => {
    clearTimeout(timer);
    timer = window.setTimeout(() => {
      if (input.value.trim().length >= 2) go();
    }, 150);
  });
  input.addEventListener("keydown", (ev) => {
    if (ev.key === "Escape") {
      input.value = "";
      input.blur();
    }
  });
  document.addEventListener("keydown", (ev) => {
    if (ev.key === "/" && !ev.metaKey && !ev.ctrlKey && !typing(ev.target)) {
      ev.preventDefault();
      input.focus();
      input.select();
    }
  });
}

/** fold is the index's folding (docs/18 §3): compatibility decomposition, marks dropped, lower case. */
function fold(s: string): string {
  return s.normalize("NFKD").replace(/\p{M}/gu, "").toLowerCase();
}

/** marked splits text into words and marks those a term matches (the last term as a prefix). */
function marked(text: string, terms: string[]): (Node | string)[] {
  const out: (Node | string)[] = [];
  for (const part of text.split(/([\p{L}\p{N}]+)/u)) {
    if (!part) continue;
    const f = fold(part);
    out.push(/[\p{L}\p{N}]/u.test(part) && terms.some((t) => f.startsWith(t)) ? h("mark", null, part) : part);
  }
  return out;
}

const SNIP_BEFORE = 100;
const SNIP_AFTER = 140;
const SNIP_AT_ONCE = 3;

/** snippet reads the bytes around a content hit's first term and marks the terms. */
async function snippet(hit: SearchHit, signal: AbortSignal): Promise<(Node | string)[]> {
  const off = Math.min(...hit.terms.map((t) => t.off));
  const start = Math.max(0, off - SNIP_BEFORE);
  const end = Math.min(hit.size, off + SNIP_AFTER) - 1;
  const r = await fetch(fileURL(hit.path), { headers: { Range: `bytes=${start}-${Math.max(end, start)}` }, signal });
  if (!r.ok) throw new Error(`${r.status}`);
  let text = new TextDecoder("utf-8").decode(await r.arrayBuffer()).replace(/�/g, "");
  if (start > 0) text = "…" + text.replace(/^[\p{L}\p{N}]*\s*/u, "");
  if (end < hit.size - 1) text = text.replace(/\s*[\p{L}\p{N}]*$/u, "") + "…";
  return marked(text.replace(/\s+/g, " "), hit.terms.map((t) => t.term));
}

function hitRow(hit: SearchHit, want: (el: HTMLElement, hit: SearchHit) => void): HTMLElement {
  const href = hit.type === "dir" ? browseHash({ path: hit.path }) : fileHash({ path: hit.path });
  let where = dirname(hit.path);
  if (hit.match === "tags" && hit.title) where += " · " + [hit.title, hit.artist, hit.album].filter(Boolean).join(" — ");
  const li = h("li", { class: "hit" }, h("a", { class: "hname", href }, basename(hit.path) || "/"), h("span", { class: "hpath" }, where), h("span", { class: "hwhy badge" }, hit.match));
  if (hit.match === "content" && hit.terms.length > 0 && hit.size > 0) {
    const snip = h("span", { class: "hsnip loading" }, "loading the snippet…");
    li.append(snip);
    want(snip, hit);
  }
  return li;
}

export function render(root: HTMLElement, q: string, params: string): () => void {
  const under = new URLSearchParams(params).get("under") ?? "";
  const input = document.querySelector<HTMLInputElement>("form.search input");
  if (input && document.activeElement !== input) input.value = q;
  const abort = new AbortController();
  const section = h("section", { class: "results" });
  root.append(section);
  const scope = under || folder;
  const sub = h("span", { class: "h-sub" }, "searching…");
  const bar = h("div", { class: "toolbar" }, h("h2", null, "Search", sub), h("span", { class: "spacer" }));
  if (scope !== "/") {
    const toggle = h("input", { type: "checkbox", checked: under !== "" }) as HTMLInputElement;
    toggle.addEventListener("change", () => location.replace(searchHash(q, toggle.checked ? scope : "")));
    bar.append(h("label", { class: "toggle" }, toggle, " in ", h("code", null, scope), " only"));
  }
  section.append(bar);

  // snippets: fetched as their rows scroll into view, at most SNIP_AT_ONCE at a time
  const queue: [HTMLElement, SearchHit][] = [];
  let running = 0;
  const pump = () => {
    while (running < SNIP_AT_ONCE && queue.length > 0) {
      const [el, hit] = queue.shift()!;
      running++;
      snippet(hit, abort.signal)
        .then(
          (parts) => {
            clear(el);
            el.classList.remove("loading");
            el.append(...parts);
          },
          () => el.remove(),
        )
        .finally(() => {
          running--;
          pump();
        });
    }
  };
  const pending = new Map<Element, SearchHit>();
  const seen = new IntersectionObserver((entries) => {
    for (const e of entries) {
      const hit = pending.get(e.target);
      if (!e.isIntersecting || !hit) continue;
      pending.delete(e.target);
      seen.unobserve(e.target);
      queue.push([e.target as HTMLElement, hit]);
    }
    pump();
  });
  const want = (el: HTMLElement, hit: SearchHit) => {
    pending.set(el, hit);
    seen.observe(el);
  };

  query(q, under).then(
    (res) => {
      if (abort.signal.aborted) return;
      clear(sub);
      sub.append(`${res.hits.length} ${res.hits.length === 1 ? "hit" : "hits"} for `, h("code", null, q));
      if (res.index) section.append(h("div", { class: "note" }, "index unavailable on this node: names only until the leader's next pass"));
      if (res.hits.length === 0) {
        const empty = h("div", { class: "empty" }, "no matches for ", h("code", null, q));
        if (under) empty.append(" under ", h("code", null, under));
        section.append(empty);
        return;
      }
      const ol = h("ol");
      for (const hit of res.hits) ol.append(hitRow(hit, want));
      section.append(ol);
    },
    (e: unknown) => {
      if (!abort.signal.aborted) sub.textContent = "search failed: " + (e instanceof Error ? e.message : String(e));
    },
  );
  return () => {
    abort.abort();
    seen.disconnect();
  };
}
