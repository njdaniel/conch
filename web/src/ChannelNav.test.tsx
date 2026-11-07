import { act, cleanup, fireEvent, render, screen, waitFor, within } from '@testing-library/react';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import * as api from './api';
import { ChannelNav } from './ChannelNav';
import type { ChannelListingV1, ChannelV0 } from './schema/channel';

const ch = (id: number, name: string, extra: Partial<ChannelV0> = {}): ChannelV0 => ({ id, name, created_at: '2026-10-18T00:00:00.000Z', ...extra });
const channels = [ch(1, 'general'), ch(2, 'game-night'), ch(3, 'book-club', { visibility: 'private' }), ch(4, 'old-games', { archived_at: '2026-10-19T00:00:00.000Z' })];
const listing = (id: number, name: string, extra: Partial<ChannelListingV1> = {}): ChannelListingV1 => ({ id, name, created_at: '2026-10-18T00:00:00.000Z', visibility: 'public', joined: false, ...extra });

afterEach(() => {
  cleanup();
  vi.restoreAllMocks();
});
beforeEach(() => {
  vi.spyOn(api, 'channelDirectory').mockResolvedValue({ ok: true, value: [listing(9, 'movie-night'), listing(1, 'general', { joined: true })] });
});

function setup(over: { channels?: readonly ChannelV0[]; unread?: ReadonlyMap<string, number>; selected?: string | null; onBrowse?: ((name: string) => void) | null; me?: number } = {}) {
  const onOpen = vi.fn();
  const onBrowse = vi.fn();
  const props = (o: typeof over) => ({
    channels: o.channels ?? channels,
    unread: o.unread ?? new Map<string, number>(),
    selected: o.selected === undefined ? 'general' : o.selected,
    me: o.me ?? 1,
    onOpen,
    ...(o.onBrowse === null ? {} : { onBrowse: o.onBrowse ?? onBrowse }),
  });
  const view = render(
    <div>
      <textarea aria-label="Message" id="composer-text" />
      <input aria-label="Search" />
      <ChannelNav {...props(over)} />
    </div>,
  );
  const again = (o: typeof over) => {
    view.rerender(
      <div>
        <textarea aria-label="Message" id="composer-text" />
        <input aria-label="Search" />
        <ChannelNav {...props(o)} />
      </div>,
    );
  };
  return { onOpen, onBrowse, again };
}

const key = (init: KeyboardEventInit, target: Element | Document = document.body) => {
  fireEvent.keyDown(target, init);
};
const box = () => screen.getByRole('combobox', { name: 'Find a channel' });

describe('opening the switcher', () => {
  it('opens from the visible entry point and from Ctrl+K and Cmd+K', async () => {
    setup();
    fireEvent.click(screen.getByRole('button', { name: /Find a channel/ }));
    expect(box()).toBe(document.activeElement);
    key({ key: 'Escape' }, box());
    expect(screen.queryByRole('dialog')).toBeNull();
    key({ key: 'k', ctrlKey: true });
    expect(screen.getByRole('dialog')).toBeTruthy();
    key({ key: 'Escape' }, box());
    key({ key: 'k', metaKey: true });
    expect(screen.getByRole('dialog')).toBeTruthy();
    await waitFor(() => {
      expect(screen.getByRole('option', { name: /movie-night/ })).toBeTruthy();
    });
  });

  it('fires in the message composer, and leaves its text alone', () => {
    setup();
    const composer = screen.getByLabelText<HTMLTextAreaElement>('Message');
    composer.value = 'half a thought';
    key({ key: 'k', ctrlKey: true }, composer);
    expect(screen.getByRole('dialog')).toBeTruthy();
    expect(composer.value).toBe('half a thought');
  });

  it('does not fire in another text field, during IME composition or while another dialog is open', () => {
    setup();
    key({ key: 'k', ctrlKey: true }, screen.getByLabelText('Search'));
    expect(screen.queryByRole('dialog')).toBeNull();
    key({ key: 'k', ctrlKey: true, isComposing: true });
    expect(screen.queryByRole('dialog')).toBeNull();
    const other = document.createElement('div');
    other.setAttribute('role', 'dialog');
    other.setAttribute('aria-modal', 'true');
    document.body.append(other);
    key({ key: 'k', ctrlKey: true });
    expect(screen.queryByRole('dialog', { name: 'Find a channel' })).toBeNull();
    other.remove();
  });

  it('ignores a dialog that is in the page but hidden', () => {
    setup();
    const hidden = document.createElement('div');
    hidden.setAttribute('role', 'dialog');
    hidden.setAttribute('aria-modal', 'true');
    hidden.hidden = true;
    document.body.append(hidden);
    key({ key: 'k', ctrlKey: true });
    expect(screen.getByRole('dialog', { name: 'Find a channel' })).toBeTruthy();
    hidden.remove();
  });

  it('gives focus back to what opened it on Escape', () => {
    setup();
    const entry = screen.getByRole('button', { name: /Find a channel/ });
    entry.focus();
    fireEvent.click(entry);
    key({ key: 'Escape' }, box());
    expect(document.activeElement).toBe(entry);
  });
});

