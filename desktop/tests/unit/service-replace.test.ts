// The writes that replace the credential they are sent with (issue #1422):
// a password change, a first password, a guest's conversion, a re-
// authentication. Main sends them, takes the new token from the answer and
// keeps it; the renderer is answered the account without it.
import { afterEach, describe, expect, it, vi } from 'vitest';

import { Session } from '../../main/api/session.ts';
import { credential, principal, sendJson, startHttp, stack, who, type Fake, type Handler } from './support.ts';

const OLD_TOKEN = 'conch_old_token_canary_1a2b';
const NEW_TOKEN = 'conch_new_token_canary_9z8y';
const OLD_PASSWORD = 'old-password-canary-3141';
const NEW_PASSWORD = 'new-password-canary-2718';
const PROOF = 'enrollment-proof-canary-5772';

const servers: Fake[] = [];
afterEach(async () => {
  await Promise.all(servers.splice(0).map((s) => s.close()));
});

// A conchd where the signed-in token OLD_TOKEN may change its password and is
// then replaced by NEW_TOKEN (the server revokes the old one).
function conchd(state: { current: string; revokedOld: number; hold?: Promise<void> }, extra?: Handler): Handler {
  return (request, res) => {
    const bearer = request.headers['authorization'];
    const ok = bearer === `Bearer ${state.current}`;
    switch (`${request.method} ${request.url}`) {
      case 'PUT /v1/password/mine': {
        const body = JSON.parse(request.body) as { current: string; password: string };
        if (state.hold !== undefined) {
          void state.hold.then(() => {
            state.current = NEW_TOKEN;
            sendJson(res, 200, { credential, token: NEW_TOKEN, revoked: 2 });
          });
        } else if (!ok) sendJson(res, 401, { code: 'unauthorized', message: 'no' });
        else if (body.current !== OLD_PASSWORD) sendJson(res, 401, { code: 'auth_failed', message: 'wrong password' });
        else {
          state.current = NEW_TOKEN;
          sendJson(res, 200, { credential, token: NEW_TOKEN, revoked: 2 });
        }
        return;
      }
      case 'POST /v1/password/mine': {
        const body = JSON.parse(request.body) as { password: string; proof?: string };
        if (!ok) sendJson(res, 401, { code: 'unauthorized', message: 'no' });
        else if (body.proof !== PROOF) sendJson(res, 403, { code: 'reauth_required', message: 'needs proof' });
        else {
          state.current = NEW_TOKEN;
          sendJson(res, 200, { credential, token: NEW_TOKEN, revoked: 0 });
        }
        return;
      }
      case 'POST /v1/guest/convert':
        if (!ok) sendJson(res, 401, { code: 'unauthorized', message: 'no' });
        else {
          state.current = NEW_TOKEN;
          sendJson(res, 200, { principal, credential, token: NEW_TOKEN });
        }
        return;
      case 'POST /v1/reauth':
        if (!ok) sendJson(res, 401, { code: 'unauthorized', message: 'no' });
        else sendJson(res, 200, { until: '2026-11-06T10:10:00Z' });
        return;
      case 'GET /v1/whoami':
        if (ok) sendJson(res, 200, who);
        else sendJson(res, 401, { code: 'unauthorized', message: 'no' });
        return;
      case 'DELETE /v1/credentials/current':
        if (bearer === `Bearer ${OLD_TOKEN}`) state.revokedOld++;
        res.writeHead(204);
        res.end();
        return;
      default:
        if (extra !== undefined) extra(request, res);
        else sendJson(res, 404, { code: 'not_found', message: 'no such route' });
    }
  };
}

async function ready(extra?: Handler, hold?: Promise<void>) {
  const state: { current: string; revokedOld: number; hold?: Promise<void> } = { current: OLD_TOKEN, revokedOld: 0, ...(hold === undefined ? {} : { hold }) };
  const fake = await startHttp(conchd(state, extra));
  servers.push(fake);
  const logs: string[] = [];
  const s = stack((line) => logs.push(line));
  s.session.setOrigin(fake.origin);
  s.session.adopt(OLD_TOKEN);
  return { fake, logs, state, ...s };
}

function scan(...received: unknown[]): void {
  const text = JSON.stringify(received);
  for (const canary of [OLD_TOKEN, NEW_TOKEN, OLD_PASSWORD, NEW_PASSWORD, PROOF]) expect(text).not.toContain(canary);
}

