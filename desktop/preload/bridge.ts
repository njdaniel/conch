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
// subscribe numbers each feed itself (so main's first event for it can never
// arrive before the number is known here), asks main to open it, hands the
// page each frame of a batch in order and then acknowledges the batch, which
// is what lets main send the next. An unsubscribed feed is forgotten here at
// once: whatever main had already sent for it is dropped.
import type { ConchDesktopBridge, FeedHandlers, FeedRequest, Unsubscribe } from '../../web/src/platform/desktop/bridge.ts';
import { channelOf, type Envelope, type FeedEvent, type InvokedFunction } from '../shared/bridge-names.ts';

export type Invoke = (channel: string, ...args: unknown[]) => Promise<unknown>;

// Listen calls back with each message main sends on the feed channel; only
// the message, never the event it came in.
export type Listen = (callback: (message: unknown) => void) => void;

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

function isEnvelope(value: unknown): value is Envelope {
  if (typeof value !== 'object' || value === null) return false;
  const record = value as Record<string, unknown>;
  return (record['ok'] === true && 'value' in record) || (record['ok'] === false && record['kind'] === 'refused');
}

export function createBridge(invoke: Invoke, listen: Listen): Readonly<ConchDesktopBridge> {
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

  return Object.freeze<ConchDesktopBridge>({
    serverAddress: call('serverAddress'),
    setServerAddress: call('setServerAddress'),
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
    uploadFile: call('uploadFile'),
    downloadFile: call('downloadFile'),
    saveFile: call('saveFile'),
  });
}
