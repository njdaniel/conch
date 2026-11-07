import { createEvent, cleanup, fireEvent, render, screen } from '@testing-library/react';
import { useState } from 'react';
import { afterEach, describe, expect, it, vi } from 'vitest';
import { Composer } from './Composer';

const target = { net: null, whisper: null, refusal: null, note: null };

function Harness({ onSend = vi.fn() }: { readonly onSend?: (body: string) => void }) {
  const [draft, setDraft] = useState('');
  return <Composer channel="general" target={target} nets={new Map()} monitored={new Set()} people={new Map()} viewer={2} draft={draft} enabled paused={false} onDraft={setDraft} onTarget={() => undefined} onSend={onSend} atBottom={() => undefined} />;
}

const box = () => screen.getByLabelText<HTMLTextAreaElement>('Message #general');

function select(text: string): void {
  const input = box();
  fireEvent.change(input, { target: { value: text } });
  input.setSelectionRange(0, text.length);
  fireEvent.select(input);
}

function composingKey(key: string, options: { ctrlKey?: boolean; metaKey?: boolean; shiftKey?: boolean } = {}): void {
  const event = createEvent.keyDown(box(), { key, ...options });
  Object.defineProperty(event, 'isComposing', { value: true });
  fireEvent(box(), event);
}

afterEach(cleanup);

describe('composer formatting shortcuts', () => {
  it.each([
    ['bold', 'b', {}, '**bold**'],
    ['italic', 'i', {}, '*italic*'],
    ['inline code', 'e', {}, '`code`'],
    ['strike', 'x', { shiftKey: true }, '~~strike~~'],
  ])('applies %s with Control and keeps the content selected', (_name, key, modifiers, want) => {
    render(<Harness />);
    const word = want.replaceAll(/[*`~]/g, '');
    select(word);
    fireEvent.keyDown(box(), { key, ctrlKey: true, ...modifiers });
    expect(box().value).toBe(want);
    expect(box().selectionStart).toBe(want.indexOf(word));
    expect(box().selectionEnd).toBe(want.indexOf(word) + word.length);
  });

  it('supports Command on macOS and inserts empty formatting at the caret', () => {
    render(<Harness />);
    select('bold');
    fireEvent.keyDown(box(), { key: 'b', metaKey: true });
    expect(box().value).toBe('**bold**');
    fireEvent.keyDown(box(), { key: 'i', metaKey: true });
    expect(box().value).toBe('***bold***');
  });

  it('does not fire for either native or tracked IME composition', () => {
    render(<Harness />);
    select('かな');
    composingKey('b', { ctrlKey: true });
    expect(box().value).toBe('かな');

    fireEvent.compositionStart(box());
    fireEvent.keyDown(box(), { key: 'i', ctrlKey: true });
    expect(box().value).toBe('かな');
    fireEvent.compositionEnd(box());
    fireEvent.keyDown(box(), { key: 'i', ctrlKey: true });
    expect(box().value).toBe('*かな*');
  });

  it('preserves Enter, Shift+Enter and slash-command sending', () => {
    const onSend = vi.fn();
    render(<Harness onSend={onSend} />);
    select('/shrug **hello**');
    fireEvent.keyDown(box(), { key: 'Enter', shiftKey: true });
    expect(onSend).not.toHaveBeenCalled();
    fireEvent.keyDown(box(), { key: 'Enter' });
    expect(onSend).toHaveBeenCalledWith('/shrug **hello**', undefined, []);
  });
});

describe('composer formatting controls', () => {
  it('offers visible labelled controls for every supported block and inline style', () => {
    render(<Harness />);
    const toolbar = screen.getByRole('toolbar', { name: 'Formatting' });
    for (const name of ['Bold', 'Italic', 'Strikethrough', 'Inline code', 'Code block', 'Quote', 'Spoiler']) {
      expect(toolbar.querySelector(`button[aria-label^="${name}"]`)).not.toBeNull();
    }

    select('secret');
    fireEvent.click(screen.getByRole('button', { name: 'Spoiler' }));
    expect(box().value).toBe('||secret||');
  });
});
