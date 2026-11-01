import { act, cleanup, fireEvent, render, screen, waitFor, within } from '@testing-library/react';
import { afterEach, describe, expect, it, vi } from 'vitest';
import type { BridgeReply, ConchDesktopBridge, ProfileAddResult, ProfilesView, ProfileView } from './bridge';
import { credentials } from './credentials';
import { DesktopShell } from './Shell';
import { serverOrigin, setServerOrigin } from './state';

const ID_A = 'a'.repeat(32);
const ID_B = 'b'.repeat(32);

function profile(id: string, host: string, account: string | null, active: boolean): ProfileView {
  return { id, label: host, host, origin: `https://${host}`, account, active };
}

function view(over: Partial<ProfilesView> = {}): ProfilesView {
  return { profiles: [], active: null, storage: { kind: 'protected' }, recovery: false, notice: null, platform: 'linux', ...over };
}

function install(overrides: Partial<ConchDesktopBridge>): void {
  const never = () => Promise.reject(new Error('unexpected bridge call'));
  const bridge: ConchDesktopBridge = {
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
    profiles: () => Promise.resolve(view()),
    profileAdd: never,
    profileRename: never,
    profileRemove: never,
    profileSwitch: never,
    storageRetry: never,
    profilesRecover: never,
    ...overrides,
  };
  Object.defineProperty(window, 'conchDesktop', { value: bridge, configurable: true, writable: true });
}

afterEach(() => {
  cleanup();
  vi.useRealTimers();
  Reflect.deleteProperty(window, 'conchDesktop');
  setServerOrigin('');
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

describe('the browser reply type', () => {
  it('is still the bridge’s', () => {
    const reply: BridgeReply = { kind: 'none' };
    expect(reply.kind).toBe('none');
  });
});
