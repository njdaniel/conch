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
    const composer = within(thread).getByRole('textbox', { name: 'Reply to bea' });
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
    expect(within(thread).getByRole('textbox', { name: 'Reply to bea' })).toHaveProperty('disabled', true);
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
    await screen.findByRole('textbox', { name: 'Reply to bea' });
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

  it('opens the thread of a reply whose root is not loaded, by the root id', async () => {
    const load = vi.spyOn(api, 'loadThread').mockResolvedValue({ ok: true, value: { messages: [root(), reply(2)] } });
    view([reply(2)]);
    fireEvent.click(screen.getByRole('button', { name: 'Open thread for message 1' }));
    const thread = await screen.findByRole('region', { name: 'Thread around message 1' });
    expect(load).toHaveBeenCalledWith('general', 1, 0);
    expect(within(thread).getByText('root')).toBeTruthy();
    expect(within(thread).getByRole('textbox', { name: 'Reply to bea' })).toBeTruthy();
    fireEvent.click(within(thread).getByRole('button', { name: 'Close thread' }));
    expect(document.activeElement).toBe(screen.getByRole('button', { name: 'Open thread for message 1' }));
  });

  it('returns focus to Open thread when it opened the thread', async () => {
    vi.spyOn(api, 'loadThread').mockResolvedValue({ ok: true, value: { messages: [root(), reply(2)] } });
    view([root(), reply(2)]);
    const opener = screen.getByRole('button', { name: 'Open thread for message 1' });
    fireEvent.click(opener);
    const thread = await screen.findByRole('region', { name: 'Thread around message 1' });
    fireEvent.click(within(thread).getByRole('button', { name: 'Close thread' }));
    expect(document.activeElement).toBe(opener);
  });

  it('keeps loaded pages and does not refetch when an unrelated message arrives', async () => {
    const load = vi.spyOn(api, 'loadThread')
      .mockResolvedValueOnce({ ok: true, value: { messages: [root(), reply(2)], next_after: 2 } })
      .mockResolvedValueOnce({ ok: true, value: { messages: [reply(3)] } });
    const { rerender } = view([root(), reply(2)]);
    fireEvent.click(screen.getByRole('button', { name: 'Open thread for message 1' }));
    const thread = await screen.findByRole('region', { name: 'Thread around message 1' });
    fireEvent.click(await within(thread).findByRole('button', { name: 'Load more replies' }));
    await within(thread).findByText('reply 3');
    const other: MessageV2 = { ...root(), id: 9, body: 'elsewhere' };
    rerender(conversation([root(), reply(2), other]));
    expect(within(thread).getByText('reply 3')).toBeTruthy();
    expect(within(thread).getByText('reply 2')).toBeTruthy();
    expect(load).toHaveBeenCalledTimes(2);
  });

  it('keeps the reply draft when loading older history moves the thread anchor', async () => {
    vi.spyOn(api, 'loadThread').mockResolvedValue({ ok: true, value: { messages: [root(), reply(2), reply(3)] } });
    const { rerender } = view([reply(3)]);
    fireEvent.click(screen.getByRole('button', { name: 'Open thread for message 1' }));
    const box = await screen.findByRole('textbox', { name: 'Reply to bea' });
    fireEvent.change(box, { target: { value: 'half written' } });
    rerender(conversation([root(), reply(2), reply(3)]));
    expect(screen.getByRole('textbox', { name: 'Reply to bea' })).toHaveProperty('value', 'half written');
  });
});

describe('what a reply answers', () => {
  const line = (id: number) => document.querySelector(`[data-message-id="${String(id)}"]`) as HTMLElement;

  it('quotes the first line of a held root, with its author', () => {
    view([{ ...root(), body: 'anyone up for game night?\nsecond line' }, reply(2)]);
    expect(within(line(2)).getByText('Replying to bea: anyone up for game night?')).toBeTruthy();
    const context = within(line(2)).getByText(/^Replying to/);
    expect(line(2).getAttribute('aria-describedby')).toBe(context.id);
    expect(within(line(1)).queryByText(/^Replying to/)).toBeNull();
  });

  it('cuts a long first line and renders markup as text', () => {
    view([{ ...root(), body: `<b>x</b>${'y'.repeat(100)}` }, reply(2)]);
    const text = within(line(2)).getByText(/^Replying to/).textContent;
    expect(text.startsWith('Replying to bea: <b>x</b>yyy')).toBe(true);
    expect(text.endsWith('…')).toBe(true);
    expect(Array.from(text.slice('Replying to bea: '.length)).length).toBe(81);
    expect(line(2).querySelector('b')).toBeNull();
  });

  it('says Reply in a thread when the root is not held', () => {
    view([reply(2)]);
    expect(within(line(2)).getByText('Reply in a thread')).toBeTruthy();
    expect(within(line(2)).queryByText(/^Replying to/)).toBeNull();
  });

  it('counts replies on the root: none, one, three, and one more live', () => {
    const { rerender } = view([root()]);
    expect(within(line(1)).queryByRole('button', { name: /repl(y|ies)$/ })).toBeNull();
    rerender(conversation([root(), reply(2)]));
    expect(within(line(1)).getByRole('button', { name: '1 reply' })).toBeTruthy();
    rerender(conversation([root(), reply(2), reply(3), reply(4)]));
    expect(within(line(1)).getByRole('button', { name: '3 replies' })).toBeTruthy();
    rerender(conversation([root(), reply(2), reply(3), reply(4), reply(5)]));
    expect(within(line(1)).getByRole('button', { name: '4 replies' })).toBeTruthy();
  });

  it('opens the thread from the count and labels the box by the root author', async () => {
    vi.spyOn(api, 'loadThread').mockResolvedValue({ ok: true, value: { messages: [root(), reply(2)] } });
    view([root(), reply(2)]);
    fireEvent.click(within(line(1)).getByRole('button', { name: '1 reply' }));
    const thread = await screen.findByRole('region', { name: 'Thread around message 1' });
    expect(within(thread).getByRole('textbox', { name: 'Reply to bea' })).toBeTruthy();
    expect(within(thread).queryByText(/^Replying to bea:/)).toBeNull();
  });
});
