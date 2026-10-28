// Temporary servers and password-only operators in the browser (ADR-011
// items 2.3 and 3, ADR-012 item 5, issue #867): the banner, Skip on the setup
// page, the setup link, and the code field that an operator sees only with an
// authenticator enrolled. conchd is a stand-in fetch.
import { cleanup, fireEvent, render, screen, waitFor, within } from '@testing-library/react';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import * as api from './api';
import { App } from './App';
import { PasswordSettings } from './Settings';
import { Reauth } from './Reauth';
import { temporaryLine } from './ServerVersion';
import { noticeText } from './state/auth';
import type { WhoAmIResponseV1 } from './schema/principal';

const origin = 'http://127.0.0.1:8080';
const key = 'conch.web.token';
const at = '2026-10-23T12:00:00.000Z';
const later = '2099-10-23T12:00:00.000Z';
const sessionToken = 'conch_operator_session_token';
const setupToken = `conch_setup_${'S'.repeat(43)}`;
const setupCredential = 'conch_setup_credential_token';
const password = 'an operator passphrase long enough';

const operatorWho: WhoAmIResponseV1 = {
  id: 1,
  kind: 'human',
  name: 'root',
  role: 'operator',
  identity_class: 'operator',
  auth: { scope: 'full', password_set: true, totp_enrolled: true, setup_complete: true, expires_at: later, idle_expires_at: later },
};
const passwordOnlyWho: WhoAmIResponseV1 = {
  ...operatorWho,
  auth: { scope: 'full', password_set: true, setup_complete: true, expires_at: later, idle_expires_at: later },
};
const principal = { id: 1, kind: 'human', name: 'root', created_at: at };
const signedIn = {
  next: 'signed_in',
  principal,
  credential: { id: 5, principal_id: 1, label: 'browser', created_at: at, expires_at: later, idle_expires_at: later },
  token: sessionToken,
};
const channelList = { channels: [{ id: 1, name: 'lobby', created_at: at }] };
const frontDoor = { server_name: 'conch.example', join: { guest: false, account: false }, password_login: true, voice: false, guest_idle_seconds: 7200, guest_absolute_seconds: 86400 };
const healthy = { status: 'ok', version: 'v0.1.0', db: 'ok', voice: 'not_configured' };

const json = (body: unknown, status = 200) => new Response(JSON.stringify(body), { status });

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

beforeEach(() => {
  window.sessionStorage.clear();
  window.localStorage.clear();
  window.location.hash = '';
});
afterEach(() => {
  cleanup();
  vi.unstubAllGlobals();
});

describe('the temporary-server banner', () => {
  it('shows on the sign-in page only when /healthz says server_mode temporary', async () => {
    serve({ 'GET /v1/join': () => json(frontDoor), 'GET /healthz': () => json({ ...healthy, server_mode: 'temporary' }) });
    render(<App origin={origin} />);
    await screen.findByRole('heading', { name: 'Sign in' });
    expect(await screen.findByText(temporaryLine)).toBeDefined();
    // Fixed: nothing dismisses it.
    expect(screen.queryByRole('button', { name: /dismiss|close|hide/i })).toBeNull();
  });

  it('shows nothing on a server that only allows password-only operators', async () => {
    serve({ 'GET /v1/join': () => json(frontDoor), 'GET /healthz': () => json(healthy) });
    render(<App origin={origin} />);
    await screen.findByRole('heading', { name: 'Sign in' });
    await screen.findByText(/v0\.1\.0/);
    expect(screen.queryByText(temporaryLine)).toBeNull();
  });

  it('shows in the lobby when whoami says server_mode temporary, and only then', async () => {
    window.sessionStorage.setItem(key, sessionToken);
    serve({ ...people({ [sessionToken]: { ...passwordOnlyWho, server_mode: 'temporary' } }), 'GET /healthz': () => json(healthy) });
    render(<App origin={origin} />);
    const line = await screen.findByText(temporaryLine);
    // In the lobby itself, visible on arrival: not in the Settings drawer,
    // which starts closed.
    expect(line.closest('.drawer-section')).toBeNull();
    expect(line.closest('[hidden], [inert]')).toBeNull();
    expect(within(screen.getByRole('region', { name: 'Conversation' })).getByText(temporaryLine)).toBe(line);
    expect(screen.getAllByText(temporaryLine)).toHaveLength(1);
    cleanup();

    window.sessionStorage.setItem(key, sessionToken);
    serve({ ...people({ [sessionToken]: passwordOnlyWho }), 'GET /healthz': () => json(healthy) });
    render(<App origin={origin} />);
    await screen.findByText(/v0\.1\.0/);
    expect(screen.queryByText(temporaryLine)).toBeNull();
  });
});

