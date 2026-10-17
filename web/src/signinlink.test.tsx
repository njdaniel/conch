// Coming back into the same account (issue #493): the warning before a
// sign-out, the sign-in link's route and its landing page. conchd is a
// stand-in fetch.
import { cleanup, fireEvent, render, screen, waitFor } from '@testing-library/react';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { App } from './App';
import { signOutWarning } from './Lobby';
import { addressWithoutFragment, signInFromHash, takeLinkFromAddress } from './route';
import { linkHeldLine, linkNoLongerWorks } from './SignInLink';

const origin = 'http://127.0.0.1:8080';
const signInCode = `conch_sil_${'Q'.repeat(40)}x-_`;
const newToken = 'conch_new_secret';
const memberToken = 'conch_member_secret';
const otherToken = 'conch_other_secret';
const key = 'conch.web.token';
const created_at = '2026-10-14T00:00:00Z';
const expires_at = '2027-01-14T00:00:00Z';

const wren = { id: 9, kind: 'human', name: 'wren', role: 'member' };
const ada = { id: 2, kind: 'human', name: 'ada', role: 'member' };
const channelList = (...names: string[]) => ({ channels: names.map((name, i) => ({ id: i + 1, name, created_at })) });
const redeemed = {
  principal: { id: 9, kind: 'human', name: 'wren', created_at },
  credential: { id: 31, principal_id: 9, label: 'sign-in link 4', created_at, expires_at, renews: true },
  token: newToken,
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

// people answers whoami and the channel list by token.
function people(byToken: Record<string, { who: object; channels: object }>): Record<string, Route> {
  const holder = (c: Call) => byToken[(c.auth ?? '').replace(/^Bearer /, '')];
  return {
    'GET /v1/whoami': (c) => {
      const h = holder(c);
      return h === undefined ? json({}, 401) : json(h.who);
    },
    'GET /v1/channels': (c) => {
      const h = holder(c);
      return h === undefined ? json({}, 401) : json(h.channels);
    },
  };
}

// wren's server: the preview, the use, and wren's lobby once signed in.
const returning: Record<string, Route> = {
  'POST /v1/sign-in-links/preview': () => json({ name: 'wren' }),
  'POST /v1/sign-in-links/redeem': () => json(redeemed, 201),
  ...people({ [newToken]: { who: wren, channels: channelList('red') } }),
};

// opened renders the page as a tab opened on the sign-in link.
function opened(routes: Record<string, Route>) {
  const calls = serve(routes);
  window.location.hash = `#/signin/${signInCode}`;
  const view = render(<App origin={origin} />);
  return { calls, view };
}

// The page, the address and the stores never hold the code.
function expectNoCode(container: HTMLElement): void {
  expect(container.innerHTML).not.toContain(signInCode);
  expect(document.documentElement.outerHTML).not.toContain(signInCode);
  expect(window.location.href).not.toContain(signInCode);
  for (const storage of [window.localStorage, window.sessionStorage]) {
    for (let i = 0; i < storage.length; i++) expect(storage.getItem(storage.key(i) ?? '')).not.toContain(signInCode);
  }
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

describe('the sign-in link route', () => {
  it.each([
    { hash: `#/signin/${signInCode}`, want: signInCode },
    { hash: '#/signin/conch_sil_a%2Db', want: 'conch_sil_a-b' },
    { hash: '#/signin/%E0%A4%A', want: '%E0%A4%A' },
    { hash: '#/signin/', want: null },
    { hash: '#/signin/a/b', want: null },
    { hash: `#/invite/${signInCode}`, want: null },
    { hash: '#/c/general', want: null },
    { hash: '', want: null },
  ])('reads $hash as $want', ({ hash, want }) => {
    expect(signInFromHash(hash)).toBe(want);
  });

  it('takes the code out of the address with the one replaceState, to an address with no fragment', () => {
    const replace = vi.spyOn(window.history, 'replaceState');
    window.location.hash = `#/signin/${signInCode}`;
    expect(takeLinkFromAddress()).toEqual({ kind: 'signin', signInCode });
    expect(replace).toHaveBeenCalledTimes(1);
    expect(String(replace.mock.calls[0]?.[2])).toBe(addressWithoutFragment(window.location));
    expect(window.location.href).not.toContain(signInCode);
    expect(takeLinkFromAddress()).toBeNull();
    expect(replace).toHaveBeenCalledTimes(1);
  });

  it('takes out a link that looks like a sign-in link but does not parse', () => {
    window.location.hash = '#/signin/a/b';
    expect(takeLinkFromAddress()).toBeNull();
    expect(window.location.hash).toBe('');
  });
});

describe('the sign-in link landing page', () => {
  it('says whose account it opens, signs into it, and keeps the code nowhere', async () => {
    const replace = vi.spyOn(window.history, 'replaceState');
    const { calls, view } = opened(returning);
    expect(replace).toHaveBeenCalledTimes(1);
    expectNoCode(view.container);

    const button = await screen.findByRole('button', { name: 'Sign in as wren' });
    expect(screen.getByRole('heading', { level: 1 }).textContent).toBe('Sign in to Conch');
    expect(screen.getByText(/signs you back into your account/).textContent).toContain('wren');
    expect(screen.getByText(origin)).toBeDefined();
    // Nothing is used until the person says so.
    expect(calls.some((c) => c.path === '/v1/sign-in-links/redeem')).toBe(false);
    fireEvent.click(button);
    await screen.findByRole('navigation', { name: 'Channels' });
    expect(screen.getByRole('status', { name: 'Status' }).textContent).toContain('wren');

    // The code went only in the bodies of preview and redeem, with no
    // credential; the new one is kept to last and carried by what follows.
    expect(calls.filter((c) => JSON.stringify(c).includes(signInCode)).map((c) => `${c.method} ${c.path}`)).toEqual([
      'POST /v1/sign-in-links/preview',
      'POST /v1/sign-in-links/redeem',
    ]);
    expect(calls.find((c) => c.path === '/v1/sign-in-links/redeem')?.body).toEqual({ code: signInCode });
    expect(calls.filter((c) => c.path.startsWith('/v1/sign-in-links/')).every((c) => c.auth === null)).toBe(true);
    expect(window.localStorage.getItem(key)).toBe(newToken);
    const after = calls.slice(calls.findIndex((c) => c.path === '/v1/sign-in-links/redeem') + 1).filter((c) => !c.path.startsWith('/healthz'));
    expect(after.length).toBeGreaterThan(0);
    expect(after.every((c) => c.auth === `Bearer ${newToken}`)).toBe(true);
    expectNoCode(view.container);
    expect(view.container.innerHTML).not.toContain(newToken);
  });

  it.each<{ readonly when: string; readonly routes: Record<string, Route> }>([
    { when: 'previewed', routes: { 'POST /v1/sign-in-links/preview': () => refusal(404, 'sign_in_link_invalid', 'this sign-in link is not valid') } },
    { when: 'used', routes: { 'POST /v1/sign-in-links/redeem': () => refusal(404, 'sign_in_link_invalid', 'this sign-in link is not valid') } },
  ])('says the one "no longer works" when the link is invalid, $when', async ({ routes }) => {
    const { view } = opened({ ...returning, ...routes });
    if (routes['POST /v1/sign-in-links/redeem'] !== undefined) fireEvent.click(await screen.findByRole('button', { name: 'Sign in as wren' }));
    expect(await screen.findByText(linkNoLongerWorks)).toBeDefined();
    // Someone else may have used it: the person is asked to say so.
    expect(linkNoLongerWorks).toContain('If you did not use it, tell an operator.');
    expect(screen.getByRole('heading', { level: 1 }).textContent).toBe('This sign-in link no longer works');
    expect(screen.queryByRole('button', { name: 'Try again' })).toBeNull();
    expect(window.localStorage.getItem(key)).toBeNull();
    expectNoCode(view.container);
    fireEvent.click(screen.getByRole('button', { name: 'Go back' }));
    await screen.findByLabelText('Credential');
  });

  it.each([
    { name: 'rate-limited', answer: () => refusal(429, 'rate_limited', 'slow down', { 'Retry-After': '120' }), line: 'Too many tries from your network. Try again in 2 minutes.' },
    { name: 'unreachable', answer: (): Response => { throw new TypeError('Failed to fetch'); }, line: 'Cannot reach the server. Check your connection and try again.' },
  ])('says so when the preview is $name, and can try again', async ({ answer, line }) => {
    opened({ ...returning, 'POST /v1/sign-in-links/preview': answer });
    expect(await screen.findByText(line)).toBeDefined();
    serve(returning);
    fireEvent.click(screen.getByRole('button', { name: 'Try again' }));
    await screen.findByRole('button', { name: 'Sign in as wren' });
  });

  it('keeps the button when using the link is rate-limited', async () => {
    opened({ ...returning, 'POST /v1/sign-in-links/redeem': () => refusal(429, 'rate_limited', 'slow down', { 'Retry-After': '30' }) });
    fireEvent.click(await screen.findByRole('button', { name: 'Sign in as wren' }));
    expect(await screen.findByText('Too many tries from your network. Try again in 30 seconds.')).toBeDefined();
    expect(screen.getByRole('button', { name: 'Sign in as wren' })).toBeDefined();
    expect(window.localStorage.getItem(key)).toBeNull();
  });

  it('says who is signed in already, and uses the link only after signing them out', async () => {
    window.localStorage.setItem(key, memberToken);
    const { calls, view } = opened({
      ...returning,
      ...people({ [memberToken]: { who: ada, channels: channelList('general') }, [newToken]: { who: wren, channels: channelList('red') } }),
      'DELETE /v1/credentials/current': () => new Response(null, { status: 204 }),
    });
    expect(await screen.findByText(/You are signed in here as/)).toBeDefined();
    expect(screen.getByText(/You are signed in here as/).textContent).toContain('ada');
    expect(screen.queryByRole('button', { name: 'Sign in as wren' })).toBeNull();
    fireEvent.click(screen.getByRole('button', { name: 'Sign ada out' }));
    fireEvent.click(await screen.findByRole('button', { name: 'Sign in as wren' }));
    await screen.findByRole('navigation', { name: 'Channels' });
    const order = calls.map((c) => `${c.method} ${c.path}`);
    expect(order.indexOf('DELETE /v1/credentials/current')).toBeGreaterThan(-1);
    expect(order.indexOf('DELETE /v1/credentials/current')).toBeLessThan(order.indexOf('POST /v1/sign-in-links/redeem'));
    expect(calls.find((c) => c.path === '/v1/credentials/current')?.auth).toBe(`Bearer ${memberToken}`);
    expect(window.localStorage.getItem(key)).toBe(newToken);
    expectNoCode(view.container);
  });

  it('offers no sign-in over a sign-in that could not be checked', async () => {
    window.localStorage.setItem(key, memberToken);
    let up = false;
    const { calls } = opened({
      ...returning,
      'GET /v1/whoami': (): Response => {
        if (!up) throw new TypeError('Failed to fetch');
        return json(ada);
      },
      'POST /v1/sign-in-links/preview': (): Response => {
        if (!up) throw new TypeError('Failed to fetch');
        return json({ name: 'wren' });
      },
    });
    await screen.findByText('Cannot reach the server. Check your connection and try again.');
    up = true;
    fireEvent.click(screen.getByRole('button', { name: 'Try again' }));
    expect(await screen.findByText(linkHeldLine)).toBeDefined();
    expect(screen.queryByRole('button', { name: 'Sign in as wren' })).toBeNull();
    expect(calls.some((c) => c.path === '/v1/sign-in-links/redeem')).toBe(false);
    expect(window.localStorage.getItem(key)).toBe(memberToken);
  });

  it.each([
    { who: 'someone else', answer: { ...wren, id: 99 } },
    { who: 'an operator', answer: { ...wren, role: 'operator' } },
  ])('revokes and keeps nothing when whoami names $who, not the person the link was for', async ({ answer }) => {
    const { calls } = opened({
      ...returning,
      ...people({ [newToken]: { who: answer, channels: channelList('red') } }),
      'DELETE /v1/credentials/current': () => new Response(null, { status: 204 }),
    });
    fireEvent.click(await screen.findByRole('button', { name: 'Sign in as wren' }));
    expect(await screen.findByText('The server gave an unexpected reply. Try again.')).toBeDefined();
    // Revoked on the server with the very credential the link gave, after
    // whoami named the wrong person, and kept nowhere here.
    const order = calls.map((c) => `${c.method} ${c.path} ${c.auth ?? ''}`);
    const revoke = order.indexOf(`DELETE /v1/credentials/current Bearer ${newToken}`);
    expect(revoke).toBeGreaterThan(order.indexOf(`GET /v1/whoami Bearer ${newToken}`));
    expect(window.localStorage.getItem(key)).toBeNull();
    expect(screen.queryByRole('navigation', { name: 'Channels' })).toBeNull();
  });

  // keepRedeemed is shared: an invite whose whoami names someone else is
  // revoked the same way.
  it('revokes an invite credential too when whoami names someone else', async () => {
    const inviteCode = `conch_inv_${'Q'.repeat(43)}`;
    const calls = serve({
      'POST /v1/invites/preview': () => json({ kind: 'personal', inviter: 'ada', channels: ['red'], voice: false, expires_at }),
      'POST /v1/invites/redeem': () => json(redeemed, 201),
      ...people({ [newToken]: { who: { ...wren, id: 99 }, channels: channelList('red') } }),
      'DELETE /v1/credentials/current': () => new Response(null, { status: 204 }),
    });
    window.location.hash = `#/invite/${inviteCode}`;
    render(<App origin={origin} />);
    fireEvent.change(await screen.findByLabelText('Your name'), { target: { value: 'wren' } });
    fireEvent.click(screen.getByRole('button', { name: 'Join' }));
    await waitFor(() => {
      expect(calls.some((c) => c.method === 'DELETE' && c.path === '/v1/credentials/current' && c.auth === `Bearer ${newToken}`)).toBe(true);
    });
    expect(window.localStorage.getItem(key)).toBeNull();
  });

  it('says so, and keeps nothing, when this browser will not keep the sign-in', async () => {
    const calls = serve({ ...returning, 'DELETE /v1/credentials/current': () => new Response(null, { status: 204 }) });
    vi.spyOn(Storage.prototype, 'setItem').mockImplementation(() => {
      throw new DOMException('blocked', 'SecurityError');
    });
    window.location.hash = `#/signin/${signInCode}`;
    render(<App origin={origin} />);
    fireEvent.click(await screen.findByRole('button', { name: 'Sign in as wren' }));
    expect(await screen.findByText(/This browser will not let the page keep your sign-in/)).toBeDefined();
    // The credential it could not keep was revoked rather than left alive.
    expect(calls.find((c) => c.path === '/v1/credentials/current')?.auth).toBe(`Bearer ${newToken}`);
  });
});

// A member signed in, in the lobby.
function inLobby(routes: Record<string, Route> = {}): Call[] {
  window.localStorage.setItem(key, otherToken);
  const calls = serve({ ...people({ [otherToken]: { who: ada, channels: channelList('general') } }), ...routes });
  render(<App origin={origin} />);
  return calls;
}

describe('the sign-out warning', () => {
  it.each([
    { button: 'Sign out', yes: 'Yes, sign out', question: /^Sign out of this browser\?/, path: '/v1/credentials/current', method: 'DELETE' },
    { button: 'Sign out everywhere', yes: 'Yes, sign out everywhere', question: /^Sign out on every device/, path: '/v1/principals/2/credentials/revoke-all', method: 'POST' },
  ])('$button says first that coming back needs a link from an operator, and revokes only on yes', async ({ button, yes, question, path, method }) => {
    const calls = inLobby({
      'DELETE /v1/credentials/current': () => new Response(null, { status: 204 }),
      'POST /v1/principals/2/credentials/revoke-all': () => json({ revoked: 1 }),
    });
    // Said beside the buttons, in Settings (issue #536), before anything is pressed.
    fireEvent.click(await screen.findByRole('button', { name: 'Settings' }));
    await screen.findByRole('button', { name: button });
    expect(document.getElementById('signout-note')?.textContent).toContain('you will need a new link from an operator to come back into this account');

    fireEvent.click(screen.getByRole('button', { name: button }));
    const asked = await screen.findByRole('group', { name: question });
    expect(asked.textContent).toContain(signOutWarning);
    // Focus is on the answer that keeps the account.
    await waitFor(() => {
      expect(document.activeElement).toBe(screen.getByRole('button', { name: 'Cancel' }));
    });
    expect(calls.some((c) => c.path === path)).toBe(false);

    // Cancel changes nothing.
    fireEvent.click(screen.getByRole('button', { name: 'Cancel' }));
    await screen.findByRole('button', { name: button });
    expect(calls.some((c) => c.path === path)).toBe(false);
    expect(window.localStorage.getItem(key)).toBe(otherToken);

    // Yes does it.
    fireEvent.click(screen.getByRole('button', { name: button }));
    fireEvent.click(await screen.findByRole('button', { name: yes }));
    await screen.findByLabelText('Credential');
    expect(calls.find((c) => c.path === path)?.method).toBe(method);
    expect(window.localStorage.getItem(key)).toBeNull();
  });
});
