// Editing one's own recent text in the browser (issue #845,
// docs/design/message-editing.md §13): the message actions menu and the four
// ways into it, the inline editor and its keys, a line for each refusal, the
// window closing while the editor is open, the "(edited)" marker and the
// author's Earlier versions.
import { act, cleanup, fireEvent, render, screen, waitFor, within } from '@testing-library/react';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import * as api from './api';
import { platform } from './platform';
import { Conversation } from './Conversation';
import { editWindowMs, when } from './MessageEdit';
import { longPressMs } from './MessageMenu';
import type { MessageV2 } from './schema/message';
import type { ChannelChat, PendingSend } from './state/messages';
import type { Person } from './useLiveChannel';

const viewer = 2;
const minute = 60_000;
const ago = (ms: number) => new Date(Date.now() - ms).toISOString();

function msg(id: number, fields: Partial<MessageV2> = {}): MessageV2 {
  return { schema: 'conch.message.v2', id, channel_id: 7, author_id: viewer, created_at: ago(minute), body: `text ${String(id)}`, ...fields };
}

const people = (kind: Person['kind'] = 'human') => new Map<number, Person>([[viewer, { name: 'ada', kind }], [3, { name: 'bea', kind: 'human' }]]);

function chat(messages: readonly MessageV2[], pending: readonly PendingSend[] = [], exhausted = true): ChannelChat {
  return { byId: new Map(messages.map((m) => [m.id, m])), pending, sentIds: new Set(), history: 'ready', older: 'idle', exhausted, trimmed: false, dropped: 0, nextLocalId: 9, edits: new Map(), final: new Set() };
}

interface Options {
  readonly highlightedId?: number;
  readonly held?: ChannelChat;
  readonly onEdited?: (message: MessageV2) => void;
  readonly kind?: Person['kind'];
  readonly pending?: readonly PendingSend[];
  readonly edits?: boolean;
  readonly channelId?: number;
  readonly exhausted?: boolean;
  readonly onLoadOlder?: () => void;
}

function conversation(messages: readonly MessageV2[], { highlightedId, held, onEdited = vi.fn(), kind = 'human', pending = [], edits = true, channelId, exhausted = true, onLoadOlder = () => undefined }: Options = {}) {
  return (
    <Conversation
      {...(channelId === undefined ? {} : { channelId })}
      highlightedId={highlightedId}
      channel="general"
      chat={held ?? chat(messages, pending, exhausted)}
      live="open"
      people={people(kind)}
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
      onLoadOlder={onLoadOlder}
      onReload={() => undefined}
      onBottom={() => undefined}
      {...(edits ? { onEdited } : {})}
    />
  );
}

// Controls are named by author and time, never by id (issue #1040), and two
// messages made a moment apart may fall in different minutes: each is found
// inside its own item, by the shape of its name.
const item = (id: number) => document.querySelector<HTMLElement>(`.log > .messages > [data-message-id="${String(id)}"]`);
const within1 = (id: number) => within(item(id) ?? document.body);
const moreName = /^More actions for ada's message, \d\d:\d\d$/;
const editName = /^Edit ada's message, \d\d:\d\d$/;
const more = (id: number) => within1(id).queryByRole('button', { name: /^More actions for/ });
const box = (id: number) => within1(id).getByRole('textbox', { name: editName });
const noBox = (id: number) => within1(id).queryByRole('textbox', { name: editName });

function openEditor(id: number): HTMLTextAreaElement {
  const button = more(id);
  if (button === null) throw new Error(`no menu on message ${String(id)}`);
  fireEvent.click(button);
  fireEvent.click(within(screen.getByRole('menu')).getByRole('menuitem', { name: 'Edit' }));
  return box(id) as HTMLTextAreaElement;
}

function openReply(id: number) {
  const row = within1(id);
  fireEvent.click(row.getByRole('button', { name: /^More actions for / }));
  fireEvent.click(row.getByRole('menuitem', { name: 'Reply' }));
}

// Copy actions remain even when Edit is unavailable (#1833).
function expectNoEdit(id: number) {
  const button = more(id);
  expect(button).not.toBeNull();
  if (button === null) throw new Error('no copy menu');
  fireEvent.click(button);
  expect(screen.queryByRole('menuitem', { name: 'Edit' })).toBeNull();
  expect(screen.getByRole('menuitem', { name: 'Copy link' })).toBeTruthy();
  fireEvent.keyDown(screen.getByRole('menu'), { key: 'Escape' });
}

beforeEach(() => {
  HTMLElement.prototype.scrollIntoView = vi.fn();
});

afterEach(() => {
  cleanup();
  window.sessionStorage.clear();
  vi.useRealTimers();
  vi.restoreAllMocks();
});

describe('which messages offer Edit', () => {
  it('only the viewer’s own text and attachment messages inside the window', () => {
    const own = msg(1);
    const attachment = msg(2, { payload: { schema: 'conch.file_attachment.v1', data: { file_ids: [5] } } });
    const others = msg(3, { author_id: 3 });
    const approval = msg(4, {
      payload: { schema: 'conch.approval.v1', data: { id: 1, title: 'Deploy', state: 'pending', requester_id: viewer } },
    });
    const old = msg(5, { created_at: ago(editWindowMs + minute) });
    const pending: PendingSend = { localId: 1, channelId: 7, authorId: viewer, body: 'on its way', at: Date.now(), status: 'failed', phase: 'failed', attachments: [], reconcileAfter: 5 };
    vi.spyOn(api, 'getFileMeta').mockResolvedValue({ ok: false, failure: 'unavailable' });
    render(conversation([own, attachment, others, approval, old], { pending: [pending] }));
    expect(more(1)).not.toBeNull();
    expect(more(2)).not.toBeNull();
    expectNoEdit(3);
    expectNoEdit(4);
    expectNoEdit(5);
    // A failed send has Retry and Dismiss, and no menu.
    expect(screen.getAllByRole('button', { name: /^More actions/ })).toHaveLength(5);
  });

  it('keeps copying for an agent and where the page cannot save an edit', () => {
    const { unmount } = render(conversation([msg(1)], { kind: 'agent' }));
    expectNoEdit(1);
    unmount();
    render(conversation([msg(1)], { edits: false }));
    expectNoEdit(1);
  });
});

