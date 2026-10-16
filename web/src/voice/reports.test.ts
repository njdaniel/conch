import { describe, expect, it } from 'vitest';
import type { TransmitAnswer } from '../api';
import { FakeClock, settle } from './fakes';
import { ReportQueue, type ReportEvent } from './reports';

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
});
