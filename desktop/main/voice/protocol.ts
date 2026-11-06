// The managed voice-helper control protocol as the desktop owner speaks it
// (issue #948; docs/design/tui-voice.md, linux-desktop.md §5): lines framed
// and read exactly as pkg/schema's Go readers read them, over the types
// generated from pkg/schema into web/src/schema/generated.ts.
//
// Input: the bytes of one line (no line feed) from the helper's events
// connection, and the commands the supervisor wants written. Output: a typed
// event or the refusal code the Go reader would give (ParseVoiceControlEvent,
// ParseVoiceControlEventV2, ParseVoiceControlPipeAttached); command lines.
// Owns: the framing rules (4096 bytes, valid UTF-8, exactly one JSON object,
// no key twice, no lone surrogate escape, exact field names, unknown fields
// ignored), each event's consistency rules, the version negotiation, and the
// owner's reading rules for a stream (the tracker). It does no I/O, so the
// fixtures in pkg/schema/testdata/voice-control*/ drive it directly
// (tests/unit/voice-protocol.test.ts replays every conformance case).
//
// It never repeats a line, a field's value or the nonce in what it returns:
// a refusal is a code and a fixed phrase.
import {
  VOICE_CONTROL_MAX_CHANNEL_BYTES,
  VOICE_CONTROL_MAX_HELPER_VERSION_BYTES,
  VOICE_CONTROL_MAX_ID,
  VOICE_CONTROL_MAX_LINE_BYTES,
  VOICE_CONTROL_MAX_RETRY_IN_MS,
  VOICE_CONTROL_MAX_SERVER_BYTES,
  VOICE_CONTROL_MAX_VERSIONS,
  VOICE_CONTROL_PIPE_NONCE_CHARS,
  VOICE_DEVICES_MAX_NAME_BYTES,
  voiceControlAckV1,
  voiceControlAckV1Schema,
  voiceControlAckV2,
  voiceControlGetStateV1,
  voiceControlGetStateV1Schema,
  voiceControlGetStateV2,
  voiceControlGetStateV2Schema,
  voiceControlHelloV1,
  voiceControlLeaveV1,
  voiceControlLeaveV2,
  voiceControlPipeAttachedV1,
  voiceControlPipeAttachedV1Schema,
  voiceControlProtocolErrorV1,
  voiceControlProtocolErrorV2,
  voiceControlSetDeafenV1,
  voiceControlSetDeafenV2,
  voiceControlSetDevicesV2,
  voiceControlSetMuteV1,
  voiceControlSetMuteV2,
  voiceControlSetTalkModeV2,
  voiceControlStartV1,
  voiceControlStartV2,
  voiceControlStartV2Schema,
  voiceControlStateV1,
  voiceControlStateV2,
  type VoiceControlAckV1,
  type VoiceControlAckV2,
  type VoiceControlBlock,
  type VoiceControlCapture,
  type VoiceControlErrorCode,
  type VoiceControlEventKind,
  type VoiceControlGetStateV1,
  type VoiceControlGetStateV2,
  type VoiceControlHelloV1,
  type VoiceControlLeaveV1,
  type VoiceControlLeaveV2,
  type VoiceControlPhase,
  type VoiceControlPipeAttachedV1,
  type VoiceControlPlayback,
  type VoiceControlProtocolErrorV1,
  type VoiceControlProtocolErrorV2,
  type VoiceControlPTT,
  type VoiceControlPTTProblem,
  type VoiceControlReason,
  type VoiceControlSetDeafenV1,
  type VoiceControlSetDeafenV2,
  type VoiceControlSetDevicesV2,
  type VoiceControlSetMuteV1,
  type VoiceControlSetMuteV2,
  type VoiceControlSetTalkModeV2,
  type VoiceControlStartV1,
  type VoiceControlStartV2,
  type VoiceControlStateV1,
  type VoiceControlStateV2,
  type VoiceControlTalkMode,
} from '../../../web/src/schema/generated.ts';
import type { Validator } from '../../../web/src/schema/validate.ts';

export const SCHEMA_V1 = voiceControlAckV1Schema;
export const SCHEMA_V2 = voiceControlStartV2Schema;
export const PIPE_SCHEMA_V1 = voiceControlPipeAttachedV1Schema;

// The versions this owner implements, best first.
export type Version = typeof SCHEMA_V1 | typeof SCHEMA_V2;
export const OWNER_VERSIONS: readonly Version[] = [SCHEMA_V2, SCHEMA_V1];

// ---------------------------------------------------------------- framing

// A refusal: the wire code Go's reader returns, and a fixed phrase that never
// holds anything from the line.
export interface Refusal {
  readonly ok: false;
  readonly code: VoiceControlErrorCode;
  readonly detail: string;
}

function refuse(code: VoiceControlErrorCode, detail: string): Refusal {
  return { ok: false, code, detail };
}

// One decoded JSON value. Numbers keep their text, so an integer field is
// read as Go's encoding/json reads an int64 (no fraction, no exponent).
type Json =
  | { readonly t: 'str'; readonly v: string }
  | { readonly t: 'num'; readonly raw: string }
  | { readonly t: 'bool'; readonly v: boolean }
  | { readonly t: 'null' }
  | { readonly t: 'arr'; readonly items: readonly Json[] }
  | { readonly t: 'obj' };

type Fields = ReadonlyMap<string, Json>;

const utf8 = new TextDecoder('utf-8', { fatal: true, ignoreBOM: true });

class Malformed extends Error {}

