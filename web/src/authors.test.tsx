// Departed authors (design §13.5, O14d): the author of a message who is not in
// the member list is named by the channel's authors route, and reads #<id>
// only where the route did not name them.
import { act, cleanup, fireEvent, render, screen, waitFor, within } from '@testing-library/react';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import * as api from './api';
import { Conversation, Message } from './Conversation';
import { longPressMs } from './MessageMenu';
import type { MessageV2 } from './schema/message';
import type { ChannelChat } from './state/messages';
import type { Person } from './useLiveChannel';

const at = '2026-10-26T00:00:00Z';
const by = (id: number, author: number, extra: Partial<MessageV2> = {}): MessageV2 => ({ schema: 'conch.message.v2', id, channel_id: 7, author_id: author, created_at: at, body: `from ${String(author)}`, ...extra });
const members = new Map<number, Person>([[2, { name: 'ada', kind: 'human' }]]);

function chat(messages: readonly MessageV2[]): ChannelChat {
  return { byId: new Map(messages.map((m) => [m.id, m])), pending: [], sentIds: new Set(), history: 'ready', older: 'idle', exhausted: true, trimmed: false, dropped: 0, nextLocalId: 3, edits: new Map(), final: new Set() };
}

function view(messages: readonly MessageV2[], people = members, channel = 'general') {
  return (
    <Conversation
      channel={channel}
      chat={chat(messages)}
      live="open"
      people={people}
      nets={new Map()}
      viewer={2}
      draft=""
      target={{ net: null, whisper: null, refusal: null, note: null }}
      monitored={new Set()}
      onDraft={() => undefined}
      onTarget={() => undefined}
      onSend={() => undefined}
      onReplySend={() => undefined}
      onRetrySend={() => undefined}
      onDismissSend={() => undefined}
      onLoadOlder={() => undefined}
      onReload={() => undefined}
      onBottom={() => undefined}
    />
  );
}

const authorNames = () => [...document.querySelectorAll('.msg-head .author')].map((n) => n.textContent);

beforeEach(() => {
  HTMLElement.prototype.scrollIntoView = vi.fn();
});
afterEach(() => {
  cleanup();
  vi.restoreAllMocks();
});

describe('naming authors who are not in the member list', () => {
  it('names them through the route, and reads #<id> for the id it did not name', async () => {
    const asked = vi.spyOn(api, 'channelAuthors').mockResolvedValue({ ok: true, value: [{ principal_id: 9, name: 'dee', kind: 'human' }] });
    render(view([by(1, 2), by(2, 9), by(3, 11)]));
    await waitFor(() => {
      expect(authorNames()).toEqual(['ada', 'dee', '#11']);
    });
    expect(asked).toHaveBeenCalledTimes(1);
    expect(asked).toHaveBeenCalledWith('general', [9, 11]);
  });

  it('shows #<id> and asks nothing about a guest author or a member already listed', async () => {
    const asked = vi.spyOn(api, 'channelAuthors').mockResolvedValue({ ok: true, value: [] });
    render(view([by(1, 2), by(2, 5, { guest_author: { handle: 'guest-1', tag: 'ab12' } })]));
    await screen.findAllByText('from 2');
    expect(asked).not.toHaveBeenCalled();
  });

  it('reads #<id> when the route does not answer', async () => {
    const asked = vi.spyOn(api, 'channelAuthors').mockResolvedValue({ ok: false, failure: 'network' });
    render(view([by(1, 9)]));
    await waitFor(() => {
      expect(asked).toHaveBeenCalled();
    });
    expect(authorNames()).toEqual(['#9']);
  });

  it('does not keep a name when the member list is replaced', async () => {
    const asked = vi.spyOn(api, 'channelAuthors').mockResolvedValueOnce({ ok: true, value: [{ principal_id: 9, name: 'dee', kind: 'human' }] }).mockResolvedValue({ ok: true, value: [] });
    const { rerender } = render(view([by(1, 9)]));
    await waitFor(() => {
      expect(authorNames()).toEqual(['dee']);
    });
    rerender(view([by(1, 9)], new Map(members)));
    await waitFor(() => {
      expect(asked).toHaveBeenCalledTimes(2);
    });
    expect(authorNames()).toEqual(['#9']);
  });

  it('prefers the member list to the route', async () => {
    vi.spyOn(api, 'channelAuthors').mockResolvedValue({ ok: true, value: [{ principal_id: 2, name: 'old-ada', kind: 'human' }] });
    render(view([by(1, 2)]));
    await screen.findByText('from 2');
    expect(authorNames()).toEqual(['ada']);
  });
});

