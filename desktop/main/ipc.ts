// The one registry of IPC handlers, and the check every invocation passes
// before anything else (linux-desktop.md §4: "On every invocation and
// subscription, main checks event.sender against the owned webContents,
// senderFrame against its live main frame, the exact local origin and the
// expected document generation, plus schema, size and rate limits").
//
// Input: an invoke event from Electron, a function name and its arguments.
// Output: an Envelope: the function's value, or one opaque `refused`, which is
// counted and logged by reason (never by content) and executes nothing. The
// order is fixed: caller, name, rate, size and shape of the arguments, then
// the function. Arguments are plain data checked here; the renderer cannot
// name a channel, a URL, a header or a method.
//
// One channel sits outside that registry: the synchronous `conch:notificationSupported`
// (registerNotificationSupport), which Electron's ipcMain.on answers through
// event.returnValue. notificationSupported() runs the same caller, rate and
// shape checks and returns an Envelope too.
import type { IpcMain } from 'electron';

import type { BridgeReply, BridgeRequest, FeedRequest, NotifyRequest, SettingsChange, SignInOutcome, VoiceDeviceKey } from '../../web/src/platform/desktop/bridge.ts';
import { VOICE_DEVICES_MAX_NAME_BYTES, voiceControlTalkMode, type VoiceControlTalkMode } from '../../web/src/schema/generated.ts';
import { isPageSaveKey, keyCodeText, saveValueFits } from './voice/oneshot.ts';
import { validChannel, validDeviceName } from './voice/protocol.ts';
import { operations } from '../../web/src/platform/operations.ts';
import { channelOf, INVOKED_FUNCTIONS, NOTIFICATION_SUPPORT, TAG_MAX, type Envelope, type InvokedFunction } from '../shared/bridge-names.ts';
import { isProfileId } from './profiles/ids.ts';
import { MISMATCH, type ProfilesService } from './profiles/service.ts';
import { parseFeedRequest, type Feeds } from './api/feeds.ts';
import { targetOf, isOperationId } from './api/target.ts';
import { previewable } from './api/target.ts';
import { invocable, type ReplaceStep, type Service, type SignInStep } from './api/service.ts';
import { isFileId, parseUpload, MAX_UPLOAD_BYTES, type Files } from './files.ts';
import { LINK_GESTURE_MS, type Gestures } from './gestures.ts';
import type { DocumentGate } from './document.ts';
import type { Notifications } from './notifications.ts';
import { HOST, SCHEME } from './scheme.ts';
import type { Installed } from './installed.ts';
import type { VoiceService } from './voice/service.ts';
import type { SettingsService } from './settings.ts';
import type { Companion } from './voice/companion.ts';

export interface FrameLike {
  readonly frameToken: string;
  readonly parent: unknown;
  readonly detached: boolean;
  readonly url: string;
}

export interface ContentsLike {
  isDestroyed(): boolean;
  readonly mainFrame: FrameLike;
}

export interface InvokeEvent {
  readonly sender: ContentsLike;
  readonly senderFrame: FrameLike | null;
}

export type Refusal = 'sender' | 'destroyed' | 'frame' | 'subframe' | 'origin' | 'generation' | 'name' | 'rate' | 'size' | 'shape' | 'gesture' | 'error';

export interface IpcDeps {
  // The one web contents that may call main.
  readonly owned: () => ContentsLike | null;
  readonly gate: DocumentGate;
  readonly service: Service;
  readonly feeds: Pick<Feeds, 'subscribe' | 'ack' | 'unsubscribe'>;
  readonly files: Pick<Files, 'upload' | 'download' | 'save'>;
  // The person's own input to the owned web contents (main/gestures.ts).
  readonly gestures: Pick<Gestures, 'take'>;
  // The saved server profiles (issue #945).
  readonly profiles: ProfilesApi;
  // Native voice (issue #1374): main's one owner of the helper.
  readonly voice: VoiceApi;
  // The installed files replaced under the running app (issue #1600).
  readonly installed: InstalledApi;
  // Desktop notifications (issue #1590): owned by the active activation.
  readonly notifications: NotificationsApi;
  // The app's own settings (issue #1587).
  readonly settings: SettingsApi;
  // The screen companion (issue #1579). Absent: every grant is refused as
  // failed, and nothing is held to end.
  readonly companion?: CompanionApi;
  readonly now: () => number;
  readonly log: (line: string) => void;
}

