// The open channel is kept in sight (issue #584): a channel opened from
// elsewhere (the voice strip, the address) may be off screen in a long list,
// and each count still only describes its channel's button (issue #601). The
// sideways row and its overflow cue of #601 are gone: on a phone the list is
// the Channels view, one row a channel (issue #929).
import { act, cleanup, fireEvent, render, screen, within } from '@testing-library/react';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { ChannelList, voiceName } from './ChannelList';
import { longPressMs } from './MessageMenu';
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
// A count alone, as a server from before issue #1103 sends it: no people.
const countsOnly = (occupancy: [number, number][]) => new Map(occupancy.map(([id, count]) => [id, { count, people: null }]));

describe('a voice count', () => {
  const occupied = (occupancy: [number, number][], unread = new Map<string, number>()) => (
    <ChannelList channels={channels} selected="general" unread={unread} occupancy={countsOnly(occupancy)} onOpen={() => undefined} />
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
    render(<ChannelList channels={channels} selected="general" unread={new Map()} occupancy={countsOnly([[3, 2]])} onOpen={(n) => { opened.push(n); }} />);
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

// Issue #1103: who is in a channel's voice, by name, nested under the
// channel; the viewer is "You"; someone not yet named is their id; the list
// folds after five; a row opens the channel and nothing more.
describe('who is in voice', () => {
  const names = new Map<number, string>([
    [10, 'alice'],
    [11, 'bob'],
    [12, 'carol'],
    [13, 'dave'],
    [14, 'erin'],
    [15, 'frank'],
    [16, 'grace'],
  ]);
  const listed = (people: Record<number, number[]>, onOpen: (n: string) => void = () => undefined, voiceNames = names) => (
    <ChannelList
      channels={channels}
      selected="general"
      unread={new Map()}
      occupancy={new Map(Object.entries(people).map(([id, ids]) => [Number(id), { count: ids.length, people: ids }]))}
      voiceNames={voiceNames}
      viewer={7}
      onOpen={onOpen}
    />
  );
  const inVoice = (channel: string) => screen.getByRole('list', { name: `In voice in #${channel}` });
  const rows = (channel: string) => within(inVoice(channel)).getAllByRole('listitem').map((li) => li.textContent);

  it('names the people under the channel, in the order the server lists them, as text outside the channel button', () => {
    render(listed({ 3: [10, 11] }));
    expect(rows('game-night')).toEqual(['alice', 'bob']);
    const button = screen.getByRole('button', { name: 'game-night' });
    expect(button.textContent).toBe('game-night');
    expect(inVoice('game-night').closest('button')).toBeNull();
    // Under the channel's row, with the count still beside the button.
    expect(inVoice('game-night').parentElement).toBe(button.parentElement);
    expect(screen.getByRole('img', { name: '2 in voice' })).toBeTruthy();
    // No list under a channel with nobody.
    expect(screen.queryByRole('list', { name: 'In voice in #general' })).toBeNull();
  });

  it('says You for the viewer and Member N for someone not yet named', () => {
    render(listed({ 2: [7, 99] }));
    expect(rows('book-club')).toEqual(['You', 'Member 99']);
  });

  it('names the viewer by their own name when the page has it, others unchanged (#1481)', () => {
    render(listed({ 2: [10, 7, 99] }, undefined, new Map([...names, [7, 'nick']])));
    expect(rows('book-club')).toEqual(['alice', 'nick', 'Member 99']);
  });

  it('renders a hostile viewer name as text (#1481)', () => {
    const hostile = '<img src=x onerror=alert(1)>';
    render(listed({ 2: [7] }, undefined, new Map([[7, hostile]])));
    expect(rows('book-club')).toEqual([hostile]);
    expect(inVoice('book-club').querySelector('img')).toBeNull();
  });

  it('folds after five with how many more', () => {
    render(listed({ 3: [10, 11, 12, 13, 14, 15, 16] }));
    expect(rows('game-night')).toEqual(['alice', 'bob', 'carol', 'dave', 'erin', '+2 more']);
  });

  it('shows all five when there are no more than five', () => {
    render(listed({ 3: [10, 11, 12, 13, 14] }));
    expect(rows('game-night')).toEqual(['alice', 'bob', 'carol', 'dave', 'erin']);
  });

  it('folds the sixth into +1 more', () => {
    render(listed({ 3: [10, 11, 12, 13, 14, 15] }));
    expect(rows('game-night')).toEqual(['alice', 'bob', 'carol', 'dave', 'erin', '+1 more']);
  });

  it('opens the channel from a name or from the fold, and nothing else', () => {
    const opened: string[] = [];
    render(listed({ 3: [10, 11, 12, 13, 14, 15] }, (n) => { opened.push(n); }));
    fireEvent.click(within(inVoice('game-night')).getByRole('button', { name: 'bob' }));
    fireEvent.click(within(inVoice('game-night')).getByRole('button', { name: '+1 more' }));
    expect(opened).toEqual(['game-night', 'game-night']);
  });

  it('keeps the rows out of the tab order: the channel button does what they do', () => {
    render(listed({ 3: [10, 11] }));
    for (const b of within(inVoice('game-night')).getAllByRole('button')) expect(b.tabIndex).toBe(-1);
    expect(screen.getByRole('button', { name: 'general' }).tabIndex).toBe(0);
  });

  it('shows the count alone when the server did not say who', () => {
    render(<ChannelList channels={channels} selected="general" unread={new Map()} occupancy={countsOnly([[3, 2]])} voiceNames={names} viewer={7} onOpen={() => undefined} />);
    expect(screen.getByRole('img', { name: '2 in voice' })).toBeTruthy();
    expect(screen.queryByRole('list', { name: /^In voice in/ })).toBeNull();
  });

  it('follows the people and their names as they change', () => {
    const view = render(listed({ 3: [10, 99] }));
    expect(rows('game-night')).toEqual(['alice', 'Member 99']);
    view.rerender(listed({ 3: [10, 99] }, undefined, new Map([...names, [99, 'zed']])));
    expect(rows('game-night')).toEqual(['alice', 'zed']);
    view.rerender(listed({ 3: [99] }, undefined, new Map([...names, [99, 'zed']])));
    expect(rows('game-night')).toEqual(['zed']);
    view.rerender(listed({}));
    expect(screen.queryByRole('list', { name: /^In voice in/ })).toBeNull();
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

// A channel renamed keeps its place, count and focus stop by id (issue #728).
describe('a rename', () => {
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

// Issue #1135: a channel's ⋯ menu, with Invite people and Rename channel…, by
// viewer and by channel. The list is given what the viewer may do; a row with
// nothing to do has no ⋯.
describe('a channel row\'s menu', () => {
  const managed = channels.map((c) => ({ ...c, manages: c.id === 2 }));
  const row = (name: string) => screen.getByRole('button', { name }).closest('li') as HTMLElement;
  const more = (name: string) => within(row(name)).getByRole('button', { name: `Actions for ${name}` });
  const labels = (name: string) => within(screen.getByRole('menu', { name: `Actions for ${name}` })).getAllByRole('menuitem').map((i) => i.textContent);
  const rename = (channel: ChannelV0, close: () => void) => (
    <form aria-label={`Rename ${channel.name}`}>
      <button type="button" onClick={close}>
        Cancel
      </button>
    </form>
  );
  const draw = (extra: Partial<Parameters<typeof ChannelList>[0]> = {}, list: readonly ChannelV0[] = managed) =>
    render(<ChannelList channels={list} selected="general" unread={new Map()} onOpen={() => undefined} {...extra} />);

  it('gives a plain member without may_invite no ⋯ at all, and leaves the native menu alone', () => {
    draw({ canRename: () => false, renderRename: rename });
    expect(screen.queryByRole('button', { name: /^Actions for/ })).toBeNull();
    expect(fireEvent.contextMenu(row('general'))).toBe(true);
  });

  it('offers Invite people alone to a member who may invite, on every channel', () => {
    draw({ onInvite: () => undefined, canRename: () => false, renderRename: rename });
    for (const name of ['general', 'book-club', 'game-night']) {
      fireEvent.click(more(name));
      expect(labels(name)).toEqual(['Invite people']);
      fireEvent.keyDown(document.activeElement as Element, { key: 'Escape' });
    }
  });

  it('offers a manager Rename on the channel they manage only', () => {
    draw({ canRename: (c) => c.manages === true, renderRename: rename });
    expect(within(row('general')).queryByRole('button', { name: /^Actions for/ })).toBeNull();
    expect(within(row('game-night')).queryByRole('button', { name: /^Actions for/ })).toBeNull();
    fireEvent.click(more('book-club'));
    expect(labels('book-club')).toEqual(['Rename channel…']);
  });

  it('offers an operator who may invite both items on every channel', () => {
    draw({ onInvite: () => undefined, canRename: () => true, renderRename: rename });
    for (const name of ['general', 'book-club', 'game-night']) {
      fireEvent.click(more(name));
      expect(labels(name)).toEqual(['Invite people', 'Rename channel…']);
      fireEvent.keyDown(document.activeElement as Element, { key: 'Escape' });
    }
  });

  it('keeps the ⋯ outside the channel button, which keeps its name and still opens the channel', () => {
    const opened: string[] = [];
    draw({ onInvite: () => undefined, onOpen: (n: string) => opened.push(n) });
    const button = screen.getByRole('button', { name: 'book-club' });
    expect(button.querySelector('button')).toBeNull();
    expect(button.textContent).toBe('book-club');
    expect(more('book-club').closest('button:not([aria-haspopup])')).toBeNull();
    fireEvent.click(button);
    expect(opened).toEqual(['book-club']);
  });

  it('Invite people hands the row\'s channel (not the open one) to the opener and closes the menu', () => {
    const asked: string[] = [];
    draw({ onInvite: (c: ChannelV0) => asked.push(c.name) });
    fireEvent.click(more('game-night'));
    fireEvent.click(screen.getByRole('menuitem', { name: 'Invite people' }));
    expect(asked).toEqual(['game-night']);
    expect(screen.queryByRole('menu')).toBeNull();
  });

  it('Rename shows the form under that row, not the open channel\'s, and Cancel returns focus to its ⋯', () => {
    draw({ canRename: () => true, renderRename: rename });
    fireEvent.click(more('book-club'));
    fireEvent.click(screen.getByRole('menuitem', { name: 'Rename channel…' }));
    expect(screen.queryByRole('menu')).toBeNull();
    expect(within(row('book-club')).getByRole('form', { name: 'Rename book-club' })).toBeDefined();
    expect(screen.queryByRole('form', { name: 'Rename general' })).toBeNull();
    fireEvent.click(screen.getByRole('button', { name: 'Cancel' }));
    expect(screen.queryByRole('form')).toBeNull();
    expect(document.activeElement).toBe(more('book-club'));
  });

  it('opens by right-click on the row only, and by a long press', () => {
    vi.useFakeTimers();
    try {
      draw({ onInvite: () => undefined });
      expect(fireEvent.contextMenu(row('general'))).toBe(false);
      expect(screen.getByRole('menu', { name: 'Actions for general' })).toBeDefined();
      expect(fireEvent.contextMenu(screen.getByRole('list'))).toBe(true);
      fireEvent.keyDown(document.activeElement as Element, { key: 'Escape' });
      fireEvent.touchStart(row('book-club'), { touches: [{ clientX: 5, clientY: 5 }] });
      act(() => {
        vi.advanceTimersByTime(longPressMs);
      });
      expect(screen.getByRole('menu', { name: 'Actions for book-club' })).toBeDefined();
    } finally {
      vi.useRealTimers();
    }
  });

  it('opens from the button with focus on the first item; Escape closes it and focuses the button; an outside press closes it', () => {
    draw({ onInvite: () => undefined });
    const button = more('general');
    expect(button.getAttribute('aria-haspopup')).toBe('menu');
    fireEvent.click(button);
    expect(document.activeElement).toBe(screen.getByRole('menuitem', { name: 'Invite people' }));
    fireEvent.keyDown(document.activeElement as Element, { key: 'Escape' });
    expect(screen.queryByRole('menu')).toBeNull();
    expect(document.activeElement).toBe(button);
    fireEvent.click(button);
    fireEvent.mouseDown(document.body);
    expect(screen.queryByRole('menu')).toBeNull();
  });

  it('leaves the arrow keys moving between channel buttons only', () => {
    draw({ onInvite: () => undefined });
    screen.getByRole('button', { name: 'general' }).focus();
    fireEvent.keyDown(document.activeElement as Element, { key: 'ArrowDown' });
    expect(document.activeElement).toBe(screen.getByRole('button', { name: 'book-club' }));
    fireEvent.keyDown(document.activeElement as Element, { key: 'End' });
    expect(document.activeElement).toBe(screen.getByRole('button', { name: 'game-night' }));
  });
});

describe('voiceName (#1481)', () => {
  const names = new Map([[10, 'alice']]);
  it.each([
    ['the viewer, name known', 7, new Map([[7, 'nick']]), 'nick'],
    ['the viewer, name not known yet', 7, names, 'You'],
    ['the viewer, empty name', 7, new Map([[7, '']]), 'You'],
    ['someone named', 10, names, 'alice'],
    ['someone not named', 99, names, 'Member 99'],
  ])('%s', (_what, id, known, want) => {
    expect(voiceName(id, 7, known)).toBe(want);
  });
});
