import { isArchived } from './archived';
import type { ChannelV0 } from './schema/channel';

// Moving between channels (issue #1492). Pure: which channel a search, a step
// or an unread jump lands on. Nothing here opens a channel, joins voice or
// reads the server; the caller opens what these name.

// The sidebar's order: live channels as the server listed them, then the
// archived group (issue #844).
export function sidebarOrder(channels: readonly ChannelV0[]): ChannelV0[] {
  return [...channels.filter((c) => !isArchived(c)), ...channels.filter(isArchived)];
}

// matchChannels is the channels whose name holds the query, ignoring case and
// surrounding space. Names that start with it come first; sidebar order
// otherwise. An empty query matches every channel.
export function matchChannels(channels: readonly ChannelV0[], query: string): ChannelV0[] {
  const ordered = sidebarOrder(channels);
  const q = query.trim().toLowerCase();
  if (q === '') return ordered;
  const hits = ordered.filter((c) => c.name.toLowerCase().includes(q));
  return [...hits.filter((c) => c.name.toLowerCase().startsWith(q)), ...hits.filter((c) => !c.name.toLowerCase().startsWith(q))];
}

// stepChannel is the name one step from `from` through `channels` (given in
// the order to walk), wrapping at the ends; null when there is nowhere to go.
export function stepChannel(channels: readonly ChannelV0[], from: string | null, direction: 1 | -1): string | null {
  const at = channels.findIndex((c) => c.name === from);
  if (channels.length === 0 || (at >= 0 && channels.length === 1)) return null;
  const to = at < 0 ? (direction === 1 ? 0 : channels.length - 1) : (at + direction + channels.length) % channels.length;
  return channels[to]?.name ?? null;
}

// nextUnread is the nearest channel after (or before) `from`, in sidebar
// order and wrapping, that the viewer holds and has unread messages in. The
// open channel is never its own answer. Counts for channels no longer held are
// ignored.
export function nextUnread(channels: readonly ChannelV0[], unread: ReadonlyMap<string, number>, from: string | null, direction: 1 | -1): string | null {
  const ordered = sidebarOrder(channels);
  const at = ordered.findIndex((c) => c.name === from);
  for (let i = 1; i <= ordered.length; i++) {
    const index = at < 0 ? (direction === 1 ? i - 1 : ordered.length - i) : (((at + direction * i) % ordered.length) + ordered.length) % ordered.length;
    const channel = ordered[index];
    if (channel !== undefined && channel.name !== from && (unread.get(channel.name) ?? 0) > 0) return channel.name;
  }
  return null;
}
