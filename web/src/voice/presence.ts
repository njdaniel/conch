// Voice presence: the roster and who is transmitting, from the voice socket
// (and, before the caller has joined, one read of the snapshot).
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
}

// occupants is who is in the channel-wide room (the one with no audience).
export function occupants(p: VoicePresenceV1 | null): Occupant[] {
  const room = p?.rooms.find((r) => r.audience === undefined);
  return (room?.participants ?? []).map((x) => ({ principal: x.principal_id, transmitting: x.transmitting }));
}
