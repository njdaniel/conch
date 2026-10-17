import { cleanup, fireEvent, render, screen, waitFor, within } from '@testing-library/react';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { App } from './App';

// Changing your own name from the lobby (issue #441), against a stand-in
// conchd that records each request's method and body.
const origin = 'http://127.0.0.1:8080';
const secret = 'conch_secret_token_value';
const created_at = '2026-10-16T00:00:00Z';
const json = (body: unknown, status = 200) => new Response(JSON.stringify(body), { status });

type Call = { readonly method: string; readonly path: string; readonly body: unknown };

function serve(me: { id: number; kind: string; name: string; role: string }, rename: (name: string) => Response) {
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
      if (path === '/v1/channels') return Promise.resolve(json({ channels: [] }));
      if (method === 'PUT' && path === `/v1/principals/${String(me.id)}/name`) {
        return Promise.resolve(rename((body as { name: string }).name));
      }
      return Promise.resolve(json({}, 404));
    }),
  );
  return calls;
}

const member = { id: 2, kind: 'human', name: 'ada', role: 'member' };

async function signInAndOpen() {
  render(<App origin={origin} />);
  fireEvent.change(screen.getByLabelText('Credential'), { target: { value: secret } });
  fireEvent.click(screen.getByRole('button', { name: 'Sign in' }));
  // Your name is in Settings (issue #536).
  fireEvent.click(await screen.findByRole('button', { name: 'Settings' }));
  const toggle = await screen.findByRole('button', { name: 'Change your name' });
  expect(toggle.getAttribute('aria-expanded')).toBe('false');
  fireEvent.click(toggle);
  expect(toggle.getAttribute('aria-expanded')).toBe('true');
  return screen.getByRole('form', { name: 'Change your name' });
}

function submit(form: HTMLElement, name: string) {
  fireEvent.change(within(form).getByLabelText('New name'), { target: { value: name } });
  fireEvent.click(within(form).getByRole('button', { name: 'Save name' }));
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

describe('change your name', () => {
  it('sends one PUT with the new name, and the lobby shows the name the server answered', async () => {
    const calls = serve(member, (name) => json({ principal: { id: 2, kind: 'human', name, created_at } }));
    const form = await signInAndOpen();
    submit(form, 'Ada Lovelace');
    await screen.findByText('Your name is now Ada Lovelace');
    const puts = calls.filter((c) => c.method === 'PUT');
    expect(puts).toEqual([{ method: 'PUT', path: '/v1/principals/2/name', body: { name: 'Ada Lovelace' } }]);
    const status = screen.getByRole('status', { name: 'Status' });
    expect(within(status).getByText('Ada Lovelace')).toBeDefined();
  });

  it.each([
    ['a name that is taken or looks the same', 409, 'principal_exists', 'That name is taken, or looks too much like someone else’s'],
    ['too many renames today', 429, 'rate_limited', 'You have changed or tried to change your name too often today. Try again tomorrow'],
    ['a name the server will not take', 400, 'invalid_request', 'That name cannot be used: it needs a letter you can see, and no spaces at either end or control characters'],
    ['a refusal it does not know', 403, 'forbidden', 'The server refused the change'],
  ])('says so for %s, and keeps the old name', async (_case, status, code, line) => {
    serve(member, () => json({ code, message: 'no' }, status));
    const form = await signInAndOpen();
    submit(form, 'Ad\u0430');
    await screen.findByText(line);
    const footer = screen.getByRole('status', { name: 'Status' });
    expect(within(footer).getByText('ada')).toBeDefined();
  });

  it('sends nothing for the name already held or a blank one', async () => {
    const calls = serve(member, () => json({}, 500));
    const form = await signInAndOpen();
    submit(form, 'ada');
    await screen.findByText('That is already your name');
    fireEvent.change(within(form).getByLabelText('New name'), { target: { value: '   ' } });
    expect(within(form).getByRole('button', { name: 'Save name' }).hasAttribute('disabled')).toBe(true);
    expect(calls.filter((c) => c.method === 'PUT')).toEqual([]);
  });

  it('returns to sign-in on a 401', async () => {
    serve(member, () => json({}, 401));
    const form = await signInAndOpen();
    submit(form, 'Ada Lovelace');
    await screen.findByText('Signed out: the credential is no longer valid');
  });

  it('is not offered to an agent', async () => {
    serve({ id: 5, kind: 'agent', name: 'bot', role: 'member' }, () => json({}, 500));
    render(<App origin={origin} />);
    fireEvent.change(screen.getByLabelText('Credential'), { target: { value: secret } });
    fireEvent.click(screen.getByRole('button', { name: 'Sign in' }));
    fireEvent.click(await screen.findByRole('button', { name: 'Settings' }));
    await screen.findByRole('button', { name: 'Sign out' });
    await waitFor(() => {
      expect(screen.queryByRole('button', { name: 'Change your name' })).toBeNull();
    });
  });
});