describe('the actions menu', () => {
  it('opens from the ⋯ button, moves by arrow, and Escape returns focus to the button', () => {
    render(conversation([msg(1)]));
    const button = more(1);
    expect(button?.getAttribute('aria-expanded')).toBe('false');
    if (button === null) throw new Error('no button');
    fireEvent.click(button);
    expect(button.getAttribute('aria-expanded')).toBe('true');
    const menu = screen.getByRole('menu', { name: moreName });
    expect(document.activeElement).toBe(within(menu).getByRole('menuitem', { name: 'Edit' }));
    fireEvent.keyDown(menu, { key: 'ArrowDown' });
    expect(document.activeElement).toBe(within(menu).getByRole('menuitem', { name: 'Reply' }));
    fireEvent.keyDown(menu, { key: 'Escape' });
    expect(screen.queryByRole('menu')).toBeNull();
    expect(document.activeElement).toBe(button);
  });

  it('opens on right-click, suppressing the browser’s menu there only', () => {
    render(conversation([msg(1), msg(3, { author_id: 3 })]));
    const own = item(1);
    const others = item(3);
    if (own === null || others === null) throw new Error('no items');
    expect(fireEvent.contextMenu(within(own).getByText('text 1'))).toBe(false);
    expect(screen.getByRole('menu', { name: moreName })).toBeTruthy();
    fireEvent.keyDown(screen.getByRole('menu'), { key: 'Escape' });
    // Someone else's message offers copying, while Edit stays absent.
    expect(fireEvent.contextMenu(within(others).getByText('text 3'))).toBe(false);
    expect(screen.queryByRole('menuitem', { name: 'Edit' })).toBeNull();
    expect(screen.getByRole('menuitem', { name: 'Copy text' })).toBeTruthy();
  });

  it('opens on a long press, and not on a tap or a scroll', () => {
    vi.useFakeTimers();
    render(conversation([msg(1)]));
    const text = screen.getByText('text 1');
    fireEvent.touchStart(text, { touches: [{ clientX: 10, clientY: 10 }] });
    act(() => {
      vi.advanceTimersByTime(longPressMs / 2);
    });
    fireEvent.touchEnd(text);
    act(() => {
      vi.advanceTimersByTime(longPressMs);
    });
    expect(screen.queryByRole('menu')).toBeNull();
    fireEvent.touchStart(text, { touches: [{ clientX: 10, clientY: 10 }] });
    fireEvent.touchMove(text, { touches: [{ clientX: 10, clientY: 60 }] });
    act(() => {
      vi.advanceTimersByTime(longPressMs);
    });
    expect(screen.queryByRole('menu')).toBeNull();
    fireEvent.touchStart(text, { touches: [{ clientX: 10, clientY: 10 }] });
    act(() => {
      vi.advanceTimersByTime(longPressMs);
    });
    expect(screen.getByRole('menu', { name: moreName })).toBeTruthy();
  });

  it('closes on a press outside it', () => {
    render(conversation([msg(1)]));
    const button = more(1);
    if (button === null) throw new Error('no button');
    fireEvent.click(button);
    expect(screen.getByRole('menu')).toBeTruthy();
    fireEvent.pointerDown(document.body);
    expect(screen.queryByRole('menu')).toBeNull();
  });
});

