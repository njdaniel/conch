import { cleanup, fireEvent, render, screen, waitFor, within } from '@testing-library/react';
import { useState } from 'react';
import { afterEach, describe, expect, it } from 'vitest';
import { Composer, ComposerAttachmentProvider } from './Composer';
import type { DraftAttachment } from './state/attachments';

afterEach(cleanup);

const target = { net: null, whisper: null, refusal: null, note: null };

function Harness({ initial = '', enabled = true, onSend = () => undefined }: { readonly initial?: string; readonly enabled?: boolean; readonly onSend?: () => void }) {
  const [draft, setDraft] = useState(initial);
  return (
    <Composer
      channel="general"
      target={target}
      nets={new Map()}
      monitored={new Set()}
      people={new Map()}
      viewer={2}
      draft={draft}
      enabled={enabled}
      paused={false}
      onDraft={setDraft}
      onTarget={() => undefined}
      onSend={onSend}
      atBottom={() => undefined}
    />
  );
}

const box = () => screen.getByRole<HTMLTextAreaElement>('textbox', { name: /Message/ });
const focused = (el: Element | undefined) => {
  expect(el).toBeDefined();
  expect(document.activeElement).toBe(el);
};

function type(text: string, caret = text.length): void {
  const el = box();
  el.focus();
  fireEvent.change(el, { target: { value: text, selectionStart: caret, selectionEnd: caret } });
  el.setSelectionRange(caret, caret);
  fireEvent.select(el);
}

