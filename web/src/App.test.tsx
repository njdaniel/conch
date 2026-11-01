import { act, cleanup, fireEvent, render, screen, waitFor, within } from '@testing-library/react';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { App } from './App';

const origin = 'http://127.0.0.1:8080';
const secret = 'conch_secret_token_value';

const member = { id: 2, kind: 'human', name: 'ada', role: 'member' };
const operator = { id: 1, kind: 'human', name: 'nick', role: 'operator' };
const created_at = '2026-10-14T00:00:00Z';
const channels = {
  channels: [
    { id: 1, name: 'general', created_at },
    { id: 2, name: 'ops', created_at },
    { id: 3, name: 'dev', created_at },
  ],
};

type Handler = (auth: string | null) => Response | Promise<Response>;
const json = (body: unknown, status = 200) => new Response(JSON.stringify(body), { status });

// server stands in for conchd: whoami and channels, each answering by token.
function serve(routes: Record<string, Handler>) {
  const fetchMock = vi.fn((input: RequestInfo | URL, init?: RequestInit) => {
    const path = typeof input === 'string' ? input : input instanceof URL ? input.pathname : input.url;
    const headers = new Headers(init?.headers);
    const handler = routes[path];
    return Promise.resolve(handler ? handler(headers.get('Authorization')) : json({}, 404));
  });
  vi.stubGlobal('fetch', fetchMock);
  return fetchMock;
}

const ticketValue = 'conch_wst_TICKETTICKETTICKETTICKETTICKETTICKETTICKET';

// A WebSocket that connects nowhere: the test opens it and sends its frames.
class FakeSocket {
  onopen: (() => void) | null = null;
  onmessage: ((event: { data: unknown }) => void) | null = null;
  onclose: ((event: { code: number }) => void) | null = null;
  constructor(
    readonly url: string,
    private readonly all: FakeSocket[],
  ) {
    all.push(this);
  }
  close() {
    this.all.splice(this.all.indexOf(this), 1);
  }
}

// socketsOf stands in for WebSocket and lists the sockets the page holds open.
function socketsOf(): FakeSocket[] {
  const all: FakeSocket[] = [];
  vi.stubGlobal(
    'WebSocket',
    class extends FakeSocket {
      constructor(url: string) {
        super(url, all);
      }
    },
  );
  return all;
}

const healthz = (version: string): Handler => () => json({ status: 'ok', version, db: 'ok', voice: 'not_configured' });

const asMember: Record<string, Handler> = {
  '/v1/whoami': (a) => (a === `Bearer ${secret}` ? json(member) : json({}, 401)),
  '/v1/channels': (a) => (a === `Bearer ${secret}` ? json(channels) : json({}, 401)),
};

// The roles a person can act on.
const controlRoles = ['button', 'link', 'checkbox', 'radio', 'switch', 'textbox', 'combobox', 'menuitem', 'tab', 'option', 'slider'] as const;

// controlsOutside lists the accessible names, matching name, of every control
// on the page that is not inside region.
function controlsOutside(region: HTMLElement, name: RegExp): string[] {
  return controlRoles.flatMap((role) =>
    screen
      .queryAllByRole(role, { name })
      .filter((el) => !region.contains(el))
      .map((el) => `${role}: ${el.getAttribute('aria-label') ?? el.textContent}`),
  );
}

function signIn(token = secret) {
  const field = screen.getByLabelText('Credential');
  fireEvent.change(field, { target: { value: token } });
  fireEvent.click(screen.getByRole('button', { name: 'Use credential' }));
}

beforeEach(() => {
  window.sessionStorage.clear();
  window.localStorage.clear();
  window.location.hash = '';
});
afterEach(() => {
  cleanup();
  vi.unstubAllGlobals();
});

