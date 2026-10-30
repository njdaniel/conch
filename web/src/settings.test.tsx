// Settings and the links that change an account (issue #714, design §5.2,
// §6): changing a password, setting a first one with an operator's
// enrollment code, what recovery cannot do, a reset link's page and its
// sign-in after, a device link's page with the account switch it asks for,
// an operator's joining settings with End all guest visits (a dry run, then
// a yes), and whom an operator's invite admits. conchd is a stand-in fetch.
import { act, cleanup, fireEvent, render, screen, waitFor, within } from '@testing-library/react';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { App } from './App';
import { memberRecoveryLine, operatorRecoveryLine } from './Settings';
import { insecureLine } from './transport';
import type { WhoAmIResponseV1 } from './schema/principal';

const origin = 'http://127.0.0.1:8080';
const key = 'conch.web.token';
const at = '2026-10-23T12:00:00.000Z';
const later = '2099-10-23T12:00:00.000Z';
const memberToken = 'conch_member_secret_token';
const rotatedToken = 'conch_member_rotated_token';
const operatorToken = 'conch_operator_session_token';
const otherToken = 'conch_other_member_token';
const oldPassword = 'the old passphrase of the member';
const newPassword = 'a brand new passphrase for the member';
const resetCode = `conch_sil_${'R'.repeat(43)}`;
const proofCode = `conch_sil_${'E'.repeat(43)}`;

const memberWho: WhoAmIResponseV1 = { id: 12, kind: 'human', name: 'pat', role: 'member', identity_class: 'member', auth: { scope: 'full', password_set: true, setup_complete: true } };
const legacyWho: WhoAmIResponseV1 = { ...memberWho, auth: { scope: 'full', password_set: false, setup_complete: false } };
const operatorWho: WhoAmIResponseV1 = {
  id: 1,
  kind: 'human',
  name: 'root',
  role: 'operator',
  identity_class: 'operator',
  may_invite: true,
  auth: { scope: 'full', password_set: true, totp_enrolled: true, setup_complete: true, expires_at: later, idle_expires_at: later },
};
const otherWho: WhoAmIResponseV1 = { id: 2, kind: 'human', name: 'ada', role: 'member' };
const channelList = { channels: [{ id: 1, name: 'lobby', created_at: at }] };
const rotated = { credential: { id: 40, principal_id: 12, label: 'browser', created_at: at, renews: true }, token: rotatedToken, revoked: 2 };

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

// signedIn keeps token as an earlier load would have, and opens Settings.
async function signedIn(token: string, place: 'tab' | 'lasting' = 'lasting') {
  (place === 'tab' ? window.sessionStorage : window.localStorage).setItem(key, token);
  const view = render(<App origin={origin} />);
  fireEvent.click(await screen.findByRole('button', { name: 'Settings' }));
  return view;
}

async function openPassword(): Promise<HTMLElement> {
  fireEvent.click(await screen.findByRole('button', { name: 'Password' }));
  return screen.getByRole('region', { name: 'Password' });
}

function fill(region: HTMLElement, label: string, value: string) {
  fireEvent.change(within(region).getByLabelText(label), { target: { value } });
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
  vi.unstubAllGlobals();
});

