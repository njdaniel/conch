import { act, cleanup, fireEvent, render, screen, waitFor, within } from '@testing-library/react';
import { afterEach, describe, expect, it, vi } from 'vitest';
import type { BridgeReply, CompatView, ConchDesktopBridge, DesktopInviteBridge, DesktopSettingsBridge, DesktopSettingsView, InviteCheck, ProfileAddResult, ProfilesView, ProfileView, SettingsResult } from './bridge';
import { credentials } from './credentials';
import { DesktopShell } from './Shell';
import { SWAP_GUARD_MS } from './ui/InviteLink';
import { serverOrigin, setServerOrigin } from './state';

const ID_A = 'a'.repeat(32);
const ID_B = 'b'.repeat(32);

function profile(id: string, host: string, account: string | null, active: boolean): ProfileView {
  return { id, label: host, host, origin: `https://${host}`, account, active };
}

function compat(over: Partial<CompatView> = {}): CompatView {
  return { state: 'supported', server: 'v1.0.0', desktop: '0.0.0', override: false, invalid: null, ...over };
}

function view(over: Partial<ProfilesView> = {}): ProfilesView {
  return { profiles: [], active: null, storage: { kind: 'protected' }, recovery: false, notice: null, compat: compat(), platform: 'linux', ...over };
}

function install(overrides: Partial<ConchDesktopBridge & DesktopInviteBridge>): void {
  const never = () => Promise.reject(new Error('unexpected bridge call'));
  const bridge: ConchDesktopBridge & Partial<DesktopInviteBridge> = {
    serverAddress: never,
    request: never,
    subscribe: () => () => undefined,
    restore: () => Promise.resolve({ ok: false, kind: 'unauthorized', status: 401 }),
    signInWithCredential: never,
    keepCredential: never,
    signInWithPassword: never,
    signInWithCode: never,
    redeemInvite: never,
    redeemInviteAsGuest: never,
    redeemSignInLink: never,
    redeemSetupToken: never,
    resetPassword: never,
    joinAsGuest: never,
    createAccount: never,
    signOut: never,
    replaceCredential: never,
    profiles: () => Promise.resolve(view()),
    profileAdd: never,
    profileRename: never,
    profileRemove: never,
    profileSwitch: never,
    storageRetry: never,
    profilesRecover: never,
    uploadFile: never,
    downloadFile: never,
    saveFile: never,
    notify: never,
    notifyClose: never,
    notificationSupported: () => false,
    onNotification: () => () => undefined,
    ...overrides,
  };
  Object.defineProperty(window, 'conchDesktop', { value: bridge, configurable: true, writable: true });
}

afterEach(() => {
  cleanup();
  vi.useRealTimers();
  Reflect.deleteProperty(window, 'conchDesktop');
  setServerOrigin('');
  window.location.hash = '';
  const held = credentials.read();
  if (held !== null) credentials.drop(held);
});

const child = <p>the client</p>;

describe('with no server active', () => {
  it('asks for an address before the client, saves it and switches to it', async () => {
    const profileAdd = vi.fn((): Promise<ProfileAddResult> => Promise.resolve({ ok: true, id: ID_A }));
    const profileSwitch = vi.fn(() => Promise.resolve(true));
    install({ profileAdd, profileSwitch });
    render(<DesktopShell>{child}</DesktopShell>);
    const field = await screen.findByLabelText('Server address');
    expect(screen.queryByText('the client')).toBeNull();
    fireEvent.change(field, { target: { value: 'https://chat.example.org' } });
    fireEvent.click(screen.getByRole('button', { name: 'Continue' }));
    await waitFor(() => {
      expect(profileSwitch).toHaveBeenCalledWith(ID_A);
    });
    expect(profileAdd).toHaveBeenCalledWith('https://chat.example.org');
  });

  it.each([
    ['scheme', /secure \(https\)/],
    ['userinfo', /user name and password/],
    ['path', /no path/],
    ['malformed', /not a web address/],
    ['certificate', /does not trust that server’s certificate/],
    ['not-conch', /not as a Conch server/],
    ['unreachable', /could not be reached/],
  ] as const)('says why an address was refused: %s', async (refusal, words) => {
    install({ profileAdd: () => Promise.resolve({ ok: false, refusal }) });
    render(<DesktopShell>{child}</DesktopShell>);
    fireEvent.change(await screen.findByLabelText('Server address'), { target: { value: 'x' } });
    fireEvent.click(screen.getByRole('button', { name: 'Continue' }));
    await waitFor(() => {
      expect(screen.getAllByRole('alert').map((a) => a.textContent).join(' ')).toMatch(words);
    });
    expect(screen.queryByText('the client')).toBeNull();
  });

  it('says so when the bridge is missing', async () => {
    Reflect.deleteProperty(window, 'conchDesktop');
    render(<DesktopShell>{child}</DesktopShell>);
    expect((await screen.findByRole('alert')).textContent).toMatch(/connection to its main process is missing/);
  });

  it('lists the saved servers with host and account, and switches without a confirmation', async () => {
    const profileSwitch = vi.fn(() => Promise.resolve(true));
    install({ profiles: () => Promise.resolve(view({ profiles: [profile(ID_A, 'a.example', 'ada', false), profile(ID_B, 'b.example', null, false)] })), profileSwitch });
    render(<DesktopShell>{child}</DesktopShell>);
    await screen.findByText('ada');
    expect(screen.getByText('not signed in yet')).toBeDefined();
    fireEvent.click(screen.getByRole('button', { name: 'Switch to b.example' }));
    await waitFor(() => {
      expect(profileSwitch).toHaveBeenCalledWith(ID_B);
    });
  });

  it('shows the recovery screen when the list cannot be read, and only starts a new list when asked', async () => {
    const profilesRecover = vi.fn(() => Promise.resolve(view()));
    install({ profiles: () => Promise.resolve(view({ recovery: true })), profilesRecover });
    render(<DesktopShell>{child}</DesktopShell>);
    await screen.findByText('The list of servers could not be read');
    expect(profilesRecover).not.toHaveBeenCalled();
    fireEvent.click(screen.getByRole('button', { name: 'Start an empty list' }));
    await screen.findByLabelText('Server address');
    expect(profilesRecover).toHaveBeenCalledOnce();
  });
});