describe('sign-in', () => {
  it('says Conch and names the server it was served from', () => {
    serve(asMember);
    render(<App origin={origin} />);
    expect(screen.getByRole('heading', { level: 1 }).textContent).toBe('Conch');
    expect(screen.getByText(origin)).toBeDefined();
  });

  // Issue #1084: a page with no stored credential has nothing to ask whoami,
  // and a refused one is a request known to fail, which the browser logs as
  // an error. A stored credential is checked, once.
  it('asks nothing of whoami when no credential is stored', async () => {
    const fetchMock = serve({ ...asMember, '/healthz': healthz('v0.1.0-alpha.1') });
    render(<App origin={origin} />);
    expect(screen.getByLabelText('Credential')).toBeDefined();
    await screen.findByText('v0.1.0-alpha.1');
    await new Promise((r) => setTimeout(r, 20));
    expect(fetchMock.mock.calls.filter(([input]) => input === '/v1/whoami')).toEqual([]);
  });

  it('asks whoami once for a stored credential that is refused, and shows the sign-in page', async () => {
    window.localStorage.setItem('conch.web.token', 'conch_revoked_token_value');
    const fetchMock = serve(asMember);
    render(<App origin={origin} />);
    expect(await screen.findByLabelText('Credential')).toBeDefined();
    await new Promise((r) => setTimeout(r, 20));
    expect(fetchMock.mock.calls.filter(([input]) => input === '/v1/whoami')).toHaveLength(1);
  });

  it('shows the server version from /healthz on the sign-in page and in the lobby', async () => {
    serve({ ...asMember, '/healthz': healthz('v0.1.0-alpha.1') });
    render(<App origin={origin} />);
    expect(await screen.findByText('v0.1.0-alpha.1')).toBeDefined();
    signIn();
    // In the lobby, the version is in Settings (issue #536).
    fireEvent.click(await screen.findByRole('button', { name: 'Settings' }));
    expect(await screen.findByText('v0.1.0-alpha.1')).toBeDefined();
  });

  it.each([
    ['does not answer', (): Response => { throw new TypeError('Failed to fetch'); }],
    ['answers with something that is not health', (): Response => json({ status: 'ok' })],
  ])('shows no version when /healthz %s, and sign-in still works', async (_case, handler) => {
    const fetchMock = serve({ ...asMember, '/healthz': handler });
    render(<App origin={origin} />);
    signIn();
    fireEvent.click(await screen.findByRole('button', { name: 'Settings' }));
    await screen.findByRole('button', { name: 'Sign out' });
    // Both pages have asked by now; the lobby's request has settled once
    // its settings have rendered, with or without a version.
    await waitFor(() => {
      expect(fetchMock.mock.calls.filter(([input]) => input === '/healthz').length).toBeGreaterThanOrEqual(2);
    });
    await new Promise((r) => setTimeout(r, 0));
    expect(screen.queryByText(/Version/)).toBeNull();
  });

  // Issue #713 (design §3.2): a returning member signs in with a name and a
  // password; the credential field stays, as the advanced way in for
  // operators and scripts, and no password manager is offered it.
  it('signs in with a name and a password, and keeps the credential field apart for operators and scripts', () => {
    serve(asMember);
    render(<App origin={origin} />);
    expect(screen.getByLabelText('Name').getAttribute('autocomplete')).toBe('username');
    const password = screen.getByLabelText('Password');
    expect(password.getAttribute('type')).toBe('password');
    expect(password.getAttribute('autocomplete')).toBe('current-password');
    const field = screen.getByLabelText('Credential');
    expect(field.getAttribute('type')).toBe('password');
    expect(field.getAttribute('autocomplete')).toBe('off');
    expect(within(screen.getByRole('region', { name: 'Operators and scripts' })).getByText(/Paste the credential an operator issued/)).toBeDefined();
    // The one text field is the name: nothing else asks for a username.
    expect(screen.queryAllByRole('textbox')).toHaveLength(1);
    expect(screen.getByRole('button', { name: 'Sign in' }).closest('section')?.getAttribute('aria-labelledby')).toBe('password-heading');
  });

  it('signs a member in to the lobby, stores the token to last (issue #439) and keeps it out of the DOM', async () => {
    serve(asMember);
    const { container } = render(<App origin={origin} />);
    signIn();
    await screen.findByRole('navigation', { name: 'Channels' });
    expect(window.localStorage.getItem('conch.web.token')).toBe(secret);
    expect(window.sessionStorage.getItem('conch.web.token')).toBeNull();
    expect(container.innerHTML).not.toContain(secret);
    expect(window.location.href).not.toContain(secret);
  });

  it("is still signed in when the page is opened again, as after closing the browser", async () => {
    serve(asMember);
    const first = render(<App origin={origin} />);
    signIn();
    await screen.findByRole('navigation', { name: 'Channels' });
    first.unmount();
    // A new page: nothing of the first is left but what it stored.
    render(<App origin={origin} />);
    await screen.findByRole('navigation', { name: 'Channels' });
    expect(screen.getByRole('status', { name: 'Status' }).textContent).toContain('ada');
  });

  it('shows one line for a refused credential, keeps focus in the field and stores nothing', async () => {
    serve(asMember);
    const { container } = render(<App origin={origin} />);
    signIn('wrong-token');
    const alert = await screen.findByText('That credential was not accepted');
    expect(alert.getAttribute('role')).toBe('alert');
    await waitFor(() => {
      expect(document.activeElement).toBe(screen.getByLabelText('Credential'));
    });
    expect(window.sessionStorage.length).toBe(0);
    expect(container.innerHTML).not.toContain('wrong-token');
  });

  it('says so when the server cannot be reached', async () => {
    vi.stubGlobal('fetch', vi.fn(() => Promise.reject(new TypeError('failed'))));
    render(<App origin={origin} />);
    signIn();
    await screen.findByText('Cannot reach the server');
    expect(window.sessionStorage.length).toBe(0);
  });

  // Issue #469: a stored sign-in the page could not check (no answer at
  // load) is not overwritten by signing in; the page offers to forget it,
  // revoking it if the server answers, and then signing in works.
  it('will not sign in over a stored sign-in it could not check, and offers to forget it', async () => {
    window.sessionStorage.setItem('conch.web.token', 'conch_stored_operator');
    vi.stubGlobal('fetch', vi.fn(() => Promise.reject(new TypeError('failed'))));
    render(<App origin={origin} />);
    await screen.findByText('Cannot reach the server');
    let revoked: string | null = null;
    serve({
      ...asMember,
      '/v1/credentials/current': (a) => {
        revoked = a;
        return new Response(null, { status: 204 });
      },
    });
    signIn();
    await screen.findByText(/holds another sign-in that could not be checked/);
    expect(window.sessionStorage.getItem('conch.web.token')).toBe('conch_stored_operator');
    expect(window.localStorage.length).toBe(0);
    fireEvent.click(screen.getByRole('button', { name: 'Forget the stored sign-in' }));
    await waitFor(() => {
      expect(revoked).toBe('Bearer conch_stored_operator');
    });
    await waitFor(() => {
      expect(window.sessionStorage.length).toBe(0);
    });
    expect(screen.queryByRole('button', { name: 'Forget the stored sign-in' })).toBeNull();
    signIn();
    await screen.findByRole('navigation', { name: 'Channels' });
    expect(window.localStorage.getItem('conch.web.token')).toBe(secret);
  });

  // #485 review: a credential another tab stored after this page loaded is
  // checked as on load, not offered for forgetting.
  it('checks a sign-in another tab made since this page loaded, rather than signing in over it', async () => {
    serve(asMember);
    render(<App origin={origin} />);
    window.localStorage.setItem('conch.web.token', secret);
    signIn('conch_someone_else');
    await screen.findByRole('navigation', { name: 'Channels' });
    expect(screen.queryByRole('button', { name: 'Forget the stored sign-in' })).toBeNull();
    expect(window.localStorage.getItem('conch.web.token')).toBe(secret);
  });

  it('warns before continuing as an operator and stores nothing until told to', async () => {
    serve({ ...asMember, '/v1/whoami': () => json(operator) });
    render(<App origin={origin} />);
    signIn();
    await screen.findByRole('heading', { name: 'This is an operator credential' });
    expect(window.sessionStorage.length).toBe(0);
    fireEvent.click(screen.getByRole('button', { name: 'Continue anyway' }));
    await screen.findByRole('navigation', { name: 'Channels' });
    expect(window.sessionStorage.getItem('conch.web.token')).toBe(secret);
  });

  it('signs out from the operator warning without ever storing the credential', async () => {
    serve({ ...asMember, '/v1/whoami': () => json(operator) });
    render(<App origin={origin} />);
    signIn();
    await screen.findByRole('heading', { name: 'This is an operator credential' });
    fireEvent.click(screen.getByRole('button', { name: 'Sign out' }));
    await screen.findByLabelText('Credential');
    expect(window.sessionStorage.length).toBe(0);
  });

  it('warns again, and keeps nothing to last, when an operator credential is found in the lasting place', async () => {
    serve({ ...asMember, '/v1/whoami': () => json(operator) });
    window.localStorage.setItem('conch.web.token', secret);
    render(<App origin={origin} />);
    await screen.findByRole('heading', { name: 'This is an operator credential' });
    expect(window.localStorage.getItem('conch.web.token')).toBeNull();
    fireEvent.click(screen.getByRole('button', { name: 'Continue anyway' }));
    await screen.findByRole('navigation', { name: 'Channels' });
    expect(window.sessionStorage.getItem('conch.web.token')).toBe(secret);
    expect(window.localStorage.getItem('conch.web.token')).toBeNull();
  });

  it('picks up a stored credential on reload', async () => {
    serve(asMember);
    window.sessionStorage.setItem('conch.web.token', secret);
    render(<App origin={origin} />);
    await screen.findByRole('navigation', { name: 'Channels' });
  });

  it('returns to sign-in with the expiry line when a stored credential no longer works', async () => {
    serve(asMember);
    window.sessionStorage.setItem('conch.web.token', 'old');
    render(<App origin={origin} />);
    await screen.findByText('Signed out: the credential is no longer valid');
    expect(window.sessionStorage.getItem('conch.web.token')).toBeNull();
  });
});

