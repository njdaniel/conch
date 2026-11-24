// A message link's jump leaves the newest message (issue #2158): the log is
// marked not at the bottom when a main-log row is highlighted, as after the
// unread jump, so a resize of the log in the same frame (the voice dock
// settling on its first presence document) does not scroll the reader back
// to the newest message and the linked row out of view. A thread reply is
// highlighted in the thread view and leaves the main log's state alone.
import { cleanup, render } from '@testing-library/react';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { Conversation } from './Conversation';
import type { MessageV2 } from './schema/message';
import type { ChannelChat } from './state/messages';

const at = '2026-11-24T00:00:00Z';
const by = (id: number, extra: Partial<MessageV2> = {}): MessageV2 => ({ schema: 'conch.message.v2', id, channel_id: 7, author_id: 2, created_at: at, body: `message ${String(id)}`, ...extra });

function chat(messages: readonly MessageV2[]): ChannelChat {
  return { byId: new Map(messages.map((m) => [m.id, m])), pending: [], sentIds: new Set(), history: 'ready', older: 'idle', exhausted: true, trimmed: false, dropped: 0, nextLocalId: 1, edits: new Map(), final: new Set() };
}

function conversation(messages: readonly MessageV2[], highlightedId: number | null, onBottom: (atBottom: boolean) => void) {
  return (
    <Conversation
      channel="general"
      chat={chat(messages)}
      live="open"
      people={new Map()}
      nets={new Map()}
      viewer={2}
      draft=""
      target={{ net: null, whisper: null, refusal: null, note: null }}
      monitored={new Set()}
      highlightedId={highlightedId}
      onDraft={() => undefined}
      onTarget={() => undefined}
      onSend={() => undefined}
      onReplySend={() => undefined}
      onRetrySend={() => undefined}
      onDismissSend={() => undefined}
      onLoadOlder={() => undefined}
      onReload={() => undefined}
      onBottom={onBottom}
    />
  );
}

beforeEach(() => {
  HTMLElement.prototype.scrollIntoView = vi.fn();
});

afterEach(cleanup);

describe('a message-link jump (issue #2158)', () => {
  const root = by(1);
  const later = by(2);
  const reply = by(3, { thread_root_id: 1 });

  it('marks the log not at the newest message when a main-log row is linked', () => {
    const onBottom = vi.fn();
    const shown = render(conversation([root, later, reply], null, onBottom));
    expect(onBottom).not.toHaveBeenCalledWith(false);
    shown.rerender(conversation([root, later, reply], root.id, onBottom));
    expect(onBottom).toHaveBeenCalledWith(false);
  });

  it('says so once per link, not again on a later render while it is highlighted', () => {
    const onBottom = vi.fn();
    const shown = render(conversation([root, later], root.id, onBottom));
    const once = onBottom.mock.calls.filter(([bottom]) => bottom === false).length;
    expect(once).toBe(1);
    shown.rerender(conversation([root, later, by(4)], root.id, onBottom));
    expect(onBottom.mock.calls.filter(([bottom]) => bottom === false)).toHaveLength(once);
  });

  it('leaves the main log alone for a linked thread reply', () => {
    const onBottom = vi.fn();
    render(conversation([root, later, reply], reply.id, onBottom));
    expect(onBottom).not.toHaveBeenCalledWith(false);
  });
});
