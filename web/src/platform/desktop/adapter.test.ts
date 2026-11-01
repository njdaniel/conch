import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { errorV0, loginResponseV1, passwordUpdatedResponseV1, whoAmIResponseV1 } from '../../schema/generated';
import { call } from '../operations';
import type { BridgeReply, ConchDesktopBridge, SignInOutcome } from './bridge';
import { credentials, isSessionToken, STAND_IN } from './credentials';
import { http } from './http';

// A bridge whose every function is a spy that fails the test unless it has
// been given an answer.
function fakeBridge(overrides: Partial<ConchDesktopBridge>): ConchDesktopBridge {
  const unexpected = (name: string) => () => Promise.reject(new Error(`unexpected bridge call: ${name}`));
  return {
    serverAddress: unexpected('serverAddress'),
    request: unexpected('request'),
    subscribe: () => () => undefined,
    restore: unexpected('restore'),
    signInWithCredential: unexpected('signInWithCredential'),
    keepCredential: unexpected('keepCredential'),
    signInWithPassword: unexpected('signInWithPassword'),
    signInWithCode: unexpected('signInWithCode'),
    redeemInvite: unexpected('redeemInvite'),
    redeemInviteAsGuest: unexpected('redeemInviteAsGuest'),
    redeemSignInLink: unexpected('redeemSignInLink'),
    redeemSetupToken: unexpected('redeemSetupToken'),
    resetPassword: unexpected('resetPassword'),
    joinAsGuest: unexpected('joinAsGuest'),
    createAccount: unexpected('createAccount'),
    signOut: unexpected('signOut'),
    profiles: unexpected('profiles'),
    profileAdd: unexpected('profileAdd'),
    profileRename: unexpected('profileRename'),
    profileRemove: unexpected('profileRemove'),
    profileSwitch: unexpected('profileSwitch'),
    storageRetry: unexpected('storageRetry'),
    profilesRecover: unexpected('profilesRecover'),
    ...overrides,
  };
}

function install(bridge: ConchDesktopBridge): void {
  Object.defineProperty(window, 'conchDesktop', { value: bridge, configurable: true, writable: true });
}

afterEach(() => {
  Reflect.deleteProperty(window, 'conchDesktop');
  const held = credentials.read();
  if (held !== null) credentials.drop(held);
});

const who = { id: 7, kind: 'human', name: 'ada', role: 'member' } as const;
const principal = { id: 7, kind: 'human', name: 'ada', created_at: '2026-10-29T10:00:00Z' } as const;
const credential = { id: 3, principal_id: 7, label: 'login', created_at: '2026-10-29T10:00:00Z' } as const;

