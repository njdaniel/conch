// The voice panel's share and watch controls, and the watch view, over the
// real voice client and a stub room (issue #444): watch only asks for no
// microphone, a share is watched with its own volume and mute, a phone's
// "tap to play sound" step, and the sharer's side says what is happening. No
// SDK, no network, no device.
import { act, cleanup, fireEvent, render, screen, within } from '@testing-library/react';
import { useSyncExternalStore, type ReactNode } from 'react';
import { afterEach, describe, expect, it, vi } from 'vitest';
import type { ShareClaimResult, VoiceSessionResult } from '../api';
import { windowOf } from '../fakewindow';
import { Lobby } from '../Lobby';
import type { VoicePresenceV1 } from '../schema/voice';
import { initialChannels } from '../state/channels';
import type { Person } from '../useLiveChannel';
import { VoiceClient, type Room, type RoomEvents, type ShareSink } from './connection';
import { FakeClock, settle } from './fakes';
import type { Captured, KeptScreen, ShareFeatures } from './share';
import type { Snapshot, VoiceControl } from './useVoice';
import { VoicePanel } from './VoicePanel';

class StubRoom implements Room {
  events: RoomEvents | null = null;
  publish: boolean | null = null;
  calls: string[] = [];
  watching: boolean[] = [];
  sink: ShareSink | null = null;
  connect = (_url: string, _token: string, _room: string, publish: boolean, events: RoomEvents): Promise<void> => {
    this.publish = publish;
    this.events = events;
    return Promise.resolve();
  };
  setSending = (): Promise<void> => Promise.resolve();
  setDeafened = (): void => undefined;
  level = (): number => 0;
  close = (): Promise<void> => Promise.resolve();
  publishMicrophone = (): Promise<void> => {
    this.calls.push('microphone');
    return Promise.resolve();
  };
  setWatching = (on: boolean): void => {
    this.watching.push(on);
  };
  attachShare = (sink: ShareSink | null): void => {
    this.sink = sink;
  };
  pickScreen = (): Promise<Captured> => {
    this.calls.push('pick');
    return Promise.resolve({ audio: true });
  };
  publishScreen = (): Promise<void> => {
    this.calls.push('publish');
    return Promise.resolve();
  };
  stopScreen = (): Promise<void> => Promise.resolve();
  keepScreen = (): KeptScreen | null => ({ audio: true, live: () => true, onEnded: null, stop: () => undefined });
  adoptScreen = (): void => undefined;
  setMicrophone = (): void => undefined;
  switchMicrophone = (): Promise<void> => Promise.resolve();
  microphones = (): Promise<null> => Promise.resolve(null);
  microphoneInUse = (): null => null;
}

const session: VoiceSessionResult = {
  ok: true,
  value: {
    livekit_url: 'wss://lk.example',
    identity: 'p7',
    rooms: [{ room: 'r', token: 't', can_publish: true, expires_at: '2026-10-16T00:00:15.000Z' }],
  },
};
const granted: ShareClaimResult = { ok: true, value: { principal_id: 7, started_at: '2026-10-16T00:00:00.000Z', audio: true } };
const busy: ShareClaimResult = { ok: false, failure: 'invalid', status: 409, code: 'voice_share_busy' };

const presence = (sharing: number | null, audio = false): VoicePresenceV1 => ({
  schema: 'conch.voice_presence.v1',
  channel_id: 1,
  configured: true,
  available: true,
  rooms: [
    {
      participants: [7, 8].map((id) => ({
        principal_id: id,
        can_publish: true,
        transmitting: false,
        joined_at: '2026-10-16T00:00:00.000Z',
        ...(id === sharing ? { sharing: true, ...(audio ? { sharing_audio: true } : {}) } : {}),
      })),
    },
  ],
});

const people: ReadonlyMap<number, Person> = new Map([
  [7, { name: 'ada', kind: 'human' }],
  [8, { name: 'bea', kind: 'human' }],
]);

const chrome: ShareFeatures = { canShare: true, chromium: true, platform: 'linux' };

