// Synthetic tones for the browser voice specs: a talker's microphone that is
// a sine the spec switches on and off, and a listener that reads what its page
// receives. Moved out of 32-voice-onset (issue #965) for 65-who-hears-whom
// (issue #1610), which gives each of three people a tone of their own.
//
// Input: a frequency and a level for the microphone; for the listener, the
// frequencies to read. Output: the level of each received audio track at each
// frequency (toneLevels), and, as before, the loudest of the tracks every 5 ms
// (startListening). It owns the in-page code and the pure decisions made on
// the levels (floorOf, heardBy, tracksAt); it owns no world and no page.
//
// The device's own capture is asked for first, so permissions and device
// errors are the browser's, and nothing here relies on Chromium's
// --use-file-for-fake-audio-capture.
import type { Page } from '@playwright/test';

export interface ToneOptions {
  // Hz; 440 by default.
  readonly frequency?: number;
  // The gain __signal(true) sets, 0.6 by default.
  readonly level?: number;
  // True: the tone is on from the start (push to talk and the mute then decide
  // what is sent). The default is off until __signal(true).
  readonly on?: boolean;
}

// syntheticMicrophone answers a request for audio with a tone whose gain the
// spec controls (window.__signal), and marks the talk key's presses and the
// indicator's changes. The device's own capture is asked for as before, so
// permissions and device errors are the browser's, and is stopped with the
// tone.
export const syntheticMicrophone = (options?: ToneOptions) => {
  const now = () => performance.timeOrigin + performance.now();
  const w = window as unknown as {
    __signal?: (on: boolean) => number;
    __marks: { down: number[]; up: number[]; on: number[]; off: number[] };
  };
  w.__marks = { down: [], up: [], on: [], off: [] };
  window.addEventListener(
    'keydown',
    (e) => {
      if (e.code === 'Backquote' && !e.repeat) w.__marks.down.push(now());
    },
    true,
  );
  window.addEventListener(
    'keyup',
    (e) => {
      if (e.code === 'Backquote') w.__marks.up.push(now());
    },
    true,
  );
  // The indicator: "Transmitting", as the strip says it.
  let shown = false;
  new MutationObserver(() => {
    const text = document.querySelector('.voice-tx')?.textContent ?? '';
    const on = text.endsWith('Transmitting') && !text.endsWith('Not transmitting');
    if (on !== shown) {
      shown = on;
      (on ? w.__marks.on : w.__marks.off).push(now());
    }
  }).observe(document, { subtree: true, childList: true, characterData: true });
  const devices = navigator.mediaDevices;
  const original = devices.getUserMedia.bind(devices);
  devices.getUserMedia = async (constraints?: MediaStreamConstraints) => {
    const real = await original(constraints);
    if (constraints?.audio === undefined || constraints.audio === false) return real;
    const context = new AudioContext();
    const tone = context.createOscillator();
    tone.frequency.value = options?.frequency ?? 440;
    const gain = context.createGain();
    gain.gain.value = options?.on === true ? (options.level ?? 0.6) : 0;
    const out = context.createMediaStreamDestination();
    tone.connect(gain).connect(out);
    tone.start();
    void context.resume();
    w.__signal = (on: boolean) => {
      gain.gain.setValueAtTime(on ? (options?.level ?? 0.6) : 0, context.currentTime);
      return now();
    };
    const track = out.stream.getAudioTracks()[0];
    if (track === undefined) return real;
    const stop = track.stop.bind(track);
    track.stop = () => {
      for (const t of real.getTracks()) t.stop();
      stop();
    };
    return new MediaStream([track]);
  };
};

// recordHeard keeps every audio track the listener's page receives, so what
// it plays can be read.
export const recordHeard = () => {
  const w = window as unknown as { __heardTracks: MediaStreamTrack[]; __heardStreams: Record<string, string> };
  w.__heardTracks = [];
  // The id of each track's stream, by track id. LiveKit's peer connection
  // raises 'track' twice for a track: first in a stream of a made-up id, then
  // in the participant's ("PA_<sid>|TR_<sid>"); the latest wins.
  w.__heardStreams = {};
  const Original = window.RTCPeerConnection;
  const Wrapped = function (this: unknown, ...args: ConstructorParameters<typeof RTCPeerConnection>) {
    const pc = new Original(...args);
    pc.addEventListener('track', (e) => {
      if (e.track.kind !== 'audio') return;
      w.__heardTracks.push(e.track);
      const stream = e.streams[0];
      if (stream !== undefined) w.__heardStreams[e.track.id] = stream.id;
    });
    return pc;
  } as unknown as typeof RTCPeerConnection;
  Wrapped.prototype = Original.prototype;
  Object.setPrototypeOf(Wrapped, Original);
  window.RTCPeerConnection = Wrapped;
};

