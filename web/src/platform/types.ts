// The platform seam (issue #770, D2-2). Everything the client asks of the place
// it runs in, as one object: how it talks to conchd, how it opens a live
// feed, where it keeps the credential, and the few services (clipboard,
// notifications, external links, the address it is served from) that a
// browser provides and a desktop shell provides differently. The browser
// implements it in ./browser; the desktop shell implements it in ./desktop
// (D2-3 and later). Outside ./browser nothing calls fetch, WebSocket, the
// clipboard or the Notification API, and nothing but browser/credentials.ts
// touches the credential's storage.
import type { ComponentType, ReactNode } from 'react';
import type { AudioSink } from '../sounds';
import type { VoiceControlTalkMode } from '../schema/generated';
import type { Call, OperationId } from './operations';
import type {
  ChannelName,
  CompanionEvent,
  CompanionGrantResult,
  CompanionPageEnd,
  NativeVoiceDevicesResult,
  NativeVoiceView,
  ShareSoundChoice,
  ShareSoundChooseResult,
  ShareSoundStartResult,
  VoiceCommandResult,
  VoiceDiagnostic,
  VoiceDeviceKey,
  VoiceKeyDevicesResult,
  VoiceSaveResult,
  VoiceTalkModeResult,
  VoiceTalkBindingResult,
  VoiceTalkBindingRequest,
} from './desktop/bridge';

// HttpTransport makes the requests of conchd that the operation table names.
// Each takes a call (an operation id with its path values and query) and
// builds the request from the table: the method, the headers and the shape of
// the body are the transport's, never the caller's. A method resolves to the
// response, or rejects when there was no answer at all, as fetch does.
export interface HttpTransport {
  // A GET with the bearer credential.
  get(call: Call, token: string): Promise<Response>;
  // A POST, PUT or DELETE with the bearer credential and, for a JSON body,
  // body. keepalive lets the request outlive the page; signal ends it.
  send(call: Call, token: string, request: { readonly body?: unknown; readonly keepalive: boolean; readonly signal?: AbortSignal }): Promise<Response>;
  // A call with no credential at all: a POST with a JSON body, or with none a
  // GET.
  open(call: Call, body?: unknown): Promise<Response>;
  // A GET with no credential, asking for the health document.
  health(call: Call): Promise<Response>;
  // A file's bytes as the request body, with the declared media type.
  upload(call: Call, token: string, mediaType: string, bytes: Blob, signal: AbortSignal): Promise<Response>;
  // A file's bytes.
  download(call: Call, token: string): Promise<Response>;
}

// The handlers of one live feed, as WebSocket's.
export interface FeedConnection {
  onopen: ((event: Event) => void) | null;
  onmessage: ((event: MessageEvent) => void) | null;
  onclose: ((event: CloseEvent) => void) | null;
  close(): void;
}

// SocketTransport opens the live feeds. When ticket is true the caller asks
// for a single-use ticket before each socket and puts it in query (the
// browser). When it is false the place opens the socket with the credential
// it holds itself, and the caller asks for no ticket (the desktop shell, whose
// main holds the bearer: a renderer never holds a ticket).
export interface SocketTransport {
  readonly ticket: boolean;
  open(id: OperationId, query: Readonly<Record<string, string>>): FeedConnection;
}

export type Place = 'tab' | 'lasting';

// The credential this page uses, and where it was found.
export interface Held {
  readonly token: string;
  readonly place: Place;
}

// CredentialStore keeps the signed-in credential. 'tab' is this tab alone, for
// an operator's and a guest's; 'lasting' is every tab of the origin until it
// is signed out, for a member's. Every method may find its storage refused and
// says so by its result, never by throwing.
export interface CredentialStore {
  // The credential an earlier load left: this tab's own first, then the
  // lasting one.
  read(): Held | null;
  // Whether held is still the credential stored at its place.
  isStored(held: Held): boolean;
  // Stores token at place and returns what is now stored there, which the
  // client pins; null when the storage refused the write. A store may keep a
  // stand-in in token's place (the desktop's keeps no credential at all).
  write(token: string, place: Place): Held | null;
  // Forgets held, but only if it is still the one stored there.
  drop(held: Held): void;
  // Calls back when another tab removes or replaces the lasting credential
  // (or clears the storage), or, given key, changes that preference key.
  // Returns the function that stops listening.
  onLastingChange(callback: () => void, key?: string): () => void;
  // Small, non-secret, per-account preferences kept beside it, in the lasting
  // storage. readPreference gives null when there is none or the storage is
  // refused.
  readPreference(key: string): string | null;
  writePreference(key: string, value: string): void;
}