describe('a password-only operator signs in', () => {
  it('lands signed in on a login that answers with a session, asking for no code', async () => {
    const calls = serve({
      'GET /v1/join': () => json(frontDoor),
      'POST /v1/login': () => json(signedIn),
      ...people({ [sessionToken]: passwordOnlyWho }),
    });
    render(<App origin={origin} />);
    await screen.findByRole('heading', { name: 'Sign in' });
    fireEvent.change(screen.getByLabelText('Name'), { target: { value: 'root' } });
    fireEvent.change(screen.getByLabelText('Password'), { target: { value: password } });
    fireEvent.click(screen.getByRole('button', { name: 'Sign in' }));
    await waitFor(() => {
      expect(screen.queryByRole('status', { name: 'Status' })?.textContent).toContain('root');
    });
    expect(screen.queryByLabelText('Code from your authenticator app')).toBeNull();
    expect(calls.some((c) => c.path === '/v1/login/totp')).toBe(false);
  });
});

describe('an operator’s setup: Skip', () => {
  const enrollment = { secret: 'JBSWY3DPEHPK3PXPJBSWY3DPEHPK3PXP', issuer: 'conch.example', account: 'root@conch.example', algorithm: 'SHA1', digits: 6, period_seconds: 30, uri: 'otpauth://totp/x' };

  // setupServer: passwordOnlyAllowed says whether setup is complete once the
  // password is set (the server's rule), as whoami reports it.
  function setupServer(passwordOnlyAllowed: boolean, scope: 'setup' | 'full' = 'setup') {
    const state = { password_set: false, totp_enrolled: false, ended: false };
    const auth = () => ({
      scope,
      password_set: state.password_set,
      totp_enrolled: state.totp_enrolled,
      setup_complete: state.password_set && (state.totp_enrolled || passwordOnlyAllowed),
    });
    const calls = serve({
      'GET /v1/join': () => json(frontDoor),
      'POST /v1/setup/redeem': (c) =>
        (c.body as { token?: string }).token === setupToken
          ? json({ principal, credential: { id: 9, principal_id: 1, label: 'setup', created_at: at, scope: 'setup' }, token: setupCredential, auth: auth() }, 201)
          : json({ code: 'setup_token_invalid', message: 'no' }, 404),
      'GET /v1/whoami': (c) => (c.auth === `Bearer ${setupCredential}` && !state.ended ? json({ ...operatorWho, auth: auth() }) : json({}, 401)),
      'POST /v1/password/mine': () => {
        state.password_set = true;
        return json({ revoked: 0 });
      },
      'POST /v1/totp/enroll': () => json(enrollment),
      'DELETE /v1/credentials/current': (c) => {
        if (c.auth !== `Bearer ${setupCredential}`) return json({}, 401);
        state.ended = true;
        return new Response(null, { status: 204 });
      },
    });
    return { calls, state };
  }

  async function toAuthenticatorStep() {
    fireEvent.click(await screen.findByRole('button', { name: 'Set up an operator account' }));
    fireEvent.change(screen.getByLabelText('Setup token'), { target: { value: setupToken } });
    fireEvent.click(screen.getByRole('button', { name: 'Continue' }));
    fireEvent.change(await screen.findByLabelText('Choose a password'), { target: { value: password } });
    fireEvent.change(screen.getByLabelText('Type it again'), { target: { value: password } });
    fireEvent.click(screen.getByRole('button', { name: 'Set password' }));
    await screen.findByRole('heading', { name: 'Your authenticator app' });
  }

  it('offers Skip when setup is complete without an authenticator; Skip ends the credential and says to sign in with name and password', async () => {
    const { calls } = setupServer(true);
    const { container } = render(<App origin={origin} />);
    await toAuthenticatorStep();
    fireEvent.click(screen.getByRole('button', { name: 'Skip' }));
    await screen.findByRole('heading', { name: 'Setup is complete' });
    const gone = calls.filter((c) => c.method === 'DELETE');
    expect(gone).toEqual([{ method: 'DELETE', path: '/v1/credentials/current', auth: `Bearer ${setupCredential}`, body: undefined }]);
    expect(screen.getByText(/Sign in with your name and your password\./)).toBeDefined();
    expect(screen.queryByText(/a code from the app/)).toBeNull();
    fireEvent.click(screen.getByRole('button', { name: 'Go to sign-in' }));
    expect(await screen.findByText(noticeText('setupDone'))).toBeDefined();
    expect(window.localStorage.length + window.sessionStorage.length).toBe(0);
    expect(container.innerHTML).not.toContain(setupCredential);
  });

  it('still enrols and confirms beside Skip', async () => {
    setupServer(true);
    render(<App origin={origin} />);
    await toAuthenticatorStep();
    fireEvent.click(screen.getByRole('button', { name: 'Show the key' }));
    expect(await screen.findByText('JBSW Y3DP EHPK 3PXP JBSW Y3DP EHPK 3PXP')).toBeDefined();
    expect(screen.getByLabelText('Code the app shows now')).toBeDefined();
  });

  it('offers no Skip on a normal server, where setup is not complete until an authenticator is enrolled', async () => {
    const { calls } = setupServer(false);
    render(<App origin={origin} />);
    await toAuthenticatorStep();
    expect(screen.queryByRole('button', { name: 'Skip' })).toBeNull();
    expect(calls.some((c) => c.method === 'DELETE')).toBe(false);
  });
});

