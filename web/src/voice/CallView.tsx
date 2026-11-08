// The call view (ADR-015, design §7.4, §7.7; multi-share
// voice-multi-share.md §7.1, issue #1509): the room's cameras and one tile per
// live stream, drawn on the stage (stage.ts). For cameras it is open only while
// the person asked for it, by turning their own camera on or by "Show video
// (n)", and never opens for them when someone else turns a camera on (O8). A
// live stream opens the grid to offer its Watch button, but no stream is
// received until the person asks to watch it. The room's cameras are received
// only while the view is open (connection.ts viewVideo).
//
// While there are at most nine camera and stream tiles, each person in voice
// gets a tile: their camera, or their initial, their name and "Camera off" in a
// compact frame. "Hide non-video participants" leaves out every camera-off
// tile, the viewer's included, and is remembered in this browser. People keep
// join order, with the viewer last, and streams keep claim-start order; nothing
// reorders while someone speaks: the speaking person's tile gets a ring and the
// word "speaking", and focus stays where it was. The grid is gridOf's. Camera
// video is muted, plays inline and starts by itself: a camera has no sound. The
// person's own tile is mirrored, by a class, and "Hide self-view" hides that
// tile alone.
//
// A tile with a camera is a button, "Enlarge {name}'s video"; a watched stream
// has "Focus {name}'s screen". The tile takes the main area, the rest go in a
// strip, and focus moves to it. Escape, or "Back to grid", returns, with focus
// back on the tile it came from. An enlarged camera has Full screen, as a share
// has; inside full screen Escape is the browser's, and leaves full screen only.
// If the enlarged tile goes, the view returns to the grid, and focus that was
// on the tile goes to Hide video.
//
// A focused stream is the main picture and the other tiles are the strip. A
// watched stream keeps the same keyed element whether in the grid, focused or
// in the strip, so moving it never starts it again or loses its volume and
// mute. While the view is closed only the focused watched stream is drawn, and
// the wrappers around it take no part in the layout. On a phone one stream is
// watched and focused at a time, with a switcher between sharers.
//
// A polite live region says someone else's camera starting or stopping while
// the view is open. If filtering the last camera closes it, the stopping words
// stay until their usual timeout. The person's own camera is CameraNote's
// (VoicePanel.tsx), and is not said twice.
import { useEffect, useRef, useState, type KeyboardEvent as KeyEvent, type ReactElement, type RefObject } from 'react';
import { Avatar, Glyph } from '../Icons';
import { usePhone } from '../phone';
import type { VoiceClient, VoiceState } from './connection';
import { gridOf } from './gridOf';
import type { Occupant } from './presence';

// How many tiles this view draws: camera and stream tiles share the grid cap,
// which matches the room's camera cap (voiceCameraCap,
// internal/server/voice_camera.go; voice-multi-share.md §7.1).
export const maxTiles = 9;

// Tile is one person or live stream in the grid. A share supplies its own
// player through render; watched and startedAt carry its subscription and
// ordering state (voice-multi-share.md §7.1, issue #1509).
export interface Tile {
  readonly principal: number;
  readonly name: string;
  readonly me: boolean;
  // Whether the person holds a camera claim (presence); for the person
  // themself, whether their own track is captured, which is what the page
  // shows them (S5).
  readonly camera: boolean;
  readonly kind?: 'person' | 'share';
  readonly watched?: boolean;
  readonly startedAt?: string;
  readonly render?: (focus: () => void) => ReactElement;
}

// callTiles is who gets a person tile: everyone in voice while they and the
// stream tiles fit within maxTiles, in join order with the viewer last; with
// more, the people whose cameras are on, as many as fit beside the streams.
// hideNonVideo leaves out every camera-off person first. hidden is how many
// cameras did not fit (voice-multi-share.md §7.1, issue #1509).
export function callTiles(roster: readonly Occupant[], viewer: number, ownCamera: boolean, nameOf: (principal: number) => string, viewerName = '', hideNonVideo = false, streamCount = 0): { readonly tiles: readonly Tile[]; readonly hidden: number } {
  const ordered = [...roster].sort((a, b) => {
    if ((a.principal === viewer) !== (b.principal === viewer)) return a.principal === viewer ? 1 : -1;
    const at = Date.parse(a.joinedAt) - Date.parse(b.joinedAt);
    return Number.isNaN(at) || at === 0 ? a.principal - b.principal : at;
  });
  const all = ordered.map((o) => {
    const me = o.principal === viewer;
    return { principal: o.principal, name: me ? (viewerName === '' ? 'You' : viewerName) : nameOf(o.principal), me, camera: me ? ownCamera : o.camera };
  });
  const eligible = hideNonVideo ? all.filter((t) => t.camera) : all;
  const limit = Math.max(0, maxTiles - streamCount);
  if (eligible.length <= limit) return { tiles: eligible, hidden: 0 };
  const cameras = eligible.filter((t) => t.camera);
  return { tiles: cameras.slice(0, limit), hidden: Math.max(0, cameras.length - limit) };
}

