// Pausing and resuming every invite in Settings → Joining (issue #882): the
// state read from the server, a change sent and the answer shown, the recent
// proof asked for over the page (a password alone for an operator with no
// code), and a refusal that leaves the switch as the server last said.
// conchd is a stand-in fetch.
import { cleanup, fireEvent, render, screen, waitFor, within } from '@testing-library/react';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { App } from './App';
import type { WhoAmIResponseV1 } from './schema/principal';

const origin = 'http://127.0.0.1:8080';
const key = 'conch.web.token';
const at = '2026-10-28T12:00:00.000Z';
const later = '2099-10-28T12:00:00.000Z';
const token = 'conch_operator_session_token';
const password = 'an operator passphrase long enough';
const pausedLine = 'Invites are paused: no one can make, preview or use an invite.';

const operatorWho = (enrolled: boolean): WhoAmIResponseV1 => ({
  id: 1,
  kind: 'human',
  name: 'root',
  role: 'operator',
  identity_class: 'operator',
  auth: { scope: 'full', password_set: true, totp_enrolled: enrolled, setup_complete: true, expires_at: later, idle_expires_at: later },
});
const joinSettings = {
  settings: {
    open_joining: 'off',
    guests_enabled: false,
    joining_paused: false,
    join_voice: false,
    max_guests: 50,
    max_guests_per_address: 5,
    guest_idle_seconds: 7200,
    guest_absolute_seconds: 86400,
    open_channels: [],
  },
  server_name: 'conch.example',
  live_guests: 0,
  warnings: [],
};

const json = (body: unknown, status = 200) => new Response(JSON.stringify(body), { status });
const refusal = (status: number, code: string, message = 'refused') => json({ code, message }, status);

interface Call {
  readonly method: string;
  readonly path: string;
  readonly body: unknown;
}
type Route = (call: Call) => Response | Promise<Response>;

function serve(routes: Record<string, Route>, enrolled = true): Call[] {
  const calls: Call[] = [];
  const all: Record<string, Route> = {
    'GET /v1/whoami': () => json(operatorWho(enrolled)),
    'GET /v1/channels': () => json({ channels: [{ id: 1, name: 'lobby', created_at: at }] }),
    'GET /v1/join/settings': () => json(joinSettings),
    'GET /v1/guests': () => json({ guests: [] }),
    ...routes,
  };
  vi.stubGlobal(
    'fetch',
    vi.fn(async (input: RequestInfo | URL, init?: RequestInit) => {
      const raw = typeof input === 'string' ? input : input instanceof URL ? input.href : input.url;
      const path = new URL(raw, origin).pathname;
      const method = init?.method ?? 'GET';
      const body: unknown = typeof init?.body === 'string' ? JSON.parse(init.body) : undefined;
      const call = { method, path, body };
      calls.push(call);
      const route = all[`${method} ${path}`];
      return route === undefined ? json({}, 404) : route(call);
    }),
  );
  return calls;
}

// opened shows the joining section and returns its region and the switch.
async function opened() {
  window.sessionStorage.setItem(key, token);
  render(<App origin={origin} />);
  fireEvent.click(await screen.findByRole('button', { name: 'Settings' }));
  fireEvent.click(await screen.findByRole('button', { name: 'Joining and guests' }));
  const region = screen.getByRole('region', { name: 'Joining and guests' });
  const box = await within(region).findByLabelText<HTMLInputElement>('Pause all invites');
  await waitFor(() => {
    expect(box.disabled).toBe(false);
  });
  return { region, box };
}

const puts = (calls: Call[]) => calls.filter((c) => c.method === 'PUT' && c.path === '/v1/invites/settings');

beforeEach(() => {
  window.sessionStorage.clear();
  window.localStorage.clear();
  window.location.hash = '';
});
afterEach(() => {
  cleanup();
  vi.unstubAllGlobals();
});

