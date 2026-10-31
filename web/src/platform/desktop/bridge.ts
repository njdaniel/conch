// The type of window.conchDesktop: the narrow surface the desktop preload
// (desktop/preload) exposes to the renderer, and that the desktop adapter in
// this directory calls (issue #770; design docs/design/linux-desktop.md §4).
// Types only. The preload implements it (desktop/preload, D2-3 and D2-4);
// nothing here is code that runs.
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

// Why main had no answer to give (BridgeReply none). certificate is a server
// whose TLS certificate this machine does not trust (never overridden);
// redirect is an answer that tried to send the request elsewhere (refused, and
// the credential was not sent there); oversize and invalid are an answer that
// was too large or not the document the operation promises; stale is an answer
// for an identity that has since ended; unsupported is an operation the
// desktop shell cannot do yet; no-server is a request made before an address
// was set.
export type NoAnswerReason = 'certificate' | 'redirect' | 'oversize' | 'invalid' | 'stale' | 'unsupported' | 'no-server';

// What main answers. A reply is the server's answer with its body already
// validated against the operation's response validator (body is undefined for
// an answer with no document; a refusal's body is its error document). none is
// no answer at all; unauthorized is a 401 that signed this window out.
// retryAfter and requestId are the two response headers the client reads.
export type BridgeReply =
  | { readonly kind: 'reply'; readonly status: number; readonly body?: unknown; readonly retryAfter?: number; readonly requestId?: string }
  | { readonly kind: 'none'; readonly reason?: NoAnswerReason }
  | { readonly kind: 'unauthorized' };

// The server's refusal of a sign-in step, as the client reads it: the status,
// the error document's code and message, and the Retry-After header.
export interface SignInRefusal {
  readonly status: number;
  readonly code?: string;
  readonly message?: string;
  readonly retryAfter?: number;
  readonly requestId?: string;
}

// The sign-in family returns the signed-in account, never a credential.
// answer is the server's own answer with every credential field removed (the
// client puts a stand-in where the token was; nothing in the renderer may hold
// one); who is the account, null when the step signs nobody in (a reset
// password). A refusal carries the server's refusal; challenge is a right
// password for an account that also needs a code: the challenge is a handle
// that only main can redeem, and expiresAt is when it ends.
export type SignInOutcome =
  | { readonly ok: true; readonly who: WhoAmIResponseV1 | null; readonly answer: Readonly<Record<string, unknown>> }
  | ({ readonly ok: false; readonly kind: 'unauthorized' | 'invalid' } & SignInRefusal)
  | { readonly ok: false; readonly kind: 'network'; readonly reason?: NoAnswerReason }
  | { readonly ok: false; readonly kind: 'unkept' }
  | { readonly ok: false; readonly kind: 'challenge'; readonly challenge: string; readonly expiresAt: string };

export type SignOutOutcome = 'ended' | 'unconfirmed' | 'unsent';

// A live feed the renderer may subscribe to. Main owns the socket, the bearer
// it is opened with, its queue and its teardown; it sends frames it has
// validated. The reconnect is the renderer's own, as in the browser: a close
// is reported once, with its code, and the renderer subscribes again.
//
// The parameters each kind takes, and no others (main refuses anything else):
// messages a channel and the cursor after; events a channel and the
// renderer's session; voice a channel; occupancy none.
export type FeedKind = 'messages' | 'events' | 'voice' | 'occupancy';

export interface FeedRequest {
  readonly kind: FeedKind;
  readonly channel?: string;
  readonly session?: number;
  readonly after?: number;
}

// onClose's code is the server's close code, or 1008 when the sign-in the
// feed was opened for has ended, 1013 when main refused the subscription or
// closed it because the page fell behind, 1006 for a socket that failed.
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
  // A credential the person pasted (an operator's or a script's): main checks
  // it with whoami and sets it aside, not yet in force.
  signInWithCredential(credential: string): Promise<SignInOutcome>;
  // Puts in force the credential signInWithCredential set aside, when the page
  // keeps it (for an operator's, once the person passed the warning). false
  // when it is not the one set aside, or it lapsed.
  keepCredential(credential: string): Promise<boolean>;
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