describe('storage status', () => {
  it('says why sign-ins are session-only, and Retry re-checks', async () => {
    const storageRetry = vi.fn(() => Promise.resolve(view({ storage: { kind: 'protected' } })));
    install({ profiles: () => Promise.resolve(view({ storage: { kind: 'session-only', reason: 'insecure-backend' } })), storageRetry });
    render(<DesktopShell>{child}</DesktopShell>);
    const status = await screen.findByRole('status', { name: 'Sign-in storage' });
    expect(status.textContent).toMatch(/protects nothing/);
    expect(status.textContent).toMatch(/sign in again next time/);
    fireEvent.click(within(status).getByRole('button', { name: 'Retry' }));
    await waitFor(() => {
      expect(screen.getByRole('status', { name: 'Sign-in storage' }).textContent).toMatch(/sealed by the system/);
    });
    expect(storageRetry).toHaveBeenCalledOnce();
    expect(screen.queryByRole('button', { name: 'Retry' })).toBeNull();
  });

  it('says the data directory is not private to the person, as its own message', async () => {
    install({ profiles: () => Promise.resolve(view({ storage: { kind: 'session-only', reason: 'unsafe-root' } })) });
    render(<DesktopShell>{child}</DesktopShell>);
    expect((await screen.findByRole('status', { name: 'Sign-in storage' })).textContent).toMatch(/data directory is not private to you/);
  });

  it('says what DPAPI protects from, and what it does not', async () => {
    install({ profiles: () => Promise.resolve(view({ platform: 'win32' })) });
    render(<DesktopShell>{child}</DesktopShell>);
    expect((await screen.findByRole('status', { name: 'Sign-in storage' })).textContent).toMatch(/not from other apps running as you/);
  });
});

describe('with a server active', () => {
  const active = view({ profiles: [profile(ID_A, 'chat.example.org', 'ada', true)], active: ID_A });

  it('shows the client under a strip with the host and the account, and restores the stand-in for a sign-in main holds', async () => {
    install({
      profiles: () => Promise.resolve(active),
      restore: () => Promise.resolve({ ok: true, who: { id: 1, kind: 'human', name: 'ada', role: 'member' }, answer: {} }),
    });
    render(<DesktopShell>{child}</DesktopShell>);
    await screen.findByText('the client');
    const bar = screen.getByRole('banner', { name: 'Server' });
    expect(bar.textContent).toContain('chat.example.org');
    expect(bar.textContent).toContain('ada');
    expect(serverOrigin()).toBe('https://chat.example.org');
    expect(credentials.read()?.place).toBe('lasting');
  });

  it('keeps an operator’s and a guest’s stand-in with this window, not lasting', async () => {
    install({
      profiles: () => Promise.resolve(active),
      restore: () => Promise.resolve({ ok: true, who: { id: 1, kind: 'human', name: 'op', role: 'operator' }, answer: {} }),
    });
    render(<DesktopShell>{child}</DesktopShell>);
    await screen.findByText('the client');
    expect(credentials.read()?.place).toBe('tab');
  });

  it('follows the account once a sign-in binds it', async () => {
    const unbound = view({ profiles: [profile(ID_A, 'chat.example.org', null, true)], active: ID_A });
    let current = unbound;
    install({ profiles: () => Promise.resolve(current) });
    vi.useFakeTimers({ toFake: ['setInterval', 'clearInterval'] });
    render(<DesktopShell>{child}</DesktopShell>);
    await vi.waitFor(() => {
      expect(screen.getByText('the client')).toBeDefined();
    });
    expect(screen.getByRole('banner', { name: 'Server' }).textContent).toContain('not signed in yet');
    current = active;
    await act(async () => {
      await vi.advanceTimersByTimeAsync(2_100);
    });
    expect(screen.getByRole('banner', { name: 'Server' }).textContent).toContain('ada');
  });

  it('removes a server only after a confirmation that says what is deleted', async () => {
    const two = view({ profiles: [profile(ID_A, 'chat.example.org', 'ada', true), profile(ID_B, 'b.example', 'grace', false)], active: ID_A });
    const profileRemove = vi.fn(() => Promise.resolve(true));
    install({ profiles: () => Promise.resolve(two), profileRemove });
    render(<DesktopShell>{child}</DesktopShell>);
    fireEvent.click(await screen.findByRole('button', { name: 'Servers' }));
    fireEvent.click(await screen.findByRole('button', { name: 'Remove b.example…' }));
    const dialog = screen.getByRole('alertdialog', { name: 'Remove b.example' });
    expect(dialog.textContent).toMatch(/deletes its saved sign-in/);
    expect(profileRemove).not.toHaveBeenCalled();
    fireEvent.click(within(dialog).getByRole('button', { name: 'Keep' }));
    expect(profileRemove).not.toHaveBeenCalled();
    fireEvent.click(screen.getByRole('button', { name: 'Remove b.example…' }));
    fireEvent.click(within(screen.getByRole('alertdialog')).getByRole('button', { name: 'Remove b.example' }));
    await waitFor(() => {
      expect(profileRemove).toHaveBeenCalledWith(ID_B);
    });
  });

  it('renames with a display label only', async () => {
    const profileRename = vi.fn(() => Promise.resolve(true));
    install({ profiles: () => Promise.resolve(active), profileRename });
    render(<DesktopShell>{child}</DesktopShell>);
    fireEvent.click(await screen.findByRole('button', { name: 'Servers' }));
    fireEvent.click(await screen.findByRole('button', { name: 'Rename chat.example.org' }));
    fireEvent.change(screen.getByLabelText('Name'), { target: { value: 'Work' } });
    fireEvent.click(screen.getByRole('button', { name: 'Save name' }));
    await waitFor(() => {
      expect(profileRename).toHaveBeenCalledWith(ID_A, 'Work');
    });
  });

  it('says when the server could not confirm a sign-out', async () => {
    install({ profiles: () => Promise.resolve(view({ ...active, notice: 'revocation-unsent' })) });
    render(<DesktopShell>{child}</DesktopShell>);
    expect((await screen.findByRole('banner', { name: 'Server' })).textContent).toMatch(/cannot prove the sign-in was ended there/);
  });
});

