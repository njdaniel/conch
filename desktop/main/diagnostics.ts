// The desktop's diagnostics bundle (issue #2084). Input: fixed diagnostic
// lines chosen by main, build versions, counts of sampled events, and the
// person's acts of previewing and saving. Output: the preview and those same
// bytes in the file the person chose in the native save dialog.
//
// Only explicitly opted-in, fixed-vocabulary log lines enter the export ring.
// Opt-in is the privacy boundary; the scrub is best-effort defense in depth.
// The ring holds at most RING_LINES and the bundle drops the oldest lines to
// fit BUNDLE_MAX_BYTES. Each preview is held by ID so later previews cannot
// change a pending save. The caller (ipc.ts) requires recent native input;
// the dialog supplies the path (as in files.ts) and save-file.ts owns the write.
// Export is local. Report a problem attachments would need a pkg/schema change.
import { randomBytes } from 'node:crypto';
import { isIP } from 'node:net';
import type { DiagnosticsPreview, FeedKind, SaveOutcome } from '../../web/src/platform/desktop/bridge.ts';

export const RING_LINES = 500;
export const LINE_MAX_CHARS = 300;
export const BUNDLE_MAX_BYTES = 256 * 1024;
// The longest line the ring scrubs; a longer one is kept as a marker.
export const RECORD_MAX_CHARS = 512;

// The only labels main supplies to the header's per-key counts.
export type Refusal = 'sender' | 'destroyed' | 'frame' | 'subframe' | 'origin' | 'generation' | 'name' | 'rate' | 'size' | 'shape' | 'gesture' | 'disabled' | 'error';
// Why a feed was closed, for the log line and the header.
export type FeedEnding = 'unsubscribed' | 'document' | 'signed out' | 'server' | 'behind';
type FeedDiagnosticEvent = 'opening' | FeedEnding | 'socket closed' | 'socket destroyed' | 'close deadline' | 'invalid frame';
export type FeedDiagnosticKey = `${FeedKind} ${FeedDiagnosticEvent}`;

// Written in two parts: this file's source holds no absolute URL.
const SCHEMES = ['https', 'http', 'wss', 'ws', 'file', 'conch'];

// Strip format characters and invisible Hangul fillers so invisible splits
// cannot hide a key or interfere with path detection. Replace controls and
// Unicode line separators so a line cannot forge another one.
const FORMAT = /[\p{Cf}\u115f\u1160\u3164\uffa0]/gu;
const CONTROLS = /[\p{Cc}\p{Zl}\p{Zp}]/gu;

// The quoted body runs to end of line if its closing delimiter is missing.
// An apostrophe following a letter or number is usually a contraction.
// In a Windows path, a backslash immediately before the quote can end the path.
const QUOTES = /"(?:[a-zA-Z]:[\\/]|\\\\)[^"]*"?|"(?:\\.|[^"\\])*"?|(?<![\p{L}\p{N}])'(?:\\.|[^'\\])*'?|`(?:\\.|[^`\\])*`?|“[^”]*”?|‘[^’]*’?|«[^»]*»?|„[^“]*“?|「[^」]*」?/gu;

// Guess conservatively where a path ends. Unquoted paths can contain spaces,
// so keep only the lead-in; also accept a path immediately after ':' or '='.
const PATH = /(?:\b[a-z]:[\\/]|\\\\|(?<![\p{L}\p{N}_/])(?:~\/|\.{1,2}\/|\/[^/\s]|[\w.-]+\/[\w.-]+\/)).*/giu;

