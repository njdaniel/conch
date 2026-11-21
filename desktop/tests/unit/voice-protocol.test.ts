// The owner's protocol reader against pkg/schema's golden and conformance
// fixtures (issue #948): every document fixture of voice-control/,
// voice-control-v2/ and voice-control-pipe/ matches its generated validator,
// every event and attach case of cases*.json is read as Go reads it, the
// command builders write the golden commands, and the tracker accepts every
// transcript and refuses the stream rules' breaches.
//
// voice-control-v3/ (issue #1851) is held to its generated validators in the
// first block, and from issue #1854 the owner's v3 reader, builders and
// tracker are held to its cases, golden commands and transcripts below
// ("the owner's v3 reader", "v3 commands", "v3 negotiation", "the Tracker in
// v3"). The owner offers v3 only with the Linux share-sound switch on.
// The v4 golden fixtures exercise generated validators for the private-call
// start, microphone diagnostics and call quality.
import { readdirSync, readFileSync } from 'node:fs';
import { join } from 'node:path';

import { describe, expect, it } from 'vitest';

import {
  voiceControlAckV1,
  voiceControlAckV2,
  voiceControlAckV3,
  voiceControlAckV4,
  voiceControlCallQualityV4,
  voiceControlGateEventV4,
  voiceControlGetCallQualityV4,
  voiceControlGetStateV1,
  voiceControlGetStateV2,
  voiceControlGetStateV3,
  voiceControlGetStateV4,
  voiceControlHelloV1,
  voiceControlLeaveV1,
  voiceControlLeaveV2,
  voiceControlLeaveV3,
  voiceControlLeaveV4,
  voiceControlLevelsV4,
  voiceControlPipeAttachedV1,
  voiceControlProtocolErrorV1,
  voiceControlProtocolErrorV2,
  voiceControlProtocolErrorV3,
  voiceControlProtocolErrorV4,
  voiceControlSetDeafenV1,
  voiceControlSetDeafenV2,
  voiceControlSetDeafenV3,
  voiceControlSetDeafenV4,
  voiceControlSetDevicesV2,
  voiceControlSetDevicesV3,
  voiceControlSetDevicesV4,
  voiceControlSetDiagnosticsV4,
  voiceControlSetMuteV1,
  voiceControlSetMuteV2,
  voiceControlSetMuteV3,
  voiceControlSetMuteV4,
  voiceControlSetShareAudioMuteV3,
  voiceControlSetShareAudioMuteV4,
  voiceControlSetShareAudioV3,
  voiceControlSetShareAudioV4,
  voiceControlSetTalkModeV2,
  voiceControlSetTalkModeV3,
  voiceControlSetTalkModeV4,
  voiceControlStartV1,
  voiceControlStartV2,
  voiceControlStartV3,
  voiceControlStartV4,
  voiceControlStateV1,
  voiceControlStateV2,
  voiceControlStateV3,
  voiceControlStateV4,
  type VoiceControlHelloV1,
} from '../../../web/src/schema/generated.ts';
import type { Validator } from '../../../web/src/schema/validate.ts';
import {
  LineSplitter,
  OWNER_VERSIONS,
  OWNER_VERSIONS_SHARE_AUDIO,
  SCHEMA_V1,
  SCHEMA_V2,
  SCHEMA_V3,
  Tracker,
  command,
  encodeCommand,
  negotiate,
  parseAttached,
  parseEventV1,
  parseEventV2,
  parseEventV3,
  startCommand,
  validApplicationName,
  validChannel,
  validNonce,
  validServer,
  type Command,
} from '../../main/voice/protocol.ts';
import { TESTDATA, readFixture, transcript } from './voice-support.ts';

const validators: Record<string, Record<string, Validator<unknown>>> = {
  'voice-control': {
    'command-start': voiceControlStartV1,
    'command-set-mute': voiceControlSetMuteV1,
    'command-set-deafen': voiceControlSetDeafenV1,
    'command-get-state': voiceControlGetStateV1,
    'command-leave': voiceControlLeaveV1,
    'event-hello': voiceControlHelloV1,
    'event-ack': voiceControlAckV1,
    'event-state': voiceControlStateV1,
    'event-protocol-error': voiceControlProtocolErrorV1,
  },
  'voice-control-v2': {
    'command-start': voiceControlStartV2,
    'command-set-mute': voiceControlSetMuteV2,
    'command-set-deafen': voiceControlSetDeafenV2,
    'command-get-state': voiceControlGetStateV2,
    'command-leave': voiceControlLeaveV2,
    'command-set-devices': voiceControlSetDevicesV2,
    'command-set-talk-mode': voiceControlSetTalkModeV2,
    // The v2 directory's hello is the v1 hello offering both.
    'event-hello': voiceControlHelloV1,
    'event-ack': voiceControlAckV2,
    'event-state': voiceControlStateV2,
    'event-protocol-error': voiceControlProtocolErrorV2,
  },
  'voice-control-v3': {
    'command-start': voiceControlStartV3,
    'command-set-mute': voiceControlSetMuteV3,
    'command-set-deafen': voiceControlSetDeafenV3,
    'command-get-state': voiceControlGetStateV3,
    'command-leave': voiceControlLeaveV3,
    'command-set-devices': voiceControlSetDevicesV3,
    'command-set-talk-mode': voiceControlSetTalkModeV3,
    // The longer family wins: a set-share-audio-mute fixture is not a
    // set-share-audio one.
    'command-set-share-audio': voiceControlSetShareAudioV3,
    'command-set-share-audio-mute': voiceControlSetShareAudioMuteV3,
    // The v3 directory's hello is the v1 hello offering the three versions.
    'event-hello': voiceControlHelloV1,
    'event-ack': voiceControlAckV3,
    'event-state': voiceControlStateV3,
    'event-protocol-error': voiceControlProtocolErrorV3,
  },
  'voice-control-v4': {
    'command-start': voiceControlStartV4,
    'command-set-mute': voiceControlSetMuteV4,
    'command-set-deafen': voiceControlSetDeafenV4,
    'command-get-state': voiceControlGetStateV4,
    'command-leave': voiceControlLeaveV4,
    'command-set-devices': voiceControlSetDevicesV4,
    'command-set-talk-mode': voiceControlSetTalkModeV4,
    'command-set-share-audio': voiceControlSetShareAudioV4,
    'command-set-share-audio-mute': voiceControlSetShareAudioMuteV4,
    'command-set-diagnostics': voiceControlSetDiagnosticsV4,
    'command-get-call-quality': voiceControlGetCallQualityV4,
    'event-hello': voiceControlHelloV1,
    'event-ack': voiceControlAckV4,
    'event-state': voiceControlStateV4,
    'event-protocol-error': voiceControlProtocolErrorV4,
    'event-levels': voiceControlLevelsV4,
    'event-gate-event': voiceControlGateEventV4,
    'event-call-quality': voiceControlCallQualityV4,
  },
  'voice-control-pipe': {
    attached: voiceControlPipeAttachedV1,
  },
};

