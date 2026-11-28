// The DM API as the transcript, the inbox and the message action see it: a
// port (DmApi) that tests replace, and dmApi, the one that talks to conchd
// through the shared transport (the routes of pkg/schema/dm.go, the operation
// table of platform/operations.ts). Wire shapes come from web/src/schema only.
//
// Input: a conversation or message id and, for a write, the text. Output: a
// DmResult, never a thrown error, with the refusal reduced to what a person
// may be told. contact_unavailable is the one refusal of a peer who cannot be
// reached, and it carries no reason (ADR-017): there is nothing here to tell
// block from preference from switch, and nothing may be added.
import { dmRequest, dmTransport, type DmCall } from '../api';
import { platform } from '../platform';
import {
  dmConversationV1,
  dmFileV1,
  dmPreferencesV1,
  dmReadStateV1,
  editDMMessageResponseV1,
  listDMChangesResponseV1,
  listDMConversationsResponseV1,
  listDMMessagesResponseV1,
  openDMResponseV1,
  postDMMessageResponseV1,
  searchDMMessagesResponseV1,
  type DMAcceptPolicy,
  type DMConversationV1,
  type DMFileV1,
  type DMMessageEditedV1,
  type DMMessageRemovedV1,
  type DMMessageV1,
  type DMPreferencesV1,
  type DMReadStateV1,
  type DMSnapshotV1,
  type DMState,
  type EditDMMessageRequestV1,
  type EditDMMessageResponseV1,
  type ListDMChangesResponseV1,
  type ListDMConversationsResponseV1,
  type ListDMMessagesResponseV1,
  type MarkReadRequestV1,
  type OpenDMRequestV1,
  type OpenDMResponseV1,
  type PostDMMessageRequestV1,
  type PostDMMessageResponseV1,
  type SearchDMMessagesResponseV1,
  type SetDMStateRequestV1,
} from '../schema/generated';
import { errorV0, type ErrorV0 } from '../schema/error';
import { fromServer, type Validator } from '../schema/validate';

export type DmFailure =
  | 'unauthorized' // 401: sign in again
  | 'network' // no answer, or one that cannot be trusted
  | 'unsupported' // the server has no such route
  | 'not_found' // dm_not_found, principal_not_found: one answer for gone and not yours
  | 'unavailable' // contact_unavailable: this person cannot be messaged now, never why
  | 'dms_off' // the caller's own switch or the server's
  | 'own_sign_in_required' // an act-as credential
  | 'forbidden' // a route-level 403 outside the DM contact policy
  | 'rate_limited'
  | 'refused'; // anything else the server said no to

export type DmResult<T> =
  | { readonly ok: true; readonly value: T }
  | { readonly ok: false; readonly failure: DmFailure; readonly code?: string; readonly retryAfter?: number };

// What a send came to. unconfirmed is no answer: the message may or may not be
// stored, and sending it again with the same nonce is safe, since the server
// answers the first copy (a replay) and never stores a second.
export type DmSendResult =
  | { readonly ok: true; readonly message: DMMessageV1 }
  | { readonly ok: false; readonly failure: DmFailure | 'unconfirmed'; readonly retryAfter?: number };

// Updates delivered to DM views, including local refresh signals. The
// message, edit, removal and snapshot payloads use the per-person stream's
// schema.
export type DmFrame =
  | { readonly type: 'snapshot'; readonly snapshot: DMSnapshotV1 }
  | { readonly type: 'message'; readonly message: DMMessageV1 }
  | { readonly type: 'edited'; readonly frame: DMMessageEditedV1 }
  | { readonly type: 'removed'; readonly frame: DMMessageRemovedV1 }
  | { readonly type: 'changed'; readonly conversation: number }
  | { readonly type: 'lost' } // the socket closed; a snapshot follows the reconnect
  | { readonly type: 'poll' };

export interface HistoryQuery {
  readonly order: 'asc' | 'desc';
  readonly before?: number;
  readonly after?: number;
}

export interface DmApi {
  snapshot?: () => Promise<DmResult<DMSnapshotV1>>;
  list: (state?: Exclude<DMState, 'hidden'>, cursor?: string) => Promise<DmResult<ListDMConversationsResponseV1>>;
  open: (peer: number) => Promise<DmResult<DMConversationV1>>;
  get: (conversation: number) => Promise<DmResult<DMConversationV1>>;
  history: (conversation: number, query: HistoryQuery) => Promise<DmResult<ListDMMessagesResponseV1>>;
  changes: (conversation: number, after: number) => Promise<DmResult<ListDMChangesResponseV1>>;
  send: (conversation: number, request: PostDMMessageRequestV1) => Promise<DmSendResult>;
  edit: (conversation: number, message: number, body: string) => Promise<DmResult<DMMessageV1>>;
  remove: (conversation: number, message: number) => Promise<DmResult<null>>;
  markRead: (conversation: number, lastRead: number) => Promise<DmResult<DMReadStateV1>>;
  setState: (conversation: number, state: DMState) => Promise<DmResult<DMConversationV1>>;
  search: (conversation: number, words: string, before?: number) => Promise<DmResult<SearchDMMessagesResponseV1>>;
  fileMeta: (conversation: number, file: number) => Promise<DmResult<DMFileV1>>;
  uploadFile: (conversation: number, name: string, mediaType: string, bytes: Blob, signal: AbortSignal) => Promise<DmResult<DMFileV1>>;
  fileBytes: (conversation: number, file: number) => Promise<DmResult<Blob>>;
  saveFile?: (conversation: number, file: number) => Promise<DmResult<null>>;
  preferences?: () => Promise<DmResult<DMPreferencesV1>>;
  setPreferences: (accept: DMAcceptPolicy) => Promise<DmResult<DMPreferencesV1>>;
  // A subscription to DM updates and signals to re-read. The returned function unsubscribes.
  live: (listener: (frame: DmFrame) => void) => () => void;
}

