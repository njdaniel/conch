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
  const captured = (state.phase === 'connected' || state.phase === 'reconnecting') && !state.watchOnly && !state.listenOnly && !state.missing;
  const waiting = [...state.watched].some((principal) => state.streamTracks.get(principal)?.video !== true);
  const reconnecting = showPhase && state.phase === 'reconnecting';
  const tx = [
    state.muted ? 'Muted. ' : '',
    state.deafened ? 'Deafened (you hear no one, and you are muted). ' : '',
    state.listenOnly ? 'Listen only. ' : '',
    state.viewer ? 'Watching beside your voice on another device. ' : state.watchOnly ? 'Watch only: your microphone is off. ' : '',
    state.mode === 'ptt' ? 'Push to talk. ' : '',
    transmitLabel(state),
  ].join('');
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
      {captured ? <span className="chip">Microphone in use</span> : null}{' '}
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

// transmitLabel says what is sent (issue #965): Transmitting only while audio
// leaves the page. A press waits for conchd's answer before the track opens
// (at most a second), and the page says so rather than claim to transmit.
export function transmitLabel(state: Pick<VoiceState, 'talking' | 'transmitting'>): string {
  if (state.transmitting) return 'Transmitting';
  return state.talking ? 'Waiting for the server' : 'Not transmitting';
}
