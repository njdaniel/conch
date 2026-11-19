// The words of the native voice strip (issue #1375, D3-4 of #771): a pure
// mapping from the helper's confirmed state to what the person reads. They
// follow the TUI's strip (tui-voice.md section 15) so both native clients say
// the same thing. Every table is a Record over a generated vocabulary, so a
// value added to pkg/schema fails the typecheck here until it has words.
// Types only from the schema: nothing here is shaped for the wire.
import type {
  VoiceControlBlock,
  VoiceControlPTT,
  VoiceControlPTTProblem,
  VoiceControlReason,
} from '../../schema/generated';
import type { CompanionGrantError, CompanionShareUnavailable, NativeShareAudio, NativeVoiceStatus, NativeVoiceView, ShareSoundReason } from '../../platform/desktop/bridge';
import { companionRefusals } from '../connection';
import { endedNote, type ShareCause, type ShareEnd, type ShareState } from '../share';

export interface VoiceWords {
  // The one line the strip shows.
  readonly text: string;
  // Whether the strip offers Retry.
  readonly retry: boolean;
}

// Why a join or retry started nothing after the installed files were replaced
// under the running app (issue #1600); main's problem kind is 'update-pending'.
export const updatePendingWords = 'Restart Conch to finish the update.';

export const statusWords: Readonly<Record<NativeVoiceStatus, string>> = {
  idle: 'voice off',
  unavailable: 'voice is not available here; text chat is unaffected',
  starting: 'joining',
  running: 'voice on',
  stopping: 'leaving…',
  ended: 'voice ended',
  failed: 'voice failed',
  lost: 'voice lost',
};

export const reasonWords: Readonly<Record<VoiceControlReason, string>> = {
  room_deleted: 'the voice room was closed',
  participant_removed: 'you were removed from the call',
  connection_lost: 'the connection was lost',
  reconnect_timed_out: 'could not reconnect',
  connect_failed: 'could not connect',
  server_unreachable: 'the server is unreachable',
  voice_unavailable: 'voice is not available on this server',
  no_session: 'you are not signed in',
  server_shutdown: 'the server is shutting down',
  left: 'you left the call',
  owner_lost: 'the app lost its voice helper',
  signal: 'the voice helper was stopped',
  not_signed_in: 'you are not signed in',
  identity_mismatch: 'the signed-in account changed',
  bad_environment: 'the voice helper could not start: bad environment',
  bad_config: 'the voice helper could not start: bad configuration',
  voice_requires_auth: 'voice needs a signed-in account',
  not_a_person: 'voice is for people, not agents',
  voice_not_allowed: 'you are not allowed to use voice here',
  not_a_member: 'you are not a member of this channel',
  voice_not_configured: 'voice is not set up on this server',
  duplicate_identity: 'you are already in this call elsewhere',
  // A moderator's voice kick (issue #1457): the browser client's own words for
  // it (removedFromVoice in useVoice.ts), in this table's lower case.
  voice_removed: 'you were removed from voice',
  audio_unavailable: 'no audio system is available',
  start_timeout: 'the voice helper did not start in time',
  protocol_error: 'the voice helper misbehaved',
  internal_error: 'the voice helper hit an internal error',
};

export const blockWords: Readonly<Record<VoiceControlBlock, string>> = {
  not_connected: 'not connected yet',
  no_publish_grant: 'listen only',
  no_microphone: 'no microphone',
  deafened: 'deafened',
  muted: 'muted',
  key_device_lost: 'the talk key device was lost',
  max_transmit: 'talk key held too long: release and press again',
};

export const pttWords: Readonly<Record<VoiceControlPTT, string>> = {
  unknown: 'checking the talk key…',
  ready: 'ready',
  not_configured: 'no talk key: set one up',
  unavailable: 'talk key unavailable',
  lost: 'talk key lost',
};

export const pttProblemWords: Readonly<Record<VoiceControlPTTProblem, string>> = {
  cannot_open: 'the talk key device cannot be opened',
  refused: 'the talk key device refused access',
  read_failed: 'the talk key device could not be read',
  ended: 'the talk key device went away',
  partial_record: 'the talk key device sent unreadable data',
};

