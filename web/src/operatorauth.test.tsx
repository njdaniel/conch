// An operator's sign-in in the browser (issue #714, design §5.3): a password
// and then a code, the code's step running out of time and of tries, a
// replayed code, a script's credential that is never taken for an
// operator's sign-in, the session's end with what was being typed kept, the
// recent proof an action asks for (asked once, never in a loop, and over a
// form that keeps what was chosen), and the setup from a host-made token.
// conchd is a stand-in fetch.
import { act, cleanup, fireEvent, render, screen, waitFor, within } from '@testing-library/react';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { App } from './App';
import { setupTokenLine } from './SignIn';
import { noticeText, operatorSession } from './state/auth';
import { insecureLine } from './transport';
import { holdShare } from './voice/heldshare';
import type { WhoAmIResponseV1 } from './schema/principal';

const origin = 'http://127.0.0.1:8080';
const key = 'conch.web.token';
const at = '2026-10-23T12:00:00.000Z';
const later = '2099-10-23T12:00:00.000Z';
const sessionToken = 'conch_operator_session_token';
const scriptToken = 'conch_operator_script_token';
const challenge = `conch_chal_${'C'.repeat(43)}`;
const setupToken = `conch_setup_${'S'.repeat(43)}`;
const setupCredential = 'conch_setup_credential_token';
const password = 'an operator passphrase long enough';
const totpSecret = 'JBSWY3DPEHPK3PXPJBSWY3DPEHPK3PXP';