export type NotificationPermissionState = 'default' | 'denied' | 'granted' | 'unsupported';

// A native notification the client has shown.
export interface NativeNotification {
  onclick: ((event: Event) => void) | null;
  close(): void;
}

// A browser's Web Push subscription as the server's registration takes it
// (CreatePushRegistrationRequestV1): the push service's address and the two
// keys, base64url without padding.
export interface PushSubscriptionTarget {
  readonly endpoint: string;
  readonly p256dh: string;
  readonly auth: string;
}

// Background push through a service worker (issue #1826). Only the browser
// has it; the desktop shell raises its own notifications and has no use for
// one.
export interface PushService {
  // Whether this browser has service workers, push and notifications.
  supported(): boolean;
  // Registers the worker if need be and subscribes with the server's key
  // (base64url), replacing an earlier subscription made with another key.
  // Rejects when the browser refuses.
  subscribe(applicationServerKey: string): Promise<PushSubscriptionTarget>;
  // Whether this browser holds a subscription now.
  subscribed(): Promise<boolean>;
  // Drops the subscription, if any. The worker stays: it does nothing
  // without one.
  unsubscribe(): Promise<void>;
}

export interface Services {
  // Optional interface audio. Unavailable on desktop pending #1924.
  readonly sounds?: AudioSink;
  // Absent where the place has no service workers (the desktop shell).
  push?: PushService;
  // The address the client is served from: the server's own, for the browser.
  origin(): string;
  // Puts text on the clipboard; rejects, or throws, when there is no
  // clipboard here.
  copyText(text: string): Promise<void>;
  notifications: {
    supported(): boolean;
    permission(): NotificationPermissionState;
    request(): Promise<NotificationPermissionState>;
    show(title: string, body: string, tag: string, channel: string): NativeNotification;
  };
  // The attributes of a link that leaves the app.
  externalLink: { readonly target?: '_blank'; readonly rel: string };
  // Saves a stored file to disk where the place does it natively: the desktop
  // shows the system save dialog and writes only where the person chooses.
  // Absent in the browser, which saves through a download link. The page names
  // the file by id and never a path.
  saveFile?: (id: number) => Promise<'saved' | 'cancelled' | 'failed' | 'unauthorized'>;
  // Hands a conch:// invite link to the desktop app installed on this
  // machine, through the browser's own link handling (issue #1592). Only the
  // browser has it: the desktop app is the thing being opened, and has no
  // use for the button. It navigates nothing by itself; a person's click
  // calls it, and a machine with no handler simply does nothing.
  openDesktopApp?(link: string): void;
  // Whether this client may publish the person's own voice. Only the browser
  // may: the desktop renderer never holds the voice grant.
  browserVoice: boolean;
  reportVoiceDiagnostic?: (diagnostic: VoiceDiagnostic) => void;
  // The desktop app's own version, shown beside the server's (issue #946).
  // Only the desktop shell has one; null when it cannot say.
  appVersion?(): Promise<string | null>;
}

// The desktop's screen companion (issue #1581, D4-04 of #772), over the
// bridge of #1579: p<id>.screen, the renderer's own LiveKit participant, which
// publishes the screen and watches others' shares. Main asks conchd for its
// grant only on the person's act and hands over the grant alone.
export interface NativeCompanion {
  // The grant for the voice destination. Call it in the person's click or key
  // handler; a refusal is a typed reason, never a thrown string.
  grant(request: { readonly channel: ChannelName }): Promise<CompanionGrantResult>;
  // Arms the share allowance (issue #1580): call it synchronously at the very
  // start of the Share click, before anything else, so main takes the click
  // for the grant and the capture that follow. One arm lets one grant and one
  // capture follow; a retry needs a new click. Answers whether main armed
  // anything; never rejects.
  shareArm(): Promise<boolean>;
  // Removes the live companion, if there is one, and says whether there was.
  // why is for main's log alone (issue #1992).
  end(why?: CompanionPageEnd): Promise<boolean>;
  // Main's word (the conch:companion channel) that it ended the companion:
  // leave, helper loss, renewal, lock, suspend, sign-out, crash. Returns the
  // function that stops listening.
  onEnd(listener: (event: CompanionEvent) => void): () => void;
}

