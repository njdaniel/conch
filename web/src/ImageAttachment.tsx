import { useCallback, useEffect, useId, useRef, useState, type KeyboardEvent } from 'react';
import { fetchFileBytes, getFileMeta } from './api';
import { platform } from './platform';
import { formatSize } from './fileSize';
import { maxImageEncodedBytes, validateImage } from './image';
import { Spoiler } from './Spoiler';
import { isSpoilerName, rememberFileName, shownName } from './spoilerFiles';

interface AttachmentView {
  // The stored name, as the server has it. A spoiler's shown name drops the
  // prefix; Download saves this one unchanged.
  readonly filename: string;
  readonly mediaType: string;
  readonly url: string | null;
  readonly note: string | null;
  // The file's size once its metadata is read. A file with a size and no note
  // is not a previewable image: it shows as a file chip (name, size, Download).
  readonly size: number | null;
  // A spoiler whose bytes have not been asked for: only its metadata is read
  // until the viewer presses Reveal.
  readonly hidden: boolean;
}

// A declared type that is plainly not an image is not fetched to be sniffed.
// An unknown or generic type still is, so an image sent by another client
// keeps its preview.
function maybeImage(mediaType: string): boolean {
  return mediaType === '' || mediaType === 'application/octet-stream' || mediaType.startsWith('image/');
}

function ignoreUnauthorized() {}

function loadingView(fileId: number): AttachmentView {
  return { filename: `File ${String(fileId)}`, mediaType: '', url: null, note: 'Loading attachment…', size: null, hidden: false };
}

// Whether the bytes are worth fetching to be shown as an image.
function previewable(view: AttachmentView): boolean {
  return view.size !== null && view.size <= maxImageEncodedBytes && maybeImage(view.mediaType);
}

