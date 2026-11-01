// Invites in the browser (issue #440): the link's route, the landing page and
// its answers, and "Invite someone". conchd is a stand-in fetch.
import { act, cleanup, fireEvent, render, screen, waitFor, within } from '@testing-library/react';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { App } from './App';
import { goneHeading, heldLine, noLongerWorks } from './Invite';
import { addressWithoutFragment, inviteFromHash, inviteLink, takeLinkFromAddress } from './route';
import { insecureLine } from './transport';

const origin = 'http://127.0.0.1:8080';
const inviteCode = `conch_inv_${'Q'.repeat(40)}x-_`;
const joinerToken = 'conch_joiner_secret';
const memberToken = 'conch_member_secret';
const operatorToken = 'conch_operator_secret';
const key = 'conch.web.token';
const created_at = '2026-10-14T00:00:00Z';
const expires_at = '2026-10-23T12:00:00Z';

const joiner = { id: 9, kind: 'human', name: 'wren', role: 'member' };
const member = { id: 2, kind: 'human', name: 'ada', role: 'member' };
const inviter = { ...member, may_invite: true };
const operator = { id: 1, kind: 'human', name: 'nick', role: 'operator', may_invite: true };
const preview = { kind: 'personal', inviter: 'ada', channels: ['red', 'blue'], voice: true, expires_at };
const channelList = (...names: string[]) => ({ channels: names.map((name, i) => ({ id: i + 1, name, created_at })) });
const redeemed = {
  principal: { id: 9, kind: 'human', name: 'wren', created_at },
  credential: { id: 30, principal_id: 9, label: 'invite', created_at, expires_at, renews: true },
  token: joinerToken,
};

const json = (body: unknown, status = 200, headers: Record<string, string> = {}) => new Response(JSON.stringify(body), { status, headers });
const refusal = (status: number, code: string, message = 'refused', headers: Record<string, string> = {}) => json({ code, message }, status, headers);

interface Call {
  readonly method: string;
  readonly path: string;
  readonly auth: string | null;
  readonly body: unknown;
}

type Route = (call: Call) => Response | Promise<Response>;

// serve stands in for conchd: routes by "METHOD path", every call recorded.
function serve(routes: Record<string, Route>): Call[] {
  const calls: Call[] = [];
  vi.stubGlobal(
    'fetch',
    vi.fn((input: RequestInfo | URL, init?: RequestInit) => {
      const path = typeof input === 'string' ? input : input instanceof URL ? input.pathname : input.url;
      const method = init?.method ?? 'GET';
      const body: unknown = typeof init?.body === 'string' ? JSON.parse(init.body) : undefined;
      const call = { method, path, auth: new Headers(init?.headers).get('Authorization'), body };
      calls.push(call);
      const route = routes[`${method} ${path}`];
      return Promise.resolve(route === undefined ? json({}, 404) : route(call));
    }),
  );
  return calls;
}

// as answers whoami and the channel list for the holder of token only.
function as(token: string, who: object, channels: object): Record<string, Route> {
  return {
    'GET /v1/whoami': (c) => (c.auth === `Bearer ${token}` ? json(who) : json({}, 401)),
    'GET /v1/channels': (c) => (c.auth === `Bearer ${token}` ? json(channels) : json({}, 401)),
  };
}

// A joiner's server: the preview, a redemption, and the joiner's lobby.
const joining: Record<string, Route> = {
  'POST /v1/invites/preview': () => json(preview),
  'POST /v2/invites/redeem': () => json(redeemed, 201),
  ...as(joinerToken, joiner, channelList('red', 'blue')),
};

// opened renders the page as a tab opened on the invite link.
function opened(routes: Record<string, Route>) {
  const calls = serve(routes);
  window.location.hash = `#/invite/${inviteCode}`;
  const view = render(<App origin={origin} />);
  return { calls, view };
}

// The page, the address and the stores never hold the code.
function expectNoCode(container: HTMLElement): void {
  expect(container.innerHTML).not.toContain(inviteCode);
  expect(document.documentElement.outerHTML).not.toContain(inviteCode);
  expect(window.location.href).not.toContain(inviteCode);
  for (const storage of [window.localStorage, window.sessionStorage]) {
    for (let i = 0; i < storage.length; i++) expect(storage.getItem(storage.key(i) ?? '')).not.toContain(inviteCode);
  }
}

// The password every joiner here chooses (issue #713: an invite makes an
// account with a password, through POST /v2/invites/redeem).
const password = 'correct horse battery staple';

// chooseName fills in the account form, name and password together, and
// sends it.
async function chooseName(name: string): Promise<void> {
  const field = await screen.findByLabelText('Your name');
  fireEvent.change(field, { target: { value: name } });
  fireEvent.change(screen.getByLabelText('Choose a password'), { target: { value: password } });
  fireEvent.change(screen.getByLabelText('Type the password again'), { target: { value: password } });
  fireEvent.click(screen.getByRole('button', { name: 'Create account' }));
}

beforeEach(() => {
  window.sessionStorage.clear();
  window.localStorage.clear();
  window.location.hash = '';
});
afterEach(() => {
  cleanup();
  vi.restoreAllMocks();
  vi.unstubAllGlobals();
});

describe('the invite route', () => {
  it.each([
    { hash: `#/invite/${inviteCode}`, want: inviteCode },
    { hash: '#/invite/conch_inv_a%2Db', want: 'conch_inv_a-b' },
    // Escapes that do not decode go on as they are; the server answers them.
    { hash: '#/invite/%E0%A4%A', want: '%E0%A4%A' },
    { hash: '#/invite/', want: null },
    { hash: '#/invite/a/b', want: null },
    { hash: '#/c/general', want: null },
    { hash: '', want: null },
  ])('reads $hash as $want', ({ hash, want }) => {
    expect(inviteFromHash(hash)).toBe(want);
  });

  it('builds the link its creator is shown', () => {
    expect(inviteLink(origin, inviteCode)).toBe(`${origin}/app/#/invite/${inviteCode}`);
    expect(inviteFromHash(new URL(inviteLink(origin, inviteCode)).hash)).toBe(inviteCode);
  });

  it('takes the code out of the address with one replaceState, to an address with no fragment', () => {
    const replace = vi.spyOn(window.history, 'replaceState');
    const before = window.history.length;
    window.location.hash = `#/invite/${inviteCode}`;
    expect(takeLinkFromAddress()).toEqual({ kind: 'invite', inviteCode });
    expect(replace).toHaveBeenCalledTimes(1);
    const url = String(replace.mock.calls[0]?.[2]);
    expect(url).not.toContain('#');
    expect(url).toBe(addressWithoutFragment(window.location));
    expect(window.location.href).not.toContain(inviteCode);
    expect(window.location.hash).toBe('');
    // The entry the link made was replaced, not added to.
    expect(window.history.length).toBe(before + 1);
    // Nothing left to take, and no second call.
    expect(takeLinkFromAddress()).toBeNull();
    expect(replace).toHaveBeenCalledTimes(1);
  });

  it('leaves any other address alone', () => {
    const replace = vi.spyOn(window.history, 'replaceState');
    window.location.hash = '#/c/general';
    expect(takeLinkFromAddress()).toBeNull();
    expect(replace).not.toHaveBeenCalled();
    expect(window.location.hash).toBe('#/c/general');
  });

  it('takes out a link that looks like an invite but does not parse', () => {
    window.location.hash = '#/invite/a/b';
    expect(takeLinkFromAddress()).toBeNull();
    expect(window.location.hash).toBe('');
  });

  it('builds the address from origin, path and query alone', () => {
    expect(addressWithoutFragment({ origin, pathname: '/app/', search: '?x=1' })).toBe(`${origin}/app/?x=1`);
  });
});

