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
// conch://invite?server=<origin>&code=<code>) hands to the same router.
import { randomBytes } from 'node:crypto';

import type { InviteAccept, InviteCheck, InviteRefusal, InviteTarget, ProfileView } from '../../web/src/platform/desktop/bridge.ts';
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
// What may be written after #/invite/: unreserved characters and escapes.
const RAW_CODE = /^[A-Za-z0-9._~%-]+$/u;
// What the code is once decoded: URL-safe characters only, so a decoded
// slash, space, control or invisible character is a refusal.
const CODE = /^[A-Za-z0-9._~-]{1,256}$/u;

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
}

interface Staged {
  readonly profileId: string;
  readonly code: string;
  readonly expiresAt: number;
}

type AcceptRefusal = Extract<InviteAccept, { ok: false }>['refusal'];
const ADD_REFUSALS: readonly AcceptRefusal[] = ['full', 'unreachable', 'certificate', 'not-conch'];

export class Invites {
  readonly #deps: InvitesDeps;
  readonly #pending = new Map<string, Pending>();
  #staged: Staged | null = null;

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
    const now = this.#deps.now();
    for (const [handle, p] of this.#pending) if (p.expiresAt <= now) this.#pending.delete(handle);
    while (this.#pending.size >= MAX_PENDING) {
      const oldest = this.#pending.keys().next();
      if (oldest.done === true) break;
      this.#pending.delete(oldest.value);
    }
    const handle = (this.#deps.newHandle ?? defaultHandle)();
    this.#pending.set(handle, { origin: parsed.origin, code: parsed.code, expiresAt: now + PENDING_MS });
    return {
      ok: true,
      handle,
      origin: parsed.origin,
      host: new URL(parsed.origin).host,
      profiles: this.#deps.profiles.findByOrigin(parsed.origin).map(({ id, label, account, active }) => ({ id, label, account, active })),
      voice: this.#deps.voiceRunning(),
    };
  }

  cancel(handle: string): boolean {
    return this.#pending.delete(handle);
  }

  // accept is the person's click. The handle is spent whatever the answer. The
  // code is staged for the chosen profile before the switch begins, so the
  // page that switch creates can take it; the profile is switched to by the
  // existing sequence, which leaves voice first.
  async accept(handle: string, target: InviteTarget): Promise<InviteAccept> {
    const pending = this.#pending.get(handle);
    this.#pending.delete(handle);
    this.#staged = null;
    if (pending === undefined || pending.expiresAt <= this.#deps.now()) return { ok: false, refusal: 'expired' };
    let profileId: string;
    let switched = true;
    if ('profile' in target) {
      const chosen = this.#deps.profiles.findByOrigin(pending.origin).find((p) => p.id === target.profile);
      if (chosen === undefined) return { ok: false, refusal: 'unknown-profile' };
      profileId = chosen.id;
      switched = !chosen.active;
    } else {
      const added = await this.#deps.profiles.add(pending.origin);
      if (!added.ok) {
        const known = ADD_REFUSALS.find((r) => r === added.refusal);
        return { ok: false, refusal: known ?? 'unsupported' };
      }
      profileId = added.id;
    }
    this.#staged = { profileId, code: pending.code, expiresAt: this.#deps.now() + STAGED_MS };
    if (switched && !(await this.#deps.profiles.switchTo(profileId))) {
      this.#staged = null;
      return { ok: false, refusal: 'unsupported' };
    }
    return { ok: true, switched };
  }

  // take is the one place the code leaves. It answers it once, to the profile
  // it was staged for while that profile is in force, and nothing otherwise;
  // a take from any other profile, or after the time, spends it unanswered.
  take(): string | null {
    const staged = this.#staged;
    this.#staged = null;
    if (staged === null || staged.expiresAt <= this.#deps.now()) return null;
    return this.#deps.profiles.view().active === staged.profileId ? staged.code : null;
  }
}

function defaultHandle(): string {
  return `invite-${randomBytes(16).toString('hex')}`;
}
