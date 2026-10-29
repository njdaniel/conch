import { afterEach, describe, expect, it } from 'vitest';

import { Client } from '../../main/api/client.ts';
import { PASTED_TTL_MS, Service } from '../../main/api/service.ts';
import { Session } from '../../main/api/session.ts';
import { credential, principal, sendJson, startHttp, stack, who, type Fake, type Handler } from './support.ts';

const CANARY_TOKEN = 'conch_canary_token_9f3a';
const CANARY_PASSWORD = 'canary-password-4471';
const CANARY_CHALLENGE = 'real-challenge-canary-77';

const servers: Fake[] = [];
afterEach(async () => {
  await Promise.all(servers.splice(0).map((s) => s.close()));
});

// A conchd that signs in one member and knows who it is.
const conchd: Handler = (request, res) => {
  const bearer = request.headers['authorization'];
  switch (`${request.method} ${request.url}`) {
    case 'GET /healthz':
      sendJson(res, 200, { status: 'ok' });
      return;
    case 'POST /v1/login': {
      const body = JSON.parse(request.body) as { name: string; password: string };
      if (body.password !== CANARY_PASSWORD) {
        sendJson(res, 401, { code: 'auth_failed', message: 'wrong name or password' }, { 'Retry-After': '2' });
        return;
      }
      if (body.name === 'op') {
        sendJson(res, 200, { next: 'totp', challenge: CANARY_CHALLENGE, challenge_expires_at: new Date(Date.now() + 300_000).toISOString() });
        return;
      }
      sendJson(res, 200, { next: 'signed_in', principal, credential, token: CANARY_TOKEN });
      return;
    }
    case 'POST /v1/login/totp': {
      const body = JSON.parse(request.body) as { challenge: string; code: string };
      if (body.challenge !== CANARY_CHALLENGE || body.code !== '123456') {
        sendJson(res, 401, { code: 'auth_failed', message: 'wrong code' });
        return;
      }
      sendJson(res, 200, { next: 'signed_in', principal, credential, token: CANARY_TOKEN });
      return;
    }
    case 'GET /v1/whoami':
      if (bearer === `Bearer ${CANARY_TOKEN}`) sendJson(res, 200, who);
      else sendJson(res, 401, { code: 'unauthorized', message: 'no' });
      return;
    case 'DELETE /v1/credentials/current':
      res.writeHead(bearer === `Bearer ${CANARY_TOKEN}` ? 204 : 401);
      res.end();
      return;
    case 'GET /v1/channels':
      if (bearer === `Bearer ${CANARY_TOKEN}`) sendJson(res, 200, { channels: [] });
      else sendJson(res, 401, { code: 'unauthorized', message: 'no' });
      return;
    case 'POST /v1/password/reset':
      sendJson(res, 200, { revoked: 1, token: 'must-not-be-kept' });
      return;
    default:
      sendJson(res, 404, { code: 'not_found', message: 'no such route' });
  }
};

// A pasted credential, checked and kept, as the page does for a member's.
async function paste(service: Service): Promise<void> {
  expect(await service.signInWithCredential(CANARY_TOKEN)).toMatchObject({ ok: true });
  expect(service.keepCredential(CANARY_TOKEN)).toBe(true);
}

async function ready(handler: Handler = conchd) {
  const fake = await startHttp(handler);
  servers.push(fake);
  const logs: string[] = [];
  const s = stack((line) => logs.push(line));
  expect(s.service.setServerAddress(fake.origin)).toEqual({ ok: true, origin: fake.origin });
  return { fake, logs, ...s };
}

// Everything a renderer could have received, to be scanned for the canaries.
function scan(...received: unknown[]): void {
  const text = JSON.stringify(received);
  for (const canary of [CANARY_TOKEN, CANARY_PASSWORD, CANARY_CHALLENGE]) expect(text).not.toContain(canary);
}

