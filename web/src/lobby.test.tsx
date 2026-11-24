// The lobby's panels and layout (issue #536): Settings and, on a narrow
// window, Members open over the page, take focus, and give it back when
// Escape, their close button or a click outside closes them. Nothing in the
// voice strip or the conversation is mounted again when a panel opens or the
// window changes width: a call or a share there carries on.
import { act, cleanup, fireEvent, render, screen, within } from '@testing-library/react';
import { useContext, useEffect, useRef, type ReactNode } from 'react';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { windowOf } from './fakewindow';
import { Lobby } from './Lobby';
import { DesktopOS } from './platform/os';
import { VoiceSheet } from './phone';
import { usePageModal } from './pageModal';
import { initialChannels } from './state/channels';

const events: string[] = [];

// Probe records when it is mounted and unmounted.
function Probe({ name }: { readonly name: string }) {
  useEffect(() => {
    events.push(`mount ${name}`);
    return () => {
      events.push(`unmount ${name}`);
    };
  }, [name]);
  return <p>{name}</p>;
}

function lobby(voice: ReactNode = <Probe name="voice" />, os: string | null = null, overlay?: ReactNode) {
  return render(lobbyTree(voice, os, overlay));
}

function lobbyTree(voice: ReactNode, os: string | null, overlay?: ReactNode) {
  return (
    <DesktopOS.Provider value={os}><Lobby
      overlay={overlay}
      origin="http://127.0.0.1:8080"
      name="ada"
      me={2}
      channels={initialChannels}
      unread={new Map()}
      connection="connected"
      conversation={<Probe name="conversation" />}
      panel={<p>Nobody</p>}
      voice={voice}
      invites={null}
      onOpen={() => undefined}
      onRetry={() => undefined}
      onSignOut={() => undefined}
      onSignOutEverywhere={() => undefined}
      signOutProblem={null}
    /></DesktopOS.Provider>
  );
}

beforeEach(() => {
  events.length = 0;
  // The server version is asked for; this test has no server.
  vi.stubGlobal(
    'fetch',
    vi.fn(() => Promise.reject(new TypeError('Failed to fetch'))),
  );
});
afterEach(() => {
  cleanup();
  vi.unstubAllGlobals();
});

describe('Settings', () => {
  it('opens as a panel with focus on its heading; Escape closes it and focus goes back to Settings', async () => {
    windowOf(true);
    lobby();
    const button = screen.getByRole('button', { name: /^Settings/ });
    expect(button.getAttribute('aria-expanded')).toBe('false');
    expect(screen.queryByRole('dialog', { name: 'Settings' })).toBeNull();
    fireEvent.click(button);
    const panel = screen.getByRole('dialog', { name: 'Settings' });
    expect(button.getAttribute('aria-expanded')).toBe('true');
    expect(document.activeElement).toBe(screen.getByRole('heading', { name: 'Settings' }));
    expect(panel.textContent).toContain('Signed in as ada');
    expect(panel.textContent).toContain('in every tab of this browser');
    // Modal: the rest of the page is inert while it is open, and only then.
    expect(panel.getAttribute('aria-modal')).toBe('true');
    expect(button.closest('[inert]')).not.toBeNull();
    fireEvent.keyDown(screen.getByRole('button', { name: 'Sign out' }), { key: 'Escape' });
    expect(screen.queryByRole('dialog', { name: 'Settings' })).toBeNull();
    expect(document.activeElement).toBe(button);
    expect(button.closest('[inert]')).toBeNull();
    await act(async () => {});
  });

  it('uses app wording in desktop Settings and the sign-out confirmation', () => {
    windowOf(true);
    lobby(undefined, 'win32');
    fireEvent.click(screen.getByRole('button', { name: /^Settings/ }));
    const panel = screen.getByRole('dialog', { name: 'Settings' });
    expect(panel.textContent).toContain('Sign out ends this sign-in in this app.');
    expect(panel.textContent).not.toContain('browser');
    fireEvent.click(within(panel).getByRole('button', { name: 'Sign out' }));
    expect(within(panel).getByText(/Sign out of this app\?/)).toBeDefined();
  });

  it('closes on its close button and on a click outside, each time giving focus back', () => {
    windowOf(true);
    const { container } = lobby();
    const button = screen.getByRole('button', { name: /^Settings/ });
    fireEvent.click(button);
    fireEvent.click(within(screen.getByRole('dialog', { name: 'Settings' })).getByRole('button', { name: 'Close' }));
    expect(screen.queryByRole('dialog', { name: 'Settings' })).toBeNull();
    expect(document.activeElement).toBe(button);
    fireEvent.click(button);
    const outside = container.querySelector('.backdrop');
    if (outside === null) throw new Error('no backdrop');
    fireEvent.click(outside);
    expect(screen.queryByRole('dialog', { name: 'Settings' })).toBeNull();
    expect(document.activeElement).toBe(button);
  });

  it('asks before signing out, and Escape there leaves the question and the panel', () => {
    windowOf(true);
    lobby();
    fireEvent.click(screen.getByRole('button', { name: /^Settings/ }));
    fireEvent.click(screen.getByRole('button', { name: 'Sign out' }));
    const cancel = screen.getByRole('button', { name: 'Cancel' });
    expect(document.activeElement).toBe(cancel);
    fireEvent.keyDown(cancel, { key: 'Escape' });
    expect(screen.queryByRole('button', { name: 'Yes, sign out' })).toBeNull();
    fireEvent.click(screen.getByRole('button', { name: /^Settings/ }));
    expect(screen.getByRole('button', { name: 'Sign out' })).toBeDefined();
  });
});

