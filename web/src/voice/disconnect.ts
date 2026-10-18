// What to do after a voice connection ends or a session is refused: the table
// of conch-voice.md section 5, as a pure function.
import type { VoiceSessionResult } from '../api';

// Why a connection ended, in Conch's words; sdk.ts maps the SDK's reasons.
export type DisconnectCause =
  | 'room_deleted'
  | 'participant_removed'
  | 'duplicate_identity'
  | 'server_shutdown'
  | 'lost';

export type Next =
  | { readonly kind: 'new_session' }
  | { readonly kind: 'retry' }
  | { readonly kind: 'stop'; readonly why: string };

export function afterDisconnect(cause: DisconnectCause): Next {
  switch (cause) {
    case 'room_deleted':
    case 'participant_removed':
      // Conchd decides: it issues a session or says why not.
      return { kind: 'new_session' };
    case 'duplicate_identity':
      // Reconnecting would only displace the other device in turn.
      return { kind: 'stop', why: 'Another device took over your place in this voice room' };
    case 'server_shutdown':
    case 'lost':
      return { kind: 'retry' };
  }
}

type Refusal = Extract<VoiceSessionResult, { ok: false }>;

export function afterRefusal(r: Refusal): Next {
  if (r.failure === 'network') return { kind: 'retry' };
  if (r.failure === 'unauthorized') return { kind: 'stop', why: 'You are signed out' };
  if (r.code === 'voice_not_configured') return { kind: 'stop', why: 'Voice is not set up on this server' };
  if (r.code === 'voice_unavailable') return { kind: 'retry' };
  switch (r.status) {
    case 400:
      return { kind: 'stop', why: 'Voice needs a server that signs people in' };
    case 403:
      // A person whose access does not include voice (issue #437), and an
      // agent, which never uses it, are both refused with a 403.
      if (r.code === 'voice_not_allowed') return { kind: 'stop', why: 'Your account does not include voice. Ask whoever invited you.' };
      return { kind: 'stop', why: 'Voice is for people, not for this account' };
    case 404:
      return { kind: 'stop', why: 'You are not a member of this channel' };
    default:
      // 5xx, or an answer that is not a refusal conchd would give.
      return r.status !== undefined && r.status < 500 ? { kind: 'stop', why: 'The server refused voice' } : { kind: 'retry' };
  }
}

export const firstRetry = 1000;
export const maxRetry = 30_000;

// retryDelay is 1 s doubling to 30 s, with jitter (random() in [0, 1)).
export function retryDelay(attempt: number, random: () => number): number {
  const base = Math.min(firstRetry * 2 ** attempt, maxRetry);
  return Math.round(base * (0.5 + random() * 0.5));
}