// A strict JSON reader over one line: exactly RFC 8259, the whitespace Go
// allows, and Go's one extra refusal of a \u escape of a surrogate that is not
// half of a pair (hasLoneSurrogateEscape). Duplicate keys are refused at the
// top level only, as decodeVoiceControlObject does.
class Reader {
  private at = 0;

  constructor(private readonly text: string) {}

  object(): Map<string, Json> {
    this.ws();
    if (this.text[this.at] !== '{') throw new Malformed();
    this.at++;
    const fields = new Map<string, Json>();
    this.ws();
    if (this.text[this.at] === '}') {
      this.at++;
    } else {
      for (;;) {
        this.ws();
        const key = this.string();
        this.ws();
        if (this.text[this.at] !== ':') throw new Malformed();
        this.at++;
        const value = this.value();
        if (fields.has(key)) throw new Duplicate();
        fields.set(key, value);
        this.ws();
        const c = this.text[this.at];
        this.at++;
        if (c === '}') break;
        if (c !== ',') throw new Malformed();
      }
    }
    this.ws();
    if (this.at !== this.text.length) throw new Malformed();
    return fields;
  }

  private ws(): void {
    for (;;) {
      const c = this.text[this.at];
      if (c === ' ' || c === '\t' || c === '\r' || c === '\n') this.at++;
      else return;
    }
  }

  private value(): Json {
    this.ws();
    const c = this.text[this.at];
    switch (c) {
      case '"':
        return { t: 'str', v: this.string() };
      case '{':
        this.skipObject();
        return { t: 'obj' };
      case '[':
        return { t: 'arr', items: this.array() };
      case 't':
        this.word('true');
        return { t: 'bool', v: true };
      case 'f':
        this.word('false');
        return { t: 'bool', v: false };
      case 'n':
        this.word('null');
        return { t: 'null' };
      default:
        return { t: 'num', raw: this.number() };
    }
  }

  private word(w: string): void {
    if (this.text.slice(this.at, this.at + w.length) !== w) throw new Malformed();
    this.at += w.length;
  }

  private skipObject(): void {
    this.at++; // {
    this.ws();
    if (this.text[this.at] === '}') {
      this.at++;
      return;
    }
    for (;;) {
      this.ws();
      this.string();
      this.ws();
      if (this.text[this.at] !== ':') throw new Malformed();
      this.at++;
      this.value();
      this.ws();
      const c = this.text[this.at];
      this.at++;
      if (c === '}') return;
      if (c !== ',') throw new Malformed();
    }
  }

  private array(): Json[] {
    this.at++; // [
    const items: Json[] = [];
    this.ws();
    if (this.text[this.at] === ']') {
      this.at++;
      return items;
    }
    for (;;) {
      items.push(this.value());
      this.ws();
      const c = this.text[this.at];
      this.at++;
      if (c === ']') return items;
      if (c !== ',') throw new Malformed();
    }
  }

  private static readonly NUMBER = /-?(?:0|[1-9][0-9]*)(?:\.[0-9]+)?(?:[eE][+-]?[0-9]+)?/y;

  private number(): string {
    Reader.NUMBER.lastIndex = this.at;
    const m = Reader.NUMBER.exec(this.text);
    if (m === null || m[0] === '') throw new Malformed();
    this.at += m[0].length;
    return m[0];
  }

  private hex4(): number {
    const h = this.text.slice(this.at, this.at + 4);
    if (!/^[0-9A-Fa-f]{4}$/.test(h)) throw new Malformed();
    this.at += 4;
    return Number.parseInt(h, 16);
  }

  private string(): string {
    if (this.text[this.at] !== '"') throw new Malformed();
    this.at++;
    let out = '';
    for (;;) {
      const c = this.text[this.at];
      if (c === undefined) throw new Malformed();
      this.at++;
      if (c === '"') return out;
      if (c < ' ') throw new Malformed();
      if (c !== '\\') {
        out += c;
        continue;
      }
      const e = this.text[this.at];
      this.at++;
      switch (e) {
        case '"':
        case '\\':
        case '/':
          out += e;
          break;
        case 'b':
          out += '\b';
          break;
        case 'f':
          out += '\f';
          break;
        case 'n':
          out += '\n';
          break;
        case 'r':
          out += '\r';
          break;
        case 't':
          out += '\t';
          break;
        case 'u': {
          const unit = this.hex4();
          if (unit >= 0xdc00 && unit <= 0xdfff) throw new LoneSurrogate();
          if (unit >= 0xd800 && unit < 0xdc00) {
            if (this.text.slice(this.at, this.at + 2) !== '\\u') throw new LoneSurrogate();
            this.at += 2;
            const low = this.hex4();
            if (low < 0xdc00 || low > 0xdfff) throw new LoneSurrogate();
            out += String.fromCharCode(unit, low);
          } else {
            out += String.fromCharCode(unit);
          }
          break;
        }
        default:
          throw new Malformed();
      }
    }
  }
}

class Duplicate extends Error {}
class LoneSurrogate extends Error {}

// decodeObject applies the framing rules shared by every reader
// (decodeVoiceControlObject) and returns the object's fields.
export function decodeObject(line: Uint8Array): { readonly ok: true; readonly fields: Fields } | Refusal {
  if (line.length > VOICE_CONTROL_MAX_LINE_BYTES) return refuse('too_long', 'the line is too long');
  let text: string;
  try {
    text = utf8.decode(line);
  } catch {
    return refuse('malformed', 'not valid UTF-8');
  }
  if (text.includes('\n')) return refuse('malformed', 'a line feed inside the line');
  try {
    return { ok: true, fields: new Reader(text).object() };
  } catch (err) {
    if (err instanceof Duplicate) return refuse('malformed', 'a key given twice');
    if (err instanceof LoneSurrogate) return refuse('malformed', 'a lone surrogate escape');
    return refuse('malformed', 'not exactly one JSON object');
  }
}

