// The desktop sign-in form's "Keep me signed in on this computer" choice and
// storage status (issue #2077), in the shared SignIn markup with the desktop
// platform's KeepSignIn. The bridge is a fake: what main says of storage is
// its view (ProfilesView), and the choice goes back as profileKeep.
import { cleanup, fireEvent, render, screen, waitFor, within } from '@testing-library/react';
import { afterEach, describe, expect, it, vi } from 'vitest';

import type { ConchDesktopBridge, ProfilesView, StorageStatus } from './platform/desktop/bridge';
import { KeepSignIn } from './platform/desktop/ui/KeepSignIn';
import { SignIn } from './SignIn';

vi.mock('./api', () => ({
  joinInfo: () => Promise.resolve({ ok: false, failure: 'unreachable' }),
  serverHealth: () => Promise.resolve(null),
  createAccount: vi.fn(),
  codeDigits: 6,
}));
vi.mock('./platform', async () => {
  const { KeepSignIn: Keep } = await import('./platform/desktop/ui/KeepSignIn');
  return { platform: { name: 'desktop', SignInKeep: Keep, services: {} } };
});

const HELP = 'https://github.com/njdaniel/conch/blob/main/docs/desktop/linux.md#staying-signed-in';

function view(over: Partial<ProfilesView> = {}): ProfilesView {
  return {
    profiles: [{ id: 'a'.repeat(32), label: 'a.example', host: 'a.example', origin: 'https://a.example', account: null, active: true }],
    active: 'a'.repeat(32),
    storage: { kind: 'protected' },
    keep: true,
    keepHelp: HELP,
    recovery: false,
    notice: null,
    compat: { state: 'supported', server: 'v1.0.0', desktop: '0.0.0', override: false, invalid: null },
    platform: 'linux',
    ...over,
  };
}

function install(first: ProfilesView, more: Partial<ConchDesktopBridge> = {}): { profileKeep: ReturnType<typeof vi.fn>; storageRetry: ReturnType<typeof vi.fn> } {
  const profileKeep = vi.fn((keep: boolean) => Promise.resolve({ ...first, keep }));
  const storageRetry = vi.fn(() => Promise.resolve(first));
  const bridge = { profiles: () => Promise.resolve(first), profileKeep, storageRetry, ...more };
  Object.defineProperty(window, 'conchDesktop', { value: bridge, configurable: true, writable: true });
  return { profileKeep, storageRetry };
}

function signIn(onPassword = vi.fn()) {
  render(<SignIn origin="https://a.example" notice={null} checking={false} onSubmit={vi.fn()} onPassword={onPassword} onForget={vi.fn()} onJoined={vi.fn()} />);
  return onPassword;
}

const keepBox = () => screen.findByRole<HTMLInputElement>('checkbox', { name: 'Keep me signed in on this computer' });
const status = () => screen.findByRole('status', { name: 'Staying signed in' });

afterEach(() => {
  cleanup();
  Reflect.deleteProperty(window, 'conchDesktop');
});