describe('a setup link', () => {
  it('is taken from the address at once, fills the field, and spends nothing until the click', async () => {
    const calls = serve({
      'GET /v1/join': () => json(frontDoor),
      'POST /v1/setup/redeem': () =>
        json({ principal, credential: { id: 9, principal_id: 1, label: 'setup', created_at: at, scope: 'setup' }, token: setupCredential, auth: { scope: 'setup', password_set: false, setup_complete: false } }, 201),
    });
    window.location.hash = `#/setup/${setupToken}`;
    const { container } = render(<App origin={origin} />);
    const field = await screen.findByLabelText<HTMLInputElement>('Setup token');
    // Out of the address and the history at once.
    expect(window.location.hash).toBe('');
    expect(field.value).toBe(setupToken);
    expect(field.type).toBe('password');
    expect(container.innerHTML).not.toContain(setupToken);
    await waitFor(() => {
      expect(calls.some((c) => c.path === '/v1/join')).toBe(true);
    });
    expect(calls.filter((c) => c.path === '/v1/setup/redeem')).toEqual([]);
    expect(window.localStorage.length + window.sessionStorage.length).toBe(0);

    fireEvent.click(screen.getByRole('button', { name: 'Continue' }));
    await screen.findByLabelText('Choose a password');
    expect(calls.filter((c) => c.path === '/v1/setup/redeem').map((c) => c.body)).toEqual([{ token: setupToken }]);
  });
});