describe('the desktop transport: a call of the table is a call of the bridge', () => {
  it('asks main for a call with its params and query, and answers with the document', async () => {
    const request = vi.fn((): Promise<BridgeReply> => Promise.resolve({ kind: 'reply', status: 200, body: { channels: [] } }));
    install(fakeBridge({ request }));
    const res = await http.get(call('listMessages', { channel: 'general' }, 'limit=5'), STAND_IN);
    expect(request).toHaveBeenCalledWith({ operation: 'listMessages', params: { channel: 'general' }, query: 'limit=5' });
    expect(res.status).toBe(200);
    expect(await res.json()).toEqual({ channels: [] });
  });

  it('never passes the token it is given to main', async () => {
    const request = vi.fn((): Promise<BridgeReply> => Promise.resolve({ kind: 'reply', status: 204 }));
    install(fakeBridge({ request }));
    await http.send(call('markRead', { channel: 'a' }), 'canary-token-123', { body: { last_seen_id: 4 }, keepalive: false });
    expect(JSON.stringify(request.mock.calls)).not.toContain('canary-token-123');
  });

  it('rebuilds a refusal with its status, its document and its Retry-After', async () => {
    install(fakeBridge({ request: () => Promise.resolve({ kind: 'reply', status: 429, body: { code: 'rate_limited', message: 'slow down' }, retryAfter: 9, requestId: 'req-1' }) }));
    const res = await http.get(call('whoami'), STAND_IN);
    expect(res.status).toBe(429);
    expect(res.headers.get('Retry-After')).toBe('9');
    expect(res.headers.get('X-Request-Id')).toBe('req-1');
    expect(errorV0(await res.json())).toEqual([]);
  });

  it('is a 401 when main says the sign-in ended', async () => {
    install(fakeBridge({ request: () => Promise.resolve({ kind: 'unauthorized' }) }));
    const res = await http.get(call('whoami'), STAND_IN);
    expect(res.status).toBe(401);
  });

  it('is no answer at all, as fetch rejects, when main had none, and says a certificate in words', async () => {
    install(fakeBridge({ request: () => Promise.resolve({ kind: 'none', reason: 'certificate' }) }));
    await expect(http.get(call('whoami'), STAND_IN)).rejects.toThrow(/certificate is not trusted/);
    install(fakeBridge({ request: () => Promise.resolve({ kind: 'none' }) }));
    await expect(http.get(call('whoami'), STAND_IN)).rejects.toBeInstanceOf(TypeError);
  });

  it('rejects when the bridge is missing', async () => {
    await expect(http.get(call('whoami'), STAND_IN)).rejects.toBeInstanceOf(TypeError);
  });

  it('does not ask main for an operation whose answer holds a credential', async () => {
    const request = vi.fn((): Promise<BridgeReply> => Promise.resolve({ kind: 'none' }));
    install(fakeBridge({ request }));
    await expect(http.send(call('createInvite'), STAND_IN, { body: {}, keepalive: false })).rejects.toBeInstanceOf(TypeError);
    await expect(http.send(call('voiceSession', { channel: 'a' }), STAND_IN, { keepalive: false })).rejects.toBeInstanceOf(TypeError);
    expect(request).not.toHaveBeenCalled();
  });

  it('has no attachments yet', async () => {
    install(fakeBridge({}));
    await expect(http.download(call('fileBytes', { id: 1 }), STAND_IN)).rejects.toBeInstanceOf(TypeError);
    await expect(http.upload(call('uploadFile', { channel: 'a' }), STAND_IN, 'image/png', new Blob(), new AbortController().signal)).rejects.toBeInstanceOf(TypeError);
  });

  it('ends a wait when the caller’s signal aborts', async () => {
    install(fakeBridge({ request: () => new Promise(() => undefined) }));
    const controller = new AbortController();
    const pending = http.send(call('postMessage', { channel: 'a' }), STAND_IN, { body: {}, keepalive: false, signal: controller.signal });
    controller.abort();
    await expect(pending).rejects.toMatchObject({ name: 'AbortError' });
  });
});