const operatorWho: WhoAmIResponseV1 = {
  id: 1,
  kind: 'human',
  name: 'root',
  role: 'operator',
  identity_class: 'operator',
  auth: { scope: 'full', password_set: true, totp_enrolled: true, setup_complete: true, expires_at: later, idle_expires_at: later },
};
// The same operator on a script's credential: no idle bound.
const scriptWho: WhoAmIResponseV1 = { ...operatorWho, auth: { scope: 'full', password_set: true, totp_enrolled: true, setup_complete: true } };
const principal = { id: 1, kind: 'human', name: 'root', created_at: at };
const challenged = { next: 'totp', challenge, challenge_expires_at: later };
const signedIn = {
  next: 'signed_in',
  principal,
  credential: { id: 5, principal_id: 1, label: 'browser', created_at: at, expires_at: later, idle_expires_at: later },
  token: sessionToken,
};
const channelList = { channels: [{ id: 1, name: 'lobby', created_at: at }] };
const frontDoor = { server_name: 'conch.example', join: { guest: false, account: false }, password_login: true, voice: false, guest_idle_seconds: 7200, guest_absolute_seconds: 86400 };
const settings = {
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

// people answers whoami and the lobby's reads for each token it names.
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

// operatorServer is a server where root signs in with a password and a code.
function operatorServer(routes: Record<string, Route> = {}): Record<string, Route> {
  return {
    'GET /v1/join': () => json(frontDoor),
    'POST /v1/login': () => json(challenged),
    'POST /v1/login/totp': () => json(signedIn),
    ...people({ [sessionToken]: operatorWho, [scriptToken]: scriptWho }),
    ...routes,
  };
}

function signInWithPassword(name = 'root', typed = password) {
  fireEvent.change(screen.getByLabelText('Name'), { target: { value: name } });
  fireEvent.change(screen.getByLabelText('Password'), { target: { value: typed } });
  fireEvent.click(screen.getByRole('button', { name: 'Sign in' }));
}

function typeCode(code: string) {
  fireEvent.change(screen.getByLabelText('Code from your authenticator app'), { target: { value: code } });
  fireEvent.click(screen.getByRole('button', { name: 'Sign in' }));
}

const statusLine = () => screen.queryByRole('status', { name: 'Status' });

async function operatorSignedIn(routes: Record<string, Route> = {}) {
  const calls = serve(operatorServer(routes));
  const view = render(<App origin={origin} />);
  await screen.findByRole('heading', { name: 'Sign in' });
  signInWithPassword();
  await screen.findByLabelText('Code from your authenticator app');
  typeCode('123456');
  await waitFor(() => {
    expect(statusLine()?.textContent).toContain('root');
  });
  return { calls, view };
}

function expectNoSecret(container: HTMLElement, ...secrets: string[]) {
  for (const secret of secrets) {
    expect(container.innerHTML).not.toContain(secret);
    expect(window.location.href).not.toContain(secret);
  }
}

beforeEach(() => {
  window.sessionStorage.clear();
  window.localStorage.clear();
  window.location.hash = '';
});
afterEach(() => {
  cleanup();
  vi.useRealTimers();
  vi.unstubAllGlobals();
});

describe('an operator signs in with a password and then a code', () => {
  it('a right password alone signs nobody in: the page asks for the code and shows nothing of the server', async () => {
    const calls = serve(operatorServer());
    const { container } = render(<App origin={origin} />);
    await screen.findByRole('heading', { name: 'Sign in' });
    signInWithPassword();
    const field = await screen.findByLabelText('Code from your authenticator app');
    expect(field.getAttribute('autocomplete')).toBe('one-time-code');
    expect(screen.getByText(/You are not signed in yet/).textContent).toContain('root');
    // No lobby, nothing kept, and nothing asked with a credential.
    expect(statusLine()).toBeNull();
    expect(screen.queryByRole('navigation', { name: 'Channels' })).toBeNull();
    expect(window.localStorage.length + window.sessionStorage.length).toBe(0);
    expect(calls.filter((c) => c.auth !== null)).toEqual([]);
    expectNoSecret(container, challenge, password);
  });

  it('signs in on the right code, for this tab alone, with no operator warning', async () => {
    const { calls, view } = await operatorSignedIn();
    expect(calls.find((c) => c.path === '/v1/login/totp')).toMatchObject({ method: 'POST', auth: null, body: { challenge, code: '123456' } });
    expect(window.sessionStorage.getItem(key)).toBe(sessionToken);
    expect(window.localStorage.getItem(key)).toBeNull();
    expect(screen.queryByRole('heading', { name: 'This is an operator credential' })).toBeNull();
    expectNoSecret(view.container, challenge, sessionToken, password);
  });

  it('keeps the step open after a code that is not right, a replayed one included, and signs in on the next', async () => {
    let tries = 0;
    const calls = serve(
      operatorServer({
        'POST /v1/login/totp': () => (++tries < 3 ? refusal(401, 'auth_failed') : json(signedIn)),
      }),
    );
    render(<App origin={origin} />);
    await screen.findByRole('heading', { name: 'Sign in' });
    signInWithPassword();
    await screen.findByLabelText('Code from your authenticator app');
    typeCode('111111');
    expect(await screen.findByText('That code is not right. Type the code the app shows now.')).toBeDefined();
    expect(screen.getByLabelText<HTMLInputElement>('Code from your authenticator app').value).toBe('');
    // The same code again: the server refuses it as it refuses a wrong one.
    typeCode('111111');
    await waitFor(() => {
      expect(calls.filter((c) => c.path === '/v1/login/totp')).toHaveLength(2);
    });
    expect(await screen.findByText('That code is not right. Type the code the app shows now.')).toBeDefined();
    typeCode('222 222');
    await waitFor(() => {
      expect(statusLine()?.textContent).toContain('root');
    });
    // Every try carried the same challenge; the password was sent once.
    expect(calls.filter((c) => c.path === '/v1/login/totp').map((c) => c.body)).toEqual([
      { challenge, code: '111111' },
      { challenge, code: '111111' },
      { challenge, code: '222222' },
    ]);
    expect(calls.filter((c) => c.path === '/v1/login')).toHaveLength(1);
  });

  it('does not send a code that is not six digits', async () => {
    const calls = serve(operatorServer());
    render(<App origin={origin} />);
    await screen.findByRole('heading', { name: 'Sign in' });
    signInWithPassword();
    await screen.findByLabelText('Code from your authenticator app');
    typeCode('12a456');
    expect(await screen.findByText('Type the 6 digits your authenticator app shows for this server.')).toBeDefined();
    expect(calls.filter((c) => c.path === '/v1/login/totp')).toEqual([]);
  });

  it('goes back to the password once the challenge has had all its tries', async () => {
    const calls = serve(operatorServer({ 'POST /v1/login/totp': () => refusal(401, 'auth_failed') }));
    render(<App origin={origin} />);
    await screen.findByRole('heading', { name: 'Sign in' });
    signInWithPassword();
    await screen.findByLabelText('Code from your authenticator app');
    for (let i = 0; i < 4; i++) {
      typeCode('111111');
      await screen.findByText('That code is not right. Type the code the app shows now.');
      await waitFor(() => {
        expect(screen.getByRole('button', { name: 'Sign in' }).getAttribute('aria-busy')).toBe('false');
      });
    }
    typeCode('111111');
    expect(await screen.findByText(noticeText('codeOver'))).toBeDefined();
    expect(screen.getByLabelText('Password')).toBeDefined();
    expect(calls.filter((c) => c.path === '/v1/login/totp')).toHaveLength(5);
    expect(window.localStorage.length + window.sessionStorage.length).toBe(0);
  });

  it('goes back to the password when the challenge runs out of time, sending nothing', async () => {
    const soon = new Date(Date.now() + 1_500).toISOString();
    const calls = serve(operatorServer({ 'POST /v1/login': () => json({ ...challenged, challenge_expires_at: soon }) }));
    render(<App origin={origin} />);
    await screen.findByRole('heading', { name: 'Sign in' });
    signInWithPassword();
    await screen.findByLabelText('Code from your authenticator app');
    expect(await screen.findByText(noticeText('codeOver'), undefined, { timeout: 4_000 })).toBeDefined();
    expect(calls.filter((c) => c.path === '/v1/login/totp')).toEqual([]);
  });

  it('Start again forgets the challenge: a code is not sent with it', async () => {
    const calls = serve(operatorServer());
    render(<App origin={origin} />);
    await screen.findByRole('heading', { name: 'Sign in' });
    signInWithPassword();
    await screen.findByLabelText('Code from your authenticator app');
    fireEvent.click(screen.getByRole('button', { name: 'Start again' }));
    expect(await screen.findByLabelText('Password')).toBeDefined();
    expect(calls.filter((c) => c.path === '/v1/login/totp')).toEqual([]);
  });

  it('takes no code on a page served over plain HTTP beyond this machine', async () => {
    serve(operatorServer());
    render(<App origin="http://192.168.1.20:8080" />);
    expect(await screen.findByText(insecureLine)).toBeDefined();
    expect(screen.queryByLabelText('Password')).toBeNull();
    expect(screen.queryByLabelText('Code from your authenticator app')).toBeNull();
  });
});

describe('a script credential', () => {
  it('is never taken for an operator’s sign-in: it is warned about, and Settings says so', async () => {
    serve(operatorServer());
    render(<App origin={origin} />);
    fireEvent.change(await screen.findByLabelText('Credential'), { target: { value: scriptToken } });
    fireEvent.click(screen.getByRole('button', { name: 'Use credential' }));
    await screen.findByRole('heading', { name: 'This is an operator credential' });
    expect(screen.getByText(/It is a script’s credential, not an operator’s sign-in: no password or code was checked/)).toBeDefined();
    fireEvent.click(screen.getByRole('button', { name: 'Continue anyway' }));
    await waitFor(() => {
      expect(statusLine()?.textContent).toContain('root');
    });
    fireEvent.click(screen.getByRole('button', { name: 'Settings' }));
    expect(await screen.findByText(/This browser uses a script’s credential, not your sign-in/)).toBeDefined();
    expect(operatorSession(scriptWho)).toBe(false);
    expect(operatorSession(operatorWho)).toBe(true);
  });

  it('a setup token pasted as one is not sent', async () => {
    const calls = serve(operatorServer());
    render(<App origin={origin} />);
    fireEvent.change(await screen.findByLabelText('Credential'), { target: { value: setupToken } });
    fireEvent.click(screen.getByRole('button', { name: 'Use credential' }));
    expect(await screen.findByText(setupTokenLine)).toBeDefined();
    expect(calls.some((c) => c.auth !== null || JSON.stringify(c.body ?? null).includes(setupToken))).toBe(false);
  });
});

describe('the operator’s session ends', () => {
  it('says so in the operator’s words when a call is refused', async () => {
    let live = true;
    serve(
      operatorServer({
        'GET /v1/whoami': (c) => (live && c.auth === `Bearer ${sessionToken}` ? json(operatorWho) : json({}, 401)),
        'GET /v1/channels': (c) => (live && c.auth === `Bearer ${sessionToken}` ? json(channelList) : json({}, 401)),
        'GET /v1/credentials/mine': () => json({}, 401),
      }),
    );
    render(<App origin={origin} />);
    await screen.findByRole('heading', { name: 'Sign in' });
    signInWithPassword();
    await screen.findByLabelText('Code from your authenticator app');
    typeCode('123456');
    await waitFor(() => {
      expect(statusLine()?.textContent).toContain('root');
    });
    live = false;
    fireEvent.click(screen.getByRole('button', { name: 'Settings' }));
    fireEvent.click(await screen.findByRole('button', { name: 'Your sign-ins' }));
    expect(await screen.findByText(noticeText('sessionEnded'))).toBeDefined();
    expect(window.sessionStorage.getItem(key)).toBeNull();
  });

  it('asks the server at the session’s twelve hours, and signs out on its 401', async () => {
    const soon = new Date(Date.now() + 200).toISOString();
    let ended = false;
    serve(
      operatorServer({
        'GET /v1/whoami': (c) => (!ended && c.auth === `Bearer ${sessionToken}` ? json({ ...operatorWho, auth: { ...operatorWho.auth, expires_at: soon } }) : json({}, 401)),
        'POST /v1/login/totp': () => json(signedIn),
      }),
    );
    render(<App origin={origin} />);
    await screen.findByRole('heading', { name: 'Sign in' });
    signInWithPassword();
    await screen.findByLabelText('Code from your authenticator app');
    typeCode('123456');
    await waitFor(() => {
      expect(statusLine()?.textContent).toContain('root');
    });
    ended = true;
    expect(await screen.findByText(noticeText('sessionEnded'), undefined, { timeout: 4_000 })).toBeDefined();
  });
});

describe('the recent proof', () => {
  // joining opens Settings → Joining, offers accounts from the address, and saves.
  async function joining() {
    fireEvent.click(screen.getByRole('button', { name: 'Settings' }));
    fireEvent.click(await screen.findByRole('button', { name: 'Joining' }));
    const region = screen.getByRole('region', { name: 'Joining' });
    fireEvent.click(await within(region).findByLabelText('Create an account'));
    fireEvent.click(within(region).getByRole('button', { name: 'Save joining' }));
    return region;
  }

  const saved = { ...settings, settings: { ...settings.settings, open_joining: 'account' } };

  it('asks for the password and a code over the form, then sends the same change once more', async () => {
    let proved = false;
    const { calls, view } = await operatorSignedIn({
      'GET /v1/join/settings': () => json(settings),
      'GET /v1/guests': () => json({ guests: [] }),
      'PUT /v1/join/settings': () => (proved ? json(saved) : refusal(403, 'reauth_required')),
      'POST /v1/reauth': () => {
        proved = true;
        return json({ until: later });
      },
    });
    const region = await joining();
    const dialog = await screen.findByRole('dialog', { name: 'Confirm it is you' });
    // The form underneath keeps what was chosen.
    expect(within(region).getByLabelText<HTMLInputElement>('Create an account').checked).toBe(true);
    fireEvent.change(within(dialog).getByLabelText('Your password'), { target: { value: password } });
    fireEvent.change(within(dialog).getByLabelText('Code from your authenticator app'), { target: { value: '654321' } });
    fireEvent.click(within(dialog).getByRole('button', { name: 'Confirm' }));
    expect(await within(region).findByText('Saved.')).toBeDefined();
    expect(screen.queryByRole('dialog', { name: 'Confirm it is you' })).toBeNull();
    const puts = calls.filter((c) => c.method === 'PUT' && c.path === '/v1/join/settings');
    expect(puts.map((c) => c.body)).toEqual([
      { open_joining: 'account' },
      { open_joining: 'account' },
    ]);
    expect(calls.filter((c) => c.path === '/v1/reauth').map((c) => c.body)).toEqual([{ password, code: '654321' }]);
    expectNoSecret(view.container, password);
  });

  it('asks once, never in a loop: a server that keeps asking gets its refusal shown', async () => {
    const { calls } = await operatorSignedIn({
      'GET /v1/join/settings': () => json(settings),
      'GET /v1/guests': () => json({ guests: [] }),
      'PUT /v1/join/settings': () => refusal(403, 'reauth_required'),
      'POST /v1/reauth': () => json({ until: later }),
    });
    const region = await joining();
    const dialog = await screen.findByRole('dialog', { name: 'Confirm it is you' });
    fireEvent.change(within(dialog).getByLabelText('Your password'), { target: { value: password } });
    fireEvent.change(within(dialog).getByLabelText('Code from your authenticator app'), { target: { value: '654321' } });
    fireEvent.click(within(dialog).getByRole('button', { name: 'Confirm' }));
    expect(await within(region).findByText('It needs your password first. Nothing was changed.')).toBeDefined();
    expect(screen.queryByRole('dialog', { name: 'Confirm it is you' })).toBeNull();
    expect(calls.filter((c) => c.method === 'PUT')).toHaveLength(2);
    expect(calls.filter((c) => c.path === '/v1/reauth')).toHaveLength(1);
  });

  it('a password that is not right keeps the person signed in and the question open', async () => {
    let tries = 0;
    const { calls } = await operatorSignedIn({
      'GET /v1/join/settings': () => json(settings),
      'GET /v1/guests': () => json({ guests: [] }),
      'PUT /v1/join/settings': () => (tries >= 2 ? json(saved) : refusal(403, 'reauth_required')),
      'POST /v1/reauth': () => (++tries < 2 ? refusal(401, 'auth_failed') : json({ until: later })),
    });
    const region = await joining();
    const dialog = await screen.findByRole('dialog', { name: 'Confirm it is you' });
    fireEvent.change(within(dialog).getByLabelText('Your password'), { target: { value: 'not the password at all' } });
    fireEvent.change(within(dialog).getByLabelText('Code from your authenticator app'), { target: { value: '654321' } });
    fireEvent.click(within(dialog).getByRole('button', { name: 'Confirm' }));
    expect(await within(dialog).findByText('The password or the code is not right.')).toBeDefined();
    expect(window.sessionStorage.getItem(key)).toBe(sessionToken);
    expect(within(dialog).getByLabelText<HTMLInputElement>('Your password').value).toBe('');
    fireEvent.change(within(dialog).getByLabelText('Your password'), { target: { value: password } });
    fireEvent.change(within(dialog).getByLabelText('Code from your authenticator app'), { target: { value: '765432' } });
    fireEvent.click(within(dialog).getByRole('button', { name: 'Confirm' }));
    expect(await within(region).findByText('Saved.')).toBeDefined();
    expect(calls.filter((c) => c.method === 'PUT')).toHaveLength(2);
  });

  it('Cancel leaves the change undone and the form as it was', async () => {
    const { calls } = await operatorSignedIn({
      'GET /v1/join/settings': () => json(settings),
      'GET /v1/guests': () => json({ guests: [] }),
      'PUT /v1/join/settings': () => refusal(403, 'reauth_required'),
    });
    const region = await joining();
    const dialog = await screen.findByRole('dialog', { name: 'Confirm it is you' });
    fireEvent.click(within(dialog).getByRole('button', { name: 'Cancel' }));
    expect(await within(region).findByText('It needs your password first. Nothing was changed.')).toBeDefined();
    expect(within(region).getByLabelText<HTMLInputElement>('Create an account').checked).toBe(true);
    expect(calls.filter((c) => c.method === 'PUT')).toHaveLength(1);
    expect(calls.filter((c) => c.path === '/v1/reauth')).toEqual([]);
  });
});

describe('an operator’s setup from a host-made token', () => {
  const setupAuth = (done: { password_set: boolean; totp_enrolled: boolean }) => ({
    scope: 'setup',
    password_set: done.password_set,
    totp_enrolled: done.totp_enrolled,
    setup_complete: done.password_set && done.totp_enrolled,
  });
  const enrollment = { secret: totpSecret, issuer: 'conch.example', account: 'root@conch.example', algorithm: 'SHA1', digits: 6, period_seconds: 30, uri: `otpauth://totp/x?secret=${totpSecret}` };

  function setupServer() {
    const state = { password_set: false, totp_enrolled: false, ended: false };
    const calls = serve({
      'GET /v1/join': () => json(frontDoor),
      'POST /v1/setup/redeem': (c) =>
        (c.body as { token?: string }).token === setupToken
          ? json({ principal, credential: { id: 9, principal_id: 1, label: 'setup', created_at: at, scope: 'setup' }, token: setupCredential, auth: setupAuth(state) }, 201)
          : refusal(404, 'setup_token_invalid'),
      'GET /v1/whoami': (c) =>
        c.auth === `Bearer ${setupCredential}` && !state.ended ? json({ ...operatorWho, auth: setupAuth(state) }) : json({}, 401),
      'POST /v1/password/mine': (c) => {
        if (c.auth !== `Bearer ${setupCredential}`) return json({}, 401);
        state.password_set = true;
        return json({ revoked: 0 });
      },
      'POST /v1/totp/enroll': (c) => (c.auth === `Bearer ${setupCredential}` ? json(enrollment) : json({}, 401)),
      'POST /v1/totp/confirm': (c) => {
        if ((c.body as { code?: string }).code !== '246810') return refusal(401, 'auth_failed');
        state.totp_enrolled = true;
        state.ended = true;
        return new Response(null, { status: 204 });
      },
    });
    return { calls, state };
  }

  it('spends the token, sets the password, shows the key only on Reveal and never while the tab shares, and confirms a code', async () => {
    const { calls } = setupServer();
    const { container } = render(<App origin={origin} />);
    fireEvent.click(await screen.findByRole('button', { name: 'Set up an operator account' }));
    fireEvent.change(screen.getByLabelText('Setup token'), { target: { value: setupToken } });
    fireEvent.click(screen.getByRole('button', { name: 'Continue' }));
    fireEvent.change(await screen.findByLabelText('Choose a password'), { target: { value: password } });
    fireEvent.change(screen.getByLabelText('Type it again'), { target: { value: password } });
    fireEvent.click(screen.getByRole('button', { name: 'Set password' }));
    await screen.findByRole('heading', { name: 'Your authenticator app' });
    expect(calls.find((c) => c.path === '/v1/password/mine')).toMatchObject({ auth: `Bearer ${setupCredential}`, body: { password } });

    // While this tab shares, the key is neither shown nor fetched.
    const share = await holdShare();
    await act(async () => {});
    expect(screen.getByRole<HTMLButtonElement>('button', { name: 'Show the key' }).disabled).toBe(true);
    expect(screen.getByText(/This tab is sharing its screen/)).toBeDefined();
    expect(calls.filter((c) => c.path === '/v1/totp/enroll')).toEqual([]);
    await act(async () => {
      await share.stop();
    });
    fireEvent.click(screen.getByRole('button', { name: 'Show the key' }));
    expect(await screen.findByText('JBSW Y3DP EHPK 3PXP JBSW Y3DP EHPK 3PXP')).toBeDefined();
    // A share that starts hides it at once.
    const again = await holdShare();
    await act(async () => {});
    expect(screen.queryByText('JBSW Y3DP EHPK 3PXP JBSW Y3DP EHPK 3PXP')).toBeNull();
    await act(async () => {
      await again.stop();
    });

    fireEvent.change(screen.getByLabelText('Code the app shows now'), { target: { value: '111111' } });
    fireEvent.click(screen.getByRole('button', { name: 'Finish setup' }));
    expect(await screen.findByText(/That code is not right/)).toBeDefined();
    fireEvent.change(screen.getByLabelText('Code the app shows now'), { target: { value: '246810' } });
    fireEvent.click(screen.getByRole('button', { name: 'Finish setup' }));
    await screen.findByRole('heading', { name: 'Setup is complete' });
    fireEvent.click(screen.getByRole('button', { name: 'Go to sign-in' }));
    expect(await screen.findByText(noticeText('setupDone'))).toBeDefined();
    // Nobody is signed in, and nothing of the setup is kept.
    expect(window.localStorage.length + window.sessionStorage.length).toBe(0);
    expectNoSecret(container, setupToken, setupCredential, password, totpSecret);
  });

  it('says so for a token that does not work', async () => {
    setupServer();
    render(<App origin={origin} />);
    fireEvent.click(await screen.findByRole('button', { name: 'Set up an operator account' }));
    fireEvent.change(screen.getByLabelText('Setup token'), { target: { value: `conch_setup_${'X'.repeat(43)}` } });
    fireEvent.click(screen.getByRole('button', { name: 'Continue' }));
    expect(await screen.findByText(/That setup token does not work/)).toBeDefined();
    expect(screen.getByLabelText<HTMLInputElement>('Setup token').value).toBe('');
  });

  it('is not offered on a page served over plain HTTP beyond this machine', async () => {
    setupServer();
    render(<App origin="http://192.168.1.20:8080" />);
    await screen.findByText(insecureLine);
    expect(screen.queryByRole('button', { name: 'Set up an operator account' })).toBeNull();
  });
});

describe('what was being typed', () => {
  class FakeSocket {
    static all: FakeSocket[] = [];
    onopen: (() => void) | null = null;
    onmessage: ((event: { data: unknown }) => void) | null = null;
    onclose: ((event: { code: number }) => void) | null = null;
    constructor(readonly url: string) {
      FakeSocket.all.push(this);
    }
    close() {
      this.onclose = null;
    }
  }

  // A member of the same server, on a credential of their own.
  const memberToken = 'conch_member_script_token';
  const memberWho: WhoAmIResponseV1 = { id: 7, kind: 'human', name: 'mo', role: 'member', identity_class: 'member' };

  // typedThenEnded signs root in with a password and a code, types a draft
  // in #lobby, and has the session end under it. It answers the switch that
  // brings the session back.
  async function typedThenEnded(): Promise<{ readonly revive: () => void }> {
    FakeSocket.all = [];
    vi.stubGlobal('WebSocket', FakeSocket);
    let live = true;
    const holder = (c: Call): WhoAmIResponseV1 | undefined =>
      c.auth === `Bearer ${memberToken}` ? memberWho : live && c.auth === `Bearer ${sessionToken}` ? operatorWho : undefined;
    const ok = (c: Call) => holder(c) !== undefined;
    serve(
      operatorServer({
        'GET /v1/whoami': (c) => {
          const who = holder(c);
          return who === undefined ? json({}, 401) : json(who);
        },
        'GET /v1/channels': (c) => (ok(c) ? json(channelList) : json({}, 401)),
        'GET /v2/channels/lobby/messages': (c) => (ok(c) ? json({ messages: [] }) : json({}, 401)),
        'POST /v1/ws-tickets': (c) => (ok(c) ? json({ ticket: 'conch_ticket', expires_at: later }, 201) : json({}, 401)),
        'POST /v2/channels/lobby/messages': () => json({}, 401),
      }),
    );
    render(<App origin={origin} />);
    await screen.findByRole('heading', { name: 'Sign in' });
    signInWithPassword();
    await screen.findByLabelText('Code from your authenticator app');
    typeCode('123456');
    await screen.findByRole('log', { name: 'Messages' });
    await waitFor(() => {
      expect(FakeSocket.all.length).toBeGreaterThan(0);
    });
    act(() => {
      for (const s of FakeSocket.all) s.onopen?.();
    });
    const composer = await screen.findByLabelText<HTMLTextAreaElement>('Message #lobby');
    await waitFor(() => {
      expect(composer.disabled).toBe(false);
    });
    fireEvent.change(composer, { target: { value: 'half a thought' } });
    // The session has ended: the send is refused, and the text is not lost.
    live = false;
    fireEvent.keyDown(composer, { key: 'Enter' });
    expect(await screen.findByText(noticeText('sessionEnded'))).toBeDefined();
    return {
      revive: () => {
        live = true;
      },
    };
  }

  // openLobby waits for #lobby's composer to be usable, as it opens.
  async function openLobby(): Promise<HTMLTextAreaElement> {
    await screen.findByRole('log', { name: 'Messages' });
    act(() => {
      for (const s of FakeSocket.all) s.onopen?.();
    });
    const composer = await screen.findByLabelText<HTMLTextAreaElement>('Message #lobby');
    await waitFor(() => {
      act(() => {
        for (const s of FakeSocket.all) s.onopen?.();
      });
      expect(composer.disabled).toBe(false);
    });
    return composer;
  }

  it('is not given to someone else who signs in next', async () => {
    await typedThenEnded();
    fireEvent.change(screen.getByLabelText('Credential'), { target: { value: memberToken } });
    fireEvent.click(screen.getByRole('button', { name: 'Use credential' }));
    await waitFor(() => {
      expect(statusLine()?.textContent).toContain('mo');
    });
    const composer = await openLobby();
    expect(composer.value).toBe('');
    expect(document.body.innerHTML).not.toContain('half a thought');
  });

  it('restores an initial POST 401 only as a draft when the same operator signs in again', async () => {
    const { revive } = await typedThenEnded();
    revive();
    signInWithPassword();
    await screen.findByLabelText('Code from your authenticator app');
    typeCode('234567');
    const back = await screen.findByLabelText<HTMLTextAreaElement>('Message #lobby');
    await waitFor(() => {
      expect(back.value).toBe('half a thought');
    });
    expect(screen.queryByText(/^Not confirmed/)).toBeNull();
    expect(screen.queryByRole('button', { name: 'Retry' })).toBeNull();
  });
});
