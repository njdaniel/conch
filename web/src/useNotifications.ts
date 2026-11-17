import { messagePreview } from './spoilerFiles';
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

function snippet(message: MessageV2): string {
  const text = messagePreview(message).replace(/\s+/g, ' ').trim();
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
  const removed = useRef(new Set<number>());
  const delivered = useRef(new Set<string>());
  const lastSeen = useRef(new Map<number, number>());
  const initialized = useRef(new Set<number>());
  // Channels whose last reconciliation read ran to the end: only for these may
  // a live message move the cursor, or it would skip a gap a failed, capped or
  // discarded read left behind it.
  const caughtUp = useRef(new Set<number>());
  const accessEpoch = useRef(new Map<number, number>());
  const requests = useRef(new Map<number, object>());
  const previousLive = useRef(false);
  const latestOpen = useRef(onOpen);
  latestOpen.current = onOpen;
  // The channels as of the latest render, so a click on a notification shown
  // earlier resolves the channel by id to its name now, not to the name it
  // had when the notification was shown (issue #1620).
  const latestChannels = useRef(channels);
  latestChannels.current = channels;
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
    removed.current.clear();
    delivered.current.clear();
    lastSeen.current.clear();
    initialized.current.clear();
    caughtUp.current.clear();
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
    // Missed removals are not replayed below the cursor. Drop these copies
    // before backfill rather than retain stale bodies from the disconnected feed.
    if (!signedIn || !eventsLive) clear();
  }, [signedIn, eventsLive, clear]);

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

  // Whether a message would be announced now, and as what. Separate from the
  // announcing so a backfill can pick the newest few of those that qualify.
  const eligible = useCallback((message: MessageV2) => {
    if (message.removed === true || removed.current.has(message.id)) return null;
    if (!signedIn || !eventsLive || principal === null || preferences.policy === 'off' || preferences.muted.includes(message.channel_id) || message.author_id === principal.id) return null;
    const channel = channels.find((candidate) => candidate.id === message.channel_id);
    if (channel === undefined) return null;
    const mention = mentions(message.body, principal.name);
    if (preferences.policy === 'mentions' && !mention) return null;
    const id = `${origin}:${String(account)}:${String(message.id)}`;
    if (delivered.current.has(id)) return null;
    return { channel, mention, id };
  }, [signedIn, eventsLive, principal, preferences, channels, origin, account]);

  const deliver = useCallback((message: MessageV2) => {
    const found = eligible(message);
    if (found === null) return;
    const { channel, mention, id } = found;
    delivered.current.add(id);
    const alert = { id, message, channel: channel.name, mention };
    setAlerts((previous) => {
      const next = [...previous.filter((candidate) => candidate.id !== id), alert];
      for (const evicted of next.slice(0, Math.max(0, next.length - 10))) close(evicted.id);
      return next.slice(-10);
    });
    if (preferences.native && browserPermission === 'granted' && document.visibilityState === 'hidden' && supported()) {
      const title = mention ? `Mention in #${channel.name}` : `New message in #${channel.name}`;
      const notification = platform.services.notifications.show(title, snippet(message), id, channel.name);
      natives.current.set(id, notification);
      // The name passed to show() is the one the desktop bridge needs when it
      // raises the notification; the click itself opens by id (issue #1620).
      const channelId = channel.id;
      notification.onclick = () => {
        // A notification already closed (access withdrawn, message removed)
        // is stale: a late click opens nothing.
        const live = natives.current.get(id) === notification;
        window.focus();
        close(id);
        setAlerts((previous) => previous.filter((candidate) => candidate.id !== id));
        if (!live) return;
        const current = latestChannels.current.find((candidate) => candidate.id === channelId);
        if (current !== undefined) latestOpen.current(current.name);
      };
    }
  }, [eligible, browserPermission, close]);
  useEffect(() => {
    if (editEvent === null) return;
    if (editEvent.removed === true) removed.current.add(editEvent.id);
    if (consumed.current?.id === editEvent.id && editEvent.removed === true) consumed.current = null;
    if (editEvent.removed === true) for (const alert of alerts) if (alert.message.id === editEvent.id) close(alert.id);
    // The alert of this message, under the client rule: an older copy never
    // replaces a newer one.
    const stale = (alert: NotificationAlert) =>
      alert.message !== editEvent && alert.message.id === editEvent.id && alert.message.channel_id === editEvent.channel_id && isLater(editEvent, alert.message);
    setAlerts((previous) => (previous.some(stale) ? previous.map((alert) => (stale(alert) ? { ...alert, message: editEvent } : alert)) : previous));
  }, [editEvent, alerts, close]);

  useEffect(() => api.watchRemovals((message) => {
    removed.current.add(message.id);
    if (consumed.current?.id === message.id) consumed.current = null;
    close(`${origin}:${String(account)}:${String(message.id)}`);
    setAlerts((previous) => previous.map((alert) => alert.message.id === message.id ? { ...alert, message } : alert));
  }), [origin, account, close]);

  const latestDeliver = useRef(deliver);
  latestDeliver.current = deliver;
  const latestEligible = useRef(eligible);
  latestEligible.current = eligible;

  useEffect(() => {
    if (event === null || consumed.current === event) return;
    consumed.current = event;
    // A message that arrived live is one the next backfill need not fetch
    // again: without this the cursor stays where the first read left it, and
    // a reconnect after a long session re-walks the whole session (and, past
    // the page cap, delivers nothing). Only while the feed has been live since
    // the last reconciliation and none is under way: a message that arrives
    // in the render the socket returns in must not hide the gap behind it.
    if (eventsLive && previousLive.current && caughtUp.current.has(event.channel_id) && !requests.current.has(event.channel_id)) {
      lastSeen.current.set(event.channel_id, Math.max(lastSeen.current.get(event.channel_id) ?? 0, event.id));
    }
    deliver(event);
  }, [event, deliver, eventsLive]);

  useEffect(() => {
    const reconnect = previousLive.current !== eventsLive;
    previousLive.current = eventsLive;
    if (reconnect) caughtUp.current.clear();
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
      caughtUp.current.delete(channel.id);
      try {
        const result = await api.loadMessagesAscending(channel.name, after);
        // A first read must be whole: it is the baseline nothing earlier than
        // it is announced against. A later one that hit the page cap is still
        // a contiguous run from the cursor: deliver it and move the cursor to
        // its end, and the next reconciliation continues from there, rather
        // than announce nothing at all (and never advance) in a busy channel.
        if (!result.ok || (first && !result.complete) || cancelled || requests.current.get(channel.id) !== request || epoch !== (accessEpoch.current.get(channel.id) ?? 0)) return;
        initialized.current.add(channel.id);
        if (result.complete) caughtUp.current.add(channel.id);
        // The cursor moves over every message; only the newest ten of those
        // that would be announced are, which is all the page keeps anyway, so
        // a long backlog does not raise one native notification apiece.
        const announce: MessageV2[] = [];
        for (const message of result.value) {
          lastSeen.current.set(channel.id, Math.max(lastSeen.current.get(channel.id) ?? 0, message.id));
          if (!first && latestEligible.current(message) !== null) announce.push(message);
        }
        for (const message of announce.slice(-10)) latestDeliver.current(message);
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
