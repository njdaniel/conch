// Invites in the browser (issue #440): the link's route, the landing page and
// its answers, and "Invite someone". conchd is a stand-in fetch.
import { act, cleanup, fireEvent, render, screen, waitFor, within } from '@testing-library/react';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { dateTimeOf } from './time';
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
const preview = { kind: 'member', server_name: 'Conch', inviter: 'ada', channels: ['red', 'blue'], expires_at, join: { guest: false, account: true } };
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
  'POST /v2/invites/preview': () => json(preview),
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
    expect(within(shown).queryByText('Voice')).toBeNull();
    expect(within(shown).getByText(dateTimeOf(expires_at)).getAttribute('datetime')).toBe(expires_at);
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
      'POST /v2/invites/preview',
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

  // Issue #990: a member link is previewed and redeemed through v2 alone, and
  // the page shows no voice row, a link that does not expire, and never the
  // guest flow.
  it('previews through v2 alone, says a link does not expire, and offers no guest flow', async () => {
    const { calls } = opened({
      ...joining,
      'POST /v2/invites/preview': () => json({ kind: 'member', server_name: 'Conch', inviter: 'ada', channels: ['red'], join: { guest: true, account: true } }),
    });
    const shown = await screen.findByLabelText('What the invite gives');
    expect(within(shown).getByText('Does not expire')).toBeDefined();
    expect(within(shown).queryByText('Voice')).toBeNull();
    expect(screen.queryByRole('button', { name: /guest/i })).toBeNull();
    expect(calls.some((c) => c.path.startsWith('/v1/invites'))).toBe(false);
    expect(calls.some((c) => c.path.includes('guest'))).toBe(false);
  });

  // A refused code shows the page's words for its code, never the server's.
  it.each([
    { status: 404, code: 'invite_invalid', line: noLongerWorks },
    { status: 503, code: 'invites_paused', line: 'Invites are paused on this server for now. Try the link again later.' },
    { status: 429, code: 'rate_limited', line: 'Too many tries from your network. Try again in 2 minutes.' },
    { status: 500, code: 'something_new', line: 'The server gave an unexpected reply. Try again.' },
  ])('says $code of a preview in the page\u2019s words, never the server\u2019s', async ({ status, code, line }) => {
    opened({ ...joining, 'POST /v2/invites/preview': () => refusal(status, code, 'SERVER WORDS', { 'Retry-After': '90' }) });
    expect(await screen.findByText(line)).toBeDefined();
    expect(document.body.textContent).not.toContain('SERVER WORDS');
  });

  // Issue #556: a guest whose invite grants #general lands there, even when
  // another granted channel comes first.
  it('lands the joiner in #general when the invite grants it', async () => {
    opened({
      ...joining,
      'POST /v2/invites/preview': () => json({ ...preview, channels: ['red', 'general'] }),
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

  // F19: guests are off (#1005), so the name hint no longer says the prefix is for them.
  it('says the guest prefix is reserved, and not that it is kept for guests', async () => {
    opened(joining);
    const field = await screen.findByLabelText('Your name');
    expect(field.getAttribute('aria-describedby')).not.toBeNull();
    expect(document.body.textContent).toContain('Names that begin with “guest” are reserved.');
    expect(document.body.textContent).not.toContain('kept for guests');
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

  // Item: the server's words for a request it could not take are other
  // people's text; the page says its own, matched on the rule's words.
  it.each([
    { message: 'SERVER WORDS <img src=x>', line: 'The server did not accept that name or password. Check both and try again.', field: 'Your name' },
    { message: 'schema: password SERVER WORDS <img src=x>', line: 'That password cannot be used. Choose another.', field: 'Choose a password' },
  ])('never shows the server\u2019s words for an invalid_request: $message', async ({ message, line, field }) => {
    opened({ ...joining, 'POST /v2/invites/redeem': () => refusal(400, 'invalid_request', message) });
    await chooseName('wren');
    expect(await screen.findByText(line)).toBeDefined();
    expect(document.body.textContent).not.toContain('SERVER WORDS');
    expect(document.body.innerHTML).not.toContain('<img');
    await waitFor(() => {
      expect(document.activeElement).toBe(screen.getByLabelText(field));
    });
  });

  it.each<{ readonly when: string; readonly routes: Record<string, Route> }>([
    { when: 'previewed', routes: { 'POST /v2/invites/preview': () => refusal(404, 'invite_invalid', 'this invite is not valid') } },
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
    serve({ ...joining, 'POST /v2/invites/preview': () => refusal(404, 'invite_invalid', 'this invite is not valid') });
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
      'POST /v2/invites/preview': () => refusal(404, 'invite_invalid', 'this invite is not valid'),
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
    const { calls } = opened({ ...joining, 'POST /v2/invites/preview': answer });
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
      'POST /v2/invites/preview': (): Response => {
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
// A member link as GET /v2/invites lists it (issue #990).
const linkOf = (id: number, createdBy: number, extra: object = {}) => ({
  id,
  kind: 'member',
  created_by: createdBy,
  created_by_name: createdBy === 1 ? 'nick' : 'ada',
  created_at,
  expires_at: '2099-01-01T00:00:00Z',
  uses: 0,
  joiners: 0,
  credential_lifetime_seconds: 7776000,
  ...extra,
});

const newCode = `conch_inv_${'N'.repeat(43)}`;

async function openPanel(): Promise<HTMLElement> {
  const panel = await screen.findByRole('region', { name: 'Invites' });
  fireEvent.click(within(panel).getByRole('button', { name: 'Invite someone' }));
  return panel;
}

const forMember = (extra: Record<string, Route> = {}) => {
  window.localStorage.setItem(key, memberToken);
  return serve({ ...as(memberToken, { ...inviter, invites_remaining: 5 }, channelList('general', 'ops')), 'GET /v2/invites': () => json({ invites: [] }), ...extra });
};
const forOperator = (extra: Record<string, Route> = {}) => {
  window.sessionStorage.setItem(key, operatorToken);
  return serve({ ...as(operatorToken, operator, channelList('general')), 'GET /v2/invites': () => json({ invites: [] }), ...extra });
};

describe('member links', () => {
  // A member whose right to make links is off (the brake) is told why and is
  // offered no form; nothing of links is asked of the server.
  it('tells a member whose right is off why, and offers no form', async () => {
    const calls = serve(as(memberToken, { ...member, may_invite: false }, channelList('general')));
    window.localStorage.setItem(key, memberToken);
    render(<App origin={origin} />);
    const panel = await openPanel();
    expect(within(panel).getByText('You may not make invite links. An operator has turned that off for you.')).toBeDefined();
    expect(within(panel).queryByRole('button', { name: 'Make link' })).toBeNull();
    expect(within(panel).queryByRole('combobox')).toBeNull();
    expect(calls.some((c) => c.path.startsWith('/v2/invites'))).toBe(false);
    // The channel row's ⋯ is only Leave, not Invite (issues #1135, #1095).
    await screen.findByRole('button', { name: 'general' });
    fireEvent.click(screen.getByRole('button', { name: 'Actions for general' }));
    expect(screen.getAllByRole('menuitem').map((item) => item.textContent)).toEqual(['Favorite', 'Leave channel']);
  });

  // The form never shows more than the server last answered: a right turned
  // off since the page loaded is shown off once the panel reads whoami again.
  it('hides the form once the server says the right has been turned off', async () => {
    let reads = 0;
    forMember({
      'GET /v1/whoami': (c) => {
        if (c.auth !== `Bearer ${memberToken}`) return json({}, 401);
        reads++;
        return json({ ...inviter, may_invite: reads <= 1 });
      },
    });
    render(<App origin={origin} />);
    const panel = await openPanel();
    expect(await within(panel).findByText('You may not make invite links. An operator has turned that off for you.')).toBeDefined();
    expect(within(panel).queryByRole('button', { name: 'Make link' })).toBeNull();
  });

  // Issue #1135: a channel's ⋯ menu opens this panel. A member link names no
  // channel, so none is ticked or sent.
  it("opens from a channel's ⋯ menu, and makes a link that names no channel", async () => {
    const calls = forMember({ 'POST /v2/invites': () => json({ invite: linkOf(7, 2), code: newCode }, 201) });
    render(<App origin={origin} />);
    const nav = within(await screen.findByRole('navigation', { name: 'Channels' }));
    await nav.findByRole('button', { name: 'ops' });
    const panel = screen.getByRole('region', { name: 'Invites' });
    expect(within(panel).getByRole('button', { name: 'Invite someone' }).getAttribute('aria-expanded')).toBe('false');

    fireEvent.click(nav.getByRole('button', { name: 'Actions for ops' }));
    fireEvent.click(screen.getByRole('menuitem', { name: 'Invite people' }));
    expect(within(panel).getByRole('button', { name: 'Invite someone' }).getAttribute('aria-expanded')).toBe('true');
    expect(within(panel).queryByRole('checkbox', { name: '#ops' })).toBeNull();
    await waitFor(() => {
      expect(panel.contains(document.activeElement)).toBe(true);
    });
    fireEvent.click(within(panel).getByRole('button', { name: 'Make link' }));
    await within(panel).findByRole('textbox', { name: 'Invite link' });
    expect(calls.filter((c) => c.method === 'POST' && c.path === '/v2/invites').map((c) => c.body)).toEqual([{ kind: 'member' }]);
  });

  it('lets a member make a link with the default expiry, copy it once, see it listed and close it', async () => {
    let listed = [linkOf(5, 2, { uses: 1, joiners: 1, max_uses: 1 })];
    const calls = forMember({
      'POST /v2/invites': () => {
        listed = [linkOf(7, 2), ...listed];
        return json({ invite: linkOf(7, 2), code: newCode }, 201);
      },
      'GET /v2/invites': () => json({ invites: listed }),
      'DELETE /v1/invites/7': () => {
        listed = listed.map((i) => (i.id === 7 ? { ...i, closed_at: '2026-10-16T00:00:00Z' } : i));
        return new Response(null, { status: 204 });
      },
    });
    const writeText = vi.fn(() => Promise.resolve());
    Object.defineProperty(window.navigator, 'clipboard', { configurable: true, value: { writeText } });
    render(<App origin={origin} />);
    const panel = await openPanel();

    // A member's form: an expiry without "never", and a use limit; no kind, no
    // channels, no voice, no right to pass on.
    expect(within(panel).queryByRole('radio')).toBeNull();
    expect(within(panel).queryByRole('checkbox', { name: 'Voice' })).toBeNull();
    expect(within(panel).queryByText(/Joiners may invite/)).toBeNull();
    const expiry = within(panel).getByRole<HTMLSelectElement>('combobox');
    expect(expiry.value).toBe('7 days');
    expect([...expiry.options].map((o) => o.value)).toEqual(['1 day', '7 days', '30 days']);
    expect(await within(panel).findByText('5 links left')).toBeDefined();
    await within(panel).findByText('Used up');

    fireEvent.click(within(panel).getByRole('button', { name: 'Make link' }));
    const link = await within(panel).findByRole<HTMLInputElement>('textbox', { name: 'Invite link' });
    expect(link.value).toBe(`${origin}/app/#/invite/${newCode}`);
    expect(within(panel).getByText(/Anyone with this link can join/)).toBeDefined();
    // The default expiry is the server's: none is sent, and no limit.
    expect(calls.find((c) => c.method === 'POST' && c.path === '/v2/invites')?.body).toEqual({ kind: 'member' });

    fireEvent.click(within(panel).getByRole('button', { name: 'Copy link' }));
    await within(panel).findByText('Copied.');
    expect(writeText).toHaveBeenCalledWith(`${origin}/app/#/invite/${newCode}`);

    // Listed, open, with a close button; closing it shows it closed.
    const list = await within(panel).findByRole('list', { name: 'Your links' });
    await waitFor(() => {
      expect(within(list).getAllByRole('listitem')).toHaveLength(2);
    });
    const fresh = within(list).getAllByRole('listitem')[0];
    if (fresh === undefined) throw new Error('no link listed');
    expect(fresh.textContent).toContain('Unused');
    expect(fresh.textContent).toContain('0 joined');
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

  it('sends the expiry and the use limit a member chose', async () => {
    const calls = forMember({ 'POST /v2/invites': () => json({ invite: linkOf(7, 2, { max_uses: 3 }), code: newCode }, 201) });
    render(<App origin={origin} />);
    const panel = await openPanel();
    const before = Date.now();
    fireEvent.change(within(panel).getByRole('combobox'), { target: { value: '1 day' } });
    fireEvent.click(within(panel).getByRole('checkbox', { name: 'Limit the number of people' }));
    fireEvent.change(within(panel).getByRole('spinbutton'), { target: { value: '3' } });
    fireEvent.click(within(panel).getByRole('button', { name: 'Make link' }));
    await within(panel).findByRole('textbox', { name: 'Invite link' });
    const sent = calls.find((c) => c.method === 'POST' && c.path === '/v2/invites')?.body as { kind: string; expires_at: string; max_uses: number; never_expires?: boolean };
    expect(sent.kind).toBe('member');
    expect(sent.max_uses).toBe(3);
    expect(sent.never_expires).toBeUndefined();
    expect(Math.abs(Date.parse(sent.expires_at) - (before + 24 * 60 * 60 * 1000))).toBeLessThan(60_000);
  });

  it('disables Make link for a use limit that is no whole number of people', async () => {
    forMember();
    render(<App origin={origin} />);
    const panel = await openPanel();
    fireEvent.click(within(panel).getByRole('checkbox', { name: 'Limit the number of people' }));
    for (const bad of ['0', '1.5', '', '2000000']) {
      fireEvent.change(within(panel).getByRole('spinbutton'), { target: { value: bad } });
      expect(within(panel).getByRole('button', { name: 'Make link' }).hasAttribute('disabled'), bad).toBe(true);
    }
    fireEvent.change(within(panel).getByRole('spinbutton'), { target: { value: '2' } });
    expect(within(panel).getByRole('button', { name: 'Make link' }).hasAttribute('disabled')).toBe(false);
  });

  it('offers "Never expires" to an operator alone, and sends it as never_expires', async () => {
    const calls = forOperator({ 'POST /v2/invites': () => json({ invite: linkOf(8, 1, { expires_at: undefined }), code: newCode }, 201) });
    render(<App origin={origin} />);
    const panel = await openPanel();
    const expiry = within(panel).getByRole<HTMLSelectElement>('combobox');
    expect([...expiry.options].map((o) => o.value)).toEqual(['1 day', '7 days', '30 days', 'Never expires']);
    fireEvent.change(expiry, { target: { value: 'Never expires' } });
    fireEvent.click(within(panel).getByRole('button', { name: 'Make link' }));
    await within(panel).findByRole('textbox', { name: 'Invite link' });
    expect(calls.find((c) => c.method === 'POST' && c.path === '/v2/invites')?.body).toEqual({ kind: 'member', never_expires: true });
  });

  it('offers no "Never expires" to an operator who lacks manage_members', async () => {
    window.sessionStorage.setItem(key, operatorToken);
    serve({
      ...as(operatorToken, { ...operator, permissions: ['make_member_links'] }, channelList('general')),
      'GET /v2/invites': () => json({ invites: [] }),
    });
    render(<App origin={origin} />);
    const panel = await openPanel();
    expect([...within(panel).getByRole<HTMLSelectElement>('combobox').options].map((o) => o.value)).not.toContain('Never expires');
  });

  it("lists only the caller's own links, with their uses, a link that never expires and who joined", async () => {
    const calls = forOperator({
      'GET /v2/invites': () =>
        json({ invites: [linkOf(3, 1, { joiners: 2, uses: 2, max_uses: 5, expires_at: undefined }), linkOf(4, 2, { joiners: 1 })] }),
      'GET /v2/invites/3/joiners': () =>
        json({ joiners: [{ id: 20, name: 'wren', created_at, disabled: false }, { id: 21, name: '<b>cy</b>', created_at, disabled: true }] }),
    });
    render(<App origin={origin} />);
    const panel = await openPanel();
    const list = await within(panel).findByRole('list', { name: 'Your links' });
    const items = within(list).getAllByRole('listitem');
    expect(items).toHaveLength(1);
    expect(items[0]?.textContent).toContain('2 of 5 joined');
    expect(items[0]?.textContent).toContain('never expires');

    fireEvent.click(within(panel).getByRole('button', { name: 'Joiners (2)' }));
    const names = await within(panel).findByRole('list', { name: 'Joiners of this link' });
    // Other people's names are text, never markup.
    expect(within(names).getAllByRole('listitem').map((i) => i.textContent)).toEqual(['wren', '<b>cy</b> (disabled)']);
    expect(names.querySelector('b')).toBeNull();
    expect(calls.some((c) => c.path === '/v2/invites/3/joiners')).toBe(true);
    fireEvent.click(within(panel).getByRole('button', { name: 'Hide joiners' }));
    expect(within(panel).queryByRole('list', { name: 'Joiners of this link' })).toBeNull();
  });

  it('says in its own words when the joiners cannot be read', async () => {
    forMember({
      'GET /v2/invites': () => json({ invites: [linkOf(3, 2, { joiners: 1 })] }),
      'GET /v2/invites/3/joiners': () => refusal(404, 'invite_not_found', 'SERVER WORDS'),
    });
    render(<App origin={origin} />);
    const panel = await openPanel();
    fireEvent.click(await within(panel).findByRole('button', { name: 'Joiners (1)' }));
    expect(await within(panel).findByText('That link is gone.')).toBeDefined();
    expect(panel.textContent).not.toContain('SERVER WORDS');
  });

  // Design O2: members get no control to remove the people a link brought in.
  it('gives a member no way to disable the people a link brought in', async () => {
    forMember({ 'GET /v2/invites': () => json({ invites: [linkOf(3, 2, { joiners: 2, uses: 2 })] }) });
    render(<App origin={origin} />);
    const panel = await openPanel();
    await within(panel).findByRole('button', { name: 'Joiners (2)' });
    expect(within(panel).queryByRole('button', { name: /disable/i })).toBeNull();
  });

  it('gives an operator Disable joiners on a link, which asks how many first and disables them only on a yes', async () => {
    const calls = forOperator({
      'GET /v2/invites': () => json({ invites: [linkOf(3, 1, { joiners: 2, uses: 2 })] }),
      'POST /v1/invites/3/disable-joiners': (c) => {
        const dry = (c.body as { dry_run: boolean }).dry_run;
        return json({ dry_run: dry, count: 2, principal_ids: [20, 21] });
      },
    });
    render(<App origin={origin} />);
    const panel = await openPanel();
    fireEvent.click(await within(panel).findByRole('button', { name: 'Disable joiners' }));
    const ask = await within(panel).findByRole('group', { name: 'Disable joiners' });
    expect(ask.textContent).toContain('signs out and disables 2 people');
    expect(calls.filter((c) => c.path.endsWith('/disable-joiners')).map((c) => c.body)).toEqual([{ dry_run: true }]);

    // Cancel changes nothing.
    fireEvent.click(within(ask).getByRole('button', { name: 'Cancel' }));
    expect(within(panel).queryByRole('group', { name: 'Disable joiners' })).toBeNull();
    expect(calls.filter((c) => c.path.endsWith('/disable-joiners'))).toHaveLength(1);

    fireEvent.click(within(panel).getByRole('button', { name: 'Disable joiners' }));
    fireEvent.click(await within(panel).findByRole('button', { name: 'Disable 2 people' }));
    expect(await within(panel).findByText('Disabled 2 people.')).toBeDefined();
    expect(calls.filter((c) => c.path.endsWith('/disable-joiners')).map((c) => c.body)).toEqual([{ dry_run: true }, { dry_run: true }, { dry_run: false }]);
  });

  it('closes the Disable joiners question when whoami is read again without manage_members, and sends nothing more', async () => {
    let stripped = false;
    const calls = forOperator({
      'GET /v1/whoami': (c) => (c.auth === `Bearer ${operatorToken}` ? json(stripped ? { ...operator, permissions: ['make_member_links'] } : operator) : json({}, 401)),
      'GET /v2/invites': () => json({ invites: [linkOf(3, 1, { joiners: 2, uses: 2 })] }),
      'POST /v1/invites/3/disable-joiners': (c) => json({ dry_run: (c.body as { dry_run: boolean }).dry_run, count: 2, principal_ids: [20, 21] }),
    });
    render(<App origin={origin} />);
    const panel = await openPanel();
    fireEvent.click(await within(panel).findByRole('button', { name: 'Disable joiners' }));
    const ask = await within(panel).findByRole('group', { name: 'Disable joiners' });
    // The question says the link is closed too.
    expect(ask.textContent).toContain('closes the link too');

    stripped = true;
    fireEvent.click(within(panel).getByRole('button', { name: 'Refresh' }));
    await waitFor(() => {
      expect(within(panel).queryByRole('group', { name: 'Disable joiners' })).toBeNull();
    });
    expect(within(panel).queryByRole('button', { name: /Disable/ })).toBeNull();
    expect(calls.filter((c) => c.path.endsWith('/disable-joiners')).map((c) => c.body)).toEqual([{ dry_run: true }]);
  });

  it('puts "Never expires" back to the default when manage_members goes', async () => {
    let stripped = false;
    forOperator({
      'GET /v1/whoami': (c) => (c.auth === `Bearer ${operatorToken}` ? json(stripped ? { ...operator, permissions: ['make_member_links'] } : operator) : json({}, 401)),
    });
    render(<App origin={origin} />);
    const panel = await openPanel();
    const expiry = within(panel).getByRole<HTMLSelectElement>('combobox');
    fireEvent.change(expiry, { target: { value: 'Never expires' } });
    expect(expiry.value).toBe('Never expires');
    stripped = true;
    fireEvent.click(within(panel).getByRole('button', { name: 'Refresh' }));
    await waitFor(() => {
      expect(within(panel).getByRole<HTMLSelectElement>('combobox').value).toBe('7 days');
    });
  });

  it('keeps focus in the panel, on Refresh, when Cancel, Disable or Close removes the button pressed', async () => {
    forOperator({
      'GET /v2/invites': () => json({ invites: [linkOf(3, 1, { joiners: 2, uses: 2 })] }),
      'POST /v1/invites/3/disable-joiners': (c) => json({ dry_run: (c.body as { dry_run: boolean }).dry_run, count: 2, principal_ids: [20, 21] }),
      'DELETE /v1/invites/3': () => new Response(null, { status: 204 }),
    });
    render(<App origin={origin} />);
    const panel = await openPanel();
    const refreshButton = within(panel).getByRole('button', { name: 'Refresh' });
    fireEvent.click(await within(panel).findByRole('button', { name: 'Disable joiners' }));
    fireEvent.click(await within(panel).findByRole('button', { name: 'Cancel' }));
    expect(document.activeElement).toBe(refreshButton);
    fireEvent.click(within(panel).getByRole('button', { name: 'Disable joiners' }));
    (within(panel).getByRole('button', { name: 'Refresh' })).focus();
    fireEvent.click(await within(panel).findByRole('button', { name: 'Disable 2 people' }));
    expect(document.activeElement).toBe(refreshButton);
    within(panel).getByRole('button', { name: 'Close' }).focus();
    fireEvent.click(within(panel).getByRole('button', { name: 'Close' }));
    expect(document.activeElement).toBe(refreshButton);
  });

  it('sends one request however often Make link is pressed before the answer', async () => {
    let release: (r: Response) => void = () => undefined;
    const calls = forMember({ 'POST /v2/invites': () => new Promise<Response>((resolve) => (release = resolve)) });
    render(<App origin={origin} />);
    const panel = await openPanel();
    const form = within(panel).getByRole('button', { name: 'Make link' }).closest('form');
    if (form === null) throw new Error('no form');
    // All inside one act: busy has not rendered between the presses.
    act(() => {
      for (let i = 0; i < 4; i++) form.dispatchEvent(new Event('submit', { bubbles: true, cancelable: true }));
    });
    expect(calls.filter((c) => c.method === 'POST' && c.path === '/v2/invites')).toHaveLength(1);
    await act(async () => {
      release(json({ invite: linkOf(7, 2), code: newCode }, 201));
      await Promise.resolve();
    });
    await within(panel).findByRole('textbox', { name: 'Invite link' });
    expect(calls.filter((c) => c.method === 'POST' && c.path === '/v2/invites')).toHaveLength(1);
  });

  // The refusal is the page's words for its code: the server's message, which
  // is other people's text as far as the page goes, is never shown.
  it.each([
    { code: 'invite_not_allowed', status: 403, line: 'You may not make invite links. An operator has turned that off for you.' },
    { code: 'invite_allowance_exceeded', status: 403, line: 'You already have as many open links as you may. Close one, or wait until one expires.' },
    { code: 'invite_grant_exceeds_inviter', status: 403, line: 'A link can land people only in channels you are in.' },
    { code: 'invites_paused', status: 503, line: 'Invites are paused on this server for now.' },
    { code: 'invite_kind_retired', status: 409, line: 'This server no longer makes that kind of invite.' },
    { code: 'rate_limited', status: 429, line: 'You have made too many links for now. Try again in 15 minutes.' },
    { code: 'invalid_request', status: 400, line: 'The server did not accept those settings. Check the number of people.' },
    { code: 'something_new', status: 500, line: 'The server refused the link.' },
  ])('says $code in its own words, never the server’s', async ({ code, status, line }) => {
    forMember({ 'POST /v2/invites': () => refusal(status, code, 'SERVER WORDS <img src=x>', { 'Retry-After': '900' }) });
    render(<App origin={origin} />);
    const panel = await openPanel();
    fireEvent.click(within(panel).getByRole('button', { name: 'Make link' }));
    expect(await within(panel).findByText(line)).toBeDefined();
    expect(panel.textContent).not.toContain('SERVER WORDS');
    expect(within(panel).queryByRole('textbox', { name: 'Invite link' })).toBeNull();
  });

  // Issue #909: an operator is told where the resume switch is, and the button
  // opens Settings with Joining expanded; a member gets the plain line.
  it('tells an operator whose link is refused as paused where to resume, and the button opens the switch', async () => {
    forOperator({
      'POST /v2/invites': () => refusal(503, 'invites_paused'),
      'GET /v1/invites/settings': () => json({ paused: true }),
    });
    render(<App origin={origin} />);
    const panel = await openPanel();
    fireEvent.click(within(panel).getByRole('button', { name: 'Make link' }));
    expect(await within(panel).findByText('Invites are paused on this server. Resume them in Settings → Joining.')).toBeDefined();
    expect(screen.queryByRole('region', { name: 'Joining' })?.hasAttribute('hidden') ?? true).toBe(true);

    fireEvent.click(within(panel).getByRole('button', { name: 'Open Settings → Joining' }));
    const region = screen.getByRole('region', { name: 'Joining' });
    expect(region.hasAttribute('hidden')).toBe(false);
    expect(document.activeElement).toBe(screen.getByRole('button', { name: 'Joining' }));
    expect((await within(region).findByLabelText<HTMLInputElement>(/^Pause all invites/)).checked).toBe(true);
  });

  it('gives a member whose link is refused as paused only the plain line, and no button', async () => {
    forMember({ 'POST /v2/invites': () => refusal(503, 'invites_paused') });
    render(<App origin={origin} />);
    const panel = await openPanel();
    fireEvent.click(within(panel).getByRole('button', { name: 'Make link' }));
    expect(await within(panel).findByText('Invites are paused on this server for now.')).toBeDefined();
    expect(within(panel).queryByText(/Settings → Joining/)).toBeNull();
    expect(screen.queryByRole('button', { name: /^(Open Settings → )?Joining$/ })).toBeNull();
  });

  // F21: the states, the pause banner, the link shown once, and the keyboard.
  it('reads Unused for a link no one has used, and Open for one that has been', async () => {
    forMember({ 'GET /v2/invites': () => json({ invites: [linkOf(1, 2), linkOf(2, 2, { uses: 1, joiners: 1 })] }) });
    render(<App origin={origin} />);
    const panel = await openPanel();
    const list = await within(panel).findByRole('list', { name: 'Your links' });
    const [unused, used] = within(list).getAllByRole('listitem');
    expect(unused?.textContent).toContain('Unused');
    expect(used?.textContent).toContain('Open');
    expect(used?.textContent).not.toContain('Unused');
  });

  it('shows an operator the paused banner while invites are paused, before any refusal', async () => {
    forOperator({
      'GET /v1/invites/settings': () => json({ paused: true }),
      'GET /v2/invites': () => json({ invites: [linkOf(1, 1)] }),
    });
    render(<App origin={origin} />);
    const panel = await openPanel();
    expect(await within(panel).findByText('Invites are paused on this server. Resume them in Settings → Joining.')).toBeDefined();
    const list = await within(panel).findByRole('list', { name: 'Your links' });
    expect(within(list).getAllByRole('listitem')[0]?.textContent).toContain('Paused');
  });

  it('shows no paused banner to an operator while invites are on', async () => {
    forOperator({ 'GET /v1/invites/settings': () => json({ paused: false }) });
    render(<App origin={origin} />);
    const panel = await openPanel();
    await within(panel).findByText('None yet.');
    expect(within(panel).queryByText(/Invites are paused/)).toBeNull();
  });

  it('shows a member the paused banner only after a paused refusal, and then marks their links Paused', async () => {
    forMember({
      'GET /v2/invites': () => json({ invites: [linkOf(1, 2)] }),
      'POST /v2/invites': () => refusal(503, 'invites_paused'),
    });
    render(<App origin={origin} />);
    const panel = await openPanel();
    const list = await within(panel).findByRole('list', { name: 'Your links' });
    expect(within(panel).queryByText(/Invites are paused/)).toBeNull();
    expect(within(list).getAllByRole('listitem')[0]?.textContent).toContain('Unused');
    fireEvent.click(within(panel).getByRole('button', { name: 'Make link' }));
    expect(await within(panel).findByText('Invites are paused on this server for now.')).toBeDefined();
    expect(within(list).getAllByRole('listitem')[0]?.textContent).toContain('Paused');
  });

  it('forgets the link shown once when the panel closes', async () => {
    forMember({ 'POST /v2/invites': () => json({ invite: linkOf(7, 2), code: newCode }, 201) });
    render(<App origin={origin} />);
    const panel = await openPanel();
    fireEvent.click(within(panel).getByRole('button', { name: 'Make link' }));
    await within(panel).findByRole('textbox', { name: 'Invite link' });
    const toggle = within(panel).getByRole('button', { name: 'Invite someone' });
    fireEvent.click(toggle);
    fireEvent.click(toggle);
    expect(within(panel).queryByRole('textbox', { name: 'Invite link' })).toBeNull();
    expect(panel.textContent).not.toContain(newCode);
  });

  it('forgets the link shown once when its first use is seen', async () => {
    let uses = 0;
    forMember({
      'POST /v2/invites': () => json({ invite: linkOf(7, 2), code: newCode }, 201),
      'GET /v2/invites': () => json({ invites: [linkOf(7, 2, { uses, joiners: uses })] }),
    });
    render(<App origin={origin} />);
    const panel = await openPanel();
    fireEvent.click(within(panel).getByRole('button', { name: 'Make link' }));
    await within(panel).findByRole('textbox', { name: 'Invite link' });
    // Read again with the link still unused: it stays.
    fireEvent.click(within(panel).getByRole('button', { name: 'Refresh' }));
    await within(panel).findByText('Unused');
    expect(within(panel).queryByRole('textbox', { name: 'Invite link' })).not.toBeNull();
    uses = 1;
    fireEvent.click(within(panel).getByRole('button', { name: 'Refresh' }));
    await waitFor(() => {
      expect(within(panel).queryByRole('textbox', { name: 'Invite link' })).toBeNull();
    });
  });

  it('closes on Escape from the Invite someone button, and opening moves focus into the panel', async () => {
    forMember();
    render(<App origin={origin} />);
    const panel = await screen.findByRole('region', { name: 'Invites' });
    const toggle = within(panel).getByRole('button', { name: 'Invite someone' });
    fireEvent.click(toggle);
    await waitFor(() => {
      expect(panel.querySelector('#invites-body')?.contains(document.activeElement)).toBe(true);
    });
    toggle.focus();
    fireEvent.keyDown(toggle, { key: 'Escape' });
    expect(toggle.getAttribute('aria-expanded')).toBe('false');
    expect(document.activeElement).toBe(toggle);
  });

  it('says it cannot reach the server when the answer does not come', async () => {
    forMember({ 'POST /v2/invites': () => Promise.reject(new TypeError('Failed to fetch')) });
    render(<App origin={origin} />);
    const panel = await openPanel();
    fireEvent.click(within(panel).getByRole('button', { name: 'Make link' }));
    expect(await within(panel).findByText('Cannot reach the server. Try again.')).toBeDefined();
  });

  it('closes on Escape, and focus goes back to Invite someone', async () => {
    forMember();
    render(<App origin={origin} />);
    const panel = await openPanel();
    const button = within(panel).getByRole('button', { name: 'Invite someone' });
    fireEvent.keyDown(within(panel).getByRole('button', { name: 'Make link' }), { key: 'Escape' });
    expect(button.getAttribute('aria-expanded')).toBe('false');
    expect(document.activeElement).toBe(button);
  });

  it('puts focus on Make link after Done, and reads the list again', async () => {
    let reads = 0;
    forMember({
      'POST /v2/invites': () => json({ invite: linkOf(7, 2), code: newCode }, 201),
      'GET /v2/invites': () => {
        reads++;
        return json({ invites: [] });
      },
    });
    render(<App origin={origin} />);
    const panel = await openPanel();
    fireEvent.click(within(panel).getByRole('button', { name: 'Make link' }));
    await within(panel).findByRole('textbox', { name: 'Invite link' });
    const before = reads;
    fireEvent.click(within(panel).getByRole('button', { name: 'Done' }));
    expect(document.activeElement).toBe(within(panel).getByRole('button', { name: 'Make link' }));
    await waitFor(() => {
      expect(reads).toBeGreaterThan(before);
    });
  });

  it('shows a use made elsewhere on Refresh, and says when the list was read', async () => {
    let listed = [linkOf(3, 2)];
    forMember({ 'GET /v2/invites': () => json({ invites: listed }) });
    render(<App origin={origin} />);
    const panel = await openPanel();
    const list = await within(panel).findByRole('list', { name: 'Your links' });
    expect(list.textContent).toContain('0 joined');
    expect(within(panel).getByText(/^Read at/)).toBeDefined();
    listed = [linkOf(3, 2, { uses: 1, joiners: 1 })];
    fireEvent.click(within(panel).getByRole('button', { name: 'Refresh' }));
    await waitFor(() => {
      expect(within(panel).getByRole('list', { name: 'Your links' }).textContent).toContain('1 joined');
    });
  });

  it('drops the answer of a link made before the panel was asked for again', async () => {
    let release: (r: Response) => void = () => undefined;
    forMember({ 'POST /v2/invites': () => new Promise<Response>((resolve) => (release = resolve)) });
    render(<App origin={origin} />);
    const nav = within(await screen.findByRole('navigation', { name: 'Channels' }));
    await nav.findByRole('button', { name: 'ops' });
    const panel = screen.getByRole('region', { name: 'Invites' });
    fireEvent.click(nav.getByRole('button', { name: 'Actions for ops' }));
    fireEvent.click(screen.getByRole('menuitem', { name: 'Invite people' }));
    fireEvent.click(within(panel).getByRole('button', { name: 'Make link' }));
    fireEvent.click(nav.getByRole('button', { name: 'Actions for general' }));
    fireEvent.click(screen.getByRole('menuitem', { name: 'Invite people' }));
    await act(async () => {
      release(json({ invite: linkOf(7, 2), code: newCode }, 201));
      await Promise.resolve();
    });
    expect(within(panel).queryByRole('textbox', { name: 'Invite link' })).toBeNull();
    expect(document.body.innerHTML).not.toContain(newCode);
  });

  it('goes back to sign-in when the server stops knowing the credential', async () => {
    forMember({ 'POST /v2/invites': () => json({}, 401) });
    render(<App origin={origin} />);
    const panel = await openPanel();
    fireEvent.click(within(panel).getByRole('button', { name: 'Make link' }));
    expect(await screen.findByRole('button', { name: 'Sign in' })).toBeDefined();
  });
});

describe('the landing page over plain HTTP', () => {
  it.each([
    { name: 'an account-only invite', join: undefined },
    { name: 'an invite that admits both', join: { guest: true, account: true } },
    { name: 'a guest-only invite', join: { guest: true, account: false } },
  ])('offers no password or guest form for $name, and explains HTTPS', async ({ join }) => {
    const calls = serve({ ...joining, 'POST /v2/invites/preview': () => json({ ...preview, join: join ?? preview.join }) });
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
