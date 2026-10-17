import { describe, expect, it } from 'vitest';
import type { TransmitAnswer } from '../api';
import { FakeClock, settle } from './fakes';
import { ReportQueue, reportTimeout, type ReportEvent } from './reports';

function setup(answers: TransmitAnswer[]) {
  const clock = new FakeClock();
  const sent: string[] = [];
  const events: ReportEvent[] = [];
  let inFlight = 0;
  let overlapped = false;
  const queue = new ReportQueue({
    clock,
    onEvent: (e) => events.push(e),
    send: async (state) => {
      inFlight++;
      if (inFlight > 1) overlapped = true;
      sent.push(state);
      await Promise.resolve();
      inFlight--;
      return answers.shift() ?? 'ok';
    },
  });
  return { clock, queue, sent, events, overlapped: () => overlapped };
}

describe('reports', () => {
  it('go one at a time, in order', async () => {
    const t = setup([]);
    t.queue.push('started');
    t.queue.push('stopped');
    t.queue.push('started');
    await settle();
    expect(t.sent).toEqual(['started', 'stopped', 'started']);
    expect(t.overlapped()).toBe(false);
  });

  it("tells each report's caller once it is answered, or dropped by a reset", async () => {
    const t = setup(['rate_limited']);
    const done: string[] = [];
    t.queue.push('started', () => done.push('started'));
    t.queue.push('stopped', () => done.push('stopped'));
    expect(done).toEqual([]);
    await settle();
    expect(done).toEqual(['started', 'stopped']);
    const held = setup([]);
    held.queue.push('started', () => done.push('dropped'));
    held.queue.reset();
    expect(done).toEqual(['started', 'stopped', 'dropped']);
  });

  it('a failed report is retried in place, before the next one', async () => {
    const t = setup(['failed', 'ok']);
    t.queue.push('started');
    t.queue.push('stopped');
    await settle();
    expect(t.sent).toEqual(['started']);
    t.clock.advance(250);
    await settle();
    expect(t.sent).toEqual(['started', 'started', 'stopped']);
    expect(t.events).toContain('retrying');
  });

  it('is given up on after the retries, and the queue is dropped', async () => {
    const t = setup(['failed', 'failed', 'failed']);
    t.queue.push('started');
    t.queue.push('stopped');
    await settle();
    t.clock.advance(250);
    await settle();
    t.clock.advance(750);
    await settle();
    expect(t.sent).toEqual(['started', 'started', 'started']);
    expect(t.events.at(-1)).toBe('gave_up');
    t.clock.advance(10_000);
    await settle();
    expect(t.sent).toHaveLength(3);
  });

  it('is not retried on voice_no_session; the owner goes back to conchd', async () => {
    const t = setup(['no_session']);
    t.queue.push('started');
    t.queue.push('stopped');
    await settle();
    expect(t.sent).toEqual(['started']);
    expect(t.events).toEqual(['no_session']);
  });

  it('shows a rate limit and goes on with the next', async () => {
    const t = setup(['rate_limited', 'ok']);
    t.queue.push('started');
    t.queue.push('stopped');
    await settle();
    expect(t.sent).toEqual(['started', 'stopped']);
    expect(t.events).toEqual(['rate_limited', 'ok']);
  });

  it('a 401 stops reporting', async () => {
    const t = setup(['unauthorized']);
    t.queue.push('started');
    await settle();
    expect(t.events).toEqual(['unauthorized']);
  });

  it('drain waits for the queue, and gives up after its limit', async () => {
    const t = setup([]);
    t.queue.push('started');
    let done = false;
    void t.queue.drain(1000).then(() => {
      done = true;
    });
    await settle();
    expect(done).toBe(true);
  });

  it('a report that never answers is aborted after the time limit, retried, given up on, and the queue moves on', async () => {
    const clock = new FakeClock();
    const events: ReportEvent[] = [];
    const sent: string[] = [];
    const signals: AbortSignal[] = [];
    let answering = false;
    const queue = new ReportQueue({
      clock,
      onEvent: (e) => events.push(e),
      send: (state, signal) => {
        sent.push(state);
        signals.push(signal);
        // Never answers, and ignores the signal, until answering is set.
        return answering ? Promise.resolve('ok') : new Promise<TransmitAnswer>(() => undefined);
      },
    });
    queue.push('started');
    queue.push('stopped');
    await settle();
    expect(sent).toEqual(['started']);
    clock.advance(reportTimeout - 1);
    await settle();
    expect(signals[0]?.aborted).toBe(false);
    clock.advance(1);
    await settle();
    expect(signals[0]?.aborted).toBe(true);
    expect(events).toEqual(['retrying']);
    clock.advance(250);
    await settle();
    expect(sent).toEqual(['started', 'started']);
    clock.advance(reportTimeout);
    await settle();
    clock.advance(750);
    await settle();
    expect(sent).toEqual(['started', 'started', 'started']);
    clock.advance(reportTimeout);
    await settle();
    expect(signals.every((s) => s.aborted)).toBe(true);
    expect(events).toEqual(['retrying', 'retrying', 'gave_up']);
    // The unsent 'stopped' was dropped with the connection, and nothing waits
    // on a timer any more.
    expect(clock.pending).toBe(0);
    // The queue is not stuck: the next report goes out.
    answering = true;
    queue.push('started');
    await settle();
    expect(sent).toEqual(['started', 'started', 'started', 'started']);
    expect(events.at(-1)).toBe('ok');
  });

  it('a send that throws counts as failed and is retried', async () => {
    const clock = new FakeClock();
    const events: ReportEvent[] = [];
    let calls = 0;
    const queue = new ReportQueue({
      clock,
      onEvent: (e) => events.push(e),
      send: () => {
        calls++;
        return calls === 1 ? Promise.reject(new Error('boom')) : Promise.resolve('ok');
      },
    });
    queue.push('started');
    await settle();
    clock.advance(250);
    await settle();
    expect(calls).toBe(2);
    expect(events).toEqual(['retrying', 'ok']);
    expect(clock.pending).toBe(0);
  });
});