describe('the desktop transport: sign-in', () => {
  const answer = { next: 'signed_in', principal, credential } as const;

  it('turns a password sign-in into the document the client reads, with the stand-in where the token was', async () => {
    const signInWithPassword = vi.fn((): Promise<SignInOutcome> => Promise.resolve({ ok: true, who, answer }));
    install(fakeBridge({ signInWithPassword }));
    const res = await http.open(call('login'), { name: 'ada', password: 'pw-canary-1' });
    expect(signInWithPassword).toHaveBeenCalledWith({ name: 'ada', password: 'pw-canary-1' });
    const body: unknown = await res.json();
    expect(loginResponseV1(body)).toEqual([]);
    expect(body).toMatchObject({ next: 'signed_in', token: STAND_IN });
  });

  it('turns a challenge into the totp document, with main’s handle', async () => {
    install(fakeBridge({ signInWithPassword: () => Promise.resolve({ ok: false, kind: 'challenge', challenge: 'challenge-abc', expiresAt: '2026-10-29T10:05:00Z' }) }));
    const body: unknown = await (await http.open(call('login'), { name: 'ada', password: 'pw' })).json();
    expect(loginResponseV1(body)).toEqual([]);
    expect(body).toEqual({ next: 'totp', challenge: 'challenge-abc', challenge_expires_at: '2026-10-29T10:05:00Z' });
  });

  it('keeps the server’s refusal: status, code, message and Retry-After', async () => {
    install(
      fakeBridge({ signInWithPassword: () => Promise.resolve({ ok: false, kind: 'unauthorized', status: 401, code: 'auth_failed', message: 'no', retryAfter: 3 }) }),
    );
    const res = await http.open(call('login'), { name: 'ada', password: 'x' });
    expect(res.status).toBe(401);
    expect(res.headers.get('Retry-After')).toBe('3');
    expect(await res.json()).toMatchObject({ code: 'auth_failed', message: 'no' });
  });

  it('is no answer for a network failure, and says why', async () => {
    install(fakeBridge({ signInWithPassword: () => Promise.resolve({ ok: false, kind: 'network', reason: 'certificate' }) }));
    await expect(http.open(call('login'), { name: 'ada', password: 'x' })).rejects.toThrow(/certificate/);
  });

  it('sends a body that is not the operation’s request to nobody', async () => {
    const signInWithPassword = vi.fn();
    install(fakeBridge({ signInWithPassword }));
    await expect(http.open(call('login'), { name: 1 })).rejects.toBeInstanceOf(TypeError);
    expect(signInWithPassword).not.toHaveBeenCalled();
  });

  it('gives a reset’s answer as it is: it signs nobody in and holds no token', async () => {
    install(fakeBridge({ resetPassword: () => Promise.resolve({ ok: true, who: null, answer: { revoked: 2 } }) }));
    const body: unknown = await (await http.open(call('resetPassword'), { code: 'c', password: 'p' })).json();
    expect(passwordUpdatedResponseV1(body)).toEqual([]);
    expect(body).toEqual({ revoked: 2 });
  });

  it('routes each step of the family to its own bridge function', async () => {
    const ok: SignInOutcome = { ok: true, who, answer: { principal, credential } };
    const names = ['redeemInvite', 'redeemInviteAsGuest', 'redeemSignInLink', 'joinAsGuest', 'createAccount', 'signInWithCode', 'redeemSetupToken'] as const;
    const spies = Object.fromEntries(names.map((n) => [n, vi.fn(() => Promise.resolve(ok))]));
    install(fakeBridge(spies));
    await http.open(call('redeemInvite'), { code: 'c', name: 'n', password: 'p' });
    await http.open(call('redeemInviteGuest'), { code: 'c' });
    await http.open(call('redeemSignInLink'), { code: 'c' });
    await http.open(call('joinGuest'), {});
    await http.open(call('joinAccount'), { name: 'n', password: 'p' });
    await http.open(call('loginTotp'), { challenge: 'c', code: '123456' });
    await http.open(call('setupRedeem'), { token: 't' });
    for (const n of names) expect(spies[n], n).toHaveBeenCalledTimes(1);
  });

  it('sends a front-door read as a plain request', async () => {
    const request = vi.fn((): Promise<BridgeReply> => Promise.resolve({ kind: 'reply', status: 200, body: {} }));
    install(fakeBridge({ request }));
    await http.open(call('joinInfo'));
    expect(request).toHaveBeenCalledWith({ operation: 'joinInfo' });
  });
});

