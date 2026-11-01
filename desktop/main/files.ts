// Attachments through main (linux-desktop.md §3: "Attachments are untrusted
// bytes, not app assets"; §4). Input: an upload (bytes, a name and a declared
// type, all from the page), a download (a file id) or a save (a file id).
// Output: the server's validated answer; a file's bytes for a blob preview;
// or a file written to the one path the person chose in the native save
// dialog.
//
// What it guarantees:
//  - an upload is capped (MAX_UPLOAD_BYTES, the server's default limit; the
//    server enforces its own, which may be lower), its type is reshaped to
//    type/subtype or application/octet-stream, its name is made plain text,
//    and main alone builds the request;
//  - a download is capped and returned as data; it is never written, opened,
//    run or handed to the OS;
//  - a save names no path from the page: main reads the file's name from the
//    server's metadata for the dialog's suggestion, the person chooses where,
//    and that chosen absolute path is the only place anything is written.
//    The caller (ipc.ts) allows it only right after the person's own input.
import { fileQuery } from '../../web/src/platform/operations.ts';
import type { BridgeReply, DownloadReply, SaveOutcome } from '../../web/src/platform/desktop/bridge.ts';
import { fileV1 } from '../../web/src/schema/file.ts';
import { replyOf } from './api/service.ts';
import type { Client } from './api/client.ts';
import type { Session } from './api/session.ts';
import { paramOk } from './api/target.ts';

export const MAX_UPLOAD_BYTES = 10 * 1024 * 1024;
export const MAX_DOWNLOAD_BYTES = 32 * 1024 * 1024;
export const MAX_FILENAME_LENGTH = 255;
// An upload of ten megabytes on a slow link takes longer than a request.
const UPLOAD_TIMEOUT_MS = 120_000;
const DOWNLOAD_TIMEOUT_MS = 120_000;

export const OCTET_STREAM = 'application/octet-stream';

