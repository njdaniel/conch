import { useBlockedVoiceMute, useBlocksContext } from '../blocks';
import { ModerationConfirm, useMayModerate, useMayModerateIn } from '../Moderation';
// The voice section. Every state says what is true: not set up, not in voice,
// connecting, connected, reconnecting, unavailable, or stopped with the reason.
// It also holds the screen share (issue #444): sharing, with what sound, why a
// share ended, and watching someone else's.
//
// It is the strip across the bottom of the lobby (issue #536): where you are,
// whether the microphone is captured and whether you transmit, who is in
// voice, and the controls. Less used choices are under Voice options. The
// watch view is drawn into the lobby's stage with a portal, so it is still
// this one component, wherever it shows. While not in voice it also says where
// else people are talking (issue #596), with a way to open that channel: only
// to open it, since joining is always Join voice, here.
//
// Voice options has the choice of microphone (issue #658): System default or
// a device the browser lists, with the device the capture is on. A microphone
// that goes is said in the notes, and nothing is sent until one is chosen.
// It also has the switch for the browser's voice processing (issue #659),
// with what the capture says of it.
//
// It has the choice of speakers too (issue #660), where the browser lets a
// page choose: System default or an output the browser lists, the one in
// use selected. A choice the browser refuses, and speakers that went, are
// said in the notes.
//
// Before joining there is a private microphone test (issue #846): a level
// meter, and the person's own voice played back if they ask, with nothing
// sent. Joining voice ends it.
//
// And a volume for each other person in voice (issue #661), on this page
// only: a slider each, under Voice options, and on a person's chip in the
// roster how loud they are played while it is not 100%.
//
// And the camera (ADR-015, design §7.1): a Camera button beside Mute and
// Share, offered to someone who holds use_camera on a grant that may
// publish, and never in Watch only or Listen only. There is no key for it.
// "Camera in use" and "Sending video" come from the person's own track
// alone (S5); others see "camera" on the person's chip. Voice options has
// the camera picker, under the microphone's, and a private camera test.
// "Show video (n)" opens the call view (CallView.tsx) while anyone in the
// room has a camera on, and it never opens by itself (O8). Its remembered
// "Hide non-video participants" choice can close it when no camera or watched
// share remains; a later camera offers Show video again, without opening it.
// Every live stream is a tile on the stage, with Watch (voice-multi-share.md
// §7.1, #1509): the people's tiles join them only with the person's choice to
// view, and a focused stream is the main picture with every other tile in the
// strip (design §7.4).
import { useCallback, useContext, useEffect, useLayoutEffect, useMemo, useRef, useState, type KeyboardEvent as KeyEvent, type MouseEvent as PressEvent, type ReactNode, type RefObject } from 'react';
import { createPortal } from 'react-dom';
import { readHideNonVideo, writeHideNonVideo } from '../api';
import { archivedLine } from '../archived';
import { Avatar, Glyph } from '../Icons';
import { IdentityMarks } from '../Identity';
import type { ChannelV0 } from '../schema/channel';
import { MicTestShown, onPhoneCamera, useCoarsePointer, usePhone, VoiceLive, VoiceSheet, VoiceStrip } from '../phone';
import { StageHost } from '../stage';
import { MicTest } from './MicTest';
import { cameraNote, cameraOffForYou, cameraOn, listenOnlyCamera, watchOnlyCamera } from './camera';
import { CallView, callTiles, type Tile } from './CallView';
import { CameraTest } from './CameraTest';
import type { Person } from '../useLiveChannel';
import { openAudio, type Phase, type Processing } from './connection';
import { clampTail, maxTail, releasePage, releaseStep } from './gate';
import { keyLabel, talkKeyForMouseButton } from './keys';
import { occupants, qualityLabel, sharers, type Occupant } from './presence';
import { endedNote, ownPageNote, resumedNote, resumingNote, sharedNote, shortShareNote, soundNote, type ShareFrameRate, type ShareResolution } from './share';
import { ShareQualityMenu, autoNote, cautionFor, estimateWords, resolutionWords, shareFrameRates, shareResolutionChoices } from './ShareQualityMenu';
import { SelfShareView, ShareCaptureWarning, ShareReadout, ShareView } from './ShareView';
import { nobody, type InVoice } from './useOccupancy';
import { VoiceDiagnostics } from './VoiceDiagnostics';
import type { VoiceControl } from './useVoice';
import { microphoneCaptured, VoiceStatus } from './VoiceStatus';

// The keys that hold Hold to talk down while it has focus.
function holdKey(e: KeyEvent): boolean {
  return e.key === ' ' || e.key === 'Enter';
}

const phaseLabel: Record<Phase, string> = {
  idle: 'Not in voice',
  connecting: 'Connecting',
  connected: 'Connected',
  reconnecting: 'Reconnecting',
  unavailable: 'Voice unavailable',
  stopped: 'Voice stopped',
};

// nameOf names someone in voice. The viewer is their own name (viewerName,
// from the signed-in identity), or "You" while that is not known; while this
// page is not the one in voice, "<name> (on another device)" (issue #591,
// F24): one person holds one place in a room, so that place is another page's.
export function nameOf(people: ReadonlyMap<number, Person>, id: number, viewer: number, elsewhere = false, viewerName = ''): string {
  if (id === viewer) {
    const own = viewerName === '' ? 'You' : viewerName;
    return elsewhere ? `${own} (on another device)` : own;
  }
  return people.get(id)?.name ?? `Member ${String(id)}`;
}

// Where else people are in voice (issue #596): the channel with the most,
// ties to the lower id, and how many other channels have someone. The channel
// on screen is not one of them: its people are the roster.
export interface Busiest {
  readonly name: string;
  readonly count: number;
  readonly more: number;
}

export function busiest(channels: readonly ChannelV0[], occupancy: InVoice, viewed: string | null): Busiest | null {
  let best: { readonly channel: ChannelV0; readonly count: number } | null = null;
  let found = 0;
  for (const channel of channels) {
    const count = occupancy.get(channel.id)?.count ?? 0;
    if (channel.name === viewed || count <= 0) continue;
    found++;
    if (best === null || count > best.count || (count === best.count && channel.id < best.channel.id)) best = { channel, count };
  }
  return best === null ? null : { name: best.channel.name, count: best.count, more: found - 1 };
}

// How long "<name> stopped sharing." stays (issue #591, F15).
export const stoppedFor = 5000;

// A state beside a name in the roster: a pill on screen, " (speaking)" in the
// text read out.
function Tag({ text }: { readonly text: string }) {
  return (
    <span className="tag">
      <span className="vh"> (</span>
      {text}
      <span className="vh">)</span>
    </span>
  );
}

// Dot is a chip's sending or speaking mark: a dot on the corner of its avatar
// (the avatar's own box, so it stays there however the chip wraps), out of the
// chip's flow, so the chip is the same size with it and without it. It used to
// be a pill after the name, and a pill wider than the room left wrapped the
// chip onto a second line and moved every control below it, Hold to talk under
// a held finger among them (issue #2211). The dot is for the eye; Said is the
// word read out, after the name as before.
function Dot({ text }: { readonly text: 'sending' | 'speaking' }) {
  return <span className={`state ${text}`} aria-hidden="true" />;
}

function Said({ text }: { readonly text: 'sending' | 'speaking' | undefined }) {
  return text === undefined ? null : <span className="vh said"> ({text})</span>;
}

