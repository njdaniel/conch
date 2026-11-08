// Voice presence: the roster, who is transmitting, who holds a camera claim and
// every live screen share, from the v2 voice socket (voice-control-plane.md §6,
// voice-multi-share.md §2, issue #1509). The socket is open for the channel
// shown whether or not the caller has joined; after the server closes it, one
// snapshot read learns why.
import { voicePresenceV2, type VoicePresenceV1, type VoicePresenceV2, type VoiceShareQuality } from '../schema/voice';

type Presence = VoicePresenceV1 | VoicePresenceV2;
import { fromServer } from '../schema/validate';

// parsePresence reads one socket frame; null for anything that is not a
// presence document of this version.
export function parsePresence(data: unknown): VoicePresenceV2 | null {
  if (typeof data !== 'string') return null;
  try {
    const body: unknown = JSON.parse(data);
    return fromServer(voicePresenceV2)(body).length === 0 ? (body as VoicePresenceV2) : null;
  } catch {
    return null;
  }
}

export interface Occupant {
  readonly principal: number;
  readonly transmitting: boolean;
  // Holds a screen share of their own (issue #444, #1509), and whether it has sound.
  readonly sharing: boolean;
  readonly sharingAudio: boolean;
  // Holds a camera claim (ADR-015): what conchd granted, not what this page
  // receives, and never how the person's own page says it sends (S5).
  readonly camera: boolean;
  // When they joined, for the call view's order.
  readonly joinedAt: string;
}

// occupants is who is in the channel-wide room (the one with no audience).
export function occupants(p: Presence | null): Occupant[] {
  const room = p?.rooms.find((r) => r.audience === undefined);
  return (room?.participants ?? []).map((x) => ({
    principal: x.principal_id,
    transmitting: x.transmitting,
    sharing: x.sharing === true,
    sharingAudio: x.sharing_audio === true,
    camera: x.camera === true,
    joinedAt: x.joined_at,
  }));
}

// Sharer is one participant's live stream: its claim start and the quality the
// sharer declared, not a measurement (voice-multi-share.md §2.2, §3.2, #1509).
export interface Sharer extends Occupant {
  readonly startedAt: string;
  readonly quality: VoiceShareQuality | null;
}

// qualityLabel formats a declared resolution and frame rate for a stream tile;
// null means the sharer declared none (voice-multi-share.md §3, #1509).
export function qualityLabel(quality: VoiceShareQuality | null): string | null {
  if (quality === null) return null;
  const resolution = quality.resolution === 'source' ? 'Source' : quality.resolution;
  return `${resolution} · ${String(quality.frame_rate)} fps`;
}

// sharers is everyone sharing a screen in the channel-wide room, replacing the
// old at-most-one sharer read (voice-multi-share.md §2, issue #1509). Stream
// order is the claim's start time, oldest first and newest last.
export function sharers(p: VoicePresenceV2 | null): Sharer[] {
  const room = p?.rooms.find((r) => r.audience === undefined);
  return (room?.participants ?? [])
    .filter((x) => x.sharing === true && x.share_started_at !== undefined)
    .map((x) => ({
      principal: x.principal_id,
      transmitting: x.transmitting,
      sharing: true,
      sharingAudio: x.sharing_audio === true,
      camera: x.camera === true,
      joinedAt: x.joined_at,
      startedAt: x.share_started_at ?? '',
      quality: x.share_quality ?? null,
    }))
    .sort((a, b) => {
      const at = Date.parse(a.startedAt) - Date.parse(b.startedAt);
      return Number.isNaN(at) || at === 0 ? a.principal - b.principal : at;
    });
}

// cameraHolders is who holds a camera claim in the channel-wide room.
export function cameraHolders(p: Presence | null): Set<number> {
  return new Set(occupants(p).filter((o) => o.camera).map((o) => o.principal));
}

// principalOf reads the principal id out of an identity p<id>.
export function principalOf(identity: string): number | null {
  const m = /^p(\d+)$/.exec(identity);
  return m === null || m[1] === undefined ? null : Number(m[1]);
}
