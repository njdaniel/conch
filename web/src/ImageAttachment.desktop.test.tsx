import { cleanup, fireEvent, render, screen, waitFor } from '@testing-library/react';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { fetchFileBytes, getFileMeta } from './api';
import { ImageAttachment } from './ImageAttachment';
import type { FileV1 } from './schema/file';

// In the desktop shell, Download is main's native save dialog: the page names
// the file by id, gets no bytes back, and makes no download link.
const saveFile = vi.hoisted(() => vi.fn<(id: number) => Promise<'saved' | 'cancelled' | 'failed' | 'unauthorized'>>());

vi.mock('./api', () => ({ fetchFileBytes: vi.fn(), getFileMeta: vi.fn() }));
vi.mock('./platform', () => ({ platform: { services: { saveFile } } }));

const metadata: FileV1 = {
  schema: 'conch.file.v1',
  id: 17,
  channel_id: 7,
  uploader_id: 2,
  filename: 'report.pdf',
  media_type: 'application/pdf',
  size: 2048,
  sha256: 'abc',
  created_at: '2026-10-25T10:00:00Z',
};

beforeEach(() => {
  vi.mocked(getFileMeta).mockResolvedValue({ ok: true, value: metadata });
  saveFile.mockResolvedValue('saved');
});

afterEach(() => {
  cleanup();
  vi.clearAllMocks();
});

describe('Download in the desktop shell', () => {
  it('asks main to save the file by id, and fetches and links nothing itself', async () => {
    const anchorClick = vi.spyOn(HTMLAnchorElement.prototype, 'click').mockImplementation(() => undefined);
    render(<ImageAttachment fileId={17} channel="general" viewer={2} />);
    fireEvent.click(await screen.findByRole('button', { name: 'Download' }));
    await waitFor(() => {
      expect(saveFile).toHaveBeenCalledExactlyOnceWith(17);
    });
    expect(fetchFileBytes).not.toHaveBeenCalled();
    expect(anchorClick).not.toHaveBeenCalled();
    expect(screen.queryByRole('alert')).toBeNull();
    anchorClick.mockRestore();
  });

  it('says nothing when the person closes the dialog, and says so when saving failed', async () => {
    saveFile.mockResolvedValueOnce('cancelled');
    render(<ImageAttachment fileId={17} channel="general" viewer={2} />);
    fireEvent.click(await screen.findByRole('button', { name: 'Download' }));
    await waitFor(() => {
      expect(saveFile).toHaveBeenCalledOnce();
    });
    expect(screen.queryByRole('alert')).toBeNull();
    saveFile.mockResolvedValueOnce('failed');
    fireEvent.click(await screen.findByRole('button', { name: 'Download' }));
    expect((await screen.findByRole('alert')).textContent).toBe('Download unavailable. Try again.');
  });

  it('reports an ended sign-in', async () => {
    saveFile.mockResolvedValueOnce('unauthorized');
    const onUnauthorized = vi.fn();
    render(<ImageAttachment fileId={17} channel="general" viewer={2} onUnauthorized={onUnauthorized} />);
    fireEvent.click(await screen.findByRole('button', { name: 'Download' }));
    await waitFor(() => {
      expect(onUnauthorized).toHaveBeenCalledOnce();
    });
  });
});
