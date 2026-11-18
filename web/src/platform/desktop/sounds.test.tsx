import { cleanup, render, screen } from '@testing-library/react';
import { afterEach, expect, it, vi } from 'vitest';
import { NotificationSettings } from '../../NotificationSettings';
import { setSoundPreferences, sounds } from '../../sounds';
import { platform } from './index';

vi.mock('#platform-selected', () => import('./index'));

afterEach(() => {
  cleanup();
  vi.unstubAllGlobals();
});

it('provides no desktop sound player and hides sound controls pending #1924', () => {
  const audio = vi.fn();
  const fetchCue = vi.fn();
  vi.stubGlobal('AudioContext', audio);
  vi.stubGlobal('fetch', fetchCue);
  expect(platform.services.sounds).toBeUndefined();
  setSoundPreferences({ message: true, join: true, leave: true, share: true, volume: 1 });
  render(<NotificationSettings policy="all" native permission="granted" supported onPolicy={() => undefined} onEnable={() => undefined} />);
  expect(screen.getByLabelText('Message notifications')).toBeDefined();
  expect(screen.queryByRole('group', { name: 'Sounds' })).toBeNull();
  for (const label of ['Message sound', 'Call join sound', 'Call leave sound', 'Share start sound', 'Sound volume']) {
    expect(screen.queryByLabelText(label)).toBeNull();
  }
  sounds.unlock();
  for (const sound of ['message', 'join', 'leave', 'share'] as const) sounds.play(sound);
  expect(audio).not.toHaveBeenCalled();
  expect(fetchCue).not.toHaveBeenCalled();
});
