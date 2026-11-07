import { cleanup, fireEvent, render, screen } from '@testing-library/react';
import { afterEach, describe, expect, it } from 'vitest';
import { Linkified } from './Linkified';
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
    expect(performance.now() - started).toBeLessThan(250);
    expect(container.textContent).toHaveLength(MAX_POST_REQUEST_BYTES);
  });

  it.each(delimiters)('renders densely nested %s delimiters at the message-size cap in bounded time', (delimiter) => {
    const pairs = Math.floor(MAX_POST_REQUEST_BYTES / (delimiter.length * 2));
    const body = maximumBody(delimiter.repeat(pairs) + 'x' + delimiter.repeat(pairs));
    const started = performance.now();
    render(<Linkified text={body} />);
    expect(performance.now() - started).toBeLessThan(250);
  });
});
