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

// A saved server, as the page may see it: display metadata only. account is
// the name of the principal the profile is bound to (null before the first
// sign-in).
export interface ProfileView {
  readonly id: string;
  readonly label: string;
  readonly host: string;
  readonly origin: string;
  readonly account: string | null;
  readonly active: boolean;
}

// Whether credentials are kept across restarts. session-only: they are not,
// and reason says why; unavailable: the store did not answer (Retry).
export type StorageStatus =
  | { readonly kind: 'protected' }
  | { readonly kind: 'session-only'; readonly reason: 'unavailable' | 'insecure-backend' | 'unknown-backend' | 'unsafe-root' }
  | { readonly kind: 'unavailable' };

// Something the last switch or sign-out has to tell the person.
export type ProfileNotice = 'sign-in-again' | 'could-not-verify' | 'account-mismatch' | 'revocation-unconfirmed' | 'revocation-unsent' | 'teardown-slow';

// Whether the active server's release is one this app was tested with
// (issue #946). none: no server is active. supported: it is on the list.
// unlisted: it is not, and the development flag let it through. unsupported:
// it answered with a version that is not on the list (or none); unreachable:
// it did not answer /healthz. Only supported and unlisted may sign in.
export type CompatState = 'none' | 'supported' | 'unlisted' | 'unsupported' | 'unreachable';

export interface CompatView {
  readonly state: CompatState;
  // The server's version as /healthz named it; null when it named none or
  // something that is not a version.
  readonly server: string | null;
  // This app's own version.
  readonly desktop: string;
  // The development flag is in force: a persistent warning, in any state.
  readonly override: boolean;
  // The first required response family (for example "channels") whose answer
  // was not what this app was built for: an incompatibility, not a network
  // error. null when none has been seen.
  readonly invalid: string | null;
}

export interface ProfilesView {
  readonly profiles: readonly ProfileView[];
  readonly active: string | null;
  readonly storage: StorageStatus;
  // profiles.json could not be read: only profilesRecover works.
  readonly recovery: boolean;
  readonly notice: ProfileNotice | null;
  readonly compat: CompatView;
  // The platform, for the storage text (DPAPI does not protect from other apps
  // of the same user).
  readonly platform: string;
}

export type ProfileAddResult =
  | { readonly ok: true; readonly id: string }
  | { readonly ok: false; readonly refusal: ServerAddressRefusal | 'certificate' | 'not-conch' | 'full' };

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
// incompatible is a sign-in main refused because the server's release is not
// one this app was tested with (issue #946).
export type NoAnswerReason = 'certificate' | 'redirect' | 'oversize' | 'invalid' | 'stale' | 'unsupported' | 'no-server' | 'incompatible';

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

// A file to upload: the bytes (base64, the bridge carries data, not binary
// objects) with the name and media type the page declares. All of it is
// untrusted data: main caps the size, shapes the type and the name, and sends
// the operation itself. The channel is a path value of the table.
export type ChannelName = string;

export interface UploadRequest {
  readonly channel: ChannelName;
  readonly filename: string;
  readonly mediaType: string;
  readonly data: string;
}

// A file's bytes (base64) for a preview, or the server's refusal or no answer.
export type DownloadReply = BridgeReply | { readonly kind: 'bytes'; readonly data: string };

// What saving a file came to. cancelled is the person closing the dialog;
// failed is anything else (no answer, too large, not writable).
export type SaveOutcome = 'saved' | 'cancelled' | 'failed' | 'unauthorized';

export interface ConchDesktopBridge {
  // The server address of the active profile (null: none is active).
  serverAddress(): Promise<ServerAddress>;

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

  // Attachments (issue #947). Upload sends the operation uploadFile; the
  // answer is the server's, validated. Download gives a file's bytes for a
  // blob preview; nothing is written or opened. Save asks main to show the
  // native save dialog (only right after the person's own click or key) and
  // write the file to the path the person chooses there: the page names a
  // file by id and never a path, and gets no bytes back.
  uploadFile(request: UploadRequest): Promise<BridgeReply>;
  downloadFile(id: number): Promise<DownloadReply>;
  saveFile(id: number): Promise<SaveOutcome>;
  // The saved server profiles (issue #945). Ids are opaque to the page.
  profiles(): Promise<ProfilesView>;
  profileAdd(address: string): Promise<ProfileAddResult>;
  profileRename(id: string, label: string): Promise<boolean>;
  profileRemove(id: string): Promise<boolean>;
  // Makes a profile (or null: the server list) the active one. The window's
  // page is replaced; the returned promise may never settle for the page that
  // asked.
  profileSwitch(id: string | null): Promise<boolean>;
  // Re-checks whether credentials can be kept, and keeps those held in memory.
  storageRetry(): Promise<ProfilesView>;
  // Starts an empty list when profiles.json could not be read.
  profilesRecover(): Promise<ProfilesView>;
}

declare global {
  interface Window {
    // Present only in the desktop shell's renderer.
    readonly conchDesktop?: ConchDesktopBridge;
  }
}