// The validator of a fixture file: the longest family prefix of its name.
function validatorOf(dir: string, file: string): Validator<unknown> | undefined {
  const base = file.replace(/\.json$/, '');
  const family = Object.keys(validators[dir] ?? {})
    .filter((f) => base === f || base.startsWith(`${f}-`))
    .sort((a, b) => b.length - a.length)[0];
  return family === undefined ? undefined : validators[dir]?.[family];
}

describe('golden fixtures match the generated types', () => {
  const cases = Object.keys(validators).flatMap((dir) =>
    readdirSync(join(TESTDATA, dir))
      .filter((f) => /^(command|event)-.*\.json$|^attached\.json$/.test(f))
      .map((file) => ({ dir, file })),
  );

  it('are found in every directory', () => {
    for (const dir of Object.keys(validators)) expect(cases.some((c) => c.dir === dir)).toBe(true);
  });

  it.each(cases)('$dir/$file', ({ dir, file }) => {
    const check = validatorOf(dir, file);
    expect(check, `no validator for ${file}`).toBeDefined();
    const doc: unknown = JSON.parse(readFileSync(join(TESTDATA, dir, file), 'utf8'));
    expect(check?.(doc)).toEqual([]);
  });

  it('a validator refuses a key the type does not have', () => {
    expect(voiceControlStateV2({ ...readFixture<object>('voice-control-v2/event-state-connected.json'), token: 'x' })).toContain(
      '$.token: key is not in the TypeScript type',
    );
  });

  // v3 (issue #1851): the same of every one of its documents, the hello
  // included, and neither version's state validator takes the other's state.
  const v3 = cases.filter((c) => c.dir === 'voice-control-v3');

  it('every kind of v3 document has a fixture', () => {
    const families = new Set(v3.map((c) => c.file.replace(/\.json$/, '')).map((base) =>
      Object.keys(validators['voice-control-v3'] ?? {})
        .filter((f) => base === f || base.startsWith(`${f}-`))
        .sort((a, b) => b.length - a.length)[0],
    ));
    expect([...families].sort()).toEqual(Object.keys(validators['voice-control-v3'] ?? {}).sort());
  });

  it.each(v3)('$dir/$file with an added key is refused', ({ dir, file }) => {
    const doc = readFixture<object>(`${dir}/${file}`);
    expect(validatorOf(dir, file)?.({ ...doc, token: 'x' })).toContain('$.token: key is not in the TypeScript type');
  });

  const v4 = cases.filter((c) => c.dir === 'voice-control-v4');
  it.each(v4)('$dir/$file with an added key is refused', ({ dir, file }) => {
    const doc = readFixture<object>(`${dir}/${file}`);
    expect(validatorOf(dir, file)?.({ ...doc, pid: 1234 })).toContain('$.pid: key is not in the TypeScript type');
  });

  it('v4 state requires both processing flags and refuses extra processing keys', () => {
    const state = readFixture<object>('voice-control-v4/event-state-connected.json');
    expect(voiceControlStateV4({ ...state, processing: { echo_cancellation: true } }))
      .toContain('$.processing.noise_suppression: required key is missing');
    expect(voiceControlStateV4({ ...state, processing: { echo_cancellation: true, noise_suppression: true, node_id: 1234 } }))
      .toContain('$.processing.node_id: key is not in the TypeScript type');
  });

  it('a state of one version is not a state of the other', () => {
    const state2 = readFixture<object>('voice-control-v2/event-state-connected.json');
    const state3 = readFixture<object>('voice-control-v3/event-state-share-audio-on.json');
    expect(voiceControlStateV3(state3)).toEqual([]);
    expect(voiceControlStateV3(state2)).not.toEqual([]);
    expect(voiceControlStateV2(state3)).not.toEqual([]);
    // Not by the schema name alone: a v2 state under v3's name lacks the
    // fields v3 always writes.
    expect(voiceControlStateV3({ ...state2, schema: 'conch.voice_control.v3' })).toContain('$.share_audio: required key is missing');
  });

  it('the vocabularies of v3 are strict', () => {
    const state = readFixture<object>('voice-control-v3/event-state-share-audio-publish-refused.json');
    expect(voiceControlStateV3({ ...state, share_audio: 'starting' })).not.toEqual([]);
    expect(voiceControlStateV3({ ...state, share_audio_problem: 'no_consent' })).not.toEqual([]);
    expect(voiceControlStateV3({ ...state, share_audio_problem: '' })).not.toEqual([]);
    const ask = readFixture<object>('voice-control-v3/command-set-share-audio.json');
    expect(voiceControlSetShareAudioV3({ ...ask, scope: 'system' })).not.toEqual([]);
    expect(voiceControlSetShareAudioV3({ ...ask, kind: 'set_volume' })).not.toEqual([]);
    // A kind of v3 is not a kind of the published versions' types.
    expect(voiceControlSetMuteV2({ schema: 'conch.voice_control.v2', kind: 'set_share_audio_mute', id: 1, muted: true })).not.toEqual([]);
  });
});