describe('the inline editor', () => {
  // F18: a touch screen has no Enter or Esc to name.
  it('says Save or Cancel on a coarse pointer, and keeps the keys on a desktop', () => {
    vi.stubGlobal('matchMedia', (q: string) => ({ matches: q === '(pointer: coarse)', addEventListener: () => undefined, removeEventListener: () => undefined }));
    try {
      render(conversation([msg(1, { body: 'helo, conch' })]));
      openEditor(1);
      expect(screen.getByText('Editing. Save or Cancel.')).toBeTruthy();
      expect(screen.queryByText(/Enter to save/)).toBeNull();
    } finally {
      vi.unstubAllGlobals();
    }
  });

  it('holds the text, focused with the caret at the end, and says how to save', () => {
    render(conversation([msg(1, { body: 'helo, conch' })]));
    const editor = openEditor(1);
    expect(editor.value).toBe('helo, conch');
    expect(document.activeElement).toBe(editor);
    expect(editor.selectionStart).toBe('helo, conch'.length);
    expect(screen.getByText('Editing. Enter to save, Esc to cancel')).toBeTruthy();
    // Save is off until the text changes, and for white space alone.
    const save = screen.getByRole('button', { name: 'Save' });
    expect(save).toHaveProperty('disabled', true);
    fireEvent.change(editor, { target: { value: '   ' } });
    expect(save).toHaveProperty('disabled', true);
  });

  it('Enter saves; the server’s copy goes to the page and focus back to ⋯', async () => {
    const saved = msg(1, { body: 'hello, conch', edited_at: new Date().toISOString() });
    const edit = vi.spyOn(api, 'editMessage').mockResolvedValue({ ok: true, message: saved });
    const onEdited = vi.fn();
    render(conversation([msg(1, { body: 'helo, conch' })], { onEdited }));
    const editor = openEditor(1);
    fireEvent.change(editor, { target: { value: 'hello, conch' } });
    fireEvent.keyDown(editor, { key: 'Enter' });
    expect(edit).toHaveBeenCalledWith('general', 1, 'hello, conch');
    expect(screen.getByRole('button', { name: 'Saving…' })).toHaveProperty('disabled', true);
    expect(editor.readOnly).toBe(true);
    await waitFor(() => {
      expect(onEdited).toHaveBeenCalledWith(saved);
    });
    expect(noBox(1)).toBeNull();
    await waitFor(() => {
      expect(document.activeElement).toBe(more(1));
    });
  });

  it('Shift+Enter adds a line, and Enter while an input method composes does nothing', () => {
    const edit = vi.spyOn(api, 'editMessage');
    render(conversation([msg(1)]));
    const editor = openEditor(1);
    fireEvent.change(editor, { target: { value: 'two\nlines' } });
    fireEvent.keyDown(editor, { key: 'Enter', shiftKey: true });
    fireEvent.keyDown(editor, { key: 'Enter', isComposing: true });
    expect(edit).not.toHaveBeenCalled();
    expect(box(1)).toBeTruthy();
  });

  it('Escape cancels, restores the text and returns focus to ⋯', () => {
    const edit = vi.spyOn(api, 'editMessage');
    render(conversation([msg(1, { body: 'as it was' })]));
    const editor = openEditor(1);
    fireEvent.change(editor, { target: { value: 'changed my mind' } });
    fireEvent.keyDown(editor, { key: 'Escape' });
    expect(edit).not.toHaveBeenCalled();
    expect(noBox(1)).toBeNull();
    expect(screen.getByText('as it was')).toBeTruthy();
    expect(document.activeElement).toBe(more(1));
  });

  it('keeps the person’s text while a live edit from another tab changes the message under it', () => {
    const view = render(conversation([msg(1, { body: 'first' })]));
    const editor = openEditor(1);
    fireEvent.change(editor, { target: { value: 'mine' } });
    view.rerender(conversation([msg(1, { body: 'from my other tab', edited_at: new Date().toISOString() })]));
    expect(box(1)).toHaveProperty('value', 'mine');
  });

  it('asks before dropping unsaved text for another message', () => {
    render(conversation([msg(1), msg(2)]));
    const first = openEditor(1);
    fireEvent.change(first, { target: { value: 'unsaved' } });
    fireEvent.click(within1(2).getByRole('button', { name: moreName }));
    fireEvent.click(screen.getByRole('menuitem', { name: 'Edit' }));
    const ask = screen.getByRole('alertdialog', { name: 'Discard your edit?' });
    expect(document.activeElement).toBe(within(ask).getByRole('button', { name: 'Discard' }));
    fireEvent.click(within(ask).getByRole('button', { name: 'Keep editing' }));
    expect(box(1)).toHaveProperty('value', 'unsaved');
    expect(document.activeElement).toBe(box(1));
    fireEvent.click(within1(2).getByRole('button', { name: moreName }));
    fireEvent.click(screen.getByRole('menuitem', { name: 'Edit' }));
    fireEvent.click(screen.getByRole('button', { name: 'Discard' }));
    expect(noBox(1)).toBeNull();
    expect(box(2)).toHaveProperty('value', 'text 2');
  });

  it('opens another without asking when nothing was changed', () => {
    render(conversation([msg(1), msg(2)]));
    openEditor(1);
    fireEvent.click(within1(2).getByRole('button', { name: moreName }));
    fireEvent.click(screen.getByRole('menuitem', { name: 'Edit' }));
    expect(screen.queryByRole('alertdialog')).toBeNull();
    expect(noBox(1)).toBeNull();
    expect(box(2)).toBeTruthy();
  });
});

