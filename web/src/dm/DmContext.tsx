// What the DM features share inside one signed-in page: the API, whose ids a
// cache holds (scope), who the viewer is, the inbox, and the drafts. It is not
// the voice client's, and nothing here reads or writes it: opening, switching
// or closing a conversation changes the middle of the page and never the
// voice destination (the voice strip is mounted beside it, untouched).
import { createContext, useContext, useMemo, useSyncExternalStore, type ReactNode } from 'react';
import { createDmSession, type DmApi } from './api';
import { pendingSends, type PendingSends } from './model';
import { useDmInbox, type Inbox } from './useDmInbox';

export interface DmDrafts {
  get(conversation: number): string;
  set(conversation: number, text: string): void;
}

export interface DmValue {
  readonly api: DmApi;
  readonly scope: string;
  readonly me: number;
  readonly inbox: Inbox;
  readonly drafts: DmDrafts;
  readonly sendsFor: (conversation: number) => PendingSends;
}

export const DmContext = createContext<DmValue | null>(null);
export const DmOpened = createContext<((opener: HTMLElement) => void) | null>(null);

// A signed-in page supplies the scope and its API. A surface outside that
// provider has no DM actions.
export const useDm = (): DmValue | null => useContext(DmContext);

export function DmProvider({ scope, me, api, children }: { readonly scope: string; readonly me: number; readonly api?: DmApi; readonly children: ReactNode }) {
  const { dm } = useDmValue(true, scope, me, api);
  return <DmContext.Provider value={dm}>{children}</DmContext.Provider>;
}

const noSubscription = () => () => undefined;
const notDenied = () => false;

export function useDmValue(allowed: boolean, scope: string, me: number, source?: DmApi) {
  const session = useMemo(() => allowed && source === undefined ? createDmSession() : null, [allowed, source, scope]);
  const denied = useSyncExternalStore(session?.subscribe ?? noSubscription, session?.denied ?? notDenied);
  const api = allowed && !denied ? session?.api ?? source ?? null : null;
  const inbox = useDmInbox(api, scope);
  // Drafts are per conversation (kind and id, never an id alone) and live as
  // long as the scope: an account or server change drops them.
  const drafts = useMemo<DmDrafts>(() => {
    const held = new Map<string, string>();
    return {
      get: (conversation) => held.get(`dm:${String(conversation)}`) ?? '',
      set: (conversation, text) => {
        if (text === '') held.delete(`dm:${String(conversation)}`);
        else held.set(`dm:${String(conversation)}`, text);
      },
    };
  }, [scope]);
  const sendsFor = useMemo(() => {
    const held = new Map<number, PendingSends>();
    return (conversation: number) => {
      const existing = held.get(conversation);
      if (existing !== undefined) return existing;
      const sends = pendingSends();
      held.set(conversation, sends);
      return sends;
    };
  }, [scope]);
  const dm: DmValue | null = api === null ? null : { api, scope, me, inbox, drafts, sendsFor };
  return { dm, selectDm: inbox.select };
}