describe('sign-in', () => {
  it('signs in with a password, keeps the token in main, and returns the account without it', async () => {
    const { service, session, logs, fake } = await ready();
    const outcome = await service.signIn('login', { name: 'ada', password: CANARY_PASSWORD });
    expect(outcome).toEqual({ ok: true, who, answer: { next: 'signed_in', principal, credential } });
    expect(session.signedIn).toBe(true);
    // The next call carries the bearer main kept.
    const reply = await service.request({ operation: 'listChannels' });
    expect(reply).toEqual({ kind: 'reply', status: 200, body: { channels: [] } });
    expect(fake.seen.at(-1)?.headers['authorization']).toBe(`Bearer ${CANARY_TOKEN}`);
    scan(outcome, reply, service.serverAddress(), logs);
  });

  it('keeps the challenge in main and gives the renderer a handle, then signs in with the code', async () => {
    const { service } = await ready();
    const step = await service.signIn('login', { name: 'op', password: CANARY_PASSWORD });
    if (step.ok || step.kind !== 'challenge') throw new Error('expected a challenge');
    expect(step.challenge).not.toBe(CANARY_CHALLENGE);
    scan(step);
    const wrong = await service.signIn('loginTotp', { challenge: step.challenge, code: '000000' });
    expect(wrong).toMatchObject({ ok: false, kind: 'unauthorized', status: 401, code: 'auth_failed' });
    const done = await service.signIn('loginTotp', { challenge: step.challenge, code: '123456' });
    expect(done).toMatchObject({ ok: true, who });
    scan(wrong, done);
    // Spent: it does not work twice.
    expect(await service.signIn('loginTotp', { challenge: step.challenge, code: '123456' })).toMatchObject({ ok: false, code: 'challenge_expired' });
  });

  it('refuses a challenge handle main never made', async () => {
    const { service, fake } = await ready();
    const before = fake.seen.length;
    expect(await service.signIn('loginTotp', { challenge: CANARY_CHALLENGE, code: '123456' })).toMatchObject({ ok: false, code: 'challenge_expired' });
    expect(fake.seen.length).toBe(before);
  });

  it('returns the server’s refusal, with Retry-After, and keeps nobody signed in', async () => {
    const { service, session } = await ready();
    const outcome = await service.signIn('login', { name: 'ada', password: 'wrong' });
    expect(outcome).toEqual({ ok: false, kind: 'unauthorized', status: 401, code: 'auth_failed', message: 'wrong name or password', retryAfter: 2 });
    expect(session.signedIn).toBe(false);
  });

  it('does not keep a credential that whoami does not accept, and revokes it', async () => {
    let revoked = 0;
    const { service, session } = await ready((request, res) => {
      if (request.url === '/v1/whoami') sendJson(res, 401, { code: 'unauthorized', message: 'no' });
      else if (request.method === 'DELETE') {
        revoked++;
        res.writeHead(204);
        res.end();
      } else conchd(request, res);
    });
    const outcome = await service.signIn('login', { name: 'ada', password: CANARY_PASSWORD });
    expect(outcome).toMatchObject({ ok: false, kind: 'unauthorized' });
    expect(session.signedIn).toBe(false);
    expect(revoked).toBe(0); // a 401 credential is already dead; nothing to revoke
  });

  it('revokes a credential whose check got no answer', async () => {
    let revoked = 0;
    const { service, session } = await ready((request, res) => {
      if (request.url === '/v1/whoami') res.destroy();
      else if (request.method === 'DELETE') {
        revoked++;
        res.writeHead(204);
        res.end();
      } else conchd(request, res);
    });
    const outcome = await service.signIn('login', { name: 'ada', password: CANARY_PASSWORD });
    expect(outcome).toMatchObject({ ok: false, kind: 'network' });
    expect(session.signedIn).toBe(false);
    expect(revoked).toBe(1);
  });

  it('signs nobody in on a password reset, whatever the answer holds', async () => {
    const { service, session } = await ready();
    const outcome = await service.signIn('resetPassword', { code: 'c', password: 'new' });
    expect(outcome).toEqual({ ok: true, who: null, answer: { revoked: 1 } });
    expect(session.signedIn).toBe(false);
  });

  it('checks a pasted credential, and puts it in force only when the page keeps it', async () => {
    const { service, session } = await ready();
    expect(await service.signInWithCredential('not-the-token')).toMatchObject({ ok: false, kind: 'unauthorized' });
    expect(session.signedIn).toBe(false);
    expect(service.keepCredential('not-the-token')).toBe(false);
    expect(await service.signInWithCredential(CANARY_TOKEN)).toEqual({ ok: true, who, answer: {} });
    // Checked, set aside, not in force: nothing runs as it, and a reload finds nothing.
    expect(session.signedIn).toBe(false);
    expect(await service.request({ operation: 'listChannels' })).toEqual({ kind: 'unauthorized' });
    expect(await service.restore()).toMatchObject({ ok: false, kind: 'unauthorized' });
    // Kept with some other credential: refused, and the one set aside is gone too.
    expect(service.keepCredential(`${CANARY_TOKEN}x`)).toBe(false);
    expect(service.keepCredential(CANARY_TOKEN)).toBe(true);
    expect(session.signedIn).toBe(true);
    // Once only.
    expect(service.keepCredential(CANARY_TOKEN)).toBe(false);
  });

  it('lets a pasted credential lapse, and drops it with any change of identity', async () => {
    const { fake } = await ready();
    let now = 1_000;
    const session = new Session();
    const service = new Service({ client: new Client({ fetch: globalThis.fetch, session }), session, now: () => now, log: () => undefined });
    service.setServerAddress(fake.origin);
    await service.signInWithCredential(CANARY_TOKEN);
    now += PASTED_TTL_MS;
    expect(service.keepCredential(CANARY_TOKEN)).toBe(false);
    expect(session.signedIn).toBe(false);
    await service.signInWithCredential(CANARY_TOKEN);
    expect(await service.signOut()).toBe('ended');
    expect(service.keepCredential(CANARY_TOKEN)).toBe(false);
  });

  it('revokes the credential a change of server ends, and not one a sign-in only repeats', async () => {
    let revoked = 0;
    const { service } = await ready((request, res) => {
      if (request.method === 'DELETE') {
        revoked++;
        res.writeHead(204);
        res.end();
      } else conchd(request, res);
    });
    await paste(service);
    await paste(service);
    await new Promise((r) => setTimeout(r, 50));
    // The same credential again replaces nothing.
    expect(revoked).toBe(0);
    // A password sign-in that gives the credential in force replaces nothing either.
    await service.signIn('login', { name: 'ada', password: CANARY_PASSWORD });
    await new Promise((r) => setTimeout(r, 50));
    expect(revoked).toBe(0);
    expect(service.setServerAddress('http://127.0.0.1:1')).toMatchObject({ ok: true });
    await new Promise((r) => setTimeout(r, 100));
    expect(revoked).toBe(1);
  });

  it('restores a sign-in main holds, and says unauthorized when it holds none', async () => {
    const { service } = await ready();
    expect(await service.restore()).toMatchObject({ ok: false, kind: 'unauthorized' });
    await paste(service);
    expect(await service.restore()).toEqual({ ok: true, who, answer: {} });
  });
});

