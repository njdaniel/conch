// The desktop's diagnostics bundle (issue #2084). Input: fixed diagnostic
// lines chosen by main, build versions, and the person's acts of previewing
// and saving. Output: the preview and those same bytes in the file the person
// chose in the native save dialog.
//
// Main keeps free-form details in its local log. The ring scrubs the remaining
// lines again, holds at most RING_LINES, and drops the oldest lines first to
// fit BUNDLE_MAX_BYTES. Each preview is held by ID so later previews cannot
// change a pending save. The caller (ipc.ts) requires recent native input;
// the dialog supplies the path (as in files.ts) and save-file.ts owns the write.
// Export is local. Report a problem attachments would need a pkg/schema change.
import { randomBytes } from 'node:crypto';
import { isIP } from 'node:net';
import type { DiagnosticsPreview, SaveOutcome } from '../../web/src/platform/desktop/bridge.ts';

export const RING_LINES = 500;
export const LINE_MAX_CHARS = 300;
export const BUNDLE_MAX_BYTES = 256 * 1024;
// The longest line the ring scrubs; a longer one is kept as a marker.
export const RECORD_MAX_CHARS = 2000;

// Written in two parts: this file's source holds no absolute URL.
const SCHEMES = ['https', 'http', 'wss', 'ws', 'file', 'conch'];

