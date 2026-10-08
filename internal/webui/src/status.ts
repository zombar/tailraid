// The status poller: one fetch every 5 s while the tab is visible, a client-side ring of
// the last 10 minutes of degraded + pending counts for the sparkline (docs/13 §1).

import { getStatus, type Report } from "./api.ts";

export interface Sample {
  t: number;
  degraded: number;
  pending: number;
}

const INTERVAL = 5000;
const HISTORY = 120;

type Listener = () => void;

class Status {
  current: Report | null = null;
  error: string | null = null;
  history: Sample[] = [];
  private listeners = new Set<Listener>();
  private timer: number | undefined;
  private started = false;

  subscribe(fn: Listener): () => void {
    this.listeners.add(fn);
    if (!this.started) this.start();
    return () => this.listeners.delete(fn);
  }

  async refresh(): Promise<void> {
    try {
      const r = await getStatus();
      this.current = r;
      this.error = null;
      this.history.push({ t: Date.now(), degraded: r.volume.degraded_chunks, pending: r.volume.repair_pending });
      if (this.history.length > HISTORY) this.history.splice(0, this.history.length - HISTORY);
    } catch (e) {
      this.error = e instanceof Error ? e.message : String(e);
    }
    for (const fn of this.listeners) fn();
  }

  private start(): void {
    this.started = true;
    document.addEventListener("visibilitychange", () => {
      if (document.visibilityState === "hidden") this.stopTimer();
      else void this.loop();
    });
    void this.loop();
  }

  private async loop(): Promise<void> {
    this.stopTimer();
    await this.refresh();
    if (document.visibilityState !== "hidden") this.timer = window.setTimeout(() => void this.loop(), INTERVAL);
  }

  private stopTimer(): void {
    if (this.timer !== undefined) window.clearTimeout(this.timer);
    this.timer = undefined;
  }
}

export const status = new Status();