// wrap puts the panel where the page puts it, for a test of the layout.
function setup(claims: ShareClaimResult[] = [], features: ShareFeatures = chrome, wrap?: (panel: ReactNode) => ReactNode) {
  const rooms: StubRoom[] = [];
  const client = new VoiceClient({
    clock: new FakeClock(),
    random: () => 0.5,
    createSession: () => Promise.resolve(session),
    report: () => Promise.resolve('ok'),
    newRoom: () => {
      const r = new StubRoom();
      rooms.push(r);
      return r;
    },
    claimShare: () => Promise.resolve(claims.shift() ?? granted),
    stopShare: () => Promise.resolve(),
  });
  const joins: string[] = [];
  let snapshot: Snapshot = { kind: 'presence', presence: presence(null) };
  function Panel() {
    const state = useSyncExternalStore(client.subscribe, client.snapshot);
    const control: VoiceControl = {
      state,
      snapshot,
      shown: 'general',
      talkKey: 'Backquote',
      capturing: false,
      secure: true,
      features,
      client,
      setTalkKey: () => undefined,
      capture: () => undefined,
      join: () => {
        joins.push('join');
        client.join('general');
      },
      watchOnly: () => {
        joins.push('watch only');
        client.join('general', { watchOnly: true });
      },
      leave: () => {
        void client.leave();
      },
    };
    return <VoicePanel voice={control} viewer={7} people={people} viewed="general" onOpen={() => undefined} />;
  }
  const tree = () => (wrap === undefined ? <Panel /> : wrap(<Panel />));
  const view = render(tree());
  // A presence document from the socket: the panel shows it, the client reads it.
  const seen = (p: VoicePresenceV1) => {
    snapshot = { kind: 'presence', presence: p };
    act(() => {
      client.presence(p);
    });
    view.rerender(tree());
  };
  return { client, rooms, joins, seen, view };
}

afterEach(() => {
  cleanup();
  vi.restoreAllMocks();
  vi.unstubAllGlobals();
});

const voice = () => screen.getByRole('region', { name: 'Voice' });

describe('watch only', () => {
  it('joins without the microphone, and asks for it only when the person chooses to talk', async () => {
    const t = setup();
    fireEvent.click(within(voice()).getByRole('button', { name: 'Watch only in #general' }));
    await act(settle);
    expect(t.joins).toEqual(['watch only']);
    expect(t.rooms[0]?.publish).toBe(false);
    expect(await within(voice()).findByText(/Watch only: your microphone is off/)).toBeTruthy();
    expect(within(voice()).getByRole('button', { name: 'Hold to talk' }).hasAttribute('disabled')).toBe(true);
    expect(t.rooms[0]?.calls).toEqual([]);
    fireEvent.click(within(voice()).getByRole('button', { name: 'Use microphone' }));
    await act(settle);
    expect(t.rooms[0]?.calls).toEqual(['microphone']);
    expect(within(voice()).getByRole('button', { name: 'Hold to talk' }).hasAttribute('disabled')).toBe(false);
  });

  it('is offered beside Join voice, and says a share can be watched from it', async () => {
    const t = setup();
    t.seen(presence(8));
    expect(within(voice()).getByRole('button', { name: 'Join voice in #general' })).toBeTruthy();
    expect(await within(voice()).findByText('bea is sharing a screen. Join, or watch only, to see it.')).toBeTruthy();
  });
});