describe('lobby', () => {
  it('lists the channels, opens the first, and shows the status line', async () => {
    serve(asMember);
    render(<App origin={origin} />);
    signIn();
    await screen.findByRole('button', { name: 'general' });
    const list = screen.getByRole('navigation', { name: 'Channels' });
    expect(list.querySelectorAll('button')).toHaveLength(3);
    expect(screen.getByRole('button', { name: 'general' }).getAttribute('aria-current')).toBe('true');
    expect(within(screen.getByRole('region', { name: 'Conversation' })).getByRole('heading', { level: 2 }).textContent).toBe('general');
    const status = screen.getByRole('status', { name: 'Status' });
    expect(status.textContent).toContain('ada');
    expect(status.textContent).toContain(origin);
    expect(status.textContent).toContain('Connected');
    // Nothing outside the voice section joins voice or offers to (issue
    // #596, narrowing #536's "nothing outside it mentions voice"): no control
    // anywhere else is named for joining or for voice. Here conchd has no
    // voice route, so it is not joinable, and it joins nothing by itself.
    const voice = screen.getByRole('region', { name: 'Voice' });
    expect(controlsOutside(voice, /join|voice/i)).toEqual([]);
    // The check sees the strip's own (Join voice, Watch only) when the strip
    // is not left out, so the empty list above means something.
    expect(controlsOutside(document.createElement('div'), /join|voice/i).length).toBeGreaterThan(0);
    expect(voice.textContent).toMatch(/^Not in voice/);
    expect(within(voice).getByRole('button', { name: 'Join voice in #general' }).hasAttribute('disabled')).toBe(true);
  });

  it('shows in the list how many are in each channel\'s voice, from one socket, and only the strip offers to join (issue #596)', async () => {
    const sockets = socketsOf();
    const fetchMock = serve({ ...asMember, '/v1/ws-tickets': () => json({ ticket: ticketValue, expires_at: created_at }, 201) });
    render(<App origin={origin} />);
    signIn();
    const voice = await screen.findByRole('region', { name: 'Voice' });
    const occupancy = await waitFor(() => {
      const found = sockets.filter((s) => new URL(s.url).pathname === '/v1/voice/occupancy/ws');
      expect(found).toHaveLength(1);
      return found[0] as FakeSocket;
    });
    // The viewer's, not a channel's: it names none.
    expect(new URL(occupancy.url).searchParams.get('channel')).toBeNull();
    act(() => {
      occupancy.onopen?.();
      occupancy.onmessage?.({
        data: JSON.stringify({
          schema: 'conch.voice_occupancy.v1',
          configured: true,
          available: true,
          channels: [
            { channel_id: 1, in_voice: 1 },
            { channel_id: 2, in_voice: 3 },
            { channel_id: 3, in_voice: 0 },
          ],
        }),
      });
    });
    const list = screen.getByRole('navigation', { name: 'Channels' });
    // The marks are text in the list, beside the buttons, which keep their names.
    expect(await within(list).findByRole('img', { name: '3 in voice' })).toBeDefined();
    expect(within(list).getByRole('img', { name: '1 in voice' })).toBeDefined();
    expect(within(list).queryAllByRole('img', { name: /in voice/ })).toHaveLength(2);
    expect(list.textContent).toContain('3');
    expect(screen.getByRole('button', { name: 'ops' }).textContent).toBe('ops');
    // Still nothing outside the strip offers to join, or names voice.
    expect(controlsOutside(voice, /join|voice/i)).toEqual([]);

    // The strip, not in voice, says where the others are, and opens it.
    expect(voice.querySelector('.voice-elsewhere')?.textContent).toBe('3 in voice in #ops Open #ops');
    fireEvent.click(within(voice).getByRole('button', { name: 'Open #ops' }));
    await waitFor(() => {
      expect(screen.getByRole('button', { name: 'ops' }).getAttribute('aria-current')).toBe('true');
    });
    // Opening is all it did: no voice session was asked for, and the strip
    // still offers Join voice, now for the channel on screen.
    expect(within(voice).getByRole('button', { name: 'Join voice in #ops' })).toBeDefined();
    expect(voice.querySelector('.voice-elsewhere')?.textContent).toBe('1 in voice in #general Open #general');
    const asked = fetchMock.mock.calls.map(([input]) => (typeof input === 'string' ? input : input instanceof URL ? input.pathname : input.url));
    expect(asked.filter((p) => p.includes('/voice/session'))).toEqual([]);
    // The one occupancy socket is the page's, whatever channel is open.
    expect(sockets.filter((s) => new URL(s.url).pathname === '/v1/voice/occupancy/ws')).toEqual([occupancy]);
  });

  it('says voice is not set up when the server has no LiveKit, and offers no join', async () => {
    const presence = { schema: 'conch.voice_presence.v1', channel_id: 1, configured: false, available: false, rooms: [] };
    const sockets = socketsOf();
    serve({ ...asMember, '/v1/ws-tickets': () => json({ ticket: ticketValue, expires_at: created_at }, 201) });
    render(<App origin={origin} />);
    signIn();
    const voice = await screen.findByRole('region', { name: 'Voice' });
    // The presence socket says so, as conchd without LiveKit does on connect.
    const socket = await waitFor(() => {
      const found = sockets.find((s) => new URL(s.url).pathname === '/v1/voice/ws');
      expect(found).toBeDefined();
      return found as FakeSocket;
    });
    expect(new URL(socket.url).searchParams.get('channel')).toBe('general');
    act(() => {
      socket.onopen?.();
      socket.onmessage?.({ data: JSON.stringify(presence) });
    });
    await waitFor(() => {
      expect(voice.textContent).toContain('Voice is not set up on this server');
    });
    expect(within(voice).queryByRole('button')).toBeNull();
  });

  it.each([
    ['a person whose access has no voice', { ...member, voice_allowed: false }],
    ['an agent', { id: 9, kind: 'agent', name: 'bot', role: 'member' }],
  ])('asks nothing of voice for %s', async (_who, principal) => {
    const sockets = socketsOf();
    const fetchMock = serve({
      '/v1/whoami': () => json(principal),
      '/v1/channels': () => json(channels),
      '/v1/ws-tickets': () => json({ ticket: ticketValue, expires_at: created_at }, 201),
    });
    render(<App origin={origin} />);
    signIn();
    const voice = await screen.findByRole('region', { name: 'Voice' });
    await waitFor(() => {
      expect(voice.textContent).toContain('Your account does not include voice. Ask whoever invited you.');
    });
    expect(within(voice).queryByRole('button')).toBeNull();
    // No voice route is asked for, by fetch or by socket, then or later.
    await act(async () => {
      await new Promise((r) => setTimeout(r, 50));
    });
    const asked = fetchMock.mock.calls.map(([input]) => (typeof input === 'string' ? input : input instanceof URL ? input.pathname : input.url));
    expect(asked.filter((p) => p.includes('/voice'))).toEqual([]);
    expect(sockets.filter((s) => new URL(s.url).pathname.includes('voice'))).toEqual([]);
  });

  it('shows Connecting while the channels load', async () => {
    serve({ ...asMember, '/v1/channels': () => new Promise<Response>(() => undefined) });
    render(<App origin={origin} />);
    signIn();
    await screen.findByText('Loading channels…');
    // The status follows in the effect after the first render; waited for, not raced.
    await waitFor(() => {
      expect(screen.getByRole('status', { name: 'Status' }).textContent).toContain('Connecting');
    });
  });

  it('opens the channel named by the hash', async () => {
    serve(asMember);
    window.location.hash = '#/c/ops';
    render(<App origin={origin} />);
    signIn();
    await screen.findByRole('button', { name: 'general' });
    expect(screen.getByRole('button', { name: 'ops' }).getAttribute('aria-current')).toBe('true');
  });

  it('opens the channel last open in this tab when the hash names none', async () => {
    serve(asMember);
    window.sessionStorage.setItem('conch.web.lastChannel', 'dev');
    render(<App origin={origin} />);
    signIn();
    await screen.findByRole('button', { name: 'dev' });
    expect(screen.getByRole('button', { name: 'dev' }).getAttribute('aria-current')).toBe('true');
  });

  it('says what to do when there are no channels', async () => {
    serve({ ...asMember, '/v1/channels': () => json({ channels: [] }) });
    render(<App origin={origin} />);
    signIn();
    await screen.findByText('No channels yet: an operator needs to add you to one');
    expect(screen.getByRole('region', { name: 'Conversation' }).textContent).toBe('');
  });

  it('has a collapsible Members panel, empty for now', async () => {
    serve(asMember);
    render(<App origin={origin} />);
    signIn();
    const toggle = await screen.findByRole('button', { name: 'Members' });
    expect(toggle.getAttribute('aria-expanded')).toBe('true');
    fireEvent.click(toggle);
    expect(toggle.getAttribute('aria-expanded')).toBe('false');
    expect(screen.queryByRole('region', { name: 'Members list' })).toBeNull();
  });

  it('moves with the arrow keys and opens with Enter', async () => {
    serve(asMember);
    render(<App origin={origin} />);
    signIn();
    const general = await screen.findByRole('button', { name: 'general' });
    general.focus();
    fireEvent.keyDown(general, { key: 'ArrowDown' });
    expect(document.activeElement).toBe(screen.getByRole('button', { name: 'ops' }));
    fireEvent.keyDown(document.activeElement as Element, { key: 'End' });
    expect(document.activeElement).toBe(screen.getByRole('button', { name: 'dev' }));
    fireEvent.keyDown(document.activeElement as Element, { key: 'ArrowDown' });
    expect(document.activeElement).toBe(screen.getByRole('button', { name: 'dev' }));
    fireEvent.keyDown(document.activeElement as Element, { key: 'ArrowUp' });
    expect(document.activeElement).toBe(screen.getByRole('button', { name: 'ops' }));
    // A button's Enter is a click.
    fireEvent.click(screen.getByRole('button', { name: 'ops' }));
    expect(within(screen.getByRole('region', { name: 'Conversation' })).getByRole('heading', { level: 2 }).textContent).toBe('ops');
    expect(window.location.hash).toBe('#/c/ops');
  });

  it('keeps one channel button in the tab order', async () => {
    serve(asMember);
    render(<App origin={origin} />);
    signIn();
    await screen.findByRole('button', { name: 'general' });
    const stops = screen.getAllByRole('button').filter((b) => b.tabIndex === 0 && b.closest('.channel-list'));
    expect(stops).toHaveLength(1);
  });

  it('gives every button and region an accessible name', async () => {
    serve(asMember);
    render(<App origin={origin} />);
    signIn();
    await screen.findByRole('button', { name: 'general' });
    for (const button of screen.getAllByRole('button')) {
      expect(button.textContent.trim()).not.toBe('');
    }
    expect(screen.getByRole('complementary', { name: 'Members panel' })).toBeDefined();
  });

  it('signs out by revoking the credential on the server, and returns to sign-in with it forgotten (issue #439)', async () => {
    const fetchMock = serve({ ...asMember, '/v1/credentials/current': (a) => new Response(null, { status: a === `Bearer ${secret}` ? 204 : 401 }) });
    render(<App origin={origin} />);
    signIn();
    fireEvent.click(await screen.findByRole('button', { name: 'Settings' }));
    const out = await screen.findByRole('button', { name: 'Sign out' });
    expect(out.getAttribute('aria-describedby')).toBe('signout-note');
    expect(document.getElementById('signout-note')?.textContent).toContain('Sign out ends this sign-in');
    fireEvent.click(out);
    // Sign out asks first (issue #493).
    fireEvent.click(await screen.findByRole('button', { name: 'Yes, sign out' }));
    await screen.findByLabelText('Credential');
    const revoke = fetchMock.mock.calls.find(([input]) => input === '/v1/credentials/current');
    expect(revoke?.[1]?.method).toBe('DELETE');
    expect(screen.getByRole('alert').textContent).toBe('');
    expect(window.localStorage.getItem('conch.web.token')).toBeNull();
    expect(window.sessionStorage.getItem('conch.web.token')).toBeNull();
  });

  // Issue #645: revoking the credential ends its sessions, so a session end
  // sent alongside it can arrive second and be answered 404. Sign out waits
  // for the session's end to be answered before it revokes.
  it('ends its own session, and waits for the answer, before it revokes the credential', async () => {
    const order: string[] = [];
    let answerEnd: () => void = () => undefined;
    const endAnswered = new Promise<void>((resolve) => {
      answerEnd = resolve;
    });
    const fetchMock = vi.fn((input: RequestInfo | URL, init?: RequestInit) => {
      const path = typeof input === 'string' ? input : input instanceof URL ? input.pathname : input.url;
      const method = init?.method ?? 'GET';
      if (path === '/v1/sessions' && method === 'POST') {
        return Promise.resolve(
          json(
            {
              schema: 'conch.session.v1',
              id: 9,
              principal_id: 2,
              kind: 'human',
              name: 'ada',
              client: 'browser',
              state: 'online',
              status: 'online',
              started_at: created_at,
              last_seen_at: created_at,
              interventions: [],
              outcome_unknown: false,
            },
            201,
          ),
        );
      }
      if (path === '/v1/sessions/9' && method === 'DELETE') {
        order.push('end session');
        return endAnswered.then(() => {
          order.push('session end answered');
          return new Response(null, { status: 204 });
        });
      }
      if (path === '/v1/credentials/current' && method === 'DELETE') {
        order.push('revoke');
        return Promise.resolve(new Response(null, { status: 204 }));
      }
      const handler = asMember[path];
      return Promise.resolve(handler ? handler(new Headers(init?.headers).get('Authorization')) : json({}, 404));
    });
    vi.stubGlobal('fetch', fetchMock);
    render(<App origin={origin} />);
    signIn();
    fireEvent.click(await screen.findByRole('button', { name: 'Settings' }));
    fireEvent.click(await screen.findByRole('button', { name: 'Sign out' }));
    await waitFor(() => {
      expect(fetchMock.mock.calls.some(([input, init]) => input === '/v1/sessions' && init?.method === 'POST')).toBe(true);
    });
    await act(async () => {});
    fireEvent.click(await screen.findByRole('button', { name: 'Yes, sign out' }));
    await waitFor(() => {
      expect(order).toEqual(['end session']);
    });
    // Held: the revoke must not have gone out while the end is unanswered.
    await act(async () => {
      await new Promise((resolve) => setTimeout(resolve, 20));
    });
    expect(order).toEqual(['end session']);
    answerEnd();
    await screen.findByLabelText('Credential');
    expect(order).toEqual(['end session', 'session end answered', 'revoke']);
  });

  it('signs out here even when the server cannot be told, and says so', async () => {
    serve({
      ...asMember,
      '/v1/credentials/current': () => {
        throw new TypeError('Failed to fetch');
      },
    });
    render(<App origin={origin} />);
    signIn();
    fireEvent.click(await screen.findByRole('button', { name: 'Settings' }));
    fireEvent.click(await screen.findByRole('button', { name: 'Sign out' }));
    // Sign out asks first (issue #493).
    fireEvent.click(await screen.findByRole('button', { name: 'Yes, sign out' }));
    await screen.findByText(/^Signed out in this browser, but the server could not be told/);
    expect(window.localStorage.getItem('conch.web.token')).toBeNull();
  });

  it('signs out everywhere: every credential revoked, then back to sign-in', async () => {
    const fetchMock = serve({ ...asMember, '/v1/principals/2/credentials/revoke-all': () => json({ revoked: 2 }) });
    render(<App origin={origin} />);
    signIn();
    fireEvent.click(await screen.findByRole('button', { name: 'Settings' }));
    fireEvent.click(await screen.findByRole('button', { name: 'Sign out everywhere' }));
    // Sign out everywhere asks first (issue #493).
    fireEvent.click(await screen.findByRole('button', { name: 'Yes, sign out everywhere' }));
    await screen.findByText('Signed out everywhere: every sign-in of yours has ended');
    const call = fetchMock.mock.calls.find(([input]) => input === '/v1/principals/2/credentials/revoke-all');
    expect(call?.[1]?.method).toBe('POST');
    expect(window.localStorage.getItem('conch.web.token')).toBeNull();
  });

  it('signs out everywhere: no draft of the person stays readable in the tab (issue #1039)', async () => {
    serve({ ...asMember, '/v1/principals/2/credentials/revoke-all': () => json({ revoked: 2 }) });
    window.sessionStorage.setItem('conch.web.drafts', JSON.stringify({ principal: 2, drafts: { '7': 'a private draft' }, edits: {} }));
    render(<App origin={origin} />);
    signIn();
    fireEvent.click(await screen.findByRole('button', { name: 'Settings' }));
    fireEvent.click(await screen.findByRole('button', { name: 'Sign out everywhere' }));
    fireEvent.click(await screen.findByRole('button', { name: 'Yes, sign out everywhere' }));
    await screen.findByText('Signed out everywhere: every sign-in of yours has ended');
    expect(JSON.stringify(Object.entries(window.sessionStorage))).not.toContain('a private draft');
    expect(window.sessionStorage.getItem('conch.web.drafts')).toBeNull();
  });

  it.each([
    ['cannot be reached', (): Response => { throw new TypeError('Failed to fetch'); }, /the server cannot be reached/],
    ['refuses the last operator', () => json({ code: 'last_operator', message: 'no' }, 409), /the only operator who can sign in/],
  ])('stays signed in, and says so, when sign out everywhere %s', async (_case, handler, line) => {
    serve({ ...asMember, '/v1/principals/2/credentials/revoke-all': handler });
    render(<App origin={origin} />);
    signIn();
    fireEvent.click(await screen.findByRole('button', { name: 'Settings' }));
    fireEvent.click(await screen.findByRole('button', { name: 'Sign out everywhere' }));
    // Sign out everywhere asks first (issue #493).
    fireEvent.click(await screen.findByRole('button', { name: 'Yes, sign out everywhere' }));
    const alert = await screen.findByText(line);
    expect(alert.getAttribute('role')).toBe('alert');
    expect(screen.getByRole('navigation', { name: 'Channels' })).toBeDefined();
    expect(window.localStorage.getItem('conch.web.token')).toBe(secret);
  });

  it('is signed out when another tab signs the shared credential out', async () => {
    serve(asMember);
    render(<App origin={origin} />);
    signIn();
    await screen.findByRole('navigation', { name: 'Channels' });
    // The listener is attached by an effect after the lobby renders; on a
    // busy machine the lobby can be on screen first (#465). Let it attach.
    await act(async () => {});
    // What the browser does in this tab when another one signs out.
    act(() => {
      window.localStorage.removeItem('conch.web.token');
      window.dispatchEvent(new StorageEvent('storage', { key: 'conch.web.token', oldValue: secret, newValue: null, storageArea: window.localStorage }));
    });
    await screen.findByText('Signed out in another tab');
    expect(screen.getByLabelText('Credential')).toBeDefined();
  });

  it('returns to sign-in with the expiry line on a 401 mid-session', async () => {
    let calls = 0;
    serve({
      ...asMember,
      '/v1/channels': () => {
        calls++;
        return json({}, 401);
      },
    });
    render(<App origin={origin} />);
    signIn();
    await screen.findByText('Signed out: the credential is no longer valid');
    expect(calls).toBe(1);
    expect(window.sessionStorage.getItem('conch.web.token')).toBeNull();
  });

  it('shows Reconnecting and offers a retry when the channel list cannot be reached', async () => {
    let up = false;
    serve({
      ...asMember,
      '/v1/channels': (a) => {
        if (!up) throw new TypeError('network');
        return a === `Bearer ${secret}` ? json(channels) : json({}, 401);
      },
    });
    render(<App origin={origin} />);
    signIn();
    await screen.findByRole('button', { name: 'Try again' });
    expect(screen.getByRole('status', { name: 'Status' }).textContent).toContain('Reconnecting');
    up = true;
    await act(async () => {
      fireEvent.click(screen.getByRole('button', { name: 'Try again' }));
      await Promise.resolve();
    });
    await screen.findByRole('button', { name: 'general' });
    expect(screen.getByRole('status', { name: 'Status' }).textContent).toContain('Connected');
  });

  it.each([
    ['owner', { ...operator, owner: true }, [], 'You are the owner.'],
    ['operator', operator, [], 'You are an operator. Only the owner changes roles or acts on the owner.'],
    ['manager', member, ['general', 'ops'], 'You manage #general and #ops.'],
    ['member', member, [], 'You are a member.'],
    [
      'guest',
      {
        id: 12,
        kind: 'human',
        name: 'guest-a7k4m2q9x3h5t8wz',
        role: 'member',
        identity_class: 'guest',
        guest: {
          state: 'active',
          origin: 'address',
          handle: 'guest-a7k4m2q9x3h5t8wz',
          tag: 'a7k4',
          started_at: created_at,
          expires_at: '2099-10-22T12:00:00.000Z',
          idle_expires_at: '2099-10-21T14:00:00.000Z',
          can_convert: true,
        },
      },
      [],
      'You are a guest; a visit cannot hold any right.',
    ],
  ])('states the caller\'s standing as %s in Who can administer', async (_standing: string, who: Record<string, unknown>, managed: readonly string[], line: string) => {
    window.sessionStorage.setItem('conch.web.token', secret);
    serve({
      '/v1/whoami': () => json(who),
      '/v1/channels': () => json({ channels: channels.channels.map((channel) => ({ ...channel, manages: managed.includes(channel.name) })) }),
    });
    render(<App origin={origin} />);
    fireEvent.click(await screen.findByRole('button', { name: 'Settings' }));
    const account = screen.getByRole('region', { name: 'Account' });
    expect(within(account).getByRole('heading', { name: 'Who can administer' })).toBeDefined();
    expect(within(account).getByText(line)).toBeDefined();
  });
});

