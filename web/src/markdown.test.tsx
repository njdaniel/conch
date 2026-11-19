import { act, cleanup, fireEvent, render, renderHook, screen, waitFor, within } from '@testing-library/react';
import { afterEach, describe, expect, it, vi } from 'vitest';
import { BodyBoundary, Linkified, previewText } from './Linkified';
import { useNotifications } from './useNotifications';
import { platform } from './platform';
import { SearchPanel } from './SearchPanel';
import { NotificationCenter } from './NotificationCenter';
import { messagePreview } from './spoilerFiles';
import * as api from './api';
import type { MessageV2 } from './schema/message';
import { formatText } from './formatting';
import { Conversation, excerptOf } from './Conversation';
import { MAX_POST_REQUEST_BYTES } from './schema/message';

afterEach(() => { cleanup(); vi.restoreAllMocks(); });

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

  it.each([
    { name: 'percent-encoding', text: 'https://example.com/a%2fb?x=hello%20world' },
    { name: 'mixed case', text: 'HTTPS://Example.COM/a' },
    { name: 'trailing host dot', text: 'https://Example.COM./a' },
    { name: 'very long URL', text: `https://example.com/${'a'.repeat(300)}` },
  ])('marks $name as external and titles it with its canonical address', ({ text }) => {
    render(<Linkified text={text} />);
    const canonical = new URL(text).href;
    const link = screen.getByRole('link', { name: `${text} (opens in a new tab)` });
    expect(link.getAttribute('href')).toBe(canonical);
    expect(link.getAttribute('title')).toBe(canonical);
    expect(link.textContent).toBe(text);
    expect(link.textContent).not.toContain('(opens in a new tab)');
    const cue = link.querySelector('.external-link-cue');
    expect(cue?.getAttribute('aria-hidden')).toBe('true');
    expect(cue?.querySelector('.icon')).not.toBeNull();
  });

  it('does not turn javascript into a link', () => {
    const body = 'javascript:alert(1)';
    const { container } = render(<Linkified text={body} />);
    expect(screen.queryByRole('link')).toBeNull();
    expect(container.textContent).toBe(body);
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

  it.each([
    { name: '100,000 headings', units: ['# x\n'], headings: 500, items: 0, code: 0 },
    { name: '100,000 fenced blocks', units: ['```\nx\n```\n'], headings: 0, items: 0, code: 500 },
    { name: '100,000 mixed blocks', units: ['# x\n', '- x\n', '```\nx\n```\n'], headings: 167, items: 167, code: 166 },
  ])('shares a structural budget for $name and retains every overflow character', ({ units, headings, items, code }) => {
    const blocks = Array.from({ length: 100_000 }, (_, i) => units[i % units.length] ?? '');
    const body = blocks.join('');
    expect(body.length).toBeLessThanOrEqual(MAX_POST_REQUEST_BYTES);
    const { container } = render(<Linkified text={body} />);
    expect(container.querySelectorAll('.markdown-heading')).toHaveLength(headings);
    expect(container.querySelectorAll('li')).toHaveLength(items);
    expect(container.querySelectorAll('.markdown-code-block')).toHaveLength(code);
    expect(container.querySelectorAll('button')).toHaveLength(code);
    expect(container.querySelectorAll('*').length).toBeLessThan(4000);
    // A single source-text span keeps all 99,500 remaining blocks readable.
    const overflow = blocks.slice(500).join('');
    expect(container.lastChild?.textContent).toBe(overflow);
    expect(previewText(body).endsWith(overflow)).toBe(true);
  });

  it('keeps overflow linkified and spoiler-safe without creating more Markdown elements', () => {
    const tail = '# **heading**\n\\*escaped*\n- *item*\n> quote\n```\n`||secret||`\n```\nhttps://example.com\n' + '||hidden||'.repeat(1000);
    const { container } = render(<Linkified text={'# x\n'.repeat(500) + tail} />);
    expect(container.querySelectorAll('.markdown-heading')).toHaveLength(500);
    expect(container.querySelector('li, blockquote, pre, code, strong, em')).toBeNull();
    expect(container.lastChild?.textContent).toContain('# **heading**\n\\*escaped*\n- *item*\n> quote\n```');
    expect(screen.getByRole('link').getAttribute('href')).toBe('https://example.com/');
    expect(container.querySelectorAll('.spoiler')).toHaveLength(500);
    expect(container.textContent).not.toContain('secret');
    expect(container.textContent).not.toContain('hidden');
    expect(previewText('# x\n'.repeat(500) + tail)).not.toMatch(/secret|hidden/);
  });

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

describe('previewText against the renderer', () => {
  const pieces = ['||', '|', '`', '```', '\n', '> ', '\\', '**', '*', '~~', ' ', 'x', '[', 'https://a.co/'];
  // A small deterministic generator, so a failure names its body.
  function bodies(count: number): string[] {
    let seed = 1496;
    const roll = (n: number): number => {
      seed = (seed * 1103515245 + 12345) % 2147483648;
      return seed % n;
    };
    return Array.from({ length: count }, () => {
      const parts = Array.from({ length: 2 + roll(14) }, () => pieces[roll(pieces.length)] ?? '');
      return parts.join('');
    });
  }

  it('shows no word the message view hides inside a spoiler', () => {
    // Words inside a spoiler are unique tokens; none may appear in the preview.
    for (const body of bodies(1500)) {
      let n = 0;
      const withSecrets = body.replace(/x/g, () => `s${String(n++)}z`);
      const { container, unmount } = render(<Linkified text={withSecrets} />);
      const shownWords = new Set((container.textContent).match(/s\d+z/g) ?? []);
      unmount();
      const previewWords = new Set(previewText(withSecrets).match(/s\d+z/g) ?? []);
      for (const word of previewWords) {
        expect(shownWords.has(word), `${JSON.stringify(body)}: ${word}`).toBe(true);
      }
    }
  }, 60_000);
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
    ['`||` x ||secret||', '`||` x [spoiler]'],
    ['\\|| text ||secret||', '\\|| text [spoiler]'],
    ['```\n||\n```\n||secret||', '```\n||\n```\n[spoiler]'],
    ['**a||b** c||secret||', '**a||b** c[spoiler]'],
    ['plain text', 'plain text'],
  ])('%j previews as %j', (body, preview) => {
    expect(previewText(body)).toBe(preview);
  });
});