describe('refusals', () => {
  const lines: [string, api.EditResult, string, boolean][] = [
    ['the window closed', { ok: false, failure: 'refused', code: 'message_edit_window_closed' }, 'This message can no longer be edited: it is more than an hour old.', true],
    ['the edit limit', { ok: false, failure: 'refused', code: 'message_edit_limit_reached' }, 'This message has been edited the most times allowed.', true],
    ['a message that cannot be edited', { ok: false, failure: 'refused', code: 'message_not_editable' }, "This message can't be edited.", true],
    ['an archived channel (issue #844)', { ok: false, failure: 'refused', code: 'channel_archived' }, 'This channel is archived.', true],
    ['no answer', { ok: false, failure: 'network' }, "Couldn't save. Your text is kept.", false],
    ['a refusal of no known code', { ok: false, failure: 'refused', code: 'invalid_request' }, "Couldn't save. Your text is kept.", false],
  ];
  it.each(lines)('%s: its line, under the editor, with the text kept', async (_name, answer, line, final) => {
    const edit = vi.spyOn(api, 'editMessage').mockResolvedValue(answer);
    render(conversation([msg(1)]));
    const editor = openEditor(1);
    fireEvent.change(editor, { target: { value: 'new words' } });
    fireEvent.click(screen.getByRole('button', { name: 'Save' }));
    expect((await screen.findByRole('alert')).textContent).toBe(line);
    expect(box(1)).toHaveProperty('value', 'new words');
    expect(screen.getByRole('button', { name: 'Save' })).toHaveProperty('disabled', final);
    if (!final) {
      // Save again is a retry, which is safe: the same text twice changes nothing.
      fireEvent.click(screen.getByRole('button', { name: 'Save' }));
      expect(edit).toHaveBeenCalledTimes(2);
    }
  });

  it.each(['message_not_found', 'channel_not_found'])('%s: the editor closes and says the message is gone', async (code) => {
    vi.spyOn(api, 'editMessage').mockResolvedValue({ ok: false, failure: 'refused', code });
    render(conversation([msg(1)]));
    fireEvent.change(openEditor(1), { target: { value: 'new words' } });
    fireEvent.click(screen.getByRole('button', { name: 'Save' }));
    expect(await screen.findByText('This message is no longer available to you.')).toBeTruthy();
    expect(noBox(1)).toBeNull();
  });

  it('rate_limited: says how long, and holds Save back until then', async () => {
    vi.spyOn(api, 'editMessage').mockResolvedValue({ ok: false, failure: 'refused', code: 'rate_limited', retryAfter: 3 });
    render(conversation([msg(1)]));
    fireEvent.change(openEditor(1), { target: { value: 'new words' } });
    vi.useFakeTimers();
    fireEvent.click(screen.getByRole('button', { name: 'Save' }));
    await act(async () => {
      await Promise.resolve();
    });
    expect(screen.getByRole('alert').textContent).toBe('Too many edits. Try again in 3 seconds.');
    expect(screen.getByRole('button', { name: 'Save' })).toHaveProperty('disabled', true);
    act(() => {
      vi.advanceTimersByTime(3000);
    });
    expect(screen.getByRole('button', { name: 'Save' })).toHaveProperty('disabled', false);
  });

  it('the window closing while editing: the text stays, the server says no, and Edit is no longer offered', async () => {
    vi.useFakeTimers();
    const created = new Date(Date.now() - editWindowMs + minute).toISOString();
    const edit = vi.spyOn(api, 'editMessage').mockResolvedValue({ ok: false, failure: 'refused', code: 'message_edit_window_closed' });
    render(conversation([msg(1, { created_at: created }), msg(2, { created_at: created })]));
    const editor = openEditor(1);
    fireEvent.change(editor, { target: { value: 'too late' } });
    act(() => {
      vi.advanceTimersByTime(2 * minute);
    });
    // The open editor is the person's; the other message stops offering Edit.
    expect(box(1)).toHaveProperty('value', 'too late');
    expectNoEdit(2);
    fireEvent.click(screen.getByRole('button', { name: 'Save' }));
    await act(async () => {
      await Promise.resolve();
    });
    expect(edit).toHaveBeenCalledWith('general', 1, 'too late');
    expect(screen.getByRole('alert').textContent).toBe('This message can no longer be edited: it is more than an hour old.');
    fireEvent.click(screen.getByRole('button', { name: 'Cancel' }));
    expect(noBox(1)).toBeNull();
    // Focus returns to the copy menu after Edit disappears.
    expect(document.activeElement).toBe(more(1));
    expectNoEdit(1);
  });
});

// The edit limit (issue #1031): the page counts the edits it has seen, and a
// refusal ends the message's edits for good.
describe('the edit limit', () => {
  const stamps = (n: number) => new Set(Array.from({ length: n }, (_, i) => `2026-10-14T10:${String(i + 1).padStart(2, '0')}:00Z`));
  const known = (n: number): ChannelChat => ({ ...chat([msg(1, { edited_at: '2026-10-14T10:09:00Z' })]), edits: new Map([[1, stamps(n)]]) });
  const rendered = (held: ChannelChat, onEditFinal: (id: number) => void = vi.fn()) => (
    <Conversation
      channel="general"
      chat={held}
      live="open"
      people={people()}
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
      onEdited={vi.fn()}
      onEditFinal={onEditFinal}
    />
  );

  it('offers Edit with nine known edits and not with ten', () => {
    const { unmount } = render(rendered(known(9)));
    expect(more(1)).not.toBeNull();
    unmount();
    render(rendered(known(10)));
    expectNoEdit(1);
  });

  it('a page opened after the edits, knowing none, still offers Edit', () => {
    render(rendered(chat([msg(1, { edited_at: '2026-10-14T10:09:00Z' })])));
    expect(more(1)).not.toBeNull();
  });

  it('a message marked final is not offered Edit', () => {
    render(rendered({ ...chat([msg(1)]), final: new Set([1]) }));
    expectNoEdit(1);
  });

  it.each(['message_edit_limit_reached', 'message_edit_window_closed'])('%s tells the page the message is final, keeping the text', async (code) => {
    vi.spyOn(api, 'editMessage').mockResolvedValue({ ok: false, failure: 'refused', code });
    const final = vi.fn();
    render(rendered(chat([msg(1)]), final));
    fireEvent.change(openEditor(1), { target: { value: 'new words' } });
    fireEvent.click(screen.getByRole('button', { name: 'Save' }));
    await screen.findByRole('alert');
    expect(final).toHaveBeenCalledWith(1);
    expect(box(1)).toHaveProperty('value', 'new words');
  });

  it('a 429 with ten known edits says the limit, not the rate, and ends the edits', async () => {
    const held = known(9);
    vi.spyOn(api, 'editMessage').mockResolvedValue({ ok: false, failure: 'refused', code: 'rate_limited', retryAfter: 5 });
    const final = vi.fn();
    const { rerender } = render(rendered(held, final));
    fireEvent.change(openEditor(1), { target: { value: 'new words' } });
    // The tenth edit arrives from another tab while the editor is open.
    rerender(rendered({ ...held, edits: new Map([[1, stamps(10)]]) }, final));
    fireEvent.click(screen.getByRole('button', { name: 'Save' }));
    expect((await screen.findByRole('alert')).textContent).toBe('This message has been edited the most times allowed.');
    expect(final).toHaveBeenCalledWith(1);
    expect(screen.getByRole('button', { name: 'Save' })).toHaveProperty('disabled', true);
  });

  it('a 429 answered after the tenth edit arrived while Save was pending says the limit', async () => {
    const held = known(9);
    let answer: (r: api.EditResult) => void = () => undefined;
    vi.spyOn(api, 'editMessage').mockReturnValue(new Promise((resolve) => { answer = resolve; }));
    const final = vi.fn();
    const { rerender } = render(rendered(held, final));
    fireEvent.change(openEditor(1), { target: { value: 'new words' } });
    fireEvent.click(screen.getByRole('button', { name: 'Save' }));
    rerender(rendered({ ...held, edits: new Map([[1, stamps(10)]]) }, final));
    await act(async () => {
      answer({ ok: false, failure: 'refused', code: 'rate_limited', retryAfter: 5 });
      await Promise.resolve();
    });
    expect((await screen.findByRole('alert')).textContent).toBe('This message has been edited the most times allowed.');
    expect(final).toHaveBeenCalledWith(1);
  });

  it('a rate line showing when the tenth edit becomes known turns into the limit line', async () => {
    const held = known(9);
    vi.spyOn(api, 'editMessage').mockResolvedValue({ ok: false, failure: 'refused', code: 'rate_limited', retryAfter: 5 });
    const final = vi.fn();
    const { rerender } = render(rendered(held, final));
    fireEvent.change(openEditor(1), { target: { value: 'new words' } });
    fireEvent.click(screen.getByRole('button', { name: 'Save' }));
    expect((await screen.findByRole('alert')).textContent).toBe('Too many edits. Try again in 5 seconds.');
    rerender(rendered({ ...held, edits: new Map([[1, stamps(10)]]) }, final));
    expect((await screen.findByRole('alert')).textContent).toBe('This message has been edited the most times allowed.');
    expect(final).toHaveBeenCalledWith(1);
  });

  it('a 429 with fewer known edits still says the rate', async () => {
    vi.spyOn(api, 'editMessage').mockResolvedValue({ ok: false, failure: 'refused', code: 'rate_limited', retryAfter: 5 });
    const final = vi.fn();
    render(rendered(known(3), final));
    fireEvent.change(openEditor(1), { target: { value: 'new words' } });
    fireEvent.click(screen.getByRole('button', { name: 'Save' }));
    expect((await screen.findByRole('alert')).textContent).toBe('Too many edits. Try again in 5 seconds.');
    expect(final).not.toHaveBeenCalled();
  });
});

