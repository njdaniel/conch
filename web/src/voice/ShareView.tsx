// One watched stream's player. Its volume and mute belong to this tile, apart
// from voice and from every other watched stream.
import { useEffect, useRef, useState } from 'react';
import { Glyph } from '../Icons';
import type { VoiceClient, VoiceState } from './connection';
import { shareReadout } from './sdk';
import type { ShareDirection, ShareSample } from './share';

export function ShareReadout({ client, direction, principal, prefix = '' }: { readonly client: VoiceClient; readonly direction: ShareDirection; readonly principal?: number; readonly prefix?: string }) {
  const [text, setText] = useState<string | null>(null);
  const previous = useRef<ShareSample | null>(null);
  useEffect(() => {
    let live = true;
    let timer: number | null = null;
    previous.current = null;
    setText(null);
    const read = async () => {
      const sample = await client.readShareStats(direction, principal);
      if (!live) return;
      setText(shareReadout(sample, previous.current));
      previous.current = sample;
      timer = window.setTimeout(() => { void read(); }, 1000);
    };
    void read();
    return () => {
      live = false;
      if (timer !== null) window.clearTimeout(timer);
    };
  }, [client, direction, principal]);
  return text === null ? null : <span className="share-readout">{prefix}{text}</span>;
}

type AppleVideo = HTMLVideoElement & { readonly webkitEnterFullscreen?: () => void };

export function ShareView({
  client,
  state,
  principal,
  name,
  audio,
  quality,
  volume: keptVolume,
  onVolumeChange,
  onFocus,
}: {
  readonly client: VoiceClient;
  readonly state: VoiceState;
  readonly principal?: number;
  readonly name: string;
  readonly audio: boolean;
  readonly quality?: string | null;
  readonly volume?: number;
  readonly onVolumeChange?: (volume: number) => void;
  readonly onFocus?: () => void;
}) {
  const box = useRef<HTMLDivElement>(null);
  const video = useRef<HTMLVideoElement>(null);
  const sound = useRef<HTMLAudioElement>(null);
  const [volume, setOwnVolume] = useState(keptVolume ?? 1);
  const [muted, setMuted] = useState(false);
  const [blocked, setBlocked] = useState(false);

  useEffect(() => {
    const v = video.current;
    const a = sound.current;
    if (v === null || a === null) return;
    if (principal === undefined) client.attachShare({ video: v, audio: a });
    else client.attachStream(principal, { video: v, audio: a });
    return () => {
      if (principal === undefined) client.attachShare(null);
      else client.attachStream(principal, null);
    };
  }, [client, principal]);

  useEffect(() => {
    const a = sound.current;
    if (a === null) return;
    a.volume = volume;
    a.muted = muted || state.deafened;
  }, [volume, muted, state.deafened]);

  const tracks = principal === undefined ? state.shareTracks : (state.streamTracks.get(principal) ?? { video: false, audio: false });
  const hasSound = tracks.audio;
  useEffect(() => {
    const a = sound.current;
    if (!hasSound || a === null) return;
    let live = true;
    play(a).then(
      () => { if (live) setBlocked(false); },
      () => { if (live) setBlocked(true); },
    );
    return () => {
      live = false;
    };
  }, [hasSound]);

  const fullscreen = () => {
    const el = box.current;
    const v: AppleVideo | null = video.current;
    if (el !== null && document.fullscreenEnabled) void el.requestFullscreen().catch(() => undefined);
    else if (v?.webkitEnterFullscreen !== undefined) v.webkitEnterFullscreen();
  };

  const tapToPlay = () => {
    const a = sound.current;
    if (a === null) return;
    play(a).then(
      () => { setBlocked(false); },
      () => { setBlocked(true); },
    );
  };

  return (
    <section aria-label="Shared screen" className="share-view">
      <p className="share-title">
        <Glyph name="screen" />
        {principal === undefined ? <>{name} is sharing{audio ? ', with sound' : ''}</> : <>{name} · LIVE{quality === null || quality === undefined ? null : <> · {quality}</>}{audio ? ' · with sound' : ''}</>}
        <ShareReadout client={client} direction="received" principal={principal} />
      </p>
      <div ref={box} className="share-frame">
        <video ref={video} aria-label={`${name}'s screen`} playsInline autoPlay muted />
        {tracks.video ? null : <p className="hint">Waiting for the picture</p>}
        {onFocus === undefined ? null : <button type="button" className="call-enlarge" aria-label={`Focus ${name}'s screen`} onClick={onFocus} />}
      </div>
      <audio ref={sound} autoPlay aria-label={`${name}'s stream audio`} />
      <div className="share-controls">
        <button type="button" aria-pressed={muted} onClick={() => { setMuted(!muted); }}>
          <Glyph name="speaker" />
          <span className="label">{muted ? 'Unmute share' : 'Mute share'}</span>
        </button>
        <label className="volume">
          <span className="label">Share volume</span>
          <input type="range" min={0} max={1} step={0.05} value={volume} onChange={(e) => {
            const next = Number(e.target.value);
            setOwnVolume(next);
            onVolumeChange?.(next);
          }} />
        </label>
        <button type="button" onClick={fullscreen}>
          <Glyph name="fullscreen" />
          <span className="label">Full screen</span>
        </button>
        <button type="button" onClick={() => {
          if (principal === undefined) client.watch(false);
          else client.watchStream(principal, false);
        }}>Stop watching</button>
        {blocked && hasSound ? <button type="button" className="primary" onClick={tapToPlay}>Tap to play sound</button> : null}
      </div>
      {state.deafened && hasSound ? <p className="hint">Deafened: the share&apos;s sound is off too.</p> : null}
    </section>
  );
}

function play(el: HTMLMediaElement): Promise<void> {
  try {
    return Promise.resolve(el.play());
  } catch (err) {
    return Promise.reject(err instanceof Error ? err : new Error('play refused'));
  }
}
