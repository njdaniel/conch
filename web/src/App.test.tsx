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

const healthz = (version: string): Handler => () => json({ status: 'ok', version, db: 'ok' });

const asMember: Record<string, Handler> = {
  '/v1/whoami': (a) => (a === `Bearer ${secret}` ? json(member) : json({}, 401)),
  '/v1/channels': (a) => (a === `Bearer ${secret}` ? json(channels) : json({}, 401)),
};

function signIn(token = secret) {
  const field = screen.getByLabelText('Credential');
  fireEvent.change(field, { target: { value: token } });
  fireEvent.click(screen.getByRole('button', { name: 'Sign in' }));
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

  it('shows the server version from /healthz on the sign-in page and in the lobby', async () => {
    serve({ ...asMember, '/healthz': healthz('v0.1.0-alpha.1') });
    render(<App origin={origin} />);
    expect(await screen.findByText('v0.1.0-alpha.1')).toBeDefined();
    signIn();
    await screen.findByRole('button', { name: 'Sign out' });
    expect(await screen.findByText('v0.1.0-alpha.1')).toBeDefined();
  });

  it.each([
    ['does not answer', (): Response => { throw new TypeError('Failed to fetch'); }],
    ['answers with something that is not health', (): Response => json({ status: 'ok' })],
  ])('shows no version when /healthz %s, and sign-in still works', async (_case, handler) => {
    const fetchMock = serve({ ...asMember, '/healthz': handler });
    render(<App origin={origin} />);
    signIn();
    await screen.findByRole('button', { name: 'Sign out' });
    // Both pages have asked by now; the lobby's request has settled once
    // its footer has rendered, with or without a version.
    await waitFor(() => {
      expect(fetchMock.mock.calls.filter(([input]) => input === '/healthz').length).toBeGreaterThanOrEqual(2);
    });
    await new Promise((r) => setTimeout(r, 0));
    expect(screen.queryByText(/Version/)).toBeNull();
  });

  it('has one password field and no username or reset', () => {
    serve(asMember);
    render(<App origin={origin} />);
    const field = screen.getByLabelText('Credential');
    expect(field.getAttribute('type')).toBe('password');
    expect(field.getAttribute('autocomplete')).toBe('off');
    expect(screen.queryAllByRole('textbox')).toHaveLength(0);
    expect(document.body.textContent).not.toMatch(/forgot|username|reset/i);
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
    // Voice is only in its own section; nothing anywhere else mentions voice
    // or offers to join it. Here conchd has no voice route, so it is not
    // joinable, and it joins nothing by itself.
    const voice = screen.getByRole('region', { name: 'Voice' });
    expect(document.body.textContent.replace(voice.textContent, '')).not.toMatch(/voice|join/i);
    expect(voice.textContent).toMatch(/^Not in voice/);
    expect(within(voice).getByRole('button', { name: 'Join voice' }).hasAttribute('disabled')).toBe(true);
  });

  it('says voice is not set up when the server has no LiveKit, and offers no join', async () => {
    const presence = { schema: 'conch.voice_presence.v1', channel_id: 1, configured: false, available: false, rooms: [] };
    serve({ ...asMember, '/v1/channels/general/voice': () => json(presence) });
    render(<App origin={origin} />);
    signIn();
    const voice = await screen.findByRole('region', { name: 'Voice' });
    await waitFor(() => {
      expect(voice.textContent).toContain('Voice is not set up on this server');
    });
    expect(within(voice).queryByRole('button')).toBeNull();
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
    const out = await screen.findByRole('button', { name: 'Sign out' });
    expect(out.getAttribute('aria-describedby')).toBe('signout-note');
    expect(document.getElementById('signout-note')?.textContent).toContain('Sign out ends this sign-in');
    fireEvent.click(out);
    await screen.findByLabelText('Credential');
    const revoke = fetchMock.mock.calls.find(([input]) => input === '/v1/credentials/current');
    expect(revoke?.[1]?.method).toBe('DELETE');
    expect(screen.getByRole('alert').textContent).toBe('');
    expect(window.localStorage.getItem('conch.web.token')).toBeNull();
    expect(window.sessionStorage.getItem('conch.web.token')).toBeNull();
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
    fireEvent.click(await screen.findByRole('button', { name: 'Sign out' }));
    await screen.findByText(/^Signed out in this browser, but the server could not be told/);
    expect(window.localStorage.getItem('conch.web.token')).toBeNull();
  });

  it('signs out everywhere: every credential revoked, then back to sign-in', async () => {
    const fetchMock = serve({ ...asMember, '/v1/principals/2/credentials/revoke-all': () => json({ revoked: 2 }) });
    render(<App origin={origin} />);
    signIn();
    fireEvent.click(await screen.findByRole('button', { name: 'Sign out everywhere' }));
    await screen.findByText('Signed out everywhere: every sign-in of yours has ended');
    const call = fetchMock.mock.calls.find(([input]) => input === '/v1/principals/2/credentials/revoke-all');
    expect(call?.[1]?.method).toBe('POST');
    expect(window.localStorage.getItem('conch.web.token')).toBeNull();
  });

  it.each([
    ['cannot be reached', (): Response => { throw new TypeError('Failed to fetch'); }, /the server cannot be reached/],
    ['refuses the last operator', () => json({ code: 'last_operator', message: 'no' }, 409), /the only operator who can sign in/],
  ])('stays signed in, and says so, when sign out everywhere %s', async (_case, handler, line) => {
    serve({ ...asMember, '/v1/principals/2/credentials/revoke-all': handler });
    render(<App origin={origin} />);
    signIn();
    fireEvent.click(await screen.findByRole('button', { name: 'Sign out everywhere' }));
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
});
