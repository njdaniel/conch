// The owner's protocol reader against pkg/schema's golden and conformance
// fixtures (issue #948): every document fixture of voice-control/,
// voice-control-v2/ and voice-control-pipe/ matches its generated validator,
// every event and attach case of cases*.json is read as Go reads it, the
// command builders write the golden commands, and the tracker accepts every
// transcript and refuses the stream rules' breaches.
import { readdirSync, readFileSync } from 'node:fs';
import { join } from 'node:path';

import { describe, expect, it } from 'vitest';

import {
  voiceControlAckV1,
  voiceControlAckV2,
  voiceControlGetStateV1,
  voiceControlGetStateV2,
  voiceControlHelloV1,
  voiceControlLeaveV1,
  voiceControlLeaveV2,
  voiceControlPipeAttachedV1,
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
  voiceControlStateV1,
  voiceControlStateV2,
  type VoiceControlHelloV1,
} from '../../../web/src/schema/generated.ts';
import type { Validator } from '../../../web/src/schema/validate.ts';
import {
  LineSplitter,
  SCHEMA_V1,
  SCHEMA_V2,
  Tracker,
  command,
  encodeCommand,
  negotiate,
  parseAttached,
  parseEventV1,
  parseEventV2,
  startCommand,
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