export function VoicePanel({
  voice,
  viewer,
  viewerName = '',
  people,
  viewed,
  channels = [],
  occupancy = nobody,
  cameraAllowed = false,
  archived = false,
  onOpen,
}: {
  readonly voice: VoiceControl;
  readonly viewer: number;
  // The viewer's own name, from the signed-in identity; empty while unknown,
  // when the viewer is "You".
  readonly viewerName?: string;
  readonly people: ReadonlyMap<number, Person>;
  readonly viewed: string | null;
  // The viewer's channels, and how many are in each one's voice: for the line
  // that says where else people are talking.
  readonly channels?: readonly ChannelV0[];
  readonly occupancy?: InVoice;
  // The channel on screen is archived (issue #844): it has no voice. Someone
  // in voice there is ended by the server, and the strip follows.
  readonly archived?: boolean;
  // Whether whoami says the viewer may turn a camera on (use_camera). The
  // server decides each claim; this only offers the button.
  readonly cameraAllowed?: boolean;
  // Opens a channel, as the channel list does: the way back to the one voice
  // is in, from any other.
  readonly onOpen: (name: string) => void;
}) {
  const { state, snapshot, client } = voice;
  const stage = useContext(StageHost);
  const [hideNonVideo, setHideNonVideo] = useState(readHideNonVideo);
  const [focusShow, setFocusShow] = useState(false);
  const showVideoButton = useRef<HTMLButtonElement>(null);
  // Each publisher's volume survives its tile moving or being watched again;
  // one stream's control never changes another's (voice-multi-share.md §7.1).
  const shareVolumes = useRef(new Map<number, number>());
  // More about sharing opens Voice options at the full share notes, and
  // takes focus there once they are shown (issue #600).
  const about = useRef<HTMLElement>(null);
  const [toAbout, setToAbout] = useState(false);
  // Whether the talk key and sharing notes are open (About talking and sharing).
  const [help, setHelp] = useState(false);
  // A touch screen has no talk key to explain (F18).
  const coarse = useCoarsePointer();
  const inVoice = state.phase !== 'idle' && state.phase !== 'stopped';
  // About talking and sharing starts closed on every join: leaving closes it.
  if (!inVoice && help) setHelp(false);
  // The open microphone has no level meter to hear with, while a connection
  // holds the microphone (as VoiceStatus's captured): said in the notes, and
  // in the phone's strip, where the notes are hidden.
  const noMeter =
    (state.phase === 'connected' || state.phase === 'reconnecting') && state.mode === 'open' && !state.meter && !state.listenOnly && !state.watchOnly && !state.missing;
  // The browser holds playback back until a tap (iOS Safari): said by a
  // button, in the notes and in the phone's strip, which hides them.
  const noPlayback = (state.phase === 'connected' || state.phase === 'reconnecting') && !state.playback && !state.companion;
  // The phone layout's Voice button and strip follow it (issue #929).
  const reportLive = useContext(VoiceLive);
  const micTestShown = useContext(MicTestShown);
  // On a phone under Chat and Channels, in voice, the section is the strip of
  // four controls (issue #1028); the rest is in the Voice view, which the
  // strip's state button opens. Nothing is unmounted for it but what the
  // strip does not draw: the notes and Voice options stay, hidden, and the
  // watched share stays on the stage.
  const toVoiceView = useContext(VoiceStrip);
  const stripOpens = inVoice ? toVoiceView : null;
  const strip = stripOpens !== null;
  const narrow = usePhone();
  useEffect(() => {
    client.showVideo(!strip);
  }, [client, strip]);
  useEffect(() => {
    reportLive(inVoice);
    return () => {
      reportLive(false);
    };
  }, [inVoice, reportLive]);
  const presence = snapshot.kind === 'presence' ? snapshot.presence : null;
  // Leave voice takes itself away once the client has drained (up to a
  // second): if it had focus then, focus goes to Join voice rather than to
  // the page's top (issue #590).
  const left = useRef(false);
  const joinButton = useRef<HTMLButtonElement>(null);
  // Whether presence lists the viewer: in voice on some page, this one or
  // another. Presence lags a Leave voice by a few seconds, and until it
  // catches up the viewer listed is this page's old place, not another
  // device's (issue #591). Only a Leave lingers so: a page stopped because
  // another device took over is not in that room any more.
  const listed = occupants(presence).some((o) => o.principal === viewer);
  const [wasIn, setWasIn] = useState(inVoice);
  const [lingers, setLingers] = useState(false);
  if (wasIn !== inVoice) {
    setWasIn(inVoice);
    setLingers(!inVoice && state.phase === 'idle');
  } else if (lingers && !listed) {
    setLingers(false);
  }
  const elsewhere = !inVoice && listed && !lingers;
  // Every live share gets a tile; anyone else's may be watched on demand
  // (voice-multi-share.md §7.1, issue #1509).
  const streams = sharers(presence);
  const theirStreams = streams.filter((s) => s.principal !== viewer);
  // Watched shares that ended (issue #591, F15): their names by publisher, the
  // previous watch set, and the note that a stream stopped, said for stoppedFor.
  const watchedNames = useRef(new Map<number, string>());
  const wasWatching = useRef<ReadonlySet<number>>(new Set());
  const [stopped, setStopped] = useState<{ readonly name: string } | null>(null);

  useEffect(() => {
    if (!voice.capturing) return;
    const onKey = (e: KeyboardEvent) => {
      e.preventDefault();
      if (e.code === 'Escape') voice.capture(false);
      else voice.setTalkKey(e.code);
    };
    const onMouse = (e: MouseEvent) => {
      const key = talkKeyForMouseButton(e.button);
      if (key === null) return;
      e.preventDefault();
      e.stopPropagation();
      voice.setTalkKey(key);
    };
    window.addEventListener('keydown', onKey);
    window.addEventListener('mousedown', onMouse);
    return () => {
      window.removeEventListener('keydown', onKey);
      window.removeEventListener('mousedown', onMouse);
    };
  }, [voice]);

  const { options, setOptions, covered, button: optionButton, panel, onClose, onShrink, sheet, onSheet } = useOptionsDisclosure({
    hidden: strip || !inVoice,
    capturing: voice.capturing,
    toAbout,
  });

  // The talk key capture ends with the panel it was started in, by whatever
  // route the panel closes (outside press, its button, the phone's strip, or
  // leaving voice): otherwise the next key anywhere would set the talk key.
  const shown = options && !strip && inVoice;
  const capturing = voice.capturing;
  const wasShown = useRef(shown);
  useEffect(() => {
    if (wasShown.current && !shown && capturing) voice.capture(false);
    wasShown.current = shown;
  }, [shown, capturing, voice]);

  useEffect(() => {
    if (!toAbout || !options) return;
    setToAbout(false);
    about.current?.focus();
  }, [toAbout, options]);

  useEffect(() => {
    if (state.phase !== 'idle' || !left.current) return;
    left.current = false;
    if (document.activeElement === null || document.activeElement === document.body) joinButton.current?.focus();
  }, [state.phase]);

  // A watch ends without a note when its stream is still live (Stop watching),
  // when this page leaves voice, or while a renewed room is still being waited
  // for. A watched stream that disappears while this page remains in voice has
  // ended: say whose it was. Other streams' state does not affect that decision
  // (issue #591, F15; voice-multi-share.md §7.1, #1509).
  const liveStreams = new Set(streams.map((s) => s.principal));
  // Watches the client still holds while their streams are gone from
  // presence: the room is being renewed (heldTiles below).
  const held = [...state.watched].filter((principal) => principal !== viewer && !liveStreams.has(principal));
  useEffect(() => {
    for (const principal of state.watched) {
      if (liveStreams.has(principal)) watchedNames.current.set(principal, nameOf(people, principal, viewer));
    }
    for (const principal of wasWatching.current) {
      if (state.watched.has(principal) || liveStreams.has(principal) || !inVoice) continue;
      const name = watchedNames.current.get(principal);
      if (name !== undefined) setStopped({ name });
      watchedNames.current.delete(principal);
    }
    wasWatching.current = new Set(state.watched);
  }, [state.watched, liveStreams, inVoice, people, viewer]);

  // A phone plays one stream at a time, focused (voice-multi-share.md §7.1).
  // A page that becomes narrow while it watches several (a window made
  // narrower, a tablet turned) keeps the focused one, or else the first
  // watched in the streams' order, focuses it and lets the others go. Once
  // narrow, a second watch is the switcher's, which ends the first; Back to
  // grid leaves the one stream watched and unfocused, as chosen.
  const wasNarrow = useRef(narrow);
  const streamOrder = streams.map((s) => s.principal).join(',');
  useEffect(() => {
    const entered = narrow && !wasNarrow.current;
    wasNarrow.current = narrow;
    if (!narrow || state.watched.size === 0) return;
    if (!entered && state.watched.size === 1) return;
    const order = streamOrder === '' ? [] : streamOrder.split(',').map(Number);
    const focused = state.focusedShare;
    const keep = focused !== null && state.watched.has(focused) ? focused : (order.find((p) => state.watched.has(p)) ?? [...state.watched][0]);
    if (keep === undefined || (state.watched.size === 1 && focused === keep)) return;
    client.switchStream(keep);
  }, [client, narrow, state.watched, state.focusedShare, streamOrder]);

  useEffect(() => {
    if (stopped === null) return;
    const timer = window.setTimeout(() => {
      setStopped(null);
    }, stoppedFor);
    return () => {
      window.clearTimeout(timer);
    };
  }, [stopped]);

  // The roster is the joined channel's when in voice, else the one on screen:
  // its moderation action, question, request and standing all follow it.
  const rosterChannel = (inVoice ? state.channel : null) ?? viewed;
  const mayModerateRoster = useMayModerateIn(rosterChannel, viewed, channels);

  // Where else people are talking, while this page is not in voice: in voice,
  // the strip describes the viewer's own connection.
  const away = inVoice ? null : busiest(channels, occupancy, viewed);
  const awayLine = away === null ? null : <Elsewhere where={away} onOpen={onOpen} />;

  const videoRoster = inVoice && presence !== null && !presence.available ? [] : occupants(presence);
  const visibleCameraCount = videoRoster.filter((o) => o.principal !== viewer && o.camera).length + (state.cameraTrack === 'published' ? 1 : 0);
  const emptyCall =
    inVoice &&
    state.viewing &&
    !strip &&
    hideNonVideo &&
    streams.length === 0 &&
    held.length === 0 &&
    state.cameraTrack === 'none' &&
    !videoRoster.some((o) => o.principal !== viewer && o.camera);
  useEffect(() => {
    if (!emptyCall) return;
    setFocusShow(true);
    client.viewVideo(false);
  }, [client, emptyCall]);
  useEffect(() => {
    if (!focusShow || visibleCameraCount === 0) return;
    const at = document.activeElement;
    if (at === null || at === document.body) showVideoButton.current?.focus();
    setFocusShow(false);
  }, [focusShow, visibleCameraCount]);

  if (viewed === null && !inVoice) {
    // No channel to show (it was lost, or none is open). A stopped client
    // still says why it stopped.
    return (
      <section aria-label="Voice" className="voice">
        <div className="voice-state">
          <p className="voice-phase">{phaseLabel[state.phase]}</p>
          {awayLine}
          {state.why === null ? null : <p role="status" className="hint">{state.why}</p>}
        </div>
      </section>
    );
  }
  if (!inVoice && archived) {
    return (
      <section aria-label="Voice" className="voice">
        <div className="voice-state">
          <p className="voice-phase">Not in voice</p>
          <p className="hint">Voice is off. {archivedLine}</p>
          {awayLine}
        </div>
      </section>
    );
  }
  if (!inVoice && presence !== null && !presence.configured) {
    return (
      <section aria-label="Voice" className="voice">
        <div className="voice-state">
          <p className="voice-phase">Not in voice</p>
          <p className="hint">Voice is not set up on this server</p>
        </div>
      </section>
    );
  }
  if (!inVoice && snapshot.kind === 'not_allowed') {
    // Nothing to join: the person's access has no voice. A stopped client
    // still says why it stopped.
    return (
      <section aria-label="Voice" className="voice">
        <div className="voice-state">
          <p className="voice-phase">Not in voice</p>
          <p className="hint">Your account does not include voice. Ask whoever invited you.</p>
          {state.why === null ? null : <p role="status" className="hint">{state.why}</p>}
        </div>
      </section>
    );
  }

  // Joined while the server reports voice down (the signalling cut while
  // media runs on, issue #644): presence has nothing true to list, so the
  // roster goes and the strip says so.
  const cutOff = inVoice && presence !== null && !presence.available;
  const roster = cutOff ? [] : occupants(presence).sort((a, b) => a.principal - b.principal);
  const speaking = state.speaking;
  // Only on a live snapshot that says voice is up: not while loading, while
  // the server cannot be reached, or after the socket was closed for good.
  const viewedChannel = channels.find((channel) => channel.name === viewed);
  const canJoin = voice.secure && presence !== null && presence.available && (viewedChannel === undefined || presence.channel_id === viewedChannel.id);
  const silenced = state.muted || state.deafened;
  // One tile per live stream, beside the camera tiles. An unwatched tile only
  // offers Watch; a watched one owns its player, volume and focus control. The
  // viewer's own tile offers Stop sharing (voice-multi-share.md §7.1, #1509).
  const streamTiles: Tile[] = streams.map((stream) => {
    const me = stream.principal === viewer;
    const name = me ? 'You' : nameOf(people, stream.principal, viewer);
    const quality = qualityLabel(stream.quality);
    const watched = state.watched.has(stream.principal);
    return {
      principal: stream.principal,
      name,
      me,
      camera: false,
      kind: 'share',
      watched,
      startedAt: stream.startedAt,
      render: (focus, compact, onToggleLayout) => {
        if (me) return <SelfShareView client={client} quality={quality} compact={compact} />;
        if (!watched) return (
          <section aria-label={`${name}'s stream`} className="share-view">
            <p className="share-title"><Glyph name="screen" />{name} · LIVE{quality === null ? null : <> · {quality}</>}</p>
            <div className="share-controls"><button type="button" disabled={state.phase !== 'connected'} onClick={() => {
              // A phone plays one stream at a time, focused: Watch there is
              // the switcher's step (voice-multi-share.md §7.1).
              if (narrow) client.switchStream(stream.principal);
              else client.watchStream(stream.principal, true);
            }}>Watch</button></div>
          </section>
        );
        return <ShareView client={client} state={state} principal={stream.principal} name={name} audio={stream.sharingAudio} quality={quality} volume={shareVolumes.current.get(stream.principal) ?? 1} onVolumeChange={(volume) => { shareVolumes.current.set(stream.principal, volume); }} onFocus={focus} compact={compact} onToggleLayout={onToggleLayout} />;
      },
    };
  });
  // A watch the client still holds while its stream is gone from presence:
  // the room is being renewed, and the client waits for the same publisher's
  // stream (connection.ts awaitShare, holdEnding; #1509, #1930). Its tile, and
  // the video element in it, stay as they were, saying it is reconnecting:
  // the next room plays the stream into that same element, so a picture
  // popped out stays popped out, which the browser lets a page do again only
  // from a tap (#1839). The tile goes when the watch does: the wait running
  // out, or Stop watching.
  const heldTiles: Tile[] = held.map((principal) => {
    const name = nameOf(people, principal, viewer);
    return {
      principal,
      name,
      me: false,
      camera: false,
      kind: 'share',
      watched: true,
      render: (focus, compact, onToggleLayout) => (
        <ShareView client={client} state={state} principal={principal} name={name} audio={false} reconnecting volume={shareVolumes.current.get(principal) ?? 1} onVolumeChange={(volume) => { shareVolumes.current.set(principal, volume); }} onFocus={focus} compact={compact} onToggleLayout={onToggleLayout} />
      ),
    };
  });
  streamTiles.push(...heldTiles);
  // The call view is open while the person asked for video or live streams
  // offer tiles. Camera and stream tiles share one layout on the stage: the
  // focused stream is the main picture and the other tiles are the strip. The
  // people's tiles come only with the person's own choice to view (O8): live
  // streams alone open it with their tiles and nothing else, so no camera tile
  // waits for a camera that is not received. It stays the one CallView whether
  // open or closed, so no watched player is drawn again by opening or closing
  // it (design §7.4; multi-share §7.1, #1509). A watch held through a renewal
  // keeps it as its live stream did (heldTiles).
  const callOpen = inVoice && (state.viewing || streamTiles.length > 0) && !strip;
  const call = callOpen && state.viewing ? callTiles(roster, viewer, state.cameraTrack !== 'none', (id) => nameOf(people, id, viewer), viewerName, hideNonVideo, streamTiles.length) : null;
  // In the phone layout under Chat and Channels (the strip) the stage holds a
  // focused watched share only: no call view and no camera tile, and no camera
  // received (showVideo). The person's choice to view is kept, so the view and
  // cameras are back with the Voice view, with no tap (O8).
  const staged = !inVoice || (strip && state.watched.size === 0) ? null : (
    <CallView
      client={client}
      state={state}
      tiles={[...(call?.tiles ?? []), ...streamTiles]}
      hidden={call?.hidden ?? 0}
      open={callOpen}
      viewed={viewed}
      onOpen={onOpen}
      nameOf={(principal) => nameOf(people, principal, viewer, false, viewerName)}
      hideNonVideo={hideNonVideo}
      onHideNonVideo={() => {
        const next = !hideNonVideo;
        setHideNonVideo(next);
        writeHideNonVideo(next);
      }}
      keepSaid={focusShow}
    />
  );
  // "<name> stopped sharing." is shown where the stage was, for stoppedFor
  // (issue #2198, UI-stream-5), not as small text after the help line.
  const stoppedNotice = stopped === null || !inVoice ? null : <p role="status" className="stage-notice">{stopped.name} stopped sharing.</p>;
  // One fragment either way, so the call view keeps its place (and its
  // state) when the notice comes and goes.
  // Where there is no stage content (a phone under Chat once its last share
  // ends), the notice alone would make the stage non-empty and the share
  // layout would clip the conversation's header: it is the strip's then.
  const onStage = staged === null ? null : <>{stoppedNotice}{staged}</>;
  // How many cameras are on in the room: everyone else's claims, and the
  // person's own while their track is published.
  const cameraCount = roster.filter((o) => o.principal !== viewer && o.camera).length + (state.cameraTrack === 'published' ? 1 : 0);
  // The Camera button is offered to someone who may publish with a camera:
  // not in Watch only, not on a grant that cannot publish (O14).
  // A camera that is on keeps its button, whatever whoami says by now, so it
  // can always be turned off.
  const canCamera = (cameraAllowed && !state.listenOnly && !state.watchOnly) || cameraOn(state.camera);
  // Where voice is (UX-09): a button back to that channel while another is on
  // screen, plain text while it is the one on screen.
  const joined = inVoice ? state.channel : null;
  const where =
    joined === null ? null : joined === viewed ? (
      ` in #${joined}`
    ) : (
      <>
        {' in '}
        <button type="button" className="voice-where" onClick={() => { onOpen(joined); }}>
          #{joined}
        </button>
      </>
    );
  // Where Join voice and Watch only go: the channel on screen. On a phone the
  // name is only read out, and the buttons show their verbs. With no channel
  // on screen there is nowhere to name.
  const dest = viewed === null ? null : <span className="dest">in #{viewed}</span>;
  // Share screen stays available while other people share; only the six-stream
  // cap can refuse a new claim now (voice-multi-share.md §5.1, #1509). Before a
  // share the strip keeps one line, with full notes in Voice options (#600).
  const { features } = voice;
  // A viewer is listen only from the first frame. Use the join's viewer flag
  // for the row's status and help before the grant confirms it (#2210).
  const listenOnly = state.viewer || state.listenOnly;
  const shareLine = !listenOnly && features.canShare && state.share.kind === 'idle';
  const sharable = shareLine;
  // What About talking and sharing would show: the talk-key line, the sharing
  // line. With neither there is nothing to disclose, and no button.
  const talkLine = state.mode === 'ptt' && !listenOnly && !state.watchOnly && !coarse;
  const hasHelp = talkLine || shareLine;
  // Everyone else in the room joined, whose volumes Voice options sets.
  const others = roster.filter((o) => o.principal !== viewer).map((o) => ({ principal: o.principal, name: nameOf(people, o.principal, viewer) }));
  const more = () => {
    setOptions(true);
    setToAbout(true);
  };
  const leave = (focused: boolean) => {
    left.current = focused;
    voice.leave();
  };

  return (
    <section aria-label="Voice" className={`voice phase-${state.phase}${state.transmitting ? ' sending' : ''}${strip ? ' strip' : ''}`}>
      {/* Two rows (issue #587): who is in voice on top, and below it where
          you are and the controls. The dock sits on the window's bottom edge,
          so a roster that grows, or a longer status, moves the strip's top and
          leaves the controls where they were. The phone's strip is the one
          row, of four controls (issue #1028). */}
      <div className="voice-bar" inert={covered}>
        {inVoice ? <BlockedVoiceMute voice={voice} others={others} /> : null}
        {strip ? null : (
          <Roster channel={rosterChannel ?? undefined} mayModerate={mayModerateRoster} reserve={inVoice} roster={roster} people={people} viewer={viewer} viewerName={viewerName} elsewhere={elsewhere} speaking={speaking} volumes={inVoice ? state.volumes : noVolumes} />
        )}
        {awayLine}
        <div className="voice-row">
          {stripOpens !== null ? (
            <StripState phase={state.phase} channel={state.channel} camera={state.cameraTrack !== 'none'} onOpen={stripOpens} />
          ) : (
            <div className="voice-state">
              <p className="voice-phase">
                {phaseLabel[state.phase]}
                {where}
              </p>
              {/* Not in voice, "Microphone off" is the hint below, said once. */}
              {inVoice ? <VoiceStatus state={listenOnly === state.listenOnly ? state : { ...state, listenOnly }} /> : null}
            </div>
          )}

          <div className="voice-controls">
            {strip ? (
              <>
                <TalkControl voice={voice} silenced={silenced} />
                <ModeToggle voice={voice} />
                <MuteButton voice={voice} />
                <LeaveButton onLeave={leave} />
              </>
            ) : inVoice ? (
              <>
                <TalkControl voice={voice} silenced={silenced} />
                <ModeToggle voice={voice} />
                {state.watchOnly && !state.listenOnly && !state.viewer ? (
                  <button type="button" onClick={() => { void client.useMicrophone(openAudio()); }} disabled={state.phase !== 'connected'}>
                    Use microphone
                  </button>
                ) : null}
                <MuteButton voice={voice} />
                <button type="button" aria-pressed={state.deafened} onClick={() => { client.setDeafened(!state.deafened); }}>
                  <Glyph name="headphones" />
                  <span className="label">{state.deafened ? 'Undeafen' : 'Deafen'}</span>
                </button>
                {canCamera ? <CameraButton voice={voice} /> : null}
                {state.viewer ? null : <ShareButton voice={voice} />}
                {state.viewing ? (
                  <button type="button" aria-pressed="true" onClick={() => { client.viewVideo(false); }}>
                    <Glyph name="camera" />
                    <span className="label">Hide video</span>
                  </button>
                ) : cameraCount > 0 ? (
                  <button ref={showVideoButton} type="button" onClick={() => { client.viewVideo(true); }}>
                    <Glyph name="camera" />
                    <span className="label">Show video ({cameraCount})</span>
                  </button>
                ) : null}
                {/* The talk key and sharing notes are behind this disclosure,
                    not always shown (issue #2198, UI-talk-6). Nothing is
                    remembered: it starts closed. */}
                {hasHelp ? (
                  <button type="button" className="quiet voice-help-button" aria-expanded={help} aria-controls="voice-notes" aria-label="About talking and sharing" onClick={() => { setHelp(!help); }}>
                    <span aria-hidden="true">i</span>
                  </button>
                ) : null}
                <button type="button" className="quiet" {...optionButton}>
                  <Glyph name="options" />
                  <span className="label">Voice options</span>
                </button>
                <LeaveButton onLeave={leave} />
              </>
            ) : (
              <>
                <button ref={joinButton} type="button" className="primary" onClick={() => { voice.join(); }} disabled={!canJoin}>
                  <Glyph name="headphones" />
                  {/* One flex item, so the words and the channel read as one line of text (F17). */}
                  <span className="btn-text">Join voice {dest}</span>
                </button>
                <button type="button" onClick={() => { voice.watchOnly(elsewhere); }} disabled={!canJoin} aria-describedby={canJoin ? 'voice-watch-hint' : undefined}>
                  <span className="btn-text">Watch only {dest}</span>
                </button>
              </>
            )}
          </div>
        </div>
        {strip && noMeter ? (
          <p role="status" className="hint strip-note">
            {meterStripNote}
          </p>
        ) : null}
        {strip && noPlayback ? <HearButton voice={voice} /> : null}
      </div>

      <div id="voice-notes" className="voice-notes" hidden={strip} inert={covered}>
        {state.why === null ? null : <p role="status" className="hint">{state.why}</p>}
        {state.reportNote === 'rate_limited' ? <p role="status" className="hint">Too many talk reports; slow down</p> : null}
        {state.reportNote === 'retrying' ? <p role="status" className="hint">Reporting your talking is slow; retrying</p> : null}
        {inVoice ? (
          <>
            {!strip && noPlayback ? <HearButton voice={voice} /> : null}
            {cutOff ? <p role="status" className="hint">{cutOffNote}</p> : null}
            <CameraNote voice={voice} />
            {state.outputNote === null ? null : (
              <p role="status" className="hint">
                {state.outputNote}
              </p>
            )}
            {/* A switch that failed says why instead (state.why). */}
            {state.missing && state.why === null ? (
              <p role="status" className="hint">
                {missingNote}
              </p>
            ) : null}
            {noMeter ? (
              <p role="status" className="hint">
                {meterNote}
              </p>
            ) : null}
            {state.mode === 'ptt' && silenced && !state.listenOnly && !state.watchOnly ? (
              <p id="voice-talk-why" className="hint">
                {state.deafened ? 'Hold to talk sends nothing while you are deafened. Undeafen to talk.' : 'Hold to talk sends nothing while you are muted. Unmute to talk.'}
              </p>
            ) : null}
            {/* Only the notes of a disclosure are behind it; what is
                happening (a share, why one ended) is not. */}
            {help && talkLine ? (
              <p className="hint talk-key-note">{talkKeyNote(voice.talkKey)}</p>
            ) : null}
            <ShareNotes voice={voice} onMore={more} othersShare={streams.length > 0} showLine={help && shareLine} />
          </>
        ) : (
          <>
            {/* The hint is a promise Watch only cannot keep while joins are
                off, so the reason stands in its place. */}
            {canJoin ? (
              <p id="voice-watch-hint" className="hint">Microphone off. You hear the room and can watch a share.</p>
            ) : voice.secure && snapshot.kind === 'loading' ? (
              <p className="hint">Checking whether voice is available.</p>
            ) : null}
            {elsewhere ? <p className="hint">You are in voice on another device. Joining here moves your voice to this device.</p> : null}
            {theirStreams.length === 0 ? null : <p className="hint">{nameOf(people, theirStreams[0]?.principal ?? 0, viewer)} is sharing a screen. Join, or watch only, to see it.</p>}
            {!voice.secure ? (
              <p className="hint">Voice needs a secure page: use https, or localhost.</p>
            ) : presence?.available === false ? (
              <p className="hint">Voice is unavailable right now; chat still works.</p>
            ) : snapshot.kind === 'unreachable' ? (
              <p className="hint">Voice status unavailable: the server cannot be reached.</p>
            ) : snapshot.kind === 'gone' ? (
              <p className="hint">Voice status could not be read.</p>
            ) : null}
            {/* Private, and only while not in voice: joining takes this
                out of the tree, which releases the microphone first. Offered
                only when Join is (a live connection, voice up), so a page
                that lost the server offers nothing to press here (P3-09). */}
            {canJoin && micTestShown ? <MicTest /> : null}
          </>
        )}
        {joined !== null && !strip ? (
          <VoiceDiagnostics
            key={joined}
            client={client}
            participants={others}
            sharing={state.share.kind === 'sharing'}
            watched={theirStreams
              .filter(({ principal }) => state.watched.has(principal))
              .map(({ principal }) => ({ principal, name: nameOf(people, principal, viewer) }))}
          />
        ) : null}
      </div>

      {inVoice ? (
        <Options panel={panel} onClose={onClose} onShrink={onShrink} sheet={sheet} onSheet={onSheet} open={options && !strip}>
          <BrowserOptions voice={voice} open={options && !strip} sharable={sharable} about={about} others={others} cameraAllowed={cameraAllowed} />
        </Options>
      ) : null}
      {staged === null ? stoppedNotice : null}
      {onStage === null ? null : stage === null ? onStage : createPortal(onStage, stage)}
    </section>
  );
}

