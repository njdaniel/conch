// The device list `conch-voice devices --json` writes (issue #1378, D3-7 of
// #771; pkg/schema/voice_devices.go), read as ParseVoiceDevices reads it.
//
// Input: the whole of the helper's standard output. Output: a VoiceDevicesV1
// that passes the generated validator and the schema's consistency rules, or
// a refusal phrase. Owns: the framing (size, UTF-8, exactly one object, no key
// twice at the top level or in a device, exact case-sensitive names, unknown
// fields ignored) and the rules Validate adds: names fit for voice.toml, a
// bounded one-line description, counts not negative, nothing listed while
// PipeWire is unavailable, configured_* only while config is ok.
// It does no I/O. pkg/schema/testdata/voice-devices/ (every golden fixture and
// every case of cases.json) drives it through tests/unit/voice-oneshot.test.ts.
//
// It never repeats the input in what it returns. One deliberate difference
// from Go: a \u escape of a lone surrogate is refused (Go's decoder would
// replace it), as the control protocol's reader refuses it.
import { VOICE_DEVICES_MAX_BYTES, VOICE_DEVICES_MAX_NAME_BYTES, voiceDevicesV1, voiceDevicesV1Schema, type VoiceDevice, type VoiceDevicesV1 } from '../../../web/src/schema/generated.ts';
import { Duplicate, LoneSurrogate, Reader, Trailing, type Fields, type Json } from './protocol.ts';

// The numbers pkg/schema holds both sides to.
export const MAX_LISTED = 64;
export const MAX_DESCRIPTION_CHARS = 80;

const utf8 = new TextDecoder('utf-8', { fatal: true, ignoreBOM: true });
const CONTROL = /\p{Cc}/u;
const INTEGER = /^-?(?:0|[1-9][0-9]*)$/;
const INT64_MIN = -(2n ** 63n);
const INT64_MAX = 2n ** 63n - 1n;
const DEFAULT_NAME = 'default';

export type DevicesParse = { readonly ok: true; readonly devices: VoiceDevicesV1 } | { readonly ok: false; readonly detail: string };

const refuse = (detail: string): DevicesParse => ({ ok: false, detail });

// decode reads one object and its fields; a key twice is Duplicate, anything
// after the object is Trailing.
function decode(text: string): Fields {
  return new Reader(text).object();
}

// Go's fieldReader: the first required field that was missing, null or of the
// wrong type.
class Fieldwise {
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

  optStr(key: string): string {
    const v = this.fields.get(key);
    if (v === undefined) return '';
    if (v.t === 'str') return v.v;
    this.miss(key);
    return '';
  }

  bool(key: string): boolean {
    const v = this.fields.get(key);
    if (v?.t === 'bool') return v.v;
    this.miss(key);
    return false;
  }

  // count reads an int as encoding/json reads it into Go's int: integer text
  // in int64 range, or a miss. A value past the safe integers is kept as the
  // nearest one; a count that large is no valid document in practice, and
  // the checks below only care for its sign.
  count(key: string): number {
    const v = this.fields.get(key);
    if (v?.t === 'num' && INTEGER.test(v.raw)) {
      const n = BigInt(v.raw);
      if (n >= INT64_MIN && n <= INT64_MAX) return Number(n);
    }
    this.miss(key);
    return 0;
  }

  // devices reads a list of devices, each an object read again under the
  // document's own rules (a key twice in one is a framing error).
  devices(text: string, key: string): VoiceDevice[] {
    const v = this.fields.get(key);
    if (v?.t !== 'arr') {
      this.miss(key);
      return [];
    }
    const list: VoiceDevice[] = [];
    for (const item of v.items) {
      if (item.t !== 'obj') {
        this.miss(key);
        return [];
      }
      const entry = new Fieldwise(decode(text.slice(item.start, item.end)));
      const dev: VoiceDevice = { name: entry.str('name'), description: entry.str('description'), default: entry.bool('default') };
      if (entry.missing !== '') {
        this.miss(`${key}.${entry.missing}`);
        return [];
      }
      list.push(dev);
    }
    return list;
  }
}

function nameProblem(field: string, name: string): string {
  if (name === '' || Buffer.byteLength(name, 'utf8') > VOICE_DEVICES_MAX_NAME_BYTES) return `${field} must be 1 to ${String(VOICE_DEVICES_MAX_NAME_BYTES)} bytes`;
  if (CONTROL.test(name)) return `${field} must not contain control characters`;
  if (name.includes('"') || name.includes('\\')) return `${field} must not contain a quotation mark or a backslash`;
  return '';
}

