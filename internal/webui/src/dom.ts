// Tiny DOM helpers. No framework: the markup is docs/assets/lookdev, built element by element.

export type Child = Node | string | number | null | undefined | false;
export type Attrs = Record<string, string | number | boolean | EventListener | null | undefined>;

/** h builds an element: "on*" attrs add listeners, "class" sets className, "style" is refused (the CSP blocks inline style; use el.style). */
export function h<K extends keyof HTMLElementTagNameMap>(tag: K, attrs?: Attrs | null, ...kids: Child[]): HTMLElementTagNameMap[K] {
  const el = document.createElement(tag);
  if (attrs) {
    for (const [k, v] of Object.entries(attrs)) {
      if (v == null || v === false) continue;
      if (k.startsWith("on") && typeof v === "function") el.addEventListener(k.slice(2).toLowerCase(), v);
      else if (k === "class") el.className = String(v);
      else if (k === "style") throw new Error("inline style is blocked by the CSP");
      else el.setAttribute(k, v === true ? "" : String(v));
    }
  }
  append(el, kids);
  return el;
}

export function append(el: Element, kids: Child[]): void {
  for (const c of kids) {
    if (c == null || c === false) continue;
    el.append(c instanceof Node ? c : String(c));
  }
}

const SVG = "http://www.w3.org/2000/svg";

export function svg(tag: string, attrs?: Record<string, string | number>, ...kids: Node[]): SVGElement {
  const el = document.createElementNS(SVG, tag);
  if (attrs) for (const [k, v] of Object.entries(attrs)) el.setAttribute(k, String(v));
  for (const c of kids) el.append(c);
  return el;
}

export function clear(el: Element): void {
  while (el.firstChild) el.removeChild(el.firstChild);
}

/** toast shows a message bottom right for 6 s; click dismisses. */
export function toast(msg: string, cls?: "ok" | "bad"): void {
  const box = document.getElementById("toasts");
  if (!box) return;
  const el = h("div", { class: "toast" + (cls ? " " + cls : ""), role: "status" }, msg);
  el.addEventListener("click", () => el.remove());
  box.append(el);
  setTimeout(() => el.remove(), 6000);
}

const ICONS: Record<string, string> = {
  dir: "M1.5 4.5h4.5l1.5 1.5h7v7.5h-13z",
  file: "M4 1.5h5.5l3 3v10h-8.5z M9.5 1.5v3h3",
  link: "M6.5 9.5a2.5 2.5 0 0 0 3.5 0l2.5-2.5a2.5 2.5 0 0 0-3.5-3.5L8 4.5 M9.5 6.5a2.5 2.5 0 0 0-3.5 0L3.5 9a2.5 2.5 0 0 0 3.5 3.5L8 11.5",
  media: "M4 2.5l9 5.5-9 5.5z",
  image: "M1.5 2.5h13v11h-13z M1.5 11l4-4 3 3 2.5-2.5 3.5 3.5",
};

export type IconKind = "dir" | "file" | "link" | "media" | "image";

/** titleCard stands in for art that does not exist: the title and the line under it (docs/13 §10, I-280). */
export function titleCard(title: string, sub?: string): HTMLElement {
  return h("span", { class: "tcard" }, h("span", { class: "ct" }, title), sub ? h("span", { class: "cy" }, sub) : null);
}

export function icon(kind: IconKind): SVGElement {
  const el = svg("svg", { class: "icon", viewBox: "0 0 16 16" }, svg("path", { d: ICONS[kind] ?? ICONS.file! }));
  if (kind === "image") el.append(svg("circle", { cx: 11, cy: 5.5, r: 1 }));
  return el;
}

/** setWidth sizes a bar fill through the CSSOM, which the CSP allows. */
export function setWidth(el: HTMLElement, frac: number): void {
  el.style.width = (Math.max(0, Math.min(1, frac)) * 100).toFixed(2) + "%";
}