describe('searching', () => {
  it('filters by name and marks private, archived and unread', () => {
    setup({ unread: new Map([['book-club', 3]]) });
    fireEvent.click(screen.getByRole('button', { name: /Find a channel/ }));
    expect(screen.getAllByRole('option').filter((o) => within(o).queryByText(/Private/) !== null).length).toBe(1);
    const book = screen.getByRole('option', { name: /book-club/ });
    expect(book.textContent).toContain('Private');
    expect(within(book).getByRole('img', { name: '3 unread' })).toBeTruthy();
    expect(screen.getByRole('option', { name: /old-games/ }).textContent).toContain('Archived');
    fireEvent.change(box(), { target: { value: 'GAME' } });
    expect(screen.getAllByRole('option').map((o) => o.textContent)).toEqual([expect.stringContaining('game-night'), expect.stringContaining('old-games')]);
    fireEvent.change(box(), { target: { value: 'nothing here' } });
    expect(screen.getByText('No channels match.')).toBeTruthy();
  });

  it('moves with the arrows, opens with Enter and ignores keys during composition', () => {
    const { onOpen } = setup();
    fireEvent.click(screen.getByRole('button', { name: /Find a channel/ }));
    key({ key: 'ArrowDown' }, box());
    expect(box().getAttribute('aria-activedescendant')).toBe(screen.getByRole('option', { name: /game-night/ }).id);
    key({ key: 'Enter', isComposing: true }, box());
    expect(onOpen).not.toHaveBeenCalled();
    key({ key: 'Enter' }, box());
    expect(onOpen).toHaveBeenCalledWith('game-night');
    expect(screen.queryByRole('dialog')).toBeNull();
  });

  it('opens a channel on a click or tap', () => {
    const { onOpen } = setup();
    fireEvent.click(screen.getByRole('button', { name: /Find a channel/ }));
    fireEvent.click(screen.getByRole('option', { name: /book-club/ }));
    expect(onOpen).toHaveBeenCalledWith('book-club');
  });

  it('lists public channels to join apart, and choosing one browses it without joining', async () => {
    const join = vi.spyOn(api, 'joinChannel');
    const { onOpen, onBrowse } = setup();
    fireEvent.click(screen.getByRole('button', { name: /Find a channel/ }));
    const joinGroup = await screen.findByRole('group', { name: 'Join' });
    expect(within(joinGroup).getAllByRole('option').map((o) => o.textContent)).toEqual([expect.stringContaining('movie-night')]);
    fireEvent.click(within(joinGroup).getByRole('option'));
    expect(onBrowse).toHaveBeenCalledWith('movie-night');
    expect(onOpen).not.toHaveBeenCalled();
    expect(join).not.toHaveBeenCalled();
  });

  it('offers no Join group, and reads no directory, where the page gives no way to browse', () => {
    setup({ onBrowse: null });
    fireEvent.click(screen.getByRole('button', { name: /Find a channel/ }));
    expect(api.channelDirectory).not.toHaveBeenCalled();
    expect(screen.queryByRole('group', { name: 'Join' })).toBeNull();
  });

  it('drops a channel the viewer loses while the switcher is open', () => {
    const { again } = setup();
    fireEvent.click(screen.getByRole('button', { name: /Find a channel/ }));
    key({ key: 'ArrowDown' }, box());
    again({ channels: channels.filter((c) => c.name !== 'game-night') });
    expect(screen.queryByRole('option', { name: /game-night/ })).toBeNull();
    expect(screen.getAllByRole('option')[0]?.getAttribute('aria-selected')).toBe('true');
  });

  it('closes, and holds no directory names, when the identity changes', async () => {
    const { again } = setup();
    fireEvent.click(screen.getByRole('button', { name: /Find a channel/ }));
    await screen.findByRole('option', { name: /movie-night/ });
    again({ me: 2 });
    expect(screen.queryByRole('dialog')).toBeNull();
    expect(document.body.textContent).not.toContain('movie-night');
    fireEvent.click(screen.getByRole('button', { name: /Find a channel/ }));
    expect(screen.queryByRole('option', { name: /movie-night/ })).toBeNull();
  });
});

describe('moving between channels', () => {
  it('Alt+Down and Alt+Up move through the sidebar order', () => {
    const { onOpen } = setup();
    key({ key: 'ArrowDown', altKey: true });
    expect(onOpen).toHaveBeenLastCalledWith('game-night');
    key({ key: 'ArrowUp', altKey: true });
    expect(onOpen).toHaveBeenLastCalledWith('book-club');
  });

  it('Alt+Shift+Down and Up jump to unread channels', () => {
    const { onOpen } = setup({ unread: new Map([['book-club', 2]]) });
    key({ key: 'ArrowDown', altKey: true, shiftKey: true });
    expect(onOpen).toHaveBeenLastCalledWith('book-club');
  });

  it('says so, and does not move, when nothing else is unread', () => {
    const { onOpen } = setup();
    key({ key: 'ArrowDown', altKey: true, shiftKey: true });
    expect(onOpen).not.toHaveBeenCalled();
    expect(screen.getByRole('status').textContent).toBe('No unread channels');
  });

  it('fires in the composer but not in another text field or during composition', () => {
    const { onOpen } = setup({ unread: new Map([['book-club', 2]]) });
    key({ key: 'ArrowDown', altKey: true }, screen.getByLabelText('Search'));
    expect(onOpen).not.toHaveBeenCalled();
    key({ key: 'ArrowDown', altKey: true, shiftKey: true }, screen.getByLabelText('Message'));
    expect(onOpen).toHaveBeenCalledWith('book-club');
    onOpen.mockClear();
    key({ key: 'ArrowDown', altKey: true, shiftKey: true, isComposing: true });
    expect(onOpen).not.toHaveBeenCalled();
  });

  it('clears the status line after a while', () => {
    vi.useFakeTimers();
    try {
      setup();
      key({ key: 'ArrowUp', altKey: true, shiftKey: true });
      expect(screen.getByRole('status').textContent).toBe('No unread channels');
      act(() => {
        vi.advanceTimersByTime(5000);
      });
      expect(screen.getByRole('status').textContent).toBe('');
    } finally {
      vi.useRealTimers();
    }
  });
});
