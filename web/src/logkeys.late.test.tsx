// Controls that mount later inside a message that is not the log's tab stop
// (issue #1040): an attachment finishing its load adds an Enlarge button, which
// must not become a Tab stop; and the observer that catches it is tied to the
// message list and let go on unmount.
import { act, cleanup, render, screen } from '@testing-library/react';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { fetchFileBytes, getFileMeta } from './api';
import { Conversation } from './Conversation';
import type { FileV1 } from './schema/file';
import type { MessageV2 } from './schema/message';
import type { ChannelChat } from './state/messages';

vi.mock('./api', () => ({ fetchFileBytes: vi.fn(), getFileMeta: vi.fn() }));
vi.mock('./image', async (importOriginal) => ({
  ...await importOriginal<typeof import('./image')>(),
  validateImage: vi.fn().mockResolvedValue({ ok: true, value: { format: 'png', width: 640, height: 480 } }),
}));

const metadata: FileV1 = { schema: 'conch.file.v1', id: 17, channel_id: 7, uploader_id: 2, filename: 'photo.png', media_type: 'image/png', size: 4, sha256: 'abc', created_at: '2026-10-25T10:00:00Z' };

function msg(id: number, fields: Partial<MessageV2> = {}): MessageV2 {
  return { schema: 'conch.message.v2', id, channel_id: 7, author_id: 3, created_at: '2026-10-25T10:00:00Z', body: `text ${String(id)}`, ...fields };
}

function mount() {
  const messages = [msg(1, { payload: { schema: 'conch.file_attachment.v1', data: { file_ids: [17] } } }), msg(2)];
  const chat: ChannelChat = { byId: new Map(messages.map((m) => [m.id, m])), pending: [], sentIds: new Set(), history: 'ready', older: 'idle', exhausted: true, trimmed: false, dropped: 0, nextLocalId: 1, edits: new Map(), final: new Set() };
  return render(
    <Conversation
      channel="general"
      chat={chat}
      live="open"
      people={new Map([[2, { name: 'ada', kind: 'human' as const }], [3, { name: 'bea', kind: 'human' as const }]])}
      nets={new Map()}
      viewer={2}
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
    />,
  );
}

const item = (id: number) => document.querySelector<HTMLElement>(`.messages > [data-message-id="${String(id)}"]`) as HTMLElement;

beforeEach(() => {
  HTMLElement.prototype.scrollIntoView = vi.fn();
  let n = 0;
  vi.stubGlobal('URL', { createObjectURL: () => `blob:p-${String(n++)}`, revokeObjectURL: () => undefined });
});

afterEach(() => {
  cleanup();
  vi.clearAllMocks();
  vi.unstubAllGlobals();
});

describe('a control that mounts later in a message that is not the stop', () => {
  it('is kept out of Tab, and given back when that message becomes the stop', async () => {
    let finish!: (value: Awaited<ReturnType<typeof fetchFileBytes>>) => void;
    vi.mocked(getFileMeta).mockResolvedValue({ ok: true, value: metadata });
    vi.mocked(fetchFileBytes).mockReturnValue(new Promise((resolve) => { finish = resolve; }));
    mount();
    expect(screen.queryByRole('button', { name: 'Enlarge photo.png' })).toBeNull();

    await act(async () => {
      await Promise.resolve();
      finish({ ok: true, value: new Blob(['raster']) });
    });
    const enlarge = await screen.findByRole('button', { name: 'Enlarge photo.png' });
    // Message 2 is the stop, so message 1's new button is out of the order.
    await vi.waitFor(() => { expect(enlarge.tabIndex).toBe(-1); });
    expect(enlarge.hasAttribute('data-roved')).toBe(true);

    act(() => { item(1).focus(); });
    expect(enlarge.tabIndex).toBe(0);
    expect(enlarge.hasAttribute('data-roved')).toBe(false);
  });
});

describe('the observer', () => {
  it('watches the message list, and disconnects on unmount', () => {
    const observe = vi.fn();
    const disconnect = vi.fn();
    vi.stubGlobal('MutationObserver', class {
      observe = observe;
      disconnect = disconnect;
    });
    vi.mocked(getFileMeta).mockReturnValue(new Promise(() => undefined));
    const view = mount();
    const list = document.querySelector('.messages');
    expect(observe).toHaveBeenCalledTimes(1);
    expect(observe).toHaveBeenCalledWith(list, { childList: true, subtree: true });
    expect(disconnect).not.toHaveBeenCalled();
    view.unmount();
    expect(disconnect).toHaveBeenCalledTimes(1);
  });
});
