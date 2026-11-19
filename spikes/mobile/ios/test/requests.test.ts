// Copied from spikes/mobile/android at ea00f74f (#1390, M4-B) for the iOS spike (#1394, M5-B).
/// <reference types="node" />
import test from 'node:test';
import assert from 'node:assert/strict';
import { afterAuthSignal, type AuthSignal } from '../src/logic/auth';
import { authorization, build, claimCameraRequest, requests, stopCameraRequest, type RequestName } from '../src/logic/requests';

const names = Object.keys(requests) as RequestName[];

test('the request list has no companion, viewer, share or screen route (M-S5)', () => {
  for (const name of names) {
    const path = requests[name].path;
    assert.doesNotMatch(path, /companion|viewer|share|screen/, `${name}: ${path}`);
  }
});

test('no request carries participant=, and no route accepts it', () => {
  for (const name of names) assert.ok(!requests[name].query.includes('participant'), name);
  assert.throws(() => build('claimCamera', { channel: 'general' }, { participant: 'p7.screen' }), /not allowed/);
  assert.throws(() => build('stopCamera', { channel: 'general' }, { participant: 'p7' }), /not allowed/);
});

const cameraCases: readonly { name: string; built: ReturnType<typeof claimCameraRequest>; method: string; url: string }[] = [
  { name: 'claim', built: claimCameraRequest('general'), method: 'POST', url: '/v1/channels/general/voice/camera' },
  { name: 'named continue', built: claimCameraRequest('general', '2026-11-05T10:00:00Z'), method: 'POST', url: '/v1/channels/general/voice/camera?started_at=2026-11-05T10%3A00%3A00Z' },
  { name: 'release', built: stopCameraRequest('general', '2026-11-05T10:00:00Z'), method: 'DELETE', url: '/v1/channels/general/voice/camera?started_at=2026-11-05T10%3A00%3A00Z' },
];

for (const c of cameraCases) {
  test(`camera request: ${c.name}`, () => {
    assert.equal(c.built.method, c.method);
    assert.equal(c.built.url, c.url);
    assert.doesNotMatch(c.built.url, /participant/);
  });
}

test('a path parameter is escaped and must be given', () => {
  assert.equal(build('voiceSession', { channel: 'a/b c' }).url, '/v1/channels/a%2Fb%20c/voice/session');
  assert.throws(() => build('voiceSession'), /missing path parameter/);
});

test('the bearer is only ever a header: no request URL can hold it', () => {
  assert.deepEqual(authorization('tok-abc'), { Authorization: 'Bearer tok-abc' });
  for (const name of names) {
    assert.ok(!requests[name].query.some((k) => /^(token|bearer|access_token|authorization)$/i.test(k)), name);
  }
});

test('sockets take the single-use ticket and nothing else in the query', () => {
  for (const name of names) {
    const r = requests[name];
    if (r.socket) assert.deepEqual(r.query, ['ticket'], name);
  }
  assert.equal(requests.wsTicket.method, 'POST');
  assert.equal(requests.wsTicket.path, '/v1/ws-tickets');
});

const signals: readonly { signal: AuthSignal; steps: readonly string[] }[] = [
  { signal: 'whoami_401', steps: ['tear_down_media', 'show_signed_out'] },
  { signal: 'ticket_401', steps: ['tear_down_media', 'show_signed_out'] },
  { signal: 'socket_closed_1008', steps: ['revalidate'] },
];

for (const c of signals) {
  test(`auth: ${c.signal} -> ${c.steps.join(', ')}`, () => {
    assert.deepEqual(afterAuthSignal(c.signal), c.steps);
  });
}