describe('lists, headings and code controls', () => {
  const golden = [
    { name: 'unordered markers', body: '- one\n* two', text: 'onetwo', lists: 1, items: 2, headings: 0, preview: 'one\ntwo' },
    { name: 'ordered markers', body: '1. one\n1. two', text: 'onetwo', lists: 1, items: 2, headings: 0, preview: 'one\ntwo' },
    { name: 'mixed nested lists', body: '- one\n  1. two\n    * three\n      - four\n- five', text: 'onetwothreefourfive', lists: 3, items: 5, headings: 0, preview: 'one\ntwo\nthree\nfour\nfive' },
    { name: 'indentation jumps', body: '- one\n        - two\n- three', text: 'onetwothree', lists: 2, items: 3, headings: 0, preview: 'one\ntwo\nthree' },
    { name: 'two heading styles', body: '# One\n## Two\n### literal', text: 'OneTwo### literal', lists: 0, items: 0, headings: 2, preview: 'One\nTwo\n### literal' },
    { name: 'inline code', body: '`# literal`\n`- literal`', text: '# literal\n- literal', lists: 0, items: 0, headings: 0, preview: '`# literal`\n`- literal`' },
    { name: 'fenced code', body: '```ts\n# literal\n- literal\n1. literal\n```', text: 'Copy# literal\n- literal\n1. literal', lists: 0, items: 0, headings: 0, preview: '```ts\n# literal\n- literal\n1. literal\n```' },
    { name: 'links and spoilers', body: '# ||secret||\n- https://example.com ||hidden||', text: 'Reveal spoilerhttps://example.com Reveal spoiler', lists: 1, items: 1, headings: 1, preview: '[spoiler]\nhttps://example.com [spoiler]' },
    { name: 'escaped markers', body: '\\# literal\n\\* literal', text: '\\# literal\n* literal', lists: 0, items: 0, headings: 0, preview: '\\# literal\n\\* literal' },
  ];
  it.each(golden)('$name', ({ body, text, lists, items, headings, preview }) => {
    const { container } = render(<Linkified text={body} />);
    expect(container.textContent).toBe(text);
    expect(screen.queryAllByRole('list')).toHaveLength(lists);
    expect(screen.queryAllByRole('listitem')).toHaveLength(items);
    expect(container.querySelectorAll('.markdown-heading')).toHaveLength(headings);
    expect(container.querySelector('h1,h2,h3,h4,h5,h6,[role="heading"]')).toBeNull();
    expect(container.querySelector('script,style,[style]')).toBeNull();
    expect(previewText(body)).toBe(preview);
    for (const list of screen.queryAllByRole('list')) {
      expect(['UL', 'OL']).toContain(list.tagName);
      expect(list.querySelector(':scope > li')).not.toBeNull();
    }
    for (const link of screen.queryAllByRole('link')) expect(link.getAttribute('rel')).toBe('noopener noreferrer');
  });

  it.each(['- ', '1. '])('bounds a 4,000-item %s list and keeps excess source readable', (marker) => {
    const body = Array.from({ length: 4000 }, (_, i) => `${'  '.repeat(i % 5)}${marker}item`).join('\n');
    const start = performance.now();
    const { container } = render(<Linkified text={body} />);
    expect(performance.now() - start).toBeLessThan(1000);
    expect(container.querySelectorAll('li')).toHaveLength(500);
    expect(container.querySelector('.markdown-list .markdown-list .markdown-list .markdown-list')).toBeNull();
    expect(container.textContent).toContain(`${marker}item`);
    expect(previewText(body).split('\n').slice(0, 500)).toEqual(Array<string>(500).fill('item'));
    expect(previewText(body).split('\n')[500]).toBe(`${marker}item`);
  });

  it('bounds 4,000 alternating inline markers inside a list item', () => {
    const body = `- ${'**~~*'.repeat(4000)}end`;
    const start = performance.now();
    render(<Linkified text={body} />);
    expect(performance.now() - start).toBeLessThan(1000);
    expect(screen.getAllByRole('listitem')).toHaveLength(1);
  });

  it('copies exact code through the platform service and announces success without moving focus', async () => {
    const text = '\t# literal  \n- item\n\n';
    const copy = vi.spyOn(platform.services, 'copyText').mockResolvedValue();
    render(<Linkified text={`\`\`\`ts\n${text}\n\`\`\``} />);
    const button = screen.getByRole('button', { name: 'Copy code' });
    expect(button.tagName).toBe('BUTTON');
    expect(button.getAttribute('type')).toBe('button');
    button.focus();
    // Native keyboard activation emits click with detail 0 (jsdom does not
    // synthesize a browser's default Enter/Space action from keydown).
    fireEvent.click(button, { detail: 0 });
    await waitFor(() => { expect(screen.getByRole('status').textContent).toBe('Copied'); });
    expect(copy).toHaveBeenCalledExactlyOnceWith(text);
    expect(document.activeElement).toBe(button);
  });

  it('announces clipboard failure and allows retry', async () => {
    vi.spyOn(platform.services, 'copyText').mockRejectedValueOnce(new Error('denied')).mockResolvedValueOnce();
    render(<Linkified text={'```\ncode\n```'} />);
    const button = screen.getByRole('button', { name: 'Copy code' });
    fireEvent.click(button);
    await waitFor(() => { expect(screen.getByRole('status').textContent).toBe('Could not copy. Try again.'); });
    fireEvent.click(button);
    await waitFor(() => { expect(screen.getByRole('status').textContent).toBe('Copied'); });
  });

  it('keeps Copy in only the focused message Tab order (F15)', async () => {
    HTMLElement.prototype.scrollIntoView = vi.fn();
    vi.spyOn(api, 'readReactions').mockResolvedValue({ ok: true, value: { summaries: [] } });
    const messages: MessageV2[] = [1, 2].map((id) => ({ schema: 'conch.message.v2', id, channel_id: 7, author_id: 3, created_at: '2026-11-18T00:00:00Z', body: `\`\`\`\nblock ${String(id)}\n\`\`\`` }));
    render(<Conversation channel="general" live="open" people={new Map()} nets={new Map()} viewer={2} draft=""
      chat={{ byId: new Map(messages.map((message) => [message.id, message])), pending: [], sentIds: new Set(), history: 'ready', older: 'idle', exhausted: true, trimmed: false, dropped: 0, nextLocalId: 9, edits: new Map(), final: new Set() }}
      target={{ net: null, whisper: null, refusal: null, note: null }} monitored={new Set()}
      onDraft={() => undefined} onTarget={() => undefined} onSend={() => undefined} onReplySend={() => undefined}
      onRetrySend={() => undefined} onDismissSend={() => undefined} onLoadOlder={() => undefined}
      onReload={() => undefined} onBottom={() => undefined} onEdited={() => undefined} />);
    const [first, last] = screen.getAllByRole('button', { name: 'Copy code' });
    expect(first?.tabIndex).toBe(-1);
    expect(last?.tabIndex).toBe(0);
    const row = first?.closest<HTMLElement>('[data-message-id]');
    if (row === null || row === undefined) throw new Error('missing message row');
    act(() => { row.focus(); });
    await waitFor(() => { expect(first?.tabIndex).toBe(0); });
    expect(last?.tabIndex).toBe(-1);
  });

  const previewCases = [
    { body: '# Title', preview: 'Title', rendered: 'Title' },
    { body: '## Title', preview: 'Title', rendered: 'Title' },
    { body: '- item', preview: 'item', rendered: 'item' },
    { body: '* item', preview: 'item', rendered: 'item' },
    { body: '  1. item', preview: 'item', rendered: 'item' },
    { body: '# # Title', preview: '# Title', rendered: '# Title' },
    { body: '- - item', preview: '- item', rendered: '- item' },
    { body: '### literal', preview: '### literal', rendered: '### literal' },
    { body: '2. literal', preview: '2. literal', rendered: '2. literal' },
    { body: '`# Title`', preview: '`# Title`', rendered: '# Title' },
    { body: '`- item`', preview: '`- item`', rendered: '- item' },
    { body: '```\n# Title\n- item\n```', preview: '```\n# Title\n- item\n```', rendered: '# Title\n- item' },
    { body: '# Topic\n- visible ||secret||\n  1. child', preview: 'Topic\nvisible [spoiler]\nchild', rendered: 'Topic' },
  ];

  it.each(previewCases)('projects $body through the shared preview helper', ({ body, preview }) => {
    expect(previewText(body)).toBe(preview);
  });

  it.each(previewCases)('quotes $body in reply excerpts', ({ body, preview }) => {
    expect(excerptOf(body)).toBe(preview.split('\n')[0]);
  });

  it.each(previewCases)('renders $body in search results', async ({ body, rendered }) => {
    const message: MessageV2 = { schema: 'conch.message.v2', id: 1836, channel_id: 7, author_id: 3, created_at: '2026-11-18T00:00:00Z', body };
    vi.spyOn(api, 'searchMessages').mockResolvedValue({ ok: true, value: { messages: [message] } });
    render(<SearchPanel channelId={7} channel="general" live="open" people={new Map()} nets={new Map()} viewer={2} onClose={() => undefined} onMissing={() => undefined} />);
    fireEvent.change(screen.getByRole('searchbox'), { target: { value: 'item' } });
    fireEvent.click(screen.getByRole('button', { name: 'Search' }));
    const results = await screen.findByRole('list', { name: 'Search results' });
    expect(within(results).getByText(rendered, { normalizer: (text) => text }).textContent).toBe(rendered);
    expect(results.textContent).not.toContain('secret');
  });

  it.each(previewCases)('previews $body in the notification center', ({ body, preview }) => {
    const message: MessageV2 = { schema: 'conch.message.v2', id: 1836, channel_id: 7, author_id: 3, created_at: '2026-11-18T00:00:00Z', body };
    expect(messagePreview(message)).toBe(preview);
    render(<NotificationCenter alerts={[{ id: 'markdown', channel: 'general', message, mention: false }]} onOpen={() => undefined} onClear={() => undefined} />);
    fireEvent.click(screen.getByRole('button', { name: 'Notifications (1)' }));
    const notification = screen.getByRole('button', { name: /New message in #general/ });
    expect(notification.textContent).toContain(preview);
    expect(notification.getAttribute('aria-label')).not.toContain('secret');
  });

  it.each(previewCases)('previews $body in native notification snippets', async ({ body, preview }) => {
    const visibility = Object.getOwnPropertyDescriptor(document, 'visibilityState');
    Object.defineProperty(document, 'visibilityState', { configurable: true, value: 'hidden' });
    vi.spyOn(platform.services.notifications, 'supported').mockReturnValue(true);
    vi.spyOn(platform.services.notifications, 'permission').mockReturnValue('granted');
    const show = vi.spyOn(platform.services.notifications, 'show').mockReturnValue({ close: () => undefined, onclick: null });
    vi.spyOn(api, 'readNotificationPreferences').mockReturnValue({ policy: 'all', muted: [], native: true });
    vi.spyOn(api, 'writeNotificationPreferences').mockImplementation(() => undefined);
    vi.spyOn(api, 'loadMessagesAscending').mockResolvedValue({ ok: true, complete: true, value: [] });
    const initial: Parameters<typeof useNotifications>[0] = {
      signedIn: true, origin: 'https://markdown.example', principal: { id: 2, name: 'ada' },
      channels: [{ id: 7, name: 'general', created_at: '2026-11-18T00:00:00Z' }],
      loaded: true, eventsLive: true, event: null, readStateEvent: null, onOpen: () => undefined,
    };
    try {
      const { rerender } = renderHook((props: typeof initial) => useNotifications(props), { initialProps: initial });
      await act(async () => { await Promise.resolve(); });
      rerender({ ...initial, event: { schema: 'conch.message.v2', id: 1836, channel_id: 7, author_id: 3, created_at: '2026-11-18T00:00:00Z', body } });
      expect(show).toHaveBeenCalledWith('New message in #general', preview.replace(/\s+/g, ' ').trim(), expect.any(String), 'general');
    } finally {
      cleanup();
      if (visibility === undefined) Reflect.deleteProperty(document, 'visibilityState');
      else Object.defineProperty(document, 'visibilityState', visibility);
    }
  });

  it('shares heading and list text across replies, search and notification previews', async () => {
    const body = '# Topic\n- visible ||secret||\n  1. child';
    const message: MessageV2 = { schema: 'conch.message.v2', id: 1836, channel_id: 7, author_id: 3, created_at: '2026-11-18T00:00:00Z', body };
    expect(excerptOf(body)).toBe('Topic');
    expect(messagePreview(message)).toBe('Topic\nvisible [spoiler]\nchild');
    vi.spyOn(api, 'searchMessages').mockResolvedValue({ ok: true, value: { messages: [message] } });
    render(<SearchPanel channelId={7} channel="general" live="open" people={new Map()} nets={new Map()} viewer={2} onClose={() => undefined} onMissing={() => undefined} />);
    fireEvent.change(screen.getByRole('searchbox'), { target: { value: 'Topic' } });
    fireEvent.click(screen.getByRole('button', { name: 'Search' }));
    const results = await screen.findByRole('list', { name: 'Search results' });
    await waitFor(() => { expect(within(results).getByText('Topic')).toBeTruthy(); });
    expect(within(results).getAllByRole('listitem')).toHaveLength(3);
    expect(results.textContent).not.toContain('# Topic');
    expect(results.textContent).not.toContain('secret');
    cleanup();
    render(<NotificationCenter alerts={[{ id: 'markdown', channel: 'general', message, mention: false }]} onOpen={() => undefined} onClear={() => undefined} />);
    fireEvent.click(screen.getByRole('button', { name: 'Notifications (1)' }));
    const notification = screen.getByRole('button', { name: /New message in #general/ });
    expect(notification.textContent).toContain('Topic\nvisible [spoiler]\nchild');
    expect(notification.getAttribute('aria-label')).not.toContain('secret');
    expect(notification.textContent).not.toContain('# Topic');
  });
});
