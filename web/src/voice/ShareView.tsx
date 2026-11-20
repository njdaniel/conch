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
import { fullscreenVideo } from './fullscreen';
import { usePictureInPicture } from './pictureInPicture';
import type { VoiceClient, VoiceState } from './connection';
import { askedText, nextShortfall, noShortfall, shareReadout, type ShortfallState } from './sdk';
import type { SettledQuality, ShareDirection, ShareSample } from './share';

// What the watch view needs of the client: the desktop's strip (issue #1582)
// gives it the companion's, which is a narrower thing than the call view's.
export type ShareViewClient = Pick<VoiceClient, 'attachStream' | 'readShareStats' | 'watchStream'>;

const cpuWarning = 'Your computer is too busy to send smoothly. Close heavy programs or choose a lower share resolution.';

// ShareReadout samples only the selected watched picture or the local screen's
// top layer. The first sample has no bitrate; the next, about a second later,
// does. Given asked (the sharer's own line), it says what was asked first, the
// measurement after, and, once the picture has been short of it for 5 s, one
// reason, until it has been back on target for 5 s. Loss shows when the engine
// reports it. The sending strip also warns after 3 s of CPU limitation.
// Every number is the browser's measurement; a stat the engine
// does not give is left out. It is one polite-off status line: read in order
// by a screen reader, never announced each second.
export function ShareReadout({ client, direction, principal, prefix = '', asked, now = () => performance.now() }: {
  readonly client: Pick<VoiceClient, 'readShareStats'>;
  readonly direction: ShareDirection;
  readonly principal?: number;
  readonly prefix?: string;
  readonly asked?: SettledQuality;
  // The clock the 5 s holds count on, in ms.
  readonly now?: () => number;
}) {
  const [text, setText] = useState<string | null>(null);
  const [cpuLimited, setCpuLimited] = useState(false);
  const [reason, setReason] = useState<string | null>(null);
  const previous = useRef<ShareSample | null>(null);
  const shortfall = useRef<ShortfallState>(noShortfall);
  const quality = useRef<SettledQuality | undefined>(asked);
  quality.current = asked;
  const clock = useRef(now);
  clock.current = now;
  // A new ask starts the explanation over: a reason found against the old one
  // must not outlast the change.
  useEffect(() => {
    shortfall.current = noShortfall;
    setReason(null);
  }, [asked?.resolution, asked?.frameRate]);
  useEffect(() => {
    let live = true;
    let timer: number | null = null;
    let cpuSince: number | null = null;
    let warned = false;
    previous.current = null;
    shortfall.current = noShortfall;
    setText(null);
    setCpuLimited(false);
    setReason(null);
    const read = async () => {
      const sample = await client.readShareStats(direction, principal);
      if (!live) return;
      const at = clock.current();
      setText(shareReadout(sample, previous.current));
      // The encoder's own reason can report CPU pressure even when the
      // requested quality is unknown or the measured picture meets it.
      if (direction !== 'sent' || sample?.limitation !== 'cpu') {
        cpuSince = null;
      } else if (cpuSince === null || sample.publication !== previous.current?.publication) {
        cpuSince = at;
      }
      const limited = cpuSince !== null && at - cpuSince >= 3000;
      if (limited && !warned) console.warn('Screen share encoder is CPU-limited.');
      warned = limited;
      setCpuLimited(limited);
      if (quality.current === undefined) {
        setReason(null);
      } else {
        shortfall.current = nextShortfall(shortfall.current, sample, quality.current, at);
        setReason(shortfall.current.reason);
      }
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
  if (text === null) return null;
  const readout = (
    <span className="share-readout" role="status" aria-live="off" aria-label="Share quality">
      {asked === undefined ? null : <span>{askedText(asked)}. </span>}
      <span>{prefix}{text}</span>
      {reason === null ? null : <span>. {reason}.</span>}
    </span>
  );
  if (direction === 'received') return readout;
  return (
    <>
      <p className="hint voice-supplemental">{readout}</p>
      {cpuLimited ? <p role="status" className="hint">{cpuWarning}</p> : null}
    </>
  );
}

// Shared by the browser and desktop sharer's panels; recovery is always a
// person choosing to stop and pick a source again.
export function ShareCaptureWarning({ stalled, client }: {
  readonly stalled: boolean;
  readonly client: Pick<VoiceClient, 'restartShare'>;
}) {
  if (!stalled) return null;
  return (
    <p role="status" className="hint">
      Screen capture stopped delivering frames.{' '}
      <button type="button" onClick={() => { void client.restartShare(); }}>Restart share</button>
    </p>
  );
}

// ShareView plays one watched publisher's screen and sound with independent
// controls and an optional focus action (voice-multi-share.md §7.1, #1509).
// Compact changes only its presentation and controls, never its attachment or
// subscription. It carries the layout toggle only when given one: on a closed
// stage with no call header (CallView says where; issue #1498). Reconnecting:
// the stream went with a renewed room and the client waits for it; the view
// stays, with its video element, which the next room plays into again, so a
// picture popped out stays out (#1839).
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
  reconnecting = false,
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
  readonly reconnecting?: boolean;
}) {
  const box = useRef<HTMLDivElement>(null);
  const video = useRef<HTMLVideoElement>(null);
  const sound = useRef<HTMLAudioElement>(null);
  const pip = usePictureInPicture(video);
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
  // are set again whenever this stream's tracks change, and after a tap that
  // starts playback (the SDK's startAudio unmutes every element it plays).
  const arrived = state.streamTracks.get(principal);
  useEffect(() => {
    const a = sound.current;
    if (a === null) return;
    a.volume = volume;
    a.muted = muted || state.deafened;
  }, [volume, muted, state.deafened, arrived, state.playback]);

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
    void fullscreenVideo(box.current, video.current);
  };

  const tapToPlay = () => {
    const a = sound.current;
    if (a === null) return;
    play(a).then(
      () => { setBlocked(false); },
      () => { setBlocked(true); },
    );
  };

  // Shown only where the browser offers the video PiP API.
  const popLabel = pip.active ? 'Return to stage' : 'Pop out';
  const popOut = pip.supported ? (
    <button type="button" className={compact ? undefined : 'share-pip'} aria-pressed={pip.active} title={popLabel} onClick={() => { void pip.toggle(); }}>
      <Glyph name="external" />
      <span className={compact ? 'label' : 'vh'}>{popLabel}</span>
    </button>
  ) : null;

  return (
    <section aria-label="Shared screen" className="share-view">
      <p className="share-title">
        <Glyph name="screen" />
        {name} · {reconnecting ? 'Reconnecting' : 'LIVE'}{quality === null || quality === undefined ? null : <> · {quality}</>}{audio ? ' · with sound' : ''}
        <ShareReadout client={client} direction="received" principal={principal} />
      </p>
      <div ref={box} className="share-frame">
        <video ref={video} aria-label={`${name}'s screen`} playsInline autoPlay muted />
        {tracks.video ? null : <p className="hint">{reconnecting ? 'Waiting for the stream to come back' : 'Waiting for the picture'}</p>}
        {onFocus === undefined ? null : <button type="button" className="call-enlarge" aria-label={`Focus ${name}'s screen`} onClick={onFocus} />}
        {/* Picture-in-picture (#1839): on the picture's corner, not in the
            row below, which is full on a phone (the log keeps its floor,
            #1028). The compact row scrolls rather than wraps, and the
            thumbnail is too small to carry it, so there it is in the row. */}
        {compact ? null : popOut}
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
        {compact ? popOut : null}
        <button type="button" onClick={() => { client.watchStream(principal, false); }}>Stop watching</button>
        {onToggleLayout === undefined ? null : <button type="button" className="call-layout" aria-pressed={compact} onClick={onToggleLayout}>{compact ? 'Expand' : 'Compact'}</button>}
        {blocked && hasSound ? <button type="button" className="primary" onClick={tapToPlay}>Tap to play sound</button> : null}
      </div>
      {pip.failed ? <p role="status" className="hint">Picture-in-picture could not open. Try again when the picture is playing.</p> : null}
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