describe('the emoji picker', () => {
  it('opens a labelled dialog with focus in the search box, and Escape returns focus to the button', async () => {
    render(<Harness />);
    const open = screen.getByRole('button', { name: 'Add emoji' });
    expect(open.getAttribute('aria-expanded')).toBe('false');
    fireEvent.click(open);
    const dialog = screen.getByRole('dialog', { name: 'Emoji' });
    expect(open.getAttribute('aria-expanded')).toBe('true');
    const search = within(dialog).getByRole('searchbox', { name: 'Search emoji' });
    focused(search);
    await within(dialog).findByRole('button', { name: 'grinning face' });
    fireEvent.keyDown(search, { key: 'Escape' });
    expect(screen.queryByRole('dialog')).toBeNull();
    focused(open);
  });

  it('browses a category and inserts the fully-qualified key at the caret, then returns focus to the box', async () => {
    render(<Harness initial="helloworld" />);
    const el = box();
    el.setSelectionRange(5, 5);
    fireEvent.click(screen.getByRole('button', { name: 'Add emoji' }));
    fireEvent.change(await screen.findByRole('combobox', { name: 'Category' }), { target: { value: '7' } });
    expect(await screen.findByRole('group', { name: 'Symbols' })).not.toBeNull();
    fireEvent.change(screen.getByRole('combobox', { name: 'Category' }), { target: { value: '0' } });
    fireEvent.click(await screen.findByRole('button', { name: 'grinning face' }));
    expect(screen.queryByRole('dialog')).toBeNull();
    expect(el.value).toBe('hello😀world');
    focused(el);
    expect(el.selectionStart).toBe('hello😀'.length);
  });

  it('replaces the selection', async () => {
    render(<Harness initial="abcdef" />);
    const el = box();
    el.setSelectionRange(1, 4);
    fireEvent.click(screen.getByRole('button', { name: 'Add emoji' }));
    fireEvent.change(screen.getByRole('searchbox'), { target: { value: 'red heart' } });
    fireEvent.click(await screen.findByRole('button', { name: 'red heart' }));
    expect(el.value).toBe('a❤️ef');
  });

  it('searches by name and shortcode, and says when nothing matches', async () => {
    render(<Harness />);
    fireEvent.click(screen.getByRole('button', { name: 'Add emoji' }));
    const search = screen.getByRole('searchbox');
    fireEvent.change(search, { target: { value: 'thumbs_up' } });
    const results = await screen.findByRole('group', { name: 'Search results' });
    expect(within(results).getByRole('button', { name: 'thumbs up' })).not.toBeNull();
    fireEvent.change(search, { target: { value: 'zzzzqq' } });
    expect(await screen.findByText('No emoji match.')).not.toBeNull();
    expect(screen.queryByRole('group', { name: 'Search results' })).toBeNull();
  });

  it('searches by alias and hides skin tones (issue #1036)', async () => {
    render(<Harness />);
    fireEvent.click(screen.getByRole('button', { name: 'Add emoji' }));
    const search = screen.getByRole('searchbox');
    fireEvent.change(search, { target: { value: 'thumbsup' } });
    const results = await screen.findByRole('group', { name: 'Search results' });
    expect(within(results).getAllByRole('button')[0]?.textContent).toBe('👍');
    fireEvent.change(search, { target: { value: 'thumb' } });
    const again = await screen.findByRole('group', { name: 'Search results' });
    expect(within(again).queryByRole('button', { name: /skin tone/ })).toBeNull();
  });

  it('moves with the arrow keys: one button in the tab order, rows of eight, Up from the first row to the search box', async () => {
    render(<Harness />);
    fireEvent.click(screen.getByRole('button', { name: 'Add emoji' }));
    const search = screen.getByRole('searchbox');
    const first = await screen.findByRole('button', { name: 'grinning face' });
    const cells = within(screen.getByRole('group')).getAllByRole('button');
    expect(cells.filter((c) => c.tabIndex === 0)).toHaveLength(1);
    fireEvent.keyDown(search, { key: 'ArrowDown' });
    focused(first);
    fireEvent.keyDown(first, { key: 'ArrowRight' });
    focused(cells[1]);
    fireEvent.keyDown(cells[1] as HTMLElement, { key: 'ArrowDown' });
    focused(cells[9]);
    expect(cells[9]?.tabIndex).toBe(0);
    fireEvent.keyDown(cells[9] as HTMLElement, { key: 'ArrowLeft' });
    focused(cells[8]);
    fireEvent.keyDown(cells[8] as HTMLElement, { key: 'End' });
    focused(cells[cells.length - 1]);
    fireEvent.keyDown(cells[cells.length - 1] as HTMLElement, { key: 'Home' });
    focused(cells[0]);
    fireEvent.keyDown(cells[3] as HTMLElement, { key: 'ArrowUp' });
    focused(search);
    fireEvent.keyDown(cells[0] as HTMLElement, { key: 'Escape' });
    focused(screen.getByRole('button', { name: 'Add emoji' }));
  });

  it('does not send the draft on Enter in the search box: it picks the first result', async () => {
    let sent = 0;
    render(<Harness initial="draft " onSend={() => { sent++; }} />);
    box().setSelectionRange(6, 6);
    fireEvent.click(screen.getByRole('button', { name: 'Add emoji' }));
    const search = screen.getByRole('searchbox');
    fireEvent.change(search, { target: { value: 'red heart' } });
    await screen.findByRole('button', { name: 'red heart' });
    // Prevented, so the browser does not submit the form around the box.
    expect(fireEvent.keyDown(search, { key: 'Enter' })).toBe(false);
    expect(box().value).toBe('draft ❤️');
    expect(sent).toBe(0);
  });

  it('does nothing on Enter in the search box when nothing matches', async () => {
    render(<Harness initial="draft" />);
    fireEvent.click(screen.getByRole('button', { name: 'Add emoji' }));
    const search = screen.getByRole('searchbox');
    fireEvent.change(search, { target: { value: 'zzzzqq' } });
    await screen.findByText('No emoji match.');
    expect(fireEvent.keyDown(search, { key: 'Enter' })).toBe(false);
    expect(box().value).toBe('draft');
    expect(screen.getByRole('dialog', { name: 'Emoji' })).not.toBeNull();
  });

  it('ignores navigation, dismissal and Enter while an input method is composing', async () => {
    render(<Harness />);
    fireEvent.click(screen.getByRole('button', { name: 'Add emoji' }));
    const search = screen.getByRole('searchbox');
    const first = await screen.findByRole('button', { name: 'grinning face' });
    fireEvent.keyDown(search, { key: 'ArrowDown', isComposing: true });
    expect(document.activeElement).toBe(search);
    expect(fireEvent.keyDown(search, { key: 'Enter', isComposing: true })).toBe(true);
    expect(box().value).toBe('');
    fireEvent.keyDown(search, { key: 'Escape', isComposing: true });
    expect(screen.getByRole('dialog', { name: 'Emoji' })).not.toBeNull();
    fireEvent.keyDown(first, { key: 'ArrowRight', isComposing: true });
    expect(document.activeElement).not.toBe(first.nextElementSibling);
    fireEvent.keyDown(search, { key: 'Escape' });
    expect(screen.queryByRole('dialog')).toBeNull();
  });

  it('puts the same emoji back over a selected one: focus returns and the caret follows it', async () => {
    render(<Harness initial="a😀b" />);
    const el = box();
    el.setSelectionRange(1, 3);
    fireEvent.click(screen.getByRole('button', { name: 'Add emoji' }));
    fireEvent.change(screen.getByRole('searchbox'), { target: { value: 'grinning face' } });
    fireEvent.click(await screen.findByRole('button', { name: 'grinning face', hidden: false }));
    expect(el.value).toBe('a😀b');
    expect(document.activeElement).toBe(el);
    expect([el.selectionStart, el.selectionEnd]).toEqual([3, 3]);
    // The caret is not pulled back by a later edit.
    el.setSelectionRange(0, 0);
    fireEvent.change(el, { target: { value: 'xa😀b', selectionStart: 1, selectionEnd: 1 } });
    expect(el.selectionStart).toBe(1);
  });

  it('chooses the focused button', async () => {
    render(<Harness />);
    fireEvent.click(screen.getByRole('button', { name: 'Add emoji' }));
    fireEvent.click(await screen.findByRole('button', { name: 'grinning face' }));
    expect(box().value).toBe('😀');
  });

  it('is off while the composer is read-only', () => {
    render(<Harness enabled={false} />);
    expect(screen.getByRole<HTMLButtonElement>('button', { name: 'Add emoji' }).disabled).toBe(true);
  });
});

