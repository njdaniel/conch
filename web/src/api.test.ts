import { afterEach, describe, expect, it, vi } from 'vitest';
import {
  claimVoiceShare,
  closeInvite,
  continueAsOperator,
  createInvite,
  hasToken,
  heldElsewhere,
  listChannels,
  listInvites,
  onSignedOutElsewhere,
  openSocket,
  postMessage,
  previewInvite,
  redeemInvite,
  restore,
  signIn,
  signOut,
  signOutEverywhere,
  stopVoiceShare,
  whoAmI,
} from './api';

afterEach(async () => {
  vi.unstubAllGlobals();
  window.sessionStorage.clear();
  window.localStorage.clear();
  // With nothing stored, the next call finds the credential gone and forgets
  // it, so no test starts signed in as the last one ended.
  await listChannels();
});

const key = 'conch.web.token';
const memberToken = 'conch_member_secret';
const otherMemberToken = 'conch_other_member_secret';
const operatorToken = 'conch_operator_secret';
const json = (body: unknown, status = 200) => new Response(JSON.stringify(body), { status });
const member = { id: 2, kind: 'human', name: 'ada', role: 'member' };
const otherMember = { id: 3, kind: 'human', name: 'bea', role: 'member' };
const operator = { id: 1, kind: 'human', name: 'nick', role: 'operator' };

interface Call {
  readonly path: string;
  readonly method: string;
  readonly auth: string | null;
}

// serve stands in for conchd. answer gets each request and returns its
// response; every call is recorded.
function serve(answer: (call: Call) => Response | Promise<Response>): Call[] {
  const calls: Call[] = [];
  vi.stubGlobal(
    'fetch',
    vi.fn((input: RequestInfo | URL, init?: RequestInit) => {
      const path = typeof input === 'string' ? input : input instanceof URL ? input.pathname : input.url;
      const call = { path, method: init?.method ?? 'GET', auth: new Headers(init?.headers).get('Authorization') };
      calls.push(call);
      return Promise.resolve(answer(call));
    }),
  );
  return calls;
}

const whoami = (call: Call) =>
  call.auth === `Bearer ${memberToken}`
    ? json(member)
    : call.auth === `Bearer ${otherMemberToken}`
      ? json(otherMember)
      : call.auth === `Bearer ${operatorToken}`
        ? json(operator)
        : json({}, 401);

// signedInFrom stores token where an earlier load would have, and signs in
// with it as the page does on load.
async function signedInFrom(storage: Storage, token: string): Promise<void> {
  storage.setItem(key, token);
  serve(whoami);
  const result = await restore();
  expect(result.ok).toBe(true);
}

describe('openSocket', () => {
  it('answers a plain network failure, carrying nothing of the error, when the WebSocket constructor throws', async () => {
    await signedInFrom(window.sessionStorage, operatorToken);
    vi.stubGlobal(
      'fetch',
      vi.fn(() => Promise.resolve(new Response(JSON.stringify({ ticket: 'conch_wst_T', expires_at: '2026-10-14T10:00:00Z' }), { status: 201 }))),
    );
    vi.stubGlobal(
      'WebSocket',
      vi.fn((url: string) => {
        throw new Error(`refused ${url}`);
      }),
    );
    const result = await openSocket('ops', 0, { onOpen: () => undefined, onFrame: () => undefined, onClose: () => undefined });
    expect(result).toEqual({ ok: false, failure: 'network' });
    expect(JSON.stringify(result)).not.toContain('conch_wst_T');
  });
});

