import { act, cleanup, fireEvent, render, screen, waitFor, within } from '@testing-library/react';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { App } from './App';

// Making a channel from the channel list (issue #695), against a stand-in
// conchd that records each request's method, path and body.
const origin = 'http://127.0.0.1:8080';
const secret = 'conch_secret_token_value';
const key = 'conch.web.token';
const created_at = '2026-10-21T00:00:00Z';
const json = (body: unknown, status = 200) => new Response(JSON.stringify(body), { status });

type Call = { readonly method: string; readonly path: string; readonly body: unknown };
type Who = { id: number; kind: string; name: string; role: string };

const operator: Who = { id: 1, kind: 'human', name: 'nick', role: 'operator' };
const member: Who = { id: 2, kind: 'human', name: 'ada', role: 'member' };
const general = { id: 1, name: 'general', created_at };

// serve answers whoami and the channel list for the one credential, and hands
// a create to create, which may answer, fail or wait.
function serve(me: Who, create: (name: string) => Promise<Response>, channels = [general]) {
  const calls: Call[] = [];
  vi.stubGlobal(
    'fetch',
    vi.fn((input: RequestInfo | URL, init?: RequestInit) => {
      const path = typeof input === 'string' ? input : input instanceof URL ? input.pathname : input.url;
      const method = init?.method ?? 'GET';
      const body: unknown = typeof init?.body === 'string' ? JSON.parse(init.body) : undefined;
      calls.push({ method, path, body });
      if (new Headers(init?.headers).get('Authorization') !== `Bearer ${secret}`) return Promise.resolve(json({}, 401));
      if (path === '/v1/whoami') return Promise.resolve(json(me));
      if (path === '/v1/channels') return Promise.resolve(json({ channels }));
      if (method === 'POST' && path === '/v0/channels') return create((body as { name: string }).name);
      return Promise.resolve(json({}, 404));
    }),
  );
  return calls;
}

const made = (name: string, id = 2) => Promise.resolve(json({ channel: { id, name, created_at } }, 201));
const refused = (status: number, code: string, message = 'no') => () => Promise.resolve(json({ code, message }, status));

const nav = () => screen.getByRole('navigation', { name: 'Channels' });
const field = (form: HTMLElement) => within(form).getByLabelText<HTMLInputElement>('Channel name');
const creates = (calls: readonly Call[]) => calls.filter((c) => c.method === 'POST' && c.path === '/v0/channels');

// open signs in as whoever the stored credential is (an operator's is kept for
// this tab) and opens the form.
async function open(): Promise<HTMLElement> {
  render(<App origin={origin} />);
  const toggle = await within(await screen.findByRole('navigation', { name: 'Channels' })).findByRole('button', { name: 'New channel' });
  // Landing put the first channel in the address; jsdom tells the page of it
  // a task later, which must not arrive after a channel made here is opened.
  await act(() => new Promise((r) => setTimeout(r, 0)));
  expect(toggle.getAttribute('aria-expanded')).toBe('false');
  fireEvent.click(toggle);
  expect(toggle.getAttribute('aria-expanded')).toBe('true');
  return screen.getByRole('form', { name: 'New channel' });
}

function type(form: HTMLElement, name: string) {
  fireEvent.change(field(form), { target: { value: name } });
}

beforeEach(() => {
  window.sessionStorage.clear();
  window.localStorage.clear();
  window.location.hash = '';
  window.sessionStorage.setItem(key, secret);
});
afterEach(() => {
  cleanup();
  vi.unstubAllGlobals();
});