describe('the landing page', () => {
  it('shows the preview, then a name prompt, and joins into the lobby with the code nowhere', async () => {
    const replace = vi.spyOn(window.history, 'replaceState');
    const { calls, view } = opened(joining);
    // Out of the address before anything else.
    expect(replace).toHaveBeenCalledTimes(1);
    expect(String(replace.mock.calls[0]?.[2])).not.toContain('#');
    expectNoCode(view.container);

    const shown = await screen.findByLabelText('What the invite gives');
    expect(within(shown).getByText('ada')).toBeDefined();
    expect(within(shown).getByText('#red')).toBeDefined();
    expect(within(shown).getByText('#blue')).toBeDefined();
    expect(within(shown).getByText('Included')).toBeDefined();
    expect(within(shown).getByText(new Date(expires_at).toLocaleString()).getAttribute('datetime')).toBe(expires_at);
    expect(screen.getByText(origin)).toBeDefined();
    expect(screen.getByText(/Up to 100 characters/)).toBeDefined();
    expectNoCode(view.container);

    await chooseName('wren');
    await screen.findByRole('navigation', { name: 'Channels' });
    await screen.findByRole('button', { name: 'red' });
    expect(screen.getByRole('button', { name: 'red' }).getAttribute('aria-current')).toBe('true');
    expect(screen.getByRole('status', { name: 'Status' }).textContent).toContain('wren');

    // The code went only in the bodies of preview and redeem, never in a URL
    // or a header; the credential is kept to last, as a member's is.
    expect(calls.filter((c) => JSON.stringify(c).includes(inviteCode)).map((c) => `${c.method} ${c.path}`)).toEqual([
      'POST /v1/invites/preview',
      'POST /v2/invites/redeem',
    ]);
    expect(calls.find((c) => c.path === '/v2/invites/redeem')?.body).toEqual({ code: inviteCode, name: 'wren', password });
    expect(calls.filter((c) => c.path.includes('/invites/')).every((c) => c.auth === null)).toBe(true);
    expect(window.localStorage.getItem(key)).toBe(joinerToken);
    // Every call after the redemption carries the new credential.
    const after = calls.slice(calls.findIndex((c) => c.path === '/v2/invites/redeem') + 1).filter((c) => !c.path.startsWith('/healthz'));
    expect(after.length).toBeGreaterThan(0);
    expect(after.every((c) => c.auth === `Bearer ${joinerToken}`)).toBe(true);
    expectNoCode(view.container);
    expect(view.container.innerHTML).not.toContain(joinerToken);
    // The password went in the redemption's body and nowhere else.
    expect(calls.filter((c) => JSON.stringify(c).includes(password)).map((c) => c.path)).toEqual(['/v2/invites/redeem']);
    expect(view.container.innerHTML).not.toContain(password);
    expect(window.location.hash).toBe('#/c/red');
  });

  // Issue #556: a guest whose invite grants #general lands there, even when
  // another granted channel comes first.
  it('lands the joiner in #general when the invite grants it', async () => {
    opened({
      ...joining,
      'POST /v1/invites/preview': () => json({ ...preview, channels: ['red', 'general'] }),
      ...as(joinerToken, joiner, channelList('red', 'general')),
    });
    await chooseName('wren');
    const general = await screen.findByRole('button', { name: 'general' });
    await waitFor(() => {
      expect(general.getAttribute('aria-current')).toBe('true');
    });
    expect(screen.getByRole('button', { name: 'red' }).getAttribute('aria-current')).not.toBe('true');
    expect(window.location.hash).toBe('#/c/general');
  });

  it('keeps the form and what was typed when the name is taken', async () => {
    const { calls } = opened({ ...joining, 'POST /v2/invites/redeem': () => refusal(409, 'principal_exists', 'that name is taken; choose another') });
    await chooseName('ada');
    expect(await screen.findByText('That name is taken. Choose another.')).toBeDefined();
    const field = screen.getByLabelText<HTMLInputElement>('Your name');
    expect(field.value).toBe('ada');
    await waitFor(() => {
      expect(document.activeElement).toBe(field);
    });
    expect(window.localStorage.getItem(key)).toBeNull();
    // Another name goes through.
    serve(joining);
    await chooseName('wren');
    await screen.findByRole('navigation', { name: 'Channels' });
    expect(calls.filter((c) => c.path === '/v2/invites/redeem')).toHaveLength(1);
  });

  it.each([
    ['invisible characters', 'schema: principal name must not contain invisible formatting characters', 'A name cannot contain invisible characters.'],
    ['whitespace at an end', 'schema: principal name must not begin or end with whitespace', 'A name cannot start or end with a space.'],
    ['control characters', 'schema: principal name must not contain control characters, line or paragraph separators, or bidi controls', 'A name cannot contain control characters.'],
    ['too long', 'schema: principal name must be at most 100 characters, got 101', 'A name can be up to 100 characters.'],
    ['the guest prefix', 'schema: a principal name must not begin with the reserved guest prefix', 'Names that begin with “guest” are reserved.'],
    ['nothing visible', 'schema: principal name must have a character a reader can see', 'A name needs at least one character you can see.'],
    ['nothing visible, as the account routes say it', 'the name is not acceptable', 'A name needs at least one character you can see.'],
    ['the plural reserved-name wording', 'names beginning with guest, or with a look-alike of it, are reserved for guests; choose another', 'Names that begin with \u201cguest\u201d are reserved.'],
    ['invalid UTF-8', 'schema: principal name must be valid UTF-8', 'That name cannot be used. Choose another.'],
    ['an unknown rule', 'schema: principal name is not to the server’s liking', 'That name cannot be used. Choose another.'],
  ])('says its own plain line for a name refused for %s', async (_rule, message, line) => {
    opened({ ...joining, 'POST /v2/invites/redeem': () => refusal(400, 'invalid_request', message) });
    await chooseName('a\u200bb');
    expect(await screen.findByText(line)).toBeDefined();
    expect(document.body.textContent).not.toMatch(/principal|schema:/i);
    const field = screen.getByLabelText<HTMLInputElement>('Your name');
    expect(field.value).toBe('a\u200bb');
    await waitFor(() => {
      expect(document.activeElement).toBe(field);
    });
  });

  it.each<{ readonly when: string; readonly routes: Record<string, Route> }>([
    { when: 'previewed', routes: { 'POST /v1/invites/preview': () => refusal(404, 'invite_invalid', 'this invite is not valid') } },
    { when: 'redeemed', routes: { 'POST /v2/invites/redeem': () => refusal(404, 'invite_invalid', 'this invite is not valid') } },
  ])('says the one "can\'t be used" when the invite is invalid, $when', async ({ routes }) => {
    const { view } = opened({ ...joining, ...routes });
    if (routes['POST /v2/invites/redeem'] !== undefined) await chooseName('wren');
    expect(await screen.findByText(noLongerWorks)).toBeDefined();
    expect(screen.getByRole('heading', { level: 1 }).textContent).toBe("This link can't be used");
    expect(screen.queryByLabelText('Your name')).toBeNull();
    expect(screen.queryByRole('button', { name: 'Try again' })).toBeNull();
    // Nobody is signed in here: the one button is Sign in, for someone who
    // joined from this link already (issue #713). Nothing else.
    expect(screen.getAllByRole('button').map((b) => b.textContent)).toEqual(['Sign in']);
    expectNoCode(view.container);
  });

  // Issue #593 (F19): the server gives one answer for every cause, so the
  // page does too. Each is a code the server does not take.
  it.each([
    { cause: 'used', hash: `#/invite/${inviteCode}` },
    { cause: 'closed', hash: `#/invite/${inviteCode}` },
    { cause: 'expired', hash: `#/invite/${inviteCode}` },
    { cause: 'malformed', hash: '#/invite/%E0%A4%A' },
  ])('shows the same heading and alert for a $cause invite, sharing no sentence', async ({ hash }) => {
    serve({ ...joining, 'POST /v1/invites/preview': () => refusal(404, 'invite_invalid', 'this invite is not valid') });
    window.location.hash = hash;
    const view = render(<App origin={origin} />);
    const alert = await screen.findByRole('alert');
    expect(alert.textContent).toBe('Ask the person who sent it for a new one. If you already joined from this link, sign in instead.');
    const heading = screen.getByRole('heading', { level: 1 });
    expect(heading.textContent).toBe(goneHeading);
    expect(alert.textContent).not.toContain(heading.textContent);
    expect(screen.getAllByRole('button').map((b) => b.textContent)).toEqual(['Sign in']);
    expect(screen.queryByLabelText('Credential')).toBeNull();
    expectNoCode(view.container);
  });

  it('offers someone signed in here "Back to Conch", to the lobby, for an invite that cannot be used', async () => {
    window.localStorage.setItem(key, memberToken);
    const { calls, view } = opened({
      ...as(memberToken, member, channelList('general')),
      'POST /v1/invites/preview': () => refusal(404, 'invite_invalid', 'this invite is not valid'),
    });
    expect(await screen.findByText(noLongerWorks)).toBeDefined();
    expect(screen.queryByRole('button', { name: 'Go back' })).toBeNull();
    expectNoCode(view.container);
    fireEvent.click(screen.getByRole('button', { name: 'Back to Conch' }));
    await screen.findByRole('navigation', { name: 'Channels' });
    expect(window.localStorage.getItem(key)).toBe(memberToken);
    expect(calls.some((c) => c.path === '/v1/credentials/current')).toBe(false);
  });

  it.each([
    { name: 'paused', answer: () => refusal(503, 'invites_paused', 'invites are paused on this server'), line: 'Invites are paused on this server for now. Try the link again later.' },
    { name: 'rate-limited', answer: () => refusal(429, 'rate_limited', 'slow down', { 'Retry-After': '120' }), line: 'Too many tries from your network. Try again in 2 minutes.' },
    { name: 'rate-limited, briefly', answer: () => refusal(429, 'rate_limited', 'slow down', { 'Retry-After': '30' }), line: 'Too many tries from your network. Try again in 30 seconds.' },
    { name: 'unreachable', answer: (): Response => { throw new TypeError('Failed to fetch'); }, line: 'Cannot reach the server. Check your connection and try again.' },
  ])('says so when the preview is $name, and can try again', async ({ answer, line }) => {
    const { calls } = opened({ ...joining, 'POST /v1/invites/preview': answer });
    expect(await screen.findByText(line)).toBeDefined();
    serve(joining);
    fireEvent.click(screen.getByRole('button', { name: 'Try again' }));
    await screen.findByLabelText('Your name');
    expect(calls.length).toBeGreaterThan(0);
  });

  it.each([
    { name: 'paused', answer: () => refusal(503, 'invites_paused'), line: 'Invites are paused on this server for now. Try the link again later.' },
    { name: 'rate-limited', answer: () => refusal(429, 'rate_limited', 'slow down', { 'Retry-After': '61' }), line: 'Too many tries from your network. Try again in 2 minutes.' },
  ])('says so when redeeming is $name, and keeps the form', async ({ answer, line }) => {
    opened({ ...joining, 'POST /v2/invites/redeem': answer });
    await chooseName('wren');
    expect(await screen.findByText(line)).toBeDefined();
    expect(screen.getByLabelText<HTMLInputElement>('Your name').value).toBe('wren');
  });

  it('says who is signed in already, and continues as a new person only after signing them out', async () => {
    window.localStorage.setItem(key, memberToken);
    const { calls, view } = opened({
      ...joining,
      'GET /v1/whoami': (c) => (c.auth === `Bearer ${memberToken}` ? json(member) : c.auth === `Bearer ${joinerToken}` ? json(joiner) : json({}, 401)),
      'GET /v1/channels': (c) => (c.auth === `Bearer ${memberToken}` ? json(channelList('general')) : c.auth === `Bearer ${joinerToken}` ? json(channelList('red', 'blue')) : json({}, 401)),
      'DELETE /v1/credentials/current': () => new Response(null, { status: 204 }),
    });
    expect(await screen.findByText(/You are signed in here as/)).toBeDefined();
    expect(screen.getByText(/You are signed in here as/).textContent).toContain('ada');
    expect(screen.queryByLabelText('Your name')).toBeNull();
    fireEvent.click(screen.getByRole('button', { name: 'Continue as a new person' }));
    await chooseName('wren');
    await screen.findByRole('button', { name: 'red' });
    // The old credential was revoked on the server before the invite was used.
    const order = calls.map((c) => `${c.method} ${c.path}`);
    expect(order.indexOf('DELETE /v1/credentials/current')).toBeGreaterThan(-1);
    expect(order.indexOf('DELETE /v1/credentials/current')).toBeLessThan(order.indexOf('POST /v2/invites/redeem'));
    expect(calls.find((c) => c.path === '/v1/credentials/current')?.auth).toBe(`Bearer ${memberToken}`);
    expect(window.localStorage.getItem(key)).toBe(joinerToken);
    expectNoCode(view.container);
  });

  // Security review of #440, item 1: the server did not answer when the page
  // checked the stored sign-in, so it is neither shown as signed in nor
  // dropped. The page does not offer to join over it.
  it.each([
    { scenario: 'A, an operator tab credential', storage: () => window.sessionStorage, token: operatorToken },
    { scenario: 'B, a member lasting credential', storage: () => window.localStorage, token: memberToken },
  ])('offers no join over a sign-in that could not be checked: $scenario', async ({ storage, token }) => {
    storage().setItem(key, token);
    let up = false;
    const { calls, view } = opened({
      ...joining,
      'GET /v1/whoami': (): Response => {
        if (!up) throw new TypeError('Failed to fetch');
        return json(joiner);
      },
      'POST /v1/invites/preview': (): Response => {
        if (!up) throw new TypeError('Failed to fetch');
        return json(preview);
      },
    });
    await screen.findByText('Cannot reach the server. Check your connection and try again.');
    // The server comes back; the preview is tried again.
    up = true;
    fireEvent.click(screen.getByRole('button', { name: 'Try again' }));
    expect(await screen.findByText(heldLine)).toBeDefined();
    expect(screen.queryByLabelText('Your name')).toBeNull();
    expect(screen.queryByRole('button', { name: 'Create account' })).toBeNull();
    expect(calls.some((c) => c.path === '/v2/invites/redeem')).toBe(false);
    expect(storage().getItem(key)).toBe(token);
    expectNoCode(view.container);
  });

  it('says another sign-in is held when redeeming finds one the page had not', async () => {
    opened(joining);
    await screen.findByLabelText('Your name');
    // Another tab of this browser signs in while the form is open.
    window.localStorage.setItem(key, memberToken);
    await chooseName('wren');
    expect(await screen.findByText(heldLine)).toBeDefined();
    expect(window.localStorage.getItem(key)).toBe(memberToken);
  });

  it('goes back to the lobby, still signed in, when the person who is signed in says so', async () => {
    window.localStorage.setItem(key, memberToken);
    const { calls } = opened({ ...joining, ...as(memberToken, member, channelList('general')) });
    await screen.findByText(/You are signed in here as/);
    fireEvent.click(screen.getByRole('button', { name: 'Go back' }));
    await screen.findByRole('navigation', { name: 'Channels' });
    expect(window.localStorage.getItem(key)).toBe(memberToken);
    expect(calls.some((c) => c.path === '/v2/invites/redeem' || c.path === '/v1/credentials/current')).toBe(false);
  });

  it('answers a link pasted into a tab that is already open', async () => {
    const replace = vi.spyOn(window.history, 'replaceState');
    serve(joining);
    const view = render(<App origin={origin} />);
    await screen.findByLabelText('Credential');
    act(() => {
      window.location.hash = `#/invite/${inviteCode}`;
      window.dispatchEvent(new HashChangeEvent('hashchange'));
    });
    await screen.findByLabelText('What the invite gives');
    expect(replace).toHaveBeenCalled();
    expectNoCode(view.container);
  });
});

