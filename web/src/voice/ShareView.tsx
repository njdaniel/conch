// The watch view (issue #444): someone's shared screen, with its sound on a
// volume and a mute of its own, apart from voice. It is open only while this
// page watches. The SDK sizes what it receives to this view (adaptive stream),
// so a small view or a phone gets a smaller layer than a full screen, and a
// closed view gets none. It asks for no device.
import { useEffect, useRef, useState } from 'react';
import type { VoiceClient, VoiceState } from './connection';

// A video element Safari on iPhone can put full screen: it has no
// requestFullscreen for other elements.
type AppleVideo = HTMLVideoElement & { readonly webkitEnterFullscreen?: () => void };

export function ShareView({
  client,
  state,
  name,
  audio,
}: {
  readonly client: VoiceClient;
  readonly state: VoiceState;
  // Who shares, as the panel names them, and whether presence says the share
  // has sound.
  readonly name: string;
  readonly audio: boolean;
}) {
  const box = useRef<HTMLDivElement>(null);
  const video = useRef<HTMLVideoElement>(null);
  const sound = useRef<HTMLAudioElement>(null);
  const [volume, setVolume] = useState(1);
  const [muted, setMuted] = useState(false);
  // The browser would not start the sound without a tap (phones' autoplay
  // rules): the view offers one.
  const [blocked, setBlocked] = useState(false);

  useEffect(() => {
    const v = video.current;
    const a = sound.current;
    if (v === null || a === null) return;
    client.attachShare({ video: v, audio: a });
    return () => {
      client.attachShare(null);
    };
  }, [client]);

  // Deafened hears nothing, a share's sound included.
  useEffect(() => {
    const a = sound.current;
    if (a === null) return;
    a.volume = volume;
    a.muted = muted || state.deafened;
  }, [volume, muted, state.deafened]);

  const hasSound = state.shareTracks.audio;
  useEffect(() => {
    const a = sound.current;
    if (!hasSound || a === null) return;
    let live = true;
    play(a).then(
      () => {
        if (live) setBlocked(false);
      },
      () => {
        if (live) setBlocked(true);
      },
    );
    return () => {
      live = false;
    };
  }, [hasSound]);

  const fullscreen = () => {
    const el = box.current;
    const v: AppleVideo | null = video.current;
    if (el !== null && document.fullscreenEnabled) {
      void el.requestFullscreen().catch(() => undefined);
    } else if (v?.webkitEnterFullscreen !== undefined) {
      v.webkitEnterFullscreen();
    }
  };

  const tapToPlay = () => {
    const a = sound.current;
    if (a === null) return;
    play(a).then(
      () => {
        setBlocked(false);
      },
      () => {
        setBlocked(true);
      },
    );
  };

  return (
    <section aria-label="Shared screen" className="share-view">
      <p>
        {name} is sharing{audio ? ', with sound' : ''}
      </p>
      <div ref={box} className="share-frame">
        <video ref={video} aria-label={`${name}'s screen`} playsInline autoPlay muted />
        {state.shareTracks.video ? null : <p className="hint">Waiting for the picture</p>}
      </div>
      <audio ref={sound} autoPlay />
      <div className="voice-controls">
        <button type="button" onClick={fullscreen}>Full screen</button>
        <button type="button" aria-pressed={muted} onClick={() => { setMuted(!muted); }}>
          {muted ? 'Unmute share' : 'Mute share'}
        </button>
        <label>
          Share volume
          <input type="range" min={0} max={1} step={0.05} value={volume} onChange={(e) => { setVolume(Number(e.target.value)); }} />
        </label>
        <button type="button" onClick={() => { client.watch(false); }}>Stop watching</button>
      </div>
      {blocked && hasSound ? (
        <button type="button" onClick={tapToPlay}>Tap to play sound</button>
      ) : null}
      {state.deafened && hasSound ? <p className="hint">Deafened: the share&apos;s sound is off too.</p> : null}
    </section>
  );
}

// play starts an element; a browser that refuses without a tap rejects.
function play(el: HTMLMediaElement): Promise<void> {
  try {
    return Promise.resolve(el.play());
  } catch (err) {
    return Promise.reject(err instanceof Error ? err : new Error('play refused'));
  }
}
