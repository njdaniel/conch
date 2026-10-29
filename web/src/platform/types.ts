// The platform seam (issue #770, D2-2). Everything the client asks of the place
// it runs in, as one object: how it talks to conchd, how it opens a live
// feed, where it keeps the credential, and the few services (clipboard,
// notifications, external links, the address it is served from) that a
// browser provides and a desktop shell provides differently. The browser
// implements it in ./browser; the desktop shell implements it in ./desktop
// (D2-3 and later). Outside ./browser nothing calls fetch, WebSocket, the
// clipboard or the Notification API, and nothing but browser/credentials.ts
// touches the credential's storage.
import type { Call, OperationId } from './operations';

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

// SocketTransport opens the live feeds. The ticket is part of query: a
// single-use credential for this socket, which the caller asked for.
export interface SocketTransport {
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
  // false when the storage refused the write.
  write(token: string, place: Place): boolean;
  // Forgets held, but only if it is still the one stored there.
  drop(held: Held): void;
  // Calls back when another tab removes or replaces the lasting credential
  // (or clears the storage). Returns the function that stops listening.
  onLastingChange(callback: () => void): () => void;
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

export interface Services {
  // The address the client is served from: the server's own, for the browser.
  origin(): string;
  // Puts text on the clipboard; rejects, or throws, when there is no
  // clipboard here.
  copyText(text: string): Promise<void>;
  notifications: {
    supported(): boolean;
    permission(): NotificationPermissionState;
    request(): Promise<NotificationPermissionState>;
    show(title: string, body: string, tag: string): NativeNotification;
  };
  // The attributes of a link that leaves the app.
  externalLink: { readonly target?: '_blank'; readonly rel: string };
  // Whether this client may publish the person's own voice. Only the browser
  // may: the desktop renderer never holds the voice grant.
  browserVoice: boolean;
}

export interface Platform {
  readonly name: 'browser' | 'desktop';
  readonly http: HttpTransport;
  readonly sockets: SocketTransport;
  readonly credentials: CredentialStore;
  readonly services: Services;
}
