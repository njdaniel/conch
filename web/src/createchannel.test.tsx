import { act, cleanup, fireEvent, render, screen, waitFor, within } from '@testing-library/react';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { App } from './App';

// Making a channel from the channel list (issues #695, #991), against a stand-in
// conchd that records each request's method, path and body.
const origin = 'http://127.0.0.1:8080';
const secret = 'conch_secret_token_value';
const key = 'conch.web.token';
const created_at = '2026-10-21T00:00:00Z';
const json = (body: unknown, status = 200) => new Response(JSON.stringify(body), { status });

type Call = { readonly method: string; readonly path: string; readonly body: unknown };
type Who = { id: number; kind: string; name: string; role: string; permissions?: string[] };

const operator: Who = { id: 1, kind: 'human', name: 'nick', role: 'operator' };
const member: Who = { id: 2, kind: 'human', name: 'ada', role: 'member', permissions: ['create_public_channels', 'create_private_channels'] };
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
      if (method === 'GET' && path === '/v1/channels') return Promise.resolve(json({ channels }));
      if (method === 'POST' && path === '/v1/channels') return create((body as { name: string }).name);
      return Promise.resolve(json({}, 404));
    }),
  );
  return calls;
}

const made = (name: string, id = 2) => Promise.resolve(json({ channel: { id, name, created_at } }, 201));
const refused = (status: number, code: string, message = 'no', headers: Record<string, string> = {}) => () =>
  Promise.resolve(new Response(JSON.stringify({ code, message }), { status, headers }));

