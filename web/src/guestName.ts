// How a temporary guest is named on every surface (ADR-009, design §4.1):
// "Guest: <label> · <tag>" when it chose a label, its handle alone when not.
// It is read from the guest attribution the server stores and sends, never
// from the text of a name, so a permanent account cannot pass for a guest
// and two visits with the same label stay apart by their tags. The tag is
// the server's: it is sent so that no client derives it differently.
import type { ChannelMemberV1 } from './schema/channel';
import type { GuestAttributionV1, GuestV1 } from './schema/guest';
import type { WhoAmIResponseV1 } from './schema/principal';

export function guestName(guest: Pick<GuestAttributionV1, 'handle' | 'label' | 'tag'>): string {
  return guest.label === undefined || guest.label === '' ? guest.handle : `Guest: ${guest.label} · ${guest.tag}`;
}

// memberName is how a channel member is named: by its guest attribution
// while it is a guest, else by its name, if the list carried one.
export function memberName(member: Pick<ChannelMemberV1, 'name' | 'guest'>): string | undefined {
  return member.guest === undefined ? member.name : guestName(member.guest);
}

// visitOf is the live visit of the signed-in principal, or null for anyone
// who is not a guest now. A server that predates guests sends neither
// identity_class nor guest, and nobody is a guest there.
export function visitOf(who: WhoAmIResponseV1): GuestV1 | null {
  if (who.guest === undefined || who.guest.state !== 'active') return null;
  if (who.identity_class !== undefined && who.identity_class !== 'guest') return null;
  return who.guest;
}

// displayName is how the signed-in principal is named in its own lobby.
export function displayName(who: WhoAmIResponseV1): string {
  const visit = visitOf(who);
  return visit === null ? who.name : guestName(visit);
}
