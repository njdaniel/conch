import { cleanup, fireEvent, render, screen, waitFor } from '@testing-library/react';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { fetchFileBytes, getFileMeta } from './api';
import { Conversation } from './Conversation';
import { ImageAttachment } from './ImageAttachment';
import { maxImageEncodedBytes, validateImage } from './image';
import type { FileV1 } from './schema/file';
import type { MessageV2, Payload } from './schema/message';
import type { ChannelChat } from './state/messages';

vi.mock('./api', () => ({ fetchFileBytes: vi.fn(), getFileMeta: vi.fn() }));
vi.mock('./image', async (importOriginal) => ({
  ...await importOriginal<typeof import('./image')>(),
  validateImage: vi.fn(),
}));

const metadata: FileV1 = {
  schema: 'conch.file.v1',
  id: 17,
  channel_id: 7,
  uploader_id: 2,
  filename: 'photo.png',
  media_type: 'image/png',
  size: 4,
  sha256: 'abc',
  created_at: '2026-10-25T10:00:00Z',
};
const bytes = new Blob(['raster'], { type: 'application/octet-stream' });
const valid = { ok: true, value: { format: 'png', width: 640, height: 480 } } as const;
const createObjectURL = vi.fn<(blob: Blob) => string>();
const revokeObjectURL = vi.fn<(url: string) => void>();
const mockedMeta = vi.mocked(getFileMeta);
const mockedBytes = vi.mocked(fetchFileBytes);
const mockedValidation = vi.mocked(validateImage);

function message(id: number, body: string, payload?: Payload): MessageV2 {
  return {
    schema: 'conch.message.v2',
    id,
    channel_id: 7,
    author_id: 2,
    created_at: '2026-10-25T10:00:00Z',
    body,
    ...(payload === undefined ? {} : { payload }),
  };
}

function conversation(messages: readonly MessageV2[], onUnauthorized?: () => void) {
  const chat: ChannelChat = {
    byId: new Map(messages.map((item) => [item.id, item])),
    pending: [],
    sentIds: new Set(),
    history: 'ready',
    older: 'idle',
    exhausted: true,
    trimmed: false,
    dropped: 0,
    nextLocalId: 1,
  };
  return render(
    <Conversation
      channel="general"
      chat={chat}
      live="open"
      people={new Map([[2, { name: 'ada', kind: 'human' }]])}
      nets={new Map()}
      viewer={2}
      draft=""
      target={{ net: null, whisper: null, refusal: null, note: null }}
      monitored={new Set()}
      onDraft={() => {}}
      onTarget={() => {}}
      onSend={() => {}}
      onReplySend={() => {}}
      onRetrySend={() => {}}
      onDismissSend={() => {}}
      onLoadOlder={() => {}}
      onReload={() => {}}
      onBottom={() => {}}
      onUnauthorized={onUnauthorized}
    />,
  );
}

beforeEach(() => {
  let next = 1;
  createObjectURL.mockImplementation(() => `blob:preview-${String(next++)}`);
  vi.stubGlobal('URL', { createObjectURL, revokeObjectURL });
  mockedMeta.mockResolvedValue({ ok: true, value: metadata });
  mockedBytes.mockResolvedValue({ ok: true, value: bytes });
  mockedValidation.mockResolvedValue(valid);
});

afterEach(() => {
  cleanup();
  vi.clearAllMocks();
  vi.restoreAllMocks();
  vi.unstubAllGlobals();
});

describe('received attachment payloads', () => {
  it('renders only a valid conch.file_attachment.v1 payload', async () => {
    conversation([
      message(1, 'caption', { schema: 'conch.file_attachment.v1', data: { file_ids: [17] } }),
      message(2, 'other schema', { schema: 'example.file_attachment.v1', data: { file_ids: [18] } }),
      message(3, 'bad attachment', { schema: 'conch.file_attachment.v1', data: { file_ids: ['19'] } }),
    ]);

    expect(await screen.findByRole('button', { name: 'Enlarge photo.png' })).toBeDefined();
    expect(screen.getByText('caption')).toBeDefined();
    expect(screen.getByText('other schema')).toBeDefined();
    expect(screen.getByText('bad attachment')).toBeDefined();
    expect(mockedMeta).toHaveBeenCalledOnce();
    expect(mockedMeta).toHaveBeenCalledWith(17);
    expect(mockedBytes).toHaveBeenCalledOnce();
    expect(mockedBytes).toHaveBeenCalledWith(17);
  });

  it('validates authenticated bytes before creating an object URL', async () => {
    mockedValidation.mockImplementation((blob) => {
      expect(blob).toBe(bytes);
      expect(createObjectURL).not.toHaveBeenCalled();
      return Promise.resolve(valid);
    });
    render(<ImageAttachment fileId={17} channel="general" viewer={2} />);

    expect(await screen.findByRole('button', { name: 'Enlarge photo.png' })).toBeDefined();
    expect(mockedMeta).toHaveBeenCalledBefore(mockedBytes);
    expect(mockedBytes).toHaveBeenCalledBefore(mockedValidation);
    expect(mockedValidation).toHaveBeenCalledBefore(createObjectURL);
    expect(screen.getByRole('button', { name: 'Enlarge photo.png' }).querySelector('img')?.getAttribute('src')).toBe('blob:preview-1');
  });
});

