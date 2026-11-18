// Bundled interface audio on the default output; independent of voice devices.
import type { AudioSink, Sound } from '../../sounds';

export class BrowserSoundSink implements AudioSink {
  private context: AudioContext | null = null;
  private loading = false;
  private buffers = new Map<Sound, AudioBuffer>();
  unlock(): void {
    try {
      if (this.context?.state === 'running') return;
      this.context ??= new AudioContext();
      const context = this.context;
      // Both resume and the silent source happen synchronously in the gesture,
      // including on Safari. Fetch/decode never triggers playback later.
      void context.resume().catch(() => undefined);
      const source = context.createBufferSource();
      source.buffer = context.createBuffer(1, 1, context.sampleRate);
      source.connect(context.destination);
      source.start();
      source.onended = () => { source.disconnect(); };
      if (this.loading) return;
      this.loading = true;
      for (const sound of ['message', 'join', 'leave', 'share'] as const) {
        if (this.buffers.has(sound)) continue;
        void fetch(`${import.meta.env.BASE_URL}sounds/${sound}.wav`)
          .then((r) => { if (!r.ok) throw new Error('Sound unavailable'); return r.arrayBuffer(); })
          .then((bytes) => context.decodeAudioData(bytes))
          .then((buffer) => { this.buffers.set(sound, buffer); }).catch(() => undefined);
      }
    } catch { /* Unsupported or blocked audio stays silent. */ }
  }
  ready(): boolean { return this.context?.state === 'running'; }
  play(sound: Sound, volume: number): void {
    const context = this.context;
    const buffer = this.buffers.get(sound);
    if (context === null || context.state !== 'running' || buffer === undefined) return;
    const source = context.createBufferSource();
    const gain = context.createGain();
    source.buffer = buffer;
    gain.gain.value = volume;
    source.connect(gain).connect(context.destination);
    source.onended = () => { source.disconnect(); gain.disconnect(); };
    source.start();
  }
}
