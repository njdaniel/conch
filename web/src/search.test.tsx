import { cleanup, fireEvent, render, screen, waitFor } from '@testing-library/react';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import * as api from './api';
import { SearchPanel } from './SearchPanel';
import type { MessageV2 } from './schema/message';

const at = '2026-10-26T00:00:00Z';
const message = (id: number, body = `match ${String(id)}`): MessageV2 => ({ schema: 'conch.message.v2', id, channel_id: 7, author_id: 3, created_at: at, body });
const people = new Map([[3, { name: 'bea', kind: 'human' as const }]]);
const props = {
  channelId: 7,
  channel: 'general',
  live: 'open' as const,
  people,
  nets: new Map<number, string>(),
  viewer: 2,
  onClose: vi.fn(),
  onMissing: vi.fn(),
};

function ids(): number[] {
  return [...screen.getByRole('list', { name: 'Search results' }).querySelectorAll('[data-message-id]')].map((element) => Number(element.getAttribute('data-message-id')));
}

function submit(words = 'match') {
  fireEvent.change(screen.getByRole('searchbox', { name: 'Messages in #general' }), { target: { value: words } });
  fireEvent.click(screen.getByRole('button', { name: 'Search' }));
}

beforeEach(() => {
  HTMLElement.prototype.scrollIntoView = vi.fn();
});

afterEach(() => {
  cleanup();
  vi.restoreAllMocks();
});