// failureOf maps a refusal's code to what a client does about it.
export function failureOf(status: number | undefined, code: string | undefined): DmFailure {
  if (status === 401) return 'unauthorized';
  if (status === 404 && code === undefined) return 'unsupported';
  // A contact or send-policy refusal still allows access to retained history.
  if (status === 403 && code !== 'contact_unavailable' && code !== 'dms_off' && code !== 'own_sign_in_required') return 'forbidden';
  if (status === 429 || code === 'rate_limited') return 'rate_limited';
  switch (code) {
    case 'dm_not_found':
    case 'principal_not_found':
      return 'not_found';
    case 'contact_unavailable':
      return 'unavailable';
    case 'dms_off':
      return 'dms_off';
    case 'own_sign_in_required':
      return 'own_sign_in_required';
    default:
      return status === 404 ? 'not_found' : 'refused';
  }
}

async function refusal(res: Response): Promise<{ readonly code?: string; readonly retryAfter?: number }> {
  try {
    const body: unknown = await res.json();
    if (fromServer(errorV0)(body).length === 0) {
      const doc = body as ErrorV0;
      return { code: doc.code, ...(doc.retry_after_seconds === undefined ? {} : { retryAfter: doc.retry_after_seconds }) };
    }
  } catch {
    // No document, or not one.
  }
  return {};
}

async function readResult<T>(request: DmCall, check: Validator<T>): Promise<DmResult<T>> {
  const result = await dmTransport.read(request, check, true);
  if (result.ok) return { ok: true, value: result.value };
  if (result.failure === 'unauthorized') return { ok: false, failure: 'unauthorized' };
  if (result.failure === 'network' || result.status === undefined || result.status >= 500) return { ok: false, failure: 'network' };
  return { ok: false, failure: failureOf(result.status, result.code), ...(result.code === undefined ? {} : { code: result.code }) };
}

async function settled<T>(reply: Awaited<ReturnType<typeof dmTransport.write>>, check: Validator<unknown> | null, value: (parsed: unknown) => T): Promise<DmResult<T>> {
  if (reply.kind === 'none') return { ok: false, failure: 'network' };
  if (reply.kind === 'unauthorized') return { ok: false, failure: 'unauthorized' };
  const res = reply.res;
  if (res.status >= 500) return { ok: false, failure: 'network' };
  if (!res.ok) {
    const why = await refusal(res);
    return { ok: false, failure: failureOf(res.status, why.code), ...why };
  }
  if (check === null) return { ok: true, value: value(null) };
  try {
    const parsed: unknown = await res.json();
    if (fromServer(check)(parsed).length === 0) return { ok: true, value: value(parsed) };
  } catch {
    // Fall through: the body was cut off.
  }
  return { ok: false, failure: 'network' };
}

const { call, queryOf } = dmRequest;
export const isDmOperationServed = dmRequest.isOperationServed;

const pageLimit = '50';