describe('author identity marks', () => {
  const header = (body: string): HTMLElement => {
    const found = screen.getByText(body).closest('li')?.querySelector<HTMLElement>('.msg-head');
    if (found === null || found === undefined) throw new Error('no author header');
    return found;
  };

  it('marks an agent author in the TUI text order', () => {
    render(view([by(1, 3)], new Map([[3, { name: 'bot', kind: 'agent' }]])));
    const head = header('from 3');
    expect(head.querySelector('[data-identity="agent"]')).not.toBeNull();
    expect(head.textContent).toMatch(/^\d{2}:\d{2} bot \[agent\]: $/);
  });

  it('marks a manager author only when the people map carries manages', () => {
    const without = new Map<number, Person>([[3, { name: 'bob', kind: 'human' }]]);
    const shown = render(view([by(1, 3)], without));
    expect(header('from 3').querySelector('[data-identity="manager"]')).toBeNull();
    shown.rerender(view([by(1, 3)], new Map([[3, { name: 'bob', kind: 'human', manages: true, role: 'operator' }]])));
    const head = header('from 3');
    expect(head.querySelector('[data-identity="manager"]')).not.toBeNull();
    expect(head.querySelector('[data-identity="operator"]')).not.toBeNull();
    expect(head.querySelector('[data-identity="owner"], [data-identity="approver"]')).toBeNull();

    fireEvent.click(within(head).getByRole('button', { name: 'bob' }));
    const card = screen.getByRole('dialog', { name: 'bob' });
    expect(card.querySelector('.avatar')).not.toBeNull();
    expect(within(card).getByText('Person')).toBeDefined();
    expect(within(card).getByText('Manager')).toBeDefined();
    expect(within(card).getByText('Operator')).toBeDefined();
    expect(card.querySelector('.profile-card-presence')).toBeNull();
  });

  it('gives an authors-route author Agent and no member standing, presence or new read on open', async () => {
    const asked = vi.spyOn(api, 'channelAuthors').mockResolvedValue({ ok: true, value: [{ principal_id: 9, name: 'former-bot', kind: 'agent' }] });
    render(view([by(1, 9)]));
    await screen.findByText('former-bot');
    const head = header('from 9');
    expect(head.querySelector('[data-identity="agent"]')).not.toBeNull();
    expect(head.querySelector('[data-identity="owner"], [data-identity="operator"], [data-identity="manager"], [data-identity="approver"]')).toBeNull();

    asked.mockClear();
    const requests = vi.spyOn(globalThis, 'fetch');
    fireEvent.click(within(head).getByRole('button', { name: 'former-bot' }));
    const card = screen.getByRole('dialog', { name: 'former-bot' });
    expect(card.querySelector('.profile-card-kind')?.textContent).toBe('Agent');
    expect(card.querySelectorAll('.identity-mark')).toHaveLength(1);
    expect(card.querySelector('[data-identity="agent"]')).not.toBeNull();
    expect(card.querySelector('.profile-card-presence')).toBeNull();
    expect(card.textContent).not.toMatch(/membership|member list|online|stale|offline/);
    expect(asked).not.toHaveBeenCalled();
    expect(requests).not.toHaveBeenCalled();
  });
});

describe('the author name and the message gestures', () => {
  it('keeps Line accessible text unchanged and opens from its native button', () => {
    render(view([by(1, 3)], new Map([[3, { name: 'bot', kind: 'agent' }]])));
    const head = screen.getByText('from 3').closest('li')?.querySelector<HTMLElement>('.msg-head');
    if (head === null || head === undefined) throw new Error('no author header');
    expect(head.textContent).toMatch(/^\d{2}:\d{2} bot \[agent\]: $/);
    const name = within(head).getByRole('button', { name: 'bot' });
    name.focus();
    fireEvent.click(name);
    expect(screen.getByRole('dialog', { name: 'bot' })).toBeDefined();
    expect(document.activeElement).toBe(screen.getByRole('button', { name: 'Close' }));
  });

  it('opens the card on a tap but still opens the message menu on a long press', () => {
    vi.useFakeTimers();
    try {
      render(<Message message={by(1, 2)} people={members} nets={new Map()} viewer={2} channel="general" sent={false} cont={false} actions={[{ key: 'edit', label: 'Edit', onSelect: vi.fn() }]} />);
      const name = screen.getByRole('button', { name: 'ada' });
      fireEvent.touchStart(name, { touches: [{ clientX: 10, clientY: 10 }] });
      fireEvent.touchEnd(name);
      fireEvent.click(name);
      expect(screen.getByRole('dialog', { name: 'ada' })).toBeDefined();
      expect(screen.queryByRole('menu')).toBeNull();
      fireEvent.keyDown(screen.getByRole('dialog'), { key: 'Escape' });

      fireEvent.touchStart(name, { touches: [{ clientX: 10, clientY: 10 }] });
      act(() => {
        vi.advanceTimersByTime(longPressMs);
      });
      const menu = screen.getByRole('menu', { name: /^More actions for ada/ });
      expect(menu).toBeDefined();
      expect(screen.queryByRole('dialog')).toBeNull();
      fireEvent.touchEnd(name);
      fireEvent.click(name);
      expect(screen.getByRole('menu', { name: /^More actions for ada/ })).toBe(menu);
      expect(screen.queryByRole('dialog')).toBeNull();
      expect(document.activeElement).toBe(within(menu).getAllByRole('menuitem')[0]);
    } finally {
      vi.useRealTimers();
    }
  });
});