describe('the (edited) marker', () => {
  const editedAt = new Date();
  editedAt.setHours(12, 40, 0, 0);

  it('is a <time> named for when, and for another reader plain text', () => {
    render(conversation([msg(1, { author_id: 3, edited_at: editedAt.toISOString() })]));
    const marker = screen.getByLabelText(`Edited at ${when(editedAt.toISOString())}`);
    expect(marker.tagName).toBe('TIME');
    expect(marker.getAttribute('dateTime')).toBe(editedAt.toISOString());
    expect(marker.textContent).toBe('(edited)');
    expect(screen.queryByRole('button', { name: /^Edited at/ })).toBeNull();
    expect(when(editedAt.toISOString())).toBe('12:40');
  });

  it('names the date as well when it is not today', () => {
    expect(when('2026-01-02T03:04:00Z', new Date('2026-10-30T12:00:00Z'))).toMatch(/2026.*\d\d:\d\d$/);
  });

  it('for the author opens Earlier versions, read only when opened, newest first', async () => {
    const revisions = vi.spyOn(api, 'messageRevisions').mockResolvedValue({
      ok: true,
      value: [
        { revision: 1, body: 'helo', created_at: '2026-10-30T12:34:00Z' },
        { revision: 2, body: 'hello', created_at: '2026-10-30T12:40:00Z' },
      ],
    });
    render(conversation([msg(1, { body: 'hello', edited_at: editedAt.toISOString() })]));
    expect(revisions).not.toHaveBeenCalled();
    const marker = screen.getByRole('button', { name: `Edited at ${when(editedAt.toISOString())}` });
    expect(marker.querySelector('time')).not.toBeNull();
    fireEvent.click(marker);
    const dialog = screen.getByRole('dialog', { name: 'Earlier versions' });
    expect(revisions).toHaveBeenCalledWith('general', 1);
    expect(within(dialog).getByText('Only you can see earlier versions. They are kept on the server.')).toBeTruthy();
    const list = await within(dialog).findByRole('list', { name: 'Versions, newest first' });
    const entries = within(list).getAllByRole('listitem');
    expect(entries.map((e) => e.querySelector('.version-body')?.textContent)).toEqual(['hello', 'helo']);
    expect(entries[0]?.textContent).toContain('Current');
    expect(entries[1]?.textContent).not.toContain('Current');
    fireEvent.keyDown(dialog, { key: 'Escape' });
    expect(screen.queryByRole('dialog')).toBeNull();
    expect(document.activeElement).toBe(marker);
  });

  it('marks and titles a link in an earlier version inside the dialog’s Tab cycle', async () => {
    vi.spyOn(api, 'messageRevisions').mockResolvedValue({
      ok: true,
      value: [
        { revision: 1, body: 'see https://example.com/old', created_at: '2026-10-30T12:34:00Z' },
        { revision: 2, body: 'hello', created_at: '2026-10-30T12:40:00Z' },
      ],
    });
    render(conversation([msg(1, { body: 'hello', edited_at: editedAt.toISOString() })]));
    fireEvent.click(screen.getByRole('button', { name: `Edited at ${when(editedAt.toISOString())}` }));
    const dialog = screen.getByRole('dialog', { name: 'Earlier versions' });
    const link = await within(dialog).findByRole('link', { name: 'https://example.com/old (opens in a new tab)' });
    expect(link.getAttribute('title')).toBe('https://example.com/old');
    expect(link.textContent).toBe('https://example.com/old');
    expect(link.querySelector('.external-link-cue .icon')).not.toBeNull();
    const close = within(dialog).getByRole('button', { name: 'Close' });
    // The link comes before Close. Tab from Close, the last control, wraps to
    // the link; Shift+Tab from the link wraps back to Close.
    close.focus();
    fireEvent.keyDown(close, { key: 'Tab' });
    expect(document.activeElement).toBe(link);
    fireEvent.keyDown(link, { key: 'Tab', shiftKey: true });
    expect(document.activeElement).toBe(close);
  });

  it('closes Earlier versions when the server says the message is not the viewer’s', async () => {
    vi.spyOn(api, 'messageRevisions').mockResolvedValue({ ok: false, failure: 'invalid', code: 'message_not_found' });
    render(conversation([msg(1, { edited_at: editedAt.toISOString() })]));
    fireEvent.click(screen.getByRole('button', { name: /^Edited at/ }));
    await waitFor(() => {
      expect(screen.queryByRole('dialog')).toBeNull();
    });
  });

  it('shows another failure in the dialog, with a way to try again', async () => {
    const revisions = vi.spyOn(api, 'messageRevisions').mockResolvedValue({ ok: false, failure: 'network' });
    render(conversation([msg(1, { edited_at: editedAt.toISOString() })]));
    fireEvent.click(screen.getByRole('button', { name: /^Edited at/ }));
    const dialog = screen.getByRole('dialog', { name: 'Earlier versions' });
    expect((await within(dialog).findByRole('alert')).textContent).toContain("Couldn't load the earlier versions.");
    fireEvent.click(within(dialog).getByRole('button', { name: 'Try again' }));
    expect(revisions).toHaveBeenCalledTimes(2);
  });
});