// A character typed into the box, as the browser reports it.
function typeChar(before: string, ch: string, inputType = 'insertText'): void {
  const el = box();
  el.focus();
  const value = before + ch;
  fireEvent.input(el, { target: { value, selectionStart: value.length, selectionEnd: value.length }, inputType, data: ch });
  el.setSelectionRange(value.length, value.length);
  fireEvent.select(el);
}

describe('the closing colon of :name: (issue #1081)', () => {
  it.each([
    ['x :tada', 'x 🎉'],
    ['x :+1', 'x 👍'],
    ['x :thumbsup', 'x 👍'],
    ['x :heart', 'x ❤️'],
  ])('swaps %j for the emoji and puts the caret after it', (before, want) => {
    render(<Harness />);
    typeChar(before, ':');
    expect(box().value).toBe(want);
    expect(box().selectionStart).toBe(want.length);
    expect(screen.queryByRole('listbox')).toBeNull();
    focused(box());
  });

  it('sends the emoji on Enter', () => {
    let sent = 0;
    render(<Harness onSend={() => { sent++; }} />);
    typeChar('x :tada', ':');
    fireEvent.keyDown(box(), { key: 'Enter' });
    expect(box().value).toBe('x 🎉');
    expect(sent).toBe(1);
  });

  it.each([
    ['x :notaname', 'x :notaname:'],
    ['12:30', '12:30:'],
    ['http://a:b', 'http://a:b:'],
    ['https://x.y/a:b', 'https://x.y/a:b:'],
    ['a:tada', 'a:tada:'],
    ['x :t', 'x :t:'],
  ])('leaves %j as typed', (before, want) => {
    render(<Harness />);
    typeChar(before, ':');
    expect(box().value).toBe(want);
  });

  it('gives back :tada: on one Backspace right after the swap', () => {
    render(<Harness />);
    typeChar('x :tada', ':');
    expect(box().value).toBe('x 🎉');
    expect(fireEvent.keyDown(box(), { key: 'Backspace' })).toBe(false);
    expect(box().value).toBe('x :tada:');
    expect(box().selectionStart).toBe('x :tada:'.length);
    // It does not swap again, and a second Backspace is the browser's own.
    expect(fireEvent.keyDown(box(), { key: 'Backspace' })).toBe(true);
  });

  it.each([
    ['isComposing', { isComposing: true }],
    ['inside a composition', null],
  ])('does not swap during an input method composition (%s)', (_name, init) => {
    render(<Harness />);
    if (init === null) fireEvent.compositionStart(box());
    const value = 'x :tada:';
    fireEvent.input(box(), { target: { value, selectionStart: value.length, selectionEnd: value.length }, inputType: 'insertText', data: ':', ...(init ?? {}) });
    expect(box().value).toBe(value);
    if (init === null) fireEvent.compositionEnd(box());
    // After the composition ends, typing swaps again.
    typeChar('y :tada', ':');
    expect(box().value).toBe('y 🎉');
  });

  it.each(['(', '[', '{'])('does not swap inside a link after %s', (open) => {
    render(<Harness />);
    typeChar(`https://example.com/?q=${open}:heart`, ':');
    expect(box().value).toBe(`https://example.com/?q=${open}:heart:`);
  });

  it('gives no Backspace undo once the swapped emoji was replaced by the picker with itself', async () => {
    render(<Harness />);
    typeChar('x :tada', ':');
    expect(box().value).toBe('x 🎉');
    fireEvent.click(screen.getByRole('button', { name: 'Add emoji' }));
    const dialog = await screen.findByRole('dialog', { name: 'Emoji' });
    box().setSelectionRange(2, 'x 🎉'.length);
    fireEvent.change(within(dialog).getByRole('searchbox'), { target: { value: 'party popper' } });
    fireEvent.click(await within(dialog).findByRole('button', { name: 'party popper' }));
    await waitFor(() => { expect(box().value).toBe('x 🎉'); });
    expect(fireEvent.keyDown(box(), { key: 'Backspace' })).toBe(true);
    expect(box().value).toBe('x 🎉');
  });

  it('keeps Backspace as the browser has it once something else was typed', () => {
    render(<Harness />);
    typeChar('x :tada', ':');
    typeChar('x 🎉', 'a');
    expect(fireEvent.keyDown(box(), { key: 'Backspace' })).toBe(true);
    expect(box().value).toBe('x 🎉a');
  });

  it('does not swap pasted text, a change with no typing, or a restored draft', () => {
    render(<Harness initial="x :tada:" />);
    expect(box().value).toBe('x :tada:');
    typeChar('x :tada', ':', 'insertFromPaste');
    expect(box().value).toBe('x :tada:');
    type('y :tada:');
    expect(box().value).toBe('y :tada:');
  });

  it('keeps the popups: :short still suggests, and a swap leaves no list open', async () => {
    render(<Harness />);
    typeChar('x :tad', 'a');
    expect(await screen.findByRole('listbox', { name: 'Emoji suggestions' })).not.toBeNull();
    typeChar('x :tada', ':');
    expect(box().value).toBe('x 🎉');
    expect(screen.queryByRole('listbox')).toBeNull();
  });
});

