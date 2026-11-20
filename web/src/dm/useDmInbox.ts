// The DM inbox: the person's conversations, unread counts, and the choice of
// which one is open. State is keyed by scope (origin and account): a scope
// change drops everything and ignores any answer still on its way, because ids
// of one server mean nothing on another.
//
// 'unsupported' is a server that does not serve DMs (it has no such route yet):
// nothing is offered and nothing is retried. 'off' is DMs switched off for
// this person or the server (the snapshot says so without saying which).
import { useCallback, useEffect, useMemo, useReducer, useRef } from 'react';
import type { DMConversationV1, DMReadStateV1, DMSnapshotV1, DMState } from '../schema/generated';
import type { DmApi, DmFailure, DmFrame, DmResult } from './api';

export type InboxStatus = 'loading' | 'ready' | 'failed' | 'unsupported' | 'own_sign_in_required';

export interface InboxState {
  readonly scope: string;
  readonly status: InboxStatus;
  readonly conversations: readonly DMConversationV1[];
  readonly enabled: boolean;
  readonly selected: number | null;
}

const start = (scope: string): InboxState => ({ scope, status: 'loading', conversations: [], enabled: true, selected: null });

// Omit that keeps the union's members apart.
type Without<A, K extends string> = A extends unknown ? Omit<A, K> : never;

type Action = { readonly scope: string } & (
  | { readonly type: 'reset' }
  | { readonly type: 'snapshot'; readonly conversations: readonly DMConversationV1[]; readonly enabled: boolean }
  | { readonly type: 'status'; readonly status: InboxStatus }
  | { readonly type: 'upsert'; readonly conversation: DMConversationV1 }
  | { readonly type: 'read'; readonly state: DMReadStateV1 }
  | { readonly type: 'select'; readonly id: number | null }
  | { readonly type: 'drop'; readonly id: number }
);

// Most recent activity first; a conversation with no message sorts by its
// creation, last.
const recent = (c: DMConversationV1): number => Date.parse(c.last_message_at ?? c.created_at ?? '') || 0;
const byRecent = (a: DMConversationV1, b: DMConversationV1): number => recent(b) - recent(a) || b.id - a.id;

// A copy of a conversation with a lower revision never replaces a higher one.
function put(list: readonly DMConversationV1[], next: DMConversationV1): readonly DMConversationV1[] {
  const held = list.find((c) => c.id === next.id);
  if (held !== undefined && held.revision > next.revision) return list;
  return [...list.filter((c) => c.id !== next.id), next].sort(byRecent);
}

export function reduceInbox(state: InboxState, action: Action): InboxState {
  if (action.type === 'reset') return state.scope === action.scope ? state : start(action.scope);
  if (action.scope !== state.scope) return state;
  switch (action.type) {
    case 'snapshot':
      return { ...state, status: 'ready', conversations: [...action.conversations].sort(byRecent), enabled: action.enabled };
    case 'status':
      return { ...state, status: action.status };
    case 'upsert':
      return { ...state, conversations: put(state.conversations, action.conversation) };
    case 'read':
      return {
        ...state,
        conversations: state.conversations.map((c) => (c.id === action.state.conversation_id && action.state.last_read_id >= c.last_read_id ? { ...c, last_read_id: action.state.last_read_id, unread: action.state.unread } : c)),
      };
    case 'select':
      return state.selected === action.id ? state : { ...state, selected: action.id };
    case 'drop':
      return { ...state, conversations: state.conversations.filter((c) => c.id !== action.id), selected: state.selected === action.id ? null : state.selected };
  }
}

export interface Inbox {
  readonly state: InboxState;
  readonly unread: number;
  readonly open: (peer: number) => Promise<DmResult<DMConversationV1>>;
  readonly select: (id: number | null) => void;
  readonly refresh: () => void;
  readonly setState: (conversation: DMConversationV1, state: DMState) => Promise<DmFailure | null>;
  readonly read: (state: DMReadStateV1) => void;
}

