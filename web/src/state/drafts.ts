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
  // A whisper given back after a reload or a sign-in whose net under it is no
  // longer one the person may transmit to (issue #1039): Escape keeps the
  // whisper rather than leave the text addressed to the whole channel, where
  // before the reload it would have gone back to that net. Choosing the
  // channel or a net ends it.
  readonly netGone?: boolean;
}

export type Targets = Readonly<Record<string, Target>>;

export type TargetAction =
  | { readonly type: 'channel'; readonly channel: string }
  | { readonly type: 'net'; readonly channel: string; readonly net: number }
  | { readonly type: 'whisper'; readonly channel: string }
  | { readonly type: 'toggle'; readonly channel: string; readonly principal: number }
  // Escape in the picker: back to the net or channel chosen before.
  | { readonly type: 'closePicker'; readonly channel: string }
  // A post is starting: what was said about the last one goes.
  | { readonly type: 'posting'; readonly channel: string }
  | { readonly type: 'refused'; readonly channel: string; readonly line: string }
  // A whisper to `sent` was accepted: back to the whole channel, with the
  // notice if one is due. Only if that is still the whisper being composed.
  | { readonly type: 'whispered'; readonly channel: string; readonly sent: readonly number[]; readonly note: string | null }
  // The channel `from` is now called `to` (issue #728): its choice goes with it.
  | { readonly type: 'renamed'; readonly from: string; readonly to: string }
  // The channel was lost (issue #1027): its choice goes with it.
  | { readonly type: 'forget'; readonly channel: string }
  // A kept audience not yet checked (issue #1039): a whisper to nobody, which
  // cannot be sent, with the line saying why.
  | { readonly type: 'held'; readonly channel: string; readonly line: string }
  // A kept audience that still stands, given back whole: it replaces the
  // choice, never merges into it, so it can only be what was kept.
  | { readonly type: 'restore'; readonly channel: string; readonly net: number | null; readonly whisper: readonly number[] | null; readonly netGone?: boolean }
  | { readonly type: 'reset' };

export const initialTargets: Targets = {};

const wholeChannel: Target = { net: null, whisper: null, refusal: null, note: null };

export function targetFor(state: Targets, channel: string): Target {
  return state[channel] ?? wholeChannel;
}

export function targetsReducer(state: Targets, action: TargetAction): Targets {
  if (action.type === 'reset') return initialTargets;
  if (action.type === 'renamed') return rekey(state, action.from, action.to);
  if (action.type === 'forget') return without(state, action.channel);
  const current = targetFor(state, action.channel);
  const set = (next: Target): Targets => ({ ...state, [action.channel]: next });
  switch (action.type) {
    case 'channel':
      return set({ ...current, net: null, whisper: null, refusal: null, held: false, netGone: false });
    case 'net':
      return set({ ...current, net: action.net, whisper: null, refusal: null, held: false, netGone: false });
    case 'whisper':
      return set({ ...current, whisper: current.whisper ?? [], refusal: null, held: false });
    case 'toggle': {
      const chosen = current.whisper ?? [];
      const next = chosen.includes(action.principal) ? chosen.filter((id) => id !== action.principal) : [...chosen, action.principal];
      return set({ ...current, whisper: next, refusal: null, held: false });
    }
    case 'closePicker':
      // A held draft stays held: closing the picker would leave it addressed
      // to the whole channel, which nobody chose.
      if (current.held === true || current.netGone === true) return state;
      return set({ ...current, whisper: null, refusal: null });
    case 'posting':
      return set({ ...current, refusal: null, note: null });
    case 'refused':
      return set({ ...current, refusal: action.line });
    case 'held':
      return set({ ...current, net: null, whisper: [], refusal: action.line, held: true });
    case 'restore':
      return set({
        ...current,
        net: action.net,
        whisper: action.whisper === null ? null : [...action.whisper],
        refusal: null,
        held: false,
        netGone: action.netGone === true,
      });
    case 'whispered': {
      // Only if the whisper still chosen is the one that was sent: a second one
      // being composed while the first was on its way is left as it is.
      const same =
        current.whisper !== null &&
        current.whisper.length === action.sent.length &&
        action.sent.every((id) => current.whisper?.includes(id) === true);
      return set(same ? { ...current, net: null, whisper: null, note: action.note, netGone: false } : { ...current, note: action.note });
    }
  }
}
