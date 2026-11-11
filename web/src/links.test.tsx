import { cleanup, fireEvent, render, screen } from '@testing-library/react';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import * as api from './api';
import { Linkified } from './Linkified';
import { SearchPanel } from './SearchPanel';
import type { MessageV2 } from './schema/message';

const body = 'see https://example.com/a?b=1. also <b>https://example.com/t</b> javascript:alert(1) ftp://x www.example.com';
const message: MessageV2 = { schema: 'conch.message.v2', id: 1, channel_id: 7, author_id: 3, created_at: '2026-10-26T00:00:00Z', body };

beforeEach(() => {
  HTMLElement.prototype.scrollIntoView = vi.fn();
});

afterEach(() => {
  cleanup();
  vi.restoreAllMocks();
});

// A result is rendered by the same Message as the log and a thread.
describe('links in a message body', () => {
  it('shows a URL as an anchor with safe attributes, and tags and other schemes as text', async () => {
    vi.spyOn(api, 'searchMessages').mockResolvedValue({ ok: true, value: { messages: [message] } });
    render(
      <SearchPanel channelId={7} channel="general" live="open" people={new Map([[3, { name: 'member-1', kind: 'human' as const }]])} nets={new Map<number, string>()} viewer={2} onClose={vi.fn()} onMissing={vi.fn()} />,
    );
    fireEvent.change(screen.getByRole('searchbox', { name: 'Messages in #general' }), { target: { value: 'see' } });
    fireEvent.click(screen.getByRole('button', { name: 'Search' }));
    await screen.findByText(/see /);
    const links = screen.getAllByRole('link');
    const written = ['https://example.com/a?b=1', 'https://example.com/t'];
    expect(links.map((a) => a.getAttribute('href'))).toEqual(written);
    for (const [index, a] of links.entries()) {
      const text = written[index] ?? '';
      expect(a.getAttribute('rel')).toBe('noopener noreferrer');
      expect(a.getAttribute('target')).toBe('_blank');
      expect(a.getAttribute('title')).toBe(new URL(text).href);
      expect(a.textContent).toBe(text);
      expect(a.textContent).not.toContain('(opens in a new tab)');
      expect(a.querySelector('.external-link-cue .icon')).not.toBeNull();
      expect(screen.getByRole('link', { name: `${text} (opens in a new tab)` })).toBe(a);
    }
    const item = links[0]?.closest('[data-message-id]');
    expect(item?.textContent).toContain('<b>');
    expect(item?.querySelector('b')).toBeNull();
    expect(item?.textContent).toContain('javascript:alert(1) ftp://x www.example.com');
  });
});

describe('the cue splits at a whole character', () => {
  afterEach(cleanup);
  // textContent hides a lone surrogate (it reads as U+FFFD only when drawn), so
  // every text node is inspected.
  function textNodes(root: Node): string[] {
    const found: string[] = [];
    const walk = document.createTreeWalker(root, NodeFilter.SHOW_TEXT);
    for (let n = walk.nextNode(); n !== null; n = walk.nextNode()) found.push(n.nodeValue ?? '');
    return found;
  }
  it.each([
    { name: 'an emoji', text: 'https://example.com/\u{1F600}', tail: '\u{1F600}' },
    { name: 'a combining mark', text: 'https://example.com/é', tail: 'é' },
    { name: 'a plain letter', text: 'https://example.com/a', tail: 'a' },
  ])('ending in $name', ({ text, tail }) => {
    const { container } = render(<Linkified text={text} />);
    const link = container.querySelector('a');
    expect(link).not.toBeNull();
    const nodes = textNodes(link as Node);
    for (const node of nodes) expect(/[\ud800-\udbff](?![\udc00-\udfff])|(?<![\ud800-\udbff])[\udc00-\udfff]/.test(node), JSON.stringify(node)).toBe(false);
    expect(container.querySelector('.external-link-tail')?.firstChild?.nodeValue).toBe(tail);
    expect(nodes[0]).toBe(text.slice(0, text.length - tail.length));
  });
});
