/// <reference types="node" />
import test from 'node:test';
import assert from 'node:assert/strict';
import { ApiClient, signInCode, signInTarget, type Fetch, type HttpInit } from '../src/app/client';
import { developmentLine, developmentNotice, developmentState } from '../src/logic/development';
import { cameraTiles } from '../src/app/camera/tiles';
import { PeopleSession } from '../src/app/peopleSession';
import { FakeClock, settle } from './fakes';

interface Call {
  url: string;
  init: HttpInit;
}

type Answer = { status: number; body: unknown };

function client(answer: (c: Call) => Answer | Promise<Answer>) {
  const calls: Call[] = [];
  const signals: string[] = [];
  const fetch: Fetch = async (url, init) => {
    const c = { url, init };
    calls.push(c);
    const a = await answer(c);
    return { status: a.status, json: () => Promise.resolve(a.body) };
  };
  const api = new ApiClient({ fetch, onSignal: (s, steps) => signals.push(`${s}: ${steps.join(', ')}`) });
  api.setServer('http://example.test:1/');
  return { api, calls, signals };
}

const principal = { id: 7, kind: 'human', name: 'sam', created_at: '2026-11-05T10:00:00Z' };
const credential = { id: 1, principal_id: 7, label: 'phone', created_at: '2026-11-05T10:00:00Z' };

function sessionAnswer(id: number): Answer {
  return { status: 200, body: {
    schema: 'conch.session.v1', id, principal_id: principal.id, name: principal.name, kind: principal.kind,
    client: 'android', state: 'online', status: 'online', started_at: principal.created_at,
    last_seen_at: principal.created_at, interventions: [], outcome_unknown: false,
  } };
}

test('the Android People session registers, heartbeats, recovers a missing session and ends on sign-out', async () => {
  let id = 0;
  let missing = false;
  const { api, calls } = client(({ url, init }) => {
    if (url.endsWith('/v1/login')) return { status: 200, body: { next: 'signed_in', principal, credential, token: 'b' } };
    if (init.method === 'DELETE') return { status: 204, body: null };
    if (url.endsWith('/heartbeat') && missing) return { status: 404, body: {} };
    if (url.endsWith('/v1/sessions')) id++;
    return sessionAnswer(id);
  });
  await api.login('sam', 'pw');
  const clock = new FakeClock();
  const session = new PeopleSession(api, clock);
  session.start();
  await settle();
  clock.advance(15_000);
  await settle();
  missing = true;
  session.refresh();
  await settle();
  clock.advance(15_000);
  await settle();
  session.stop();
  api.signOut();
  clock.advance(30_000);
  assert.deepEqual(calls.slice(1).map(({ url, init }) => [init.method, new URL(url).pathname, init.headers.Authorization]), [
    ['POST', '/v1/sessions', 'Bearer b'],
    ['POST', '/v1/sessions/1/heartbeat', 'Bearer b'],
    ['POST', '/v1/sessions/1/heartbeat', 'Bearer b'],
    ['POST', '/v1/sessions', 'Bearer b'],
    ['DELETE', '/v1/sessions/2', 'Bearer b'],
  ]);
  assert.deepEqual(calls[1]?.init.body, JSON.stringify({ client: 'android' }));
});

void test('sign-out ends a session whose registration reply arrives during a new sign-in', async () => {
  const late = Promise.withResolvers<Answer>();
  const { api, calls } = client(({ url, init }) => {
    const oldServer = new URL(url).hostname === 'example.test';
    if (url.endsWith('/v1/login')) {
      return { status: 200, body: { next: 'signed_in', principal, credential, token: oldServer ? 'old' : 'new' } };
    }
    if (init.method === 'DELETE') return { status: 204, body: null };
    return oldServer ? late.promise : sessionAnswer(2);
  });
  await api.login('sam', 'pw');
  const clock = new FakeClock();
  const session = new PeopleSession(api, clock);
  session.start();
  session.stop();
  api.signOut();
  api.setServer('https://other.test');
  await api.login('sam', 'pw');
  session.start();
  await settle();

  assert.equal(calls[1]?.init.signal?.aborted ?? false, false);
  late.resolve(sessionAnswer(1));
  await settle();
  clock.advance(15_000);
  await settle();
  assert.deepEqual(calls.slice(4).map(({ url, init }) => [init.method, url, init.headers.Authorization]), [
    ['DELETE', 'http://example.test:1/v1/sessions/1', 'Bearer old'],
    ['POST', 'https://other.test/v1/sessions/2/heartbeat', 'Bearer new'],
  ]);
});