describe('changing a password', () => {
  it('needs the current password, keeps the person signed in when it is wrong, and keeps the rotated credential', async () => {
    let tries = 0;
    const calls = serve({
      ...people({ [memberToken]: memberWho, [rotatedToken]: memberWho }),
      'PUT /v1/password/mine': () => (++tries === 1 ? refusal(401, 'auth_failed') : json(rotated)),
    });
    const { container } = await signedIn(memberToken);
    const region = await openPassword();
    expect(within(region).queryByLabelText('Code from your authenticator app')).toBeNull();
    expect(within(region).getByText(memberRecoveryLine)).toBeDefined();
    fill(region, 'Current password', 'not the old one at all');
    fill(region, 'New password', newPassword);
    fill(region, 'Type it again', newPassword);
    fireEvent.click(within(region).getByRole('button', { name: 'Change password' }));
    expect(await within(region).findByText('The current password is not right.')).toBeDefined();
    // Still signed in, on the same credential.
    expect(window.localStorage.getItem(key)).toBe(memberToken);
    expect(within(region).getByLabelText<HTMLInputElement>('Current password').value).toBe('');

    fill(region, 'Current password', oldPassword);
    fireEvent.click(within(region).getByRole('button', { name: 'Change password' }));
    expect(await within(region).findByText('Your password is changed. Every other sign-in of yours has ended; this one goes on.')).toBeDefined();
    expect(window.localStorage.getItem(key)).toBe(rotatedToken);
    expect(calls.filter((c) => c.path === '/v1/password/mine').map((c) => [c.auth, c.body])).toEqual([
      [`Bearer ${memberToken}`, { current: 'not the old one at all', password: newPassword }],
      [`Bearer ${memberToken}`, { current: oldPassword, password: newPassword }],
    ]);
    // The next call goes with the new credential.
    await waitFor(() => {
      expect(calls.some((c) => c.path === '/v1/whoami' && c.auth === `Bearer ${rotatedToken}`)).toBe(true);
    });
    expectNoSecret(container, oldPassword, newPassword, rotatedToken);
  });

  // Security review N2: another tab replaced the stored credential while the
  // change was on its way. The rotated credential has nobody here to hold
  // it: it is revoked, not left alive, and the other tab's is untouched.
  it('revokes the rotated credential when another tab replaced the stored one meanwhile', async () => {
    const calls = serve({
      ...people({ [memberToken]: memberWho, [otherToken]: otherWho }),
      'PUT /v1/password/mine': () => {
        window.localStorage.setItem(key, otherToken);
        return json(rotated);
      },
      'DELETE /v1/credentials/current': () => new Response(null, { status: 204 }),
    });
    await signedIn(memberToken);
    const region = await openPassword();
    fill(region, 'Current password', oldPassword);
    fill(region, 'New password', newPassword);
    fill(region, 'Type it again', newPassword);
    fireEvent.click(within(region).getByRole('button', { name: 'Change password' }));
    await waitFor(() => {
      expect(calls.find((c) => c.method === 'DELETE' && c.path === '/v1/credentials/current')?.auth).toBe(`Bearer ${rotatedToken}`);
    });
    expect(window.localStorage.getItem(key)).toBe(otherToken);
    expect(calls.some((c) => c.auth === `Bearer ${rotatedToken}` && c.method !== 'DELETE')).toBe(false);
  });

  it('sends a short new password (issue #977)', async () => {
    const calls = serve({ ...people({ [memberToken]: memberWho, [rotatedToken]: memberWho }), 'PUT /v1/password/mine': () => json(rotated) });
    await signedIn(memberToken);
    const region = await openPassword();
    fill(region, 'Current password', oldPassword);
    fill(region, 'New password', 'tide');
    fill(region, 'Type it again', 'tide');
    fireEvent.click(within(region).getByRole('button', { name: 'Change password' }));
    await waitFor(() => {
      expect(calls.filter((c) => c.path === '/v1/password/mine').map((c) => c.body)).toEqual([{ current: oldPassword, password: 'tide' }]);
    });
    expect(within(region).queryByText(/at least 15/)).toBeNull();
  });

  it('checks the new password before sending anything', async () => {
    const calls = serve(people({ [memberToken]: memberWho }));
    await signedIn(memberToken);
    const region = await openPassword();
    fill(region, 'Current password', oldPassword);
    fill(region, 'New password', '');
    fill(region, 'Type it again', '');
    fireEvent.click(within(region).getByRole('button', { name: 'Change password' }));
    expect(await within(region).findByText('Choose a password.')).toBeDefined();
    fill(region, 'New password', newPassword);
    fill(region, 'Type it again', `${newPassword}!`);
    fireEvent.click(within(region).getByRole('button', { name: 'Change password' }));
    expect(await within(region).findByText('The two passwords are not the same. Type them again.')).toBeDefined();
    expect(calls.filter((c) => c.path === '/v1/password/mine')).toEqual([]);
  });

  it('asks an operator for a code as well, and says recovery is on the host', async () => {
    const calls = serve({ ...people({ [operatorToken]: operatorWho }), 'PUT /v1/password/mine': () => json({ ...rotated, token: operatorToken }) });
    await signedIn(operatorToken, 'tab');
    const region = await openPassword();
    expect(within(region).getByText(operatorRecoveryLine)).toBeDefined();
    fill(region, 'Current password', oldPassword);
    fill(region, 'Code from your authenticator app', '135 790');
    fill(region, 'New password', newPassword);
    fill(region, 'Type it again', newPassword);
    fireEvent.click(within(region).getByRole('button', { name: 'Change password' }));
    await within(region).findByText(/Your password is changed/);
    expect(calls.find((c) => c.path === '/v1/password/mine')?.body).toEqual({ current: oldPassword, password: newPassword, code: '135790' });
    expect(window.sessionStorage.getItem(key)).toBe(operatorToken);
  });

  it('shows no password field on a page served over plain HTTP beyond this machine', async () => {
    serve(people({ [memberToken]: memberWho }));
    window.localStorage.setItem(key, memberToken);
    render(<App origin="http://192.168.1.20:8080" />);
    fireEvent.click(await screen.findByRole('button', { name: 'Settings' }));
    fireEvent.click(await screen.findByRole('button', { name: 'Password' }));
    const region = screen.getByRole('region', { name: 'Password' });
    expect(within(region).getByText(insecureLine)).toBeDefined();
    expect(region.querySelector('input[type="password"]')).toBeNull();
  });
});

