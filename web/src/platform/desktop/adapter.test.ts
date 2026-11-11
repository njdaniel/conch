import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { errorV0, loginResponseV1, passwordUpdatedResponseV1, whoAmIResponseV1 } from '../../schema/generated';
import { call, previewCall } from '../operations';
import type { BridgeReply, BridgeRequest, ConchDesktopBridge, ReplaceCredentialRequest, SignInOutcome } from './bridge';
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
    replaceCredential: unexpected('replaceCredential'),
    createMemberLink: unexpected('createMemberLink'),
    createSignInLink: unexpected('createSignInLink'),
    totpEnroll: unexpected('totpEnroll'),
    createPrincipalSignInLink: unexpected('createPrincipalSignInLink'),
    createPrincipalCredential: unexpected('createPrincipalCredential'),
    createHook: unexpected('createHook'),
    profiles: unexpected('profiles'),
    profileAdd: unexpected('profileAdd'),
    profileRename: unexpected('profileRename'),
    profileRemove: unexpected('profileRemove'),
    profileSwitch: unexpected('profileSwitch'),
    storageRetry: unexpected('storageRetry'),
    profilesRecover: unexpected('profilesRecover'),
    uploadFile: unexpected('uploadFile'),
    downloadFile: unexpected('downloadFile'),
    saveFile: unexpected('saveFile'),
    notify: unexpected('notify'),
    notifyClose: unexpected('notifyClose'),
    notificationSupported: () => false,
    onNotification: () => () => undefined,
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

  // An operation main refuses is not a network failure: the server was never
  // asked, so the page is not told it cannot reach it (issue #1768).
  it.each([
    ['a socket ticket', call('wsTicket')],
    ['the person’s own voice grant', call('voiceSession', { channel: 'a' })],
  ] as const)('answers %s, which main alone may ask for, as the app’s own refusal and not as a network failure', async (_name, op) => {
    const request = vi.fn((): Promise<BridgeReply> => Promise.resolve({ kind: 'none' }));
    install(fakeBridge({ request }));
    const res = await http.send(op, STAND_IN, { keepalive: false });
    expect(res.status).toBe(501);
    expect(await res.json()).toEqual({ code: 'desktop_unavailable', message: 'The desktop app does not do this.' });
    expect(request).not.toHaveBeenCalled();
  });

  it('answers main’s own unsupported as the same refusal, and a real failure to answer as a network failure', async () => {
    install(fakeBridge({ request: () => Promise.resolve({ kind: 'none', reason: 'unsupported' }) }));
    expect((await http.get(call('whoami'), STAND_IN)).status).toBe(501);
    install(fakeBridge({ request: () => Promise.resolve({ kind: 'none', reason: 'certificate' }) }));
    await expect(http.get(call('whoami'), STAND_IN)).rejects.toBeInstanceOf(TypeError);
  });

  it('uploads a file as base64 through the bridge, with the name and declared type, and reads the server’s answer', async () => {
    const uploadFile = vi.fn((): Promise<BridgeReply> => Promise.resolve({ kind: 'reply', status: 201, body: { id: 7 } }));
    install(fakeBridge({ uploadFile }));
    const res = await http.upload(call('uploadFile', { channel: 'general' }, 'filename=a%20b.png'), STAND_IN, 'image/png', new Blob([new Uint8Array([1, 2, 3])]), new AbortController().signal);
    expect(res.status).toBe(201);
    expect(uploadFile).toHaveBeenCalledExactlyOnceWith({ channel: 'general', filename: 'a b.png', mediaType: 'image/png', data: 'AQID' });
  });

  it('does not upload to another operation, or without a name', async () => {
    const uploadFile = vi.fn((): Promise<BridgeReply> => Promise.resolve({ kind: 'none' }));
    install(fakeBridge({ uploadFile }));
    const signal = new AbortController().signal;
    await expect(http.upload(call('postMessage', { channel: 'a' }), STAND_IN, 'x/y', new Blob(), signal)).rejects.toBeInstanceOf(TypeError);
    await expect(http.upload(call('uploadFile', { channel: 'a' }), STAND_IN, 'x/y', new Blob(), signal)).rejects.toBeInstanceOf(TypeError);
    expect(uploadFile).not.toHaveBeenCalled();
  });

  it('downloads a file’s bytes as a response, and a refusal as its status', async () => {
    install(fakeBridge({ downloadFile: () => Promise.resolve({ kind: 'bytes', data: 'AQID' }) }));
    const res = await http.download(call('fileBytes', { id: 1 }), STAND_IN);
    expect(new Uint8Array(await res.arrayBuffer())).toEqual(new Uint8Array([1, 2, 3]));
    install(fakeBridge({ downloadFile: () => Promise.resolve({ kind: 'reply', status: 404, body: { code: 'file_not_found', message: 'no' } }) }));
    expect((await http.download(call('fileBytes', { id: 1 }), STAND_IN)).status).toBe(404);
    await expect(http.download(call('whoami'), STAND_IN)).rejects.toBeInstanceOf(TypeError);
  });

  it('ends a wait when the caller’s signal aborts', async () => {
    install(fakeBridge({ request: () => new Promise(() => undefined) }));
    const controller = new AbortController();
    const pending = http.send(call('postMessage', { channel: 'a' }), STAND_IN, { body: {}, keepalive: false, signal: controller.signal });
    controller.abort();
    await expect(pending).rejects.toMatchObject({ name: 'AbortError' });
  });
});

