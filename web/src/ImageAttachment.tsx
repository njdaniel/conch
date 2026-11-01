import { useCallback, useEffect, useId, useRef, useState, type KeyboardEvent } from 'react';
import { fetchFileBytes, getFileMeta } from './api';
import { platform } from './platform';
import { formatSize } from './fileSize';
import { maxImageEncodedBytes, validateImage } from './image';

interface AttachmentView {
  readonly filename: string;
  readonly url: string | null;
  readonly note: string | null;
  // The file's size once its metadata is read. A file with a size and no note
  // is not a previewable image: it shows as a file chip (name, size, Download).
  readonly size: number | null;
}

// A declared type that is plainly not an image is not fetched to be sniffed.
// An unknown or generic type still is, so an image sent by another client
// keeps its preview.
function maybeImage(mediaType: string): boolean {
  return mediaType === '' || mediaType === 'application/octet-stream' || mediaType.startsWith('image/');
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
  const [view, setView] = useState<AttachmentView>({ filename: `File ${String(fileId)}`, url: null, note: 'Loading attachment…', size: null });
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
    setView({ filename: `File ${String(fileId)}`, url: null, note: 'Loading attachment…', size: null });

    void (async () => {
      const metadata = await getFileMeta(fileId);
      if (stale()) return;
      if (!metadata.ok) {
        if (metadata.failure === 'unauthorized') onUnauthorized();
        setView({ filename: `File ${String(fileId)}`, url: null, note: 'Attachment unavailable. Try downloading it again.', size: null });
        return;
      }
      const filename = metadata.value.filename;
      const size = metadata.value.size;
      if (size > maxImageEncodedBytes || !maybeImage(metadata.value.media_type)) {
        setView({ filename, url: null, note: null, size });
        return;
      }
      setView({ filename, url: null, note: 'Loading attachment…', size });
      const bytes = await fetchFileBytes(fileId);
      if (stale()) return;
      if (!bytes.ok) {
        if (bytes.failure === 'unauthorized') onUnauthorized();
        setView({ filename, url: null, note: 'Attachment unavailable. Try downloading it again.', size });
        return;
      }
      const validated = await validateImage(bytes.value);
      if (stale()) return;
      if (!validated.ok) {
        setView({ filename, url: null, note: null, size });
        return;
      }
      let url: string;
      try {
        url = URL.createObjectURL(bytes.value);
      } catch {
        setView({ filename, url: null, note: 'Image preview is not available in this browser.', size });
        return;
      }
      if (stale()) {
        URL.revokeObjectURL(url);
        return;
      }
      objectURL.current = url;
      setView({ filename, url, note: null, size });
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
    // The desktop saves through main's native dialog; the bytes never come
    // back to the page and it names no path.
    const save = platform.services.saveFile;
    if (save !== undefined) {
      const saved = await save(fileId);
      if (generation.current !== own) return;
      setDownloading(false);
      if (saved === 'unauthorized') onUnauthorized();
      if (saved === 'failed' || saved === 'unauthorized') setDownloadError('Download unavailable. Try again.');
      return;
    }
    const result = await fetchFileBytes(fileId);
    if (generation.current !== own) return;
    setDownloading(false);
    if (!result.ok) {
      if (result.failure === 'unauthorized') onUnauthorized();
      if (result.failure === 'unauthorized' || result.failure === 'unavailable') {
        clearPreview();
        setOpen(false);
        setView((current) => ({ ...current, url: null, note: 'Attachment unavailable. Try downloading it again.', size: null }));
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

  const chipSize = view.url === null && view.note === null ? view.size : null;
  const chip = chipSize !== null;
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
    <figure className={view.url !== null ? 'attachment attachment-image' : chip ? 'attachment attachment-file' : 'attachment attachment-fallback'}>
      {preview}
      <figcaption className="attachment-filename">{view.filename}</figcaption>
      {chipSize === null ? null : <span className="attachment-size">{formatSize(chipSize)}</span>}
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
