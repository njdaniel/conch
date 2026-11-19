/// <reference types="node" />
import test from 'node:test';
import assert from 'node:assert/strict';
import { droppedLimit, settleDropped, type DroppedEngine, type Settled } from '../src/logic/dropped';
import { FakeClock, settle } from './fakes';

// An engine as livekit-client 2.22.3 behaves (#1972): close() marks it closed,
// and a reconnect in flight reopens it when its signal join is answered
// (rejoin), then ends when its attempt finishes (finish).
class FakeEngine implements DroppedEngine {
  closed = false;
  reconnecting = false;
  readonly log: string[] = [];
  leave(): Promise<void> {
    this.log.push('leave');
    return Promise.resolve();
  }
  close(): Promise<void> {
    this.log.push('close');
    this.closed = true;
    return Promise.resolve();
  }
  rejoin(): void {
    this.closed = false;
  }
}

function rig(engine: FakeEngine) {
  const clock = new FakeClock();
  let result: Settled | null = null;
  void settleDropped(engine, {
    wait: (ms) => new Promise((resolve) => clock.setTimeout(resolve, ms)),
    now: () => clock.now,
  }).then((r) => {
    result = r;
  });
  return { clock, result: () => result };
}

const table: { name: string; start: Partial<FakeEngine>; run: (e: FakeEngine, clock: FakeClock) => Promise<void>; want: Settled; log: string[] }[] = [
  {
    name: 'an open engine with nothing in flight: leave, close, settled at once',
    start: {},
    run: async () => undefined,
    want: 'settled',
    log: ['leave', 'close'],
  },
  {
    name: 'an engine the SDK already closed, nothing in flight: nothing to do',
    start: { closed: true },
    run: async () => undefined,
    want: 'settled',
    log: [],
  },
  {
    // The repro: the server's leave closed the engine during a restart, and
    // the restart's join then reopened it on the old token.
    name: 'closed mid-restart, reopened by the restart\'s join: closed again, settled when the attempt ends',
    start: { closed: true, reconnecting: true },
    run: async (e, clock) => {
      clock.advance(300);
      await settle();
      e.rejoin();
      clock.advance(100);
      await settle();
      assert.equal(e.closed, true, 'closed again as soon as it came back');
      e.reconnecting = false;
      clock.advance(100);
      await settle();
    },
    want: 'settled',
    log: ['leave', 'close'],
  },
  {
    name: 'an attempt that never ends: given up at the limit, the engine left closed',
    start: { reconnecting: true },
    run: async (_e, clock) => {
      for (let t = 0; t <= droppedLimit; t += 1000) {
        clock.advance(1000);
        await settle();
      }
    },
    want: 'timed_out',
    log: ['leave', 'close'],
  },
];

for (const c of table) {
  test(`dropped: ${c.name}`, async () => {
    const e = Object.assign(new FakeEngine(), c.start);
    const r = rig(e);
    await settle();
    await c.run(e, r.clock);
    await settle();
    assert.equal(r.result(), c.want);
    assert.deepEqual(e.log, c.log);
    assert.equal(e.closed, true);
  });
}

test('dropped: not settled while the attempt is in flight, even though the engine reads closed', async () => {
  const e = Object.assign(new FakeEngine(), { closed: true, reconnecting: true });
  const r = rig(e);
  r.clock.advance(5000);
  await settle();
  assert.equal(r.result(), null);
});

test('dropped: a leave or close that fails is retried, and never throws', async () => {
  const e = new FakeEngine();
  let fails = 1;
  e.close = () => {
    e.log.push('close');
    if (fails-- > 0) return Promise.reject(new Error('busy'));
    e.closed = true;
    return Promise.resolve();
  };
  e.leave = () => (e.log.push('leave'), Promise.reject(new Error('socket gone')));
  const r = rig(e);
  await settle();
  r.clock.advance(100);
  await settle();
  assert.equal(r.result(), 'settled');
  assert.deepEqual(e.log, ['leave', 'close', 'leave', 'close']);
});
