/// <reference types="node" />
import test from 'node:test';
import assert from 'node:assert/strict';
import { Connection, droppedLimit, lingerLimit, settleDropped, type DroppedEngine, type Retired, type SettleOptions } from '../src/logic/dropped';
import { FakeClock, settle } from './fakes';

// An engine as livekit-client 2.22.3 behaves (#1972): close() marks it closed,
// and a reconnect in flight reopens it when its signal join is answered
// (rejoin), then ends when its attempt finishes (connecting = false). Sealed,
// it refuses to start a join (startJoin), as app/engine.ts makes the SDK's.
class FakeEngine implements DroppedEngine {
  closed = false;
  connecting = false;
  sealed = false;
  readonly log: string[] = [];
  seal(): void {
    this.log.push('seal');
    this.sealed = true;
  }
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
  // A restart's next join (a retry, another region): refused once sealed.
  startJoin(): boolean {
    if (this.sealed) return false;
    this.rejoin();
    return true;
  }
}

function rig(engine: DroppedEngine, extra: Partial<SettleOptions> = {}) {
  const clock = new FakeClock();
  let result: Retired | null = null;
  let lingered = false;
  void settleDropped(engine, {
    wait: (ms) => new Promise((resolve) => clock.setTimeout(resolve, ms)),
    now: () => clock.now,
    ...extra,
  }).then((r) => {
    result = r;
    void r.lingering?.then(() => {
      lingered = true;
    });
  });
  return { clock, result: (): Retired | null => result, lingered: () => lingered };
}

// run moves the fake clock in steps, letting promises run between them.
async function run(clock: FakeClock, ms: number, step = 1000): Promise<void> {
  for (let t = 0; t < ms; t += step) {
    clock.advance(Math.min(step, ms - t));
    await settle();
  }
}

const table: { name: string; start: Partial<FakeEngine>; run: (e: FakeEngine, clock: FakeClock) => Promise<void>; settled: boolean; log: string[] }[] = [
  {
    name: 'an open engine with nothing in flight: sealed, left, closed, settled at once',
    start: {},
    run: async () => undefined,
    settled: true,
    log: ['seal', 'leave', 'close'],
  },
  {
    name: 'an engine the SDK already closed, nothing in flight: sealed, nothing else',
    start: { closed: true },
    run: async () => undefined,
    settled: true,
    log: ['seal'],
  },
  {
    // The repro: the server's leave closed the engine during a restart, and
    // the restart's join then reopened it on the old token.
    name: 'closed mid-restart, reopened by the restart\'s join: closed again, settled when the attempt ends',
    start: { closed: true, connecting: true },
    run: async (e, clock) => {
      clock.advance(300);
      await settle();
      e.rejoin();
      clock.advance(100);
      await settle();
      assert.equal(e.closed, true, 'closed again as soon as it came back');
      e.connecting = false;
      clock.advance(100);
      await settle();
    },
    settled: true,
    log: ['seal', 'leave', 'close'],
  },
  {
    // The restart's retry on the legacy endpoint, or in another region: a
    // second join of the same attempt, refused by the seal.
    name: 'a restart that tries another join after the drop: refused, never reopens',
    start: { closed: true, connecting: true },
    run: async (e, clock) => {
      await run(clock, 5000, 100);
      assert.equal(e.startJoin(), false, 'the seal refuses the retry');
      e.connecting = false;
      await run(clock, 200, 100);
    },
    settled: true,
    log: ['seal'],
  },
  {
    name: 'an attempt that never ends: given up at the limit, the engine left closed, and it lingers',
    start: { connecting: true },
    run: async (_e, clock) => run(clock, droppedLimit + 1000),
    settled: false,
    log: ['seal', 'leave', 'close'],
  },
];

for (const c of table) {
  test(`dropped: ${c.name}`, async () => {
    const e = Object.assign(new FakeEngine(), c.start);
    const r = rig(e);
    await settle();
    await c.run(e, r.clock);
    await settle();
    assert.equal(r.result()?.settled, c.settled);
    assert.equal(r.result()?.lingering === null, c.settled, 'lingers only when not settled');
    assert.deepEqual(e.log, c.log);
    assert.equal(e.closed, true);
  });
}

test('dropped: sealed before anything else, the teardown included', async () => {
  const e = new FakeEngine();
  const order: string[] = [];
  const r = rig(e, {
    teardown: () => {
      order.push(e.sealed ? 'teardown after seal' : 'teardown before seal');
      return Promise.resolve();
    },
  });
  await settle();
  assert.deepEqual(order, ['teardown after seal']);
  assert.equal(r.result()?.settled, true);
});

test('dropped: not settled while the attempt is in flight, even though the engine reads closed', async () => {
  const e = Object.assign(new FakeEngine(), { closed: true, connecting: true });
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
  assert.equal(r.result()?.settled, true);
  assert.deepEqual(e.log, ['seal', 'leave', 'close', 'leave', 'close']);
});

