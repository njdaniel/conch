// The call view (ADR-015, design §7.4, §7.7): the cameras of the room, drawn
// on the stage (stage.ts) with the watched share. It is open only while the
// person asked for it, by turning their own camera on or by "Show video (n)",
// and never opens for them when someone else turns a camera on (O8). The
// room's cameras are received only while it is open (connection.ts
// viewVideo).
//
// While the room has at most nine people in voice, each gets a tile: their
// camera, or their initial, their name and "Camera off" in a compact frame.
// "Hide non-video participants" leaves out every camera-off tile, the viewer's
// included, and is remembered in this browser. The order is the order they
// joined, with the person themself last, and nothing reorders while someone
// speaks: the speaking person's tile gets a ring and the word "speaking", and
// focus stays where it was. The grid is gridOf's. Video is muted, plays inline
// and starts by itself: a camera has no sound. The person's own tile is
// mirrored, by a class, and "Hide self-view" hides that tile alone.
//
// A tile with a camera is a button, "Enlarge {name}'s video": the tile takes
// the main area, the rest go in a strip, and focus moves to it. Escape, or
// "Back to grid", returns, with focus back on the tile it came from. The
// enlarged tile has Full screen, as the share has; inside full screen Escape
// is the browser's, and leaves full screen only. If the enlarged camera goes,
// or its person leaves, the view returns to the grid, and focus that was on
// the tile goes to Hide video.
//
// While a share is watched the share is the main picture and the cameras are
// the strip; enlarging a camera swaps it with the share. The share is always
// the same element in the same place (the list's first item), so moving it
// never starts it again or loses its volume and mute: only its class changes.
// While the view is closed the share is drawn alone, as before, and the
// wrappers around it take no part in the layout.
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

// How many tiles this view draws: the room's cap on cameras
// (voiceCameraCap, internal/server/voice_camera.go).
export const maxTiles = 9;

export interface Tile {
  readonly principal: number;
  readonly name: string;
  readonly me: boolean;
  // Whether the person holds a camera claim (presence); for the person
  // themself, whether their own track is captured, which is what the page
  // shows them (S5).
  readonly camera: boolean;
}

// callTiles is who gets a tile: everyone in voice while there are at most
// maxTiles, in the order they joined, the viewer last; with more, the people
// whose cameras are on, as many as fit. hideNonVideo leaves out every person
// whose camera is off first. hidden is how many cameras did not fit.
export function callTiles(roster: readonly Occupant[], viewer: number, ownCamera: boolean, nameOf: (principal: number) => string, viewerName = '', hideNonVideo = false): { readonly tiles: readonly Tile[]; readonly hidden: number } {
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
  if (eligible.length <= maxTiles) return { tiles: eligible, hidden: 0 };
  const cameras = eligible.filter((t) => t.camera);
  return { tiles: cameras.slice(0, maxTiles), hidden: Math.max(0, cameras.length - maxTiles) };
}

// The words of a tile and its states.
export const enlargeName = (t: Tile): string => (t.me ? 'Enlarge your video' : `Enlarge ${t.name}'s video`);
export const enlargedWords = (t: Tile): string => (t.me ? 'Your video is enlarged' : `${t.name}'s video is enlarged`);
export const cameraOnWords = (name: string): string => `${name}'s camera is on`;
export const cameraOffWords = (name: string): string => `${name}'s camera is off`;

// Where focus goes once the view has drawn: the enlarged tile; the enlarge
// button of the tile it came from; Hide video, when the enlarged tile went
// with focus in it; or the enlarge button of the tile that was enlarged,
// when the share beside it went with focus in it.
type FocusTo =
  | { readonly kind: 'main' }
  | { readonly kind: 'tile'; readonly principal: number }
  | { readonly kind: 'gone'; readonly principal: number }
  | { readonly kind: 'unshared'; readonly principal: number };

// How long the live region keeps what it said.
export const saidFor = 5000;

// Others' cameras that are on, by principal, as one key that changes when
// the set does.
interface Cameras {
  readonly key: string;
  readonly names: ReadonlyMap<number, string>;
}