function listProblem(field: string, list: readonly VoiceDevice[]): string {
  if (list.length > MAX_LISTED) return `${field} lists ${String(list.length)} devices, more than ${String(MAX_LISTED)}`;
  for (const [i, dev] of list.entries()) {
    const bad = nameProblem(`${field}[${String(i)}].name`, dev.name);
    if (bad !== '') return bad;
    if (Array.from(dev.description).length > MAX_DESCRIPTION_CHARS) return `${field}[${String(i)}].description must be at most ${String(MAX_DESCRIPTION_CHARS)} characters`;
    if (CONTROL.test(dev.description)) return `${field}[${String(i)}].description must not contain control characters`;
  }
  return '';
}

// validate is VoiceDevicesV1.Validate, after the generated validator has
// checked the shape; '' when the document is consistent.
export function consistencyProblem(d: VoiceDevicesV1): string {
  const lists = listProblem('microphones', d.microphones) || listProblem('speakers', d.speakers);
  if (lists !== '') return lists;
  if (d.unshown_microphones < 0) return `unshown_microphones must not be negative, got ${String(d.unshown_microphones)}`;
  if (d.unshown_speakers < 0) return `unshown_speakers must not be negative, got ${String(d.unshown_speakers)}`;
  if (d.pipewire === 'unavailable' && (d.microphones.length !== 0 || d.speakers.length !== 0 || d.unshown_microphones !== 0 || d.unshown_speakers !== 0)) {
    return 'must list and count nothing while pipewire is unavailable';
  }
  for (const [field, value] of [
    ['configured_input', d.configured_input],
    ['configured_output', d.configured_output],
  ] as const) {
    if (value === undefined || value === '') continue;
    if (d.config !== 'ok') return `${field} is only for config ok, got config "${d.config}"`;
    const bad = nameProblem(field, value);
    if (bad !== '') return bad;
    if (value === DEFAULT_NAME) return `${field} says the default by absence, not by the value "${DEFAULT_NAME}"`;
  }
  return '';
}

function schemaOf(fields: Fields): string | undefined {
  const v: Json | undefined = fields.get('schema');
  return v?.t === 'str' ? v.v : undefined;
}

// parseVoiceDevices is ParseVoiceDevices. Its refusals carry Go's phrases.
export function parseVoiceDevices(data: Uint8Array): DevicesParse {
  if (data.length > VOICE_DEVICES_MAX_BYTES) return refuse(`document is ${String(data.length)} bytes, more than ${String(VOICE_DEVICES_MAX_BYTES)}`);
  let text: string;
  try {
    text = utf8.decode(data);
  } catch {
    return refuse('document is not valid UTF-8');
  }
  let fields: Fields;
  try {
    fields = decode(text);
  } catch (err) {
    if (err instanceof Duplicate) return refuse('document gives a key twice');
    if (err instanceof Trailing) return refuse('document holds more than one JSON value');
    if (err instanceof LoneSurrogate) return refuse('document holds a lone surrogate escape');
    return refuse('document is not one JSON object');
  }
  if (schemaOf(fields) !== voiceDevicesV1Schema) return refuse(`schema is not ${voiceDevicesV1Schema}`);
  const r = new Fieldwise(fields);
  let candidate: Record<string, unknown>;
  try {
    const pipewire = r.str('pipewire');
    const synced = r.bool('synced');
    const microphones = r.devices(text, 'microphones');
    const speakers = r.devices(text, 'speakers');
    const unshownMicrophones = r.count('unshown_microphones');
    const unshownSpeakers = r.count('unshown_speakers');
    const config = r.str('config');
    const configuredInput = r.optStr('configured_input');
    const configuredOutput = r.optStr('configured_output');
    if (r.missing !== '') return refuse(`document needs ${r.missing} of the right type`);
    candidate = {
      schema: voiceDevicesV1Schema,
      pipewire,
      synced,
      microphones,
      speakers,
      unshown_microphones: unshownMicrophones,
      unshown_speakers: unshownSpeakers,
      config,
      ...(configuredInput === '' ? {} : { configured_input: configuredInput }),
      ...(configuredOutput === '' ? {} : { configured_output: configuredOutput }),
    };
  } catch (err) {
    if (err instanceof Duplicate) return refuse('document gives a key twice');
    return refuse('document is not one JSON object');
  }
  // The generated validator: the vocabularies and the shape.
  const problems = voiceDevicesV1(candidate);
  if (problems.length > 0) return refuse(vocabularyPhrase(candidate));
  const doc = candidate as unknown as VoiceDevicesV1;
  const bad = consistencyProblem(doc);
  return bad === '' ? { ok: true, devices: doc } : refuse(bad);
}

// The phrase for a pipewire or config value outside its vocabulary, as Go
// words it (the value is a short word of the vocabulary's own length, but it
// is the helper's, so it is not repeated).
function vocabularyPhrase(candidate: Record<string, unknown>): string {
  if (candidate['pipewire'] !== 'ok' && candidate['pipewire'] !== 'unavailable') return 'pipewire is not one of ok, unavailable';
  return 'config is not one of ok, missing, refused, none';
}
