// How times read in the panel and in run detail, and the timeline of a run.
import type { AgentActivityV1 } from '../schema/activity';
import type { InterventionV1 } from '../schema/intervention';

// ago is how long before now `at` was, as a short phrase (the TUI's).
export function ago(now: number, at: string): string {
  const ms = now - Date.parse(at);
  if (!(ms >= 5000)) return 'just now';
  const s = Math.floor(ms / 1000);
  if (s < 60) return `${String(s)}s ago`;
  const m = Math.floor(s / 60);
  if (m < 60) return `${String(m)}m ago`;
  const h = Math.floor(m / 60);
  if (h < 48) return `${String(h)}h ago`;
  return `${String(Math.floor(h / 24))}d ago`;
}

const two = (n: number) => String(n).padStart(2, '0');

// clockSeconds is HH:MM:SS in the reader's own time zone.
export function clockSeconds(at: string): string {
  const d = new Date(at);
  return `${two(d.getHours())}:${two(d.getMinutes())}:${two(d.getSeconds())}`;
}

// spanText is a stretch of time as the views write it: 12.4s, 3m02s, 1h02m03s
// (the TUI's). With tenths false a span under a minute is whole seconds. A
// negative span (clocks that disagree) reads as none.
export function spanText(ms: number, tenths: boolean): string {
  const d = Math.max(0, Math.floor(ms));
  const s = Math.floor(d / 1000);
  if (s < 60) return tenths ? `${String(s)}.${String(Math.floor((d % 1000) / 100))}s` : `${String(s)}s`;
  const m = Math.floor(s / 60);
  if (m < 60) return `${String(m)}m${two(s % 60)}s`;
  return `${String(Math.floor(m / 60))}h${two(m % 60)}m${two(s % 60)}s`;
}

// runSpan is how long a run ran: to its end, or to now while it is open.
export function runSpan(run: { readonly started_at: string; readonly ended_at?: string }, now: number): number {
  const end = run.ended_at === undefined ? now : Date.parse(run.ended_at);
  return end - Date.parse(run.started_at);
}

// The most of an entry's data the page shows, in bytes of UTF-8.
export const dataLimit = 2 << 10;

// dataBody is opaque data pretty-printed, cut at dataLimit bytes with an
// ellipsis (never in the middle of a character).
export function dataBody(data: unknown): string {
  const text = JSON.stringify(data, null, 2) as string | undefined;
  if (text === undefined) return '';
  const bytes = new TextEncoder().encode(text);
  if (bytes.length <= dataLimit) return text;
  let cut = dataLimit;
  while (cut > 0 && ((bytes[cut] ?? 0) & 0xc0) === 0x80) cut--;
  return `${new TextDecoder().decode(bytes.subarray(0, cut))}…`;
}

// One line of run detail's timeline: an activity entry, or an intervention's
// request or its answer.
export type TimelineItem =
  | { readonly kind: 'activity'; readonly at: number; readonly key: string; readonly entry: AgentActivityV1 }
  | { readonly kind: 'ask' | 'answer'; readonly at: number; readonly key: string; readonly iv: InterventionV1 };

// timeline is the activity and the interventions in one stream, oldest first.
// An intervention is its request, and its answer when it has one, each at the
// time the server recorded it. Ties keep the activity first, and a request
// before its answer (the TUI's order).
export function timeline(activity: readonly AgentActivityV1[], interventions: readonly InterventionV1[]): TimelineItem[] {
  const items: TimelineItem[] = [];
  for (const entry of activity) items.push({ kind: 'activity', at: Date.parse(entry.at), key: `a${String(entry.id)}`, entry });
  for (const iv of interventions) items.push({ kind: 'ask', at: Date.parse(iv.requested_at), key: `r${String(iv.id)}`, iv });
  for (const iv of interventions) {
    if (iv.state === 'requested') continue;
    items.push({ kind: 'answer', at: Date.parse(iv.answered_at ?? iv.requested_at), key: `s${String(iv.id)}`, iv });
  }
  return items.sort((a, b) => a.at - b.at);
}

// The state of an intervention as the timeline words it: what the last
// document said, and "requested…" until one says more.
export function askState(iv: InterventionV1): string {
  switch (iv.state) {
    case 'requested':
      return 'requested…';
    case 'failed':
      return `failed: ${iv.reason ?? ''}`;
    default:
      return iv.state;
  }
}

// askText is the words of an intervention item, after the offset and the gap;
// the line starts with the mark ↳.
export function askText(item: TimelineItem & { readonly kind: 'ask' | 'answer' }): string {
  const iv = item.iv;
  const id = `↳ #${String(iv.id)}`;
  if (item.kind === 'answer') return `${id} ${askState(iv)}`;
  // The request names the state only while it is the only word: an answer has
  // a line of its own, at the time it was given.
  const line = iv.state === 'requested' ? `${id} ${iv.kind} · ${askState(iv)}` : `${id} ${iv.kind} · asked`;
  return iv.body === undefined || iv.body === '' ? line : `${line} · ${iv.body}`;
}