const RULES: readonly (readonly [RegExp, string | ((match: string) => string)])[] = [
  [QUOTES, '"[text]"'],
  // File URLs can themselves contain a path with spaces.
  [/\bfile:\/\/.*/giu, '[url]'],
  // Keep a bare scheme name used as a label in a fixed diagnostic.
  [new RegExp(`\\b(?:[a-z][a-z0-9+.-]*:\\/\\/|(?:${SCHEMES.join('|')}):)\\S+`, 'giu'), '[url]'],
  // A value can run across words and cookie delimiters.
  [/\b(?:bearer|basic)\s+\S.*/giu, '[redacted]'],
  [/\b[\w-]*(?:token|secret|password|passwd|passphrase|credential|authorization|cookie|ticket|api[ _-]?key|access[ _-]?key|private[ _-]?key|session[ _-]?id|invite[ _-]?code)[\w-]*\s*(?:[=:]|\bis\b)\s*\S.*/giu, '[redacted]'],
  [/\b(?:invite|pass|pwd|session|otp|pin|auth|sid|sig|nonce|room|channel|profile|user|name)\s*[=:]\s*\S.*/giu, '[redacted]'],
  [/\bcode\s*[=:]\s*\S.*/giu, '[redacted]'],
  [/\bkey\s*=\s*\S.*/giu, '[redacted]'],
  [/\b(?:the invite|joined room|identity)\s+\S.*/giu, '[redacted]'],
  // Credentials and e-mail must be removed before @names and hosts.
  [/[\p{L}\p{N}_.%+-]+:[^\s@]+@[\p{L}\p{N}.-]+(?::\d+)?|[\p{L}\p{N}_.%+-]+@[\p{L}\p{N}-]+(?:\.[\p{L}\p{N}-]+)+/gu, '[redacted]'],
  // A colon run with two separators can be IPv6, IPv6 plus a port or zone,
  // a MAC address or a fingerprint. Consume it whole, even if malformed.
  [/(?:(?<!\w)\[?[a-f\d]*:|(?<![a-f\d])\d[a-f\d]*:|(?=::):)[a-f\d:]*:[\da-f:.]*(?:%[\w.-]+)?(?:\](?::\d+)?)?(?:\/\d+)?/giu, '[address]'],
  // Include glued IPv4 addresses and preserve four-part build versions
  // whose numbers fall outside the IPv4 octet range.
  [/\d+(?:\.\d+){3}(?::\d+)?/gu, (match) => isIP(match.split(':', 1)[0] ?? '') === 4 ? '[address]' : match],
  [/\b0x[\da-f]{8}\b|\b\d{10}\b/giu, '[address]'],
  // A channel name can contain spaces. Keep its lead-in only.
  [/[#＃][\p{L}\p{N}\p{M}_.-]+.*/gu, '[name]'],
  [/[@＠][\p{L}\p{N}\p{M}_.-]+/gu, '[name]'],
  // Dotted hostnames can use any TLD, including Unicode and private TLDs.
  // A slash or query after a host can carry credentials.
  [/(?:[\p{L}\p{N}_-]+\.)+[\p{L}][\p{L}\p{N}_-]*(?::\d+)?(?:[/?#]\S*)?|\b[\p{L}][\p{L}\p{N}-]*:\d+\b(?:[/?#]\S*)?/gu, '[host]'],
  [PATH, '[path]'],
  // Runs that look like keys and the dot-delimited form used for tokens.
  [/[\w-]{8,}\.[\w-]{8,}\.[\w-]+|[A-Za-z0-9_+/=-]{22,}/gu, '[redacted]'],
];

// Whole fixed lines whose reviewed values resemble hosts, keys or addresses.
// Digit runs are bounded (a generation, an exit status, a count), so a long
// number can't ride through as an address or a key.
// Anchoring the complete shape keeps an adjoining payload subject to the scrub.
const FIXED_LINES = [
  /^conch-desktop: compat\.json is missing or malformed; no server release is supported$/u,
  /^conch-desktop: CONCH_LINUX_SHARE_AUDIO is on: the Linux screen share offers its sound \(experimental\)$/u,
  /^voice: helper \d{1,9} speaks conch\.voice_control\.v[123]$/u,
  /^voice: unavailable: runtime-dir-(?:unset|not-absolute|missing|symlink|not-directory|not-owned|not-private)$/u,
  /^voice: helper \d{1,9} exited \(code -?\d{1,10}\)$/u,
  /^voice: (?:config (?:set|set-mute-binding)|devices): (?:failed|unsupported) \(exit status -?\d{1,10}\)$/u,
  /^vault: temporarilyUnavailable$/u,
  /^conch-desktop: refused an IPC call \((?:sender|destroyed|frame|subframe|origin|generation|name|rate|size|shape|gesture|disabled|error)\); \d{1,15} so far$/u,
];

// Bound work before scrubbing and bound the result after redaction.
export function scrubLine(line: string): string {
  // Cutting first could shorten a key below the long-run rule's length.
  if (line.length > RECORD_MAX_CHARS) return `[a ${String(line.length)}-character line, not kept]`;
  let out = line.replace(FORMAT, '').replace(CONTROLS, ' ');
  if (out.length <= LINE_MAX_CHARS && FIXED_LINES.some((pattern) => pattern.test(out))) return out;
  for (const [pattern, replacement] of RULES) {
    out = out.replace(pattern, typeof replacement === 'string' ? () => replacement : replacement);
  }
  out = out.trim();
  // Avoid ending on half of a surrogate pair.
  return out.length > LINE_MAX_CHARS ? `${out.slice(0, LINE_MAX_CHARS - 1).replace(/[\uD800-\uDBFF]$/u, '')}…` : out;
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

const MAX_SAMPLE_KEYS = 64;

// Each key must come from a closed vocabulary. Export events 1, 2, 4, 8,
// etc. so repetition takes logarithmic space in the ring. Count events that
// the caller deliberately keeps silent, without using an export slot on them.
export class GeometricSampler<Key extends string> {
  readonly #counts = new Map<Key, { count: number; eligible: number; next: number }>();

  sample(key: Key, eligible = true): boolean {
    let state = this.#counts.get(key);
    if (state === undefined) {
      // Keep the original keys and refuse new ones at the limit. Evicting a
      // key would let it repeatedly re-enter and sample at its first event.
      if (this.#counts.size >= MAX_SAMPLE_KEYS) return false;
      state = { count: 0, eligible: 0, next: 1 };
      this.#counts.set(key, state);
    }
    state.count++;
    if (!eligible || ++state.eligible !== state.next) return false;
    state.next *= 2;
    return true;
  }

  counts(): readonly (readonly [Key, number])[] {
    return Array.from(this.#counts, ([key, { count }]) => [key, count] as const);
  }

  count(key: Key): number {
    return this.#counts.get(key)?.count ?? 0;
  }
}

// Call sites opt in only with fixed text, numbers or closed vocabularies.
export type MainLog = (line: string, options?: { readonly ring: true }) => void;

// Keep the sampler sequences and counts for the lifetime of the export
// logger, across profile feed and session replacements.
interface DiagnosticSamples {
  readonly feeds: GeometricSampler<FeedDiagnosticKey>;
  readonly blocked: GeometricSampler<'blocked'>;
}
const diagnosticSamples = new WeakMap<MainLog, DiagnosticSamples>();

export function diagnosticSamplesFor(log: MainLog): DiagnosticSamples | undefined {
  return diagnosticSamples.get(log);
}

export function diagnosticsLogger(ring: LogRing, writeLocal: (line: string) => void): MainLog {
  const log: MainLog = (line, options) => {
    if (options?.ring === true) ring.record(line);
    writeLocal(line);
  };
  diagnosticSamples.set(log, { feeds: new GeometricSampler(), blocked: new GeometricSampler() });
  return log;
}

export interface BundleInput {
  // This build's versions: names and version strings main itself read.
  readonly versions: Readonly<Record<string, string>>;
  readonly lines: readonly string[];
  readonly refusals: number;
  readonly refusalCounts?: readonly (readonly [Refusal, number])[];
  readonly feedCounts?: readonly (readonly [FeedDiagnosticKey, number])[];
  readonly blocked?: number;
  readonly now: Date;
}

function countSummary(counts: readonly (readonly [Refusal | FeedDiagnosticKey, number])[], total: number): string {
  const ordered = [...counts].sort(([a, n], [b, m]) => m - n || a.localeCompare(b));
  return ordered.length === 0 ? String(total) : `${String(total)} (${ordered.map(([key, count]) => `${key} ${String(count)}`).join(', ')})`;
}

// buildBundle is the text the person sees and may save. If the lines make it
// larger than BUNDLE_MAX_BYTES the oldest are left out, and it says how many.
export function buildBundle({ versions, lines, refusals, refusalCounts = [], feedCounts = [], blocked = 0, now }: BundleInput): string {
  const head = [
    'Conch desktop diagnostics',
    `made: ${now.toISOString()}`,
    ...Object.entries(versions).map(([name, value]) => `${scrubLine(name)}: ${scrubLine(value)}`),
    `IPC refusals since start: ${countSummary(refusalCounts, refusals)}`,
    `Feed events since start: ${countSummary(feedCounts, feedCounts.reduce((total, [, count]) => total + count, 0))}`,
    `Blocked requests since start: ${String(blocked)}`,
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
  readonly log: MainLog;
}

const SNAPSHOT_LIMIT = 4;

export class Diagnostics {
  readonly #deps: DiagnosticsDeps;
  // Insertion order lets a new preview evict the oldest snapshot.
  readonly #shown = new Map<string, string>();

  constructor(deps: DiagnosticsDeps) {
    this.#deps = deps;
  }

  // Hold the exact text returned with each main-generated ID, including the
  // sampler counts at the time of this preview.
  preview(refusals: number, refusalCounts: readonly (readonly [Refusal, number])[] = []): DiagnosticsPreview {
    const { versions, log, now } = this.#deps;
    const samples = diagnosticSamplesFor(log);
    const text = buildBundle({
      versions, lines: this.#deps.ring.lines(), refusals, refusalCounts,
      feedCounts: samples?.feeds.counts() ?? [], blocked: samples?.blocked.count('blocked') ?? 0, now: now(),
    });
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
      log('conch-desktop: the save dialog gave no usable path', { ring: true });
      return 'failed';
    }
    try {
      await writeFile(chosen, new TextEncoder().encode(text));
    } catch {
      log('conch-desktop: diagnostics could not be saved', { ring: true });
      return 'failed';
    }
    return 'saved';
  }
}