// ----------------------------------------------------------- field readers

const INTEGER = /^-?(?:0|[1-9][0-9]*)$/;
const INT64_MIN = -(2n ** 63n);
const INT64_MAX = 2n ** 63n - 1n;

// asInt reads a number as encoding/json reads it into an int64: integer text
// in range, or nothing. Values past VOICE_CONTROL_MAX_ID are clamped to one
// past it, which every caller refuses.
function asInt(v: Json | undefined): number | undefined {
  if (v?.t !== 'num' || !INTEGER.test(v.raw)) return undefined;
  const n = BigInt(v.raw);
  if (n < INT64_MIN || n > INT64_MAX) return undefined;
  if (n > BigInt(VOICE_CONTROL_MAX_ID)) return VOICE_CONTROL_MAX_ID + 1;
  if (n < -BigInt(VOICE_CONTROL_MAX_ID)) return -VOICE_CONTROL_MAX_ID - 1;
  return Number(n);
}

function fieldString(fields: Fields, key: string): string | undefined {
  const v = fields.get(key);
  return v?.t === 'str' ? v.v : undefined;
}

// FieldReader takes fields by exact name and remembers the first required
// one that was missing, null or of the wrong type (Go's fieldReader).
class FieldReader {
  missing = '';

  constructor(private readonly fields: Fields) {}

  private miss(key: string): void {
    if (this.missing === '') this.missing = key;
  }

  str(key: string): string {
    const v = this.fields.get(key);
    if (v?.t === 'str') return v.v;
    this.miss(key);
    return '';
  }

  // optWord: absent is none; present, it must be a non-empty string.
  optWord(key: string): string | undefined {
    const v = this.fields.get(key);
    if (v === undefined) return undefined;
    if (v.t === 'str' && v.v !== '') return v.v;
    this.miss(key);
    return undefined;
  }

  int(key: string): number {
    const n = asInt(this.fields.get(key));
    if (n !== undefined) return n;
    this.miss(key);
    return 0;
  }

  optInt(key: string): number | undefined {
    const v = this.fields.get(key);
    if (v === undefined) return undefined;
    const n = asInt(v);
    if (n === undefined) this.miss(key);
    return n;
  }

  bool(key: string): boolean {
    const v = this.fields.get(key);
    if (v?.t === 'bool') return v.v;
    this.miss(key);
    return false;
  }

  // strs: an array of strings; a null element is "" as encoding/json reads
  // it, which every caller then refuses as not a word of its vocabulary.
  strs(key: string): string[] {
    const v = this.fields.get(key);
    if (v?.t !== 'arr') {
      this.miss(key);
      return [];
    }
    const out: string[] = [];
    for (const item of v.items) {
      if (item.t === 'str') out.push(item.v);
      else if (item.t === 'null') out.push('');
      else {
        this.miss(key);
        return [];
      }
    }
    return out;
  }
}

// ------------------------------------------------------- consistency rules

const VERSION_NAME = /^conch\.voice_control\.v[1-9][0-9]{0,3}$/;
const ACK_REFUSALS: readonly string[] = ['unknown_kind', 'invalid', 'already_started', 'stopping'];
const FATAL: readonly string[] = ['too_long', 'malformed', 'unknown_schema', 'invalid', 'id_not_increasing', 'not_started'];
const BLOCK_ORDER: readonly VoiceControlBlock[] = [
  'not_connected',
  'no_publish_grant',
  'no_microphone',
  'deafened',
  'muted',
  'key_device_lost',
  'max_transmit',
];
const PHASES: readonly string[] = ['starting', 'connecting', 'connected', 'reconnecting', 'stopping', 'stopped', 'failed'];
const RETRY_REASONS: readonly string[] = [
  'room_deleted',
  'participant_removed',
  'connection_lost',
  'reconnect_timed_out',
  'connect_failed',
  'server_unreachable',
  'voice_unavailable',
  'no_session',
  'server_shutdown',
];
const STOP_REASONS: readonly string[] = ['left', 'owner_lost', 'signal'];
const FAIL_REASONS: readonly string[] = [
  'not_signed_in',
  'identity_mismatch',
  'bad_environment',
  'bad_config',
  'voice_requires_auth',
  'not_a_person',
  'voice_not_allowed',
  'not_a_member',
  'voice_not_configured',
  'duplicate_identity',
  'audio_unavailable',
  'server_unreachable',
  'start_timeout',
  'protocol_error',
  'internal_error',
];
const ALL_REASONS: readonly string[] = [...RETRY_REASONS, ...STOP_REASONS, ...FAIL_REASONS];

export function finalPhase(phase: VoiceControlPhase): boolean {
  return phase === 'stopped' || phase === 'failed';
}

function validId(n: number): boolean {
  return Number.isSafeInteger(n) && n >= 1 && n <= VOICE_CONTROL_MAX_ID;
}

