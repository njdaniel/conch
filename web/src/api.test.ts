import { afterEach, describe, expect, it, vi } from 'vitest';
import { openSocket } from './api';

afterEach(() => {
  vi.unstubAllGlobals();
  window.sessionStorage.clear();
});

describe('openSocket', () => {
  it('answers a plain network failure, carrying nothing of the error, when the WebSocket constructor throws', async () => {
    window.sessionStorage.setItem('conch.web.token', 'conch_secret');
    vi.stubGlobal(
      'fetch',
      vi.fn(() => Promise.resolve(new Response(JSON.stringify({ ticket: 'conch_wst_T', expires_at: '2026-10-14T10:00:00Z' }), { status: 201 }))),
    );
    vi.stubGlobal(
      'WebSocket',
      vi.fn((url: string) => {
        throw new Error(`refused ${url}`);
      }),
    );
    const result = await openSocket('ops', 0, { onOpen: () => undefined, onFrame: () => undefined, onClose: () => undefined });
    expect(result).toEqual({ ok: false, failure: 'network' });
    expect(JSON.stringify(result)).not.toContain('conch_wst_T');
  });
});
