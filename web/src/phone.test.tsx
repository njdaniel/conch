// The phone layout (issue #929): below 700 px the lobby has a bottom navigation
// of Chat, Channels and Voice, and draws one view at a time. Every view stays
// mounted, so a switch can neither join nor leave voice, lose a draft, nor
// mount the conversation or the voice section a second time.
import { act, cleanup, fireEvent, render, screen, within } from '@testing-library/react';
import { useContext, useEffect, useState, type ReactNode } from 'react';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { headingId } from './Conversation';
import { Lobby } from './Lobby';
import { VoiceLive } from './phone';
import type { ChannelV0 } from './schema/channel';
import type { ChannelsState } from './state/channels';

const events: string[] = [];

function Probe({ name }: { readonly name: string }) {
  useEffect(() => {
    events.push(`mount ${name}`);
    return () => {
      events.push(`unmount ${name}`);
    };
  }, [name]);
  return <p>{name}</p>;
}

// The conversation: a heading focus can go to, and a draft box that keeps what
// is typed in its own state, as the composer does for the page.
function Conversation() {
  const [draft, setDraft] = useState('');
  return (
    <>
      <Probe name="conversation" />
      <h2 id={headingId} tabIndex={-1}>
        general
      </h2>
      <textarea aria-label="Message #general" value={draft} onChange={(e) => { setDraft(e.target.value); }} />
    </>
  );
}

// Voice stands for the voice section: it reports whether it is in voice, and
// has a button that would join, to see that no view change presses it.
function Voice({ live, joined }: { readonly live: boolean; readonly joined: () => void }) {
  const report = useContext(VoiceLive);
  useEffect(() => {
    report(live);
  }, [live, report]);
  return (
    <section aria-label="Voice">
      <Probe name="voice" />
      <button type="button" onClick={joined}>
        Join voice
      </button>
    </section>
  );
}

const channel = (id: number, name: string): ChannelV0 => ({ id, name, created_at: '2026-10-18T00:00:00.000Z' });
const loaded: ChannelsState = { channels: [channel(1, 'general'), channel(2, 'game-night')], loaded: true, selected: 'general' };

// A window that is a phone or not, and can change.
function windowOf(phone: boolean): { readonly turn: (toPhone: boolean) => void } {
  const queries = new Set<{ readonly text: string; matches: boolean; readonly listeners: Set<() => void> }>();
  const matchesNow = { phone };
  vi.stubGlobal(
    'matchMedia',
    vi.fn((text: string) => {
      const query = { text, matches: text.includes('max-width: 699px') ? matchesNow.phone : false, listeners: new Set<() => void>() };
      queries.add(query);
      return {
        get matches() {
          return query.matches;
        },
        addEventListener: (_type: string, listener: () => void) => query.listeners.add(listener),
        removeEventListener: (_type: string, listener: () => void) => query.listeners.delete(listener),
      };
    }),
  );
  return {
    turn(toPhone) {
      matchesNow.phone = toPhone;
      act(() => {
        for (const query of queries) {
          if (!query.text.includes('max-width: 699px')) continue;
          query.matches = toPhone;
          for (const listener of query.listeners) listener();
        }
      });
    },
  };
}

function lobby({
  channels = loaded,
  live = false,
  onOpen = () => undefined,
  joined = () => undefined,
}: { channels?: ChannelsState; live?: boolean; onOpen?: (name: string) => void; joined?: () => void } = {}) {
  const page = (voice: ReactNode) => (
    <Lobby
      origin="http://127.0.0.1:8080"
      name="ada"
      me={2}
      channels={channels}
      unread={new Map()}
      connection="connected"
      conversation={<Conversation />}
      panel={<p>Nobody</p>}
      voice={voice}
      invites={null}
      onOpen={onOpen}
      onRetry={() => undefined}
      onSignOut={() => undefined}
      onSignOutEverywhere={() => undefined}
      signOutProblem={null}
    />
  );
  const view = render(page(<Voice live={live} joined={joined} />));
  return { ...view, setLive: (next: boolean) => { view.rerender(page(<Voice live={next} joined={joined} />)); } };
}

const nav = () => screen.getByRole('navigation', { name: 'Main navigation' });
const tab = (name: string) => within(nav()).getByRole('button', { name });
const root = (container: HTMLElement) => container.querySelector('.lobby');

beforeEach(() => {
  events.length = 0;
  vi.stubGlobal(
    'fetch',
    vi.fn(() => Promise.reject(new TypeError('Failed to fetch'))),
  );
});
afterEach(() => {
  cleanup();
  vi.unstubAllGlobals();
});

