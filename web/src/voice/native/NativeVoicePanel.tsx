// The voice strip of the desktop app (issue #1376, D3-5 of #771): Join, Leave,
// mute, deafen, the roster and Retry, all from the native voice store's
// confirmed state. The helper, not this page, owns the microphone and the talk
// key, so this renders no browser voice engine and asks for no device. A
// command is never optimistic: the strip shows the helper's last report, and
// "muting…" while a command waits for its answer. The destination is the
// channel joined until Leave; browsing never moves it.
import { useEffect, useRef, useState } from 'react';
import { isArchived } from '../../archived';
import { Glyph } from '../../Icons';
import type { NativeVoiceView } from '../../platform/desktop/bridge';
import type { NativeVoice, NativeVoiceError } from '../../platform/types';
import type { ChannelV0 } from '../../schema/channel';
import type { Person } from '../../useLiveChannel';
import { occupants } from '../presence';
import { Roster } from '../VoicePanel';
import { useVoiceNames } from '../useVoiceNames';
import { usePresence } from './usePresence';
import { useNativeVoice } from './useNativeVoice';
import { TalkKeySetup } from './TalkKeySetup';

// How long the duplicate-identity warning waits for a second Join.
export const confirmWithin = 10_000;

export const duplicateWarning = 'This account is already in voice from another client; joining here will disconnect it';

// Why voice cannot start here (supervisor and endpoint refusals), in words.
export const unavailableWords: Readonly<Record<string, string>> = {
  'not-installed': 'The voice helper is not installed.',
  untrusted: 'The voice helper is not trusted, so it was not started.',
  'profile-dir-unsafe': 'The voice runtime directory was refused as unsafe.',
  'not-signed-in': 'Sign in to use voice.',
};

const runtimeDirectory = 'The voice runtime directory was refused as unsafe.';

function unavailableWhy(problem: string | undefined): string | null {
  if (problem === undefined || problem === 'unsupported') return null;
  if (Object.prototype.hasOwnProperty.call(unavailableWords, problem)) return unavailableWords[problem] ?? null;
  return problem.startsWith('runtime-dir-') ? runtimeDirectory : null;
}

const errorWords: Readonly<Record<NativeVoiceError, string>> = {
  too_long: 'Voice could not do that.',
  malformed: 'Voice could not do that.',
  unknown_schema: 'Voice could not do that.',
  unknown_kind: 'Voice could not do that.',
  invalid: 'Voice could not do that.',
  id_not_increasing: 'Voice could not do that.',
  not_started: 'Voice is not running.',
  already_started: 'Voice is already running.',
  stopping: 'Voice is leaving; try again in a moment.',
  'not-running': 'Voice is not running.',
  busy: 'Voice is busy; try again in a moment.',
  unsupported: 'Voice is not available here.',
  lost: 'Voice lost touch with the app; try again.',
};

// The phase class the strip's dot follows (the browser strip's).
function phaseClass(view: NativeVoiceView): string {
  if (view.status === 'starting') return 'phase-connecting';
  if (view.status === 'running') {
    const phase = view.helper?.phase;
    if (phase === 'connected') return 'phase-connected';
    if (phase === 'reconnecting') return 'phase-reconnecting';
    return 'phase-connecting';
  }
  return 'phase-idle';
}

// A session is on while the helper runs, starts, stops or has just failed or
// been lost and may be retried or left.
function hasSession(view: NativeVoiceView): boolean {
  return view.channel !== null && view.status !== 'idle' && view.status !== 'unavailable' && view.status !== 'ended';
}