// Issue #590 (F20): arriving in the lobby puts focus on the open channel's
// heading, so the next Tab starts there and not at the top of the page. Only
// arriving does: opening another channel leaves focus where it is.
describe('focus on arriving', () => {
  const heading = (name: string) => within(screen.getByRole('region', { name: 'Conversation' })).getByRole('heading', { level: 2, name });

  it('goes to the heading after signing in from the keyboard, and stays put on a channel switch', async () => {
    serve(asMember);
    render(<App origin={origin} />);
    const field = screen.getByLabelText('Credential');
    field.focus();
    fireEvent.change(field, { target: { value: secret } });
    // Enter in the field submits its form.
    fireEvent.submit(field);
    await waitFor(() => {
      expect(document.activeElement).toBe(heading('general'));
    });
    expect(document.activeElement?.getAttribute('tabindex')).toBe('-1');

    const ops = screen.getByRole('button', { name: 'ops' });
    ops.focus();
    fireEvent.click(ops);
    await screen.findByRole('heading', { level: 2, name: 'ops' });
    await act(async () => {});
    expect(document.activeElement).toBe(ops);
  });

  it('goes to the heading when a stored sign-in is restored on reload', async () => {
    serve(asMember);
    window.sessionStorage.setItem('conch.web.token', secret);
    render(<App origin={origin} />);
    await waitFor(() => {
      expect(document.activeElement).toBe(heading('general'));
    });
  });

  it('is left alone when it is already somewhere on arriving', async () => {
    serve(asMember);
    window.sessionStorage.setItem('conch.web.token', secret);
    const elsewhere = document.createElement('button');
    document.body.append(elsewhere);
    elsewhere.focus();
    render(<App origin={origin} />);
    await screen.findByRole('heading', { level: 2, name: 'general' });
    await act(async () => {});
    expect(document.activeElement).toBe(elsewhere);
    elsewhere.remove();
  });
});