describe('a member from before passwords', () => {
  it('asks for the enrollment code, not the password, when the server wants one (issue #815)', async () => {
    const calls = serve({
      ...people({ [memberToken]: legacyWho }),
      'POST /v1/password/mine': () => refusal(403, 'enrollment_proof_required'),
    });
    await signedIn(memberToken);
    const region = await openPassword();
    fill(region, 'Enrollment code from an operator', proofCode);
    fill(region, 'Choose a password', newPassword);
    fill(region, 'Type it again', newPassword);
    fireEvent.click(within(region).getByRole('button', { name: 'Set password' }));
    expect(await within(region).findByText(/needs the enrollment code an operator gave you/)).toBeDefined();
    expect(screen.queryByText('Confirm it is you')).toBeNull();
    expect(calls.filter((c) => c.path === '/v1/password/mine')).toHaveLength(1);
    expect(calls.some((c) => c.path === '/v1/reauth')).toBe(false);
  });

  it('sets a first password with an operator’s enrollment code, and says when the code does not work', async () => {
    let tries = 0;
    const calls = serve({
      ...people({ [memberToken]: legacyWho }),
      'POST /v1/password/mine': () => (++tries === 1 ? refusal(404, 'sign_in_link_invalid') : json({ revoked: 0 })),
    });
    const { container } = await signedIn(memberToken);
    const region = await openPassword();
    expect(within(region).getByText(/Your account has no password yet/)).toBeDefined();
    fill(region, 'Choose a password', newPassword);
    fill(region, 'Type it again', newPassword);
    fireEvent.click(within(region).getByRole('button', { name: 'Set password' }));
    expect(await within(region).findByText('Paste the enrollment code an operator gave you.')).toBeDefined();
    expect(calls.filter((c) => c.path === '/v1/password/mine')).toEqual([]);
    fill(region, 'Enrollment code from an operator', proofCode);
    fireEvent.click(within(region).getByRole('button', { name: 'Set password' }));
    expect(await within(region).findByText(/That enrollment code does not work/)).toBeDefined();
    fill(region, 'Enrollment code from an operator', proofCode);
    fill(region, 'Choose a password', newPassword);
    fill(region, 'Type it again', newPassword);
    fireEvent.click(within(region).getByRole('button', { name: 'Set password' }));
    expect(await within(region).findByText(/Your password is set/)).toBeDefined();
    expect(calls.filter((c) => c.path === '/v1/password/mine').map((c) => c.body)).toEqual([
      { password: newPassword, proof: proofCode },
      { password: newPassword, proof: proofCode },
    ]);
    expectNoSecret(container, proofCode, newPassword);
  });
});