export type ProfilesApi = Pick<
  ProfilesService,
  'view' | 'add' | 'rename' | 'remove' | 'switchTo' | 'retry' | 'recover' | 'signOut' | 'signedIn' | 'keptCredential' | 'signInPermitted' | 'active'
>;

export type InstalledApi = Pick<Installed, 'view' | 'restart'>;

export type VoiceApi = Pick<VoiceService, 'state' | 'join' | 'leave' | 'retry' | 'setMute' | 'setDeafen' | 'setDevices' | 'setTalkMode' | 'devices' | 'saveDevice' | 'keyDevices' | 'saveTalkBinding'>;
export type NotificationsApi = Pick<Notifications, 'show' | 'close' | 'supported'>;

export type SettingsApi = Pick<SettingsService, 'view' | 'set'>;
export type CompanionApi = Pick<Companion, 'grant' | 'stop' | 'end'>;

const NO_COMPANION: CompanionApi = {
  grant: () => Promise.resolve({ ok: false, error: 'failed' }),
  stop: () => Promise.resolve(false),
  end: () => Promise.resolve(false),
};

// What a sign-in step answers when the server's release is not one the app may
// sign in to (issue #946): nothing was sent.
const INCOMPATIBLE: SignInOutcome = { ok: false, kind: 'network', reason: 'incompatible' };
const INCOMPATIBLE_REPLY: BridgeReply = { kind: 'none', reason: 'incompatible' };

const ORIGIN_PREFIX = `${SCHEME}://${HOST}/`;

// caller says why an invocation is not from the one document of the one
// window, or null when it is.
export function checkCaller(event: InvokeEvent, owned: ContentsLike | null, gate: DocumentGate): Refusal | null {
  if (owned === null || event.sender !== owned) return 'sender';
  if (event.sender.isDestroyed()) return 'destroyed';
  const frame = event.senderFrame;
  if (frame === null || frame.detached) return 'frame';
  if (frame.parent !== null) return 'subframe';
  if (frame !== event.sender.mainFrame && frame.frameToken !== event.sender.mainFrame.frameToken) return 'frame';
  if (!frame.url.startsWith(ORIGIN_PREFIX)) return 'origin';
  if (!gate.current(frame)) return 'generation';
  return null;
}

// A fixed window of one minute per function.
class RateLimit {
  #start = 0;
  #count = 0;
  readonly #limit: number;

  constructor(limit: number) {
    this.#limit = limit;
  }

  allow(now: number): boolean {
    if (now - this.#start >= 60_000) {
      this.#start = now;
      this.#count = 0;
    }
    this.#count++;
    return this.#count <= this.#limit;
  }
}

function isRecord(value: unknown): value is Record<string, unknown> {
  return typeof value === 'object' && value !== null && !Array.isArray(value);
}

// plain says whether a value is JSON-shaped data: objects and arrays built from
// Object and Array, strings, finite numbers, booleans and null, nested at most
// sixteen deep, with at most ten thousand nodes. Anything else (a function, a
// Map, a typed array, a symbol, undefined inside a structure) is not data.
export function plain(value: unknown): boolean {
  let nodes = 0;
  const walk = (v: unknown, depth: number): boolean => {
    if (++nodes > 10_000 || depth > 16) return false;
    if (v === null || typeof v === 'string' || typeof v === 'boolean') return true;
    if (typeof v === 'number') return Number.isFinite(v);
    if (Array.isArray(v)) return v.every((item) => walk(item, depth + 1));
    if (typeof v !== 'object') return false;
    const proto: unknown = Object.getPrototypeOf(v);
    if (proto !== Object.prototype && proto !== null) return false;
    return Object.values(v).every((item) => walk(item, depth + 1));
  };
  return walk(value, 0);
}

interface Spec {
  readonly limit: number;
  readonly maxBytes: number;
  // The argument list as the function takes it, or null when it is not a
  // valid call.
  readonly parse: (args: readonly unknown[]) => readonly unknown[] | null;
  readonly run: (to: Handlers, args: readonly unknown[]) => unknown;
  // True for a function that is only for the person's own act: it runs only
  // right after a click or key in the window, never from a script alone.
  // A function of the arguments, for one that is the person's act only in some
  // of its forms (open-mic mode transmits without a press).
  readonly gesture?: true | ((args: readonly unknown[]) => boolean);
}

