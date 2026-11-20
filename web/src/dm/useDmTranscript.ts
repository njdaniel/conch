// The state of one open DM conversation: history oldest first, sends in
// flight, the read mark, and what the person may still do. The state is keyed
// by (scope, conversation): an answer that arrives for a key the hook has left
// is dropped by the reducer, so switching conversations, accounts or servers
// can never show another conversation's messages (no matter what is late).
import { useCallback, useEffect, useMemo, useReducer, useRef } from 'react';
import type { DMConversationV1, DMMessageV1, DMReadStateV1 } from '../schema/generated';
import type { DmApi, DmFailure, DmFrame } from './api';
import { merge, ordered, revisionOf, settle, type Held, type Pending } from './model';

// What the person may do in the open conversation. 'gone' is a conversation
// that is not theirs (any more): one state for "no such" and "not yours".
export type Access = 'ok' | 'unavailable' | 'off' | 'own_sign_in_required';

export interface TranscriptState {
  readonly key: string;
  readonly status: 'loading' | 'ready' | 'failed' | 'gone';
  readonly conversation: DMConversationV1 | null;
  readonly messages: Held;
  readonly older: number | null;
  readonly loadingOlder: boolean;
  readonly olderFailed: boolean;
  readonly revision: number;
  readonly lastRead: number;
  readonly pending: readonly Pending[];
  readonly access: Access;
}

const empty = (key: string): TranscriptState => ({
  key,
  status: 'loading',
  conversation: null,
  messages: new Map(),
  older: null,
  loadingOlder: false,
  olderFailed: false,
  revision: 0,
  lastRead: 0,
  pending: [],
  access: 'ok',
});

// Omit that keeps the union's members apart.
type Without<A, K extends string> = A extends unknown ? Omit<A, K> : never;

type Action = { readonly key: string } & (
  | { readonly type: 'reset' }
  | { readonly type: 'loaded'; readonly conversation: DMConversationV1; readonly messages: readonly DMMessageV1[]; readonly older: number | null }
  | { readonly type: 'status'; readonly status: 'loading' | 'failed' | 'gone' }
  | { readonly type: 'olderStart' }
  | { readonly type: 'olderDone'; readonly messages: readonly DMMessageV1[]; readonly older: number | null }
  | { readonly type: 'olderFailed' }
  | { readonly type: 'merge'; readonly messages: readonly DMMessageV1[] }
  | { readonly type: 'conversation'; readonly conversation: DMConversationV1 }
  | { readonly type: 'read'; readonly lastRead: number }
  | { readonly type: 'pending'; readonly pending: Pending }
  | { readonly type: 'pendingStatus'; readonly nonce: string; readonly status: Pending['status']; readonly failure?: DmFailure | 'unconfirmed' }
  | { readonly type: 'discard'; readonly nonce: string }
  | { readonly type: 'access'; readonly access: Access }
);

export function reduce(state: TranscriptState, action: Action): TranscriptState {
  if (action.type === 'reset') return state.key === action.key ? state : empty(action.key);
  if (action.key !== state.key) return state;
  switch (action.type) {
    case 'loaded': {
      const messages = merge(state.messages, action.messages);
      return {
        ...state,
        status: 'ready',
        conversation: action.conversation,
        messages,
        older: action.older,
        revision: revisionOf(action.messages, Math.max(state.revision, action.conversation.revision)),
        lastRead: Math.max(state.lastRead, action.conversation.last_read_id),
        pending: settle(state.pending, action.messages),
      };
    }
    case 'status':
      return { ...state, status: action.status };
    case 'olderStart':
      return { ...state, loadingOlder: true, olderFailed: false };
    case 'olderDone':
      return { ...state, loadingOlder: false, messages: merge(state.messages, action.messages), older: action.older, revision: revisionOf(action.messages, state.revision) };
    case 'olderFailed':
      return { ...state, loadingOlder: false, olderFailed: true };
    case 'merge': {
      const messages = merge(state.messages, action.messages);
      const pending = settle(state.pending, action.messages);
      const revision = revisionOf(action.messages, state.revision);
      return messages === state.messages && pending === state.pending && revision === state.revision ? state : { ...state, messages, pending, revision };
    }
    case 'conversation':
      return { ...state, conversation: action.conversation, revision: Math.max(state.revision, action.conversation.revision) };
    case 'read':
      return action.lastRead > state.lastRead ? { ...state, lastRead: action.lastRead } : state;
    case 'pending':
      return { ...state, pending: [...state.pending.filter((p) => p.nonce !== action.pending.nonce), action.pending] };
    case 'pendingStatus':
      return {
        ...state,
        pending: state.pending.map((p) =>
          p.nonce === action.nonce ? { nonce: p.nonce, body: p.body, fileIds: p.fileIds, status: action.status, ...(action.failure === undefined ? {} : { failure: action.failure }) } : p,
        ),
      };
    case 'discard':
      return { ...state, pending: state.pending.filter((p) => p.nonce !== action.nonce) };
    case 'access':
      return state.access === action.access ? state : { ...state, access: action.access };
  }
}

