import { cleanup, fireEvent, render, screen, waitFor, within } from '@testing-library/react';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import * as api from './api';
import { Conversation } from './Conversation';
import type { Audience, MessageV2 } from './schema/message';
import type { ChannelChat, PendingSend } from './state/messages';

const at = '2026-10-26T00:00:00Z';
const root = (audience?: Audience): MessageV2 => ({ schema: 'conch.message.v2', id: 1, channel_id: 7, author_id: 3, created_at: at, body: 'root', ...(audience === undefined ? {} : { audience }) });
const reply = (id: number, body = `reply ${String(id)}`): MessageV2 => ({ schema: 'conch.message.v2', id, channel_id: 7, author_id: 3, created_at: at, body, thread_root_id: 1 });
const people = new Map([[2, { name: 'ada', kind: 'human' as const }], [3, { name: 'bea', kind: 'human' as const }]]);

function chat(messages: readonly MessageV2[], pending: readonly PendingSend[] = []): ChannelChat {
  return { byId: new Map(messages.map((message) => [message.id, message])), pending, sentIds: new Set(), history: 'ready', older: 'idle', exhausted: true, trimmed: false, dropped: 0, nextLocalId: 3 };
}

function conversation(messages: readonly MessageV2[], pending: readonly PendingSend[] = [], onReplySend = vi.fn(), live: 'open' | 'reconnecting' = 'open') {
  return (
    <Conversation
      channel="general"
      chat={chat(messages, pending)}
      live={live}
      people={people}
      nets={new Map([[4, 'blue']])}
      viewer={2}
      draft="channel draft"
      target={{ net: null, whisper: null, refusal: null, note: null }}
      monitored={new Set()}
      onDraft={() => undefined}
      onTarget={() => undefined}
      onSend={() => undefined}
      onReplySend={onReplySend}
      onRetrySend={() => undefined}
      onDismissSend={() => undefined}
      onLoadOlder={() => undefined}
      onReload={() => undefined}
      onBottom={() => undefined}
    />
  );
}

function view(messages: readonly MessageV2[], pending: readonly PendingSend[] = [], onReplySend = vi.fn(), live: 'open' | 'reconnecting' = 'open') {
  return render(conversation(messages, pending, onReplySend, live));
}

beforeEach(() => {
  HTMLElement.prototype.scrollIntoView = vi.fn();
});

afterEach(() => {
  cleanup();
  vi.restoreAllMocks();
});

