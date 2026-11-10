// Opening an invite link in the app (issue #1591, D5-05 of #773). Input: the
// text a person pasted. Output: the server it names (canonical, for the
// person to read before anything is contacted), the saved profiles for that
// server, and, after the person's click, the invite code handed once to the
// page of the profile it was for.
//
// An invite link is untrusted input and its code is a credential. Main parses
// (the renderer's parse is not trusted); the code lives in main's memory
// behind a one-time handle and is returned by exactly one function,
// take(), once, and only to the profile it was staged for. It is never logged
// (log lines here are fixed words), never written to disk and never part of an
// answer to the page but that one. Nothing is contacted, added or switched
// until accept(), which the IPC takes only right after the person's own input.
// Nothing here joins voice: a switch tears media down first
// (profiles/switch.ts), and the invite views join only on their own button.
//
// The parsed shape (origin, code) is what the link scheme of #1592 (D5-06,
// conch://invite?server=<origin>&code=<code>; linux-desktop.md section 9)
// hands to the same router. A scheme URL reaches the app as an argument, at a
// first launch or in a second one (the single-instance lock); schemeLinkFrom
// picks at most one such argument, parseSchemeLink maps it onto
// parseInviteLink's rules, and arrive() keeps it behind nothing but a timer
// until the page asks (arrived()), when it goes through exactly the checks of
// a pasted link and the same choice of profile. Nothing is contacted, added
// or switched by a URL, and an argument that is not one well-formed URL of the
// scheme is ignored, a flag never read as one. argv is never logged.
import { randomBytes } from 'node:crypto';

import type { DocumentGate } from './document.ts';
import type { InviteAccept, InviteCheck, InviteRefusal, InviteTarget, ProfileView } from '../../web/src/platform/desktop/bridge.ts';
import { INVITE_ARRIVED, INVITE_EVENTS } from '../shared/bridge-names.ts';
import { parseServerAddress } from './api/origin.ts';
import { hasUnsafeCharacter } from './links.ts';

export const MAX_INVITE_LINK_LENGTH = 2048;
export const MAX_INVITE_CODE_LENGTH = 256;

// How long a checked link waits for the person's click, and how long a code
// staged by that click waits for the page it was for. Both are short: a code
// is not kept for a person who walked away.
export const PENDING_MS = 5 * 60_000;
export const STAGED_MS = 2 * 60_000;
const MAX_PENDING = 4;

export type InviteParse = { readonly ok: true; readonly origin: string; readonly code: string } | { readonly ok: false; readonly refusal: InviteRefusal };

const refuse = (refusal: InviteRefusal): InviteParse => ({ ok: false, refusal });

// A host a person can read: letters, digits, dots and hyphens, or a bracketed
// IPv6 literal, with an optional port. No percent escapes, no non-ASCII.
const AUTHORITY = /^(?:[A-Za-z0-9.-]+|\[[0-9A-Fa-f:.]+\])(?::[0-9]{1,5})?$/u;
const MAX_HOST_LENGTH = 253;
// One DNS label: letters, digits and inner hyphens, 1 to 63 long. An empty
// label (a trailing dot, "a..b") does not match.
const LABEL = /^[a-z0-9](?:[a-z0-9-]{0,61}[a-z0-9])?$/u;
// What may be written after #/invite/: unreserved characters and escapes.
const RAW_CODE = /^[A-Za-z0-9._~%-]+$/u;
// What the code is once decoded: URL-safe characters only, so a decoded
// slash, space, control or invisible character is a refusal.
const CODE = /^[A-Za-z0-9._~-]{1,256}$/u;

