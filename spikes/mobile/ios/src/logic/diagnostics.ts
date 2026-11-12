// Copied from spikes/mobile/android at cc5fab02 (#1390, M4-B, with its review fixes) for the iOS spike (#1394, M5-B).
// The diagnostics export (#1109): a bounded, redacted record that holds no
// bearer, no address and no device identifier. Built on the browser's
// Redactor (redact.ts), which hides registered secrets and signed tokens; this
// adds the shapes a phone's log carries that the browser's does not.
import { LogRing, Redactor, placeholder } from './redact';

const patterns: readonly RegExp[] = [
  // An Authorization header's value.
  /\bBearer\s+[\w.~+/=-]+/gi,
  // A ticket or token in a query.
  /([?&](?:ticket|token|access_token)=)[^&\s"']+/gi,
  // A URL of any scheme the app speaks.
  /\b(?:https?|wss?):\/\/[^\s"'<>]+/gi,
  // An IPv4 address, with or without a port.
  /\b\d{1,3}(?:\.\d{1,3}){3}(?::\d{1,5})?\b/g,
  // A bracketed or bare IPv6 address.
  /\[?\b(?:[0-9a-f]{1,4}:){2,7}[0-9a-f]{1,4}\]?/gi,
  // A compressed IPv6 address, with "::" (2001:db8::1, fe80::1c2a:3b4c, ::1).
  /\[?(?<![\w:])(?:[0-9a-f]{1,4}(?::[0-9a-f]{1,4}){0,6})?::(?:[0-9a-f]{1,4}(?::[0-9a-f]{1,4}){0,6})?(?![\w:])\]?/gi,
  // A host name with a dot and a well-known last label (server.example.com,
  // conch.example.co.uk). A name the list misses is caught only when held.
  /\b(?:[a-z0-9-]+\.)+(?:com|net|org|io|dev|app|local|lan|home|internal|test|me|co|uk|de|fr|eu|nl|se|ca|au|us|cloud|xyz|info|biz)\b(?::\d{1,5})?/gi,
  // A UUID (also the iOS vendor identifier), the Android ID (16 hex digits), a MAC address.
  /\b[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}\b/gi,
  /\b[0-9a-f]{16}\b/gi,
  /\b(?:[0-9a-f]{2}:){5}[0-9a-f]{2}\b/gi,
];

// scrub replaces everything shaped like a secret, an address or a device id.
export function scrub(input: string): string {
  let out = input;
  for (const p of patterns) {
    out = out.replace(p, (match: string, prefix: unknown) => (typeof prefix === 'string' && match.startsWith(prefix) ? prefix + placeholder : placeholder));
  }
  return out;
}

export class Diagnostics {
  private readonly redactor = new Redactor();
  private readonly ring: LogRing;
  private readonly owner = {};

  constructor(size = 50) {
    this.ring = new LogRing(this.redactor, size);
  }

  // hold registers values that must never appear (the bearer, the server
  // address, the device id), whatever shape they take.
  hold(...secrets: string[]): void {
    this.redactor.hold(this.owner, ...secrets);
  }

  add(level: string, message: string, context?: unknown): void {
    this.ring.add(level, message, context);
  }

  // export is the text the person may share: every line scrubbed once more.
  export(): string {
    return this.ring.recent().map(scrub).join('\n');
  }
}
