// The state of one open DM conversation: history oldest first, sends in
// flight, the read mark, and what the person may still do. The state is keyed
// by (scope, conversation): an answer that arrives for a key the hook has left
// is dropped by the reducer, so switching conversations, accounts or servers
// can never show another conversation's messages (no matter what is late).
import { useCallback, useEffect, useMemo, useReducer, useRef, useState, useSyncExternalStore } from 'react';
import type { DMConversationV1, DMMessageV1, DMReadStateV1 } from '../schema/generated';
import { isDmOperationServed, type DmApi, type DmFailure, type DmFrame } from './api';
import { canRetrySend, merge, ordered, pendingSends, revisionOf, settle, type Held, type Pending, type PendingSends } from './model';

// What the person may write while their retained history is still readable.
export type Access = 'ok' | 'unavailable' | 'off';

export interface TranscriptState {
  readonly key: string;
  readonly status: 'loading' | 'ready' | 'metadata' | 'failed' | 'gone' | 'own_sign_in_required';
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
  | { readonly type: 'status'; readonly status: 'loading' | 'failed' }
  | { readonly type: 'metadata'; readonly conversation: DMConversationV1 }
  | { readonly type: 'revoked'; readonly status: 'gone' | 'own_sign_in_required' }
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
    case 'metadata':
      return { ...state, status: 'metadata', conversation: action.conversation };
    case 'revoked':
      return { ...empty(state.key), status: action.status };
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
  readonly canWrite: boolean;
  readonly send: (body: string, fileIds: readonly number[]) => boolean;
  readonly retry: (nonce: string) => Promise<void>;
  readonly discard: (nonce: string) => void;
  readonly edit: (message: number, body: string) => Promise<DmFailure | null>;
  readonly remove: (message: number) => Promise<DmFailure | null>;
  readonly loadOlder: () => Promise<void>;
  readonly reload: () => void;
}

