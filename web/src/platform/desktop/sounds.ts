// Bundled interface WAVs on the default output, separate from native voice.
// #1924: <audio> uses media-src 'self', never fetch or an AudioContext, so
// neither connect-src nor the conch-app scheme needs another privilege.
import type { AudioSink, Sound } from '../../sounds';

export class DesktopSoundSink implements AudioSink {
  private readonly clips = new Map<Sound, HTMLAudioElement>();

  unlock(): void {
    for (const sound of ['message', 'join', 'leave', 'share'] as const) {
      if (this.clips.has(sound)) continue;
      try {
        const audio = new Audio(`conch-app://ui/sounds/${sound}.wav`);
        audio.preload = 'auto';
        audio.load();
        this.clips.set(sound, audio);
      } catch { /* Unsupported or blocked audio stays silent; retry on a gesture. */ }
    }
  }

  ready(): boolean { return this.clips.size > 0; }

  play(sound: Sound, volume: number): void {
    const audio = this.clips.get(sound);
    // Skip an unloaded cue rather than queueing playback for its load event.
    if (audio === undefined || audio.readyState < 2) return;
    try {
      audio.volume = volume;
      audio.currentTime = 0;
      void audio.play().catch(() => undefined);
    } catch { /* Media errors must never interrupt the interface. */ }
  }
}
