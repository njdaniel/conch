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
//    (a WhoAmIResponseV1, or a refusal kind) instead. The one credential that
//    does is the screen companion's grant (DesktopCompanionBridge below);
//  - every body is a type of web/src/schema, so none of this is a new wire
//    shape (decision Q1: these in-process messages carry only existing
//    pkg/schema types).
import type {
  VoiceControlBlock,
  VoiceControlCapture,
  VoiceControlErrorCode,
  VoiceControlPhase,
  VoiceControlPlayback,
  VoiceControlPTT,
  VoiceControlPTTProblem,
  VoiceControlReason,
  VoiceControlTalkMode,
  VoiceDevice,
  VoiceDevicesPipeWire,
} from '../../schema/generated';
import type {
  ChangePasswordRequestV1,
  LoginRequestV1,
  ReauthRequestV1,
  RedeemSetupTokenRequestV1,
  ResetPasswordRequestV1,
  SetPasswordRequestV1,
  TOTPLoginRequestV1,
} from '../../schema/auth';
import type { ConvertGuestRequestV1, JoinAccountRequestV1, JoinGuestRequestV1 } from '../../schema/guest';
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
  // The principal whose view this read asks for (issue #1540). Main sends it as
  // Conch-Preview-As on the five previewable reads and refuses it on every
  // other operation, by its own table, not by anything the renderer says.
  readonly previewAs?: number;
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
  | { readonly ok: true; readonly who: WhoAmIResponseV1 | null; readonly answer: Readonly<Record<string, unknown>>; readonly handle?: string }
  | ({ readonly ok: false; readonly kind: 'unauthorized' | 'invalid' } & SignInRefusal)
  | { readonly ok: false; readonly kind: 'network'; readonly reason?: NoAnswerReason }
  | { readonly ok: false; readonly kind: 'unkept' }
  | { readonly ok: false; readonly kind: 'challenge'; readonly challenge: string; readonly expiresAt: string };

// Exactly an operation of the replacing family and the request body of its
// type; main refuses any other key, type or size.
export type ReplaceCredentialRequest =
  | { readonly operation: 'changePassword'; readonly body: ChangePasswordRequestV1 }
  | { readonly operation: 'setPassword'; readonly body: SetPasswordRequestV1 }
  | { readonly operation: 'convertGuest'; readonly body: ConvertGuestRequestV1 }
  | { readonly operation: 'reauth'; readonly body: ReauthRequestV1 };

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

// A desktop notification (issue #1590): exactly the fields main may show or
// retain. The channel is owned by main's activation record; clicks carry only
// the tag back to the document that showed it.
export interface NotifyRequest {
  readonly tag: string;
  readonly title: string;
  readonly body: string;
  readonly channel: ChannelName;
}

// The only notification event: no title, body, channel or native event object.
export interface NotificationClick {
  readonly type: 'click';
  readonly tag: string;
}

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
  // it with whoami and sets it aside, not yet in force, and answers with a
  // one-time handle (outcome.handle) for keepCredential.
  signInWithCredential(credential: string): Promise<SignInOutcome>;
  // Puts in force the credential signInWithCredential set aside, named by the
  // handle it returned, when the page keeps it (for an operator's, once the
  // person passed the warning). The credential is never passed again. false
  // when the handle is not the one issued, was used, or lapsed.
  keepCredential(handle: string): Promise<boolean>;
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
  // The writes that replace the credential they are sent with (issue #1422):
  // change password, first password, a guest's conversion, and a
  // re-authentication. Main sends the operation, rotates its bearer to the
  // token the answer holds, and answers the account with the token left out
  // (the client puts a stand-in in its place). The password and code are sent
  // once and kept nowhere.
  replaceCredential(request: ReplaceCredentialRequest): Promise<SignInOutcome>;

  // Attachments (issue #947). Upload sends the operation uploadFile; the
  // answer is the server's, validated. Download gives a file's bytes for a
  // blob preview; nothing is written or opened. Save asks main to show the
  // native save dialog (only right after the person's own click or key) and
  // write the file to the path the person chooses there: the page names a
  // file by id and never a path, and gets no bytes back.
  uploadFile(request: UploadRequest): Promise<BridgeReply>;
  downloadFile(id: number): Promise<DownloadReply>;
  saveFile(id: number): Promise<SaveOutcome>;
  // Desktop notifications (issue #1590). Support is a narrow synchronous IPC
  // because the platform's permission API is synchronous; clicks carry no text.
  notify(request: NotifyRequest): Promise<boolean>;
  notifyClose(request: { readonly tag: string }): Promise<boolean>;
  notificationSupported(): boolean;
  onNotification(listener: (event: NotificationClick) => void): Unsubscribe;
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
    readonly conchDesktop?: ConchDesktopBridge & Partial<DesktopVoiceBridge> & Partial<DesktopSettingsBridge> & Partial<DesktopCompanionBridge>;
  }
}