function helloProblem(h: VoiceControlHelloV1): string | null {
  if (h.seq !== 1) return 'hello seq must be 1';
  if (h.versions.length === 0 || h.versions.length > VOICE_CONTROL_MAX_VERSIONS) return 'hello versions out of range';
  for (let i = 0; i < h.versions.length; i++) {
    const v = h.versions[i] ?? '';
    if (!VERSION_NAME.test(v)) return 'hello version is not a voice control schema name';
    if (h.versions.slice(0, i).includes(v)) return 'hello offers a version twice';
  }
  const bytes = Buffer.from(h.helper_version, 'utf8');
  if (bytes.length === 0 || bytes.length > VOICE_CONTROL_MAX_HELPER_VERSION_BYTES) return 'hello helper_version out of range';
  for (const b of bytes) if (b < 0x20 || b > 0x7e) return 'hello helper_version is not printable ASCII';
  return null;
}

function ackProblem(a: { id: number; ok: boolean; error?: VoiceControlErrorCode | undefined }): string | null {
  if (!validId(a.id)) return 'ack id out of range';
  if (a.ok && a.error !== undefined) return 'ack with ok has an error';
  if (!a.ok && (a.error === undefined || !ACK_REFUSALS.includes(a.error))) return 'ack error is not a refusal';
  return null;
}

function blockedProblem(blocked: readonly string[]): string | null {
  let last = -1;
  for (const b of blocked) {
    const at = BLOCK_ORDER.indexOf(b as VoiceControlBlock);
    if (at < 0) return 'state block reason not recognized';
    if (at <= last) return 'state block reasons out of order';
    last = at;
  }
  return null;
}

function reasonProblem(phase: VoiceControlPhase, reason: VoiceControlReason | undefined, retry: number | undefined): string | null {
  if (reason !== undefined && !ALL_REASONS.includes(reason)) return 'state reason not recognized';
  switch (phase) {
    case 'starting':
    case 'connecting':
    case 'connected':
      if (reason !== undefined) return 'state phase takes no reason';
      break;
    case 'reconnecting':
      if (reason !== undefined && !RETRY_REASONS.includes(reason)) return 'state reason is not a reason to reconnect';
      break;
    case 'stopping':
      if (reason === undefined || !(STOP_REASONS.includes(reason) || FAIL_REASONS.includes(reason))) return 'state reason is not a reason to stop';
      break;
    case 'stopped':
      if (reason === undefined || !STOP_REASONS.includes(reason)) return 'state stopped needs a stop reason';
      break;
    case 'failed':
      if (reason === undefined || !FAIL_REASONS.includes(reason)) return 'state reason is not a reason to fail';
      break;
  }
  if (retry !== undefined) {
    if (phase !== 'reconnecting' || reason === undefined) return 'state retry_in_ms only while reconnecting for a reason';
    if (retry < 0 || retry > VOICE_CONTROL_MAX_RETRY_IN_MS) return 'state retry_in_ms out of range';
  }
  return null;
}

interface StateCore {
  seq: number;
  phase: VoiceControlPhase;
  reason?: VoiceControlReason | undefined;
  retry_in_ms?: number | undefined;
  capture: VoiceControlCapture;
  transmitting: boolean;
  muted: boolean;
  deafened: boolean;
  blocked: VoiceControlBlock[];
  ptt: VoiceControlPTT;
  ptt_problem?: VoiceControlPTTProblem | undefined;
}

// stateProblem is VoiceControlStateV1.Validate, and with v2 the
// VoiceControlStateV2.Validate changes (playback and talk_mode).
function stateProblem(s: StateCore, v2?: { playback: VoiceControlPlayback; talk_mode: VoiceControlTalkMode }): string | null {
  if (!validId(s.seq)) return 'seq out of range';
  if (!PHASES.includes(s.phase)) return 'state phase not recognized';
  const live = s.phase === 'connecting' || s.phase === 'connected' || s.phase === 'reconnecting';
  if (live && s.ptt === 'unknown') return 'state must say whether the key device is read';
  if (v2 !== undefined) {
    if (live && v2.playback === 'off') return 'state must have playback on or unavailable';
    if (s.phase === 'starting' && v2.talk_mode !== 'push_to_talk') return 'state starting must be push_to_talk';
    if (v2.playback !== 'off' && (s.phase === 'starting' || finalPhase(s.phase))) return 'state must have playback off';
  }
  const blocked = blockedProblem(s.blocked);
  if (blocked !== null) return blocked;
  const reason = reasonProblem(s.phase, s.reason, s.retry_in_ms);
  if (reason !== null) return reason;
  const problem = s.ptt === 'unavailable' || s.ptt === 'lost';
  if (problem && s.ptt_problem === undefined) return 'state ptt needs a ptt_problem';
  if (!problem && s.ptt_problem !== undefined) return 'state ptt takes no ptt_problem';
  const blocks = (b: VoiceControlBlock): boolean => s.blocked.includes(b);
  if (s.muted !== blocks('muted')) return 'state muted disagrees with its block';
  if (s.deafened !== blocks('deafened')) return 'state deafened disagrees with its block';
  const open = v2?.talk_mode === 'open';
  if (open) {
    if (blocks('key_device_lost')) return 'state in open mode cannot block with key_device_lost';
  } else if ((s.ptt === 'lost') !== blocks('key_device_lost')) {
    return 'state ptt lost disagrees with its block';
  }
  if (s.phase !== 'connected' && !blocks('not_connected')) return 'state must block with not_connected';
  if (s.capture === 'on' && blocks('no_microphone')) return 'state capture on and no_microphone';
  if (s.capture === 'unavailable' && !blocks('no_microphone')) return 'state capture unavailable without no_microphone';
  if (s.capture !== 'off' && (s.phase === 'starting' || finalPhase(s.phase))) return 'state must have capture off';
  if (s.transmitting) {
    if (s.blocked.length !== 0) return 'state transmitting while blocked';
    if (s.capture !== 'on') return 'state transmitting without capture';
    if (!open && s.ptt !== 'ready') return 'state transmitting without ptt ready';
  }
  return null;
}