// hostAsWritten: the canonical origin's authority is the lower-cased one that
// was written, apart from a default port that the canonical form drops. Also
// refused: an empty or ill-formed label (which includes a trailing dot), port
// 0, and a host over 253 characters.
function hostAsWritten(authority: string, scheme: string, origin: string): boolean {
  const written = authority.toLowerCase();
  const canonical = new URL(origin).host;
  const defaultPort = scheme === 'https' ? ':443' : ':80';
  if (canonical !== written && `${canonical}${defaultPort}` !== written) return false;
  const portAt = written.lastIndexOf(':');
  const bracketed = written.startsWith('[');
  const port = portAt > (bracketed ? written.indexOf(']') : -1) ? written.slice(portAt + 1) : '';
  if (port !== '' && Number(port) === 0) return false;
  const host = port === '' ? written : written.slice(0, portAt);
  if (host.length > MAX_HOST_LENGTH) return false;
  if (bracketed) return true;
  return host.split('.').every((label) => LABEL.test(label));
}

// parseInviteLink accepts exactly <origin>/app/#/invite/<code> (the form of
// inviteLink in web/src/route.ts): https, or http to loopback; the origin as
// api/origin.ts canonicalises it; the path /app/; no query; the fragment
// #/invite/ and a code of 1 to 256 URL-safe characters. Anything else is one
// refusal naming the rule, never repeating the input.
export function parseInviteLink(input: unknown): InviteParse {
  if (typeof input !== 'string') return refuse('type');
  if (input.length === 0 || input.length > MAX_INVITE_LINK_LENGTH) return refuse('length');
  // Pasting often brings a trailing newline or space; only the ends are
  // trimmed, and nothing inside is repaired.
  const text = input.trim();
  if (text === '') return refuse('length');
  if (hasUnsafeCharacter(text)) return refuse('characters');
  const scheme = /^([A-Za-z][A-Za-z0-9+.-]*):/u.exec(text)?.[1]?.toLowerCase();
  if (scheme === undefined) return refuse('malformed');
  if (scheme !== 'https' && scheme !== 'http') return refuse('scheme');
  if (text.includes('\\')) return refuse('malformed');
  const split = /^[A-Za-z]+:\/\/([^/?#]*)(.*)$/su.exec(text);
  if (split === null) return refuse('malformed');
  const authority = split[1] ?? '';
  const rest = split[2] ?? '';
  if (authority.includes('@')) return refuse('userinfo');
  // Punycode labels and escapes make one host read as another.
  if (!AUTHORITY.test(authority) || authority.split('.').some((label) => label.toLowerCase().startsWith('xn--'))) return refuse('host');
  const server = parseServerAddress(`${scheme}://${authority}`);
  if (!server.ok) return refuse(server.refusal === 'scheme' ? 'scheme' : 'host');
  // The host must be the one written: the URL parser also reads 2130706433,
  // 0x7f.0.0.1 and 127.1 as 127.0.0.1, and a person reading the server's
  // name must not be shown one host while another is contacted.
  if (!hostAsWritten(authority, scheme, server.origin)) return refuse('host');
  if (!rest.startsWith('/app/')) return refuse('path');
  const tail = rest.slice('/app/'.length);
  const hashAt = tail.indexOf('#');
  const before = hashAt < 0 ? tail : tail.slice(0, hashAt);
  if (before.includes('?')) return refuse('query');
  if (before !== '') return refuse('path');
  if (hashAt < 0 || !tail.slice(hashAt).startsWith('#/invite/')) return refuse('fragment');
  const raw = tail.slice(hashAt + '#/invite/'.length);
  if (raw === '' || !RAW_CODE.test(raw)) return refuse('code');
  let code: string;
  try {
    code = decodeURIComponent(raw);
  } catch {
    return refuse('code');
  }
  if (!CODE.test(code)) return refuse('code');
  return { ok: true, origin: server.origin, code };
}

// What a scheme URL may be, once decoded: the values are percent-encoded and
// nothing else, so a raw colon, slash, plus, space or non-ASCII character in
// the query is a refusal.
const QUERY_VALUE = /^[A-Za-z0-9._~%-]*$/u;
// The server value, decoded, is an origin and only an origin. parseInviteLink
// then applies every host rule of a pasted link to it.
const SERVER_ORIGIN = /^https?:\/\/[^/?#@\\\s]+$/iu;
// The most arguments looked at, and the scheme's name.
const MAX_ARGV = 64;
const SCHEME = 'conch:';

// schemeLinkFrom picks the one argument that is a link of the scheme: the
// first that is a string, does not start with "-" (a switch, however it
// continues), and starts with "conch:" in any case. Later ones are not
// considered, whether or not the first parses. Index 0 is the executable.
export function schemeLinkFrom(argv: unknown): string | null {
  if (!Array.isArray(argv)) return null;
  for (const arg of argv.slice(1, MAX_ARGV) as unknown[]) {
    if (typeof arg !== 'string' || arg.startsWith('-')) continue;
    if (arg.slice(0, SCHEME.length).toLowerCase() === SCHEME) return arg;
  }
  return null;
}

// parseSchemeLink accepts exactly conch://invite?server=<percent-encoded
// origin>&code=<code>, the two parameters once each in either order, and then
// holds the result to parseInviteLink's rules by building the https link it
// names. Anything else is one refusal naming the rule, never repeating the
// input. No trimming and no repair: the text is one argument as given.
export function parseSchemeLink(input: unknown): InviteParse {
  if (typeof input !== 'string') return refuse('type');
  if (input.length === 0 || input.length > MAX_INVITE_LINK_LENGTH) return refuse('length');
  if (hasUnsafeCharacter(input) || /[\s\\]/u.test(input)) return refuse('characters');
  if (input.slice(0, SCHEME.length).toLowerCase() !== SCHEME) return refuse('scheme');
  const rest = input.slice(SCHEME.length);
  if (!rest.startsWith('//invite?')) return refuse('malformed');
  const query = rest.slice('//invite?'.length);
  if (query.includes('#')) return refuse('fragment');
  const parts = query.split('&');
  if (parts.length !== 2) return refuse('malformed');
  const values = new Map<string, string>();
  for (const part of parts) {
    const eq = part.indexOf('=');
    if (eq < 0 || part.indexOf('=', eq + 1) >= 0) return refuse('malformed');
    const name = part.slice(0, eq);
    const value = part.slice(eq + 1);
    if ((name !== 'server' && name !== 'code') || values.has(name)) return refuse('malformed');
    if (value === '' || !QUERY_VALUE.test(value)) return refuse(name === 'server' ? 'host' : 'code');
    values.set(name, value);
  }
  let server: string;
  let code: string;
  try {
    server = decodeURIComponent(values.get('server') ?? '');
    code = decodeURIComponent(values.get('code') ?? '');
  } catch {
    return refuse('malformed');
  }
  if (!SERVER_ORIGIN.test(server)) return refuse('host');
  if (!CODE.test(code)) return refuse('code');
  return parseInviteLink(`${server}/app/#/invite/${encodeURIComponent(code)}`);
}

export interface InvitesDeps {
  readonly profiles: {
    findByOrigin(origin: string): readonly ProfileView[];
    view(): { readonly active: string | null };
    add(address: string): Promise<{ readonly ok: true; readonly id: string } | { readonly ok: false; readonly refusal: string }>;
    switchTo(id: string | null): Promise<boolean>;
  };
  // Whether native voice is starting or running, in whatever profile.
  readonly voiceRunning: () => boolean;
  readonly now: () => number;
  // A fixed event. Never a link, a code or an origin.
  readonly log: (line: string) => void;
  readonly newHandle?: () => string;
}

interface Pending {
  readonly origin: string;
  readonly code: string;
  readonly expiresAt: number;
  // Whether the person was told voice was running when they were shown this
  // link; accept compares it with voice now.
  readonly voice: boolean;
  readonly timer: ReturnType<typeof setTimeout>;
}

interface Staged {
  readonly profileId: string;
  readonly code: string;
  readonly expiresAt: number;
}

type AcceptRefusal = Extract<InviteAccept, { ok: false }>['refusal'];
const ADD_REFUSALS: readonly AcceptRefusal[] = ['full', 'unreachable', 'certificate', 'not-conch'];

interface Arrival {
  readonly origin: string;
  readonly code: string;
  readonly timer: ReturnType<typeof setTimeout>;
}

export class Invites {
  readonly #deps: InvitesDeps;
  readonly #pending = new Map<string, Pending>();
  // The one scheme link waiting for the page to ask for it; a newer link
  // replaces it.
  #arrival: Arrival | null = null;
  #staged: Staged | null = null;
  #stagedTimer: ReturnType<typeof setTimeout> | null = null;

  constructor(deps: InvitesDeps) {
    this.#deps = deps;
  }

  // check parses a pasted link and, if it is an invite link, keeps its code
  // behind a handle and answers the server it names and the profiles saved
  // for it. It contacts nothing.
  check(link: unknown): InviteCheck {
    const parsed = parseInviteLink(link);
    if (!parsed.ok) {
      this.#deps.log(`conch-desktop: refused an invite link (${parsed.refusal})`);
      return { ok: false, refusal: parsed.refusal };
    }
    return this.#register(parsed);
  }

  // arrive is a launch's arguments: a first launch's, or a second one's while
  // this process holds the lock. It keeps the one scheme link in them, if it
  // is well-formed, until arrived() takes it (or PENDING_MS passes). It
  // contacts nothing and answers whether a link was kept, which is the cue to
  // tell the page.
  arrive(argv: unknown): boolean {
    const link = schemeLinkFrom(argv);
    if (link === null) return false;
    const parsed = parseSchemeLink(link);
    if (!parsed.ok) {
      this.#deps.log(`conch-desktop: refused a conch: link (${parsed.refusal})`);
      return false;
    }
    this.#dropArrival();
    const timer = setTimeout(() => {
      this.#arrival = null;
    }, PENDING_MS);
    timer.unref();
    this.#arrival = { origin: parsed.origin, code: parsed.code, timer };
    this.#deps.log('conch-desktop: a conch: invite link arrived');
    return true;
  }

  // arrived hands the page what arrive kept, once, as a check of a pasted
  // link is handed: a handle, the server and its profiles, never the code.
  arrived(): InviteCheck | null {
    const kept = this.#arrival;
    this.#dropArrival();
    if (kept === null) return null;
    return this.#register({ origin: kept.origin, code: kept.code });
  }

  #dropArrival(): void {
    if (this.#arrival !== null) clearTimeout(this.#arrival.timer);
    this.#arrival = null;
  }

  // #register keeps a parsed link's code behind a new handle and answers the
  // choice shown to the person.
  #register(parsed: { readonly origin: string; readonly code: string }): InviteCheck {
    const now = this.#deps.now();
    for (const [handle, p] of this.#pending) if (p.expiresAt <= now) this.#spend(handle);
    while (this.#pending.size >= MAX_PENDING) {
      const oldest = this.#pending.keys().next();
      if (oldest.done === true) break;
      this.#spend(oldest.value);
    }
    const handle = (this.#deps.newHandle ?? defaultHandle)();
    const voice = this.#deps.voiceRunning();
    // The code is dropped at its time even if nothing else happens: an unref'd
    // timer, so it never keeps the app alive.
    const timer = setTimeout(() => {
      this.#pending.delete(handle);
    }, PENDING_MS);
    timer.unref();
    this.#pending.set(handle, { origin: parsed.origin, code: parsed.code, expiresAt: now + PENDING_MS, voice, timer });
    return {
      ok: true,
      handle,
      origin: parsed.origin,
      host: new URL(parsed.origin).host,
      profiles: this.#deps.profiles.findByOrigin(parsed.origin).map(({ id, label, account, active }) => ({ id, label, account, active })),
      voice,
    };
  }

  cancel(handle: string): boolean {
    return this.#spend(handle);
  }

  // #spend forgets a checked link and its timer.
  #spend(handle: string): boolean {
    const p = this.#pending.get(handle);
    if (p === undefined) return false;
    clearTimeout(p.timer);
    return this.#pending.delete(handle);
  }

  #clearStaged(): void {
    if (this.#stagedTimer !== null) clearTimeout(this.#stagedTimer);
    this.#stagedTimer = null;
    this.#staged = null;
  }

  // accept is the person's click. The handle is spent whatever the answer. The
  // code is staged for the chosen profile before the switch begins, so the
  // page that switch creates can take it; the profile is switched to by the
  // existing sequence, which leaves voice first.
  async accept(handle: string, target: InviteTarget): Promise<InviteAccept> {
    const pending = this.#pending.get(handle);
    this.#spend(handle);
    this.#clearStaged();
    if (pending === undefined || pending.expiresAt <= this.#deps.now()) return { ok: false, refusal: 'expired' };
    let profileId: string;
    let switched = true;
    if ('profile' in target) {
      const chosen = this.#deps.profiles.findByOrigin(pending.origin).find((p) => p.id === target.profile);
      if (chosen === undefined) return { ok: false, refusal: 'unknown-profile' };
      profileId = chosen.id;
      switched = !chosen.active;
      if (switched && !pending.voice && this.#deps.voiceRunning()) return { ok: false, refusal: 'voice-started' };
    } else {
      // Adding always switches.
      if (!pending.voice && this.#deps.voiceRunning()) return { ok: false, refusal: 'voice-started' };
      const added = await this.#deps.profiles.add(pending.origin);
      if (!added.ok) {
        const known = ADD_REFUSALS.find((r) => r === added.refusal);
        return { ok: false, refusal: known ?? 'unsupported' };
      }
      profileId = added.id;
    }
    const staged: Staged = { profileId, code: pending.code, expiresAt: this.#deps.now() + STAGED_MS };
    this.#staged = staged;
    // The code is dropped at its time even if no page ever takes it.
    const timer = setTimeout(() => {
      if (this.#staged === staged) this.#clearStaged();
    }, STAGED_MS);
    timer.unref();
    this.#stagedTimer = timer;
    let done = false;
    try {
      if (switched && !(await this.#deps.profiles.switchTo(profileId))) return { ok: false, refusal: 'unsupported' };
      done = true;
      return { ok: true, switched };
    } finally {
      // A refused or thrown switch leaves no code behind.
      if (!done && this.#staged === staged) this.#clearStaged();
    }
  }

  // take is the one place the code leaves. It answers it once, to the profile
  // it was staged for while that profile is in force, and nothing otherwise;
  // a take from any other profile, or after the time, spends it unanswered.
  take(): string | null {
    const staged = this.#staged;
    this.#clearStaged();
    if (staged === null || staged.expiresAt <= this.#deps.now()) return null;
    return this.#deps.profiles.view().active === staged.profileId ? staged.code : null;
  }
}

function defaultHandle(): string {
  return `invite-${randomBytes(16).toString('hex')}`;
}

export interface InviteArrivalTarget {
  // The owned web contents' send, or null when there is no live window.
  readonly contents: () => { send(channel: string, message: typeof INVITE_ARRIVED): void } | null;
  readonly gate: () => DocumentGate;
}

// deliverInvite tells the owned page, once its current document has committed,
// that a scheme link is waiting. The message holds nothing of the link. A page
// that is not there yet asks for it when it mounts.
export function deliverInvite(target: InviteArrivalTarget): void {
  const contents = target.contents();
  const gate = target.gate();
  if (contents === null || !gate.shows(gate.generation)) return;
  contents.send(INVITE_EVENTS, INVITE_ARRIVED);
}
