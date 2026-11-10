// The watch view (issue #444; multi-share voice-multi-share.md §7.1, issue
// #1509): one watched stream, with its sound on a volume and a mute of its own,
// apart from voice and every other watched stream. It is open only while this
// page watches that stream. An unfocused stream is held to the lowest layer;
// the focused one may take the top layer, which the SDK sizes to this view
// (adaptive stream), so a phone gets a smaller layer than a full-screen
// desktop, and a closed view gets none (voice-multi-share.md §8). It asks for
// no device.
import { useEffect, useRef, useState } from 'react';
import { Glyph } from '../Icons';
import type { VoiceClient, VoiceState } from './connection';
import { shareReadout } from './sdk';
import type { ShareDirection, ShareSample } from './share';

// What the watch view needs of the client: the desktop's strip (issue #1582)
// gives it the companion's, which is a narrower thing than the call view's.
export type ShareViewClient = Pick<VoiceClient, 'attachStream' | 'readShareStats' | 'watchStream'>;

// ShareReadout samples only the selected watched picture or the local screen's
// top layer. The first sample has no bitrate; the next, about a second later,
// does.
export function ShareReadout({ client, direction, principal, prefix = '' }: { readonly client: Pick<VoiceClient, 'readShareStats'>; readonly direction: ShareDirection; readonly principal?: number; readonly prefix?: string }) {
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
      // A missing sample resets the baseline.
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

// A video element Safari on iPhone can put full screen: it has no
// requestFullscreen for other elements.
type AppleVideo = HTMLVideoElement & { readonly webkitEnterFullscreen?: () => void };

// ShareView plays one watched publisher's screen and sound with independent
// controls and an optional focus action (voice-multi-share.md §7.1, #1509).
// Compact changes only its presentation and controls, never its attachment or
// subscription. It carries the layout toggle only when given one: on a closed
// stage with no call header (CallView says where; issue #1498).
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
  compact = false,
  onToggleLayout,
}: {
  readonly client: ShareViewClient;
  readonly state: VoiceState;
  // Whose stream it is: the publisher its tracks are kept under.
  readonly principal: number;
  // Who shares, as the panel names them, and whether presence says the share
  // has sound.
  readonly name: string;
  readonly audio: boolean;
  // The declared quality shown before and during watching; not a measurement.
  readonly quality?: string | null;
  // This stream's volume belongs to the panel, so moving its tile keeps it.
  readonly volume?: number;
  readonly onVolumeChange?: (volume: number) => void;
  readonly onFocus?: () => void;
  readonly compact?: boolean;
  readonly onToggleLayout?: () => void;
}) {
  const box = useRef<HTMLDivElement>(null);
  const video = useRef<HTMLVideoElement>(null);
  const sound = useRef<HTMLAudioElement>(null);
  const [volume, setOwnVolume] = useState(keptVolume ?? 1);
  const [muted, setMuted] = useState(false);
  // The browser would not start the sound without a tap (phones' autoplay
  // rules): the view offers one.
  const [blocked, setBlocked] = useState(false);

  useEffect(() => {
    const v = video.current;
    const a = sound.current;
    if (v === null || a === null) return;
    client.attachStream(principal, { video: v, audio: a });
    return () => {
      client.attachStream(principal, null);
    };
  }, [client, principal]);

  // Deafened hears nothing, a share's sound included. The SDK unmutes an
  // element each time it attaches a track with sound to it (a track arriving,
  // or attached again on a new connection), so the tile's own mute and volume
  // are set again whenever this stream's tracks change.
  const arrived = state.streamTracks.get(principal);
  useEffect(() => {
    const a = sound.current;
    if (a === null) return;
    a.volume = volume;
    a.muted = muted || state.deafened;
  }, [volume, muted, state.deafened, arrived]);

  const tracks = arrived ?? { video: false, audio: false };
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
        {name} · LIVE{quality === null || quality === undefined ? null : <> · {quality}</>}{audio ? ' · with sound' : ''}
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
        {compact ? null : (
          <>
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
          </>
        )}
        <button type="button" onClick={() => { client.watchStream(principal, false); }}>Stop watching</button>
        {onToggleLayout === undefined ? null : <button type="button" className="call-layout" aria-pressed={compact} onClick={onToggleLayout}>{compact ? 'Expand' : 'Compact'}</button>}
        {blocked && hasSound ? <button type="button" className="primary" onClick={tapToPlay}>Tap to play sound</button> : null}
      </div>
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