describe('shortcode completion', () => {
  it('offers matches for :short at the caret and inserts the first on Enter', async () => {
    render(<Harness />);
    type('say :thumbs_u');
    const list = await screen.findByRole('listbox', { name: 'Emoji suggestions' });
    const options = within(list).getAllByRole('option');
    expect(options[0]?.getAttribute('aria-selected')).toBe('true');
    expect(options[0]?.textContent).toContain(':thumbs_up:');
    expect(box().getAttribute('aria-activedescendant')).toBe(options[0]?.id);
    fireEvent.keyDown(box(), { key: 'Enter' });
    expect(box().value).toBe('say 👍');
    expect(screen.queryByRole('listbox')).toBeNull();
    focused(box());
  });

  it('completes an alias to the emoji itself (issue #1036)', async () => {
    render(<Harness />);
    type('yay :tada');
    const list = await screen.findByRole('listbox', { name: 'Emoji suggestions' });
    expect(within(list).getAllByRole('option')[0]?.textContent).toContain('🎉');
    fireEvent.keyDown(box(), { key: 'Enter' });
    expect(box().value).toBe('yay 🎉');
  });

  it('keeps the text after the caret and puts the caret after the emoji', async () => {
    render(<Harness />);
    type('a :fire b', 7);
    await screen.findByRole('listbox');
    fireEvent.keyDown(box(), { key: 'Tab' });
    expect(box().value).toBe('a 🔥 b');
    expect(box().selectionStart).toBe('a 🔥'.length);
  });

  it('moves with the arrows, wrapping, and Escape closes it until the text changes', async () => {
    render(<Harness />);
    type('x :cat');
    const list = await screen.findByRole('listbox');
    const n = within(list).getAllByRole('option').length;
    expect(n).toBeGreaterThan(2);
    fireEvent.keyDown(box(), { key: 'ArrowUp' });
    expect(within(list).getAllByRole('option')[n - 1]?.getAttribute('aria-selected')).toBe('true');
    fireEvent.keyDown(box(), { key: 'ArrowDown' });
    expect(within(list).getAllByRole('option')[0]?.getAttribute('aria-selected')).toBe('true');
    fireEvent.keyDown(box(), { key: 'Escape' });
    expect(screen.queryByRole('listbox')).toBeNull();
    type('x :cat_');
    expect(await screen.findByRole('listbox')).not.toBeNull();
  });

  it('inserts the option that is clicked', async () => {
    render(<Harness />);
    type(':smiling_face_with_he');
    fireEvent.click(await screen.findByRole('option', { name: /smiling_face_with_heart_eyes/ }));
    await waitFor(() => { expect(box().value).toBe('😍'); });
  });

  it.each([
    ['Shift+Enter', { key: 'Enter', shiftKey: true }],
    ['Shift+Tab', { key: 'Tab', shiftKey: true }],
    ['Ctrl+Enter', { key: 'Enter', ctrlKey: true }],
    ['Alt+Tab', { key: 'Tab', altKey: true }],
    ['Meta+Enter', { key: 'Enter', metaKey: true }],
  ])('leaves %s alone while suggestions are open', async (_name, init) => {
    render(<Harness />);
    type('go :fire');
    await screen.findByRole('listbox');
    fireEvent.keyDown(box(), init);
    // Not accepted: the text is as typed (Shift+Enter and Shift+Tab are left
    // to the browser; Ctrl/Meta+Enter are the composer's own).
    expect(box().value).toBe('go :fire');
    if ('shiftKey' in init) expect(fireEvent.keyDown(box(), init)).toBe(true);
  });

  it('is not offered for a time, a link, one letter, or when nothing matches', async () => {
    render(<Harness />);
    for (const text of ['see 12:30', 'http://x.test', 'hi :s', 'hi :zzzzqq']) {
      type(text);
      await new Promise((r) => setTimeout(r, 20));
      expect(screen.queryByRole('listbox')).toBeNull();
    }
  });

  it('leaves Enter to the composer when nothing is offered', () => {
    render(<Harness />);
    type('plain');
    // The composer's own handler took it (it prevents default to send).
    expect(fireEvent.keyDown(box(), { key: 'Enter' })).toBe(false);
    expect(box().getAttribute('aria-controls')).toBeNull();
  });
});