// Native voice (issue #1373, D3-2 of #771; linux-desktop.md §5, §9). Its own
// interface, which the preload exposes beside ConchDesktopBridge (issue #1374,
// D3-3; ExposedDesktopBridge below). In-process TypeScript (decision Q1 on #770), not a wire shape.
// No press, release, gate or transmit method exists: push-to-talk is the
// helper's own. No token, path, nonce, pid or stderr is in any type here.
export type NativeVoiceStatus = 'idle' | 'unavailable' | 'starting' | 'running' | 'stopping' | 'ended' | 'failed' | 'lost';

// The helper's confirmed state, from VoiceControlStateV1 or V2: never what
// the page asked for, only what the helper reported.
export interface NativeVoiceHelper {
  readonly version: 1 | 2;
  readonly phase: VoiceControlPhase;
  readonly reason?: VoiceControlReason;
  readonly retryInMs?: number;
  readonly capture: VoiceControlCapture;
  readonly playback?: VoiceControlPlayback;
  readonly transmitting: boolean;
  readonly muted: boolean;
  readonly deafened: boolean;
  readonly blocked: readonly VoiceControlBlock[];
  readonly talkMode?: VoiceControlTalkMode;
  readonly ptt: VoiceControlPTT;
  readonly pttProblem?: VoiceControlPTTProblem;
}

export interface NativeVoiceView {
  readonly status: NativeVoiceStatus;
  readonly generation: number;
  // The destination: set only by voiceJoin, never by browsing.
  readonly channel: ChannelName | null;
  // A supervisor refusal, failure or loss kind, or a helper's reason: a
  // fixed vocabulary, never free text.
  readonly problem?: string;
  readonly retryable: boolean;
  readonly helper?: NativeVoiceHelper;
  // Set while main's mute at a screen lock (issue #1588) is still in force:
  // the strip says "You were muted when your screen locked".
  readonly mutedByLock?: true;
}

export type VoiceCommandResult =
  | { readonly ok: true }
  | { readonly ok: false; readonly error: VoiceControlErrorCode | 'not-running' | 'busy' | 'unsupported' | 'lost' | 'locked' };

// The helper's one-shot runs (issue #1378, D3-7): the device list and a save.
// Why a run did not give what was asked; never text of the helper's.
export type VoiceOneShotError =
  | 'not-signed-in'
  | 'not-installed'
  | 'untrusted'
  | 'busy'
  | 'invalid'
  | 'spawn-failed'
  | 'timed-out'
  | 'too-much-output'
  | 'unsupported'
  | 'failed'
  | 'bad-output'
  | 'cancelled';

