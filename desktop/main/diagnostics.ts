// The desktop's diagnostics bundle (issue #2084). Input: every line main logs
// (the ring records each one as it is written to the standard error log), the
// versions of this build, and the person's two acts: look at the bundle, then
// save it. Output: a plain-text bundle, and that same text written to the one
// file the person chose in the native save dialog.
//
// What it guarantees:
//  - the ring holds the last RING_LINES lines and nothing more, each scrubbed
//    again as it is recorded (scrubLine): the log's lines are fixed words by
//    construction, and this is the second wall behind that, so the ring holds
//    nothing a stdout line would not. No token, URL, address, path, quoted
//    name (a room, channel or profile label), #channel or message text
//    survives it, whatever a call site interpolates;
//  - the bundle is at most BUNDLE_MAX_BYTES, the oldest lines dropped first;
//  - what the person is shown (preview) is what Save writes: the text is held
//    from the preview and written as it was, not rebuilt, so a line logged in
//    between is not in a file the person did not see. A save with no preview
//    first is refused;
//  - the page names no path: the dialog gives it (the same rule as
//    attachments, files.ts), the write is save-file.ts's, and the caller
//    (ipc.ts) allows it only right after the person's own input.
// Nothing here is sent anywhere. Attaching it to Report a problem is a wire
// change (pkg/schema), not made here.
import type { SaveOutcome } from '../../web/src/platform/desktop/bridge.ts';

export const RING_LINES = 500;
export const LINE_MAX_CHARS = 300;
export const BUNDLE_MAX_BYTES = 256 * 1024;

// Written in two parts: this file's source holds no absolute URL.
const SCHEMES = ['https', 'http', 'wss', 'ws', 'file', 'conch'];

// What a call site must never be able to leak. Applied in this order: a URL
// first (it holds tokens and names inside it), then the rest.
const RULES: readonly (readonly [RegExp, string])[] = [
  // Any scheme://..., and the bare conch: link form, to the next space.
  [new RegExp(`\\b(?:[a-z][a-z0-9+.-]*:\\/\\/|(?:${SCHEMES.join('|')}):)\\S*`, 'giu'), '[url]'],
  // An Authorization value, and name=value or name: value for a secret's name.
  [/\b(?:bearer|basic)\s+\S+/giu, '[redacted]'],
  // Named for what holds a secret, not for any word with 'code', 'session'
  // or 'key' in it: an exit code, a session phase or the talk key is what a
  // diagnosis needs, and is never a secret.
  [/\b[\w-]*(?:token|secret|password|passwd|credential|authorization|cookie|ticket|api[_-]?key|access[_-]?key|private[_-]?key|session[_-]?id|invite[_-]?code)[\w-]*\s*[=:]\s*\S+/giu, '[redacted]'],
  // A quoted string: a label, a name, a message.
  [/"[^"]*"|'[^']*'|`[^`]*`|“[^”]*”|‘[^’]*’/gu, '"[text]"'],
  // A channel or room as #name or @name.
  [/(?<![\w&])[#@][\w.-]+/gu, '[name]'],
  // A host:port or a dotted address.
  [/\b(?:\d{1,3}\.){3}\d{1,3}(?::\d+)?\b/gu, '[address]'],
  [/\b[a-z0-9-]+(?:\.[a-z0-9-]+)*\.(?:com|org|net|io|dev|app|local|lan|internal|test|example)\b(?::\d+)?/giu, '[host]'],
  // A path of the machine: a Windows drive path, or two or more segments.
  [/\b[a-z]:\\\S*/giu, '[path]'],
  [/(?<![\w.:])~?(?:\/[^\s/]+){2,}/gu, '[path]'],
  // Any long run that looks like a key, a JWT or a hash.
  [/[A-Za-z0-9_+/=-]{24,}/gu, '[redacted]'],
];

// Controls, and the invisible and direction-changing characters (as files.ts).
const CONTROLS = new RegExp('[\\u0000-\\u001f\\u007f-\\u009f\\u200b-\\u200f\\u2028-\\u202e\\u2060-\\u206f\\ufeff]', 'gu');

// scrubLine makes one log line safe to hold: no control or direction
// character (a line cannot become two), the rules above, a length bound.
export function scrubLine(line: string): string {
  let out = line.replace(CONTROLS, ' ');
  for (const [pattern, replacement] of RULES) out = out.replace(pattern, replacement);
  out = out.trim();
  return out.length > LINE_MAX_CHARS ? `${out.slice(0, LINE_MAX_CHARS)}…` : out;
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
    this.#lines.push(scrubLine(line));
    if (this.#lines.length > this.#capacity) this.#lines.splice(0, this.#lines.length - this.#capacity);
  }

  lines(): readonly string[] {
    return [...this.#lines];
  }
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
    'This holds no sign-in, address, room, channel or message text. It was made on this computer and has not been sent anywhere.',
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

export class Diagnostics {
  readonly #deps: DiagnosticsDeps;
  // The text last shown: what a save writes.
  #shown: string | null = null;

  constructor(deps: DiagnosticsDeps) {
    this.#deps = deps;
  }

  // preview builds the bundle and holds it as the one a save will write.
  preview(): string {
    const text = buildBundle({ versions: this.#deps.versions, lines: this.#deps.ring.lines(), now: this.#deps.now() });
    this.#shown = text;
    return text;
  }

  async save(): Promise<SaveOutcome> {
    const { chooseSavePath, writeFile, log } = this.#deps;
    const text = this.#shown;
    if (text === null) return 'failed';
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
