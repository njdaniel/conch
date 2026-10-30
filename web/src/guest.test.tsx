// Temporary guests, accounts and signing in again, in the browser (issue
// #713, ADR-009): the front door, Join as guest, the visit's panel, End
// visit, Create an account during a visit (its lost answer included), a
// returning member's password sign-in, an invite's guest and account modes,
// and guest attribution in the conversation. conchd is a stand-in fetch.
import { act, cleanup, fireEvent, render, screen, waitFor, within } from '@testing-library/react';
import { afterEach, beforeEach, describe, expect, it, onTestFinished, vi } from 'vitest';
import { App } from './App';
import { authorOf } from './Conversation';
import { durationWords } from './Guest';
import { displayName, guestName, memberName } from './guestName';
import { httpsRequiredLine } from './Invite';
import { insecureLine, needsHttps } from './transport';
import type { GuestV1 } from './schema/guest';
import type { WhoAmIResponseV1 } from './schema/principal';
import { authReducer, noticeText } from './state/auth';

const origin = 'http://127.0.0.1:8080';
const key = 'conch.web.token';
const at = '2026-10-21T12:00:00.000Z';
const guestToken = 'conch_guest_secret_token';
const memberToken = 'conch_member_secret_token';
const otherToken = 'conch_other_member_token';
const inviteCode = `conch_inv_${'G'.repeat(43)}`;
const password = 'correct horse battery staple';
const handle = 'guest-a7k4m2q9x3h5t8wz';

const visit: GuestV1 = {
  state: 'active',
  origin: 'address',
  handle,
  label: 'Pat',
  tag: 'a7k4',
  started_at: at,
  expires_at: '2099-10-22T12:00:00.000Z',
  idle_expires_at: '2099-10-21T14:00:00.000Z',
  can_convert: true,
};
const guestWho: WhoAmIResponseV1 = { id: 12, kind: 'human', name: handle, role: 'member', identity_class: 'guest', guest: visit };
const memberWho: WhoAmIResponseV1 = { id: 12, kind: 'human', name: 'pat', role: 'member', identity_class: 'member', auth: { scope: 'full', password_set: true, setup_complete: true } };
const otherWho: WhoAmIResponseV1 = { id: 2, kind: 'human', name: 'ada', role: 'member' };
const principal = { id: 12, kind: 'human', name: handle, created_at: at };
const joinedAsGuest = {
  principal,
  guest: visit,
  credential: { id: 30, principal_id: 12, label: 'guest visit', created_at: at },
  token: guestToken,
  channels: [{ id: 1, name: 'lobby' }],
};
const converted = {
  principal: { ...principal, name: 'pat' },
  credential: { id: 31, principal_id: 12, label: 'browser', created_at: at, renews: true },
  token: memberToken,
};
const signedIn = { next: 'signed_in', principal: { ...principal, name: 'pat' }, credential: converted.credential, token: memberToken };
const channelList = { channels: [{ id: 1, name: 'lobby', created_at: at }] };
const frontDoor = (join: object, extra: object = {}) => ({
  server_name: 'conch.example',
  join,
  password_login: true,
  voice: false,
  guest_idle_seconds: 7200,
  guest_absolute_seconds: 86400,
  ...extra,
});

const json = (body: unknown, status = 200, headers: Record<string, string> = {}) => new Response(JSON.stringify(body), { status, headers });
const refusal = (status: number, code: string, message = 'refused', headers: Record<string, string> = {}) => json({ code, message }, status, headers);

interface Call {
  readonly method: string;
  readonly path: string;
  readonly auth: string | null;
  readonly body: unknown;
}

type Route = (call: Call) => Response | Promise<Response>;

// serve stands in for conchd: routes by "METHOD path" (the path without its
// query), every call recorded. A route that throws is no answer.
function serve(routes: Record<string, Route>): Call[] {
  const calls: Call[] = [];
  vi.stubGlobal(
    'fetch',
    vi.fn(async (input: RequestInfo | URL, init?: RequestInit) => {
      const raw = typeof input === 'string' ? input : input instanceof URL ? input.href : input.url;
      const path = new URL(raw, origin).pathname;
      const method = init?.method ?? 'GET';
      const body: unknown = typeof init?.body === 'string' ? JSON.parse(init.body) : undefined;
      const call = { method, path, auth: new Headers(init?.headers).get('Authorization'), body };
      calls.push(call);
      const route = routes[`${method} ${path}`];
      return route === undefined ? json({}, 404) : route(call);
    }),
  );
  return calls;
}

// people answers whoami and the channel list for each token it names.
function people(by: Record<string, object>): Record<string, Route> {
  return {
    'GET /v1/whoami': (c) => {
      const who = by[(c.auth ?? '').replace(/^Bearer /, '')];
      return who === undefined ? json({}, 401) : json(who);
    },
    'GET /v1/channels': (c) => (by[(c.auth ?? '').replace(/^Bearer /, '')] === undefined ? json({}, 401) : json(channelList)),
  };
}

const both = { 'GET /v1/join': () => json(frontDoor({ guest: true, account: true })) };

// guestServer is a server where a guest visit can start from the bare
// address, and the guest's lobby answers.
function guestServer(routes: Record<string, Route> = {}): Record<string, Route> {
  return {
    ...both,
    'POST /v1/join/guest': () => json(joinedAsGuest, 201),
    ...people({ [guestToken]: guestWho }),
    ...routes,
  };
}

