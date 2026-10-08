// The charts of docs/13 §Design: bars on a track with an optional expected-share tick, a
// full-width sparkline, and the slot strip. Fill is neutral unless a status applies.

import { h, setWidth, svg } from "./dom.ts";
import type { SlotReport } from "./api.ts";

export type Tone = "" | "warn" | "bad";

/** bar builds `.bar > i` (+ `b.tick` at tickFrac); "off" draws the hatched unknown track. */
export function bar(frac: number, tone: Tone | "off" = "", tickFrac?: number, tall = false): HTMLElement {
  const el = h("div", { class: "bar" + (tone ? " " + tone : "") + (tall ? " tall" : "") });
  if (tone === "off") return el;
  const fill = h("i");
  setWidth(fill, frac);
  el.append(fill);
  if (tickFrac !== undefined) {
    const tick = h("b", { class: "tick" });
    tick.style.left = (Math.max(0, Math.min(1, tickFrac)) * 100).toFixed(2) + "%";
    el.append(tick);
  }
  return el;
}

/** barRow is a labelled bar with its value underneath. */
export function barRow(label: string, b: HTMLElement, value: Node | string): HTMLElement {
  return h("div", { class: "bar-row" }, h("span", { class: "bar-label" }, label), b, h("span", { class: "bar-val" }, value));
}

/** tone picks the capacity colour: amber above 80 %, red above 95 %. */
export function fillTone(frac: number): Tone {
  return frac > 0.95 ? "bad" : frac > 0.8 ? "warn" : "";
}

/** sparkline draws values full width: 2 px line, 8 % wash, the end dot coloured by the last value. */
export function sparkline(values: number[], lastTone: Tone = ""): HTMLElement {
  const wrap = h("div", { class: "spark-wrap" });
  const W = 120;
  const H = 32;
  const n = values.length;
  const max = Math.max(1, ...values);
  const pts = values.map((v, i) => [n === 1 ? W : (i / (n - 1)) * W, 28 - (v / max) * 24] as const);
  const line = pts.map(([x, y], i) => (i ? "L" : "M") + x.toFixed(1) + " " + y.toFixed(1)).join(" ");
  const el = svg("svg", { class: "spark", viewBox: `0 0 ${W} ${H}`, preserveAspectRatio: "none", "aria-hidden": "true" });
  if (n >= 2) {
    el.append(svg("path", { class: "area", d: `${line} L${W} ${H} L0 ${H} Z` }));
    el.append(svg("path", { class: "line", d: line }));
  }
  wrap.append(el);
  if (n >= 1) {
    const dot = h("span", { class: "spark-dot" + (lastTone ? " " + lastTone : "") });
    const y = pts[n - 1]![1];
    dot.style.top = ((y / H) * 48).toFixed(1) + "px";
    wrap.append(dot);
  }
  return wrap;
}

/** slotStrip is one cell per slot: present green, live-but-absent amber "!", dead red "×", unplaced dashed, unverified faded. */
export function slotStrip(slots: SlotReport[]): HTMLElement {
  const strip = h("div", { class: "cells" });
  for (const s of slots) {
    let cls = "cell";
    let title = `slot ${s.idx}`;
    if (s.node === 0) {
      cls += " unplaced";
      title += " · unplaced";
    } else {
      title += " · " + (s.name || "node " + s.node);
      if (!s.live) {
        cls += " bad";
        title += " · dead";
      } else if (s.present === false) {
        cls += " warn";
        title += " · live, shard absent";
      } else if (s.present == null) {
        cls += " unverified";
        title += " · not verified";
      }
    }
    strip.append(h("span", { class: cls, title }));
  }
  return strip;
}