describe('Members', () => {
  it('on a wide window is a column that Members folds away, focus staying on the button', () => {
    windowOf(true);
    lobby();
    const button = screen.getByRole('button', { name: 'Members' });
    expect(button.getAttribute('aria-expanded')).toBe('true');
    expect(screen.getByRole('region', { name: 'Members list' })).toBeDefined();
    button.focus();
    fireEvent.click(button);
    expect(screen.queryByRole('region', { name: 'Members list' })).toBeNull();
    expect(document.activeElement).toBe(button);
    expect(screen.queryByRole('button', { name: 'Close' })).toBeNull();
  });

  it('on a narrow window starts closed, opens over the page with focus in it, and Escape gives focus back', () => {
    windowOf(false);
    lobby();
    const button = screen.getByRole('button', { name: 'Members' });
    expect(button.getAttribute('aria-expanded')).toBe('false');
    expect(screen.queryByRole('region', { name: 'Members list' })).toBeNull();
    fireEvent.click(button);
    const list = screen.getByRole('region', { name: 'Members list' });
    expect(document.activeElement).toBe(list);
    fireEvent.keyDown(list, { key: 'Escape' });
    expect(screen.queryByRole('region', { name: 'Members list' })).toBeNull();
    expect(document.activeElement).toBe(button);
    // Its close button does the same.
    fireEvent.click(button);
    fireEvent.click(within(screen.getByRole('region', { name: 'Members list' })).getByRole('button', { name: 'Close' }));
    expect(screen.queryByRole('region', { name: 'Members list' })).toBeNull();
    expect(document.activeElement).toBe(button);
  });

  it('closes when the window narrows, rather than cover the page', () => {
    const size = windowOf(true);
    lobby();
    expect(screen.getByRole('region', { name: 'Members list' })).toBeDefined();
    size.resize(false);
    expect(screen.queryByRole('region', { name: 'Members list' })).toBeNull();
    expect(screen.getByRole('button', { name: 'Members' }).getAttribute('aria-expanded')).toBe('false');
  });

  it('gives focus to Members when the window narrows with focus in the column', () => {
    const size = windowOf(true);
    lobby();
    screen.getByRole('region', { name: 'Members list' }).focus();
    size.resize(false);
    expect(document.activeElement).toBe(screen.getByRole('button', { name: 'Members' }));
  });
});