// ------------------------------------------------------------------ events

export type EventV1 =
  | { readonly kind: 'hello'; readonly seq: number; readonly hello: VoiceControlHelloV1 }
  | { readonly kind: 'ack'; readonly seq: number; readonly ack: VoiceControlAckV1 }
  | { readonly kind: 'state'; readonly seq: number; readonly state: VoiceControlStateV1 }
  | { readonly kind: 'protocol_error'; readonly seq: number; readonly error: VoiceControlProtocolErrorV1 }
  | { readonly kind: 'unknown'; readonly seq: number };

export type EventV2 =
  | { readonly kind: 'ack'; readonly seq: number; readonly ack: VoiceControlAckV2 }
  | { readonly kind: 'state'; readonly seq: number; readonly state: VoiceControlStateV2 }
  | { readonly kind: 'protocol_error'; readonly seq: number; readonly error: VoiceControlProtocolErrorV2 }
  | { readonly kind: 'unknown'; readonly seq: number };

export type Parsed<T> = ({ readonly ok: true } & T) | Refusal;

// checked runs the generated validator over a value this module built: the
// shape and every vocabulary come from pkg/schema, not from here.
function checked<T>(validator: Validator<T>, value: T): string | null {
  return validator(value).length === 0 ? null : 'not of the generated type';
}

const KNOWN_EVENTS: readonly string[] = ['hello', 'ack', 'state', 'protocol_error'];

interface Head {
  readonly fields: Fields;
  readonly seq: number;
  readonly kind: VoiceControlEventKind | null;
}

function eventHead(line: Uint8Array, version: Version): { readonly ok: true; readonly head: Head } | Refusal {
  const decoded = decodeObject(line);
  if (!decoded.ok) return decoded;
  const { fields } = decoded;
  if (fieldString(fields, 'schema') !== version) return refuse('unknown_schema', 'schema is not the version in use');
  const seq = asInt(fields.get('seq'));
  if (seq === undefined || !validId(seq)) return refuse('invalid', 'seq out of range');
  const kind = fieldString(fields, 'kind');
  if (kind === undefined || kind === '') return refuse('invalid', 'kind must be a string');
  return { ok: true, head: { fields, seq, kind: KNOWN_EVENTS.includes(kind) ? (kind as VoiceControlEventKind) : null } };
}

function done<T>(r: FieldReader, problem: string | null, value: T): Parsed<T> {
  if (r.missing !== '') return refuse('invalid', 'a field is missing or of the wrong type');
  if (problem !== null) return refuse('invalid', problem);
  return { ok: true, ...value };
}

// parseEventV1 is ParseVoiceControlEvent: the hello always, and every event
// of a v1 session.
export function parseEventV1(line: Uint8Array): Parsed<{ event: EventV1 }> {
  const h = eventHead(line, SCHEMA_V1);
  if (!h.ok) return h;
  const { fields, seq, kind } = h.head;
  if (kind === null) return { ok: true, event: { kind: 'unknown', seq } };
  const r = new FieldReader(fields);
  switch (kind) {
    case 'hello': {
      const hello: VoiceControlHelloV1 = { schema: SCHEMA_V1, kind, seq, versions: r.strs('versions'), helper_version: r.str('helper_version') };
      return done(r, r.missing === '' ? (helloProblem(hello) ?? checked(voiceControlHelloV1, hello)) : null, { event: { kind, seq, hello } });
    }
    case 'ack': {
      const ack: VoiceControlAckV1 = { schema: SCHEMA_V1, kind, seq, id: r.int('id'), ok: r.bool('ok') };
      const error = r.optWord('error');
      if (error !== undefined) ack.error = error as VoiceControlErrorCode;
      return done(r, r.missing === '' ? (ackProblem(ack) ?? checked(voiceControlAckV1, ack)) : null, { event: { kind, seq, ack } });
    }
    case 'state': {
      const state = readStateV1(r, seq);
      return done(r, r.missing === '' ? (stateProblem(state) ?? checked(voiceControlStateV1, state)) : null, { event: { kind, seq, state } });
    }
    case 'protocol_error': {
      const error: VoiceControlProtocolErrorV1 = { schema: SCHEMA_V1, kind, seq, code: r.str('code') as VoiceControlErrorCode };
      const problem = FATAL.includes(error.code) ? checked(voiceControlProtocolErrorV1, error) : 'protocol_error code does not end the session';
      return done(r, r.missing === '' ? problem : null, { event: { kind, seq, error } });
    }
  }
}

function readStateV1(r: FieldReader, seq: number): VoiceControlStateV1 {
  const state: VoiceControlStateV1 = {
    schema: SCHEMA_V1,
    kind: 'state',
    seq,
    phase: r.str('phase') as VoiceControlPhase,
    capture: 'off',
    transmitting: false,
    muted: false,
    deafened: false,
    blocked: [],
    ptt: 'unknown',
  };
  const reason = r.optWord('reason');
  const retry = r.optInt('retry_in_ms');
  state.capture = r.str('capture') as VoiceControlCapture;
  state.transmitting = r.bool('transmitting');
  state.muted = r.bool('muted');
  state.deafened = r.bool('deafened');
  state.blocked = r.strs('blocked') as VoiceControlBlock[];
  state.ptt = r.str('ptt') as VoiceControlPTT;
  const problem = r.optWord('ptt_problem');
  if (reason !== undefined) state.reason = reason as VoiceControlReason;
  if (retry !== undefined) state.retry_in_ms = retry;
  if (problem !== undefined) state.ptt_problem = problem as VoiceControlPTTProblem;
  return state;
}

