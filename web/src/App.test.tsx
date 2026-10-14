import { act, cleanup, fireEvent, render, screen, waitFor } from '@testing-library/react';
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

  it('has one password field and no username or reset', () => {
    serve(asMember);
    render(<App origin={origin} />);
    const field = screen.getByLabelText('Credential');
    expect(field.getAttribute('type')).toBe('password');
    expect(field.getAttribute('autocomplete')).toBe('off');
    expect(screen.queryAllByRole('textbox')).toHaveLength(0);
    expect(document.body.textContent).not.toMatch(/forgot|username|reset/i);
  });

  it('signs a member in to the lobby, stores the token for the tab and keeps it out of the DOM', async () => {
    serve(asMember);
    const { container } = render(<App origin={origin} />);
    signIn();
    await screen.findByRole('navigation', { name: 'Channels' });
    expect(window.sessionStorage.getItem('conch.web.token')).toBe(secret);
    expect(container.innerHTML).not.toContain(secret);
    expect(window.location.href).not.toContain(secret);
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
    expect(screen.getByRole('region', { name: 'Conversation' }).textContent).toBe('general');
    const status = screen.getByRole('status', { name: 'Status' });
    expect(status.textContent).toContain('ada');
    expect(status.textContent).toContain(origin);
    expect(status.textContent).toContain('Connected');
    expect(document.body.textContent).not.toMatch(/voice|join/i);
  });

  it('shows Connecting while the channels load', async () => {
    serve({ ...asMember, '/v1/channels': () => new Promise<Response>(() => undefined) });
    render(<App origin={origin} />);
    signIn();
    await screen.findByText('Loading channels…');
    expect(screen.getByRole('status', { name: 'Status' }).textContent).toContain('Connecting');
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
    expect(screen.getByRole('region', { name: 'Conversation' }).textContent).toBe('ops');
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

  it('says sign-out revokes nothing, and returns to sign-in clearing the tab', async () => {
    serve(asMember);
    render(<App origin={origin} />);
    signIn();
    const out = await screen.findByRole('button', { name: 'Sign out' });
    expect(out.getAttribute('aria-describedby')).toBe('signout-note');
    expect(document.getElementById('signout-note')?.textContent).toContain('does not revoke');
    fireEvent.click(out);
    await screen.findByLabelText('Credential');
    expect(window.sessionStorage.getItem('conch.web.token')).toBeNull();
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
