// Hash routing by hand: #/c/<channel> names the open channel. The hash is per
// tab and survives a reload, and a link to it carries no credential.
// #/invite/<code> is an invite link and #/signin/<code> a sign-in link, each
// taken out of the address as soon as the page reads it.

export function channelFromHash(hash: string): string | null {
  const match = /^#\/c\/(.+)$/.exec(hash);
  if (match?.[1] === undefined) return null;
  try {
    return decodeURIComponent(match[1]);
  } catch {
    return null;
  }
}

export function hashForChannel(name: string): string {
  return `#/c/${encodeURIComponent(name)}`;
}

// Invite links (issue #440): <origin>/app/#/invite/<code>. The code is in the
// fragment, which no server, proxy log or Referer sees. It is handled like a
// credential: read into memory, never stored, logged or put in the DOM.

// inviteFromHash is the code of an invite link. One whose escapes do not
// decode is handed on as it is: the server answers it as it answers any code
// that is not an invite, with the one "no longer works".
export function inviteFromHash(hash: string): string | null {
  const match = /^#\/invite\/([^/]+)$/.exec(hash);
  if (match?.[1] === undefined) return null;
  try {
    return decodeURIComponent(match[1]);
  } catch {
    return match[1];
  }
}

// inviteLink is the link handed to the person who made an invite, the one
// place the code is shown.
export function inviteLink(origin: string, inviteCode: string): string {
  return `${origin}/app/#/invite/${encodeURIComponent(inviteCode)}`;
}

// Sign-in links (issue #493): <origin>/app/#/signin/<code>, made by an
// operator (`conch admin principal sign-in-link`) for a person to come back
// into their own account. The code is handled exactly as an invite's: read
// into memory, never stored, logged or put in the DOM. The page never makes
// one, so there is no builder here.

// signInFromHash is the code of a sign-in link, read as inviteFromHash reads
// an invite's.
export function signInFromHash(hash: string): string | null {
  const match = /^#\/signin\/([^/]+)$/.exec(hash);
  if (match?.[1] === undefined) return null;
  try {
    return decodeURIComponent(match[1]);
  } catch {
    return match[1];
  }
}

// A code found in the address: an invite's, or a sign-in link's.
export type LinkInAddress = { readonly kind: 'invite'; readonly inviteCode: string } | { readonly kind: 'signin'; readonly signInCode: string };

// addressWithoutFragment is the page's address with no fragment at all, so
// writing it over the history entry leaves nothing of the hash behind.
export function addressWithoutFragment(location: Pick<Location, 'origin' | 'pathname' | 'search'>): string {
  return `${location.origin}${location.pathname}${location.search}`;
}

// takeLinkFromAddress reads an invite or sign-in code from the address bar
// and at once replaces the history entry with the address without its
// fragment, so the code is neither in the address bar nor in the back
// history. This is the one place the page writes history (hygiene.test.ts
// holds it to this call). replaceState fires no hashchange and loads
// nothing. A link that looks like either but does not parse is taken out of
// the address all the same.
//
// What this cannot reach (security review of #440, accepted): when the page is
// loaded from the link, the browser's navigation timing entry
// (performance.getEntriesByType('navigation')) keeps the address it loaded,
// code and all, for as long as the document lives; no script can clear it,
// and only this origin's own scripts can read it. The browser itself may also
// have recorded the link before the page ran, in its global history, session
// restore or a synced history, and replaceState does not purge those. So the
// code's lifetime is the protection: a personal code dies after its one use,
// and an open link relies on its short expiry and on being closed. A sign-in
// link is used once and lasts a day unless its operator chose otherwise.
export function takeLinkFromAddress(): LinkInAddress | null {
  const hash = window.location.hash;
  const invite = hash.startsWith('#/invite/');
  if (!invite && !hash.startsWith('#/signin/')) return null;
  const inviteCode = invite ? inviteFromHash(hash) : null;
  const signInCode = invite ? null : signInFromHash(hash);
  window.history.replaceState(null, '', addressWithoutFragment(window.location));
  if (inviteCode !== null) return { kind: 'invite', inviteCode };
  if (signInCode !== null) return { kind: 'signin', signInCode };
  return null;
}

// The channel last open in this tab, for a load whose hash names none.
const lastKey = 'conch.web.lastChannel';

export function rememberedChannel(): string | null {
  try {
    return window.sessionStorage.getItem(lastKey);
  } catch {
    return null;
  }
}

// forgetChannel drops the remembered channel, at sign-out: the next person to
// sign in on this tab is not steered to the last one's channel.
export function forgetChannel(): void {
  try {
    window.sessionStorage.removeItem(lastKey);
  } catch {
    // Nothing stored that could be removed.
  }
}

export function rememberChannel(name: string): void {
  try {
    window.sessionStorage.setItem(lastKey, name);
  } catch {
    // Not remembered; the first channel is chosen next time.
  }
}