describe('the desktop transport: a pasted credential', () => {
  it('goes to main to be checked, and what comes back is the account', async () => {
    const signInWithCredential = vi.fn((): Promise<SignInOutcome> => Promise.resolve({ ok: true, who, answer: {} }));
    install(fakeBridge({ signInWithCredential }));
    const res = await http.get(call('whoami'), 'conch_pasted_secret');
    expect(signInWithCredential).toHaveBeenCalledWith('conch_pasted_secret');
    expect(whoAmIResponseV1(await res.json())).toEqual([]);
  });

  it('is put in force in main only when the client keeps it, and the store keeps the stand-in', async () => {
    let release: (value: boolean) => void = () => undefined;
    const keepCredential = vi.fn((): Promise<boolean> => new Promise<boolean>((r) => { release = r; }));
    const request = vi.fn((): Promise<BridgeReply> => Promise.resolve({ kind: 'reply', status: 200, body: who }));
    install(fakeBridge({ keepCredential, request }));
    const kept = credentials.write('conch_pasted_secret', 'lasting');
    expect(keepCredential).toHaveBeenCalledWith('conch_pasted_secret');
    // What the client pins is the stand-in: the page no longer holds the paste.
    expect(kept).toEqual({ token: STAND_IN, place: 'lasting' });
    expect(credentials.read()).toEqual({ token: STAND_IN, place: 'lasting' });
    expect(isSessionToken('conch_pasted_secret')).toBe(false);
    // No call goes ahead of the hand-over.
    const next = http.get(call('whoami'), STAND_IN);
    await new Promise((r) => setTimeout(r, 10));
    expect(request).not.toHaveBeenCalled();
    release(true);
    await next;
    expect(request).toHaveBeenCalledTimes(1);
    expect(JSON.stringify(request.mock.calls)).not.toContain('conch_pasted_secret');
  });

  it('is never put in force when the client does not keep it (an operator declining the warning)', async () => {
    const keepCredential = vi.fn((): Promise<boolean> => Promise.resolve(true));
    install(fakeBridge({ signInWithCredential: () => Promise.resolve({ ok: true, who, answer: {} }), keepCredential }));
    await http.get(call('whoami'), 'conch_pasted_secret');
    expect(keepCredential).not.toHaveBeenCalled();
    expect(credentials.read()).toBeNull();
  });

  it('is a 401 when main refuses it', async () => {
    install(fakeBridge({ signInWithCredential: () => Promise.resolve({ ok: false, kind: 'unauthorized', status: 401 }) }));
    expect((await http.get(call('whoami'), 'nope')).status).toBe(401);
  });
});

describe('the desktop transport: sign-out', () => {
  beforeEach(() => {
    install(fakeBridge({}));
  });

  it.each([
    ['ended', 204],
    ['unconfirmed', 502],
  ] as const)('%s is a %i', async (outcome, status) => {
    install(fakeBridge({ signOut: () => Promise.resolve(outcome) }));
    const res = await http.send(call('logout'), STAND_IN, { keepalive: false });
    expect(res.status).toBe(status);
  });

  it('unsent is no answer, so the client says it could not tell the server', async () => {
    install(fakeBridge({ signOut: () => Promise.resolve('unsent') }));
    await expect(http.send(call('logout'), STAND_IN, { keepalive: false })).rejects.toBeInstanceOf(TypeError);
  });
});

describe('the desktop credential store', () => {
  it('keeps a stand-in in memory only, tab and lasting alike, and no web storage', () => {
    window.localStorage.clear();
    window.sessionStorage.clear();
    expect(credentials.read()).toBeNull();
    expect(credentials.write(STAND_IN, 'lasting')).toEqual({ token: STAND_IN, place: 'lasting' });
    expect(credentials.read()).toEqual({ token: STAND_IN, place: 'lasting' });
    expect(credentials.isStored({ token: STAND_IN, place: 'lasting' })).toBe(true);
    expect(credentials.isStored({ token: STAND_IN, place: 'tab' })).toBe(false);
    // A late drop of a credential that is not the stored one changes nothing.
    credentials.drop({ token: 'stale', place: 'lasting' });
    expect(credentials.read()).not.toBeNull();
    credentials.drop({ token: STAND_IN, place: 'lasting' });
    expect(credentials.read()).toBeNull();
    expect(window.localStorage.length + window.sessionStorage.length).toBe(0);
  });

  it('keeps preferences in memory', () => {
    credentials.writePreference('k', 'v');
    expect(credentials.readPreference('k')).toBe('v');
    expect(window.localStorage.getItem('k')).toBeNull();
  });
});