describe('watching', () => {
  async function watching(t = setup()) {
    t.client.join('general', { watchOnly: true });
    await act(settle);
    t.seen(presence(8, true));
    fireEvent.click(await within(voice()).findByRole('button', { name: 'Watch' }));
    return t;
  }

  it('subscribes on Watch and plays into an inline video and an audio element of its own', async () => {
    const t = await watching();
    const view = await screen.findByRole('region', { name: 'Shared screen' });
    expect(view.textContent).toContain('bea is sharing, with sound');
    expect(t.rooms[0]?.watching.at(-1)).toBe(true);
    const sink = t.rooms[0]?.sink;
    expect(sink?.video.playsInline).toBe(true);
    expect(sink?.video.muted).toBe(true);
    expect(sink?.audio.tagName).toBe('AUDIO');
    expect(within(view).getByText('Waiting for the picture')).toBeTruthy();
    act(() => {
      t.rooms[0]?.events?.onShareTracks({ video: true, audio: false });
    });
    expect(within(view).queryByText('Waiting for the picture')).toBeNull();
  });

  it("the share's sound has its own volume and mute, apart from voice; deafen silences it too", async () => {
    const t = await watching();
    const view = await screen.findByRole('region', { name: 'Shared screen' });
    const audio = t.rooms[0]?.sink?.audio;
    fireEvent.change(within(view).getByLabelText('Share volume'), { target: { value: '0.4' } });
    expect(audio?.volume).toBeCloseTo(0.4);
    fireEvent.click(within(view).getByRole('button', { name: 'Mute share' }));
    expect(audio?.muted).toBe(true);
    // Voice's own mute is untouched.
    expect(t.client.snapshot().muted).toBe(false);
    fireEvent.click(within(view).getByRole('button', { name: 'Unmute share' }));
    expect(audio?.muted).toBe(false);
    act(() => {
      t.client.setDeafened(true);
    });
    expect(audio?.muted).toBe(true);
  });

  it('offers a tap to play the sound when the browser will not start it by itself', async () => {
    const play = vi.spyOn(HTMLMediaElement.prototype, 'play').mockRejectedValueOnce(new Error('NotAllowedError')).mockResolvedValue(undefined);
    const t = await watching();
    act(() => {
      t.rooms[0]?.events?.onShareTracks({ video: true, audio: true });
    });
    const tap = await screen.findByRole('button', { name: 'Tap to play sound' });
    fireEvent.click(tap);
    await act(settle);
    expect(play).toHaveBeenCalledTimes(2);
    expect(screen.queryByRole('button', { name: 'Tap to play sound' })).toBeNull();
  });

  it('goes full screen on request', async () => {
    await watching();
    const view = await screen.findByRole('region', { name: 'Shared screen' });
    Object.defineProperty(document, 'fullscreenEnabled', { value: true, configurable: true });
    const requested = vi.fn(() => Promise.resolve());
    const frame = view.querySelector('.share-frame');
    if (!(frame instanceof HTMLElement)) throw new Error('no frame');
    frame.requestFullscreen = requested;
    fireEvent.click(within(view).getByRole('button', { name: 'Full screen' }));
    expect(requested).toHaveBeenCalledTimes(1);
  });

  it('closes when the share ends, and on Stop watching', async () => {
    const t = await watching();
    expect(await screen.findByRole('region', { name: 'Shared screen' })).toBeTruthy();
    fireEvent.click(screen.getByRole('button', { name: 'Stop watching' }));
    expect(screen.queryByRole('region', { name: 'Shared screen' })).toBeNull();
    expect(t.rooms[0]?.watching.at(-1)).toBe(false);
    fireEvent.click(within(voice()).getByRole('button', { name: 'Watch' }));
    expect(await screen.findByRole('region', { name: 'Shared screen' })).toBeTruthy();
    t.seen(presence(null));
    expect(screen.queryByRole('region', { name: 'Shared screen' })).toBeNull();
    expect(within(voice()).queryByRole('button', { name: 'Watch' })).toBeNull();
  });
});