// The note the strip shows after main muted the person at a screen lock
// (issue #1588), until they unmute or leave. Said as a plain fact.
export const lockMutedNote = 'You were muted when your screen locked';

function has<T extends string>(table: Readonly<Record<T, string>>, key: string | undefined): key is T {
  return key !== undefined && Object.prototype.hasOwnProperty.call(table, key);
}

// A reason in words; an unknown kind (a supervisor refusal, failure or loss
// kind) gets the generic line, never the raw string.
function problemText(problem: string | undefined): string | undefined {
  if (problem === undefined) return undefined;
  return has(reasonWords, problem) ? reasonWords[problem] : 'something went wrong with voice';
}

// The short reason the strip gives beside "not available here", by the kind
// of refusal main reports (a closed set; any other kind says nothing more).
// An untrusted helper whose folder other users can change says so (issue
// #1850); the folder's name stays in main's log.
const unavailableReasonWords: Readonly<Record<string, string>> = {
  'not-installed': "the voice helper isn't installed",
  untrusted: "the voice helper isn't trusted",
  'profile-dir-unsafe': "the voice runtime folder isn't safe",
};
const folderWritableWords = "the voice helper's folder can be changed by other users";

function unavailableReason(view: NativeVoiceView): string | undefined {
  if (view.problem === 'untrusted' && view.cause === 'folder-writable') return folderWritableWords;
  return has(unavailableReasonWords, view.problem) ? unavailableReasonWords[view.problem] : undefined;
}

function where(view: NativeVoiceView): string {
  return view.channel === null ? '' : ` #${view.channel}`;
}

function joining(view: NativeVoiceView): string {
  return `joining${where(view)}…`;
}

function seconds(ms: number): string {
  return `${String(Math.max(1, Math.ceil(ms / 1000)))} s`;
}

// Blocks in the order the strip names them.
const blockOrder: readonly VoiceControlBlock[] = ['deafened', 'muted', 'no_publish_grant', 'no_microphone', 'max_transmit', 'key_device_lost', 'not_connected'];

// words turns a view into the strip's line. binding is the name of the talk
// key as the person knows it (for example "Right Ctrl"), when known.
export function words(view: NativeVoiceView, binding?: string): VoiceWords {
  const retry = view.retryable;
  switch (view.status) {
    case 'idle':
    case 'unavailable':
    case 'stopping':
    case 'ended': {
      const base = statusWords[view.status];
      if (view.status === 'unavailable') {
        const why = unavailableReason(view);
        return { text: why === undefined ? base : `${base}: ${why}`, retry: false };
      }
      const reason = view.status === 'idle' || view.status === 'stopping' ? undefined : problemText(view.problem);
      return { text: reason === undefined ? base : `${base}: ${reason}`, retry: false };
    }
    case 'failed':
    case 'lost': {
      const reason = problemText(view.problem);
      return { text: reason === undefined ? statusWords[view.status] : `${statusWords[view.status]}: ${reason}`, retry };
    }
    case 'starting':
      return { text: joining(view), retry: false };
    case 'running':
      return runningWords(view, binding);
  }
}

function runningWords(view: NativeVoiceView, binding: string | undefined): VoiceWords {
  const retry = view.retryable;
  const h = view.helper;
  if (h === undefined) return { text: joining(view), retry: false };
  switch (h.phase) {
    case 'starting':
    case 'connecting':
      return { text: joining(view), retry: false };
    case 'stopping':
    case 'stopped':
      return { text: h.phase === 'stopping' ? statusWords.stopping : statusWords.idle, retry: false };
    case 'reconnecting': {
      const why = problemText(h.reason ?? view.problem);
      const parts = ['reconnecting'];
      if (why !== undefined) parts.push(`(${why})`);
      if (h.retryInMs !== undefined) parts.push(`in ${seconds(h.retryInMs)}`);
      return { text: parts.join(' ') + '…', retry: false };
    }
    case 'failed': {
      const why = problemText(h.reason ?? view.problem);
      return { text: why === undefined ? statusWords.failed : `${statusWords.failed}: ${why}`, retry };
    }
    case 'connected':
      break;
  }
  if (h.transmitting) return { text: 'TALKING', retry: false };
  const blocked = new Set<VoiceControlBlock>(h.blocked);
  if (h.deafened) blocked.add('deafened');
  if (h.muted) blocked.add('muted');
  if (h.capture === 'unavailable') blocked.add('no_microphone');
  for (const b of blockOrder) if (blocked.has(b)) return { text: blockWords[b], retry: false };
  switch (h.ptt) {
    case 'not_configured':
      return { text: pttWords.not_configured, retry: false };
    case 'unavailable':
    case 'lost': {
      const problem = h.pttProblem === undefined ? pttWords[h.ptt] : pttProblemWords[h.pttProblem];
      return { text: problem, retry: false };
    }
    case 'unknown':
      return { text: 'mic on', retry: false };
    case 'ready':
      if (h.talkMode === 'open') return { text: 'mic on · open mic', retry: false };
      return { text: binding === undefined || binding === '' ? 'mic on · hold your talk key to talk' : `mic on · hold ${binding} to talk`, retry: false };
  }
}

