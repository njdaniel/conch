// The voice section. Every state says what is true: not set up, not in voice,
// connecting, connected, reconnecting, unavailable, or stopped with the reason.
// It also holds the screen share (issue #444): sharing, with what sound, why a
// share ended, and watching someone else's.
//
// It is the strip across the bottom of the lobby (issue #536): where you are,
// whether the microphone is captured and whether you transmit, who is in
// voice, and the controls. Less used choices are under Voice options. The
// watch view is drawn into the lobby's stage with a portal, so it is still
// this one component, wherever it shows.
import { useContext, useEffect, useState } from 'react';
import { createPortal } from 'react-dom';
import { Avatar, Glyph } from '../Icons';
import { StageHost } from '../stage';
import type { Person } from '../useLiveChannel';
import type { Phase } from './connection';
import { keyLabel } from './keys';
import { occupants, sharer } from './presence';
import { endedNote, ownPageNote, resumedNote, resumingNote, sharedNote, soundNote, type ShareContent } from './share';
import { ShareView } from './ShareView';
import type { VoiceControl } from './useVoice';

const phaseLabel: Record<Phase, string> = {
  idle: 'Not in voice',
  connecting: 'Connecting',
  connected: 'Connected',
  reconnecting: 'Reconnecting',
  unavailable: 'Voice unavailable',
  stopped: 'Voice stopped',
};

function nameOf(people: ReadonlyMap<number, Person>, id: number, viewer: number): string {
  if (id === viewer) return 'You';
  return people.get(id)?.name ?? `Member ${String(id)}`;
}

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
}: {
  readonly voice: VoiceControl;
  readonly viewer: number;
  readonly people: ReadonlyMap<number, Person>;
  readonly viewed: string | null;
}) {
  const { state, snapshot, client } = voice;
  const stage = useContext(StageHost);
  const [options, setOptions] = useState(false);
  // What the next share is for, chosen under Voice options.
  const [content, setContent] = useState<ShareContent>('detail');
  const inVoice = state.phase !== 'idle' && state.phase !== 'stopped';
  const presence = snapshot.kind === 'presence' ? snapshot.presence : null;

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

  if (viewed === null && !inVoice) {
    // No channel to show (it was lost, or none is open). A stopped client
    // still says why it stopped.
    return (
      <section aria-label="Voice" className="voice">
        <div className="voice-state">
          <p className="voice-phase">{phaseLabel[state.phase]}</p>
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

  const roster = occupants(presence).sort((a, b) => a.principal - b.principal);
  const speaking = state.speaking;
  const canJoin = voice.secure && presence?.available !== false && presence !== null;
  const holder = sharer(presence);
  // Someone else's share, which this page may watch.
  const theirs = holder !== null && holder.principal !== viewer ? holder : null;
  const theirName = theirs === null ? '' : nameOf(people, theirs.principal, viewer);
  // The microphone is captured while connected with it, whether or not
  // anything is sent (UX-11): said apart from transmitting. A reconnect keeps
  // the capture it had.
  const captured = (state.phase === 'connected' || state.phase === 'reconnecting') && !state.watchOnly && !state.listenOnly;
  const view =
    theirs !== null && state.watching ? <ShareView client={client} state={state} name={theirName} audio={theirs.sharingAudio} /> : null;

  return (
    <section aria-label="Voice" className={`voice phase-${state.phase}${state.transmitting ? ' sending' : ''}`}>
      <div className="voice-bar">
        <div className="voice-state">
          <p className="voice-phase">
            {phaseLabel[state.phase]}
            {inVoice && state.channel !== null ? ` in #${state.channel}` : ''}
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

        {roster.length === 0 ? null : (
          <ul aria-label="In voice" className="voice-roster">
            {roster.map((o) => (
              <li key={o.principal} className={speaking.has(o.principal) ? 'speaking' : undefined}>
                <Avatar id={o.principal} name={people.get(o.principal)?.name ?? nameOf(people, o.principal, viewer)} />
                <span className="who">{nameOf(people, o.principal, viewer)}</span>
                {o.transmitting ? <Tag text="transmitting" /> : null}
                {speaking.has(o.principal) ? <Tag text="speaking" /> : null}
                {o.sharing ? <Tag text="sharing" /> : null}
              </li>
            ))}
          </ul>
        )}

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
              <button type="button" aria-pressed={state.muted} onClick={() => { client.setMuted(!state.muted); }}>
                <Glyph name="micOff" />
                <span className="label">{state.muted ? 'Unmute' : 'Mute'}</span>
              </button>
              <button type="button" aria-pressed={state.deafened} onClick={() => { client.setDeafened(!state.deafened); }}>
                <Glyph name="headphones" />
                <span className="label">{state.deafened ? 'Undeafen' : 'Deafen'}</span>
              </button>
              <ShareButton voice={voice} content={content} />
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
              <button type="button" className="danger" onClick={voice.leave}>
                <Glyph name="leave" />
                <span className="label">Leave voice</span>
              </button>
            </>
          ) : (
            <>
              <button type="button" className="primary" onClick={voice.join} disabled={!canJoin}>
                <Glyph name="headphones" />
                Join voice
              </button>
              <button type="button" onClick={voice.watchOnly} disabled={!canJoin}>
                Watch only
              </button>
            </>
          )}
        </div>
      </div>

      <div className="voice-notes">
        {state.why === null ? null : <p role="status" className="hint">{state.why}</p>}
        {state.reportNote === 'rate_limited' ? <p role="status" className="hint">Too many talk reports; slow down</p> : null}
        {state.reportNote === 'retrying' ? <p role="status" className="hint">Reporting your talking is slow; retrying</p> : null}
        {inVoice ? (
          <>
            {state.mode === 'ptt' && !state.listenOnly && !state.watchOnly ? (
              <p className="hint talk-key-note">Talk key: {keyLabel(voice.talkKey)}. It works only while this tab has focus.</p>
            ) : null}
            <ShareNotes voice={voice} holderName={holder === null ? null : nameOf(people, holder.principal, viewer)} othersShare={theirs !== null} />
            {theirs === null || state.watching ? null : (
              <p className="hint">
                {theirName} is sharing a screen{theirs.sharingAudio ? ', with sound' : ''}.
              </p>
            )}
          </>
        ) : (
          <>
            {theirs === null ? null : <p className="hint">{theirName} is sharing a screen. Join, or watch only, to see it.</p>}
            {!voice.secure ? (
              <p className="hint">Voice needs a secure page: use https, or localhost.</p>
            ) : presence?.available === false ? (
              <p className="hint">Voice is unavailable right now; chat still works.</p>
            ) : snapshot.kind === 'unknown' ? (
              <p className="hint">Voice status could not be read.</p>
            ) : null}
          </>
        )}
      </div>

      {inVoice ? <Options voice={voice} open={options} content={content} onContent={setContent} /> : null}
      {view === null ? null : stage === null ? view : createPortal(view, stage)}
    </section>
  );
}

