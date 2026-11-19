// People presence is a signed-in client session. Android may suspend the JS
// timer in the background, so the server can mark it offline. Resume sends a
// heartbeat immediately to bring it back online.
import type { Clock } from '../logic/clock';
import type { ApiClient, SessionRegistration } from './client';

type SessionApi = Pick<ApiClient, 'createSession' | 'heartbeatSession'>;
const heartbeatEvery = 15_000; // Below the server's default 30 s stale threshold.
const heartbeatDeadline = 5_000;

export class PeopleSession {
  private run: object | null = null;
  private session: SessionRegistration | null = null;
  private heartbeat: AbortController | null = null;
  private timer: unknown = null;
  private pending = false;

  constructor(private readonly api: SessionApi, private readonly clock: Clock) {}

  start(): void {
    this.run ??= {};
    this.refresh();
  }

  refresh(): void {
    if (this.run === null || this.pending) return;
    this.clearTimer();
    this.pending = true;
    if (this.session === null) void this.register(this.run);
    else void this.beat(this.session.id);
  }

  stop(): void {
    this.run = null;
    this.heartbeat?.abort();
    this.heartbeat = null;
    this.clearTimer();
    this.pending = false;
    if (this.session !== null) void this.session.end();
    this.session = null;
  }

  private clearTimer(): void {
    if (this.timer !== null) this.clock.clearTimeout(this.timer);
    this.timer = null;
  }

  private async register(run: object): Promise<void> {
    const result = await this.api.createSession();
    if (run !== this.run) {
      if (result.ok) void result.value.end();
      return;
    }
    if (result.ok) this.session = result.value;
    this.scheduleNext();
  }

  private async beat(id: number): Promise<void> {
    const request = new AbortController();
    this.heartbeat = request;
    this.timer = this.clock.setTimeout(() => {
      // Reopen the loop at the deadline even if the fetch never settles.
      request.abort();
      this.scheduleNext();
    }, heartbeatDeadline);
    const result = await this.api.heartbeatSession(id, request.signal);
    if (request.signal.aborted) return;
    this.clearTimer();
    // A missing session was ended. A network failure keeps the ID so the
    // next heartbeat can revive the existing row.
    if (!result.ok && result.status === 404) this.session = null;
    this.scheduleNext();
  }

  private scheduleNext(): void {
    this.heartbeat = null;
    this.pending = false;
    this.timer = this.clock.setTimeout(() => {
      this.refresh();
    }, heartbeatEvery);
  }
}
