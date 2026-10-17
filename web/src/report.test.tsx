import { cleanup, fireEvent, render, screen, within } from '@testing-library/react';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { App } from './App';
import { forget, record } from './diagnostics';
import { Lobby } from './Lobby';
import { buildReport, ReportProblem } from './Report';
import { createProblemReportRequestV1 } from './schema/report';
import { initialChannels } from './state/channels';

// "Report a problem" from the lobby (issue #446), against a stand-in conchd
// that records each request's method, path and body.
const origin = 'http://127.0.0.1:8080';
const secret = 'conch_secret_token_value';
const json = (body: unknown, status = 200, headers: Record<string, string> = {}) => new Response(JSON.stringify(body), { status, headers });

type Call = { readonly method: string; readonly path: string; readonly body: unknown };

function serve(report: () => Response, channels: () => Response = () => json({ channels: [] })) {
  const calls: Call[] = [];
  vi.stubGlobal(
    'fetch',
    vi.fn((input: RequestInfo | URL, init?: RequestInit) => {
      const path = typeof input === 'string' ? input : input instanceof URL ? input.pathname : input.url;
      const method = init?.method ?? 'GET';
      const body: unknown = typeof init?.body === 'string' ? JSON.parse(init.body) : undefined;
      calls.push({ method, path, body });
      if (path === '/healthz') return Promise.resolve(json({ status: 'ok', version: 'v0.1.0-alpha.2', db: 'ok', voice: 'ok' }));
      if (new Headers(init?.headers).get('Authorization') !== `Bearer ${secret}`) return Promise.resolve(json({}, 401));
      if (path === '/v1/whoami') return Promise.resolve(json({ id: 2, kind: 'human', name: 'ada', role: 'member' }));
      if (path === '/v1/channels') return Promise.resolve(channels());
      if (method === 'POST' && path === '/v1/reports') return Promise.resolve(report());
      return Promise.resolve(json({}, 404));
    }),
  );
  return calls;
}

async function signInAndOpen() {
  render(<App origin={origin} />);
  await screen.findByText('v0.1.0-alpha.2');
  fireEvent.change(screen.getByLabelText('Credential'), { target: { value: secret } });
  fireEvent.click(screen.getByRole('button', { name: 'Sign in' }));
  const header = (await screen.findByRole('button', { name: 'Sign out' })).closest('header');
  if (header === null) throw new Error('no header');
  const toggle = within(header).getByRole('button', { name: 'Report a problem' });
  expect(toggle.getAttribute('aria-expanded')).toBe('false');
  fireEvent.click(toggle);
  expect(toggle.getAttribute('aria-expanded')).toBe('true');
  return within(header).getByRole('form', { name: 'Report a problem' });
}

function fill(form: HTMLElement) {
  fireEvent.change(within(form).getByLabelText('What you did'), { target: { value: 'Joined voice' } });
  fireEvent.change(within(form).getByLabelText('What you expected'), { target: { value: 'To hear the others' } });
  fireEvent.change(within(form).getByLabelText('What happened instead'), { target: { value: 'Silence' } });
  fireEvent.change(within(form).getByLabelText('How often does it happen?'), { target: { value: 'sometimes' } });
}

beforeEach(() => {
  window.sessionStorage.clear();
  window.localStorage.clear();
  window.location.hash = '';
  forget();
});
afterEach(() => {
  cleanup();
  vi.unstubAllGlobals();
  forget();
});