// The invite panel: an inviter's own invites, as the server lists them.
const inviteOf = (id: number, createdBy: number, extra: object = {}) => ({
  id,
  kind: 'personal',
  created_by: createdBy,
  created_at,
  expires_at: '2099-01-01T00:00:00Z',
  max_uses: 1,
  uses: 0,
  channels: [{ id: 1, name: 'general' }],
  voice: false,
  may_invite: false,
  credential_lifetime_seconds: 7776000,
  ...extra,
});

const newCode = `conch_inv_${'N'.repeat(43)}`;

async function openPanel(): Promise<HTMLElement> {
  const panel = await screen.findByRole('region', { name: 'Invites' });
  fireEvent.click(within(panel).getByRole('button', { name: 'Invite someone' }));
  return panel;
}

describe('invite someone', () => {
  it('is not offered to a person who may not invite', async () => {
    serve(as(memberToken, member, channelList('general')));
    window.localStorage.setItem(key, memberToken);
    render(<App origin={origin} />);
    await screen.findByRole('button', { name: 'general' });
    expect(screen.queryByRole('button', { name: 'Invite someone' })).toBeNull();
    expect(screen.queryByRole('region', { name: 'Invites' })).toBeNull();
    // Nor does any channel row carry a ⋯ (issue #1135).
    expect(screen.queryByRole('button', { name: /^Actions for/ })).toBeNull();
  });

  // Issue #1135: a channel's ⋯ menu opens this panel with that channel ticked.
  it('opens from a channel\'s ⋯ menu with that channel ticked and the others not, and makes the invite to it', async () => {
    const calls = serve({
      ...as(memberToken, inviter, channelList('general', 'ops')),
      'GET /v1/invites': () => json({ invites: [] }),
      'POST /v1/invites': () => json({ invite: inviteOf(7, 2), code: newCode }, 201),
    });
    window.localStorage.setItem(key, memberToken);
    render(<App origin={origin} />);
    const nav = within(await screen.findByRole('navigation', { name: 'Channels' }));
    await nav.findByRole('button', { name: 'ops' });
    const panel = screen.getByRole('region', { name: 'Invites' });
    expect(within(panel).getByRole('button', { name: 'Invite someone' }).getAttribute('aria-expanded')).toBe('false');

    fireEvent.click(nav.getByRole('button', { name: 'Actions for ops' }));
    expect(within(screen.getByRole('menu')).getAllByRole('menuitem').map((i) => i.textContent)).toEqual(['Invite people']);
    fireEvent.click(screen.getByRole('menuitem', { name: 'Invite people' }));

    expect(within(panel).getByRole('button', { name: 'Invite someone' }).getAttribute('aria-expanded')).toBe('true');
    expect(within(panel).getByRole<HTMLInputElement>('checkbox', { name: '#ops' }).checked).toBe(true);
    expect(within(panel).getByRole<HTMLInputElement>('checkbox', { name: '#general' }).checked).toBe(false);
    await waitFor(() => {
      expect(panel.contains(document.activeElement)).toBe(true);
    });

    fireEvent.click(within(panel).getByRole('button', { name: 'Make invite' }));
    await waitFor(() => {
      expect(calls.filter((c) => c.method === 'POST' && c.path === '/v1/invites').map((c) => (c.body as { channels: string[] }).channels)).toEqual([['ops']]);
    });
  });

  it('asked for another channel after an invite was made, drops the old link and its copy state', async () => {
    serve({
      ...as(memberToken, inviter, channelList('private', 'public')),
      'GET /v1/invites': () => json({ invites: [] }),
      'POST /v1/invites': () => json({ invite: inviteOf(7, 2), code: newCode }, 201),
    });
    window.localStorage.setItem(key, memberToken);
    render(<App origin={origin} />);
    const nav = within(await screen.findByRole('navigation', { name: 'Channels' }));
    await nav.findByRole('button', { name: 'public' });
    const panel = screen.getByRole('region', { name: 'Invites' });
    fireEvent.click(nav.getByRole('button', { name: 'Actions for private' }));
    fireEvent.click(screen.getByRole('menuitem', { name: 'Invite people' }));
    fireEvent.click(within(panel).getByRole('button', { name: 'Make invite' }));
    await within(panel).findByRole('textbox', { name: 'Invite link' });

    fireEvent.click(nav.getByRole('button', { name: 'Actions for public' }));
    fireEvent.click(screen.getByRole('menuitem', { name: 'Invite people' }));
    expect(within(panel).queryByRole('textbox', { name: 'Invite link' })).toBeNull();
    expect(within(panel).queryByRole('group', { name: 'New invite link' })).toBeNull();
    expect(within(panel).getByRole<HTMLInputElement>('checkbox', { name: '#public' }).checked).toBe(true);
    expect(within(panel).getByRole<HTMLInputElement>('checkbox', { name: '#private' }).checked).toBe(false);
    await act(async () => {});
  });

  it('asked again for another channel, re-ticks only that one', async () => {
    serve({ ...as(memberToken, inviter, channelList('general', 'ops')), 'GET /v1/invites': () => json({ invites: [] }) });
    window.localStorage.setItem(key, memberToken);
    render(<App origin={origin} />);
    const nav = within(await screen.findByRole('navigation', { name: 'Channels' }));
    await nav.findByRole('button', { name: 'ops' });
    const panel = screen.getByRole('region', { name: 'Invites' });
    for (const [chosen, other] of [['ops', 'general'], ['general', 'ops']] as const) {
      fireEvent.click(nav.getByRole('button', { name: `Actions for ${chosen}` }));
      fireEvent.click(screen.getByRole('menuitem', { name: 'Invite people' }));
      expect(within(panel).getByRole<HTMLInputElement>('checkbox', { name: `#${chosen}` }).checked).toBe(true);
      expect(within(panel).getByRole<HTMLInputElement>('checkbox', { name: `#${other}` }).checked).toBe(false);
    }
    await act(async () => {});
  });

  // Issue #536: the form opens over the page, so Escape closes it.
  it('closes on Escape, and focus goes back to Invite someone', async () => {
    serve({ ...as(memberToken, inviter, channelList('general')), 'GET /v1/invites': () => json({ invites: [] }) });
    window.localStorage.setItem(key, memberToken);
    render(<App origin={origin} />);
    const panel = await openPanel();
    const button = within(panel).getByRole('button', { name: 'Invite someone' });
    expect(button.getAttribute('aria-expanded')).toBe('true');
    fireEvent.keyDown(within(panel).getByRole('checkbox', { name: '#general' }), { key: 'Escape' });
    expect(button.getAttribute('aria-expanded')).toBe('false');
    expect(within(panel).queryByRole('button', { name: 'Make invite' })).toBeNull();
    expect(document.activeElement).toBe(button);
    await act(async () => {});
  });

  it('lets a member make a personal invite to their own channels, copy the link once, see it listed and close it', async () => {
    let listed = [inviteOf(5, 2, { uses: 1 })];
    const calls = serve({
      ...as(memberToken, inviter, channelList('general', 'ops')),
      'POST /v1/invites': () => {
        listed = [inviteOf(7, 2), ...listed];
        return json({ invite: inviteOf(7, 2), code: newCode }, 201);
      },
      'GET /v1/invites': () => json({ invites: listed }),
      'DELETE /v1/invites/7': () => {
        listed = listed.map((i) => (i.id === 7 ? { ...i, closed_at: '2026-10-16T00:00:00Z' } : i));
        return new Response(null, { status: 204 });
      },
    });
    const writeText = vi.fn(() => Promise.resolve());
    Object.defineProperty(window.navigator, 'clipboard', { configurable: true, value: { writeText } });
    window.localStorage.setItem(key, memberToken);
    render(<App origin={origin} />);
    const panel = await openPanel();

    // A member gets personal invites only: no kind, no cap, no right to invite.
    expect(within(panel).queryByRole('radio')).toBeNull();
    expect(within(panel).queryByText(/Joiners may invite/)).toBeNull();
    expect(within(panel).queryByText(/Limit the number/)).toBeNull();
    expect(within(panel).getByRole<HTMLSelectElement>('combobox').value).toBe('7 days');
    const make = within(panel).getByRole('button', { name: 'Make invite' });
    expect(make.hasAttribute('disabled')).toBe(true);
    await within(panel).findByText('Used');

    fireEvent.click(await within(panel).findByRole('checkbox', { name: '#general' }));
    fireEvent.click(make);
    const link = await within(panel).findByRole<HTMLInputElement>('textbox', { name: 'Invite link' });
    expect(link.value).toBe(`${origin}/app/#/invite/${newCode}`);
    expect(within(panel).getByText(/The first person to open this link joins/)).toBeDefined();
    expect(within(panel).queryByText(/Anyone with this link/)).toBeNull();
    // The default expiry is the server's: none is sent.
    expect(calls.find((c) => c.method === 'POST' && c.path === '/v1/invites')?.body).toEqual({ kind: 'personal', channels: ['general'], voice: false });

    fireEvent.click(within(panel).getByRole('button', { name: 'Copy link' }));
    await within(panel).findByText('Copied.');
    expect(writeText).toHaveBeenCalledWith(`${origin}/app/#/invite/${newCode}`);

    // Listed, open, with a close button; closing it shows it closed.
    const list = await within(panel).findByRole('list', { name: 'Your invites' });
    await waitFor(() => {
      expect(within(list).getAllByRole('listitem')).toHaveLength(2);
    });
    const fresh = within(list).getAllByRole('listitem')[0];
    if (fresh === undefined) throw new Error('no invite listed');
    expect(fresh.textContent).toContain('Open');
    fireEvent.click(within(fresh).getByRole('button', { name: 'Close' }));
    await waitFor(() => {
      expect(within(list).getAllByRole('listitem')[0]?.textContent).toContain('Closed');
    });
    expect(calls.some((c) => c.method === 'DELETE' && c.path === '/v1/invites/7')).toBe(true);

    // Done: the link is gone, and is not shown again.
    fireEvent.click(within(panel).getByRole('button', { name: 'Done' }));
    expect(within(panel).queryByRole('textbox', { name: 'Invite link' })).toBeNull();
    expect(document.body.innerHTML).not.toContain(newCode);
    Reflect.deleteProperty(window.navigator, 'clipboard');
  });

  it.each([
    { code: 'invite_allowance_exceeded', line: 'You already have as many unused personal invites as you may. Close one, or wait until one is used or expires.' },
    { code: 'invite_grant_exceeds_inviter', line: 'An invite can give only what you have: channels you are in, and voice only if you have it.' },
    { code: 'invites_paused', line: 'Invites are paused on this server for now.', status: 503 },
    { code: 'rate_limited', line: 'You have made too many invites for now. Try again in 15 minutes.', status: 429 },
  ])('says $code in words', async ({ code, line, status }) => {
    serve({
      ...as(memberToken, inviter, channelList('general')),
      'POST /v1/invites': () => refusal(status ?? 403, code, 'no', { 'Retry-After': '900' }),
      'GET /v1/invites': () => json({ invites: [] }),
    });
    window.localStorage.setItem(key, memberToken);
    render(<App origin={origin} />);
    const panel = await openPanel();
    fireEvent.click(within(panel).getByRole('checkbox', { name: 'Voice' }));
    fireEvent.click(await within(panel).findByRole('checkbox', { name: '#general' }));
    fireEvent.click(within(panel).getByRole('button', { name: 'Make invite' }));
    expect(await within(panel).findByText(line)).toBeDefined();
    expect(within(panel).queryByRole('textbox', { name: 'Invite link' })).toBeNull();
  });

  // Issue #909: an operator is told where the resume switch is, and the button
  // opens Settings with Joining expanded; a member gets the plain line.
  it('tells an operator whose invite is refused as paused where to resume, and the button opens the switch', async () => {
    serve({
      ...as(operatorToken, operator, channelList('general')),
      'POST /v1/invites': () => refusal(503, 'invites_paused'),
      'GET /v1/invites': () => json({ invites: [] }),
      'GET /v1/invites/settings': () => json({ paused: true }),
    });
    window.sessionStorage.setItem(key, operatorToken);
    render(<App origin={origin} />);
    const panel = await openPanel();
    fireEvent.click(await within(panel).findByRole('checkbox', { name: '#general' }));
    fireEvent.click(within(panel).getByRole('button', { name: 'Make invite' }));
    expect(await within(panel).findByText('Invites are paused on this server. Resume them in Settings → Joining.')).toBeDefined();
    expect(screen.queryByRole('region', { name: 'Joining' })?.hasAttribute('hidden') ?? true).toBe(true);

    fireEvent.click(within(panel).getByRole('button', { name: 'Open Settings → Joining' }));
    const region = screen.getByRole('region', { name: 'Joining' });
    expect(region.hasAttribute('hidden')).toBe(false);
    const toggle = screen.getByRole('button', { name: 'Joining' });
    expect(toggle.getAttribute('aria-expanded')).toBe('true');
    // The section has focus, not the Settings heading, so it is scrolled to.
    expect(document.activeElement).toBe(toggle);
    const box = await within(region).findByLabelText<HTMLInputElement>('Pause all invites');
    expect(box.checked).toBe(true);
  });

  // Joining stays expanded when Settings closes; a jump must still
  // show what the server holds now, not what it held when the section opened.
  it('reads the switch afresh when an operator is sent to a section left open', async () => {
    let paused = false;
    serve({
      ...as(operatorToken, operator, channelList('general')),
      'POST /v1/invites': () => refusal(503, 'invites_paused'),
      'GET /v1/invites': () => json({ invites: [] }),
      'GET /v1/invites/settings': () => json({ paused }),
    });
    window.sessionStorage.setItem(key, operatorToken);
    render(<App origin={origin} />);
    fireEvent.click(await screen.findByRole('button', { name: 'Settings' }));
    fireEvent.click(await screen.findByRole('button', { name: 'Joining' }));
    const region = screen.getByRole('region', { name: 'Joining' });
    expect((await within(region).findByLabelText<HTMLInputElement>('Pause all invites')).checked).toBe(false);
    fireEvent.click(within(screen.getByRole('dialog', { name: 'Settings' })).getByRole('button', { name: 'Close' }));

    paused = true; // another operator paused them meanwhile
    const panel = await openPanel();
    fireEvent.click(await within(panel).findByRole('checkbox', { name: '#general' }));
    fireEvent.click(within(panel).getByRole('button', { name: 'Make invite' }));
    fireEvent.click(await within(panel).findByRole('button', { name: 'Open Settings → Joining' }));
    await waitFor(() => {
      expect(within(region).getByLabelText<HTMLInputElement>('Pause all invites').checked).toBe(true);
    });
  });

  it('gives a member whose invite is refused as paused only the plain line, and no button', async () => {
    serve({
      ...as(memberToken, inviter, channelList('general')),
      'POST /v1/invites': () => refusal(503, 'invites_paused'),
      'GET /v1/invites': () => json({ invites: [] }),
    });
    window.localStorage.setItem(key, memberToken);
    render(<App origin={origin} />);
    const panel = await openPanel();
    fireEvent.click(await within(panel).findByRole('checkbox', { name: '#general' }));
    fireEvent.click(within(panel).getByRole('button', { name: 'Make invite' }));
    expect(await within(panel).findByText('Invites are paused on this server for now.')).toBeDefined();
    expect(within(panel).queryByText(/Settings → Joining/)).toBeNull();
    expect(screen.queryByRole('button', { name: /^(Open Settings → )?Joining$/ })).toBeNull();
  });

  it("gives an operator an open link with its defaults, and lists only the operator's own invites", async () => {
    const calls = serve({
      ...as(operatorToken, operator, channelList('general')),
      'POST /v1/invites': () => json({ invite: inviteOf(8, 1, { kind: 'open' }), code: newCode }, 201),
      'GET /v1/invites': () => json({ invites: [inviteOf(3, 1), inviteOf(4, 2)] }),
    });
    window.sessionStorage.setItem(key, operatorToken);
    render(<App origin={origin} />);
    const panel = await openPanel();
    const list = await within(panel).findByRole('list', { name: 'Your invites' });
    expect(within(list).getAllByRole('listitem')).toHaveLength(1);

    fireEvent.click(within(panel).getByRole('radio', { name: 'Open link: anyone with it' }));
    expect(within(panel).getByRole<HTMLSelectElement>('combobox').value).toBe('24 hours');
    const capped = within(panel).getByRole<HTMLInputElement>('checkbox', { name: 'Limit the number of people' });
    const mayInvite = within(panel).getByRole<HTMLInputElement>('checkbox', { name: 'Joiners may invite others' });
    expect(capped.checked).toBe(false);
    expect(mayInvite.checked).toBe(false);
    fireEvent.click(await within(panel).findByRole('checkbox', { name: '#general' }));
    fireEvent.click(within(panel).getByRole('button', { name: 'Make invite' }));
    await within(panel).findByRole('textbox', { name: 'Invite link' });
    expect(calls.find((c) => c.method === 'POST' && c.path === '/v1/invites')?.body).toEqual({ kind: 'open', channels: ['general'], voice: false });
  });

  it('sends what an operator chose: an expiry, a use cap and the right to invite', async () => {
    const calls = serve({
      ...as(operatorToken, operator, channelList('general', 'ops')),
      'POST /v1/invites': () => json({ invite: inviteOf(8, 1, { kind: 'open' }), code: newCode }, 201),
      'GET /v1/invites': () => json({ invites: [] }),
    });
    window.sessionStorage.setItem(key, operatorToken);
    render(<App origin={origin} />);
    const panel = await openPanel();
    fireEvent.click(within(panel).getByRole('radio', { name: 'Open link: anyone with it' }));
    fireEvent.change(within(panel).getByRole('combobox'), { target: { value: '1 hour' } });
    fireEvent.click(within(panel).getByRole('checkbox', { name: 'Limit the number of people' }));
    const make = within(panel).getByRole('button', { name: 'Make invite' });
    fireEvent.change(within(panel).getByRole('spinbutton'), { target: { value: '0' } });
    fireEvent.click(await within(panel).findByRole('checkbox', { name: '#ops' }));
    // A cap that is not a number of people is not sent as no cap.
    expect(make.hasAttribute('disabled')).toBe(true);
    fireEvent.change(within(panel).getByRole('spinbutton'), { target: { value: '12' } });
    fireEvent.click(within(panel).getByRole('checkbox', { name: 'Joiners may invite others' }));
    const before = Date.now();
    fireEvent.click(make);
    await within(panel).findByRole('textbox', { name: 'Invite link' });
    const body = calls.find((c) => c.method === 'POST' && c.path === '/v1/invites')?.body as { expires_at?: string };
    expect(body).toEqual({ kind: 'open', channels: ['ops'], voice: false, expires_at: body.expires_at, max_uses: 12, may_invite: true });
    const expires = Date.parse(body.expires_at ?? '');
    expect(expires - before).toBeGreaterThanOrEqual(60 * 60 * 1000 - 1000);
    expect(expires - before).toBeLessThanOrEqual(60 * 60 * 1000 + 5000);
  });

  // Issue #468: what whoami says of voice and of the allowance.
  it('does not offer voice to a person who does not hold it', async () => {
    const calls = serve({
      ...as(memberToken, { ...inviter, voice_allowed: false, invites_remaining: 5 }, channelList('general')),
      'POST /v1/invites': () => json({ invite: inviteOf(7, 2), code: newCode }, 201),
      'GET /v1/invites': () => json({ invites: [] }),
    });
    window.localStorage.setItem(key, memberToken);
    render(<App origin={origin} />);
    const panel = await openPanel();
    await within(panel).findByText('5 invites left');
    expect(within(panel).queryByRole('checkbox', { name: /voice/i })).toBeNull();
    fireEvent.click(await within(panel).findByRole('checkbox', { name: '#general' }));
    fireEvent.click(within(panel).getByRole('button', { name: 'Make invite' }));
    await within(panel).findByRole('textbox', { name: 'Invite link' });
    expect(calls.find((c) => c.method === 'POST' && c.path === '/v1/invites')?.body).toEqual({ kind: 'personal', channels: ['general'], voice: false });
  });

  it('offers voice to a person who holds it, and counts the invites left down as they make one and up as they close it', async () => {
    let remaining = 2;
    let listed: ReturnType<typeof inviteOf>[] = [];
    const calls = serve({
      'GET /v1/whoami': (c) =>
        c.auth === `Bearer ${memberToken}` ? json({ ...inviter, voice_allowed: true, invites_remaining: remaining }) : json({}, 401),
      'GET /v1/channels': () => json(channelList('general')),
      'POST /v1/invites': () => {
        remaining -= 1;
        listed = [inviteOf(7, 2, { voice: true })];
        return json({ invite: inviteOf(7, 2, { voice: true }), code: newCode }, 201);
      },
      'GET /v1/invites': () => json({ invites: listed }),
      'DELETE /v1/invites/7': () => {
        remaining += 1;
        listed = listed.map((i) => ({ ...i, closed_at: '2026-10-16T00:00:00Z' }));
        return new Response(null, { status: 204 });
      },
    });
    window.localStorage.setItem(key, memberToken);
    render(<App origin={origin} />);
    const panel = await openPanel();
    await within(panel).findByText('2 invites left');

    fireEvent.click(within(panel).getByRole('checkbox', { name: 'Voice' }));
    fireEvent.click(await within(panel).findByRole('checkbox', { name: '#general' }));
    fireEvent.click(within(panel).getByRole('button', { name: 'Make invite' }));
    await within(panel).findByText('1 invite left');
    expect(calls.find((c) => c.method === 'POST' && c.path === '/v1/invites')?.body).toEqual({ kind: 'personal', channels: ['general'], voice: true });

    const list = await within(panel).findByRole('list', { name: 'Your invites' });
    fireEvent.click(await within(list).findByRole('button', { name: 'Close' }));
    await within(panel).findByText('2 invites left');
  });

  // Issue #593 (F18): the new link's words follow its kind.
  it.each([
    { kind: 'personal', says: /The first person to open this link joins/, not: /Anyone with this link/ },
    { kind: 'open', says: /Anyone with this link can join/, not: /The first person to open this link/ },
  ])('says who a new $kind link lets in', async ({ kind, says, not }) => {
    serve({
      ...as(operatorToken, operator, channelList('general')),
      'POST /v1/invites': () => json({ invite: inviteOf(8, 1, { kind }), code: newCode }, 201),
      'GET /v1/invites': () => json({ invites: [] }),
    });
    window.sessionStorage.setItem(key, operatorToken);
    render(<App origin={origin} />);
    const panel = await openPanel();
    if (kind === 'open') fireEvent.click(within(panel).getByRole('radio', { name: 'Open link: anyone with it' }));
    fireEvent.click(await within(panel).findByRole('checkbox', { name: '#general' }));
    fireEvent.click(within(panel).getByRole('button', { name: 'Make invite' }));
    const box = await within(panel).findByRole('group', { name: 'New invite link' });
    expect(within(box).getByText(says)).toBeDefined();
    expect(box.textContent).not.toMatch(not);
    expect(box.textContent).toContain('It is shown only now: copy it and send it to the person you are inviting.');
  });

  it('puts focus on Make invite after Done, reads the list again, and Escape still closes the panel', async () => {
    const calls = serve({
      ...as(memberToken, inviter, channelList('general')),
      'POST /v1/invites': () => json({ invite: inviteOf(7, 2), code: newCode }, 201),
      'GET /v1/invites': () => json({ invites: [inviteOf(7, 2)] }),
    });
    const reads = () => calls.filter((c) => c.path === '/v1/invites' && c.method === 'GET').length;
    window.localStorage.setItem(key, memberToken);
    render(<App origin={origin} />);
    const panel = await openPanel();
    fireEvent.click(await within(panel).findByRole('checkbox', { name: '#general' }));
    fireEvent.click(within(panel).getByRole('button', { name: 'Make invite' }));
    const done = await within(panel).findByRole('button', { name: 'Done' });
    await within(panel).findByRole('list', { name: 'Your invites' });
    await act(async () => {});
    const before = reads();
    done.focus();
    fireEvent.click(done);
    const make = within(panel).getByRole('button', { name: 'Make invite' });
    expect(document.activeElement).toBe(make);
    await waitFor(() => {
      expect(reads()).toBe(before + 1);
    });
    fireEvent.keyDown(make, { key: 'Escape' });
    const toggle = within(panel).getByRole('button', { name: 'Invite someone' });
    expect(toggle.getAttribute('aria-expanded')).toBe('false');
    expect(document.activeElement).toBe(toggle);
    await act(async () => {});
  });

  it('says when the list was read, and reads it again when the page is shown again while open', async () => {
    let listed = [inviteOf(7, 2)];
    const calls = serve({
      ...as(memberToken, inviter, channelList('general')),
      'GET /v1/invites': () => json({ invites: listed }),
    });
    const reads = () => calls.filter((c) => c.path === '/v1/invites' && c.method === 'GET').length;
    window.localStorage.setItem(key, memberToken);
    render(<App origin={origin} />);
    const panel = await openPanel();
    const list = await within(panel).findByRole('list', { name: 'Your invites' });
    expect(await within(list).findByRole('button', { name: 'Close' })).toBeDefined();
    const readAt = await within(panel).findByText((_, el) => el?.tagName === 'SPAN' && /^Read at \d\d:\d\d$/.test(el.textContent));
    expect(readAt.querySelector('time')?.getAttribute('datetime')).toMatch(/^\d{4}-\d\d-\d\dT/);
    await act(async () => {});

    // Someone redeems it elsewhere; the tab is hidden, then shown again.
    listed = [inviteOf(7, 2, { uses: 1 })];
    const visibility = vi.spyOn(document, 'visibilityState', 'get').mockReturnValue('hidden');
    const before = reads();
    act(() => {
      document.dispatchEvent(new Event('visibilitychange'));
    });
    expect(reads()).toBe(before);
    visibility.mockReturnValue('visible');
    act(() => {
      document.dispatchEvent(new Event('visibilitychange'));
    });
    await within(list).findByText('Used');
    expect(within(list).getByText('· joined')).toBeDefined();
    expect(within(list).queryByRole('button', { name: 'Close' })).toBeNull();
    expect(reads()).toBe(before + 1);

    // Once closed, the panel does not read on being shown again.
    fireEvent.click(within(panel).getByRole('button', { name: 'Invite someone' }));
    await act(async () => {});
    const closed = reads();
    act(() => {
      document.dispatchEvent(new Event('visibilitychange'));
    });
    expect(reads()).toBe(closed);
  });

  it('shows a use elsewhere on Refresh', async () => {
    let listed = [inviteOf(7, 2)];
    serve({
      ...as(memberToken, inviter, channelList('general')),
      'GET /v1/invites': () => json({ invites: listed }),
    });
    window.localStorage.setItem(key, memberToken);
    render(<App origin={origin} />);
    const panel = await openPanel();
    const list = await within(panel).findByRole('list', { name: 'Your invites' });
    await within(list).findByText('Open');
    listed = [inviteOf(7, 2, { uses: 1 })];
    fireEvent.click(within(panel).getByRole('button', { name: 'Refresh' }));
    await within(list).findByText('Used');
  });

  it('shows an operator no count, since operators have no allowance', async () => {
    serve({
      ...as(operatorToken, { ...operator, voice_allowed: true }, channelList('general')),
      'GET /v1/invites': () => json({ invites: [inviteOf(3, 1)] }),
    });
    window.sessionStorage.setItem(key, operatorToken);
    render(<App origin={origin} />);
    const panel = await openPanel();
    await within(panel).findByRole('list', { name: 'Your invites' });
    expect(within(panel).getByRole('checkbox', { name: 'Voice' })).toBeDefined();
    expect(within(panel).queryByText(/invites? left/)).toBeNull();
  });
});