export function NativeVoicePanel({
  source,
  binding,
  signedIn,
  channel,
  viewer,
  people,
  voiceAllowed,
  channels,
  onUnauthorized,
  onOpen,
  onBound,
}: {
  readonly source: NativeVoice;
  // The talk key as the person knows it, when known.
  readonly binding?: string;
  readonly signedIn: boolean;
  readonly channel: string | null;
  readonly viewer: number;
  readonly people: ReadonlyMap<number, Person>;
  readonly voiceAllowed: boolean;
  readonly channels: readonly ChannelV0[];
  readonly onUnauthorized: () => void;
  readonly onOpen: (name: string) => void;
  // The talk key's name once the setup saved it.
  readonly onBound?: (label: string) => void;
}) {
  const { view, words, actions } = useNativeVoice(source, binding);
  const session = hasSession(view);
  const [setup, setSetup] = useState(false);
  const joined = session ? view.channel : null;
  const shown = joined ?? (signedIn ? channel : null);
  const snapshot = usePresence({ channel: shown, voiceAllowed, onUnauthorized });
  const presence = snapshot.kind === 'presence' ? snapshot.presence : null;
  const named = useVoiceNames({ joined, viewed: channel, presence, people });

  const helper = view.helper;
  // The person's last mute and deafen, as the helper confirmed them: what the
  // next Join asks for.
  const last = useRef({ muted: false, deafened: false });
  if (helper !== undefined && helper.phase === 'connected') last.current = { muted: helper.muted, deafened: helper.deafened };

  // This app's own session in the room: the joined_at presence gave while this
  // app was in voice, so a just-left session (which presence lags) is not
  // taken for another client's (#558).
  const own = useRef<{ readonly channel: string; readonly joinedAt: string } | null>(null);
  const room = occupants(presence);
  const mine = room.find((o) => o.principal === viewer);
  if (session && joined !== null && mine !== undefined && presence !== null) own.current = { channel: joined, joinedAt: mine.joinedAt };
  const duplicate = !session && mine !== undefined && !(own.current !== null && own.current.channel === shown && own.current.joinedAt === mine.joinedAt);

  const [warned, setWarned] = useState<string | null>(null);
  useEffect(() => {
    if (warned === null) return;
    const timer = window.setTimeout(() => {
      setWarned(null);
    }, confirmWithin);
    return () => {
      window.clearTimeout(timer);
    };
  }, [warned]);
  useEffect(() => {
    setWarned(null);
  }, [channel]);

  // A command waits for its answer: shown as "muting…" until then.
  const [pending, setPending] = useState<'mute' | 'deafen' | null>(null);
  const [note, setNote] = useState<string | null>(null);
  const run = async (what: 'mute' | 'deafen', go: () => Promise<{ readonly ok: boolean; readonly error?: NativeVoiceError }>) => {
    setPending(what);
    setNote(null);
    try {
      const out = await go();
      if (!out.ok) setNote(out.error === undefined ? errorWords.lost : errorWords[out.error]);
    } finally {
      setPending(null);
    }
  };
  const outcome = async (go: () => Promise<{ readonly ok: boolean; readonly error?: NativeVoiceError }>) => {
    setNote(null);
    const out = await go();
    if (!out.ok) setNote(out.error === undefined ? errorWords.lost : errorWords[out.error]);
  };

  const join = () => {
    if (actions === undefined || channel === null) return;
    if (duplicate && warned !== channel) {
      setWarned(channel);
      return;
    }
    setWarned(null);
    void outcome(() => actions.join({ channel, muted: last.current.muted, deafened: last.current.deafened }));
  };

  const available = presence !== null && presence.available && presence.configured;
  const archived = isArchived(channels.find((c) => c.name === channel));
  const cutOff = presence !== null && !presence.available;
  const roster = cutOff ? [] : room.sort((a, b) => a.principal - b.principal);
  const noSpeaking: ReadonlySet<number> = new Set();
  const noVolumes: ReadonlyMap<number, number> = new Map();

  const why = view.status === 'unavailable' ? unavailableWhy(view.problem) : null;
  const unavailable = view.status === 'unavailable';
  const live = helper !== undefined && view.status === 'running' && helper.phase === 'connected';
  const muted = helper?.muted === true;
  const deafened = helper?.deafened === true;
  const dest = channel === null ? null : <span className="dest">in #{channel}</span>;
  const where =
    joined === null ? null : joined === channel ? (
      ` in #${joined}`
    ) : (
      <>
        {' in '}
        <button type="button" className="voice-where" onClick={() => { onOpen(joined); }}>
          #{joined}
        </button>
      </>
    );

  let idle: string | null = null;
  if (!session && !unavailable) {
    if (channel === null) idle = null;
    else if (archived) idle = 'Voice is off. This channel is archived.';
    else if (presence !== null && !presence.configured) idle = 'Voice is not set up on this server';
    else if (snapshot.kind === 'not_allowed') idle = 'Your account does not include voice. Ask whoever invited you.';
    else if (snapshot.kind === 'unreachable') idle = 'Voice status unavailable: the server cannot be reached.';
    else if (snapshot.kind === 'gone') idle = 'Voice status could not be read.';
    else if (presence !== null && !presence.available) idle = 'Voice is unavailable right now; chat still works.';
  }
  const canJoin = !unavailable && channel !== null && !archived && available && view.status !== 'stopping';

  return (
    <section aria-label="Voice" className={`voice ${phaseClass(view)}${helper?.transmitting === true ? ' sending' : ''}`}>
      <div className="voice-bar">
        {roster.length === 0 ? null : <Roster roster={roster} people={named} viewer={viewer} elsewhere={false} speaking={noSpeaking} volumes={noVolumes} />}
        <div className="voice-row">
          <div className="voice-state">
            <p className="voice-phase">
              {session ? 'In voice' : 'Not in voice'}
              {where}
            </p>
            <p className="voice-sub" role="status">
              {words.text}
            </p>
          </div>
          <div className="voice-controls">
            {session ? (
              <>
                {words.retry ? (
                  <button type="button" className="primary" onClick={() => { if (actions !== undefined) void outcome(() => actions.retry()); }}>
                    <span className="label">Retry</span>
                  </button>
                ) : null}
                {live ? (
                  <>
                    <button
                      type="button"
                      aria-pressed={muted || deafened}
                      disabled={deafened || pending !== null || actions === undefined}
                      onClick={() => { if (actions !== undefined) void run('mute', () => actions.setMute(!muted)); }}
                    >
                      <Glyph name="micOff" />
                      <span className="label">{pending === 'mute' ? 'muting…' : deafened ? 'Muted by deafen' : muted ? 'Unmute' : 'Mute'}</span>
                    </button>
                    <button
                      type="button"
                      aria-pressed={deafened}
                      disabled={pending !== null || actions === undefined}
                      onClick={() => { if (actions !== undefined) void run('deafen', () => actions.setDeafen(!deafened)); }}
                    >
                      <Glyph name="headphones" />
                      <span className="label">{pending === 'deafen' ? 'deafening…' : deafened ? 'Undeafen' : 'Deafen'}</span>
                    </button>
                  </>
                ) : null}
                <button type="button" className="danger" disabled={view.status === 'stopping' || actions === undefined} onClick={() => { if (actions !== undefined) void outcome(() => actions.leave()); }}>
                  <Glyph name="leave" />
                  <span className="label">Leave voice</span>
                </button>
              </>
            ) : unavailable ? null : (
              <button type="button" className="primary" onClick={join} disabled={!canJoin || actions === undefined}>
                <Glyph name="headphones" />
                <span className="btn-text">Join voice {dest}</span>
              </button>
            )}
          </div>
        </div>
      </div>
      <div className="voice-notes">
        {why === null ? null : <p role="status" className="hint">{why}</p>}
        {note === null ? null : <p role="status" className="hint">{note}</p>}
        {idle === null ? null : <p role="status" className="hint">{idle}</p>}
        {!session && warned !== null && warned === channel ? <p role="status" className="hint">{duplicateWarning}</p> : null}
        {!session && !unavailable && channel !== null && presence === null && idle === null ? <p className="hint">Checking whether voice is available.</p> : null}
        {unavailable || source.keyDevices === undefined || setup ? null : (
          <button type="button" className="more" onClick={() => { setSetup(true); }}>
            {helper?.ptt === 'not_configured' || helper?.ptt === 'unavailable' || helper?.ptt === 'lost' ? 'Set up the talk key' : 'Talk key'}
          </button>
        )}
      </div>
      {setup ? (
        <TalkKeySetup
          source={source}
          joined={session}
          onSaved={(label) => { onBound?.(label); }}
          onClose={() => { setSetup(false); }}
        />
      ) : null}
    </section>
  );
}
