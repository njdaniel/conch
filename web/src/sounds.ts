import { BrowserSoundSink } from './platform/browser/sounds';
// Optional interface audio owns its context only; it never opens a device or
// changes the voice client's audio routing. Skipped sounds are never queued.
import { useSyncExternalStore } from 'react';
import { platform } from './platform';
import type { VoicePresenceV2 } from './schema/voice';

export const soundNames = { message: 'Message sound', join: 'Call join sound', leave: 'Call leave sound', share: 'Share start sound' };
export type Sound = keyof typeof soundNames;
export interface SoundPreferences {
  readonly message: boolean;
  readonly join: boolean;
  readonly leave: boolean;
  readonly share: boolean;
  readonly volume: number;
}
const defaults: SoundPreferences = { message: false, join: false, leave: false, share: false, volume: 0.5 };
const key = 'conch.web.sounds.v1';
export function readSoundPreferences(): SoundPreferences {
  try {
    const raw: unknown = JSON.parse(platform.credentials.readPreference(key) ?? 'null');
    if (typeof raw !== 'object' || raw === null) return defaults;
    const p = raw as Partial<Record<keyof SoundPreferences, unknown>>;
    return { message: p.message === true, join: p.join === true, leave: p.leave === true, share: p.share === true,
      volume: typeof p.volume === 'number' && Number.isFinite(p.volume) ? Math.max(0, Math.min(1, p.volume)) : defaults.volume };
  } catch { return defaults; }
}
export interface AudioSink {
  unlock(): void;
  ready(): boolean;
  play(sound: Sound, volume: number): void;
}
export class SoundPlayer {
  private unlocked = false;
  constructor(private sink: AudioSink, private preferences: () => SoundPreferences) {}
  unlock(): void {
    const p = this.preferences();
    if (!p.message && !p.join && !p.leave && !p.share) return;
    this.unlocked = true;
    this.sink.unlock();
  }
  play(sound: Sound): void {
    const p = this.preferences();
    if (!this.unlocked || !p[sound] || p.volume === 0 || !this.sink.ready()) return;
    try { this.sink.play(sound, p.volume); } catch { /* Audio failure is silent. */ }
  }
}
let preferences: SoundPreferences | undefined;
const listeners = new Set<() => void>();
const snapshot = () => preferences ??= readSoundPreferences();
const subscribe = (listener: () => void) => {
  listeners.add(listener);
  return () => { listeners.delete(listener); };
};
export const sounds = new SoundPlayer(new BrowserSoundSink(), snapshot);
export function setSoundPreferences(next: SoundPreferences): void {
  preferences = next;
  try { platform.credentials.writePreference(key, JSON.stringify(next)); } catch { /* Keep the choice in memory. */ }
  for (const listener of listeners) listener();
}
export function useSoundPreferences(): SoundPreferences {
  return useSyncExternalStore(subscribe, snapshot);
}
// Installed by the notification hook, so all sound events obey the same
// current account, policy and channel mutes. No hook means no sound.
export let soundAudience: { me: number; allows: (channel: number) => boolean } | null = null;
export function setSoundAudience(value: typeof soundAudience): void { soundAudience = value; }

export function voiceSounds(before: VoicePresenceV2 | null, after: VoicePresenceV2, me: number, play: (sound: Sound) => void): void {
  if (before === null) return;
  const oldRoom = before.rooms.find((r) => r.audience === undefined);
  const room = after.rooms.find((r) => r.audience === undefined);
  if (oldRoom === undefined || room === undefined || before.channel_id !== after.channel_id || !after.available || !after.configured) return;
  const old = oldRoom.participants;
  const next = room.participants;
  if (!old.some((p) => p.principal_id === me) || !next.some((p) => p.principal_id === me)) return;
  for (const p of next) {
    if (p.principal_id === me) continue;
    const previous = old.find((o) => o.principal_id === p.principal_id);
    if (previous === undefined) play('join');
    if (p.sharing === true && (previous?.sharing !== true || previous.share_started_at !== p.share_started_at)) play('share');
  }
  for (const p of old) if (p.principal_id !== me && !next.some((n) => n.principal_id === p.principal_id)) play('leave');
}