// What a function runs against: the services, the activation, and the document
// generation of the caller (already checked to be the current one).
interface Handlers {
  readonly service: Service;
  readonly feeds: IpcDeps['feeds'];
  readonly files: IpcDeps['files'];
  readonly profiles: ProfilesApi;
  readonly voice: VoiceApi;
  readonly installed: InstalledApi;
  readonly notifications: NotificationsApi;
  readonly settings: SettingsApi;
  readonly companion: CompanionApi;
  readonly document: number;
}

// A feed's number, which the preload chose: a positive whole number.
const isFeedId = (value: unknown): value is number => typeof value === 'number' && Number.isSafeInteger(value) && value > 0 && value <= 2 ** 31;

const feedId = (args: readonly unknown[]): readonly unknown[] | null => (args.length === 1 && isFeedId(args[0]) ? args : null);

const none = (args: readonly unknown[]): readonly unknown[] | null => (args.length === 0 ? args : null);

// signIn is the spec of a function that takes one request of the operation's
// request type and sends that operation.
function signIn(step: SignInStep): Spec {
  return {
    limit: 30,
    maxBytes: 16 * 1024,
    parse: (args) => {
      const [body] = args;
      return args.length === 1 && isRecord(body) && operations[step].request?.(body).length === 0 ? [body] : null;
    },
    run: async ({ service, profiles }, [body]) =>
      profiles.signInPermitted() ? bound(profiles, await service.signIn(step, body as Record<string, unknown>)) : INCOMPATIBLE,
  };
}

// The account a sign-in gave is bound to the active profile and its credential
// kept (or held in memory only); one that belongs to another account is
// refused, and its credential has been forgotten.
async function bound(profiles: ProfilesApi, outcome: SignInOutcome): Promise<SignInOutcome> {
  if (!outcome.ok || outcome.who === null) return outcome;
  const { who } = outcome;
  const kept = await profiles.signedIn({ id: who.id, name: who.name, lasting: who.role === 'member' && who.guest === undefined, guest: who.guest !== undefined });
  return kept
    ? outcome
    : {
        ok: false,
        kind: 'invalid',
        status: 409,
        code: MISMATCH,
        message: 'This saved server belongs to a different account. Add the server again as a new entry to sign in as someone else.',
      };
}

// request parses a BridgeRequest: its keys, the operation, whether the
// renderer may invoke it, its path values and query (as a target), and its
// body against the operation's request validator.
export function parseRequest(value: unknown): BridgeRequest | null {
  if (!isRecord(value)) return null;
  const allowed = new Set(['operation', 'params', 'query', 'body', 'keepalive', 'previewAs']);
  if (!Object.keys(value).every((key) => allowed.has(key))) return null;
  const { operation, params, query, body, keepalive, previewAs } = value;
  if (typeof operation !== 'string' || !isOperationId(operation) || !invocable(operation)) return null;
  if (params !== undefined && (!isRecord(params) || Object.keys(params).length > 8)) return null;
  if (query !== undefined && typeof query !== 'string') return null;
  if (keepalive !== undefined && typeof keepalive !== 'boolean') return null;
  // The preview header rides on the five previewable reads and nowhere else.
  // Whether an operation is one is main's own table, never the renderer's word.
  if (previewAs !== undefined && (!previewable(operation) || typeof previewAs !== 'number' || !Number.isSafeInteger(previewAs) || previewAs < 1)) return null;
  if (targetOf(operation, params, query) === null) return null;
  const check = operations[operation].request;
  if (check === undefined ? body !== undefined : check(body).length > 0) return null;
  return {
    operation,
    ...(params === undefined ? {} : { params: params as Record<string, string | number> }),
    ...(query === undefined ? {} : { query }),
    ...(body === undefined ? {} : { body }),
    ...(keepalive === undefined ? {} : { keepalive }),
    ...(previewAs === undefined ? {} : { previewAs }),
  };
}

// The voice family's argument shapes. A channel and a device name are
// checked by the protocol's own validators (validChannel, validDeviceName),
// so the IPC layer and the encoder agree and a request the encoder would
// refuse never enters the service's state. The extra pattern refuses the
// line and paragraph separators as well.
const NO_SEPARATOR = /[\u2028\u2029]/u;
const isChannel = (value: unknown): value is string => typeof value === 'string' && validChannel(value) && !NO_SEPARATOR.test(value);
const isDeviceName = (value: unknown): value is string => typeof value === 'string' && validDeviceName(value) && !NO_SEPARATOR.test(value);

