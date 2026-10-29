// The names of the bridge's functions, shared by the preload (which exposes
// them) and main (which answers them), so that neither can grow a function the
// other does not know. The type of each is web/src/platform/desktop/bridge.ts.
//
// A channel is derived from the name here; the renderer never names one. The
// bridge's subscribe has no channel yet: it is the live feeds of #943, and
// until then the preload answers it locally.
import type { ConchDesktopBridge } from '../../web/src/platform/desktop/bridge.ts';

export const BRIDGE_FUNCTIONS = [
  'serverAddress',
  'setServerAddress',
  'request',
  'subscribe',
  'restore',
  'signInWithCredential',
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

export type BridgeFunction = (typeof BRIDGE_FUNCTIONS)[number];

// The functions that cross into main as one invoke each.
export type InvokedFunction = Exclude<BridgeFunction, 'subscribe'>;

export const INVOKED_FUNCTIONS: readonly InvokedFunction[] = BRIDGE_FUNCTIONS.filter((name): name is InvokedFunction => name !== 'subscribe');

export function channelOf(name: InvokedFunction): string {
  return `conch:${name}`;
}

// What main answers every invoke with. A refusal is one opaque kind: it says
// nothing about which check failed.
export type Envelope = { readonly ok: true; readonly value: unknown } | { readonly ok: false; readonly kind: 'refused' };
