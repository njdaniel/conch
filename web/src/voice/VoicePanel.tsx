// The voice section. Every state says what is true: not set up, not in voice,
// connecting, connected, reconnecting, unavailable, or stopped with the reason.
// It also holds the screen share (issue #444): sharing, with what sound, why a
// share ended, and watching someone else's.
import { useEffect, useState } from 'react';
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
        <p>{phaseLabel[state.phase]}</p>
        {state.why === null ? null : <p role="status" className="hint">{state.why}</p>}
      </section>
    );
  }
  if (!inVoice && presence !== null && !presence.configured) {
    return (
      <section aria-label="Voice" className="voice">
        <p>Not in voice</p>
        <p className="hint">Voice is not set up on this server</p>
      </section>
    );
  }

  const roster = occupants(presence).sort((a, b) => a.principal - b.principal);
  const speaking = state.speaking;
  const canJoin = voice.secure && presence?.available !== false && presence !== null;
  const holder = sharer(presence);
  // Someone else's share, which this page may watch.
  const theirs = holder !== null && holder.principal !== viewer ? holder : null;

  return (
    <section aria-label="Voice" className="voice">
      <p>
        {phaseLabel[state.phase]}
        {inVoice && state.channel !== null ? ` in #${state.channel}` : ''}
      </p>
      {state.why === null ? null : <p role="status" className="hint">{state.why}</p>}
      {state.reportNote === 'rate_limited' ? <p role="status" className="hint">Too many talk reports; slow down</p> : null}
      {state.reportNote === 'retrying' ? <p role="status" className="hint">Reporting your talking is slow; retrying</p> : null}

      {inVoice ? (
        <>
          <div className="voice-controls">
            <button type="button" aria-pressed={state.muted} onClick={() => { client.setMuted(!state.muted); }}>
              {state.muted ? 'Unmute' : 'Mute'}
            </button>
            <button type="button" aria-pressed={state.deafened} onClick={() => { client.setDeafened(!state.deafened); }}>
              {state.deafened ? 'Undeafen' : 'Deafen'}
            </button>
            <button type="button" onClick={voice.leave}>Leave voice</button>
          </div>
          <p>
            {state.muted ? 'Muted. ' : ''}
            {state.deafened ? 'Deafened (you hear no one, and you are muted). ' : ''}
            {state.listenOnly ? 'Listen only. ' : ''}
            {state.watchOnly ? 'Watch only: your microphone is off. ' : ''}
            {state.transmitting ? 'Transmitting' : 'Not transmitting'}
          </p>
          {state.watchOnly && !state.listenOnly ? (
            <button type="button" onClick={() => { void client.useMicrophone(); }} disabled={state.phase !== 'connected'}>
              Use microphone
            </button>
          ) : null}
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
            <div>
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
                Hold to talk
              </button>
              <p className="hint">
                Talk key: {keyLabel(voice.talkKey)}.{' '}
                <button type="button" onClick={() => { voice.capture(!voice.capturing); }}>
                  {voice.capturing ? 'Press a key (Escape cancels)' : 'Change talk key'}
                </button>
              </p>
            </div>
          ) : (
            <div>
              <label>
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
              <meter min={0} max={1} value={state.level} aria-label="Microphone level" />
            </div>
          )}
          <p className="hint">
            The talk key works only while this tab has focus. On Linux, conch-voice has a global talk key. On a phone, audio continues
            only while the page is open on screen.
          </p>
          <ShareControls voice={voice} holderName={holder === null ? null : nameOf(people, holder.principal, viewer)} />
          {theirs === null ? null : state.watching ? (
            <ShareView client={client} state={state} name={nameOf(people, theirs.principal, viewer)} audio={theirs.sharingAudio} />
          ) : (
            <div className="voice-share">
              <p>
                {nameOf(people, theirs.principal, viewer)} is sharing a screen{theirs.sharingAudio ? ', with sound' : ''}.
              </p>
              <button type="button" onClick={() => { client.watch(true); }} disabled={state.phase !== 'connected'}>Watch</button>
            </div>
          )}
        </>
      ) : (
        <>
          <div className="voice-controls">
            <button type="button" onClick={voice.join} disabled={!canJoin}>Join voice</button>
            <button type="button" onClick={voice.watchOnly} disabled={!canJoin}>Watch only</button>
          </div>
          {theirs === null ? null : (
            <p className="hint">{nameOf(people, theirs.principal, viewer)} is sharing a screen. Join, or watch only, to see it.</p>
          )}
          {!voice.secure ? (
            <p className="hint">Voice needs a secure page: use https, or localhost.</p>
          ) : presence?.available === false ? (
            <p className="hint">Voice is unavailable right now; chat still works.</p>
          ) : snapshot.kind === 'unknown' ? (
            <p className="hint">Voice status could not be read.</p>
          ) : null}
        </>
      )}

      {roster.length === 0 ? null : (
        <ul aria-label="In voice" className="voice-roster">
          {roster.map((o) => (
            <li key={o.principal} className={speaking.has(o.principal) ? 'speaking' : undefined}>
              {nameOf(people, o.principal, viewer)}
              {o.transmitting ? ' (transmitting)' : ''}
              {speaking.has(o.principal) ? ' (speaking)' : ''}
              {o.sharing ? ' (sharing)' : ''}
            </li>
          ))}
        </ul>
      )}
    </section>
  );
}