// The device list as the page gets it: the validated document's microphones
// and speakers (each with its default flag), and whether PipeWire was
// reached, and the names saved in voice.toml (issue #1421; absent when none is
// saved, 'default' being said by absence). No path, nothing else of the
// configuration.
export type NativeVoiceDevicesResult =
  | {
      readonly ok: true;
      readonly pipewire: VoiceDevicesPipeWire;
      readonly synced: boolean;
      readonly microphones: readonly VoiceDevice[];
      readonly speakers: readonly VoiceDevice[];
      readonly configuredInput?: string;
      readonly configuredOutput?: string;
    }
  | { readonly ok: false; readonly error: VoiceOneShotError };

// What `config set` saves from the page. A device is a name from the list or
// 'default'; the talk mode a word of its vocabulary.
export type VoiceDeviceKey = 'audio.input' | 'audio.output' | 'audio.talk_mode';

export type VoiceSaveResult = { readonly ok: true } | { readonly ok: false; readonly error: VoiceOneShotError };

// The keys main also saves, but only from the talk-key setup below: the page
// never names them (issue #1381, D3-10). keys.mute and keys.deafen are not
// set from here.
export type VoiceBindingKey = 'keys.device' | 'keys.talk';

// A device the person could bind a talk key on, listed by name from
// /dev/input/by-id without opening anything. id is opaque and valid only for
// the listing it came in. rule is present only for a device this user cannot
// read: the exact udev rule the person would install themselves.
export interface VoiceKeyDeviceRule {
  // The file the line goes in, as root, by the person.
  readonly file: string;
  // The one rule line.
  readonly line: string;
  // What to do after: reload, then unplug and replug the device.
  readonly reload: readonly string[];
}

export interface VoiceKeyDevice {
  readonly id: string;
  readonly name: string;
  readonly kind: 'keyboard' | 'mouse';
  readonly readable: boolean;
  readonly rule?: VoiceKeyDeviceRule;
}

export type VoiceKeyDevicesResult =
  | {
      readonly ok: true;
      readonly devices: readonly VoiceKeyDevice[];
      // What a read grant means, to be shown before the rule is.
      readonly disclosure: readonly string[];
    }
  | { readonly ok: false; readonly error: VoiceOneShotError };

// Saves keys.device then keys.talk. failed says which of the two did not save.
export type VoiceTalkBindingResult =
  | { readonly ok: true }
  | { readonly ok: false; readonly error: VoiceOneShotError; readonly failed?: 'device' | 'talk' };

export interface DesktopVoiceBridge {
  voiceState(): Promise<NativeVoiceView>;
  voiceJoin(request: { readonly channel: ChannelName; readonly muted: boolean; readonly deafened: boolean }): Promise<NativeVoiceView>;
  voiceLeave(): Promise<NativeVoiceView>;
  voiceRetry(): Promise<NativeVoiceView>;
  voiceSetMute(muted: boolean): Promise<VoiceCommandResult>;
  voiceSetDeafen(deafened: boolean): Promise<VoiceCommandResult>;
  voiceSetDevices(devices: { readonly input?: string; readonly output?: string }): Promise<VoiceCommandResult>;
  voiceSetTalkMode(mode: VoiceControlTalkMode): Promise<VoiceCommandResult>;
  voiceDevices(): Promise<NativeVoiceDevicesResult>;
  voiceSaveDevice(request: { readonly key: VoiceDeviceKey; readonly value: string }): Promise<VoiceSaveResult>;
  voiceKeyDevices(): Promise<VoiceKeyDevicesResult>;
  voiceSaveTalkBinding(request: { readonly deviceId: string; readonly code: number }): Promise<VoiceTalkBindingResult>;
  onVoiceState(listener: (view: NativeVoiceView) => void): Unsubscribe;
}

// The app's own settings (issue #1587; linux-desktop.md §9). App-wide, kept by
// main in <userData>/settings.json, not per account. In-process TypeScript, not
// a wire shape.
export interface DesktopSettingsView {
  // Closing the window keeps the app running, in the tray. Takes effect only
  // while trayWorking.
  readonly closeToTray: boolean;
  // Open the app when the person logs in.
  readonly launchAtLogin: boolean;
  // Whether a working tray exists right now.
  readonly trayWorking: boolean;
  // Whether this build may set launch at login (an installed build may).
  readonly launchAvailable: boolean;
  // Why not, when it is not: an unpackaged run, or an app that sits where
  // others could change it.
  readonly launchUnavailable?: 'unpackaged' | 'unsafe-location';
}

