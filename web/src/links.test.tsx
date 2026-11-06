import { cleanup, fireEvent, render, screen } from '@testing-library/react';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import * as api from './api';
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
    expect(links.map((a) => a.getAttribute('href'))).toEqual(['https://example.com/a?b=1', 'https://example.com/t']);
    for (const a of links) {
      expect(a.getAttribute('rel')).toBe('noopener noreferrer');
      expect(a.getAttribute('target')).toBe('_blank');
    }
    const item = links[0]?.closest('[data-message-id]');
    expect(item?.textContent).toContain('<b>');
    expect(item?.querySelector('b')).toBeNull();
    expect(item?.textContent).toContain('javascript:alert(1) ftp://x www.example.com');
  });
});