// StripState is the first of the phone strip's four controls (issue #1028):
// where voice is, as a button that opens the Voice view, where the rest is.
// Its words are the state and the channel, on two short lines; a long name
// ends in an ellipsis. While this page's own camera is captured or sent, the
// state line says "Camera on", with the camera glyph in place of the dot
// (issue #1268, O10): from the local track alone, never from presence (S5),
// and for a capture not yet sent too, since the device is in use either way.
// The control stays one, and still opens the Voice view, where the Camera
// button is.
function StripState({
  phase,
  channel,
  camera,
  onOpen,
}: {
  readonly phase: Phase;
  readonly channel: string | null;
  readonly camera: boolean;
  readonly onOpen: () => void;
}) {
  const going = phase === 'connecting' || phase === 'reconnecting';
  return (
    <button type="button" className={camera ? 'voice-open has-camera' : 'voice-open'} onClick={onOpen}>
      {camera ? (
        <span className="voice-open-phase camera-on">
          <Glyph name="camera" />
          Camera on
          {/* Where the connection is not up, the state is still read out. */}
          {phase === 'connected' ? null : <span className="vh">, {phaseLabel[phase]}</span>}
        </span>
      ) : (
        <span className="voice-open-phase">{phase === 'connected' ? 'In voice' : phaseLabel[phase]}</span>
      )}
      {channel === null ? null : (
        <>
          {/* The spaces are outside the spans, where a flex column draws
              none and the name still has them. */}
          {' '}
          <span className="vh">{going ? 'to' : 'in'}</span>{' '}
          <span className="voice-open-where">#{channel}</span>
        </>
      )}
    </button>
  );
}

