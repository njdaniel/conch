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
import type { NativeVoiceStatus, NativeVoiceView } from '../../platform/desktop/bridge';

export interface VoiceWords {
  // The one line the strip shows.
  readonly text: string;
  // Whether the strip offers Retry.
  readonly retry: boolean;
}

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
      const reason = view.status === 'idle' || view.status === 'stopping' ? undefined : problemText(view.problem);
      const base = statusWords[view.status];
      return { text: reason === undefined || view.status === 'unavailable' ? base : `${base}: ${reason}`, retry: false };
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
