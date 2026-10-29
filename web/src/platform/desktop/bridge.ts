// The type of window.conchDesktop: the narrow surface the desktop preload
// (desktop/preload) exposes to the renderer, and that the desktop adapter in
// this directory calls (issue #770; design docs/design/linux-desktop.md §4).
// Types only. The preload implements it in D2-3 and D2-4; nothing here is
// code that runs.
//
// The rules the type holds to:
//  - named functions only: no channel argument, no event object, no Electron
//    type, no `any`, no function that takes a URL, a header map or a method;
//  - a request is an operation id of the table (../operations) with its path
//    values, query and body, which main maps to a fixed public route and
//    validates with the table's validators;
//  - the bearer credential, a socket ticket, a proof and the person's own
//    voice grant never cross it: main keeps them, and answers sanitised state
//    (a WhoAmIResponseV1, or a refusal kind) instead;
//  - every body is a type of web/src/schema, so none of this is a new wire
//    shape (decision Q1: these in-process messages carry only existing
//    pkg/schema types).
import type { LoginRequestV1, RedeemSetupTokenRequestV1, ResetPasswordRequestV1, TOTPLoginRequestV1 } from '../../schema/auth';
import type { JoinAccountRequestV1, JoinGuestRequestV1 } from '../../schema/guest';
import type { RedeemInviteGuestRequestV1, RedeemInviteRequestV2 } from '../../schema/invite';
import type { WhoAmIResponseV1 } from '../../schema/principal';
import type { RedeemSignInLinkRequestV1 } from '../../schema/signin';
import type { OperationId, Params } from '../operations';

// The server this window talks to: the canonical origin of the selected
// profile, or none until the person has entered one.
export interface ServerAddress {
  readonly origin: string | null;
}

// Why an address was not accepted. Only a root-mounted https origin (or plain
// http to loopback, for development) is; the rest are refused by name.
export type ServerAddressRefusal = 'malformed' | 'scheme' | 'userinfo' | 'path' | 'unreachable' | 'unsupported';

export type SetServerAddressResult =
  | { readonly ok: true; readonly origin: string }
  | { readonly ok: false; readonly refusal: ServerAddressRefusal };

// One call of the operation table, as the renderer asks for it.
export interface BridgeRequest {
  readonly operation: OperationId;
  readonly params?: Params;
  // Already encoded, without a leading "?", as Call.query.
  readonly query?: string;
  readonly body?: unknown;
  // Lets the request outlive the page, as fetch's keepalive.
  readonly keepalive?: boolean;
}

// What main answers. A reply is the server's answer with its body already
// validated against the operation's response validator (body is undefined for
// an answer with no document); none is no answer at all; unauthorized is a 401
// that signed this window out.
export type BridgeReply =
  | { readonly kind: 'reply'; readonly status: number; readonly body?: unknown }
  | { readonly kind: 'none' }
  | { readonly kind: 'unauthorized' };

// The sign-in family returns the signed-in account, never a credential. A
// refusal is a kind; challenge is a right password for an account that also
// needs a code, and expiresAt is when that challenge ends.
export type SignInOutcome =
  | { readonly ok: true; readonly who: WhoAmIResponseV1 }
  | { readonly ok: false; readonly kind: 'network' | 'unauthorized' | 'invalid' | 'unkept'; readonly code?: string }
  | { readonly ok: false; readonly kind: 'challenge'; readonly expiresAt: string };

export type SignOutOutcome = 'ended' | 'unconfirmed' | 'unsent';

// A live feed the renderer may subscribe to. Main owns the socket, the ticket
// and the reconnect; it sends validated frames.
export type FeedKind = 'messages' | 'events' | 'voice' | 'occupancy';

export interface FeedRequest {
  readonly kind: FeedKind;
  readonly channel?: string;
  readonly session?: number;
  readonly after?: number;
}

export interface FeedHandlers {
  readonly onOpen: () => void;
  readonly onFrame: (frame: string) => void;
  readonly onClose: (code: number, reason: string) => void;
}

export type Unsubscribe = () => void;

export interface ConchDesktopBridge {
  // The server address.
  serverAddress(): Promise<ServerAddress>;
  setServerAddress(origin: string): Promise<SetServerAddressResult>;

  // Any operation of the table that the renderer may invoke. Main refuses one
  // whose entry is marked renderer: false (a credential-bearing answer, a
  // socket ticket, the person's own voice grant).
  request(request: BridgeRequest): Promise<BridgeReply>;

  // Feeds: subscribe, and call the returned function to unsubscribe.
  subscribe(feed: FeedRequest, handlers: FeedHandlers): Unsubscribe;

  // Sign in, and the other operations that give a credential. The secret is
  // accepted for its operation alone; main consumes the token the answer holds.
  restore(): Promise<SignInOutcome>;
  signInWithPassword(request: LoginRequestV1): Promise<SignInOutcome>;
  signInWithCode(request: TOTPLoginRequestV1): Promise<SignInOutcome>;
  redeemInvite(request: RedeemInviteRequestV2): Promise<SignInOutcome>;
  redeemInviteAsGuest(request: RedeemInviteGuestRequestV1): Promise<SignInOutcome>;
  redeemSignInLink(request: RedeemSignInLinkRequestV1): Promise<SignInOutcome>;
  redeemSetupToken(request: RedeemSetupTokenRequestV1): Promise<SignInOutcome>;
  resetPassword(request: ResetPasswordRequestV1): Promise<SignInOutcome>;
  joinAsGuest(request: JoinGuestRequestV1): Promise<SignInOutcome>;
  createAccount(request: JoinAccountRequestV1): Promise<SignInOutcome>;
  signOut(): Promise<SignOutOutcome>;
}

declare global {
  interface Window {
    // Present only in the desktop shell's renderer.
    readonly conchDesktop?: ConchDesktopBridge;
  }
}