// Defense in depth for lines admitted by main. Handle quotes before shorter
// matches can consume their opening delimiter and expose the rest.
const RULES: readonly (readonly [RegExp, string])[] = [
  [/"(?:\\.|[^"\\])*"|'(?:\\.|[^'\\])*'|`(?:\\.|[^`\\])*`|“(?:\\.|[^”\\])*”|‘(?:\\.|[^’\\])*’/gu, '"[text]"'],
  // Keep a bare scheme name used as a label in a fixed diagnostic.
  [new RegExp(`\\b(?:[a-z][a-z0-9+.-]*:\\/\\/|(?:${SCHEMES.join('|')}):)\\S+`, 'giu'), '[url]'],
  // Authorization and named secrets. Quoted values are now one token.
  [/\b(?:bearer|basic)\s+\S+/giu, '[redacted]'],
  [/\b[\w-]*(?:token|secret|password|passwd|credential|authorization|cookie|ticket|api[_-]?key|access[_-]?key|private[_-]?key|session[_-]?id|invite[_-]?code)[\w-]*\s*[=:]\s*\S+/giu, '[redacted]'],
  // A bare key assignment is a secret. Keep fixed labels such as 'talk key:'.
  [/\bkey\s*=\s*\S+/giu, '[redacted]'],
  // A channel or room as #name or @name.
  [/(?<![\w&])[#@][\w.-]+/gu, '[name]'],
  [/\b[a-z0-9-]+(?:\.[a-z0-9-]+)*\.(?:com|org|net|io|dev|app|local|lan|internal|test|example)\b(?::\d+)?/giu, '[host]'],
  // A path of the machine: a Windows drive path, or two or more segments.
  [/\b[a-z]:\\\S*/giu, '[path]'],
  [/(?<![\w.:])~?(?:\/[^\s/]+){2,}/gu, '[path]'],
];

// Controls, and the invisible and direction-changing characters (as files.ts).
const CONTROLS = new RegExp('[\\u0000-\\u001f\\u007f-\\u009f\\u200b-\\u200f\\u2028-\\u202e\\u2060-\\u206f\\ufeff]', 'gu');

// scrubLine applies another pass of redaction and bounds each line.
// Control and direction characters cannot forge a second line.
export function scrubLine(line: string): string {
  let out = line.replace(CONTROLS, ' ');
  for (const [pattern, replacement] of RULES) out = out.replace(pattern, replacement);
  // IPv6 first so a mapped IPv4 suffix stays part of the address.
  out = out.replace(/\[([a-f\d:.]+(?:%[\w.-]+)?)\](?::\d+)?|(?<![\w:.])(?:[a-f\d]*:){2,}[a-f\d:.]*(?:%[\w.-]+)?/giu, (match, bracketed: string | undefined) => {
    const address = bracketed ?? match;
    const candidate = bracketed === undefined ? address.replace(/\.+$/u, '') : address;
    return isIP(candidate) === 6 ? `[address]${address.slice(candidate.length)}` : match;
  });
  out = out.replace(/\b(?:\d{1,3}\.){3}\d{1,3}(?::\d+)?\b/gu, '[address]');
  // Any long run that looks like a key, a JWT or a hash.
  out = out.replace(/[A-Za-z0-9_+/=-]{24,}/gu, '[redacted]').trim();
  return out.length > LINE_MAX_CHARS ? `${out.slice(0, LINE_MAX_CHARS - 1)}…` : out;
}

// The last RING_LINES lines, oldest first. Memory only: it is never written
// anywhere but through Diagnostics.save.
export class LogRing {
  readonly #lines: string[] = [];
  readonly #capacity: number;

  constructor(capacity = RING_LINES) {
    this.#capacity = capacity;
  }

  record(line: string): void {
    // The scrub is slow on a very long line (its patterns backtrack), and it
    // runs on main's thread: such a line is kept as a fixed marker instead.
    // Not sliced and then scrubbed: a cut could shorten a key below the
    // long-run rule's length.
    this.#lines.push(line.length > RECORD_MAX_CHARS ? `[a ${String(line.length)}-character line, not kept]` : scrubLine(line));
    if (this.#lines.length > this.#capacity) this.#lines.splice(0, this.#lines.length - this.#capacity);
  }

  lines(): readonly string[] {
    return [...this.#lines];
  }
}

// Call sites opt out of the ring for any line with free-form details.
export type MainLog = (line: string, options?: { readonly ring: false }) => void;

export function diagnosticsLogger(ring: LogRing, writeLocal: (line: string) => void): MainLog {
  return (line, options) => {
    if (options?.ring !== false) ring.record(line);
    writeLocal(line);
  };
}

export interface BundleInput {
  // This build's versions: names and version strings main itself read.
  readonly versions: Readonly<Record<string, string>>;
  readonly lines: readonly string[];
  readonly now: Date;
}

// buildBundle is the text the person sees and may save. If the lines make it
// larger than BUNDLE_MAX_BYTES the oldest are left out, and it says how many.
export function buildBundle({ versions, lines, now }: BundleInput): string {
  const head = [
    'Conch desktop diagnostics',
    `made: ${now.toISOString()}`,
    ...Object.entries(versions).map(([name, value]) => `${scrubLine(name)}: ${scrubLine(value)}`),
    '',
    'This file contains build versions and recent diagnostic events. Read it before sharing. It was made on this computer and has not been sent anywhere.',
    '',
  ];
  const render = (from: number): string => {
    const kept = lines.slice(from);
    const note = from > 0 ? [`(${String(from)} older log lines left out to keep this small)`] : [];
    return [...head, `log (last ${String(kept.length)} lines, oldest first):`, ...note, ...kept, ''].join('\n');
  };
  let from = 0;
  let text = render(0);
  // Drop the oldest one at a time: the ring is at most RING_LINES lines.
  while (Buffer.byteLength(text, 'utf8') > BUNDLE_MAX_BYTES && from < lines.length) {
    from += 1;
    text = render(from);
  }
  return text;
}

export interface DiagnosticsDeps {
  readonly ring: Pick<LogRing, 'lines'>;
  readonly versions: Readonly<Record<string, string>>;
  readonly now: () => Date;
  // Shows the native save dialog with this suggested path and gives the path
  // the person chose, or null if they closed it.
  readonly chooseSavePath: (suggested: string) => Promise<string | null>;
  readonly writeFile: (path: string, bytes: Uint8Array) => Promise<void>;
  readonly downloadsDir: () => string;
  readonly join: (...parts: string[]) => string;
  readonly isAbsolute: (path: string) => boolean;
  readonly log: (line: string) => void;
}

const SNAPSHOT_LIMIT = 4;

export class Diagnostics {
  readonly #deps: DiagnosticsDeps;
  // Insertion order lets a new preview evict the oldest snapshot.
  readonly #shown = new Map<string, string>();

  constructor(deps: DiagnosticsDeps) {
    this.#deps = deps;
  }

  // Hold the exact text returned with each main-generated ID.
  preview(): DiagnosticsPreview {
    const text = buildBundle({ versions: this.#deps.versions, lines: this.#deps.ring.lines(), now: this.#deps.now() });
    const id = randomBytes(16).toString('hex');
    this.#shown.set(id, text);
    if (this.#shown.size > SNAPSHOT_LIMIT) {
      const oldest = this.#shown.keys().next();
      if (!oldest.done) this.#shown.delete(oldest.value);
    }
    return { id, text };
  }

  async save(id: string): Promise<SaveOutcome> {
    const { chooseSavePath, writeFile, log } = this.#deps;
    const text = this.#shown.get(id);
    if (text === undefined) return 'failed';
    const stamp = this.#deps.now().toISOString().slice(0, 10);
    const chosen = await chooseSavePath(this.#deps.join(this.#deps.downloadsDir(), `conch-diagnostics-${stamp}.txt`));
    if (chosen === null) return 'cancelled';
    if (typeof chosen !== 'string' || chosen === '' || chosen.includes('\u0000') || !this.#deps.isAbsolute(chosen)) {
      log('conch-desktop: the save dialog gave no usable path');
      return 'failed';
    }
    try {
      await writeFile(chosen, new TextEncoder().encode(text));
    } catch {
      log('conch-desktop: diagnostics could not be saved');
      return 'failed';
    }
    return 'saved';
  }
}
