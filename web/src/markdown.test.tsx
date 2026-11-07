import { cleanup, fireEvent, render, screen } from '@testing-library/react';
import { afterEach, describe, expect, it } from 'vitest';
import { BodyBoundary, Linkified } from './Linkified';
import { previewText } from './linkify';
import { formatText } from './formatting';
import { excerptOf } from './Conversation';
import { MAX_POST_REQUEST_BYTES } from './schema/message';

afterEach(cleanup);

describe('message Markdown', () => {
  it('renders inline formatting and a code block without rendering source HTML', () => {
    const { container } = render(<Linkified text={'**bold** *italic* ~~strike~~ `code <b>`\n\n> quoted\n\n```\nconst tag = "<img>";\n```'} />);
    expect(screen.getByText('bold').tagName).toBe('STRONG');
    expect(screen.getByText('italic').tagName).toBe('EM');
    expect(screen.getByText('strike').tagName).toBe('S');
    expect(screen.getByText('code <b>').tagName).toBe('CODE');
    expect(screen.getByText('quoted').closest('blockquote')).not.toBeNull();
    expect(screen.getByText('const tag = "<img>";').closest('pre')).not.toBeNull();
    expect(container.querySelector('img')).toBeNull();
    expect(container.querySelector('b')).toBeNull();
  });

  it('renders masked links literally and autolinks only the written HTTP(S) addresses', () => {
    const body = '[https://bank.example](https://evil.example) https://example.com/b';
    const { container } = render(<Linkified text={body} />);
    const links = screen.getAllByRole('link');
    expect(container.textContent).toBe(body);
    expect(links.map((link) => link.getAttribute('href'))).toEqual(['https://bank.example/', 'https://evil.example/', 'https://example.com/b']);
    for (const link of links) {
      expect(link.getAttribute('target')).toBe('_blank');
      expect(link.getAttribute('rel')).toBe('noopener noreferrer');
    }
  });

  it('keeps hostile HTML, CSS and unsafe schemes as inert text', () => {
    const { container } = render(<Linkified text={'<img src=x onerror="alert(1)"> <style>*{display:none}</style> [x](data:text/html,<script>alert(1)</script>)'} />);
    expect(container.querySelector('img, style, script, link, iframe')).toBeNull();
    expect(screen.queryByRole('link')).toBeNull();
    expect(container.textContent).toContain('<img src=x onerror="alert(1)">');
    expect(container.textContent).toContain('data:text/html');
  });
});

describe('spoilers', () => {
  it('keeps text out of the accessibility tree until revealed, then hides it again', () => {
    const { container } = render(<Linkified text="before ||secret **bold**|| after" />);
    const reveal = screen.getByRole('button', { name: 'Reveal spoiler' });
    expect(reveal.getAttribute('aria-expanded')).toBe('false');
    expect(container.textContent).not.toContain('secret');
    expect(screen.queryByText('secret bold')).toBeNull();

    reveal.focus();
    expect(document.activeElement).toBe(reveal);
    fireEvent.click(reveal);
    const hide = screen.getByRole('button', { name: 'Hide spoiler' });
    expect(hide.getAttribute('aria-expanded')).toBe('true');
    expect(screen.getByText('secret')).toBeDefined();
    expect(screen.getByText('bold').tagName).toBe('STRONG');

    fireEvent.click(hide);
    expect(screen.getByRole('button', { name: 'Reveal spoiler' }).getAttribute('aria-expanded')).toBe('false');
    expect(container.textContent).not.toContain('secret');
  });

  it('leaves unmatched spoiler markers readable', () => {
    const { container } = render(<Linkified text="not || hidden" />);
    expect(container.textContent).toBe('not || hidden');
    expect(screen.queryByRole('button')).toBeNull();
  });
});

describe('message Markdown pathological input', () => {
  const delimiters = ['[', '`', '||', '*', '**', '~~', '***'] as const;
  const maximumBody = (unit: string): string => {
    const body = unit.repeat(Math.ceil(MAX_POST_REQUEST_BYTES / unit.length));
    return body.slice(0, MAX_POST_REQUEST_BYTES);
  };

  it.each(delimiters)('renders unclosed %s delimiters at the message-size cap in bounded time', (delimiter) => {
    const body = maximumBody(`${delimiter}a\n`);
    const started = performance.now();
    const { container } = render(<Linkified text={body} />);
    expect(performance.now() - started).toBeLessThan(1000);
    // Spoiler text is hidden, so only the other delimiters keep every character.
    if (delimiter !== '||') expect(container.textContent).toHaveLength(MAX_POST_REQUEST_BYTES);
  });

  it('renders 160 000 spoilers in bounded time, with all but 500 left as hidden text', () => {
    const body = '||z||'.repeat(160_000);
    const started = performance.now();
    const { container } = render(<Linkified text={body} />);
    expect(performance.now() - started).toBeLessThan(5000);
    expect(container.querySelectorAll('.spoiler')).toHaveLength(500);
    expect(container.textContent).not.toContain('z');
  }, 30_000);

  // A body of tens of thousands of links once overflowed the call stack (a
  // spread of every segment into one push) and blanked the whole page.
  it('renders a body of 80 000 links without overflowing the stack', () => {
    const body = 'https://a.io '.repeat(80_000);
    expect(body.length).toBeLessThanOrEqual(MAX_POST_REQUEST_BYTES);
    const { container } = render(<Linkified text={body} />);
    expect(container.querySelectorAll('a')).toHaveLength(80_000);
  }, 60_000);

  it.each(delimiters)('renders densely nested %s delimiters at the message-size cap in bounded time', (delimiter) => {
    const pairs = Math.floor(MAX_POST_REQUEST_BYTES / (delimiter.length * 2));
    const body = maximumBody(delimiter.repeat(pairs) + 'x' + delimiter.repeat(pairs));
    const started = performance.now();
    render(<Linkified text={body} />);
    expect(performance.now() - started).toBeLessThan(1000);
  });
});

