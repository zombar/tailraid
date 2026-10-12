// The markdown document (docs/13 §4, I-209): the node renders the file escape-first
// (GET /api/render) and the page inserts the fragment; "Rendered | Raw" switches to the
// source, remembered for the session. An in-page #anchor scrolls to its heading
// instead of reaching the hash router.

import { clear, h } from "./dom.ts";
import { rendered, text } from "./api.ts";
import { session } from "./util.ts";

export function isMarkdown(name: string): boolean {
  return /\.(md|markdown)$/i.test(name);
}

function message(e: unknown): string {
  return e instanceof Error ? e.message : String(e);
}

/** view builds the switch for the title row and the body that follows it. */
export function view(path: string, root: number | undefined): { seg: HTMLElement; body: HTMLElement } {
  const body = h("div");
  const asRendered = h("button", { type: "button" }, "Rendered");
  const asRaw = h("button", { type: "button" }, "Raw");
  const seg = h("span", { class: "seg", role: "group", "aria-label": "view" }, asRendered, asRaw);
  const show = (raw: boolean) => {
    session.set("tr.md.raw", raw);
    asRendered.setAttribute("aria-pressed", String(!raw));
    asRaw.setAttribute("aria-pressed", String(raw));
    clear(body);
    if (raw) {
      const pre = h("pre", null, "loading…");
      body.append(pre);
      text(path, root).then(
        (t) => (pre.textContent = t),
        (e: unknown) => (pre.textContent = "could not load: " + message(e)),
      );
      return;
    }
    const article = h("article", { class: "md md-doc" }, "loading…");
    body.append(article);
    rendered(path, root).then(
      (html) => {
        article.innerHTML = html; // the node's escape-first renderer: only its own tags (I-209)
      },
      (e: unknown) => (article.textContent = "could not render: " + message(e)),
    );
  };
  body.addEventListener("click", (ev) => {
    const a = (ev.target as Element).closest("a");
    const href = a?.getAttribute("href") ?? "";
    if (!href.startsWith("#") || href.startsWith("#/")) return;
    ev.preventDefault();
    document.getElementById(decodeURIComponent(href.slice(1)))?.scrollIntoView({ behavior: "smooth", block: "start" });
  });
  asRendered.addEventListener("click", () => show(false));
  asRaw.addEventListener("click", () => show(true));
  show(session.get("tr.md.raw", false));
  return { seg, body };
}