describe('the desktop app settings (issue #1587)', () => {
  const active = view({ profiles: [profile(ID_A, 'chat.example.org', 'ada', true)], active: ID_A });
  const settings = (over: Partial<DesktopSettingsView> = {}): DesktopSettingsView => ({ closeToTray: false, launchAtLogin: false, trayWorking: false, trayDetected: false, trayConfirmed: false, launchAvailable: true, ...over });

  function installSettings(first: DesktopSettingsView, settingsSet: DesktopSettingsBridge['settingsSet']): void {
    install({ profiles: () => Promise.resolve(active) });
    const base = window.conchDesktop;
    Object.defineProperty(window, 'conchDesktop', { value: { ...base, settings: () => Promise.resolve(first), settingsSet }, configurable: true, writable: true });
  }

  async function open(): Promise<void> {
    render(<DesktopShell>{child}</DesktopShell>);
    fireEvent.click(await screen.findByRole('button', { name: 'Desktop app' }));
    await screen.findByRole('heading', { name: 'Desktop app' });
  }

  it('shows both settings, and the tray choice disabled with its reason while no tray works', async () => {
    installSettings(settings(), () => Promise.reject(new Error('unexpected')));
    await open();
    expect(screen.getByRole('radio', { name: 'Quit Conch' })).toHaveProperty('checked', true);
    const tray = screen.getByRole('radio', { name: 'Keep running in the tray' });
    expect(tray).toHaveProperty('disabled', true);
    expect(screen.getByText('No tray is available on this desktop')).toBeDefined();
    expect(screen.getByRole('checkbox', { name: 'Open Conch when I log in' })).toHaveProperty('checked', false);
  });

  it('lets the tray choice be made once a tray works', async () => {
    const settingsSet = vi.fn((): Promise<SettingsResult> => Promise.resolve({ ok: true, view: settings({ trayWorking: true, trayDetected: true, trayConfirmed: true, closeToTray: true }) }));
    installSettings(settings({ trayWorking: true, trayDetected: true }), settingsSet);
    await open();
    expect(screen.queryByText('No tray is available on this desktop')).toBeNull();
    fireEvent.click(screen.getByRole('radio', { name: 'Keep running in the tray' }));
    await waitFor(() => {
      expect(settingsSet).toHaveBeenCalledWith({ closeToTray: true });
    });
    await waitFor(() => {
      expect(screen.getByRole('radio', { name: 'Keep running in the tray' })).toHaveProperty('checked', true);
    });
  });

  it('after the tray is chosen, asks whether the icon can be seen, and confirms on yes', async () => {
    const settingsSet = vi.fn((change: Parameters<DesktopSettingsBridge['settingsSet']>[0]): Promise<SettingsResult> =>
      Promise.resolve({ ok: true, view: settings({ closeToTray: true, trayDetected: true, trayConfirmed: change.trayConfirmed === true, trayWorking: change.trayConfirmed === true }) }),
    );
    installSettings(settings({ closeToTray: true, trayDetected: true }), settingsSet);
    await open();
    expect(screen.getByText('Can you see the Conch icon in your panel?')).toBeDefined();
    fireEvent.click(screen.getByRole('button', { name: 'Yes, I can see it' }));
    await waitFor(() => {
      expect(settingsSet).toHaveBeenCalledWith({ trayConfirmed: true });
    });
    await waitFor(() => {
      expect(screen.queryByText('Can you see the Conch icon in your panel?')).toBeNull();
    });
  });

  it('turns the tray choice off again on no', async () => {
    const settingsSet = vi.fn((): Promise<SettingsResult> => Promise.resolve({ ok: true, view: settings({ trayDetected: true }) }));
    installSettings(settings({ closeToTray: true, trayDetected: true }), settingsSet);
    await open();
    fireEvent.click(screen.getByRole('button', { name: 'No, quit when I close' }));
    await waitFor(() => {
      expect(settingsSet).toHaveBeenCalledWith({ closeToTray: false });
    });
  });

  it('says so when main finds no tray host', async () => {
    const settingsSet = vi.fn((): Promise<SettingsResult> => Promise.resolve({ ok: false, reason: 'no-tray' }));
    installSettings(settings({ trayDetected: true }), settingsSet);
    await open();
    fireEvent.click(screen.getByRole('radio', { name: 'Keep running in the tray' }));
    expect((await screen.findByRole('alert')).textContent).toMatch(/No tray is available/);
  });

  it('asks main to turn launch at login on, and says why when main refuses', async () => {
    const settingsSet = vi.fn((): Promise<SettingsResult> => Promise.resolve({ ok: false, reason: 'foreign-file' }));
    installSettings(settings(), settingsSet);
    await open();
    fireEvent.click(screen.getByRole('checkbox', { name: 'Open Conch when I log in' }));
    await waitFor(() => {
      expect(settingsSet).toHaveBeenCalledWith({ launchAtLogin: true });
    });
    expect((await screen.findByRole('alert')).textContent).toMatch(/left alone/);
    expect(screen.getByRole('checkbox', { name: 'Open Conch when I log in' })).toHaveProperty('checked', false);
  });

  it('says so, not nothing, when the build cannot launch at login', async () => {
    installSettings(settings({ launchAvailable: false }), () => Promise.reject(new Error('unexpected')));
    await open();
    expect(screen.getByRole('checkbox', { name: 'Open Conch when I log in' })).toHaveProperty('disabled', true);
    expect(screen.getByText(/Only an installed copy of Conch/)).toBeDefined();
  });

  it('shows no section when the bridge has no settings (an older preload)', async () => {
    install({ profiles: () => Promise.resolve(active) });
    render(<DesktopShell>{child}</DesktopShell>);
    fireEvent.click(await screen.findByRole('button', { name: 'Desktop app' }));
    expect(screen.queryByRole('heading', { name: 'Desktop app' })).toBeNull();
  });
});