// The words of a tile and its states.
export const enlargeName = (t: Tile): string => (t.me ? 'Enlarge your video' : `Enlarge ${t.name}'s video`);
export const enlargedWords = (t: Tile): string => (t.me ? 'Your video is enlarged' : `${t.name}'s video is enlarged`);
export const cameraOnWords = (name: string): string => `${name}'s camera is on`;
export const cameraOffWords = (name: string): string => `${name}'s camera is off`;

// A person and their stream have distinct keys despite sharing a principal id.
const keyOf = (t: Tile): string => `${t.kind === 'share' ? 'share' : 'person'}:${String(t.principal)}`;

// Where focus goes once the view has drawn: the enlarged tile; the enlarge
// button of the tile it came from; Hide video, when the enlarged tile went
// with focus in it; or the enlarge button of the tile that was enlarged,
// when the share beside it went with focus in it.
type FocusTo =
  | { readonly kind: 'main' }
  | { readonly kind: 'tile'; readonly key: string }
  | { readonly kind: 'gone'; readonly key: string }
  | { readonly kind: 'unshared'; readonly key: string };

// How long the live region keeps what it said.
export const saidFor = 5000;

// Others' cameras that are on, by principal, as one key that changes when
// the set does.
interface Cameras {
  readonly key: string;
  readonly names: ReadonlyMap<number, string>;
}

function camerasOf(tiles: readonly Tile[]): Cameras {
  const on = tiles.filter((t) => t.kind !== 'share' && !t.me && t.camera);
  return { key: on.map((t) => String(t.principal)).sort().join(','), names: new Map(on.map((t) => [t.principal, t.name])) };
}

// changed says what changed between two sets of cameras, in the order the
// tiles are in.
function changed(before: Cameras, now: Cameras): string {
  const said: string[] = [];
  for (const [principal, name] of now.names) if (!before.names.has(principal)) said.push(cameraOnWords(name));
  for (const [principal, name] of before.names) if (!now.names.has(principal)) said.push(cameraOffWords(name));
  return said.join('. ');
}

