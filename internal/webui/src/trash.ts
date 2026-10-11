// The trash banner (docs/13 §4, I-158): above a deleted folder's listing and a deleted
// file's view; Restore puts the entry back where it was, or under another name or into
// another folder after a 409; Delete now purges it.

import { h, toast } from "./dom.ts";
import { ApiError, purge, restore, type TrashEntry } from "./api.ts";
import { pick } from "./picker.ts";
import { basename, bytes, count, daysAgo, daysUntil, dirname, encodePathForHash } from "./util.ts";

export interface BannerInfo {
  root: TrashEntry;
  items?: number;
  bytes?: number;
}

export function banner(info: BannerInfo): HTMLElement {
  const r = info.root;
  const text = h("span");
  const parts = [`Deleted ${daysAgo(r.deleted_at)}`];
  if (r.purge_at) parts.push(`gone for good ${daysUntil(r.purge_at)}`);
  if (info.items !== undefined) parts.push(`${count(info.items)} ${info.items === 1 ? "item" : "items"}`);
  if (info.bytes !== undefined && info.bytes > 0) parts.push(bytes(info.bytes));
  text.textContent = parts.join(" · ");
  const restoreBtn = h("button", { type: "button" }, "Restore");
  const purgeBtn = h("button", { type: "button" }, "Delete now");
  const el = h("div", { class: "banner deleted" }, text, h("span", { class: "actions" }, restoreBtn, purgeBtn));

  const landed = (path: string) => {
    toast(`Restored ${basename(path)}`, "ok");
    location.hash = "#/browse" + encodePathForHash(dirname(path));
  };
  const fail = (e: unknown) => toast("Restore: " + (e instanceof Error ? e.message : String(e)), "bad");

  const asForm = () => {
    text.replaceChildren(h("span", { class: "mono" }, r.name), " is taken in ", h("span", { class: "mono" }, basename(dirname(r.path)) || "/"), " · restore under another name");
    const dot = r.name.lastIndexOf(".");
    const stem = dot > 0 ? r.name.slice(0, dot) : r.name;
    const extn = dot > 0 ? r.name.slice(dot) : "";
    const input = h("input", { type: "text", value: `${stem} (restored)${extn}`, "aria-label": "new name" }) as HTMLInputElement;
    const form = h("form", { class: "inline-form rename" }, input, h("button", { type: "submit" }, "Restore as"), h("button", { type: "button", onClick: () => el.remove() }, "Cancel"));
    form.addEventListener("submit", (ev) => {
      ev.preventDefault();
      const name = input.value.trim();
      if (!name) return;
      restore(r.ino, { name }).then((res) => landed(res.path), fail);
    });
    el.append(form);
    queueMicrotask(() => {
      input.focus();
      input.setSelectionRange(0, stem.length);
    });
  };

  const elsewhere = async () => {
    const to = await pick({ start: "/", disabled: () => null, action: (p) => `Restore into ${basename(p) || "/"}` });
    if (to === null) return;
    restore(r.ino, { to }).then((res) => landed(res.path), fail);
  };

  restoreBtn.addEventListener("click", () => {
    restoreBtn.disabled = true;
    restore(r.ino).then(
      (res) => landed(res.path),
      (e: unknown) => {
        restoreBtn.disabled = false;
        if (e instanceof ApiError && e.status === 409 && e.body?.taken) asForm();
        else if (e instanceof ApiError && e.status === 409 && e.body?.in_trash) void elsewhere();
        else fail(e);
      },
    );
  });
  purgeBtn.addEventListener("click", () => {
    purgeBtn.disabled = true;
    purge({ ino: r.ino }).then(
      () => {
        toast(`Deleted ${r.name} for good`, "ok");
        location.hash = "#/browse" + encodePathForHash(dirname(r.path));
      },
      (e: unknown) => {
        purgeBtn.disabled = false;
        toast("Delete now: " + (e instanceof Error ? e.message : String(e)), "bad");
      },
    );
  });
  return el;
}

/** badge is the "deleted · 27 d" badge after a deleted entry's name. */
export function badge(purgeAt: string | undefined): HTMLElement {
  const left = purgeAt ? daysUntil(purgeAt).replace(/^in /, "") : "";
  return h("span", { class: "badge" }, left ? `deleted · ${left}` : "deleted");
}
