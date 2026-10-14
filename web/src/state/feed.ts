// The live feed of one open channel: keeps a socket open, and when it closes
// reconnects with a fresh ticket and resumes from the newest id held, waiting
// 1 s, then twice as long each time up to 30 s (the TUI's backoff). It owns no
// timer, socket or ticket itself: the host gives it the clock and `open`, so
// the tests drive it with a fake of each.
import type { ApiResult } from '../api';

// A close the server meant: policy violation, which conchd uses for lost
// membership, a revoked credential and a reader too slow. It is not retried.
export const policyClose = 1008;

export const firstDelay = 1000;
export const maxDelay = 30_000;

export interface SocketEvents {
  readonly onOpen: () => void;
  readonly onFrame: (data: unknown) => void;
  readonly onClose: (code: number) => void;
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
}

export class Feed {
  private readonly options: FeedOptions;
  private socket: FeedSocket | null = null;
  private timer: unknown = null;
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
    this.closeSocket();
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
    void this.options
      .open(this.options.channel, this.options.after(), {
        onOpen: () => {
          if (!live()) return;
          this.delay = firstDelay;
          this.options.onStatus('open');
        },
        onFrame: (data) => {
          if (live()) this.options.onFrame(data);
        },
        onClose: (code) => {
          if (!live()) return;
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
          this.options.onStatus('unauthorized');
        } else {
          this.wait();
        }
      });
  }

  private wait(): void {
    this.attempt++;
    this.options.onStatus('waiting');
    const delay = this.delay;
    this.delay = Math.min(this.delay * 2, maxDelay);
    this.timer = this.options.clock.setTimeout(() => {
      this.timer = null;
      if (!this.stopped) this.attemptNow();
    }, delay);
  }
}