void test('a stalled heartbeat expires and the next heartbeat runs even before the first settles', async () => {
  const stalled = Promise.withResolvers<Answer>();
  let heartbeats = 0;
  const { api, calls } = client(({ url }) => {
    if (url.endsWith('/v1/login')) return { status: 200, body: { next: 'signed_in', principal, credential, token: 'b' } };
    if (url.endsWith('/heartbeat') && ++heartbeats === 1) return stalled.promise;
    return sessionAnswer(1);
  });
  await api.login('sam', 'pw');
  const clock = new FakeClock();
  const session = new PeopleSession(api, clock);
  session.start();
  await settle();
  clock.advance(15_000);
  const firstSignal = calls.at(2)?.init.signal;
  assert.ok(firstSignal);
  assert.equal(firstSignal.aborted, false);
  clock.advance(5_000);
  assert.equal(firstSignal.aborted, true);
  clock.advance(15_000);
  assert.equal(heartbeats, 2);
  await settle();

  stalled.resolve({ status: 404, body: {} });
  await settle();
  session.refresh();
  assert.equal(heartbeats, 3);
  assert.ok(calls.slice(2).every(({ url }) => url.endsWith('/v1/sessions/1/heartbeat')));
});

void test('the camera grid keeps a remote tile with a generic label when the member list returns 503', async () => {
  const { api } = client(({ url }) => url.endsWith('/v1/login')
    ? { status: 200, body: { next: 'signed_in', principal, credential, token: 'b' } }
    : { status: 503, body: {} });
  await api.login('sam', 'pw');
  const members = await api.members('general');
  assert.ok(!members.ok && members.status === 503);

  // The voice view passes an empty names map until it gets a member list.
  const tracks = new Map([[principal.id, 'local track'], [42, 'remote track']]);
  assert.deepEqual(cameraTiles(tracks, principal.id, new Map()), [
    { id: 42, track: 'remote track', name: 'Unknown person' },
  ]);
  assert.deepEqual(cameraTiles(tracks, principal.id, new Map([[42, 'Lee']])), [
    { id: 42, track: 'remote track', name: 'Lee' },
  ]);
});

test('login keeps the bearer in memory and never puts it in a URL', async () => {
  const { api, calls } = client((c) =>
    c.url.endsWith('/v1/login')
      ? { status: 200, body: { next: 'signed_in', principal, credential, token: 'secret-bearer-1' } }
      : { status: 200, body: { id: 7, kind: 'human', name: 'sam', role: 'member' } },
  );
  const result = await api.login('sam', 'pw');
  assert.equal(api.signedIn, result.ok && result.value.next === 'signed_in');
  const me = await api.whoami();
  assert.ok(me.ok);
  assert.equal(calls[0]?.init.headers['Authorization'], undefined, 'login sends no bearer');
  assert.equal(calls[1]?.init.headers['Authorization'], 'Bearer secret-bearer-1');
  for (const c of calls) {
    assert.doesNotMatch(c.url, /secret-bearer-1/);
    assert.doesNotMatch(c.url, /participant|companion/);
  }
  assert.equal(calls[0]?.url, 'http://example.test:1/v1/login', 'trailing slash trimmed');
});

test('a 401 on an authenticated call forgets the bearer and signals teardown before signed out', async () => {
  const { api, signals } = client((c) =>
    c.url.endsWith('/v1/login')
      ? { status: 200, body: { next: 'signed_in', principal, credential, token: 'b' } }
      : { status: 401, body: { code: 'unauthorized', message: 'no' } },
  );
  await api.login('sam', 'pw');
  const r = await api.whoami();
  assert.deepEqual(r.ok, false);
  assert.equal(api.signedIn, false);
  assert.deepEqual(signals, ['whoami_401: tear_down_media, show_signed_out']);
});

test('a wrong password is a refusal, not a signal', async () => {
  const { api, signals } = client(() => ({ status: 401, body: { code: 'auth_failed', message: 'no' } }));
  const r = await api.login('sam', 'bad');
  assert.ok(!r.ok && r.code === 'auth_failed');
  assert.deepEqual(signals, []);
});

test('a sign-in link is redeemed by its code; a whole link is accepted', async () => {
  assert.equal(signInCode('http://x.test/app/#/signin/abc%20d'), 'abc d');
  assert.equal(signInCode('  abc  '), 'abc');
  const { api, calls } = client(() => ({ status: 200, body: { principal, credential, token: 'tok' } }));
  const r = await api.redeemSignInLink('http://example.test:1/app/#/signin/code9');
  assert.ok(r.ok);
  assert.equal(calls[0]?.init.body, JSON.stringify({ code: 'code9' }));
  assert.equal(api.signedIn, true);
});

test('a link made for another server is refused with no request; a bad escape does not throw', async () => {
  const { api, calls } = client(() => ({ status: 200, body: { principal, credential, token: 'tok' } }));
  const other = await api.redeemSignInLink('http://elsewhere.test/app/#/signin/code9');
  assert.ok(!other.ok && other.code === 'link_other_server');
  const bad = await api.redeemSignInLink('http://example.test:1/app/#/signin/100%');
  assert.ok(!bad.ok && bad.code === 'link_malformed');
  assert.equal(calls.length, 0, 'nothing was sent to either');
  assert.equal(api.signedIn, false);
  assert.deepEqual(signInTarget('HTTP://Example.test:1/app/#/signin/ok', 'http://example.test:1'), { ok: true, code: 'ok' }, 'the origin is compared without case');
  assert.deepEqual(signInTarget(' barecode ', 'http://example.test:1'), { ok: true, code: 'barecode' });
  assert.equal(signInCode('#/signin/100%'), '100%', 'signInCode never throws');
});