describe('a reset link', () => {
  const opened = (routes: Record<string, Route>, at = origin) => {
    const calls = serve({ 'POST /v1/sign-in-links/preview': () => json({ name: 'pat', purpose: 'reset' }), 'GET /v1/join': () => json({}, 404), ...routes });
    window.location.hash = `#/signin/${resetCode}`;
    const view = render(<App origin={at} />);
    return { calls, view };
  };

  it('replaces the forgotten password, signs nobody in, and the new one signs in after', async () => {
    const signedInAnswer = {
      next: 'signed_in',
      principal: { id: 12, kind: 'human', name: 'pat', created_at: at },
      credential: { id: 41, principal_id: 12, label: 'browser', created_at: at },
      token: memberToken,
    };
    const { calls, view } = opened({
      'POST /v1/password/reset': () => json({ revoked: 3 }),
      'POST /v1/login': (c) => ((c.body as { password?: string }).password === newPassword ? json(signedInAnswer) : refusal(401, 'auth_failed')),
      ...people({ [memberToken]: memberWho }),
    });
    expect(await screen.findByRole('heading', { name: 'Choose a new password' })).toBeDefined();
    expect(screen.getByText(/Choose a new password for/).textContent).toContain('pat');
    fireEvent.change(screen.getByLabelText('New password'), { target: { value: newPassword } });
    fireEvent.change(screen.getByLabelText('Type it again'), { target: { value: newPassword } });
    fireEvent.click(screen.getByRole('button', { name: 'Set the new password' }));
    expect(await screen.findByRole('heading', { name: 'Your password is changed' })).toBeDefined();
    expect(calls.find((c) => c.path === '/v1/password/reset')).toMatchObject({ auth: null, body: { code: resetCode, password: newPassword } });
    expect(window.localStorage.length + window.sessionStorage.length).toBe(0);

    fireEvent.click(screen.getByRole('button', { name: 'Go to sign-in' }));
    fireEvent.change(await screen.findByLabelText('Name'), { target: { value: 'pat' } });
    fireEvent.change(screen.getByLabelText('Password'), { target: { value: newPassword } });
    fireEvent.click(screen.getByRole('button', { name: 'Sign in' }));
    await waitFor(() => {
      expect(screen.getByRole('status', { name: 'Status' }).textContent).toContain('pat');
    });
    expectNoSecret(view.container, resetCode, newPassword, memberToken);
  });

  it('says a used or expired link no longer works', async () => {
    opened({ 'POST /v1/password/reset': () => refusal(404, 'sign_in_link_invalid') });
    await screen.findByRole('heading', { name: 'Choose a new password' });
    fireEvent.change(screen.getByLabelText('New password'), { target: { value: newPassword } });
    fireEvent.change(screen.getByLabelText('Type it again'), { target: { value: newPassword } });
    fireEvent.click(screen.getByRole('button', { name: 'Set the new password' }));
    expect(await screen.findByRole('heading', { name: 'This sign-in link no longer works' })).toBeDefined();
  });

  it('takes no password on a page served over plain HTTP beyond this machine', async () => {
    opened({}, 'http://192.168.1.20:8080');
    await screen.findByRole('heading', { name: 'Choose a new password' });
    expect(screen.getByText(insecureLine)).toBeDefined();
    expect(document.querySelector('input[type="password"]')).toBeNull();
  });
});

describe('a device link', () => {
  it('says whose account it adds this device to, and signs whoever is here out first, on their own button', async () => {
    window.localStorage.setItem(key, otherToken);
    const calls = serve({
      'POST /v1/sign-in-links/preview': () => json({ name: 'pat', purpose: 'device' }),
      'DELETE /v1/credentials/current': () => new Response(null, { status: 204 }),
      ...people({ [otherToken]: otherWho }),
    });
    window.location.hash = `#/signin/${resetCode}`;
    render(<App origin={origin} />);
    expect((await screen.findByText(/This link adds this device to your account/)).textContent).toContain('pat');
    expect(screen.getByText(/You are signed in here as/).textContent).toContain('and this link is for pat');
    // Opening the link used nothing.
    expect(calls.some((c) => c.path === '/v1/sign-in-links/redeem')).toBe(false);
    expect(screen.queryByRole('button', { name: 'Sign in as pat' })).toBeNull();
    fireEvent.click(screen.getByRole('button', { name: 'Sign ada out' }));
    expect(await screen.findByRole('button', { name: 'Sign in as pat' })).toBeDefined();
    expect(calls.find((c) => c.method === 'DELETE')?.auth).toBe(`Bearer ${otherToken}`);
    expect(calls.some((c) => c.path === '/v1/sign-in-links/redeem')).toBe(false);
  });
});

