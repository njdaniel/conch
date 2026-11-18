// Bundled interface audio on the default output; independent of voice devices.
import type { AudioSink, Sound } from '../../sounds';

export class BrowserSoundSink implements AudioSink {
  private context: AudioContext | null = null;
  private loading = new Set<Sound>();
  private buffers = new Map<Sound, AudioBuffer>();
  unlock(): void {
    try {
      this.context ??= new AudioContext();
      const context = this.context;
      // Both resume and the silent source happen synchronously in the gesture,
      // including on Safari. Fetch/decode never triggers playback later.
      if (context.state !== 'running') {
        void context.resume().catch(() => undefined);
        const source = context.createBufferSource();
        source.buffer = context.createBuffer(1, 1, context.sampleRate);
        source.connect(context.destination);
        source.start();
        source.onended = () => { source.disconnect(); };
      }
      for (const sound of ['message', 'join', 'leave', 'share'] as const) {
        if (this.buffers.has(sound) || this.loading.has(sound)) continue;
        this.loading.add(sound);
        void fetch(new URL(`sounds/${sound}.wav`, document.baseURI).href)
          .then((r) => { if (!r.ok) throw new Error('Sound unavailable'); return r.arrayBuffer(); })
          .then((bytes) => context.decodeAudioData(bytes))
          .then((buffer) => { this.buffers.set(sound, buffer); }).catch(() => undefined)
          .finally(() => { this.loading.delete(sound); });
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
