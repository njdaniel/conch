import { describe, expect, it } from 'vitest';
import type { NativeVoiceHelper, NativeVoiceStatus, NativeVoiceView } from '../../platform/desktop/bridge';
import type { VoiceControlBlock, VoiceControlPTT, VoiceControlPTTProblem, VoiceControlReason } from '../../schema/generated';
import type { CompanionGrantError } from '../../platform/desktop/bridge';
import type { ShareCause, ShareEnd, ShareState } from '../share';
import { blockWords, indicatorWords, pttProblemWords, pttWords, reasonWords, shareCauseWords, shareFailedWords, shareFullWords, shareNote, shareRefusalWords, shareSourceClosedWords, statusWords, words } from './words';

// Each Record is exhaustive at compile time; a vocabulary value added in Go
// fails the typecheck here until it is listed, and the tables below until it
// has words.
const statuses: Record<NativeVoiceStatus, true> = { idle: true, unavailable: true, starting: true, running: true, stopping: true, ended: true, failed: true, lost: true };
const reasons: Record<VoiceControlReason, true> = {
  room_deleted: true, participant_removed: true, connection_lost: true, reconnect_timed_out: true, connect_failed: true, server_unreachable: true,
  voice_unavailable: true, no_session: true, server_shutdown: true, left: true, owner_lost: true, signal: true, not_signed_in: true,
  identity_mismatch: true, bad_environment: true, bad_config: true, voice_requires_auth: true, not_a_person: true, voice_not_allowed: true,
  not_a_member: true, voice_not_configured: true, duplicate_identity: true, audio_unavailable: true, start_timeout: true, protocol_error: true,
  internal_error: true,
};
const blocks: Record<VoiceControlBlock, true> = { not_connected: true, no_publish_grant: true, no_microphone: true, deafened: true, muted: true, key_device_lost: true, max_transmit: true };
const ptts: Record<VoiceControlPTT, true> = { unknown: true, ready: true, not_configured: true, unavailable: true, lost: true };
const problems: Record<VoiceControlPTTProblem, true> = { cannot_open: true, refused: true, read_failed: true, ended: true, partial_record: true };

function view(over: Partial<NativeVoiceView> = {}): NativeVoiceView {
  return { status: 'running', generation: 1, channel: 'general', retryable: false, ...over };
}
function helper(over: Partial<NativeVoiceHelper> = {}): NativeVoiceHelper {
  return { version: 2, phase: 'connected', capture: 'on', transmitting: false, muted: false, deafened: false, blocked: [], ptt: 'ready', ...over };
}
const running = (h: Partial<NativeVoiceHelper>, binding?: string) => words(view({ helper: helper(h) }), binding).text;

describe('words tables cover every vocabulary value', () => {
  it.each(Object.keys(statuses))('status %s', (k) => {
    expect(statusWords[k as NativeVoiceStatus]).not.toBe('');
  });
  it.each(Object.keys(reasons))('reason %s', (k) => {
    expect(reasonWords[k as VoiceControlReason]).not.toBe('');
  });
  it.each(Object.keys(blocks))('block %s', (k) => {
    expect(blockWords[k as VoiceControlBlock]).not.toBe('');
  });
  it.each(Object.keys(ptts))('ptt %s', (k) => {
    expect(pttWords[k as VoiceControlPTT]).not.toBe('');
  });
  it.each(Object.keys(problems))('ptt problem %s', (k) => {
    expect(pttProblemWords[k as VoiceControlPTTProblem]).not.toBe('');
  });
  it('has no row beyond the vocabulary', () => {
    expect(Object.keys(reasonWords).sort()).toEqual(Object.keys(reasons).sort());
    expect(Object.keys(blockWords).sort()).toEqual(Object.keys(blocks).sort());
    expect(Object.keys(pttWords).sort()).toEqual(Object.keys(ptts).sort());
    expect(Object.keys(pttProblemWords).sort()).toEqual(Object.keys(problems).sort());
    expect(Object.keys(statusWords).sort()).toEqual(Object.keys(statuses).sort());
  });
});