test('changing the server forgets the bearer; the same server keeps it', async () => {
  const { api } = client(() => ({ status: 200, body: { next: 'signed_in', principal, credential, token: 'b' } }));
  await api.login('sam', 'pw');
  assert.equal(api.signedIn, true);
  api.setServer('http://example.test:1');
  assert.equal(api.signedIn, true, 'the same origin, spelt differently, is the same server');
  api.setServer('http://other.test:1');
  assert.equal(api.signedIn, false);
  assert.equal(api.secret, null);
});

test('transmit reports map statuses as the browser does', async () => {
  const statuses: [number, string][] = [[204, 'ok'], [409, 'no_session'], [429, 'rate_limited'], [503, 'failed']];
  for (const [status, want] of statuses) {
    const { api } = client((c) =>
      c.url.endsWith('/v1/login')
        ? { status: 200, body: { next: 'signed_in', principal, credential, token: 'b' } }
        : { status, body: {} },
    );
    await api.login('sam', 'pw');
    assert.equal(await api.reportTransmit('general', 'started', new AbortController().signal), want, String(status));
  }
});

test('an unauthenticated call is refused without a request', async () => {
  const { api, calls } = client(() => ({ status: 200, body: {} }));
  const r = await api.channels();
  assert.ok(!r.ok && r.failure === 'unauthorized');
  assert.equal(calls.length, 0);
});

test('the camera claim is a POST naming nothing, or the claim it continues; the release names the claim and nothing else', async () => {
  const { api, calls } = client((c) =>
    c.url.includes('/v1/login') ? { status: 200, body: { next: 'signed_in', principal, credential, token: 'b' } } : c.init.method === 'DELETE' ? { status: 204, body: null } : { status: 200, body: { principal_id: 7, started_at: '2026-11-05T10:00:00Z' } },
  );
  await api.login('sam', 'pw');
  const first = await api.claimCamera('general');
  assert.ok(first.ok);
  await api.claimCamera('general', '2026-11-05T10:00:00Z');
  const released = await api.releaseCamera('general', '2026-11-05T10:00:00Z');
  assert.ok(released.ok);
  assert.deepEqual(calls.slice(1).map((c) => `${c.init.method} ${c.url}`), [
    'POST http://example.test:1/v1/channels/general/voice/camera',
    'POST http://example.test:1/v1/channels/general/voice/camera?started_at=2026-11-05T10%3A00%3A00Z',
    'DELETE http://example.test:1/v1/channels/general/voice/camera?started_at=2026-11-05T10%3A00%3A00Z',
  ]);
});

test('a refused camera claim carries its code; a bad body is invalid; a 401 signals teardown', async () => {
  let status = 409;
  let body: unknown = { code: 'voice_camera_full', message: 'full' };
  const { api, signals } = client((c) => (c.url.includes('/v1/login') ? { status: 200, body: { next: 'signed_in', principal, credential, token: 'b' } } : { status, body }));
  await api.login('sam', 'pw');
  const full = await api.claimCamera('general');
  assert.deepEqual(full.ok ? null : full.code, 'voice_camera_full');
  status = 200;
  body = { nonsense: true };
  const bad = await api.claimCamera('general');
  assert.deepEqual(bad.ok ? null : bad.failure, 'invalid');
  status = 401;
  body = {};
  await api.claimCamera('general');
  assert.equal(signals.length, 1);
});

test('development marking: http:// sign-in marks each screen, https:// marks none (M-R1)', () => {
  const screens = ['channels', 'voice', 'diagnostics'] as const;
  const cases: { origin: string; development: boolean }[] = [
    { origin: 'http://192.168.1.20:8080/', development: true },
    { origin: ' HTTP://nas.local:8080', development: true },
    { origin: 'https://chat.example.test', development: false },
    { origin: 'HTTPS://chat.example.test/', development: false },
    { origin: '', development: false },
  ];
  for (const c of cases) {
    const { api } = client(() => ({ status: 200, body: {} }));
    api.setServer(c.origin);
    assert.equal(api.development, c.development, c.origin);
    for (const screen of screens) {
      const want = !c.development ? null : screen === 'diagnostics' ? developmentState : developmentLine;
      assert.equal(developmentNotice(api.server, screen), want, `${c.origin} ${screen}`);
    }
  }
  assert.match(developmentLine, /development server/i);
  assert.match(developmentLine, /not encrypted/);
  assert.equal(developmentState, 'server development');
});