// CallView draws camera and stream tiles, preserving players and focus as
// tiles move between the grid, main area and strip (design §7.4, §7.7).
export function CallView({ client, state, tiles, hidden, open, share = null, hideNonVideo = false, onHideNonVideo = () => undefined, keepSaid = false }: {
  readonly client: VoiceClient;
  readonly state: VoiceState;
  readonly tiles: readonly Tile[];
  readonly hidden: number;
  // Whether the call view is shown. While it is not, only a watched stream is drawn.
  readonly open: boolean;
  // The legacy watched share (ShareView), or null; multi-share tiles carry their own.
  readonly share?: ReactElement | null;
  readonly hideNonVideo?: boolean;
  readonly onHideNonVideo?: () => void;
  // Keep the last camera-off announcement when filtering closed the view.
  readonly keepSaid?: boolean;
}) {
  const narrow = usePhone();
  const [selfHidden, setSelfHidden] = useState(false);
  // The enlarged tile, by a key that distinguishes a person from their stream.
  const [enlarged, setEnlarged] = useState<string | null>(null);
  const [focusTo, setFocusTo] = useState<FocusTo | null>(null);
  // The live region's words, and the cameras they were last said from: null
  // while the view is closed, so nothing is said then, nor on opening.
  const [said, setSaid] = useState('');
  const [cameras, setCameras] = useState<Cameras | null>(null);
  const [wasOpen, setWasOpen] = useState(open);

  const hideButton = useRef<HTMLButtonElement>(null);
  const mainFrame = useRef<HTMLElement>(null);
  const buttons = useRef(new Map<string, HTMLButtonElement>());
  const items = useRef(new Map<string, HTMLLIElement>());

  const shown = !open ? tiles.filter((t) => t.kind === 'share' && t.watched === true && (state.focusedShare === null || state.focusedShare === t.principal)) : selfHidden ? tiles.filter((t) => !t.me || t.kind === 'share') : tiles;
  const watched = share !== null;
  const [wasWatched, setWasWatched] = useState(watched);
  const main = enlarged === null
    ? narrow ? (shown.find((t) => t.kind === 'share' && t.watched === true && state.focusedShare === t.principal) ?? null) : null
    : (shown.find((t) => keyOf(t) === enlarged && (t.kind === 'share' ? t.watched === true : t.camera)) ?? null);

  // Stop watching the legacy share leaves the grid, whatever was enlarged
  // beside it: focus, gone with the share's own button, goes back to that tile.
  if (wasWatched !== watched) {
    setWasWatched(watched);
    if (!watched && enlarged !== null) {
      setEnlarged(null);
      setFocusTo({ kind: 'unshared', key: enlarged });
    }
  }

  if (wasOpen !== open) {
    setWasOpen(open);
    if (open && said !== '') setSaid('');
  }

  // Closing the view forgets it all; the next opening starts at the grid. An
  // automatic close after the last camera went keeps that announcement.
  if (!open) {
    if (enlarged !== null) setEnlarged(null);
    if (selfHidden) setSelfHidden(false);
    if (cameras !== null) setCameras(null);
    if (said !== '' && !keepSaid) setSaid('');
  } else {
    const now = camerasOf(tiles);
    if (cameras === null) setCameras(now);
    else if (cameras.key !== now.key) {
      setCameras(now);
      const words = changed(cameras, now);
      if (words !== '') setSaid(words);
    }
    // The enlarged tile went (its camera, its person, its stream or the
    // self-view): back to the grid. The live region has said why, for a camera.
    if (enlarged !== null && main === null) {
      setEnlarged(null);
      client.focusShare(null);
      setFocusTo({ kind: 'gone', key: enlarged });
    }
  }

  // A focus that waits for full screen to end: the browser drops focus as it
  // leaves full screen, so focus moved before then would be lost.
  const afterFull = useRef<(() => void) | null>(null);
  useEffect(() => {
    const onChange = () => {
      const then = afterFull.current;
      if (then === null || document.fullscreenElement) return;
      afterFull.current = null;
      then();
    };
    document.addEventListener('fullscreenchange', onChange);
    return () => {
      document.removeEventListener('fullscreenchange', onChange);
    };
  }, []);

  useEffect(() => {
    if (focusTo === null) return;
    setFocusTo(null);
    if (focusTo.kind === 'main') {
      const target = main?.kind === 'share' ? items.current.get(keyOf(main)) : mainFrame.current;
      target?.focus();
      return;
    }
    if (focusTo.kind === 'tile') {
      (buttons.current.get(focusTo.key) ?? hideButton.current)?.focus();
      return;
    }
    // Only when focus went with the tile or the share: someone typing
    // elsewhere keeps their place.
    const at = document.activeElement;
    const lost = at === null || at === document.body || (items.current.get(focusTo.key)?.contains(at) ?? false);
    if (!lost) return;
    const key = focusTo.key;
    const move = focusTo.kind === 'gone' ? () => hideButton.current?.focus() : () => (buttons.current.get(key) ?? hideButton.current)?.focus();
    // The enlarged tile was full screen when it went: it leaves full screen
    // (TileView), and focus moves once it has.
    if (document.fullscreenElement) afterFull.current = move;
    else move();
  }, [focusTo]);

  // A tile that held focus and went (filtered out, its camera off, its person
  // or stream gone), enlarged or not: focus goes to Hide video rather than
  // being lost, unless it already moved somewhere else.
  const focusedTile = useRef<string | null>(null);
  useEffect(() => {
    const key = focusedTile.current;
    if (key === null || shown.some((t) => keyOf(t) === key)) return;
    focusedTile.current = null;
    const at = document.activeElement;
    if (open && (at === null || at === document.body)) hideButton.current?.focus();
  }, [shown, open]);

  // The live region's words go after a while, so that what it said last is
  // not read again as if it were new.
  useEffect(() => {
    if (said === '') return;
    const timer = window.setTimeout(() => { setSaid(''); }, saidFor);
    return () => {
      window.clearTimeout(timer);
    };
  }, [said]);

  // Enlarging a watched stream also selects it for the top layer; enlarging a
  // camera clears that selection (voice-multi-share.md §7.1, #1509).
  const enlarge = (t: Tile) => {
    setEnlarged(keyOf(t));
    client.focusShare(t.kind === 'share' ? t.principal : null);
    setFocusTo({ kind: 'main' });
  };
  const back = () => {
    if (enlarged === null) return;
    setEnlarged(null);
    client.focusShare(null);
    setFocusTo({ kind: 'tile', key: enlarged });
  };
  // Escape returns to the grid. Inside full screen the browser's Escape
  // leaves full screen first, and is not taken from it.
  const onKey = (e: KeyEvent) => {
    if (e.key !== 'Escape' || e.defaultPrevented || main === null || Boolean(document.fullscreenElement)) return;
    e.preventDefault();
    back();
  };

  const focus = main !== null || (open && watched);
  const grid = gridOf(shown.length + (watched ? 1 : 0), narrow);
  const listClass = !open ? 'call-pass' : focus ? 'call-grid focus' : `call-grid cols-${String(grid.columns)} rows-${String(grid.rows)}`;
  const hasSelf = tiles.some((t) => t.kind !== 'share' && t.me);
  const focusedStream = main?.kind === 'share' ? main : null;
  const switchable = tiles.filter((t) => t.kind === 'share' && !t.me);

  return (
    <section aria-label={open ? 'Video call' : undefined} className={open ? 'call-view' : 'call-stage'} onKeyDown={open ? onKey : undefined}>
      {open ? (
        <div className="call-head">
          <p className="call-title">
            <Glyph name="camera" />
            Video
            {main === null ? null : <span className="call-state" title={main.kind === 'share' ? `Watching ${main.name}'s screen` : enlargedWords(main)}>{main.kind === 'share' ? `Watching ${main.name}'s screen` : enlargedWords(main)}</span>}
          </p>
          <div className="call-controls">
            {main === null ? null : <button type="button" onClick={back}>{watched ? 'Back to the share' : 'Back to grid'}</button>}
            <button type="button" aria-pressed={hideNonVideo} onClick={onHideNonVideo}>Hide non-video participants</button>
            {hasSelf ? <button type="button" aria-pressed={selfHidden} onClick={() => { setSelfHidden(!selfHidden); }}>{selfHidden ? 'Show self-view' : 'Hide self-view'}</button> : null}
            <button ref={hideButton} type="button" onClick={() => { client.viewVideo(false); }}>Hide video</button>
          </div>
        </div>
      ) : null}
      {open || said !== '' ? <p role="status" className="vh">{said}</p> : null}
      {narrow && focusedStream !== null ? (
        <label className="call-state">Watching {focusedStream.name}&apos;s screen
          <select aria-label={`Watching ${focusedStream.name}'s screen`} value={focusedStream.principal} onChange={(e) => {
            const principal = Number(e.target.value);
            client.switchStream(principal);
            setEnlarged(`share:${String(principal)}`);
          }}>
            {switchable.map((t) => <option key={t.principal} value={t.principal}>{t.name}</option>)}
          </select>
        </label>
      ) : null}
      <ul className={listClass} role={open ? undefined : 'none'}>
        {watched ? <li key="share" role={open ? undefined : 'none'} className={open ? (main === null ? 'call-share main' : 'call-share in-strip') : undefined}>{share}</li> : null}
        {shown.map((t) => {
          const key = keyOf(t);
          const isMain = main !== null && keyOf(main) === key;
          return (
            <li
              key={key}
              role={open ? undefined : 'none'}
              tabIndex={isMain ? -1 : undefined}
              onFocus={() => { focusedTile.current = key; }}
              ref={(el) => { if (el === null) items.current.delete(key); else items.current.set(key, el); }}
              className={[t.kind === 'share' ? 'call-share' : 'call-tile', main === null ? (focus ? 'in-strip' : '') : isMain ? 'main' : 'in-strip', t.kind !== 'share' && state.speaking.has(t.principal) ? 'speaking' : ''].filter((c) => c !== '').join(' ')}
            >
              {t.kind === 'share' && t.render !== undefined ? t.render(() => { enlarge(t); }) : (
                <TileView
                  client={client}
                  tile={t}
                  arrived={t.me || state.cameraTracks.has(t.principal)}
                  speaking={state.speaking.has(t.principal)}
                  main={isMain}
                  frameRef={isMain ? mainFrame : null}
                  onEnlarge={() => { enlarge(t); }}
                  buttonRef={(el) => { if (el === null) buttons.current.delete(key); else buttons.current.set(key, el); }}
                />
              )}
            </li>
          );
        })}
      </ul>
      {open && shown.length === 0 && !watched ? <p className="hint">{selfHidden ? 'Your self-view is hidden.' : 'Nobody has a camera on.'}</p> : null}
      {open && hidden > 0 ? <p className="hint">{`${String(hidden)} more ${hidden === 1 ? 'camera is' : 'cameras are'} on and not shown here.`}</p> : null}
    </section>
  );
}

