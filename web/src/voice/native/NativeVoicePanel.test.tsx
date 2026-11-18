// The desktop voice strip (issue #1376, D3-5 of #771), driven by a fake native
// voice and a fake presence socket: no helper, no browser engine, no network.
import { act, cleanup, fireEvent, render, screen, waitFor, within } from '@testing-library/react';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import * as api from '../../api';
import { setSoundAudience, sounds } from '../../sounds';
import type { NativeVoiceHelper, NativeVoiceView } from '../../platform/desktop/bridge';
import type { NativeVoice, NativeVoiceError, NativeVoiceOutcome } from '../../platform/types';
import type { ChannelV0 } from '../../schema/channel';
import type { VoicePresenceV2 } from '../../schema/voice';
import type { SocketEvents } from '../../state/feed';
import type { Person } from '../../useLiveChannel';
import * as sdk from '../sdk';
import { confirmWithin, duplicateWarning, NativeVoicePanel } from './NativeVoicePanel';
import { initialVoice, type VoiceState } from '../connection';
import type { Companion, CompanionClient } from './useCompanion';
import { shareDisabledWords, silentShareBefore, statusWords, words } from './words';

const idle: NativeVoiceView = { status: 'idle', generation: 0, channel: null, retryable: false };
const helper = (over: Partial<NativeVoiceHelper> = {}): NativeVoiceHelper => ({ version: 2, phase: 'connected', capture: 'on', transmitting: false, muted: false, deafened: false, blocked: [], ptt: 'ready', ...over });
const running = (over: Partial<NativeVoiceHelper> = {}, channel = 'general'): NativeVoiceView => ({ status: 'running', generation: 1, channel, retryable: false, helper: helper(over) });

function presence(...joins: readonly [number, string][]): VoicePresenceV2 {
  return {
    schema: 'conch.voice_presence.v2',
    channel_id: 1,
    configured: true,
    available: true,
    rooms: [{ participants: joins.map(([id, at]) => ({ principal_id: id, can_publish: true, transmitting: false, joined_at: at })) }],
  };
}

class Fake {
  view: NativeVoiceView;
  readonly calls: string[] = [];
  private readonly listeners = new Set<() => void>();
  deferred: ((out: { ok: true } | { ok: false; error: NativeVoiceError }) => void) | null = null;
  constructor(view: NativeVoiceView = idle) {
    this.view = view;
  }
  readonly nv: NativeVoice = {
    snapshot: () => this.view,
    subscribe: (l) => {
      this.listeners.add(l);
      return () => {
        this.listeners.delete(l);
      };
    },
    join: (r) => {
      this.calls.push(`join ${r.channel} muted=${String(r.muted)} deafened=${String(r.deafened)}`);
      return Promise.resolve<NativeVoiceOutcome>({ ok: true, view: this.view });
    },
    leave: () => {
      this.calls.push('leave');
      return Promise.resolve<NativeVoiceOutcome>({ ok: true, view: this.view });
    },
    retry: () => {
      this.calls.push('retry');
      return Promise.resolve<NativeVoiceOutcome>({ ok: true, view: this.view });
    },
    setMute: (m) => {
      this.calls.push(`mute ${String(m)}`);
      return new Promise((resolve) => {
        this.deferred = resolve;
      });
    },
    setDeafen: (d) => {
      this.calls.push(`deafen ${String(d)}`);
      return Promise.resolve({ ok: true });
    },
    setDevices: () => Promise.resolve({ ok: true }),
    setTalkMode: () => Promise.resolve({ ok: true }),
    devices: () => Promise.resolve({ ok: false, error: 'unsupported' }),
    saveDevice: () => Promise.resolve({ ok: true }),
  };
  set(view: NativeVoiceView) {
    this.view = view;
    act(() => {
      for (const l of this.listeners) l();
    });
  }
}

// The presence socket: the test pushes frames to the page.
let sockets: { channel: string; events: SocketEvents }[] = [];
beforeEach(() => {
  sockets = [];
  vi.spyOn(api, 'openVoiceSocket').mockImplementation((channel, events) => {
    sockets.push({ channel, events });
    return Promise.resolve({ ok: true, value: { close: () => undefined } });
  });
  vi.spyOn(api, 'getVoicePresence').mockResolvedValue({ ok: false, failure: 'unreachable' } as never);
});
afterEach(() => {
  cleanup();
  setSoundAudience(null);
  vi.useRealTimers();
  vi.restoreAllMocks();
});

async function frame(p: VoicePresenceV2) {
  await waitFor(() => { expect(sockets.length).toBeGreaterThan(0); });
  act(() => {
    const s = sockets[sockets.length - 1];
    s?.events.onOpen();
    s?.events.onFrame(JSON.stringify(p));
  });
}