// Issue #439: where the credential is kept depends on whose it is.
describe('where the credential is kept', () => {
  it.each([
    { role: 'member', token: memberToken, lasting: true },
    { role: 'operator', token: operatorToken, lasting: false },
  ])('keeps a $role credential lasting: $lasting', async ({ token, lasting }) => {
    const calls = serve(whoami);
    const result = await signIn(token);
    expect(result.ok).toBe(true);
    if (!lasting) {
      // Nothing is stored, or sent, before the operator warning is accepted.
      expect(window.localStorage.length + window.sessionStorage.length).toBe(0);
      expect((await listChannels()).ok).toBe(false);
      expect(continueAsOperator()).toBe(true);
    }
    expect(window.localStorage.getItem(key)).toBe(lasting ? token : null);
    expect(window.sessionStorage.getItem(key)).toBe(lasting ? null : token);
    expect(hasToken()).toBe(true);
    // Calls from now on carry this credential.
    await whoAmI();
    expect(calls.at(-1)?.auth).toBe(`Bearer ${token}`);
  });

  it('restores the lasting credential in a new tab, as after closing the browser', async () => {
    window.localStorage.setItem(key, memberToken);
    const calls = serve(whoami);
    expect(await restore()).toEqual({ ok: true, value: { principal: member, warn: false } });
    expect(calls[0]?.auth).toBe(`Bearer ${memberToken}`);
  });

  it("prefers this tab's own operator credential to the lasting one", async () => {
    window.localStorage.setItem(key, memberToken);
    window.sessionStorage.setItem(key, operatorToken);
    const calls = serve(whoami);
    expect(await restore()).toEqual({ ok: true, value: { principal: operator, warn: false } });
    expect(calls.map((c) => c.auth)).toEqual([`Bearer ${operatorToken}`]);
  });

  it('drops the lasting credential on a 401', async () => {
    await signedInFrom(window.localStorage, memberToken);
    serve(() => json({}, 401));
    expect(await listChannels()).toEqual({ ok: false, failure: 'unauthorized' });
    expect(window.localStorage.getItem(key)).toBeNull();
  });

  it('does not drop a credential another tab stored after this one read its own', async () => {
    await signedInFrom(window.localStorage, memberToken);
    serve(() => {
      // Another tab signs in as someone else while the request is out.
      window.localStorage.setItem(key, otherMemberToken);
      return json({}, 401);
    });
    await listChannels();
    expect(window.localStorage.getItem(key)).toBe(otherMemberToken);
  });

  // Security review M1, scenario A: an operator tab's credential is refused;
  // the next call must not go out with the member credential that lasts in
  // the browser.
  it("does not fall through to the lasting member credential after the operator tab's 401", async () => {
    window.localStorage.setItem(key, memberToken);
    await signedInFrom(window.sessionStorage, operatorToken);
    const calls = serve((c) => (c.auth === `Bearer ${operatorToken}` ? json({}, 401) : json({ channels: [] })));
    // A parallel load, then a socket ticket and a queued post behind it.
    const results = await Promise.all([listChannels(), listChannels()]);
    expect(results.every((r) => !r.ok && r.failure === 'unauthorized')).toBe(true);
    expect(await openSocket('ops', 0, { onOpen: () => undefined, onFrame: () => undefined, onClose: () => undefined })).toEqual({ ok: false, failure: 'unauthorized' });
    expect(await postMessage('ops', 'hello')).toEqual({ ok: false, outcome: 'unauthorized' });
    expect(calls.map((c) => c.auth).filter((a) => a !== `Bearer ${operatorToken}`)).toEqual([]);
    expect(window.sessionStorage.getItem(key)).toBeNull();
    // The member's lasting credential is left where it is, for a sign-in.
    expect(window.localStorage.getItem(key)).toBe(memberToken);
  });

  // Security review M1, scenario B: another tab replaces the lasting
  // credential with another person's; this page, still showing the first
  // person, must not send it.
  it('does not send a replaced lasting credential under the old identity', async () => {
    await signedInFrom(window.localStorage, memberToken);
    window.localStorage.setItem(key, otherMemberToken);
    const calls = serve(() => json({ channels: [] }));
    expect(await listChannels()).toEqual({ ok: false, failure: 'unauthorized' });
    expect(await postMessage('ops', 'hello')).toEqual({ ok: false, outcome: 'unauthorized' });
    expect(await whoAmI()).toEqual({ ok: false, failure: 'unauthorized' });
    expect(calls).toEqual([]);
    // The other person's credential is theirs: left alone.
    expect(window.localStorage.getItem(key)).toBe(otherMemberToken);
  });

  // Security review L1: an operator credential found in localStorage is not
  // kept there; it is held back for the warning.
  it('takes an operator credential out of the lasting place and holds it for the warning', async () => {
    window.localStorage.setItem(key, operatorToken);
    const calls = serve(whoami);
    expect(await restore()).toEqual({ ok: true, value: { principal: operator, warn: true } });
    expect(window.localStorage.getItem(key)).toBeNull();
    // Not signed in until the warning is accepted: nothing is sent with it.
    expect(await listChannels()).toEqual({ ok: false, failure: 'unauthorized' });
    expect(calls).toHaveLength(1);
    expect(continueAsOperator()).toBe(true);
    expect(window.sessionStorage.getItem(key)).toBe(operatorToken);
    expect(window.localStorage.getItem(key)).toBeNull();
    await whoAmI();
    expect(calls.at(-1)?.auth).toBe(`Bearer ${operatorToken}`);
  });

  it('signs out from the warning after a restore with nothing kept', async () => {
    window.localStorage.setItem(key, operatorToken);
    const calls = serve(whoami);
    await restore();
    expect(await signOut()).toBe(true);
    expect(continueAsOperator()).toBe(false);
    expect(window.localStorage.length + window.sessionStorage.length).toBe(0);
    expect(calls).toHaveLength(1);
  });
});