export function ImageAttachment({
  fileId,
  channel,
  viewer,
  onUnauthorized = ignoreUnauthorized,
  onFilename,
}: {
  readonly fileId: number;
  readonly channel: string;
  readonly viewer: number;
  readonly onUnauthorized?: () => void;
  // Told the file's name once its metadata is read, so a message whose text is
  // only that name can show it once (F26).
  readonly onFilename?: (filename: string) => void;
}) {
  const id = useId();
  const nameSink = useRef(onFilename);
  nameSink.current = onFilename;
  const generation = useRef(0);
  const objectURL = useRef<string | null>(null);
  const trigger = useRef<HTMLButtonElement>(null);
  const dialog = useRef<HTMLDivElement>(null);
  const closeButton = useRef<HTMLButtonElement>(null);
  const unauthorized = useRef(onUnauthorized);
  unauthorized.current = onUnauthorized;
  const revealing = useRef(0);
  const [view, setView] = useState<AttachmentView>(() => loadingView(fileId));
  const [open, setOpen] = useState(false);
  const [downloading, setDownloading] = useState(false);
  const [downloadError, setDownloadError] = useState<string | null>(null);

  const clearPreview = useCallback(() => {
    const url = objectURL.current;
    objectURL.current = null;
    if (url !== null) URL.revokeObjectURL(url);
  }, []);

  // load fetches the bytes and shows the image, or the file chip when they are
  // not a valid image. An ordinary file does it at once; a spoiler, on Reveal.
  const load = useCallback(async (own: number, base: AttachmentView) => {
    const mine = ++revealing.current;
    const stale = () => generation.current !== own || revealing.current !== mine;
    const { filename, mediaType, size } = base;
    const show = (next: Partial<AttachmentView>) => { setView({ filename, mediaType, size, url: null, note: null, hidden: false, ...next }); };
    show({ note: 'Loading attachment…' });
    try {
      const bytes = await fetchFileBytes(fileId);
      if (stale()) return;
      if (!bytes.ok) {
        if (bytes.failure === 'unauthorized') unauthorized.current();
        show({ note: 'Attachment unavailable. Try downloading it again.' });
        return;
      }
      const validated = await validateImage(bytes.value);
      if (stale()) return;
      if (!validated.ok) {
        show({});
        return;
      }
      let url: string;
      try {
        url = URL.createObjectURL(bytes.value);
      } catch {
        show({ note: 'Image preview is not available in this browser.' });
        return;
      }
      if (stale()) {
        URL.revokeObjectURL(url);
        return;
      }
      objectURL.current = url;
      show({ url });
    } catch {
      if (!stale()) setView((current) => ({ ...current, url: null, note: 'Attachment unavailable. Try downloading it again.' }));
    }
  }, [fileId]);

  useEffect(() => {
    const own = ++generation.current;
    revealing.current++;
    const stale = () => generation.current !== own;
    clearPreview();
    setOpen(false);
    setDownloading(false);
    setDownloadError(null);
    setView(loadingView(fileId));

    void (async () => {
      const metadata = await getFileMeta(fileId);
      if (stale()) return;
      if (!metadata.ok) {
        if (metadata.failure === 'unauthorized') unauthorized.current();
        setView({ ...loadingView(fileId), note: 'Attachment unavailable. Try downloading it again.' });
        return;
      }
      const filename = metadata.value.filename;
      rememberFileName(fileId, filename);
      nameSink.current?.(filename);
      const size = metadata.value.size;
      const mediaType = metadata.value.media_type;
      const base: AttachmentView = { filename, mediaType, url: null, note: null, size, hidden: false };
      if (size > maxImageEncodedBytes || !maybeImage(mediaType)) {
        // A spoiler that is only a file chip has nothing to fetch or decode;
        // it is hidden all the same, so the card reads alike.
        setView({ ...base, hidden: isSpoilerName(filename) });
        return;
      }
      // Only the metadata is read for a spoiler until the viewer reveals it.
      if (isSpoilerName(filename)) {
        setView({ ...base, hidden: true });
        return;
      }
      await load(own, base);
    })().catch(() => {
      if (!stale()) setView((current) => ({ ...current, url: null, note: 'Attachment unavailable. Try downloading it again.' }));
    });

    return () => {
      if (generation.current === own) generation.current++;
      revealing.current++;
      clearPreview();
    };
  }, [fileId, channel, viewer, clearPreview, load]);

  function reveal() {
    setDownloadError(null);
    // A file that is not a previewable image has nothing to fetch: Reveal
    // shows its chip.
    if (!previewable(view)) setView({ ...view, hidden: false });
    else void load(generation.current, view);
  }

  function hide() {
    revealing.current++;
    clearPreview();
    setOpen(false);
    setView((current) => ({ ...current, url: null, note: null, hidden: true }));
  }

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

  const spoiler = isSpoilerName(view.filename);
  const shown = shownName(view.filename);
  const chipSize = view.url === null && view.note === null ? view.size : null;
  const chip = chipSize !== null;
  const preview = view.url === null ? null : (
    <>
      <button ref={trigger} type="button" className="attachment-thumbnail" aria-label={`Enlarge ${shown}`} onClick={() => { setOpen(true); }}>
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
            <h2 id={`${id}-title`}>{shown}</h2>
            <img className="attachment-enlarged" src={view.url} alt={shown} onError={breakPreview} />
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
  const hiddenSpoiler = spoiler && view.hidden;
  const body = (
    <>
      {preview}
      {spoiler ? <span className="attachment-filename">{shown}</span> : <figcaption className="attachment-filename">{shown}</figcaption>}
      {chipSize === null ? null : <span className="attachment-size">{formatSize(chipSize)}</span>}
      {view.note === null ? null : <span className="attachment-note">{view.note}</span>}
    </>
  );
  // The Spoiler stays at one place in the tree through Reveal and Hide, so the
  // toggle keeps its focus. Hidden, only metadata has been read: no image, no
  // blob URL, no bytes.
  return (
    <figure className={hiddenSpoiler ? 'attachment attachment-spoiler' : view.url !== null ? 'attachment attachment-image' : chip ? 'attachment attachment-file' : 'attachment attachment-fallback'}>
      {hiddenSpoiler ? <figcaption className="attachment-filename">{`Spoiler: ${shown}, ${view.mediaType}, ${view.size === null ? '' : formatSize(view.size)}`}</figcaption> : null}
      {spoiler ? <Spoiler block revealed={!view.hidden} onToggle={view.hidden ? reveal : hide} showText="Reveal" hideText="Hide" label={shown}>{body}</Spoiler> : body}
      {spoiler || view.url === null ? (
        <button type="button" className="attachment-download" disabled={downloading} onClick={() => { void download(); }}>
          {downloading ? 'Downloading…' : 'Download'}
        </button>
      ) : null}
      {downloadError === null ? null : <span className="attachment-download-error" role="alert">{downloadError}</span>}
    </figure>
  );
}