describe('sign-out', () => {
  it('forgets at once, revokes through the API, and aborts what was in flight', async () => {
    let release: () => void = () => undefined;
    const held = new Promise<void>((r) => {
      release = r;
    });
    const { service, session, fake } = await ready((request, res) => {
      if (request.url === '/v1/channels') void held.then(() => {
        sendJson(res, 200, { channels: [] });
      });
      else conchd(request, res);
    });
    await paste(service);
    const inFlight = service.request({ operation: 'listChannels' });
    await new Promise((r) => setTimeout(r, 50));
    expect(await service.signOut()).toBe('ended');
    expect(session.signedIn).toBe(false);
    expect(fake.seen.some((s) => s.method === 'DELETE' && s.url === '/v1/credentials/current')).toBe(true);
    release();
    expect(await inFlight).toEqual({ kind: 'none', reason: 'stale' });
    // And the next call has no credential to send.
    expect(await service.request({ operation: 'listChannels' })).toEqual({ kind: 'unauthorized' });
  });

  it('forgets locally even when the server cannot be reached, and says it did not tell it', async () => {
    const { service, session, fake } = await ready();
    await paste(service);
    await fake.close();
    servers.length = 0;
    expect(await service.signOut()).toBe('unsent');
    expect(session.signedIn).toBe(false);
  });

  it('says unconfirmed when the server answered something else', async () => {
    const { service } = await ready((request, res) => {
      if (request.method === 'DELETE') sendJson(res, 500, { code: 'internal', message: 'x' });
      else conchd(request, res);
    });
    await paste(service);
    expect(await service.signOut()).toBe('unconfirmed');
  });

  it('is ended when nobody was signed in', async () => {
    const { service } = await ready();
    expect(await service.signOut()).toBe('ended');
  });
});

describe('the request function', () => {
  it('does not run an operation the renderer may not invoke, a socket, or logout', async () => {
    const { service, fake } = await ready();
    for (const operation of ['login', 'wsTicket', 'voiceSession', 'messagesSocket', 'logout'] as const) {
      expect(await service.request({ operation })).toEqual({ kind: 'none', reason: 'unsupported' });
    }
    expect(fake.seen).toHaveLength(0);
  });

  it('changes server only through the address rules, and a change ends the sign-in', async () => {
    const { service, session } = await ready();
    await paste(service);
    expect(service.setServerAddress('http://example.org')).toEqual({ ok: false, refusal: 'scheme' });
    expect(session.signedIn).toBe(true);
    expect(service.setServerAddress('http://127.0.0.1:1')).toMatchObject({ ok: true });
    expect(session.signedIn).toBe(false);
  });
});
