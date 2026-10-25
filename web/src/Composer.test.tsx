import { createEvent, cleanup, fireEvent, render, screen, waitFor } from '@testing-library/react';
import { useReducer, useState } from 'react';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { Composer, ComposerAttachmentProvider } from './Composer';
import { attachmentDraftsReducer, attachmentsFor, initialAttachmentDrafts } from './state/attachments';
import type { Audience } from './schema/message';

const target = { net: null, whisper: null, refusal: null, note: null };

function png(width = 1, height = 1): Uint8Array<ArrayBuffer> {
  return Uint8Array.from([
    0x89, 0x50, 0x4e, 0x47, 0x0d, 0x0a, 0x1a, 0x0a,
    0, 0, 0, 13, 0x49, 0x48, 0x44, 0x52,
    (width >>> 24) & 255, (width >>> 16) & 255, (width >>> 8) & 255, width & 255,
    (height >>> 24) & 255, (height >>> 16) & 255, (height >>> 8) & 255, height & 255,
    8, 6, 0, 0, 0, 0, 0, 0, 0,
  ]);
}

function image(name: string, type = 'image/png'): File {
  return new File([png()], name, { type });
}

function Harness({ onSend }: { readonly onSend: (body: string, audience: Audience | undefined, attachments?: readonly unknown[]) => void }) {
  const [draft, setDraft] = useState('');
  const [attachments, dispatch] = useReducer(attachmentDraftsReducer, initialAttachmentDrafts);
  return (
    <ComposerAttachmentProvider value={{
      attachments: attachmentsFor(attachments, 'general'),
      add: (files) => { dispatch({ type: 'add', channel: 'general', files }); },
      remove: (clientId) => { dispatch({ type: 'remove', channel: 'general', clientId }); },
      cancel: () => undefined,
      retry: () => undefined,
    }}>
      <Composer
        channel="general"
        target={target}
        nets={new Map()}
        monitored={new Set()}
        people={new Map([[2, { name: 'ada', kind: 'human' }]])}
        viewer={2}
        draft={draft}
        enabled
        paused={false}
        onDraft={setDraft}
        onTarget={() => undefined}
        onSend={onSend}
        atBottom={() => undefined}
      />
    </ComposerAttachmentProvider>
  );
}

const createObjectURL = vi.fn<(blob: Blob) => string>();
const revokeObjectURL = vi.fn<(url: string) => void>();
const decode = vi.fn<() => Promise<{ width: number; height: number; close: () => void }>>();

beforeEach(() => {
  let next = 1;
  createObjectURL.mockImplementation(() => `blob:draft-${String(next++)}`);
  decode.mockImplementation(() => Promise.resolve({ width: 1, height: 1, close: vi.fn() }));
  vi.stubGlobal('URL', { createObjectURL, revokeObjectURL });
  vi.stubGlobal('createImageBitmap', decode);
});

afterEach(() => {
  cleanup();
  vi.clearAllMocks();
  vi.unstubAllGlobals();
});