describe('sharing', () => {
  it('says what sound will be shared before the picker opens, then whether sound came', async () => {
    const t = setup();
    t.client.join('general');
    await act(settle);
    expect(await within(voice()).findByText(/only a tab shares its sound/)).toBeTruthy();
    // And that a whole screen or this window would show this page and its chat.
    expect(within(voice()).getByText(/entire screen or this browser window shows this page too, chat included/)).toBeTruthy();
    fireEvent.click(within(voice()).getByRole('button', { name: 'Share screen' }));
    await act(settle);
    expect(t.rooms[0]?.calls).toEqual(['pick', 'publish']);
    expect(await within(voice()).findByText('You are sharing your screen with its sound.')).toBeTruthy();
    fireEvent.click(within(voice()).getByRole('button', { name: 'Stop sharing' }));
    await act(settle);
    expect(await within(voice()).findByText('You stopped sharing.')).toBeTruthy();
  });

  // Issue #554: there is no choice of what a share is for. Voice options asks
  // nothing about sharing, and Share screen opens the picker at once.
  it('asks nothing before a share: Voice options has no share choice, and Share screen opens the picker', async () => {
    const t = setup();
    t.client.join('general');
    await act(settle);
    fireEvent.click(await within(voice()).findByRole('button', { name: 'Voice options' }));
    expect(within(voice()).getByRole('group', { name: 'Microphone' })).toBeTruthy();
    expect(within(voice()).queryByRole('group', { name: 'Share your screen' })).toBeNull();
    expect(within(voice()).queryAllByRole('radio').map((r) => r.closest('label')?.textContent)).toEqual(['Push to talk', 'Open microphone']);
    fireEvent.click(within(voice()).getByRole('button', { name: 'Share screen' }));
    await act(settle);
    expect(t.rooms[0]?.calls).toEqual(['pick', 'publish']);
  });

  it('a second person trying to share is told who is sharing', async () => {
    const t = setup([busy]);
    t.client.join('general');
    await act(settle);
    t.seen(presence(8));
    // No picker can open while bea shares, so the notes about it wait (issue #536).
    expect(within(voice()).queryByText(/only a tab shares its sound/)).toBeNull();
    fireEvent.click(await within(voice()).findByRole('button', { name: 'Share screen' }));
    await act(settle);
    expect(await within(voice()).findByText('bea is sharing a screen. One person shares at a time.')).toBeTruthy();
    expect(t.rooms[0]?.calls).toEqual([]);
  });

  // Issue #487: the sharer's own presence says why the server ended the
  // share, and the panel says it in those words.
  it('the sharer is told why the server ended the share', async () => {
    for (const [reason, text] of [
      ['operator', 'An operator stopped your share.'],
      ['left', 'You left voice, so your share ended.'],
      ['access_lost', 'Your share ended: you no longer have voice in this channel.'],
      // Not an end while the capture lasts: the share is resumed (issue #516).
      ['rotated', 'The voice room was renewed. Resuming your share…'],
      ['idle', 'Your share ended because nothing was shared for a while.'],
    ] as const) {
      const t = setup();
      t.client.join('general');
      await act(settle);
      fireEvent.click(await within(voice()).findByRole('button', { name: 'Share screen' }));
      await act(settle);
      expect(await within(voice()).findByText('You are sharing your screen with its sound.')).toBeTruthy();
      t.seen(presence(7));
      t.seen({ ...presence(null), share_ended: { reason, started_at: '2026-10-16T00:00:00.000Z', ended_at: '2026-10-16T00:01:00.000Z' } });
      await act(settle);
      expect(await within(voice()).findByText(text), reason).toBeTruthy();
      cleanup();
    }
  });

  // Issue #516: the room renewed under the share.
  it('the sharer is told the share is being resumed, then that it carried on, or why it could not', async () => {
    const renewed = { ...presence(null), rooms: [{ participants: [] }], share_ended: { reason: 'rotated' as const, started_at: '2026-10-16T00:00:00.000Z', ended_at: '2026-10-16T00:01:00.000Z' } };
    for (const [answer, text] of [
      [granted, 'The voice room was renewed, and your share carried on in the new one.'],
      [busy, 'bea is sharing a screen. One person shares at a time.'],
    ] as const) {
      const t = setup([granted, granted, answer]);
      t.client.join('general');
      await act(settle);
      fireEvent.click(await within(voice()).findByRole('button', { name: 'Share screen' }));
      await act(settle);
      expect(await within(voice()).findByText('You are sharing your screen with its sound.')).toBeTruthy();
      t.seen(presence(7));
      t.seen(renewed);
      expect(await within(voice()).findByText('The voice room was renewed. Resuming your share…')).toBeTruthy();
      if (answer === busy) t.seen({ ...presence(8), share_ended: renewed.share_ended });
      act(() => {
        t.rooms[0]?.events?.onDisconnected('room_deleted');
      });
      await act(settle);
      expect(await within(voice()).findByText(text), text).toBeTruthy();
      cleanup();
    }
  });

  it('the reason may come after the room withdrew the share, and replaces the guess', async () => {
    const t = setup();
    t.client.join('general');
    await act(settle);
    fireEvent.click(await within(voice()).findByRole('button', { name: 'Share screen' }));
    await act(settle);
    act(() => {
      t.rooms[0]?.events?.onScreenEnded('server');
    });
    await act(settle);
    expect(await within(voice()).findByText(/The server ended your share/)).toBeTruthy();
    t.seen({ ...presence(null), share_ended: { reason: 'operator', started_at: '2026-10-16T00:00:00.000Z', ended_at: '2026-10-16T00:01:00.000Z' } });
    expect(await within(voice()).findByText('An operator stopped your share.')).toBeTruthy();
  });

  it('a browser without a picker is not offered sharing, and is told it can still watch', async () => {
    const t = setup([], { canShare: false, chromium: false, platform: 'ios' });
    t.client.join('general');
    await act(settle);
    expect(await within(voice()).findByText(/cannot share a screen.*You can still watch/)).toBeTruthy();
    expect(within(voice()).queryByRole('button', { name: 'Share screen' })).toBeNull();
  });
});