describe('Keep me signed in on this computer', () => {
  it('is in the password form, on by default where storage is protected, and says the sign-in goes to the keyring', async () => {
    install(view());
    signIn();
    const box = await keepBox();
    expect(box.checked).toBe(true);
    expect(box.disabled).toBe(false);
    const form = screen.getByLabelText('Password').closest('form');
    expect(form).not.toBeNull();
    expect(within(form as HTMLFormElement).getByRole('checkbox', { name: 'Keep me signed in on this computer' })).toBe(box);
    expect((await status()).textContent).toContain('Your sign-in will be kept in the system keyring.');
    // The operator rule is said before the sign-in, not only after it.
    expect((await status()).textContent).toContain('Operator sign-ins are never kept');
    expect(screen.queryByRole('button', { name: 'Check again' })).toBeNull();
  });

  it('unchecked, it tells main, and the password still goes only to the sign-in', async () => {
    const { profileKeep } = install(view());
    const onPassword = signIn();
    const box = await keepBox();
    fireEvent.click(box);
    await waitFor(() => {
      expect(profileKeep).toHaveBeenCalledWith(false);
    });
    await waitFor(() => {
      expect(box.checked).toBe(false);
    });
    fireEvent.change(screen.getByLabelText('Name'), { target: { value: 'ada' } });
    fireEvent.change(screen.getByLabelText('Password'), { target: { value: 'password-canary' } });
    fireEvent.click(screen.getByRole('button', { name: 'Sign in' }));
    expect(onPassword).toHaveBeenCalledWith('ada', 'password-canary');
    expect(JSON.stringify(profileKeep.mock.calls)).not.toContain('password-canary');
  });

  it('shows main’s choice for the window, not a default of its own', async () => {
    install(view({ keep: false }));
    signIn();
    expect((await keepBox()).checked).toBe(false);
  });

  // What main does is what the box and the line say (#2077 review): main
  // keeps a member's sign-in if the keyring answers by the time of the
  // sign-in, whatever the form saw when it opened, so the box shows main's
  // choice, unchecking is always allowed, and checking is only where storage
  // is protected.
  it.each<[string, StorageStatus, string, string, boolean]>([
    ['no keyring unlocked', { kind: 'session-only', reason: 'no-keyring' }, 'linux', 'No system keyring is unlocked, so you’ll be asked to sign in each time Conch starts.', true],
    ['a keyring that protects nothing', { kind: 'session-only', reason: 'insecure-backend' }, 'linux', 'This desktop gives Conch no keyring that protects anything, so you’ll be asked to sign in each time Conch starts.', false],
    ['no secret store', { kind: 'session-only', reason: 'unavailable' }, 'linux', 'This computer has no secret store, so you’ll be asked to sign in each time Conch starts.', false],
    ['no Windows protection', { kind: 'session-only', reason: 'unavailable' }, 'win32', 'Windows credential protection is unavailable, so you’ll be asked to sign in each time Conch starts.', false],
    ['an unknown keyring', { kind: 'session-only', reason: 'unknown-backend' }, 'linux', 'Conch can’t tell that this computer’s keyring is a safe one, so you’ll be asked to sign in each time Conch starts.', false],
    ['a data folder that is not private', { kind: 'session-only', reason: 'unsafe-root' }, 'linux', 'Conch’s data folder is not private to you, so nothing is saved and you’ll be asked to sign in each time Conch starts.', false],
    ['a keyring that has not answered', { kind: 'unavailable' }, 'linux', 'The system keyring has not answered yet.', true],
  ])('%s: the box shows main’s choice, the line says why, unchecking is allowed and checking is not, and Check again asks again', async (_name, storage, platform, line, mayYetKeep) => {
    const { profileKeep, storageRetry } = install(view({ storage, platform }));
    signIn();
    const box = await keepBox();
    expect(box.checked).toBe(true);
    expect(box.disabled).toBe(false);
    const said = (await status()).textContent;
    expect(said).toContain(line);
    // Where the keyring may still answer before the sign-in, the line says
    // that the sign-in will then be kept; where it cannot, it does not.
    expect(said.includes('your sign-in will be kept in it')).toBe(mayYetKeep);
    fireEvent.click(box);
    await waitFor(() => {
      expect(profileKeep).toHaveBeenCalledWith(false);
    });
    await waitFor(() => {
      expect(box.checked).toBe(false);
    });
    // Off here cannot be turned back on: there is nothing to keep it in.
    expect(box.disabled).toBe(true);
    expect((await status()).textContent).toContain('This sign-in will last only until Conch closes');
    fireEvent.click(screen.getByRole('button', { name: 'Check again' }));
    await waitFor(() => {
      expect(storageRetry).toHaveBeenCalledTimes(1);
    });
  });

  it('never says the sign-in will not be kept while main will keep it: no keyring yet, choice on', async () => {
    install(view({ storage: { kind: 'session-only', reason: 'no-keyring' }, keep: true }));
    signIn();
    expect((await keepBox()).checked).toBe(true);
    expect((await status()).textContent).toContain('If you unlock one before you sign in, your sign-in will be kept in it');
  });

  it('with the choice off on protected storage, says the sign-in lasts only until Conch closes', async () => {
    install(view({ keep: false }));
    signIn();
    const box = await keepBox();
    expect(box.checked).toBe(false);
    expect(box.disabled).toBe(false);
    const said = (await status()).textContent;
    expect(said).toContain('This sign-in will last only until Conch closes');
    expect(said).not.toContain('will be kept in the system keyring');
  });

  it.each<[string, string]>([
    ['win32', 'Your sign-in will be kept on this computer, sealed by Windows for your account.'],
    ['darwin', 'Your sign-in will be kept in your macOS keychain.'],
  ])('protected storage on %s says where it goes', async (platform, line) => {
    install(view({ platform }));
    signIn();
    expect((await status()).textContent).toContain(line);
  });

  it('a check that finds the keyring unlocked turns the choice on', async () => {
    install(view({ storage: { kind: 'session-only', reason: 'no-keyring' } }), { storageRetry: () => Promise.resolve(view()) });
    signIn();
    fireEvent.click(await screen.findByRole('button', { name: 'Check again' }));
    await waitFor(async () => {
      expect((await keepBox()).checked).toBe(true);
    });
    expect((await status()).textContent).toContain('kept in the system keyring');
  });

  it('links the help main names, and none where main names none', async () => {
    install(view());
    signIn();
    const link = await screen.findByRole('link', { name: 'How staying signed in works' });
    expect(link.getAttribute('href')).toBe(HELP);
    expect(link.getAttribute('target')).toBe('_blank');
    cleanup();
    install(view({ platform: 'win32', keepHelp: null }));
    signIn();
    await keepBox();
    expect(screen.queryByRole('link', { name: 'How staying signed in works' })).toBeNull();
  });

  it('says it is checking before main has answered', () => {
    install(view(), { profiles: () => new Promise<ProfilesView>(() => undefined) });
    render(<KeepSignIn />);
    expect(screen.getByRole('status').textContent).toContain('Checking whether this computer can keep your sign-in');
  });
});