interface Case {
  readonly name: string;
  readonly direction: string;
  readonly line?: string;
  readonly line_hex?: string;
  readonly line_file?: string;
  readonly want: { readonly result: 'accept' | 'refuse' | 'skip'; readonly kind?: string; readonly code?: string; readonly offers_v1?: boolean };
}

function lineOf(dir: string, c: Case): Buffer {
  if (c.line !== undefined) return Buffer.from(c.line, 'utf8');
  if (c.line_hex !== undefined) return Buffer.from(c.line_hex, 'hex');
  return readFileSync(join(TESTDATA, dir, c.line_file ?? ''));
}

function casesOf(dir: string, direction: string): (Case & { readonly dir: string; readonly file: string })[] {
  return readdirSync(join(TESTDATA, dir))
    .filter((f) => /^cases.*\.json$/.test(f))
    .flatMap((file) =>
      readFixture<{ cases: Case[] }>(`${dir}/${file}`)
        .cases.filter((c) => c.direction === direction)
        .map((c) => ({ ...c, dir, file })),
    );
}

describe('the v4 TypeScript command validators match the Go conformance cases', () => {
  const cases = casesOf('voice-control-v4', 'command');

  it('finds the command cases', () => {
    expect(cases.length).toBeGreaterThan(0);
  });

  it.each(cases)('$name', (c) => {
    const doc: unknown = JSON.parse(lineOf(c.dir, c).toString('utf8'));
    if (typeof doc !== 'object' || doc === null || !('kind' in doc) || typeof doc.kind !== 'string') {
      throw new Error('expected a command kind in the fixture');
    }
    const family = `command-${doc.kind.replaceAll('_', '-')}`;
    const check = validators[c.dir]?.[family];
    if (c.want.result === 'accept') {
      expect(check).toBeDefined();
      expect(check?.(doc)).toEqual([]);
    } else if (c.want.code === 'unknown_kind') {
      expect(check).toBeUndefined();
    } else {
      expect(check).toBeDefined();
      expect(check?.(doc)).not.toEqual([]);
    }
  });
});

describe.each([
  { dir: 'voice-control', parse: parseEventV1, field: 'retry_in_ms' },
  { dir: 'voice-control-v2', parse: parseEventV2, field: 'retry_in_ms' },
  { dir: 'voice-control-v3', parse: parseEventV3, field: 'retry_in_ms' },
  { dir: 'voice-control-v3', parse: parseEventV3, field: 'share_audio_sources' },
])('$dir integer field $field (#1857)', ({ dir, parse, field }) => {
  const zero = readFixture<{ cases: Case[] }>(`${dir}/cases-integers.json`).cases.find((c) => c.name === `${field} may be zero`);

  it.each(['-0', ' \t-0 '])('refuses %s as invalid', (raw) => {
    expect(zero?.line).toBeDefined();
    const line = zero?.line?.replace(`"${field}":0`, `"${field}":${raw}`) ?? '';
    const got = parse(Buffer.from(line));
    expect(got.ok ? 'accepted' : got.code).toBe('invalid');
  });

  it('accepts ordinary zero and ignores negative zero in unknown fields', () => {
    expect(zero?.line).toBeDefined();
    const line = zero?.line ?? '';
    expect(parse(Buffer.from(line)).ok).toBe(true);
    expect(parse(Buffer.from(line.replace(/}$/, ',"ignored":-0}'))).ok).toBe(true);
  });
});

describe('the owner reads every conformance case as Go does', () => {
  const v1 = casesOf('voice-control', 'event');
  const v2 = casesOf('voice-control-v2', 'event');
  const attached = casesOf('voice-control-pipe', 'attached');

  it('finds them', () => {
    expect(v1.length).toBeGreaterThan(20);
    expect(v2.length).toBeGreaterThan(20);
    expect(attached.length).toBeGreaterThan(40);
  });

  it.each(v1)('v1 $file: $name', (c) => {
    const got = parseEventV1(lineOf(c.dir, c));
    switch (c.want.result) {
      case 'accept':
        expect(got.ok, got.ok ? '' : got.code).toBe(true);
        if (got.ok) {
          expect(got.event.kind).toBe(c.want.kind);
          if (c.want.offers_v1 !== undefined && got.event.kind === 'hello') {
            expect(got.event.hello.versions.includes(SCHEMA_V1)).toBe(c.want.offers_v1);
          }
        }
        break;
      case 'skip':
        expect(got.ok && got.event.kind === 'unknown').toBe(true);
        break;
      case 'refuse':
        expect(got.ok ? 'accepted' : got.code).toBe(c.want.code);
    }
  });

  it.each(v2)('v2 $file: $name', (c) => {
    const got = parseEventV2(lineOf(c.dir, c));
    switch (c.want.result) {
      case 'accept':
        expect(got.ok, got.ok ? '' : got.code).toBe(true);
        if (got.ok) expect(got.event.kind).toBe(c.want.kind);
        break;
      case 'skip':
        expect(got.ok && got.event.kind === 'unknown').toBe(true);
        break;
      case 'refuse':
        expect(got.ok ? 'accepted' : got.code).toBe(c.want.code);
    }
  });

  it.each(attached)('attach $file: $name', (c) => {
    const got = parseAttached(lineOf(c.dir, c));
    if (c.want.result === 'accept') {
      expect(got.ok, got.ok ? '' : got.code).toBe(true);
      if (got.ok) expect(got.attached.kind).toBe(c.want.kind);
    } else {
      expect(got.ok ? 'accepted' : got.code).toBe(c.want.code);
    }
  });

  it('never repeats the line or the nonce in a refusal', () => {
    const nonce = readFixture<{ fake_nonces: string[] }>('voice-control-pipe/sensitive.json').fake_nonces[0] ?? '';
    const got = parseAttached(Buffer.from(JSON.stringify({ schema: 'conch.voice_control_pipe.v1', kind: 'detached', nonce })));
    expect(got.ok).toBe(false);
    expect(JSON.stringify(got)).not.toContain(nonce);
  });
});

