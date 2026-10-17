// Voice presence: the roster, who is transmitting and who is sharing a screen,
// from the voice socket (and, before the caller has joined, one read of the
// snapshot).
import { voicePresenceV1, type VoicePresenceV1 } from '../schema/voice';

// parsePresence reads one socket frame; null for anything that is not a
// presence document of this version.
export function parsePresence(data: unknown): VoicePresenceV1 | null {
  if (typeof data !== 'string') return null;
  try {
    const body: unknown = JSON.parse(data);
    return voicePresenceV1(body).length === 0 ? (body as VoicePresenceV1) : null;
  } catch {
    return null;
  }
}

export interface Occupant {
  readonly principal: number;
  readonly transmitting: boolean;
  // Holds the room's screen share (issue #444), and whether it has sound.
  readonly sharing: boolean;
  readonly sharingAudio: boolean;
}

// occupants is who is in the channel-wide room (the one with no audience).
export function occupants(p: VoicePresenceV1 | null): Occupant[] {
  const room = p?.rooms.find((r) => r.audience === undefined);
  return (room?.participants ?? []).map((x) => ({
    principal: x.principal_id,
    transmitting: x.transmitting,
    sharing: x.sharing === true,
    sharingAudio: x.sharing_audio === true,
  }));
}

// sharer is who shares a screen in the channel-wide room: at most one person.
export function sharer(p: VoicePresenceV1 | null): Occupant | null {
  return occupants(p).find((o) => o.sharing) ?? null;
}

// principalOf reads the principal id out of an identity p<id>.
export function principalOf(identity: string): number | null {
  const m = /^p(\d+)$/.exec(identity);
  return m === null || m[1] === undefined ? null : Number(m[1]);
}
