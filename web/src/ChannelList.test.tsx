// The open channel is kept in sight (issue #584): on a phone the channel list
// is one row that scrolls sideways, and a channel opened from elsewhere (the
// voice strip, the address) may be off screen. The row says when there are
// more channels than it shows, and each count still only describes its
// channel's button (issue #601).
import { cleanup, fireEvent, render, screen } from '@testing-library/react';
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

// geometry gives an element the scroll measurements jsdom does not lay out.
function geometry(el: Element, box: { scrollLeft: number; clientWidth: number; scrollWidth: number }) {
  for (const [key, value] of Object.entries(box)) Object.defineProperty(el, key, { configurable: true, value });
}

describe('the overflow cue', () => {
  const row = () => screen.getByRole('list');

  it('is neither edge while every channel fits', () => {
    render(list('general'));
    geometry(row(), { scrollLeft: 0, clientWidth: 300, scrollWidth: 300 });
    fireEvent.scroll(row());
    expect(row().className).toBe('channel-list');
  });

  it('is more-after at the start of a row that overflows, more-before and not more-after at its end', () => {
    render(list('general'));
    geometry(row(), { scrollLeft: 0, clientWidth: 100, scrollWidth: 300 });
    fireEvent.scroll(row());
    expect(row().classList.contains('more-after')).toBe(true);
    expect(row().classList.contains('more-before')).toBe(false);

    geometry(row(), { scrollLeft: 100, clientWidth: 100, scrollWidth: 300 });
    fireEvent.scroll(row());
    expect(row().classList.contains('more-before')).toBe(true);
    expect(row().classList.contains('more-after')).toBe(true);

    geometry(row(), { scrollLeft: 200, clientWidth: 100, scrollWidth: 300 });
    fireEvent.scroll(row());
    expect(row().classList.contains('more-before')).toBe(true);
    expect(row().classList.contains('more-after')).toBe(false);
  });

  it('allows a pixel of slack at the end, for a fractional width', () => {
    render(list('general'));
    geometry(row(), { scrollLeft: 199.5, clientWidth: 100, scrollWidth: 300 });
    fireEvent.scroll(row());
    expect(row().classList.contains('more-after')).toBe(false);
  });

  it('is measured again when the window is resized', () => {
    render(list('general'));
    geometry(row(), { scrollLeft: 0, clientWidth: 100, scrollWidth: 300 });
    fireEvent(window, new Event('resize'));
    expect(row().classList.contains('more-after')).toBe(true);
    geometry(row(), { scrollLeft: 0, clientWidth: 300, scrollWidth: 300 });
    fireEvent(window, new Event('resize'));
    expect(row().classList.contains('more-after')).toBe(false);
  });

  it('is measured when the list first draws, and again when its channels change', () => {
    const proto = HTMLElement.prototype;
    let width = 300;
    Object.defineProperty(proto, 'scrollLeft', { configurable: true, get: () => 0 });
    Object.defineProperty(proto, 'clientWidth', { configurable: true, get: () => 100 });
    Object.defineProperty(proto, 'scrollWidth', { configurable: true, get: () => width });
    try {
      const view = render(list('general'));
      expect(row().classList.contains('more-after')).toBe(true);
      width = 100;
      view.rerender(<ChannelList channels={channels.slice(0, 1)} selected="general" unread={new Map()} onOpen={() => undefined} />);
      expect(row().classList.contains('more-after')).toBe(false);
    } finally {
      // Defined on HTMLElement over Element's own: deleting them restores jsdom's.
      for (const key of ['scrollLeft', 'clientWidth', 'scrollWidth']) Reflect.deleteProperty(proto, key);
    }
  });
});

describe('an unread count', () => {
  it('describes its channel button and leaves the button named for the channel alone', async () => {
    render(<ChannelList channels={channels} selected="general" unread={new Map([['book-club', 3]])} onOpen={() => undefined} />);
    // Exact name: the count is not part of it.
    const button = await screen.findByRole('button', { name: 'book-club' });
    expect(button.textContent).toBe('book-club');
    const count = screen.getByRole('img', { name: '3 unread' });
    expect(button.getAttribute('aria-describedby')).toBe(count.id);
    // Beside the button, in the same item: never inside it.
    expect(count.closest('button')).toBeNull();
    expect(count.parentElement).toBe(button.parentElement);
    expect(screen.getByRole('button', { name: 'general' }).getAttribute('aria-describedby')).toBeNull();
  });
});

// Issue #596: how many are in a channel's voice, as text beside its button,
// describing it as the unread count does, never in its name.
describe('a voice count', () => {
  const occupied = (occupancy: [number, number][], unread = new Map<string, number>()) => (
    <ChannelList channels={channels} selected="general" unread={unread} occupancy={new Map(occupancy)} onOpen={() => undefined} />
  );

  it('says how many are in voice, describes its channel button and leaves the name alone', async () => {
    render(occupied([[3, 3]]));
    const button = await screen.findByRole('button', { name: 'game-night' });
    expect(button.textContent).toBe('game-night');
    const mark = screen.getByRole('img', { name: '3 in voice' });
    expect(mark.textContent).toBe('3');
    // A speaker, hidden from a screen reader: the mark's name says it.
    expect(mark.querySelector('svg')?.getAttribute('aria-hidden')).toBe('true');
    expect(button.getAttribute('aria-describedby')).toBe(mark.id);
    expect(mark.closest('button')).toBeNull();
    expect(mark.parentElement).toBe(button.parentElement);
  });

  it('shows nothing for a channel with nobody in voice', () => {
    render(occupied([[1, 0]]));
    expect(screen.queryByRole('img', { name: /in voice/ })).toBeNull();
    for (const b of screen.getAllByRole('button')) expect(b.getAttribute('aria-describedby')).toBeNull();
  });

  it('sits beside an unread count, and the button is described by both', () => {
    render(occupied([[2, 1]], new Map([['book-club', 4]])));
    const button = screen.getByRole('button', { name: 'book-club' });
    const mark = screen.getByRole('img', { name: '1 in voice' });
    const unread = screen.getByRole('img', { name: '4 unread' });
    expect(button.getAttribute('aria-describedby')).toBe(`${mark.id} ${unread.id}`);
  });

  it('opens the channel when its button is pressed, as any channel does', () => {
    const opened: string[] = [];
    render(<ChannelList channels={channels} selected="general" unread={new Map()} occupancy={new Map([[3, 2]])} onOpen={(n) => { opened.push(n); }} />);
    fireEvent.click(screen.getByRole('button', { name: 'game-night' }));
    expect(opened).toEqual(['game-night']);
  });

  it('follows the counts as they change', () => {
    const view = render(occupied([[3, 2]]));
    expect(screen.getByRole('img', { name: '2 in voice' })).toBeTruthy();
    view.rerender(occupied([[3, 1], [1, 4]]));
    expect(screen.getByRole('img', { name: '1 in voice' })).toBeTruthy();
    expect(screen.getByRole('img', { name: '4 in voice' })).toBeTruthy();
    view.rerender(occupied([]));
    expect(screen.queryByRole('img', { name: /in voice/ })).toBeNull();
  });
});
