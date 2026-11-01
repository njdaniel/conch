// External links (linux-desktop.md §3: "Only explicit user link actions open
// canonical https:/http: URLs in the OS browser; reject executable, custom and
// file: schemes and credentials in URLs. Links never inherit Conch
// authorisation"). Input: a URL the page asked to open (a click on a link that
// targets a new window, or window.open). Output: the OS browser opens the
// canonical URL, or nothing does and the person is told why.
//
// Main decides, from its own parse and its own record of the person's input;
// nothing the renderer checked is trusted. Only the URL goes to the OS: no
// header, cookie, token or referrer, and the app never fetches it. A window
// the page opens is never created, whatever the answer.
import { LINK_GESTURE_MS, type Gestures } from './gestures.ts';

export const MAX_LINK_LENGTH = 2048;

export type LinkRefusal = 'type' | 'length' | 'characters' | 'malformed' | 'scheme' | 'userinfo' | 'host';

export type LinkVerdict = { readonly ok: true; readonly url: string } | { readonly ok: false; readonly refusal: LinkRefusal };

// Whitespace, controls, and the invisible and direction-changing characters
// that make one URL read as another, as inclusive code point ranges.
const UNSAFE_RANGES: readonly (readonly [number, number])[] = [
  [0x0000, 0x0020],
  [0x007f, 0x00a0],
  [0x00ad, 0x00ad],
  [0x034f, 0x034f],
  [0x061c, 0x061c],
  [0x115f, 0x1160],
  [0x17b4, 0x17b5],
  [0x180b, 0x180f],
  [0x2000, 0x200f],
  [0x2028, 0x202f],
  [0x205f, 0x206f],
  [0x3000, 0x3000],
  [0x3164, 0x3164],
  [0xfe00, 0xfe0f],
  [0xfeff, 0xfeff],
  [0xffa0, 0xffa0],
  [0xfff9, 0xfffb],
  [0xe0000, 0xe0fff],
];

export function hasUnsafeCharacter(text: string): boolean {
  for (const char of text) {
    const code = char.codePointAt(0) ?? 0;
    if (UNSAFE_RANGES.some(([low, high]) => code >= low && code <= high)) return true;
  }
  return false;
}

// checkLink validates a URL the page asked to open and gives its canonical
// form, which is the only form ever handed to the OS.
export function checkLink(input: unknown): LinkVerdict {
  if (typeof input !== 'string') return { ok: false, refusal: 'type' };
  if (input.length === 0 || input.length > MAX_LINK_LENGTH) return { ok: false, refusal: 'length' };
  if (hasUnsafeCharacter(input)) return { ok: false, refusal: 'characters' };
  let url: URL;
  try {
    url = new URL(input);
  } catch {
    return { ok: false, refusal: 'malformed' };
  }
  if (url.protocol !== 'https:' && url.protocol !== 'http:') return { ok: false, refusal: 'scheme' };
  // Credentials, in any spelling the parser would drop or keep: look at the
  // text as written as well as at what it parsed to.
  const authority = /^[a-z][a-z0-9+.-]*:[/\\]*([^/\\?#]*)/iu.exec(input)?.[1] ?? '';
  if (url.username !== '' || url.password !== '' || authority.includes('@')) return { ok: false, refusal: 'userinfo' };
  // A host the person can read: ASCII, not percent-encoded, no punycode label
  // (the web client refuses the same; main does not rely on it).
  if (/[^\x00-\x7f]/u.test(authority) || authority.includes('%')) return { ok: false, refusal: 'host' };
  const host = url.hostname;
  if (host === '' || host.split('.').some((label) => label.toLowerCase().startsWith('xn--'))) return { ok: false, refusal: 'host' };
  const href = url.href;
  if (href.length > MAX_LINK_LENGTH) return { ok: false, refusal: 'length' };
  return { ok: true, url: href };
}

// What the person is told, in words that do not repeat the link.
export function refusalMessage(refusal: LinkRefusal): string {
  switch (refusal) {
    case 'scheme':
      return 'Conch only opens web addresses that start with https:// or http://.';
    case 'userinfo':
      return 'Conch does not open links that contain a user name or password.';
    case 'host':
      return 'Conch does not open links whose address is written with hidden or look-alike characters.';
    case 'length':
      return 'That link is too long.';
    default:
      return 'That link is not a web address Conch can open.';
  }
}

export interface LinkDeps {
  readonly gestures: Gestures;
  // The one web contents that is the app's window.
  readonly owned: () => object | null;
  readonly openExternal: (url: string) => Promise<void>;
  // Asks the person, in a native box of main's own, whether to open this
  // link: its host and its whole address. True only for "Open".
  readonly confirm: (host: string, url: string) => Promise<boolean>;
  // Tells the person a link was refused. Never given the link.
  readonly tell: (message: string) => void;
  readonly log: (line: string) => void;
  readonly now?: () => number;
}

// At most this many acted-on links a minute, whatever they come to, so that a
// run of refused ones cannot become a storm of message boxes.
export const LINKS_PER_MINUTE = 10;

export class Links {
  readonly #deps: LinkDeps;
  #windowStart = 0;
  #count = 0;

  constructor(deps: LinkDeps) {
    this.#deps = deps;
  }

  // fromWindow answers a page's request for a new window at url. The window is
  // never made (the caller denies it); the URL opens in the OS browser only
  // when it came from the owned window right after the person's own input, had
  // no form data, and passes checkLink. Returns what happened, for tests.
  fromWindow(contents: object, url: string, hasPostBody: boolean): 'opened' | 'confirming' | 'refused' | 'ignored' {
    const { gestures, owned, log } = this.#deps;
    if (contents !== owned()) {
      log('conch-desktop: refused a new window (not the app window)');
      return 'ignored';
    }
    // The act is used up by this request, whatever the answer.
    const kind = gestures.take(contents, LINK_GESTURE_MS);
    if (hasPostBody || kind === null) {
      // A script's window.open, or a form: nothing the person did, so nothing
      // to answer.
      log('conch-desktop: refused a new window (no user gesture)');
      return 'ignored';
    }
    const now = (this.#deps.now ?? Date.now)();
    if (now - this.#windowStart >= 60_000) {
      this.#windowStart = now;
      this.#count = 0;
    }
    if (++this.#count > LINKS_PER_MINUTE) {
      log('conch-desktop: refused a link (too many)');
      // Once for the minute, so the person is not left wondering and the
      // limit is not itself a storm of message boxes.
      if (this.#count === LINKS_PER_MINUTE + 1) this.#deps.tell('Too many links were opened at once. Try again in a minute.');
      return 'ignored';
    }
    const verdict = checkLink(url);
    if (!verdict.ok) {
      log(`conch-desktop: refused a link (${verdict.refusal})`);
      this.#deps.tell(refusalMessage(verdict.refusal));
      return 'refused';
    }
    const open = (): void => {
      this.#deps.openExternal(verdict.url).catch(() => {
        log('conch-desktop: the system could not open a link');
      });
    };
    // A click names the link the person clicked. Enter and Space are also
    // what sending a message takes, so a link a key "opened" is first shown to
    // them, and opens only if they say so.
    if (kind === 'key') {
      log('conch-desktop: confirming a link opened with the keyboard');
      this.#deps.confirm(new URL(verdict.url).host, verdict.url).then(
        (yes) => {
          if (yes) open();
          else log('conch-desktop: a link was not opened (declined)');
        },
        () => {
          log('conch-desktop: a link was not opened (no answer)');
        },
      );
      return 'confirming';
    }
    open();
    return 'opened';
  }
}