// Issue #1457: voice_removed is a reason to fail, read in v2 and refused in v1.
describe('voice_removed', () => {
  const golden = readFixture<Record<string, unknown>>('voice-control-v2/event-state-failed-voice-removed.json');
  const line = (doc: object): Buffer => Buffer.from(JSON.stringify(doc));

  it('a v2 owner reads it with failed, and with stopping on the way there', () => {
    const failed = parseEventV2(line(golden));
    expect(failed).toMatchObject({ ok: true, event: { kind: 'state', state: { phase: 'failed', reason: 'voice_removed' } } });
    expect(parseEventV2(line({ ...golden, phase: 'stopping' })).ok).toBe(true);
  });

  it('is not a reason to stop cleanly or to reconnect', () => {
    expect(parseEventV2(line({ ...golden, phase: 'stopped' })).ok).toBe(false);
    expect(parseEventV2(line({ ...golden, phase: 'reconnecting' })).ok).toBe(false);
  });

  it('a v1 state with it is refused, and the same state with a v1 reason is read', () => {
    const v1 = { ...readFixture<Record<string, unknown>>('voice-control/event-state-stopped.json'), phase: 'failed' };
    expect(parseEventV1(line({ ...v1, reason: 'internal_error' })).ok).toBe(true);
    expect(parseEventV1(line({ ...v1, reason: 'voice_removed' })).ok).toBe(false);
    expect(parseEventV1(line({ ...v1, phase: 'stopping', reason: 'voice_removed' })).ok).toBe(false);
  });
});

describe('commands', () => {
  const start = { server: 'https://conch.example.org', channel: 'general', principalId: 7, muted: false, deafened: false };

  it('the v1 start is the golden one, with the credential in the environment', () => {
    const golden = readFixture<{ server: string; channel: string; principal_id: number; muted: boolean; deafened: boolean }>('voice-control/command-start-env.json');
    const c = startCommand(SCHEMA_V1, 1, { server: golden.server, channel: golden.channel, principalId: golden.principal_id, muted: golden.muted, deafened: golden.deafened });
    expect(JSON.parse(encodeCommand(c).toString())).toEqual(golden);
  });

  it.each([
    ['voice-control/command-set-mute.json', SCHEMA_V1, 7, { kind: 'set_mute', muted: true }],
    ['voice-control/command-set-deafen.json', SCHEMA_V1, 8, { kind: 'set_deafen', deafened: true }],
    ['voice-control/command-get-state.json', SCHEMA_V1, 9, { kind: 'get_state' }],
    ['voice-control/command-leave.json', SCHEMA_V1, 10, { kind: 'leave' }],
    ['voice-control-v2/command-set-talk-mode.json', SCHEMA_V2, 0, { kind: 'set_talk_mode', talkMode: 'open' }],
    ['voice-control-v2/command-leave.json', SCHEMA_V2, 0, { kind: 'leave' }],
  ] as const)('%s', (file, version, _unused, want) => {
    const golden = readFixture<{ id: number } & Record<string, unknown>>(file);
    const c = command(version, golden.id, want);
    expect(c).not.toBeNull();
    expect(JSON.parse(encodeCommand(c as Command).toString())).toEqual(golden);
  });

  it('a v1 session has no set_devices or set_talk_mode', () => {
    expect(command(SCHEMA_V1, 2, { kind: 'set_devices', input: 'default' })).toBeNull();
    expect(command(SCHEMA_V1, 2, { kind: 'set_talk_mode', talkMode: 'open' })).toBeNull();
  });

  it('refuses to write a start the helper must refuse', () => {
    expect(() => encodeCommand(startCommand(SCHEMA_V2, 1, { ...start, server: 'https://Conch.example.org' }))).toThrow(/server address/);
    expect(() => encodeCommand(startCommand(SCHEMA_V2, 1, { ...start, channel: 'a\u0007b' }))).toThrow(/channel/);
    expect(() => encodeCommand(startCommand(SCHEMA_V2, 1, { ...start, principalId: 0 }))).toThrow(/principal_id/);
    expect(() => encodeCommand(startCommand(SCHEMA_V2, 0, start))).toThrow(/id/);
  });

  it('server and channel rules', () => {
    for (const ok of ['https://conch.example.org', 'http://127.0.0.1:8080', 'https://example.org/conch']) expect(validServer(ok), ok).toBe(true);
    for (const bad of ['', 'ftp://x', 'https://x/', 'https://u@x', 'https://x?q', 'https://x#f', 'https://X.org', 'conch.example.org']) {
      expect(validServer(bad), bad).toBe(false);
    }
    expect(validChannel('general')).toBe(true);
    for (const bad of ['', '  ', 'a\nb', '\ud800', 'x'.repeat(401)]) expect(validChannel(bad), JSON.stringify(bad)).toBe(false);
  });

  it('nonce form', () => {
    for (const n of readFixture<{ fake_nonces: string[] }>('voice-control-pipe/sensitive.json').fake_nonces) expect(validNonce(n)).toBe(true);
    expect(validNonce('A'.repeat(42) + 'B')).toBe(false);
    expect(validNonce('A'.repeat(42))).toBe(false);
  });
});