describe('report a problem', () => {
  it('shows exactly what will be sent, and sends that, once', async () => {
    const calls = serve(() => json({ id: 7, created_at: '2026-10-16T09:00:00.000Z' }, 201));
    record({ kind: 'http_failure', status: 503, error: 'voice_unavailable', requestId: '0123456789abcdef' });
    const form = await signInAndOpen();
    const review = within(form).getByRole('button', { name: 'Review report' });
    expect(review.hasAttribute('disabled')).toBe(true);
    fill(form);
    fireEvent.click(review);

    const preview = await screen.findByLabelText('The report as it will be sent');
    const shown: unknown = JSON.parse(preview.textContent);
    expect(createProblemReportRequestV1(shown)).toEqual([]);
    expect(shown).toMatchObject({
      did: 'Joined voice',
      expected: 'To hear the others',
      happened: 'Silence',
      often: 'sometimes',
      client: { name: 'conch-web', version: 'v0.1.0-alpha.2' },
    });
    const sent0 = (shown as { diagnostics: { kind: string; request_id?: string }[] }).diagnostics;
    expect(sent0.some((e) => e.kind === 'http_failure' && e.request_id === '0123456789abcdef')).toBe(true);
    expect(JSON.stringify(shown)).not.toContain(secret);

    fireEvent.click(screen.getByRole('button', { name: 'Send report' }));
    await screen.findByText(/Your report was sent as number 7/);
    const posts = calls.filter((c) => c.method === 'POST' && c.path === '/v1/reports');
    expect(posts).toHaveLength(1);
    expect(posts[0]?.body).toEqual(shown);
  });

  it('can go back and change the answers before sending', async () => {
    const calls = serve(() => json({ id: 8, created_at: '2026-10-16T09:00:00.000Z' }, 201));
    const form = await signInAndOpen();
    fill(form);
    fireEvent.click(within(form).getByRole('button', { name: 'Review report' }));
    fireEvent.click(await screen.findByRole('button', { name: 'Edit' }));
    const again = screen.getAllByRole('form', { name: 'Report a problem' })[0];
    if (again === undefined) throw new Error('no form');
    expect(within(again).getByLabelText<HTMLTextAreaElement>('What happened instead').value).toBe('Silence');
    expect(calls.filter((c) => c.path === '/v1/reports')).toEqual([]);
  });

  it.each([
    ['too many reports', () => json({ code: 'rate_limited', message: 'slow down' }, 429, { 'Retry-After': '120' }), 'You have sent several reports recently. Try again in 2 minutes.'],
    ['a refusal', () => json({ code: 'invalid_request', message: 'schema: report did is required' }, 400), 'The server refused the report: schema: report did is required'],
  ])('says so on %s, and keeps the report', async (_case, answer, line) => {
    serve(answer);
    const form = await signInAndOpen();
    fill(form);
    fireEvent.click(within(form).getByRole('button', { name: 'Review report' }));
    fireEvent.click(await screen.findByRole('button', { name: 'Send report' }));
    await screen.findByText(line);
    expect(screen.getByLabelText('The report as it will be sent')).toBeDefined();
  });

  it('is offered where the page says it cannot reach the server', () => {
    serve(() => json({}, 500));
    render(
      <Lobby
        origin={origin}
        name="ada"
        channels={initialChannels}
        unread={new Map()}
        connection="reconnecting"
        conversation={null}
        panel={null}
        voice={null}
        invites={null}
        report={<ReportProblem onUnauthorized={() => undefined} />}
        onOpen={() => undefined}
        onRetry={() => undefined}
        onSignOut={() => undefined}
        onSignOutEverywhere={() => undefined}
        signOutProblem={null}
      />,
    );
    const notice = screen.getByText('Cannot reach the server').parentElement;
    if (notice === null) throw new Error('no notice');
    expect(within(notice).getByRole('button', { name: 'Try again' })).toBeDefined();
    fireEvent.click(within(notice).getByRole('button', { name: 'Report a problem' }));
    expect(within(notice).getByRole('form', { name: 'Report a problem' })).toBeDefined();
    // And in the header, beside the sign-outs.
    const header = screen.getByRole('button', { name: 'Sign out everywhere' }).closest('header');
    if (header === null) throw new Error('no header');
    expect(within(header).getByRole('button', { name: 'Report a problem' })).toBeDefined();
  });

  it('builds a report the server takes even when the page knows little of itself', () => {
    const report = buildReport({ did: ' a ', expected: 'b', happened: 'c', often: 'once' });
    expect(createProblemReportRequestV1(report)).toEqual([]);
    expect(report.did).toBe('a');
    expect(report.client.name).toBe('conch-web');
    expect(report.client.platform).not.toBe('');
  });
});
