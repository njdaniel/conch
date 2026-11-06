/// <reference types="node" />
import test from 'node:test';
import assert from 'node:assert/strict';
import { ApiClient, signInCode, type Fetch, type HttpInit } from '../src/app/client';

interface Call {
  url: string;
  init: HttpInit;
}

function client(answer: (c: Call) => { status: number; body: unknown }) {
  const calls: Call[] = [];
  const signals: string[] = [];
  const fetch: Fetch = (url, init) => {
    const c = { url, init };
    calls.push(c);
    const a = answer(c);
    return Promise.resolve({ status: a.status, json: () => Promise.resolve(a.body) });
  };
  const api = new ApiClient({ fetch, onSignal: (s, steps) => signals.push(`${s}: ${steps.join(', ')}`) });
  api.setServer('http://example.test:1/');
  return { api, calls, signals };
}

const principal = { id: 7, kind: 'human', name: 'sam', created_at: '2026-11-05T10:00:00Z' };
const credential = { id: 1, principal_id: 7, label: 'phone', created_at: '2026-11-05T10:00:00Z' };

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
  const r = await api.redeemSignInLink('http://x.test/app/#/signin/code9');
  assert.ok(r.ok);
  assert.equal(calls[0]?.init.body, JSON.stringify({ code: 'code9' }));
  assert.equal(api.signedIn, true);
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
