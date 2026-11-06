// Settings → Reports, for an operator (issue #1029): shown to an operator and
// not to a member, the list (empty, and two reports), one report read as
// text, Mark filed (success, an address the schema refuses, a server 400 and
// 403), and a 401. conchd is a stand-in fetch.
import { cleanup, fireEvent, render, screen, waitFor, within } from '@testing-library/react';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { App } from './App';
import { issueRefusal } from './admin/ReportsSection';
import type { WhoAmIResponseV1 } from './schema/principal';
import type { ListProblemReportsResponseV1, ProblemReportV1 } from './schema/report';

const origin = 'http://127.0.0.1:8080';
const key = 'conch.web.token';
const at = '2026-10-23T12:00:00.000Z';
const later = '2099-10-23T12:00:00.000Z';
const operatorToken = 'conch_operator_session_token';
const memberToken = 'conch_member_secret_token';

const operatorWho: WhoAmIResponseV1 = {
  id: 1,
  kind: 'human',
  name: 'owner',
  role: 'operator',
  identity_class: 'operator',
  auth: { scope: 'full', password_set: true, totp_enrolled: true, setup_complete: true, expires_at: later, idle_expires_at: later },
};
const memberWho: WhoAmIResponseV1 = { id: 12, kind: 'human', name: 'member-1', role: 'member', identity_class: 'member', auth: { scope: 'full', password_set: true, setup_complete: true } };
const channelList = { channels: [{ id: 1, name: 'channel-a', created_at: at }] };

const report: ProblemReportV1 = {
  id: 8,
  state: 'open',
  created_at: at,
  principal_id: 12,
  server_version: 'v0.1.0-alpha.9',
  voice: 'ok',
  did: 'Sent a message',
  expected: 'It to arrive',
  happened: 'It said <b>x</b> and then <img src=x onerror=alert(1)>',
  often: 'sometimes',
  client: { name: 'conch-web', version: 'v0.1.0-alpha.9', user_agent: 'agent', platform: 'Linux x86_64', screen: '1280x800', voice: 'idle' },
  diagnostics: [],
  server: { requests: [], socket_closes: [] },
};
const filedReport: ProblemReportV1 = { ...report, id: 7, state: 'filed', issue: 'https://example.test/issues/900', filed_at: later };
const list: ListProblemReportsResponseV1 = {
  reports: [
    { id: 8, state: 'open', created_at: at, principal_id: 12, client_name: 'conch-web', client_version: 'v0.1.0-alpha.9', server_version: 'v0.1.0-alpha.9', often: 'sometimes' },
    { id: 7, state: 'filed', created_at: at, principal_id: 13, client_name: 'conch', client_version: 'dev-0123', server_version: 'v0.1.0-alpha.9', often: 'once', issue: 'https://example.test/issues/900' },
  ],
};

const json = (body: unknown, status = 200) => new Response(JSON.stringify(body), { status });
const refusal = (status: number, code: string, message = 'refused') => json({ code, message }, status);

interface Call {
  readonly method: string;
  readonly path: string;
  readonly auth: string | null;
  readonly body: unknown;
}
type Route = (call: Call) => Response | Promise<Response>;

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

function people(by: Record<string, WhoAmIResponseV1>): Record<string, Route> {
  const who = (c: Call) => by[(c.auth ?? '').replace(/^Bearer /, '')];
  return {
    'GET /v1/whoami': (c) => {
      const found = who(c);
      return found === undefined ? json({}, 401) : json(found);
    },
    'GET /v1/channels': (c) => (who(c) === undefined ? json({}, 401) : json(channelList)),
  };
}

async function settings(token: string, place: 'tab' | 'lasting') {
  (place === 'tab' ? window.sessionStorage : window.localStorage).setItem(key, token);
  const view = render(<App origin={origin} />);
  fireEvent.click(await screen.findByRole('button', { name: 'Settings' }));
  return view;
}

async function openReports(routes: Record<string, Route>) {
  const calls = serve({ ...people({ [operatorToken]: operatorWho }), ...routes });
  await settings(operatorToken, 'tab');
  fireEvent.click(await screen.findByRole('button', { name: 'Reports' }));
  return { calls, region: screen.getByRole('region', { name: 'Reports' }) };
}

