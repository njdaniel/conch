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
  render(view([by(1, 2)]));
  const root = document.querySelector<HTMLElement>('li[data-message-id="1"]');
  if (root === null) throw new Error('missing root');
  await waitFor(() => { expect(within(root).getByRole('button', { name: /Add reaction/ }).hasAttribute('disabled')).toBe(false); });
  root.focus();
  fireEvent.keyDown(root, { key: 'ArrowRight' });
  const ownAdd = within(root).getByRole('button', { name: /Add reaction/ });
  expect(document.activeElement).toBe(ownAdd);
  expect(ownAdd.tabIndex).toBe(-1);
  fireEvent.click(within(root).getByRole('button', { name: /Reply to/ }));
  const thread = await screen.findByRole('region', { name: /Thread around/ });
  await waitFor(() => { expect(within(thread).getAllByRole('button', { name: /Add reaction/ })).toHaveLength(2); });
  const replyAdd = within(thread).getAllByRole('button', { name: /Add reaction/ })[1];
  await waitFor(() => { expect(replyAdd?.hasAttribute('disabled')).toBe(false); });
  expect(replyAdd?.tabIndex).toBe(0);
  replyAdd?.focus();
  fireEvent.keyDown(replyAdd ?? thread, { key: 'Escape' });
  expect(document.activeElement?.getAttribute('data-message-id')).toBe('2');
  // With the outer toolbar disabled, Right must not enter the nested thread.
  ownAdd.setAttribute('disabled', '');
  root.focus();
  fireEvent.keyDown(root, { key: 'ArrowRight' });
  expect(document.activeElement).toBe(root);
  expect(thread.querySelector('li[aria-description]')).toBeNull();
});

it.each([false, true])('does not advertise absent reaction navigation (removed=%s)', async (removed) => {
  if (!removed) vi.mocked(api.readReactions).mockResolvedValue({ ok: false, failure: 'invalid', code: 'reactions_unsupported' });
  render(view([by(1, 2, { removed })]));
  await waitFor(() => { expect(document.querySelector('.reactions')).toBeNull(); });
  expect(document.querySelector('li[aria-description]')).toBeNull();
});

it('does not advertise arrow navigation in search/context messages', () => {
  render(<Message message={by(1, 2)} people={members} nets={new Map()} viewer={2} channel="general" sent={false} cont={false} />);
  expect(document.querySelector('li[aria-description]')).toBeNull();
});
