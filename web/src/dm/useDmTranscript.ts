// The state of one open DM conversation: history oldest first, sends in
// flight, the read mark, and what the person may still do. The state is keyed
// by (scope, conversation): an answer that arrives for a key the hook has left
// is dropped by the reducer, so switching conversations, accounts or servers
// can never show another conversation's messages (no matter what is late).
import { useCallback, useEffect, useMemo, useReducer, useRef, useSyncExternalStore } from 'react';
import type { DMConversationV1, DMMessageV1, DMReadStateV1 } from '../schema/generated';
import type { DmApi, DmFailure, DmFrame } from './api';
import { merge, ordered, pendingSends, revisionOf, settle, type Held, type Pending, type PendingSends } from './model';

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
  readonly lastRead: number;
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
  lastRead: 0,
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
        lastRead: Math.max(state.lastRead, action.conversation.last_read_id),
      };
    }
    case 'status':
      return { ...state, status: action.status };
    case 'olderStart':
      return { ...state, loadingOlder: true, olderFailed: false };
    case 'olderDone':
      return { ...state, loadingOlder: false, messages: merge(state.messages, action.messages), older: action.older };
    case 'olderFailed':
      return { ...state, loadingOlder: false, olderFailed: true };
    case 'merge': {
      const messages = merge(state.messages, action.messages);
      return messages === state.messages ? state : { ...state, messages };
    }
    case 'conversation':
      return { ...state, conversation: action.conversation };
    case 'read':
      return action.lastRead > state.lastRead ? { ...state, lastRead: action.lastRead } : state;
    case 'access':
      return state.access === action.access ? state : { ...state, access: action.access };
  }
}

export interface Transcript {
  readonly state: TranscriptState;
  readonly messages: readonly DMMessageV1[];
  readonly pending: readonly Pending[];
  readonly send: (body: string, fileIds: readonly number[]) => Promise<boolean>;
  readonly retry: (nonce: string) => Promise<void>;
  readonly discard: (nonce: string) => void;
  readonly edit: (message: number, body: string) => Promise<DmFailure | null>;
  readonly remove: (message: number) => Promise<DmFailure | null>;
  readonly loadOlder: () => Promise<void>;
  readonly reload: () => void;
}

const newNonce = (): string => globalThis.crypto.randomUUID();

