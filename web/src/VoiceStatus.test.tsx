// Which voice strip the platform gets (issue #1376): the native one where the
// platform has a native voice, the browser's everywhere else.
import { act, cleanup, render, screen, waitFor } from '@testing-library/react';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import * as api from './api';
import { platform } from './platform';
import type { NativeVoiceView } from './platform/desktop/bridge';
import type { NativeVoice } from './platform/types';
import type { ChannelV0 } from './schema/channel';
import type { VoicePresenceV2 } from './schema/voice';
import type { SocketEvents } from './state/feed';
import { ChannelJoin, shortcutWait, type ChannelJoinHandler } from './voice/channelJoin';
import { duplicateWarning } from './voice/native/NativeVoicePanel';
import * as sdk from './voice/sdk';
import { VoiceStatus } from './VoiceStatus';

const idle: NativeVoiceView = { status: 'idle', generation: 0, channel: null, retryable: false };
const nv: NativeVoice = {
  snapshot: () => idle,
  subscribe: () => () => undefined,
  join: () => Promise.reject(new Error('not in this test')),
  leave: () => Promise.reject(new Error('not in this test')),
  retry: () => Promise.reject(new Error('not in this test')),
  setMute: () => Promise.resolve({ ok: true }),
  setDeafen: () => Promise.resolve({ ok: true }),
  setDevices: () => Promise.resolve({ ok: true }),
  setTalkMode: () => Promise.resolve({ ok: true }),
  devices: () => Promise.resolve({ ok: false, error: 'unsupported' }),
  saveDevice: () => Promise.resolve({ ok: true }),
};

function strip() {
  return (
    <VoiceStatus signedIn channel="general" viewer={1} people={new Map()} voiceAllowed reachable channels={[]} occupancy={new Map()} onUnauthorized={() => undefined} onOpen={() => undefined} />
  );
}

beforeEach(() => {
  vi.spyOn(api, 'openVoiceSocket').mockResolvedValue({ ok: true, value: { close: () => undefined } });
});
afterEach(() => {
  cleanup();
  vi.useRealTimers();
  vi.unstubAllGlobals();
  vi.restoreAllMocks();
  Reflect.deleteProperty(platform, 'nativeVoice');
});

const general: ChannelV0 = { id: 1, name: 'general', created_at: '2026-10-18T00:00:00Z' };
const other: ChannelV0 = { ...general, id: 2, name: 'other' };

function shortcut(initial = general, reachable = true, voiceAllowed = true) {
  const handler: { current: ChannelJoinHandler | null } = { current: null };
  const feeds = new Map<string, SocketEvents>();
  vi.mocked(api.openVoiceSocket).mockImplementation((name, events) => {
    feeds.set(name, events);
    return Promise.resolve({ ok: true, value: { close: () => undefined } });
  });
  const ui = (selected: ChannelV0, ready: boolean, allowed: boolean) => (
    <ChannelJoin.Provider value={handler}>
      <VoiceStatus signedIn channel={selected.name} viewer={1} people={new Map()} voiceAllowed={allowed} reachable={ready} channels={[general, other]} occupancy={new Map()} onUnauthorized={() => undefined} onOpen={() => undefined} />
    </ChannelJoin.Provider>
  );
  const view = render(ui(initial, reachable, voiceAllowed));
  return {
    show: (selected: ChannelV0, ready = true, allowed = true) => {
      view.rerender(ui(selected, ready, allowed));
    },
    request: (target: ChannelV0) => {
      act(() => { handler.current?.(target); });
    },
    frame: (channel: ChannelV0, fields: Partial<VoicePresenceV2> = {}) => {
      const feed = feeds.get(channel.name);
      if (feed === undefined) throw new Error('no voice presence feed');
      act(() => {
        feed.onOpen();
        feed.onFrame(JSON.stringify({ schema: 'conch.voice_presence.v2', channel_id: channel.id, configured: true, available: true, rooms: [], ...fields }));
      });
    },
  };
}

