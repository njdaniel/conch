// A message link's jump survives a resize of the log in the same frame
// (issue #2158). The jump scrolls the linked row into view; the log's scroll
// handler ignores a scroll at a height the resize observer has not handled
// (Chrome's scroll anchoring), so the jump itself says where it left the log.
// The test drives that sequence: a log with real-looking geometry, a jump
// that moves scrollTop, then a resize observer callback at a new height.
import { act, cleanup, render, waitFor } from '@testing-library/react';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import * as api from './api';
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

// The log's geometry, as a browser would report it: 3000 px of messages in a
// 400 px window. scrollTop is clamped to the scrollable range.
const geometry = { scrollHeight: 3000, clientHeight: 400, scrollTop: 0 };
const maxTop = () => geometry.scrollHeight - geometry.clientHeight;
// Where the next highlighted row's scrollIntoView leaves the log.
let jumpTo = 0;
// The resize observers the conversation made, called as a browser would.
const observers: (() => void)[] = [];

class FakeResizeObserver {
  constructor(private readonly callback: () => void) {}
  observe() { observers.push(this.callback); }
  unobserve() { /* nothing to do */ }
  disconnect() { observers.splice(observers.indexOf(this.callback), 1); }
}

function resize(clientHeight: number) {
  geometry.clientHeight = clientHeight;
  act(() => { for (const observe of [...observers]) observe(); });
}

function shaped(log: Element) {
  Object.defineProperty(log, 'scrollHeight', { configurable: true, get: () => geometry.scrollHeight });
  Object.defineProperty(log, 'clientHeight', { configurable: true, get: () => geometry.clientHeight });
  Object.defineProperty(log, 'scrollTop', {
    configurable: true,
    get: () => geometry.scrollTop,
    set: (value: number) => { geometry.scrollTop = Math.max(0, Math.min(value, maxTop())); },
  });
}

beforeEach(() => {
  geometry.scrollHeight = 3000;
  geometry.clientHeight = 400;
  geometry.scrollTop = 0;
  jumpTo = 0;
  observers.length = 0;
  vi.stubGlobal('ResizeObserver', FakeResizeObserver);
  HTMLElement.prototype.scrollIntoView = vi.fn(() => { geometry.scrollTop = Math.min(jumpTo, maxTop()); });
});

afterEach(() => {
  cleanup();
  vi.unstubAllGlobals();
  vi.restoreAllMocks();
});

// Mounts the conversation at the newest message, as a reader opening it is.
function opened(messages: readonly MessageV2[], onBottom: (atBottom: boolean) => void) {
  const shown = render(conversation(messages, null, onBottom));
  const log = shown.container.querySelector('.log');
  if (log === null) throw new Error('no log');
  shaped(log);
  resize(400);
  geometry.scrollTop = maxTop();
  return shown;
}

describe('a message-link jump (issue #2158)', () => {
  const root = by(1);
  const later = by(2);
  const reply = by(3, { thread_root_id: 1 });

  it('keeps an older linked row in view when the log resizes in the same frame', () => {
    const onBottom = vi.fn();
    const shown = opened([root, later], onBottom);
    jumpTo = 0;
    shown.rerender(conversation([root, later], root.id, onBottom));
    expect(geometry.scrollTop).toBe(0);
    // The voice dock settles: the log grows, before any scroll event.
    resize(430);
    expect(geometry.scrollTop).toBe(0);
    expect(onBottom).toHaveBeenLastCalledWith(false);
  });

  it('keeps following when the linked message is already at the bottom', () => {
    const onBottom = vi.fn();
    const shown = opened([root, later], onBottom);
    jumpTo = maxTop();
    shown.rerender(conversation([root, later], later.id, onBottom));
    expect(onBottom).toHaveBeenLastCalledWith(true);
    // The log grows taller; a reader at the newest message stays there.
    geometry.scrollHeight = 3200;
    resize(430);
    expect(geometry.scrollTop).toBe(maxTop());
  });

  it('keeps a linked thread reply in view: the thread is shown inline in the log', async () => {
    vi.spyOn(api, 'loadThread').mockResolvedValue({ ok: true, value: { messages: [root, reply] } });
    const onBottom = vi.fn();
    const shown = opened([root, later, reply], onBottom);
    jumpTo = 120;
    shown.rerender(conversation([root, later, reply], reply.id, onBottom));
    await waitFor(() => { expect(onBottom).toHaveBeenLastCalledWith(false); });
    expect(geometry.scrollTop).toBe(120);
    resize(430);
    expect(geometry.scrollTop).toBe(120);
  });
});