const hasExactKeys = (value: Record<string, unknown>, keys: readonly string[]): boolean => {
  const own = Object.keys(value);
  return own.length === keys.length && keys.every((key) => own.includes(key));
};

// A settings change is a non-empty object of known keys with boolean values.
function parseSettingsChange(args: readonly unknown[]): readonly unknown[] | null {
  const [change] = args;
  if (args.length !== 1 || !isRecord(change)) return null;
  const keys = Object.keys(change);
  if (keys.length === 0 || !keys.every((key) => key === 'closeToTray' || key === 'launchAtLogin')) return null;
  const { closeToTray, launchAtLogin } = change;
  if ((closeToTray !== undefined && typeof closeToTray !== 'boolean') || (launchAtLogin !== undefined && typeof launchAtLogin !== 'boolean')) return null;
  return [{ ...(closeToTray === undefined ? {} : { closeToTray }), ...(launchAtLogin === undefined ? {} : { launchAtLogin }) }];
}

const REPLACE_STEPS: readonly ReplaceStep[] = ['changePassword', 'setPassword', 'convertGuest', 'reauth'];

// parseReplace is exactly { operation, body }: an operation of the replacing
// family and a body that passes that operation's request validator. A password
// is data here, never echoed: a refusal names the reason, not the value.
function parseReplace(args: readonly unknown[]): readonly unknown[] | null {
  const [request] = args;
  if (args.length !== 1 || !isRecord(request) || !hasExactKeys(request, ['operation', 'body'])) return null;
  const { operation, body } = request;
  if (typeof operation !== 'string' || !(REPLACE_STEPS as readonly string[]).includes(operation)) return null;
  if (!isRecord(body) || operations[operation as ReplaceStep].request?.(body).length !== 0) return null;
  return [{ operation, body }];
}

const oneBoolean = (args: readonly unknown[]): readonly unknown[] | null => (args.length === 1 && typeof args[0] === 'boolean' ? args : null);

function parseJoin(args: readonly unknown[]): readonly unknown[] | null {
  const [request] = args;
  if (args.length !== 1 || !isRecord(request) || !hasExactKeys(request, ['channel', 'muted', 'deafened'])) return null;
  const { channel, muted, deafened } = request;
  if (!isChannel(channel) || typeof muted !== 'boolean' || typeof deafened !== 'boolean') return null;
  return [{ channel, muted, deafened }];
}

// Notifications carry exactly four bounded fields, or exactly a tag to close.
function parseNotify(args: readonly unknown[]): readonly unknown[] | null {
  const [request] = args;
  if (args.length !== 1 || !isRecord(request) || !hasExactKeys(request, ['tag', 'title', 'body', 'channel'])) return null;
  const { tag, title, body, channel } = request;
  if (typeof tag !== 'string' || tag.length === 0 || tag.length > TAG_MAX) return null;
  if (typeof title !== 'string' || title.length > 128 || typeof body !== 'string' || body.length > 200 || !isChannel(channel)) return null;
  return [{ tag, title, body, channel }];
}

function parseNotifyClose(args: readonly unknown[]): readonly unknown[] | null {
  const [request] = args;
  if (args.length !== 1 || !isRecord(request) || !hasExactKeys(request, ['tag'])) return null;
  return typeof request['tag'] === 'string' && request['tag'].length > 0 && request['tag'].length <= TAG_MAX ? [request] : null;
}

// A save is exactly a key `config set` takes from the page and a value that
// fits it (a device name or `default`, a talk mode): checked again, in the
// same way, by the one-shot run before anything is passed.
function parseSaveDevice(args: readonly unknown[]): readonly unknown[] | null {
  const [request] = args;
  if (args.length !== 1 || !isRecord(request) || !hasExactKeys(request, ['key', 'value'])) return null;
  const { key, value } = request;
  // keys.device and keys.talk are not the page's to name: see parseTalkBinding.
  if (!isPageSaveKey(key) || !saveValueFits(key, value)) return null;
  return [{ key, value }];
}

