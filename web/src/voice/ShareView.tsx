// The watch view (issue #444; multi-share voice-multi-share.md §7.1, issue
// #1509): one watched stream, with its sound on a volume and a mute of its own,
// apart from voice and every other watched stream. It is open only while this
// page watches that stream. An unfocused stream is held to the lowest layer;
// the focused one may take the top layer, which the SDK sizes to this view
// (adaptive stream), so a phone gets a smaller layer than a full-screen
// desktop, and a closed view gets none (voice-multi-share.md §8). It asks for
// no device.
import { useCallback, useEffect, useRef, useState, useSyncExternalStore, type MouseEvent, type PointerEvent } from 'react';
import { Glyph } from '../Icons';
import { platform } from '../platform';
import { fullscreenVideo } from './fullscreen';
import { usePictureInPicture } from './pictureInPicture';
import type { ShareSurface, VoiceClient, VoiceState } from './connection';
import { askedText, nextShortfall, noShortfall, shareReadout, type ShortfallState } from './sdk';
import type { SettledQuality, ShareDirection, ShareSample } from './share';

// What the watch view needs of the client: the desktop's strip (issue #1582)
// gives it the companion's, which is a narrower thing than the call view's.
export type ShareViewClient = Pick<VoiceClient, 'attachStream' | 'readShareStats' | 'watchStream'>;

// How long the controls over the picture stay after the last pointer move.
export const controlsIdleMs = 3000;

const cpuWarning = 'Your computer is too busy to send smoothly. Close heavy programs or choose a lower share resolution.';