async function openReport(number: number, region: HTMLElement) {
  fireEvent.click(await within(region).findByRole('button', { name: `Report #${String(number)}` }));
  return screen.findByRole('group', { name: `Report #${String(number)}` });
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

describe('who is shown Reports', () => {
  it('shows an operator a Reports item', async () => {
    serve({ ...people({ [operatorToken]: operatorWho }) });
    await settings(operatorToken, 'tab');
    expect(await screen.findByRole('button', { name: 'Reports' })).toBeDefined();
  });

  it('shows a member none, and asks the server for nothing', async () => {
    const calls = serve({ ...people({ [memberToken]: memberWho }) });
    await settings(memberToken, 'lasting');
    await screen.findByRole('button', { name: 'Sign out' });
    expect(screen.queryByRole('button', { name: 'Reports' })).toBeNull();
    expect(calls.some((c) => c.path.startsWith('/v1/reports'))).toBe(false);
  });
});

describe('the list', () => {
  it('says so when there are no reports', async () => {
    const { region } = await openReports({ 'GET /v1/reports': () => json({ reports: [] }) });
    expect(await within(region).findByText('No reports yet.')).toBeDefined();
  });

  it('shows each report: number, state, sender, client and how often, with a filed one’s issue as a link', async () => {
    const { calls, region } = await openReports({ 'GET /v1/reports': () => json(list) });
    const items = await within(region).findAllByRole('listitem');
    expect(items).toHaveLength(2);
    expect(calls.find((c) => c.path === '/v1/reports')?.auth).toBe(`Bearer ${operatorToken}`);
    // In the server's order, newest first; a sender the page does not know is its number.
    expect(items[0]?.textContent).toContain('Report #8');
    expect(items[0]?.textContent).toContain('Open');
    expect(items[0]?.textContent).toContain('person #12');
    expect(items[0]?.textContent).toContain('conch-web v0.1.0-alpha.9');
    expect(items[0]?.textContent).toContain('Sometimes');
    expect(items[1]?.textContent).toContain('Filed');
    expect(items[1]?.textContent).toContain('Once so far');
    const link = within(items[1] as HTMLElement).getByRole('link', { name: 'https://example.test/issues/900' });
    expect(link.getAttribute('href')).toBe('https://example.test/issues/900');
    expect(link.getAttribute('rel')).toBe('noopener noreferrer');
  });

  it('shows an issue address the link rules refuse as plain text', async () => {
    const odd = 'https://exa​mple.test/issues/1';
    const { region } = await openReports({
      'GET /v1/reports': () => json({ reports: [{ ...list.reports[1], issue: odd }] }),
    });
    const items = await within(region).findAllByRole('listitem');
    expect(within(items[0] as HTMLElement).queryByRole('link')).toBeNull();
    expect(items[0]?.textContent).toContain(odd);
  });

  it('follows the sign-out path on a 401', async () => {
    serve({ ...people({ [operatorToken]: operatorWho }), 'GET /v1/reports': () => json({}, 401) });
    await settings(operatorToken, 'tab');
    fireEvent.click(await screen.findByRole('button', { name: 'Reports' }));
    await waitFor(() => {
      expect(window.sessionStorage.getItem(key)).toBeNull();
    });
  });

  it('hides the section with one line on a 403', async () => {
    await openReports({ 'GET /v1/reports': () => refusal(403, 'forbidden', 'operator role required') });
    expect(await screen.findByText('Reports are not open to you on this server.')).toBeDefined();
    expect(screen.queryByRole('button', { name: 'Reports' })).toBeNull();
  });
});

describe('one report', () => {
  it('shows its text as text, never as HTML, with the diagnostics behind a disclosure', async () => {
    const { region } = await openReports({ 'GET /v1/reports': () => json(list), 'GET /v1/reports/8': () => json(report) });
    const view = await openReport(8, region);
    expect(within(view).getByText(report.happened)).toBeDefined();
    expect(view.querySelector('b')).toBeNull();
    expect(view.querySelector('img')).toBeNull();
    expect(within(view).getByText('Sent a message')).toBeDefined();
    expect(within(view).getByText('It to arrive')).toBeDefined();
    expect(within(view).getByText('Voice: working')).toBeDefined();
    const details = view.querySelector('details');
    expect(details?.open).toBe(false);
    expect(details?.textContent).toContain('Diagnostics');
  });

  it('shows a filed report’s issue and when it was filed, and no form', async () => {
    const { region } = await openReports({ 'GET /v1/reports': () => json(list), 'GET /v1/reports/7': () => json(filedReport) });
    const view = await openReport(7, region);
    expect(within(view).getByRole('link', { name: 'https://example.test/issues/900' })).toBeDefined();
    expect(within(view).queryByRole('form', { name: 'Mark filed' })).toBeNull();
  });
});

describe('mark filed', () => {
  const mark = (view: HTMLElement, address: string) => {
    fireEvent.change(within(view).getByLabelText('Issue address'), { target: { value: address } });
    fireEvent.click(within(view).getByRole('button', { name: 'Mark filed' }));
  };

  it('sends state filed with the address, then shows the report as filed with its link, in the list too', async () => {
    const filed = { ...report, state: 'filed', issue: 'https://example.test/issues/901', filed_at: later } as const;
    const { calls, region } = await openReports({
      'GET /v1/reports': () => json(list),
      'GET /v1/reports/8': () => json(report),
      'PUT /v1/reports/8': () => json(filed),
    });
    const view = await openReport(8, region);
    mark(view, '  https://example.test/issues/901 ');
    await waitFor(() => {
      expect(within(view).getByRole('link', { name: 'https://example.test/issues/901' })).toBeDefined();
    });
    expect(calls.find((c) => c.method === 'PUT')?.body).toEqual({ state: 'filed', issue: 'https://example.test/issues/901' });
    fireEvent.click(within(view).getByRole('button', { name: 'Back to the reports' }));
    const items = await within(region).findAllByRole('listitem');
    expect(items[0]?.textContent).toContain('Filed');
    expect(within(items[0] as HTMLElement).getByRole('link', { name: 'https://example.test/issues/901' })).toBeDefined();
  });

  it.each(['ftp://example.test/1', 'not an address', 'javascript:alert(1)', 'https:example.test/1', '   ', ''])('refuses %j in the page, with its minimal check, and sends nothing', async (address) => {
    const { calls, region } = await openReports({ 'GET /v1/reports': () => json(list), 'GET /v1/reports/8': () => json(report) });
    const view = await openReport(8, region);
    mark(view, address);
    expect(await within(view).findByText(/The issue address must be/)).toBeDefined();
    expect(calls.some((c) => c.method === 'PUT')).toBe(false);
  });

  it('shows a server 400 by its code, and keeps the report open', async () => {
    const { region } = await openReports({
      'GET /v1/reports': () => json(list),
      'GET /v1/reports/8': () => json(report),
      'PUT /v1/reports/8': () => refusal(400, 'invalid_request', 'schema: report issue must be an http or https address'),
    });
    const view = await openReport(8, region);
    mark(view, 'https://example.test/issues/902');
    expect(await within(view).findByText('The server refused (invalid_request). The report was not marked filed.')).toBeDefined();
    expect(within(view).getByRole('button', { name: 'Mark filed' })).toBeDefined();
  });

  it.each(['https://example.test/%zz', 'https://[fe80::1%25eth0]/issue'])('sends %j, which the page leaves to the server, and shows the server’s answer', async (address) => {
    const { calls, region } = await openReports({
      'GET /v1/reports': () => json(list),
      'GET /v1/reports/8': () => json(report),
      'PUT /v1/reports/8': () => refusal(400, 'invalid_request', 'refused'),
    });
    const view = await openReport(8, region);
    mark(view, address);
    expect(await within(view).findByText('The server refused (invalid_request). The report was not marked filed.')).toBeDefined();
    expect(calls.find((c) => c.method === 'PUT')?.body).toEqual({ state: 'filed', issue: address });
  });

  it('answers to the report the operator is looking at when an earlier choice answers late', async () => {
    const other: ProblemReportV1 = { ...report, id: 9 };
    const withNine: ListProblemReportsResponseV1 = { reports: [...list.reports, { ...(list.reports[0] as ListProblemReportsResponseV1['reports'][number]), id: 9 }] };
    let late: (r: Response) => void = () => undefined;
    const slow = new Promise<Response>((resolve) => {
      late = resolve;
    });
    const { calls, region } = await openReports({
      'GET /v1/reports': () => json(withNine),
      'GET /v1/reports/8': () => slow,
      'GET /v1/reports/9': () => json(other),
      'PUT /v1/reports/9': () => json({ ...other, state: 'filed', issue: 'https://example.test/issues/9', filed_at: later }),
      'PUT /v1/reports/8': () => json({ ...report, state: 'filed', issue: 'https://example.test/issues/9', filed_at: later }),
    });
    fireEvent.click(await within(region).findByRole('button', { name: 'Report #8' }));
    fireEvent.click(await within(region).findByRole('button', { name: 'Report #9' }));
    const view = await screen.findByRole('group', { name: 'Report #9' });
    fireEvent.change(within(view).getByLabelText('Issue address'), { target: { value: 'https://example.test/issues/9' } });
    // The slow answer for report 8 arrives now.
    late(json(report));
    await new Promise((resolve) => setTimeout(resolve, 20));
    expect(screen.queryByRole('group', { name: 'Report #8' })).toBeNull();
    const still = screen.getByRole('group', { name: 'Report #9' });
    fireEvent.click(within(still).getByRole('button', { name: 'Mark filed' }));
    await waitFor(() => {
      expect(calls.some((c) => c.method === 'PUT')).toBe(true);
    });
    expect(calls.filter((c) => c.method === 'PUT').map((c) => c.path)).toEqual(['/v1/reports/9']);
  });

  it('keeps the report now open, and its typed address, when an earlier report’s filing answers late', async () => {
    const other: ProblemReportV1 = { ...report, id: 9 };
    const withNine: ListProblemReportsResponseV1 = { reports: [...list.reports, { ...(list.reports[0] as ListProblemReportsResponseV1['reports'][number]), id: 9 }] };
    let answer: (r: Response) => void = () => undefined;
    const slow = new Promise<Response>((resolve) => {
      answer = resolve;
    });
    const { region } = await openReports({
      'GET /v1/reports': () => json(withNine),
      'GET /v1/reports/8': () => json(report),
      'GET /v1/reports/9': () => json(other),
      'PUT /v1/reports/8': () => slow,
    });
    const first = await openReport(8, region);
    mark(first, 'https://example.test/issues/8');
    fireEvent.click(within(first).getByRole('button', { name: 'Back to the reports' }));
    const second = await openReport(9, region);
    fireEvent.change(within(second).getByLabelText('Issue address'), { target: { value: 'https://example.test/issues/9' } });
    answer(json({ ...report, state: 'filed', issue: 'https://example.test/issues/8', filed_at: later }));
    await new Promise((resolve) => setTimeout(resolve, 20));
    // Report 9 is still shown, with what was typed for it.
    const still = screen.getByRole('group', { name: 'Report #9' });
    expect(within(still).getByLabelText<HTMLInputElement>('Issue address').value).toBe('https://example.test/issues/9');
    // Report 8's row says Filed.
    fireEvent.click(within(still).getByRole('button', { name: 'Back to the reports' }));
    const row = (await within(region).findByRole('button', { name: 'Report #8' })).closest('li');
    expect(row?.textContent).toContain('Filed');
    expect(row?.textContent).toContain('https://example.test/issues/8');
  });

  const filedEight = { ...report, state: 'filed', issue: 'https://example.test/issues/8', filed_at: later } as const;

  it('leaves a report’s row Filed when a list read that began before the filing lands after it', async () => {
    let reads = 0;
    let answer: (r: Response) => void = () => undefined;
    const slow = new Promise<Response>((resolve) => {
      answer = resolve;
    });
    const { region } = await openReports({
      'GET /v1/reports': () => (++reads === 2 ? slow : json(list)),
      'GET /v1/reports/8': () => json(report),
      'PUT /v1/reports/8': () => json(filedEight),
    });
    // Close and reopen the section: a second list read starts and stays pending.
    await within(region).findAllByRole('listitem');
    fireEvent.click(screen.getByRole('button', { name: 'Reports' }));
    fireEvent.click(screen.getByRole('button', { name: 'Reports' }));
    await waitFor(() => {
      expect(reads).toBe(2);
    });
    const again = await openReport(8, region);
    mark(again, 'https://example.test/issues/8');
    await waitFor(() => {
      expect(within(again).getByRole('link', { name: 'https://example.test/issues/8' })).toBeDefined();
    });
    answer(json(list));
    await new Promise((resolve) => setTimeout(resolve, 20));
    fireEvent.click(within(region).getByRole('button', { name: 'Back to the reports' }));
    const row = (await within(region).findByRole('button', { name: 'Report #8' })).closest('li');
    expect(row?.textContent).toContain('Filed');
    expect(row?.textContent).toContain('https://example.test/issues/8');
  });

  it('shows a report as filed, with no form, when a read of it that began before the filing lands after it', async () => {
    let gets = 0;
    let answer: (r: Response) => void = () => undefined;
    const slow = new Promise<Response>((resolve) => {
      answer = resolve;
    });
    const { region } = await openReports({
      'GET /v1/reports': () => json(list),
      'GET /v1/reports/8': () => (++gets === 2 ? slow : json(report)),
      'PUT /v1/reports/8': () => json(filedEight),
    });
    const first = await openReport(8, region);
    mark(first, 'https://example.test/issues/8');
    await waitFor(() => {
      expect(within(first).getByRole('link', { name: 'https://example.test/issues/8' })).toBeDefined();
    });
    fireEvent.click(within(first).getByRole('button', { name: 'Back to the reports' }));
    // Reopened, and the read is still pending; it answers with the old, open report.
    fireEvent.click(await within(region).findByRole('button', { name: 'Report #8' }));
    await waitFor(() => {
      expect(gets).toBe(2);
    });
    answer(json(report));
    const view = await screen.findByRole('group', { name: 'Report #8' });
    expect(within(view).getByRole('link', { name: 'https://example.test/issues/8' })).toBeDefined();
    expect(within(view).queryByRole('form', { name: 'Mark filed' })).toBeNull();
  });

  it('hides the section on a 403', async () => {
    const { region } = await openReports({
      'GET /v1/reports': () => json(list),
      'GET /v1/reports/8': () => json(report),
      'PUT /v1/reports/8': () => refusal(403, 'forbidden', 'operator role required'),
    });
    const view = await openReport(8, region);
    mark(view, 'https://example.test/issues/903');
    expect(await screen.findByText('Reports are not open to you on this server.')).toBeDefined();
    expect(screen.queryByRole('button', { name: 'Reports' })).toBeNull();
  });
});

describe('the issue address pre-check', () => {
  const bytes = 'The issue address must be 1 to 512 bytes.';
  const scheme = 'The issue address must be an http or https address.';
  // "é" is two bytes: 8 bytes of "https://" and the rest to 512 or 513.
  const padded = (total: number) => `https://${'é'.repeat((total - 8) / 2)}${total % 2 === 0 ? '' : 'a'}`;
  it.each([
    ['https://example.test/1', null],
    ['HTTP://example.test/1', null],
    ['https://[fe80::1%25eth0]/issue', null],
    ['https://example.test/%zz', null],
    ['ftp://example.test/1', scheme],
    ['https:example.test/1', scheme],
    ['', bytes],
    ['   ', bytes],
    [padded(512), null],
    [padded(513), bytes],
    [`  ${padded(512)}  `, null],
  ])('%j', (address, refusalLine) => {
    expect(issueRefusal(address)).toBe(refusalLine);
  });

  it('counts bytes, not characters', () => {
    expect(new TextEncoder().encode(padded(512)).length).toBe(512);
    expect(padded(512).length).toBeLessThan(512);
    expect(new TextEncoder().encode(padded(513)).length).toBe(513);
  });
});
