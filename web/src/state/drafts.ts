// What has been typed and not sent, per channel. In memory for the tab, and
// written to the tab's sessionStorage by the page (draftStore.ts, issue
// #1039) so it survives a reload too, not closing the tab. Decision D2 of the
// P3 plan kept nothing on disk because the bearer lived in the tab; ADR-008's
// O1 has since let the sign-in last, so a draft in the tab is no wider.
import { rekey, without } from './messages';

export type Drafts = Readonly<Record<string, string>>;

export type DraftAction =
  | { readonly type: 'edit'; readonly channel: string; readonly text: string }
  // The channel `from` is now called `to` (issue #728): its draft goes with it.
  | { readonly type: 'renamed'; readonly from: string; readonly to: string }
  // The channel was lost (issue #1027): its draft goes with it.
  | { readonly type: 'forget'; readonly channel: string }
  // The drafts kept from a sign-in that ended by itself, given back to the
  // same person signing in again (issue #714). What was typed since wins.
  | { readonly type: 'restore'; readonly drafts: Drafts }
  | { readonly type: 'reset' };

export const initialDrafts: Drafts = {};

export function draftsReducer(state: Drafts, action: DraftAction): Drafts {
  switch (action.type) {
    case 'edit': {
      if (action.text === '') return Object.fromEntries(Object.entries(state).filter(([name]) => name !== action.channel));
      return { ...state, [action.channel]: action.text };
    }
    case 'renamed':
      return rekey(state, action.from, action.to);
    case 'forget':
      return without(state, action.channel);
    case 'restore':
      return { ...action.drafts, ...state };
    case 'reset':
      return initialDrafts;
  }
}

export function draftFor(state: Drafts, channel: string): string {
  return state[channel] ?? '';
}

// Who the next message of each channel goes to, kept beside the draft. A net is
// sticky until changed, as /net is in the TUI. A whisper is not: it is the
// choice of the next message only, and ends when that is sent.
export interface Target {
  // The net chosen, or null for the whole channel.
  readonly net: number | null;
  // The principals chosen for a whisper, or null when not whispering. An empty
  // list is the picker open with nobody chosen yet.
  readonly whisper: readonly number[] | null;
  // The server's line when it refused the last post here.
  readonly refusal: string | null;
  // A note to show beside the composer (the audit-log notice).
  readonly note: string | null;
  // A kept audience not yet given back (issue #1039): a whisper to nobody,
  // which cannot be sent, that Escape cannot close. Only the person choosing
  // who it goes to ends it, or the kept audience coming back whole.
  readonly held?: boolean;
  // An audience that came back from anywhere (issue #1039): the tab's
  // storage after a reload, the drafts kept across a sign-in, or a send a 401
  // gave back. Escape (closePicker) never changes it, so what came back can
  // never fall to the whole channel by a key. Only choosing the channel or a
  // net in the To: list ends it; who a whisper goes to can still be changed.
  readonly protected?: boolean;
  // The draft generation of the composer (issue #1066): the request id of the
  // send this composer's draft last went out as, for as long as the composer
  // still holds that same draft, with nothing typed or chosen since. Anything
  // written or chosen after the send ends it, so the send's answer can tell
  // its own draft from a newer one with the same recipients.
  readonly sentAs?: string;
}

export type Targets = Readonly<Record<string, Target>>;