// A talk binding is exactly an id from the last listing (an opaque string, no
// longer than a UUID plus a margin; the service looks it up) and a key code.
function parseTalkBinding(args: readonly unknown[]): readonly unknown[] | null {
  const [request] = args;
  if (args.length !== 1 || !isRecord(request) || !hasExactKeys(request, ['deviceId', 'code'])) return null;
  const { deviceId, code } = request;
  if (typeof deviceId !== 'string' || !/^[A-Za-z0-9-]{1,64}$/u.test(deviceId) || keyCodeText(code) === null) return null;
  return [{ deviceId, code }];
}

// A companion grant is asked for exactly one channel, checked as a voice
// destination is; the service then compares it to the destination itself.
function parseCompanionGrant(args: readonly unknown[]): readonly unknown[] | null {
  const [request] = args;
  if (args.length !== 1 || !isRecord(request) || !hasExactKeys(request, ['channel'])) return null;
  const { channel } = request;
  return isChannel(channel) ? [{ channel }] : null;
}

// At least one of input and output; each a printable device name.
function parseDevices(args: readonly unknown[]): readonly unknown[] | null {
  const [devices] = args;
  if (args.length !== 1 || !isRecord(devices)) return null;
  const keys = Object.keys(devices);
  if (keys.length === 0 || !keys.every((key) => key === 'input' || key === 'output')) return null;
  const { input, output } = devices;
  if ((input !== undefined && !isDeviceName(input)) || (output !== undefined && !isDeviceName(output))) return null;
  return [{ ...(input === undefined ? {} : { input }), ...(output === undefined ? {} : { output }) }];
}

