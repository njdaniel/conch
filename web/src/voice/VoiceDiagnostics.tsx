// The same disclosure in the browser dock and the desktop dock. It sits in the
// dock's notes row (.voice-notes), so it adds no line of its own to a short
// window (#1813); open, its section takes the row's full width. Closing it
// unmounts the reader, so it keeps neither a timer nor any previous samples.
import { useEffect, useId, useState, type ReactNode } from 'react';
import type { VoiceClient } from './connection';
import { startDiagnostics, type Diagnostics } from './stats';

interface NamedParticipant {
  readonly principal: number;
  readonly name: string;
}

interface Props {
  readonly client: Pick<VoiceClient, 'diagnosticsSources'> | undefined;
  readonly participants?: readonly NamedParticipant[];
  readonly sharing: boolean;
  readonly watched: readonly NamedParticipant[];
  readonly desktop?: boolean;
}

function metric(value: number | undefined, unit = '', decimals = 1): string {
  return value === undefined ? '—' : `${String(Number(value.toFixed(decimals)))}${unit}`;
}

function VoiceDiagnosticsReadout({ client, participants = [], sharing, watched, desktop }: Props) {
  const [stats, setStats] = useState<Diagnostics>();
  useEffect(() => {
    if (client === undefined) return;
    let stopped = false;
    const stop = startDiagnostics(() => client.diagnosticsSources(), (sample) => {
      if (!stopped) setStats(sample);
    });
    return () => {
      stopped = true;
      stop();
    };
  }, [client]);
  const sent = stats?.sent;
  return (
    <>
      <section aria-label="Call quality">
        <h3>Call quality</h3>
        {desktop ? <p className="hint">Call quality from the desktop voice needs a newer helper</p> : participants.length === 0 ? (
          <p className="hint">No remote participants.</p>
        ) : (
          <ul>
            {participants.map(({ principal, name }) => {
              const quality = stats?.calls.get(principal);
              return (
                <li key={principal}>
                  {name} · Packet loss {metric(quality?.lossPercent, '%')} · Jitter {metric(quality?.jitterMs, ' ms')} · Server RTT {metric(quality?.rttMs, ' ms')}
                </li>
              );
            })}
          </ul>
        )}
      </section>
      {sharing || watched.length > 0 ? (
        <section aria-label="Screen-share quality">
          <h3>Screen-share quality</h3>
          <ul>
            {sharing ? (
              <li>
                Your share · Capture {metric(sent?.captureFps, ' fps')} · Sent {metric(sent?.sentFps, ' fps')} · Resolution {sent?.size ?? '—'} · Limit {sent?.limitation ?? '—'} · Codec {sent?.codec ?? '—'}
              </li>
            ) : null}
            {watched.map(({ principal, name }) => {
              const share = stats?.received.get(principal);
              return (
                <li key={principal}>
                  {name} · Received {metric(share?.receivedFps, ' fps')} · Rendered {metric(share?.renderedFps, ' fps')} · Playback freezes {metric(share?.freezes, '', 0)} · Resolution {share?.size ?? '—'}
                </li>
              );
            })}
          </ul>
        </section>
      ) : null}
    </>
  );
}

export function VoiceDiagnostics(props: Props): ReactNode {
  const [open, setOpen] = useState(false);
  const id = useId();
  return (
    <>
      <button type="button" className="more" aria-expanded={open} aria-controls={id} onClick={() => { setOpen(!open); }}>
        Voice diagnostics
      </button>
      <section id={id} className="voice-diagnostics" aria-label="Voice diagnostics" hidden={!open}>
        {open ? <VoiceDiagnosticsReadout {...props} /> : null}
      </section>
    </>
  );
}
