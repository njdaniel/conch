// The lobby's panels and layout (issue #536): Settings and, on a narrow
// window, Members open over the page, take focus, and give it back when
// Escape, their close button or a click outside closes them. Nothing in the
// voice strip or the conversation is mounted again when a panel opens or the
// window changes width: a call or a share there carries on.
import { act, cleanup, fireEvent, render, screen, waitFor, within } from '@testing-library/react';
import { useContext, useEffect, useRef, useState, type ReactNode } from 'react';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { windowOf } from './fakewindow';
import { Lobby } from './Lobby';
import { DesktopOS } from './platform/os';
import { VoiceSheet } from './phone';
import { LeaveDrawer, usePageModal } from './pageModal';
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

function lobbyTree(voice: ReactNode, os: string | null, overlay?: ReactNode, loaded = false) {
  return (
    <DesktopOS.Provider value={os}><Lobby
      overlay={overlay}
      origin="http://127.0.0.1:8080"
      name="ada"
      me={2}
      channels={loaded ? { ...initialChannels, loaded: true } : initialChannels}
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

// Issue #2196: below the wide breakpoint the members panel is a modal drawer
// with a backdrop, and the lobby, the one owner of inert, makes the rest of the
// page inert while it is over it.
describe('Members as a modal drawer', () => {
  const inert = (selector: string) => document.querySelector(selector)?.hasAttribute('inert');
  const list = () => screen.getByRole('region', { name: 'Members list' });
  function Outside() {
    return <button type="button" inert={usePageModal()}>Servers</button>;
  }
  function Behind() {
    return <p>{useContext(VoiceSheet).behind ? 'behind' : 'clear'}</p>;
  }

  it('makes the channels, conversation, topbar, dock and nav inert, and not the panel', () => {
    windowOf(false);
    lobby();
    for (const s of ['.channels', '.centre', '.topbar', '.dock', '.areas']) expect(inert(s), s).toBe(false);
    fireEvent.click(screen.getByRole('button', { name: 'Members' }));
    expect(inert('.channels')).toBe(true);
    expect(inert('.centre')).toBe(true);
    expect(inert('.topbar')).toBe(true);
    expect(inert('.dock')).toBe(true);
    // The panel, its close button and the backdrop are live.
    expect(inert('.areas')).toBe(false);
    expect(list().closest('[inert]')).toBeNull();
    expect(document.querySelector('.backdrop')?.closest('[inert]')).toBeNull();
    expect(screen.getByRole('button', { name: 'Close' }).closest('[inert]')).toBeNull();
  });

  it('clears on close, and on widening', () => {
    const size = windowOf(false);
    lobby();
    fireEvent.click(screen.getByRole('button', { name: 'Members' }));
    expect(inert('.centre')).toBe(true);
    fireEvent.keyDown(list(), { key: 'Escape' });
    for (const s of ['.channels', '.centre', '.topbar', '.dock']) expect(inert(s), s).toBe(false);
    fireEvent.click(screen.getByRole('button', { name: 'Members' }));
    expect(inert('.centre')).toBe(true);
    size.resize(true);
    for (const s of ['.channels', '.centre', '.topbar', '.dock', '.areas']) expect(inert(s), s).toBe(false);
  });

  it('is nothing at a wide width, where Members is a side column', () => {
    windowOf(true);
    lobby();
    for (const s of ['.channels', '.centre', '.topbar', '.dock', '.areas']) expect(inert(s), s).toBe(false);
  });

  it('returns focus to the Members button on Escape, the button being inert until then', () => {
    windowOf(false);
    lobby();
    const button = screen.getByRole('button', { name: 'Members' });
    fireEvent.click(button);
    expect(document.activeElement).toBe(list());
    fireEvent.keyDown(list(), { key: 'Escape' });
    expect(document.activeElement).toBe(button);
    expect(button.closest('[inert]')).toBeNull();
  });

  it('wraps Tab and Shift+Tab inside the panel, so focus never reaches the browser chrome', () => {
    windowOf(false);
    lobby();
    fireEvent.click(screen.getByRole('button', { name: 'Members' }));
    const close = screen.getByRole('button', { name: 'Close' });
    // The list takes focus first, then Close is the last stop.
    fireEvent.keyDown(list(), { key: 'Tab', shiftKey: true });
    expect(document.activeElement).toBe(close);
    fireEvent.keyDown(close, { key: 'Tab' });
    expect(document.activeElement).toBe(list());
    // In the middle of the panel Tab is the browser's own.
    expect(fireEvent.keyDown(list(), { key: 'Tab' })).toBe(true);
  });

  it('returns focus to the Members button on a click on the backdrop', () => {
    windowOf(false);
    lobby();
    const button = screen.getByRole('button', { name: 'Members' });
    fireEvent.click(button);
    fireEvent.click(document.querySelector('.backdrop') as Element);
    expect(screen.queryByRole('region', { name: 'Members list' })).toBeNull();
    expect(document.activeElement).toBe(button);
  });

  it('says to the page outside the lobby that a modal is up, and to the sheet that it is behind', () => {
    windowOf(false);
    render(
      <>
        <Outside />
        {lobbyTree(<Behind />, null)}
      </>,
    );
    const outside = () => screen.getByRole('button', { name: 'Servers', hidden: true });
    expect(outside().hasAttribute('inert')).toBe(false);
    expect(screen.getByText('clear')).toBeTruthy();
    fireEvent.click(screen.getByRole('button', { name: 'Members' }));
    expect(outside().hasAttribute('inert')).toBe(true);
    expect(screen.getByText('behind')).toBeTruthy();
    fireEvent.keyDown(list(), { key: 'Escape' });
    expect(outside().hasAttribute('inert')).toBe(false);
    expect(screen.getByText('clear')).toBeTruthy();
  });
});

// Issue #2196, round 2: what else reaches the page behind the drawer, and what
// strands focus in it.
describe('Members as a modal drawer: shortcuts, stranded focus, other channels, the centre', () => {
  const at = '2026-10-25T00:00:00.000Z';
  const two = [{ id: 1, name: 'a', created_at: at }, { id: 2, name: 'b', created_at: at }];
  const inert = (selector: string) => document.querySelector(selector)?.hasAttribute('inert');
  const list = () => screen.getByRole('region', { name: 'Members list' });
  const membersButton = () => screen.getByRole('button', { name: 'Members' });
  function tree(props: { selected?: string | null; panel?: ReactNode; conversation?: ReactNode; onOpen?: (name: string) => void; overlay?: ReactNode }) {
    return (
      <Lobby
        overlay={props.overlay}
        origin="http://127.0.0.1:8080"
        name="ada"
        me={2}
        channels={{ ...initialChannels, loaded: true, channels: two, selected: props.selected === undefined ? 'a' : props.selected }}
        unread={new Map()}
        connection="connected"
        conversation={props.conversation ?? <p>conversation</p>}
        panel={props.panel ?? <p>Nobody</p>}
        voice={null}
        invites={null}
        onOpen={props.onOpen ?? (() => undefined)}
        onRetry={() => undefined}
        onSignOut={() => undefined}
        onSignOutEverywhere={() => undefined}
        signOutProblem={null}
      />
    );
  }

  it('leaves Ctrl+K and Alt+Down alone while the drawer is over the page, and works as before without it', () => {
    windowOf(false);
    const onOpen = vi.fn();
    render(tree({ onOpen }));
    fireEvent.click(membersButton());
    fireEvent.keyDown(list(), { key: 'k', ctrlKey: true });
    expect(screen.queryByRole('dialog', { name: 'Find a channel' })).toBeNull();
    fireEvent.keyDown(list(), { key: 'ArrowDown', altKey: true });
    expect(onOpen).not.toHaveBeenCalled();
    fireEvent.keyDown(list(), { key: 'Escape' });
    fireEvent.keyDown(document.body, { key: 'ArrowDown', altKey: true });
    expect(onOpen).toHaveBeenCalledWith('b');
    fireEvent.keyDown(document.body, { key: 'k', ctrlKey: true });
    expect(screen.getByRole('dialog', { name: 'Find a channel' })).toBeTruthy();
  });

  it('puts focus back in the drawer when re-authentication that stranded it goes away', () => {
    windowOf(false);
    const reauth = <div className="reauth-layer"><input aria-label="Password" /></div>;
    const { rerender } = render(tree({}));
    fireEvent.click(membersButton());
    rerender(tree({ overlay: reauth }));
    (document.activeElement as HTMLElement | null)?.blur();
    expect(document.activeElement).toBe(document.body);
    rerender(tree({}));
    expect(document.activeElement).toBe(list());
    fireEvent.keyDown(document.activeElement as Element, { key: 'Escape' });
    expect(screen.queryByRole('region', { name: 'Members list' })).toBeNull();
    expect(document.activeElement).toBe(membersButton());
  });

  it('leaves focus on re-authentication alone while it is up', () => {
    windowOf(false);
    const reauth = <div className="reauth-layer"><input aria-label="Password" /></div>;
    const { rerender } = render(tree({}));
    fireEvent.click(membersButton());
    rerender(tree({ overlay: reauth }));
    const field = screen.getByLabelText('Password');
    field.focus();
    rerender(tree({ overlay: reauth, panel: <p>Somebody</p> }));
    expect(document.activeElement).toBe(field);
  });

  it('keeps focus in the drawer when the focused row remounts, and Escape still works', async () => {
    windowOf(false);
    function Rows() {
      const [group, setGroup] = useState('Online');
      return (
        <>
          <button type="button" onClick={() => { setGroup('Offline'); }}>Move</button>
          <ul key={group} aria-label={group}><li><button type="button">ada</button></li></ul>
        </>
      );
    }
    render(tree({ panel: <Rows /> }));
    fireEvent.click(membersButton());
    screen.getByRole('button', { name: 'ada' }).focus();
    fireEvent.click(screen.getByRole('button', { name: 'Move' }));
    expect(screen.getByRole('list', { name: 'Offline' })).toBeTruthy();
    await waitFor(() => { expect(document.activeElement).toBe(list()); });
    fireEvent.keyDown(document.activeElement as Element, { key: 'Escape' });
    expect(screen.queryByRole('region', { name: 'Members list' })).toBeNull();
    expect(document.activeElement).toBe(membersButton());
  });

  it.each([['another channel', 'b'], ['none', null]] as const)('closes when the shown channel changes to %s, focus on Members', (_name, next) => {
    windowOf(false);
    const { rerender } = render(tree({}));
    fireEvent.click(membersButton());
    expect(document.activeElement).toBe(list());
    rerender(tree({ selected: next }));
    expect(screen.queryByRole('region', { name: 'Members list' })).toBeNull();
    for (const s of ['.channels', '.centre', '.topbar', '.dock']) expect(inert(s), s).toBe(false);
    expect(document.activeElement).toBe(membersButton());
  });

  it('stays open when the shown channel settles through none, then A, then B before it opens, and after on none to A', () => {
    windowOf(false);
    const { rerender } = render(tree({ selected: null }));
    rerender(tree({ selected: 'a' }));
    rerender(tree({ selected: 'b' }));
    fireEvent.click(membersButton());
    expect(screen.getByRole('region', { name: 'Members list' })).toBeTruthy();
    // Shown again, same channel: nothing.
    rerender(tree({ selected: 'b', panel: <p>Somebody</p> }));
    expect(screen.getByRole('region', { name: 'Members list' })).toBeTruthy();
    // Opened before the first channel was shown: that one is the channel it opened on.
    cleanup();
    const early = render(tree({ selected: null }));
    fireEvent.click(membersButton());
    early.rerender(tree({ selected: 'a' }));
    expect(screen.getByRole('region', { name: 'Members list' })).toBeTruthy();
    early.rerender(tree({ selected: 'b' }));
    expect(screen.queryByRole('region', { name: 'Members list' })).toBeNull();
  });

  it('leaves the wide side column open across channels', () => {
    windowOf(true);
    const { rerender } = render(tree({}));
    rerender(tree({ selected: 'b' }));
    expect(screen.getByRole('region', { name: 'Members list' })).toBeTruthy();
  });

  it('closes before an action in it shows something in the centre, and focuses what it showed', () => {
    windowOf(false);
    // The page draws the detail in the conversation, in the same update.
    function Open({ show }: { readonly show: () => void }) {
      const leave = useContext(LeaveDrawer);
      return <button type="button" onClick={() => { leave(); show(); }}>Open run #4</button>;
    }
    function Page() {
      const [run, setRun] = useState(false);
      return tree({
        panel: <Open show={() => { setRun(true); }} />,
        conversation: run ? <article aria-label="Run detail" tabIndex={-1} data-centre-target>Run</article> : <p>conversation</p>,
      });
    }
    render(<Page />);
    fireEvent.click(membersButton());
    fireEvent.click(screen.getByRole('button', { name: 'Open run #4' }));
    expect(screen.queryByRole('region', { name: 'Members list' })).toBeNull();
    for (const s of ['.channels', '.centre', '.topbar', '.dock']) expect(inert(s), s).toBe(false);
    expect(document.activeElement).toBe(screen.getByRole('article', { name: 'Run detail' }));
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

  // The quick switcher is drawn into the body: the sheet is told it is behind.
  it('tells the sheet it is behind while the quick switcher is open', () => {
    windowOf(true);
    function Behind() {
      return <p>{useContext(VoiceSheet).behind ? 'behind' : 'clear'}</p>;
    }
    render(lobbyTree(<Behind />, null, undefined, true));
    expect(screen.getByText('clear')).toBeTruthy();
    fireEvent.click(screen.getByRole('button', { name: /Find a channel/ }));
    expect(screen.getByRole('dialog', { name: 'Find a channel' })).toBeTruthy();
    expect(screen.getByText('behind')).toBeTruthy();
    fireEvent.keyDown(screen.getByRole('combobox', { name: 'Find a channel' }), { key: 'Escape' });
    expect(screen.queryByRole('dialog', { name: 'Find a channel' })).toBeNull();
    expect(screen.getByText('clear')).toBeTruthy();
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
