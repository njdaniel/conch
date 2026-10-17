// The live feed of one open channel: keeps a socket open, and when it closes
// reconnects with a fresh ticket and resumes from the newest id held, waiting
// 1 s, then twice as long each time up to 30 s (the TUI's backoff). It owns no
// timer, socket or ticket itself: the host gives it the clock and `open`, so
// the tests drive it with a fake of each.
import type { ApiResult } from '../api';
import { heartbeatV1Schema } from '../schema/generated';

// A close the server meant: policy violation, which conchd uses for lost
// membership, a revoked credential and a reader too slow. It is not retried.
export const policyClose = 1008;

// How long a socket may be silent before it is taken for dead (the Go
// schema.HeartbeatTimeoutSeconds). The server sends a heartbeat frame every
// 25 s, so one lost frame is survived. A browser cannot see the pings that
// would tell it, which is why the frame exists.
export const silenceTimeout = 60_000;

// isHeartbeat reports whether a frame is the server's heartbeat. It looks at
// short text frames only: a heartbeat is a few dozen bytes, and a message is
// not parsed twice.
export function isHeartbeat(data: unknown): boolean {
  if (typeof data !== 'string' || data.length > 64) return false;
  try {
    // By schema alone, so a field a later server adds does not turn it into
    // a frame the page has to make sense of.
    return (JSON.parse(data) as { schema?: unknown } | null)?.schema === heartbeatV1Schema;
  } catch {
    return false;
  }
}

export const firstDelay = 1000;
export const maxDelay = 30_000;

export interface SocketEvents {
  readonly onOpen: () => void;
  readonly onFrame: (data: unknown) => void;
  // reason is the close frame's text; conchd says why in it ("session ended").
  readonly onClose: (code: number, reason?: string) => void;
}

export interface FeedSocket {
  close: () => void;
}

// open asks for a ticket and opens the socket (src/api.ts openSocket). A
// failure is the ticket call's.
export type OpenSocket = (channel: string, after: number, events: SocketEvents) => Promise<ApiResult<FeedSocket>>;

export interface Clock {
  setTimeout: (run: () => void, ms: number) => unknown;
  clearTimeout: (handle: unknown) => void;
}

// What the host is told. `waiting` is between attempts; `denied` is a 1008
// close, after which nothing is retried until the host calls start again;
// `unauthorized` is a ticket refused with 401.
export type FeedStatus = 'connecting' | 'open' | 'waiting' | 'denied' | 'unauthorized';

export interface FeedOptions {
  readonly channel: string;
  readonly open: OpenSocket;
  readonly clock: Clock;
  // after is read at each attempt: the newest id held then.
  readonly after: () => number;
  readonly onFrame: (data: unknown) => void;
  readonly onStatus: (status: FeedStatus) => void;
  // How long a silent attempt may last before it is dropped and retried;
  // silenceTimeout when absent. Any frame, a heartbeat included, restarts it.
  readonly silence?: number;
}

export class Feed {
  private readonly options: FeedOptions;
  private socket: FeedSocket | null = null;
  private timer: unknown = null;
  private watchdog: unknown = null;
  private delay = firstDelay;
  // Each attempt has a number; events from an older one are ignored.
  private attempt = 0;
  private stopped = false;

  constructor(options: FeedOptions) {
    this.options = options;
  }

  // start (re)starts at once, dropping any socket or wait in progress.
  start(): void {
    this.stopped = false;
    this.clearTimer();
    this.attemptNow();
  }

  // retryLater goes through the backoff, reporting `waiting`. It is how a host
  // retries after a `denied` it could not settle: never start() from the
  // status callback, which would loop as fast as the server answers.
  retryLater(): void {
    if (this.stopped) return;
    this.clearTimer();
    this.wait();
  }

  // stop ends the feed and reports nothing more.
  stop(): void {
    this.stopped = true;
    this.attempt++;
    this.clearTimer();
    this.clearWatchdog();
    this.closeSocket();
  }

  // arm (re)starts the silence watchdog of attempt `mine`. When it fires the
  // socket is closed and the feed goes through the backoff like any other
  // loss; the old socket's close event is ignored because wait() moves the
  // attempt number on. Nothing is resent: the next attempt asks for the ids
  // after the newest held, as after any close.
  private arm(mine: number): void {
    this.clearWatchdog();
    this.watchdog = this.options.clock.setTimeout(() => {
      this.watchdog = null;
      if (this.stopped || mine !== this.attempt) return;
      this.closeSocket();
      this.wait();
    }, this.options.silence ?? silenceTimeout);
  }

  private clearWatchdog(): void {
    if (this.watchdog !== null) this.options.clock.clearTimeout(this.watchdog);
    this.watchdog = null;
  }

  private clearTimer(): void {
    if (this.timer !== null) this.options.clock.clearTimeout(this.timer);
    this.timer = null;
  }

  private closeSocket(): void {
    const socket = this.socket;
    this.socket = null;
    socket?.close();
  }

  private attemptNow(): void {
    this.closeSocket();
    const mine = ++this.attempt;
    this.options.onStatus('connecting');
    const live = () => !this.stopped && mine === this.attempt;
    // Covers the ticket request and the handshake too: a connection that never
    // completes is as lost as one that goes quiet.
    this.arm(mine);
    void this.options
      .open(this.options.channel, this.options.after(), {
        onOpen: () => {
          if (!live()) return;
          this.delay = firstDelay;
          this.arm(mine);
          this.options.onStatus('open');
        },
        onFrame: (data) => {
          if (!live()) return;
          this.arm(mine);
          if (!isHeartbeat(data)) this.options.onFrame(data);
        },
        onClose: (code) => {
          if (!live()) return;
          this.clearWatchdog();
          this.socket = null;
          if (code === policyClose) {
            this.attempt++;
            this.options.onStatus('denied');
          } else {
            this.wait();
          }
        },
      })
      .then((result) => {
        if (this.stopped || mine !== this.attempt) {
          // Stopped while the ticket was being fetched: do not leave a socket.
          if (result.ok) result.value.close();
          return;
        }
        if (result.ok) {
          this.socket = result.value;
        } else if (result.failure === 'unauthorized') {
          this.attempt++;
          this.clearWatchdog();
          this.options.onStatus('unauthorized');
        } else {
          this.wait();
        }
      });
  }

  private wait(): void {
    this.attempt++;
    this.clearWatchdog();
    this.options.onStatus('waiting');
    const delay = this.delay;
    this.delay = Math.min(this.delay * 2, maxDelay);
    this.timer = this.options.clock.setTimeout(() => {
      this.timer = null;
      if (!this.stopped) this.attemptNow();
    }, delay);
  }
}
