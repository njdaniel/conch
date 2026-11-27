// Attachments through main (linux-desktop.md §3: "Attachments are untrusted
// bytes, not app assets"; §4). Input: an upload (bytes, a name and a declared
// type, all from the page), a download or a save (a file ID and, for a DM,
// its conversation ID).
// Output: the server's validated answer; a file's bytes for a blob preview;
// or a file written to the one path the person chose in the native save
// dialog.
//
// What it guarantees:
//  - an upload is capped (MAX_UPLOAD_BYTES, the desktop limit; the
//    server enforces its own, which may be lower), its type is reshaped to
//    type/subtype or application/octet-stream, its name is made plain text,
//    and main alone builds the request;
//  - a download is capped and returned as data; it is never written, opened,
//    run or handed to the OS;
//  - a save names no path from the page: main reads the file's name from the
//    server's metadata for the dialog's suggestion, the person chooses where,
//    and that chosen absolute path is the only place anything is written.
//    The caller (ipc.ts) allows it only right after the person's own input.
import type { MainLog } from './diagnostics.ts';
import { DESKTOP_UPLOAD_MAX_BYTES } from '../../web/src/appLimit.ts';
import { fileQuery } from '../../web/src/platform/operations.ts';
import type { BridgeReply, DownloadReply, FileTarget, SaveOutcome, UploadTarget } from '../../web/src/platform/desktop/bridge.ts';
import { fileV1 } from '../../web/src/schema/file.ts';
import { dmFileV1 } from '../../web/src/schema/generated.ts';
import { fromServer } from '../../web/src/schema/validate.ts';
import { replyOf } from './api/service.ts';
import type { Client } from './api/client.ts';
import type { Session } from './api/session.ts';
import { paramOk } from './api/target.ts';

export const MAX_UPLOAD_BYTES = DESKTOP_UPLOAD_MAX_BYTES;
export const MAX_DOWNLOAD_BYTES = 32 * 1024 * 1024;
export const MAX_FILENAME_LENGTH = 255;
// A file upload on a slow link takes longer than an ordinary request: the
// timeout is at least 120 s and grows with the file, assuming a link of no
// less than 256 kbit/s (32 000 bytes a second), plus 30 s of slack. At the
// 20 MiB cap that is about 685 s.
export const UPLOAD_TIMEOUT_FLOOR_MS = 120_000;
export const UPLOAD_MIN_BYTES_PER_SECOND = 32_000;
const UPLOAD_SLACK_MS = 30_000;
export function uploadTimeoutMs(bytes: number): number {
  return Math.max(UPLOAD_TIMEOUT_FLOOR_MS, Math.ceil((bytes / UPLOAD_MIN_BYTES_PER_SECOND) * 1000) + UPLOAD_SLACK_MS);
}
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

export type ParsedUpload = UploadTarget & {
  readonly filename: string;
  readonly mediaType: string;
  readonly bytes: Uint8Array;
};

// parseUpload checks the shape of an upload request, or gives null.
export function parseUpload(value: unknown): ParsedUpload | null {
  if (typeof value !== 'object' || value === null || Array.isArray(value)) return null;
  const record = value as Record<string, unknown>;
  const keys = Object.keys(record);
  if (keys.length !== 4 || !['filename', 'mediaType', 'data'].every((key) => keys.includes(key))) return null;
  const { channel, conversation, filename, mediaType, data } = record;
  const target = keys.includes('channel') && typeof channel === 'string' && paramOk(channel) ? { channel }
    : keys.includes('conversation') && isFileId(conversation) ? { conversation }
    : null;
  if (target === null) return null;
  if (typeof filename !== 'string' || filename.length === 0 || filename.length > 4 * MAX_FILENAME_LENGTH) return null;
  const bytes = decodeBase64(data, MAX_UPLOAD_BYTES);
  if (bytes === null) return null;
  return { ...target, filename: plainName(filename) || 'file', mediaType: mediaTypeOf(mediaType), bytes };
}