export function useDmTranscript(api: DmApi, scope: string, conversation: number, viewing: boolean, enabled: boolean, blocked: ReadonlySet<number>, onRead?: (state: DMReadStateV1) => void, kept?: PendingSends): Transcript {
  const key = `${scope}/${String(conversation)}`;
  const local = useMemo(() => pendingSends(), [key]);
  const sends = kept ?? local;
  const pending = useSyncExternalStore(sends.subscribe, sends.get);
  const recovery = useMemo(() => ({
    after: null as number | null,
    historyAvailable: null as boolean | null, // null until history support is known
    running: false,
    requested: false,
    active: false,
    revoked: false,
    load: 0,
  }), [api, key]);
  const [raw, dispatch] = useReducer(reduce, key, empty);
  const [visible, setVisible] = useState(() => document.visibilityState !== 'hidden');
  const [readPoll, setReadPoll] = useState(0);
  // Until the reset effect has run for a new key, show an empty state for it:
  // a render never shows the previous key's messages.
  const state = raw.key === key ? raw : empty(key);
  const live = useRef(state);
  live.current = state;
  const reading = useMemo(() => ({ id: 0, retry: false }), [api, key]);
  const read = useRef(onRead);
  read.current = onRead;

  const act = useCallback((action: Without<Action, 'key'>) => {
    if ('messages' in action) sends.update((held) => settle(held, action.messages));
    if (!recovery.active || (recovery.revoked && action.type !== 'revoked')) return;
    dispatch({ ...action, key });
  }, [key, sends, recovery]);

  const fail = useCallback((failure: DmFailure) => {
    if (failure === 'not_found' || failure === 'unauthorized' || failure === 'forbidden' || failure === 'own_sign_in_required') {
      recovery.revoked = true;
      recovery.after = null;
      act({ type: 'revoked', status: failure === 'own_sign_in_required' ? failure : 'gone' });
      return true;
    }
    if (failure === 'dms_off') act({ type: 'access', access: 'off' });
    else if (failure === 'unavailable') act({ type: 'access', access: 'unavailable' });
    return false;
  }, [act, recovery]);

  // Only history's baseline and successful changes pages advance this
  // cursor. A write reply or live frame can arrive ahead of unseen changes.
  const catchUp = useCallback(async () => {
    const active = () => recovery.active && !recovery.revoked;
    const requested = () => recovery.requested;
    if (!active()) return;
    if (recovery.running) {
      recovery.requested = true;
      return;
    }
    recovery.running = true;
    // A successful poll may leave the messages unchanged. Wake a failed read mark anyway.
    const retryRead = () => {
      if (reading.retry) setReadPoll((poll) => poll + 1);
    };
    try {
      for (let page = 0; page < 20 && recovery.after !== null; page++) {
        recovery.requested = false;
        const after: number = recovery.after;
        const result = await api.changes(conversation, after);
        if (!active()) return;
        if (!result.ok) {
          if (result.failure !== 'unsupported') {
            fail(result.failure);
            return;
          }
          recovery.after = null;
          break;
        }
        act({ type: 'merge', messages: result.value.messages });
        retryRead();
        recovery.after = revisionOf(result.value.messages, after);
        const more = result.value.next_after !== undefined && result.value.next_after > after;
        if (!more && !requested()) return;
      }
      if (recovery.after !== null) return;
      if (recovery.historyAvailable === null) {
        if (live.current.status === 'loading') recovery.requested = true;
        return;
      }
      recovery.requested = false;
      const [info, latest] = await Promise.all([
        api.get(conversation),
        recovery.historyAvailable ? api.history(conversation, { order: 'desc' }) : null,
      ]);
      if (!active()) return;
      if (info.ok) act({ type: 'conversation', conversation: info.value });
      else if (fail(info.failure)) return;
      // Merge the latest page without moving the cursor for earlier history.
      if (latest?.ok) act({ type: 'merge', messages: latest.value.messages });
      else if (latest !== null && fail(latest.failure)) return;
      if (info.ok || latest?.ok) retryRead();
    } finally {
      recovery.running = false;
    }
  }, [api, conversation, act, fail, reading, recovery]);

  const load = useCallback(async () => {
    // Read the baseline before history so changes concurrent with history
    // never fall below the recovery cursor.
    const request = ++recovery.load;
    const current = () => recovery.active && !recovery.revoked && recovery.load === request;
    const info = await api.get(conversation);
    if (!current()) return;
    if (!info.ok) {
      if (!fail(info.failure)) act({ type: 'status', status: 'failed' });
      return;
    }
    const page = await api.history(conversation, { order: 'desc' });
    if (!current()) return;
    if (!page.ok) {
      if (page.failure === 'unsupported') {
        recovery.historyAvailable = false;
        act({ type: 'metadata', conversation: info.value });
      } else if (!fail(page.failure)) act({ type: 'status', status: 'failed' });
      return;
    }
    recovery.historyAvailable = true;
    recovery.after = isDmOperationServed('listDmChanges') ? info.value.revision : null;
    act({ type: 'loaded', conversation: info.value, messages: [...page.value.messages].reverse(), older: page.value.next_before ?? null });
    if (recovery.requested || (recovery.after !== null && revisionOf(page.value.messages, recovery.after) > recovery.after)) void catchUp();
  }, [api, conversation, act, fail, recovery, catchUp]);

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
          if (recovery.after === null || message.revision > recovery.after + 1) void catchUp();
          return;
        }
        case 'changed':
          if (frame.conversation === conversation) void catchUp();
          return;
        case 'snapshot': {
          const mine = frame.snapshot.conversations.find((c) => c.id === conversation);
          if (mine === undefined) return;
          act({ type: 'conversation', conversation: mine });
          if (recovery.after === null || mine.revision > recovery.after) void catchUp();
          return;
        }
        case 'poll':
        case 'lost':
          void catchUp();
          return;
      }
    });
    const onFocus = () => {
      const visibleNow = document.visibilityState !== 'hidden';
      setVisible(visibleNow);
      if (visibleNow) void catchUp();
    };
    document.addEventListener('visibilitychange', onFocus);
    window.addEventListener('focus', onFocus);
    return () => {
      recovery.active = false;
      stop();
      document.removeEventListener('visibilitychange', onFocus);
      window.removeEventListener('focus', onFocus);
    };
  }, [api, key, conversation, load, catchUp, act, recovery]);

  // The read mark follows the newest message the person can see here.
  const messages = useMemo(() => ordered(state.messages), [state.messages]);
  const newest = messages.at(-1)?.id ?? 0;
  useEffect(() => {
    if (!isDmOperationServed('markDmRead')) return;
    if (state.status !== 'ready' || !viewing || !visible || newest <= state.lastRead) return;
    if (reading.id >= newest && !reading.retry) return;
    reading.id = newest;
    reading.retry = false;
    void api.markRead(conversation, newest).then((result) => {
      if (!recovery.active || recovery.revoked) return;
      if (!result.ok) {
        if (reading.id === newest) reading.retry = result.failure === 'network' || result.failure === 'rate_limited';
        fail(result.failure);
        return;
      }
      act({ type: 'read', lastRead: result.value.last_read_id });
      read.current?.(result.value);
    });
  }, [api, conversation, state.status, state.lastRead, newest, visible, viewing, act, fail, reading, readPoll, recovery]);

  const canWrite = enabled && state.status === 'ready' && state.access === 'ok' &&
    state.conversation !== null && !blocked.has(state.conversation.peer_id);

  const discard = useCallback((nonce: string) => {
    sends.update((held) => held.filter((p) => p.nonce !== nonce));
  }, [sends]);

  const transmit = useCallback(async (send: Pending) => {
    const { nonce, body, fileIds } = send;
    const result = await api.send(conversation, { body, ...(fileIds.length === 0 ? {} : { file_ids: [...fileIds] }), client_nonce: nonce });
    if (result.ok) {
      act({ type: 'merge', messages: [result.message] });
      discard(nonce);
      if (recovery.after !== null && result.message.revision > recovery.after + 1) void catchUp();
      return;
    }
    const status = result.failure === 'unconfirmed' || result.failure === 'network' ? 'unconfirmed' : 'failed';
    sends.update((held) => held.map((p) => p.nonce === nonce ? { ...p, status, failure: result.failure } : p));
    if (recovery.active && result.failure !== 'unconfirmed') fail(result.failure);
  }, [api, conversation, act, discard, recovery, catchUp, sends, fail]);

  const send = useCallback((body: string, fileIds: readonly number[]): boolean => {
    if (!canWrite || body.trim() === '') return false;
    // getRandomValues also works on a plain-HTTP LAN origin.
    const bytes = globalThis.crypto.getRandomValues(new Uint8Array(16));
    const nonce = Array.from(bytes, (byte) => byte.toString(16).padStart(2, '0')).join('');
    const pending: Pending = { nonce, body, fileIds, status: 'sending' };
    sends.update((held) => [...held, pending]);
    void transmit(pending);
    return true;
  }, [canWrite, sends, transmit]);

  const retry = useCallback(async (nonce: string) => {
    const send = sends.get().find((p) => p.nonce === nonce);
    if (!canWrite || send === undefined || !canRetrySend(send)) return;
    sends.update((held) => held.map((p) => p.nonce === nonce ? { ...p, status: 'sending' } : p));
    await transmit(send);
  }, [canWrite, sends, transmit]);

  const edit = useCallback(async (message: number, body: string): Promise<DmFailure | null> => {
    if (!canWrite) return 'unavailable';
    const result = await api.edit(conversation, message, body);
    if (!recovery.active || recovery.revoked) return null;
    if (result.ok) {
      act({ type: 'merge', messages: [result.value] });
      if (recovery.after !== null && result.value.revision > recovery.after + 1) void catchUp();
      return null;
    }
    fail(result.failure);
    return result.failure;
  }, [api, canWrite, conversation, act, fail, recovery, catchUp]);

  const remove = useCallback(async (message: number): Promise<DmFailure | null> => {
    const active = () => recovery.active && !recovery.revoked;
    const result = await api.remove(conversation, message);
    if (!active()) return null;
    if (!result.ok) {
      fail(result.failure);
      return result.failure;
    }
    if (recovery.after !== null) await catchUp();
    if (!active() || recovery.after !== null) return null;
    // A message removed from an earlier page may be outside the latest page.
    // History cursors must name an existing message in the conversation.
    const held = ordered(live.current.messages);
    const at = held.findIndex((copy) => copy.id === message);
    const newer = held[at + 1];
    const page = await api.history(conversation, newer === undefined
      ? { order: 'asc', after: held[at - 1]?.id }
      : { order: 'desc', before: newer.id });
    if (!active()) return null;
    if (page.ok) {
      const tombstone = page.value.messages.find((copy) => copy.id === message && copy.removed === true);
      if (tombstone !== undefined) act({ type: 'merge', messages: [tombstone] });
    } else fail(page.failure);
    return null;
  }, [api, conversation, act, catchUp, fail, recovery]);

  const loadOlder = useCallback(async () => {
    const before = live.current.older;
    if (before === null || live.current.loadingOlder) return;
    act({ type: 'olderStart' });
    const result = await api.history(conversation, { order: 'desc', before });
    if (!recovery.active || recovery.revoked) return;
    if (!result.ok) {
      if (!fail(result.failure)) act({ type: 'olderFailed' });
      return;
    }
    act({ type: 'olderDone', messages: result.value.messages, older: result.value.next_before ?? null });
  }, [api, conversation, act, fail, recovery]);

  return {
    state,
    pending,
    messages,
    canWrite,
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
