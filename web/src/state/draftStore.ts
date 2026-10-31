// What has been typed and not sent, kept for the tab across a reload (issue
// #1039). It is in sessionStorage, per tab: a reload or a tab the browser
// restores finds it, closing the tab ends it. It holds only text, by the
// signed-in person's id and the channel's id (so a rename keeps it), and an
// open edit by message id. Never a credential, token or code.
//
// Every access is guarded: with storage refused the page keeps its drafts in
// memory, as before, and says nothing.
import { MAX_POST_REQUEST_BYTES } from '../schema/message';

const key = 'conch.web.drafts';

export interface StoredEdit {
  readonly message: number;
  readonly text: string;
}

// Who the next message of a channel goes to when not the whole channel: a net,
// or the principals of a whisper (an empty list is the picker open with
// nobody chosen). Kept so a draft meant for a few is never sent to everyone.
export interface StoredTarget {
  readonly net: number | null;
  readonly whisper: readonly number[] | null;
}

// storedAudience is who a message from this choice would go to, as the
// composer sends it (App's audienceForTarget): an open whisper wins over the
// net under it. Only that is kept, so what comes back can never be wider.
export function storedAudience(target: { readonly net: number | null; readonly whisper: readonly number[] | null }): StoredTarget | null {
  if (target.whisper !== null) return { net: null, whisper: [...target.whisper] };
  if (target.net !== null) return { net: target.net, whisper: null };
  return null;
}

export interface Stored {
  readonly principal: number;
  // Channel id to draft.
  readonly drafts: Readonly<Record<string, string>>;
  // Channel id to its audience, when not the whole channel.
  readonly targets: Readonly<Record<string, StoredTarget>>;
  // Channel id to the edit open there.
  readonly edits: Readonly<Record<string, StoredEdit>>;
}

// The room a stored draft leaves for the rest of a post request (the
// audience, a payload, the keys): MAX_POST_REQUEST_BYTES holds the whole JSON
// request, not the text.
export const envelopeBytes = 64 * 1024;
export const draftBudgetBytes = MAX_POST_REQUEST_BYTES - envelopeBytes;

// jsonBytes is what the text costs inside the request: its UTF-8 bytes once
// JSON has escaped it (quotes, backslashes and control characters grow).
function jsonBytes(text: string, encoder: TextEncoder): number {
  return encoder.encode(JSON.stringify(text)).length;
}

// capped is the text kept to the budget above, cut only between code points
// so an emoji is never split, and so a draft that is kept can be sent.
export function capped(text: string): string {
  const encoder = new TextEncoder();
  if (jsonBytes(text, encoder) <= draftBudgetBytes) return text;
  // The cut is moved off the middle of a surrogate pair, which keeps the size
  // growing with the cut and so makes the search sound.
  const snap = (at: number): number => {
    const before = text.charCodeAt(at - 1);
    const after = text.charCodeAt(at);
    return at > 0 && before >= 0xd800 && before <= 0xdbff && after >= 0xdc00 && after <= 0xdfff ? at - 1 : at;
  };
  let low = 0;
  let high = text.length;
  while (low < high) {
    const mid = snap(Math.ceil((low + high) / 2));
    if (mid <= low) break;
    if (jsonBytes(text.slice(0, mid), encoder) <= draftBudgetBytes) low = mid;
    else high = snap(mid - 1);
  }
  return text.slice(0, low);
}

function isRecord(value: unknown): value is Record<string, unknown> {
  return typeof value === 'object' && value !== null && !Array.isArray(value);
}

function targetOf(value: unknown): StoredTarget | null {
  if (!isRecord(value)) return null;
  const net = typeof value.net === 'number' ? value.net : null;
  const whisper = Array.isArray(value.whisper) && value.whisper.every((id): id is number => typeof id === 'number') ? value.whisper : null;
  return storedAudience({ net, whisper });
}