export const SPECS: Readonly<Record<InvokedFunction, Spec>> = {
  serverAddress: { limit: 120, maxBytes: 0, parse: none, run: ({ service }) => service.serverAddress() },
  request: {
    limit: 1200,
    maxBytes: 1024 * 1024,
    parse: (args) => {
      const parsed = args.length === 1 ? parseRequest(args[0]) : null;
      return parsed === null ? null : [parsed];
    },
    // On a server the app may not sign in to (issue #946), nothing but the
    // health check goes out: a preview of an invite or a sign-in link carries
    // a secret code, and the page has no use for anything else there.
    run: ({ service, profiles }, [request]) =>
      profiles.signInPermitted() || (request as BridgeRequest).operation === 'health'
        ? service.request(request as BridgeRequest)
        : Promise.resolve(INCOMPATIBLE_REPLY),
  },
  restore: { limit: 120, maxBytes: 0, parse: none, run: ({ service }) => service.restore() },
  signInWithCredential: {
    limit: 30,
    maxBytes: 8 * 1024,
    parse: (args) => (args.length === 1 && typeof args[0] === 'string' && /^\S{1,4096}$/u.test(args[0]) ? args : null),
    run: ({ service, profiles }, [credential]) => (profiles.signInPermitted() ? service.signInWithCredential(credential as string) : INCOMPATIBLE),
  },
  keepCredential: {
    limit: 30,
    maxBytes: 8 * 1024,
    parse: (args) => (args.length === 1 && typeof args[0] === 'string' && /^paste-[0-9a-f]{32}$/u.test(args[0]) ? args : null),
    run: async ({ service, profiles }, [handle]) => {
      if (!profiles.signInPermitted()) return false;
      if (!service.keepCredential(handle as string)) return false;
      // The credential is in force: bind it to the profile and keep it.
      return profiles.keptCredential();
    },
  },
  signInWithPassword: signIn('login'),
  signInWithCode: signIn('loginTotp'),
  redeemInvite: signIn('redeemInvite'),
  redeemInviteAsGuest: signIn('redeemInviteGuest'),
  redeemSignInLink: signIn('redeemSignInLink'),
  redeemSetupToken: signIn('setupRedeem'),
  resetPassword: signIn('resetPassword'),
  joinAsGuest: signIn('joinGuest'),
  createAccount: signIn('joinAccount'),
  signOut: { limit: 120, maxBytes: 0, parse: none, run: ({ profiles }) => profiles.signOut() },
  // The writes that replace the credential they are sent with (issue #1422):
  // main sends the write, takes the new token from the answer and keeps it;
  // the renderer is answered the account, never the token. Rated as the
  // sign-in steps are, since each carries a password.
  replaceCredential: {
    limit: 30,
    maxBytes: 16 * 1024,
    parse: parseReplace,
    run: async ({ service, profiles }, [request]) => {
      if (!profiles.signInPermitted()) return INCOMPATIBLE;
      const { operation, body } = request as { operation: ReplaceStep; body: Record<string, unknown> };
      // bound keeps the rotated credential where the old one was kept (the
      // vault, for a member's), or binds the member a guest has become.
      return bound(profiles, await service.replaceCredential(operation, body));
    },
  },
  // The saved server profiles (issue #945). Ids are 32 hex characters; a
  // label is plain text of at most 64 characters (main trims and checks it).
  profiles: { limit: 120, maxBytes: 0, parse: none, run: ({ profiles }) => profiles.view() },
  profileAdd: {
    limit: 30,
    maxBytes: 4 * 1024,
    parse: (args) => (args.length === 1 && typeof args[0] === 'string' ? args : null),
    run: ({ profiles }, [address]) => profiles.add(address as string),
  },
  profileRename: {
    limit: 30,
    maxBytes: 1024,
    parse: (args) => (args.length === 2 && isProfileId(args[0]) && typeof args[1] === 'string' && args[1].length <= 256 ? args : null),
    run: ({ profiles }, [id, label]) => profiles.rename(id as string, label as string),
  },
  profileRemove: {
    limit: 30,
    maxBytes: 128,
    parse: (args) => (args.length === 1 && isProfileId(args[0]) ? args : null),
    run: ({ profiles }, [id]) => profiles.remove(id as string),
  },
  profileSwitch: {
    limit: 30,
    maxBytes: 128,
    parse: (args) => (args.length === 1 && (args[0] === null || isProfileId(args[0])) ? args : null),
    run: ({ profiles }, [id]) => profiles.switchTo(id as string | null),
  },
  storageRetry: { limit: 30, maxBytes: 0, parse: none, run: ({ profiles }) => profiles.retry() },
  profilesRecover: { limit: 10, maxBytes: 0, parse: none, run: ({ profiles }) => profiles.recover() },
  // The feeds (issue #943). A reconnecting feed subscribes at most once a
  // second, and an acknowledgement follows each batch of frames; past these
  // a batch waits, and a feed that waits too long is closed and resubscribes.
  feedSubscribe: {
    limit: 600,
    maxBytes: 1024,
    parse: (args) => {
      const [id, request] = args;
      const feed = args.length === 2 && isFeedId(id) ? parseFeedRequest(request) : null;
      return feed === null ? null : [id, feed];
    },
    run: ({ feeds, document }, [id, request]) => feeds.subscribe(document, id as number, request as FeedRequest),
  },
  feedAck: { limit: 60_000, maxBytes: 32, parse: feedId, run: ({ feeds, document }, [id]) => feeds.ack(document, id as number) },
  // Attachments (issue #947). The bytes cross as base64 text, capped; a
  // download is bytes for a preview, a save is the person's act alone.
  uploadFile: {
    limit: 30,
    maxBytes: Math.ceil((MAX_UPLOAD_BYTES * 4) / 3) + 8 * 1024,
    parse: (args) => {
      const parsed = args.length === 1 ? parseUpload(args[0]) : null;
      return parsed === null ? null : [parsed];
    },
    run: ({ files }, [upload]) => files.upload(upload as Parameters<IpcDeps['files']['upload']>[0]),
  },
  downloadFile: {
    limit: 300,
    maxBytes: 32,
    parse: (args) => (args.length === 1 && isFileId(args[0]) ? args : null),
    run: ({ files }, [id]) => files.download(id as number),
  },
  saveFile: {
    limit: 30,
    maxBytes: 32,
    parse: (args) => (args.length === 1 && isFileId(args[0]) ? args : null),
    run: ({ files }, [id]) => files.save(id as number),
    gesture: true,
  },
  // Notifications are activation-owned and carry no action or activation data.
  // The byte bounds fit the largest request parseNotify accepts, every
  // character quoted as JSON escapes it (a tag of TAG_MAX, title 128, body 200,
  // a channel at its byte bound), so a size check never refuses a valid one.
  notify: {
    limit: 30,
    maxBytes: 4096,
    parse: parseNotify,
    run: ({ notifications, profiles, document }, [request]) => notifications.show(profiles.active, document, request as NotifyRequest),
  },
  notifyClose: {
    limit: 600,
    maxBytes: 2048,
    parse: parseNotifyClose,
    run: ({ notifications, profiles, document }, [request]) => notifications.close(profiles.active, document, (request as { tag: string }).tag),
  },
  // Native voice (issue #1374). Every argument is main's to check; none is a
  // token, a path or a press: the talk key is read by the helper itself, and
  // nothing here starts a transmission. Join, retry and open-mic mode (which
  // transmits without a press) are the person's act alone.
  voiceState: { limit: 600, maxBytes: 0, parse: none, run: ({ voice }) => voice.state() },
  voiceJoin: {
    limit: 20,
    maxBytes: 1024,
    parse: parseJoin,
    run: ({ voice }, [request]) => voice.join(request as { channel: string; muted: boolean; deafened: boolean }),
    gesture: true,
  },
  // The person's Leave ends the screen companion too (issue #1579): the page
  // is told and the DELETE sent before the helper is asked to leave.
  voiceLeave: {
    limit: 60,
    maxBytes: 0,
    parse: none,
    run: async ({ voice, companion }) => {
      const ended = companion.end('leave');
      const view = await voice.leave();
      await ended;
      return view;
    },
  },
  voiceRetry: { limit: 20, maxBytes: 0, parse: none, run: ({ voice }) => voice.retry(), gesture: true },
  voiceSetMute: { limit: 120, maxBytes: 16, parse: oneBoolean, run: ({ voice }, [muted]) => voice.setMute(muted as boolean) },
  voiceSetDeafen: { limit: 120, maxBytes: 16, parse: oneBoolean, run: ({ voice }, [deafened]) => voice.setDeafen(deafened as boolean) },
  voiceSetDevices: {
    limit: 30,
    maxBytes: 2 * VOICE_DEVICES_MAX_NAME_BYTES + 128,
    parse: parseDevices,
    run: ({ voice }, [devices]) => voice.setDevices(devices as { input?: string; output?: string }),
  },
  voiceSetTalkMode: {
    limit: 30,
    maxBytes: 32,
    parse: (args) => (args.length === 1 && typeof args[0] === 'string' && voiceControlTalkMode(args[0]).length === 0 ? args : null),
    run: ({ voice }, [mode]) => voice.setTalkMode(mode as VoiceControlTalkMode),
    gesture: (args) => args[0] === 'open',
  },
  // The helper's one-shot runs (issue #1378). The list is read-only; a save
  // writes the person's voice.toml and is the person's act alone. Each is
  // one process at a time, so the rates are low.
  voiceDevices: { limit: 30, maxBytes: 0, parse: none, run: ({ voice }) => voice.devices() },
  voiceSaveDevice: {
    limit: 30,
    maxBytes: 2 * VOICE_DEVICES_MAX_NAME_BYTES + 128,
    parse: parseSaveDevice,
    run: ({ voice }, [request]) => voice.saveDevice(request as { key: VoiceDeviceKey; value: string }),
    gesture: true,
  },
  // Talk-key setup (issue #1381). The listing opens no device; a save writes
  // the person's voice.toml and is the person's act alone. Two processes in a
  // row, so the rate is low.
  voiceKeyDevices: { limit: 20, maxBytes: 0, parse: none, run: ({ voice }) => voice.keyDevices() },
  voiceSaveTalkBinding: {
    limit: 10,
    maxBytes: 256,
    parse: parseTalkBinding,
    run: ({ voice }, [request]) => voice.saveTalkBinding(request as { deviceId: string; code: number }),
    gesture: true,
  },
  // The update family (issue #1600). Restart is the person's act alone: it
  // runs only right after a click or key in the window, and nothing else
  // restarts the app.
  updateState: { limit: 120, maxBytes: 0, parse: none, run: ({ installed }) => installed.view() },
  updateRestart: {
    limit: 6,
    maxBytes: 0,
    parse: none,
    run: ({ installed }) => {
      installed.restart();
    },
    gesture: true,
  },
  // The app's own settings (issue #1587). A change is the person's act alone.
  settings: { limit: 120, maxBytes: 0, parse: none, run: ({ settings }) => settings.view() },
  settingsSet: {
    limit: 30,
    maxBytes: 128,
    parse: parseSettingsChange,
    run: ({ settings }, [change]) => settings.set(change as SettingsChange),
    gesture: true,
  },
  feedUnsubscribe: { limit: 600, maxBytes: 32, parse: feedId, run: ({ feeds, document }, [id]) => feeds.unsubscribe(document, id as number) },
  // The screen companion (issue #1579, D4-02 of #772). A grant is the
  // person's act alone, and only ever the companion's: never the bearer or
  // the person's own voice grant. Stopping needs no act.
  companionGrant: {
    limit: 20,
    maxBytes: 1024,
    parse: parseCompanionGrant,
    run: ({ companion }, [request]) => companion.grant(request as { channel: string }),
    gesture: true,
  },
  companionEnd: { limit: 60, maxBytes: 0, parse: none, run: ({ companion }) => companion.stop() },
};