export type TargetAction =
  | { readonly type: 'channel'; readonly channel: string }
  | { readonly type: 'net'; readonly channel: string; readonly net: number }
  | { readonly type: 'whisper'; readonly channel: string }
  | { readonly type: 'toggle'; readonly channel: string; readonly principal: number }
  // Escape in the picker: back to the net or channel chosen before.
  | { readonly type: 'closePicker'; readonly channel: string }
  // A post is starting: what was said about the last one goes. A send of the
  // composer's own draft carries its request id; a Retry from a pending line
  // does not, since it is not the draft the composer holds.
  | { readonly type: 'posting'; readonly channel: string; readonly requestId?: string }
  // The composer's text changed: whatever it holds now is a newer draft than
  // any send already on its way (issue #1066).
  | { readonly type: 'typed'; readonly channel: string }
  | { readonly type: 'refused'; readonly channel: string; readonly line: string }
  // The whisper sent as `requestId` was accepted: back to the whole channel,
  // with the notice if one is due, only if the composer still holds the very
  // draft that was sent (issue #1066). Never matched by recipients.
  | { readonly type: 'whispered'; readonly channel: string; readonly requestId: string; readonly note: string | null }
  // The channel `from` is now called `to` (issue #728): its choice goes with it.
  | { readonly type: 'renamed'; readonly from: string; readonly to: string }
  // The channel was lost (issue #1027): its choice goes with it.
  | { readonly type: 'forget'; readonly channel: string }
  // A kept audience not yet checked (issue #1039): a whisper to nobody, which
  // cannot be sent, with the line saying why.
  | { readonly type: 'held'; readonly channel: string; readonly line: string }
  // A kept audience that still stands, given back whole: it replaces the
  // choice, never merges into it, so it can only be what was kept.
  | { readonly type: 'restore'; readonly channel: string; readonly net: number | null; readonly whisper: readonly number[] | null }
  | { readonly type: 'reset' };

export const initialTargets: Targets = {};

const wholeChannel: Target = { net: null, whisper: null, refusal: null, note: null };

export function targetFor(state: Targets, channel: string): Target {
  return state[channel] ?? wholeChannel;
}

// The rule for the answer to a send (issue #1066): no completion handler ever
// widens the audience of a draft the composer holds. Success, a refusal, a
// 401, a Retry and a sign-in that ended may say how the send went, never who
// the next message goes to. The one change a completion may make is the end
// of a whisper on its own success ('whispered'), and only while the composer
// still holds the very draft it was sent as (Target.sentAs, the send's
// request id): emptied by the send, with nothing typed or chosen since. A
// newer draft to the same people is not that draft, so it keeps its whisper.
// Every other action here starts a newer draft, or keeps sentAs as it was.
export function targetsReducer(state: Targets, action: TargetAction): Targets {
  if (action.type === 'reset') return initialTargets;
  if (action.type === 'renamed') return rekey(state, action.from, action.to);
  if (action.type === 'forget') return without(state, action.channel);
  const current = targetFor(state, action.channel);
  const set = (next: Target): Targets => ({ ...state, [action.channel]: next });
  // A choice of who the draft goes to makes it a newer draft than any send.
  const chosen: Target = { ...current, sentAs: undefined };
  switch (action.type) {
    case 'channel':
      return set({ ...chosen, net: null, whisper: null, refusal: null, held: false, protected: false });
    case 'net':
      return set({ ...chosen, net: action.net, whisper: null, refusal: null, held: false, protected: false });
    case 'whisper':
      return set({ ...chosen, whisper: current.whisper ?? [], refusal: null, held: false });
    case 'toggle': {
      const picked = current.whisper ?? [];
      const next = picked.includes(action.principal) ? picked.filter((id) => id !== action.principal) : [...picked, action.principal];
      return set({ ...chosen, whisper: next, refusal: null, held: false });
    }
    case 'closePicker':
      // A held or protected draft stays as it is: closing the picker would
      // leave it addressed to the whole channel, which nobody chose.
      if (current.held === true || current.protected === true) return state;
      return set({ ...chosen, whisper: null, refusal: null });
    case 'typed':
      // Once per send, not per key: a draft already newer stays as it is.
      return current.sentAs === undefined ? state : set(chosen);
    case 'posting':
      return set(action.requestId === undefined ? { ...current, refusal: null, note: null } : { ...current, refusal: null, note: null, sentAs: action.requestId });
    case 'refused':
      return set({ ...current, refusal: action.line });
    case 'held':
      return set({ ...chosen, net: null, whisper: [], refusal: action.line, held: true, protected: true });
    case 'restore':
      return set({
        ...chosen,
        net: action.net,
        whisper: action.whisper === null ? null : [...action.whisper],
        refusal: null,
        held: false,
        protected: true,
      });
    case 'whispered': {
      // Only the draft this very send took: a newer one, to anyone, is left
      // as it is, with the notice beside it.
      const same = current.sentAs === action.requestId && current.whisper !== null;
      return set(same ? { ...chosen, net: null, whisper: null, note: action.note, protected: false } : { ...current, note: action.note });
    }
  }
}
