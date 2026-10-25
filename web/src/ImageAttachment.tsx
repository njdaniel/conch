import { useCallback, useEffect, useId, useRef, useState, type KeyboardEvent } from 'react';
import { fetchFileBytes, getFileMeta } from './api';
import { maxImageEncodedBytes, validateImage, type ImageValidationResult } from './image';

interface AttachmentView {
  readonly filename: string;
  readonly url: string | null;
  readonly note: string | null;
}

function validationNote(result: Exclude<ImageValidationResult, { readonly ok: true }>): string {
  switch (result.failure) {
    case 'unsupported':
      return 'This file format is not supported for image preview.';
    case 'too-large':
      return 'This image is too large to preview.';
    case 'unavailable':
      return 'Image preview is not available in this browser.';
    case 'invalid':
      return 'The file is broken or is not a valid PNG or JPEG.';
  }
}

function ignoreUnauthorized() {}

export function ImageAttachment({
  fileId,
  channel,
  viewer,
  onUnauthorized = ignoreUnauthorized,
}: {
  readonly fileId: number;
  readonly channel: string;
  readonly viewer: number;
  readonly onUnauthorized?: () => void;
}) {
  const id = useId();
  const generation = useRef(0);
  const objectURL = useRef<string | null>(null);
  const trigger = useRef<HTMLButtonElement>(null);
  const dialog = useRef<HTMLDivElement>(null);
  const closeButton = useRef<HTMLButtonElement>(null);
  const [view, setView] = useState<AttachmentView>({ filename: `File ${String(fileId)}`, url: null, note: 'Loading attachment…' });
  const [open, setOpen] = useState(false);
  const [downloading, setDownloading] = useState(false);
  const [downloadError, setDownloadError] = useState<string | null>(null);

  const clearPreview = useCallback(() => {
    const url = objectURL.current;
    objectURL.current = null;
    if (url !== null) URL.revokeObjectURL(url);
  }, []);

  useEffect(() => {
    const own = ++generation.current;
    const stale = () => generation.current !== own;
    clearPreview();
    setOpen(false);
    setDownloading(false);
    setDownloadError(null);
    setView({ filename: `File ${String(fileId)}`, url: null, note: 'Loading attachment…' });

    void (async () => {
      const metadata = await getFileMeta(fileId);
      if (stale()) return;
      if (!metadata.ok) {
        if (metadata.failure === 'unauthorized') onUnauthorized();
        setView({ filename: `File ${String(fileId)}`, url: null, note: 'Attachment unavailable. Try downloading it again.' });
        return;
      }
      const filename = metadata.value.filename;
      if (metadata.value.size > maxImageEncodedBytes) {
        setView({ filename, url: null, note: 'This image is too large to preview.' });
        return;
      }
      setView({ filename, url: null, note: 'Loading attachment…' });
      const bytes = await fetchFileBytes(fileId);
      if (stale()) return;
      if (!bytes.ok) {
        if (bytes.failure === 'unauthorized') onUnauthorized();
        setView({ filename, url: null, note: 'Attachment unavailable. Try downloading it again.' });
        return;
      }
      const validated = await validateImage(bytes.value);
      if (stale()) return;
      if (!validated.ok) {
        setView({ filename, url: null, note: validationNote(validated) });
        return;
      }
      let url: string;
      try {
        url = URL.createObjectURL(bytes.value);
      } catch {
        setView({ filename, url: null, note: 'Image preview is not available in this browser.' });
        return;
      }
      if (stale()) {
        URL.revokeObjectURL(url);
        return;
      }
      objectURL.current = url;
      setView({ filename, url, note: null });
    })().catch(() => {
      if (!stale()) setView((current) => ({ ...current, url: null, note: 'Attachment unavailable. Try downloading it again.' }));
    });

    return () => {
      if (generation.current === own) generation.current++;
      clearPreview();
    };
  }, [fileId, channel, viewer, clearPreview, onUnauthorized]);

  useEffect(() => {
    if (!open) return;
    closeButton.current?.focus();
    return () => {
      trigger.current?.focus();
    };
  }, [open]);

  function breakPreview() {
    clearPreview();
    setOpen(false);
    setView((current) => ({ ...current, url: null, note: 'The image preview could not be displayed.' }));
  }

  async function download() {
    if (downloading) return;
    const own = generation.current;
    setDownloading(true);
    setDownloadError(null);
    const result = await fetchFileBytes(fileId);
    if (generation.current !== own) return;
    setDownloading(false);
    if (!result.ok) {
      if (result.failure === 'unauthorized') onUnauthorized();
      if (result.failure === 'unauthorized' || result.failure === 'unavailable') {
        clearPreview();
        setOpen(false);
        setView((current) => ({ ...current, url: null, note: 'Attachment unavailable. Try downloading it again.' }));
      }
      setDownloadError('Download unavailable. Try again.');
      return;
    }
    let url: string | undefined;
    try {
      url = URL.createObjectURL(result.value);
      const anchor = document.createElement('a');
      anchor.href = url;
      anchor.download = view.filename;
      document.body.append(anchor);
      anchor.click();
      anchor.remove();
    } catch {
      setDownloadError('Download unavailable. Try again.');
      return;
    } finally {
      if (url !== undefined) URL.revokeObjectURL(url);
    }
  }

  function onDialogKey(event: KeyboardEvent<HTMLDivElement>) {
    if (event.key === 'Escape') {
      event.preventDefault();
      setOpen(false);
      return;
    }
    if (event.key !== 'Tab') return;
    const controls = dialog.current?.querySelectorAll<HTMLButtonElement>('button:not(:disabled)');
    if (controls === undefined || controls.length === 0) return;
    const first = controls[0];
    const last = controls[controls.length - 1];
    if (event.shiftKey && document.activeElement === first) {
      event.preventDefault();
      last?.focus();
    } else if (!event.shiftKey && document.activeElement === last) {
      event.preventDefault();
      first?.focus();
    }
  }

  const preview = view.url === null ? null : (
    <>
      <button ref={trigger} type="button" className="attachment-thumbnail" aria-label={`Enlarge ${view.filename}`} onClick={() => { setOpen(true); }}>
        <img src={view.url} alt="" onError={breakPreview} />
      </button>
      {open ? (
        <div className="attachment-dialog-layer">
          <div className="attachment-dialog-backdrop" />
          <div
            ref={dialog}
            className="attachment-dialog"
            role="dialog"
            aria-modal="true"
            aria-labelledby={`${id}-title`}
            onKeyDown={onDialogKey}
          >
            <h2 id={`${id}-title`}>{view.filename}</h2>
            <img className="attachment-enlarged" src={view.url} alt={view.filename} onError={breakPreview} />
            <div className="actions">
              <button type="button" disabled={downloading} onClick={() => { void download(); }}>
                {downloading ? 'Downloading…' : 'Download'}
              </button>
              <button ref={closeButton} type="button" onClick={() => { setOpen(false); }}>
                Close
              </button>
            </div>
          </div>
        </div>
      ) : null}
    </>
  );

  return (
    <figure className={view.url === null ? 'attachment attachment-fallback' : 'attachment attachment-image'}>
      {preview}
      <figcaption className="attachment-filename">{view.filename}</figcaption>
      {view.note === null ? null : <span className="attachment-note">{view.note}</span>}
      {view.url !== null ? null : (
        <button type="button" className="attachment-download" disabled={downloading} onClick={() => { void download(); }}>
          {downloading ? 'Downloading…' : 'Download'}
        </button>
      )}
      {downloadError === null ? null : <span className="attachment-download-error" role="alert">{downloadError}</span>}
    </figure>
  );
}
