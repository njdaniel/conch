import { useCallback, useEffect, useRef, useState } from 'react';
import * as api from './api';
import type { NotificationPolicy } from './api';
import { platform } from './platform';
import type { NativeNotification } from './platform/types';
import type { ChannelV0 } from './schema/channel';
import type { MessageV2 } from './schema/message';
import type { ReadStateV1 } from './schema/readstate';
import { isLater } from './state/messages';

export type { NotificationPolicy } from './api';
export type BrowserPermission = 'default' | 'denied' | 'granted' | 'unsupported';

export interface NotificationAlert {
  readonly id: string;
  readonly message: MessageV2;
  readonly channel: string;
  readonly mention: boolean;
}

type Preferences = api.NotificationPreferences;

interface Account {
  readonly id: number;
  readonly name: string;
}

const initial: Preferences = { policy: 'off', muted: [], native: false };

export function mentions(body: string, name: string): boolean {
  if (name === '') return false;
  const escaped = name.replace(/[.*+?^${}()|[\]\\]/g, '\\$&');
  return new RegExp(`(?:^|[\\s.,!?;:'"()[\\]{}<>])@${escaped}(?=$|[\\s.,!?;:'"()[\\]{}<>])`, 'iu').test(body);
}

function snippet(body: string): string {
  const text = body.replace(/\s+/g, ' ').trim();
  return text.length <= 160 ? text : `${text.slice(0, 159)}…`;
}

function supported(): boolean {
  return platform.services.notifications.supported();
}

function permission(): BrowserPermission {
  return platform.services.notifications.permission();
}