// ---- sharing and watching from the desktop (issue #1582, D4-05 of #772)

// The Linux app's share carries no sound (#1552; #1578 Q-D4-1 to Q-D4-4). Said
// before the picker opens, and in short while sharing, and not said when the
// client reports a sound route (none does on Linux yet).
export const silentShareBefore = "Your screen's sound isn't shared from the Linux app yet. Others will see your screen without sound.";
export const silentShareWhile = "Sharing without sound: your screen's sound isn't shared from the Linux app yet.";

// Why Share is not offered, in words (UX-15): the strip says what to do.
export const shareDisabledWords = {
  notJoined: 'Join voice to share your screen.',
  connecting: 'Share is available once voice is connected.',
  noPortal: 'This desktop has no screen-cast portal',
} as const;

export function shareElsewhereWords(joined: string): string {
  return `Share is for the channel you are in voice with: #${joined}.`;
}

// What more is known of a share that did not start (ShareCause).
export const shareCauseWords: Readonly<Record<ShareCause, string>> = {
  denied: 'The screen could not be captured',
};

// Why this desktop cannot share, as the grant says (CompanionShareUnavailable):
// capture is unimplemented, the ScreenCast portal is missing, or the session is X11.
export const shareUnavailableWords: Readonly<Record<CompanionShareUnavailable, string>> = {
  'capture-unsupported': 'Screen sharing is not available in this desktop app yet.',
  'no-portal': shareDisabledWords.noPortal,
  'not-wayland': 'Sharing a screen from the Linux app needs a Wayland session; this desktop is running X11',
};

// voice_share_full: the room's six-stream cap (voiceShareCap = 6).
export const shareFullWords = 'Six people are already sharing in this room.';

// A claim conchd refused for any reason but a full room (a person without the
// right to share here is one) and a share that could not start. The share's
// state does not say which, so one line covers both.
export const shareFailedWords = 'Your share could not start. You may not be permitted to share in this channel.';

// The source closed under a live share: the window shared was closed, or the
// portal or system ended the capture.
export const shareSourceClosedWords = 'Sharing stopped: the screen or window you were sharing closed, or the system ended the capture.';

// Ends the strip does not say: the person stopped it, or closed the picker,
// which is not an error.
const unsaid: ReadonlySet<ShareEnd> = new Set<ShareEnd>(['self', 'cancelled']);

// Ends whose words main's end event (or a lost connection) already gave as
// the strip's reason (VoiceState.why).
const toldByWhy: ReadonlySet<ShareEnd> = new Set<ShareEnd>(['left', 'lost', 'rotated']);