describe('Session.rotate', () => {
  it('replaces the credential in force without a new identity: the generation does not move and nothing in flight is aborted', () => {
    const session = new Session();
    session.setOrigin('https://chat.example');
    session.adopt('a');
    const before = session.capture();
    expect(session.rotate('a', 'b')).toBe(true);
    expect(session.generation).toBe(before.generation);
    expect(before.signal.aborted).toBe(false);
    expect(session.current(before)).toBe(true);
    expect(session.capture().token).toBe('b');
  });

  it('does nothing unless old is the credential in force', () => {
    const session = new Session();
    expect(session.rotate('a', 'b')).toBe(false);
    session.setOrigin('https://chat.example');
    session.adopt('a');
    expect(session.rotate('x', 'b')).toBe(false);
    expect(session.capture().token).toBe('a');
    session.forget();
    expect(session.rotate('a', 'b')).toBe(false);
    expect(session.signedIn).toBe(false);
  });
});

describe('Session.successor', () => {
  it('names the credential in force for the one rotate replaced, in the identity it was made in only', () => {
    const session = new Session();
    session.setOrigin('https://chat.example');
    session.adopt('a');
    const before = session.capture();
    expect(session.successor(before, 'a')).toBeNull();
    session.rotate('a', 'b');
    expect(session.successor(before, 'a')).toBe('b');
    expect(session.successor(before, 'b')).toBeNull();
    expect(session.successor(before, 'x')).toBeNull();
    session.adopt('a');
    expect(session.successor(before, 'a')).toBeNull();
    expect(session.successor(session.capture(), 'a')).toBeNull();
    session.rotate('a', 'c');
    session.setOrigin('https://other.example');
    expect(session.successor(session.capture(), 'a')).toBeNull();
    session.adopt('a');
    session.rotate('a', 'd');
    session.forget();
    expect(session.successor(before, 'a')).toBeNull();
  });
});