// parseEventV2 is ParseVoiceControlEventV2: every event of a v2 session after
// the hello. A hello in v2 is invalid.
export function parseEventV2(line: Uint8Array): Parsed<{ event: EventV2 }> {
  const h = eventHead(line, SCHEMA_V2);
  if (!h.ok) return h;
  const { fields, seq, kind } = h.head;
  if (kind === null) return { ok: true, event: { kind: 'unknown', seq } };
  const r = new FieldReader(fields);
  switch (kind) {
    case 'hello':
      return refuse('invalid', 'a hello is written in v1 only');
    case 'ack': {
      const ack: VoiceControlAckV2 = { schema: SCHEMA_V2, kind, seq, id: r.int('id'), ok: r.bool('ok') };
      const error = r.optWord('error');
      if (error !== undefined) ack.error = error as VoiceControlErrorCode;
      return done(r, r.missing === '' ? (ackProblem(ack) ?? checked(voiceControlAckV2, ack)) : null, { event: { kind, seq, ack } });
    }
    case 'state': {
      const v1 = readStateV1(r, seq);
      // Field order as Go reads them does not matter: every field is read.
      const state: VoiceControlStateV2 = {
        ...v1,
        schema: SCHEMA_V2,
        playback: r.str('playback') as VoiceControlPlayback,
        talk_mode: r.str('talk_mode') as VoiceControlTalkMode,
      };
      const problem = r.missing === '' ? (stateProblem(state, state) ?? checked(voiceControlStateV2, state)) : null;
      return done(r, problem, { event: { kind, seq, state } });
    }
    case 'protocol_error': {
      const error: VoiceControlProtocolErrorV2 = { schema: SCHEMA_V2, kind, seq, code: r.str('code') as VoiceControlErrorCode };
      const problem = FATAL.includes(error.code) ? checked(voiceControlProtocolErrorV2, error) : 'protocol_error code does not end the session';
      return done(r, r.missing === '' ? problem : null, { event: { kind, seq, error } });
    }
  }
}

// ------------------------------------------------------------ attach line

const BASE64URL = /^[A-Za-z0-9_-]*$/;
const NONCE_LAST = 'AEIMQUYcgkosw048';

// validNonce is ValidateVoiceControlPipeNonce: 43 characters of canonical
// unpadded base64url, the encoding of 32 bytes.
export function validNonce(nonce: string): boolean {
  return nonce.length === VOICE_CONTROL_PIPE_NONCE_CHARS && BASE64URL.test(nonce) && NONCE_LAST.includes(nonce.slice(-1));
}

// parseAttached is ParseVoiceControlPipeAttached: the first line on the
// events endpoint. Whether the nonce is the owner's is the caller's to decide,
// in constant time. Nothing it returns holds the nonce but the value itself.
export function parseAttached(line: Uint8Array): Parsed<{ attached: VoiceControlPipeAttachedV1 }> {
  const decoded = decodeObject(line);
  if (!decoded.ok) return decoded;
  const { fields } = decoded;
  if (fieldString(fields, 'schema') !== PIPE_SCHEMA_V1) return refuse('unknown_schema', 'not an attach line');
  const kind = fieldString(fields, 'kind');
  if (kind === undefined || kind === '') return refuse('invalid', 'kind must be a string');
  const nonce = fieldString(fields, 'nonce');
  if (nonce === undefined || !validNonce(nonce)) return refuse('invalid', 'the nonce is not well-formed');
  if (kind !== 'attached') return refuse('unknown_kind', 'not a kind of the attach schema');
  const attached: VoiceControlPipeAttachedV1 = { schema: PIPE_SCHEMA_V1, kind, nonce };
  if (voiceControlPipeAttachedV1(attached).length !== 0) return refuse('invalid', 'not of the generated type');
  return { ok: true, attached };
}

// ---------------------------------------------------------------- commands

export interface StartParams {
  readonly server: string;
  readonly channel: string;
  readonly principalId: number;
  readonly muted: boolean;
  readonly deafened: boolean;
}

export type Command =
  | VoiceControlStartV1
  | VoiceControlStartV2
  | VoiceControlSetMuteV1
  | VoiceControlSetMuteV2
  | VoiceControlSetDeafenV1
  | VoiceControlSetDeafenV2
  | VoiceControlGetStateV1
  | VoiceControlGetStateV2
  | VoiceControlLeaveV1
  | VoiceControlLeaveV2
  | VoiceControlSetDevicesV2
  | VoiceControlSetTalkModeV2;

// The owner's wishes, before the version and the id are known.
export type Want =
  | { readonly kind: 'set_mute'; readonly muted: boolean }
  | { readonly kind: 'set_deafen'; readonly deafened: boolean }
  | { readonly kind: 'get_state' }
  | { readonly kind: 'leave' }
  | { readonly kind: 'set_devices'; readonly input?: string; readonly output?: string }
  | { readonly kind: 'set_talk_mode'; readonly talkMode: VoiceControlTalkMode };

const CONTROL = /\p{Cc}/u;
const LONE_SURROGATE = /\p{Cs}/u;

