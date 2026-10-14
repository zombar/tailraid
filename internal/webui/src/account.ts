// The account chip and the session (docs/21 §6, I-354): who the page is signed in as,
// sign-out, the way to the sign-in page when a request is refused, and the hand-off
// that signs another node's page into the same session.

import { h } from "./dom.ts";
import { icon } from "./icons.ts";
import { HEADER } from "./api.ts";

export interface Session {
  user: string;
  admin: boolean;
}

let session: Session | null = null;

/** current is the session the page runs under; null before the cluster has users. */
export function current(): Session | null {
  return session;
}

/** toLogin sends the browser to the sign-in page, with the way back to this page. */
export function toLogin(): void {
  const to = location.pathname + location.search + location.hash;
  location.replace("/login?to=" + encodeURIComponent(to));
}

/** mount asks who the page is signed in as and draws the chip before the theme button. */
export async function mount(header: HTMLElement): Promise<void> {
  let r: Response;
  try {
    r = await fetch("/api/session");
  } catch {
    return;
  }
  if (r.status === 401) {
    toLogin();
    return;
  }
  if (!r.ok) return; // no users: the page is open, no chip
  session = (await r.json()) as Session;
  const chip = h("span", { class: "account", title: `signed in as ${session.user}` }, icon("person"), h("span", null, session.user));
  const out = h("button", { type: "button", class: "signout", title: "Sign out", "aria-label": "Sign out" }, icon("logout"));
  out.addEventListener("click", async () => {
    out.disabled = true;
    try {
      await fetch("/api/logout", { method: "POST", headers: { [HEADER]: "1" } });
    } finally {
      location.replace("/login");
    }
  });
  chip.append(out);
  const theme = header.querySelector("button.theme");
  if (theme) theme.before(chip);
  else header.append(chip);
}

/** memberLink is a link to another member's page: with users it goes through the hand-off, else straight. */
export function memberLink(name: string, href: string, ...kids: (string | Node)[]): HTMLAnchorElement {
  const a = h("a", { href, "data-node": name }, ...kids);
  return a;
}

/** wire sends every click on a member's link through the hand-off while the page has a session. */
export function wire(root: Document): void {
  root.addEventListener("click", (ev) => {
    const a = (ev.target as Element | null)?.closest<HTMLAnchorElement>("a[data-node]");
    if (!a || !session) return;
    ev.preventDefault();
    void follow(a);
  });
}

async function follow(a: HTMLAnchorElement): Promise<void> {
  const to = new URL(a.href, location.href);
  try {
    const r = await fetch("/api/handoff?to=" + encodeURIComponent(a.dataset.node ?? ""));
    if (r.status === 401) {
      toLogin();
      return;
    }
    if (!r.ok) {
      location.assign(a.href); // no hand-off for this member: the plain link, which asks to sign in
      return;
    }
    const { url } = (await r.json()) as { url: string };
    location.assign(url + "&to=" + encodeURIComponent(to.pathname + to.search + to.hash));
  } catch {
    location.assign(a.href);
  }
}
