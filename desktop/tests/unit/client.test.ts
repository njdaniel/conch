import { afterEach, describe, expect, it } from 'vitest';

import { Client } from '../../main/api/client.ts';
import { Session } from '../../main/api/session.ts';
import { makePki, sendJson, startHttp, startHttps, stack, who, type Fake, type Pki } from './support.ts';

const servers: Fake[] = [];
afterEach(async () => {
  await Promise.all(servers.splice(0).map((s) => s.close()));
});
const serve = async (...args: Parameters<typeof startHttp>): Promise<Fake> => {
  const fake = await startHttp(...args);
  servers.push(fake);
  return fake;
};

describe('the request executor', () => {
  it('sends the bearer for a bearer operation only, and never an Origin or a Cookie', async () => {
    const fake = await serve((_r, res) => {
      sendJson(res, 200, who);
    });
    const { session, client } = stack();
    session.setOrigin(fake.origin);
    session.adopt('bearer-canary-1');
    await client.send({ op: 'whoami' });
    await client.send({ op: 'health' });
    const [authed, open] = fake.seen;
    expect(authed?.headers['authorization']).toBe('Bearer bearer-canary-1');
    expect(open?.headers['authorization']).toBeUndefined();
    for (const seen of fake.seen) {
      expect(seen.headers['origin']).toBeUndefined();
      expect(seen.headers['cookie']).toBeUndefined();
      expect(seen.headers['referer']).toBeUndefined();
    }
  });

  it('builds the URL from the operation and encodes a value into one path segment', async () => {
    const fake = await serve((_r, res) => {
      sendJson(res, 200, { messages: [] });
    });
    const { session, client } = stack();
    session.setOrigin(fake.origin);
    session.adopt('t');
    await client.send({ op: 'listMessages', params: { channel: 'a b' }, query: 'limit=3' });
    expect(fake.seen[0]?.url).toBe('/v2/channels/a%20b/messages?limit=3');
    expect(fake.seen[0]?.method).toBe('GET');
  });

  it('refuses a path value that would leave its segment, without asking anyone', async () => {
    const fake = await serve((_r, res) => {
      sendJson(res, 200, {});
    });
    const { session, client } = stack();
    session.setOrigin(fake.origin);
    session.adopt('t');
    for (const channel of ['a/b', '..', 'a%2fb']) {
      expect(await client.send({ op: 'listMessages', params: { channel } })).toEqual({ kind: 'none', reason: 'unsupported' });
    }
    expect(fake.seen).toHaveLength(0);
  });

  it('is a 401 without asking when there is no credential, and with no address there is no request', async () => {
    const fake = await serve((_r, res) => {
      sendJson(res, 200, who);
    });
    const { session, client } = stack();
    expect(await client.send({ op: 'whoami' })).toEqual({ kind: 'none', reason: 'no-server' });
    session.setOrigin(fake.origin);
    expect(await client.send({ op: 'whoami' })).toEqual({ kind: 'unauthorized' });
    expect(fake.seen).toHaveLength(0);
  });

  it('does not follow a redirect, on a bearer operation or any other, and the target never sees the credential', async () => {
    const target = await serve((_r, res) => {
      sendJson(res, 200, who);
    });
    const origin = await serve((_r, res) => {
      res.writeHead(307, { Location: `${target.origin}/v1/whoami` });
      res.end();
    });
    const { session, client } = stack();
    session.setOrigin(origin.origin);
    session.adopt('redirect-canary');
    expect(await client.send({ op: 'whoami' })).toEqual({ kind: 'none', reason: 'redirect' });
    expect(await client.send({ op: 'login', body: { name: 'a', password: 'redirect-password' } })).toEqual({ kind: 'none', reason: 'redirect' });
    expect(target.seen).toHaveLength(0);
    // The signed-in state is untouched: a redirect is not a 401.
    expect(session.signedIn).toBe(true);
  });

  it('refuses a response over the cap, declared or streamed', async () => {
    const fake = await serve((request, res) => {
      if (request.url.endsWith('/healthz')) {
        res.writeHead(200, { 'Content-Type': 'application/json' });
        res.write('{"status":"');
        res.write('x'.repeat(5000));
        res.end('"}');
        return;
      }
      sendJson(res, 200, who);
    });
    const { session } = stack();
    const client = new Client({ fetch: globalThis.fetch, session, maxResponseBytes: 1000 });
    session.setOrigin(fake.origin);
    session.adopt('t');
    expect(await client.send({ op: 'health' })).toEqual({ kind: 'none', reason: 'oversize' });
    expect((await client.send({ op: 'whoami' })).kind).toBe('answer');
  });

  it('validates the answer before anyone sees it', async () => {
    const fake = await serve((request, res) => {
      sendJson(res, 200, request.url === '/v1/whoami' ? { id: 'seven' } : { not: 'an error' });
    });
    const { session, client } = stack();
    session.setOrigin(fake.origin);
    session.adopt('t');
    expect(await client.send({ op: 'whoami' })).toEqual({ kind: 'none', reason: 'invalid' });
  });

  it('passes a refusal’s error document and drops any other body of a refusal', async () => {
    let n = 0;
    const fake = await serve((_r, res) => {
      n++;
      if (n === 1) sendJson(res, 429, { code: 'rate_limited', message: 'slow' }, { 'Retry-After': '12', 'X-Request-Id': 'rid-9' });
      else sendJson(res, 500, { stack: 'secret internals' });
    });
    const { session, client } = stack();
    session.setOrigin(fake.origin);
    session.adopt('t');
    expect(await client.send({ op: 'listChannels' })).toEqual({
      kind: 'answer',
      status: 429,
      ok: false,
      body: { code: 'rate_limited', message: 'slow' },
      retryAfter: 12,
      requestId: 'rid-9',
    });
    expect(await client.send({ op: 'listChannels' })).toEqual({ kind: 'answer', status: 500, ok: false });
  });

  it('signs out on a 401 for a bearer operation, but not for one that takes a secret, and not for a sign-in', async () => {
    const fake = await serve((_r, res) => {
      sendJson(res, 401, { code: 'auth_failed', message: 'no' });
    });
    const { session, client } = stack();
    session.setOrigin(fake.origin);
    session.adopt('t');
    const secret = await client.send({ op: 'reauth', body: { password: 'p' } });
    expect(secret).toMatchObject({ kind: 'answer', status: 401 });
    expect(session.signedIn).toBe(true);
    expect(await client.send({ op: 'login', body: { name: 'a', password: 'b' } })).toMatchObject({ kind: 'answer', status: 401 });
    expect(session.signedIn).toBe(true);
    expect(await client.send({ op: 'whoami' })).toEqual({ kind: 'unauthorized' });
    expect(session.signedIn).toBe(false);
  });

  it('drops an answer that arrives for an older generation, and aborts the request in flight', async () => {
    let release: () => void = () => undefined;
    const gate = new Promise<void>((resolve) => {
      release = resolve;
    });
    const fake = await serve((_r, res) => {
      void gate.then(() => {
        sendJson(res, 200, who);
      });
    });
    const { session, client } = stack();
    session.setOrigin(fake.origin);
    session.adopt('account-a');
    const pending = client.send({ op: 'whoami' });
    await new Promise((r) => setTimeout(r, 50));
    // The identity changes (sign-out, a switch) while the answer is delayed.
    session.forget();
    session.adopt('account-b');
    release();
    expect(await pending).toEqual({ kind: 'none', reason: 'stale' });
  });

  it('drops a late answer even when the request was not aborted in time (a response already read)', async () => {
    const fake = await serve((_r, res) => {
      sendJson(res, 200, who);
    });
    const { session, client } = stack();
    session.setOrigin(fake.origin);
    session.adopt('a');
    const capture = session.capture();
    session.forget();
    expect(await client.send({ op: 'whoami', capture })).toEqual({ kind: 'none', reason: 'stale' });
  });

  it('sends no body unless the caller has one, and a JSON body with its type', async () => {
    const fake = await serve((_r, res) => {
      sendJson(res, 200, {});
    });
    const { session, client } = stack();
    session.setOrigin(fake.origin);
    session.adopt('t');
    await client.send({ op: 'listChannels' });
    await client.send({ op: 'markRead', params: { channel: 'a' }, body: { last_seen_id: 3 } });
    expect(fake.seen[0]?.headers['content-type']).toBeUndefined();
    expect(fake.seen[1]?.headers['content-type']).toBe('application/json');
    expect(fake.seen[1]?.body).toBe('{"last_seen_id":3}');
  });
});