describe('joining and guests, for an operator', () => {
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
    live_guests: 2,
    warnings: ['live_guests_remain'],
  };
  const guest = (id: number, label: string) => ({
    id,
    handle: `guest-${String(id).padStart(16, 'a')}`,
    label,
    tag: 'a7k4',
    state: 'active',
    origin: 'address',
    started_at: at,
    last_active_at: at,
    expires_at: later,
    idle_expires_at: later,
  });
  const live = [guest(30, 'Pat'), guest(31, 'Sam')];

  async function openJoining(routes: Record<string, Route>) {
    const calls = serve({ ...people({ [operatorToken]: operatorWho }), 'GET /v1/join/settings': () => json(settings), 'GET /v1/guests': () => json({ guests: live }), ...routes });
    await signedIn(operatorToken, 'tab');
    fireEvent.click(await screen.findByRole('button', { name: 'Joining' }));
    const region = screen.getByRole('region', { name: 'Joining' });
    await within(region).findByLabelText('Create an account');
    return { calls, region };
  }

  it('offers no guest switch and no guest mode: the address offers nothing or accounts, and only what changed is sent', async () => {
    const { calls, region } = await openJoining({ 'PUT /v1/join/settings': () => json({ ...settings, settings: { ...settings.settings, open_joining: 'account', open_channels: ['lobby'] }, warnings: [] }) });
    expect(within(region).queryByLabelText('Allow guest visits')).toBeNull();
    expect(within(region).queryByLabelText('Join as guest')).toBeNull();
    expect(within(region).queryByLabelText('Both')).toBeNull();
    expect(within(region).getAllByRole('radio').map((r) => (r.parentElement?.textContent ?? '').trim())).toEqual(['Nothing: invite-only', 'Create an account']);
    // Live visits are still said, so an operator can end them.
    expect(within(region).getByText(/2 guest visits are still live/)).toBeDefined();
    fireEvent.click(within(region).getByLabelText('Create an account'));
    fireEvent.click(within(region).getByLabelText('#lobby'));
    fireEvent.click(within(region).getByRole('button', { name: 'Save joining' }));
    expect(await within(region).findByText('Saved.')).toBeDefined();
    expect(calls.find((c) => c.method === 'PUT')?.body).toEqual({ open_joining: 'account', open_channels: ['lobby'] });
  });

  it('shows a server that still reads a guest mode as invite-only, and never sends it back', async () => {
    const { calls, region } = await openJoining({
      'GET /v1/join/settings': () => json({ ...settings, settings: { ...settings.settings, guests_enabled: true, open_joining: 'both' } }),
      'PUT /v1/join/settings': () => json({ ...settings, settings: { ...settings.settings, joining_paused: true } }),
    });
    expect(within(region).getByLabelText<HTMLInputElement>('Nothing: invite-only').checked).toBe(true);
    fireEvent.click(within(region).getByLabelText('Pause joining from the address'));
    fireEvent.click(within(region).getByRole('button', { name: 'Save joining' }));
    expect(await within(region).findByText('Saved.')).toBeDefined();
    expect(calls.find((c) => c.method === 'PUT')?.body).toEqual({ joining_paused: true });
  });

  it('says plainly what a stale page is told when the server refuses a guest setting', async () => {
    const { region } = await openJoining({ 'PUT /v1/join/settings': () => refusal(409, 'join_policy_conflict', 'guests are not available') });
    fireEvent.click(within(region).getByLabelText('Create an account'));
    fireEvent.click(within(region).getByRole('button', { name: 'Save joining' }));
    expect(await within(region).findByText('Guests are not available.')).toBeDefined();
  });

  it('says what the server refused, and keeps what was chosen', async () => {
    const { region } = await openJoining({ 'PUT /v1/join/settings': () => refusal(409, 'join_policy_conflict', 'open joining needs at least one open channel') });
    fireEvent.click(within(region).getByLabelText('Create an account'));
    fireEvent.click(within(region).getByRole('button', { name: 'Save joining' }));
    expect(await within(region).findByText('Open joining needs at least one open channel.')).toBeDefined();
    expect(within(region).getByLabelText<HTMLInputElement>('Create an account').checked).toBe(true);
  });

  it('ends every live visit only after a dry run and a yes', async () => {
    const { calls, region } = await openJoining({
      'POST /v1/guests/end': (c) =>
        (c.body as { dry_run?: boolean }).dry_run === true
          ? json({ dry_run: true, count: 2, guests: live.map((g) => ({ id: g.id, handle: g.handle })), remaining: 0 })
          : json({ dry_run: false, count: 2, guests: live.map((g) => ({ id: g.id, handle: g.handle })), remaining: 0 }),
    });
    expect(await within(region).findByText('Guest: Pat · a7k4')).toBeDefined();
    fireEvent.click(within(region).getByRole('button', { name: 'End all guest visits' }));
    const question = await within(region).findByRole('group', { name: /^End 2 guest visits\?/ });
    expect(within(question).getAllByRole('listitem')).toHaveLength(2);
    expect(calls.filter((c) => c.path === '/v1/guests/end').map((c) => c.body)).toEqual([{ dry_run: true }]);
    fireEvent.click(within(question).getByRole('button', { name: 'Cancel' }));
    expect(within(region).queryByRole('group', { name: /^End 2 guest visits\?/ })).toBeNull();
    expect(calls.filter((c) => c.path === '/v1/guests/end')).toHaveLength(1);

    fireEvent.click(within(region).getByRole('button', { name: 'End all guest visits' }));
    fireEvent.click(within(await within(region).findByRole('group', { name: /^End 2 guest visits\?/ })).getByRole('button', { name: 'Yes, end them' }));
    expect(await within(region).findByText('Ended 2 guest visits.')).toBeDefined();
    expect(calls.filter((c) => c.path === '/v1/guests/end').map((c) => c.body)).toEqual([{ dry_run: true }, { dry_run: true }, { dry_run: false }]);
  });

  it('says there is nothing to end when the dry run finds no visit', async () => {
    const { calls, region } = await openJoining({ 'POST /v1/guests/end': () => json({ dry_run: true, count: 0, guests: [], remaining: 0 }) });
    fireEvent.click(within(region).getByRole('button', { name: 'End all guest visits' }));
    expect(await within(region).findByText('No guest visit is live: there is nothing to end.')).toBeDefined();
    expect(calls.filter((c) => c.path === '/v1/guests/end')).toHaveLength(1);
  });

  it('is not offered to a member', async () => {
    serve(people({ [memberToken]: memberWho }));
    await signedIn(memberToken);
    await screen.findByRole('button', { name: 'Password' });
    expect(screen.queryByRole('button', { name: 'Joining' })).toBeNull();
  });
});

