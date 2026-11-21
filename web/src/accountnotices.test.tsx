import { cleanup, fireEvent, render, screen, waitFor } from '@testing-library/react';
import { afterEach, describe, expect, it, vi } from 'vitest';
import { App } from './App';
import { noticeText } from './AccountNotices';
import { dateTimeOf } from './time';

// Account notices (issue #2042): a member is told their password was set
// through a link another principal made, on a sign-in of their own, and
// dismisses it themselves. Against a stand-in conchd that records requests.
const origin = 'http://127.0.0.1:8080';
const secret = 'conch_secret_token_value';
const json = (body: unknown, status = 200) => new Response(JSON.stringify(body), { status });
const member = { id: 2, kind: 'human', name: 'ada', role: 'member' };
const notice = { id: 3, kind: 'password_set_by_link', by_name: 'opal', at: '2026-11-19T12:00:00.000Z' };

type Call = { readonly method: string; readonly path: string };

function serve(options: { notices: unknown[]; dismiss?: () => Response }) {
  const calls: Call[] = [];
  let notices = options.notices;
  vi.stubGlobal(
    'fetch',
    vi.fn((input: RequestInfo | URL, init?: RequestInit) => {
      const path = typeof input === 'string' ? input : input instanceof URL ? input.pathname : input.url;
      const method = init?.method ?? 'GET';
      calls.push({ method, path });
      if (new Headers(init?.headers).get('Authorization') !== `Bearer ${secret}`) return Promise.resolve(json({}, 401));
      if (path === '/v1/whoami') return Promise.resolve(json(member));
      if (path === '/v1/channels') return Promise.resolve(json({ channels: [] }));
      if (path === '/v1/notices/mine' && method === 'GET') return Promise.resolve(json({ notices }));
      if (path.startsWith('/v1/notices/mine/') && method === 'DELETE') {
        const answer = options.dismiss?.() ?? new Response(null, { status: 204 });
        if (answer.status === 204) notices = [];
        return Promise.resolve(answer);
      }
      return Promise.resolve(json({}, 404));
    }),
  );
  return calls;
}

function signIn() {
  render(<App origin={origin} />);
  fireEvent.change(screen.getByLabelText('Credential'), { target: { value: secret } });
  fireEvent.click(screen.getByRole('button', { name: 'Use credential' }));
}

afterEach(() => {
  cleanup();
  vi.unstubAllGlobals();
  window.localStorage.clear();
  window.sessionStorage.clear();
});

describe('account notices', () => {
  it('says whose link set the password, and when, in words of the page', () => {
    const text = noticeText({ id: 3, kind: 'password_set_by_link', by_name: 'opal', at: '2026-11-19T12:00:00.000Z' });
    expect(text).toContain('Your password was set through a link made by opal');
    expect(text).toContain(dateTimeOf('2026-11-19T12:00:00.000Z'));
  });

  it('shows the notice after sign-in and dismisses it on the member’s click', async () => {
    const calls = serve({ notices: [notice] });
    signIn();
    const banner = (await screen.findAllByText(/Your password was set through a link made by opal/))[0];
    expect(banner?.closest('[role="status"]')).not.toBeNull();
    fireEvent.click(screen.getAllByRole('button', { name: 'Dismiss' })[0] as HTMLElement);
    await waitFor(() => {
      expect(screen.queryByText(/Your password was set through a link made by opal/)).toBeNull();
    });
    expect(calls.filter((c) => c.method === 'DELETE')).toEqual([{ method: 'DELETE', path: '/v1/notices/mine/3' }]);
  });

  it('shows nothing when there is no notice', async () => {
    const calls = serve({ notices: [] });
    signIn();
    await screen.findByRole('button', { name: 'Settings' });
    await waitFor(() => {
      expect(calls.some((c) => c.path === '/v1/notices/mine')).toBe(true);
    });
    expect(screen.queryByText(/Your password was set through a link/)).toBeNull();
  });

  it('keeps the notice when the server did not dismiss it', async () => {
    serve({ notices: [notice], dismiss: () => json({ code: 'internal' }, 500) });
    signIn();
    await screen.findAllByText(/Your password was set through a link made by opal/);
    fireEvent.click(screen.getAllByRole('button', { name: 'Dismiss' })[0] as HTMLElement);
    await screen.findAllByText('Could not dismiss that notice. Try again.');
    expect(screen.getAllByText(/Your password was set through a link made by opal/).length).toBeGreaterThanOrEqual(1);
  });

  it('is also in Settings', async () => {
    serve({ notices: [notice] });
    signIn();
    fireEvent.click(await screen.findByRole('button', { name: 'Settings' }));
    await waitFor(() => {
      expect(screen.getAllByText(/Your password was set through a link made by opal/).length).toBeGreaterThanOrEqual(1);
    });
  });
});
