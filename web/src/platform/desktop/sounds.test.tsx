import { cleanup, fireEvent, render, screen } from '@testing-library/react';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { NotificationSettings } from '../../NotificationSettings';
import { setSoundPreferences, sounds } from '../../sounds';
import { platform } from './index';
import { DesktopSoundSink } from './sounds';

vi.mock('#platform-selected', () => import('./index'));

class FakeAudio {
  preload = '';
  readyState = 4;
  currentTime = 9;
  volume = 1;
  load = vi.fn();
  play = vi.fn(() => Promise.resolve());
  constructor(readonly src: string) { clips.push(this); }
}
let clips: FakeAudio[];
const fetchCue = vi.fn();
const context = vi.fn();
beforeEach(() => {
  clips = [];
  vi.stubGlobal('Audio', FakeAudio);
  vi.stubGlobal('AudioContext', context);
  vi.stubGlobal('fetch', fetchCue);
});
afterEach(() => {
  cleanup();
  expect(fetchCue).not.toHaveBeenCalled();
  expect(context).not.toHaveBeenCalled();
  vi.unstubAllGlobals();
});

describe('desktop interface audio', () => {
  it('loads only the four bundled WAVs after unlock, and plays through Audio with volume and restart', () => {
    const sink = new DesktopSoundSink();
    expect(sink.ready()).toBe(false);
    sink.play('message', 0.3);
    expect(clips).toEqual([]);
    sink.unlock();
    sink.unlock();
    expect(sink.ready()).toBe(true);
    expect(clips.map((a) => a.src)).toEqual(['message', 'join', 'leave', 'share'].map((s) => `conch-app://ui/sounds/${s}.wav`));
    for (const clip of clips) {
      expect(clip.preload).toBe('auto');
      expect(clip.load).toHaveBeenCalledTimes(1);
      expect(clip.play).not.toHaveBeenCalled();
    }
    for (const sound of ['message', 'join', 'leave', 'share'] as const) sink.play(sound, 0.3);
    for (const clip of clips) {
      expect(clip.play).toHaveBeenCalledTimes(1);
      expect(clip.volume).toBe(0.3);
      expect(clip.currentTime).toBe(0);
    }
    const message = clips[0];
    if (message === undefined) throw new Error('missing message clip');
    message.currentTime = 5;
    sink.play('message', 0.7);
    expect(message.currentTime).toBe(0);
    expect(message.volume).toBe(0.7);
    expect(message.play).toHaveBeenCalledTimes(2);
  });

  it('skips unloaded cues without replaying them when ready, and ignores rejected or throwing playback', async () => {
    const sink = new DesktopSoundSink();
    sink.unlock();
    const message = clips[0];
    if (message === undefined) throw new Error('missing message clip');
    message.readyState = 0;
    sink.play('message', 1);
    message.readyState = 4;
    await Promise.resolve();
    expect(message.play).not.toHaveBeenCalled();
    message.play.mockRejectedValueOnce(new Error('autoplay blocked'));
    sink.play('message', 0.3);
    await Promise.resolve();
    message.play.mockImplementationOnce(() => { throw new Error('media failed'); });
    expect(() => { sink.play('message', 0.3); }).not.toThrow();
    sink.play('message', 0.3);
    expect(message.play).toHaveBeenCalledTimes(3);
  });

  it('stays silent when Audio is unsupported and retries construction on a later gesture', () => {
    const sink = new DesktopSoundSink();
    vi.stubGlobal('Audio', vi.fn(function () { throw new Error('unavailable'); }));
    expect(() => { sink.unlock(); }).not.toThrow();
    expect(sink.ready()).toBe(false);
    vi.stubGlobal('Audio', FakeAudio);
    sink.unlock();
    expect(sink.ready()).toBe(true);
    expect(clips).toHaveLength(4);
  });
});

it('provides desktop audio and shows working sound toggles and volume in notification settings', () => {
  expect(platform.services.sounds).toBeInstanceOf(DesktopSoundSink);
  setSoundPreferences({ message: false, join: false, leave: false, share: false, volume: 0.5 });
  render(<NotificationSettings policy="all" native permission="granted" supported onPolicy={() => undefined} onEnable={() => undefined} />);
  expect(screen.getByLabelText('Message notifications')).toBeDefined();
  expect(screen.getByRole('group', { name: 'Sounds' })).toBeDefined();
  for (const label of ['Message sound', 'Call join sound', 'Call leave sound', 'Share start sound']) {
    const toggle = screen.getByLabelText<HTMLInputElement>(label);
    expect(toggle.checked).toBe(false);
    fireEvent.click(toggle);
    expect(toggle.checked).toBe(true);
  }
  fireEvent.change(screen.getByLabelText('Sound volume'), { target: { value: '30' } });
  sounds.unlock();
  for (const sound of ['message', 'join', 'leave', 'share'] as const) sounds.play(sound);
  expect(clips).toHaveLength(4);
  for (const clip of clips) {
    expect(clip.play).toHaveBeenCalledTimes(1);
    expect(clip.volume).toBe(0.3);
  }
});