describe('the browser reply type', () => {
  it('is still the bridge’s', () => {
    const reply: BridgeReply = { kind: 'none' };
    expect(reply.kind).toBe('none');
  });
});

describe('server compatibility (issue #946)', () => {
  const on = (over: Partial<CompatView>) => view({ profiles: [profile(ID_A, 'chat.example.org', 'ada', true)], active: ID_A, compat: compat(over) });

  it('shows the client on a supported server, and no warning', async () => {
    install({ profiles: () => Promise.resolve(on({})) });
    render(<DesktopShell>{child}</DesktopShell>);
    await screen.findByText('the client');
    expect(screen.queryByRole('alert')).toBeNull();
    expect(screen.queryByText(/has not been tested/)).toBeNull();
  });

  it('shows the unsupported state instead of the client, with both versions and no sign-in, and asks nothing of the server', async () => {
    const restore = vi.fn(() => Promise.resolve({ ok: false as const, kind: 'unauthorized' as const, status: 401 }));
    install({ profiles: () => Promise.resolve(on({ state: 'unsupported', server: 'v9.9.9', desktop: '1.2.3' })), restore });
    render(<DesktopShell>{child}</DesktopShell>);
    const alert = await screen.findByText(/has not been tested with this app/);
    expect(alert.textContent).toBe('This server’s version (v9.9.9) has not been tested with this app (1.2.3).');
    expect(screen.queryByText('the client')).toBeNull();
    expect(screen.queryByLabelText('Password')).toBeNull();
    expect(restore).not.toHaveBeenCalled();
  });

  it('says "unknown" for a server that named no version', async () => {
    install({ profiles: () => Promise.resolve(on({ state: 'unsupported', server: null })) });
    render(<DesktopShell>{child}</DesktopShell>);
    expect((await screen.findByText(/has not been tested/)).textContent).toMatch(/version \(unknown\)/);
  });

  it('says a server that did not answer could not be checked, and Check again asks main to switch to it', async () => {
    const profileSwitch = vi.fn(() => Promise.resolve(true));
    install({ profiles: () => Promise.resolve(on({ state: 'unreachable' })), profileSwitch });
    render(<DesktopShell>{child}</DesktopShell>);
    expect((await screen.findByText(/could not be read/)).textContent).toMatch(/Nothing was signed in/);
    expect(screen.queryByText('the client')).toBeNull();
    fireEvent.click(screen.getByRole('button', { name: 'Check again' }));
    await waitFor(() => {
      expect(profileSwitch).toHaveBeenCalledWith(ID_A);
    });
  });

  it('offers no hand-off of an invite code for the blocked profile, and leaves the address empty', async () => {
    const CODE = 'CANARY-code-1';
    const handle = `invite-${'0'.repeat(32)}`;
    const inviteCheck = vi.fn(() =>
      Promise.resolve<InviteCheck>({
        ok: true,
        handle,
        origin: 'https://chat.example.org',
        host: 'chat.example.org',
        profiles: [{ id: ID_A, label: 'Work', account: 'ada', active: true }],
        voice: false,
      }),
    );
    const inviteAccept = vi.fn(() => Promise.resolve({ ok: true, switched: false } as const));
    const inviteTake = vi.fn(() => Promise.resolve(CODE));
    install({ profiles: () => Promise.resolve(on({ state: 'unsupported' })), inviteCheck, inviteAccept, inviteTake });
    render(<DesktopShell>{child}</DesktopShell>);
    fireEvent.change(await screen.findByLabelText('Invite link'), { target: { value: `https://chat.example.org/app/#/invite/${CODE}` } });
    fireEvent.click(screen.getByRole('button', { name: 'Check link' }));
    const group = await screen.findByRole('group', { name: 'Open this invite' });
    expect(within(group).queryByRole('button', { name: /Open with Work/ })).toBeNull();
    expect(group.textContent).toMatch(/release is not supported/);
    expect(within(group).getByRole('button', { name: 'Open as a new server entry' })).toBeTruthy();
    expect(inviteAccept).not.toHaveBeenCalled();
    expect(inviteTake).not.toHaveBeenCalled();
    expect(window.location.hash).toBe('');
  });

  it('lets the person go to another saved server from the unsupported state', async () => {
    const profileSwitch = vi.fn(() => Promise.resolve(true));
    install({
      profiles: () => Promise.resolve(view({ profiles: [profile(ID_A, 'a.example', 'ada', true), profile(ID_B, 'b.example', null, false)], active: ID_A, compat: compat({ state: 'unsupported' }) })),
      profileSwitch,
    });
    render(<DesktopShell>{child}</DesktopShell>);
    fireEvent.click(await screen.findByRole('button', { name: 'Switch to b.example' }));
    await waitFor(() => {
      expect(profileSwitch).toHaveBeenCalledWith(ID_B);
    });
  });

  it('shows the development warning persistently, and the untested version on an unlisted server it let through', async () => {
    install({ profiles: () => Promise.resolve(on({ state: 'unlisted', server: 'v9.9.9', override: true })) });
    render(<DesktopShell>{child}</DesktopShell>);
    await screen.findByText('the client');
    const note = screen.getByRole('note', { name: 'Development build' });
    expect(note.textContent).toMatch(/not on the tested list are allowed/);
    expect(note.textContent).toMatch(/version \(v9\.9\.9\) has not been tested with this app \(0\.0\.0\)/);
  });

  it('shows the development warning on the server list too', async () => {
    install({ profiles: () => Promise.resolve(view({ compat: compat({ state: 'none', server: null, override: true }) })) });
    render(<DesktopShell>{child}</DesktopShell>);
    expect((await screen.findByRole('note', { name: 'Development build' })).textContent).toMatch(/allowed/);
  });

  it('shows an answer that was not what the app expects as an incompatibility, not a network error', async () => {
    install({ profiles: () => Promise.resolve(on({ invalid: 'channels' })) });
    render(<DesktopShell>{child}</DesktopShell>);
    await screen.findByText('the client');
    const alert = screen.getByRole('alert');
    expect(alert.textContent).toMatch(/channels/);
    expect(alert.textContent).toMatch(/incompatib/i);
    expect(alert.textContent).toMatch(/v1\.0\.0/);
  });
});

