// Transmit reports: each gate open and close is reported to conchd, one at a
// time and in order, because conchd takes them in arrival order (conch-voice.md
// section 6, "Client side"). Audio never waits for a report.
//
// A failed report is retried briefly in place. When it is given up on, the
// owner drops the connection, so a request cannot be delivered late. 409
// (no_session) is not retried: the owner goes back to conchd. 429 is shown.
// An attempt that has no answer after reportTimeout is aborted and counts as
// failed, so a report that never answers is retried and then given up on, and
// the queue is never stuck behind it.
import type { TransmitAnswer } from '../api';
import type { Clock } from '../state/feed';
import type { VoiceTransmitState } from '../schema/voice';

export type ReportEvent = 'ok' | 'retrying' | 'rate_limited' | 'gave_up' | 'no_session' | 'unauthorized';

export interface ReportQueueOptions {
  readonly send: (state: VoiceTransmitState, signal: AbortSignal) => Promise<TransmitAnswer>;
  readonly clock: Clock;
  readonly onEvent: (event: ReportEvent) => void;
  // The waits before each retry; the number of attempts is one more.
  readonly retryDelays?: readonly number[];
}

export const defaultRetryDelays: readonly number[] = [250, 750];

// How long one attempt may take, on the injected clock.
export const reportTimeout = 4000;

export class ReportQueue {
  private readonly options: ReportQueueOptions;
  private queue: VoiceTransmitState[] = [];
  private busy = false;
  private generation = 0;
  private controller: AbortController | null = null;
  private timer: unknown = null;
  private deadline: unknown = null;
  private waiters: (() => void)[] = [];

  constructor(options: ReportQueueOptions) {
    this.options = options;
  }

  push(state: VoiceTransmitState): void {
    this.queue.push(state);
    if (!this.busy) void this.pump();
  }

  // reset drops everything unsent and abandons the request in flight.
  reset(): void {
    this.generation++;
    this.queue = [];
    this.busy = false;
    if (this.timer !== null) this.options.clock.clearTimeout(this.timer);
    this.timer = null;
    if (this.deadline !== null) this.options.clock.clearTimeout(this.deadline);
    this.deadline = null;
    this.controller?.abort();
    this.controller = null;
    this.release();
  }

  // drain resolves when nothing is queued or in flight, or after `limit` ms.
  drain(limit: number): Promise<void> {
    if (!this.busy && this.queue.length === 0) return Promise.resolve();
    return new Promise((resolve) => {
      const done = () => {
        this.waiters = this.waiters.filter((w) => w !== done);
        this.options.clock.clearTimeout(handle);
        resolve();
      };
      const handle = this.options.clock.setTimeout(done, limit);
      this.waiters.push(done);
    });
  }

  private release(): void {
    const waiting = this.waiters;
    this.waiters = [];
    for (const w of waiting) w();
  }

  private wait(ms: number): Promise<void> {
    return new Promise((resolve) => {
      this.timer = this.options.clock.setTimeout(() => {
        this.timer = null;
        resolve();
      }, ms);
    });
  }

  // attempt sends once. It answers 'failed' if the send has not answered
  // within reportTimeout, aborting the request, or if the send throws.
  private attempt(state: VoiceTransmitState, controller: AbortController): Promise<TransmitAnswer> {
    const clock = this.options.clock;
    return new Promise((resolve) => {
      let settled = false;
      const finish = (answer: TransmitAnswer) => {
        if (settled) return;
        settled = true;
        if (this.deadline === handle) this.deadline = null;
        clock.clearTimeout(handle);
        resolve(answer);
      };
      const handle = clock.setTimeout(() => {
        controller.abort();
        finish('failed');
      }, reportTimeout);
      this.deadline = handle;
      this.options.send(state, controller.signal).then(finish, () => {
        finish('failed');
      });
    });
  }

  private async pump(): Promise<void> {
    const mine = this.generation;
    this.busy = true;
    const delays = this.options.retryDelays ?? defaultRetryDelays;
    while (this.queue.length > 0 && mine === this.generation) {
      const state = this.queue[0];
      if (state === undefined) break;
      let answer: TransmitAnswer = 'failed';
      for (let attempt = 0; attempt <= delays.length; attempt++) {
        const controller = new AbortController();
        this.controller = controller;
        answer = await this.attempt(state, controller);
        if (mine !== this.generation) return;
        if (answer !== 'failed') break;
        const delay = delays[attempt];
        if (delay === undefined) break;
        this.options.onEvent('retrying');
        await this.wait(delay);
        if (mine !== this.generation) return;
      }
      this.queue.shift();
      switch (answer) {
        case 'ok':
          this.options.onEvent('ok');
          break;
        case 'rate_limited':
          this.options.onEvent('rate_limited');
          break;
        case 'failed':
          this.reset();
          this.options.onEvent('gave_up');
          return;
        case 'no_session':
          this.reset();
          this.options.onEvent('no_session');
          return;
        case 'unauthorized':
          this.reset();
          this.options.onEvent('unauthorized');
          return;
      }
    }
    if (mine === this.generation) {
      this.busy = false;
      this.release();
    }
  }
}