describe('preview fallbacks', () => {
  it.each([
    { name: 'a spoofed unsupported format', failure: 'unsupported' },
    { name: 'corrupt raster bytes', failure: 'invalid' },
  ] as const)('keeps the filename and Download for $name without creating a preview URL', async ({ failure }) => {
    mockedValidation.mockResolvedValue({ ok: false, failure });
    render(<ImageAttachment fileId={17} channel="general" viewer={2} />);

    expect(await screen.findByText('photo.png')).toBeDefined();
    expect(screen.getByRole('button', { name: 'Download' })).toBeDefined();
    expect(screen.queryByRole('img')).toBeNull();
    expect(createObjectURL).not.toHaveBeenCalled();
  });

  it('rejects oversized metadata without fetching preview bytes and keeps explicit download cleanup', async () => {
    mockedMeta.mockResolvedValue({ ok: true, value: { ...metadata, size: maxImageEncodedBytes + 1 } });
    const click = vi.spyOn(HTMLAnchorElement.prototype, 'click').mockImplementation(() => {});
    render(<ImageAttachment fileId={17} channel="general" viewer={2} />);

    expect(await screen.findByText('This image is too large to preview.')).toBeDefined();
    expect(screen.getByText('photo.png')).toBeDefined();
    expect(mockedBytes).not.toHaveBeenCalled();
    expect(mockedValidation).not.toHaveBeenCalled();
    expect(createObjectURL).not.toHaveBeenCalled();

    fireEvent.click(screen.getByRole('button', { name: 'Download' }));
    await waitFor(() => {
      expect(click).toHaveBeenCalledOnce();
    });
    expect(mockedBytes).toHaveBeenCalledOnce();
    expect(createObjectURL).toHaveBeenCalledWith(bytes);
    expect(revokeObjectURL).toHaveBeenCalledWith('blob:preview-1');
  });

  it('keeps the metadata filename when bytes are denied', async () => {
    mockedBytes.mockResolvedValue({ ok: false, failure: 'unavailable' });
    render(<ImageAttachment fileId={17} channel="general" viewer={2} />);

    expect(await screen.findByText('photo.png')).toBeDefined();
    expect(screen.getByText('Attachment unavailable. Try downloading it again.')).toBeDefined();
    expect(screen.getByRole('button', { name: 'Download' })).toBeDefined();
    expect(mockedValidation).not.toHaveBeenCalled();
    expect(createObjectURL).not.toHaveBeenCalled();
  });

  it('offers an identified download fallback when metadata is denied', async () => {
    mockedMeta.mockResolvedValue({ ok: false, failure: 'unavailable' });
    render(<ImageAttachment fileId={17} channel="general" viewer={2} />);

    expect(await screen.findByText('File 17')).toBeDefined();
    expect(screen.getByRole('button', { name: 'Download' })).toBeDefined();
    expect(mockedBytes).not.toHaveBeenCalled();
    expect(mockedValidation).not.toHaveBeenCalled();
  });

  it('reports an unauthorized metadata response', async () => {
    const onUnauthorized = vi.fn();
    mockedMeta.mockResolvedValue({ ok: false, failure: 'unauthorized' });
    render(<ImageAttachment fileId={17} channel="general" viewer={2} onUnauthorized={onUnauthorized} />);

    await waitFor(() => {
      expect(onUnauthorized).toHaveBeenCalledOnce();
    });
    expect(mockedBytes).not.toHaveBeenCalled();
    expect(createObjectURL).not.toHaveBeenCalled();
  });

  it('reports an unauthorized preview-byte response', async () => {
    const onUnauthorized = vi.fn();
    mockedBytes.mockResolvedValue({ ok: false, failure: 'unauthorized' });
    render(<ImageAttachment fileId={17} channel="general" viewer={2} onUnauthorized={onUnauthorized} />);

    await waitFor(() => {
      expect(onUnauthorized).toHaveBeenCalledOnce();
    });
    expect(mockedValidation).not.toHaveBeenCalled();
    expect(createObjectURL).not.toHaveBeenCalled();
  });

  it('downloads through the authenticated API and immediately cleans up its temporary URL', async () => {
    mockedValidation.mockResolvedValue({ ok: false, failure: 'unsupported' });
    const click = vi.spyOn(HTMLAnchorElement.prototype, 'click').mockImplementation(() => {});
    render(<ImageAttachment fileId={17} channel="general" viewer={2} />);
    fireEvent.click(await screen.findByRole('button', { name: 'Download' }));

    await waitFor(() => {
      expect(click).toHaveBeenCalledOnce();
    });
    expect(createObjectURL).toHaveBeenCalledWith(bytes);
    expect(revokeObjectURL).toHaveBeenCalledWith('blob:preview-1');
  });
});

