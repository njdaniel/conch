// The voice section. Every state says what is true: not set up, not in voice,
// connecting, connected, reconnecting, unavailable, or stopped with the reason.
import { useEffect } from 'react';
import type { Person } from '../useLiveChannel';
import type { Phase } from './connection';
import { keyLabel } from './keys';
import { occupants } from './presence';
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
            {state.transmitting ? 'Transmitting' : 'Not transmitting'}
          </p>
          <fieldset className="voice-mode" disabled={state.listenOnly}>
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
                disabled={state.listenOnly}
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
        </>
      ) : (
        <>
          <button type="button" onClick={voice.join} disabled={!canJoin}>Join voice</button>
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
            </li>
          ))}
        </ul>
      )}
    </section>
  );
}