describe('an operator’s invite admits accounts only', () => {
  async function inviting(guestsEnabled: boolean) {
    const calls = serve({
      ...people({ [operatorToken]: operatorWho }),
      'GET /v1/join/settings': () =>
        json({
          settings: {
            open_joining: 'off',
            guests_enabled: guestsEnabled,
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
        }),
      'GET /v1/invites': () => json({ invites: [] }),
      'POST /v1/invites': () => refusal(500, 'internal_error'),
    });
    window.sessionStorage.setItem(key, operatorToken);
    render(<App origin={origin} />);
    fireEvent.click(await screen.findByRole('button', { name: 'Invite someone' }));
    return calls;
  }

  // Guests are removed for now (issue #1005): no choice, whatever the
  // joining settings read, and the request names none.
  it.each([true, false])('offers no choice of whom an invite admits (a server reading guests_enabled %s): it admits accounts', async (guestsEnabled) => {
    const calls = await inviting(guestsEnabled);
    // Let any read of the joining settings land before looking.
    await act(async () => {
      await new Promise((resolve) => setTimeout(resolve, 30));
    });
    expect(screen.queryByLabelText('A temporary guest')).toBeNull();
    expect(screen.queryByRole('group', { name: 'Who may join with it' })).toBeNull();
    fireEvent.click(screen.getByLabelText('#lobby'));
    fireEvent.click(screen.getByRole('button', { name: 'Make invite' }));
    await waitFor(() => {
      expect(calls.some((c) => c.method === 'POST' && c.path === '/v1/invites')).toBe(true);
    });
    expect(calls.find((c) => c.method === 'POST' && c.path === '/v1/invites')?.body).not.toHaveProperty('join_as');
  });
});
