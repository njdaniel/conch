import { BrowserSoundSink } from './platform/browser/sounds';
import { beforeEach, describe, expect, it, vi } from 'vitest';
import { SoundPlayer, readSoundPreferences, setSoundPreferences, voiceSounds, type SoundPreferences } from './sounds';
import type { VoicePresenceV2 } from './schema/voice';
const enabled: SoundPreferences = { message: true, join: true, leave: true, share: true, volume: 0.3 };
beforeEach(() => { localStorage.clear(); });
describe('optional sounds', () => {
  it('never plays before unlock, never queues, and applies every toggle and volume', () => {
    let preferences = enabled;
    const sink = { unlock: vi.fn(), ready: vi.fn(() => true), play: vi.fn() };
    const player = new SoundPlayer(sink, () => preferences);
    player.play('message');
    expect(sink.play).not.toHaveBeenCalled();
    player.unlock();
    expect(sink.play).not.toHaveBeenCalled();
    for (const sound of ['message', 'join', 'leave', 'share'] as const) {
      player.play(sound);
      expect(sink.play).toHaveBeenLastCalledWith(sound, 0.3);
      preferences = { ...preferences, [sound]: false };
      player.play(sound);
    }
    expect(sink.play).toHaveBeenCalledTimes(4);
    preferences = { ...enabled, volume: 0 };
    player.play('message');
    preferences = enabled;
    sink.ready.mockReturnValue(false);
    player.play('message');
    expect(sink.play).toHaveBeenCalledTimes(4);
  });
  it('defaults off and persists the independent toggles and volume', () => {
    expect(readSoundPreferences()).toEqual({ message: false, join: false, leave: false, share: false, volume: 0.5 });
    const changed = { ...enabled, join: false, share: false };
    setSoundPreferences(changed);
    expect(readSoundPreferences()).toEqual(changed);
    localStorage.setItem('conch.web.sounds.v1', '{"volume":9,"message":"true"}');
    expect(readSoundPreferences()).toMatchObject({ volume: 1, message: false });
  });
  it('creates no context before a gesture and stays silent if Safari is suspended', () => {
    const constructor = vi.fn();
    vi.stubGlobal('AudioContext', constructor);
    const sink = new BrowserSoundSink();
    sink.play('message', 1);
    expect(constructor).not.toHaveBeenCalled();
    expect(sink.ready()).toBe(false);
    const source = { buffer: null, connect: vi.fn(), start: vi.fn(), disconnect: vi.fn(), onended: null };
    const resume = vi.fn(() => Promise.resolve());
    vi.stubGlobal('AudioContext', class {
      state = 'suspended';
      sampleRate = 24000;
      destination = {};
      resume = resume;
      createBufferSource = () => source;
      createBuffer = () => ({});
    });
    vi.stubGlobal('fetch', vi.fn(() => Promise.reject(new Error('offline'))));
    const suspended = new BrowserSoundSink();
    const player = new SoundPlayer(suspended, () => enabled);
    player.unlock();
    expect(resume).toHaveBeenCalledTimes(1);
    expect(source.start).toHaveBeenCalledTimes(1); // Silent gesture source only.
    player.play('message');
    expect(suspended.ready()).toBe(false);
    expect(source.start).toHaveBeenCalledTimes(1);
    vi.unstubAllGlobals();
  });
});
const presence = (people: number[], sharing = false): VoicePresenceV2 => ({
  schema: 'conch.voice_presence.v2', channel_id: 7, configured: true, available: true,
  rooms: [{ participants: people.map((id) => ({ principal_id: id, can_publish: true, transmitting: false, joined_at: '2026-11-17T00:00:00Z', sharing: sharing && id === 3, share_started_at: sharing && id === 3 ? '2026-11-17T01:00:00Z' : undefined })) }],
});
it('announces each remote presence transition once, with no initial, own, other-room or unavailable cues', () => {
  const play = vi.fn();
  const one = presence([2]);
  const two = presence([2, 3]);
  const share = presence([2, 3], true);
  voiceSounds(null, two, 2, play);
  voiceSounds(presence([]), one, 2, play);
  voiceSounds(one, two, 2, play);
  voiceSounds(two, two, 2, play);
  voiceSounds(two, share, 2, play);
  voiceSounds(share, share, 2, play);
  voiceSounds(share, one, 2, play);
  expect(play.mock.calls).toEqual([['join'], ['share'], ['leave']]);
  play.mockClear();
  voiceSounds(two, presence([3]), 2, play);
  voiceSounds(presence([3]), presence([3], true), 2, play);
  voiceSounds(one, { ...two, channel_id: 8 }, 2, play);
  voiceSounds(two, { ...one, available: false }, 2, play);
  expect(play).not.toHaveBeenCalled();
});
