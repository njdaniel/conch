// Share and watch in the desktop voice strip (issue #1582, D4-05 of #772):
// the Share and Stop control, the indicator, the notes and the stream tiles,
// all from the screen companion's state (#1581). The strip's own state is the
// helper's; this is the companion's, which publishes the screen and plays
// watched shares and nothing of the person's voice, so Stop leaves mute and
// deafen alone. UX-15 (share: explicit, limits shown first, an unmistakable
// indicator and Stop, a closed source says why), UX-16 (watch: volume and
// full screen are local; stopping keeps voice) and UX-23 (honest words).
//
// Nothing here opens a picker or asks for a device: the click calls the
// client, which asks main for the grant inside that click. The browser's
// share UI is not used here; the stream tile is the browser's ShareView,
// which takes the client as a prop.
import { useRef, type ReactNode } from 'react';
import { Glyph } from '../../Icons';
import type { Person } from '../../useLiveChannel';
import type { VoiceState } from '../connection';
import { companionDefaultQuality, companionQualities } from '../connection';
import { qualityLabel, sharers, type Sharer } from '../presence';
import type { VoicePresenceV2 } from '../../schema/voice';
import { ShareView } from '../ShareView';
import type { ShareQuality } from '../share';
import { nameOf } from '../VoicePanel';
import type { Companion } from './useCompanion';
import { indicatorWords, shareDisabledWords, shareElsewhereWords, shareNote, shareQualityWords, silentShareBefore, silentShareWhile } from './words';

// Why Share is not offered now, in words, or null when it is. Share is for
// the channel joined, shown, with the helper connected (UX-15), and is gone
// for good on a desktop whose last try found no screen-cast portal.
export function shareDisabledReason({ state, joined, shown, live }: { readonly state: VoiceState; readonly joined: string | null; readonly shown: string | null; readonly live: boolean }): string | null {
  if (joined === null) return shareDisabledWords.notJoined;
  if (shown !== null && shown !== joined) return shareElsewhereWords(joined);
  if (!live) return shareDisabledWords.connecting;
  if (state.share.kind === 'idle' && state.share.cause === 'no_portal') return shareDisabledWords.noPortal;
  return null;
}

const shareDescription = 'native-share-why';

// ShareButton is Share, or Stop sharing while a share is starting, live or
// resuming. It is disabled with its reason, which the notes say.
export function ShareButton({ companion, joined, reason }: { readonly companion: Companion; readonly joined: string | null; readonly reason: string | null }) {
  const { client, state } = companion;
  const share = state.share;
  if (share.kind === 'stopping') {
    return (
      <button type="button" className="sharing" disabled>
        <Glyph name="screenStop" />
        <span className="label">Stopping…</span>
      </button>
    );
  }
  if (share.kind !== 'idle') {
    return (
      <button type="button" className="sharing" onClick={() => { client.stopShare(); }}>
        <Glyph name="screenStop" />
        <span className="label">Stop sharing</span>
      </button>
    );
  }
  return (
    <button
      type="button"
      disabled={reason !== null || joined === null}
      aria-describedby={reason === null ? undefined : shareDescription}
      onClick={() => {
        if (joined !== null) client.startCompanionShare(joined);
      }}
    >
      <Glyph name="screen" />
      <span className="label">Share screen</span>
    </button>
  );
}

function qualityKey(q: ShareQuality): string {
  return `${q.resolution}${String(q.frameRate)}`;
}