describe('what is playing', () => {
  it('is never mounted again when panels open and close or the window changes width', () => {
    const size = windowOf(true);
    lobby();
    expect(events).toEqual(['mount conversation', 'mount voice']);
    fireEvent.click(screen.getByRole('button', { name: 'Members' }));
    fireEvent.click(screen.getByRole('button', { name: 'Members' }));
    fireEvent.click(screen.getByRole('button', { name: /^Settings/ }));
    fireEvent.keyDown(screen.getByRole('dialog', { name: 'Settings' }), { key: 'Escape' });
    size.resize(false);
    fireEvent.click(screen.getByRole('button', { name: 'Members' }));
    fireEvent.keyDown(screen.getByRole('region', { name: 'Members list' }), { key: 'Escape' });
    size.resize(true);
    expect(events).toEqual(['mount conversation', 'mount voice']);
  });
});

// Issue #2189: the voice options sheet is modal, and the lobby owns every
// inert: the page behind it is inert with Settings' and re-authentication's
// own attribute, one expression each, and re-authentication outranks it.
describe('the voice options sheet and the lobby', () => {
  // Reports the sheet open (or not) as VoicePanel does, and shows what the lobby says back.
  function Sheet({ open }: { readonly open: boolean }) {
    const { report, blocked } = useContext(VoiceSheet);
    useEffect(() => {
      report(open);
      return () => { report(false); };
    }, [open, report]);
    return <p>{blocked ? 'blocked' : 'free'}</p>;
  }
  const reauth = <div className="reauth-layer"><input aria-label="Password" /></div>;
  const inert = (selector: string) => document.querySelector(selector)?.hasAttribute('inert');

  it('makes the page behind an open sheet inert, and not the dock or the overlay', () => {
    lobby(<Sheet open />);
    expect(inert('.areas')).toBe(true);
    expect(inert('.topbar')).toBe(true);
    expect(inert('.dock')).toBe(false);
  });

  it('leaves the page alone while the sheet is closed', () => {
    lobby(<Sheet open={false} />);
    expect(inert('.areas')).toBe(false);
  });

  it('keeps the page inert through re-authentication over an open sheet, and the prompt usable', () => {
    const { rerender } = lobby(<Sheet open />);
    rerender(lobbyTree(<Sheet open />, null, reauth));
    expect(screen.getByText('blocked')).toBeTruthy();
    expect(inert('.areas')).toBe(true);
    expect(inert('.reauth-layer')).toBe(false);
    expect(document.querySelector('.reauth-layer')?.closest('[inert]')).toBeNull();
    // The sheet closes in answer (it is blocked): re-authentication still holds the page.
    rerender(lobbyTree(<Sheet open={false} />, null, reauth));
    expect(inert('.areas')).toBe(true);
    expect(inert('.dock')).toBe(true);
    // Re-authentication done, the sheet closed: the page is free.
    rerender(lobbyTree(<Sheet open={false} />, null, undefined));
    expect(inert('.areas')).toBe(false);
  });

  it('keeps the page inert when re-authentication is dismissed while the sheet stays open', () => {
    const { rerender } = lobby(<Sheet open />);
    rerender(lobbyTree(<Sheet open />, null, reauth));
    rerender(lobbyTree(<Sheet open />, null, undefined));
    expect(inert('.areas')).toBe(true);
  });

  // What the page draws outside the lobby (the desktop strip, the update
  // notices) reads the lobby's one answer, pageModal.
  function Outside() {
    return <button type="button" inert={usePageModal()}>Servers</button>;
  }
  const outside = () => screen.getByRole('button', { name: 'Servers', hidden: true });
  const withOutside = (voice: ReactNode, overlay?: ReactNode) => (
    <>
      <Outside />
      {lobbyTree(voice, null, overlay)}
    </>
  );

  it('makes what is outside the lobby inert for the sheet, Settings and re-authentication', () => {
    windowOf(true);
    const { rerender } = render(withOutside(<Sheet open={false} />));
    expect(outside().hasAttribute('inert')).toBe(false);
    rerender(withOutside(<Sheet open />));
    expect(outside().hasAttribute('inert')).toBe(true);
    rerender(withOutside(<Sheet open={false} />));
    expect(outside().hasAttribute('inert')).toBe(false);
    fireEvent.click(screen.getByRole('button', { name: /^Settings/ }));
    expect(outside().hasAttribute('inert')).toBe(true);
    fireEvent.click(within(screen.getByRole('dialog', { name: 'Settings' })).getByRole('button', { name: 'Close' }));
    expect(outside().hasAttribute('inert')).toBe(false);
    rerender(withOutside(<Sheet open={false} />, reauth));
    expect(outside().hasAttribute('inert')).toBe(true);
    rerender(withOutside(<Sheet open={false} />));
    expect(outside().hasAttribute('inert')).toBe(false);
  });

  it('clears it when the lobby goes', () => {
    const { rerender } = render(withOutside(<Sheet open />));
    expect(outside().hasAttribute('inert')).toBe(true);
    rerender(<Outside />);
    expect(outside().hasAttribute('inert')).toBe(false);
  });

  // Re-authentication over Settings (issue #2189): whichever of the two goes
  // first, focus ends up on something usable, never the body or an inert button.
  it('re-authentication clearing over open Settings puts focus back in Settings', () => {
    windowOf(true);
    const { rerender } = render(lobbyTree(<Sheet open={false} />, null));
    fireEvent.click(screen.getByRole('button', { name: /^Settings/ }));
    rerender(lobbyTree(<Sheet open={false} />, null, reauth));
    (document.activeElement as HTMLElement | null)?.blur();
    expect(document.activeElement).toBe(document.body);
    rerender(lobbyTree(<Sheet open={false} />, null));
    expect(screen.getByRole('dialog', { name: 'Settings' })).toBeTruthy();
    expect(document.activeElement).toBe(screen.getByRole('heading', { name: 'Settings' }));
  });

  it('Settings closing while re-authentication is up waits, and focus goes to the Settings button when it clears', () => {
    windowOf(true);
    const { rerender } = render(lobbyTree(<Sheet open={false} />, null));
    const settings = screen.getByRole('button', { name: /^Settings/ });
    fireEvent.click(settings);
    rerender(lobbyTree(<Sheet open={false} />, null, reauth));
    fireEvent.keyDown(screen.getByRole('button', { name: 'Sign out' }), { key: 'Escape' });
    expect(screen.queryByRole('dialog', { name: 'Settings' })).toBeNull();
    // The button is still inert behind re-authentication: nothing was focused on it.
    expect(document.activeElement).not.toBe(settings);
    rerender(lobbyTree(<Sheet open={false} />, null));
    expect(document.activeElement).toBe(settings);
  });

  it('re-authentication clearing leaves usable focus the person already has', () => {
    windowOf(true);
    const { rerender } = render(<><input aria-label="Elsewhere" />{lobbyTree(<Sheet open={false} />, null)}</>);
    const settings = screen.getByRole('button', { name: /^Settings/ });
    fireEvent.click(settings);
    const wrapped = (overlay?: ReactNode) => <><input aria-label="Elsewhere" />{lobbyTree(<Sheet open={false} />, null, overlay)}</>;
    rerender(wrapped(reauth));
    fireEvent.keyDown(screen.getByRole('button', { name: 'Sign out' }), { key: 'Escape' });
    // Focus the person has on a usable control outside the lobby.
    screen.getByLabelText('Elsewhere').focus();
    rerender(wrapped());
    expect(document.activeElement).toBe(screen.getByLabelText('Elsewhere'));
    expect(document.activeElement).not.toBe(settings);
  });

  // The sheet, as VoicePanel does, takes focus into its Close when nothing is over it.
  function Focusing() {
    const { report, behind } = useContext(VoiceSheet);
    const close = useRef<HTMLButtonElement>(null);
    useEffect(() => {
      report(true);
      return () => { report(false); };
    }, [report]);
    useEffect(() => {
      if (!behind) close.current?.focus();
    }, [behind]);
    return <button ref={close} type="button">Close sheet</button>;
  }

  it('gives focus to the sheet, not the inert Settings button, when Settings closes over it', () => {
    windowOf(true);
    lobby(<Focusing />);
    const settings = screen.getByRole('button', { name: /^Settings/ });
    fireEvent.click(settings);
    fireEvent.keyDown(screen.getByRole('button', { name: 'Sign out' }), { key: 'Escape' });
    expect(screen.queryByRole('dialog', { name: 'Settings' })).toBeNull();
    expect(document.activeElement).toBe(screen.getByRole('button', { name: 'Close sheet' }));
    expect(document.activeElement).not.toBe(settings);
  });
});
