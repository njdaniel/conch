// The message log as one tab stop (issue #1040): a roving tabindex over the
// messages, arrows between them, Tab into a message's own controls and out,
// and controls named by author and time, never by id.
import { act, cleanup, fireEvent, render, screen, within } from '@testing-library/react';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { clock, Conversation, messageName } from './Conversation';
import type { MessageV2 } from './schema/message';
import type { ChannelChat } from './state/messages';

const viewer = 2;
const people = new Map([[viewer, { name: 'member-1', kind: 'human' as const }], [3, { name: 'owner', kind: 'human' as const }]]);

function msg(id: number, fields: Partial<MessageV2> = {}): MessageV2 {
  return { schema: 'conch.message.v2', id, channel_id: 7, author_id: id % 2 === 0 ? viewer : 3, created_at: new Date(Date.now() - 60_000).toISOString(), body: `text ${String(id)}`, ...fields };
}

function chat(messages: readonly MessageV2[]): ChannelChat {
  return { byId: new Map(messages.map((m) => [m.id, m])), pending: [], sentIds: new Set(), history: 'ready', older: 'idle', exhausted: true, trimmed: false, dropped: 0, nextLocalId: 9 };
}

function conversation(messages: readonly MessageV2[]) {
  return (
    <Conversation
      channel="general"
      chat={chat(messages)}
      live="open"
      people={people}
      nets={new Map()}
      viewer={viewer}
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
      onEdited={() => undefined}
    />
  );
}

const many = (n: number) => Array.from({ length: n }, (_, i) => msg(i + 1));
const item = (id: number) => {
  const el = document.querySelector<HTMLElement>(`.messages > [data-message-id="${String(id)}"]`);
  if (el === null) throw new Error(`no message ${String(id)}`);
  return el;
};
const log = () => screen.getByRole('log', { name: 'Messages' });

// The elements Tab would stop at inside the log, in order: what the browser
// would visit, given each one's tabindex.
function stops(): HTMLElement[] {
  return Array.from(log().querySelectorAll<HTMLElement>('li[data-message-id], button, a[href], textarea, input')).filter((el) => el.tabIndex >= 0);
}

beforeEach(() => {
  HTMLElement.prototype.scrollIntoView = vi.fn();
});

afterEach(() => {
  cleanup();
  vi.restoreAllMocks();
});

describe('the log is one tab stop', () => {
  it('has the newest message and its own controls as its only stops, with 30 held', () => {
    render(conversation(many(30)));
    // 30 is member-1's: the message, ⋯ and Reply. Nothing else in the log.
    expect(stops().map((el) => el.getAttribute('data-message-id') ?? el.textContent)).toEqual(['30', '⋯', 'Reply']);
    expect(stops()[0]).toBe(item(30));
    expect(log().tabIndex).toBe(-1);
  });

  it('keeps the log itself focusable for scrolling when it holds no message', () => {
    render(conversation([]));
    expect(log().tabIndex).toBe(0);
  });

  it('moves the stop to the message last focused, with its controls', () => {
    render(conversation(many(6)));
    fireEvent.focus(item(3));
    expect(stops().map((el) => el.getAttribute('data-message-id') ?? el.textContent)).toEqual(['3', 'Reply']);
    // Focus on a control inside another message moves the stop there too.
    fireEvent.focus(within(item(4)).getByRole('button', { name: /^Reply to / }));
    expect(stops()[0]).toBe(item(4));
    expect(stops()).toHaveLength(3);
  });

  it('falls back to the newest when the message last focused is gone', () => {
    const { rerender } = render(conversation(many(6)));
    fireEvent.focus(item(2));
    rerender(conversation(many(6).filter((m) => m.id !== 2)));
    expect(stops()[0]).toBe(item(6));
  });
});

describe('arrow keys', () => {
  it('move focus between messages, and Home and End go to the first and last held', () => {
    render(conversation(many(5)));
    item(5).focus();
    fireEvent.keyDown(item(5), { key: 'ArrowUp' });
    expect(document.activeElement).toBe(item(4));
    fireEvent.keyDown(item(4), { key: 'ArrowUp' });
    expect(document.activeElement).toBe(item(3));
    fireEvent.keyDown(item(3), { key: 'ArrowDown' });
    expect(document.activeElement).toBe(item(4));
    fireEvent.keyDown(item(4), { key: 'Home' });
    expect(document.activeElement).toBe(item(1));
    fireEvent.keyDown(item(1), { key: 'ArrowUp' });
    expect(document.activeElement).toBe(item(1));
    fireEvent.keyDown(item(1), { key: 'End' });
    expect(document.activeElement).toBe(item(5));
    fireEvent.keyDown(item(5), { key: 'ArrowDown' });
    expect(document.activeElement).toBe(item(5));
  });

  it('are left alone when focus is on a control inside a message', () => {
    render(conversation(many(3)));
    const reply = within(item(3)).getByRole('button', { name: /^Reply to / });
    reply.focus();
    const proceeded = fireEvent.keyDown(reply, { key: 'ArrowUp' });
    expect(proceeded).toBe(true);
    expect(document.activeElement).toBe(reply);
  });

  it('are left alone with a modifier held', () => {
    render(conversation(many(3)));
    item(3).focus();
    fireEvent.keyDown(item(3), { key: 'ArrowUp', altKey: true });
    expect(document.activeElement).toBe(item(3));
  });

  it('mark the message they are on', () => {
    render(conversation(many(3)));
    act(() => { item(2).focus(); });
    expect(document.activeElement).toBe(item(2));
    expect(item(2).tabIndex).toBe(0);
    expect(item(3).tabIndex).toBe(-1);
  });
});

describe('a message arriving', () => {
  it('does not move focus, and moves the stop only when nothing was chosen', () => {
    const { rerender } = render(conversation(many(3)));
    act(() => { item(2).focus(); });
    rerender(conversation([...many(3), msg(4)]));
    expect(document.activeElement).toBe(item(2));
    expect(stops()[0]).toBe(item(2));

    cleanup();
    const fresh = render(conversation(many(3)));
    expect(stops()[0]).toBe(item(3));
    fresh.rerender(conversation([...many(3), msg(4)]));
    expect(stops()[0]).toBe(item(4));
    expect(document.activeElement).toBe(document.body);
  });
});

describe('the existing keyboard flows', () => {
  it('opens the menu and the editor from the keyboard in the stop message', () => {
    render(conversation(many(4)));
    const more = within(item(4)).getByRole('button', { name: /^More actions for / });
    more.focus();
    fireEvent.click(more);
    fireEvent.click(screen.getByRole('menuitem', { name: 'Edit' }));
    const box = within(item(4)).getByRole('textbox', { name: /^Edit member-1's message, / });
    expect(document.activeElement).toBe(box);
    expect(box.tabIndex).toBe(0);
  });
});

describe('names', () => {
  it('say whose message it is and when, and never carry an id', () => {
    const messages = many(12);
    render(conversation(messages));
    const stamp = clock((messages[11] as MessageV2).created_at);
    expect(messageName(messages[11] as MessageV2, people)).toBe(`member-1's message, ${stamp}`);
    expect(within(item(12)).getByRole('button', { name: `More actions for member-1's message, ${stamp}` })).toBeTruthy();
    expect(within(item(11)).getByRole('button', { name: /^Reply to owner's message, \d\d:\d\d$/ })).toBeTruthy();
    // Every name in the log is free of ids.
    for (const el of Array.from(log().querySelectorAll('[aria-label]'))) {
      expect(el.getAttribute('aria-label')).not.toMatch(/message \d/);
    }
  });
});
