import { afterEach, describe, expect, it, vi } from 'vitest';
import { writeNotificationPreferences } from '../api';
import { platform } from '../platform';
import { setSoundPreferences, sounds } from '../sounds';
import type { NativeNotification } from '../platform/types';
import { alertIncomingCall } from './alerts';

const scope = { origin: 'https://conch.example.test', account: 5 };
const silent = { message: false, join: false, leave: false, share: false, volume: 0.5 };

afterEach(() => {
  setSoundPreferences(silent);
  writeNotificationPreferences(scope.origin, scope.account, { policy: 'off', muted: [], native: false });
  vi.restoreAllMocks();
});

describe('private call alerts', () => {
  it('plays the foreground cue through the existing sound preference and volume', () => {
    vi.spyOn(document, 'visibilityState', 'get').mockReturnValue('visible');
    const sink = platform.services.sounds;
    if (sink === undefined) throw new Error('missing browser sound sink');
    vi.spyOn(sink, 'unlock').mockImplementation(() => undefined);
    vi.spyOn(sink, 'ready').mockReturnValue(true);
    const play = vi.spyOn(sink, 'play').mockImplementation(() => undefined);

    alertIncomingCall(scope);
    expect(play).not.toHaveBeenCalled();
    setSoundPreferences({ ...silent, join: true, volume: 0.2 });
    sounds.unlock();
    alertIncomingCall(scope);
    expect(play).toHaveBeenCalledExactlyOnceWith('join', 0.2);
    setSoundPreferences({ ...silent, join: true, volume: 0 });
    alertIncomingCall(scope);
    expect(play).toHaveBeenCalledTimes(1);
  });

  it('uses a content-free native notification in a hidden browser with permission', () => {
    const visibility = vi.spyOn(document, 'visibilityState', 'get').mockReturnValue('hidden');
    const sound = vi.spyOn(sounds, 'play');
    const close = vi.fn();
    const notification: NativeNotification = { onclick: null, close };
    const notifications = platform.services.notifications;
    vi.spyOn(notifications, 'supported').mockReturnValue(true);
    const permission = vi.spyOn(notifications, 'permission').mockReturnValue('granted');
    const show = vi.spyOn(notifications, 'show').mockReturnValue(notification);

    expect(alertIncomingCall(scope)).toBeUndefined();
    writeNotificationPreferences(scope.origin, scope.account, { policy: 'all', muted: [], native: true });
    const dismiss = alertIncomingCall(scope);
    expect(show).toHaveBeenCalledExactlyOnceWith('Incoming private call', 'Open Conch to answer.', expect.any(String), '');
    expect(sound).not.toHaveBeenCalled();
    dismiss?.();
    expect(close).toHaveBeenCalledTimes(1);
    expect(notification.onclick).toBeNull();

    permission.mockReturnValue('denied');
    expect(alertIncomingCall(scope)).toBeUndefined();
    visibility.mockReturnValue('visible');
    document.dispatchEvent(new Event('visibilitychange'));
    expect(show).toHaveBeenCalledTimes(1);
    expect(sound).not.toHaveBeenCalled();
  });
});