export function useNotifications({
  signedIn,
  origin,
  principal,
  channels,
  loaded,
  eventsLive,
  event,
  editEvent = null,
  readStateEvent,
  onOpen,
}: {
  readonly signedIn: boolean;
  readonly origin: string;
  readonly principal: Account | null;
  readonly channels: readonly ChannelV0[];
  readonly loaded: boolean;
  readonly eventsLive: boolean;
  readonly event: MessageV2 | null;
  // A message edited (issue #845, conch.message_edited.v1). It updates the
  // text of an alert already shown, in place, and never notifies, counts or
  // matches a mention: an edit is not a new message.
  readonly editEvent?: MessageV2 | null;
  readonly readStateEvent: ReadStateV1 | null;
  readonly onOpen: (channel: string) => void;
}) {
  const account = principal?.id ?? 0;
  const [preferences, setPreferences] = useState<Preferences>(initial);
  const [alerts, setAlerts] = useState<readonly NotificationAlert[]>([]);
  const [browserPermission, setBrowserPermission] = useState<BrowserPermission>(permission);
  const natives = useRef(new Map<string, NativeNotification>());
  const consumed = useRef<MessageV2 | null>(null);
  const delivered = useRef(new Set<string>());
  const lastSeen = useRef(new Map<number, number>());
  const initialized = useRef(new Set<number>());
  const accessEpoch = useRef(new Map<number, number>());
  const requests = useRef(new Map<number, object>());
  const previousLive = useRef(false);
  const latestOpen = useRef(onOpen);
  latestOpen.current = onOpen;
  const skipWrite = useRef(true);

  const close = useCallback((id: string) => {
    natives.current.get(id)?.close();
    natives.current.delete(id);
  }, []);
  const clear = useCallback(() => {
    for (const notification of natives.current.values()) notification.close();
    natives.current.clear();
    setAlerts([]);
  }, []);

  useEffect(() => () => {
    for (const notification of natives.current.values()) notification.close();
    natives.current.clear();
  }, []);

  useEffect(() => {
    skipWrite.current = true;
    consumed.current = null;
    delivered.current.clear();
    lastSeen.current.clear();
    initialized.current.clear();
    accessEpoch.current.clear();
    requests.current.clear();
    previousLive.current = false;
    setPreferences(api.readNotificationPreferences(origin, account));
    setBrowserPermission(permission());
    clear();
  }, [origin, account, clear]);

  useEffect(() => {
    if (skipWrite.current) {
      skipWrite.current = false;
      return;
    }
    if (account !== 0) api.writeNotificationPreferences(origin, account, preferences);
  }, [origin, account, preferences]);

  useEffect(() => {
    if (!signedIn) clear();
  }, [signedIn, clear]);

  useEffect(() => {
    if (!loaded) return;
    const visible = new Set(channels.map((channel) => channel.id));
    for (const alert of alerts) if (!visible.has(alert.message.channel_id)) close(alert.id);
    setAlerts((previous) => previous.some((alert) => !visible.has(alert.message.channel_id)) ? previous.filter((alert) => visible.has(alert.message.channel_id)) : previous);
  }, [loaded, channels, alerts, close]);

  useEffect(() => {
    if (readStateEvent === null || readStateEvent.unread !== 0) return;
    accessEpoch.current.set(readStateEvent.channel_id, (accessEpoch.current.get(readStateEvent.channel_id) ?? 0) + 1);
    for (const alert of alerts) if (alert.message.channel_id === readStateEvent.channel_id) close(alert.id);
    setAlerts((previous) => previous.some((alert) => alert.message.channel_id === readStateEvent.channel_id) ? previous.filter((alert) => alert.message.channel_id !== readStateEvent.channel_id) : previous);
  }, [readStateEvent, alerts, close]);

  const openAlert = useCallback((id: string) => {
    const alert = alerts.find((candidate) => candidate.id === id);
    if (alert === undefined) return;
    close(id);
    setAlerts((previous) => previous.filter((candidate) => candidate.id !== id));
    const channel = channels.find((candidate) => candidate.id === alert.message.channel_id);
    if (channel !== undefined) latestOpen.current(channel.name);
  }, [alerts, channels, close]);

  const deliver = useCallback((message: MessageV2) => {
    if (!signedIn || !eventsLive || principal === null || preferences.policy === 'off' || preferences.muted.includes(message.channel_id) || message.author_id === principal.id) return;
    const channel = channels.find((candidate) => candidate.id === message.channel_id);
    if (channel === undefined) return;
    const mention = mentions(message.body, principal.name);
    if (preferences.policy === 'mentions' && !mention) return;
    const id = `${origin}:${String(account)}:${String(message.id)}`;
    if (delivered.current.has(id)) return;
    delivered.current.add(id);
    const alert = { id, message, channel: channel.name, mention };
    setAlerts((previous) => {
      const next = [...previous.filter((candidate) => candidate.id !== id), alert];
      for (const evicted of next.slice(0, Math.max(0, next.length - 10))) close(evicted.id);
      return next.slice(-10);
    });
    if (preferences.native && browserPermission === 'granted' && document.visibilityState === 'hidden' && supported()) {
      const title = mention ? `Mention in #${channel.name}` : `New message in #${channel.name}`;
      const notification = platform.services.notifications.show(title, snippet(message.body), id);
      natives.current.set(id, notification);
      notification.onclick = () => {
        window.focus();
        close(id);
        setAlerts((previous) => previous.filter((candidate) => candidate.id !== id));
        latestOpen.current(channel.name);
      };
    }
  }, [signedIn, eventsLive, principal, preferences, browserPermission, channels, origin, account, close]);
  useEffect(() => {
    if (editEvent === null) return;
    // The alert of this message, under the client rule: an older copy never
    // replaces a newer one.
    const stale = (alert: NotificationAlert) =>
      alert.message.id === editEvent.id && alert.message.channel_id === editEvent.channel_id && isLater(editEvent, alert.message);
    setAlerts((previous) => (previous.some(stale) ? previous.map((alert) => (stale(alert) ? { ...alert, message: editEvent } : alert)) : previous));
  }, [editEvent]);

  const latestDeliver = useRef(deliver);
  latestDeliver.current = deliver;

  useEffect(() => {
    if (event === null || consumed.current === event) return;
    consumed.current = event;
    deliver(event);
  }, [event, deliver]);

  useEffect(() => {
    const reconnect = previousLive.current !== eventsLive;
    previousLive.current = eventsLive;
    if (!signedIn || !eventsLive || !loaded || account === 0) return;
    let cancelled = false;
    void Promise.all(channels.map(async (channel) => {
      const first = !initialized.current.has(channel.id);
      const pending = requests.current.has(channel.id);
      if (!reconnect && !first && !pending) return;
      const after = lastSeen.current.get(channel.id) ?? 0;
      const epoch = accessEpoch.current.get(channel.id) ?? 0;
      const request = {};
      requests.current.set(channel.id, request);
      try {
        const result = await api.loadMessagesAscending(channel.name, after);
        if (!result.ok || !result.complete || cancelled || requests.current.get(channel.id) !== request || epoch !== (accessEpoch.current.get(channel.id) ?? 0)) return;
        initialized.current.add(channel.id);
        for (const message of result.value) {
          lastSeen.current.set(channel.id, Math.max(lastSeen.current.get(channel.id) ?? 0, message.id));
          if (!first) latestDeliver.current(message);
        }
      } finally {
        if (requests.current.get(channel.id) === request) requests.current.delete(channel.id);
      }
    }));
    return () => {
      cancelled = true;
    };
  }, [signedIn, eventsLive, loaded, account, channels]);

  const setPolicy = useCallback((policy: NotificationPolicy) => {
    setPreferences((previous) => ({ ...previous, policy }));
    if (policy === 'off') clear();
  }, [clear]);

  const toggleMute = useCallback((channel: number) => {
    setPreferences((previous) => {
      const muted = previous.muted.includes(channel) ? previous.muted.filter((id) => id !== channel) : [...previous.muted, channel];
      return { ...previous, muted };
    });
    for (const alert of alerts) if (alert.message.channel_id === channel) close(alert.id);
    setAlerts((previous) => previous.filter((alert) => alert.message.channel_id !== channel));
  }, [alerts, close]);

  const enableNative = useCallback(async () => {
    if (!supported()) {
      setBrowserPermission('unsupported');
      return;
    }
    const answer = await platform.services.notifications.request();
    setBrowserPermission(answer);
    setPreferences((previous) => ({ ...previous, native: answer === 'granted' }));
    if (answer !== 'granted') for (const notification of natives.current.values()) notification.close();
    if (answer !== 'granted') natives.current.clear();
  }, []);

  return {
    alerts,
    policy: preferences.policy,
    muted: new Set(preferences.muted),
    native: preferences.native,
    browserPermission,
    browserSupported: supported(),
    setPolicy,
    toggleMute,
    enableNative,
    openAlert,
    clearAlerts: clear,
  };
}
