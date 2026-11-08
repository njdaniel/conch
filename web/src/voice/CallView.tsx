// The call grid: camera and avatar tiles beside one tile per live stream.
// Watched streams keep the same player whether in the grid, focused or in the strip.
import { useEffect, useRef, useState, type KeyboardEvent as KeyEvent, type ReactElement, type RefObject } from 'react';
import { Avatar, Glyph } from '../Icons';
import { usePhone } from '../phone';
import type { VoiceClient, VoiceState } from './connection';
import { gridOf } from './gridOf';
import type { Occupant } from './presence';

export const maxTiles = 9;

export interface Tile {
  readonly principal: number;
  readonly name: string;
  readonly me: boolean;
  readonly camera: boolean;
  readonly kind?: 'person' | 'share';
  readonly watched?: boolean;
  readonly startedAt?: string;
  readonly render?: (focus: () => void) => ReactElement;
}

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

export const enlargeName = (t: Tile): string => (t.me ? 'Enlarge your video' : `Enlarge ${t.name}'s video`);
export const enlargedWords = (t: Tile): string => (t.me ? 'Your video is enlarged' : `${t.name}'s video is enlarged`);
export const cameraOnWords = (name: string): string => `${name}'s camera is on`;
export const cameraOffWords = (name: string): string => `${name}'s camera is off`;

const keyOf = (t: Tile): string => `${t.kind === 'share' ? 'share' : 'person'}:${String(t.principal)}`;

type FocusTo =
  | { readonly kind: 'main' }
  | { readonly kind: 'tile'; readonly key: string }
  | { readonly kind: 'gone'; readonly key: string }
  | { readonly kind: 'unshared'; readonly key: string };

export const saidFor = 5000;

interface Cameras {
  readonly key: string;
  readonly names: ReadonlyMap<number, string>;
}

function camerasOf(tiles: readonly Tile[]): Cameras {
  const on = tiles.filter((t) => t.kind !== 'share' && !t.me && t.camera);
  return { key: on.map((t) => String(t.principal)).sort().join(','), names: new Map(on.map((t) => [t.principal, t.name])) };
}

function changed(before: Cameras, now: Cameras): string {
  const said: string[] = [];
  for (const [principal, name] of now.names) if (!before.names.has(principal)) said.push(cameraOnWords(name));
  for (const [principal, name] of before.names) if (!now.names.has(principal)) said.push(cameraOffWords(name));
  return said.join('. ');
}

export function CallView({ client, state, tiles, hidden, open, share = null, hideNonVideo = false, onHideNonVideo = () => undefined, keepSaid = false }: {
  readonly client: VoiceClient;
  readonly state: VoiceState;
  readonly tiles: readonly Tile[];
  readonly hidden: number;
  readonly open: boolean;
  readonly share?: ReactElement | null;
  readonly hideNonVideo?: boolean;
  readonly onHideNonVideo?: () => void;
  readonly keepSaid?: boolean;
}) {
  const narrow = usePhone();
  const [selfHidden, setSelfHidden] = useState(false);
  const [enlarged, setEnlarged] = useState<string | null>(null);
  const [focusTo, setFocusTo] = useState<FocusTo | null>(null);
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
    if (enlarged !== null && main === null) {
      setEnlarged(null);
      client.focusShare(null);
      setFocusTo({ kind: 'gone', key: enlarged });
    }
  }

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
    const at = document.activeElement;
    const lost = at === null || at === document.body || (items.current.get(focusTo.key)?.contains(at) ?? false);
    if (!lost) return;
    const key = focusTo.key;
    const move = focusTo.kind === 'gone' ? () => hideButton.current?.focus() : () => (buttons.current.get(key) ?? hideButton.current)?.focus();
    if (document.fullscreenElement) afterFull.current = move;
    else move();
  }, [focusTo]);

  const focusedTile = useRef<string | null>(null);
  useEffect(() => {
    const key = focusedTile.current;
    if (key === null || shown.some((t) => keyOf(t) === key)) return;
    focusedTile.current = null;
    const at = document.activeElement;
    if (open && (at === null || at === document.body)) hideButton.current?.focus();
  }, [shown, open]);

  useEffect(() => {
    if (said === '') return;
    const timer = window.setTimeout(() => { setSaid(''); }, saidFor);
    return () => {
      window.clearTimeout(timer);
    };
  }, [said]);

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

type AppleVideo = HTMLVideoElement & { readonly webkitEnterFullscreen?: () => void };

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
