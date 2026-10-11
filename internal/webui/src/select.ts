// The selection: names in the current folder, with shift-click ranges over the listing's
// visible order (docs/13 §Design "Select box").

export class Selection {
  private set = new Set<string>();
  private anchor: string | null = null;
  private listeners = new Set<() => void>();

  get size(): number {
    return this.set.size;
  }

  has(name: string): boolean {
    return this.set.has(name);
  }

  names(): string[] {
    return Array.from(this.set);
  }

  /** click toggles one name; with shift it selects the range from the last click in order. */
  click(name: string, shift: boolean, order: string[]): void {
    if (shift && this.anchor !== null) {
      const a = order.indexOf(this.anchor);
      const b = order.indexOf(name);
      if (a >= 0 && b >= 0) {
        for (const n of order.slice(Math.min(a, b), Math.max(a, b) + 1)) this.set.add(n);
        this.notify();
        return;
      }
    }
    if (this.set.has(name)) this.set.delete(name);
    else this.set.add(name);
    this.anchor = name;
    this.notify();
  }

  all(names: string[]): void {
    for (const n of names) this.set.add(n);
    this.notify();
  }

  clear(): void {
    this.set.clear();
    this.anchor = null;
    this.notify();
  }

  /** keep drops names that are no longer listed (after a refresh). */
  keep(names: Set<string>): void {
    let changed = false;
    for (const n of this.set) {
      if (!names.has(n)) {
        this.set.delete(n);
        changed = true;
      }
    }
    if (changed) this.notify();
  }

  subscribe(fn: () => void): () => void {
    this.listeners.add(fn);
    return () => this.listeners.delete(fn);
  }

  private notify(): void {
    for (const fn of this.listeners) fn();
  }
}