function camerasOf(tiles: readonly Tile[]): Cameras {
  const on = tiles.filter((t) => !t.me && t.camera);
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

export function CallView({ client, state, tiles, hidden, open, share, hideNonVideo = false, onHideNonVideo = () => undefined, keepSaid = false }: {
  readonly client: VoiceClient;
  readonly state: VoiceState;
  readonly tiles: readonly Tile[];
  readonly hidden: number;
  // Whether the call view is shown. While it is not, only the share is drawn.
  readonly open: boolean;
  // The watched share (ShareView), or null.
  readonly share: ReactElement | null;
  readonly hideNonVideo?: boolean;
  readonly onHideNonVideo?: () => void;
  // Keep the last camera-off announcement when filtering closed the view.
  readonly keepSaid?: boolean;
}) {
  const narrow = usePhone();
  const [selfHidden, setSelfHidden] = useState(false);
  // The enlarged tile, by its person.
  const [enlarged, setEnlarged] = useState<number | null>(null);
  const [focusTo, setFocusTo] = useState<FocusTo | null>(null);
  // The live region's words, and the cameras they were last said from: null
  // while the view is closed, so nothing is said then, nor on opening.
  const [said, setSaid] = useState('');
  const [cameras, setCameras] = useState<Cameras | null>(null);
  const [wasOpen, setWasOpen] = useState(open);

  const hideButton = useRef<HTMLButtonElement>(null);
  const mainFrame = useRef<HTMLElement>(null);
  const buttons = useRef(new Map<number, HTMLButtonElement>());
  const items = useRef(new Map<number, HTMLLIElement>());

  const shown = !open ? [] : selfHidden ? tiles.filter((t) => !t.me) : tiles;
  const watched = share !== null;
  const [wasWatched, setWasWatched] = useState(watched);
  const main = enlarged === null ? null : (shown.find((t) => t.principal === enlarged && t.camera) ?? null);

  // Stop watching leaves the grid, whatever was enlarged beside the share:
  // focus, gone with the share's own button, goes back to that tile.
  if (wasWatched !== watched) {
    setWasWatched(watched);
    if (!watched && enlarged !== null) {
      setEnlarged(null);
      setFocusTo({ kind: 'unshared', principal: enlarged });
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
    // The enlarged tile went (its camera, or its person, or the self-view):
    // back to the grid. The live region has said why, for someone else.
    if (enlarged !== null && main === null) {
      setEnlarged(null);
      setFocusTo({ kind: 'gone', principal: enlarged });
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
      mainFrame.current?.focus();
      return;
    }
    if (focusTo.kind === 'tile') {
      (buttons.current.get(focusTo.principal) ?? hideButton.current)?.focus();
      return;
    }
    // Only when focus went with the tile or the share: someone typing
    // elsewhere keeps their place.
    const at = document.activeElement;
    const lost = at === null || at === document.body || (items.current.get(focusTo.principal)?.contains(at) ?? false);
    if (!lost) return;
    const principal = focusTo.principal;
    const move = focusTo.kind === 'gone' ? () => hideButton.current?.focus() : () => (buttons.current.get(principal) ?? hideButton.current)?.focus();
    // The enlarged tile was full screen when its camera went: it leaves full
    // screen (TileView), and focus moves once it has.
    if (document.fullscreenElement) afterFull.current = move;
    else move();
  }, [focusTo]);

  // A tile that held focus and went (filtered out, its camera off, its person
  // gone), enlarged or not: focus goes to Hide video rather than being lost,
  // unless it already moved somewhere else.
  const focusedTile = useRef<number | null>(null);
  useEffect(() => {
    const principal = focusedTile.current;
    if (principal === null || shown.some((t) => t.principal === principal)) return;
    focusedTile.current = null;
    const at = document.activeElement;
    if (open && (at === null || at === document.body)) hideButton.current?.focus();
  }, [shown, open]);

  // The live region's words go after a while, so that what it said last is
  // not read again as if it were new.
  useEffect(() => {
    if (said === '') return;
    const timer = window.setTimeout(() => {
      setSaid('');
    }, saidFor);
    return () => {
      window.clearTimeout(timer);
    };
  }, [said]);

  const enlarge = (t: Tile) => {
    setEnlarged(t.principal);
    setFocusTo({ kind: 'main' });
  };
  const back = () => {
    if (enlarged === null) return;
    setEnlarged(null);
    setFocusTo({ kind: 'tile', principal: enlarged });
  };
  // Escape returns to the grid. Inside full screen the browser's Escape
  // leaves full screen first, and is not taken from it.
  const onKey = (e: KeyEvent) => {
    if (e.key !== 'Escape' || e.defaultPrevented || main === null || Boolean(document.fullscreenElement)) return;
    e.preventDefault();
    back();
  };

  const focus = main !== null || (open && watched);
  const grid = gridOf(shown.length, narrow);
  const listClass = !open ? 'call-pass' : focus ? 'call-grid focus' : `call-grid cols-${String(grid.columns)} rows-${String(grid.rows)}`;
  const hasSelf = tiles.some((t) => t.me);

  return (
    <section
      aria-label={open ? 'Video call' : undefined}
      className={open ? 'call-view' : 'call-stage'}
      onKeyDown={open ? onKey : undefined}
    >
      {open ? (
        <div className="call-head">
          <p className="call-title">
            <Glyph name="camera" />
            Video
            {main === null ? null : <span className="call-state" title={enlargedWords(main)}>{enlargedWords(main)}</span>}
          </p>
          <div className="call-controls">
            {main === null ? null : (
              <button type="button" onClick={back}>
                {watched ? 'Back to the share' : 'Back to grid'}
              </button>
            )}
            <button type="button" aria-pressed={hideNonVideo} onClick={onHideNonVideo}>
              Hide non-video participants
            </button>
            {hasSelf ? (
              <button type="button" aria-pressed={selfHidden} onClick={() => { setSelfHidden(!selfHidden); }}>
                {selfHidden ? 'Show self-view' : 'Hide self-view'}
              </button>
            ) : null}
            <button ref={hideButton} type="button" onClick={() => { client.viewVideo(false); }}>
              Hide video
            </button>
          </div>
        </div>
      ) : null}
      {open || said !== '' ? (
        <p role="status" className="vh">
          {said}
        </p>
      ) : null}
      <ul className={listClass} role={open ? undefined : 'none'}>
        {watched ? (
          <li key="share" role={open ? undefined : 'none'} className={open ? (main === null ? 'call-share main' : 'call-share in-strip') : undefined}>
            {share}
          </li>
        ) : null}
        {shown.map((t) => (
          <li
            key={t.principal}
            onFocus={() => { focusedTile.current = t.principal; }}
            ref={(el) => {
              if (el === null) items.current.delete(t.principal);
              else items.current.set(t.principal, el);
            }}
            className={['call-tile', main === null ? (focus ? 'in-strip' : '') : main.principal === t.principal ? 'main' : 'in-strip', state.speaking.has(t.principal) ? 'speaking' : ''].filter((c) => c !== '').join(' ')}
          >
            <TileView
              client={client}
              tile={t}
              arrived={t.me || state.cameraTracks.has(t.principal)}
              speaking={state.speaking.has(t.principal)}
              main={main?.principal === t.principal}
              frameRef={main?.principal === t.principal ? mainFrame : null}
              onEnlarge={() => { enlarge(t); }}
              buttonRef={(el) => {
                if (el === null) buttons.current.delete(t.principal);
                else buttons.current.set(t.principal, el);
              }}
            />
          </li>
        ))}
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
      return () => {
        client.attachSelf(null);
      };
    }
    client.attachCamera(principal, video.current);
    return () => {
      client.attachCamera(principal, null);
    };
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
    if (el !== null && document.fullscreenEnabled) {
      void el.requestFullscreen().catch(() => undefined);
    } else if (v?.webkitEnterFullscreen !== undefined) {
      v.webkitEnterFullscreen();
    }
  };

  const setFrame = (el: HTMLElement | null) => {
    frame.current = el;
    if (frameRef !== null) frameRef.current = el;
  };

  return (
    <>
      <figure ref={setFrame} className={on ? 'call-frame' : 'call-frame off'} tabIndex={main ? -1 : undefined} aria-label={main ? enlargedWords(tile) : undefined}>
        {on ? (
          <video ref={video} className={me ? 'mirrored' : undefined} aria-label={me ? 'Your camera' : `${tile.name}'s camera`} muted playsInline autoPlay />
        ) : (
          <CameraOff principal={me ? 0 : principal} name={tile.name} />
        )}
        {on && !arrived ? <p className="hint call-waiting">Waiting for the picture</p> : null}
        <figcaption className="call-name">{tile.name}</figcaption>
        {speaking ? <span className="call-speaking">speaking</span> : null}
        {on && !main ? <button ref={buttonRef} type="button" className="call-enlarge" aria-label={enlargeName(tile)} onClick={onEnlarge} /> : null}
      </figure>
      {main ? (
        <div className="call-tile-controls">
          <button ref={fullButton} type="button" onClick={fullscreen}>
            <Glyph name="fullscreen" />
            <span className="label">Full screen</span>
          </button>
        </div>
      ) : null}
    </>
  );
}

function CameraOff({ principal, name }: { readonly principal: number; readonly name: string }) {
  return (
    <div className="call-off">
      <Avatar id={principal} name={name} />
      <span>Camera off</span>
    </div>
  );
}