// talkKeyNote is the talk key and when it works: the notes' line, and the
// description of Hold to talk.
function talkKeyNote(key: string): string {
  return `Talk key: ${keyLabel(key)}. It works only while this tab has focus, and not while you are typing.`;
}

// What Hold to talk says where the session cannot send.
const listenOnlyTalkNote = 'Hold to talk is off: this session is listen only.';
const viewerTalkNote = 'Hold to talk is off: you are watching beside your voice on another device.';
const watchOnlyTalkNote = 'Hold to talk is off: you are in Watch only. Use microphone to talk.';

// TalkControl is Hold to talk, or, with the open microphone, its level: the
// one way to talk, in the strip and in the Voice view alike.
function TalkControl({ voice, silenced }: { readonly voice: VoiceControl; readonly silenced: boolean }) {
  const { state, client } = voice;
  // Without a microphone nothing can be sent: the key is not advertised, and
  // the description says why. The reason muted is only referenced while it is
  // drawn (the notes' #voice-talk-why), so no description points at nothing.
  const coarse = useCoarsePointer();
  const cannot = state.listenOnly ? listenOnlyTalkNote : state.viewer ? viewerTalkNote : state.watchOnly ? watchOnlyTalkNote : null;
  const why = silenced && cannot === null;
  // On a touch screen no keyboard key is named (F18); only a reason is said.
  const description = cannot ?? (coarse ? null : talkKeyNote(voice.talkKey));
  if (state.mode !== 'ptt') {
    return (
      <span className="open-mic">
        <Glyph name="mic" />
        {/* In the phone's strip the words are only read out, and the meter
            has their room (issue #1028). */}
        <span className="open-mic-label">Open microphone</span>{' '}
        <meter min={0} max={1} value={state.level} aria-label="Microphone level" />
      </span>
    );
  }
  return (
    <>
      <button
        type="button"
        className="talk"
        aria-pressed={state.talking}
        disabled={state.listenOnly || state.watchOnly}
        // Muted or deafened it stays pressable (a press reaches the gate, which
        // is shut and sends nothing), but it looks off and says why in the note
        // below (UX-07).
        aria-disabled={silenced ? true : undefined}
        title={cannot ?? (coarse ? undefined : `Talk key: ${keyLabel(voice.talkKey)}`)}
        aria-describedby={[why ? 'voice-talk-why' : '', description === null ? '' : 'voice-talk-key'].filter((id) => id !== '').join(' ') || undefined}
        onPointerDown={(e) => { e.currentTarget.setPointerCapture(e.pointerId); client.press(); }}
        onPointerUp={() => { client.release(); }}
        onPointerCancel={() => { client.release(); }}
        // The keyboard holds it as a pointer does (issue #590): a held key
        // repeats, and only its first press counts. Focus moving away lets go,
        // as a lost pointer does.
        onKeyDown={(e) => {
          if (!holdKey(e) || e.repeat) return;
          e.preventDefault();
          client.press();
        }}
        onKeyUp={(e) => {
          if (holdKey(e)) client.release();
        }}
        onBlur={() => { client.release(); }}
        onContextMenu={(e) => { e.preventDefault(); }}
      >
        <Glyph name="mic" />
        Hold to talk
      </button>
    {/* The talk key is in the button's description as well as its title,
        whether or not the notes behind About talking and sharing are open. */}
    {description === null ? null : <span id="voice-talk-key" hidden>{description}</span>}
    </>
  );
}

// ModeToggle is the one-tap switch between the open microphone and push to
// talk (issue #1244), in the bar and in the phone's strip. Pressed is the open
// microphone; the Voice options radios follow the same state. Listen-only and
// watch-only have no microphone to switch, as the radios are.
function ModeToggle({ voice }: { readonly voice: VoiceControl }) {
  const { state, client } = voice;
  const open = state.mode === 'open';
  // With no microphone, nothing is shown as on: not pressed, and disabled.
  const off = state.listenOnly || state.watchOnly;
  return (
    <button
      type="button"
      className="mode-toggle"
      aria-pressed={open && !off}
      disabled={off}
      onClick={() => { client.setMode(open ? 'ptt' : 'open'); }}
    >
      <Glyph name="wave" />
      <span className="label">Open mic</span>
    </button>
  );
}

// MuteButton is Mute. Deafened is muted too (UX-11), so Mute shows pressed and
// cannot be let go of. The choice made before is kept, and is back on
// Undeafen (issue #591, F13).
function MuteButton({ voice }: { readonly voice: VoiceControl }) {
  const { state, client } = voice;
  return (
    <button type="button" aria-pressed={state.muted || state.deafened} disabled={state.deafened} onClick={() => { client.setMuted(!state.muted); }}>
      <Glyph name="micOff" />
      <span className="label">{state.deafened ? 'Muted by deafen' : state.muted ? 'Unmute' : 'Mute'}</span>
    </button>
  );
}

// LeaveButton is Leave voice. It says whether it had focus when pressed, for
// where focus goes once the client has drained (issue #590).
function LeaveButton({ onLeave }: { readonly onLeave: (focused: boolean) => void }) {
  return (
    <button
      type="button"
      className="danger"
      onClick={(e) => {
        onLeave(document.activeElement === e.currentTarget);
      }}
    >
      <Glyph name="leave" />
      <span className="label">Leave voice</span>
    </button>
  );
}

// Elsewhere is the line "3 in voice in #game-night", and a button that opens
// that channel (issue #596). It opens the channel and does nothing else: Join
// voice there is the viewer's next choice, not this button's.
function Elsewhere({ where, onOpen }: { readonly where: Busiest; readonly onOpen: (name: string) => void }) {
  const more = where.more === 0 ? '' : ` and ${String(where.more)} more ${where.more === 1 ? 'channel' : 'channels'}`;
  return (
    <p className="voice-elsewhere">
      <Glyph name="speaker" />
      <span>
        {where.count} in voice in #{where.name}
        {more}
      </span>{' '}
      <button
        type="button"
        onClick={() => {
          onOpen(where.name);
        }}
      >
        Open #{where.name}
      </button>
    </p>
  );
}

// How many chips a phone shows before "+N more" (issue #587). Which ones
// are hidden is a class each chip carries and a CSS rule hides, so the strip
// measures nothing.
const phoneChips = 3;

// shownOnPhone is who a capped roster keeps: the viewer's own chip first
// (issue #641: otherwise a high principal id is among the "+N more" and the
// viewer cannot see themselves in the room), then the others in the order
// the roster has them. The list itself stays in that order.
function shownOnPhone(roster: readonly Occupant[], viewer: number): ReadonlySet<number> {
  const mine = roster.filter((o) => o.principal === viewer);
  const others = roster.filter((o) => o.principal !== viewer);
  return new Set([...mine, ...others].slice(0, phoneChips).map((o) => o.principal));
}

