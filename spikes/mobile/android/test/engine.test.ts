/// <reference types="node" />
// app/engine.ts against the real livekit-client (2.22.3, pinned), built in
// node: no network, no WebRTC is touched. If an SDK upgrade renames or hides
// what the adapter reads or seals, these fail (review P3, #1972).
import test from 'node:test';
import assert from 'node:assert/strict';
import { ConnectionError, ConnectionErrorReason, Room } from 'livekit-client';
import { droppedEngine, engineKnown, type RoomEngine } from '../src/app/engine';

// The SDK's internals that a restart runs on, as this test drives them.
interface Internals {
  _isClosed: boolean;
  url?: string;
  token?: string;
  signalOpts?: object;
  fullReconnectOnNext: boolean;
  regionStrategy?: { getNextUrl: () => Promise<string | undefined>; resetAttempts: () => void };
  attemptReconnect: () => Promise<void>;
  attemptingReconnect?: boolean;
  configure: (...args: unknown[]) => Promise<unknown>;
  client: { join: (...args: unknown[]) => Promise<unknown>; connect: (...args: unknown[]) => Promise<unknown> };
}

// restarting puts a fresh engine where #1972's was: open, its signal gone,
// a full reconnect ("restart") due, with one more region to try after the
// first. The first steps of a real join (the peer connection's setup, the
// signal client's join and connect) are counted and refused: no socket.
function restarting(): { engine: RoomEngine; internals: Internals; joins: () => number } {
  const engine = new Room().engine;
  const internals = engine as unknown as Internals;
  let joins = 0;
  const counted = (): Promise<never> => {
    joins++;
    return Promise.reject(new Error('the test opens no socket'));
  };
  internals.configure = counted;
  internals.client.join = counted;
  internals.client.connect = counted;
  internals._isClosed = false;
  internals.url = 'ws://127.0.0.1:9';
  internals.token = 'old-token';
  internals.signalOpts = { maxRetries: 1, websocketTimeout: 1000, peerConnectionTimeout: 1000 };
  internals.fullReconnectOnNext = true;
  const regions = ['ws://127.0.0.1:8'];
  internals.regionStrategy = { getNextUrl: () => Promise.resolve(regions.shift()), resetAttempts: () => undefined };
  return { engine, internals, joins: () => joins };
}

test('engine: livekit-client has the internals app/engine.ts reads and seals', () => {
  const engine = new Room().engine;
  assert.equal(engineKnown(engine), true, 'attemptingReconnect, join, client.join, client.reconnect and client.sendLeave are where 2.22.3 has them');
  const d = droppedEngine(engine);
  assert.equal(d.closed, true, 'a new engine reads closed');
  assert.equal(d.connecting, false, 'and nothing in flight');
});

test('engine: an engine whose attempt flag is gone reads as still connecting, not as settled', () => {
  const engine = new Room().engine;
  delete (engine as unknown as { attemptingReconnect?: boolean }).attemptingReconnect;
  assert.equal(engineKnown(engine), false);
  assert.equal(droppedEngine(engine).connecting, true);
});

test('engine: sealed, every way to join is refused, as a token the SDK may not use', async () => {
  const engine = new Room().engine;
  droppedEngine(engine).seal();
  const refusals = [
    engine.join('ws://127.0.0.1:9', 't', { maxRetries: 1, websocketTimeout: 1000 } as Parameters<RoomEngine['join']>[2]),
    engine.client.join('ws://127.0.0.1:9', 't', { maxRetries: 1, websocketTimeout: 1000 } as Parameters<RoomEngine['client']['join']>[2]),
    engine.client.reconnect('ws://127.0.0.1:9', 't'),
  ];
  for (const p of refusals) {
    await assert.rejects(p, (e: unknown) => e instanceof ConnectionError && e.reason === ConnectionErrorReason.NotAllowed);
  }
});

test('engine: a restart in flight on a sealed engine opens no signal connection, in either region, and ends closed', async () => {
  const r = restarting();
  droppedEngine(r.engine).seal();
  await r.internals.attemptReconnect();
  assert.equal(r.joins(), 0, 'no join was started');
  assert.equal(r.engine.isClosed, true, 'the SDK gave up and closed the engine');
  assert.equal(r.internals.attemptingReconnect, false);
  assert.equal(r.engine.pendingReconnect, false, 'and scheduled no further attempt');
});

test('engine: the same restart unsealed does start a join, in each region (the test drives the real path)', async () => {
  const r = restarting();
  await r.internals.attemptReconnect();
  assert.equal(r.joins(), 2);
  await r.engine.close();
});
