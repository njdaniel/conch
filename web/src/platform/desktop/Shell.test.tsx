import { cleanup, fireEvent, render, screen, waitFor } from '@testing-library/react';
import { afterEach, describe, expect, it, vi } from 'vitest';
import type { BridgeReply, ConchDesktopBridge, SetServerAddressResult } from './bridge';
import { credentials } from './credentials';
import { DesktopShell } from './Shell';
import { serverOrigin, setServerOrigin } from './state';

function install(overrides: Partial<ConchDesktopBridge>): void {
  const never = () => Promise.reject(new Error('unexpected bridge call'));
  const bridge: ConchDesktopBridge = {
    serverAddress: () => Promise.resolve({ origin: null }),
    setServerAddress: never,
    request: never,
    subscribe: () => () => undefined,
    restore: () => Promise.resolve({ ok: false, kind: 'unauthorized', status: 401 }),
    signInWithCredential: never,
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
    ...overrides,
  };
  Object.defineProperty(window, 'conchDesktop', { value: bridge, configurable: true, writable: true });
}

afterEach(() => {
  cleanup();
  Reflect.deleteProperty(window, 'conchDesktop');
  setServerOrigin('');
  const held = credentials.read();
  if (held !== null) credentials.drop(held);
});

const child = <p>the client</p>;

describe('the Server address step', () => {
  it('asks for an address before the client, and starts the client once the server answers', async () => {
    const setServerAddress = vi.fn((): Promise<SetServerAddressResult> => Promise.resolve({ ok: true, origin: 'https://chat.example.org' }));
    const request = vi.fn((): Promise<BridgeReply> => Promise.resolve({ kind: 'reply', status: 200, body: {} }));
    install({ setServerAddress, request });
    render(<DesktopShell>{child}</DesktopShell>);
    const field = await screen.findByLabelText('Server address');
    expect(screen.queryByText('the client')).toBeNull();
    fireEvent.change(field, { target: { value: 'https://chat.example.org' } });
    fireEvent.click(screen.getByRole('button', { name: 'Continue' }));
    await screen.findByText('the client');
    expect(setServerAddress).toHaveBeenCalledWith('https://chat.example.org');
    expect(request).toHaveBeenCalledWith({ operation: 'health' });
    expect(serverOrigin()).toBe('https://chat.example.org');
  });

  it.each([
    ['scheme', /secure \(https\)/],
    ['userinfo', /user name and password/],
    ['path', /no path/],
    ['malformed', /not a web address/],
  ] as const)('says why an address was refused: %s', async (refusal, words) => {
    install({ setServerAddress: () => Promise.resolve({ ok: false, refusal }) });
    render(<DesktopShell>{child}</DesktopShell>);
    fireEvent.change(await screen.findByLabelText('Server address'), { target: { value: 'x' } });
    fireEvent.click(screen.getByRole('button', { name: 'Continue' }));
    expect((await screen.findByRole('alert')).textContent).toMatch(words);
    expect(screen.queryByText('the client')).toBeNull();
  });

  it('says a certificate that is not trusted is that, and stays on the step', async () => {
    install({
      setServerAddress: () => Promise.resolve({ ok: true, origin: 'https://private.example' }),
      request: () => Promise.resolve({ kind: 'none', reason: 'certificate' }),
    });
    render(<DesktopShell>{child}</DesktopShell>);
    fireEvent.change(await screen.findByLabelText('Server address'), { target: { value: 'https://private.example' } });
    fireEvent.click(screen.getByRole('button', { name: 'Continue' }));
    expect((await screen.findByRole('alert')).textContent).toMatch(/does not trust that server’s certificate/);
    expect(screen.queryByText('the client')).toBeNull();
  });

  it('says a server that does not answer is that, and a server that is no Conch server', async () => {
    const reply: { current: BridgeReply } = { current: { kind: 'none' } };
    install({ setServerAddress: () => Promise.resolve({ ok: true, origin: 'https://x.example' }), request: () => Promise.resolve(reply.current) });
    render(<DesktopShell>{child}</DesktopShell>);
    fireEvent.change(await screen.findByLabelText('Server address'), { target: { value: 'https://x.example' } });
    fireEvent.click(screen.getByRole('button', { name: 'Continue' }));
    expect((await screen.findByRole('alert')).textContent).toMatch(/Could not reach https:\/\/x\.example/);
    reply.current = { kind: 'reply', status: 404 };
    fireEvent.click(screen.getByRole('button', { name: 'Continue' }));
    await waitFor(() => {
      expect(screen.getByRole('alert').textContent).toMatch(/not as a Conch server/);
    });
  });

  it('skips the step when main already has an address, and restores the stand-in for a sign-in main holds', async () => {
    install({
      serverAddress: () => Promise.resolve({ origin: 'https://chat.example.org' }),
      restore: () => Promise.resolve({ ok: true, who: { id: 1, kind: 'human', name: 'ada', role: 'member' }, answer: {} }),
    });
    render(<DesktopShell>{child}</DesktopShell>);
    await screen.findByText('the client');
    expect(serverOrigin()).toBe('https://chat.example.org');
    expect(credentials.read()?.place).toBe('lasting');
  });

  it('keeps an operator’s and a guest’s stand-in with this window, not lasting', async () => {
    install({
      serverAddress: () => Promise.resolve({ origin: 'https://chat.example.org' }),
      restore: () => Promise.resolve({ ok: true, who: { id: 1, kind: 'human', name: 'op', role: 'operator' }, answer: {} }),
    });
    render(<DesktopShell>{child}</DesktopShell>);
    await screen.findByText('the client');
    expect(credentials.read()?.place).toBe('tab');
  });

  it('says so when the bridge is missing', async () => {
    render(<DesktopShell>{child}</DesktopShell>);
    expect((await screen.findByRole('alert')).textContent).toMatch(/connection to its main process is missing/);
  });
});