describe('negotiation', () => {
  const hello = (versions: string[]): VoiceControlHelloV1 => ({ ...readFixture<VoiceControlHelloV1>('voice-control/event-hello.json'), versions });

  it('v2 when both sides implement it', () => {
    expect(negotiate(readFixture<VoiceControlHelloV1>('voice-control-v2/event-hello-v1-and-v2.json'))).toBe(SCHEMA_V2);
  });

  it('v1 when the helper offers only v1', () => {
    expect(negotiate(readFixture<VoiceControlHelloV1>('voice-control/event-hello.json'))).toBe(SCHEMA_V1);
  });

  it('nothing when the helper offers only versions this owner does not implement', () => {
    expect(negotiate(hello(['conch.voice_control.v3']))).toBeNull();
    expect(negotiate(hello(['conch.voice_control.v3', 'conch.voice_control.v9']))).toBeNull();
    // The v1 directory's mismatch is a helper offering only v2, which this
    // owner speaks.
    const v2only = transcript('voice-control/transcript-version-mismatch.json')[0]?.message as unknown as VoiceControlHelloV1;
    expect(negotiate(v2only)).toBe(SCHEMA_V2);
  });
});

describe('LineSplitter', () => {
  it('splits across chunks and reports a long line as soon as it passes the bound', () => {
    const got: string[] = [];
    const s = new LineSplitter();
    const push = (b: Buffer): void => {
      s.push(b, (l) => got.push(l === 'too_long' ? l : l.toString()));
    };
    push(Buffer.from('{"a"'));
    push(Buffer.from(':1}\n{"b":2}\n'));
    push(Buffer.alloc(4097, 0x78));
    expect(got).toEqual(['{"a":1}', '{"b":2}', 'too_long']);
    push(Buffer.from('xxxx\n{"c":3}\n'));
    expect(got).toEqual(['{"a":1}', '{"b":2}', 'too_long', '{"c":3}']);
  });

  it('takes a line of exactly 4096 bytes', () => {
    const got: (Buffer | 'too_long')[] = [];
    new LineSplitter().push(Buffer.concat([Buffer.alloc(4096, 0x78), Buffer.from('\n')]), (l) => got.push(l));
    expect(got.length).toBe(1);
    expect(got[0]).not.toBe('too_long');
  });
});

describe('Tracker', () => {
  const files = ['voice-control', 'voice-control-v2', 'voice-control-pipe'].flatMap((dir) =>
    readdirSync(join(TESTDATA, dir))
      .filter((f) => f.startsWith('transcript-'))
      .map((f) => `${dir}/${f}`),
  );

  // Replays a transcript's helper lines, recording each owner command as
  // written; the version is the one the owner's start names.
  it.each(files)('accepts every helper line of %s', (file) => {
    const t = new Tracker();
    for (const step of transcript(file)) {
      if (step.message === undefined) continue;
      if (step.message['kind'] === 'attached') continue;
      if (step.from === 'owner') {
        if (step.message['kind'] === 'start') t.version = step.message['schema'] === SCHEMA_V2 ? SCHEMA_V2 : SCHEMA_V1;
        t.written(step.message as unknown as Command);
        continue;
      }
      const r = t.read(Buffer.from(JSON.stringify(step.message)));
      expect(r.ok, `${file}: ${r.ok ? '' : r.detail}`).toBe(true);
    }
  });

  const hello = Buffer.from(JSON.stringify(readFixture('voice-control/event-hello.json')));
  const state = (seq: number): Buffer => Buffer.from(JSON.stringify({ ...readFixture<object>('voice-control/event-state-starting.json'), seq }));
  const ack = (seq: number, id: number): Buffer => Buffer.from(JSON.stringify({ ...readFixture<object>('voice-control/event-ack.json'), seq, id }));
  const start = startCommand(SCHEMA_V1, 1, { server: 'https://conch.example.org', channel: 'general', principalId: 7, muted: false, deafened: false });

  it('refuses a first event that is not a hello', () => {
    const t = new Tracker();
    t.written(start);
    expect(t.read(state(1))).toEqual({ ok: false, detail: 'the first event is not a hello' });
  });

  it('refuses a state before any command', () => {
    const t = new Tracker();
    expect(t.read(hello).ok).toBe(true);
    expect(t.read(state(2))).toEqual({ ok: false, detail: 'an event other than hello before any command' });
  });

  it('refuses a seq that does not increase, and an ack for another command', () => {
    const t = new Tracker();
    t.read(hello);
    t.written(start);
    expect(t.read(state(2)).ok).toBe(true);
    expect(t.read(state(2))).toEqual({ ok: false, detail: 'seq did not increase' });
    expect(t.read(ack(3, 5))).toEqual({ ok: false, detail: 'an ack for a command that is not the next one' });
  });

  it('marks a gap and refuses an event after the final state', () => {
    const t = new Tracker();
    t.read(hello);
    t.written(start);
    const gap = t.read(state(5));
    expect(gap.ok && gap.gap).toBe(true);
    const stopped = Buffer.from(JSON.stringify({ ...readFixture<object>('voice-control/event-state-stopped.json'), seq: 6 }));
    expect(t.read(stopped).ok).toBe(true);
    expect(t.read(state(7))).toEqual({ ok: false, detail: 'an event after the final state' });
  });

  it('refuses a line too long, and reads v2 once the session is v2', () => {
    const t = new Tracker();
    expect(t.read('too_long')).toEqual({ ok: false, detail: 'too_long' });
    const t2 = new Tracker();
    t2.read(Buffer.from(JSON.stringify(readFixture('voice-control-v2/event-hello-v1-and-v2.json'))));
    t2.version = SCHEMA_V2;
    t2.written(startCommand(SCHEMA_V2, 1, { server: 'https://conch.example.org', channel: 'general', principalId: 7, muted: false, deafened: false }));
    expect(t2.read(state(2))).toEqual({ ok: false, detail: 'unknown_schema' });
    expect(t2.read(Buffer.from(JSON.stringify(readFixture('voice-control-v2/event-state-starting.json')))).ok).toBe(true);
  });
});