// Roster is who is in voice, with how many as its heading, which also names
// the list. At 700 px and wider every chip shows, wrapping onto more lines;
// on a phone three show, the viewer's own among them, and "+N more" shows
// the rest. While this page is in voice (reserve) and presence does not yet
// list it, the roster holds the page's own chip, invisible, in the slot the
// real one takes (the roster is in principal order): the real chip arrives with
// the next presence snapshot a moment after the join, and where the roster is
// above the controls a row that wrapped differently would push them (issue #2202).
export function Roster({ channel, mayModerate: standing, reserve = false, roster, people, viewer, viewerName = '', elsewhere, speaking, volumes }: {
  // The channel the roster belongs to, and whether the viewer may moderate it
  // (when not said, the standing in the channel on screen).
  readonly channel?: string;
  readonly mayModerate?: boolean;
  readonly reserve?: boolean;
  readonly roster: readonly Occupant[];
  readonly people: ReadonlyMap<number, Person>;
  readonly viewer: number;
  // The viewer's own name, from the signed-in identity; empty while unknown.
  readonly viewerName?: string;
  // The viewer listed is in voice on another page, not on this one.
  readonly elsewhere: boolean;
  readonly speaking: ReadonlySet<number>;
  // How loud this page plays each person not at full volume.
  readonly volumes: ReadonlyMap<number, number>;
}) {
  const here = useMayModerate();
  const mayModerate = standing ?? here;
  const [kick, setKick] = useState<{ readonly id: number; readonly name: string } | null>(null);
  const kickOpener = useRef<HTMLButtonElement>(null);
  const [all, setAll] = useState(false);
  const more = roster.length - phoneChips;
  const capped = more > 0 && !all;
  const kept = shownOnPhone(roster, viewer);
  // The page's own chip, before presence lists it (issue #2202): the real
  // chip's markup, name and moderation button included, unseen, inert and
  // hidden from assistive technology, in the slot where the real one will
  // sort (by principal, as the roster is), so the row wraps now and not a
  // moment after the join.
  const unlisted = reserve && !roster.some((o) => o.principal === viewer);
  const ownName = nameOf(people, viewer, viewer, false, viewerName);
  const placeholder = (
    <li key="held" className="me reserved" aria-hidden="true" inert>
      <span className="face"><Avatar id={viewer} name={ownName} /></span>
      <span className="who">{nameOf(people, viewer, viewer, elsewhere, viewerName)}</span>
      <IdentityMarks kind={people.get(viewer)?.kind} only={['agent']} />
      {!mayModerate || channel === undefined ? null : <button type="button" tabIndex={-1} aria-label={`Remove ${ownName} from voice`}>Remove from voice</button>}
    </li>
  );
  const slot = roster.findIndex((o) => o.principal > viewer);
  const heldAt = slot < 0 ? roster.length : slot;
  if (roster.length === 0 && kick === null) {
    return unlisted ? (
      <div className="voice-who reserved" aria-hidden="true" inert>
        <span className="voice-count">1 in voice</span>
        <ul className="voice-roster">{placeholder}</ul>
      </div>
    ) : null;
  }
  return (
    <div className="voice-who">
      <h2 id="voice-count" className="voice-count">
        {roster.length} in voice
      </h2>
      <ul id="voice-roster" aria-labelledby="voice-count" className={capped ? 'voice-roster capped' : 'voice-roster'}>
        {roster.flatMap((o, i) => {
          const mark = speaking.has(o.principal) ? 'speaking' : o.transmitting ? 'sending' : undefined;
          return [
          unlisted && i === heldAt ? placeholder : null,
          (
          <li key={o.principal} className={[speaking.has(o.principal) ? 'speaking' : '', kept.has(o.principal) ? '' : 'beyond', o.principal === viewer ? 'me' : ''].filter((c) => c !== '').join(' ') || undefined}>
            <span className="face">
              <Avatar id={o.principal} name={people.get(o.principal)?.name ?? nameOf(people, o.principal, viewer, false, viewerName)} />
              {/* One badge for sending audio (issue #2198, UI-talk-4): "speaking"
                  when the level says so, else "sending"; the level is the
                  avatar's ring. */}
              {mark === undefined ? null : <Dot text={mark} />}
            </span>
            <span className="who">{nameOf(people, o.principal, viewer, elsewhere, viewerName)}</span>
            <IdentityMarks kind={people.get(o.principal)?.kind} only={['agent']} />
            {!mayModerate || channel === undefined ? null : <button type="button" aria-label={`Remove ${nameOf(people, o.principal, viewer, false, viewerName)} from voice`} onClick={(event) => { kickOpener.current = event.currentTarget; setKick({ id: o.principal, name: nameOf(people, o.principal, viewer, false, viewerName) }); }}>Remove from voice</button>}
            <Said text={mark} />
            {o.sharing ? <Tag text="sharing" /> : null}
            {/* Others' claims; the person's own camera is said from their
                own track, in the strip (S5). */}
            {o.camera && o.principal !== viewer ? <Tag text="camera" /> : null}
            {o.principal === viewer ? null : <VolumeTag volume={volumes.get(o.principal)} />}
          </li>
          ),
        ];})}
        {unlisted && heldAt === roster.length ? placeholder : null}
      </ul>
      {kick === null || channel === undefined || !mayModerate ? null : <ModerationConfirm key={kick.id} op="kickVoice" channel={channel} id={kick.id} name={kick.name} onClose={() => { setKick(null); kickOpener.current?.focus(); }} />}
      {more > 0 ? (
        <button type="button" className="voice-more quiet" aria-expanded={all} aria-controls="voice-roster" onClick={() => { setAll(!all); }}>
          {all ? 'Show fewer' : `+${String(more)} more`}
        </button>
      ) : null}
    </div>
  );
}

// Nobody turned down: the roster while this page is not in voice.
const noVolumes: ReadonlyMap<number, number> = new Map();
// BlockedVoiceMute holds the default mute of blocked people (issue #1832)
// while this page is in voice: a component, so its hooks run in a fixed order
// whatever the panel shows. It renders nothing.
function BlockedVoiceMute({ voice, others }: { readonly voice: VoiceControl; readonly others: readonly { readonly principal: number }[] }) {
  const { blocked } = useBlocksContext();
  const client = voice.client;
  const setVolume = useCallback((principal: number, volume: number) => { client.setVolume(principal, volume); }, [client]);
  const ids = others.map((o) => o.principal).join(',');
  // The room by its ids, so a new array of the same people is not a change.
  const room = useMemo(() => (ids === '' ? [] : ids.split(',').map((id) => ({ principal: Number(id) }))), [ids]);
  useBlockedVoiceMute(blocked, room, voice.state.volumes, setVolume);
  return null;
}

// percent is a volume as the page shows it: 0.4 is 40.
const percent = (volume: number): number => Math.round(volume * 100);

// VolumeTag says on a chip that this page plays the person at other than
// full volume (issue #661): why they may sound quiet.
function VolumeTag({ volume }: { readonly volume: number | undefined }) {
  if (volume === undefined || percent(volume) === 100) return null;
  return <Tag text={`volume ${String(percent(volume))}%`} />;
}

// Volumes is a slider for each other person in the room, 0 to 100% in steps
// of 5%. It changes only how loud this page plays them.
function Volumes({ voice, others }: { readonly voice: VoiceControl; readonly others: readonly { readonly principal: number; readonly name: string }[] }) {
  const { state, client } = voice;
  const { blocked } = useBlocksContext();
  if (others.length === 0) return null;
  return (
    <fieldset className="voice-volumes">
      <legend>Volume of each person</legend>
      {others.map((o) => {
        const id = `voice-volume-${String(o.principal)}`;
        const value = percent(state.volumes.get(o.principal) ?? 1);
        return (
          <div key={o.principal} className="voice-volume">
            <label htmlFor={id}>Volume of {o.name}</label>
            <input
              id={id}
              type="range"
              min={0}
              max={100}
              step={5}
              value={value}
              aria-valuetext={`${String(value)}%`}
              onChange={(e) => {
                client.setVolume(o.principal, Number(e.target.value) / 100);
              }}
            />
            <span className="voice-volume-value" aria-hidden="true">
              {value}%
            </span>
            {value === 0 && blocked.has(o.principal) ? (
              <button
                type="button"
                className="voice-volume-unmute"
                onClick={() => {
                  client.setVolume(o.principal, 1);
                }}
              >
                Unmute {o.name} (blocked, muted for you)
              </button>
            ) : null}
          </div>
        );
      })}
    </fieldset>
  );
}

// The tap that lets the browser play the channel. startAudio is called inside
// the click itself: no await, timer or effect comes before it, or the browser
// no longer counts it as the gesture.
function HearButton({ voice }: { readonly voice: VoiceControl }) {
  return (
    <button type="button" onClick={() => { voice.client.startAudio(); }}>
      {hearLabel}
    </button>
  );
}

export const hearLabel = 'Tap to hear the channel';

// What the notes say while the microphone is missing.
export const cutOffNote = 'Voice server unreachable. You may be cut off.';
export const missingNote = 'Your microphone was disconnected. Choose a microphone in Voice options.';
// The open microphone listens through the level meter; a browser that refused the
// audio context behind it leaves the gate shut (push to talk is unaffected).
// The strip, which has one row and hides the notes, says it shorter.
export const meterNote = 'Open microphone needs the level meter, which this browser would not start, so nothing is sent. Switch to Push to talk.';
export const meterStripNote = 'Open mic needs the level meter; use Push to talk.';

// The values of the microphone list: System default, a device by id, and
// the entry shown while nothing is chosen (the microphone went), so that
// choosing any other, System default included, is a change. A device id is
// the browser's own string, so it is set apart by a prefix.
const systemDefault = 'system';
const devicePrefix = 'device:';
const unchosen = '';

export const followsNote = "System default follows your computer's sound settings.";

// MicrophoneChoice is the choice of microphone in Voice options: in voice
// with the microphone, the list and the device in use. In watch only nothing
// is listed: the list is read only once the microphone is used, as it is
// when the browser does not name its devices.
function MicrophoneChoice({ voice }: { readonly voice: VoiceControl }) {
  const { state, client } = voice;
  if (state.listenOnly) return null;
  const list = state.microphones;
  const live = state.phase === 'connected' || state.phase === 'reconnecting';
  if (state.watchOnly || list === null || !live) {
    return <p className="hint">Microphones are listed once you use your microphone.</p>;
  }
  const chosen = state.microphone;
  const listed = chosen === null || list.some((m) => m.id === chosen.id);
  const value = state.missing || !listed ? unchosen : chosen === null ? systemDefault : devicePrefix + chosen.id;
  const inUse = state.inUse === null ? undefined : list.find((m) => m.id === state.inUse);
  return (
    <div className="voice-device">
      <label htmlFor="voice-microphone">Microphone</label>
      <select
        id="voice-microphone"
        value={value}
        onChange={(e) => {
          const v = e.target.value;
          if (v === unchosen) return;
          if (v === systemDefault) {
            void client.chooseMicrophone(null);
            return;
          }
          const picked = list.find((m) => devicePrefix + m.id === v);
          if (picked !== undefined) void client.chooseMicrophone(picked);
        }}
      >
        {value === unchosen ? (
          <option value={unchosen} disabled>
            Choose a microphone
          </option>
        ) : null}
        <option value={systemDefault}>System default</option>
        {list.map((m) => (
          <option key={m.id} value={devicePrefix + m.id}>
            {m.label}
          </option>
        ))}
      </select>
      {inUse === undefined ? null : <p className="hint">In use: {inUse.label}</p>}
      {chosen === null ? <p className="hint">{followsNote}</p> : null}
    </div>
  );
}

// What Voice options says where the speakers cannot be chosen, and where the
// browser lists none but the default (issue #660).
export const noOutputChoice = 'This browser plays sound on its default output.';
export const noOtherOutputs = 'This browser lists no other speakers yet.';

// SpeakersChoice is the choice of speakers in Voice options: System default
// first, then each output the browser lists, the one in use selected. One in
// use that is not listed (the browser names none yet) is shown as well, so
// that the list says what is in use.
function SpeakersChoice({ voice }: { readonly voice: VoiceControl }) {
  const { state, client } = voice;
  if (!state.outputSelection) return <p className="hint">{noOutputChoice}</p>;
  const list = state.outputs ?? [];
  const inUse = state.output;
  const shown = inUse === null || list.some((o) => o.id === inUse.id) ? list : [...list, inUse];
  return (
    <div className="voice-device">
      <label htmlFor="voice-speakers">Speakers</label>
      <select
        id="voice-speakers"
        value={inUse === null ? systemDefault : devicePrefix + inUse.id}
        onChange={(e) => {
          const v = e.target.value;
          if (v === systemDefault) {
            void client.chooseOutput(null);
            return;
          }
          const picked = shown.find((o) => devicePrefix + o.id === v);
          if (picked !== undefined) void client.chooseOutput(picked);
        }}
      >
        <option value={systemDefault}>System default</option>
        {shown.map((o) => (
          <option key={o.id} value={devicePrefix + o.id}>
            {o.label}
          </option>
        ))}
      </select>
      {list.length === 0 ? <p className="hint">{noOtherOutputs}</p> : null}
    </div>
  );
}

// The voice processing switch (issue #659): its words, the note said while it
// is off, and the hint by the open microphone's level.
export const processingLabel = 'Voice processing (echo cancellation, noise suppression, automatic gain)';
export const processingOffNote = 'Off: your microphone is sent as it is.';
export const processingUseNote =
  'Use this for a virtual microphone (such as one a text-to-speech program provides) or a studio microphone. With speakers rather than headphones, others may hear themselves.';
