import { describe, expect, it } from 'vitest';
import { matchChannels, nextUnread, stepChannel } from './channelNav';
import type { ChannelV0 } from './schema/channel';

const ch = (id: number, name: string, extra: Partial<ChannelV0> = {}): ChannelV0 => ({ id, name, created_at: '2026-10-18T00:00:00.000Z', ...extra });
const all = [ch(1, 'general'), ch(2, 'game-night'), ch(3, 'book-club', { visibility: 'private' }), ch(4, 'old-games', { archived_at: '2026-10-19T00:00:00.000Z' }), ch(5, 'gaming')];

describe('matchChannels', () => {
  it('lists every channel for an empty query, live ones first in sidebar order', () => {
    expect(matchChannels(all, '').map((c) => c.name)).toEqual(['general', 'game-night', 'book-club', 'gaming', 'old-games']);
  });
  it('matches by name, ignoring case and surrounding space, prefix matches first', () => {
    expect(matchChannels(all, '  GAM ').map((c) => c.name)).toEqual(['game-night', 'gaming', 'old-games']);
    expect(matchChannels(all, 'club').map((c) => c.name)).toEqual(['book-club']);
  });
  it('finds nothing for a name nobody holds', () => {
    expect(matchChannels(all, 'secret')).toEqual([]);
  });
});

describe('stepChannel', () => {
  const live = all.filter((c) => c.archived_at === undefined);
  it('moves through the live channels in sidebar order and wraps', () => {
    expect(stepChannel(live, 'general', 1)).toBe('game-night');
    expect(stepChannel(live, 'gaming', 1)).toBe('general');
    expect(stepChannel(live, 'general', -1)).toBe('gaming');
  });
  it('starts at the first or last when nothing is open', () => {
    expect(stepChannel(live, null, 1)).toBe('general');
    expect(stepChannel(live, null, -1)).toBe('gaming');
  });
  it('has nowhere to go with fewer than two channels', () => {
    expect(stepChannel([ch(1, 'general')], 'general', 1)).toBeNull();
    expect(stepChannel([], null, 1)).toBeNull();
  });
});

describe('nextUnread', () => {
  const unread = new Map([['book-club', 2], ['gaming', 1]]);
  it('finds the next channel with unread, wrapping, and the previous', () => {
    expect(nextUnread(all, unread, 'general', 1)).toBe('book-club');
    expect(nextUnread(all, unread, 'book-club', 1)).toBe('gaming');
    expect(nextUnread(all, unread, 'gaming', 1)).toBe('book-club');
    expect(nextUnread(all, unread, 'general', -1)).toBe('gaming');
  });
  it('is null when nothing else is unread', () => {
    expect(nextUnread(all, new Map(), 'general', 1)).toBeNull();
    expect(nextUnread(all, new Map([['general', 3]]), 'general', 1)).toBeNull();
  });
  it('ignores counts of channels the viewer no longer holds', () => {
    expect(nextUnread(all, new Map([['revoked', 4]]), 'general', 1)).toBeNull();
  });
});
