// The open channel is kept in sight (issue #584): a channel opened from
// elsewhere (the voice strip, the address) may be off screen in a long list,
// and each count still only describes its channel's button (issue #601). The
// sideways row and its overflow cue of #601 are gone: on a phone the list is
// the Channels view, one row a channel (issue #929).
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

describe('a manages badge', () => {
  it('marks only managed channels and describes the channel button without changing its name', () => {
    const managed = channels.map((channel) => ({ ...channel, manages: channel.id === 2 }));
    render(<ChannelList channels={managed} selected="general" unread={new Map()} onOpen={() => undefined} />);
    const button = screen.getByRole('button', { name: 'book-club' });
    const badge = screen.getByText('manages');
    expect(button.textContent).toBe('book-club');
    expect(button.getAttribute('aria-describedby')).toBe(badge.id);
    expect(badge.closest('button')).toBeNull();
    expect(badge.parentElement).toBe(button.parentElement);
    expect(screen.getByRole('button', { name: 'general' }).getAttribute('aria-describedby')).toBeNull();
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

// An empty list says who can change that (issue #695): by default an
// operator, and for someone who may make a channel, less, since the control
// to do it is beside the list.
describe('an empty list', () => {
  it('says an operator needs to add the person, by default', () => {
    render(<ChannelList channels={[]} selected={null} unread={new Map()} onOpen={() => undefined} />);
    expect(screen.getByText('No channels yet: an operator needs to add you to one')).toBeTruthy();
    expect(screen.queryByRole('list')).toBeNull();
  });

  it('says what it is given instead', () => {
    render(<ChannelList channels={[]} selected={null} unread={new Map()} empty="No channels yet" onOpen={() => undefined} />);
    expect(screen.getByText('No channels yet')).toBeTruthy();
    expect(screen.queryByText(/an operator needs to add you/)).toBeNull();
  });
});

// The rename control (issue #728) is the caller's to give, for an operator or
// this channel's manager; the list shows it under itself while a channel is
// open, and keeps each channel's place, count and focus stop by id across a rename.
describe('the rename control', () => {
  const control = <button type="button">Rename the open channel</button>;

  it('is shown under the list while a channel is open, when given', () => {
    render(<ChannelList channels={channels} selected="book-club" unread={new Map()} rename={control} onOpen={() => undefined} />);
    const button = screen.getByRole('button', { name: 'Rename the open channel' });
    expect(screen.getByRole('list').contains(button)).toBe(false);
  });

  it('is not shown when not given (anyone without the grant), nor while no channel is open', () => {
    const view = render(<ChannelList channels={channels} selected="book-club" unread={new Map()} onOpen={() => undefined} />);
    expect(screen.queryByRole('button', { name: 'Rename the open channel' })).toBeNull();
    view.rerender(<ChannelList channels={channels} selected={null} unread={new Map()} rename={control} onOpen={() => undefined} />);
    expect(screen.queryByRole('button', { name: 'Rename the open channel' })).toBeNull();
  });

  it('a channel renamed keeps its place, stays the open one and is opened by its new name', () => {
    const opened: string[] = [];
    const renamed = channels.map((c) => (c.id === 2 ? { ...c, name: 'readers' } : c));
    const props = {
      unread: new Map([['readers', 2]]),
      onOpen: (name: string) => {
        opened.push(name);
      },
    };
    const view = render(<ChannelList channels={channels} selected="book-club" {...props} />);
    const before = screen.getByRole('button', { name: 'book-club' });
    view.rerender(<ChannelList channels={renamed} selected="readers" {...props} />);
    const after = screen.getByRole('button', { name: 'readers' });
    // The same element, in the same place: the list follows the channel by id.
    expect(after).toBe(before);
    expect(screen.getAllByRole('listitem').map((li) => li.textContent)).toEqual(['general', 'readers2', 'game-night']);
    expect(after.getAttribute('aria-current')).toBe('true');
    expect(after.tabIndex).toBe(0);
    expect(screen.getByRole('img', { name: '2 unread' })).toBeTruthy();
    fireEvent.click(after);
    expect(opened).toEqual(['readers']);
  });
});