// ShareNotes is what the share control says: why it is off, what sound the
// share will carry (none, from the Linux app, said before the picker opens
// and in short while sharing), the indicator naming the channel, the
// quality choice before a share, and why the last share did not start or ended.
// soundRoute is the client reporting a way to share sound, which none does on
// Linux yet; with it the silent-share notes are not said.
export function ShareNotes({ companion, joined, reason, soundRoute = false }: { readonly companion: Companion; readonly joined: string | null; readonly reason: string | null; readonly soundRoute?: boolean }): ReactNode {
  const { client, state } = companion;
  const share = state.share;
  const live = share.kind === 'sharing' || share.kind === 'resuming' || share.kind === 'publishing';
  const starting = share.kind === 'claiming' || share.kind === 'picking';
  const note = share.kind === 'idle' ? shareNote(share, state.companionRefusal, state.phase === 'stopped' ? state.why : null) : null;
  const quality = companionQualities.find((q) => qualityKey(q) === qualityKey(state.shareQuality)) ?? companionDefaultQuality;
  return (
    <>
      {reason === null ? null : <p id={shareDescription} role="status" className="hint">{reason}</p>}
      {live || starting || share.kind === 'stopping' ? (
        <p role="status" className="hint sharing-note">
          {share.kind === 'stopping' ? 'Stopping your screen share…' : live ? indicatorWords(state.channel ?? joined ?? '') : 'Starting your screen share…'}
        </p>
      ) : null}
      {!soundRoute && (share.kind === 'idle' ? reason === null : starting) ? <p className="hint">{silentShareBefore}</p> : null}
      {!soundRoute && live ? <p className="hint">{silentShareWhile}</p> : null}
      {share.kind === 'idle' && reason === null ? (
        <label className="share-quality">
          <span className="label">Share quality</span>
          <select
            value={qualityKey(quality)}
            onChange={(e) => {
              const next = companionQualities.find((q) => qualityKey(q) === e.target.value);
              if (next !== undefined) void client.setShareQuality(next);
            }}
          >
            {companionQualities.map((q) => (
              <option key={qualityKey(q)} value={qualityKey(q)}>
                {q.resolution === '720p' || q.resolution === '1080p' ? shareQualityWords[q.resolution] : qualityKey(q)}
              </option>
            ))}
          </select>
        </label>
      ) : null}
      {note === null || note === reason ? null : <p role="status" className="hint">{note}</p>}
    </>
  );
}

// NativeStreams is a tile for each live stream of someone else in the room:
// Watch for one not watched (it starts the companion if it is not running),
// and the watch view, with its sound, volume, full screen, focus and Stop
// watching, for one that is. The person's own stream is the indicator's.
export function NativeStreams({
  companion,
  presence,
  viewer,
  people,
  joined,
  live,
}: {
  readonly companion: Companion;
  readonly presence: VoicePresenceV2 | null;
  readonly viewer: number;
  readonly people: ReadonlyMap<number, Person>;
  // The channel voice is joined in, and whether the helper is connected.
  readonly joined: string | null;
  readonly live: boolean;
}) {
  const { client, state } = companion;
  // A stream's volume survives its tile being watched again.
  const volumes = useRef(new Map<number, number>());
  if (joined === null) return null;
  const theirs: Sharer[] = sharers(presence).filter((s) => s.principal !== viewer);
  if (theirs.length === 0) return null;
  return (
    <div className="voice-streams">
      {theirs.map((stream) => {
        const name = nameOf(people, stream.principal, viewer);
        const quality = qualityLabel(stream.quality);
        if (!state.watched.has(stream.principal)) {
          return (
            <section key={stream.principal} aria-label={`${name}'s stream`} className="share-view">
              <p className="share-title">
                <Glyph name="screen" />
                {name} · LIVE{quality === null ? null : <> · {quality}</>}
                {stream.sharingAudio ? ' · with sound' : ''}
              </p>
              <div className="share-controls">
                <button type="button" disabled={!live} onClick={() => { client.watchCompanionStream(joined, stream.principal); }}>
                  Watch
                </button>
              </div>
            </section>
          );
        }
        return (
          <ShareView
            key={stream.principal}
            client={client}
            state={state}
            principal={stream.principal}
            name={name}
            audio={stream.sharingAudio}
            quality={quality}
            volume={volumes.current.get(stream.principal) ?? 1}
            onVolumeChange={(volume) => {
              volumes.current.set(stream.principal, volume);
            }}
            onFocus={() => {
              client.focusShare(state.focusedShare === stream.principal ? null : stream.principal);
            }}
          />
        );
      })}
    </div>
  );
}
