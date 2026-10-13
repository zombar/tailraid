// The folder menu (docs/13 §4, I-163, I-162): Protect, Mark nsfw and Keep on this
// device, each a checkbox row; the rows explain themselves and are disabled with a
// reason on the root and inside the trash.

import { h, toast } from "./dom.ts";
import { getPins, ls, pin, setFlags, type DirInfo, type Entry } from "./api.ts";
import { basename, bytes, dirname } from "./util.ts";
import { status } from "./status.ts";

export interface MenuContext {
  path: string;
  inTrash: boolean;
  dir: DirInfo;
  onChange: () => void;
}

export function menu(ctx: MenuContext): HTMLElement {
  const body = h("div", { class: "menu-body" });
  const details = h("details", { class: "menu" }, h("summary", null, "Folder"), body);
  const isRoot = ctx.path === "/";
  const row = (label: string, note: string, checked: boolean, disabled: string | null, onToggle: (on: boolean) => Promise<unknown>) => {
    const box = h("input", { type: "checkbox" }) as HTMLInputElement;
    box.checked = checked;
    const noteEl = h("span", { class: "small dim" }, disabled ?? note);
    const lab = h("label", { class: "menu-row" }, box, h("span", null, label), noteEl);
    if (disabled) {
      box.disabled = true;
      lab.setAttribute("aria-disabled", "true");
    }
    box.addEventListener("change", () => {
      box.disabled = true;
      onToggle(box.checked).then(
        () => ctx.onChange(),
        (e: unknown) => {
          box.checked = !box.checked;
          box.disabled = false;
          toast(label + ": " + (e instanceof Error ? e.message : String(e)), "bad");
        },
      );
    });
    return lab;
  };
  // the Library row: a five-way select, the Media tab lists the folder as films, shows,
  // music or games (docs/18 §9)
  const libraryRow = (current: string, why: string | null) => {
    const select = h("select", { "aria-label": "library kind" }) as HTMLSelectElement;
    for (const word of ["none", "movies", "tv", "music", "roms"]) select.append(h("option", { value: word, selected: (current || "none") === word ? "" : undefined }, word));
    const note = h("span", { class: "small dim" }, why ?? "the Media tab lists this folder as films, shows, music or games");
    const lab = h("label", { class: "menu-row library" }, h("span", null, "Library"), select, note);
    if (why) {
      select.disabled = true;
      lab.setAttribute("aria-disabled", "true");
    }
    select.addEventListener("change", () => {
      select.disabled = true;
      setFlags(ctx.path, { library: select.value }).then(
        () => ctx.onChange(),
        (e: unknown) => {
          select.value = current || "none";
          select.disabled = false;
          toast("Library: " + (e instanceof Error ? e.message : String(e)), "bad");
        },
      );
    });
    return lab;
  };
  const fill = async () => {
    body.replaceChildren(h("div", { class: "loading" }, "loading"));
    let own: Entry | undefined;
    let pinned: { bytes: number; cached_bytes: number } | undefined;
    if (!isRoot && !ctx.inTrash) {
      try {
        own = (await ls(dirname(ctx.path))).entries.find((e) => e.name === basename(ctx.path) && !e.deleted_at);
      } catch {
        // the rows fall back to the effective flags
      }
      try {
        pinned = (await getPins()).pins.find((p) => p.path === ctx.path);
      } catch {
        // pins may not be wired
      }
    }
    const inherited = (eff: boolean, mine: boolean | undefined) => (eff && !mine ? "inherited from a parent folder: change it there" : null);
    const why = isRoot ? "not for the root" : ctx.inTrash ? "not for a deleted folder" : null;
    const self = status.current?.nodes.find((n) => n.self)?.name ?? "this device";
    body.replaceChildren(
      row("Protect", "nothing in it can be deleted, replaced or moved out, from any device", own?.protected ?? ctx.dir.protected, why ?? inherited(ctx.dir.protected, own?.protected), (on) => setFlags(ctx.path, { protected: on })),
      row("Mark nsfw", "thumbnails and previews are blurred until tapped", own?.nsfw ?? ctx.dir.nsfw, why ?? inherited(ctx.dir.nsfw, own?.nsfw), (on) => setFlags(ctx.path, { nsfw: on })),
      row(
        "Keep on this device",
        pinned ? `${self} holds every chunk in its cache · ${bytes(pinned.cached_bytes)} of ${bytes(pinned.bytes)} here` : `${self} fetches every chunk under it into its cache and keeps it there`,
        ctx.dir.pinned,
        why,
        (on) => pin(ctx.path, on),
      ),
      libraryRow(own?.library ?? "", why ?? (ctx.dir.library && !own?.library ? `inside a ${ctx.dir.library} library: this folder is listed as part of it` : null)),
    );
  };
  details.addEventListener("toggle", () => {
    if (details.open) void fill();
  });
  return details;
}