// ShareControls is the sharer's side: what sound a share will carry, the
// button, what is happening, and why the last share ended.
function ShareControls({ voice, holderName }: { readonly voice: VoiceControl; readonly holderName: string | null }) {
  const { state, client, features } = voice;
  const [content, setContent] = useState<ShareContent>('detail');
  const share = state.share;
  if (state.listenOnly) return null;
  if (!features.canShare) {
    return (
      <div className="voice-share">
        <p className="hint">{soundNote(features)}</p>
      </div>
    );
  }
  switch (share.kind) {
    case 'sharing':
      return (
        <div className="voice-share">
          <p role="status">{sharedNote(share.audio, features)}</p>
          {share.resumed === true ? <p className="hint">{resumedNote}</p> : null}
          <button type="button" onClick={() => { client.stopShare(); }}>Stop sharing</button>
        </div>
      );
    case 'resuming':
      return (
        <div className="voice-share">
          <p role="status">{resumingNote}</p>
          <button type="button" onClick={() => { client.stopShare(); }}>Stop sharing</button>
        </div>
      );
    case 'claiming':
    case 'picking':
    case 'publishing':
      return (
        <div className="voice-share">
          <p role="status">{share.kind === 'picking' ? 'Choose what to share in the browser\'s window.' : 'Starting your share…'}</p>
          <button type="button" onClick={() => { client.stopShare(); }}>Stop sharing</button>
        </div>
      );
    case 'stopping':
      return (
        <div className="voice-share">
          <p role="status">Stopping your share…</p>
        </div>
      );
    case 'idle':
      return (
        <div className="voice-share">
          <fieldset className="voice-mode">
            <legend>Share your screen</legend>
            <label>
              <input type="radio" name="share-content" checked={content === 'detail'} onChange={() => { setContent('detail'); }} />
              Text and pictures
            </label>
            <label>
              <input type="radio" name="share-content" checked={content === 'motion'} onChange={() => { setContent('motion'); }} />
              Video or a game
            </label>
          </fieldset>
          <p className="hint">{soundNote(features)}</p>
          <p className="hint">{ownPageNote}</p>
          <button type="button" onClick={() => { client.startShare(content); }} disabled={state.phase !== 'connected'}>
            Share screen
          </button>
          {share.ended === null ? null : (
            <p role="status" className="hint">
              {endedNote(share.ended, holderName)}
            </p>
          )}
        </div>
      );
  }
}