describe('object URL lifecycle', () => {
  it('revokes on channel replacement and unmount', async () => {
    const view = render(<ImageAttachment fileId={17} channel="general" viewer={2} />);
    await screen.findByRole('button', { name: 'Enlarge photo.png' });

    view.rerender(<ImageAttachment fileId={17} channel="ops" viewer={2} />);
    expect(revokeObjectURL).toHaveBeenCalledWith('blob:preview-1');
    await screen.findByRole('button', { name: 'Enlarge photo.png' });
    view.unmount();
    expect(revokeObjectURL).toHaveBeenCalledWith('blob:preview-2');
    expect(revokeObjectURL).toHaveBeenCalledTimes(2);
  });

  it('reports and revokes a ready preview when a later download loses access', async () => {
    const onUnauthorized = vi.fn();
    render(<ImageAttachment fileId={17} channel="general" viewer={2} onUnauthorized={onUnauthorized} />);
    fireEvent.click(await screen.findByRole('button', { name: 'Enlarge photo.png' }));
    mockedBytes.mockResolvedValue({ ok: false, failure: 'unauthorized' });
    fireEvent.click(screen.getByRole('button', { name: 'Download' }));

    await waitFor(() => {
      expect(onUnauthorized).toHaveBeenCalledOnce();
    });
    expect(screen.queryByRole('dialog')).toBeNull();
    expect(revokeObjectURL).toHaveBeenCalledWith('blob:preview-1');
    expect(screen.getByRole('button', { name: 'Download' })).toBeDefined();
    expect(screen.queryByRole('img')).toBeNull();
  });

  it('revokes every visible preview when a revoked session unmounts the conversation', async () => {
    const onUnauthorized = vi.fn(() => {
      rendered.unmount();
    });
    const rendered = conversation([message(1, 'caption', { schema: 'conch.file_attachment.v1', data: { file_ids: [17, 18] } })], onUnauthorized);
    const previews = await screen.findAllByRole('button', { name: 'Enlarge photo.png' });
    expect(previews).toHaveLength(2);
    const preview = previews[0];
    if (preview === undefined) throw new Error('missing preview');
    fireEvent.click(preview);
    mockedBytes.mockResolvedValue({ ok: false, failure: 'unauthorized' });
    fireEvent.click(screen.getByRole('button', { name: 'Download' }));

    await waitFor(() => {
      expect(onUnauthorized).toHaveBeenCalledOnce();
    });
    expect(revokeObjectURL).toHaveBeenCalledWith('blob:preview-1');
    expect(revokeObjectURL).toHaveBeenCalledWith('blob:preview-2');
    expect(revokeObjectURL).toHaveBeenCalledTimes(2);
    expect(screen.queryByRole('img')).toBeNull();
  });
});

describe('enlarged image dialog', () => {
  it('contains Tab, closes on Escape, and returns focus to the thumbnail', async () => {
    render(<ImageAttachment fileId={17} channel="general" viewer={2} />);
    const thumbnail = await screen.findByRole('button', { name: 'Enlarge photo.png' });
    thumbnail.focus();
    fireEvent.click(thumbnail);

    const enlarged = screen.getByRole('dialog', { name: 'photo.png' });
    const download = screen.getByRole('button', { name: 'Download' });
    const close = screen.getByRole('button', { name: 'Close' });
    expect(screen.getByRole('img', { name: 'photo.png' })).toBeDefined();
    expect(document.activeElement).toBe(close);
    fireEvent.keyDown(enlarged, { key: 'Tab' });
    expect(document.activeElement).toBe(download);
    fireEvent.keyDown(enlarged, { key: 'Tab', shiftKey: true });
    expect(document.activeElement).toBe(close);
    fireEvent.keyDown(enlarged, { key: 'Escape' });
    expect(screen.queryByRole('dialog')).toBeNull();
    expect(document.activeElement).toBe(thumbnail);

    fireEvent.keyDown(thumbnail, { key: 'Enter' });
    fireEvent.click(thumbnail);
    fireEvent.click(screen.getByRole('button', { name: 'Close' }));
    expect(screen.queryByRole('dialog')).toBeNull();
    expect(document.activeElement).toBe(thumbnail);
  });
});