// Voice options: the less used choices, out of the strip.
function Options({ voice, open, content, onContent }: {
  readonly voice: VoiceControl;
  readonly open: boolean;
  readonly content: ShareContent;
  readonly onContent: (content: ShareContent) => void;
}) {
  const { state, client } = voice;
  return (
    <div id="voice-options" className="voice-options" hidden={!open}>
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
      </fieldset>
      {state.mode === 'ptt' ? (
        <p className="hint">
          Talk key: {keyLabel(voice.talkKey)}.{' '}
          <button type="button" onClick={() => { voice.capture(!voice.capturing); }}>
            {voice.capturing ? 'Press a key (Escape cancels)' : 'Change talk key'}
          </button>
        </p>
      ) : (
        <label className="threshold">
          Level that opens the microphone
          <input
            type="range"
            min={0.02}
            max={0.6}
            step={0.01}
            value={state.threshold}
            onChange={(e) => { client.setThreshold(Number(e.target.value)); }}
          />
        </label>
      )}
      <p className="hint">
        The talk key works only while this tab has focus. On Linux, conch-voice has a global talk key. On a phone, audio continues
        only while the page is open on screen.
      </p>
      <ShareChoice voice={voice} content={content} onContent={onContent} />
    </div>
  );
}

// What a share is for, which sets how it is encoded. Chosen before sharing.
function ShareChoice({ voice, content, onContent }: {
  readonly voice: VoiceControl;
  readonly content: ShareContent;
  readonly onContent: (content: ShareContent) => void;
}) {
  const { state, features } = voice;
  if (state.listenOnly || !features.canShare || state.share.kind !== 'idle') return null;
  return (
    <fieldset className="voice-mode">
      <legend>Share your screen</legend>
      <label>
        <input type="radio" name="share-content" checked={content === 'detail'} onChange={() => { onContent('detail'); }} />
        Text and pictures
      </label>
      <label>
        <input type="radio" name="share-content" checked={content === 'motion'} onChange={() => { onContent('motion'); }} />
        Video or a game
      </label>
    </fieldset>
  );
}

// ShareButton is the sharer's one control in the strip: Share screen, or
// Stop sharing while a share is starting, live or resuming.
function ShareButton({ voice, content }: { readonly voice: VoiceControl; readonly content: ShareContent }) {
  const { state, client, features } = voice;
  const share = state.share;
  if (state.listenOnly || !features.canShare || share.kind === 'stopping') return null;
  if (share.kind === 'idle') {
    return (
      <button type="button" onClick={() => { client.startShare(content); }} disabled={state.phase !== 'connected'}>
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
// last share ended. While someone else shares no picker can open (the claim
// is refused first), so the notes about it wait until their share ends.
function ShareNotes({ voice, holderName, othersShare }: {
  readonly voice: VoiceControl;
  readonly holderName: string | null;
  readonly othersShare: boolean;
}) {
  const { state, features } = voice;
  const share = state.share;
  if (state.listenOnly) return null;
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
          {othersShare ? null : (
            <p className="hint">
              <span>{soundNote(features)}</span> <span>{ownPageNote}</span>
            </p>
          )}
          {share.ended === null ? null : (
            <p role="status" className="hint">
              {endedNote(share.ended, holderName)}
            </p>
          )}
        </>
      );
  }
}
