// The names of the bridge's functions, shared by the preload (which exposes
// them) and main (which answers them), so that neither can grow a function the
// other does not know. The type of each is web/src/platform/desktop/bridge.ts.
//
// A channel is derived from the name here; the renderer never names one. The
// bridge's subscribe is not one invoke: the preload carries it as the three
// feed calls below, and main answers with feed events on the one channel
// FEED_EVENTS (issue #943).
import type { ConchDesktopBridge } from '../../web/src/platform/desktop/bridge.ts';

export const BRIDGE_FUNCTIONS = [
  'serverAddress',
  'request',
  'subscribe',
  'restore',
  'signInWithCredential',
  'keepCredential',
  'signInWithPassword',
  'signInWithCode',
  'redeemInvite',
  'redeemInviteAsGuest',
  'redeemSignInLink',
  'redeemSetupToken',
  'resetPassword',
  'joinAsGuest',
  'createAccount',
  'signOut',
] as const satisfies readonly (keyof ConchDesktopBridge)[];

// The saved server profiles (issue #945), a family of their own.
export const PROFILE_FUNCTIONS = [
  'profiles',
  'profileAdd',
  'profileRename',
  'profileRemove',
  'profileSwitch',
  'storageRetry',
  'profilesRecover',
] as const satisfies readonly (keyof ConchDesktopBridge)[];

export type ProfileFunction = (typeof PROFILE_FUNCTIONS)[number];

export type BridgeFunction = (typeof BRIDGE_FUNCTIONS)[number];

// The calls a subscription is made of: open a feed under a number the preload
// chose, acknowledge a batch of its frames, and close it.
export const FEED_CALLS = ['feedSubscribe', 'feedAck', 'feedUnsubscribe'] as const;

export type FeedCall = (typeof FEED_CALLS)[number];

// The functions that cross into main as one invoke each.
export type InvokedFunction = Exclude<BridgeFunction, 'subscribe'> | FeedCall | ProfileFunction;

export const INVOKED_FUNCTIONS: readonly InvokedFunction[] = [
  ...BRIDGE_FUNCTIONS.filter((name): name is Exclude<BridgeFunction, 'subscribe'> => name !== 'subscribe'),
  ...FEED_CALLS,
  ...PROFILE_FUNCTIONS,
];

export function channelOf(name: InvokedFunction): string {
  return `conch:${name}`;
}

// What main answers every invoke with. A refusal is one opaque kind: it says
// nothing about which check failed.
export type Envelope = { readonly ok: true; readonly value: unknown } | { readonly ok: false; readonly kind: 'refused' };

// The one channel main sends on: the events of the renderer's feeds. Main
// sends it to the owned web contents only, for the document that subscribed.
export const FEED_EVENTS = 'conch:feed';

// A feed event. id is the number the preload chose at subscribe. frames is a
// batch of text frames that main has validated, in the order they came;
// main sends the next batch only when the preload has acknowledged this one.
// close is the end of the feed, whoever ended it: the server's close code and
// reason, 1008 when the sign-in it was opened for has ended, 1013 when the
// renderer fell too far behind. These carry nothing but frames of
// pkg/schema types, as text, and the codes a WebSocket's close event has.
export type FeedEvent =
  | { readonly id: number; readonly type: 'open' }
  | { readonly id: number; readonly type: 'frames'; readonly frames: readonly string[] }
  | { readonly id: number; readonly type: 'close'; readonly code: number; readonly reason: string };
