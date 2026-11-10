// Handing an invite code to the client (issue #1591). The code comes from
// main's inviteTake, once, and goes to the client the one way it already
// reads an invite: the address fragment #/invite/<code>, which route.ts's
// takeLinkFromAddress reads on mount and on hashchange and at once replaces
// with the address without it. The invite views then preview the invite and
// join only on their own button. Nothing here stores, logs or renders the
// code, and the hash is the one part of the address a script may write
// (hygiene.test.ts).
import { inviteBridge } from './access';

export function handOffInvite(code: string): void {
  window.location.hash = `#/invite/${encodeURIComponent(code)}`;
}

// takeInvite asks main for the code staged for this profile, once, and hands
// it to the client. A page with no invite function, or main refusing, is
// simply no invite.
export async function takeInvite(): Promise<boolean> {
  const take = inviteBridge()?.inviteTake;
  if (take === undefined) return false;
  const code = await take().catch(() => null);
  if (code === null) return false;
  handOffInvite(code);
  return true;
}