// Design §5.4 (security review of #713, B-1): on plain HTTP beyond this
// machine the landing page offers no form that would send a password or
// start a visit, whatever the invite allows, and says what the operator has
// to do. Signing in instead is still offered: that page is held to the same
// rule.
describe('the landing page over plain HTTP', () => {
  it.each([
    { name: 'an account-only invite', join: undefined },
    { name: 'an invite that admits both', join: { guest: true, account: true } },
    { name: 'a guest-only invite', join: { guest: true, account: false } },
  ])('offers no password or guest form for $name, and explains HTTPS', async ({ join }) => {
    const calls = serve({ ...joining, 'POST /v1/invites/preview': () => json(join === undefined ? preview : { ...preview, join }) });
    window.location.hash = `#/invite/${inviteCode}`;
    render(<App origin="http://192.168.1.20:8080" />);
    expect(await screen.findByText(insecureLine)).toBeDefined();
    expect(document.querySelector('input[type="password"]')).toBeNull();
    expect(screen.queryByLabelText('Your name')).toBeNull();
    expect(screen.queryByLabelText('Name to show (optional)')).toBeNull();
    expect(screen.queryByRole('group', { name: 'How to join' })).toBeNull();
    expect(calls.some((c) => c.path.includes('/redeem'))).toBe(false);
    // The sign-in page it leads to is gated too.
    fireEvent.click(screen.getByRole('button', { name: 'Sign in instead' }));
    expect(await screen.findByText(insecureLine)).toBeDefined();
    expect(screen.queryByLabelText('Password')).toBeNull();
  });
});