describe('Settings → Joining: pause all invites', () => {
  it.each([
    { paused: false, checked: false, line: /Invites are on/ },
    { paused: true, checked: true, line: new RegExp(pausedLine) },
  ])('shows paused=$paused as the server says', async ({ paused, checked, line }) => {
    serve({ 'GET /v1/invites/settings': () => json({ paused }) });
    const { region, box } = await opened();
    expect(box.checked).toBe(checked);
    expect(within(region).getByText(line)).toBeDefined();
  });

  it('resume sends paused false and shows the answer', async () => {
    const calls = serve({ 'GET /v1/invites/settings': () => json({ paused: true }), 'PUT /v1/invites/settings': () => json({ paused: false }) });
    const { region, box } = await opened();
    fireEvent.click(box);
    await within(region).findByText(/Invites are on/);
    expect(puts(calls).map((c) => c.body)).toEqual([{ paused: false }]);
    expect(box.checked).toBe(false);
  });

  it('pause sends paused true, and shows the server’s answer rather than the click', async () => {
    // The server answers with the value it already had: the box does not follow the click.
    const calls = serve({ 'GET /v1/invites/settings': () => json({ paused: false }), 'PUT /v1/invites/settings': () => json({ paused: false }) });
    const { box } = await opened();
    fireEvent.click(box);
    await waitFor(() => {
      expect(puts(calls)).toHaveLength(1);
    });
    expect(puts(calls)[0]?.body).toEqual({ paused: true });
    await waitFor(() => {
      expect(box.disabled).toBe(false);
    });
    expect(box.checked).toBe(false);
  });

  it('pause shows the paused line', async () => {
    serve({ 'GET /v1/invites/settings': () => json({ paused: false }), 'PUT /v1/invites/settings': () => json({ paused: true }) });
    const { region, box } = await opened();
    fireEvent.click(box);
    await within(region).findByText(pausedLine);
    expect(box.checked).toBe(true);
  });

  it('asks for the password and a code over the page, then sends the same change again', async () => {
    let proved = false;
    const calls = serve({
      'GET /v1/invites/settings': () => json({ paused: true }),
      'PUT /v1/invites/settings': () => (proved ? json({ paused: false }) : refusal(403, 'reauth_required')),
      'POST /v1/reauth': () => {
        proved = true;
        return json({ until: later });
      },
    });
    const { region, box } = await opened();
    fireEvent.click(box);
    const dialog = await screen.findByRole('dialog', { name: 'Confirm it is you' });
    fireEvent.change(within(dialog).getByLabelText('Your password'), { target: { value: password } });
    fireEvent.change(within(dialog).getByLabelText('Code from your authenticator app'), { target: { value: '654321' } });
    fireEvent.click(within(dialog).getByRole('button', { name: 'Confirm' }));
    await within(region).findByText(/Invites are on/);
    expect(puts(calls).map((c) => c.body)).toEqual([{ paused: false }, { paused: false }]);
    expect(calls.filter((c) => c.path === '/v1/reauth').map((c) => c.body)).toEqual([{ password, code: '654321' }]);
  });

  it('asks a password-only operator for the password alone', async () => {
    let proved = false;
    const calls = serve(
      {
        'GET /v1/invites/settings': () => json({ paused: true }),
        'PUT /v1/invites/settings': () => (proved ? json({ paused: false }) : refusal(403, 'reauth_required')),
        'POST /v1/reauth': () => {
          proved = true;
          return json({ until: later });
        },
      },
      false,
    );
    const { region, box } = await opened();
    fireEvent.click(box);
    const dialog = await screen.findByRole('dialog', { name: 'Confirm it is you' });
    expect(within(dialog).queryByLabelText('Code from your authenticator app')).toBeNull();
    fireEvent.change(within(dialog).getByLabelText('Your password'), { target: { value: password } });
    fireEvent.click(within(dialog).getByRole('button', { name: 'Confirm' }));
    await within(region).findByText(/Invites are on/);
    expect(calls.filter((c) => c.path === '/v1/reauth').map((c) => c.body)).toEqual([{ password }]);
  });

  it('Cancel leaves the switch as the server last said, with one line', async () => {
    const calls = serve({ 'GET /v1/invites/settings': () => json({ paused: true }), 'PUT /v1/invites/settings': () => refusal(403, 'reauth_required') });
    const { region, box } = await opened();
    fireEvent.click(box);
    const dialog = await screen.findByRole('dialog', { name: 'Confirm it is you' });
    fireEvent.click(within(dialog).getByRole('button', { name: 'Cancel' }));
    await within(region).findByText('It needs your password first. Invites were not changed.');
    expect(box.checked).toBe(true);
    expect(puts(calls)).toHaveLength(1);
    expect(calls.filter((c) => c.path === '/v1/reauth')).toEqual([]);
  });

  it('a change whose answer is lost is read again, not assumed', async () => {
    // The write reaches the server, then the connection drops before the answer.
    let held = true;
    serve({
      'GET /v1/invites/settings': () => json({ paused: held }),
      'PUT /v1/invites/settings': (call) => {
        held = (call.body as { paused: boolean }).paused;
        throw new TypeError('network down');
      },
    });
    const { region, box } = await opened();
    fireEvent.click(box);
    await within(region).findByText(/could not be reached, so it is not known whether invites changed/);
    const again = await within(region).findByLabelText<HTMLInputElement>('Pause all invites');
    expect(again.checked).toBe(false);
    expect(within(region).getByText(/Invites are on/)).toBeDefined();
  });

  it('a change answered 200 whose body is cut off is read again, not called a refusal', async () => {
    let held = true;
    const cut = () =>
      new Response(
        new ReadableStream({
          start(controller) {
            controller.error(new TypeError('connection dropped'));
          },
        }),
        { status: 200 },
      );
    serve({
      'GET /v1/invites/settings': () => json({ paused: held }),
      'PUT /v1/invites/settings': (call) => {
        held = (call.body as { paused: boolean }).paused;
        return cut();
      },
    });
    const { region, box } = await opened();
    fireEvent.click(box);
    await within(region).findByText(/answer could not be read, so it is not known whether invites changed/);
    expect(within(region).queryByText(/Invites were not changed/)).toBeNull();
    const again = await within(region).findByLabelText<HTMLInputElement>('Pause all invites');
    expect(again.checked).toBe(false);
  });

  it('a change whose answer is lost, with the server still unreachable, shows no state', async () => {
    let reads = 0;
    serve({
      'GET /v1/invites/settings': () => {
        reads++;
        if (reads > 1) throw new TypeError('network down');
        return json({ paused: true });
      },
      'PUT /v1/invites/settings': () => {
        throw new TypeError('network down');
      },
    });
    const { region, box } = await opened();
    fireEvent.click(box);
    await within(region).findByText('Cannot reach the server.');
    expect(within(region).queryByLabelText('Pause all invites')).toBeNull();
    expect(within(region).getByText('Whether invites are paused is not known.')).toBeDefined();
  });

  it('shows no switch until the server has answered, nor when the read fails', async () => {
    let answer: (r: Response) => void = () => undefined;
    serve({ 'GET /v1/invites/settings': () => new Promise<Response>((resolve) => (answer = resolve)) });
    window.sessionStorage.setItem(key, token);
    render(<App origin={origin} />);
    fireEvent.click(await screen.findByRole('button', { name: 'Settings' }));
    fireEvent.click(await screen.findByRole('button', { name: 'Joining and guests' }));
    const region = screen.getByRole('region', { name: 'Joining and guests' });
    await within(region).findByText('Loading whether invites are paused…');
    expect(within(region).queryByLabelText('Pause all invites')).toBeNull();
    answer(refusal(500, 'internal'));
    await within(region).findByText('Could not read whether invites are paused.');
    expect(within(region).queryByLabelText('Pause all invites')).toBeNull();
  });

  it('a read wanted while a change is in flight waits for it, and shows what it left', async () => {
    let held = true;
    let finish: () => void = () => undefined;
    const calls = serve({
      'GET /v1/invites/settings': () => json({ paused: held }),
      'PUT /v1/invites/settings': (call) =>
        new Promise<Response>((resolve) => {
          finish = () => {
            held = (call.body as { paused: boolean }).paused;
            resolve(json({ paused: held }));
          };
        }),
    });
    const { region, box } = await opened();
    fireEvent.click(box);
    await waitFor(() => {
      expect(puts(calls)).toHaveLength(1);
    });
    // Close and open the section while the change is in flight.
    const toggle = screen.getByRole('button', { name: 'Joining and guests' });
    fireEvent.click(toggle);
    fireEvent.click(toggle);
    const reads = () => calls.filter((c) => c.method === 'GET' && c.path === '/v1/invites/settings').length;
    expect(reads()).toBe(1);
    finish();
    await waitFor(() => {
      expect(reads()).toBe(2);
    });
    const again = await within(region).findByLabelText<HTMLInputElement>('Pause all invites');
    await waitFor(() => {
      expect(again.checked).toBe(false);
    });
  });

  it('another refusal keeps the old value and says so', async () => {
    serve({ 'GET /v1/invites/settings': () => json({ paused: false }), 'PUT /v1/invites/settings': () => refusal(500, 'internal') });
    const { region, box } = await opened();
    fireEvent.click(box);
    await within(region).findByText('The server refused. Invites were not changed.');
    expect(box.checked).toBe(false);
  });
});
