// The phone layout (issue #929): below 700 px the lobby has a bottom navigation
// of Chat, Channels and Voice, and draws one view at a time. Every view stays
// mounted, so a switch can neither join nor leave voice, lose a draft, nor
// mount the conversation or the voice section a second time.
import { act, cleanup, fireEvent, render, screen, within } from '@testing-library/react';
import { useContext, useEffect, useState, type ReactNode } from 'react';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { headingId } from './Conversation';
import { Lobby } from './Lobby';
import { MicTestShown, useAppHeight, VoiceLive, VoiceStrip } from './phone';
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
  const micTest = useContext(MicTestShown);
  const strip = useContext(VoiceStrip);
  useEffect(() => {
    report(live);
  }, [live, report]);
  return (
    <section aria-label="Voice">
      <Probe name="voice" />
      {micTest ? <p>microphone test offered</p> : null}
      {/* The strip's state button, while the lobby says this is the strip. */}
      {strip === null ? null : (
        <button type="button" onClick={strip}>
          In voice in #general
        </button>
      )}
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
  const page = (voice: ReactNode, shown: ChannelsState = channels) => (
    <Lobby
      origin="http://127.0.0.1:8080"
      name="ada"
      me={2}
      channels={shown}
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
  return {
    ...view,
    setLive: (next: boolean) => {
      view.rerender(page(<Voice live={next} joined={joined} />));
    },
    setChannels: (next: ChannelsState) => {
      view.rerender(page(<Voice live={live} joined={joined} />, next));
    },
  };
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
    fireEvent.click(screen.getByRole('button', { name: /^Settings/ }));
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

describe('focus when the strip goes (leaving voice under Chat or Channels)', () => {
  it('goes to the Voice button when focus was in the strip that is no longer drawn', () => {
    windowOf(true);
    const { setLive } = lobby({ live: true });
    const join = screen.getByRole('button', { name: 'Join voice' });
    join.focus();
    expect(document.activeElement).toBe(join);
    setLive(false);
    expect(document.activeElement).toBe(tab('Voice'));
  });

  it('goes to the Voice button when it was lost to the page body', () => {
    windowOf(true);
    const { setLive } = lobby({ live: true });
    (document.activeElement as HTMLElement | null)?.blur();
    setLive(false);
    expect(document.activeElement).toBe(tab('Voice'));
  });

  it('leaves focus alone elsewhere: in the conversation, in the Voice view, or on a wide window', () => {
    windowOf(true);
    const first = lobby({ live: true });
    const box = screen.getByLabelText('Message #general');
    box.focus();
    first.setLive(false);
    expect(document.activeElement).toBe(box);
    cleanup();

    const second = lobby({ live: true });
    fireEvent.click(tab('Voice'));
    const join = screen.getByRole('button', { name: 'Join voice' });
    join.focus();
    second.setLive(false);
    expect(document.activeElement).toBe(join);
    cleanup();

    windowOf(false);
    const third = lobby({ live: true });
    const wideJoin = screen.getByRole('button', { name: 'Join voice' });
    wideJoin.focus();
    third.setLive(false);
    expect(document.activeElement).toBe(wideJoin);
  });
});

describe('the strip under Chat and Channels (issue #1028)', () => {
  const stateButton = () => screen.queryByRole('button', { name: 'In voice in #general' });

  it('is the strip under Chat and Channels and the whole section in the Voice view', () => {
    windowOf(true);
    lobby({ live: true });
    expect(stateButton()).not.toBeNull();
    fireEvent.click(tab('Channels'));
    expect(stateButton()).not.toBeNull();
    fireEvent.click(tab('Voice'));
    expect(stateButton()).toBeNull();
    fireEvent.click(tab('Chat'));
    expect(stateButton()).not.toBeNull();
  });

  it('is never the strip on a wide window', () => {
    windowOf(false);
    lobby({ live: true });
    expect(stateButton()).toBeNull();
  });

  it('opens the Voice view from its state button, with focus on the Voice button, and mounts nothing again', () => {
    windowOf(true);
    lobby({ live: true });
    events.length = 0;
    const button = stateButton();
    if (button === null) throw new Error('no state button');
    button.focus();
    fireEvent.click(button);
    expect(tab('Voice').getAttribute('aria-current')).toBe('page');
    expect(document.activeElement).toBe(tab('Voice'));
    expect(events).toEqual([]);
  });
});

describe('the microphone test', () => {
  it('is offered in the Voice view and, on a phone, in no other: leaving it ends a running test', () => {
    windowOf(true);
    lobby();
    expect(screen.queryByText('microphone test offered')).toBeNull();
    fireEvent.click(tab('Voice'));
    expect(screen.getByText('microphone test offered')).toBeTruthy();
    fireEvent.click(tab('Channels'));
    expect(screen.queryByText('microphone test offered')).toBeNull();
    fireEvent.click(tab('Chat'));
    expect(screen.queryByText('microphone test offered')).toBeNull();
  });

  it('is offered on a wide window, where the strip is always drawn', () => {
    windowOf(false);
    lobby();
    expect(screen.getByText('microphone test offered')).toBeTruthy();
  });
});

describe('Chat with nothing open', () => {
  it('falls back to Channels when the open channel goes, and Chat is back when one opens', () => {
    windowOf(true);
    const { setChannels } = lobby();
    expect(tab('Chat').getAttribute('aria-current')).toBe('page');
    fireEvent.click(tab('Chat'));
    setChannels({ ...loaded, selected: null });
    expect(tab('Channels').getAttribute('aria-current')).toBe('page');
    expect(tab('Chat').getAttribute('aria-current')).toBeNull();
    setChannels(loaded);
    expect(tab('Chat').getAttribute('aria-current')).toBe('page');
  });
});

describe('the height follows the visual viewport', () => {
  function Height({ on = true }: { readonly on?: boolean }) {
    useAppHeight(on);
    return null;
  }
  // A visual viewport of this height at this pinch-zoom scale.
  function viewport(height: number, scale: number) {
    const listeners = new Set<() => void>();
    const box = { height, scale };
    Object.defineProperty(window, 'visualViewport', {
      configurable: true,
      value: {
        get height() {
          return box.height;
        },
        get scale() {
          return box.scale;
        },
        addEventListener: (_type: string, listener: () => void) => listeners.add(listener),
        removeEventListener: (_type: string, listener: () => void) => listeners.delete(listener),
      },
    });
    return {
      change(next: { height?: number; scale?: number }) {
        box.height = next.height ?? box.height;
        box.scale = next.scale ?? box.scale;
        act(() => {
          for (const listener of listeners) listener();
        });
      },
    };
  }
  const height = () => document.documentElement.style.getPropertyValue('--app-height');
  afterEach(() => {
    Reflect.deleteProperty(window, 'visualViewport');
  });

  it('is the visual viewport as the layout sees it: shorter with the keyboard up', () => {
    const vv = viewport(844, 1);
    render(<Height />);
    expect(height()).toBe('844px');
    vv.change({ height: 480 });
    expect(height()).toBe('480px');
  });

  it('is not made half the screen by pinch zoom: at 2x the visual viewport is half the layout viewport, so its height is scaled back', () => {
    const vv = viewport(422, 2);
    render(<Height />);
    expect(height()).toBe('844px');
    vv.change({ height: 400, scale: 2.5 });
    expect(height()).toBe('1000px');
  });

  it('is not set off a phone layout, and is removed with it', () => {
    viewport(844, 1);
    const view = render(<Height on={false} />);
    expect(height()).toBe('');
    view.rerender(<Height />);
    expect(height()).toBe('844px');
    view.unmount();
    expect(height()).toBe('');
  });
});

// Issue #1030: the phone's top bar has no room for the server address, and the
// person's name is one tap away, in the Settings button's name.
describe('the top bar', () => {
  it('on a phone has no server address, and Settings names the person', () => {
    windowOf(true);
    lobby();
    const bar = screen.getByRole('status', { name: 'Status' });
    expect(bar.textContent).not.toContain('127.0.0.1');
    expect(bar.textContent).toContain('Connected');
    expect(screen.getByRole('button', { name: 'Settings, signed in as ada' })).toBeTruthy();
  });

  it('off a phone keeps the address and the plain Settings name', () => {
    windowOf(false);
    lobby();
    expect(screen.getByRole('status', { name: 'Status' }).textContent).toContain('127.0.0.1');
    expect(screen.getByRole('button', { name: 'Settings' }).getAttribute('aria-label')).toBeNull();
  });
});
