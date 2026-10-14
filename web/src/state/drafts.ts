// What has been typed and not sent, per channel. In memory for the tab: it
// survives switching channels, not a reload (decision D2 keeps nothing on disk).
export type Drafts = Readonly<Record<string, string>>;

export type DraftAction =
  | { readonly type: 'edit'; readonly channel: string; readonly text: string }
  | { readonly type: 'reset' };

export const initialDrafts: Drafts = {};

export function draftsReducer(state: Drafts, action: DraftAction): Drafts {
  switch (action.type) {
    case 'edit': {
      if (action.text === '') return Object.fromEntries(Object.entries(state).filter(([name]) => name !== action.channel));
      return { ...state, [action.channel]: action.text };
    }
    case 'reset':
      return initialDrafts;
  }
}

export function draftFor(state: Drafts, channel: string): string {
  return state[channel] ?? '';
}
