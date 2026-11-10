// The bridge object the preload exposes, built from one function that makes an
// invoke and one that listens for main's feed events. Input: those functions
// (Electron's ipcRenderer.invoke and ipcRenderer.on in the real preload, fakes
// in tests). Output: a frozen object with exactly the functions of
// web/src/platform/desktop/bridge.ts.
//
// Each function sends its one argument on a channel derived from its own name
// and unwraps main's envelope. A refusal rejects with a plain Error that says
// "refused" and nothing more. No function takes a channel, a URL, a header map
// or a method, and nothing here holds an ipcRenderer, an event or a credential.
//
// One call is synchronous: notificationSupported sends on the channel
// `conch:notificationSupported` (ipcRenderer.sendSync) outside the invoke
// registry, and unwraps the same Envelope from the returned value.
//
// onVoiceState hands the page each NativeVoiceView main sends on the voice
// channel, and drops any message that is not exactly one.
//
// subscribe numbers each feed itself (so main's first event for it can never
// arrive before the number is known here), asks main to open it, hands the
// page each frame of a batch in order and then acknowledges the batch, which
// is what lets main send the next. An unsubscribed feed is forgotten here at
// once: whatever main had already sent for it is dropped.
import type { ExposedDesktopBridge, FeedHandlers, FeedRequest, NativeVoiceView, NotificationClick, Unsubscribe } from '../../web/src/platform/desktop/bridge.ts';
import {
  voiceControlBlock,
  voiceControlCapture,
  voiceControlPhase,
  voiceControlPlayback,
  voiceControlPTT,
  voiceControlPTTProblem,
  voiceControlReason,
  voiceControlTalkMode,
} from '../../web/src/schema/generated.ts';
import { channelOf, NOTIFICATION_SUPPORT, TAG_MAX, type Envelope, type FeedEvent, type InvokedFunction } from '../shared/bridge-names.ts';

export type Invoke = (channel: string, ...args: unknown[]) => Promise<unknown>;

// Listen calls back with each message main sends on the feed channel; only
// the message, never the event it came in.
export type Listen = (callback: (message: unknown) => void) => void;

// Sync makes one named synchronous call and returns its unvalidated answer.
export type Sync = (channel: string) => unknown;

// What a refused subscription is told: a close the page's feed retries later.
const REFUSED = 1013;

function isFeedEvent(value: unknown): value is FeedEvent {
  if (typeof value !== 'object' || value === null) return false;
  const record = value as Record<string, unknown>;
  if (typeof record['id'] !== 'number') return false;
  switch (record['type']) {
    case 'open':
      return true;
    case 'frames':
      return Array.isArray(record['frames']) && record['frames'].every((frame) => typeof frame === 'string');
    case 'close':
      return typeof record['code'] === 'number' && typeof record['reason'] === 'string';
    default:
      return false;
  }
}

function isNotificationClick(value: unknown): value is NotificationClick {
  if (typeof value !== 'object' || value === null || Array.isArray(value)) return false;
  const record = value as Record<string, unknown>;
  return Object.keys(record).length === 2 && record['type'] === 'click' && typeof record['tag'] === 'string' && record['tag'].length > 0 && record['tag'].length <= TAG_MAX;
}

const STATUSES: ReadonlySet<unknown> = new Set(['idle', 'unavailable', 'starting', 'running', 'stopping', 'ended', 'failed', 'lost']);

const only = (record: Record<string, unknown>, keys: readonly string[]): boolean => Object.keys(record).every((key) => keys.includes(key));
const absent = (value: unknown): boolean => value === undefined;
const isCount = (value: unknown): value is number => typeof value === 'number' && Number.isSafeInteger(value) && value >= 0;
const isVocab = (check: (value: unknown) => string[], value: unknown): boolean => check(value).length === 0;