describe('one-level replies', () => {
  it('opens root and replies, preserves audience, sends by keyboard, and restores focus', async () => {
    const audience = { kind: 'net', net_id: 4 } as const;
    vi.spyOn(api, 'loadThread').mockResolvedValue({ ok: true, value: { messages: [root(audience), reply(2)] } });
    const onReplySend = vi.fn();
    view([root(audience)], [], onReplySend);
    fireEvent.click(screen.getByRole('button', { name: 'Reply to message 1' }));
    const thread = await screen.findByRole('region', { name: 'Thread around message 1' });
    expect(within(thread).getByText('root')).toBeTruthy();
    expect(within(thread).getByText('reply 2')).toBeTruthy();
    expect(within(thread).getByText('Replying to → net blue; the audience cannot be changed.')).toBeTruthy();
    expect(within(thread).queryByRole('button', { name: 'Reply to message 2' })).toBeNull();
    expect(within(thread).queryByText('To:')).toBeNull();
    const composer = within(thread).getByRole('textbox', { name: 'Reply to message #1' });
    expect(document.activeElement).toBe(composer);
    fireEvent.change(composer, { target: { value: 'answer' } });
    fireEvent.keyDown(composer, { key: 'Enter' });
    expect(onReplySend).toHaveBeenCalledWith('answer', 1, audience);
    expect(screen.getByRole('textbox', { name: 'Message #general' })).toHaveProperty('value', 'channel draft');
    fireEvent.click(within(thread).getByRole('button', { name: 'Close thread' }));
    expect(document.activeElement).toBe(screen.getByRole('button', { name: 'Reply to message 1' }));
  });

  it('deduplicates live replies and pages with next_after', async () => {
    const load = vi.spyOn(api, 'loadThread')
      .mockResolvedValueOnce({ ok: true, value: { messages: [root(), reply(2)], next_after: 2 } })
      .mockResolvedValueOnce({ ok: true, value: { messages: [reply(2), reply(3)] } });
    view([root(), reply(3)]);
    fireEvent.click(screen.getByRole('button', { name: 'Open thread for message 1' }));
    const thread = await screen.findByRole('region', { name: 'Thread around message 1' });
    expect(within(thread).getAllByRole('listitem')).toHaveLength(3);
    fireEvent.click(within(thread).getByRole('button', { name: 'Load more replies' }));
    await waitFor(() => {
      expect(within(thread).getAllByRole('listitem')).toHaveLength(3);
    });
    expect(load).toHaveBeenLastCalledWith('general', 1, 2);
    expect(within(thread).getAllByText('reply 3')).toHaveLength(1);
  });

  it('keeps reply pending rows inside the thread and disables sending on reconnect', async () => {
    vi.spyOn(api, 'loadThread').mockResolvedValue({ ok: true, value: { messages: [root()] } });
    const pending: PendingSend = { localId: 2, channelId: 7, authorId: 2, body: 'pending reply', at: 1, status: 'failed', phase: 'failed', attachments: [], reconcileAfter: 0, threadRootId: 1 };
    view([root()], [pending], vi.fn(), 'reconnecting');
    expect(screen.queryByText('pending reply')).toBeNull();
    fireEvent.click(screen.getByRole('button', { name: 'Reply to message 1' }));
    const thread = await screen.findByRole('region', { name: 'Thread around message 1' });
    expect(within(thread).getByText('pending reply')).toBeTruthy();
    expect(within(thread).getByRole('textbox', { name: 'Reply to message #1' })).toHaveProperty('disabled', true);
    expect(within(thread).getByRole('button', { name: 'Send reply' })).toHaveProperty('disabled', true);
  });

  it('shows an error and retries the initial page', async () => {
    const load = vi.spyOn(api, 'loadThread')
      .mockResolvedValueOnce({ ok: false, failure: 'network' })
      .mockResolvedValueOnce({ ok: true, value: { messages: [root()] } });
    view([root()]);
    fireEvent.click(screen.getByRole('button', { name: 'Reply to message 1' }));
    await screen.findByRole('alert');
    fireEvent.click(screen.getByRole('button', { name: 'Try again' }));
    await screen.findByRole('textbox', { name: 'Reply to message #1' });
    expect(load).toHaveBeenCalledTimes(2);
  });

  it('shows a reply that arrives live once, and again once after a reconnect refetch', async () => {
    vi.spyOn(api, 'loadThread').mockResolvedValue({ ok: true, value: { messages: [root(), reply(2)] } });
    const { rerender } = view([root(), reply(2)]);
    fireEvent.click(screen.getByRole('button', { name: 'Open thread for message 1' }));
    const thread = await screen.findByRole('region', { name: 'Thread around message 1' });
    expect(within(thread).getAllByRole('listitem')).toHaveLength(2);
    rerender(conversation([root(), reply(2), reply(3)]));
    expect(within(thread).getAllByText('reply 3')).toHaveLength(1);
    // After a reconnect the history is read again and holds the same replies.
    rerender(conversation([root(), reply(2), reply(3)], [], vi.fn(), 'reconnecting'));
    rerender(conversation([root(), reply(2), reply(3)]));
    expect(within(thread).getAllByRole('listitem')).toHaveLength(3);
  });

  it('offers a reply only on a root, never on a reply', () => {
    view([root(), reply(2)]);
    expect(screen.getAllByRole('button', { name: /^Reply to message/ })).toHaveLength(1);
    expect(screen.getByRole('button', { name: 'Open thread for message 1' })).toBeTruthy();
  });
});