describe('words', () => {
  it('says each status', () => {
    expect(words(view({ status: 'idle', channel: null })).text).toBe('voice off');
    expect(words(view({ status: 'starting' })).text).toBe('joining #general…');
    expect(words(view({ status: 'stopping' })).text).toBe('leaving…');
    expect(words(view({ status: 'unavailable', problem: 'unsupported' })).text).toContain('text chat is unaffected');
    expect(words(view({ status: 'running' })).text).toBe('joining #general…');
  });

  it('names a failure by its reason and offers Retry only when retryable', () => {
    const f = words(view({ status: 'failed', problem: 'connect_failed', retryable: true }));
    expect(f).toEqual({ text: 'voice failed: could not connect', retry: true });
    expect(words(view({ status: 'lost', problem: 'owner_lost' })).retry).toBe(false);
  });

  it('never shows a problem string that is not in the vocabulary', () => {
    expect(words(view({ status: 'failed', problem: 'ENOENT /home/x/helper' })).text).not.toContain('ENOENT');
  });

  it('covers every reason as a failure', () => {
    for (const r of Object.keys(reasons) as VoiceControlReason[]) {
      expect(words(view({ status: 'failed', problem: r })).text).toContain(reasonWords[r]);
    }
  });

  it('connecting and reconnecting', () => {
    expect(running({ phase: 'connecting' })).toBe('joining #general…');
    expect(running({ phase: 'reconnecting', reason: 'connection_lost', retryInMs: 2500 })).toBe('reconnecting (the connection was lost) in 3 s…');
    expect(running({ phase: 'stopping' })).toBe('leaving…');
    expect(words(view({ helper: helper({ phase: 'failed', reason: 'start_timeout' }), retryable: true }))).toEqual({
      text: 'voice failed: the voice helper did not start in time',
      retry: true,
    });
  });

  it('connected: mic on, talking, muted, deafened', () => {
    expect(running({}, 'Right Ctrl')).toBe('mic on · hold Right Ctrl to talk');
    expect(running({ talkMode: 'open' })).toBe('mic on · open mic');
    expect(running({ transmitting: true, ptt: 'ready' })).toBe('TALKING');
    expect(running({ muted: true })).toBe('muted');
    expect(running({ deafened: true, muted: true })).toBe('deafened');
  });

  it('connected: blocks', () => {
    for (const b of Object.keys(blocks) as VoiceControlBlock[]) {
      expect(running({ blocked: [b] })).toBe(blockWords[b]);
    }
    expect(running({ blocked: ['no_publish_grant'] })).toBe('listen only');
    expect(running({ blocked: ['max_transmit'] })).toBe('talk key held too long: release and press again');
  });

  it('says "no microphone", not "mic off", when capture is unavailable', () => {
    expect(running({ capture: 'unavailable' })).toBe('no microphone');
    expect(running({ capture: 'off' })).not.toContain('mic off');
  });

  it('connected: every ptt state and problem', () => {
    expect(running({ ptt: 'not_configured' })).toBe('no talk key: set one up');
    expect(running({ ptt: 'unknown' })).toBe('mic on');
    for (const p of ['unavailable', 'lost'] as const) {
      expect(running({ ptt: p })).toBe(pttWords[p]);
      for (const prob of Object.keys(problems) as VoiceControlPTTProblem[]) {
        expect(running({ ptt: p, pttProblem: prob })).toBe(pttProblemWords[prob]);
      }
    }
  });
});

// ---- share and watch (issue #1582)

const grants: Record<CompanionGrantError, true> = { 'not-in-voice': true, 'not-yet': true, elsewhere: true, ended: true, failed: true };
const causes: Record<ShareCause, true> = { no_portal: true, denied: true };
const idleShare = (ended: ShareEnd | null, cause?: ShareCause): ShareState => ({ kind: 'idle', ended, ...(cause === undefined ? {} : { cause }) });

describe('share words', () => {
  it.each(Object.keys(grants))('a refused grant: %s', (k) => {
    expect(shareRefusalWords[k as CompanionGrantError]).not.toBe('');
    expect(shareNote(idleShare(null), k as CompanionGrantError, 'the client words')).toBe(shareRefusalWords[k as CompanionGrantError]);
  });

  it.each(Object.keys(causes))('a cause: %s', (k) => {
    expect(shareCauseWords[k as ShareCause]).not.toBe('');
    expect(shareNote(idleShare('cancelled', k as ShareCause), null, null)).toBe(shareCauseWords[k as ShareCause]);
  });

  it('says the refusals the issue names, word for word', () => {
    expect(shareRefusalWords.elsewhere).toBe('Your screen is being shared from another device of yours');
    expect(shareRefusalWords['not-yet']).toBe('Voice is still connecting; try again in a moment');
    expect(shareCauseWords.no_portal).toBe('This desktop has no screen-cast portal');
    expect(shareNote(idleShare('full'), null, null)).toBe(shareFullWords);
    expect(shareFullWords).toBe('Six people are already sharing in this room.');
  });

  it('says nothing for a closed picker or a share the person stopped', () => {
    expect(shareNote(idleShare('cancelled'), null, null)).toBeNull();
    expect(shareNote(idleShare('self'), null, null)).toBeNull();
    expect(shareNote(idleShare(null), null, null)).toBeNull();
  });

  it('says why a share ended: the client reason first, then its own words', () => {
    expect(shareNote(idleShare('rotated'), null, 'renewed')).toBe('renewed');
    expect(shareNote(idleShare('rotated'), null, null)).toBeNull();
    expect(shareNote(idleShare('browser'), null, null)).toBe(shareSourceClosedWords);
    expect(shareNote(idleShare('failed'), null, null)).toBe(shareFailedWords);
    expect(shareNote(idleShare('operator'), null, null)).toBe('An operator stopped your share.');
  });

  it('says nothing about a share that is not idle', () => {
    expect(shareNote({ kind: 'sharing', audio: false }, null, 'x')).toBeNull();
  });

  it('names the channel in the indicator', () => {
    expect(indicatorWords('general')).toBe('Sharing your screen in #general');
  });
});
