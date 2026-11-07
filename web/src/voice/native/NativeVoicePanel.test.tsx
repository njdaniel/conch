// The desktop voice strip (issue #1376, D3-5 of #771), driven by a fake native
// voice and a fake presence socket: no helper, no browser engine, no network.
import { act, cleanup, fireEvent, render, screen, waitFor, within } from '@testing-library/react';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import * as api from '../../api';
import type { NativeVoiceHelper, NativeVoiceView } from '../../platform/desktop/bridge';
import type { NativeVoice, NativeVoiceOutcome } from '../../platform/types';
import type { ChannelV0 } from '../../schema/channel';
import type { VoicePresenceV1 } from '../../schema/voice';
import type { SocketEvents } from '../../state/feed';
import type { Person } from '../../useLiveChannel';
import * as sdk from '../sdk';
import { confirmWithin, duplicateWarning, NativeVoicePanel } from './NativeVoicePanel';
import { statusWords, words } from './words';

const idle: NativeVoiceView = { status: 'idle', generation: 0, channel: null, retryable: false };
const helper = (over: Partial<NativeVoiceHelper> = {}): NativeVoiceHelper => ({ version: 2, phase: 'connected', capture: 'on', transmitting: false, muted: false, deafened: false, blocked: [], ptt: 'ready', ...over });
const running = (over: Partial<NativeVoiceHelper> = {}, channel = 'general'): NativeVoiceView => ({ status: 'running', generation: 1, channel, retryable: false, helper: helper(over) });

function presence(...joins: readonly [number, string][]): VoicePresenceV1 {
  return {
    schema: 'conch.voice_presence.v1',
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
  deferred: ((out: { ok: true }) => void) | null = null;
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
  vi.useRealTimers();
  vi.restoreAllMocks();
});

async function frame(p: VoicePresenceV1) {
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
});