// validServer is validateVoiceControlServer for the addresses main uses: an
// http(s) URL with a lower-case host, no user, query, fragment or trailing
// slash.
export function validServer(server: string): boolean {
  if (server === '' || Buffer.byteLength(server, 'utf8') > VOICE_CONTROL_MAX_SERVER_BYTES) return false;
  let u: URL;
  try {
    u = new URL(server);
  } catch {
    return false;
  }
  if (!/^https?:\/\//.test(server)) return false;
  const rawHost = server.slice(server.indexOf('//') + 2).split(/[/?#]/, 1)[0] ?? '';
  if (rawHost === '' || rawHost !== rawHost.toLowerCase() || rawHost.includes('@')) return false;
  if (server.includes('?') || server.includes('#')) return false;
  return !u.pathname.endsWith('/') || (u.pathname === '/' && !server.endsWith('/'));
}

// validChannel is validateVoiceControlChannel.
export function validChannel(channel: string): boolean {
  return (
    channel.trim() !== '' &&
    Buffer.byteLength(channel, 'utf8') <= VOICE_CONTROL_MAX_CHANNEL_BYTES &&
    !LONE_SURROGATE.test(channel) &&
    !CONTROL.test(channel)
  );
}

function validDeviceName(name: string): boolean {
  return (
    name !== '' &&
    Buffer.byteLength(name, 'utf8') <= VOICE_DEVICES_MAX_NAME_BYTES &&
    !LONE_SURROGATE.test(name) &&
    !CONTROL.test(name) &&
    !name.includes('"') &&
    !name.includes('\\')
  );
}

// startCommand is the start, always with credential env: the desktop owner
// hands the token over in CONCH_TOKEN (linux-desktop.md §4.1).
export function startCommand(version: Version, id: number, p: StartParams): VoiceControlStartV1 | VoiceControlStartV2 {
  const body = { kind: 'start' as const, id, server: p.server, channel: p.channel, principal_id: p.principalId, credential: 'env' as const, muted: p.muted, deafened: p.deafened };
  return version === SCHEMA_V2 ? { schema: SCHEMA_V2, ...body } : { schema: SCHEMA_V1, ...body };
}

// command builds a later command in the session's version, or null when
// that version has no such command (set_devices and set_talk_mode are v2's).
export function command(version: Version, id: number, want: Want): Command | null {
  const v2 = version === SCHEMA_V2;
  switch (want.kind) {
    case 'set_mute':
      return v2 ? { schema: SCHEMA_V2, kind: 'set_mute', id, muted: want.muted } : { schema: SCHEMA_V1, kind: 'set_mute', id, muted: want.muted };
    case 'set_deafen':
      return v2
        ? { schema: SCHEMA_V2, kind: 'set_deafen', id, deafened: want.deafened }
        : { schema: SCHEMA_V1, kind: 'set_deafen', id, deafened: want.deafened };
    case 'get_state':
      return v2 ? { schema: voiceControlGetStateV2Schema, kind: 'get_state', id } : { schema: voiceControlGetStateV1Schema, kind: 'get_state', id };
    case 'leave':
      return v2 ? { schema: SCHEMA_V2, kind: 'leave', id } : { schema: SCHEMA_V1, kind: 'leave', id };
    case 'set_devices': {
      if (!v2) return null;
      const c: VoiceControlSetDevicesV2 = { schema: SCHEMA_V2, kind: 'set_devices', id };
      if (want.input !== undefined) c.input = want.input;
      if (want.output !== undefined) c.output = want.output;
      return c;
    }
    case 'set_talk_mode':
      return v2 ? { schema: SCHEMA_V2, kind: 'set_talk_mode', id, talk_mode: want.talkMode } : null;
  }
}

function commandProblem(c: Command): string | null {
  if (!validId(c.id)) return 'id out of range';
  const v2 = c.schema === SCHEMA_V2;
  // The generated kind field is the whole vocabulary, so it does not narrow
  // the union; each command was built by this module for its kind.
  switch (c.kind) {
    case 'start': {
      const s = c as VoiceControlStartV1 | VoiceControlStartV2;
      if (!validServer(s.server)) return 'the server address is not one a start may carry';
      if (!validChannel(s.channel)) return 'the channel is not one a start may carry';
      if (!Number.isSafeInteger(s.principal_id) || s.principal_id <= 0) return 'principal_id must be positive';
      return checked<VoiceControlStartV1 | VoiceControlStartV2>(v2 ? voiceControlStartV2 : voiceControlStartV1, s);
    }
    case 'set_mute':
      return checked<VoiceControlSetMuteV1 | VoiceControlSetMuteV2>(v2 ? voiceControlSetMuteV2 : voiceControlSetMuteV1, c as VoiceControlSetMuteV1 | VoiceControlSetMuteV2);
    case 'set_deafen':
      return checked<VoiceControlSetDeafenV1 | VoiceControlSetDeafenV2>(
        v2 ? voiceControlSetDeafenV2 : voiceControlSetDeafenV1,
        c as VoiceControlSetDeafenV1 | VoiceControlSetDeafenV2,
      );
    case 'get_state':
      return checked<VoiceControlGetStateV1 | VoiceControlGetStateV2>(v2 ? voiceControlGetStateV2 : voiceControlGetStateV1, c);
    case 'leave':
      return checked<VoiceControlLeaveV1 | VoiceControlLeaveV2>(v2 ? voiceControlLeaveV2 : voiceControlLeaveV1, c);
    case 'set_devices': {
      const d = c as VoiceControlSetDevicesV2;
      if (d.input === undefined && d.output === undefined) return 'set_devices must name a side';
      if (d.input !== undefined && !validDeviceName(d.input)) return 'set_devices input is not a device name';
      if (d.output !== undefined && !validDeviceName(d.output)) return 'set_devices output is not a device name';
      return checked(voiceControlSetDevicesV2, d);
    }
    case 'set_talk_mode':
      return checked(voiceControlSetTalkModeV2, c as VoiceControlSetTalkModeV2);
    default:
      return 'not a command';
  }
}

// encodeCommand writes a command as one line with its line feed, refusing one
// a helper must refuse (MarshalVoiceControlLine). The error never holds a
// value from the command.
export function encodeCommand(c: Command): Buffer {
  const problem = commandProblem(c);
  if (problem !== null) throw new Error(`voice: refusing to write ${c.kind}: ${problem}`);
  const line = Buffer.from(JSON.stringify(c), 'utf8');
  if (line.length > VOICE_CONTROL_MAX_LINE_BYTES) throw new Error(`voice: refusing to write ${c.kind}: the line is too long`);
  return Buffer.concat([line, Buffer.from('\n')]);
}

// ------------------------------------------------------------- negotiation

// negotiate picks the highest version both sides implement, or null: then the
// owner writes nothing at all and closes standard input.
export function negotiate(hello: VoiceControlHelloV1): Version | null {
  return OWNER_VERSIONS.find((v) => hello.versions.includes(v)) ?? null;
}

// ------------------------------------------------------------------ lines

// LineSplitter cuts a byte stream into lines without their line feed and
// never holds more than one line's bound: past it, the line is reported as
// too long at once and the rest of it, up to its line feed, is discarded
// (Go's lineReader).
export class LineSplitter {
  private buffered: Buffer[] = [];
  private size = 0;
  private skipping = false;

  push(chunk: Buffer, out: (line: Buffer | 'too_long') => void): void {
    let start = 0;
    while (start < chunk.length) {
      const lf = chunk.indexOf(0x0a, start);
      const end = lf < 0 ? chunk.length : lf;
      if (this.skipping) {
        if (lf >= 0) this.skipping = false;
      } else {
        const piece = chunk.subarray(start, end);
        if (this.size + piece.length > VOICE_CONTROL_MAX_LINE_BYTES) {
          this.buffered = [];
          this.size = 0;
          this.skipping = lf < 0;
          out('too_long');
        } else if (lf >= 0) {
          const line = Buffer.concat([...this.buffered, piece]);
          this.buffered = [];
          this.size = 0;
          out(line);
        } else {
          this.buffered.push(Buffer.from(piece));
          this.size += piece.length;
        }
      }
      start = lf < 0 ? chunk.length : lf + 1;
    }
  }
}

// ---------------------------------------------------------------- tracker

export type Event = EventV1 | EventV2;

export interface Reading {
  readonly event: Event;
  // gap: events were dropped before this one; the owner asks for a snapshot.
  readonly gap: boolean;
  // acked: the kind of the command an ack answers.
  readonly acked?: Command['kind'];
}

export type TrackResult = ({ readonly ok: true } & Reading) | { readonly ok: false; readonly detail: string };

// Tracker holds one helper's event stream to the owner's reading rules: the
// hello first and once, seq always increasing, nothing but the hello before
// the owner has written, acks in the order the commands were written,
// nothing after the final state (internal/cli/voicehelper's tracker). Before
// the version is chosen it reads v1, the hello's version.
export class Tracker {
  private lastSeq = 0;
  private hello: VoiceControlHelloV1 | null = null;
  private final = false;
  private wrote = false;
  private readonly pending: { readonly id: number; readonly kind: Command['kind'] }[] = [];
  version: Version = SCHEMA_V1;

  written(c: Command): void {
    this.wrote = true;
    this.pending.push({ id: c.id, kind: c.kind });
  }

  get unanswered(): number {
    return this.pending.length;
  }

  read(line: Buffer | 'too_long'): TrackResult {
    if (line === 'too_long') return { ok: false, detail: 'too_long' };
    const parsed = this.version === SCHEMA_V2 && this.hello !== null ? parseEventV2(line) : parseEventV1(line);
    if (!parsed.ok) return { ok: false, detail: parsed.code };
    const ev = parsed.event;
    if (this.final) return { ok: false, detail: 'an event after the final state' };
    if (this.hello === null && ev.kind !== 'hello') return { ok: false, detail: 'the first event is not a hello' };
    if (ev.seq <= this.lastSeq) return { ok: false, detail: 'seq did not increase' };
    if (ev.kind === 'hello' && this.hello !== null) return { ok: false, detail: 'a second hello' };
    if ((ev.kind === 'ack' || ev.kind === 'state' || ev.kind === 'protocol_error') && !this.wrote) {
      return { ok: false, detail: 'an event other than hello before any command' };
    }
    const gap = this.lastSeq !== 0 && ev.seq > this.lastSeq + 1;
    let acked: Command['kind'] | undefined;
    if (ev.kind === 'hello') this.hello = ev.hello;
    if (ev.kind === 'ack') {
      const next = this.pending[0];
      if (next?.id !== ev.ack.id) return { ok: false, detail: 'an ack for a command that is not the next one' };
      this.pending.shift();
      acked = next.kind;
    }
    if (ev.kind === 'state') this.final = finalPhase(ev.state.phase);
    this.lastSeq = ev.seq;
    return acked === undefined ? { ok: true, event: ev, gap } : { ok: true, event: ev, gap, acked };
  }
}