export function useDmTranscript(api: DmApi, scope: string, conversation: number, onRead?: (state: DMReadStateV1) => void, kept?: PendingSends): Transcript {
  const key = `${scope}/${String(conversation)}`;
  const local = useMemo(() => pendingSends(), [key]);
  const sends = kept ?? local;
  const pending = useSyncExternalStore(sends.subscribe, sends.get);
  const recovery = useMemo(() => ({ after: null as number | null, running: false, requested: false, active: false, load: 0 }), [api, key]);
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
    if ('messages' in action) sends.update((held) => settle(held, action.messages));
    dispatch({ ...action, key });
  }, [key, sends]);

  const fail = useCallback((failure: DmFailure) => {
    if (failure === 'not_found') act({ type: 'status', status: 'gone' });
    else if (failure === 'dms_off') act({ type: 'access', access: 'off' });
    else if (failure === 'own_sign_in_required') act({ type: 'access', access: 'own_sign_in_required' });
    else if (failure === 'unavailable') act({ type: 'access', access: 'unavailable' });
  }, [act]);

  // Only history's baseline and successful changes pages advance this
  // cursor. A write reply or live frame can arrive ahead of unseen changes.
  const catchUp = useCallback(async () => {
    const active = () => recovery.active;
    const requested = () => recovery.requested;
    if (recovery.after === null || !active()) return;
    if (recovery.running) {
      recovery.requested = true;
      return;
    }
    recovery.running = true;
    try {
      for (let page = 0; page < 20 && active(); page++) {
        recovery.requested = false;
        const after: number = recovery.after;
        const result = await api.changes(conversation, after);
        if (!active()) return;
        if (!result.ok) {
          if (result.failure === 'not_found') {
            recovery.after = null;
            act({ type: 'status', status: 'gone' });
          }
          return;
        }
        act({ type: 'merge', messages: result.value.messages });
        recovery.after = revisionOf(result.value.messages, after);
        const more = result.value.next_after !== undefined && result.value.next_after > after;
        if (!more && !requested()) return;
      }
    } finally {
      recovery.running = false;
    }
  }, [api, conversation, act, recovery]);

  const load = useCallback(async () => {
    // Read the baseline before history so changes concurrent with history
    // never fall below the recovery cursor.
    const request = ++recovery.load;
    const current = () => recovery.active && recovery.load === request;
    const info = await api.get(conversation);
    if (!current()) return;
    const page = info.ok ? await api.history(conversation, { order: 'desc' }) : info;
    if (!current()) return;
    if (!info.ok || !page.ok) {
      const failure = !page.ok ? page.failure : 'network';
      act({ type: 'status', status: failure === 'not_found' ? 'gone' : 'failed' });
      return;
    }
    recovery.after = info.value.revision;
    act({ type: 'loaded', conversation: info.value, messages: [...page.value.messages].reverse(), older: page.value.next_before ?? null });
    if (revisionOf(page.value.messages, info.value.revision) > info.value.revision) void catchUp();
  }, [api, conversation, act, recovery, catchUp]);

  useEffect(() => {
    dispatch({ type: 'reset', key });
    recovery.active = true;
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
          if (recovery.after !== null && message.revision > recovery.after + 1) void catchUp();
          return;
        }
        case 'changed':
          if (frame.conversation === conversation) void catchUp();
          return;
        case 'snapshot': {
          const mine = frame.snapshot.conversations.find((c) => c.id === conversation);
          if (mine === undefined) return;
          act({ type: 'conversation', conversation: mine });
          if (recovery.after !== null && mine.revision > recovery.after) void catchUp();
          return;
        }
        case 'poll':
        case 'lost':
          void catchUp();
          return;
      }
    });
    const onFocus = () => {
      if (document.visibilityState === 'visible' && live.current.status === 'ready') void catchUp();
    };
    document.addEventListener('visibilitychange', onFocus);
    return () => {
      recovery.active = false;
      stop();
      document.removeEventListener('visibilitychange', onFocus);
    };
  }, [api, key, conversation, load, catchUp, act, recovery]);

  // The read mark follows the newest message the person can see here.
  const newest = useMemo(() => (state.messages.size === 0 ? 0 : Math.max(...state.messages.keys())), [state.messages]);
  useEffect(() => {
    if (state.status !== 'ready' || newest <= state.lastRead || document.visibilityState === 'hidden') return;
    if (reading.current?.key === key && reading.current.id >= newest) return;
    reading.current = { key, id: newest };
    void api.markRead(conversation, newest).then((result) => {
      if (!recovery.active) return;
      if (!result.ok) {
        if (reading.current?.key === key) reading.current = null;
        return;
      }
      act({ type: 'read', lastRead: result.value.last_read_id });
      read.current?.(result.value);
    });
  }, [api, key, conversation, state.status, state.lastRead, newest, act, recovery]);

  const discard = useCallback((nonce: string) => {
    sends.update((held) => held.filter((p) => p.nonce !== nonce));
  }, [sends]);

  const transmit = useCallback(async (send: Pending): Promise<boolean> => {
    const { nonce, body, fileIds } = send;
    const result = await api.send(conversation, { body, ...(fileIds.length === 0 ? {} : { file_ids: [...fileIds] }), client_nonce: nonce });
    if (result.ok) {
      act({ type: 'merge', messages: [result.message] });
      discard(nonce);
      if (recovery.after !== null && result.message.revision > recovery.after + 1) void catchUp();
      return true;
    }
    const status = result.failure === 'unconfirmed' || result.failure === 'network' ? 'unconfirmed' : 'failed';
    sends.update((held) => held.map((p) => p.nonce === nonce ? { ...p, status, failure: result.failure } : p));
    if (result.failure !== 'unconfirmed') fail(result.failure);
    return false;
  }, [api, conversation, act, discard, recovery, catchUp, sends, fail]);

  const send = useCallback(async (body: string, fileIds: readonly number[]): Promise<boolean> => {
    if (body.trim() === '') return false;
    const pending: Pending = { nonce: newNonce(), body, fileIds, status: 'sending' };
    sends.update((held) => [...held, pending]);
    return transmit(pending);
  }, [sends, transmit]);

  const retry = useCallback(async (nonce: string) => {
    const send = sends.get().find((p) => p.nonce === nonce);
    if (send === undefined || send.status === 'sending') return;
    sends.update((held) => held.map((p) => p.nonce === nonce ? { ...p, status: 'sending' } : p));
    await transmit(send);
  }, [sends, transmit]);

  const edit = useCallback(async (message: number, body: string): Promise<DmFailure | null> => {
    const result = await api.edit(conversation, message, body);
    if (result.ok) {
      act({ type: 'merge', messages: [result.value] });
      if (recovery.after !== null && result.value.revision > recovery.after + 1) void catchUp();
      return null;
    }
    fail(result.failure);
    return result.failure;
  }, [api, conversation, act, fail, recovery, catchUp]);

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
    pending,
    messages: useMemo(() => ordered(state.messages), [state.messages]),
    send,
    retry,
    discard,
    edit,
    remove,
    loadOlder,
    reload: useCallback(() => {
      act({ type: 'status', status: 'loading' });
      void load();
    }, [act, load]),
  };
}