export interface SettingsChange {
  readonly closeToTray?: boolean;
  readonly launchAtLogin?: boolean;
}

export type SettingsResult =
  | { readonly ok: true; readonly view: DesktopSettingsView }
  | { readonly ok: false; readonly reason: 'unpackaged' | 'unsafe-location' | 'foreign-file' | 'failed' };

export interface DesktopSettingsBridge {
  settings(): Promise<DesktopSettingsView>;
  // The person's act: main refuses it without a recent gesture in the window.
  settingsSet(change: SettingsChange): Promise<SettingsResult>;
}


// The screen companion (issue #1579, D4-02 of #772; linux-desktop.md §4,
// §6.4): the renderer's own LiveKit participant, p<id>.screen, which publishes
// the screen and watches others' shares (B2). Main asks conchd for its grant
// only on the person's act, only for the voice destination while the helper
// is connected there, and hands over the grant alone: never the bearer, the
// room's name or the person's own voice grant. The token lives 15 s, and
// every grant needs a new act (Q-D4-2 on #1578: a share is not resumed after a
// room renewal). In-process TypeScript, as the voice family (decision Q1 on
// #770), not a wire shape.
export interface CompanionGrant {
  // The LiveKit server to connect to, as conchd names it.
  readonly livekitUrl: string;
  // The companion's join token: the one credential the renderer holds.
  readonly token: string;
  // The companion's identity, p<id>.screen.
  readonly identity: string;
  // When the token stops being accepted for a join (RFC 3339).
  readonly expiresAt: string;
}

// Why no grant was given. not-in-voice: voice is not connected in that
// channel (browsing another channel never gets one). not-yet: conchd has not
// yet seen the person's voice in the room; try again shortly. elsewhere: a
// companion of this person is already live, here or on another device.
// ended: a Leave, a lock or a switch overtook the request. failed: anything
// else, said no more precisely.
export type CompanionGrantError = 'not-in-voice' | 'not-yet' | 'elsewhere' | 'ended' | 'failed';

export type CompanionGrantResult = { readonly ok: true; readonly grant: CompanionGrant } | { readonly ok: false; readonly error: CompanionGrantError };

// Why main ended the live companion on its own. leave: the person left
// voice; helper: the helper stopped, failed or lost its connection; renewal:
// conchd renewed the voice room (the page says "The voice room was renewed.
// Share again to carry on"); sign-out, switch, quit: the identity is ending;
// crash and reload: the document that held the grant has gone; lock and
// suspend: the session was locked or is going to sleep (#1588, D5-02).
export type CompanionEndReason = 'leave' | 'helper' | 'renewal' | 'sign-out' | 'switch' | 'quit' | 'crash' | 'reload' | 'lock' | 'suspend';

// What main tells the page on the companion channel: always before it asks
// conchd to remove the companion, so the page disconnects its room first.
export interface CompanionEvent {
  readonly type: 'end';
  readonly reason: CompanionEndReason;
}

export interface DesktopCompanionBridge {
  // The companion's grant for the voice destination. Only right after the
  // person's own click or key.
  companionGrant(request: { readonly channel: ChannelName }): Promise<CompanionGrantResult>;
  // Removes the live companion, if there is one, and says whether there was.
  companionEnd(): Promise<boolean>;
  onCompanion(listener: (event: CompanionEvent) => void): Unsubscribe;
}

// The object the preload exposes: the bridge, the voice family, the settings
// and the companion. Still no press, release, gate or transmit function in any.
export type ExposedDesktopBridge = ConchDesktopBridge & DesktopVoiceBridge & DesktopSettingsBridge & DesktopCompanionBridge;