// ------------------------------------------------------------------ v3 (#1854)

describe("the owner's v3 reader", () => {
  const v3 = casesOf('voice-control-v3', 'event');
  const events = readdirSync(join(TESTDATA, 'voice-control-v3')).filter((f) => /^event-(ack|state|protocol-error).*\.json$/.test(f));

  it('finds them', () => {
    expect(v3.length).toBeGreaterThan(60);
    expect(events.length).toBeGreaterThan(15);
  });

  it.each(v3)('v3 $file: $name', (c) => {
    const got = parseEventV3(lineOf(c.dir, c));
    switch (c.want.result) {
      case 'accept':
        expect(got.ok, got.ok ? '' : got.code).toBe(true);
        if (got.ok) expect(got.event.kind).toBe(c.want.kind);
        break;
      case 'skip':
        expect(got.ok && got.event.kind === 'unknown').toBe(true);
        break;
      case 'refuse':
        expect(got.ok ? 'accepted' : got.code).toBe(c.want.code);
    }
  });

  // Every golden event is read as it was written: the problem left out when
  // there is none, no_sources only beside on and zero sources, every field the
  // generated type has.
  it.each(events)('reads the golden %s into exactly its fields', (file) => {
    const golden = readFixture<Record<string, unknown>>(`voice-control-v3/${file}`);
    const got = parseEventV3(Buffer.from(JSON.stringify(golden)));
    expect(got.ok, got.ok ? '' : got.code).toBe(true);
    if (!got.ok) return;
    const doc = got.event.kind === 'ack' ? got.event.ack : got.event.kind === 'state' ? got.event.state : got.event.kind === 'protocol_error' ? got.event.error : null;
    expect(doc).toEqual(golden);
    // Not a key more than the type has, and the problem omitted when none.
    expect(Object.keys(doc ?? {}).sort()).toEqual(Object.keys(golden).sort());
  });

  it('a state with no problem has no share_audio_problem key at all', () => {
    const got = parseEventV3(Buffer.from(JSON.stringify(readFixture('voice-control-v3/event-state-share-audio-on.json'))));
    expect(got.ok && got.event.kind === 'state' && 'share_audio_problem' in got.event.state).toBe(false);
  });

  it('refuses a line of another version, and every other version refuses a v3 line', () => {
    const state3 = Buffer.from(JSON.stringify(readFixture('voice-control-v3/event-state-share-audio-on.json')));
    const state2 = Buffer.from(JSON.stringify(readFixture('voice-control-v2/event-state-connected.json')));
    const state1 = Buffer.from(JSON.stringify(readFixture('voice-control/event-state-connected.json')));
    expect(parseEventV3(state2)).toMatchObject({ ok: false, code: 'unknown_schema' });
    expect(parseEventV3(state1)).toMatchObject({ ok: false, code: 'unknown_schema' });
    expect(parseEventV2(state3)).toMatchObject({ ok: false, code: 'unknown_schema' });
    expect(parseEventV1(state3)).toMatchObject({ ok: false, code: 'unknown_schema' });
    // Under v3's name a v2 state lacks the fields v3 always writes: not read as off.
    expect(parseEventV3(Buffer.from(JSON.stringify({ ...readFixture<object>('voice-control-v2/event-state-connected.json'), schema: SCHEMA_V3 })))).toMatchObject({ ok: false, code: 'invalid' });
  });

  it('a hello is v1’s, and invalid in v3', () => {
    expect(parseEventV3(Buffer.from(JSON.stringify({ ...readFixture<object>('voice-control/event-hello.json'), schema: SCHEMA_V3 })))).toMatchObject({ ok: false, code: 'invalid' });
  });
});