// The limit is on the whole drop: a step that never returns cannot hold the
// next connection back past it (the review's in-memory check, #1972).
const hung: { name: string; hang: (e: FakeEngine) => Partial<SettleOptions> }[] = [
  {
    name: 'a leave that never returns',
    hang: (e) => {
      e.leave = () => (e.log.push('leave'), new Promise<void>(() => undefined));
      return {};
    },
  },
  {
    name: 'a close that never returns',
    hang: (e) => {
      e.close = () => (e.log.push('close'), new Promise<void>(() => undefined));
      return {};
    },
  },
  {
    name: 'a teardown (the SDK room\'s disconnect, the audio session) that never returns',
    hang: () => ({ teardown: () => new Promise<void>(() => undefined) }),
  },
];

for (const h of hung) {
  test(`dropped: ${h.name}: answered at the limit, not later`, async () => {
    const e = new FakeEngine();
    const r = rig(e, h.hang(e));
    await run(r.clock, droppedLimit - 1000);
    assert.equal(r.result(), null, 'still waiting before the limit');
    await run(r.clock, 1000);
    assert.notEqual(r.result(), null, 'answered at the limit');
    // A leave or close still pending is not started again on every pass.
    assert.ok(e.log.filter((l) => l === 'leave').length <= 1, 'one leave at a time');
  });
}

test('dropped: past the limit the guard lingers, closes a late reopen at once, and ends when the attempt does', async () => {
  const e = Object.assign(new FakeEngine(), { connecting: true });
  const r = rig(e);
  await run(r.clock, droppedLimit + 1000);
  assert.equal(r.result()?.settled, false);
  assert.equal(r.lingered(), false);
  // The late reopen the limit used to let through.
  e.rejoin();
  await run(r.clock, 200, 100);
  assert.equal(e.closed, true, 'closed again by the lingering guard');
  assert.deepEqual(e.log, ['seal', 'leave', 'close', 'leave', 'close']);
  e.connecting = false;
  await run(r.clock, 200, 100);
  assert.equal(r.lingered(), true);
});

test('dropped: the lingering guard gives up at lingerLimit', async () => {
  const e = Object.assign(new FakeEngine(), { connecting: true });
  const r = rig(e);
  await run(r.clock, lingerLimit - 1000);
  assert.equal(r.lingered(), false);
  await run(r.clock, 2000);
  assert.equal(r.lingered(), true);
});

// Connection: the order of a room's connect and its retirement (review P1:
// a room retired while its audio was set up later joined on its old token).
function connection() {
  const clock = new FakeClock();
  const log: string[] = [];
  const c = new Connection();
  let audio: () => void = () => undefined;
  let joined: () => void = () => undefined;
  const prepare = () => new Promise<void>((resolve) => {
    log.push('audio start');
    audio = resolve;
  });
  const join = () => new Promise<void>((resolve) => {
    log.push('sdk connect');
    joined = resolve;
  });
  const undo = () => (log.push('audio stop'), Promise.resolve());
  let connected: boolean | null = null;
  void c.connect(prepare, join, undo).then((ok) => {
    connected = ok;
  });
  const e = Object.assign(new FakeEngine(), { closed: true });
  let retired: Retired | null = null;
  const retire = () => {
    void c.retire(e, { wait: (ms) => new Promise((resolve) => clock.setTimeout(resolve, ms)), now: () => clock.now }).then((r) => {
      retired = r;
    });
  };
  return {
    clock, log, e, c, retire,
    audio: () => audio(),
    joined: () => joined(),
    connected: () => connected,
    retired: () => retired,
  };
}

test('connection: retired while the audio is set up: the SDK connect never runs, and the retirement waits for the audio to be undone', async () => {
  const t = connection();
  await settle();
  t.retire();
  await run(t.clock, 5000, 100);
  assert.equal(t.retired(), null, 'not settled while the connect is in flight');
  t.audio();
  await run(t.clock, 200, 100);
  assert.deepEqual(t.log, ['audio start', 'audio stop']);
  assert.equal(t.connected(), false);
  assert.equal(t.retired()?.settled, true);
  // A rejoin after it settled: nothing here starts one.
  await run(t.clock, 60_000);
  assert.ok(!t.log.includes('sdk connect'));
});

test('connection: retired while the SDK connects: settled only once that connect has ended, and the engine is closed if it joined', async () => {
  const t = connection();
  t.audio();
  await settle();
  assert.deepEqual(t.log, ['audio start', 'sdk connect']);
  t.retire();
  await run(t.clock, 5000, 100);
  assert.equal(t.retired(), null, 'not settled while the SDK connect is in flight');
  // The connect, already on the wire, was answered: the engine is open.
  t.e.rejoin();
  t.joined();
  await run(t.clock, 200, 100);
  assert.equal(t.connected(), false, 'a retired connection does not count as joined');
  assert.equal(t.retired()?.settled, true);
  assert.equal(t.e.closed, true, 'closed after the late join');
});

test('connection: a second retire has the first one\'s answer, and seals once', async () => {
  const t = connection();
  t.audio();
  await settle();
  t.joined();
  await settle();
  assert.equal(t.connected(), true);
  const e = t.e;
  const o = { wait: (ms: number) => new Promise<void>((resolve) => t.clock.setTimeout(resolve, ms)), now: () => t.clock.now };
  const a = t.c.retire(e, o);
  const b = t.c.retire(e, o);
  assert.equal(a, b);
  await settle();
  assert.deepEqual(e.log, ['seal']);
});
