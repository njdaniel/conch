// healthReader (issue #946), and its strict form for the sweep of #2217
// (issue #2217's review, N3): the sweep opens an older build's saved sign-in
// only for a server whose /healthz is the health document, not any 200.
import { afterEach, describe, expect, it, vi } from 'vitest';

import { healthReader } from '../../main/profiles/activation.ts';

const ORIGIN = 'https://chat.example.org';
const HEALTH = { status: 'ok', version: 'v1.0.0', db: 'ok', voice: 'ok' };

// No network: every request is answered here.
function replying(status: number, body: unknown): void {
  vi.stubGlobal('fetch', () => Promise.resolve(new Response(typeof body === 'string' ? body : JSON.stringify(body), { status, headers: { 'Content-Type': 'application/json' } })));
}

afterEach(() => {
  vi.unstubAllGlobals();
});

describe('healthReader', () => {
  it.each([
    ['the health document', 200, HEALTH, { kind: 'answered', version: 'v1.0.0' }, { kind: 'answered', version: 'v1.0.0' }],
    ['a 200 that is some other JSON', 200, { hello: 'world' }, { kind: 'answered', version: null }, { kind: 'unreachable' }],
    ['a 200 that is not JSON', 200, '<html>captive portal</html>', { kind: 'answered', version: null }, { kind: 'unreachable' }],
    ['a 200 health document with a field of the wrong type', 200, { ...HEALTH, voice: 'loud' }, { kind: 'answered', version: null }, { kind: 'unreachable' }],
    ['a 503', 503, HEALTH, { kind: 'unreachable' }, { kind: 'unreachable' }],
  ] as const)('%s: lenient and strict', async (_name, status, body, lenient, strict) => {
    replying(status, body);
    expect(await healthReader()(ORIGIN)).toEqual(lenient);
    expect(await healthReader({ strict: true })(ORIGIN)).toEqual(strict);
  });

  it('no answer at all is unreachable either way', async () => {
    vi.stubGlobal('fetch', () => Promise.reject(new TypeError('fetch failed')));
    expect(await healthReader()(ORIGIN)).toEqual({ kind: 'unreachable' });
    expect(await healthReader({ strict: true })(ORIGIN)).toEqual({ kind: 'unreachable' });
  });
});