const channels: ChannelV0[] = [];
const people: ReadonlyMap<number, Person> = new Map([[2, { name: 'Ada' } as Person]]);

function strip(fake: Fake, channel: string | null = 'general', binding?: string, viewerName?: string) {
  const ui = (ch: string | null) => (
    <NativeVoicePanel source={fake.nv} binding={binding} signedIn channel={ch} viewer={1} {...(viewerName === undefined ? {} : { viewerName })} people={people} voiceAllowed channels={channels} onUnauthorized={() => undefined} onOpen={() => undefined} />
  );
  const r = render(ui(channel));
  return { ...r, show: (ch: string | null) => { r.rerender(ui(ch)); } };
}

describe('NativeVoicePanel', () => {
  it('plays remote join, leave and share cues once, with no initial or reconnect backlog', async () => {
    const play = vi.spyOn(sounds, 'play').mockImplementation(() => undefined);
    setSoundAudience({ me: 1, allows: () => true });
    strip(new Fake(running()));
    const one = presence([1, '2026-11-17T00:00:00Z']);
    const two = presence([1, '2026-11-17T00:00:00Z'], [2, '2026-11-17T00:01:00Z']);
    const sharing: VoicePresenceV2 = { ...two, rooms: [{ participants: (two.rooms[0]?.participants ?? []).map((p) => p.principal_id === 2 ? { ...p, sharing: true, share_started_at: '2026-11-17T00:02:00Z' } : p) }] };
    await frame(one);
    expect(play).not.toHaveBeenCalled();
    await frame(two);
    await frame(sharing);
    await frame(sharing);
    await frame(one);
    expect(play.mock.calls).toEqual([['join'], ['share'], ['leave']]);
    const connections = sockets.length;
    act(() => { sockets.at(-1)?.events.onClose(1006); });
    await waitFor(() => { expect(sockets).toHaveLength(connections + 1); }, { timeout: 3000 });
    await frame(two);
    expect(play.mock.calls).toEqual([['join'], ['share'], ['leave']]);
    await frame(one);
    expect(play.mock.calls).toEqual([['join'], ['share'], ['leave'], ['leave']]);
  });

  it('keeps presence cues silent without an audience, while muted, or when only browsing', async () => {
    const play = vi.spyOn(sounds, 'play').mockImplementation(() => undefined);
    const fake = new Fake(running());
    strip(fake);
    const one = presence([1, '2026-11-17T00:00:00Z']);
    const two = presence([1, '2026-11-17T00:00:00Z'], [2, '2026-11-17T00:01:00Z']);
    await frame(one);
    await frame(two);
    setSoundAudience({ me: 1, allows: () => false });
    await frame(one);
    await frame(two);
    setSoundAudience({ me: 1, allows: () => true });
    fake.set(idle);
    await frame(one);
    await frame(two);
    expect(play).not.toHaveBeenCalled();
    // Rejoining this app resumes live cues without replacing its socket.
    fake.set(running());
    await frame(one);
    expect(play.mock.calls).toEqual([['leave']]);
  });

  it('never builds the browser engine or asks for a device', async () => {
    const room = vi.spyOn(sdk, 'newLiveKitRoom');
    const gum = vi.fn();
    const devices = vi.fn();
    Object.defineProperty(navigator, 'mediaDevices', { value: { getUserMedia: gum, enumerateDevices: devices }, configurable: true });
    const fake = new Fake(running());
    strip(fake);
    await frame(presence([1, '2026-01-01T00:00:00Z']));
    expect(room).not.toHaveBeenCalled();
    expect(gum).not.toHaveBeenCalled();
    expect(devices).not.toHaveBeenCalled();
    // Voice options is the helper's list, closed until opened.
    expect(screen.getByRole('button', { name: 'Voice options' }).getAttribute('aria-expanded')).toBe('false');
  });

  it('Join names the channel on screen and sends the last mute and deafen', async () => {
    const fake = new Fake(running({ muted: true, deafened: false }));
    const s = strip(fake);
    await frame(presence());
    fake.set(idle);
    const join = screen.getByRole('button', { name: /Join voice in #general/ });
    fireEvent.click(join);
    await waitFor(() => { expect(fake.calls).toEqual(['join general muted=true deafened=false']); });
    s.unmount();
  });

  // Issue #1457: a moderator's voice kick ends the helper with voice_removed.
  // The strip says so, never an internal error, and Join is there to go back.
  it('a voice kick says "removed from voice" and leaves Join on', async () => {
    const fake = new Fake(running());
    const s = strip(fake);
    await frame(presence([1, '2026-01-01T00:00:00Z']));
    fake.set({ status: 'ended', generation: 1, channel: 'general', retryable: false, problem: 'voice_removed', helper: helper({ phase: 'failed', reason: 'voice_removed', capture: 'off', blocked: ['not_connected'] }) });
    await frame(presence());
    expect(screen.getByText('voice ended: you were removed from voice')).toBeTruthy();
    expect(screen.getByText(/Not in voice/)).toBeTruthy();
    expect(screen.queryByText(/internal error/)).toBeNull();
    expect(screen.queryByRole('button', { name: 'Retry' })).toBeNull();
    const join = screen.getByRole('button', { name: /Join voice in #general/ });
    expect(join).toHaveProperty('disabled', false);
    fireEvent.click(join);
    await waitFor(() => { expect(fake.calls).toEqual(['join general muted=false deafened=false']); });
    s.unmount();
  });

  it('keeps its destination when the channel on screen changes while joined', async () => {
    const fake = new Fake(running());
    const s = strip(fake, 'general');
    await frame(presence([1, '2026-01-01T00:00:00Z']));
    expect(screen.getByText(/In voice/).textContent).toContain('in #general');
    s.show('random');
    expect(screen.getByRole('button', { name: '#general' })).toBeTruthy();
    expect(fake.calls).toEqual([]);
    expect(sockets.every((k) => k.channel === 'general')).toBe(true);
  });

  it('shows muting… until the answer, then the confirmed state', async () => {
    const fake = new Fake(running());
    strip(fake);
    await frame(presence([1, '2026-01-01T00:00:00Z']));
    fireEvent.click(screen.getByRole('button', { name: 'Mute' }));
    expect(screen.getByRole('button', { name: 'muting…' })).toBeTruthy();
    expect(fake.calls).toEqual(['mute true']);
    // Not optimistic: nothing says muted yet.
    expect(screen.queryByText('muted')).toBeNull();
    fake.set(running({ muted: true }));
    await act(async () => {
      fake.deferred?.({ ok: true });
      await Promise.resolve();
    });
    expect(screen.getByRole('button', { name: 'Unmute' })).toBeTruthy();
    expect(screen.getByText('muted')).toBeTruthy();
  });

  it('deafen sends the toggle, and mute is locked while deafened', async () => {
    const fake = new Fake(running());
    strip(fake);
    await frame(presence([1, '2026-01-01T00:00:00Z']));
    fireEvent.click(screen.getByRole('button', { name: 'Deafen' }));
    await waitFor(() => { expect(fake.calls).toEqual(['deafen true']); });
    fake.set(running({ deafened: true }));
    expect(screen.getByRole('button', { name: 'Undeafen' })).toBeTruthy();
    expect(screen.getByRole<HTMLButtonElement>('button', { name: 'Muted by deafen' }).disabled).toBe(true);
  });

  it('lists the roster of the joined channel with names', async () => {
    const fake = new Fake(running());
    strip(fake);
    await frame(presence([1, '2026-01-01T00:00:00Z'], [2, '2026-01-01T00:00:01Z']));
    const list = screen.getByRole('list');
    expect(within(list).getByText('You')).toBeTruthy();
    expect(within(list).getByText('Ada')).toBeTruthy();
  });

  describe('the viewer in the roster (#1481)', () => {
    const roster = () => Array.from(screen.getByRole('list').querySelectorAll('.who')).map((el) => el.textContent);

    it('names the viewer by their own name when this app is in voice', async () => {
      strip(new Fake(running()), 'general', undefined, 'nick');
      await frame(presence([1, '2026-01-01T00:00:00Z'], [2, '2026-01-01T00:00:01Z']));
      expect(roster()).toEqual(['nick', 'Ada']);
    });

    it('says You while the name is not known', async () => {
      strip(new Fake(running()));
      await frame(presence([1, '2026-01-01T00:00:00Z']));
      expect(roster()).toEqual(['You']);
    });

    it('says the name is on another device while another client holds the place', async () => {
      strip(new Fake(), 'general', undefined, 'nick');
      await frame(presence([1, '2026-01-01T00:00:00Z']));
      expect(roster()).toEqual(['nick (on another device)']);
    });

    it('says You (on another device) when the name is not known', async () => {
      strip(new Fake());
      await frame(presence([1, '2026-01-01T00:00:00Z']));
      expect(roster()).toEqual(['You (on another device)']);
    });

    it('does not say another device of this app’s own just-left session', async () => {
      const fake = new Fake(running());
      strip(fake, 'general', undefined, 'nick');
      await frame(presence([1, '2026-01-01T00:00:00Z']));
      fake.set(idle);
      expect(roster()).toEqual(['nick']);
    });
  });

  it('says the person was muted at a screen lock until they unmute or leave', async () => {
    const note = 'You were muted when your screen locked';
    const fake = new Fake({ ...running({ muted: true }), mutedByLock: true });
    strip(fake);
    await frame(presence([1, '2026-01-01T00:00:00Z']));
    expect(screen.getByText(note)).toBeTruthy();
    // Unmuted (main drops the marker with the mute): the note is gone.
    fake.set(running({ muted: false }));
    expect(screen.queryByText(note)).toBeNull();
    // A marker with no mute in force is not shown either.
    fake.set({ ...running({ muted: false }), mutedByLock: true });
    expect(screen.queryByText(note)).toBeNull();
    // Left: gone.
    fake.set(idle);
    expect(screen.queryByText(note)).toBeNull();
  });

  it('says why an unmute was refused while the screen is locked', async () => {
    const fake = new Fake({ ...running({ muted: true }), mutedByLock: true });
    strip(fake);
    await frame(presence([1, '2026-01-01T00:00:00Z']));
    fireEvent.click(screen.getByRole('button', { name: 'Unmute' }));
    expect(fake.calls).toEqual(['mute false']);
    await act(async () => {
      fake.deferred?.({ ok: false, error: 'locked' });
      await Promise.resolve();
    });
    expect(screen.getByText('Your screen is locked. Unlock it to unmute.')).toBeTruthy();
  });

  it('Leave calls leave', async () => {
    const fake = new Fake(running());
    strip(fake);
    await frame(presence([1, '2026-01-01T00:00:00Z']));
    fireEvent.click(screen.getByRole('button', { name: 'Leave voice' }));
    await waitFor(() => { expect(fake.calls).toEqual(['leave']); });
  });

  describe('the words', () => {
    const views: [string, NativeVoiceView][] = [
      ['idle', idle],
      ['unavailable', { status: 'unavailable', generation: 1, channel: null, retryable: false, problem: 'not-installed' }],
      ['starting', { status: 'starting', generation: 1, channel: 'general', retryable: false }],
      ['running', running()],
      ['stopping', { status: 'stopping', generation: 1, channel: 'general', retryable: false }],
      ['ended', { status: 'ended', generation: 1, channel: 'general', retryable: false, problem: 'left', helper: helper() }],
      ['failed', { status: 'failed', generation: 1, channel: 'general', retryable: true, problem: 'connect_failed' }],
      ['lost', { status: 'lost', generation: 1, channel: 'general', retryable: false, problem: 'owner_lost' }],
      ['reconnecting', running({ phase: 'reconnecting', reason: 'connection_lost', retryInMs: 3000 })],
      ['talking', running({ transmitting: true })],
      ['deafened', running({ deafened: true })],
    ];
    it.each(views)('says the line for %s', async (_name, view) => {
      const fake = new Fake(view);
      strip(fake, 'general', 'Right Ctrl');
      await frame(presence());
      expect(screen.getAllByText(words(view, 'Right Ctrl').text).length).toBeGreaterThan(0);
    });
    it('names a status for every state', () => {
      expect(Object.keys(statusWords)).toHaveLength(8);
    });
    it('says why voice is unavailable', () => {
      strip(new Fake({ status: 'unavailable', generation: 1, channel: null, retryable: false, problem: 'untrusted' }));
      expect(screen.getByText('The voice helper is not trusted, so it was not started.')).toBeTruthy();
      expect(screen.queryByRole('button', { name: /Join/ })).toBeNull();
    });
    it('says the folder is the problem for an untrusted helper in a writable folder (issue #1850)', () => {
      strip(new Fake({ status: 'unavailable', generation: 1, channel: null, retryable: false, problem: 'untrusted', cause: 'folder-writable' }));
      expect(screen.getByText("voice is not available here; text chat is unaffected: the voice helper's folder can be changed by other users")).toBeTruthy();
      expect(screen.getByText('The voice helper is in a folder that other users can change, so it was not started.')).toBeTruthy();
    });
    it('keeps the connection detail behind Details, as text (issue #1850)', () => {
      const detail = '<b>could not reach</b> wss://lk.example/rtc?access_token=<redacted>';
      strip(new Fake({ status: 'ended', generation: 1, channel: 'general', retryable: true, problem: 'connect_failed', helper: helper(), detail }));
      expect(screen.queryByText(detail)).toBeNull();
      const toggle = screen.getByRole('button', { name: 'Details' });
      expect(toggle.getAttribute('aria-expanded')).toBe('false');
      fireEvent.click(toggle);
      expect(screen.getByText(detail).tagName).toBe('P');
      expect(screen.getByText(detail).querySelector('b')).toBeNull();
      expect(toggle.getAttribute('aria-expanded')).toBe('true');
    });
    it('says the folder is the problem for an untrusted helper in a writable folder (issue #1850)', () => {
      strip(new Fake({ status: 'unavailable', generation: 1, channel: null, retryable: false, problem: 'untrusted', cause: 'folder-writable' }));
      expect(screen.getByText("voice is not available here; text chat is unaffected: the voice helper's folder can be changed by other users")).toBeTruthy();
      expect(screen.getByText('The voice helper is in a folder that other users can change, so it was not started.')).toBeTruthy();
    });
    it('keeps the connection detail behind Details, as text (issue #1850)', () => {
      const detail = '<b>could not reach</b> wss://lk.example/rtc?access_token=<redacted>';
      strip(new Fake({ status: 'ended', generation: 1, channel: 'general', retryable: true, problem: 'connect_failed', helper: helper(), detail }));
      expect(screen.queryByText(detail)).toBeNull();
      const toggle = screen.getByRole('button', { name: 'Details' });
      expect(toggle.getAttribute('aria-expanded')).toBe('false');
      fireEvent.click(toggle);
      expect(screen.getByText(detail).tagName).toBe('P');
      expect(screen.getByText(detail).querySelector('b')).toBeNull();
      expect(toggle.getAttribute('aria-expanded')).toBe('true');
    });
    it('says to restart after an update replaced the install, and offers neither Join nor Retry (issue #1600)', () => {
      strip(new Fake({ status: 'unavailable', generation: 1, channel: null, retryable: false, problem: 'update-pending' }));
      expect(screen.getByText('Restart Conch to finish the update.')).toBeTruthy();
      expect(screen.queryByRole('button', { name: /Join|Retry/ })).toBeNull();
    });
    it('uses the talk key name given, and the fallback otherwise', () => {
      strip(new Fake(running()), 'general', 'Right Ctrl');
      expect(screen.getByText('mic on · hold Right Ctrl to talk')).toBeTruthy();
      cleanup();
      strip(new Fake(running()));
      expect(screen.getByText('mic on · hold your talk key to talk')).toBeTruthy();
    });
  });

  describe('Retry', () => {
    it('shows for a retryable failure and calls retry', async () => {
      const fake = new Fake({ status: 'failed', generation: 1, channel: 'general', retryable: true, problem: 'connect_failed' });
      strip(fake);
      fireEvent.click(screen.getByRole('button', { name: 'Retry' }));
      await waitFor(() => { expect(fake.calls).toEqual(['retry']); });
      expect(screen.getByRole('button', { name: 'Leave voice' })).toBeTruthy();
    });
    it('is absent when the view is not retryable', () => {
      strip(new Fake({ status: 'failed', generation: 1, channel: 'general', retryable: false, problem: 'duplicate_identity' }));
      expect(screen.queryByRole('button', { name: 'Retry' })).toBeNull();
      expect(screen.getByText(/already in this call elsewhere/)).toBeTruthy();
    });
  });

  describe('duplicate identity', () => {
    it('warns, then joins only on a second Join within 10 s', async () => {
      vi.useFakeTimers({ shouldAdvanceTime: true });
      const fake = new Fake();
      strip(fake);
      await frame(presence([1, '2026-01-01T00:00:00Z']));
      const join = () => screen.getByRole('button', { name: /Join voice/ });
      fireEvent.click(join());
      expect(screen.getByText(duplicateWarning)).toBeTruthy();
      expect(fake.calls).toEqual([]);
      fireEvent.click(join());
      await vi.waitFor(() => { expect(fake.calls).toEqual(['join general muted=false deafened=false']); });
    });

    it('asks again once 10 s have passed', async () => {
      vi.useFakeTimers({ shouldAdvanceTime: true });
      const fake = new Fake();
      strip(fake);
      await frame(presence([1, '2026-01-01T00:00:00Z']));
      fireEvent.click(screen.getByRole('button', { name: /Join voice/ }));
      act(() => {
        vi.advanceTimersByTime(confirmWithin + 1);
      });
      expect(screen.queryByText(duplicateWarning)).toBeNull();
      fireEvent.click(screen.getByRole('button', { name: /Join voice/ }));
      expect(fake.calls).toEqual([]);
      expect(screen.getByText(duplicateWarning)).toBeTruthy();
    });

    it('does not warn about this app’s own just-left session', async () => {
      const fake = new Fake(running());
      strip(fake);
      await frame(presence([1, '2026-01-01T00:00:00Z']));
      fake.set(idle);
      expect(screen.queryByText(duplicateWarning)).toBeNull();
      fireEvent.click(screen.getByRole('button', { name: /Join voice/ }));
      await waitFor(() => { expect(fake.calls).toEqual(['join general muted=false deafened=false']); });
    });

    it('warns when presence shows a different session than the one noted', async () => {
      const fake = new Fake(running());
      strip(fake);
      await frame(presence([1, '2026-01-01T00:00:00Z']));
      fake.set(idle);
      await frame(presence([1, '2026-01-01T00:05:00Z']));
      fireEvent.click(screen.getByRole('button', { name: /Join voice/ }));
      expect(screen.getByText(duplicateWarning)).toBeTruthy();
      expect(fake.calls).toEqual([]);
    });
  });

  it('names the talk key from the binding, else the fallback', async () => {
    const named = new Fake(running({ ptt: 'ready' }));
    const a = strip(named, 'general', 'Mouse 4');
    expect(await screen.findByText(/hold Mouse 4 to talk/)).toBeTruthy();
    a.unmount();
    cleanup();
    strip(new Fake(running({ ptt: 'ready' })));
    expect(await screen.findByText(/hold your talk key to talk/)).toBeTruthy();
  });

  describe('share and watch (#1582)', () => {
    function companionOf(over: Partial<VoiceState> = {}) {
      const calls: string[] = [];
      const presences: VoicePresenceV2[] = [];
      const state: VoiceState = { ...initialVoice, ...over };
      const client: CompanionClient = {
        subscribe: () => () => undefined,
        snapshot: () => state,
        startCompanionShare: (c) => { calls.push(`share ${c}`); },
        watchCompanionStream: (c, p) => { calls.push(`watch ${c} ${String(p)}`); },
        stopShare: () => { calls.push('stop'); },
        restartShare: () => { calls.push('restart'); return Promise.resolve(); },
        watchStream: () => undefined,
        focusShare: () => undefined,
        setShareQuality: () => Promise.resolve(),
        presence: (p) => { presences.push(p); },
        attachStream: () => undefined,
        readShareStats: () => Promise.resolve(null),
      };
      const companion: Companion = { client, state };
      return { companion, calls, presences };
    }
    function withCompanion(fake: Fake, companion: Companion | null, channel = 'general') {
      return render(
        <NativeVoicePanel source={fake.nv} signedIn channel={channel} viewer={1} people={people} voiceAllowed channels={channels} companion={companion} onUnauthorized={() => undefined} onOpen={() => undefined} />,
      );
    }
    const sharingPresence = (): VoicePresenceV2 => {
      const p = presence([1, '2026-01-01T00:00:00Z'], [2, '2026-01-01T00:00:01Z']);
      const second = p.rooms[0]?.participants[1];
      if (second !== undefined) Object.assign(second, { sharing: true, share_started_at: '2026-01-01T00:00:02Z' });
      return p;
    };

    it('offers no share where the bridge has no companion', async () => {
      withCompanion(new Fake(running()), null);
      await frame(presence([1, '2026-01-01T00:00:00Z']));
      expect(screen.queryByRole('button', { name: 'Share screen' })).toBeNull();
    });

    it('Share starts the companion in the joined channel and leaves mute and deafen alone', async () => {
      const fake = new Fake(running());
      const c = companionOf();
      withCompanion(fake, c.companion);
      await frame(presence([1, '2026-01-01T00:00:00Z']));
      expect(screen.getByText(silentShareBefore)).toBeTruthy();
      fireEvent.click(screen.getByRole('button', { name: 'Share screen' }));
      expect(c.calls).toEqual(['share general']);
      expect(fake.calls).toEqual([]);
      expect(screen.getByRole('button', { name: 'Mute' })).toBeTruthy();
      expect(screen.getByRole('button', { name: 'Deafen' })).toBeTruthy();
    });

    it('while sharing, the indicator names the channel and Stop returns to the strip with voice untouched', async () => {
      const fake = new Fake(running({ muted: true }));
      const c = companionOf({ phase: 'connected', channel: 'general', companion: true, share: { kind: 'sharing', audio: false } });
      withCompanion(fake, c.companion);
      await frame(presence([1, '2026-01-01T00:00:00Z']));
      expect(screen.getByText('Sharing your screen in #general')).toBeTruthy();
      fireEvent.click(screen.getByRole('button', { name: 'Stop sharing' }));
      expect(c.calls).toEqual(['stop']);
      expect(fake.calls).toEqual([]);
      expect(screen.getByRole('button', { name: 'Unmute' })).toBeTruthy();
      expect(screen.getByRole('button', { name: 'Leave voice' })).toBeTruthy();
    });

    it('is disabled with its reason out of voice, and while the helper connects', async () => {
      const out = withCompanion(new Fake(idle), companionOf().companion);
      await frame(presence());
      expect(screen.getByRole<HTMLButtonElement>('button', { name: 'Share screen' }).disabled).toBe(true);
      expect(screen.getByText(shareDisabledWords.notJoined)).toBeTruthy();
      out.unmount();
      cleanup();
      withCompanion(new Fake(running({ phase: 'connecting' })), companionOf().companion);
      await frame(presence([1, '2026-01-01T00:00:00Z']));
      expect(screen.getByRole<HTMLButtonElement>('button', { name: 'Share screen' }).disabled).toBe(true);
      expect(screen.getByText(shareDisabledWords.connecting)).toBeTruthy();
    });

    it('is disabled when the channel on screen is not the one joined', async () => {
      withCompanion(new Fake(running()), companionOf().companion, 'random');
      await frame(presence([1, '2026-01-01T00:00:00Z']));
      expect(screen.getByRole<HTMLButtonElement>('button', { name: 'Share screen' }).disabled).toBe(true);
    });

    it('gives the companion the joined channel’s presence, and tiles for the others’ streams with Watch', async () => {
      const c = companionOf();
      withCompanion(new Fake(running()), c.companion);
      await frame(sharingPresence());
      expect(c.presences.length).toBeGreaterThan(0);
      fireEvent.click(within(screen.getByRole('region', { name: "Ada's stream" })).getByRole('button', { name: 'Watch' }));
      expect(c.calls).toEqual(['watch general 2']);
    });

    it('draws no tiles and gives no presence out of voice', async () => {
      const c = companionOf();
      withCompanion(new Fake(idle), c.companion);
      await frame(sharingPresence());
      expect(c.presences).toEqual([]);
      expect(screen.queryByRole('region', { name: "Ada's stream" })).toBeNull();
    });

    // The Linux share's sound (issue #1854): main's view carries it only with
    // main's switch on.
    describe('the share’s sound', () => {
      const v3 = (audio: NonNullable<NativeVoiceHelper['shareAudio']>, over: Partial<NativeVoiceHelper> = {}): NativeVoiceHelper =>
        helper({ version: 3, playback: 'on', talkMode: 'push_to_talk', shareAudio: audio, ...over });
      const on = { state: 'on', muted: false, sources: 2 } as const;
      const sounding = (audio: NonNullable<NativeVoiceHelper['shareAudio']> = on): NativeVoiceView => ({
        status: 'running',
        generation: 1,
        channel: 'general',
        retryable: false,
        helper: v3(audio),
        shareSound: { choice: 'system_excluding_call' },
      });
      const sharing = (): Companion => companionOf({ phase: 'connected', channel: 'general', companion: true, share: { kind: 'sharing', audio: false } }).companion;

      function withSource(fake: Fake, calls: string[], companion: Companion) {
        const source: NativeVoice = {
          ...fake.nv,
          shareSound: {
            start: () => {
              calls.push('start');
              return Promise.resolve({ ok: false, error: 'not-offered' });
            },
            choose: (c) => {
              calls.push(`choose ${c}`);
              return Promise.resolve({ ok: true, share: null });
            },
            mute: (m) => {
              calls.push(`share-mute ${String(m)}`);
              return Promise.resolve({ ok: true });
            },
            end: () => {
              calls.push('end');
              return Promise.resolve(true);
            },
            shareEnded: () => {
              calls.push('share ended');
              return Promise.resolve(true);
            },
          },
        };
        return render(
          <NativeVoicePanel source={source} signedIn channel="general" viewer={1} people={people} voiceAllowed channels={channels} companion={companion} onUnauthorized={() => undefined} onOpen={() => undefined} />,
        );
      }

      it('with the switch off (no share sound in the view): no toggle, no mute, the silent words, and nothing asked of main', async () => {
        const calls: string[] = [];
        withSource(new Fake({ status: 'running', generation: 1, channel: 'general', retryable: false, helper: v3(on) }), calls, sharing());
        await frame(presence([1, '2026-01-01T00:00:00Z']));
        expect(screen.queryByRole('checkbox', { name: 'Share with sound' })).toBeNull();
        expect(screen.queryByRole('button', { name: /share sound/ })).toBeNull();
        expect(screen.getByText('Sharing your screen in #general')).toBeTruthy();
        expect(calls).toEqual([]);
      });

      it('the mute sits beside Stop, shows the helper’s confirmed state, and is apart from the microphone’s Mute and Deafen', async () => {
        const calls: string[] = [];
        const fake = new Fake(sounding());
        withSource(fake, calls, sharing());
        await frame(presence([1, '2026-01-01T00:00:00Z']));
        const stop = screen.getByRole('button', { name: 'Stop sharing' });
        const mute = screen.getByRole('button', { name: 'Mute share sound' });
        expect(stop.nextElementSibling).toBe(mute);
        expect(mute.getAttribute('aria-pressed')).toBe('false');
        expect(screen.getByText('Sharing your screen and its sound in #general (sound from 2 apps)')).toBeTruthy();
        // The share-sound mute asks main for the share's mute and nothing of the microphone's.
        fireEvent.click(mute);
        await waitFor(() => { expect(calls).toContain('share-mute true'); });
        expect(fake.calls).toEqual([]);
        // The microphone's Mute is its own and asks for no share mute.
        calls.length = 0;
        fireEvent.click(screen.getByRole('button', { name: 'Mute' }));
        expect(fake.calls).toEqual(['mute true']);
        expect(calls.filter((c) => c.startsWith('share-mute'))).toEqual([]);
        // The helper's word moves the button, nothing else.
        fake.set(sounding({ state: 'on', muted: true, sources: 2 }));
        expect(screen.getByRole('button', { name: 'Unmute share sound' }).getAttribute('aria-pressed')).toBe('true');
      });

      it('says without sound until the helper says on, then with it', async () => {
        const fake = new Fake(sounding({ state: 'off', muted: false, sources: 0 }));
        withSource(fake, [], sharing());
        await frame(presence([1, '2026-01-01T00:00:00Z']));
        expect(screen.getByText('Sharing your screen without sound in #general')).toBeTruthy();
        expect(screen.queryByRole('button', { name: /share sound/ })).toBeNull();
        fake.set(sounding());
        expect(screen.getByText(/Sharing your screen and its sound in #general/)).toBeTruthy();
        expect(screen.getByRole('button', { name: 'Mute share sound' })).toBeTruthy();
      });

      it('asks main to add the sound when the share goes live (the end is the hook’s, tested with it)', async () => {
        const calls: string[] = [];
        const fake = new Fake(sounding({ state: 'off', muted: false, sources: 0 }));
        withSource(fake, calls, sharing());
        await frame(presence([1, '2026-01-01T00:00:00Z']));
        await waitFor(() => { expect(calls).toEqual(['start']); });
      });

      it('the toggle before the picker: on by default, and one click to “without sound”', async () => {
        const calls: string[] = [];
        const idleShare = companionOf({ phase: 'connected', channel: 'general', companion: true }).companion;
        withSource(new Fake(sounding({ state: 'off', muted: false, sources: 0 })), calls, idleShare);
        await frame(presence([1, '2026-01-01T00:00:00Z']));
        const box = screen.getByRole<HTMLInputElement>('checkbox', { name: 'Share with sound' });
        expect(box.checked).toBe(true);
        expect(screen.queryByText(silentShareBefore)).toBeNull();
        fireEvent.click(box);
        await waitFor(() => { expect(calls).toEqual(['choose none']); });
      });
    });
  });
});

it('keeps the active Windows binding label until a new helper generation, even when browsing text', async () => {
  const f = new Fake(running());
  const source: NativeVoice = { ...f.nv,
    keyDevices: () => Promise.resolve({ ok: true, platform: 'win32', devices: [], disclosure: [] }),
    saveTalkBinding: () => Promise.resolve({ ok: true }),
  };
  const onBound = vi.fn();
  const panel = (binding: string, channel = 'general') => <NativeVoicePanel source={source} binding={binding} onBound={onBound} signedIn channel={channel} viewer={1} people={people} voiceAllowed channels={channels} onUnauthorized={() => undefined} onOpen={() => undefined} />;
  const ui = render(panel('Left Ctrl'));
  fireEvent.click(screen.getByRole('button', { name: 'Talk key' }));
  await screen.findByText('none chosen');
  fireEvent.change(screen.getByRole('combobox', { name: 'Or pick one' }), { target: { value: 'Mouse5' } });
  fireEvent.click(screen.getByRole('button', { name: 'Save' }));
  await waitFor(() => { expect(onBound).toHaveBeenCalledWith('Mouse 5'); });
  ui.rerender(panel('Mouse 5', 'another-text-channel'));
  expect(screen.getByText('mic on · hold Left Ctrl to talk')).toBeTruthy();
  f.set({ ...running(), generation: 2 });
  expect(await screen.findByText('mic on · hold Mouse 5 to talk')).toBeTruthy();
  expect(f.calls).toEqual([]);
});
