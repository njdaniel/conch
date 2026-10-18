// The open channel is kept in sight (issue #584): on a phone the channel list
// is one row that scrolls sideways, and a channel opened from elsewhere (the
// voice strip, the address) may be off screen.
import { cleanup, render, screen } from '@testing-library/react';
import { afterEach, beforeEach, describe, expect, it } from 'vitest';
import { ChannelList } from './ChannelList';
import type { ChannelV0 } from './schema/channel';

const channels: ChannelV0[] = ['general', 'book-club', 'game-night'].map((name, i) => ({
  id: i + 1,
  name,
  created_at: '2026-10-18T00:00:00.000Z',
}));

let scrolled: { el: Element; options: unknown }[];
beforeEach(() => {
  scrolled = [];
  // jsdom has no scrollIntoView; the list asks for it only where there is one.
  Element.prototype.scrollIntoView = function (this: Element, options?: boolean | ScrollIntoViewOptions) {
    scrolled.push({ el: this, options });
  };
});
afterEach(() => {
  cleanup();
  Reflect.deleteProperty(Element.prototype, 'scrollIntoView');
});

const list = (selected: string | null) => <ChannelList channels={channels} selected={selected} unread={new Map()} onOpen={() => undefined} />;

describe('the open channel', () => {
  it('is scrolled into view, the least distance, whenever it changes', () => {
    const view = render(list('general'));
    expect(scrolled).toEqual([{ el: screen.getByRole('button', { name: 'general' }), options: { block: 'nearest', inline: 'nearest' } }]);
    view.rerender(list('game-night'));
    expect(scrolled.map((s) => s.el)).toEqual([screen.getByRole('button', { name: 'general' }), screen.getByRole('button', { name: 'game-night' })]);
    expect(scrolled[1]?.options).toEqual({ block: 'nearest', inline: 'nearest' });
  });

  it('is not scrolled to again while it stays the same', () => {
    const view = render(list('book-club'));
    view.rerender(list('book-club'));
    expect(scrolled).toHaveLength(1);
  });

  it('nothing is scrolled while no channel is open', () => {
    render(list(null));
    expect(scrolled).toEqual([]);
  });

  it('works without scrollIntoView, as in jsdom', () => {
    Reflect.deleteProperty(Element.prototype, 'scrollIntoView');
    const view = render(list('general'));
    view.rerender(list('book-club'));
    expect(screen.getByRole('button', { name: 'book-club' }).getAttribute('aria-current')).toBe('true');
  });
});
