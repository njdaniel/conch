import { act, cleanup, fireEvent, render, screen, within } from '@testing-library/react';
import { useState } from 'react';
import { afterEach, describe, expect, it, vi } from 'vitest';
import { ChannelList } from './ChannelList';
import { GroupsPanel, type ArrangementControls } from './ChannelGroups';
import { emptyArrangement, type Arrangement } from './arrangement';
import type { ChannelV0 } from './schema/channel';

const ch = (id: number, name: string, extra: Partial<ChannelV0> = {}): ChannelV0 => ({ id, name, created_at: '2026-10-18T00:00:00.000Z', ...extra });
const channels = [ch(1, 'general'), ch(2, 'games'), ch(3, 'books', { visibility: 'private' }), ch(4, 'music'), ch(5, 'old', { archived_at: '2026-10-19T00:00:00.000Z' })];

afterEach(cleanup);

const inNav = () => within(screen.getByRole('navigation', { name: 'Channels' }));

function Harness({ start = emptyArrangement, onOpen = () => undefined, list = channels, selected = 'general' }: { start?: Arrangement; onOpen?: (n: string) => void; list?: readonly ChannelV0[]; selected?: string | null }) {
  const [arrangement, setArrangement] = useState(start);
  const controls: ArrangementControls = { arrangement, edit: (change) => { setArrangement((a) => change(a)); } };
  return (
    <>
      <nav aria-label="Channels">
        <ChannelList channels={list} selected={selected} unread={new Map()} onOpen={onOpen} arrangement={controls} />
      </nav>
      <GroupsPanel controls={controls} />
    </>
  );
}

const rowOf = (name: string) => screen.getByRole('button', { name }).closest('li') as HTMLElement;
function choose(name: string, item: string) {
  fireEvent.click(within(rowOf(name)).getByRole('button', { name: `Actions for ${name}` }));
  fireEvent.click(screen.getByRole('menuitem', { name: item }));
}
const order = (label: string) => within(screen.getByRole('group', { name: label })).getAllByRole('button', { name: /^[a-z]+$/ }).map((b) => b.textContent);

describe('Edit groups', () => {
  it('is outside the Channels navigation, which holds channel controls only', () => {
    render(<Harness />);
    expect(within(screen.getByRole('navigation', { name: 'Channels' })).queryByRole('button', { name: 'Edit groups' })).toBeNull();
    expect(screen.getByRole('button', { name: 'Edit groups' })).toBeTruthy();
  });
});

describe('favorites', () => {
  it('draws a Favorites group first, and Unfavorite puts the channel back', () => {
    render(<Harness />);
    expect(screen.queryByRole('group', { name: 'Favorites' })).toBeNull();
    choose('games', 'Favorite');
    expect(order('Favorites')).toEqual(['games']);
    expect(screen.getAllByRole('button', { name: 'games' })).toHaveLength(1);
    expect(inNav().getByRole('status').textContent).toBe('games added to favorites');
    choose('games', 'Unfavorite');
    expect(screen.queryByRole('group', { name: 'Favorites' })).toBeNull();
  });

  it('orders favorites with Move up and Move down, from the menu alone', () => {
    render(<Harness start={{ favorites: [1, 2, 4], groups: [] }} />);
    expect(order('Favorites')).toEqual(['general', 'games', 'music']);
    choose('music', 'Move up');
    expect(order('Favorites')).toEqual(['general', 'music', 'games']);
    expect(inNav().getByRole('status').textContent).toBe('music moved up in favorites, position 2 of 3');
    fireEvent.click(within(rowOf('general')).getByRole('button', { name: 'Actions for general' }));
    expect(screen.getByRole('menuitem', { name: /Move up/ }).getAttribute('aria-disabled')).toBe('true');
  });

  it('offers no arrangement on an archived channel', () => {
    render(<Harness />);
    fireEvent.click(screen.getByRole('button', { name: /Archived/ }));
    expect(within(rowOf('old')).queryByRole('button', { name: 'Actions for old' })).toBeNull();
  });
});