describe('the code field of Reauth and the password change', () => {
  const baseReauth = {
    insecure: false,
    onDone: vi.fn(),
    onUnauthorized: vi.fn(),
  };

  it('Reauth asks an enrolled operator for the code and a password-only one for the password alone', async () => {
    const calls = serve({
      'GET /v1/join': () => json(frontDoor),
      'POST /v1/login': () => json(signedIn),
      'POST /v1/reauth': () => json({ until: later }),
      ...people({ [sessionToken]: passwordOnlyWho }),
    });
    // A held session for the proof to be sent with.
    expect((await api.signInWithPassword('root', password)).ok).toBe(true);

    const enrolled = render(<Reauth operator={true} {...baseReauth} />);
    expect(screen.getByLabelText('Code from your authenticator app')).toBeDefined();
    expect(screen.getByText(/password and a code from your authenticator app/)).toBeDefined();
    enrolled.unmount();

    render(<Reauth operator={false} {...baseReauth} />);
    expect(screen.queryByLabelText('Code from your authenticator app')).toBeNull();
    expect(screen.queryByText(/and a code/)).toBeNull();
    fireEvent.change(screen.getByLabelText('Your password'), { target: { value: password } });
    fireEvent.click(screen.getByRole('button', { name: 'Confirm' }));
    await waitFor(() => {
      expect(baseReauth.onDone).toHaveBeenCalledWith(true);
    });
    expect(calls.filter((c) => c.path === '/v1/reauth').map((c) => c.body)).toEqual([{ password }]);
  });

  it('the password change shows the code field only for an enrolled operator', () => {
    const props = { insecure: false, onChanged: vi.fn(), onUnauthorized: vi.fn() };
    const enrolled = render(<PasswordSettings who={operatorWho} {...props} />);
    fireEvent.click(screen.getByRole('button', { name: 'Password' }));
    expect(within(screen.getByRole('region', { name: 'Password', hidden: true })).getByLabelText('Code from your authenticator app')).toBeDefined();
    enrolled.unmount();

    render(<PasswordSettings who={passwordOnlyWho} {...props} />);
    fireEvent.click(screen.getByRole('button', { name: 'Password' }));
    const region = screen.getByRole('region', { name: 'Password' });
    expect(within(region).getByLabelText('Current password')).toBeDefined();
    expect(within(region).queryByLabelText('Code from your authenticator app')).toBeNull();
  });
});

describe('Skip that fails', () => {
  it('keeps the setup credential: a network or server failure leaves Try again working', async () => {
    let deletes = 0;
    const state = { password_set: false };
    const auth = () => ({ scope: 'setup', password_set: state.password_set, totp_enrolled: false, setup_complete: state.password_set });
    serve({
      'GET /v1/join': () => json(frontDoor),
      'POST /v1/setup/redeem': () =>
        json({ principal, credential: { id: 9, principal_id: 1, label: 'setup', created_at: at, scope: 'setup' }, token: setupCredential, auth: auth() }, 201),
      'GET /v1/whoami': (c) => (c.auth === `Bearer ${setupCredential}` ? json({ ...operatorWho, auth: auth() }) : json({}, 401)),
      'POST /v1/password/mine': () => {
        state.password_set = true;
        return json({ revoked: 0 });
      },
      'DELETE /v1/credentials/current': (c) => {
        deletes++;
        if (c.auth !== `Bearer ${setupCredential}`) return json({}, 401);
        return deletes === 1 ? json({ code: 'internal', message: 'oops' }, 500) : new Response(null, { status: 204 });
      },
    });
    render(<App origin={origin} />);
    fireEvent.click(await screen.findByRole('button', { name: 'Set up an operator account' }));
    fireEvent.change(screen.getByLabelText('Setup token'), { target: { value: setupToken } });
    fireEvent.click(screen.getByRole('button', { name: 'Continue' }));
    fireEvent.change(await screen.findByLabelText('Choose a password'), { target: { value: password } });
    fireEvent.change(screen.getByLabelText('Type it again'), { target: { value: password } });
    fireEvent.click(screen.getByRole('button', { name: 'Set password' }));
    await screen.findByRole('heading', { name: 'Your authenticator app' });
    fireEvent.click(screen.getByRole('button', { name: 'Skip' }));
    await screen.findByText(/The server refused/);
    expect(screen.getByRole('button', { name: 'Skip' })).toBeDefined();
    fireEvent.click(screen.getByRole('button', { name: 'Skip' }));
    await screen.findByRole('heading', { name: 'Setup is complete' });
    expect(deletes).toBe(2);
  });
});