// Issue #536: in the lobby the watch view is drawn on the stage above the
// conversation, and opening panels or narrowing the window never mounts it
// again, so the picture and the sound carry on.
describe('in the lobby', () => {
  it('draws the watch view on the stage, and keeps it attached while panels open and the window narrows', async () => {
    const size = windowOf(true);
    vi.stubGlobal(
      'fetch',
      vi.fn(() => Promise.reject(new TypeError('Failed to fetch'))),
    );
    const t = setup([], chrome, (panel) => (
      <Lobby
        origin="http://127.0.0.1:8080"
        name="ada"
        me={7}
        channels={initialChannels}
        unread={new Map()}
        connection="connected"
        conversation={<p>the conversation</p>}
        panel={<p>Nobody</p>}
        voice={panel}
        invites={null}
        onOpen={() => undefined}
        onRetry={() => undefined}
        onSignOut={() => undefined}
        onSignOutEverywhere={() => undefined}
        signOutProblem={null}
      />
    ));
    const attached = vi.spyOn(t.client, 'attachShare');
    t.client.join('general', { watchOnly: true });
    await act(settle);
    t.seen(presence(8, true));
    fireEvent.click(await within(voice()).findByRole('button', { name: 'Watch' }));
    const view = await screen.findByRole('region', { name: 'Shared screen' });
    expect(view.closest('.stage')).not.toBeNull();
    expect(voice().contains(view)).toBe(false);
    const video = view.querySelector('video');
    expect(video).not.toBeNull();

    fireEvent.click(screen.getByRole('button', { name: 'Members' }));
    fireEvent.click(screen.getByRole('button', { name: 'Settings' }));
    fireEvent.keyDown(screen.getByRole('dialog', { name: 'Settings' }), { key: 'Escape' });
    fireEvent.click(within(voice()).getByRole('button', { name: 'Voice options' }));
    size.resize(false);
    fireEvent.click(screen.getByRole('button', { name: 'Members' }));
    fireEvent.keyDown(screen.getByRole('region', { name: 'Members list' }), { key: 'Escape' });
    size.resize(true);

    expect(screen.getByRole('region', { name: 'Shared screen' }).querySelector('video')).toBe(video);
    expect(attached.mock.calls.map(([sink]) => sink?.video ?? null)).toEqual([video]);
    expect(t.rooms).toHaveLength(1);
    expect(t.rooms[0]?.watching.at(-1)).toBe(true);
    expect(t.client.snapshot().phase).toBe('connected');
  });
});