export const thresholdHint = 'Without automatic gain your level may differ: check the meter and set the level.';

// The strip and compact call header share this implementation (issue #1498).
export { transmitLabel } from './VoiceStatus';

// releaseLabel names a release delay for people.
export function releaseLabel(ms: number): string {
  return `${String(ms)} ms`;
}

// The filters, by the names the switch gives them.
const filterNames: readonly (readonly [keyof Processing, string])[] = [
  ['echoCancellation', 'echo cancellation'],
  ['noiseSuppression', 'noise suppression'],
  ['autoGainControl', 'automatic gain'],
];

// reportedNote says what a capture made with processing off reports: off, or
// the filters the browser kept on. A browser that does not say, of a filter
// it did not keep, is not said to have turned it off.
export function reportedNote(reported: Processing | null): string | null {
  if (reported === null) return null;
  const kept = filterNames.filter(([key]) => reported[key] === true).map(([, name]) => name);
  if (kept.length > 0) {
    const last = kept.pop() ?? '';
    const names = kept.length === 0 ? last : `${kept.join(', ')} and ${last}`;
    return `Your browser kept ${names} on.`;
  }
  return filterNames.every(([key]) => reported[key] === false) ? processingOffNote : null;
}

// ProcessingChoice is the voice processing switch, for the microphone chosen,
// and while it is off what the capture says and when to use it.
function ProcessingChoice({ voice }: { readonly voice: VoiceControl }) {
  const { state, client } = voice;
  const said = state.processing ? null : reportedNote(state.reported);
  return (
    <div className="voice-processing">
      <label>
        <input
          type="checkbox"
          checked={state.processing}
          onChange={(e) => {
            void client.setProcessing(e.target.checked);
          }}
        />
        {processingLabel}
      </label>
      {said === null ? null : <p className="hint">{said}</p>}
      {state.processing ? null : <p className="hint">{processingUseNote}</p>}
    </div>
  );
}

// CameraButton is Camera, with aria-pressed, as Mute is. Turning it off is
// the button, Tab and Enter; nothing turns it on but a press of it.
function CameraButton({ voice }: { readonly voice: VoiceControl }) {
  const { state, client } = voice;
  const on = cameraOn(state.camera);
  const stopping = state.camera.kind === 'stopping';
  return (
    <button
      type="button"
      aria-pressed={on}
      disabled={stopping || (!on && state.phase !== 'connected')}
      onClick={() => {
        if (on) void client.stopCamera();
        else void client.startCamera(onPhoneCamera() ? 'phone' : 'desktop');
      }}
    >
      <Glyph name="camera" />
      <span className="label">Camera</span>
    </button>
  );
}

// CameraNote says what the camera is doing, or why it stopped or did not
// start, in words (design §7.2).
function CameraNote({ voice }: { readonly voice: VoiceControl }) {
  const note = cameraNote(voice.state.camera);
  if (note === null) return null;
  return (
    <p role="status" className="hint camera-note">
      {note}
    </p>
  );
}

// The values of the camera list: a device by id, and the entry shown while
// nothing listed is chosen. There is no default entry: a camera is never
// taken by its place or as the default (N7).
const cameraPrefix = 'camera:';

// CameraChoice is the choice of camera in Voice options, under the
// microphone's. The cameras are listed once the browser names them, which is
// after the camera, or its test, was allowed.
function CameraChoice({ voice }: { readonly voice: VoiceControl }) {
  const { state, client } = voice;
  const list = state.cameras;
  const chosen = state.cameraChoice;
  if (list === null) {
    return (
      <p className="hint">
        {chosen === null ? 'Cameras are listed once you turn on your camera or test it.' : `Camera: ${chosen.label === '' ? 'your chosen camera' : chosen.label}. Cameras are listed once you turn on your camera or test it.`}
      </p>
    );
  }
  const listed = chosen !== null && list.some((c) => c.id === chosen.id);
  const value = listed ? cameraPrefix + chosen.id : unchosen;
  const inUse = state.cameraInUse === null ? undefined : list.find((c) => c.id === state.cameraInUse);
  return (
    <div className="voice-device">
      <label htmlFor="voice-camera">Camera</label>
      <select
        id="voice-camera"
        value={value}
        onChange={(e) => {
          const picked = list.find((c) => cameraPrefix + c.id === e.target.value);
          if (picked !== undefined) void client.chooseCamera(picked);
        }}
      >
        {value === unchosen ? (
          <option value={unchosen} disabled>
            {chosen === null ? 'Choose a camera' : 'Your chosen camera is not connected'}
          </option>
        ) : null}
        {list.map((c) => (
          <option key={c.id} value={cameraPrefix + c.id}>
            {c.label}
          </option>
        ))}
      </select>
      {chosen !== null && !listed ? <p className="hint">Your chosen camera is not connected.</p> : null}
      {inUse === undefined ? null : <p className="hint">In use: {inUse.label}</p>}
    </div>
  );
}

// CameraOptions is the camera's part of Voice options: why there is none, or
// the picker and, while the camera is off, its private test.
function CameraOptions({ voice, open, cameraAllowed }: { readonly voice: VoiceControl; readonly open: boolean; readonly cameraAllowed: boolean }) {
  const { state, client } = voice;
  if (state.watchOnly) return <p className="hint">{watchOnlyCamera}</p>;
  if (state.listenOnly) return <p className="hint">{listenOnlyCamera}</p>;
  if (!cameraAllowed) return <p className="hint">{cameraOffForYou}</p>;
  const off = !cameraOn(state.camera);
  return (
    <>
      <CameraChoice voice={voice} />
      {/* Drawn only while the options are open and the camera is off:
          closing them takes the test out of the tree, which stops it. */}
      {open && off ? (
        <CameraTest
          choose={() => client.cameraToUse()}
          chosen={state.cameraChoice?.id ?? ''}
          onCaptured={() => {
            client.refreshCameras();
          }}
        />
      ) : null}
    </>
  );
}

// Whether el is inside a dialog or aria-modal element that is not the panel
// and not shut away: another modal has the focus (the switcher, Settings).
function inOtherModal(el: Element | null, panel: Element | null): boolean {
  const modal = el === null ? null : el.closest('[role="dialog"], [role="alertdialog"], [aria-modal="true"]');
  return modal !== null && modal !== panel && modal.closest('[hidden], [inert]') === null;
}

const tabbable = 'a[href], button:not(:disabled), input:not(:disabled), select:not(:disabled), textarea:not(:disabled), [tabindex]:not([tabindex="-1"])';
// A press on a control or its label can activate it even if the browser
// leaves focus on the body. Count programmatically focusable elements too.
const pressControl = 'a[href], area[href], button, input, select, textarea, label, summary, iframe, audio[controls], video[controls], [tabindex], [contenteditable=""], [contenteditable="true"], [contenteditable="plaintext-only"]';

function isDrawn(control: HTMLElement): boolean {
  return (control.offsetParent !== null || control.getClientRects().length > 0) && getComputedStyle(control).visibility === 'visible';
}

// The controls of the panel that Tab reaches, in order: enabled and drawn.
function panelTabbables(panel: HTMLElement): HTMLElement[] {
  const visible = Array.from(panel.querySelectorAll<HTMLElement>(tabbable)).filter(isDrawn);
  // A radio group is one Tab stop. If none is checked, use the focused
  // radio when already in the group, or the first one when entering it.
  return visible.filter((control) => {
    if (!(control instanceof HTMLInputElement) || control.type !== 'radio' || control.name === '') return true;
    const group = visible.filter((c): c is HTMLInputElement =>
      c instanceof HTMLInputElement && c.type === 'radio' && c.name === control.name && c.form === control.form);
    return control === (group.find((radio) => radio.checked) ?? group.find((radio) => radio === document.activeElement) ?? group[0]);
  });
}

// After a nested confirmation closes, the preferred radio can be disabled
// while its command runs. Keep focus in the disclosure in either case.
export function focusOptionsControl(panel: HTMLElement, radio: HTMLInputElement | null): void {
  const target = radio !== null && !radio.matches(':disabled') && isDrawn(radio) ? radio : panelTabbables(panel)[0] ?? panel;
  target.focus();
}

// The next control after the button in its own row (Leave voice, say), as Tab
// would reach it, or null.
function tabbablesAfter(button: HTMLElement): HTMLElement | null {
  const row = button.parentElement;
  if (row === null) return null;
  const all = Array.from(row.querySelectorAll<HTMLElement>(tabbable));
  const next = all[all.indexOf(button) + 1];
  return next ?? null;
}

