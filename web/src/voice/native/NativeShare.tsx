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
import { companionDefaultQuality, companionQualities, shareQualityRefusedNote } from '../connection';
import { cautionFor, estimateWords } from '../ShareQualityMenu';
import { qualityLabel, sharers, type Sharer } from '../presence';
import type { VoicePresenceV2 } from '../../schema/voice';
import { ShareReadout, ShareView } from '../ShareView';
import { provisionalQuality, qualityLaterNote, type ShareQuality } from '../share';
import { nameOf } from '../VoicePanel';
import type { Companion } from './useCompanion';
import type { ShareSoundUi } from './useShareSound';
import {
  indicatorWords,
  noSoundRoute,
  shareDisabledWords,
  shareElsewhereWords,
  shareNote,
  shareQualityWords,
  shareSoundMuteLabel,
  shareSoundWords,
  shareWithSoundLabel,
  shareWithoutSoundNote,
  silentShareBefore,
  silentShareWhile,
  soundIndicatorWords,
} from './words';

// Why Share is not offered now, in words, or null when it is. Share is for
// the channel joined, shown, with the helper connected (UX-15). A desktop
// with no screen-cast portal is said in the notes after the grant says so; it
// never turns Share off, so there is no lock to get stuck in.
export function shareDisabledReason({ joined, shown, live }: { readonly joined: string | null; readonly shown: string | null; readonly live: boolean }): string | null {
  if (joined === null) return shareDisabledWords.notJoined;
  if (shown !== null && shown !== joined) return shareElsewhereWords(joined);
  if (!live) return shareDisabledWords.connecting;
  return null;
}

const shareDescription = 'native-share-why';
const cautionId = 'native-share-caution';
const soundWhyId = 'native-share-sound-why';

// ShareSoundToggle is the standing choice for the share's sound, shown before
// the picker and during a share (issue #1854): "Share with sound", on by
// default; unchecked is "share without sound", one click. It always shows
// main's choice, the one the next share acts on: a box that read otherwise
// would let the person believe a share is silent that is not. With no route
// (the helper has none, or the server cannot record the consent) it is
// disabled with the reason, and the share goes on silent. Before the picker
// any reason is "no route at this moment" (the helper's last state said
// unavailable, or it has no v3, or the server refused the publisher), so the
// toggle is disabled with it. Once the share is live a failure of the sound
// that was asked for does not disable it: the person may try again, or turn
// it off to withdraw the consent.
export function ShareSoundToggle({ sound }: { readonly sound: ShareSoundUi }) {
  const reason = sound.view.unavailable;
  const blocked = reason !== undefined && (noSoundRoute.has(reason) || !sound.live);
  const on = sound.view.choice === 'system_excluding_call';
  return (
    <label className="share-sound">
      <input
        type="checkbox"
        checked={on}
        disabled={blocked || sound.busy}
        aria-describedby={reason === undefined ? undefined : soundWhyId}
        onChange={(e) => {
          if (blocked || sound.busy) return;
          sound.choose(e.target.checked ? 'system_excluding_call' : 'none');
        }}
      />
      <span className="label">{shareWithSoundLabel}</span>
    </label>
  );
}