describe('groups', () => {
  it('creates, renames and deletes a group, and refuses a duplicate or empty name', () => {
    render(<Harness />);
    fireEvent.click(screen.getByRole('button', { name: 'Edit groups' }));
    const add = (name: string) => {
      fireEvent.change(screen.getByLabelText('New group name'), { target: { value: name } });
      fireEvent.click(screen.getByRole('button', { name: 'Add group' }));
    };
    add('  ');
    expect(screen.getByRole('alert').textContent).toBe('Give the group a name.');
    add('Raids');
    expect(screen.getByRole('group', { name: 'Raids' })).toBeTruthy();
    add('raids');
    expect(screen.getByRole('alert').textContent).toBe('A group already has that name.');
    fireEvent.change(screen.getByLabelText('Name of group Raids'), { target: { value: 'Raid night' } });
    fireEvent.click(screen.getByRole('button', { name: 'Rename' }));
    expect(screen.getByRole('group', { name: 'Raid night' })).toBeTruthy();
    fireEvent.click(screen.getByRole('button', { name: 'Delete group Raid night' }));
    expect(screen.queryByRole('group', { name: 'Raid night' })).toBeNull();
  });

  it('moves a channel to a group and out again from the menu; the rest stay in today\'s order', () => {
    render(<Harness start={{ favorites: [], groups: [{ id: 'g1', name: 'Play', collapsed: false, channels: [] }] }} />);
    expect(screen.getByText(/Empty\./)).toBeTruthy();
    choose('music', 'Move to group…');
    fireEvent.change(screen.getByLabelText('Group'), { target: { value: 'g1' } });
    fireEvent.click(screen.getByRole('button', { name: 'Move' }));
    expect(order('Play')).toEqual(['music']);
    expect(inNav().getByRole('status').textContent).toBe('music moved to Play');
    expect(screen.getAllByRole('button', { name: 'music' })).toHaveLength(1);
    choose('music', 'Move to group…');
    fireEvent.change(screen.getByLabelText('Group'), { target: { value: '' } });
    fireEvent.click(screen.getByRole('button', { name: 'Move' }));
    expect(screen.getByRole('button', { name: /Play/ }).textContent).toContain('(0)');
  });

  it('folds and unfolds, counts its channels, and keeps the open channel in sight when folded', () => {
    const start: Arrangement = { favorites: [], groups: [{ id: 'g1', name: 'Play', collapsed: false, channels: [2, 4] }] };
    render(<Harness start={start} selected="music" />);
    const head = screen.getByRole('button', { name: /Play/ });
    expect(head.getAttribute('aria-expanded')).toBe('true');
    expect(head.textContent).toContain('(2)');
    fireEvent.click(head);
    expect(head.getAttribute('aria-expanded')).toBe('false');
    expect(order('Play')).toEqual(['music']);
    expect(screen.queryByRole('button', { name: 'games' })).toBeNull();
    fireEvent.click(head);
    expect(order('Play')).toEqual(['games', 'music']);
  });

  it('opens a channel in a group as any other, and arrow keys walk the drawn order', () => {
    const onOpen = vi.fn();
    render(<Harness onOpen={onOpen} start={{ favorites: [4], groups: [{ id: 'g1', name: 'Play', collapsed: false, channels: [2] }] }} selected="music" />);
    const music = screen.getByRole('button', { name: 'music' });
    music.focus();
    fireEvent.keyDown(music, { key: 'ArrowDown' });
    expect(document.activeElement).toBe(screen.getByRole('button', { name: 'games' }));
    fireEvent.click(screen.getByRole('button', { name: 'games' }));
    expect(onOpen).toHaveBeenCalledWith('games');
  });

  it('draws nothing of a channel not held, and counts nothing for it', () => {
    render(<Harness start={{ favorites: [99], groups: [{ id: 'g1', name: 'Play', collapsed: false, channels: [98, 2] }] }} />);
    expect(screen.queryByRole('group', { name: 'Favorites' })).toBeNull();
    expect(screen.getByRole('button', { name: /Play/ }).textContent).toContain('(1)');
  });
});

describe('revocation and focus', () => {
  it('stops announcing a channel the moment it is no longer held', () => {
    const view = render(<Harness />);
    choose('books', 'Favorite');
    expect(inNav().getByRole('status').textContent).toBe('books added to favorites');
    view.rerender(<Harness list={channels.filter((c) => c.name !== 'books')} />);
    expect(inNav().getByRole('status').textContent).toBe('');
    expect(document.body.textContent).not.toContain('books');
  });

  it('does not offer, or announce, a move that changes nothing visible', () => {
    render(<Harness start={{ favorites: [2], groups: [{ id: 'g1', name: 'Play', collapsed: false, channels: [2, 3] }] }} />);
    fireEvent.click(within(rowOf('books')).getByRole('button', { name: 'Actions for books' }));
    expect(screen.getByRole('menuitem', { name: /Move up/ }).getAttribute('aria-disabled')).toBe('true');
    expect(screen.getByRole('menuitem', { name: /Move down/ }).getAttribute('aria-disabled')).toBe('true');
  });

  it('expands a folded group a channel is moved into, and keeps focus on the channel', () => {
    vi.useFakeTimers();
    try {
      render(<Harness start={{ favorites: [], groups: [{ id: 'g1', name: 'Play', collapsed: true, channels: [] }] }} />);
      choose('music', 'Move to group…');
      fireEvent.change(screen.getByLabelText('Group'), { target: { value: 'g1' } });
      fireEvent.click(screen.getByRole('button', { name: 'Move' }));
      act(() => { vi.runAllTimers(); });
      expect(screen.getByRole('button', { name: /Play/ }).getAttribute('aria-expanded')).toBe('true');
      expect(document.activeElement).toBe(screen.getByRole('button', { name: 'music' }));
    } finally {
      vi.useRealTimers();
    }
  });
});