describe('a body that fails to render', () => {
  it('shows that body as plain text and leaves the rest of the view', () => {
    function Throws(): never {
      throw new Error('render failed');
    }
    const quiet = console.error;
    console.error = () => undefined;
    try {
      render(
        <div>
          <p>before</p>
          <BodyBoundary text="the body **as typed**"><Throws /></BodyBoundary>
          <p>after</p>
        </div>,
      );
    } finally {
      console.error = quiet;
    }
    expect(screen.getByText('before')).toBeTruthy();
    expect(screen.getByText('the body **as typed**')).toBeTruthy();
    expect(screen.getByText('after')).toBeTruthy();
  });
});

describe('multiline spoilers', () => {
  it('hides a spoiler that spans lines until it is revealed', () => {
    const { container } = render(<Linkified text={'||line one\nline two||'} />);
    expect(container.textContent).not.toContain('line');
    fireEvent.click(screen.getByRole('button', { name: 'Reveal spoiler' }));
    expect(container.textContent).toContain('line one\nline two');
  });

  it('hides every line of a toolbar-made spoiler over a multiline selection, quotes and code included', () => {
    const selection = 'first\n> quoted\n```\ncoded\n```\nlast';
    const made = formatText(selection, 0, selection.length, 'spoiler').text;
    const { container } = render(<Linkified text={made} />);
    for (const word of ['first', 'quoted', 'coded', 'last']) expect(container.textContent).not.toContain(word);
  });

  it.each([
    ['use a || b\nnext line', ['use a || b', 'next line']],
    ['||secret\nmore', ['||secret', 'more']],
    ['> ||quoted\n> more', ['||quoted', 'more']],
  ])('leaves an unclosed spoiler literal, line breaks or not: %j', (body, shown) => {
    const { container } = render(<Linkified text={body} />);
    for (const part of shown) expect(container.textContent).toContain(part);
    expect(screen.queryByRole('button')).toBeNull();
  });

  it('hides a spoiler nested deeper than the renderer parses', () => {
    const body = `${'||'.repeat(30)}secret\n${'||'.repeat(30)}`;
    const { container } = render(<Linkified text={body} />);
    expect(container.textContent).not.toContain('secret');
  });

  it('starts a changed body hidden again', () => {
    const { container, rerender } = render(<Linkified text="||old||" />);
    fireEvent.click(screen.getByRole('button', { name: 'Reveal spoiler' }));
    expect(container.textContent).toContain('old');
    rerender(<Linkified text="||new secret||" />);
    expect(container.textContent).not.toContain('new secret');
    expect(screen.getByRole('button', { name: 'Reveal spoiler' }).getAttribute('aria-expanded')).toBe('false');
  });

  it('keeps spoilers out of a reply excerpt', () => {
    expect(excerptOf('||the answer|| is here')).toBe('[spoiler] is here');
    expect(excerptOf('||the\nanswer||')).toBe('[spoiler]');
  });
});

describe('code block from a partial-line selection', () => {
  it('breaks the line around both fences and renders as a code block', () => {
    const text = 'hello world';
    const made = formatText(text, 6, 11, 'codeBlock');
    expect(made.text).toBe('hello \n```\nworld\n```');
    expect(made.text.slice(made.start, made.end)).toBe('world');
    const { container } = render(<Linkified text={made.text} />);
    expect(container.querySelector('pre code')?.textContent).toBe('world');
    expect(container.textContent).toContain('hello');
  });

  it('breaks after the selection when text follows it', () => {
    const made = formatText('ab cd ef', 3, 5, 'codeBlock');
    expect(made.text).toBe('ab \n```\ncd\n```\n ef');
    expect(made.text.slice(made.start, made.end)).toBe('cd');
  });

  it('adds no extra line breaks for a whole-line selection', () => {
    const made = formatText('a\nb\nc', 2, 3, 'codeBlock');
    expect(made.text).toBe('a\n```\nb\n```\nc');
  });
});

describe('previews outside the message view', () => {
  it.each([
    ['@nick ||the answer is X||', '@nick [spoiler]'],
    ['a ||one|| and ||two|| end', 'a [spoiler] and [spoiler] end'],
    ['unclosed ||spoiler stays', 'unclosed ||spoiler stays'],
    ['line one ||x\ny|| line two', 'line one [spoiler] line two'],
    ['line one ||x\ny', 'line one ||x\ny'],
    ['use a || b\nnext line', 'use a || b\nnext line'],
    ['plain text', 'plain text'],
  ])('%j previews as %j', (body, preview) => {
    expect(previewText(body)).toBe(preview);
  });
});