// While a send with images is under way the box is read-only, and so is
// everything that would edit the draft the send was made from.
describe('a send in progress', () => {
  const image = (status: DraftAttachment['status']): DraftAttachment => ({ clientId: 1, filename: 'a.png', mediaType: 'image/png', size: 1, bytes: new Blob(['x']), status, attempt: 1 });
  function WithImage({ status }: { readonly status: DraftAttachment['status'] }) {
    const [draft, setDraft] = useState('caption :fire');
    return (
      <ComposerAttachmentProvider value={{ attachments: [image(status)], add: () => undefined, remove: () => undefined, cancel: () => undefined, retry: () => undefined, editing: () => undefined }}>
        <Composer channel="general" target={target} nets={new Map()} monitored={new Set()} people={new Map()} viewer={2} draft={draft} enabled paused={false} onDraft={setDraft} onTarget={() => undefined} onSend={() => undefined} atBottom={() => undefined} />
      </ComposerAttachmentProvider>
    );
  }

  it('offers no suggestions, and takes the open ones away, while the images upload', async () => {
    const { rerender } = render(<WithImage status="ready" />);
    box().focus();
    box().setSelectionRange(13, 13);
    fireEvent.select(box());
    expect(await screen.findByRole('listbox', { name: 'Emoji suggestions' })).not.toBeNull();
    rerender(<WithImage status="uploading" />);
    expect(screen.queryByRole('listbox')).toBeNull();
    expect(box().value).toBe('caption :fire');
    // Enter is not taken for a completion either.
    fireEvent.keyDown(box(), { key: 'Enter' });
    expect(box().value).toBe('caption :fire');
  });

  it('gives no Backspace undo of a swap while the images upload (issue #1081)', () => {
    const { rerender } = render(<WithImage status="ready" />);
    typeChar('x :tada', ':');
    expect(box().value).toBe('x 🎉');
    rerender(<WithImage status="uploading" />);
    expect(fireEvent.keyDown(box(), { key: 'Backspace' })).toBe(true);
    expect(box().value).toBe('x 🎉');
  });

  it('closes an open picker and disables its button while the images upload', async () => {
    const { rerender } = render(<WithImage status="ready" />);
    fireEvent.click(screen.getByRole('button', { name: 'Add emoji' }));
    await screen.findByRole('button', { name: 'grinning face' });
    rerender(<WithImage status="uploading" />);
    expect(screen.queryByRole('dialog')).toBeNull();
    expect(screen.getByRole<HTMLButtonElement>('button', { name: 'Add emoji' }).disabled).toBe(true);
    // Once the send is over the picker is closed, not reopened under the user.
    rerender(<WithImage status="ready" />);
    expect(screen.queryByRole('dialog')).toBeNull();
  });
});