// shareNote is the one line the share control says about a share that did not
// start or has ended, or null: a refused grant, a cause, a full room, the
// reason the companion ended (why, ahead of the rest of an idle share), the
// desktop's inability to share (unavailable, which the connection sets only
// after a Share), or the share's own end in words. A closed picker says
// nothing.
export function shareNote(share: ShareState, refusal: CompanionGrantError | null, why: string | null, unavailable: CompanionShareUnavailable | null = null): string | null {
  if (refusal !== null) return companionRefusals[refusal];
  if (share.kind !== 'idle') return null;
  // The companion's end words come first: a lock or a leave is news, and the
  // note of a desktop that cannot share is only background to it.
  if (why !== null && (share.ended === null || toldByWhy.has(share.ended) || share.cause !== undefined)) return why;
  if (unavailable !== null) return shareUnavailableWords[unavailable];
  if (share.cause !== undefined) return shareCauseWords[share.cause];
  if (share.ended === 'full') return shareFullWords;
  if (share.ended === null || unsaid.has(share.ended) || toldByWhy.has(share.ended)) return null;
  if (share.ended === 'browser') return shareSourceClosedWords;
  if (share.ended === 'failed') return shareFailedWords;
  return endedNote(share.ended);
}

// What each of the desktop's quality choices is called (Q-D4-3, #1772), by
// resolution and frame rate; the estimate beside it and the Source at 60 fps
// caution are the browser's (ShareQualityMenu).
export function shareQualityWords(quality: { readonly resolution: string; readonly frameRate: number }): string {
  const resolution = quality.resolution === 'source' ? 'Source (up to 4K)' : quality.resolution;
  return `${resolution}, ${String(quality.frameRate)} fps`;
}

export function indicatorWords(channel: string): string {
  return `Sharing your screen in #${channel}`;
}

// ---- the Linux share's sound (issue #1854; linux-share-audio.md §9, §11)
//
// Said only when main's switch is on (the view carries a share sound); with it
// off every word above stands as it was. None names a token, a room, a path, a
// device or an application: the reasons are a closed set, a count is a number.

// The toggle before the picker (and during a share): "Share with sound", on
// by default; off is "Share without sound", one click away.
export const shareWithSoundLabel = 'Share with sound';
export const shareWithoutSoundNote = 'Your screen will be shared without sound.';

// Why the screen's sound is not offered or not carried, in words (§11), and
// the toggle's reason when it is disabled. The helper that has no route (it
// does not speak v3) and a process tree that could not be read share the
// generic line; the detail of the second is main's log, by reason only.
const cantShareNow = "Your screen's sound can't be shared right now.";
const couldntAdd = "Sound couldn't be added to this share; try Share again.";

export const shareSoundWords: Readonly<Record<ShareSoundReason, string>> = {
  'old-helper': cantShareNow,
  'old-server': "This server can't record a sound consent for the Linux app yet. Your screen is shared without sound.",
  'consent-failed': couldntAdd,
  'no-pipewire': "Your screen's sound can't be shared right now: the sound system isn't reachable.",
  'no-process-tree': cantShareNow,
  'publish-refused': couldntAdd,
  unavailable: cantShareNow,
};

// The reasons that leave no route at all, so the toggle is disabled with them
// (before the picker or during a share). Any other reason is a failure of the
// share's sound, said beside it, and the toggle stays usable to try again.
export const noSoundRoute: ReadonlySet<ShareSoundReason> = new Set<ShareSoundReason>(['old-helper', 'old-server']);

export function soundFromWords(sources: number): string {
  return `sound from ${String(sources)} ${sources === 1 ? 'app' : 'apps'}`;
}

// The indicator with the share's sound on: "with sound" ONLY when the helper
// said on (audio is its confirmed state; nothing is optimistic). On with
// nothing playing says so; a muted track says so; otherwise the count.
export function soundIndicatorWords(channel: string, audio: NativeShareAudio | undefined): string {
  if (audio?.state !== 'on') return `Sharing your screen without sound in #${channel}`;
  const base = `Sharing your screen and its sound in #${channel}`;
  const notes = [audio.muted ? 'sound muted' : null, audio.sources === 0 ? 'nothing is playing' : soundFromWords(audio.sources)].filter((n): n is string => n !== null);
  return `${base} (${notes.join(', ')})`;
}

// The share-sound mute beside Stop, labelled so that it is not the
// microphone's: the microphone's is "Mute", this one says whose sound it is.
export function shareSoundMuteLabel(muted: boolean): string {
  return muted ? 'Unmute share sound' : 'Mute share sound';
}

// A call that did not take (the page's own "no": nothing was kept or muted).
export const shareSoundNotKept = "That choice couldn't be saved.";
export const shareSoundMuteFailed = "The share's sound could not be muted.";