function isHelper(value: unknown): boolean {
  if (typeof value !== 'object' || value === null) return false;
  const h = value as Record<string, unknown>;
  const keys = ['version', 'phase', 'reason', 'retryInMs', 'capture', 'playback', 'transmitting', 'muted', 'deafened', 'blocked', 'talkMode', 'ptt', 'pttProblem'];
  return (
    only(h, keys) &&
    (h['version'] === 1 || h['version'] === 2) &&
    isVocab(voiceControlPhase, h['phase']) &&
    (absent(h['reason']) || isVocab(voiceControlReason, h['reason'])) &&
    (absent(h['retryInMs']) || isCount(h['retryInMs'])) &&
    isVocab(voiceControlCapture, h['capture']) &&
    (absent(h['playback']) || isVocab(voiceControlPlayback, h['playback'])) &&
    typeof h['transmitting'] === 'boolean' &&
    typeof h['muted'] === 'boolean' &&
    typeof h['deafened'] === 'boolean' &&
    Array.isArray(h['blocked']) &&
    h['blocked'].every((block) => isVocab(voiceControlBlock, block)) &&
    (absent(h['talkMode']) || isVocab(voiceControlTalkMode, h['talkMode'])) &&
    isVocab(voiceControlPTT, h['ptt']) &&
    (absent(h['pttProblem']) || isVocab(voiceControlPTTProblem, h['pttProblem']))
  );
}

// A voice message is exactly a NativeVoiceView: nothing else, and no key it
// does not have, reaches the page.
function isNativeVoiceView(value: unknown): value is NativeVoiceView {
  if (typeof value !== 'object' || value === null || Array.isArray(value)) return false;
  const v = value as Record<string, unknown>;
  return (
    only(v, ['status', 'generation', 'channel', 'problem', 'retryable', 'helper', 'mutedByLock']) &&
    STATUSES.has(v['status']) &&
    isCount(v['generation']) &&
    (v['channel'] === null || (typeof v['channel'] === 'string' && v['channel'].length <= 1024)) &&
    (absent(v['problem']) || (typeof v['problem'] === 'string' && v['problem'].length <= 256)) &&
    typeof v['retryable'] === 'boolean' &&
    (absent(v['helper']) || isHelper(v['helper'])) &&
    (absent(v['mutedByLock']) || v['mutedByLock'] === true)
  );
}

function isEnvelope(value: unknown): value is Envelope {
  if (typeof value !== 'object' || value === null) return false;
  const record = value as Record<string, unknown>;
  return (record['ok'] === true && 'value' in record) || (record['ok'] === false && record['kind'] === 'refused');
}