export interface Transcript {
  readonly state: TranscriptState;
  readonly messages: readonly DMMessageV1[];
  readonly send: (body: string, fileIds: readonly number[]) => Promise<boolean>;
  readonly retry: (nonce: string) => Promise<void>;
  readonly discard: (nonce: string) => void;
  readonly edit: (message: number, body: string) => Promise<DmFailure | null>;
  readonly remove: (message: number) => Promise<DmFailure | null>;
  readonly loadOlder: () => Promise<void>;
  readonly reload: () => void;
}

const newNonce = (): string => globalThis.crypto.randomUUID();

export function useDmTranscript(api: DmApi, scope: string, conversation: number, onRead?: (state: DMReadStateV1) => void): Transcript {
  const key = `${scope}/${String(conversation)}`;
  const [raw, dispatch] = useReducer(reduce, key, empty);
  // Until the reset effect has run for a new key, show an empty state for it:
  // a render never shows the previous key's messages.
  const state = raw.key === key ? raw : empty(key);
  const live = useRef(state);
  live.current = state;
  const reading = useRef<{ key: string; id: number } | null>(null);
  const read = useRef(onRead);
  read.current = onRead;

  const act = useCallback((action: Without<Action, 'key'>) => {
    dispatch({ ...action, key });
  }, [key]);

  const fail = useCallback((failure: DmFailure) => {
    if (failure === 'not_found') act({ type: 'status', status: 'gone' });
    else if (failure === 'dms_off') act({ type: 'access', access: 'off' });
    else if (failure === 'own_sign_in_required') act({ type: 'access', access: 'own_sign_in_required' });
    else if (failure === 'unavailable') act({ type: 'access', access: 'unavailable' });
  }, [act]);

  // catchUp reads what changed above the revision held: a message, an edit or a
  // removal that came while a frame was missed.
  const catchUp = useCallback(async () => {
    let after = live.current.revision;
    for (let page = 0; page < 20; page++) {
      const result = await api.changes(conversation, after);
      if (!result.ok) {
        if (result.failure === 'not_found') act({ type: 'status', status: 'gone' });
        return;
      }
      act({ type: 'merge', messages: result.value.messages });
      const next = result.value.next_after;
      if (next === undefined || next <= after) return;
      after = next;
    }
  }, [api, conversation, act]);

  const load = useCallback(async () => {
    const [info, page] = await Promise.all([api.get(conversation), api.history(conversation, { order: 'desc' })]);
    if (!info.ok || !page.ok) {
      const failure = !info.ok ? info.failure : !page.ok ? page.failure : 'network';
      act({ type: 'status', status: failure === 'not_found' ? 'gone' : 'failed' });
      return;
    }
    act({ type: 'loaded', conversation: info.value, messages: [...page.value.messages].reverse(), older: page.value.next_before ?? null });
  }, [api, conversation, act]);

  useEffect(() => {
    dispatch({ type: 'reset', key });
    void load();
    // A frame for this conversation; other conversations are the inbox's.
    const stop = api.live((frame: DmFrame) => {
      switch (frame.type) {
        case 'message':
        case 'edited':
        case 'removed': {
          const message = frame.type === 'message' ? frame.message : frame.frame.message;
          if (message.conversation_id !== conversation) return;
          act({ type: 'merge', messages: [message] });
          if (message.revision > live.current.revision + 1) void catchUp();
          return;
        }
        case 'changed':
          if (frame.conversation === conversation) void catchUp();
          return;
        case 'snapshot': {
          const mine = frame.snapshot.conversations.find((c) => c.id === conversation);
          if (mine === undefined) return;
          act({ type: 'conversation', conversation: mine });
          if (mine.revision > live.current.revision) void catchUp();
          return;
        }
        case 'lost':
          return;
      }
    });
    const onFocus = () => {
      if (document.visibilityState === 'visible' && live.current.status === 'ready') void catchUp();
    };
    document.addEventListener('visibilitychange', onFocus);
    return () => {
      stop();
      document.removeEventListener('visibilitychange', onFocus);
    };
  }, [api, key, conversation, load, catchUp, act]);

  // The read mark follows the newest message the person can see here.
  const newest = useMemo(() => (state.messages.size === 0 ? 0 : Math.max(...state.messages.keys())), [state.messages]);
  useEffect(() => {
    if (state.status !== 'ready' || newest <= state.lastRead || document.visibilityState === 'hidden') return;
    if (reading.current?.key === key && reading.current.id >= newest) return;
    reading.current = { key, id: newest };
    void api.markRead(conversation, newest).then((result) => {
      if (!result.ok) {
        if (reading.current?.key === key) reading.current = null;
        return;
      }
      act({ type: 'read', lastRead: result.value.last_read_id });
      read.current?.(result.value);
    });
  }, [api, key, conversation, state.status, state.lastRead, newest, act]);

  const transmit = useCallback(async (nonce: string, body: string, fileIds: readonly number[]): Promise<boolean> => {
    const result = await api.send(conversation, { body, ...(fileIds.length === 0 ? {} : { file_ids: [...fileIds] }), client_nonce: nonce });
    if (result.ok) {
      act({ type: 'merge', messages: [result.message] });
      act({ type: 'discard', nonce });
      return true;
    }
    act({ type: 'pendingStatus', nonce, status: result.failure === 'unconfirmed' || result.failure === 'network' ? 'unconfirmed' : 'failed', failure: result.failure });
    if (result.failure !== 'unconfirmed') fail(result.failure);
    return false;
  }, [api, conversation, act, fail]);

  const send = useCallback(async (body: string, fileIds: readonly number[]): Promise<boolean> => {
    if (body.trim() === '') return false;
    const nonce = newNonce();
    act({ type: 'pending', pending: { nonce, body, fileIds, status: 'sending' } });
    return transmit(nonce, body, fileIds);
  }, [act, transmit]);

  const retry = useCallback(async (nonce: string) => {
    const held = live.current.pending.find((p) => p.nonce === nonce);
    if (held === undefined || held.status === 'sending') return;
    act({ type: 'pendingStatus', nonce, status: 'sending' });
    await transmit(nonce, held.body, held.fileIds);
  }, [act, transmit]);

  const edit = useCallback(async (message: number, body: string): Promise<DmFailure | null> => {
    const result = await api.edit(conversation, message, body);
    if (result.ok) {
      act({ type: 'merge', messages: [result.value] });
      return null;
    }
    fail(result.failure);
    return result.failure;
  }, [api, conversation, act, fail]);

  const remove = useCallback(async (message: number): Promise<DmFailure | null> => {
    const result = await api.remove(conversation, message);
    if (!result.ok) {
      fail(result.failure);
      return result.failure;
    }
    // The tombstone is the server's copy, with its revision.
    await catchUp();
    return null;
  }, [api, conversation, catchUp, fail]);

  const loadOlder = useCallback(async () => {
    const before = live.current.older;
    if (before === null || live.current.loadingOlder) return;
    act({ type: 'olderStart' });
    const result = await api.history(conversation, { order: 'desc', before });
    if (!result.ok) {
      act({ type: 'olderFailed' });
      return;
    }
    act({ type: 'olderDone', messages: result.value.messages, older: result.value.next_before ?? null });
  }, [api, conversation, act]);

  return {
    state,
    messages: useMemo(() => ordered(state.messages), [state.messages]),
    send,
    retry,
    discard: useCallback((nonce: string) => { act({ type: 'discard', nonce }); }, [act]),
    edit,
    remove,
    loadOlder,
    reload: useCallback(() => {
      act({ type: 'status', status: 'loading' });
      void load();
    }, [act, load]),
  };
}
