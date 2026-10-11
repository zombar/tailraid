// The folder picker (docs/13 §Design "Folder picker"): a dialog that browses live folders
// and resolves with the chosen path, or null.

import { h, icon } from "./dom.ts";
import { ls } from "./api.ts";
import { basename, dirname } from "./util.ts";

export interface PickOptions {
  start: string;
  /** disabled gives the reason a folder cannot be chosen, or null when it can. */
  disabled: (path: string) => string | null;
  /** action labels the confirm button for the current folder. */
  action: (path: string) => string;
}

export function pick(opts: PickOptions): Promise<string | null> {
  return new Promise((resolve) => {
    const dlg = h("dialog", { class: "picker", "aria-label": "move to" });
    let cur = opts.start;
    const crumbs = h("div", { class: "crumbs" });
    const list = h("ul", { class: "picker-list" });
    const confirm = h("button", { type: "button" }, "");
    const cancel = h("button", { type: "button" }, "Cancel");
    dlg.append(crumbs, list, h("div", { class: "picker-actions" }, cancel, confirm));
    const close = (v: string | null) => {
      dlg.close();
      dlg.remove();
      resolve(v);
    };
    cancel.addEventListener("click", () => close(null));
    confirm.addEventListener("click", () => {
      if (!opts.disabled(cur)) close(cur);
    });
    dlg.addEventListener("cancel", (ev) => {
      ev.preventDefault();
      close(null);
    });
    dlg.addEventListener("keydown", (ev) => {
      if (ev.key === "Enter" && !(ev.target instanceof HTMLButtonElement)) {
        ev.preventDefault();
        if (!opts.disabled(cur)) close(cur);
      }
    });
    const draw = async () => {
      crumbs.replaceChildren(h("a", { href: "#", onClick: (ev: Event) => { ev.preventDefault(); go("/"); } }, "/"));
      const parts = cur.split("/").filter(Boolean);
      let acc = "";
      parts.forEach((p, i) => {
        acc += "/" + p;
        if (i > 0) crumbs.append(h("span", { class: "sep" }, "/"));
        const target = acc;
        if (i === parts.length - 1) crumbs.append(h("span", null, p));
        else crumbs.append(h("a", { href: "#", onClick: (ev: Event) => { ev.preventDefault(); go(target); } }, p));
      });
      const reason = opts.disabled(cur);
      confirm.textContent = opts.action(cur);
      confirm.disabled = reason !== null;
      confirm.title = reason ?? "";
      list.replaceChildren();
      if (cur !== "/") list.append(h("li", { onClick: () => go(dirname(cur)) }, icon("dir"), ".."));
      try {
        const r = await ls(cur);
        for (const e of r.entries) {
          if (e.type !== "dir" || e.deleted_at) continue;
          const path = cur === "/" ? "/" + e.name : cur + "/" + e.name;
          const why = opts.disabled(path);
          const li = h("li", why ? { "aria-disabled": "true", title: why } : null, icon("dir"), e.name);
          if (!why) li.addEventListener("click", () => go(path));
          list.append(li);
        }
      } catch (e) {
        list.append(h("li", { "aria-disabled": "true" }, "could not list: " + (e instanceof Error ? e.message : String(e))));
      }
    };
    const go = (path: string) => {
      cur = path;
      void draw();
    };
    document.body.append(dlg);
    dlg.showModal();
    void draw();
    void basename; // the current folder's name is in the crumbs
  });
}