describe('an open edit across a reload (issue #1039)', () => {
  it('is kept as it is typed, and reopens with its text while the message is editable', () => {
    const first = render(conversation([msg(1, { body: 'helo, conch' })], { channelId: 7 }));
    fireEvent.change(openEditor(1), { target: { value: 'hello, half done' } });
    first.unmount();
    render(conversation([msg(1, { body: 'helo, conch' })], { channelId: 7 }));
    expect((box(1) as HTMLTextAreaElement).value).toBe('hello, half done');
  });

  it('is dropped once the message is no longer editable', () => {
    const first = render(conversation([msg(1, { body: 'helo, conch' })], { channelId: 7 }));
    fireEvent.change(openEditor(1), { target: { value: 'hello, half done' } });
    first.unmount();
    render(conversation([msg(1, { body: 'helo, conch', created_at: ago(editWindowMs + minute) })], { channelId: 7 }));
    expect(noBox(1)).toBeNull();
    expect(window.sessionStorage.length).toBe(0);
  });

  it('on a message below the page read is kept while the page asks for older ones, and reopens when it arrives', () => {
    const first = render(conversation([msg(1, { body: 'old one' }), msg(5)], { channelId: 7 }));
    fireEvent.change(openEditor(1), { target: { value: 'old one, changed' } });
    first.unmount();
    const onLoadOlder = vi.fn();
    const page = render(conversation([msg(5), msg(6)], { channelId: 7, exhausted: false, onLoadOlder }));
    expect(onLoadOlder).toHaveBeenCalled();
    expect(noBox(1)).toBeNull();
    expect(window.sessionStorage.getItem('conch.web.drafts')).toContain('old one, changed');
    page.rerender(conversation([msg(1, { body: 'old one' }), msg(5), msg(6)], { channelId: 7, exhausted: false, onLoadOlder }));
    expect((box(1) as HTMLTextAreaElement).value).toBe('old one, changed');
  });

  it('on a message that is not there once every page is read is dropped', () => {
    const first = render(conversation([msg(1), msg(5)], { channelId: 7 }));
    fireEvent.change(openEditor(1), { target: { value: 'changed' } });
    first.unmount();
    render(conversation([msg(5)], { channelId: 7, exhausted: true }));
    expect(noBox(1)).toBeNull();
    expect(window.sessionStorage.length).toBe(0);
  });

  it('is forgotten when the editor is cancelled', () => {
    const first = render(conversation([msg(1)], { channelId: 7 }));
    fireEvent.change(openEditor(1), { target: { value: 'changed' } });
    fireEvent.keyDown(box(1), { key: 'Escape' });
    first.unmount();
    render(conversation([msg(1)], { channelId: 7 }));
    expect(noBox(1)).toBeNull();
  });

  it('works with storage refused', () => {
    vi.spyOn(Storage.prototype, 'getItem').mockImplementation(() => {
      throw new Error('blocked');
    });
    vi.spyOn(Storage.prototype, 'setItem').mockImplementation(() => {
      throw new Error('blocked');
    });
    render(conversation([msg(1, { body: 'helo' })], { channelId: 7 }));
    fireEvent.change(openEditor(1), { target: { value: 'fixed' } });
    expect((box(1) as HTMLTextAreaElement).value).toBe('fixed');
  });
});