// A video element Safari on iPhone can put full screen: it has no
// requestFullscreen for other elements.
type AppleVideo = HTMLVideoElement & { readonly webkitEnterFullscreen?: () => void };

// TileView is one person's tile: their camera (the person's own from the
// local track, mirrored; anyone else's received only while it is claimed and
// this view is open), or their initial and "Camera off". The same element
// whether the tile is in the grid, enlarged or in the strip, so its video is
// never started again by a change of place.
function TileView({ client, tile, arrived, speaking, main, frameRef, onEnlarge, buttonRef }: {
  readonly client: VoiceClient;
  readonly tile: Tile;
  readonly arrived: boolean;
  readonly speaking: boolean;
  readonly main: boolean;
  readonly frameRef: RefObject<HTMLElement | null> | null;
  readonly onEnlarge: () => void;
  readonly buttonRef: (el: HTMLButtonElement | null) => void;
}) {
  const video = useRef<HTMLVideoElement>(null);
  const frame = useRef<HTMLElement>(null);
  const on = tile.camera;
  const { me, principal } = tile;
  useEffect(() => {
    if (!on) return;
    if (me) {
      client.attachSelf(video.current);
      return () => { client.attachSelf(null); };
    }
    client.attachCamera(principal, video.current);
    return () => { client.attachCamera(principal, null); };
  }, [client, on, me, principal]);

  // Leaving full screen, the browser drops focus: it comes back to Full
  // screen, so that the next Escape is the view's, and returns to the grid.
  const fullButton = useRef<HTMLButtonElement>(null);
  useEffect(() => {
    if (!main) return;
    const el = frame.current;
    let full = false;
    const onChange = () => {
      if (document.fullscreenElement === frame.current && frame.current !== null) {
        full = true;
        return;
      }
      if (!full) return;
      full = false;
      const at = document.activeElement;
      if (at === null || at === document.body) fullButton.current?.focus();
    };
    document.addEventListener('fullscreenchange', onChange);
    return () => {
      document.removeEventListener('fullscreenchange', onChange);
      // No longer enlarged (its camera went, or its person) while full
      // screen: the browser would go on showing it. It leaves full screen.
      if (el !== null && document.fullscreenElement === el) void document.exitFullscreen().catch(() => undefined);
    };
  }, [main]);

  const fullscreen = () => {
    const el = frame.current;
    const v: AppleVideo | null = video.current;
    if (el !== null && document.fullscreenEnabled) void el.requestFullscreen().catch(() => undefined);
    else if (v?.webkitEnterFullscreen !== undefined) v.webkitEnterFullscreen();
  };

  const setFrame = (el: HTMLElement | null) => {
    frame.current = el;
    if (frameRef !== null) frameRef.current = el;
  };

  return (
    <>
      <figure ref={setFrame} className={on ? 'call-frame' : 'call-frame off'} tabIndex={main ? -1 : undefined} aria-label={main ? enlargedWords(tile) : undefined}>
        {on ? <video ref={video} className={me ? 'mirrored' : undefined} aria-label={me ? 'Your camera' : `${tile.name}'s camera`} muted playsInline autoPlay /> : <CameraOff principal={me ? 0 : principal} name={tile.name} />}
        {on && !arrived ? <p className="hint call-waiting">Waiting for the picture</p> : null}
        <figcaption className="call-name">{tile.name}</figcaption>
        {speaking ? <span className="call-speaking">speaking</span> : null}
        {on && !main ? <button ref={buttonRef} type="button" className="call-enlarge" aria-label={enlargeName(tile)} onClick={onEnlarge} /> : null}
      </figure>
      {main ? <div className="call-tile-controls"><button ref={fullButton} type="button" onClick={fullscreen}><Glyph name="fullscreen" /><span className="label">Full screen</span></button></div> : null}
    </>
  );
}

function CameraOff({ principal, name }: { readonly principal: number; readonly name: string }) {
  return <div className="call-off"><Avatar id={principal} name={name} /><span>Camera off</span></div>;
}
