// Voice presence: the roster, who is transmitting, the camera claims and every
// live screen share, from the channel's v2 presence socket.
import { voicePresenceV2, type VoicePresenceV1, type VoicePresenceV2, type VoiceShareQuality } from '../schema/voice';

type Presence = VoicePresenceV1 | VoicePresenceV2;
import { fromServer } from '../schema/validate';

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
  readonly sharing: boolean;
  readonly sharingAudio: boolean;
  readonly camera: boolean;
  readonly joinedAt: string;
}

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

export interface Sharer extends Occupant {
  readonly startedAt: string;
  readonly quality: VoiceShareQuality | null;
}

export function qualityLabel(quality: VoiceShareQuality | null): string | null {
  if (quality === null) return null;
  const resolution = quality.resolution === 'source' ? 'Source' : quality.resolution;
  return `${resolution} · ${String(quality.frame_rate)} fps`;
}

// Stream order is the claim's start time, oldest first and newest last.
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

export function cameraHolders(p: Presence | null): Set<number> {
  return new Set(occupants(p).filter((o) => o.camera).map((o) => o.principal));
}

export function principalOf(identity: string): number | null {
  const m = /^p(\d+)$/.exec(identity);
  return m === null || m[1] === undefined ? null : Number(m[1]);
}
