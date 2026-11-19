// People presence is a signed-in client session. Android may suspend the JS
// timer in the background, so the server can mark it offline. Resume sends a
// heartbeat immediately to bring it back online.
import type { Clock } from '../logic/clock';
import type { ApiClient } from './client';

type SessionApi = Pick<ApiClient, 'createSession' | 'heartbeatSession' | 'endSession'>;
const heartbeatEvery = 15_000; // Below the server's default 30 s stale threshold.

export class PeopleSession {
  private run: AbortController | null = null;
  private id: number | null = null;
  private timer: unknown = null;
  private pending = false;

  constructor(private readonly api: SessionApi, private readonly clock: Clock) {}

  start(): void {
    this.run ??= new AbortController();
    this.refresh();
  }

  refresh(): void {
    if (this.run === null || this.pending) return;
    this.clearTimer();
    void this.tick(this.run);
  }

  stop(): void {
    this.run?.abort();
    this.run = null;
    this.clearTimer();
    this.pending = false;
    if (this.id !== null) void this.api.endSession(this.id);
    this.id = null;
  }

  private clearTimer(): void {
    if (this.timer !== null) this.clock.clearTimeout(this.timer);
    this.timer = null;
  }

  private async tick(run: AbortController): Promise<void> {
    this.pending = true;
    const result = this.id === null
      ? await this.api.createSession(run.signal)
      : await this.api.heartbeatSession(this.id, run.signal);
    if (run !== this.run) return;
    this.pending = false;
    if (result.ok) this.id = result.value.id;
    // A missing session was ended. A network failure keeps the ID so the
    // next heartbeat can revive the existing row.
    else if (result.status === 404) this.id = null;
    this.timer = this.clock.setTimeout(() => this.refresh(), heartbeatEvery);
  }
}