describe('replaceCredential', () => {
  it('changes the password, keeps the new token in main, and answers the account without it', async () => {
    const { service, session, fake, logs } = await ready();
    const generation = session.generation;
    const outcome = await service.replaceCredential('changePassword', { current: OLD_PASSWORD, password: NEW_PASSWORD });
    expect(outcome).toEqual({ ok: true, who, answer: { credential, revoked: 2 } });
    expect(outcome.ok && 'token' in outcome.answer).toBe(false);
    expect(session.generation).toBe(generation);
    // The next call carries the new bearer, and the old one is not revoked
    // (the server did that with the change).
    await service.request({ operation: 'listChannels' });
    expect(fake.seen.at(-1)?.headers['authorization']).toBe(`Bearer ${NEW_TOKEN}`);
    scan(outcome, service.serverAddress(), logs);
  });

  it('keeps a request in flight across the rotation', async () => {
    let release: () => void = () => undefined;
    const held = new Promise<void>((r) => {
      release = r;
    });
    const { service } = await ready((request, res) => {
      if (request.url === '/v1/channels') {
        void held.then(() => {
          sendJson(res, 200, { channels: [] });
        });
      }
      else sendJson(res, 404, { code: 'not_found', message: 'no' });
    });
    const inFlight = service.request({ operation: 'listChannels' });
    await new Promise((r) => setTimeout(r, 50));
    expect(await service.replaceCredential('changePassword', { current: OLD_PASSWORD, password: NEW_PASSWORD })).toMatchObject({ ok: true });
    release();
    expect(await inFlight).toEqual({ kind: 'reply', status: 200, body: { channels: [] } });
  });

  it('sets a first password the same way', async () => {
    const { service, fake } = await ready();
    const outcome = await service.replaceCredential('setPassword', { password: NEW_PASSWORD, proof: PROOF });
    expect(outcome).toMatchObject({ ok: true, who, answer: { revoked: 0 } });
    scan(outcome);
    await service.request({ operation: 'listChannels' });
    expect(fake.seen.at(-1)?.headers['authorization']).toBe(`Bearer ${NEW_TOKEN}`);
  });

  it('converts a guest: the member’s token replaces the guest’s, in main alone', async () => {
    const { service, fake, session } = await ready();
    const outcome = await service.replaceCredential('convertGuest', { name: 'ada', password: NEW_PASSWORD });
    expect(outcome).toEqual({ ok: true, who, answer: { principal, credential } });
    scan(outcome);
    expect(session.signedIn).toBe(true);
    await service.request({ operation: 'listChannels' });
    expect(fake.seen.at(-1)?.headers['authorization']).toBe(`Bearer ${NEW_TOKEN}`);
  });

  it('proves the password again without changing the credential', async () => {
    const { service, fake } = await ready();
    const outcome = await service.replaceCredential('reauth', { password: OLD_PASSWORD });
    expect(outcome).toEqual({ ok: true, who: null, answer: { until: '2026-11-06T10:10:00Z' } });
    await service.request({ operation: 'listChannels' });
    expect(fake.seen.at(-1)?.headers['authorization']).toBe(`Bearer ${OLD_TOKEN}`);
  });

  it('keeps the person signed in on a wrong password, with the server’s refusal, and rotates nothing', async () => {
    const { service, session, fake } = await ready();
    const outcome = await service.replaceCredential('changePassword', { current: 'wrong', password: NEW_PASSWORD });
    expect(outcome).toMatchObject({ ok: false, kind: 'unauthorized', status: 401, code: 'auth_failed' });
    expect(session.signedIn).toBe(true);
    await service.request({ operation: 'listChannels' });
    expect(fake.seen.at(-1)?.headers['authorization']).toBe(`Bearer ${OLD_TOKEN}`);
    scan(outcome);
  });

  it('passes on a refusal that asks for a proof, so the page can ask', async () => {
    const { service, session } = await ready();
    expect(await service.replaceCredential('setPassword', { password: NEW_PASSWORD, proof: 'wrong' })).toMatchObject({ ok: false, kind: 'invalid', status: 403, code: 'reauth_required' });
    expect(session.signedIn).toBe(true);
  });

  it('is unauthorized, without asking, when nobody is signed in', async () => {
    const { service, session, fake } = await ready();
    session.forget();
    const before = fake.seen.length;
    expect(await service.replaceCredential('changePassword', { current: OLD_PASSWORD, password: NEW_PASSWORD })).toMatchObject({ ok: false, kind: 'unauthorized' });
    expect(fake.seen.length).toBe(before);
  });

  it('does not put a new credential in force for an identity that has ended meanwhile', async () => {
    let release: () => void = () => undefined;
    const held = new Promise<void>((r) => {
      release = r;
    });
    const { service, session } = await ready(undefined, held);
    const pending = service.replaceCredential('changePassword', { current: OLD_PASSWORD, password: NEW_PASSWORD });
    await new Promise((r) => setTimeout(r, 50));
    session.forget();
    release();
    // Main drops the answer of an identity that has ended, as it does for a
    // sign-in (client.ts): the token is never seen, let alone kept.
    expect(await pending).toEqual({ ok: false, kind: 'network', reason: 'stale' });
    expect(session.signedIn).toBe(false);
  });

  it('does not put a new credential in force when the credential was replaced meanwhile', async () => {
    let release: () => void = () => undefined;
    const held = new Promise<void>((r) => {
      release = r;
    });
    const { service, session } = await ready(undefined, held);
    const pending = service.replaceCredential('changePassword', { current: OLD_PASSWORD, password: NEW_PASSWORD });
    await new Promise((r) => setTimeout(r, 50));
    session.rotate(OLD_TOKEN, 'conch_someone_else_1');
    release();
    expect(await pending).toMatchObject({ ok: false, kind: 'network', reason: 'stale' });
    expect(session.capture().token).toBe('conch_someone_else_1');
  });

  it('writes nothing to the log: no password, no proof, no token', async () => {
    const { service, logs } = await ready();
    await service.replaceCredential('changePassword', { current: 'wrong', password: NEW_PASSWORD });
    await service.replaceCredential('changePassword', { current: OLD_PASSWORD, password: NEW_PASSWORD });
    await service.replaceCredential('setPassword', { password: NEW_PASSWORD, proof: PROOF });
    await service.replaceCredential('reauth', { password: NEW_PASSWORD });
    await service.replaceCredential('convertGuest', { name: 'ada', password: NEW_PASSWORD });
    scan(logs);
  });
});

describe('a request in flight across a password change (issue #1046)', () => {
  it('is re-sent once with the new bearer and the window stays signed in', async () => {
    let release: () => void = () => undefined;
    const held = new Promise<void>((r) => {
      release = r;
    });
    let channels = 0;
    const { service, session, fake } = await ready((request, res) => {
      if (request.url !== '/v1/channels') {
        sendJson(res, 404, { code: 'not_found', message: 'no' });
        return;
      }
      channels++;
      if (request.headers['authorization'] === `Bearer ${NEW_TOKEN}`) {
        sendJson(res, 200, { channels: [] });
        return;
      }
      void held.then(() => {
        sendJson(res, 401, { code: 'unauthorized', message: 'no' });
      });
    });
    const inFlight = service.request({ operation: 'listChannels' });
    await vi.waitFor(() => {
      expect(channels).toBe(1);
    });
    expect(await service.replaceCredential('changePassword', { current: OLD_PASSWORD, password: NEW_PASSWORD })).toMatchObject({ ok: true });
    release();
    expect(await inFlight).toMatchObject({ kind: 'reply', status: 200 });
    expect(channels).toBe(2);
    expect(session.signedIn).toBe(true);
    expect(fake.seen.filter((x) => x.url === '/v1/channels').map((x) => x.headers['authorization'])).toEqual([`Bearer ${OLD_TOKEN}`, `Bearer ${NEW_TOKEN}`]);
  });
});
