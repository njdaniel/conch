import type { VoiceState } from './connection';

// VoiceStatus is the one rendering of the connection, capture and transmit
// states shared by the voice strip and the compact call header (issue #1498).
// Every word comes from VoiceState, never from the stage's layout. In the
// compact header (showPhase) it also says Reconnecting, and carries all its
// words in its title: on a wide window that header is one line, which cuts
// them short.
export function VoiceStatus({ state, showPhase = false }: { readonly state: VoiceState; readonly showPhase?: boolean }) {
  // The microphone is captured while connected with it, whether or not
  // anything is sent (UX-11): said apart from transmitting. A reconnect keeps
  // the capture it had. A microphone that went is not captured.
  const captured = microphoneCaptured(state);
  const waiting = [...state.watched].some((principal) => state.streamTracks.get(principal)?.video !== true);
  const reconnecting = showPhase && state.phase === 'reconnecting';
  // One phrase says what others hear (issue #2198, UI-talk-3). With no
  // microphone to send from, the words that say so are the whole status.
  const tx = state.listenOnly || state.watchOnly || state.viewer
    ? [
        state.muted ? 'Muted. ' : '',
        state.deafened ? 'Deafened (you hear no one, and you are muted). ' : '',
        state.listenOnly ? 'Listen only. ' : '',
        state.viewer ? 'Watching beside your voice on another device.' : state.watchOnly ? 'Watch only: your microphone is off.' : '',
      ].join('').trim()
    : state.deafened
      ? `Deafened (you hear no one, and you are muted). ${transmitLabel(state)}`
      : transmitLabel(state);
  const words = showPhase
    ? [
        reconnecting ? 'Reconnecting' : '',
        captured ? 'Microphone in use' : '',
        state.cameraTrack === 'captured' ? 'Camera in use' : '',
        state.cameraTrack === 'published' ? 'Sending video' : '',
        waiting ? 'Waiting for the picture' : '',
        tx,
      ].filter((w) => w !== '').join(' · ')
    : undefined;
  return (
    <p className="voice-sub" title={words}>
      {reconnecting ? <><span className="chip">Reconnecting</span>{' '}</> : null}
      {/* That the page holds the microphone is said in Voice options, not
          here: an amber chip beside "not sending" read as a contradiction
          (issue #2198, UI-talk-3). The tooltip of the compact header keeps it. */}
      {/* From this page's own camera track, never from presence (S5). */}
      {state.cameraTrack === 'captured' ? (
        <>
          <span className="chip">Camera in use</span>{' '}
        </>
      ) : null}
      {state.cameraTrack === 'published' ? (
        <>
          <span className="chip video-sending">Sending video</span>{' '}
        </>
      ) : null}
      {waiting ? <><span className="share-waiting">Waiting for the picture</span>{' '}</> : null}
      <span className="voice-tx">{tx}</span>
    </p>
  );
}

// microphoneCaptured is whether this page holds the microphone: while
// connected with one, whether or not anything is sent (UX-11). The strip no
// longer draws it as a chip; Voice options says it in a plain line.
export function microphoneCaptured(state: Pick<VoiceState, 'phase' | 'watchOnly' | 'listenOnly' | 'missing'>): boolean {
  return (state.phase === 'connected' || state.phase === 'reconnecting') && !state.watchOnly && !state.listenOnly && !state.missing;
}

// transmitLabel says what others hear (issues #965, #2198): "<mode>: <what is
// sent>", or one word where the mode does not matter. Sending is said only
// while audio leaves the page, and it is the one highlighted state (the word
// itself, never colour alone). A press waits for conchd's answer before the
// track opens (at most a second), and the page says so rather than claim to
// send. Muted is muted in either mode.
export function transmitLabel(state: Pick<VoiceState, 'talking' | 'transmitting' | 'mode' | 'muted' | 'deafened'>): string {
  if (state.transmitting) return state.mode === 'open' ? 'Open mic: sending' : 'Sending';
  if (state.muted || state.deafened) return 'Muted: not sending';
  if (state.talking) return 'Waiting for the server';
  return state.mode === 'open' ? 'Open mic: not sending' : 'Push to talk: not sending';
}