describe('new channel', () => {
  it('sends one POST, then lists and opens the channel the server made, without joining its voice', async () => {
    const calls = serve(operator, (name) => made(name));
    const form = await open();
    type(form, 'book-club');
    fireEvent.click(within(form).getByRole('button', { name: 'Create channel' }));
    await screen.findByText('Created #book-club');
    expect(creates(calls)).toEqual([{ method: 'POST', path: '/v0/channels', body: { name: 'book-club' } }]);
    const button = within(nav()).getByRole('button', { name: 'book-club' });
    expect(button.getAttribute('aria-current')).toBe('true');
    expect(within(nav()).getAllByRole('listitem').map((li) => li.textContent)).toEqual(['general', 'book-club']);
    // The form closes and is empty for the next one; focus is back on its button.
    expect(form.hidden).toBe(true);
    expect(field(form).value).toBe('');
    expect(document.activeElement).toBe(within(nav()).getByRole('button', { name: 'New channel' }));
    // The address is written by an effect after the render that opens the
    // channel, which may not have run yet (issue #804).
    await waitFor(() => {
      expect(window.location.hash).toBe('#/c/book-club');
    });
    // Opening the channel asked for no voice session.
    await act(async () => {});
    expect(calls.filter((c) => c.path.includes('/voice/session'))).toEqual([]);
  });

  it.each([
    ['a name already taken', refused(409, 'channel_exists'), 'There is already a channel with that name, perhaps one you are not in. Choose another name.'],
    [
      'a name the server will not take, with its rule',
      refused(400, 'invalid_request', 'schema: channel name must not begin or end with whitespace'),
      'Channel name must not begin or end with whitespace.',
    ],
    ['a refusal of a member', refused(403, 'forbidden', 'operator role required'), 'Only an operator can create channels.'],
    ['no answer at all', () => Promise.reject(new TypeError('failed')), 'Cannot reach the server. Try again.'],
    ['a server failure', refused(500, 'internal_error'), 'The server could not create the channel. Try again.'],
  ])('says so for %s, keeps the name as typed and lists nothing new', async (_case, answer, line) => {
    const calls = serve(operator, answer);
    const form = await open();
    type(form, 'book club ');
    fireEvent.click(within(form).getByRole('button', { name: 'Create channel' }));
    const said = await within(nav()).findByText(line);
    expect(said.getAttribute('role')).toBe('status');
    expect(form.hidden).toBe(false);
    expect(field(form).value).toBe('book club ');
    expect(within(nav()).getAllByRole('listitem').map((li) => li.textContent)).toEqual(['general']);
    expect(creates(calls)).toHaveLength(1);
    // The same name may be sent again as it stands, once more.
    expect(within(form).getByRole('button', { name: 'Create channel' }).hasAttribute('disabled')).toBe(false);
  });

  it('sends one request however often it is submitted while waiting for the answer', async () => {
    let answer: (res: Response) => void = () => undefined;
    const calls = serve(operator, () => new Promise<Response>((resolve) => (answer = resolve)));
    const form = await open();
    type(form, 'book-club');
    // Twice in one tick, before a render could disable anything, then again.
    act(() => {
      fireEvent.submit(form);
      fireEvent.submit(form);
    });
    fireEvent.click(within(form).getByRole('button', { name: 'Create channel' }));
    fireEvent.keyDown(field(form), { key: 'Enter' });
    fireEvent.submit(form);
    await waitFor(() => {
      expect(creates(calls)).toHaveLength(1);
    });
    expect(within(form).getByRole('button', { name: 'Create channel' }).hasAttribute('disabled')).toBe(true);
    answer(json({ channel: { id: 2, name: 'book-club', created_at } }, 201));
    await screen.findByText('Created #book-club');
    expect(creates(calls)).toHaveLength(1);
    expect(within(nav()).getAllByRole('button', { name: 'book-club' })).toHaveLength(1);
  });

  it('sends nothing for a blank name', async () => {
    const calls = serve(operator, (name) => made(name));
    const form = await open();
    type(form, '   ');
    expect(within(form).getByRole('button', { name: 'Create channel' }).hasAttribute('disabled')).toBe(true);
    fireEvent.submit(form);
    await act(async () => {});
    expect(creates(calls)).toEqual([]);
  });

  it('returns to sign-in on a 401', async () => {
    serve(operator, () => Promise.resolve(json({}, 401)));
    const form = await open();
    type(form, 'book-club');
    fireEvent.click(within(form).getByRole('button', { name: 'Create channel' }));
    await screen.findByText('Signed out: the credential is no longer valid');
  });

  it('is offered to an operator with no channels yet, and the first one made is opened', async () => {
    serve(operator, (name) => made(name, 1), []);
    const form = await open();
    expect(within(nav()).getByText('No channels yet')).toBeDefined();
    type(form, 'general');
    fireEvent.click(within(form).getByRole('button', { name: 'Create channel' }));
    const button = await within(nav()).findByRole('button', { name: 'general' });
    expect(button.getAttribute('aria-current')).toBe('true');
    expect(within(nav()).queryByText('No channels yet')).toBeNull();
  });

  // Issue #1032: what an operator sees first on a server with no channels.
  it('shows an operator with no channels the first-run panel', async () => {
    serve(operator, (name) => made(name, 1), []);
    render(<App origin={origin} />);
    const heading = await screen.findByRole('heading', { name: 'Welcome to your server' });
    // Focus lands on the heading after sign-in, as on a conversation's.
    await waitFor(() => {
      expect(document.activeElement).toBe(heading);
    });
    const conversation = screen.getByRole('region', { name: 'Conversation' });
    expect(within(conversation).getByRole('button', { name: 'Make your first channel' })).toBeDefined();
    const invite = within(conversation).getByRole('button', { name: 'Invite someone' });
    expect(invite.hasAttribute('disabled')).toBe(true);
    expect(invite.getAttribute('aria-describedby')).toBeTruthy();
    expect(within(conversation).getByText('Make a channel first')).toBeDefined();
    expect(within(conversation).getByRole('button', { name: 'Decide how people join' })).toBeDefined();
    expect(within(screen.getByRole('region', { name: 'Members list' })).queryByText('Connecting…')).toBeNull();
  });

  it('opens the New channel form from the first-run panel, and the panel is gone once the channel is made and open', async () => {
    serve(operator, (name) => made(name, 1), []);
    render(<App origin={origin} />);
    fireEvent.click(await screen.findByRole('button', { name: 'Make your first channel' }));
    const form = screen.getByRole('form', { name: 'New channel' });
    expect(within(nav()).getByRole('button', { name: 'New channel' }).getAttribute('aria-expanded')).toBe('true');
    await waitFor(() => {
      expect(document.activeElement).toBe(field(form));
    });
    type(form, 'general');
    fireEvent.click(within(form).getByRole('button', { name: 'Create channel' }));
    const button = await within(nav()).findByRole('button', { name: 'general' });
    expect(button.getAttribute('aria-current')).toBe('true');
    expect(screen.queryByRole('heading', { name: 'Welcome to your server' })).toBeNull();
    expect(screen.queryByRole('button', { name: 'Make your first channel' })).toBeNull();
  });

  it('opens Settings with Joining from the first-run panel', async () => {
    serve(operator, (name) => made(name, 1), []);
    render(<App origin={origin} />);
    fireEvent.click(await screen.findByRole('button', { name: 'Decide how people join' }));
    expect(screen.getByRole('button', { name: 'Settings' }).getAttribute('aria-expanded')).toBe('true');
  });

  it('is not offered to a member, whose list still says who can add them', async () => {
    window.sessionStorage.clear();
    window.localStorage.setItem(key, secret);
    const calls = serve(member, (name) => made(name), []);
    render(<App origin={origin} />);
    await within(await screen.findByRole('navigation', { name: 'Channels' })).findByText('No channels yet: an operator needs to add you to one');
    expect(screen.queryByRole('button', { name: 'New channel' })).toBeNull();
    expect(screen.queryByRole('form', { name: 'New channel' })).toBeNull();
    expect(screen.queryByRole('heading', { name: 'Welcome to your server' })).toBeNull();
    expect(screen.queryByRole('button', { name: 'Make your first channel' })).toBeNull();
    expect(creates(calls)).toEqual([]);
  });
});