describe('signOut', () => {
  it('revokes the credential in use on the server, then forgets it', async () => {
    await signedInFrom(window.localStorage, memberToken);
    const calls = serve((c) => (c.path === '/v1/credentials/current' ? new Response(null, { status: 204 }) : json({}, 404)));
    expect(await signOut()).toBe(true);
    expect(calls).toEqual([{ path: '/v1/credentials/current', method: 'DELETE', auth: `Bearer ${memberToken}` }]);
    expect(window.localStorage.getItem(key)).toBeNull();
    expect(hasToken()).toBe(false);
    // Signed out: nothing more is sent with it, even if it were stored again.
    window.localStorage.setItem(key, memberToken);
    expect(await listChannels()).toEqual({ ok: false, failure: 'unauthorized' });
    expect(calls).toHaveLength(1);
  });

  it.each([
    { name: 'no answer', answer: (): Response => { throw new TypeError('Failed to fetch'); }, told: false },
    { name: 'a refusal', answer: () => json({ code: 'last_operator', message: 'no' }, 409), told: false },
    { name: 'a server error', answer: () => json({ code: 'internal_error', message: 'no' }, 500), told: false },
    { name: 'a credential already dead', answer: () => json({}, 401), told: true },
  ])('forgets the credential here after $name, and says whether the server was told', async ({ answer, told }) => {
    await signedInFrom(window.sessionStorage, operatorToken);
    serve(answer);
    expect(await signOut()).toBe(told);
    expect(window.sessionStorage.getItem(key)).toBeNull();
    expect(hasToken()).toBe(false);
  });

  it('asks nothing of the server when not signed in', async () => {
    const calls = serve(() => json({}, 500));
    expect(await signOut()).toBe(true);
    expect(calls).toEqual([]);
  });
});