export function createBridge(
  invoke: Invoke,
  listen: Listen,
  listenVoice: Listen,
  listenNotification: Listen = () => undefined,
  sync: Sync = () => ({ ok: false, kind: 'refused' }),
): Readonly<ExposedDesktopBridge> {
  const call =
    <T>(name: InvokedFunction) =>
    async (...args: unknown[]): Promise<T> => {
      const answer = await invoke(channelOf(name), ...args);
      if (!isEnvelope(answer) || !answer.ok) throw new Error('refused');
      return answer.value as T;
    };

  const feeds = new Map<number, FeedHandlers>();
  let next = 0;

  listen((message) => {
    if (!isFeedEvent(message)) return;
    const handlers = feeds.get(message.id);
    if (handlers === undefined) return;
    switch (message.type) {
      case 'open':
        handlers.onOpen();
        return;
      case 'frames':
        for (const frame of message.frames) {
          // A handler may unsubscribe; the rest of the batch is then not its.
          if (feeds.get(message.id) !== handlers) return;
          try {
            handlers.onFrame(frame);
          } catch (error) {
            // As a WebSocket's onmessage: the next frame is still delivered,
            // and the error still reaches the console.
            queueMicrotask(() => {
              throw error;
            });
          }
        }
        if (feeds.get(message.id) === handlers) void invoke(channelOf('feedAck'), message.id).catch(() => undefined);
        return;
      case 'close':
        feeds.delete(message.id);
        handlers.onClose(message.code, message.reason);
        return;
    }
  });

  // Voice state: the helper's confirmed view, from main, to each listener.
  const voiceListeners = new Set<(view: NativeVoiceView) => void>();
  listenVoice((message) => {
    if (!isNativeVoiceView(message)) return;
    for (const listener of [...voiceListeners]) {
      try {
        listener(message);
      } catch (error) {
        queueMicrotask(() => {
          throw error;
        });
      }
    }
  });
  const onVoiceState = (listener: (view: NativeVoiceView) => void): Unsubscribe => {
    voiceListeners.add(listener);
    return () => {
      voiceListeners.delete(listener);
    };
  };

  // Notification clicks carry only a bounded tag to listeners of this bridge.
  const notificationListeners = new Set<(event: NotificationClick) => void>();
  listenNotification((message) => {
    if (!isNotificationClick(message)) return;
    for (const listener of [...notificationListeners]) {
      try {
        listener(message);
      } catch (error) {
        queueMicrotask(() => {
          throw error;
        });
      }
    }
  });
  const onNotification = (listener: (event: NotificationClick) => void): Unsubscribe => {
    notificationListeners.add(listener);
    return () => {
      notificationListeners.delete(listener);
    };
  };
  const notificationSupported = (): boolean => {
    const answer = sync(channelOf(NOTIFICATION_SUPPORT));
    if (!isEnvelope(answer) || !answer.ok || typeof answer.value !== 'boolean') throw new Error('refused');
    return answer.value;
  };

  const subscribe = (feed: FeedRequest, handlers: FeedHandlers): Unsubscribe => {
    const id = ++next;
    feeds.set(id, handlers);
    const refused = () => {
      if (feeds.get(id) !== handlers) return;
      feeds.delete(id);
      handlers.onClose(REFUSED, 'refused');
    };
    invoke(channelOf('feedSubscribe'), id, feed).then((answer) => {
      if (!isEnvelope(answer) || !answer.ok || answer.value !== true) refused();
    }, refused);
    return () => {
      if (feeds.get(id) !== handlers) return;
      feeds.delete(id);
      void invoke(channelOf('feedUnsubscribe'), id).catch(() => undefined);
    };
  };

  return Object.freeze<ExposedDesktopBridge>({
    serverAddress: call('serverAddress'),
    request: call('request'),
    subscribe,
    restore: call('restore'),
    signInWithCredential: call('signInWithCredential'),
    keepCredential: call('keepCredential'),
    signInWithPassword: call('signInWithPassword'),
    signInWithCode: call('signInWithCode'),
    redeemInvite: call('redeemInvite'),
    redeemInviteAsGuest: call('redeemInviteAsGuest'),
    redeemSignInLink: call('redeemSignInLink'),
    redeemSetupToken: call('redeemSetupToken'),
    resetPassword: call('resetPassword'),
    joinAsGuest: call('joinAsGuest'),
    createAccount: call('createAccount'),
    signOut: call('signOut'),
    replaceCredential: call('replaceCredential'),
    uploadFile: call('uploadFile'),
    downloadFile: call('downloadFile'),
    saveFile: call('saveFile'),
    // Desktop notifications (issue #1590): no native event object crosses.
    notify: call('notify'),
    notifyClose: call('notifyClose'),
    notificationSupported,
    onNotification,
    // The saved server profiles (issue #945).
    profiles: call('profiles'),
    profileAdd: call('profileAdd'),
    profileRename: call('profileRename'),
    profileRemove: call('profileRemove'),
    profileSwitch: call('profileSwitch'),
    storageRetry: call('storageRetry'),
    profilesRecover: call('profilesRecover'),
    // Native voice (issue #1374). No press or release: the helper reads the
    // talk key itself.
    voiceState: call('voiceState'),
    voiceJoin: call('voiceJoin'),
    voiceLeave: call('voiceLeave'),
    voiceRetry: call('voiceRetry'),
    voiceSetMute: call('voiceSetMute'),
    voiceSetDeafen: call('voiceSetDeafen'),
    voiceSetDevices: call('voiceSetDevices'),
    voiceSetTalkMode: call('voiceSetTalkMode'),
    voiceDevices: call('voiceDevices'),
    voiceSaveDevice: call('voiceSaveDevice'),
    voiceKeyDevices: call('voiceKeyDevices'),
    voiceSaveTalkBinding: call('voiceSaveTalkBinding'),
    onVoiceState,
  });
}