const nav = () => screen.getByRole('navigation', { name: 'Channels' });
const field = (form: HTMLElement) => within(form).getByLabelText<HTMLInputElement>('Channel name');
const creates = (calls: readonly Call[]) => calls.filter((c) => c.method === 'POST' && c.path === '/v1/channels');

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
    expect(creates(calls)).toEqual([{ method: 'POST', path: '/v1/channels', body: { name: 'book-club', visibility: 'public' } }]);
    const button = within(nav()).getByRole('button', { name: 'book-club' });
    expect(button.getAttribute('aria-current')).toBe('true');
    expect(within(nav()).getAllByRole('listitem').map((li) => li.querySelector('li > button')?.textContent)).toEqual(['general', 'book-club']);
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
    // The one line, whatever the collision, and never the server's own words.
    ['a name already taken', refused(409, 'channel_exists', 'a channel with this name, or one that looks the same, already exists'), 'That name is taken'],
    [
      "a name the server will not take, with the page's own rule",
      refused(400, 'invalid_request', 'schema: channel name must not begin or end with whitespace'),
      'That name cannot be used: it needs a letter you can see, at most 100 characters, and no spaces at either end or control characters.',
    ],
    ['a refusal of a non-member', refused(403, 'forbidden', 'only a member of the community may create a channel'), 'Only members of this community can make channels.'],
    ['a missing permission', refused(403, 'permission_denied', 'you may not create a public channel'), 'You do not have permission to make a public channel.'],
    [
      'the daily cap, with its Retry-After',
      refused(429, 'rate_limited', 'you may create channels at most 10 times, and try at most 30 times, a day', { 'Retry-After': '7300' }),
      'You have reached the daily limit for making channels. Try again in 3 hours.',
    ],
    ['the daily cap, without one', refused(429, 'rate_limited'), 'You have reached the daily limit for making channels. Try again in a few minutes.'],
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
    expect(within(nav()).getAllByRole('listitem').map((li) => li.querySelector('li > button')?.textContent)).toEqual(['general']);
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
    const heading = await screen.findByRole('heading', { name: "You're not in any channels yet" });
    // Focus lands on the heading after sign-in, as on a conversation's.
    await waitFor(() => {
      expect(document.activeElement).toBe(heading);
    });
    const conversation = screen.getByRole('region', { name: 'Conversation' });
    expect(within(conversation).getByRole('button', { name: 'Make a channel' })).toBeDefined();
    const invite = within(conversation).getByRole('button', { name: 'Invite someone' });
    expect(invite.hasAttribute('disabled')).toBe(true);
    expect(invite.getAttribute('aria-describedby')).toBeTruthy();
    expect(within(conversation).getByText('Needs a channel first')).toBeDefined();
    expect(within(conversation).getByRole('button', { name: 'Decide how people join' })).toBeDefined();
    // /v1/channels lists only this operator's channels, so the panel never
    // says the server has none: it may be a busy server they are in no
    // channel of (codex on #1085).
    expect(within(conversation).queryByText(/no channels yet|your server|first channel/i)).toBeNull();
    expect(within(screen.getByRole('region', { name: 'Members list' })).queryByText('Connecting…')).toBeNull();
  });

  // Issue #1701: POST /v1/channels checks the create permissions, not
  // manage_channels and not the role.
  const panel = () => screen.queryByRole('heading', { name: "You're not in any channels yet" });
  const emptyList = async () => within(await screen.findByRole('navigation', { name: 'Channels' })).findByText('No channels yet');

  it('offers an operator with manage_channels but both create cells off no first-run panel', async () => {
    serve({ ...operator, permissions: ['manage_channels'] }, (name) => made(name, 1), []);
    render(<App origin={origin} />);
    expect(await emptyList()).toBeDefined();
    expect(panel()).toBeNull();
    expect(screen.queryByRole('button', { name: 'Make a channel' })).toBeNull();
  });

  it('shows an operator with only create_public_channels the first-run panel, in an operator’s words', async () => {
    serve({ ...operator, permissions: ['create_public_channels'] }, (name) => made(name, 1), []);
    render(<App origin={origin} />);
    expect(await screen.findByRole('heading', { name: "You're not in any channels yet" })).toBeDefined();
    expect(screen.getByRole('button', { name: 'Make a channel' })).toBeDefined();
    expect(screen.queryByText(/ask an operator/)).toBeNull();
  });

  it('opens the New channel form from the first-run panel, and the panel is gone once the channel is made and open', async () => {
    serve(operator, (name) => made(name, 1), []);
    render(<App origin={origin} />);
    fireEvent.click(await screen.findByRole('button', { name: 'Make a channel' }));
    const form = screen.getByRole('form', { name: 'New channel' });
    expect(within(nav()).getByRole('button', { name: 'New channel' }).getAttribute('aria-expanded')).toBe('true');
    await waitFor(() => {
      expect(document.activeElement).toBe(field(form));
    });
    type(form, 'general');
    fireEvent.click(within(form).getByRole('button', { name: 'Create channel' }));
    const button = await within(nav()).findByRole('button', { name: 'general' });
    expect(button.getAttribute('aria-current')).toBe('true');
    expect(screen.queryByRole('heading', { name: "You're not in any channels yet" })).toBeNull();
    expect(screen.queryByRole('button', { name: 'Make a channel' })).toBeNull();
  });

  it('opens Settings with Joining from the first-run panel', async () => {
    serve(operator, (name) => made(name, 1), []);
    render(<App origin={origin} />);
    fireEvent.click(await screen.findByRole('button', { name: 'Decide how people join' }));
    expect(screen.getByRole('button', { name: 'Settings' }).getAttribute('aria-expanded')).toBe('true');
  });

  it('is offered to a member with a visibility choice, and sends the one chosen', async () => {
    window.sessionStorage.clear();
    window.localStorage.setItem(key, secret);
    const calls = serve(member, (name) => made(name));
    const form = await open();
    expect(within(form).getByRole<HTMLInputElement>('radio', { name: /^Public/ }).checked).toBe(true);
    fireEvent.click(within(form).getByRole('radio', { name: /^Private/ }));
    type(form, 'quiet-room');
    fireEvent.click(within(form).getByRole('button', { name: 'Create channel' }));
    await screen.findByText('Created #quiet-room');
    expect(creates(calls)).toEqual([{ method: 'POST', path: '/v1/channels', body: { name: 'quiet-room', visibility: 'private' } }]);
    // It is opened, as the creator's own.
    expect(within(nav()).getByRole('button', { name: 'quiet-room' }).getAttribute('aria-current')).toBe('true');
  });

  it('offers only the visibility a member holds the permission for', async () => {
    window.sessionStorage.clear();
    window.localStorage.setItem(key, secret);
    const calls = serve({ ...member, permissions: ['create_private_channels'] }, (name) => made(name));
    const form = await open();
    expect(within(form).queryByRole('radio')).toBeNull();
    type(form, 'only-private');
    fireEvent.click(within(form).getByRole('button', { name: 'Create channel' }));
    await screen.findByText('Created #only-private');
    expect(creates(calls)[0]?.body).toEqual({ name: 'only-private', visibility: 'private' });
  });

  it('is not offered to a member who holds neither create permission', async () => {
    window.sessionStorage.clear();
    window.localStorage.setItem(key, secret);
    const calls = serve({ ...member, permissions: [] }, (name) => made(name), []);
    render(<App origin={origin} />);
    await within(await screen.findByRole('navigation', { name: 'Channels' })).findByText('No channels yet');
    expect(screen.queryByRole('button', { name: 'New channel' })).toBeNull();
    expect(screen.queryByRole('form', { name: 'New channel' })).toBeNull();
    expect(screen.queryByRole('heading', { name: "You're not in any channels yet" })).toBeNull();
    expect(screen.queryByRole('button', { name: 'Make a channel' })).toBeNull();
    expect(creates(calls)).toEqual([]);
  });
});