function createDmApi(access?: { readonly denied: () => boolean; readonly deny: () => void }): DmApi {
  async function request<T>(run: () => Promise<DmResult<T>>): Promise<DmResult<T>> {
    if (access?.denied() === true) return { ok: false, failure: 'forbidden' };
    const result = await run();
    if (!result.ok && (result.failure === 'forbidden' || result.failure === 'own_sign_in_required')) access?.deny();
    return result;
  }

  const read = <T>(call: DmCall, check: Validator<T>) => request(() => readResult(call, check));
  const write = <T>(call: DmCall, body: unknown, check: Validator<unknown> | null, value: (parsed: unknown) => T) =>
    request(async () => settled(await dmTransport.write(call, body), check, value));
  const nativeSave = platform.services.saveFile;

  return {
    list: (state, cursor) => read(call('listDms', undefined, queryOf({ ...(state === undefined ? {} : { state }), ...(cursor === undefined ? {} : { cursor }) })), listDMConversationsResponseV1),
    open: async (peer) => {
      const request: OpenDMRequestV1 = { peer_id: peer };
      const result = await write(call('openDm'), request, openDMResponseV1, (parsed) => (parsed as OpenDMResponseV1).conversation);
      return result;
    },
    get: (conversation) => read(call('getDm', { conversation }), dmConversationV1),
    history: (conversation, query) => {
      const values: Record<string, string> = { order: query.order, limit: pageLimit };
      if (query.before !== undefined) values['before'] = String(query.before);
      if (query.after !== undefined) values['after'] = String(query.after);
      return read(call('listDmMessages', { conversation }, queryOf(values)), listDMMessagesResponseV1);
    },
    changes: (conversation, after) => read(call('listDmChanges', { conversation }, queryOf({ after: String(after) })), listDMChangesResponseV1),
    send: async (conversation, request) => {
      const result = await write(call('postDmMessage', { conversation }), request, postDMMessageResponseV1, (parsed) => (parsed as PostDMMessageResponseV1).message);
      if (result.ok) return { ok: true, message: result.value };
      // A 5xx or a cut-off body may come after the server stored it.
      return { ok: false, failure: result.failure === 'network' ? 'unconfirmed' : result.failure, ...(result.retryAfter === undefined ? {} : { retryAfter: result.retryAfter }) };
    },
    edit: (conversation, message, body) => {
      const request: EditDMMessageRequestV1 = { body };
      return write(call('editDmMessage', { conversation, message }), request, editDMMessageResponseV1, (parsed) => (parsed as EditDMMessageResponseV1).message);
    },
    remove: (conversation, message) => write(call('removeDmMessage', { conversation, message }), undefined, null, () => null),
    markRead: (conversation, lastRead) => {
      const request: MarkReadRequestV1 = { last_read_id: lastRead };
      return write(call('markDmRead', { conversation }), request, dmReadStateV1, (parsed) => parsed as DMReadStateV1);
    },
    setState: (conversation, state) => {
      const request: SetDMStateRequestV1 = { state };
      return write(call('setDmState', { conversation }), request, dmConversationV1, (parsed) => parsed as DMConversationV1);
    },
    search: (conversation, words, before) => {
      const values: Record<string, string> = { q: words, limit: pageLimit };
      if (before !== undefined) values['before'] = String(before);
      return read(call('searchDmMessages', { conversation }, queryOf(values)), searchDMMessagesResponseV1);
    },
    fileMeta: (conversation, file) => read(call('dmFileMeta', { conversation, file }), dmFileV1),
    uploadFile: (conversation, name, mediaType, bytes, signal) => request(async () => {
      const reply = await dmTransport.upload(call('uploadDmFile', { conversation }, `filename=${encodeURIComponent(name)}`), mediaType, bytes, signal);
      return settled(reply, dmFileV1, (parsed) => parsed as DMFileV1);
    }),
    fileBytes: (conversation, file) => request(async () => {
      const reply = await dmTransport.download(call('dmFileBytes', { conversation, file }));
      if (reply.kind === 'none') return { ok: false, failure: 'network' };
      if (reply.kind === 'unauthorized') return { ok: false, failure: 'unauthorized' };
      if (!reply.res.ok) return { ok: false, failure: failureOf(reply.res.status, (await refusal(reply.res)).code) };
      try {
        return { ok: true, value: await reply.res.blob() };
      } catch {
        return { ok: false, failure: 'network' };
      }
    }),
    ...(nativeSave === undefined ? {} : {
      saveFile: (conversation: number, file: number) => request<null>(async () => {
        const outcome = await nativeSave({ conversation, file });
        if (outcome === 'unauthorized' || outcome === 'forbidden') return { ok: false, failure: outcome };
        if (outcome === 'failed') return { ok: false, failure: 'refused' };
        return { ok: true, value: null };
      }),
    }),
    // The server has no preference endpoint yet. Keep the selector hidden until
    // the adapter can read the current value.
    setPreferences: (accept) => {
      const request: DMPreferencesV1 = { accept };
      return write(call('putDmPreferences'), request, dmPreferencesV1, (parsed) => parsed as DMPreferencesV1);
    },
    // The person socket exists (#2040), but its DM snapshot and fan-out are
    // unwired on the server (#2022). Re-read served REST routes while visible.
    live: (listener) => {
      const timer = window.setInterval(() => {
        if (document.visibilityState === 'visible') listener({ type: 'poll' });
      }, 15_000);
      return () => { window.clearInterval(timer); };
    },
  };
}

export const dmApi = createDmApi();

// Latch a credential refusal across every DM surface for this sign-in.
// Guard further calls immediately, including pagination already in progress.
export function createDmSession() {
  let refused = false;
  const listeners = new Set<() => void>();
  const denied = () => refused;
  const api = createDmApi({
    denied,
    deny: () => {
      if (refused) return;
      refused = true;
      for (const listener of listeners) listener();
    },
  });
  return {
    api,
    denied,
    subscribe: (listener: () => void) => {
      listeners.add(listener);
      return () => { listeners.delete(listener); };
    },
  };
}
