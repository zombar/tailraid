// The theme switch (docs/13 §Design Tokens, I-200): system → light → dark → system. The
// choice is a per-viewer convenience kept in localStorage; it is applied at module load,
// before any view renders, and the server never sees it.

import { icon } from "./icons.ts";

export type Theme = "system" | "light" | "dark";

const KEY = "theme";
const NEXT: Record<Theme, Theme> = { system: "light", light: "dark", dark: "system" };

/** stored reads the choice; a storage that throws or holds junk means system. */
export function stored(): Theme {
  try {
    const v = localStorage.getItem(KEY);
    return v === "light" || v === "dark" ? v : "system";
  } catch {
    return "system";
  }
}

/** apply sets data-theme on the document (removed for system) and remembers the choice. */
export function apply(t: Theme): void {
  if (t === "system") document.documentElement.removeAttribute("data-theme");
  else document.documentElement.setAttribute("data-theme", t);
  try {
    if (t === "system") localStorage.removeItem(KEY);
    else localStorage.setItem(KEY, t);
  } catch {
    // a private window or cleared site data: the choice lives for this page only
  }
}

/** next is the cycle's successor. */
export function next(t: Theme): Theme {
  return NEXT[t];
}

const GLYPHS: Record<Theme, () => SVGElement> = {
  system: () => icon("contrast"),
  light: () => icon("light_mode"),
  dark: () => icon("dark_mode"),
};

/** wire binds the header's button: its glyph and label follow the choice, a tap cycles. */
export function wire(button: HTMLButtonElement): void {
  const draw = (t: Theme) => {
    const label = `Theme: ${t} (tap for ${next(t)})`;
    button.title = label;
    button.setAttribute("aria-label", label);
    button.replaceChildren(GLYPHS[t](), Object.assign(document.createElement("span"), { textContent: t }));
  };
  let current = stored();
  draw(current);
  button.addEventListener("click", () => {
    current = next(current);
    apply(current);
    draw(current);
  });
}

apply(stored());