async function joinAsGuest(label = 'Pat') {
  fireEvent.click(await screen.findByRole('button', { name: 'Join as guest' }));
  fireEvent.change(screen.getByLabelText('Name to show (optional)'), { target: { value: label } });
  fireEvent.click(screen.getByRole('button', { name: 'Join as guest' }));
  await screen.findByRole('region', { name: 'Guest visit' });
}

function fillAccount(name: string, chosen = password, again = chosen) {
  fireEvent.change(screen.getByLabelText('Your name'), { target: { value: name } });
  fireEvent.change(screen.getByLabelText('Choose a password'), { target: { value: chosen } });
  fireEvent.change(screen.getByLabelText('Type the password again'), { target: { value: again } });
}

function signInWithPassword(name: string, typed = password) {
  fireEvent.change(screen.getByLabelText('Name'), { target: { value: name } });
  fireEvent.change(screen.getByLabelText('Password'), { target: { value: typed } });
  fireEvent.click(screen.getByRole('button', { name: 'Sign in' }));
}

// Nothing of a secret is left in the page or the address.
function expectNoSecret(container: HTMLElement, ...secrets: string[]) {
  for (const secret of secrets) {
    expect(container.innerHTML).not.toContain(secret);
    expect(window.location.href).not.toContain(secret);
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

describe('naming a guest', () => {
  it('reads the stored attribution, never the text of a name', () => {
    expect(guestName({ handle, label: 'Pat', tag: 'a7k4' })).toBe('Guest: Pat · a7k4');
    expect(guestName({ handle, tag: 'a7k4' })).toBe(handle);
    // A member called "Guest: Pat · a7k4" is a member: no attribution, its name.
    expect(memberName({ name: 'Guest: Pat · a7k4' })).toBe('Guest: Pat · a7k4');
    expect(memberName({ name: handle, guest: { handle, label: 'Pat', tag: 'a7k4' } })).toBe('Guest: Pat · a7k4');
  });

  it('names a post by the attribution it was posted under, whoever is in the list now', () => {
    const post = { schema: 'conch.message.v2', id: 1, channel_id: 1, author_id: 12, created_at: at, body: 'hi' } as const;
    const now = new Map([[12, { name: 'pat', kind: 'human' as const }]]);
    // Made during the visit: the guest's, after the guest made an account.
    expect(authorOf({ ...post, guest_author: { handle, label: 'Pat', tag: 'a7k4' } }, now)).toBe('Guest: Pat · a7k4');
    // And after the visit ended, with nobody in the list.
    expect(authorOf({ ...post, guest_author: { handle, tag: 'a7k4' } }, new Map())).toBe(handle);
    // Made as the member: the member's name.
    expect(authorOf(post, now)).toBe('pat');
  });

  it('says a visit bound in words', () => {
    expect(durationWords(7200)).toBe('2 hours');
    expect(durationWords(86400)).toBe('1 day');
    expect(durationWords(1800)).toBe('30 minutes');
  });
});

describe('the front door', () => {
  it.each([
    { join: { guest: true, account: true }, offered: ['Join as guest', 'Create an account'] },
    { join: { guest: true, account: false }, offered: ['Join as guest'] },
    { join: { guest: false, account: true }, offered: ['Create an account'] },
    { join: { guest: false, account: false }, offered: [] },
    { join: { guest: false, account: true, guests_full: true }, offered: ['Create an account'] },
    { join: { guest: false, account: false, paused: true }, offered: [] },
  ])('offers only what the server says the address allows: $join', async ({ join, offered }) => {
    serve({ 'GET /v1/join': () => json(frontDoor(join)) });
    render(<App origin={origin} />);
    const door = await screen.findByRole('region', { name: 'Join' });
    expect(within(door).queryAllByRole('button').map((b) => b.textContent)).toEqual(offered);
    // Sign in is always there, for a returning member.
    expect(screen.getByRole('button', { name: 'Sign in' })).toBeDefined();
  });

  it.each([
    { join: { guest: false, account: false, paused: true }, line: 'Joining is paused on this server for now. If you have an account, sign in below.' },
    { join: { guest: false, account: false, guests_full: true }, line: 'No more guests can join right now. Try again later.' },
    { join: { guest: false, account: true, guests_full: true }, line: 'Guest visits are full right now; you can still create an account.' },
    { join: { guest: false, account: false }, line: 'This server is invite-only. To join, ask someone on it for an invite link.' },
  ])('says honestly why: $line', async ({ join, line }) => {
    serve({ 'GET /v1/join': () => json(frontDoor(join)) });
    render(<App origin={origin} />);
    expect(await screen.findByText(line)).toBeDefined();
  });

  it('offers sign-in alone, as before, when the server has no front door', async () => {
    const calls = serve({});
    render(<App origin={origin} />);
    await waitFor(() => {
      expect(calls.some((c) => c.path === '/v1/join')).toBe(true);
    });
    await act(async () => {});
    expect(screen.queryByRole('region', { name: 'Join' })).toBeNull();
    expect(screen.getByLabelText('Password')).toBeDefined();
    expect(screen.getByLabelText('Credential')).toBeDefined();
  });

  it('asks for no credential to read the front door', async () => {
    const calls = serve(both);
    render(<App origin={origin} />);
    await screen.findByRole('region', { name: 'Join' });
    expect(calls.find((c) => c.path === '/v1/join')).toMatchObject({ method: 'GET', auth: null });
  });

  // Design §5.4: a bare LAN HTTP page explains how to enable TLS, and does
  // not invite a password with a warning.
  it('explains HTTPS on a plain HTTP page beyond this machine, and offers no password or guest form', async () => {
    serve(both);
    render(<App origin="http://192.168.1.20:8080" />);
    expect(await screen.findByText(/This page is not served over HTTPS/)).toBeDefined();
    expect(screen.queryByLabelText('Password')).toBeNull();
    expect(screen.queryByRole('button', { name: 'Join as guest' })).toBeNull();
    expect(screen.queryByRole('button', { name: 'Create an account' })).toBeNull();
    // The operator's credential still works over it, as before.
    expect(screen.getByLabelText('Credential')).toBeDefined();
  });

  it.each([
    ['http://192.168.1.20:8080', true],
    ['http://conch.example', true],
    ['http://127.0.0.1:8080', false],
    ['http://localhost:5173', false],
    ['http://[::1]:8080', false],
    ['https://conch.example', false],
  ])('needs HTTPS for %s: %s', (at, want) => {
    expect(needsHttps(at)).toBe(want);
  });
});

describe('a guest visit', () => {
  it('starts with a label, is kept for this tab alone, and shows who, until when, and what to do', async () => {
    const getUserMedia = vi.fn();
    Object.defineProperty(window.navigator, 'mediaDevices', { value: { getUserMedia }, configurable: true });
    onTestFinished(() => {
      Reflect.deleteProperty(window.navigator, 'mediaDevices');
    });
    const calls = serve(guestServer());
    const { container } = render(<App origin={origin} />);
    await joinAsGuest('Pat');
    expect(calls.find((c) => c.path === '/v1/join/guest')).toMatchObject({ method: 'POST', auth: null, body: { label: 'Pat' } });
    // Tab-scoped: in sessionStorage, never in localStorage.
    expect(window.sessionStorage.getItem(key)).toBe(guestToken);
    expect(window.localStorage.getItem(key)).toBeNull();
    const panel = screen.getByRole('region', { name: 'Guest visit' });
    expect(panel.textContent).toContain('Temporary guest');
    expect(panel.textContent).toContain('Guest: Pat · a7k4');
    expect(panel.textContent).toContain('End visit signs you out permanently. Your messages remain. Create an account to keep access.');
    expect(within(panel).getByText(new Date(visit.expires_at).toLocaleString()).getAttribute('datetime')).toBe(visit.expires_at);
    // It never says closing the browser ends the visit.
    expect(panel.textContent).toContain('Closing the tab does not end it.');
    expect(within(panel).getByRole('button', { name: 'Create an account' })).toBeDefined();
    expect(within(panel).getByRole('button', { name: 'End visit' })).toBeDefined();
    // The lobby names the guest by its attribution.
    expect(screen.getByRole('status', { name: 'Status' }).textContent).toContain('Guest: Pat · a7k4');
    // Joining chat asks for no microphone and no voice session.
    expect(getUserMedia).not.toHaveBeenCalled();
    expect(calls.some((c) => c.path.includes('/voice/'))).toBe(false);
    expectNoSecret(container, guestToken);
  });

  it('sends no label when none is typed', async () => {
    const calls = serve(guestServer());
    render(<App origin={origin} />);
    await joinAsGuest('');
    expect(calls.find((c) => c.path === '/v1/join/guest')?.body).toEqual({});
  });

  it.each([
    { code: 'guests_full', status: 429, line: 'No more guests can join right now. Try again later.' },
    { code: 'guests_disabled', status: 403, line: 'Guest visits are not open here now. Ask someone on this server for a way in.' },
    { code: 'https_required', status: 403, line: httpsRequiredLine },
  ])('says why a visit did not start: $code, and keeps nothing', async ({ code, status, line }) => {
    serve(guestServer({ 'POST /v1/join/guest': () => refusal(status, code) }));
    render(<App origin={origin} />);
    fireEvent.click(await screen.findByRole('button', { name: 'Join as guest' }));
    fireEvent.click(screen.getByRole('button', { name: 'Join as guest' }));
    expect(await screen.findByText(line)).toBeDefined();
    expect(window.sessionStorage.length + window.localStorage.length).toBe(0);
  });

  it('survives a refresh while the visit is live, and only then', async () => {
    serve(guestServer());
    const first = render(<App origin={origin} />);
    await joinAsGuest();
    first.unmount();
    // A reload of the tab: its sessionStorage is still there.
    render(<App origin={origin} />);
    await screen.findByRole('region', { name: 'Guest visit' });
    cleanup();
    // The server has ended the visit since: the refresh signs the tab out
    // and keeps nothing.
    serve({ ...both, ...people({}) });
    render(<App origin={origin} />);
    await screen.findByText(noticeText('expired'));
    expect(window.sessionStorage.getItem(key)).toBeNull();
  });

  it('is not shared with another tab: a new tab of the origin has no visit', async () => {
    serve(guestServer());
    const first = render(<App origin={origin} />);
    await joinAsGuest();
    first.unmount();
    // Another tab starts with its own, empty, sessionStorage.
    window.sessionStorage.clear();
    render(<App origin={origin} />);
    await screen.findByRole('region', { name: 'Join' });
    expect(screen.queryByRole('region', { name: 'Guest visit' })).toBeNull();
  });

  it('moves a guest credential found kept to last into the tab', async () => {
    window.localStorage.setItem(key, guestToken);
    serve(guestServer());
    render(<App origin={origin} />);
    await screen.findByRole('region', { name: 'Guest visit' });
    expect(window.localStorage.getItem(key)).toBeNull();
    expect(window.sessionStorage.getItem(key)).toBe(guestToken);
  });

  it('signs out with the visit’s own words at its hard end, and keeps nothing', async () => {
    let live = true;
    const ending = { ...visit, expires_at: new Date(Date.now() + 100).toISOString(), idle_expires_at: new Date(Date.now() + 100).toISOString() };
    serve(
      guestServer({
        'POST /v1/join/guest': () => json({ ...joinedAsGuest, guest: ending }, 201),
        'GET /v1/whoami': (c) => (live && c.auth === `Bearer ${guestToken}` ? json({ ...guestWho, guest: ending }) : json({}, 401)),
      }),
    );
    render(<App origin={origin} />);
    await joinAsGuest();
    // The server ends the visit at its deadline; the page asks then.
    live = false;
    expect(await screen.findByText(noticeText('visitEnded'), undefined, { timeout: 5_000 })).toBeDefined();
    expect(window.sessionStorage.getItem(key)).toBeNull();
  });

  it('says the visit ended, not that a credential failed, on a 401 mid-visit', () => {
    expect(authReducer({ phase: 'signedIn', principal: guestWho }, { type: 'unauthorized' })).toEqual({ phase: 'signedOut', notice: 'visitEnded' });
    expect(authReducer({ phase: 'signedIn', principal: otherWho }, { type: 'unauthorized' })).toEqual({ phase: 'signedOut', notice: 'expired' });
    expect(displayName(guestWho)).toBe('Guest: Pat · a7k4');
    expect(displayName(otherWho)).toBe('ada');
  });

  it('hides what a guest cannot do: renaming, sign-in lists and sign out everywhere', async () => {
    serve(guestServer());
    render(<App origin={origin} />);
    await joinAsGuest();
    fireEvent.click(screen.getByRole('button', { name: 'Settings' }));
    const settings = await screen.findByRole('dialog', { name: 'Settings' });
    expect(within(settings).queryByRole('button', { name: 'Sign out everywhere' })).toBeNull();
    expect(within(settings).queryByRole('button', { name: 'Change name' })).toBeNull();
    expect(within(settings).queryByRole('region', { name: 'Your sign-ins' })).toBeNull();
    expect(within(settings).getByText(/closing the tab does not end the visit/)).toBeDefined();
  });
});

describe('End visit', () => {
  // Security review of #713, N-4: with no credential left to send, nothing
  // was told to the server, and the page does not claim the visit ended.
  it('says the server was not told when this tab no longer holds the credential', async () => {
    const calls = serve(guestServer());
    render(<App origin={origin} />);
    await joinAsGuest();
    window.sessionStorage.removeItem(key);
    fireEvent.click(screen.getByRole('button', { name: 'End visit' }));
    fireEvent.click(screen.getByRole('button', { name: 'Yes, end visit' }));
    expect(await screen.findByText(noticeText('visitUntold'))).toBeDefined();
    expect(calls.some((c) => c.path === '/v1/guest/end')).toBe(false);
  });

  it('asks first, then ends the visit on the server and forgets it here', async () => {
    const calls = serve(guestServer({ 'POST /v1/guest/end': () => json({ guest: { ...visit, state: 'ended', ended_at: at, end_reason: 'end_visit' } }) }));
    render(<App origin={origin} />);
    await joinAsGuest();
    fireEvent.click(screen.getByRole('button', { name: 'End visit' }));
    const asked = screen.getByRole('group', { name: /^End your visit\?/ });
    await waitFor(() => {
      expect(document.activeElement).toBe(within(asked).getByRole('button', { name: 'Cancel' }));
    });
    // Cancel changes nothing.
    fireEvent.click(within(asked).getByRole('button', { name: 'Cancel' }));
    expect(calls.some((c) => c.path === '/v1/guest/end')).toBe(false);
    fireEvent.click(screen.getByRole('button', { name: 'End visit' }));
    fireEvent.click(screen.getByRole('button', { name: 'Yes, end visit' }));
    expect(await screen.findByText(noticeText('visitEnded'))).toBeDefined();
    expect(calls.find((c) => c.path === '/v1/guest/end')).toMatchObject({ method: 'POST', auth: `Bearer ${guestToken}` });
    expect(window.sessionStorage.getItem(key)).toBeNull();
    expect(window.localStorage.getItem(key)).toBeNull();
  });

  it('keeps the visit when the server does not answer, says so, and tries again', async () => {
    let up = false;
    const calls = serve(
      guestServer({
        'POST /v1/guest/end': (): Response => {
          if (!up) throw new TypeError('Failed to fetch');
          return refusal(409, 'guest_ended', 'the visit has already ended');
        },
      }),
    );
    render(<App origin={origin} />);
    await joinAsGuest();
    fireEvent.click(screen.getByRole('button', { name: 'End visit' }));
    fireEvent.click(screen.getByRole('button', { name: 'Yes, end visit' }));
    expect(await screen.findByText(/The server did not confirm that your visit ended/)).toBeDefined();
    // Not forgotten: clearing storage would prove nothing about the server.
    expect(window.sessionStorage.getItem(key)).toBe(guestToken);
    up = true;
    fireEvent.click(within(screen.getByRole('region', { name: 'Guest visit' })).getByRole('button', { name: 'Try again' }));
    // guest_ended is the server's word that it is over.
    expect(await screen.findByText(noticeText('visitEnded'))).toBeDefined();
    expect(calls.filter((c) => c.path === '/v1/guest/end')).toHaveLength(2);
    expect(window.sessionStorage.getItem(key)).toBeNull();
  });

  it('forgets it in this tab when asked, saying the server was not told', async () => {
    serve(
      guestServer({
        'POST /v1/guest/end': (): Response => {
          throw new TypeError('Failed to fetch');
        },
        'DELETE /v1/credentials/current': (): Response => {
          throw new TypeError('Failed to fetch');
        },
      }),
    );
    render(<App origin={origin} />);
    await joinAsGuest();
    fireEvent.click(screen.getByRole('button', { name: 'End visit' }));
    fireEvent.click(screen.getByRole('button', { name: 'Yes, end visit' }));
    fireEvent.click(await screen.findByRole('button', { name: 'Forget it in this tab' }));
    expect(await screen.findByText(noticeText('visitUntold'))).toBeDefined();
    expect(window.sessionStorage.getItem(key)).toBeNull();
  });
});

describe('Create an account during a visit', () => {
  function converting(answer: Route, extra: Record<string, Route> = {}) {
    return guestServer({
      'POST /v1/guest/convert': answer,
      ...people({ [guestToken]: guestWho, [memberToken]: memberWho }),
      ...extra,
    });
  }

  async function openForm() {
    fireEvent.click(screen.getByRole('button', { name: 'Create an account' }));
    await screen.findByLabelText('Your name');
  }

  it('asks for a permanent name and password, and only after the server says so becomes the member', async () => {
    let answer: (r: Response) => void = () => undefined;
    const pending = new Promise<Response>((resolve) => {
      answer = resolve;
    });
    const calls = serve(converting(() => pending));
    const { container } = render(<App origin={origin} />);
    await joinAsGuest();
    await openForm();
    fillAccount('pat');
    fireEvent.click(screen.getByRole('button', { name: 'Create my account' }));
    // Until the answer, the page is still the guest's.
    await waitFor(() => {
      expect(calls.some((c) => c.path === '/v1/guest/convert')).toBe(true);
    });
    expect(screen.getByRole('region', { name: 'Guest visit' })).toBeDefined();
    expect(window.sessionStorage.getItem(key)).toBe(guestToken);
    answer(json(converted));
    // The page starts over as the member: no guest panel, the member's name.
    await waitFor(() => {
      expect(screen.getByRole('status', { name: 'Status' }).textContent).toContain('pat');
    });
    expect(screen.queryByRole('region', { name: 'Guest visit' })).toBeNull();
    expect(calls.find((c) => c.path === '/v1/guest/convert')).toMatchObject({ auth: `Bearer ${guestToken}`, body: { name: 'pat', password } });
    // The member's credential follows a member's rule, to last; the guest's
    // is gone from the tab.
    expect(window.localStorage.getItem(key)).toBe(memberToken);
    expect(window.sessionStorage.getItem(key)).toBeNull();
    expectNoSecret(container, guestToken, memberToken, password);
  });

  it('sends a short password, as the server decides on the rest (issue #977)', async () => {
    const calls = serve(converting(() => json(converted)));
    render(<App origin={origin} />);
    await joinAsGuest();
    await openForm();
    fillAccount('pat', 'tide', 'tide');
    fireEvent.click(screen.getByRole('button', { name: 'Create my account' }));
    await waitFor(() => {
      expect(calls.find((c) => c.path === '/v1/guest/convert')).toMatchObject({ body: { name: 'pat', password: 'tide' } });
    });
    expect(screen.queryByText(/at least 15/)).toBeNull();
  });

  it('changes nothing on cancel', async () => {
    const calls = serve(converting(() => json(converted)));
    render(<App origin={origin} />);
    await joinAsGuest();
    await openForm();
    fillAccount('pat');
    fireEvent.click(screen.getByRole('button', { name: 'Cancel' }));
    expect(screen.getByRole('button', { name: 'Create an account' })).toBeDefined();
    expect(calls.some((c) => c.path === '/v1/guest/convert')).toBe(false);
    expect(window.sessionStorage.getItem(key)).toBe(guestToken);
  });

  it.each([
    { name: 'two passwords that differ', fill: ['pat', password, `${password}!`], line: 'The two passwords are not the same. Type them again.' },
    { name: 'no password', fill: ['pat', '', ''], line: 'Choose a password.' },
    { name: 'no name', fill: ['', password, password], line: 'Choose a name.' },
  ])('checks $name before sending anything', async ({ fill, line }) => {
    const calls = serve(converting(() => json(converted)));
    render(<App origin={origin} />);
    await joinAsGuest();
    await openForm();
    fillAccount(fill[0] ?? '', fill[1], fill[2]);
    fireEvent.click(screen.getByRole('button', { name: 'Create my account' }));
    expect(await screen.findByText(line)).toBeDefined();
    expect(calls.some((c) => c.path === '/v1/guest/convert')).toBe(false);
  });

  it.each([
    { code: 'principal_exists', status: 409, line: 'That name is taken. Choose another.' },
    { code: 'password_unacceptable', status: 400, line: 'That password is too common, or it contains your name. Choose another.' },
    { code: 'conversion_not_allowed', status: 403, line: 'Accounts cannot be made from this visit now. An operator can allow it.' },
    { code: 'https_required', status: 403, line: httpsRequiredLine },
  ])('keeps the guest as it was when the server refuses: $code', async ({ code, status, line }) => {
    serve(converting(() => refusal(status, code)));
    render(<App origin={origin} />);
    await joinAsGuest();
    await openForm();
    fillAccount('pat');
    fireEvent.click(screen.getByRole('button', { name: 'Create my account' }));
    expect(await screen.findByText(line)).toBeDefined();
    expect(screen.getByRole('region', { name: 'Guest visit' }).textContent).toContain('Guest: Pat · a7k4');
    expect(window.sessionStorage.getItem(key)).toBe(guestToken);
    expect(window.localStorage.getItem(key)).toBeNull();
  });

  // Design §3.3: a lost answer after a conversion that committed is
  // recovered by password sign-in, never by the old guest credential.
  it('recovers from a lost answer by password sign-in with the new name', async () => {
    let convertedNow = false;
    const calls = serve(
      converting(
        (): Response => {
          // The server makes the account, and the answer is lost on the way.
          convertedNow = true;
          throw new TypeError('Failed to fetch');
        },
        {
          'GET /v1/whoami': (c) => {
            if (c.auth === `Bearer ${guestToken}`) return convertedNow ? json({}, 401) : json(guestWho);
            if (c.auth === `Bearer ${memberToken}`) return json(memberWho);
            return json({}, 401);
          },
          'POST /v1/login': (c) => ((c.body as { name: string }).name === 'pat' ? json(signedIn) : refusal(401, 'auth_failed')),
        },
      ),
    );
    render(<App origin={origin} />);
    await joinAsGuest();
    await openForm();
    fillAccount('pat');
    fireEvent.click(screen.getByRole('button', { name: 'Create my account' }));
    expect(await screen.findByText(noticeText('convertGone'))).toBeDefined();
    expect(window.sessionStorage.getItem(key)).toBeNull();
    signInWithPassword('pat');
    await waitFor(() => {
      expect(screen.getByRole('status', { name: 'Status' }).textContent).toContain('pat');
    });
    expect(window.localStorage.getItem(key)).toBe(memberToken);
    // The old guest credential was never offered again after it stopped working.
    const lastGuest = calls.map((c) => c.auth).lastIndexOf(`Bearer ${guestToken}`);
    expect(calls.slice(lastGuest + 1).every((c) => c.auth !== `Bearer ${guestToken}`)).toBe(true);
  });

  it('keeps the guest when the answer is lost and the visit is still on', async () => {
    serve(
      converting((): Response => {
        throw new TypeError('Failed to fetch');
      }),
    );
    render(<App origin={origin} />);
    await joinAsGuest();
    await openForm();
    fillAccount('pat');
    fireEvent.click(screen.getByRole('button', { name: 'Create my account' }));
    expect(await screen.findByText(/The server did not answer about your account. You are still a guest/)).toBeDefined();
    expect(window.sessionStorage.getItem(key)).toBe(guestToken);
  });

  it('keeps the member in this tab when another tab keeps a sign-in to last', async () => {
    serve(converting(() => json(converted)));
    render(<App origin={origin} />);
    await joinAsGuest();
    // Another tab of this browser signs a member in meanwhile.
    window.localStorage.setItem(key, otherToken);
    await openForm();
    fillAccount('pat');
    fireEvent.click(screen.getByRole('button', { name: 'Create my account' }));
    await waitFor(() => {
      expect(window.sessionStorage.getItem(key)).toBe(memberToken);
    });
    expect(window.localStorage.getItem(key)).toBe(otherToken);
  });

  it('is not offered when the visit cannot become an account now', async () => {
    serve(guestServer({ ...people({ [guestToken]: { ...guestWho, guest: { ...visit, can_convert: false } } }) }));
    render(<App origin={origin} />);
    await joinAsGuest();
    expect(screen.queryByRole('button', { name: 'Create an account' })).toBeNull();
    expect(screen.getByText('Accounts cannot be made from this visit now.')).toBeDefined();
  });

  // Design §5.4 (security review of #713, B-1): no page on plain HTTP beyond
  // this machine offers a form that sends a password.
  it('is not offered on a plain HTTP page beyond this machine, which says why', async () => {
    window.sessionStorage.setItem(key, guestToken);
    serve(guestServer());
    render(<App origin="http://192.168.1.20:8080" />);
    const panel = await screen.findByRole('region', { name: 'Guest visit' });
    expect(within(panel).queryByRole('button', { name: 'Create an account' })).toBeNull();
    expect(within(panel).getByText(insecureLine)).toBeDefined();
    expect(document.querySelector('input[type="password"]')).toBeNull();
    // End visit takes no password: it stays.
    expect(within(panel).getByRole('button', { name: 'End visit' })).toBeDefined();
  });

  // Security review of #713, N-1: a success whose body is not the schema's
  // shape may still carry the member's credential; it is not left alive.
  it('revokes the credential of a success it cannot read, then asks about the guest', async () => {
    const stray = 'conch_stray_member_token';
    const calls = serve(
      converting(() => json({ token: stray, principal: 'garbled' }), {
        'GET /v1/whoami': (c) => (c.auth === `Bearer ${guestToken}` && !calls.some((x) => x.path === '/v1/guest/convert') ? json(guestWho) : json({}, 401)),
        'DELETE /v1/credentials/current': () => new Response(null, { status: 204 }),
      }),
    );
    render(<App origin={origin} />);
    await joinAsGuest();
    await openForm();
    fillAccount('pat');
    fireEvent.click(screen.getByRole('button', { name: 'Create my account' }));
    expect(await screen.findByText(noticeText('convertGone'))).toBeDefined();
    expect(calls.find((c) => c.method === 'DELETE' && c.path === '/v1/credentials/current')?.auth).toBe(`Bearer ${stray}`);
    expect(window.localStorage.length + window.sessionStorage.length).toBe(0);
  });

  // N-2: a refusal with no conchd error document (a proxy's) says nothing of
  // whether the account was made: the guest credential is asked about.
  it.each([404, 408])('asks about the guest after a %d with no error document, rather than say the reply was unexpected', async (status) => {
    const calls = serve(converting(() => new Response('<html>proxy</html>', { status })));
    render(<App origin={origin} />);
    await joinAsGuest();
    await openForm();
    fillAccount('pat');
    fireEvent.click(screen.getByRole('button', { name: 'Create my account' }));
    expect(await screen.findByText(/The server did not answer about your account. You are still a guest/)).toBeDefined();
    expect(screen.queryByText(/unexpected reply/)).toBeNull();
    const convertAt = calls.findIndex((c) => c.path === '/v1/guest/convert');
    expect(calls.slice(convertAt + 1).some((c) => c.path === '/v1/whoami' && c.auth === `Bearer ${guestToken}`)).toBe(true);
    expect(window.sessionStorage.getItem(key)).toBe(guestToken);
  });
});

describe('a guest credential this tab cannot keep', () => {
  // N-3: taken out of localStorage, a guest credential that sessionStorage
  // will not take is revoked rather than kept nowhere and left alive.
  it('is revoked when it cannot be moved into the tab', async () => {
    window.localStorage.setItem(key, guestToken);
    const calls = serve(guestServer({ 'DELETE /v1/credentials/current': () => new Response(null, { status: 204 }) }));
    // Nothing can be written: the tab's storage refuses the credential.
    vi.spyOn(Storage.prototype, 'setItem').mockImplementation(() => {
      throw new DOMException('blocked', 'SecurityError');
    });
    render(<App origin={origin} />);
    await screen.findByText(noticeText('expired'));
    expect(calls.find((c) => c.method === 'DELETE' && c.path === '/v1/credentials/current')?.auth).toBe(`Bearer ${guestToken}`);
    expect(window.localStorage.getItem(key)).toBeNull();
    expect(window.sessionStorage.getItem(key)).toBeNull();
  });
});

describe('a returning member', () => {
  it('signs in with a name and a password from a fresh browser, kept to last', async () => {
    const calls = serve({ ...both, 'POST /v1/login': () => json(signedIn), ...people({ [memberToken]: memberWho }) });
    const { container } = render(<App origin={origin} />);
    await screen.findByRole('region', { name: 'Join' });
    signInWithPassword('pat');
    await waitFor(() => {
      expect(screen.getByRole('status', { name: 'Status' }).textContent).toContain('pat');
    });
    expect(calls.find((c) => c.path === '/v1/login')).toMatchObject({ method: 'POST', auth: null, body: { name: 'pat', password } });
    expect(window.localStorage.getItem(key)).toBe(memberToken);
    expect(window.sessionStorage.getItem(key)).toBeNull();
    expect(calls.filter((c) => JSON.stringify(c).includes(password)).map((c) => c.path)).toEqual(['/v1/login']);
    expectNoSecret(container, memberToken, password);
    // Signing out says how to come back: with the password.
    fireEvent.click(screen.getByRole('button', { name: 'Settings' }));
    expect((await screen.findByText(/Either way, you sign in again with your name and password/)).id).toBe('signout-note');
  });

  it.each([
    { name: 'a wrong name or password', answer: () => refusal(401, 'auth_failed'), line: 'The name or password is not right.' },
    { name: 'too many tries', answer: () => refusal(429, 'rate_limited', 'slow down', { 'Retry-After': '90' }), line: 'Too many tries. Try again in 2 minutes.' },
    { name: 'plain HTTP behind a proxy', answer: () => refusal(403, 'https_required'), line: httpsRequiredLine },
    // An operator's account answers with a code's step since issue #714:
    // operatorauth.test.tsx covers it.
  ])('says so for $name, keeps nothing, and puts focus back on the password', async ({ answer, line }) => {
    serve({ ...both, 'POST /v1/login': answer });
    render(<App origin={origin} />);
    await screen.findByRole('region', { name: 'Join' });
    signInWithPassword('pat');
    expect((await screen.findByText(line)).getAttribute('role')).toBe('alert');
    await waitFor(() => {
      expect(document.activeElement).toBe(screen.getByLabelText('Password'));
    });
    expect(screen.getByLabelText<HTMLInputElement>('Password').value).toBe('');
    expect(window.localStorage.length + window.sessionStorage.length).toBe(0);
  });
});

describe('an invite and its join modes', () => {
  const preview = (join: object) => ({ kind: 'open', inviter: 'ada', channels: ['red'], voice: false, expires_at: '2099-10-23T12:00:00Z', join });

  function opened(routes: Record<string, Route>) {
    const calls = serve({ ...both, ...routes });
    window.location.hash = `#/invite/${inviteCode}`;
    const view = render(<App origin={origin} />);
    return { calls, view };
  }

  it('offers both when the invite allows both, and joins as a guest for this tab only', async () => {
    const { calls, view } = opened({
      'POST /v1/invites/preview': () => json(preview({ guest: true, account: true })),
      'POST /v1/invites/redeem-guest': () => json({ ...joinedAsGuest, guest: { ...visit, origin: 'invite' } }, 201),
      ...people({ [guestToken]: { ...guestWho, guest: { ...visit, origin: 'invite' } } }),
    });
    const choice = await screen.findByRole('group', { name: 'How to join' });
    expect(within(choice).getAllByRole('button').map((b) => b.textContent)).toEqual(['Create an account', 'Join as guest']);
    fireEvent.click(within(choice).getByRole('button', { name: 'Join as guest' }));
    fireEvent.change(screen.getByLabelText('Name to show (optional)'), { target: { value: 'Pat' } });
    fireEvent.click(screen.getByRole('button', { name: 'Join as guest' }));
    await screen.findByRole('region', { name: 'Guest visit' });
    expect(calls.find((c) => c.path === '/v1/invites/redeem-guest')).toMatchObject({ auth: null, body: { code: inviteCode, label: 'Pat' } });
    expect(window.sessionStorage.getItem(key)).toBe(guestToken);
    expect(window.localStorage.getItem(key)).toBeNull();
    expectNoSecret(view.container, inviteCode, guestToken);
  });

  it('offers the guest form alone for a guest-only invite', async () => {
    opened({ 'POST /v1/invites/preview': () => json(preview({ guest: true, account: false })) });
    expect(await screen.findByLabelText('Name to show (optional)')).toBeDefined();
    expect(screen.queryByLabelText('Choose a password')).toBeNull();
  });

  it('offers the account form alone for an account-only invite, as invites were', async () => {
    opened({ 'POST /v1/invites/preview': () => json(preview({ guest: false, account: true })) });
    expect(await screen.findByLabelText('Choose a password')).toBeDefined();
    expect(screen.queryByLabelText('Name to show (optional)')).toBeNull();
  });

  it.each([
    { join: { guest: false, account: false, paused: true }, line: 'Joining through this invite is paused for now. Try the link again later.' },
    { join: { guest: false, account: false, guests_full: true }, line: 'No more guests can join right now. Try the link again later.' },
    { join: { guest: false, account: false }, line: 'This invite cannot be used to join right now. Ask ada for another way in.' },
  ])('says honestly why it cannot be used now: $line', async ({ join, line }) => {
    opened({ 'POST /v1/invites/preview': () => json(preview(join)) });
    expect(await screen.findByText(line)).toBeDefined();
    expect(screen.queryByLabelText('Your name')).toBeNull();
    expect(screen.queryByLabelText('Name to show (optional)')).toBeNull();
  });

  it('tells someone signed in that the invite does not add their account to its channels', async () => {
    window.localStorage.setItem(key, otherToken);
    opened({ 'POST /v1/invites/preview': () => json(preview({ guest: false, account: true })), ...people({ [otherToken]: otherWho }) });
    const said = await screen.findByText(/You are signed in here as/);
    expect(said.textContent).toContain('it does not add ada to #red');
    expect(said.textContent).toContain('ask ada to add you');
  });

  it('signs in instead, uses nothing of the invite, and says which of its channels the account is not in', async () => {
    const { calls } = opened({
      'POST /v1/invites/preview': () => json(preview({ guest: false, account: true })),
      'POST /v1/login': () => json({ ...signedIn, token: otherToken, principal: { id: 2, kind: 'human', name: 'ada', created_at: at } }),
      ...people({ [otherToken]: otherWho }),
    });
    fireEvent.click(await screen.findByRole('button', { name: 'Sign in instead' }));
    signInWithPassword('ada');
    const said = await screen.findByText(/Signing in did not use the invite from ada/);
    expect(said.textContent).toContain('this account is not in #red');
    expect(calls.some((c) => c.path.includes('/invites/redeem'))).toBe(false);
    fireEvent.click(screen.getByRole('button', { name: 'Dismiss' }));
    expect(screen.queryByText(/Signing in did not use the invite/)).toBeNull();
  });
});

describe('guest attribution in the conversation', () => {
  const message = (id: number, extra: object) => ({ schema: 'conch.message.v2', id, channel_id: 1, author_id: 12, created_at: at, body: `m${String(id)}`, ...extra });

  it('names guest posts by their stored attribution, apart from the roster and from later member posts', async () => {
    window.localStorage.setItem(key, otherToken);
    serve({
      ...people({ [otherToken]: otherWho }),
      // The guest is no longer in the members list: the visit ended, or it is a member now.
      'GET /v1/channels/lobby/members': () => json({ members: [{ principal_id: 12, created_at: at, name: 'pat', kind: 'human' }] }),
      'GET /v2/channels/lobby/messages': () =>
        json({ messages: [message(2, {}), message(1, { guest_author: { handle, label: 'Pat', tag: 'a7k4' } })] }),
    });
    render(<App origin={origin} />);
    const log = await screen.findByRole('log', { name: 'Messages' });
    await within(log).findByText('m1');
    const items = within(log).getAllByRole('listitem');
    expect(items[0]?.textContent).toContain('Guest: Pat · a7k4');
    // The member's own post is drawn apart, under the member's name.
    expect(items[1]?.textContent).toContain('pat');
    expect(items[1]?.textContent).not.toContain('Guest:');
    expect(items[1]?.classList.contains('cont')).toBe(false);
  });
});