describe('Up from the composer', () => {
  const composer = () => screen.getByRole('textbox', { name: 'Message #general' });
  it('edits the newest own message regardless of map order, then Escape returns to the composer', () => {
    render(conversation([msg(4, { author_id: 3 }), msg(2), msg(1)]));
    composer().focus();
    fireEvent.keyDown(composer(), { key: 'ArrowUp' });
    expect(document.activeElement).toBe(box(2));
    expect(noBox(1)).toBeNull();
    fireEvent.keyDown(box(2), { key: 'Escape' });
    expect(noBox(2)).toBeNull();
    expect(document.activeElement).toBe(composer());
  });

  it.each([
    ['no messages', []],
    ['only another author', [msg(1, { author_id: 3 })]],
    ['newest outside the window', [msg(1), msg(2, { created_at: ago(editWindowMs + minute) })]],
    ['removed newest', [msg(1), msg(2, { removed: true })]],
  ] as const)('leaves Up alone with %s, without falling back', (_name, messages) => {
    render(conversation(messages));
    expect(fireEvent.keyDown(composer(), { key: 'ArrowUp' })).toBe(true);
    expect(screen.queryByRole('textbox', { name: editName })).toBeNull();
  });

  it.each(['ten edits', 'final refusal'])('does not skip a newest message with %s', (reason) => {
    const held = { ...chat([msg(1), msg(2)]),
      edits: new Map([[2, new Set(Array.from({ length: 10 }, (_, i) => String(i)))]]),
      final: new Set(reason === 'final refusal' ? [2] : []),
    };
    if (reason === 'final refusal') held.edits.clear();
    render(conversation([], { held }));
    expect(fireEvent.keyDown(composer(), { key: 'ArrowUp' })).toBe(true);
    expect(screen.queryByRole('textbox', { name: editName })).toBeNull();
  });

  it('refocuses an existing edit without replacing unsaved text', () => {
    render(conversation([msg(1)]));
    const editor = openEditor(1);
    fireEvent.change(editor, { target: { value: 'unsaved' } });
    composer().focus();
    expect(fireEvent.keyDown(composer(), { key: 'ArrowUp' })).toBe(false);
    expect(box(1)).toBe(editor);
    expect(editor.value).toBe('unsaved');
    expect(document.activeElement).toBe(editor);
  });

  it.each(['close button', 'thread_not_found'])('asks before closing a dirty thread edit through %s, then allows channel editing', async (reason) => {
    const root = msg(1, { author_id: 3 });
    const reply = msg(2, { thread_root_id: 1 });
    vi.spyOn(api, 'loadThread').mockResolvedValueOnce({ ok: true, value: { messages: [root, reply], next_after: 2 } })
      .mockResolvedValue({ ok: false, failure: 'invalid', code: 'thread_not_found' });
    render(conversation([root, msg(3)]));
    openReply(root.id);
    const thread = screen.getByRole('region', { name: /^Thread around/ });
    await within(thread).findByText('text 2');
    fireEvent.keyDown(within(thread).getByRole('textbox', { name: 'Reply to bea' }), { key: 'ArrowUp' });
    fireEvent.change(within(thread).getByRole('textbox', { name: editName }), { target: { value: 'unsaved' } });
    const close = () => fireEvent.click(within(thread).getByRole('button', { name: reason === 'close button' ? 'Close thread' : 'Load more replies' }));
    close();
    await within(thread).findByRole('alertdialog', { name: 'Discard your edit?' });
    fireEvent.click(within(thread).getByRole('button', { name: 'Keep editing' }));
    expect(within(thread).getByRole('textbox', { name: editName })).toHaveProperty('value', 'unsaved');
    close();
    await within(thread).findByRole('alertdialog', { name: 'Discard your edit?' });
    fireEvent.click(within(thread).getByRole('button', { name: 'Discard' }));
    expect(screen.queryByRole('region', { name: /^Thread around/ })).toBeNull();
    expect(openEditor(3).value).toBe('text 3');
    expect(screen.queryByRole('alertdialog')).toBeNull();
  });

  it('keeps a thread edit and its origin when the same message is requested from the channel', async () => {
    const root = msg(1, { author_id: 3 });
    const reply = msg(2, { thread_root_id: 1 });
    vi.spyOn(api, 'loadThread').mockResolvedValue({ ok: true, value: { messages: [root, reply] } });
    render(conversation([root, reply]));
    openReply(root.id);
    const thread = screen.getByRole('region', { name: /^Thread around/ });
    await within(thread).findByText('text 2');
    const replyBox = within(thread).getByRole('textbox', { name: 'Reply to bea' });
    fireEvent.keyDown(replyBox, { key: 'ArrowUp' });
    const editor = within(thread).getByRole('textbox', { name: editName });
    fireEvent.change(editor, { target: { value: 'unsaved' } });
    fireEvent.click(within1(2).getByRole('button', { name: moreName }));
    fireEvent.click(screen.getByRole('menuitem', { name: 'Edit' }));
    expect(within(thread).getByRole('textbox', { name: editName })).toBe(editor);
    expect(editor).toHaveProperty('value', 'unsaved');
    expect(document.activeElement).toBe(editor);
    composer().focus();
    fireEvent.keyDown(composer(), { key: 'ArrowUp' });
    expect(within(thread).getByRole('textbox', { name: editName })).toBe(editor);
    expect(editor).toHaveProperty('value', 'unsaved');
    fireEvent.keyDown(editor, { key: 'Escape' });
    expect(document.activeElement).toBe(replyBox);
  });

  it('keeps the current surface while another edit request awaits discard confirmation', async () => {
    const root = msg(1, { author_id: 3 });
    const reply = msg(2, { thread_root_id: 1 });
    vi.spyOn(api, 'loadThread').mockResolvedValue({ ok: true, value: { messages: [root, reply] } });
    render(conversation([root, msg(3)]));
    const editor = openEditor(3);
    fireEvent.change(editor, { target: { value: 'unsaved channel text' } });
    openReply(root.id);
    const thread = screen.getByRole('region', { name: /^Thread around/ });
    await within(thread).findByText('text 2');
    const replyBox = within(thread).getByRole('textbox', { name: 'Reply to bea' });
    fireEvent.keyDown(replyBox, { key: 'ArrowUp' });
    expect(box(3)).toBe(editor);
    expect(within(thread).queryByRole('textbox', { name: editName })).toBeNull();
    fireEvent.click(screen.getByRole('button', { name: 'Keep editing' }));
    expect(document.activeElement).toBe(editor);
    fireEvent.keyDown(replyBox, { key: 'ArrowUp' });
    fireEvent.click(screen.getByRole('button', { name: 'Discard' }));
    const threadEditor = within(thread).getByRole('textbox', { name: editName });
    expect(document.activeElement).toBe(threadEditor);
    fireEvent.keyDown(threadEditor, { key: 'Escape' });
    expect(document.activeElement).toBe(replyBox);
  });

  it.each(['message_not_found', 'channel_not_found'])('shows %s after a failed edit of a reply outside channel history', async (code) => {
    const root = msg(1, { author_id: 3 });
    const reply = msg(2, { thread_root_id: 1 });
    vi.spyOn(api, 'loadThread').mockResolvedValue({ ok: true, value: { messages: [root, reply] } });
    vi.spyOn(api, 'editMessage').mockResolvedValue({ ok: false, failure: 'refused', code });
    render(conversation([root]));
    openReply(root.id);
    const thread = screen.getByRole('region', { name: /^Thread around/ });
    await within(thread).findByText('text 2');
    fireEvent.keyDown(within(thread).getByRole('textbox', { name: 'Reply to bea' }), { key: 'ArrowUp' });
    fireEvent.change(within(thread).getByRole('textbox', { name: editName }), { target: { value: 'fixed' } });
    fireEvent.click(within(thread).getByRole('button', { name: 'Save' }));
    await waitFor(() => { expect(within(thread).queryByRole('textbox', { name: editName })).toBeNull(); });
    expect(within(thread).getByText('This message is no longer available to you.')).toBeTruthy();
  });

  it('edits the newest own message in the open thread, including replies outside channel history', async () => {
    const root = msg(1, { author_id: 3 });
    const reply = msg(2, { thread_root_id: 1 });
    vi.spyOn(api, 'loadThread').mockResolvedValue({ ok: true, value: { messages: [root, reply] } });
    const onEdited = vi.fn();
    vi.spyOn(api, 'editMessage').mockResolvedValue({ ok: true, message: { ...reply, body: 'fixed', edited_at: new Date().toISOString() } });
    render(conversation([root, msg(3)], { onEdited }));
    openReply(root.id);
    const thread = screen.getByRole('region', { name: /^Thread around/ });
    await within(thread).findByText('text 2');
    const replyBox = within(thread).getByRole('textbox', { name: 'Reply to bea' });
    fireEvent.keyDown(replyBox, { key: 'ArrowUp' });
    const editor = within(thread).getByRole('textbox', { name: editName });
    expect(document.activeElement).toBe(editor);
    expect(noBox(3)).toBeNull();
    fireEvent.keyDown(editor, { key: 'Escape' });
    expect(document.activeElement).toBe(replyBox);
    fireEvent.keyDown(replyBox, { key: 'ArrowUp' });
    fireEvent.change(within(thread).getByRole('textbox', { name: editName }), { target: { value: 'fixed' } });
    fireEvent.click(within(thread).getByRole('button', { name: 'Save' }));
    await waitFor(() => { expect(onEdited).toHaveBeenCalledWith(expect.objectContaining({ id: 2, body: 'fixed' })); });
    expect(within(thread).getByText('fixed')).toBeTruthy();
    expect(within(thread).queryByRole('textbox', { name: editName })).toBeNull();
  });
});


