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
import { useContext, useEffect, useRef, useState, type KeyboardEvent as KeyEvent, type RefObject } from 'react';
import { createPortal } from 'react-dom';
import { Avatar, Glyph } from '../Icons';
import type { ChannelV0 } from '../schema/channel';
import { StageHost } from '../stage';
import type { Person } from '../useLiveChannel';
import type { Phase, Processing } from './connection';
import { keyLabel } from './keys';
import { occupants, sharer, type Occupant } from './presence';
import { busyNote, endedNote, othersShareNote, ownPageNote, resumedNote, resumingNote, sharedNote, shortShareNote, soundNote } from './share';
import { ShareView } from './ShareView';
import { nobody, type InVoice } from './useOccupancy';
import type { VoiceControl } from './useVoice';

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

// nameOf names someone in voice. The viewer is "You", or, while this page is
// not the one in voice, "You (on another device)" (issue #591, F24): one
// person holds one place in a room, so that place is another page's.
function nameOf(people: ReadonlyMap<number, Person>, id: number, viewer: number, elsewhere = false): string {
  if (id === viewer) return elsewhere ? 'You (on another device)' : 'You';
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
    const count = occupancy.get(channel.id) ?? 0;
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

export function VoicePanel({
  voice,
  viewer,
  people,
  viewed,
  channels = [],
  occupancy = nobody,
  onOpen,
}: {
  readonly voice: VoiceControl;
  readonly viewer: number;
  readonly people: ReadonlyMap<number, Person>;
  readonly viewed: string | null;
  // The viewer's channels, and how many are in each one's voice: for the line
  // that says where else people are talking.
  readonly channels?: readonly ChannelV0[];
  readonly occupancy?: InVoice;
  // Opens a channel, as the channel list does: the way back to the one voice
  // is in, from any other.
  readonly onOpen: (name: string) => void;
}) {
  const { state, snapshot, client } = voice;
  const stage = useContext(StageHost);
  const [options, setOptions] = useState(false);
  // More about sharing opens Voice options at the full share notes, and
  // takes focus there once they are shown (issue #600).
  const about = useRef<HTMLElement>(null);
  const [toAbout, setToAbout] = useState(false);
  const inVoice = state.phase !== 'idle' && state.phase !== 'stopped';
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
  // A watched share that ended (issue #591, F15): whose share this page was
  // watching, and the note that it stopped, said for stoppedFor.
  const watchedName = useRef<string | null>(null);
  const [stopped, setStopped] = useState<{ readonly name: string } | null>(null);
  // Someone else's share, which this page may watch.
  const holder = sharer(presence);
  const theirs = holder !== null && holder.principal !== viewer ? holder : null;
  const theirName = theirs === null ? '' : nameOf(people, theirs.principal, viewer);

  useEffect(() => {
    if (!voice.capturing) return;
    const onKey = (e: KeyboardEvent) => {
      e.preventDefault();
      if (e.code === 'Escape') voice.capture(false);
      else voice.setTalkKey(e.code);
    };
    window.addEventListener('keydown', onKey);
    return () => {
      window.removeEventListener('keydown', onKey);
    };
  }, [voice]);

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

  // The watching ends because the share is gone (its holder stopped, or one
  // that went with a renewed room did not come back in time): nobody else
  // shares when it ends. Stop watching, or leaving voice, ends it with the
  // share still there or with this page out of voice, and says nothing. A
  // share waited for after a renewal keeps the watching on, so it is not
  // said to stop.
  const holderId = theirs?.principal ?? null;
  useEffect(() => {
    if (state.watching) {
      if (holderId !== null) watchedName.current = theirName;
      return;
    }
    const name = watchedName.current;
    watchedName.current = null;
    if (name !== null && holderId === null && inVoice) setStopped({ name });
  }, [state.watching, holderId, theirName, inVoice]);

  useEffect(() => {
    if (stopped === null) return;
    const timer = window.setTimeout(() => {
      setStopped(null);
    }, stoppedFor);
    return () => {
      window.clearTimeout(timer);
    };
  }, [stopped]);

  // Where else people are talking, while this page is not in voice: in voice,
  // the strip describes the viewer's own connection.
  const away = inVoice ? null : busiest(channels, occupancy, viewed);
  const awayLine = away === null ? null : <Elsewhere where={away} onOpen={onOpen} />;

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

  const roster = occupants(presence).sort((a, b) => a.principal - b.principal);
  const speaking = state.speaking;
  // Only on a live snapshot that says voice is up: not while loading, while
  // the server cannot be reached, or after the socket was closed for good.
  const canJoin = voice.secure && presence !== null && presence.available;
  // The microphone is captured while connected with it, whether or not
  // anything is sent (UX-11): said apart from transmitting. A reconnect keeps
  // the capture it had. A microphone that went is not captured.
  const captured = (state.phase === 'connected' || state.phase === 'reconnecting') && !state.watchOnly && !state.listenOnly && !state.missing;
  const view =
    theirs !== null && state.watching ? <ShareView client={client} state={state} name={theirName} audio={theirs.sharingAudio} /> : null;
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
  // Whether this page could start a share now, but for someone else's. Then
  // who else shares is said in the line that says why Share screen is off
  // (issue #600: one line, not two), and while nobody else does, the strip's
  // one line about sharing shows, with the full notes in Voice options.
  const { features } = voice;
  const busyLine = !state.listenOnly && features.canShare && state.share.kind === 'idle';
  const sharable = busyLine && theirs === null;
  const more = () => {
    setOptions(true);
    setToAbout(true);
  };

  return (
    <section aria-label="Voice" className={`voice phase-${state.phase}${state.transmitting ? ' sending' : ''}`}>
      {/* Two rows (issue #587): who is in voice on top, and below it where
          you are and the controls. The dock sits on the window's bottom edge,
          so a roster that grows, or a longer status, moves the strip's top and
          leaves the controls where they were. */}
      <div className="voice-bar">
        {roster.length === 0 ? null : <Roster roster={roster} people={people} viewer={viewer} elsewhere={elsewhere} speaking={speaking} />}
        {awayLine}
        <div className="voice-row">
          <div className="voice-state">
            <p className="voice-phase">
              {phaseLabel[state.phase]}
              {where}
            </p>
            {inVoice ? (
              <p className="voice-sub">
                {captured ? <span className="chip">Microphone in use</span> : null}{' '}
                <span className="voice-tx">
                  {state.muted ? 'Muted. ' : ''}
                  {state.deafened ? 'Deafened (you hear no one, and you are muted). ' : ''}
                  {state.listenOnly ? 'Listen only. ' : ''}
                  {state.watchOnly ? 'Watch only: your microphone is off. ' : ''}
                  {state.transmitting ? 'Transmitting' : 'Not transmitting'}
                </span>
              </p>
            ) : (
              <p className="voice-sub">Microphone off</p>
            )}
          </div>

          <div className="voice-controls">
            {inVoice ? (
              <>
                {state.mode === 'ptt' ? (
                  <button
                    type="button"
                    className="talk"
                    aria-pressed={state.transmitting}
                    disabled={state.listenOnly || state.watchOnly}
                    onPointerDown={(e) => { e.currentTarget.setPointerCapture(e.pointerId); client.press(); }}
                    onPointerUp={() => { client.release(); }}
                    onPointerCancel={() => { client.release(); }}
                    // The keyboard holds it as a pointer does (issue #590): a
                    // held key repeats, and only its first press counts. Focus
                    // moving away lets go, as a lost pointer does.
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
                ) : (
                  <span className="open-mic">
                    <Glyph name="mic" />
                    Open microphone <meter min={0} max={1} value={state.level} aria-label="Microphone level" />
                  </span>
                )}
                {state.watchOnly && !state.listenOnly ? (
                  <button type="button" onClick={() => { void client.useMicrophone(); }} disabled={state.phase !== 'connected'}>
                    Use microphone
                  </button>
                ) : null}
                {/* Deafened is muted too (UX-11), so Mute shows pressed and
                    cannot be let go of. The choice made before is kept, and
                    is back on Undeafen (issue #591, F13). */}
                <button
                  type="button"
                  aria-pressed={state.muted || state.deafened}
                  disabled={state.deafened}
                  onClick={() => { client.setMuted(!state.muted); }}
                >
                  <Glyph name="micOff" />
                  <span className="label">{state.deafened ? 'Muted by deafen' : state.muted ? 'Unmute' : 'Mute'}</span>
                </button>
                <button type="button" aria-pressed={state.deafened} onClick={() => { client.setDeafened(!state.deafened); }}>
                  <Glyph name="headphones" />
                  <span className="label">{state.deafened ? 'Undeafen' : 'Deafen'}</span>
                </button>
                <ShareButton voice={voice} othersShare={theirs !== null} />
                {theirs !== null && !state.watching ? (
                  <button type="button" onClick={() => { client.watch(true); }} disabled={state.phase !== 'connected'}>
                    <Glyph name="screen" />
                    <span className="label">Watch</span>
                  </button>
                ) : null}
                <button type="button" className="quiet" aria-expanded={options} aria-controls="voice-options" onClick={() => { setOptions(!options); }}>
                  <Glyph name="options" />
                  <span className="label">Voice options</span>
                </button>
                <button
                  type="button"
                  className="danger"
                  onClick={(e) => {
                    left.current = document.activeElement === e.currentTarget;
                    voice.leave();
                  }}
                >
                  <Glyph name="leave" />
                  <span className="label">Leave voice</span>
                </button>
              </>
            ) : (
              <>
                <button ref={joinButton} type="button" className="primary" onClick={voice.join} disabled={!canJoin}>
                  <Glyph name="headphones" />
                  Join voice {dest}
                </button>
                <button type="button" onClick={voice.watchOnly} disabled={!canJoin} aria-describedby="voice-watch-hint">
                  Watch only {dest}
                </button>
              </>
            )}
          </div>
        </div>
      </div>

      <div className="voice-notes">
        {state.why === null ? null : <p role="status" className="hint">{state.why}</p>}
        {state.reportNote === 'rate_limited' ? <p role="status" className="hint">Too many talk reports; slow down</p> : null}
        {state.reportNote === 'retrying' ? <p role="status" className="hint">Reporting your talking is slow; retrying</p> : null}
        {inVoice ? (
          <>
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
            {state.mode === 'ptt' && !state.listenOnly && !state.watchOnly ? (
              <p className="hint talk-key-note">Talk key: {keyLabel(voice.talkKey)}. It works only while this tab has focus, and not while you are typing.</p>
            ) : null}
            <ShareNotes
              voice={voice}
              holderName={holder === null ? null : nameOf(people, holder.principal, viewer)}
              others={theirs === null ? null : { name: theirName, audio: theirs.sharingAudio }}
              onMore={more}
            />
            {stopped === null ? null : (
              <p role="status" className="hint">
                {stopped.name} stopped sharing.
              </p>
            )}
            {theirs === null || state.watching || busyLine ? null : <p className="hint">{othersShareNote(theirName, theirs.sharingAudio)}</p>}
          </>
        ) : (
          <>
            <p id="voice-watch-hint" className="hint">Microphone off. You hear the room and can watch a share.</p>
            {elsewhere ? <p className="hint">You are in voice on another device. Joining here moves your voice to this device.</p> : null}
            {theirs === null ? null : <p className="hint">{theirName} is sharing a screen. Join, or watch only, to see it.</p>}
            {!voice.secure ? (
              <p className="hint">Voice needs a secure page: use https, or localhost.</p>
            ) : presence?.available === false ? (
              <p className="hint">Voice is unavailable right now; chat still works.</p>
            ) : snapshot.kind === 'unreachable' ? (
              <p className="hint">Voice status unavailable: the server cannot be reached.</p>
            ) : snapshot.kind === 'gone' ? (
              <p className="hint">Voice status could not be read.</p>
            ) : null}
          </>
        )}
      </div>

      {inVoice ? <Options voice={voice} open={options} sharable={sharable} about={about} /> : null}
      {view === null ? null : stage === null ? view : createPortal(view, stage)}
    </section>
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
        className="quiet"
        onClick={() => {
          onOpen(where.name);
        }}
      >
        Open #{where.name}
      </button>
    </p>
  );
}

// How many chips a phone shows before "+N more" (issue #587). The cap itself
// is a CSS rule, so the strip measures nothing.
const phoneChips = 3;

// Roster is who is in voice, with how many as its heading, which also names
// the list. At 700 px and wider every chip shows, wrapping onto more lines;
// on a phone the first three show, and "+N more" shows the rest.
function Roster({ roster, people, viewer, elsewhere, speaking }: {
  readonly roster: readonly Occupant[];
  readonly people: ReadonlyMap<number, Person>;
  readonly viewer: number;
  // The viewer listed is in voice on another page, not on this one.
  readonly elsewhere: boolean;
  readonly speaking: ReadonlySet<number>;
}) {
  const [all, setAll] = useState(false);
  const more = roster.length - phoneChips;
  const capped = more > 0 && !all;
  return (
    <div className="voice-who">
      <h2 id="voice-count" className="voice-count">
        {roster.length} in voice
      </h2>
      <ul id="voice-roster" aria-labelledby="voice-count" className={capped ? 'voice-roster capped' : 'voice-roster'}>
        {roster.map((o) => (
          <li key={o.principal} className={speaking.has(o.principal) ? 'speaking' : undefined}>
            <Avatar id={o.principal} name={people.get(o.principal)?.name ?? nameOf(people, o.principal, viewer)} />
            <span className="who">{nameOf(people, o.principal, viewer, elsewhere)}</span>
            {o.transmitting ? <Tag text="transmitting" /> : null}
            {speaking.has(o.principal) ? <Tag text="speaking" /> : null}
            {o.sharing ? <Tag text="sharing" /> : null}
          </li>
        ))}
      </ul>
      {more > 0 ? (
        <button type="button" className="voice-more quiet" aria-expanded={all} aria-controls="voice-roster" onClick={() => { setAll(!all); }}>
          {all ? 'Show fewer' : `+${String(more)} more`}
        </button>
      ) : null}
    </div>
  );
}

// What the notes say while the microphone is missing.
export const missingNote = 'Your microphone was disconnected. Choose a microphone in Voice options.';

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

// Voice options: the less used choices, out of the strip.
function Options({ voice, open, sharable, about }: {
  readonly voice: VoiceControl;
  readonly open: boolean;
  // Whether the full share notes are said: while the strip's line about
  // sharing shows.
  readonly sharable: boolean;
  readonly about: RefObject<HTMLElement | null>;
}) {
  const { state, client, features } = voice;
  return (
    <div id="voice-options" className="voice-options" hidden={!open}>
      <MicrophoneChoice voice={voice} />
      <SpeakersChoice voice={voice} />
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
        <p className="hint">
          Talk key: {keyLabel(voice.talkKey)}.{' '}
          <button type="button" onClick={() => { voice.capture(!voice.capturing); }}>
            {voice.capturing ? 'Press a key (Escape cancels)' : 'Change talk key'}
          </button>
        </p>
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
      {sharable ? (
        <section id="voice-share-about" ref={about} tabIndex={-1} className="voice-about" aria-labelledby="voice-share-about-title">
          <h3 id="voice-share-about-title">Sharing your screen</h3>
          <p className="hint">{soundNote(features)}</p>
          <p className="hint">{ownPageNote}</p>
        </section>
      ) : null}
    </div>
  );
}

// ShareButton is the sharer's one control in the strip: Share screen, or
// Stop sharing while a share is starting, live or resuming. One person shares
// at a time: while someone else does, Share screen is off, and its
// description, in the notes, says who shares (issue #591, F14).
function ShareButton({ voice, othersShare }: { readonly voice: VoiceControl; readonly othersShare: boolean }) {
  const { state, client, features } = voice;
  const share = state.share;
  if (state.listenOnly || !features.canShare || share.kind === 'stopping') return null;
  if (share.kind === 'idle') {
    return (
      <button
        type="button"
        onClick={() => { client.startShare(); }}
        disabled={state.phase !== 'connected' || othersShare}
        aria-describedby={othersShare ? 'voice-share-busy' : undefined}
      >
        <Glyph name="screen" />
        <span className="label">Share screen</span>
      </button>
    );
  }
  return (
    <button type="button" className="sharing" onClick={() => { client.stopShare(); }}>
      <Glyph name="screenStop" />
      <span className="label">Stop sharing</span>
    </button>
  );
}

// ShareNotes is what the sharer's side says: what sound a share will carry
// and what it shows, before the picker opens; what is happening; and why the
// last share ended. Before a share that is one line (issue #600), and More
// about sharing opens Voice options at the full notes. While someone else
// shares no picker can open, so that line waits until their share ends, and
// one line says who shares instead, and with what sound: Share screen's
// description.
//
// A claim refused because someone else shares (busy) says the same words, so
// it is that line, read out when it comes, and it goes with the other share
// (issue #591, F14): once nobody else shares it is no longer true.
function ShareNotes({ voice, holderName, others, onMore }: {
  readonly voice: VoiceControl;
  readonly holderName: string | null;
  // Who else shares, and whether with sound, or null while nobody else does.
  readonly others: { readonly name: string; readonly audio: boolean } | null;
  readonly onMore: () => void;
}) {
  const { state, features } = voice;
  const share = state.share;
  if (state.listenOnly) return null;
  const line = (
    <p className="hint share-line">
      {shortShareNote(features)}{' '}
      <button type="button" className="more" aria-controls="voice-options" onClick={onMore}>
        More about sharing
      </button>
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
          {others === null ? (
            line
          ) : share.ended === 'busy' ? (
            <p key="refused" id="voice-share-busy" role="status" className="hint">
              {busyNote(others.name, others.audio)}
            </p>
          ) : (
            <p key="busy" id="voice-share-busy" className="hint">
              {busyNote(others.name, others.audio)}
            </p>
          )}
          {share.ended === null || share.ended === 'busy' ? null : (
            <p role="status" className="hint">
              {endedNote(share.ended, holderName)}
            </p>
          )}
        </>
      );
  }
}