// isFileId is a file's number: a positive whole number.
export function isFileId(value: unknown): value is number {
  return typeof value === 'number' && Number.isSafeInteger(value) && value > 0;
}

export function parseFileTarget(value: unknown): FileTarget | null {
  if (isFileId(value)) return value;
  if (typeof value !== 'object' || value === null || Array.isArray(value)) return null;
  const record = value as Record<string, unknown>;
  const keys = Object.keys(record);
  const { conversation, file } = record;
  return keys.length === 2 && keys.includes('conversation') && keys.includes('file') && isFileId(conversation) && isFileId(file)
    ? { conversation, file }
    : null;
}

function bytesTarget(file: FileTarget) {
  return typeof file === 'number' ? { op: 'fileBytes', params: { id: file } } as const
    : { op: 'dmFileBytes', params: file } as const;
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
  readonly log: MainLog;
}

export class Files {
  readonly #deps: FilesDeps;

  constructor(deps: FilesDeps) {
    this.#deps = deps;
  }

  async upload(request: ParsedUpload): Promise<BridgeReply> {
    const target = 'channel' in request ? { op: 'uploadFile', params: { channel: request.channel } } as const
      : { op: 'uploadDmFile', params: { conversation: request.conversation } } as const;
    const exchange = await this.#deps.client.send({
      ...target,
      query: fileQuery(request.filename),
      upload: { mediaType: request.mediaType, bytes: request.bytes },
      timeoutMs: uploadTimeoutMs(request.bytes.length),
    });
    if (exchange.kind === 'none' && exchange.reason !== undefined && exchange.reason !== 'stale') {
      this.#deps.log(`conch-desktop: an upload had no answer: ${exchange.reason}`);
    }
    return replyOf(exchange);
  }

  async download(file: FileTarget): Promise<DownloadReply> {
    const exchange = await this.#deps.client.send({
      ...bytesTarget(file),
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
  async save(file: FileTarget): Promise<SaveOutcome> {
    const { client, session, chooseSavePath, writeFile, log } = this.#deps;
    const capture = session.capture();
    const target = typeof file === 'number' ? { op: 'fileMeta', params: { id: file }, check: fromServer(fileV1), id: file } as const
      : { op: 'dmFileMeta', params: file, check: fromServer(dmFileV1), id: file.file } as const;
    const meta = await client.send({ op: target.op, params: target.params, capture });
    if (meta.kind === 'unauthorized') return 'unauthorized';
    if (meta.kind !== 'answer') return 'failed';
    if (target.op === 'dmFileMeta' && meta.status === 403) return 'forbidden';
    if (!meta.ok || target.check(meta.body).length > 0) return 'failed';
    const info = meta.body as { filename: string; size: number };
    if (info.size > MAX_DOWNLOAD_BYTES) return 'failed';
    if (!session.current(capture)) return 'failed';

    const name = plainName(info.filename) || `file-${String(target.id)}`;
    const chosen = await chooseSavePath(this.#deps.join(this.#deps.downloadsDir(), name));
    if (chosen === null) return 'cancelled';
    if (typeof chosen !== 'string' || chosen === '' || chosen.includes('\u0000') || !this.#deps.isAbsolute(chosen)) {
      log('conch-desktop: the save dialog gave no usable path');
      return 'failed';
    }
    // Switched servers or accounts while the dialog was open: the file the
    // person chose a place for is not one this identity may have.
    if (!session.current(capture)) return 'failed';

    const got = await client.send({ ...bytesTarget(file), capture, download: { maxBytes: MAX_DOWNLOAD_BYTES }, timeoutMs: DOWNLOAD_TIMEOUT_MS });
    if (got.kind === 'unauthorized') return 'unauthorized';
    if (got.kind !== 'answer') return 'failed';
    if (target.op === 'dmFileMeta' && got.status === 403) return 'forbidden';
    if (!got.ok || got.bytes === undefined) return 'failed';
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