// startListening reads the level of what the listener's page receives every
// 5 ms, from 256 samples (5 ms at 48 kHz), into window.__heard: the loudest
// of the tracks received.
export async function startListening(page: Page): Promise<void> {
  await page.evaluate(() => {
    const w = window as unknown as { __heardTracks: MediaStreamTrack[]; __heard: { t: number; rms: number }[] };
    w.__heard = [];
    const context = new AudioContext();
    void context.resume();
    const analysers: AnalyserNode[] = [];
    let wired = 0;
    const buffer = new Float32Array(256);
    const read = () => {
      // A context a browser suspended (an autoplay policy) reads silence.
      if (context.state === 'suspended') void context.resume();
      for (; wired < w.__heardTracks.length; wired++) {
        const track = w.__heardTracks[wired];
        if (track === undefined) continue;
        const analyser = context.createAnalyser();
        analyser.fftSize = 256;
        context.createMediaStreamSource(new MediaStream([track])).connect(analyser);
        analysers.push(analyser);
      }
      let rms = 0;
      for (const a of analysers) {
        a.getFloatTimeDomainData(buffer);
        let sum = 0;
        for (const v of buffer) sum += v * v;
        rms = Math.max(rms, Math.sqrt(sum / buffer.length));
      }
      w.__heard.push({ t: performance.timeOrigin + performance.now(), rms });
      setTimeout(read, 5);
    };
    read();
  });
}

// toneLevels waits ms and then returns, for each audio track the page has
// received (recordHeard), the highest level in dB seen at each of the
// frequencies over that time: the peak within three bins of the frequency in an
// 8192-point spectrum (6 Hz a bin), read every 50 ms. Silence reads -120. A
// track is listed once however often it was announced, and is live only when
// it has not ended and carries a participant's stream id ("PA_..."): the peer
// connection also holds spare transceivers that carry nothing.
export async function toneLevels(page: Page, frequencies: readonly number[], ms: number): Promise<TrackLevel[]> {
  return page.evaluate(
    async ({ frequencies: freqs, ms: window_ }) => {
      const w = window as unknown as {
        __heardTracks: MediaStreamTrack[];
        __heardStreams: Record<string, string>;
        __toneContext?: AudioContext;
        __toneAnalysers?: Map<string, { track: MediaStreamTrack; analyser: AnalyserNode }>;
      };
      const context = (w.__toneContext ??= new AudioContext());
      void context.resume();
      const analysers = (w.__toneAnalysers ??= new Map<string, { track: MediaStreamTrack; analyser: AnalyserNode }>());
      // A track received since the last reading gets its analyser now.
      for (const track of w.__heardTracks) {
        if (analysers.has(track.id)) continue;
        const analyser = context.createAnalyser();
        analyser.fftSize = 8192;
        analyser.smoothingTimeConstant = 0;
        context.createMediaStreamSource(new MediaStream([track])).connect(analyser);
        analysers.set(track.id, { track, analyser });
      }
      const entries = [...analysers.entries()];
      const out = entries.map(([id, e]) => {
        const streamId = w.__heardStreams[id] ?? '';
        return { streamId, live: e.track.readyState === 'live' && streamId.startsWith('PA_'), db: freqs.map(() => -120) };
      });
      const spectrum = new Float32Array(8192 / 2);
      const binHz = context.sampleRate / 8192;
      const until = performance.now() + window_;
      do {
        await new Promise((r) => setTimeout(r, 50));
        entries.forEach(([, e], i) => {
          e.analyser.getFloatFrequencyData(spectrum);
          freqs.forEach((f, j) => {
            const centre = Math.round(f / binHz);
            for (let k = centre - 3; k <= centre + 3; k++) {
              const v = Math.max(spectrum[k] ?? -120, -120);
              const row = out[i];
              if (row !== undefined && v > (row.db[j] ?? -120)) row.db[j] = v;
            }
          });
        });
      } while (performance.now() < until);
      return out;
    },
    { frequencies, ms },
  );
}

// What one received audio track showed: its stream's id, whether the track is
// still live, and its level in dB at each frequency asked for.
export interface TrackLevel {
  readonly streamId: string;
  readonly live: boolean;
  readonly db: readonly number[];
}

// floorOf is a page's idle floor: the highest level of any live track at any
// frequency while nobody transmits. Measured, never assumed; atLeast is the
// lowest it is taken to be (digital silence reads -120, which no codec noise
// beside a real signal stays above).
export function floorOf(idle: readonly TrackLevel[], atLeast = -120): number {
  let floor = atLeast;
  for (const t of idle) if (t.live) for (const v of t.db) floor = Math.max(floor, v);
  return floor;
}

// heardBy lists, for each live track in order, the indexes of the frequencies
// at which it is more than marginDb above the floor.
export function heardBy(tracks: readonly TrackLevel[], floor: number, marginDb: number): number[][] {
  return tracks.filter((t) => t.live).map((t) => t.db.flatMap((v, i) => (v > floor + marginDb ? [i] : [])));
}

// tracksAt is how many live tracks are heard at frequency i.
export function tracksAt(tracks: readonly TrackLevel[], floor: number, marginDb: number, i: number): number {
  return heardBy(tracks, floor, marginDb).filter((at) => at.includes(i)).length;
}

// table is the per-track table a failing spec attaches: levels, the floor and
// the margin. It names no token, room or identity.
export function table(who: string, tracks: readonly TrackLevel[], frequencies: readonly number[], floor: number, marginDb: number): string {
  const rows = tracks.map((t, n) => `  track ${String(n)} ${t.live ? 'live' : 'ended'} ${t.db.map((v, i) => `${String(frequencies[i])} Hz ${v.toFixed(0)} dB`).join(', ')}`);
  return [`${who}: floor ${floor.toFixed(0)} dB, margin ${String(marginDb)} dB`, ...rows].join('\n');
}