describe('the desktop transport: secrets made for someone else (issue #1768)', () => {
  const secret = 'display-secret-canary-31';
  const answers = {
    createMemberLink: { invite: { id: 1 }, code: secret },
    createSignInLink: { sign_in_link: { id: 1 }, code: secret },
    totpEnroll: { secret },
    createPrincipalSignInLink: { sign_in_link: { id: 1 }, code: secret },
    createPrincipalCredential: { credential: { id: 1 }, token: secret },
    createHook: { id: 1, token: secret },
  } as const;

  // Each operation, the call and body the client makes, and what the bridge
  // function is given.
  const rows = [
    ['createMemberLink', call('createMemberLink'), { kind: 'member' }, { kind: 'member' }],
    ['createSignInLink', call('createSignInLink'), { label: 'phone' }, { label: 'phone' }],
    ['totpEnroll', call('totpEnroll'), undefined, undefined],
    ['createPrincipalSignInLink', call('createPrincipalSignInLink', { id: 12 }), { purpose: 'sign_in' }, { id: 12, body: { purpose: 'sign_in' } }],
    ['createPrincipalCredential', call('createPrincipalCredential', { id: 12 }), { label: 'ci' }, { id: 12, body: { label: 'ci' } }],
    ['createHook', call('createHook'), { channel: 'general', principal: 12 }, { channel: 'general', principal: 12 }],
  ] as const;

  it.each(rows)('sends %s to its own bridge function, never the generic request, and reads the answer main gives', async (name, op, body, given) => {
    const reply = (): Promise<BridgeReply> => Promise.resolve({ kind: 'reply', status: 200, body: answers[name] });
    const fn = vi.fn(reply);
    const request = vi.fn();
    install(fakeBridge({ [name]: fn, request }));
    const res = await http.send(op, STAND_IN, { ...(body === undefined ? {} : { body }), keepalive: false });
    expect(res.status).toBe(200);
    expect(await res.json()).toEqual(answers[name]);
    expect(request).not.toHaveBeenCalled();
    if (given === undefined) expect(fn).toHaveBeenCalledExactlyOnceWith();
    else expect(fn).toHaveBeenCalledExactlyOnceWith(given);
  });

  it.each(rows)('keeps the server’s refusal of %s, with its status, code and Retry-After, and does not call it a network failure', async (name, op, body) => {
    install(fakeBridge({ [name]: () => Promise.resolve({ kind: 'reply', status: 429, body: { code: 'rate_limited', message: 'slow' }, retryAfter: 9 }) }));
    const res = await http.send(op, STAND_IN, { ...(body === undefined ? {} : { body }), keepalive: false });
    expect(res.status).toBe(429);
    expect(res.headers.get('Retry-After')).toBe('9');
    expect(await res.json()).toMatchObject({ code: 'rate_limited' });
  });

  it.each(rows)('reads a sign-in that has ended during %s as 401', async (name, op, body) => {
    install(fakeBridge({ [name]: () => Promise.resolve({ kind: 'unauthorized' }) }));
    expect((await http.send(op, STAND_IN, { ...(body === undefined ? {} : { body }), keepalive: false })).status).toBe(401);
  });

  it.each([
    ['createMemberLink', call('createMemberLink'), { kind: 5 }],
    ['createPrincipalCredential', call('createPrincipalCredential', { id: 12 }), { label: 5 }],
    ['createHook', call('createHook'), { channel: 'general' }],
  ] as const)('sends a body that is not %s’s request to nobody', async (name, op, body) => {
    const fn = vi.fn();
    install(fakeBridge({ [name]: fn }));
    await expect(http.send(op, STAND_IN, { body, keepalive: false })).rejects.toBeInstanceOf(TypeError);
    expect(fn).not.toHaveBeenCalled();
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

describe('the desktop transport: the writes that replace the credential (issue #1422)', () => {
  const rotated = { revoked: 2 } as const;

  it('sends a password change to main alone and reads the answer with the stand-in where the token was', async () => {
    const replaceCredential = vi.fn((...[,]: [ReplaceCredentialRequest]): Promise<SignInOutcome> => Promise.resolve({ ok: true, who, answer: { ...rotated, credential } }));
    const request = vi.fn();
    install(fakeBridge({ replaceCredential, request }));
    const res = await http.send(call('changePassword'), STAND_IN, { keepalive: false, body: { current: 'old-canary', password: 'new-canary' } });
    expect(replaceCredential).toHaveBeenCalledWith({ operation: 'changePassword', body: { current: 'old-canary', password: 'new-canary' } });
    // Not through the generic request, which refuses the operation anyway.
    expect(request).not.toHaveBeenCalled();
    const body: unknown = await res.json();
    expect(passwordUpdatedResponseV1(body)).toEqual([]);
    expect(body).toMatchObject({ token: STAND_IN, revoked: 2 });
  });

  it.each([
    ['setPassword', { password: 'p', proof: 'c' }],
    ['convertGuest', { name: 'ada', password: 'p' }],
    ['reauth', { password: 'p' }],
  ] as const)('routes %s to the same function', async (op, body) => {
    const replaceCredential = vi.fn((...[,]: [ReplaceCredentialRequest]): Promise<SignInOutcome> => Promise.resolve({ ok: true, who: null, answer: { until: '2026-11-06T10:10:00Z' } }));
    install(fakeBridge({ replaceCredential }));
    await http.send(call(op), STAND_IN, { keepalive: false, body });
    expect(replaceCredential).toHaveBeenCalledWith({ operation: op, body });
  });

  it('keeps the server’s refusal of a wrong password: 401 auth_failed', async () => {
    install(fakeBridge({ replaceCredential: () => Promise.resolve({ ok: false, kind: 'unauthorized', status: 401, code: 'auth_failed', message: 'wrong' }) }));
    const res = await http.send(call('changePassword'), STAND_IN, { keepalive: false, body: { current: 'x', password: 'y' } });
    expect(res.status).toBe(401);
    expect(await res.json()).toMatchObject({ code: 'auth_failed' });
  });

  it('sends a body that is not the operation’s request to nobody', async () => {
    const replaceCredential = vi.fn();
    install(fakeBridge({ replaceCredential }));
    await expect(http.send(call('changePassword'), STAND_IN, { keepalive: false, body: { current: 1 } })).rejects.toBeInstanceOf(TypeError);
    expect(replaceCredential).not.toHaveBeenCalled();
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
    const keepCredential = vi.fn((...[,]: [string]): Promise<boolean> => new Promise<boolean>((r) => { release = r; }));
    const request = vi.fn((): Promise<BridgeReply> => Promise.resolve({ kind: 'reply', status: 200, body: who }));
    const signInWithCredential = vi.fn((): Promise<SignInOutcome> => Promise.resolve({ ok: true, who, answer: {}, handle: 'paste-0123' }));
    install(fakeBridge({ keepCredential, request, signInWithCredential }));
    await http.get(call('whoami'), 'conch_pasted_secret');
    const kept = credentials.write('conch_pasted_secret', 'lasting');
    // Main is told the handle it issued, never the credential.
    expect(keepCredential).toHaveBeenCalledWith('paste-0123');
    expect(JSON.stringify(keepCredential.mock.calls)).not.toContain('conch_pasted_secret');
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

  it('uses a handle once: a second keep has none to present', async () => {
    const keepCredential = vi.fn((...[,]: [string]): Promise<boolean> => Promise.resolve(true));
    install(fakeBridge({ signInWithCredential: () => Promise.resolve({ ok: true, who, answer: {}, handle: 'paste-0123' }), keepCredential }));
    await http.get(call('whoami'), 'conch_pasted_secret');
    expect(credentials.write('conch_pasted_secret', 'lasting')).not.toBeNull();
    expect(credentials.write('conch_pasted_secret', 'lasting')).toBeNull();
    expect(keepCredential).toHaveBeenCalledTimes(1);
  });

  it('drops the handle of a credential that failed its check', async () => {
    const keepCredential = vi.fn((...[,]: [string]): Promise<boolean> => Promise.resolve(true));
    const outcomes: SignInOutcome[] = [
      { ok: true, who, answer: {}, handle: 'paste-0123' },
      { ok: false, kind: 'unauthorized', status: 401 },
    ];
    install(fakeBridge({ signInWithCredential: () => Promise.resolve(outcomes.shift() as SignInOutcome), keepCredential }));
    await http.get(call('whoami'), 'conch_pasted_secret');
    await http.get(call('whoami'), 'another_secret');
    expect(credentials.write('another_secret', 'lasting')).toBeNull();
    expect(keepCredential).not.toHaveBeenCalled();
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

describe('Preview as member through the desktop transport (issue #1540)', () => {
  const ok = (): Promise<BridgeReply> => Promise.resolve({ kind: 'reply', status: 200, body: {} });

  it('hands main previewAs on each of the five previewable reads, and on those alone', async () => {
    const request = vi.fn<(r: BridgeRequest) => Promise<BridgeReply>>(() => ok());
    install(fakeBridge({ request }));
    await http.get(previewCall('preview', 7), STAND_IN);
    await http.get(previewCall('listChannels', 7), STAND_IN);
    await http.get(previewCall('channelDirectory', 7), STAND_IN);
    await http.get(previewCall('directoryMembers', 7, {}, 'limit=5'), STAND_IN);
    await http.get(previewCall('listMembers', 7, { channel: 'general' }), STAND_IN);
    expect(request.mock.calls.map(([r]) => [r.operation, r.previewAs])).toEqual([
      ['preview', 7],
      ['listChannels', 7],
      ['channelDirectory', 7],
      ['directoryMembers', 7],
      ['listMembers', 7],
    ]);
    request.mockClear();
    await http.get(call('listChannels'), STAND_IN);
    expect(request.mock.calls[0]?.[0]).not.toHaveProperty('previewAs');
  });

  it('never hands main previewAs for an operation that is not previewable, even from a hand-built call', async () => {
    const request = vi.fn<(r: BridgeRequest) => Promise<BridgeReply>>(() => ok());
    install(fakeBridge({ request }));
    const forged = { ...call('listMessages', { channel: 'a' }), previewAs: 7 };
    await expect(http.get(forged, STAND_IN)).rejects.toBeInstanceOf(TypeError);
    await expect(http.send({ ...call('postMessage', { channel: 'a' }), previewAs: 7 }, STAND_IN, { body: {}, keepalive: false })).rejects.toBeInstanceOf(TypeError);
    await expect(http.open({ ...call('health'), previewAs: 7 }, undefined)).rejects.toBeInstanceOf(TypeError);
    await expect(http.health({ ...call('health'), previewAs: 7 })).rejects.toBeInstanceOf(TypeError);
    expect(request).not.toHaveBeenCalled();
  });

  it('refuses previewAs at the top of upload and download', async () => {
    const uploadFile = vi.fn(() => ok());
    const downloadFile = vi.fn(() => Promise.resolve({ kind: 'bytes', data: 'AQID' } as const));
    install(fakeBridge({ uploadFile, downloadFile }));
    const up = { ...call('uploadFile', { channel: 'general' }, 'filename=a.png'), previewAs: 7 };
    await expect(http.upload(up, STAND_IN, 'image/png', new Blob([new Uint8Array([1])]), new AbortController().signal)).rejects.toBeInstanceOf(TypeError);
    await expect(http.download({ ...call('fileBytes', { id: 1 }), previewAs: 7 }, STAND_IN)).rejects.toBeInstanceOf(TypeError);
    expect(uploadFile).not.toHaveBeenCalled();
    expect(downloadFile).not.toHaveBeenCalled();
  });
});