function read(): Stored | null {
  try {
    const raw = window.sessionStorage.getItem(key);
    if (raw === null) return null;
    const parsed: unknown = JSON.parse(raw);
    if (!isRecord(parsed) || typeof parsed.principal !== 'number') return null;
    const drafts: Record<string, string> = {};
    if (isRecord(parsed.drafts)) {
      for (const [id, text] of Object.entries(parsed.drafts)) if (typeof text === 'string' && text !== '') drafts[id] = text;
    }
    const targets: Record<string, StoredTarget> = {};
    if (isRecord(parsed.targets)) {
      for (const [id, value] of Object.entries(parsed.targets)) {
        const target = targetOf(value);
        if (target !== null) targets[id] = target;
      }
    }
    const edits: Record<string, StoredEdit> = {};
    if (isRecord(parsed.edits)) {
      for (const [id, edit] of Object.entries(parsed.edits)) {
        if (isRecord(edit) && typeof edit.message === 'number' && typeof edit.text === 'string') edits[id] = { message: edit.message, text: edit.text };
      }
    }
    return { principal: parsed.principal, drafts, targets, edits };
  } catch {
    return null;
  }
}

// write keeps the snapshot. If it cannot (refused, or the quota is full) the
// old one is removed rather than left: it would bring back text that has
// since been sent or deleted, and the page goes on in memory.
function write(stored: Stored): void {
  try {
    if (Object.keys(stored.drafts).length === 0 && Object.keys(stored.targets).length === 0 && Object.keys(stored.edits).length === 0) {
      window.sessionStorage.removeItem(key);
      return;
    }
    window.sessionStorage.setItem(key, JSON.stringify(stored));
  } catch {
    clearStored();
  }
}

// clearStored removes everything kept, at sign-out and when the person signing
// in is not the one who typed it.
export function clearStored(): void {
  try {
    window.sessionStorage.removeItem(key);
  } catch {
    // Nothing stored that could be removed.
  }
}

// loadStored is what this person left in the tab. Anyone else's is removed.
export function loadStored(principal: number): Stored {
  const stored = read();
  if (stored === null || stored.principal !== principal) {
    if (stored !== null) clearStored();
    return { principal, drafts: {}, targets: {}, edits: {} };
  }
  return stored;
}

// saveDrafts replaces the kept drafts and audiences (by channel id), leaving
// open edits.
export function saveDrafts(principal: number, drafts: Readonly<Record<number, string>>, targets: Readonly<Record<number, StoredTarget>> = {}): void {
  const was = loadStored(principal);
  const nextDrafts: Record<string, string> = {};
  for (const [id, text] of Object.entries(drafts)) if (text !== '') nextDrafts[id] = capped(text);
  write({ principal, drafts: nextDrafts, targets: { ...targets }, edits: was.edits });
}

const without = <T,>(kept: Readonly<Record<string, T>>, keep: (id: string) => boolean): Record<string, T> =>
  Object.fromEntries(Object.entries(kept).filter(([id]) => keep(id)));

// saveEdit keeps the edit open in a channel, or with null forgets it.
export function saveEdit(principal: number, channelId: number, edit: StoredEdit | null): void {
  const was = loadStored(principal);
  const edits = without(was.edits, (id) => id !== String(channelId));
  if (edit !== null) edits[String(channelId)] = { message: edit.message, text: capped(edit.text) };
  write({ ...was, edits });
}

// forgetChannelStored drops what is kept for a channel that was lost.
export function forgetChannelStored(principal: number, channelId: number): void {
  pruneStored(principal, (id) => id !== channelId);
}

// pruneStored keeps only what belongs to channels the person can still open,
// for channels lost while the page was closed.
export function pruneStored(principal: number, keep: (channelId: number) => boolean): void {
  const was = loadStored(principal);
  const kept = (id: string) => keep(Number(id));
  write({ principal, drafts: without(was.drafts, kept), targets: without(was.targets, kept), edits: without(was.edits, kept) });
}
