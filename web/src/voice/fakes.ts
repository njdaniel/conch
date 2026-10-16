// Test doubles for the voice client: a clock the test moves by hand. Imported
// by tests only.
import type { Clock } from '../state/feed';

export class FakeClock implements Clock {
  now = 0;
  private next = 1;
  private timers = new Map<number, { at: number; run: () => void }>();

  setTimeout = (run: () => void, ms: number): unknown => {
    const id = this.next++;
    this.timers.set(id, { at: this.now + ms, run });
    return id;
  };

  clearTimeout = (handle: unknown): void => {
    this.timers.delete(handle as number);
  };

  // advance moves time forward, running each timer due on the way in order.
  advance(ms: number): void {
    const end = this.now + ms;
    for (;;) {
      let pick: [number, { at: number; run: () => void }] | null = null;
      for (const entry of this.timers) {
        if (entry[1].at <= end && (pick === null || entry[1].at < pick[1].at)) pick = entry;
      }
      if (pick === null) break;
      this.timers.delete(pick[0]);
      this.now = Math.max(this.now, pick[1].at);
      pick[1].run();
    }
    this.now = end;
  }

  get pending(): number {
    return this.timers.size;
  }
}

// settle lets promise continuations run.
export async function settle(): Promise<void> {
  for (let i = 0; i < 10; i++) await Promise.resolve();
}