describe('setup links in one tab', () => {
  const second = `conch_setup_${'T'.repeat(43)}`;

  function redeemServer() {
    return serve({
      'GET /v1/join': () => json(frontDoor),
      'POST /v1/setup/redeem': () =>
        json({ principal, credential: { id: 9, principal_id: 1, label: 'setup', created_at: at, scope: 'setup' }, token: setupCredential, auth: { scope: 'setup', password_set: false, setup_complete: false } }, 201),
    });
  }

  it('a second link replaces the first: Continue redeems the second token', async () => {
    const calls = redeemServer();
    window.location.hash = `#/setup/${setupToken}`;
    render(<App origin={origin} />);
    expect((await screen.findByLabelText<HTMLInputElement>('Setup token')).value).toBe(setupToken);
    window.location.hash = `#/setup/${second}`;
    fireEvent(window, new HashChangeEvent('hashchange'));
    await waitFor(() => {
      expect(screen.getByLabelText<HTMLInputElement>('Setup token').value).toBe(second);
    });
    expect(window.location.hash).toBe('');
    fireEvent.click(screen.getByRole('button', { name: 'Continue' }));
    await screen.findByLabelText('Choose a password');
    expect(calls.filter((c) => c.path === '/v1/setup/redeem').map((c) => c.body)).toEqual([{ token: second }]);
  });

  it('a link opened while signed in is dropped: nothing is pre-filled after sign-out', async () => {
    window.sessionStorage.setItem(key, sessionToken);
    const calls = serve({
      'GET /v1/join': () => json(frontDoor),
      'DELETE /v1/credentials/current': () => new Response(null, { status: 204 }),
      ...people({ [sessionToken]: passwordOnlyWho }),
    });
    render(<App origin={origin} />);
    await waitFor(() => {
      expect(screen.queryByRole('status', { name: 'Status' })?.textContent).toContain('root');
    });
    window.location.hash = `#/setup/${setupToken}`;
    fireEvent(window, new HashChangeEvent('hashchange'));
    await waitFor(() => {
      expect(window.location.hash).not.toContain('setup');
    });
    // No setup page over the lobby.
    expect(screen.queryByLabelText('Setup token')).toBeNull();
    fireEvent.click(screen.getByRole('button', { name: 'Settings' }));
    fireEvent.click(screen.getByRole('button', { name: 'Sign out' }));
    fireEvent.click(screen.getByRole('button', { name: 'Yes, sign out' }));
    await screen.findByRole('heading', { name: 'Sign in' });
    fireEvent.click(screen.getByRole('button', { name: 'Set up an operator account' }));
    expect(screen.getByLabelText<HTMLInputElement>('Setup token').value).toBe('');
    expect(calls.filter((c) => c.path === '/v1/setup/redeem')).toEqual([]);
  });
});

describe('a pasted operator session', () => {
  it('is warned about, and Continue keeps it for this tab and pins it: the next calls succeed', async () => {
    const calls = serve({ 'GET /v1/join': () => json(frontDoor), ...people({ [sessionToken]: passwordOnlyWho }) });
    render(<App origin={origin} />);
    fireEvent.change(await screen.findByLabelText('Credential'), { target: { value: sessionToken } });
    fireEvent.click(screen.getByRole('button', { name: 'Use credential' }));
    await screen.findByRole('heading', { name: 'This is an operator credential' });
    // Nothing is kept before Continue.
    expect(window.sessionStorage.getItem(key)).toBeNull();
    fireEvent.click(screen.getByRole('button', { name: 'Continue anyway' }));
    await waitFor(() => {
      expect(screen.queryByRole('status', { name: 'Status' })?.textContent).toContain('root');
    });
    await waitFor(() => {
      expect(calls.some((c) => c.path === '/v1/channels' && c.auth === `Bearer ${sessionToken}`)).toBe(true);
    });
    expect(window.sessionStorage.getItem(key)).toBe(sessionToken);
    expect(window.localStorage.getItem(key)).toBeNull();
  });
});