describe('signOutEverywhere', () => {
  it('revokes every credential of the person, then forgets this one', async () => {
    await signedInFrom(window.localStorage, memberToken);
    const calls = serve(() => json({ revoked: 3 }));
    expect(await signOutEverywhere(2)).toEqual({ ok: true, value: 3 });
    expect(calls).toEqual([{ path: '/v1/principals/2/credentials/revoke-all', method: 'POST', auth: `Bearer ${memberToken}` }]);
    expect(window.localStorage.getItem(key)).toBeNull();
  });

  it.each([
    { name: 'no answer', answer: (): Response => { throw new TypeError('Failed to fetch'); }, want: { ok: false, failure: 'network' } },
    { name: 'the last operator refused', answer: () => json({ code: 'last_operator', message: 'no' }, 409), want: { ok: false, failure: 'invalid', code: 'last_operator' } },
  ])('keeps the person signed in after $name', async ({ answer, want }) => {
    await signedInFrom(window.localStorage, memberToken);
    serve(answer);
    expect(await signOutEverywhere(2)).toEqual(want);
    expect(window.localStorage.getItem(key)).toBe(memberToken);
    const calls = serve(whoami);
    expect((await whoAmI()).ok).toBe(true);
    expect(calls[0]?.auth).toBe(`Bearer ${memberToken}`);
  });

  it('forgets the credential on a success whose body is unreadable', async () => {
    await signedInFrom(window.localStorage, memberToken);
    serve(() => new Response('{', { status: 200 }));
    expect(await signOutEverywhere(2)).toEqual({ ok: true, value: null });
    expect(window.localStorage.getItem(key)).toBeNull();
  });
});

describe('onSignedOutElsewhere', () => {
  const event = (init: StorageEventInit) => new StorageEvent('storage', { storageArea: window.localStorage, ...init });

  it.each([
    { name: 'the lasting credential removed', change: () => { window.localStorage.removeItem(key); }, init: { key, oldValue: memberToken, newValue: null }, signsOut: true },
    { name: 'the lasting credential replaced by another', change: () => { window.localStorage.setItem(key, otherMemberToken); }, init: { key, oldValue: memberToken, newValue: otherMemberToken }, signsOut: true },
    { name: 'the storage cleared', change: () => { window.localStorage.clear(); }, init: { key: null }, signsOut: true },
    { name: 'the same credential written again', change: () => { window.localStorage.setItem(key, memberToken); }, init: { key, oldValue: memberToken, newValue: memberToken }, signsOut: false },
    { name: 'another key', change: () => { window.localStorage.removeItem('conch.web.lastChannel'); }, init: { key: 'conch.web.lastChannel', oldValue: 'a', newValue: null }, signsOut: false },
    { name: 'the tab storage', change: () => { window.sessionStorage.removeItem(key); }, init: { key, oldValue: memberToken, newValue: null, storageArea: window.sessionStorage }, signsOut: false },
  ])('$name: signs this tab out = $signsOut', async ({ change, init, signsOut }) => {
    await signedInFrom(window.localStorage, memberToken);
    const callback = vi.fn();
    const stop = onSignedOutElsewhere(callback);
    change();
    window.dispatchEvent(event(init));
    expect(callback).toHaveBeenCalledTimes(signsOut ? 1 : 0);
    stop();
    window.dispatchEvent(event(init));
    expect(callback).toHaveBeenCalledTimes(signsOut ? 1 : 0);
  });

  it('leaves a tab on its own operator credential signed in', async () => {
    window.localStorage.setItem(key, memberToken);
    await signedInFrom(window.sessionStorage, operatorToken);
    const callback = vi.fn();
    const stop = onSignedOutElsewhere(callback);
    window.localStorage.removeItem(key);
    window.dispatchEvent(event({ key, oldValue: memberToken, newValue: null }));
    expect(callback).not.toHaveBeenCalled();
    stop();
  });
});

