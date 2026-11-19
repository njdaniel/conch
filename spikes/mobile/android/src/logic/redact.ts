// Copied from web/src/voice/redact.ts at ed4a68f9 and adapted for the phone (#1109, M4-B).
// Keeps the join token and the room name out of anything the SDK says. The SDK
// logs the room name at info and debug and may carry a token in a URL; before a
// record is kept, the token and room name in use and anything shaped like a
// signed token are replaced with a placeholder. The phone also masks voice
// identifiers from structured logger context.
export const placeholder = '[redacted]';

// A signed token is three base64url parts joined by dots, the first starting
// with eyJ (a JSON header).
const signedToken = /eyJ[\w-]+\.[\w-]+\.[\w-]*/g;

// LiveKit uses these keys for identities and SIDs in logger context. Match the
// keys as well as the shapes in diagnostics.ts so opaque values are hidden.
const voiceContextKey = /^(?:room(?:name|id|sid)?|(?:remote|local)?participant(?:identity|id|sid)?|identity)$/i;

export class Redactor {
  // The values to hide, by the connection that registered them. Each
  // connection releases only its own, so a late release by an old connection
  // cannot unhide a new one's.
  private readonly held = new Map<object, readonly string[]>();

  // hold registers values to hide for owner, adding to what owner and every
  // other owner already hold. A value shorter than 4 characters is not
  // registered, since replacing it would mangle ordinary text.
  hold(owner: object, ...values: string[]): void {
    const mine = this.held.get(owner) ?? [];
    this.held.set(owner, [...mine, ...values.filter((v) => v.length >= 4)]);
  }

  // release forgets the values owner registered, and no one else's.
  release(owner: object): void {
    this.held.delete(owner);
  }

  // The longest first, so a value that contains another is hidden whole.
  private secrets(): string[] {
    return [...new Set([...this.held.values()].flat())].sort((a, b) => b.length - a.length);
  }

  text(input: string): string {
    let out = input.replace(signedToken, placeholder);
    for (const s of this.secrets()) out = out.split(s).join(placeholder);
    return out;
  }

  // json redacts context at every depth. Scrub string values before
  // serialization so JSON escapes cannot hide identifier boundaries.
  // A cyclic or otherwise unserializable context is omitted.
  json(context: unknown, scrub: (line: string) => string): string | undefined {
    try {
      return JSON.stringify(context, (key, value: unknown) => {
        if (value !== undefined && voiceContextKey.test(key)) return placeholder;
        return typeof value === 'string' ? scrub(this.text(value)) : value;
      });
    } catch {
      return undefined;
    }
  }
}

// A bounded, redacted record of what the SDK warned about. Nothing goes to the
// console: the page makes no console call.
export class LogRing {
  private readonly lines: string[] = [];
  private readonly redactor: Redactor;
  private readonly size: number;
  private readonly scrub: (line: string) => string;

  constructor(redactor: Redactor, size: number, scrub: (line: string) => string) {
    this.redactor = redactor;
    this.size = size;
    this.scrub = scrub;
  }

  add(level: string, message: string, context?: unknown): void {
    const json = this.redactor.json(context, this.scrub);
    const extra = json === undefined ? '' : ` ${json}`;
    this.lines.push(this.scrub(this.redactor.text(`${level}: ${message}${extra}`)));
    if (this.lines.length > this.size) this.lines.shift();
  }

  recent(): readonly string[] {
    return [...this.lines];
  }
}