export function useDmInbox(api: DmApi, scope: string): Inbox {
  const [raw, dispatch] = useReducer(reduceInbox, scope, start);
  const state = raw.scope === scope ? raw : start(scope);
  const act = useCallback((action: Without<Action, 'scope'>) => {
    dispatch({ ...action, scope });
  }, [scope]);
  const held = useRef(state);
  held.current = state;

  const requests = useMemo(() => ({ snapshot: 0, open: 0, running: null as number | null }), [api, scope]);

  const applySnapshot = useCallback(async (snapshot: DMSnapshotV1, isCurrent: () => boolean) => {
    const rows = new Map(snapshot.conversations.map((c) => [c.id, c]));
    if (snapshot.more) {
      for (const state of ['open', 'archived'] as const) {
        const seen = new Set<string>();
        let cursor: string | undefined;
        for (;;) {
          const page = await api.list(state, cursor);
          if (!isCurrent()) return;
          if (!page.ok || (page.value.next_cursor && seen.has(page.value.next_cursor))) {
            if (held.current.status !== 'ready') act({ type: 'status', status: 'failed' });
            return;
          }
          for (const c of page.value.conversations) {
            if ((rows.get(c.id)?.revision ?? 0) <= c.revision) rows.set(c.id, c);
          }
          cursor = page.value.next_cursor;
          if (!cursor) break;
          seen.add(cursor);
        }
      }
    }
    if (isCurrent()) act({ type: 'snapshot', conversations: [...rows.values()], enabled: snapshot.dms_enabled });
  }, [api, act]);

  const refresh = useCallback(() => {
    if (requests.running !== null) return;
    const version = ++requests.snapshot;
    requests.running = version;
    const isCurrent = () => version === requests.snapshot;
    void api.snapshot().then(async (result) => {
      if (!isCurrent()) return;
      if (result.ok) await applySnapshot(result.value, isCurrent);
      else if (result.failure === 'own_sign_in_required') act({ type: 'status', status: 'own_sign_in_required' });
      // A server that has no DM routes answers not found: nothing to offer.
      else if (result.failure === 'not_found' || result.failure === 'refused') act({ type: 'status', status: 'unsupported' });
      else if (result.failure !== 'unauthorized' && held.current.status !== 'ready') act({ type: 'status', status: 'failed' });
    }).finally(() => {
      if (requests.running === version) requests.running = null;
    });
  }, [api, act, applySnapshot, requests]);

  useEffect(() => {
    dispatch({ type: 'reset', scope });
    refresh();
    const sync = () => {
      if (held.current.status !== 'unsupported' && held.current.status !== 'own_sign_in_required') refresh();
    };
    const stop = api.live((frame: DmFrame) => {
      if (frame.type === 'snapshot') {
        const version = ++requests.snapshot;
        void applySnapshot(frame.snapshot, () => version === requests.snapshot);
      } else sync();
    });
    const onFocus = () => {
      if (document.visibilityState === 'visible') sync();
    };
    document.addEventListener('visibilitychange', onFocus);
    return () => {
      requests.open++;
      requests.snapshot++;
      requests.running = null;
      stop();
      document.removeEventListener('visibilitychange', onFocus);
    };
  }, [api, scope, refresh, applySnapshot, requests]);

  const select = useCallback((id: number | null) => {
    requests.open++;
    act({ type: 'select', id });
  }, [requests, act]);

  const open = useCallback(async (peer: number): Promise<DmResult<DMConversationV1>> => {
    const attempt = ++requests.open;
    const result = await api.open(peer);
    if (result.ok && attempt === requests.open) {
      requests.snapshot++;
      act({ type: 'upsert', conversation: result.value });
      act({ type: 'select', id: result.value.id });
    }
    return result;
  }, [api, act, requests]);

  return {
    state,
    unread: state.conversations.reduce((sum, c) => sum + c.unread, 0),
    open,
    select,
    refresh,
    setState: useCallback(async (conversation: DMConversationV1, next: DMState): Promise<DmFailure | null> => {
      const result = await api.setState(conversation.id, next);
      if (!result.ok) return result.failure;
      requests.snapshot++;
      if (next === 'hidden') act({ type: 'drop', id: conversation.id });
      else act({ type: 'upsert', conversation: result.value });
      return null;
    }, [api, act, requests]),
    read: useCallback((read: DMReadStateV1) => { act({ type: 'read', state: read }); }, [act]),
  };
}