describe('v3 commands', () => {
  const dir = 'voice-control-v3';
  const start = { server: 'https://conch.example.org', channel: 'general', principalId: 7, muted: false, deafened: false };

  // The want a golden command stands for, read from the golden itself.
  function wantOf(g: Record<string, unknown>): Parameters<typeof command>[2] | null {
    switch (g['kind']) {
      case 'set_mute':
        return { kind: 'set_mute', muted: g['muted'] as boolean };
      case 'set_deafen':
        return { kind: 'set_deafen', deafened: g['deafened'] as boolean };
      case 'get_state':
        return { kind: 'get_state' };
      case 'leave':
        return { kind: 'leave' };
      case 'set_devices':
        return { kind: 'set_devices', ...(g['input'] === undefined ? {} : { input: g['input'] as string }), ...(g['output'] === undefined ? {} : { output: g['output'] as string }) };
      case 'set_talk_mode':
        return { kind: 'set_talk_mode', talkMode: g['talk_mode'] as 'open' };
      case 'set_share_audio':
        return g['capturing'] === true
          ? { kind: 'set_share_audio', capturing: true, scope: 'system_excluding_call', applicationName: g['application_name'] as string }
          : { kind: 'set_share_audio', capturing: false };
      case 'set_share_audio_mute':
        return { kind: 'set_share_audio_mute', muted: g['muted'] as boolean };
      default:
        return null;
    }
  }

  const goldens = readdirSync(join(TESTDATA, dir)).filter((f) => /^command-.*\.json$/.test(f) && f !== 'command-start.json' && f !== 'command-set-share-audio-application.json');

  it('finds them', () => {
    expect(goldens).toEqual(
      expect.arrayContaining(['command-set-share-audio.json', 'command-set-share-audio-stop.json', 'command-set-share-audio-mute.json', 'command-set-share-audio-mute-off.json', 'command-leave.json']),
    );
  });

  it.each(goldens)('builds the golden %s byte for byte', (file) => {
    const golden = readFixture<{ id: number } & Record<string, unknown>>(`${dir}/${file}`);
    const want = wantOf(golden);
    expect(want, file).not.toBeNull();
    const c = command(SCHEMA_V3, golden.id, want as NonNullable<typeof want>);
    expect(c).not.toBeNull();
    const line = encodeCommand(c as Command);
    expect(line.at(-1)).toBe(0x0a);
    expect(JSON.parse(line.toString())).toEqual(golden);
  });

  it('writes the v3 start with the credential in the environment, as v1 and v2 do', () => {
    const golden = readFixture<Record<string, unknown>>(`${dir}/command-start.json`);
    const c = startCommand(SCHEMA_V3, 1, { ...start, principalId: 7 });
    expect(JSON.parse(encodeCommand(c).toString())).toEqual({ ...golden, credential: 'env' });
  });

  it('a stop carries no scope and no name, and a start of the sound carries both', () => {
    const stop = JSON.parse(encodeCommand(command(SCHEMA_V3, 3, { kind: 'set_share_audio', capturing: false }) as Command).toString()) as Record<string, unknown>;
    expect(Object.keys(stop).sort()).toEqual(['capturing', 'id', 'kind', 'schema']);
    const on = JSON.parse(encodeCommand(command(SCHEMA_V3, 3, { kind: 'set_share_audio', capturing: true, scope: 'system_excluding_call', applicationName: 'Conch' }) as Command).toString()) as Record<string, unknown>;
    expect(on).toMatchObject({ capturing: true, scope: 'system_excluding_call', application_name: 'Conch' });
  });

  it('never writes the application scope: the golden that has it is refused, whoever built it', () => {
    const application = readFixture<Command>(`${dir}/command-set-share-audio-application.json`);
    expect(() => encodeCommand(application)).toThrow(/system_excluding_call/);
    const built = command(SCHEMA_V3, 4, { kind: 'set_share_audio', capturing: true, scope: 'system_excluding_call', applicationName: 'Conch' }) as Command;
    expect(() => encodeCommand({ ...built, scope: 'application' } as Command)).toThrow(/system_excluding_call/);
  });

  it('refuses to write a share-audio command the helper must refuse', () => {
    const on = command(SCHEMA_V3, 4, { kind: 'set_share_audio', capturing: true, scope: 'system_excluding_call', applicationName: 'Conch' }) as unknown as Record<string, unknown>;
    const stop = command(SCHEMA_V3, 5, { kind: 'set_share_audio', capturing: false }) as unknown as Record<string, unknown>;
    const mute = command(SCHEMA_V3, 6, { kind: 'set_share_audio_mute', muted: true }) as unknown as Record<string, unknown>;
    const refused = [
      { ...on, application_name: undefined },
      { ...on, application_name: '' },
      { ...on, application_name: 'x'.repeat(129) },
      { ...on, application_name: 'Conch\n' },
      { ...on, application_name: 'Cönch' },
      { ...on, scope: undefined },
      { ...stop, scope: 'system_excluding_call' },
      { ...stop, application_name: 'Conch' },
      { ...on, token: 'secret' },
      { ...mute, scope: 'system_excluding_call' },
      { ...mute, id: 0 },
      { ...on, schema: SCHEMA_V2 },
      { ...mute, schema: SCHEMA_V1 },
    ];
    for (const c of refused) expect(() => encodeCommand(c as unknown as Command), JSON.stringify(c)).toThrow();
    expect(() => encodeCommand(on as unknown as Command)).not.toThrow();
    expect(() => encodeCommand(stop as unknown as Command)).not.toThrow();
  });

  it('the names it will write: 1 to 128 bytes of printable ASCII', () => {
    for (const ok of ['Conch', 'c', 'x'.repeat(128), 'My App (beta) 1.2 "q" \\ ~']) expect(validApplicationName(ok), ok).toBe(true);
    for (const bad of ['', 'x'.repeat(129), 'a\tb', 'a\u007fb', 'ü', '😀']) expect(validApplicationName(bad), JSON.stringify(bad)).toBe(false);
  });

  it('only a v3 session has the share-audio commands', () => {
    for (const version of [SCHEMA_V1, SCHEMA_V2] as const) {
      expect(command(version, 2, { kind: 'set_share_audio', capturing: false })).toBeNull();
      expect(command(version, 2, { kind: 'set_share_audio', capturing: true, scope: 'system_excluding_call', applicationName: 'Conch' })).toBeNull();
      expect(command(version, 2, { kind: 'set_share_audio_mute', muted: true })).toBeNull();
    }
  });

  it('writes every v1 and v2 command of the v3 session in v3', () => {
    for (const want of [{ kind: 'set_mute', muted: true }, { kind: 'set_deafen', deafened: true }, { kind: 'get_state' }, { kind: 'leave' }, { kind: 'set_devices', input: 'default' }, { kind: 'set_talk_mode', talkMode: 'open' }] as const) {
      const c = command(SCHEMA_V3, 9, want);
      expect(c?.schema).toBe(SCHEMA_V3);
      expect(() => encodeCommand(c as Command)).not.toThrow();
    }
  });

  it('refuses a v3 start the helper must refuse, as v2’s', () => {
    expect(() => encodeCommand(startCommand(SCHEMA_V3, 1, { ...start, server: 'https://Conch.example.org' }))).toThrow(/server address/);
    expect(() => encodeCommand(startCommand(SCHEMA_V3, 1, { ...start, principalId: 0 }))).toThrow(/principal_id/);
  });
});