// Invites (issue #440): the code goes in a POST body only, and the credential
// an invite gives is kept as a member's is.
describe('invites', () => {
  const inviteCode = `conch_inv_${'Z'.repeat(43)}`;
  const joinerToken = 'conch_joiner_secret';
  const joiner = { id: 9, kind: 'human', name: 'wren', role: 'member' };
  const at = '2026-10-16T00:00:00Z';
  const redeemed = {
    principal: { id: 9, kind: 'human', name: 'wren', created_at: at },
    credential: { id: 30, principal_id: 9, label: 'invite', created_at: at },
    token: joinerToken,
  };
  const preview = { kind: 'open', inviter: 'nick', channels: ['red'], voice: false, expires_at: at };

  // sentBodies records the JSON body of each call, by path, on top of serve.
  function sentBodies(): Map<string, unknown> {
    const sent = new Map<string, unknown>();
    const inner = fetch;
    vi.stubGlobal(
      'fetch',
      vi.fn((input: RequestInfo | URL, init?: RequestInit) => {
        if (typeof input === 'string' && typeof init?.body === 'string') sent.set(input, JSON.parse(init.body));
        return inner(input, init);
      }),
    );
    return sent;
  }

  it('previews and redeems with no credential, the code in the body alone, and keeps the new credential to last', async () => {
    const calls = serve((c) =>
      c.path === '/v1/invites/preview'
        ? json(preview)
        : c.path === '/v1/invites/redeem'
          ? json(redeemed, 201)
          : c.auth === `Bearer ${joinerToken}`
            ? json(joiner)
            : json({}, 401),
    );
    const sent = sentBodies();
    expect(await previewInvite(inviteCode)).toEqual({ ok: true, value: preview });
    expect(await redeemInvite(inviteCode, 'wren')).toEqual({ ok: true, value: joiner });
    expect(sent.get('/v1/invites/preview')).toEqual({ code: inviteCode });
    expect(sent.get('/v1/invites/redeem')).toEqual({ code: inviteCode, name: 'wren' });
    expect(calls.map((c) => [c.method, c.path, c.auth])).toEqual([
      ['POST', '/v1/invites/preview', null],
      ['POST', '/v1/invites/redeem', null],
      ['GET', '/v1/whoami', `Bearer ${joinerToken}`],
    ]);
    expect(calls.some((c) => c.path.includes(inviteCode))).toBe(false);
    expect(window.localStorage.getItem(key)).toBe(joinerToken);
    expect(window.sessionStorage.length).toBe(0);
    // Pinned: calls from now on carry it.
    await whoAmI();
    expect(calls.at(-1)?.auth).toBe(`Bearer ${joinerToken}`);
  });

  it('will not redeem while someone is signed in: they are signed out first', async () => {
    await signedInFrom(window.localStorage, memberToken);
    expect(heldElsewhere()).toBe(true);
    const calls = serve(() => json(redeemed, 201));
    expect(await redeemInvite(inviteCode, 'wren')).toEqual({ ok: false, failure: 'held' });
    expect(calls).toEqual([]);
    expect(window.localStorage.getItem(key)).toBe(memberToken);
  });

  // Security review of #440, item 1: a credential that restore could not
  // check (no answer) is neither pinned nor dropped. Redeeming must not write
  // over it or beside it.
  it.each([
    // A: an operator's tab credential would win over the joiner's at the next load.
    { scenario: 'A, an operator tab credential', storage: () => window.sessionStorage, token: operatorToken },
    // B: a member's lasting credential would be overwritten, never revoked.
    { scenario: 'B, a member lasting credential', storage: () => window.localStorage, token: memberToken },
  ])('will not redeem over a sign-in that could not be checked: $scenario', async ({ storage, token }) => {
    storage().setItem(key, token);
    serve((): Response => {
      throw new TypeError('Failed to fetch');
    });
    expect(await restore()).toEqual({ ok: false, failure: 'network' });
    // The server is back, but this page never checked the stored credential.
    const calls = serve((c) => (c.path === '/v1/invites/redeem' ? json(redeemed, 201) : json(joiner)));
    expect(heldElsewhere()).toBe(true);
    expect(await redeemInvite(inviteCode, 'wren')).toEqual({ ok: false, failure: 'held' });
    expect(calls).toEqual([]);
    expect(storage().getItem(key)).toBe(token);
    expect(window.localStorage.length + window.sessionStorage.length).toBe(1);
  });

  it('will not redeem while an operator credential waits behind the warning', async () => {
    serve(whoami);
    await signIn(operatorToken);
    expect(window.localStorage.length + window.sessionStorage.length).toBe(0);
    const calls = serve(() => json(redeemed, 201));
    expect(await redeemInvite(inviteCode, 'wren')).toEqual({ ok: false, failure: 'held' });
    expect(calls).toEqual([]);
    // Signing out of the warning lets it go ahead.
    expect(await signOut()).toBe(true);
    expect(heldElsewhere()).toBe(false);
  });

  it.each([
    {
      name: 'an invalid invite',
      answer: () => json({ code: 'invite_invalid', message: 'this invite is not valid' }, 404),
      want: { ok: false, failure: 'invalid', code: 'invite_invalid', message: 'this invite is not valid' },
    },
    {
      name: 'a taken name',
      answer: () => json({ code: 'principal_exists', message: 'that name is taken; choose another' }, 409),
      want: { ok: false, failure: 'invalid', code: 'principal_exists', message: 'that name is taken; choose another' },
    },
    {
      name: 'a rate limit',
      answer: () => new Response(JSON.stringify({ code: 'rate_limited', message: 'too many' }), { status: 429, headers: { 'Retry-After': '42' } }),
      want: { ok: false, failure: 'invalid', code: 'rate_limited', message: 'too many', retryAfter: 42 },
    },
    { name: 'no answer', answer: (): Response => { throw new TypeError('Failed to fetch'); }, want: { ok: false, failure: 'network' } },
    { name: 'a body of another shape', answer: () => json({ token: joinerToken }, 201), want: { ok: false, failure: 'invalid' } },
  ])('keeps nothing after $name', async ({ answer, want }) => {
    serve(answer);
    expect(await redeemInvite(inviteCode, 'wren')).toEqual(want);
    expect(window.localStorage.length + window.sessionStorage.length).toBe(0);
    expect(hasToken()).toBe(false);
  });

  it('revokes the new credential, then says the account was made but not kept, when the browser refuses storage', async () => {
    const calls = serve((c) => (c.path === '/v1/invites/redeem' ? json(redeemed, 201) : new Response(null, { status: 204 })));
    const blocked = vi.spyOn(Storage.prototype, 'setItem').mockImplementation(() => {
      throw new DOMException('blocked', 'SecurityError');
    });
    expect(await redeemInvite(inviteCode, 'wren')).toEqual({ ok: false, failure: 'unkept' });
    blocked.mockRestore();
    expect(calls).toEqual([
      { path: '/v1/invites/redeem', method: 'POST', auth: null },
      { path: '/v1/credentials/current', method: 'DELETE', auth: `Bearer ${joinerToken}` },
    ]);
    expect(hasToken()).toBe(false);
    // Nothing is pinned: no call goes out with it.
    expect(await whoAmI()).toEqual({ ok: false, failure: 'unauthorized' });
    expect(calls).toHaveLength(2);
  });

  it('still says unkept when the revocation gets no answer', async () => {
    serve((c): Response => {
      if (c.path === '/v1/invites/redeem') return json(redeemed, 201);
      throw new TypeError('Failed to fetch');
    });
    const blocked = vi.spyOn(Storage.prototype, 'setItem').mockImplementation(() => {
      throw new DOMException('blocked', 'SecurityError');
    });
    expect(await redeemInvite(inviteCode, 'wren')).toEqual({ ok: false, failure: 'unkept' });
    blocked.mockRestore();
  });

  it('never leaves an operator credential to last: whoami saying operator drops it', async () => {
    const calls = serve((c) => (c.path === '/v1/invites/redeem' ? json(redeemed, 201) : json(operator)));
    expect(await redeemInvite(inviteCode, 'wren')).toEqual({ ok: false, failure: 'invalid' });
    expect(window.localStorage.length + window.sessionStorage.length).toBe(0);
    expect(hasToken()).toBe(false);
    expect(await listChannels()).toEqual({ ok: false, failure: 'unauthorized' });
    expect(calls.map((c) => c.path)).toEqual(['/v1/invites/redeem', '/v1/whoami']);
  });

  it('keeps the credential when whoami does not answer after redeeming', async () => {
    serve((c) => (c.path === '/v1/invites/redeem' ? json(redeemed, 201) : json({}, 503)));
    expect(await redeemInvite(inviteCode, 'wren')).toEqual({ ok: false, failure: 'unchecked' });
    expect(window.localStorage.getItem(key)).toBe(joinerToken);
  });

  it('makes, lists and closes invites with the signed-in credential', async () => {
    await signedInFrom(window.localStorage, memberToken);
    const invite = {
      id: 4,
      kind: 'personal',
      created_by: 2,
      created_at: at,
      expires_at: at,
      max_uses: 1,
      uses: 0,
      channels: [{ id: 1, name: 'red' }],
      voice: false,
      may_invite: false,
      credential_lifetime_seconds: 60,
    };
    const calls = serve((c) =>
      c.method === 'POST' ? json({ invite, code: inviteCode }, 201) : c.method === 'DELETE' ? new Response(null, { status: 204 }) : json({ invites: [invite] }),
    );
    expect(await createInvite({ kind: 'personal', channels: ['red'], voice: false })).toEqual({ ok: true, value: { invite, code: inviteCode } });
    expect(await listInvites()).toEqual({ ok: true, value: [invite] });
    expect(await closeInvite(4)).toEqual({ ok: true, value: null });
    expect(calls).toEqual([
      { path: '/v1/invites', method: 'POST', auth: `Bearer ${memberToken}` },
      { path: '/v1/invites', method: 'GET', auth: `Bearer ${memberToken}` },
      { path: '/v1/invites/4', method: 'DELETE', auth: `Bearer ${memberToken}` },
    ]);
  });

  it('claims and stops a screen share with the signed-in credential (issue #444)', async () => {
    await signedInFrom(window.localStorage, memberToken);
    const share = { principal_id: 2, started_at: at, audio: true };
    const calls = serve((c) => (c.method === 'POST' ? json(share) : new Response(null, { status: 204 })));
    expect(await claimVoiceShare('red team')).toEqual({ ok: true, value: share });
    expect(await stopVoiceShare('red team')).toEqual({ ok: true, value: null });
    expect(calls).toEqual([
      { path: '/v1/channels/red%20team/voice/share', method: 'POST', auth: `Bearer ${memberToken}` },
      { path: '/v1/channels/red%20team/voice/share', method: 'DELETE', auth: `Bearer ${memberToken}` },
    ]);
  });

  it("answers a refused share claim with its status and the server's code, and a malformed one as invalid", async () => {
    await signedInFrom(window.localStorage, memberToken);
    serve(() => json({ code: 'voice_share_busy', message: 'someone else is sharing in this room' }, 409));
    expect(await claimVoiceShare('red')).toEqual({ ok: false, failure: 'invalid', status: 409, code: 'voice_share_busy' });
    serve(() => json({ principal_id: 0, audio: 'yes' }));
    expect(await claimVoiceShare('red')).toEqual({ ok: false, failure: 'invalid' });
    serve(() => Promise.reject(new Error('offline')));
    expect(await claimVoiceShare('red')).toEqual({ ok: false, failure: 'network' });
  });

  it("answers a refused invite with the server's code and line", async () => {
    await signedInFrom(window.localStorage, memberToken);
    serve(() => json({ code: 'invite_allowance_exceeded', message: 'you already have as many open personal invites as you may' }, 403));
    expect(await createInvite({ kind: 'personal', channels: ['red'], voice: false })).toEqual({
      ok: false,
      failure: 'invalid',
      code: 'invite_allowance_exceeded',
      message: 'you already have as many open personal invites as you may',
    });
  });
});