// The browser and helper-backed panels share the disclosure and sheet rules.
export function useOptionsDisclosure({ hidden, capturing = false, toAbout = false, native = false }: {
  readonly hidden: boolean;
  readonly capturing?: boolean;
  readonly toAbout?: boolean;
  readonly native?: boolean;
}) {
  const [options, setOptions] = useState(false);
  // The Voice options button: Escape returns focus to it, and a press on it is
  // its own toggle, not an outside press (issue #2189).
  const optionsButton = useRef<HTMLButtonElement>(null);
  const optionsPanel = useRef<HTMLDivElement>(null);
  // The toggle can be inert while a sheet is open. Close and Escape restore
  // focus after the panel closes. An outside press waits for its release.
  const focusBack = useRef(false);
  // The release listeners outlive the open panel's dismissal listeners.
  const outsidePress = useRef<AbortController | null>(null);
  useLayoutEffect(() => () => { outsidePress.current?.abort(); }, []);
  useLayoutEffect(() => {
    if (options || hidden) outsidePress.current?.abort();
  }, [options, hidden]);
  const closeOptions = useCallback(() => {
    focusBack.current = true;
    setOptions(false);
  }, []);
  // A layout change may hide the anchor or call for a modal sheet (issue
  // #2189). Close the disclosure so a sheet only opens by a press. Return
  // focus to the toggle if it was in the panel and the toggle is available.
  const shrinkOptions = useCallback(() => {
    // The button may be inert or covered at that moment (a modal over it): then
    // focus stays where it is.
    const button = optionsButton.current;
    focusBack.current = optionsPanel.current?.contains(document.activeElement) === true && button !== null && button.closest('[inert]') === null;
    setOptions(false);
  }, []);
  // Whether the open panel is a bottom sheet (set by Options from its place).
  const [sheet, setSheet] = useState(false);
  // What the lobby says of the sheet (VoiceSheet), read here for the press
  // below and for the sheet's own handling after it.
  const { report, blocked, behind } = useContext(VoiceSheet);
  // Escape and an outside press close Voice options. Escape while the talk
  // key is being chosen only cancels that, as Change talk key says.
  useEffect(() => {
    if (!options || hidden) return;
    const onKey = (e: KeyboardEvent) => {
      if (e.key !== 'Escape' || e.defaultPrevented || capturing) return;
      // Settings or re-authentication is over the panel (behind): Escape is
      // theirs, even when focus has fallen to the body and their own handler
      // is not on its path.
      if (behind) return;
      closeOptions();
    };
    // Focus reaching the share quality menu's button closes the panel, so the
    // menu, which paints over it, is not drawn on top of an open panel.
    //
    // The anchored panel is a popover, not a modal: focus moving to anything
    // else than the panel or its own button closes it, where focus went (it is
    // not moved), so no control is left focused under it, covered. Only focus
    // gained counts (focusin): the window losing focus, a native picker or the
    // talk key capture moves none. Not while something is over the panel
    // (behind), and not as a sheet, which is modal and has the rest inert.
    const onFocus = (e: FocusEvent) => {
      const at = e.target;
      if (!(at instanceof Element)) return;
      if (at.closest('.share-chooser') !== null) setOptions(false);
      // Focus going into another modal (the quick switcher, whose field takes
      // it before the lobby has said it is over the panel) is not leaving it:
      // the panel stays open beneath, and Escape from that modal returns.
      else if (!sheet && !behind && !capturing && optionsPanel.current?.contains(at) !== true && optionsButton.current?.contains(at) !== true && !inOtherModal(at, optionsPanel.current)) setOptions(false);
    };
    // Keep the pointer identity for the compatibility mousedown. Dismissing
    // there also keeps the usual click on touch browsers.
    let pointerDown: PointerEvent | null = null;
    const onPointerDown = (e: PointerEvent) => { pointerDown = e; };
    const onPress = (e: MouseEvent) => {
      const { target: at, button } = e;
      const pointerId = pointerDown?.target === at && pointerDown.button === button ? pointerDown.pointerId : undefined;
      pointerDown = null;
      if (!(at instanceof Node)) return;
      if (optionsPanel.current?.contains(at) === true || optionsButton.current?.contains(at) === true) return;
      // Settings or re-authentication is over the panel (behind): a press on
      // them, their Close and their backdrop among them, is theirs, and the
      // panel stays open beneath, to take focus back when they go.
      if (behind) return;
      // Controls get their usual focus and click. On other space, wait for
      // release before recovering focus so a held press can turn into a
      // text selection. The lobby handles focus on view changes.
      const element = at instanceof Element ? at : at.parentElement;
      if (element?.closest(pressControl) == null) {
        outsidePress.current?.abort();
        const pending = new AbortController();
        outsidePress.current = pending;
        const matchesPointer = (event: MouseEvent) => pointerId === undefined || !('pointerId' in event) || event.pointerId === pointerId;
        const stranded = (el: EventTarget | null) => el === null || el === document.body || (el instanceof Node && optionsPanel.current?.contains(el) === true);
        const onRelease = (event: MouseEvent) => {
          if (event.button !== button || !matchesPointer(event) || (event.type === 'click' && event.detail === 0)) return;
          pending.abort();
          if (!stranded(document.activeElement) || window.getSelection()?.isCollapsed === false) return;
          const toggle = optionsButton.current;
          if (toggle !== null && toggle.closest('[hidden], [inert]') === null) toggle.focus();
        };
        const listen = { signal: pending.signal };
        document.addEventListener('pointerup', onRelease, listen);
        document.addEventListener('mouseup', onRelease, listen);
        document.addEventListener('click', onRelease, listen);
        document.addEventListener('pointercancel', (event) => {
          if (matchesPointer(event)) pending.abort();
        }, listen);
        // A later press of this button means the original release was missed.
        document.addEventListener('mousedown', (event) => {
          if (event.button === button) pending.abort();
        }, { ...listen, capture: true });
        document.addEventListener('focusin', (event) => {
          if (!stranded(event.target)) pending.abort();
        }, listen);
      }
      setOptions(false);
    };
    document.addEventListener('keydown', onKey);
    document.addEventListener('pointerdown', onPointerDown, true);
    document.addEventListener('mousedown', onPress);
    document.addEventListener('focusin', onFocus);
    return () => {
      document.removeEventListener('keydown', onKey);
      document.removeEventListener('pointerdown', onPointerDown, true);
      document.removeEventListener('mousedown', onPress);
      document.removeEventListener('focusin', onFocus);
    };
  }, [options, hidden, capturing, closeOptions, behind, sheet]);

  useEffect(() => {
    if (options) return;
    if (!hidden && focusBack.current) optionsButton.current?.focus();
    focusBack.current = false;
  }, [options, hidden]);

  // The sheet is modal (role dialog, aria-modal). The voice section makes
  // its controls and notes inert, and the lobby does the same for the rest
  // of the page through VoiceSheet. The phone's navigation stays available
  // as a way out. A sheet opens with focus on Close, unless More about
  // sharing takes it to the notes. In the anchored disclosure the dock
  // stays in the tab order. Re-authentication closes either placement.
  const covered = options && !hidden && sheet && !blocked;
  useEffect(() => {
    if (!covered) return;
    report(true);
    return () => { report(false); };
  }, [covered, report]);
  useEffect(() => {
    if (blocked && options) setOptions(false);
  }, [blocked, options]);
  // Close whenever the panel goes out of sight, such as switching to Chat
  // or leaving voice. Returning to the section starts with it closed. The
  // lobby recovers focus on view switches, and Leave voice has its own
  // handoff when it goes away.
  useEffect(() => {
    if (options && hidden) setOptions(false);
  }, [options, hidden]);
  // A foreign dialog can take focus before the lobby reports behind.
  const inForeignModal = () => inOtherModal(document.activeElement, optionsPanel.current);
  useEffect(() => {
    if (!covered || toAbout || behind || inForeignModal()) return;
    if (optionsPanel.current?.contains(document.activeElement) === true) return;
    optionsPanel.current?.querySelector<HTMLElement>('.voice-options-close')?.focus();
    // Behind: Settings was opened over the sheet and has closed again, and the
    // focus the sheet lost to the dock's inert is given back to it.
  }, [covered, toAbout, behind]);
  // The sheet turning into the panel above the strip (a resize, a scroll)
  // hides the browser panel's Close: focus held there moves to the first control.
  const wasCovered = useRef(false);
  useEffect(() => {
    if (!native && wasCovered.current && !covered && options && !inForeignModal()) {
      const at = document.activeElement;
      if (at === null || at === document.body || at.classList.contains('voice-options-close')) {
        // The first control that is enabled and drawn, else the panel itself
        // (Watch only has few of them).
        const controls = optionsPanel.current?.querySelectorAll<HTMLElement>('select:not(:disabled), input:not(:disabled), button:not(:disabled):not(.voice-options-close)') ?? [];
        const first = Array.from(controls).find((c) => c.offsetParent !== null);
        (first ?? optionsPanel.current)?.focus();
      }
    }
    wasCovered.current = covered;
  }, [covered, options, native]);

  // Opened with the keyboard, the anchored panel takes focus at its first
  // control. The bar containing its toggle can be inert while a sheet is
  // open, so the panel is farther down the DOM. A sheet takes focus at its
  // Close, and More about sharing at the notes. A pointer leaves focus on
  // the anchored panel's toggle.
  const byKeyboard = useRef(false);
  useEffect(() => {
    if (!options) {
      byKeyboard.current = false;
      return;
    }
    if (!byKeyboard.current || toAbout) return;
    byKeyboard.current = false;
    const panel = optionsPanel.current;
    if (panel === null || panel.classList.contains('sheet')) return;
    panelTabbables(panel)[0]?.focus();
  }, [options, toAbout]);
  // Tab order of a disclosure, kept inside the anchored panel: Shift+Tab from
  // its first control goes back to the button (the panel stays open), and Tab
  // from its last goes on to what follows the button, which closes it.
  useEffect(() => {
    const panel = optionsPanel.current;
    if (!options || sheet || panel === null) return;
    const onTab = (e: KeyboardEvent) => {
      // While the talk key is being chosen the capture has every key, Tab
      // and Shift+Tab among them, and focus stays put.
      if (e.key !== 'Tab' || e.defaultPrevented || capturing) return;
      const list = panelTabbables(panel);
      const at = document.activeElement;
      if (e.shiftKey && at === list[0]) {
        e.preventDefault();
        optionsButton.current?.focus();
      } else if (!e.shiftKey && at === list[list.length - 1]) {
        const after = optionsButton.current === null ? null : tabbablesAfter(optionsButton.current);
        if (after === null) {
          // Join can be disabled in the desktop dock. Let Tab proceed
          // through the page and dismiss the panel even in that case.
          if (native) setOptions(false);
          return;
        }
        e.preventDefault();
        after.focus();
      }
    };
    panel.addEventListener('keydown', onTab);
    return () => {
      panel.removeEventListener('keydown', onTab);
    };
  }, [options, sheet, capturing, native]);

  const button = {
    ref: optionsButton,
    'aria-expanded': options,
    'aria-controls': 'voice-options',
    onKeyDown: (e: KeyEvent<HTMLButtonElement>) => {
      // Tab from the button enters the open anchored panel, which follows
      // the dock controls in the DOM.
      const panel = optionsPanel.current;
      if (e.key !== 'Tab' || e.shiftKey || !options || sheet || panel === null || capturing) return;
      const first = panelTabbables(panel)[0];
      if (first === undefined) return;
      e.preventDefault();
      first.focus();
    },
    onClick: (e: PressEvent<HTMLButtonElement>) => {
      // Enter and Space click with no pointer (detail 0): put focus inside
      // the disclosure. A pointer press leaves it on the button.
      if (!options) byKeyboard.current = e.detail === 0;
      setOptions(!options);
    },
  };
  return { options, setOptions, covered, button, panel: optionsPanel, onClose: closeOptions, onShrink: shrinkOptions, sheet, onSheet: setSheet };
}

// Voice options: the less used choices, out of the strip. A panel over the
// page above the voice section, not a row in it (issue #2189): opening it moves
// nothing under it. It is fixed, so the dock's own scroll does not clip it;
// its place is the section's, set on it from the section's box, or a bottom
// sheet over the window where there is too little room above the section.
export function Options({ panel, onClose, onShrink, sheet, onSheet, open, anchor, children }: {
  readonly panel: RefObject<HTMLDivElement | null>;
  // Closes the panel and returns focus to its button.
  readonly onClose: () => void;
  // Closes the panel for the window having changed under it: it was anchored
  // and would now be a sheet.
  readonly onShrink: () => void;
  // Whether the panel is drawn as a bottom sheet, which is a modal dialog.
  readonly sheet: boolean;
  // Says whether the panel is drawn as a bottom sheet.
  readonly onSheet: (sheet: boolean) => void;
  readonly open: boolean;
  // The desktop toggle, in the row the panel must keep clear.
  readonly anchor?: RefObject<HTMLButtonElement | null>;
  readonly children: ReactNode;
}) {
  // The panel's left, width and bottom (and so its tallest) follow the voice
  // section's box while open: CSSOM custom properties, which the page's CSP
  // allows where a style attribute would not.
  useLayoutEffect(() => {
    const el = panel.current;
    const section = el?.parentElement;
    if (!open || !el || !section) return;
    const controls = anchor?.current?.parentElement ?? undefined;
    // Whether the panel was anchored when last placed: null until it is first.
    let anchored: boolean | null = null;
    let phone = false;
    const place = () => {
      const r = section.getBoundingClientRect();
      const view = document.documentElement.clientHeight;
      const nav = document.querySelector('.phone-nav')?.getBoundingClientRect();
      const navTop = nav === undefined ? view : Math.min(view, nav.top);
      // The desktop panel follows the toggle's whole controls row. On a
      // short Voice view, it can open below the row if there is more room
      // there. Both placements keep the controls clear even when the
      // helper's device list grows. Reserve the nav's space in either case.
      const row = controls?.getBoundingClientRect();
      const top = row === undefined ? r.top : Math.min(row.top, navTop);
      const below = row === undefined ? 0 : navTop - Math.max(0, row.bottom);
      const opensBelow = row !== undefined && top < view * 0.4 && below > top;
      const room = opensBelow ? below : top;
      // Same breakpoint as the phone layout in phone.ts.
      const narrow = window.innerWidth < 700;
      // Where the section's top leaves less than 40% of the window above it
      // (or is itself above the window, as a scrolled dock can leave it), the
      // browser panel is a bottom sheet. On desktop, the sheet shares the
      // anchored bounds and is modal only on a narrow Voice view with little
      // room on either side of the controls.
      const sheet = room < view * 0.4 && (row === undefined || narrow);
      const rowOutOfView = row !== undefined && view > 0 && (row.bottom <= 0 || row.top >= navTop);
      // Close on entering the narrow layout, losing the anchor, or when
      // the open disclosure would turn into a modal.
      if (rowOutOfView || (anchored !== null && row !== undefined && narrow && !phone) || (anchored === true && sheet)) {
        onShrink();
        return;
      }
      anchored = !sheet;
      phone = narrow;
      el.classList.toggle('sheet', sheet);
      onSheet(sheet);
      // The sheet rests on the phone's navigation, which stays in reach.
      el.style.setProperty('--vo-sheet-bottom', `${String(Math.max(0, view - navTop))}px`);
      el.style.setProperty('--vo-left', `${String(Math.max(0, r.left))}px`);
      el.style.setProperty('--vo-width', `${String(r.width)}px`);
      el.style.setProperty('--vo-bottom', `${String(Math.max(0, view - top))}px`);
      if (row !== undefined) {
        el.classList.toggle('below', opensBelow);
        el.style.setProperty('--vo-top', `${String(Math.max(0, row.bottom))}px`);
        el.style.setProperty('--vo-room', `${String(Math.max(0, room))}px`);
      }
    };
    place();
    window.addEventListener('resize', place);
    window.addEventListener('scroll', place, true);
    const watch = typeof ResizeObserver === 'undefined' ? null : new ResizeObserver(place);
    watch?.observe(section);
    if (controls !== undefined) watch?.observe(controls);
    return () => {
      window.removeEventListener('resize', place);
      window.removeEventListener('scroll', place, true);
      watch?.disconnect();
    };
  }, [open, panel, onSheet, onShrink, anchor]);
  // Make a closing native launcher inert before key setup checks focus.
  return (
    <div
      ref={panel}
      id="voice-options"
      className={anchor === undefined ? 'voice-options popover' : 'voice-options popover native'}
      hidden={!open}
      inert={anchor !== undefined && !open}
      tabIndex={-1}
      role={sheet ? 'dialog' : undefined}
      aria-modal={sheet ? true : undefined}
      aria-label={sheet ? 'Voice options' : undefined}
    >
      {/* The browser shows Close on its sheet, and the desktop in either
          placement. */}
      <button type="button" className="voice-options-close" onClick={onClose}>Close</button>
      {children}
    </div>
  );
}