describe('v3 negotiation', () => {
  const hello = readFixture<VoiceControlHelloV1>('voice-control-v3/event-hello-v1-v2-and-v3.json');

  it('offers v3 first only with the switch on, and never otherwise', () => {
    expect(OWNER_VERSIONS).toEqual([SCHEMA_V2, SCHEMA_V1]);
    expect(OWNER_VERSIONS_SHARE_AUDIO).toEqual([SCHEMA_V3, SCHEMA_V2, SCHEMA_V1]);
  });

  it('a v3 helper is spoken to in v3 with the switch on, and in v2 (silent) with it off', () => {
    expect(negotiate(hello, OWNER_VERSIONS_SHARE_AUDIO)).toBe(SCHEMA_V3);
    expect(negotiate(hello)).toBe(SCHEMA_V2);
  });

  it('falls back to v2 and then v1 when the helper does not offer v3', () => {
    expect(negotiate(readFixture<VoiceControlHelloV1>('voice-control-v2/event-hello-v1-and-v2.json'), OWNER_VERSIONS_SHARE_AUDIO)).toBe(SCHEMA_V2);
    expect(negotiate(readFixture<VoiceControlHelloV1>('voice-control/event-hello.json'), OWNER_VERSIONS_SHARE_AUDIO)).toBe(SCHEMA_V1);
  });

  it('nothing when no version is shared', () => {
    expect(negotiate({ ...hello, versions: ['conch.voice_control.v9'] }, OWNER_VERSIONS_SHARE_AUDIO)).toBeNull();
  });
});

describe('the Tracker in v3', () => {
  const files = readdirSync(join(TESTDATA, 'voice-control-v3'))
    .filter((f) => f.startsWith('transcript-'))
    .map((f) => `voice-control-v3/${f}`);

  // The version is the one the owner's start names, as in the other versions.
  it.each(files)('accepts every helper line of %s', (file) => {
    const t = new Tracker();
    for (const step of transcript(file)) {
      if (step.message === undefined) continue;
      if (step.from === 'owner') {
        if (step.message['kind'] === 'start') t.version = step.message['schema'] === SCHEMA_V3 ? SCHEMA_V3 : step.message['schema'] === SCHEMA_V2 ? SCHEMA_V2 : SCHEMA_V1;
        t.written(step.message as unknown as Command);
        continue;
      }
      const r = t.read(Buffer.from(JSON.stringify(step.message)));
      expect(r.ok, `${file}: ${r.ok ? '' : r.detail}`).toBe(true);
    }
  });

  it('refuses a v2 line in a v3 session, and a v3 line in a v2 session', () => {
    const hello = Buffer.from(JSON.stringify(readFixture('voice-control-v3/event-hello-v1-v2-and-v3.json')));
    const t3 = new Tracker();
    t3.read(hello);
    t3.version = SCHEMA_V3;
    t3.written(startCommand(SCHEMA_V3, 1, { server: 'https://conch.example.org', channel: 'general', principalId: 7, muted: false, deafened: false }));
    expect(t3.read(Buffer.from(JSON.stringify(readFixture('voice-control-v2/event-state-starting.json'))))).toEqual({ ok: false, detail: 'unknown_schema' });
    expect(t3.read(Buffer.from(JSON.stringify(readFixture('voice-control-v3/event-state-starting.json')))).ok).toBe(true);
    const t2 = new Tracker();
    t2.read(hello);
    t2.version = SCHEMA_V2;
    t2.written(startCommand(SCHEMA_V2, 1, { server: 'https://conch.example.org', channel: 'general', principalId: 7, muted: false, deafened: false }));
    expect(t2.read(Buffer.from(JSON.stringify(readFixture('voice-control-v3/event-state-starting.json'))))).toEqual({ ok: false, detail: 'unknown_schema' });
  });

  it('pairs the ack of a share-audio command with the command written', () => {
    const t = new Tracker();
    t.read(Buffer.from(JSON.stringify(readFixture('voice-control-v3/event-hello-v1-v2-and-v3.json'))));
    t.version = SCHEMA_V3;
    t.written(startCommand(SCHEMA_V3, 1, { server: 'https://conch.example.org', channel: 'general', principalId: 7, muted: false, deafened: false }));
    expect(t.read(Buffer.from(JSON.stringify({ ...readFixture<object>('voice-control-v3/event-ack.json'), seq: 2, id: 1 }))).ok).toBe(true);
    t.written(command(SCHEMA_V3, 2, { kind: 'set_share_audio', capturing: false }) as Command);
    const acked = t.read(Buffer.from(JSON.stringify({ ...readFixture<object>('voice-control-v3/event-ack.json'), seq: 3, id: 2 })));
    expect(acked).toMatchObject({ ok: true, acked: 'set_share_audio' });
  });
});
