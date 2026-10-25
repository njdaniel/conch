# Browser image attachments

Issue [#762](https://github.com/njdaniel/conch/issues/762). This is the browser rendering and send contract for the existing file routes and `conch.file_attachment.v1` payload from #23. It adds no wire shape and no public file URL.

## 1. Scope and input

Paste, drop and the file picker feed one draft flow. Text-only paste and IME composition keep their ordinary behavior. The browser reads selected or pasted `File`/`Blob` bytes only; it never fetches clipboard HTML, a remote image URL or a URL found in a filename.

Upload happens only after an explicit Send. Choosing, pasting, dropping, previewing or removing an image sends nothing to conchd. At Send, freeze the server, account, channel, audience, caption and file list. An audience or server change does not retarget an upload or post already in flight. Logout, access loss and server/account change invalidate pending completions and revoke local object URLs.

The initial inline formats are PNG and JPEG. SVG, HTML, GIF, WebP and every other format are never inline images. An unsupported format may use a generic download-only attachment flow, but must not be given an image preview.

## 2. Raster validation

`web/src/image.ts` exports `validateImage(blob: Blob): Promise<ImageValidationResult>`. Validation is content-based; `File.name`, `File.type`, stored `media_type` and response `Content-Type` are not evidence that bytes are safe.

The first pass has no dependencies:

- PNG requires the complete eight-byte signature and a complete, correctly positioned 13-byte IHDR segment. Width and height come from IHDR.
- JPEG requires SOI and walks complete marker segments to a supported start-of-frame segment. Segment lengths must fit the available bytes, and start-of-frame component length must be consistent. Width and height come from that segment.
- A recognized but short or malformed header is invalid. Parsing never returns dimensions from a truncated segment.
- Zero dimensions are invalid. Either axis above 8192 or more than 25,000,000 decoded pixels is `too-large`; both limits are inclusive at the boundary.

Only after those checks does validation call the browser's actual `createImageBitmap(blob, { imageOrientation: 'none' })`. A decoder rejection is invalid. The decoded width and height are independently bounded and must equal the parsed dimensions. Every successfully created bitmap is closed on every path. A browser without `createImageBitmap` returns `unavailable`, not an inline preview.

The result is one of:

- `{ ok: true, value: { format: 'png' | 'jpeg', width, height } }`;
- `{ ok: false, failure: 'unsupported' | 'invalid' | 'too-large' | 'unavailable' }`.

The function returns metadata only. It does not create an object URL, retain a bitmap or upload bytes. The same function validates a local draft and every downloaded file before inline display. Header inspection alone is not sufficient, and decoder support alone must not expand the allowed formats.

## 3. Authenticated private previews

Draft previews may use an object URL for the already validated local `Blob`. History previews are different:

1. Read the attachment's `conch.file.v1` metadata with `getFileMeta(id)`.
2. Fetch bytes with `fetchFileBytes(id)`. This sends the pinned credential in the `Authorization` header, uses `cache: 'no-store'` and `credentials: 'omit'`, and never puts a token in a URL.
3. Run `validateImage` on those bytes. Only a successful result may become `URL.createObjectURL(blob)` and an `<img src="blob:…">`.

Never use `/v2/files/{id}` directly as an `<img>` or other subresource URL. The route deliberately returns `application/octet-stream`, `Content-Disposition: attachment`, `X-Content-Type-Options: nosniff` and `Cache-Control: private, no-store`; the preview contract does not weaken it. Metadata and bytes both pass the server's central file visibility predicate, including channel, net, whisper, history watermark and revocation rules. A private 404 is `unavailable`; a 401 ends the client credential as elsewhere.

The CSP allowance is `blob:` in `img-src` only. It does not permit blob scripts, connections, objects or frames. Do not add public or token-bearing file URLs to make rendering easier.

Object URLs are revoked when a preview is removed or replaced, when its component unmounts, and on logout, access loss or server/account change. Cached blobs and URLs are not durable history and must not survive into another identity's view. A broken, denied or unsupported history preview has a filename and ordinary download fallback, not a direct image response.

## 4. Upload and post

`uploadFile(channel, name, mediaType, bytes, signal)` posts the raw `Blob` body to `/v2/channels/{channel}/files?filename=…`; it does not use multipart or JSON. Progress is indeterminate because portable `fetch` upload byte progress is unavailable. The caller supplies an `AbortSignal` for Cancel. Results distinguish `cancelled`, `unauthorized`, `network` and `refused`; refusals retain the server's code and one-line reason, including `uploads_disabled`, `file_too_large` and permission failures.

`fileAttachmentPayload(fileIds)` builds the generated-schema payload:

```json
{
  "schema": "conch.file_attachment.v1",
  "data": { "file_ids": [17, 18] }
}
```

Pass it as the fourth argument to `postMessage(channel, body, audience, payload)`. Generated `FileV1` validators check upload and metadata responses. The server remains authoritative for the attachment-count limit, distinct positive IDs, uploader, channel and message audience.

Upload each unsent file once and keep every successful file ID with the draft. A failed or cancelled file may be retried without uploading the successful files again. Preserve caption, files and IDs after failure. Do not upload again automatically on an uncertain post.

### Single-use IDs and reconciliation

A file ID can be attached to at most one message. A successful post consumes its IDs; a later attempt to attach one is `invalid_file_attachment`. This makes reusing an ID after an uncertain result unsafe until reconciliation.

After `postMessage` returns `unconfirmed` (no answer, a 5xx, or an unreadable success), do not immediately post again. Instead:

1. Keep the exact frozen caption, audience, channel and attachment ID set.
2. Read visible history with `loadMessagesAscending(channel, lastConfirmedMessageId)`. It follows `order=asc`, `after` and `next_after` and returns messages in ascending ID order.
3. Look for the caller's message with the exact frozen body, audience and attachment payload. If found, adopt the returned message and clear the draft; do not post again.
4. Retry only with the same frozen target and IDs after reconciliation has caught up without finding it. If history cannot be read completely, remains unauthorized, or exceeds the helper's bounded page walk, leave the outcome uncertain rather than claiming a safe retry.

A definitive `refused` post stored no message. Keep the uploaded IDs for the unsent draft, but `invalid_file_attachment` still requires refreshing state: it can mean an ID was consumed by an earlier uncertain post, not merely that the new request was malformed. Reconcile before reuploading or attaching it elsewhere.

The API has no idempotency key. Matching and the single-use server constraint reduce duplicate risk; they do not justify blind automatic retries.

## 5. Actual abandoned-upload persistence

Completed uploads are not temporary drafts on the server. conchd currently has no cleanup or retention job for an uploaded row that is never attached. Its metadata row and content-addressed bytes persist indefinitely, including when:

- the person cancels or abandons the draft after a successful upload;
- the browser closes, logs out or loses access;
- an upload succeeds but its response or file ID is lost;
- a post remains unconfirmed or is refused.

Uploading identical bytes again can reuse the same physical blob, but creates another file row and ID. Those rows also persist. Temporary `.upload-*` files from interrupted writes are different: conchd removes them at startup. Backup retention is also separate; normal backups include the `files/` directory and do not reclaim live abandoned uploads.

The client must not claim that abandoned images expire or are automatically deleted. Any future server-side reclamation needs a separate design covering row references, content-addressed blob sharing, message visibility and backups.

## 6. Browser API summary

The integration surface is:

- `validateImage(blob)` in `web/src/image.ts`, with `maxImageAxis = 8192` and `maxImagePixels = 25_000_000`;
- `uploadFile(...)`, `getFileMeta(id)` and `fetchFileBytes(id)` in `web/src/api.ts`;
- `fileAttachmentPayload(fileIds)` and `postMessage(channel, body, audience?, payload?)`;
- `loadMessagesAscending(channel, after?)` for reconciliation;
- generated `FileV1`, `FileAttachmentV1`, `fileV1` and `fileAttachmentV1` bindings in `web/src/schema/file.ts`.

Tests must include spoofed MIME/extension, all unsupported inline formats, truncation, corrupt decoder input, both dimension caps, decoded-dimension disagreement and bitmap cleanup. API tests cover raw upload/download requests, payload JSON, refusals, cancellation and 401. Real-browser and two-client tests must additionally exercise actual decoding, object-URL lifecycle, history reload, private audiences, guest watermarks and revoked sessions.
