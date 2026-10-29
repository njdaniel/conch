// The bridge object the preload exposes, built from one function that makes an
// invoke. Input: that function (Electron's ipcRenderer.invoke in the real
// preload, a fake in tests). Output: a frozen object with exactly the
// functions of web/src/platform/desktop/bridge.ts.
//
// Each function sends its one argument on a channel derived from its own name
// and unwraps main's envelope. A refusal rejects with a plain Error that says
// "refused" and nothing more. No function takes a channel, a URL, a header map
// or a method, and nothing here holds an ipcRenderer, an event or a credential.
import type { ConchDesktopBridge, FeedHandlers, FeedRequest, Unsubscribe } from '../../web/src/platform/desktop/bridge.ts';
import { channelOf, type Envelope, type InvokedFunction } from '../shared/bridge-names.ts';

export type Invoke = (channel: string, ...args: unknown[]) => Promise<unknown>;

function isEnvelope(value: unknown): value is Envelope {
  if (typeof value !== 'object' || value === null) return false;
  const record = value as Record<string, unknown>;
  return (record['ok'] === true && 'value' in record) || (record['ok'] === false && record['kind'] === 'refused');
}

export function createBridge(invoke: Invoke): Readonly<ConchDesktopBridge> {
  const call =
    <T>(name: InvokedFunction) =>
    async (...args: unknown[]): Promise<T> => {
      const answer = await invoke(channelOf(name), ...args);
      if (!isEnvelope(answer) || !answer.ok) throw new Error('refused');
      return answer.value as T;
    };

  // Live feeds are #943. Until then a subscription ends at once, so that a
  // caller sees a closed feed rather than silence.
  const subscribe = (_feed: FeedRequest, handlers: FeedHandlers): Unsubscribe => {
    let live = true;
    queueMicrotask(() => {
      if (live) handlers.onClose(1013, 'feeds are not available yet');
    });
    return () => {
      live = false;
    };
  };

  return Object.freeze<ConchDesktopBridge>({
    serverAddress: call('serverAddress'),
    setServerAddress: call('setServerAddress'),
    request: call('request'),
    subscribe,
    restore: call('restore'),
    signInWithCredential: call('signInWithCredential'),
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
  });
}
