// The lobby's panels and layout (issue #536): Settings and, on a narrow
// window, Members open over the page, take focus, and give it back when
// Escape, their close button or a click outside closes them. Nothing in the
// voice strip or the conversation is mounted again when a panel opens or the
// window changes width: a call or a share there carries on.
import { act, cleanup, fireEvent, render, screen, within } from '@testing-library/react';
import { useEffect, type ReactNode } from 'react';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { windowOf } from './fakewindow';
import { Lobby } from './Lobby';
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

function lobby(voice: ReactNode = <Probe name="voice" />) {
  return render(
    <Lobby
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
    />,
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
    // Modal: the rest of the page is inert while it is open, and only then.
    expect(panel.getAttribute('aria-modal')).toBe('true');
    expect(button.closest('[inert]')).not.toBeNull();
    fireEvent.keyDown(screen.getByRole('button', { name: 'Sign out' }), { key: 'Escape' });
    expect(screen.queryByRole('dialog', { name: 'Settings' })).toBeNull();
    expect(document.activeElement).toBe(button);
    expect(button.closest('[inert]')).toBeNull();
    await act(async () => {});
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
