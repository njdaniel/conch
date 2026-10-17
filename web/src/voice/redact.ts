// Keeps the join token and the room name out of anything the SDK says. The SDK
// logs the room name at info and debug and may carry a token in a URL; before a
// record is kept, the token and room name in use and anything shaped like a
// signed token are replaced with a placeholder.
export const placeholder = '[redacted]';

// A signed token is three base64url parts joined by dots, the first starting
// with eyJ (a JSON header).
const signedToken = /eyJ[\w-]+\.[\w-]+\.[\w-]*/g;

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

  // any redacts every string inside a value that is logged as context.
  any(value: unknown, depth = 0): unknown {
    if (typeof value === 'string') return this.text(value);
    if (depth > 4 || value === null || typeof value !== 'object') return value;
    if (Array.isArray(value)) return value.map((v) => this.any(v, depth + 1));
    const out: Record<string, unknown> = {};
    for (const [k, v] of Object.entries(value)) out[k] = this.any(v, depth + 1);
    return out;
  }
}

// A bounded, redacted record of what the SDK warned about. Nothing goes to the
// console: the page makes no console call.
export class LogRing {
  private readonly lines: string[] = [];
  private readonly redactor: Redactor;
  private readonly size: number;

  constructor(redactor: Redactor, size = 20) {
    this.redactor = redactor;
    this.size = size;
  }

  add(level: string, message: string, context?: unknown): void {
    const extra = context === undefined ? '' : ` ${safeJson(this.redactor.any(context))}`;
    this.lines.push(`${level}: ${this.redactor.text(message)}${this.redactor.text(extra)}`);
    if (this.lines.length > this.size) this.lines.shift();
  }

  recent(): readonly string[] {
    return [...this.lines];
  }
}

function safeJson(value: unknown): string {
  try {
    return JSON.stringify(value);
  } catch {
    return '';
  }
}