describe('composer image drafts', () => {
  it('feeds paste, drop and picker through one preview flow without sending', async () => {
    const onSend = vi.fn();
    const { container } = render(<Harness onSend={onSend} />);
    const box = screen.getByLabelText('Message #general');
    fireEvent.paste(box, { clipboardData: { files: [image('paste.png')] } });
    await screen.findByText('paste.png');
    fireEvent.drop(container.querySelector('form') as HTMLFormElement, { dataTransfer: { files: [image('drop.png')], types: ['Files'] } });
    await screen.findByText('drop.png');
    fireEvent.change(screen.getByLabelText('Choose images'), { target: { files: [image('picker.png', 'image/jpeg')] } });
    await screen.findByText('picker.png');

    expect(screen.getAllByRole('listitem')).toHaveLength(3);
    expect(onSend).not.toHaveBeenCalled();
    expect(createObjectURL).toHaveBeenCalledTimes(3);
    expect(screen.getByRole<HTMLButtonElement>('button', { name: 'Send' }).disabled).toBe(false);
  });

  it('leaves text-only paste and IME Enter alone', () => {
    const onSend = vi.fn();
    render(<Harness onSend={onSend} />);
    const box = screen.getByLabelText('Message #general');
    const allowed = fireEvent.paste(box, { clipboardData: { files: [], items: [{ kind: 'string' }] } });
    expect(allowed).toBe(true);
    expect(screen.queryByRole('list', { name: 'Images to send' })).toBeNull();

    fireEvent.change(box, { target: { value: 'かな' } });
    const composing = createEvent.keyDown(box, { key: 'Enter' });
    Object.defineProperty(composing, 'isComposing', { value: true });
    fireEvent(box, composing);
    expect(onSend).not.toHaveBeenCalled();
    fireEvent.keyDown(box, { key: 'Enter' });
    expect(onSend).toHaveBeenCalledWith('かな', undefined, []);
  });

  it('uses filenames for an image-only body and keeps a caption when present', async () => {
    const onSend = vi.fn();
    render(<Harness onSend={onSend} />);
    fireEvent.change(screen.getByLabelText('Choose images'), { target: { files: [image('shot.png')] } });
    await screen.findByText('shot.png');
    fireEvent.click(screen.getByRole('button', { name: 'Send' }));
    expect(onSend).toHaveBeenLastCalledWith('shot.png', undefined, [expect.objectContaining({ filename: 'shot.png' })]);

    cleanup();
    render(<Harness onSend={onSend} />);
    fireEvent.change(screen.getByLabelText('Choose images'), { target: { files: [image('captioned.png')] } });
    await screen.findByText('captioned.png');
    fireEvent.change(screen.getByLabelText('Message #general'), { target: { value: 'look at this' } });
    fireEvent.click(screen.getByRole('button', { name: 'Send' }));
    expect(onSend).toHaveBeenLastCalledWith('look at this', undefined, [expect.objectContaining({ filename: 'captioned.png' })]);
  });

  it('removes previews and revokes their object URLs on removal and unmount', async () => {
    const view = render(<Harness onSend={vi.fn()} />);
    fireEvent.change(screen.getByLabelText('Choose images'), { target: { files: [image('one.png'), image('two.png')] } });
    await screen.findByText('two.png');
    fireEvent.click(screen.getByRole('button', { name: 'Remove one.png' }));
    expect(screen.queryByText('one.png')).toBeNull();
    expect(revokeObjectURL).toHaveBeenCalledWith('blob:draft-1');
    view.unmount();
    expect(revokeObjectURL).toHaveBeenCalledWith('blob:draft-2');
  });

  it('rejects unsupported, corrupt and excessive-dimension images without previews', async () => {
    render(<Harness onSend={vi.fn()} />);
    const picker = screen.getByLabelText('Choose images');
    fireEvent.change(picker, { target: { files: [new File(['<svg/>'], 'fake.png', { type: 'image/png' })] } });
    await screen.findByRole('alert');
    expect(screen.getByRole('alert').textContent).toContain('only PNG and JPEG');
    expect(createObjectURL).not.toHaveBeenCalled();

    decode.mockRejectedValueOnce(new Error('broken raster'));
    fireEvent.change(picker, { target: { files: [image('corrupt.png')] } });
    await waitFor(() => {
      expect(screen.getByRole('alert').textContent).toContain('damaged');
    });
    expect(screen.queryByText('corrupt.png')).toBeNull();

    fireEvent.change(picker, { target: { files: [new File([png(8193, 1)], 'huge.png', { type: 'image/png' })] } });
    await waitFor(() => {
      expect(screen.getByRole('alert').textContent).toContain('dimensions are too large');
    });
    expect(screen.queryByText('huge.png')).toBeNull();
  });

  it('accepts only the remaining count and explains the limit', async () => {
    render(<Harness onSend={vi.fn()} />);
    const files = Array.from({ length: 11 }, (_, index) => image(`${String(index)}.png`));
    fireEvent.change(screen.getByLabelText('Choose images'), { target: { files } });
    await screen.findByText('9.png');
    expect(screen.getAllByRole('listitem')).toHaveLength(10);
    expect(screen.queryByText('10.png')).toBeNull();
    expect(screen.getByRole('alert').textContent).toContain('Only 10 images');
  });
});
