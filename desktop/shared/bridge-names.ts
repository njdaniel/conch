// The names of the bridge's functions, shared by the preload (which exposes
// them) and main (which answers them), so that neither can grow a function the
// other does not know. The type of each is web/src/platform/desktop/bridge.ts.
//
// A channel is derived from the name here; the renderer never names one. The
// bridge's subscribe is not one invoke: the preload carries it as the three
// feed calls below, and main answers with feed events on the one channel
// FEED_EVENTS (issue #943).
import type { ConchDesktopBridge, DesktopCompanionBridge, DesktopSettingsBridge, DesktopUpdateBridge, DesktopVoiceBridge } from '../../web/src/platform/desktop/bridge.ts';

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
  'replaceCredential',
  'uploadFile',
  'downloadFile',
  'saveFile',
] as const satisfies readonly (keyof ConchDesktopBridge)[];

// Desktop notifications (issue #1590): fixed requests, plus the synchronous
// support check and the click event exposed separately by the preload.
export const NOTIFY_FUNCTIONS = ['notify', 'notifyClose'] as const satisfies readonly (keyof ConchDesktopBridge)[];
export const NOTIFICATION_SUPPORT = 'notificationSupported' as const;
export const NOTIFICATION_EVENTS = 'conch:notification';
// The longest notification tag. The page builds it as origin:account:messageId
// (web/src/useNotifications.ts); an origin can be 8 + 253 + 6 characters
// (scheme, DNS name, port), so the earlier 128 would refuse a long origin.
export const TAG_MAX = 512;

export type NotifyFunction = (typeof NOTIFY_FUNCTIONS)[number];
// Main's tag-only click message to the notification's owning document.
export type NotificationEvent = { readonly type: 'click'; readonly tag: string };

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

// Native voice (issue #1374, D3-3 of #771): the invoked functions of
// DesktopVoiceBridge (onVoiceState is its event, not an invoke). There is no
// press, release, gate or transmit function: the talk key never crosses IPC,
// the helper reads it natively (linux-desktop.md §4).
export const VOICE_FUNCTIONS = [
  'voiceState',
  'voiceJoin',
  'voiceLeave',
  'voiceRetry',
  'voiceSetMute',
  'voiceSetDeafen',
  'voiceSetDevices',
  'voiceSetTalkMode',
  'voiceDevices',
  'voiceSaveDevice',
  'voiceKeyDevices',
  'voiceSaveTalkBinding',
] as const satisfies readonly (keyof DesktopVoiceBridge)[];

export type VoiceFunction = (typeof VOICE_FUNCTIONS)[number];

// The app's own settings (issue #1587): read them, change them.
export const SETTINGS_FUNCTIONS = ['settings', 'settingsSet'] as const satisfies readonly (keyof DesktopSettingsBridge)[];

export type SettingsFunction = (typeof SETTINGS_FUNCTIONS)[number];
// The screen companion (issue #1579, D4-02 of #772): the invoked functions of
// DesktopCompanionBridge (onCompanion is its event, COMPANION_EVENTS).
export const COMPANION_FUNCTIONS = ['companionGrant', 'companionEnd'] as const satisfies readonly (keyof DesktopCompanionBridge)[];

export type CompanionFunction = (typeof COMPANION_FUNCTIONS)[number];

// The update family (issue #1600, D6-06): the installed files replaced under
// the running app. onUpdateState is its event, not an invoke.
export const UPDATE_FUNCTIONS = ['updateState', 'updateRestart'] as const satisfies readonly (keyof DesktopUpdateBridge)[];

export type UpdateFunction = (typeof UPDATE_FUNCTIONS)[number];

export type BridgeFunction = (typeof BRIDGE_FUNCTIONS)[number];

// The calls a subscription is made of: open a feed under a number the preload
// chose, acknowledge a batch of its frames, and close it.
export const FEED_CALLS = ['feedSubscribe', 'feedAck', 'feedUnsubscribe'] as const;

export type FeedCall = (typeof FEED_CALLS)[number];

// The functions that cross into main as one invoke each.
export type InvokedFunction = Exclude<BridgeFunction, 'subscribe'> | FeedCall | ProfileFunction | VoiceFunction | NotifyFunction | SettingsFunction | CompanionFunction | UpdateFunction;

export const INVOKED_FUNCTIONS: readonly InvokedFunction[] = [
  ...BRIDGE_FUNCTIONS.filter((name): name is Exclude<BridgeFunction, 'subscribe'> => name !== 'subscribe'),
  ...FEED_CALLS,
  ...PROFILE_FUNCTIONS,
  ...VOICE_FUNCTIONS,
  ...NOTIFY_FUNCTIONS,
  ...SETTINGS_FUNCTIONS,
  ...COMPANION_FUNCTIONS,
  ...UPDATE_FUNCTIONS,
];

export function channelOf(name: InvokedFunction | typeof NOTIFICATION_SUPPORT): string {
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

// The second channel main sends on: the helper's confirmed voice state, a
// NativeVoiceView as the message. Main sends it to the owned web contents
// only, while its current document is committed.
export const VOICE_EVENTS = 'conch:voice';

// The third: the companion's end (a CompanionEvent), told to the owned web
// contents only, and only to the document that was given the grant, before
// main asks conchd to remove the companion.
export const COMPANION_EVENTS = 'conch:companion';

// Another channel main sends on: the installed state, an InstalledView as
// the message, when it changes. Same targets and gate as VOICE_EVENTS.
export const UPDATE_EVENTS = 'conch:update';
