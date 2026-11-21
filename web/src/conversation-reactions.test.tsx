import { cleanup, fireEvent, render, screen, waitFor, within } from '@testing-library/react';
import { afterEach, beforeEach, expect, it, vi } from 'vitest';
import * as api from './api';
import { Conversation, Message } from './Conversation';
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

beforeEach(() => {
  HTMLElement.prototype.scrollIntoView = vi.fn();
  vi.spyOn(api, 'readReactions').mockResolvedValue({ ok: true, value: { summaries: [] } });
  vi.spyOn(api, 'loadThread').mockResolvedValue({ ok: true, value: { messages: [by(2, 2, { thread_root_id: 1 })] } });
});
afterEach(() => { cleanup(); vi.restoreAllMocks(); });

it('reaches thread reactions by Tab and keeps main-log arrows in their own bar', async () => {
  vi.mocked(api.readReactions).mockImplementation((_channel, ids) => Promise.resolve({
    ok: true,
    value: { summaries: ids.includes(2) ? [{
      schema: 'conch.reaction_summary.v1', channel_id: 7, message_id: 2,
      as_of: at, reactions: [{ emoji: '👍', count: 1, me: false }],
    }] : [] },
  }));
  render(view([by(1, 2)]));
  const root = document.querySelector<HTMLElement>('li[data-message-id="1"]');
  if (root === null) throw new Error('missing root');
  fireEvent.click(within(root).getByRole('button', { name: /^More actions for / }));
  fireEvent.click(within(root).getByRole('menuitem', { name: 'Reply' }));
  const thread = await screen.findByRole('region', { name: /Thread around/ });
  const chip = await within(thread).findByRole('button', { name: 'thumbs up: 1 reaction' });
  expect(chip.tabIndex).toBe(0);
  chip.focus();
  fireEvent.keyDown(chip, { key: 'Escape' });
  expect(document.activeElement?.getAttribute('data-message-id')).toBe('2');
  // An empty outer toolbar must not send Right Arrow into the nested thread.
  root.focus();
  fireEvent.keyDown(root, { key: 'ArrowRight' });
  expect(document.activeElement).toBe(root);
  expect(thread.querySelector('li[aria-description]')).toBeNull();
});

it.each([false, true])('does not advertise absent reaction navigation (removed=%s)', async (removed) => {
  if (!removed) vi.mocked(api.readReactions).mockResolvedValue({ ok: false, failure: 'invalid', code: 'reactions_unsupported' });
  render(view([by(1, 2, { removed })]));
  await waitFor(() => { expect(document.querySelector('.reaction-controls')).toBeNull(); });
  expect(document.querySelector('li[aria-description]')).toBeNull();
});

it('does not advertise arrow navigation in search/context messages', () => {
  render(<Message message={by(1, 2)} people={members} nets={new Map()} viewer={2} channel="general" sent={false} cont={false} />);
  expect(document.querySelector('li[aria-description]')).toBeNull();
});

it('offers Add reaction and Reply in the message menu without rendering an empty reaction row', async () => {
  render(view([by(1, 2)]));
  const root = document.querySelector<HTMLElement>('li[data-message-id="1"]');
  if (root === null) throw new Error('missing root');
  const button = within(root).getByRole('button', { name: /^More actions for / });
  expect(within(root).queryByRole('button', { name: /^(Add reaction|Reply)/ })).toBeNull();
  expect(root.hasAttribute('aria-description')).toBe(false);
  expect(root.querySelector('.reactions')).toBeNull();
  fireEvent.contextMenu(within(root).getByText('from 2'));
  const menu = within(root).getByRole('menu');
  expect(within(menu).getByRole('menuitem', { name: 'Reply' })).toBeTruthy();
  const add = within(menu).getByRole('menuitem', { name: 'Add reaction' });
  await waitFor(() => { expect(add.hasAttribute('aria-disabled')).toBe(false); });
  expect(root.querySelector('.reactions')).toBeNull();
  fireEvent.click(add);
  expect(within(root).getByRole('dialog', { name: 'Emoji' })).toBeTruthy();
  fireEvent.keyDown(screen.getByRole('searchbox'), { key: 'Escape' });
  expect(root.querySelector('.reactions')).toBeNull();
  expect(document.activeElement).toBe(button);
});