// A media type token: RFC 7231's type "/" subtype with no parameters.
const MEDIA_TYPE = /^[a-z0-9][a-z0-9!#$&^_.+-]{0,126}\/[a-z0-9][a-z0-9!#$&^_.+-]{0,126}$/iu;

// Controls, and the invisible and direction-changing characters.
const INVISIBLE_RANGES: readonly (readonly [number, number])[] = [
  [0x0000, 0x001f],
  [0x007f, 0x009f],
  [0x00ad, 0x00ad],
  [0x061c, 0x061c],
  [0x180e, 0x180e],
  [0x200b, 0x200f],
  [0x2028, 0x202e],
  [0x2060, 0x206f],
  [0xfeff, 0xfeff],
  [0xfff9, 0xfffb],
];

function withoutInvisible(text: string): string {
  return Array.from(text).filter((char) => {
    const code = char.codePointAt(0) ?? 0;
    return !INVISIBLE_RANGES.some(([low, high]) => code >= low && code <= high);
  }).join('');
}

// mediaTypeOf reshapes a declared type: only a bare type/subtype survives,
// lower-cased; anything else is an unknown file.
export function mediaTypeOf(declared: unknown): string {
  if (typeof declared !== 'string') return OCTET_STREAM;
  const bare = declared.split(';')[0]?.trim() ?? '';
  return MEDIA_TYPE.test(bare) ? bare.toLowerCase() : OCTET_STREAM;
}

// plainName makes an untrusted file name into a name that is text and nothing
// else: no directory part (either separator), no control or invisible
// character, no leading dots or spaces, at most 255 characters. It may be
// empty; the caller says what then.
export function plainName(name: unknown): string {
  if (typeof name !== 'string') return '';
  const base = name.slice(Math.max(name.lastIndexOf('/'), name.lastIndexOf('\\')) + 1);
  return withoutInvisible(base).replace(/^[\s.]+/u, '').replace(/[\s.]+$/u, '').slice(0, MAX_FILENAME_LENGTH);
}

// decodeBase64 is strict: the alphabet, padding in place, and no larger than
// cap once decoded; null otherwise.
export function decodeBase64(data: unknown, cap: number): Uint8Array | null {
  if (typeof data !== 'string' || data.length % 4 !== 0 || !/^[A-Za-z0-9+/]*={0,2}$/u.test(data)) return null;
  if ((data.length / 4) * 3 - 2 > cap) return null;
  const bytes = Buffer.from(data, 'base64');
  return bytes.byteLength > cap ? null : new Uint8Array(bytes.buffer, bytes.byteOffset, bytes.byteLength);
}

export interface ParsedUpload {
  readonly channel: string;
  readonly filename: string;
  readonly mediaType: string;
  readonly bytes: Uint8Array;
}

// parseUpload checks the shape of an upload request, or gives null.
export function parseUpload(value: unknown): ParsedUpload | null {
  if (typeof value !== 'object' || value === null || Array.isArray(value)) return null;
  const record = value as Record<string, unknown>;
  const keys = Object.keys(record);
  if (keys.length !== 4 || !['channel', 'filename', 'mediaType', 'data'].every((key) => keys.includes(key))) return null;
  const { channel, filename, mediaType, data } = record;
  if (typeof channel !== 'string' || !paramOk(channel)) return null;
  if (typeof filename !== 'string' || filename.length === 0 || filename.length > 4 * MAX_FILENAME_LENGTH) return null;
  const bytes = decodeBase64(data, MAX_UPLOAD_BYTES);
  if (bytes === null) return null;
  return { channel, filename: plainName(filename) || 'file', mediaType: mediaTypeOf(mediaType), bytes };
}

// isFileId is a file's number: a positive whole number.
export function isFileId(value: unknown): value is number {
  return typeof value === 'number' && Number.isSafeInteger(value) && value > 0;
}

export interface FilesDeps {
  readonly client: Pick<Client, 'send'>;
  // The identity a save is made for (capture) and whether it still is (current).
  readonly session: Pick<Session, 'capture' | 'current'>;
  // Shows the native save dialog with this suggested path (a plain name in
  // the downloads directory) and gives the path the person chose, or null if
  // they closed it. The only way a path comes into being.
  readonly chooseSavePath: (suggested: string) => Promise<string | null>;
  readonly writeFile: (path: string, bytes: Uint8Array) => Promise<void>;
  readonly downloadsDir: () => string;
  readonly join: (...parts: string[]) => string;
  readonly isAbsolute: (path: string) => boolean;
  readonly log: (line: string) => void;
}

export class Files {
  readonly #deps: FilesDeps;

  constructor(deps: FilesDeps) {
    this.#deps = deps;
  }

  async upload(request: ParsedUpload): Promise<BridgeReply> {
    const exchange = await this.#deps.client.send({
      op: 'uploadFile',
      params: { channel: request.channel },
      query: fileQuery(request.filename),
      upload: { mediaType: request.mediaType, bytes: request.bytes },
      timeoutMs: UPLOAD_TIMEOUT_MS,
    });
    if (exchange.kind === 'none' && exchange.reason !== undefined && exchange.reason !== 'stale') {
      this.#deps.log(`conch-desktop: an upload had no answer: ${exchange.reason}`);
    }
    return replyOf(exchange);
  }

  async download(id: number): Promise<DownloadReply> {
    const exchange = await this.#deps.client.send({
      op: 'fileBytes',
      params: { id },
      download: { maxBytes: MAX_DOWNLOAD_BYTES },
      timeoutMs: DOWNLOAD_TIMEOUT_MS,
    });
    if (exchange.kind === 'answer' && exchange.ok && exchange.bytes !== undefined) {
      return { kind: 'bytes', data: Buffer.from(exchange.bytes).toString('base64') };
    }
    return replyOf(exchange);
  }

  // save: the file's name from the server, the person's choice of path, the
  // bytes, one write. Nothing is opened afterwards. The sign-in and server it
  // began under are captured once: both requests are made under that capture,
  // and the dialog (which can stay open as long as the person likes) is not
  // shown to, nor the file written for, an identity that has since ended.
  async save(id: number): Promise<SaveOutcome> {
    const { client, session, chooseSavePath, writeFile, log } = this.#deps;
    const capture = session.capture();
    const meta = await client.send({ op: 'fileMeta', params: { id }, capture });
    if (meta.kind === 'unauthorized') return 'unauthorized';
    if (meta.kind !== 'answer' || !meta.ok || fileV1(meta.body).length > 0) return 'failed';
    const info = meta.body as { filename: string; size: number };
    if (info.size > MAX_DOWNLOAD_BYTES) return 'failed';
    if (!session.current(capture)) return 'failed';

    const name = plainName(info.filename) || `file-${String(id)}`;
    const chosen = await chooseSavePath(this.#deps.join(this.#deps.downloadsDir(), name));
    if (chosen === null) return 'cancelled';
    if (typeof chosen !== 'string' || chosen === '' || chosen.includes('\u0000') || !this.#deps.isAbsolute(chosen)) {
      log('conch-desktop: the save dialog gave no usable path');
      return 'failed';
    }
    // Switched servers or accounts while the dialog was open: the file the
    // person chose a place for is not one this identity may have.
    if (!session.current(capture)) return 'failed';

    const got = await client.send({ op: 'fileBytes', params: { id }, capture, download: { maxBytes: MAX_DOWNLOAD_BYTES }, timeoutMs: DOWNLOAD_TIMEOUT_MS });
    if (got.kind === 'unauthorized') return 'unauthorized';
    if (got.kind !== 'answer' || !got.ok || got.bytes === undefined) return 'failed';
    if (!session.current(capture)) return 'failed';
    try {
      await writeFile(chosen, got.bytes);
    } catch {
      log('conch-desktop: a file could not be saved');
      return 'failed';
    }
    return 'saved';
  }
}