describe('VoiceStatus', () => {
  it('renders the browser strip on the browser platform', () => {
    render(strip());
    // "Microphone off" is the Watch only hint's, said once and only where
    // joining is offered (issue #2198, UI-talk-7); this strip has no line of it.
    expect(screen.getByRole('region', { name: 'Voice' })).toBeTruthy();
    expect(screen.queryByText('Microphone off')).toBeNull();
  });

  it('renders the native strip, and builds no room, where the platform has a native voice', async () => {
    const room = vi.spyOn(sdk, 'newLiveKitRoom');
    Object.defineProperty(platform, 'nativeVoice', { value: nv, configurable: true });
    render(strip());
    await waitFor(() => { expect(screen.getByText('voice off')).toBeTruthy(); });
    expect(screen.queryByText(/Microphone off/)).toBeNull();
    expect(room).not.toHaveBeenCalled();
  });

  it('waits for the selected browser room, joins once, and keeps voice when browsing', () => {
    vi.stubGlobal('isSecureContext', true);
    const session = vi.spyOn(api, 'createVoiceSession').mockImplementation(() => new Promise(() => undefined));
    const s = shortcut();
    s.frame(general);
    s.show(other);
    s.request(other);
    s.request(other);
    s.frame(other, { channel_id: general.id });
    expect(session).not.toHaveBeenCalled();
    s.frame(other);
    expect(session.mock.calls.map(([channel]) => channel)).toEqual(['other']);
    s.request(other);
    s.show(general);
    s.request(general);
    expect(session).toHaveBeenCalledTimes(1);
    expect(screen.getByRole('button', { name: '#other' })).toBeTruthy();
  });

  it('cancels browser requests when a room is unavailable, times out, or is left before its frame', () => {
    vi.useFakeTimers();
    vi.stubGlobal('isSecureContext', true);
    const session = vi.spyOn(api, 'createVoiceSession').mockImplementation(() => new Promise(() => undefined));
    const close = vi.fn(() => Promise.resolve());
    const resume = vi.fn(() => Promise.resolve());
    vi.stubGlobal('AudioContext', class { close = close; resume = resume; });
    const s = shortcut();
    s.request(general);
    expect(resume).toHaveBeenCalledTimes(1);
    s.frame(general, { available: false });
    expect(close).toHaveBeenCalledTimes(1);
    s.show(other);
    s.request(other);
    act(() => { vi.advanceTimersByTime(shortcutWait); });
    s.frame(other);
    expect(close).toHaveBeenCalledTimes(2);
    s.show(general);
    s.request(general);
    s.show(other);
    s.frame(other);
    expect(close).toHaveBeenCalledTimes(3);
    expect(session).not.toHaveBeenCalled();
  });

  it('uses the native duplicate-device confirmation and de-duplicates an in-flight join', () => {
    const join = vi.spyOn(nv, 'join').mockImplementation(() => new Promise(() => undefined));
    Object.defineProperty(platform, 'nativeVoice', { value: nv, configurable: true });
    const s = shortcut();
    s.request(general);
    s.frame(general, {
      rooms: [{ participants: [{ principal_id: 1, can_publish: true, transmitting: false, joined_at: '2026-10-18T00:00:00Z' }] }],
    });
    expect(screen.getByText(duplicateWarning)).toBeTruthy();
    expect(join).not.toHaveBeenCalled();
    s.request(general);
    s.request(general);
    expect(join).toHaveBeenCalledExactlyOnceWith({ channel: 'general', muted: false, deafened: false });
  });

  it('keeps native voice in the current room and uses the last mute state for a later join', () => {
    const join = vi.spyOn(nv, 'join').mockImplementation(() => new Promise(() => undefined));
    const snapshot = vi.spyOn(nv, 'snapshot').mockReturnValue({
      status: 'running',
      generation: 1,
      channel: 'general',
      retryable: false,
      helper: { version: 2, phase: 'connected', capture: 'on', transmitting: false, muted: true, deafened: true, blocked: [], ptt: 'ready' },
    });
    Object.defineProperty(platform, 'nativeVoice', { value: nv, configurable: true });
    const s = shortcut();
    s.frame(general);
    s.request(general);
    s.show(other);
    s.request(other);
    expect(join).not.toHaveBeenCalled();
    expect(screen.getByRole('button', { name: '#general' })).toBeTruthy();

    snapshot.mockReturnValue(idle);
    s.show(other);
    s.frame(other);
    s.request(other);
    expect(join).toHaveBeenCalledExactlyOnceWith({ channel: 'other', muted: true, deafened: true });
  });

  it('has no native shortcut while disconnected or without voice permission', () => {
    const join = vi.spyOn(nv, 'join').mockImplementation(() => new Promise(() => undefined));
    Object.defineProperty(platform, 'nativeVoice', { value: nv, configurable: true });
    const s = shortcut(general, false);
    s.frame(general);
    s.request(general);
    expect(join).not.toHaveBeenCalled();
    s.show(general, true, false);
    s.request(general);
    expect(join).not.toHaveBeenCalled();
  });
});