export class Ipc {
  readonly #deps: IpcDeps;
  readonly #limits = new Map<InvokedFunction, RateLimit>(INVOKED_FUNCTIONS.map((name) => [name, new RateLimit(SPECS[name].limit)]));
  readonly #supportLimit = new RateLimit(120);
  #refusals = 0;

  constructor(deps: IpcDeps) {
    this.#deps = deps;
  }

  // How many invocations have been refused since start.
  get refusals(): number {
    return this.#refusals;
  }

  #refuse(reason: Refusal): Envelope {
    this.#refusals++;
    this.#deps.log(`conch-desktop: refused an IPC call (${reason}); ${String(this.#refusals)} so far`);
    return { ok: false, kind: 'refused' };
  }

  async invoke(event: InvokeEvent, name: unknown, args: readonly unknown[]): Promise<Envelope> {
    const why = checkCaller(event, this.#deps.owned(), this.#deps.gate);
    if (why !== null) return this.#refuse(why);
    if (typeof name !== 'string' || !(INVOKED_FUNCTIONS as readonly string[]).includes(name)) return this.#refuse('name');
    const fn = name as InvokedFunction;
    const spec = SPECS[fn];
    if (this.#limits.get(fn)?.allow(this.#deps.now()) !== true) return this.#refuse('rate');
    if (!plain(args)) return this.#refuse('shape');
    if (JSON.stringify(args).length > spec.maxBytes + 2) return this.#refuse('size');
    const parsed = spec.parse(args);
    if (parsed === null) return this.#refuse('shape');
    const needsGesture = spec.gesture === true || (typeof spec.gesture === 'function' && spec.gesture(parsed));
    if (needsGesture && this.#deps.gestures.take(this.#deps.owned(), LINK_GESTURE_MS) === null) return this.#refuse('gesture');
    try {
      const to: Handlers = {
        service: this.#deps.service,
        feeds: this.#deps.feeds,
        files: this.#deps.files,
        profiles: this.#deps.profiles,
        voice: this.#deps.voice,
        installed: this.#deps.installed,
        notifications: this.#deps.notifications,
        settings: this.#deps.settings,
        companion: this.#deps.companion ?? NO_COMPANION,
        document: this.#deps.gate.generation,
      };
      return { ok: true, value: await spec.run(to, parsed) };
    } catch {
      // A bug in a function is not the renderer's to learn about.
      return this.#refuse('error');
    }
  }

  // The synchronous support check passes the same caller, rate and shape gates.
  notificationSupported(event: InvokeEvent, args: readonly unknown[]): Envelope {
    const why = checkCaller(event, this.#deps.owned(), this.#deps.gate);
    if (why !== null) return this.#refuse(why);
    if (!this.#supportLimit.allow(this.#deps.now())) return this.#refuse('rate');
    if (!plain(args)) return this.#refuse('shape');
    if (JSON.stringify(args).length > 2) return this.#refuse('size');
    if (args.length !== 0) return this.#refuse('shape');
    try {
      return { ok: true, value: this.#deps.notifications.supported() };
    } catch {
      return this.#refuse('error');
    }
  }

  // register installs one handler per invoked function and nothing else.
  register(ipcMain: Pick<IpcMain, 'handle'>): void {
    for (const name of INVOKED_FUNCTIONS) {
      ipcMain.handle(channelOf(name), (event, ...args: unknown[]) => this.invoke(event, name, args));
    }
  }

  // Registers the one synchronous call, whose answer is still an Envelope.
  registerNotificationSupport(ipcMain: Pick<IpcMain, 'on'>): void {
    ipcMain.on(channelOf(NOTIFICATION_SUPPORT), (event, ...args: unknown[]) => {
      event.returnValue = this.notificationSupported(event, args);
    });
  }
}
