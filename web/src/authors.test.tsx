// Departed authors (design §13.5, O14d): the author of a message who is not in
// the member list is named by the channel's authors route, and reads #<id>
// only where the route did not name them.
import { cleanup, render, screen, waitFor } from '@testing-library/react';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import * as api from './api';
import { Conversation } from './Conversation';
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
  });

  it('gives an authors-route author Agent and no member standing', async () => {
    vi.spyOn(api, 'channelAuthors').mockResolvedValue({ ok: true, value: [{ principal_id: 9, name: 'former-bot', kind: 'agent' }] });
    render(view([by(1, 9)]));
    await screen.findByText('former-bot');
    const head = header('from 9');
    expect(head.querySelector('[data-identity="agent"]')).not.toBeNull();
    expect(head.querySelector('[data-identity="owner"], [data-identity="operator"], [data-identity="manager"], [data-identity="approver"]')).toBeNull();
  });
});