// The Linux screen share's sound (issue #1854), over the bridge's five
// requests. The page asks and main decides: main records the sharer's consent,
// tells the helper, and says what is true in the voice view (the helper's
// confirmed share_audio, main's choice and reason). The page names no scope, no
// application and no identifier but the opaque share number main gave. Absent
// where the bridge has none; the view then carries no share sound either and
// the page offers no toggle.
export interface NativeShareSound {
  // After the page's share is live: ask main to add the share's sound.
  start(): Promise<ShareSoundStartResult>;
  // The person's click on the toggle: keep the choice and apply it now.
  choose(choice: ShareSoundChoice): Promise<ShareSoundChooseResult>;
  // The share-sound mute: apart from the microphone's mute and deafen.
  mute(muted: boolean): Promise<VoiceCommandResult>;
  // The share ended in the page; names the share main gave.
  end(share: number): Promise<boolean>;
  // The share ended in the page, whether or not it had a sound: main forgets
  // that the capture was a share. Only takes away.
  shareEnded(): Promise<boolean>;
}

// What a refused or failed native-voice call comes to: the bridge's own
// refusal vocabulary, never a thrown string.
export type NativeVoiceError = Extract<VoiceCommandResult, { readonly ok: false }>['error'];

// A call that answers with the new view (join, leave, retry).
export type NativeVoiceOutcome =
  | { readonly ok: true; readonly view: NativeVoiceView }
  | { readonly ok: false; readonly error: NativeVoiceError };

// The native voice of the desktop shell (issue #1375, D3-4 of #771). The
// helper, not the page, owns the microphone and push-to-talk: the store only
// shows the helper's confirmed state and asks main to act. It is a
// useSyncExternalStore source: snapshot() is stable until the view changes.
// Present only where services.browserVoice is false and a native voice
// exists; the browser's platform has none.
export interface NativeVoice {
  readonly snapshot: () => NativeVoiceView;
  readonly subscribe: (listener: () => void) => () => void;
  join(request: { readonly channel: ChannelName; readonly muted?: boolean; readonly deafened?: boolean }): Promise<NativeVoiceOutcome>;
  leave(): Promise<NativeVoiceOutcome>;
  retry(): Promise<NativeVoiceOutcome>;
  setMute(muted: boolean): Promise<VoiceCommandResult>;
  setDeafen(deafened: boolean): Promise<VoiceCommandResult>;
  setDevices(devices: { readonly input?: string; readonly output?: string }): Promise<VoiceCommandResult>;
  setTalkMode(mode: VoiceControlTalkMode, save?: true): Promise<VoiceTalkModeResult>;
  // The helper's one-shot runs (issue #1379, D3-8): the device list, and a
  // choice kept for the next session (`config set`). Neither changes a live
  // session; setDevices and setTalkMode do.
  devices(): Promise<NativeVoiceDevicesResult>;
  saveDevice(request: { readonly key: VoiceDeviceKey; readonly value: string }): Promise<VoiceSaveResult>;
  // The talk-key setup (issue #1382, D3-11). Absent when the bridge has none.
  readonly keyDevices?: () => Promise<VoiceKeyDevicesResult>;
  readonly saveTalkBinding?: (request: VoiceTalkBindingRequest) => Promise<VoiceTalkBindingResult>;
  // The screen companion (issue #1581). Absent when the bridge has none.
  readonly companion?: NativeCompanion;
  // The share's sound (issue #1854). Absent when the bridge has none.
  readonly shareSound?: NativeShareSound;
}

export interface Platform {
  readonly name: 'browser' | 'desktop';
  // What the client is rendered inside, when the place needs a step before the
  // client can run: the desktop shell asks which server to talk to. The
  // browser has none; it is served from its server.
  readonly Shell?: ComponentType<{ readonly children: ReactNode }>;
  readonly http: HttpTransport;
  readonly sockets: SocketTransport;
  readonly credentials: CredentialStore;
  readonly services: Services;
  // Native voice, where the place has one (the desktop shell). Absent in the browser.
  readonly nativeVoice?: NativeVoice;
}