// ShareReadout samples only the selected watched picture or the local screen's
// top layer. The first sample has no bitrate; the next, about a second later,
// does. Given asked (the sharer's own line), it says what was asked first, the
// measurement after, and, once the picture has been short of it for 5 s, one
// reason, until it has been back on target for 5 s. Loss shows when the engine
// reports it. The sending strip also warns after 3 s of CPU limitation.
// Every number is the browser's measurement; a stat the engine does not give
// is left out. The readout is a status line with live announcements off.
export function ShareReadout({ client, direction, principal, prefix = '', asked, now = () => performance.now() }: {
  readonly client: Pick<VoiceClient, 'readShareStats'>;
  readonly direction: ShareDirection;
  readonly principal?: number;
  readonly prefix?: string;
  readonly asked?: SettledQuality;
  // The clock for the quality and CPU holds, in ms.
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
    let cpu: { publication: ShareSample['publication']; since: number } | null = null;
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
      if (direction === 'sent') {
        // Missed polls interrupt the initial hold, but an active warning
        // needs a new sample to confirm recovery or a replaced publication.
        if (sample === null) {
          if (!warned) cpu = null;
        } else {
          if (sample.limitation !== 'cpu') cpu = null;
          else if (cpu === null || sample.publication !== cpu.publication) cpu = { publication: sample.publication, since: at };
          const limited = cpu !== null && at - cpu.since >= 3000;
          if (limited && !warned) platform.services.reportVoiceDiagnostic?.('encoder-cpu');
          warned = limited;
          setCpuLimited(limited);
        }
      }
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
  const readout = text === null ? null : (
    <span className="share-readout" role="status" aria-live="off" aria-label="Share quality">
      {asked === undefined ? null : <span>{askedText(asked)}. </span>}
      <span>{prefix}{text}</span>
      {reason === null ? null : <span>. {reason}.</span>}
    </span>
  );
  if (direction === 'received') return readout;
  return (
    <>
      {readout === null ? null : <p className="hint voice-supplemental">{readout}</p>}
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
  // The controls drawn over the picture (single share, open call view: CSS
  // says where) fade after controlsIdleMs without a pointer on the share, and
  // return on a pointer move or a tap. Keyboard focus inside them keeps them
  // (CSS :focus-visible; a control pressed by a pointer keeps focus but not
  // the row).
  const [idle, setIdle] = useState(false);
  // The press that brings a faded row back is a pure wake: it presses no
  // control, enlarges nothing and focuses nothing. Its click is swallowed
  // whatever it lands on (on touch the row would otherwise be pressable before
  // that tap's click is aimed), and the row takes no press while it fades in
  // (CSS .waking), so a quick second tap is not a control's either.
  const [waking, setWaking] = useState(false);
  const idleTimer = useRef<number | null>(null);
  // Whether the row is faded as far as the last event knows: set at once,
  // where the state above waits for a render, so a press right after the
  // pointer moved onto the share (which woke it) is not taken for a waking one.
  const idleNow = useRef(false);
  const wake = useCallback(() => {
    idleNow.current = false;
    setIdle(false);
    if (idleTimer.current !== null) window.clearTimeout(idleTimer.current);
    idleTimer.current = window.setTimeout(() => { idleNow.current = true; setIdle(true); }, controlsIdleMs);
  }, []);
  const swallow = useRef(false);
  const pointerDown = useRef(false);
  const controls = useRef<HTMLDivElement>(null);
  const stillWaking = useRef(false);
  const wakingTimer = useRef<number | null>(null);
  const swallowTimer = useRef<number | null>(null);
  const afterMs = (timer: { current: number | null }, ms: number, then: () => void) => {
    if (timer.current !== null) window.clearTimeout(timer.current);
    timer.current = window.setTimeout(then, ms);
  };
  const pressDown = (e: PointerEvent) => {
    // The pop-out is not in the row and never fades: its press is its own.
    const own = e.target instanceof Element && e.target.closest('.share-pip') !== null;
    // Only where the row really is faded: the row fades in the single-share
    // overlay alone (CSS says where), so what the page computes for it, not
    // the idle timer, decides; elsewhere the first press is the control's.
    const row = controls.current;
    const inert = row !== null && getComputedStyle(row).pointerEvents === 'none';
    if (!own && ((faded && idleNow.current && inert) || stillWaking.current)) {
      swallow.current = true;
      stillWaking.current = true;
      pointerDown.current = true;
      setWaking(true);
      afterMs(wakingTimer, 300, () => { stillWaking.current = false; setWaking(false); });
      // Backstop: a press whose click never comes is forgotten, but not while
      // the pointer is still down (a long hold is still one press).
      afterMs(swallowTimer, 1000, () => { if (!pointerDown.current) swallow.current = false; });
    }
    wake();
  };
  // The click of a waking press is swallowed here, before any control, the
  // focus button or the picture sees it.
  const swallowClick = (e: MouseEvent) => {
    if (!swallow.current) return;
    swallow.current = false;
    e.stopPropagation();
    e.preventDefault();
  };
  // A press released anywhere, outside the share too, ends its wait for a click.
  useEffect(() => {
    const released = () => {
      pointerDown.current = false;
      if (swallow.current) afterMs(swallowTimer, 300, () => { swallow.current = false; });
    };
    // A cancelled press (a drag, a lost pointer) is followed by no click.
    const cancelled = () => {
      pointerDown.current = false;
      swallow.current = false;
    };
    window.addEventListener('pointerup', released);
    window.addEventListener('pointercancel', cancelled);
    window.addEventListener('blur', cancelled);
    return () => {
      window.removeEventListener('pointerup', released);
      window.removeEventListener('pointercancel', cancelled);
      window.removeEventListener('blur', cancelled);
    };
  }, []);
  useEffect(() => {
    wake();
    return () => {
      if (idleTimer.current !== null) window.clearTimeout(idleTimer.current);
      if (wakingTimer.current !== null) window.clearTimeout(wakingTimer.current);
      if (swallowTimer.current !== null) window.clearTimeout(swallowTimer.current);
    };
  }, [wake]);

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
  // The controls stay shown while the share is not playing: waiting for the
  // picture or the stream, or for the tap that starts the sound.
  const faded = idle && tracks.video && !reconnecting && !(blocked && hasSound);
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
    <section aria-label="Shared screen" className={`share-view${faded ? ' idle' : ''}${waking ? ' waking' : ''}`} onPointerMove={wake} onPointerDown={pressDown} onClickCapture={swallowClick}>
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
        {/* The tap that starts the sound is the picture's, centred on it; the
            compact row keeps it among its controls. */}
        {!compact && blocked && hasSound ? <button type="button" className="primary share-tap" onClick={tapToPlay}>Tap to play sound</button> : null}
      </div>
      <audio ref={sound} autoPlay aria-label={`${name}'s stream audio`} />
      <div ref={controls} className="share-controls">
        <button type="button" aria-pressed={muted} onClick={() => { setMuted(!muted); }}>
          <Glyph name={muted ? 'speakerOff' : 'speaker'} />
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
        <button type="button" className="stop" onClick={() => { client.watchStream(principal, false); }}>
          <Glyph name="close" />
          <span className="label">Stop watching</span>
        </button>
        {onToggleLayout === undefined ? null : <button type="button" className="call-layout" aria-pressed={compact} onClick={onToggleLayout}>{compact ? 'Expand' : 'Compact'}</button>}
        {compact && blocked && hasSound ? <button type="button" className="primary" onClick={tapToPlay}>Tap to play sound</button> : null}
      </div>
      {pip.failed ? <p role="status" className="hint">Picture-in-picture could not open. Try again when the picture is playing.</p> : null}
      {state.deafened && hasSound ? <p className="hint">Deafened: the share&apos;s sound is off too.</p> : null}
    </section>
  );
}

// Whether the sharer hides the preview of their own screen (#2197): kept for
// this page session, in memory, not stored. One sharer tile exists at a time,
// but it moves between the grid, the strip and the stage, which draw it again,
// so the choice is the module's and not the tile's.
let previewHidden = false;
const previewListeners = new Set<() => void>();
const setPreviewHidden = (hidden: boolean): void => {
  previewHidden = hidden;
  for (const listener of previewListeners) listener();
};
// Whether the preview is hidden now: it is the choice of the sharer's tile in
// every layout, so it is read from the page session's one place.
export function useSharePreviewHidden(): boolean {
  return useSyncExternalStore(
    (listener) => {
      previewListeners.add(listener);
      return () => { previewListeners.delete(listener); };
    },
    () => previewHidden,
  );
}
// For tests: the page session starts again.
export function resetSharePreview(): void {
  setPreviewHidden(false);
}

// What the sharer's tile needs of the client.
export type SelfShareClient = Pick<VoiceClient, 'attachShareSelf' | 'stopShare' | 'shareSurface'>;

// What the capture is of, in the sharer's title: the thumbnail is too small to
// tell two similar windows apart, so the title says the kind of surface. Left
// out when the browser does not say.
export function surfaceWords(surface: ShareSurface | null): string | null {
  switch (surface) {
    case 'monitor': return 'your entire screen';
    case 'window': return 'a window';
    case 'browser': return 'a tab';
    default: return null;
  }
}

// SelfShareView is the sharer's own tile (#2197): a small live preview of the
// outgoing screen beside the title and the controls, so they can see what they
// are sharing. The picture is the raw local capture, muted (the sharing sound
// is never played back) and with no subscription or upload of its own; the
// client follows it through the capture's stopping or replacing, so no frame is
// left after Stop sharing. The title names the kind of surface shared. Hide
// preview collapses the tile to the title and Stop sharing. None of a watched
// share's controls (mute, volume, full screen, pop out) applies to one's own
// share, so none wakes or fades. The tile is the container its toggle is sized
// by: icon alone when it is narrow (CSS).
export function SelfShareView({ client, quality, compact = false }: {
  readonly client: SelfShareClient;
  // In the compact strip the toggle is an icon (its name is the same, its
  // title the words); a narrow tile does the same by its own width (CSS), so
  // that the row does not wrap.
  readonly compact?: boolean;
  // The declared quality, as in the watched title.
  readonly quality?: string | null;
}) {
  const hidden = useSharePreviewHidden();
  const video = useRef<HTMLVideoElement>(null);
  useEffect(() => {
    if (hidden) return;
    client.attachShareSelf(video.current);
    return () => { client.attachShareSelf(null); };
  }, [client, hidden]);
  const label = hidden ? 'Show preview' : 'Hide preview';
  const surface = surfaceWords(client.shareSurface());
  const words = `You · LIVE${surface === null ? '' : ` · ${surface}`}${quality === null || quality === undefined ? '' : ` · ${quality}`}`;
  return (
    <section aria-label="Your stream" className={`share-view own${hidden ? '' : ' self'}`}>
      <p className="share-title">
        <Glyph name="screen" />
        <span className="share-words" title={words}>{words}</span>
      </p>
      {hidden ? null : (
        <div className="share-frame">
          <video ref={video} aria-label="Preview of your screen" playsInline autoPlay muted />
        </div>
      )}
      <div className="share-controls">
        <button type="button" onClick={() => { client.stopShare(); }}>Stop sharing</button>
        <button type="button" className={`preview-toggle${compact ? ' icon' : ''}`} aria-label={`${label} of your screen`} title={label} onClick={() => { setPreviewHidden(!hidden); }}>
          <Glyph name={hidden ? 'eye' : 'eyeOff'} />
          <span className="words">{label}</span>
        </button>
      </div>
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
