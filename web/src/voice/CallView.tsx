// The call view (ADR-015, design §7.4): the cameras of the room, drawn on the
// stage (stage.ts) as the watched share is. It is open only while the person
// asked for it, by turning their own camera on or by "Show video (n)", and
// never opens for them when someone else turns a camera on (O8). The room's
// cameras are received only while it is open (connection.ts viewVideo).
//
// While the room has at most four people in voice, each gets a tile: their
// camera, or their initial, their name and "Camera off". The order is the
// order they joined, with the person themself last, and nothing reorders
// while someone speaks. The grid is gridOf's. Video is muted, plays inline
// and starts by itself: a camera has no sound. The person's own tile is
// mirrored, by a class, and "Hide self-view" hides that tile alone.
import { useEffect, useRef, useState } from 'react';
import { Avatar, Glyph } from '../Icons';
import { usePhone } from '../phone';
import type { VoiceClient, VoiceState } from './connection';
import { gridOf } from './gridOf';
import type { Occupant } from './presence';

// How many tiles this view draws: the larger grid, enlarging and the active
// speaker are the next delivery's (C6b).
export const maxTiles = 4;

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
// whose cameras are on, as many as fit. hidden is how many cameras did not.
export function callTiles(roster: readonly Occupant[], viewer: number, ownCamera: boolean, nameOf: (principal: number) => string): { readonly tiles: readonly Tile[]; readonly hidden: number } {
  const ordered = [...roster].sort((a, b) => {
    if ((a.principal === viewer) !== (b.principal === viewer)) return a.principal === viewer ? 1 : -1;
    const at = Date.parse(a.joinedAt) - Date.parse(b.joinedAt);
    return Number.isNaN(at) || at === 0 ? a.principal - b.principal : at;
  });
  const all = ordered.map((o) => {
    const me = o.principal === viewer;
    return { principal: o.principal, name: me ? 'You' : nameOf(o.principal), me, camera: me ? ownCamera : o.camera };
  });
  if (all.length <= maxTiles) return { tiles: all, hidden: 0 };
  const cameras = all.filter((t) => t.camera);
  return { tiles: cameras.slice(0, maxTiles), hidden: Math.max(0, cameras.length - maxTiles) };
}

export function CallView({ client, state, tiles, hidden }: {
  readonly client: VoiceClient;
  readonly state: VoiceState;
  readonly tiles: readonly Tile[];
  readonly hidden: number;
}) {
  const narrow = usePhone();
  const [selfHidden, setSelfHidden] = useState(false);
  const shown = selfHidden ? tiles.filter((t) => !t.me) : tiles;
  const grid = gridOf(shown.length, narrow);
  const hasSelf = tiles.some((t) => t.me);
  return (
    <section aria-label="Video call" className="call-view">
      <div className="call-head">
        <p className="call-title">
          <Glyph name="camera" />
          Video
        </p>
        <div className="call-controls">
          {hasSelf ? (
            <button type="button" aria-pressed={selfHidden} onClick={() => { setSelfHidden(!selfHidden); }}>
              {selfHidden ? 'Show self-view' : 'Hide self-view'}
            </button>
          ) : null}
          <button type="button" onClick={() => { client.viewVideo(false); }}>
            Hide video
          </button>
        </div>
      </div>
      {shown.length === 0 ? (
        <p className="hint">{selfHidden ? 'Your self-view is hidden.' : 'Nobody has a camera on.'}</p>
      ) : (
        <ul className={`call-grid cols-${String(grid.columns)} rows-${String(grid.rows)}`}>
          {shown.map((t) => (
            <li key={t.principal} className="call-tile">
              {t.me ? <SelfTile client={client} on={t.camera} /> : <PersonTile client={client} tile={t} arrived={state.cameraTracks.has(t.principal)} />}
            </li>
          ))}
        </ul>
      )}
      {hidden > 0 ? <p className="hint">{`${String(hidden)} more ${hidden === 1 ? 'camera is' : 'cameras are'} on and not shown here.`}</p> : null}
    </section>
  );
}

// SelfTile is the person's own camera, from the local track, mirrored.
function SelfTile({ client, on }: { readonly client: VoiceClient; readonly on: boolean }) {
  const video = useRef<HTMLVideoElement>(null);
  useEffect(() => {
    if (!on) return;
    client.attachSelf(video.current);
    return () => {
      client.attachSelf(null);
    };
  }, [client, on]);
  return (
    <figure className="call-frame">
      {on ? <video ref={video} className="mirrored" aria-label="Your camera" muted playsInline autoPlay /> : <CameraOff principal={0} name="You" />}
      <figcaption className="call-name">You</figcaption>
    </figure>
  );
}

// PersonTile is someone else's camera, received only while it is claimed and
// this view is open; or their initial and "Camera off".
function PersonTile({ client, tile, arrived }: { readonly client: VoiceClient; readonly tile: Tile; readonly arrived: boolean }) {
  const video = useRef<HTMLVideoElement>(null);
  const on = tile.camera;
  useEffect(() => {
    if (!on) return;
    client.attachCamera(tile.principal, video.current);
    return () => {
      client.attachCamera(tile.principal, null);
    };
  }, [client, on, tile.principal]);
  return (
    <figure className="call-frame">
      {on ? (
        <>
          <video ref={video} aria-label={`${tile.name}'s camera`} muted playsInline autoPlay />
          {arrived ? null : <p className="hint call-waiting">Waiting for the picture</p>}
        </>
      ) : (
        <CameraOff principal={tile.principal} name={tile.name} />
      )}
      <figcaption className="call-name">{tile.name}</figcaption>
    </figure>
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
