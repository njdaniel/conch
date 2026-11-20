// What the DM features share inside one signed-in page: the API, whose ids a
// cache holds (scope), who the viewer is, the inbox, and the drafts. It is not
// the voice client's, and nothing here reads or writes it: opening, switching
// or closing a conversation changes the middle of the page and never the
// voice destination (the voice strip is mounted beside it, untouched).
import { createContext, useContext, useMemo, type ReactNode } from 'react';
import { dmApi, type DmApi } from './api';
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
}

const DmContext = createContext<DmValue | null>(null);

// useDm is null outside a DmProvider (a guest, an agent, a page whose server
// has no DMs): every DM control then simply is not there.
export const useDm = (): DmValue | null => useContext(DmContext);

export function DmProvider({ scope, me, api = dmApi, children }: { readonly scope: string; readonly me: number; readonly api?: DmApi; readonly children: ReactNode }) {
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
  const value = useMemo<DmValue>(() => ({ api, scope, me, inbox, drafts }), [api, scope, me, inbox, drafts]);
  return <DmContext.Provider value={value}>{children}</DmContext.Provider>;
}