describe('message link and clipboard focus', () => {
  it('keeps focus on a recent linked reply when its older root arrives', async () => {
    const root = msg(1, { author_id: 3 });
    const reply = msg(110, { thread_root_id: 1 });
    let release: () => void = () => undefined;
    const gate = new Promise<void>((resolve) => { release = resolve; });
    vi.spyOn(api, 'loadThread').mockImplementation(async () => {
      await gate;
      return { ok: true, value: { messages: [root, reply] } };
    });
    render(conversation([reply], { highlightedId: reply.id }));
    const thread = screen.getByRole('region', { name: 'Thread' });
    const linked = thread.querySelector('[data-message-id="110"]');
    expect(document.activeElement).toBe(linked);
    expect(within(thread).queryByRole('textbox')).toBeNull();
    await act(async () => { release(); await gate; });
    await within(thread).findByRole('textbox', { name: 'Reply to bea' });
    expect(document.activeElement).toBe(linked);
  });

  it.each(['Copy link', 'Copy text'])('returns focus to the menu button after %s succeeds', async (action) => {
    vi.spyOn(platform.services, 'copyText').mockResolvedValue();
    render(conversation([msg(1)], { edits: false }));
    const button = more(1);
    if (button === null) throw new Error('no copy menu');
    button.focus();
    fireEvent.click(button);
    const entry = screen.getByRole('menuitem', { name: action });
    entry.focus();
    fireEvent.click(entry);
    await screen.findByText(action === 'Copy link' ? 'Link copied.' : 'Text copied.');
    expect(document.activeElement).toBe(button);
  });

  it.each([false, true])('removes manual copy content on a tombstone (late clipboard failure: %s)', async (late) => {
    let refuse: () => void = () => undefined;
    const gate = new Promise<void>((_resolve, reject) => { refuse = () => { reject(new Error('refused')); }; });
    const copy = vi.spyOn(platform.services, 'copyText').mockReturnValue(gate);
    const original = msg(1, { body: 'secret removed text' });
    const view = render(conversation([original], { edits: false }));
    fireEvent.click(more(1) as HTMLElement);
    fireEvent.click(screen.getByRole('menuitem', { name: 'Copy text' }));
    await waitFor(() => { expect(copy).toHaveBeenCalledWith(original.body); });
    if (!late) {
      await act(async () => { refuse(); await Promise.resolve(); });
      const field = await screen.findByLabelText<HTMLTextAreaElement>('Copy manually');
      expect(field.value).toBe(original.body);
      expect(document.activeElement).toBe(field);
    }
    view.rerender(conversation([{ ...original, body: 'This message was removed by a moderator.', removed: true }], { edits: false }));
    if (late) await act(async () => { refuse(); await Promise.resolve(); });
    expect(screen.queryByLabelText('Copy manually')).toBeNull();
    expect(screen.queryByText('Clipboard unavailable. Copy the selected text.')).toBeNull();
    expect(document.body.textContent).not.toContain(original.body);
  });
});
