import { readNotificationPreferences } from '../api';
import { platform } from '../platform';
import { sounds } from '../sounds';
import type { CallScope } from './api';

export type CallAlert = (scope: CallScope) => (() => void) | undefined;

export const alertIncomingCall: CallAlert = (scope) => {
  const preferences = readNotificationPreferences(scope.origin, scope.account);
  // Direct calls have no message mention. The in-app prompt stays visible.
  if (preferences.policy !== 'all') return;

  // The existing player enforces opt-in, volume and gesture unlock. A
  // reconnect snapshot and a tab returning to the foreground play nothing.
  if (document.visibilityState === 'visible') {
    sounds.play('join');
    return;
  }

  const notifications = platform.services.notifications;
  // Desktop's native bridge requires a channel to route the notification.
  // The shared in-app invitation is available in both clients.
  if (platform.name !== 'browser' || !preferences.native ||
      !notifications.supported() || notifications.permission() !== 'granted') return;

  try {
    const notification = notifications.show('Incoming private call', 'Open Conch to answer.', crypto.randomUUID(), '');
    const close = () => {
      notification.onclick = null;
      notification.close();
    };
    notification.onclick = () => {
      window.focus();
      close();
    };
    return close;
  } catch {
    // Native permission can change between the check and show.
    return;
  }
};