// ShareSoundMute is the share-sound mute beside Stop: its own, labelled so, and
// showing what the helper confirmed. Offered only while the helper says the
// sound is on; the microphone's Mute and Deafen are the voice strip's.
export function ShareSoundMute({ sound }: { readonly sound: ShareSoundUi }) {
  const audio = sound.audio;
  if (!sound.live || audio?.state !== 'on') return null;
  return (
    <button
      type="button"
      aria-pressed={audio.muted}
      disabled={sound.busy}
      onClick={() => {
        sound.mute(!audio.muted);
      }}
    >
      <Glyph name="speaker" />
      <span className="label">{shareSoundMuteLabel(audio.muted)}</span>
    </button>
  );
}

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
// Linux unless main's switch is on; with it the silent-share notes are not
// said. sound (issue #1854) is main's share sound: with it the toggle, the
// failure words and the indicator's "with sound" (the helper's confirmed state
// only) take the silent notes' place; without it everything is as it was.
export function ShareNotes({
  companion,
  joined,
  reason,
  soundRoute = false,
  sound,
}: {
  readonly companion: Companion;
  readonly joined: string | null;
  readonly reason: string | null;
  readonly soundRoute?: boolean;
  readonly sound?: ShareSoundUi | undefined;
}): ReactNode {
  const { client, state } = companion;
  const share = state.share;
  const live = share.kind === 'sharing' || share.kind === 'resuming' || share.kind === 'publishing';
  const starting = share.kind === 'claiming' || share.kind === 'picking';
  const offered = soundRoute || sound !== undefined;
  const soundReason = sound?.view.unavailable;
  const soundNote = sound?.note ?? null;
  const note = share.kind === 'idle' ? shareNote(share, state.companionRefusal, state.phase === 'stopped' ? state.why : null, state.companionShareUnavailable) : null;
  const quality = companionQualities.find((q) => qualityKey(q) === qualityKey(state.shareQuality)) ?? companionDefaultQuality;
  const caution = cautionFor(quality);
  // The picker is open before a share and while one is sharing: never while
  // it is claiming, picking, publishing (the capture and its sender are not
  // settled yet), resuming or stopping. A live change reaches the capture and
  // the sender without ending the share; if the capture cannot take it, or
  // conchd defers the label, setShareQuality's note says so, and the choice
  // is kept for the next share. Only those two notes are shown here: any
  // other `why` is not this picker's.
  const pickerOpen = share.kind === 'idle' ? reason === null : share.kind === 'sharing';
  const liveWhy = share.kind === 'sharing' && (state.why === shareQualityRefusedNote || state.why === qualityLaterNote) ? state.why : null;
  // The toggle: before the picker, and during a share only when the hook
  // counts it live (sharing or resuming, not publishing), so a choice is never
  // made for a share the hook does not hold yet.
  const showToggle = sound !== undefined && (share.kind === 'idle' ? reason === null : sound.live);
  return (
    <>
      {reason === null ? null : <p id={shareDescription} role="status" className="hint">{reason}</p>}
      {live || starting || share.kind === 'stopping' ? (
        <p role="status" className="hint sharing-note">
          {share.kind === 'stopping'
            ? 'Stopping your screen share…'
            : live
              ? sound === undefined
                ? indicatorWords(state.channel ?? joined ?? '')
                : soundIndicatorWords(state.channel ?? joined ?? '', sound.audio)
              : 'Starting your screen share…'}
        </p>
      ) : null}
      {!offered && (share.kind === 'idle' ? reason === null : starting) ? <p className="hint">{silentShareBefore}</p> : null}
      {share.kind === 'sharing' ? (
        <p className="hint voice-supplemental">
          <ShareReadout client={client} direction="sent" prefix="Sending: " asked={provisionalQuality(quality)} />
        </p>
      ) : null}
      {!offered && live ? <p className="hint">{silentShareWhile}</p> : null}
      {showToggle ? <ShareSoundToggle sound={sound} /> : null}
      {sound !== undefined && soundReason !== undefined && (share.kind === 'idle' ? reason === null : live || starting) ? (
        <p id={soundWhyId} role="status" className="hint">
          {shareSoundWords[soundReason]}
        </p>
      ) : null}
      {sound !== undefined && sound.view.choice === 'none' && soundReason === undefined && share.kind === 'idle' && reason === null ? (
        <p className="hint">{shareWithoutSoundNote}</p>
      ) : null}
      {soundNote === null ? null : (
        <p role="status" className="hint">
          {soundNote}
        </p>
      )}
      {pickerOpen ? (
        <label className="share-quality">
          <span className="label">Share quality</span>
          <select
            aria-describedby={caution === null ? undefined : cautionId}
            value={qualityKey(quality)}
            onChange={(e) => {
              const next = companionQualities.find((q) => qualityKey(q) === e.target.value);
              if (next !== undefined) void client.setShareQuality(next);
            }}
          >
            {companionQualities.map((q) => (
              <option key={qualityKey(q)} value={qualityKey(q)}>
                {shareQualityWords(q)} · {estimateWords(q)}
              </option>
            ))}
          </select>
        </label>
      ) : null}
      {pickerOpen && caution !== null ? <p id={cautionId} className="hint" role="note">{caution}</p> : null}
      {liveWhy === null ? null : <p role="status" className="hint">{liveWhy}</p>}
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