function BrowserOptions({ voice, open, sharable, about, others, cameraAllowed }: {
  readonly voice: VoiceControl;
  readonly open: boolean;
  readonly cameraAllowed: boolean;
  // Everyone else in the room joined, by name, for their volumes.
  readonly others: readonly { readonly principal: number; readonly name: string }[];
  // Whether the full share notes are said: while the strip's line about
  // sharing shows.
  readonly sharable: boolean;
  readonly about: RefObject<HTMLElement | null>;
}) {
  const { state, client, features } = voice;
  return (
    <>
      {/* Where the strip no longer says it (issue #2198, UI-talk-3). */}
      {microphoneCaptured(state) ? <p className="hint">This page is using your microphone.</p> : null}
      <MicrophoneChoice voice={voice} />
      <CameraOptions voice={voice} open={open} cameraAllowed={cameraAllowed} />
      <SpeakersChoice voice={voice} />
      <Volumes voice={voice} others={others} />
      <fieldset className="voice-mode" disabled={state.listenOnly || state.watchOnly}>
        <legend>Microphone</legend>
        <label>
          <input type="radio" name="voice-mode" checked={state.mode === 'ptt'} onChange={() => { client.setMode('ptt'); }} />
          Push to talk
        </label>
        <label>
          <input type="radio" name="voice-mode" checked={state.mode === 'open'} onChange={() => { client.setMode('open'); }} />
          Open microphone
        </label>
        <ProcessingChoice voice={voice} />
      </fieldset>
      {state.mode === 'ptt' ? (
        <>
          <p className="hint talk-key-note">
            Talk key: {keyLabel(voice.talkKey)}.{' '}
            <button type="button" onClick={() => { voice.capture(!voice.capturing); }}>
              {voice.capturing ? 'Press a key or Mouse 4/5 (Escape cancels)' : 'Change talk key'}
            </button>
          </p>
          {/* How long the microphone stays open after the key comes up, so
              the last word is not cut (issue #965). */}
          <label className="threshold">
            Release delay
            <input
              type="range"
              min={0}
              max={maxTail}
              step={releaseStep}
              value={state.releaseDelay}
              aria-valuetext={releaseLabel(state.releaseDelay)}
              onKeyDown={(e) => {
                // A browser's own page step varies; here it is 250 ms (F22).
                if (e.key !== 'PageUp' && e.key !== 'PageDown') return;
                e.preventDefault();
                voice.setReleaseDelay(clampTail(state.releaseDelay + (e.key === 'PageUp' ? releasePage : -releasePage)));
              }}
              onChange={(e) => { voice.setReleaseDelay(Number(e.target.value)); }}
            />
            <output>{releaseLabel(state.releaseDelay)}</output>
          </label>
        </>
      ) : (
        <>
          <label className="threshold">
            Level that opens the microphone
            <input
              type="range"
              min={0.02}
              max={0.6}
              step={0.01}
              value={state.threshold}
              aria-describedby={state.processing ? undefined : 'voice-threshold-hint'}
              onChange={(e) => { client.setThreshold(Number(e.target.value)); }}
            />
          </label>
          {/* Without automatic gain a quiet microphone may not reach the
              level, and a noisy room may (issue #659). */}
          {state.processing ? null : (
            <p id="voice-threshold-hint" className="hint">
              {thresholdHint}
            </p>
          )}
        </>
      )}
      <p className="hint">
        The talk key works only while this tab has focus, and not while you are typing. On Linux, conch-voice has a global talk key. On a phone, audio continues
        only while the page is open on screen.
      </p>
      {sharable || (features.canShare && state.share.kind !== 'idle') ? (
        <section id="voice-share-about" ref={about} tabIndex={-1} className="voice-about" aria-labelledby="voice-share-about-title">
          <h3 id="voice-share-about-title">Sharing your screen</h3>
          <label>
            Share resolution
            <select
              value={state.shareQuality.resolution}
              aria-describedby="voice-share-estimate"
              onChange={(e) => {
                const resolution = e.target.value as ShareResolution;
                void client.setShareQuality({ ...state.shareQuality, resolution });
              }}
            >
              {shareResolutionChoices(state.shareQualityV3).map((resolution) => (
                <option key={resolution} value={resolution}>{resolutionWords[resolution]}</option>
              ))}
            </select>
          </label>
          <label>
            Share frame rate
            <select
              value={state.shareQuality.frameRate}
              disabled={state.shareQuality.resolution === 'auto'}
              onChange={(e) => {
                const frameRate = Number(e.target.value) as ShareFrameRate;
                void client.setShareQuality({ ...state.shareQuality, frameRate });
              }}
            >
              {shareFrameRates.map((frameRate) => (
                <option key={frameRate} value={frameRate}>{String(frameRate)} fps</option>
              ))}
            </select>
          </label>
          <p id="voice-share-estimate" className="hint">
            {resolutionWords[state.shareQuality.resolution]}: {estimateWords(state.shareQuality)}.
            {state.shareQuality.resolution === 'auto' ? ` ${autoNote}` : null}
            {` ${cautionFor(state.shareQuality) ?? ''}`}
          </p>
          <p className="hint">{soundNote(features)}</p>
          <p className="hint">{ownPageNote}</p>
        </section>
      ) : null}
    </>
  );
}

// ShareButton is the sharer's one control in the strip: Share screen, or Stop
// sharing while a share is starting, live or resuming. Other people's streams
// no longer disable it; only a disconnected phase does (voice-multi-share.md
// §5.1, §7.1, issue #1509; the old one-sharer refusal was issue #591, F14).
function ShareButton({ voice }: { readonly voice: VoiceControl }) {
  const { state, client, features } = voice;
  const share = state.share;
  if (state.listenOnly || !features.canShare || share.kind === 'stopping') return null;
  if (share.kind === 'idle') return (
    <span className="share-split">
      <button type="button" onClick={() => { client.startShare(); }} disabled={state.phase !== 'connected'}>
        <Glyph name="screen" />
        <span className="label">Share screen</span>
      </button>
      <ShareQualityMenu quality={state.shareQuality} v3={state.shareQualityV3} onChoose={(next) => { void client.setShareQuality(next); }} />
    </span>
  );
  return (
    <button type="button" className="sharing" onClick={() => { client.stopShare(); }}>
      <Glyph name="screenStop" />
      <span className="label">Stop sharing</span>
    </button>
  );
}

// StoppedNote is "You stopped sharing.", which the sharer knows already: it
// goes after stoppedFor, and at once when anyone's share starts, and then
// stays gone however long the other share lasts (issue #642, N03).
function StoppedNote({ text, othersShare }: { readonly text: string; readonly othersShare: boolean }) {
  const [gone, setGone] = useState(false);
  if (othersShare && !gone) setGone(true);
  useEffect(() => {
    const timer = window.setTimeout(() => { setGone(true); }, stoppedFor);
    return () => {
      window.clearTimeout(timer);
    };
  }, []);
  return gone ? null : <p role="status" className="hint">{text}</p>;
}

// ShareNotes is what the sharer's side says: what sound a share will carry and
// what it shows, before the picker opens; what is happening; and why the last
// share ended. Before a share that is one line (issue #600), and More about
// sharing opens Voice options at the full notes. Other live streams no longer
// suppress that line or the picker (voice-multi-share.md §7.1, #1509).
// A claim refused at the six-stream cap says so here; the old busy refusal and
// its who-shares line are gone (voice-multi-share.md §4, §5.1; issue #591, F14).
function ShareNotes({ voice, onMore, othersShare, showLine = true }: { readonly voice: VoiceControl; readonly onMore: () => void; readonly othersShare: boolean; readonly showLine?: boolean }) {
  const { state, features, client } = voice;
  const share = state.share;
  if (state.listenOnly) return null;
  const inCall = state.phase !== 'idle' && state.phase !== 'stopped';
  // The one-line sharing note is behind About talking and sharing.
  const line = !showLine ? null : (
    <p className="hint share-line">
      {shortShareNote(features)}{' '}
      <button type="button" className="more" aria-controls="voice-options" onClick={onMore}>More about sharing</button>
    </p>
  );
  // A browser that cannot share is told so in one sentence, and there is no
  // more to say about sharing.
  if (!features.canShare) return <p className="hint">{soundNote(features)}</p>;
  switch (share.kind) {
    case 'sharing':
      return (
        <>
          <p role="status" className="sharing-note">{sharedNote(share.audio, features)}</p>
          <ShareReadout client={client} direction="sent" prefix="Sending: " asked={state.shareQuality.resolution === 'auto' ? share.settled : { resolution: state.shareQuality.resolution, frameRate: state.shareQuality.frameRate }} />
          <ShareCaptureWarning stalled={state.shareCaptureStalled} client={client} />
          {share.resumed === true ? <p className="hint">{resumedNote}</p> : null}
        </>
      );
    case 'resuming':
      return <p role="status">{resumingNote}</p>;
    case 'claiming':
    case 'picking':
    case 'publishing':
      return <p role="status">{share.kind === 'picking' ? 'Choose what to share in the browser\'s window.' : 'Starting your share…'}</p>;
    case 'stopping':
      return <p role="status">Stopping your share…</p>;
    case 'idle':
      return (
        <>
          {line}
          {share.ended === null ? null : share.ended === 'self' ? <StoppedNote key="self" text={endedNote(share.ended, inCall)} othersShare={othersShare} /> : <p role="status" className="hint">{endedNote(share.ended, inCall)}</p>}
        </>
      );
  }
}