describe('the update notice (issue #1600)', () => {
  const updated = { updateState: () => Promise.resolve({ updated: true, version: '0.1.0-alpha.18' }), updateRestart: () => Promise.resolve(), onUpdateState: () => () => undefined };

  it.each([
    ['the server list', view()],
    ['the recovery screen', view({ recovery: true })],
    ['the blocked screen', view({ profiles: [profile(ID_A, 'a.example', null, true)], active: ID_A, compat: compat({ state: 'unsupported' }) })],
  ] as const)('is shown on %s', async (_name, shown) => {
    install({ profiles: () => Promise.resolve(shown), ...updated });
    render(<DesktopShell>{child}</DesktopShell>);
    expect(await screen.findByText('Conch was updated to 0.1.0-alpha.18. Restart when you’re ready.')).toBeTruthy();
    expect(screen.queryByText('the client')).toBeNull();
  });
});

describe('opening an invite link (issue #1591)', () => {
  const active = view({ profiles: [profile(ID_A, 'chat.example.org', 'ada', true)], active: ID_A });
  const CODE = 'CANARY-code-1';
  const HANDLE = `invite-${'0'.repeat(32)}`;
  const LINK = `https://chat.example.org/app/#/invite/${CODE}`;

  function checkFor(over: Partial<Extract<InviteCheck, { ok: true }>> = {}): InviteCheck {
    return {
      ok: true,
      handle: HANDLE,
      origin: 'https://chat.example.org',
      host: 'chat.example.org',
      profiles: [{ id: ID_A, label: 'Work', account: 'ada', active: false }],
      voice: false,
      ...over,
    };
  }

  it('takes the staged code once, before the client mounts, and hands it to the client in the address', async () => {
    const inviteTake = vi.fn().mockResolvedValueOnce(CODE).mockResolvedValue(null);
    install({ profiles: () => Promise.resolve(active), inviteTake });
    const seen: string[] = [];
    function Client() {
      seen.push(window.location.hash);
      return <p>the client</p>;
    }
    render(
      <DesktopShell>
        <Client />
      </DesktopShell>,
    );
    await screen.findByText('the client');
    expect(inviteTake).toHaveBeenCalledOnce();
    expect(seen[0]).toBe(`#/invite/${CODE}`);
    // The code is not in the page's text.
    expect(document.body.textContent).not.toContain('CANARY');
  });

  it('hands over nothing when main has no code for this profile', async () => {
    const inviteTake = vi.fn(() => Promise.resolve(null));
    install({ profiles: () => Promise.resolve(active), inviteTake });
    render(<DesktopShell>{child}</DesktopShell>);
    await screen.findByText('the client');
    expect(inviteTake).toHaveBeenCalledOnce();
    expect(window.location.hash).toBe('');
  });

  it('does not stop the client when taking fails', async () => {
    install({ profiles: () => Promise.resolve(active), inviteTake: () => Promise.reject(new Error('refused')) });
    render(<DesktopShell>{child}</DesktopShell>);
    await screen.findByText('the client');
    expect(window.location.hash).toBe('');
  });

  it('shows the server and the saved profile with its account, and acts only on a click', async () => {
    const inviteCheck = vi.fn(() => Promise.resolve(checkFor()));
    const inviteAccept = vi.fn(() => Promise.resolve({ ok: true, switched: true } as const));
    const profileSwitch = vi.fn(() => Promise.resolve(true));
    const profileAdd = vi.fn();
    install({ profiles: () => Promise.resolve(view()), inviteCheck, inviteAccept, profileSwitch, profileAdd });
    render(<DesktopShell>{child}</DesktopShell>);
    const field = await screen.findByLabelText('Invite link');
    fireEvent.change(field, { target: { value: LINK } });
    fireEvent.click(screen.getByRole('button', { name: 'Check link' }));
    const group = await screen.findByRole('group', { name: 'Open this invite' });
    expect(group.textContent).toContain('This invite is for https://chat.example.org.');
    expect(inviteCheck).toHaveBeenCalledWith(LINK);
    // The link and its code are gone from the page; nothing has happened.
    expect((field as HTMLInputElement).value).toBe('');
    expect(document.body.textContent).not.toContain('CANARY');
    expect(inviteAccept).not.toHaveBeenCalled();
    expect(profileSwitch).not.toHaveBeenCalled();
    expect(profileAdd).not.toHaveBeenCalled();
    fireEvent.click(within(group).getByRole('button', { name: 'Open with Work (signed in as ada)' }));
    await waitFor(() => {
      expect(inviteAccept).toHaveBeenCalledWith(HANDLE, { profile: ID_A });
    });
    expect(profileSwitch).not.toHaveBeenCalled();
    expect(profileAdd).not.toHaveBeenCalled();
    await screen.findByText('Opening…');
  });

  it('offers to add the server when none is saved for it, and adds only on the click', async () => {
    const inviteAccept = vi.fn(() => Promise.resolve({ ok: true, switched: true } as const));
    install({ profiles: () => Promise.resolve(view()), inviteCheck: () => Promise.resolve(checkFor({ profiles: [] })), inviteAccept });
    render(<DesktopShell>{child}</DesktopShell>);
    fireEvent.change(await screen.findByLabelText('Invite link'), { target: { value: LINK } });
    fireEvent.click(screen.getByRole('button', { name: 'Check link' }));
    const group = await screen.findByRole('group', { name: 'Open this invite' });
    expect(inviteAccept).not.toHaveBeenCalled();
    fireEvent.click(within(group).getByRole('button', { name: 'Add this server and open the invite' }));
    await waitFor(() => {
      expect(inviteAccept).toHaveBeenCalledWith(HANDLE, { add: true });
    });
  });

  it('says voice will be left, and declining changes nothing and spends the handle', async () => {
    const inviteCancel = vi.fn(() => Promise.resolve(true));
    const inviteAccept = vi.fn();
    install({ profiles: () => Promise.resolve(view()), inviteCheck: () => Promise.resolve(checkFor({ voice: true })), inviteAccept, inviteCancel });
    render(<DesktopShell>{child}</DesktopShell>);
    fireEvent.change(await screen.findByLabelText('Invite link'), { target: { value: LINK } });
    fireEvent.click(screen.getByRole('button', { name: 'Check link' }));
    const group = await screen.findByRole('group', { name: 'Open this invite' });
    expect(within(group).getByRole('status').textContent).toMatch(/Voice is running\. .* leaves voice/);
    fireEvent.click(within(group).getByRole('button', { name: 'Cancel' }));
    await waitFor(() => {
      expect(inviteCancel).toHaveBeenCalledWith(HANDLE);
    });
    expect(inviteAccept).not.toHaveBeenCalled();
    expect(screen.queryByRole('group', { name: 'Open this invite' })).toBeNull();
  });

  it('opens the invite without a switch for the profile in force: takes the code and hands it to the client', async () => {
    const inviteTake = vi.fn().mockResolvedValueOnce(null).mockResolvedValueOnce(CODE).mockResolvedValue(null);
    const inviteAccept = vi.fn(() => Promise.resolve({ ok: true, switched: false } as const));
    const profileSwitch = vi.fn();
    install({
      profiles: () => Promise.resolve(active),
      inviteCheck: () => Promise.resolve(checkFor({ profiles: [{ id: ID_A, label: 'Work', account: 'ada', active: true }] })),
      inviteAccept,
      inviteTake,
      profileSwitch,
    });
    render(<DesktopShell>{child}</DesktopShell>);
    await screen.findByText('the client');
    fireEvent.click(screen.getByRole('button', { name: 'Servers' }));
    fireEvent.change(await screen.findByLabelText('Invite link'), { target: { value: LINK } });
    fireEvent.click(screen.getByRole('button', { name: 'Check link' }));
    fireEvent.click(await screen.findByRole('button', { name: 'Open with Work (signed in as ada)' }));
    await waitFor(() => {
      expect(window.location.hash).toBe(`#/invite/${CODE}`);
    });
    expect(profileSwitch).not.toHaveBeenCalled();
    expect(inviteTake).toHaveBeenCalledTimes(2);
  });

  it.each([
    ['scheme', /secure \(https\) address/],
    ['userinfo', /user name or password/],
    ['host', /look-alike characters/],
    ['code', /code in that link/],
  ] as const)('says why a link was refused (%s) without repeating it', async (refusal, words) => {
    install({ profiles: () => Promise.resolve(view()), inviteCheck: () => Promise.resolve({ ok: false, refusal }) });
    render(<DesktopShell>{child}</DesktopShell>);
    fireEvent.change(await screen.findByLabelText('Invite link'), { target: { value: LINK } });
    fireEvent.click(screen.getByRole('button', { name: 'Check link' }));
    await waitFor(() => {
      expect(screen.getAllByRole('alert').map((a) => a.textContent).join(' ')).toMatch(words);
    });
    expect(document.body.textContent).not.toContain('CANARY');
    expect(screen.queryByRole('group', { name: 'Open this invite' })).toBeNull();
  });

  // A conch:// link (issue #1592): main has checked it; the page is handed
  // the same answer as a pasted link gets, and acts only on a click.
  it('shows a link that arrived before the page did, as the same choice, and does nothing until the click', async () => {
    const inviteArrived = vi.fn().mockResolvedValueOnce(checkFor()).mockResolvedValue(null);
    const inviteAccept = vi.fn(() => Promise.resolve({ ok: true, switched: true } as const));
    const profileSwitch = vi.fn();
    const profileAdd = vi.fn();
    install({ profiles: () => Promise.resolve(view()), inviteCheck: () => Promise.reject(new Error('unexpected')), inviteArrived, onInviteArrived: () => () => undefined, inviteAccept, profileSwitch, profileAdd });
    render(<DesktopShell>{child}</DesktopShell>);
    const group = await screen.findByRole('group', { name: 'Open this invite' });
    expect(group.textContent).toContain('This invite is for https://chat.example.org.');
    expect(document.body.textContent).not.toContain('CANARY');
    expect(inviteAccept).not.toHaveBeenCalled();
    expect(profileSwitch).not.toHaveBeenCalled();
    expect(profileAdd).not.toHaveBeenCalled();
    fireEvent.click(within(group).getByRole('button', { name: 'Open with Work (signed in as ada)' }));
    await waitFor(() => {
      expect(inviteAccept).toHaveBeenCalledWith(HANDLE, { profile: ID_A });
    });
  });

  it('opens the server panel over the client when a link arrives, and a newer link replaces and cancels the older', async () => {
    const second = `invite-${'1'.repeat(32)}`;
    let tell: () => void = () => undefined;
    const inviteArrived = vi.fn().mockResolvedValueOnce(null).mockResolvedValueOnce(checkFor()).mockResolvedValueOnce(checkFor({ handle: second, origin: 'https://other.example.org', host: 'other.example.org', profiles: [] })).mockResolvedValue(null);
    const inviteCancel = vi.fn(() => Promise.resolve(true));
    install({
      profiles: () => Promise.resolve(active),
      inviteCheck: () => Promise.reject(new Error('unexpected')),
      inviteArrived,
      onInviteArrived: (listener) => {
        tell = listener;
        return () => undefined;
      },
      inviteCancel,
    });
    render(<DesktopShell>{child}</DesktopShell>);
    await screen.findByText('the client');
    expect(screen.queryByLabelText('Invite link')).toBeNull();
    // The ask at mount has been answered (nothing waiting) before main speaks.
    await waitFor(() => {
      expect(inviteArrived).toHaveBeenCalledTimes(1);
    });
    tell();
    const group = await screen.findByRole('group', { name: 'Open this invite' });
    expect(group.textContent).toContain('https://chat.example.org');
    tell();
    await waitFor(() => {
      expect(screen.getByRole('group', { name: 'Open this invite' }).textContent).toContain('https://other.example.org');
    });
    expect(inviteCancel).toHaveBeenCalledExactlyOnceWith(HANDLE);
    expect(document.body.textContent).not.toContain('CANARY');
  });

  // Security review S1: a newer link replaces the choice in place. The page
  // holds the buttons still for the interval and says why; main refuses the
  // handle in the same interval, whatever the page does.
  it('holds the choices still for a moment after a newer link replaces them, and works after', async () => {
    const second = `invite-${'1'.repeat(32)}`;
    let tell: () => void = () => undefined;
    const inviteArrived = vi
      .fn()
      .mockResolvedValueOnce(null)
      .mockResolvedValueOnce(checkFor())
      .mockResolvedValueOnce(checkFor({ handle: second, profiles: [{ id: ID_A, label: 'Work', account: 'ada', active: false }] }))
      .mockResolvedValue(null);
    const inviteAccept = vi
      .fn()
      .mockResolvedValueOnce({ ok: false, refusal: 'just-arrived' } as const)
      .mockResolvedValue({ ok: true, switched: true } as const);
    install({
      profiles: () => Promise.resolve(active),
      inviteCheck: () => Promise.reject(new Error('unexpected')),
      inviteArrived,
      onInviteArrived: (listener) => {
        tell = listener;
        return () => undefined;
      },
      inviteCancel: () => Promise.resolve(true),
      inviteAccept,
    });
    render(<DesktopShell>{child}</DesktopShell>);
    await screen.findByText('the client');
    await waitFor(() => {
      expect(inviteArrived).toHaveBeenCalledTimes(1);
    });
    tell();
    const first = await screen.findByRole('group', { name: 'Open this invite' });
    // A first arrival is not a swap: nothing is held on the page.
    expect(within(first).queryByText('A new invite link arrived')).toBeNull();
    expect((within(first).getByRole<HTMLButtonElement>('button', { name: 'Open with Work (signed in as ada)' })).disabled).toBe(false);
    // From here only the timer is faked, so the waits above are real.
    vi.useFakeTimers({ toFake: ['setTimeout', 'clearTimeout'] });
    // No waitFor while the timers are faked: act settles the answer.
    await act(async () => {
      tell();
      await Promise.resolve();
    });
    const group = screen.getByRole('group', { name: 'Open this invite' });
    expect(within(group).getByText('A new invite link arrived')).toBeTruthy();
    const choose = within(group).getByRole<HTMLButtonElement>('button', { name: 'Open with Work (signed in as ada)' });
    const add = within(group).getByRole<HTMLButtonElement>('button', { name: 'Open as a new server entry' });
    expect(choose.disabled).toBe(true);
    expect(add.disabled).toBe(true);
    expect((within(group).getByRole<HTMLButtonElement>('button', { name: 'Cancel' })).disabled).toBe(false);
    fireEvent.click(choose);
    expect(inviteAccept).not.toHaveBeenCalled();
    act(() => {
      vi.advanceTimersByTime(SWAP_GUARD_MS);
    });
    expect(choose.disabled).toBe(false);
    expect(within(group).queryByText('A new invite link arrived')).toBeNull();
    vi.useRealTimers();
    // Main's own guard is the one that counts: when it refuses, the page says so and keeps the choice.
    fireEvent.click(choose);
    await screen.findByText('A new invite link just arrived. Check the server, then choose again.');
    expect(screen.getByRole('group', { name: 'Open this invite' })).toBeTruthy();
    fireEvent.click(screen.getByRole('button', { name: 'Open with Work (signed in as ada)' }));
    await waitFor(() => {
      expect(inviteAccept).toHaveBeenLastCalledWith(second, { profile: ID_A });
    });
    expect(inviteAccept).toHaveBeenCalledTimes(2);
  });

  it('ignores a failed or empty ask for an arrived link', async () => {
    install({ profiles: () => Promise.resolve(view()), inviteCheck: () => Promise.reject(new Error('x')), inviteArrived: () => Promise.reject(new Error('refused')), onInviteArrived: () => () => undefined });
    render(<DesktopShell>{child}</DesktopShell>);
    await screen.findByLabelText('Invite link');
    expect(screen.queryByRole('group', { name: 'Open this invite' })).toBeNull();
    expect(screen.queryAllByRole('alert').map((a) => a.textContent).join(' ')).not.toMatch(/could not/);
  });

  it('has no invite field when the bridge has no invite functions', async () => {
    install({ profiles: () => Promise.resolve(view()) });
    render(<DesktopShell>{child}</DesktopShell>);
    await screen.findByLabelText('Server address');
    expect(screen.queryByLabelText('Invite link')).toBeNull();
  });
});