describe('the bottom navigation', () => {
  it('is not there at 700 px and wider, where the lobby is as it was', () => {
    windowOf(false);
    const { container } = lobby();
    expect(screen.queryByRole('navigation', { name: 'Main navigation' })).toBeNull();
    expect(root(container)?.className).toBe('lobby');
  });

  it('is three labelled buttons below 700 px, the current view marked with aria-current', () => {
    windowOf(true);
    const { container } = lobby();
    expect(within(nav()).getAllByRole('button').map((b) => b.textContent)).toEqual(['Chat', 'Channels', 'Voice']);
    expect(tab('Chat').getAttribute('aria-current')).toBe('page');
    expect(tab('Channels').getAttribute('aria-current')).toBeNull();
    expect(root(container)?.classList.contains('view-chat')).toBe(true);

    fireEvent.click(tab('Voice'));
    expect(tab('Voice').getAttribute('aria-current')).toBe('page');
    expect(tab('Chat').getAttribute('aria-current')).toBeNull();
    expect(root(container)?.classList.contains('view-voice')).toBe(true);

    fireEvent.click(tab('Channels'));
    expect(tab('Channels').getAttribute('aria-current')).toBe('page');
    expect(root(container)?.classList.contains('view-channels')).toBe(true);
  });

  it('opens on Channels while the channels are loading, or none is open, and on Chat once one is', () => {
    windowOf(true);
    const loading = lobby({ channels: { channels: [], loaded: false, selected: null } });
    expect(tab('Channels').getAttribute('aria-current')).toBe('page');
    loading.unmount();
    lobby({ channels: { channels: [], loaded: true, selected: null } });
    expect(tab('Channels').getAttribute('aria-current')).toBe('page');
    cleanup();
    lobby();
    expect(tab('Chat').getAttribute('aria-current')).toBe('page');
  });
});

describe('switching views', () => {
  it('mounts the conversation and the voice section once and never again, through every view and a rotation', () => {
    const phone = windowOf(true);
    lobby();
    expect(events).toEqual(['mount conversation', 'mount voice']);
    for (const name of ['Channels', 'Voice', 'Chat', 'Voice', 'Channels', 'Chat']) fireEvent.click(tab(name));
    phone.turn(false);
    expect(screen.queryByRole('navigation', { name: 'Main navigation' })).toBeNull();
    phone.turn(true);
    for (const name of ['Voice', 'Chat']) fireEvent.click(tab(name));
    expect(events).toEqual(['mount conversation', 'mount voice']);
  });

  it('keeps the draft where it was, in the same box', () => {
    windowOf(true);
    lobby();
    const box = screen.getByLabelText('Message #general');
    fireEvent.change(box, { target: { value: 'half a thought' } });
    for (const name of ['Channels', 'Voice', 'Chat']) fireEvent.click(tab(name));
    expect(screen.getByLabelText('Message #general')).toBe(box);
    expect((box as HTMLTextAreaElement).value).toBe('half a thought');
  });

  it('presses nothing in the voice section and opens no channel', () => {
    windowOf(true);
    const joined = vi.fn();
    const onOpen = vi.fn();
    lobby({ joined, onOpen });
    for (const name of ['Channels', 'Voice', 'Chat', 'Voice']) fireEvent.click(tab(name));
    fireEvent.click(screen.getByRole('button', { name: 'Settings' }));
    fireEvent.keyDown(screen.getByRole('button', { name: 'Sign out' }), { key: 'Escape' });
    fireEvent.click(screen.getByRole('button', { name: 'Members' }));
    expect(joined).not.toHaveBeenCalled();
    expect(onOpen).not.toHaveBeenCalled();
  });
});

describe('choosing a channel in Channels', () => {
  it('opens that channel, returns to Chat and puts focus on the conversation heading', () => {
    windowOf(true);
    const onOpen = vi.fn();
    lobby({ onOpen });
    fireEvent.click(tab('Channels'));
    const list = screen.getByRole('navigation', { name: 'Channels' });
    fireEvent.click(within(list).getByRole('button', { name: 'game-night' }));
    expect(onOpen).toHaveBeenCalledExactlyOnceWith('game-night');
    expect(tab('Chat').getAttribute('aria-current')).toBe('page');
    expect(document.activeElement).toBe(document.getElementById(headingId));
  });

  it('does not change the view on a wide window', () => {
    windowOf(false);
    const onOpen = vi.fn();
    lobby({ onOpen });
    const button = within(screen.getByRole('navigation', { name: 'Channels' })).getByRole('button', { name: 'game-night' });
    button.focus();
    fireEvent.click(button);
    expect(onOpen).toHaveBeenCalledWith('game-night');
    // Focus stays where the tap left it: only a phone moves it.
    expect(document.activeElement).toBe(button);
  });
});

describe('being in voice', () => {
  it('marks the Voice button and the lobby while in voice, and says so in words', () => {
    windowOf(true);
    const { container, setLive } = lobby();
    expect(root(container)?.classList.contains('in-voice')).toBe(false);
    expect(tab('Voice').getAttribute('aria-describedby')).toBeNull();
    // The name is the word alone, in voice or not.
    setLive(true);
    expect(root(container)?.classList.contains('in-voice')).toBe(true);
    expect(tab('Voice').textContent).toBe('Voice');
    const described = tab('Voice').getAttribute('aria-describedby');
    expect(document.getElementById(described ?? '')?.textContent).toBe('You are in voice');
    expect(screen.getByRole('button', { name: 'Voice' })).toBe(tab('Voice'));
    setLive(false);
    expect(root(container)?.classList.contains('in-voice')).toBe(false);
  });
});
