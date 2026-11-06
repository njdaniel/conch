/// <reference types="node" />
import test from 'node:test';
import assert from 'node:assert/strict';
import { afterDisconnect, afterRefusal, cameraAfter, maxRetry, retryDelay, type DisconnectCause, type Next } from '../src/logic/disconnect';

const causes: readonly { cause: DisconnectCause; next: Next['kind']; camera: string }[] = [
  { cause: 'room_deleted', next: 'new_session', camera: 'renewal' },
  { cause: 'participant_removed', next: 'new_session', camera: 'stop' },
  { cause: 'duplicate_identity', next: 'stop', camera: 'displaced' },
  { cause: 'server_shutdown', next: 'retry', camera: 'keep' },
  { cause: 'lost', next: 'retry', camera: 'keep' },
];

for (const c of causes) {
  test(`disconnect ${c.cause}: ${c.next}, camera ${c.camera}`, () => {
    assert.equal(afterDisconnect(c.cause).kind, c.next);
    assert.equal(cameraAfter(c.cause), c.camera);
  });
}

test('duplicate_identity means stop, with no rejoin', () => {
  const next = afterDisconnect('duplicate_identity');
  assert.equal(next.kind, 'stop');
});

const refusals: readonly { name: string; r: Parameters<typeof afterRefusal>[0]; kind: Next['kind'] }[] = [
  { name: 'network', r: { ok: false, failure: 'network' }, kind: 'retry' },
  { name: 'unauthorized', r: { ok: false, failure: 'unauthorized' }, kind: 'stop' },
  { name: 'not configured', r: { ok: false, failure: 'invalid', status: 503, code: 'voice_not_configured' }, kind: 'stop' },
  { name: 'unavailable', r: { ok: false, failure: 'invalid', status: 503, code: 'voice_unavailable' }, kind: 'retry' },
  { name: '403 not allowed', r: { ok: false, failure: 'invalid', status: 403, code: 'voice_not_allowed' }, kind: 'stop' },
  { name: '404', r: { ok: false, failure: 'invalid', status: 404 }, kind: 'stop' },
  { name: '500', r: { ok: false, failure: 'invalid', status: 500 }, kind: 'retry' },
];

for (const c of refusals) {
  test(`refusal ${c.name}: ${c.kind}`, () => {
    assert.equal(afterRefusal(c.r).kind, c.kind);
  });
}

test('retry delay doubles to the cap, with jitter', () => {
  assert.equal(retryDelay(0, () => 1 - Number.EPSILON), 1000);
  assert.equal(retryDelay(1, () => 0), 1000);
  assert.equal(retryDelay(20, () => 0), maxRetry / 2);
});