describe('TLS', () => {
  let pki: Pki;
  afterEach(() => {
    pki.cleanup();
  });

  it('fails closed on a certificate this machine does not trust, and sends nothing after the handshake', async () => {
    pki = makePki();
    const https = await startHttps(pki, (_r, res) => {
      sendJson(res, 200, who);
    });
    servers.push(https);
    const session = new Session();
    const client = new Client({ fetch: globalThis.fetch, session });
    session.setOrigin(https.origin);
    session.adopt('tls-canary-token');
    expect(await client.send({ op: 'whoami' })).toEqual({ kind: 'none', reason: 'certificate' });
    expect(https.seen).toHaveLength(0);
  });

  it('is a plain no-answer, not a certificate failure, for a closed port', async () => {
    pki = makePki();
    const session = new Session();
    const client = new Client({ fetch: globalThis.fetch, session });
    const fake = await serve(() => undefined);
    const closed = fake.origin;
    await fake.close();
    servers.length = 0;
    session.setOrigin(closed);
    expect(await client.send({ op: 'health' })).toEqual({ kind: 'none' });
  });
});

describe('the identity', () => {
  it('counts a change of address, a credential and a sign-out as new identities, and ignores the same address again', () => {
    const { session } = stack();
    const g0 = session.generation;
    session.setOrigin('http://127.0.0.1:1');
    session.adopt('a');
    const g1 = session.generation;
    expect(g1).toBeGreaterThan(g0);
    session.setOrigin('http://127.0.0.1:1');
    expect(session.generation).toBe(g1);
    expect(session.forget()).toBe('a');
    expect(session.generation).toBeGreaterThan(g1);
    expect(session.forget()).toBeNull();
    session.adopt('b');
    session.setOrigin('http://127.0.0.1:2');
    expect(session.signedIn).toBe(false);
  });

  it('keeps a challenge as a handle, once, until it ends, and not past a new identity', () => {
    const { session } = stack();
    const handle = session.keepChallenge('real-challenge', 1_000);
    expect(handle).not.toContain('real-challenge');
    expect(session.challenge(handle, 999)).toBe('real-challenge');
    expect(session.challenge(handle, 1_000)).toBeNull();
    const again = session.keepChallenge('c2', 5_000);
    session.adopt('x');
    expect(session.challenge(again, 0)).toBeNull();
  });
});