describe('channel search', () => {
  it('orders and deduplicates pages, then opens context and returns to the same results', async () => {
    const search = vi.spyOn(api, 'searchMessages')
      .mockResolvedValueOnce({ ok: true, value: { messages: [message(3), message(2)], next_before: 2 } })
      .mockResolvedValueOnce({ ok: true, value: { messages: [message(2), message(1)] } });
    vi.spyOn(api, 'loadMessages').mockResolvedValue({ ok: true, value: [message(1)] });
    vi.spyOn(api, 'loadMessagesAfter').mockResolvedValue({ ok: true, value: [message(2), message(3)] });
    render(<SearchPanel {...props} />);
    submit();
    await screen.findByText('match 3');
    expect(ids()).toEqual([3, 2]);
    fireEvent.click(screen.getByRole('button', { name: 'Load older results' }));
    await screen.findByText('match 1');
    expect(ids()).toEqual([3, 2, 1]);
    expect(search).toHaveBeenLastCalledWith('general', 'match', 2);
    fireEvent.click(screen.getByRole('button', { name: 'Open context for message 2' }));
    await screen.findByRole('list', { name: 'Message context' });
    await waitFor(() => {
      expect(document.activeElement?.getAttribute('data-message-id')).toBe('2');
    });
    fireEvent.click(screen.getByRole('button', { name: 'Back to results' }));
    expect(ids()).toEqual([3, 2, 1]);
    expect(screen.getByRole('searchbox')).toHaveProperty('value', 'match');
    await waitFor(() => {
      expect(document.activeElement?.getAttribute('data-message-id')).toBe('2');
    });
  });

  it('shows empty results and retries an error', async () => {
    const search = vi.spyOn(api, 'searchMessages')
      .mockResolvedValueOnce({ ok: false, failure: 'network' })
      .mockResolvedValueOnce({ ok: true, value: { messages: [] } });
    render(<SearchPanel {...props} />);
    submit();
    await screen.findByRole('alert');
    fireEvent.click(screen.getByRole('button', { name: 'Try again' }));
    await screen.findByText('No messages match “match”.');
    expect(search).toHaveBeenCalledTimes(2);
  });

  it('ignores an older query response that finishes after a newer one', async () => {
    let finishFirst!: (value: api.ApiResult<import('./schema/message').SearchMessagesResponseV2>) => void;
    const search = vi.spyOn(api, 'searchMessages')
      .mockReturnValueOnce(new Promise((resolve) => {
        finishFirst = resolve;
      }))
      .mockResolvedValueOnce({ ok: true, value: { messages: [message(4, 'newer')] } });
    render(<SearchPanel {...props} />);
    submit('first');
    submit('second');
    await screen.findByText('newer');
    finishFirst({ ok: true, value: { messages: [message(3, 'older')] } });
    await waitFor(() => {
      expect(search).toHaveBeenCalledTimes(2);
    });
    expect(screen.queryByText('older')).toBeNull();
    expect(screen.getByText('newer')).toBeTruthy();
  });

  it('marks results stale while reconnecting and refreshes without duplicates', async () => {
    const search = vi.spyOn(api, 'searchMessages')
      .mockResolvedValueOnce({ ok: true, value: { messages: [message(3), message(2)] } })
      .mockResolvedValueOnce({ ok: true, value: { messages: [message(3), message(2)] } });
    const view = render(<SearchPanel {...props} />);
    submit();
    await screen.findByText('match 3');
    view.rerender(<SearchPanel {...props} live="reconnecting" />);
    expect(screen.getByText('Reconnecting; search results may be out of date.')).toBeTruthy();
    view.rerender(<SearchPanel {...props} />);
    await waitFor(() => {
      expect(search).toHaveBeenCalledTimes(2);
    });
    expect(ids()).toEqual([3, 2]);
  });

  it('signs out and clears results on an unauthorized page', async () => {
    const onUnauthorized = vi.fn();
    vi.spyOn(api, 'searchMessages')
      .mockResolvedValueOnce({ ok: true, value: { messages: [message(3)], next_before: 3 } })
      .mockResolvedValueOnce({ ok: false, failure: 'unauthorized' });
    render(<SearchPanel {...props} onUnauthorized={onUnauthorized} />);
    submit();
    await screen.findByText('match 3');
    fireEvent.click(screen.getByRole('button', { name: 'Load older results' }));
    await waitFor(() => {
      expect(onUnauthorized).toHaveBeenCalled();
    });
    expect(screen.queryByRole('list', { name: 'Search results' })).toBeNull();
    expect(screen.queryByRole('button', { name: 'Load older results' })).toBeNull();
  });

  it('drops an old cursor when a fresh search fails', async () => {
    vi.spyOn(api, 'searchMessages')
      .mockResolvedValueOnce({ ok: true, value: { messages: [message(3)], next_before: 3 } })
      .mockResolvedValueOnce({ ok: false, failure: 'network' });
    render(<SearchPanel {...props} />);
    submit();
    await screen.findByText('match 3');
    submit('other');
    await screen.findByRole('alert');
    expect(screen.queryByRole('button', { name: 'Load older results' })).toBeNull();
  });

  it('does not reopen context after Back cancels its requests', async () => {
    vi.spyOn(api, 'searchMessages').mockResolvedValue({ ok: true, value: { messages: [message(3)] } });
    let finishBefore!: (value: api.ApiResult<MessageV2[]>) => void;
    let finishAfter!: (value: api.ApiResult<MessageV2[]>) => void;
    vi.spyOn(api, 'loadMessages').mockReturnValue(new Promise((resolve) => {
      finishBefore = resolve;
    }));
    vi.spyOn(api, 'loadMessagesAfter').mockReturnValue(new Promise((resolve) => {
      finishAfter = resolve;
    }));
    render(<SearchPanel {...props} />);
    submit();
    fireEvent.click(await screen.findByRole('button', { name: 'Open context for message 3' }));
    fireEvent.click(screen.getByRole('button', { name: 'Back to results' }));
    finishBefore({ ok: true, value: [message(2)] });
    finishAfter({ ok: true, value: [message(3)] });
    await waitFor(() => {
      expect(screen.queryByRole('list', { name: 'Message context' })).toBeNull();
    });
    expect(screen.getByRole('list', { name: 'Search results' })).toBeTruthy();
  });

  it('refreshes the latest attempted query, not the previous completed query', async () => {
    let finishOld!: (value: api.ApiResult<import('./schema/message').SearchMessagesResponseV2>) => void;
    const search = vi.spyOn(api, 'searchMessages')
      .mockResolvedValueOnce({ ok: true, value: { messages: [message(3, 'first')] } })
      .mockReturnValueOnce(new Promise((resolve) => {
        finishOld = resolve;
      }))
      .mockResolvedValueOnce({ ok: true, value: { messages: [message(4, 'latest')] } });
    const view = render(<SearchPanel {...props} />);
    submit('first');
    await screen.findByText('first');
    view.rerender(<SearchPanel {...props} live="reconnecting" />);
    submit('second');
    view.rerender(<SearchPanel {...props} />);
    await screen.findByText('latest');
    finishOld({ ok: true, value: { messages: [message(3, 'stale')] } });
    await waitFor(() => {
      expect(search).toHaveBeenCalledTimes(3);
    });
    expect(screen.queryByText('stale')).toBeNull();
    expect(search).toHaveBeenLastCalledWith('general', 'second', undefined);
  });

  it('honors an unauthorized context response even when the other read has a network error', async () => {
    const onUnauthorized = vi.fn();
    vi.spyOn(api, 'searchMessages').mockResolvedValue({ ok: true, value: { messages: [message(3)] } });
    vi.spyOn(api, 'loadMessages').mockResolvedValue({ ok: false, failure: 'network' });
    vi.spyOn(api, 'loadMessagesAfter').mockResolvedValue({ ok: false, failure: 'unauthorized' });
    render(<SearchPanel {...props} onUnauthorized={onUnauthorized} />);
    submit();
    fireEvent.click(await screen.findByRole('button', { name: 'Open context for message 3' }));
    await waitFor(() => {
      expect(onUnauthorized).toHaveBeenCalled();
    });
    expect(screen.queryByRole('list', { name: 'Search results' })).toBeNull();
    expect(screen.queryByRole('list', { name: 'Message context' })).toBeNull();
  });

  it('does not resurrect context that was cleared by a disconnect', async () => {
    vi.spyOn(api, 'searchMessages').mockResolvedValue({ ok: true, value: { messages: [message(3)] } });
    let finishBefore!: (value: api.ApiResult<MessageV2[]>) => void;
    let finishAfter!: (value: api.ApiResult<MessageV2[]>) => void;
    vi.spyOn(api, 'loadMessages').mockReturnValue(new Promise((resolve) => {
      finishBefore = resolve;
    }));
    vi.spyOn(api, 'loadMessagesAfter').mockReturnValue(new Promise((resolve) => {
      finishAfter = resolve;
    }));
    const view = render(<SearchPanel {...props} />);
    submit();
    fireEvent.click(await screen.findByRole('button', { name: 'Open context for message 3' }));
    view.rerender(<SearchPanel {...props} live="reconnecting" />);
    finishBefore({ ok: true, value: [message(2)] });
    finishAfter({ ok: true, value: [message(3)] });
    await waitFor(() => {
      expect(screen.queryByRole('list', { name: 'Message context' })).toBeNull();
    });
  });

  it('retries the failed query rather than the previous successful one', async () => {
    const search = vi.spyOn(api, 'searchMessages')
      .mockResolvedValueOnce({ ok: true, value: { messages: [message(3, 'first')] } })
      .mockResolvedValueOnce({ ok: false, failure: 'network' })
      .mockResolvedValueOnce({ ok: true, value: { messages: [] } });
    render(<SearchPanel {...props} />);
    submit('first');
    await screen.findByText('first');
    submit('second');
    await screen.findByRole('alert');
    fireEvent.click(screen.getByRole('button', { name: 'Try again' }));
    await screen.findByText('No messages match “second”.');
    expect(search).toHaveBeenLastCalledWith('general', 'second', undefined);
  });

  it('focuses the query on entry', () => {
    render(<SearchPanel {...props} />);
    expect(document.activeElement).toBe(screen.getByRole('searchbox'));
  });

  it('drops results and reports a missing channel', async () => {
    const onMissing = vi.fn();
    vi.spyOn(api, 'searchMessages').mockResolvedValue({ ok: false, failure: 'network', code: 'channel_not_found' });
    render(<SearchPanel {...props} onMissing={onMissing} />);
    submit();
    await screen.findByRole('alert');
    expect(onMissing).toHaveBeenCalledWith(7);
    expect(screen.queryByRole('list', { name: 'Search results' })).toBeNull();
  });
});
