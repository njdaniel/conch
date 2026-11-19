// Integration (issues #1854 and #1586): main's real share-sound code, the real
// Supervisor, VoiceService, Companion and ShareSound and main's real API
// client, against the real conch-voice over conch.voice_control.v3, with a
// stub conchd on loopback. The helper runs with `--mic none --sink count
// --test-share-source tone:440` (the hidden test-only flag of #1586): its
// share sound comes from a synthetic tone, so the test needs no microphone, no
// PipeWire and no display.
//
// Two parts:
//  - the negotiation, which needs only the helper and a refusing endpoint
//    (the helper stays reconnecting): v3 with the switch on, v2 with it off;
//  - the share's sound end to end, which needs a connected helper, so a real
//    LiveKit: the pinned livekit-server image (e2e/internal/lkit) run in Docker,
//    as voice/conch-voice/tests/live_share.rs runs it. The stub conchd hands
//    out a session that points at it, with tokens minted for its dev key, and
//    plays conchd's part of a consent: the REST routes, and the participant's
//    publish grant on the server (UpdateParticipant). The test asks the server
//    which tracks it holds.
//
// Without the binary, or without Docker, a part is skipped and says why; with
// CI set, a missing binary fails instead (as voice-helper.test.ts does).
// Run with `npm run test:integration` (make desktop-voice-it).
import { execFileSync, spawnSync } from 'node:child_process';
import { createHmac } from 'node:crypto';
import { chmodSync, copyFileSync, existsSync } from 'node:fs';
import { createServer, type IncomingMessage, type Server } from 'node:http';
import { createServer as listenProbe, type AddressInfo } from 'node:net';
import { join, resolve } from 'node:path';

import { afterAll, afterEach, beforeAll, beforeEach, describe, expect, it } from 'vitest';

import type { NativeVoiceView } from '../../../web/src/platform/desktop/bridge.ts';
import type { VoiceCompanionAudioRequestV1, VoiceCompanionAudioV1, VoiceCompanionResponseV1, VoiceSessionResponseV1 } from '../../../web/src/schema/generated.ts';
import { Client } from '../../main/api/client.ts';
import { Session } from '../../main/api/session.ts';
import { Companion } from '../../main/voice/companion.ts';
import { OWNER_VERSIONS_SHARE_AUDIO } from '../../main/voice/protocol.ts';
import { OneShot } from '../../main/voice/oneshot.ts';
import { VoiceService, type VoiceIdentity } from '../../main/voice/service.ts';
import { linuxShareAudioEnabled, registeredApplicationName, ShareSound, SHARE_AUDIO_FLAG } from '../../main/voice/share-audio.ts';
import { HELPER_NAME, defaultSpawn } from '../../main/voice/spawn.ts';
import { Supervisor } from '../../main/voice/supervisor.ts';
import { FAKE_TOKEN, readFixture, scratch, until, type Scratch } from '../unit/voice-support.ts';

const binary = process.env['CONCH_VOICE_BIN'] ?? resolve(import.meta.dirname, '../../../bin/conch-voice');
const have = existsSync(binary);
const ci = process.env['CI'] === 'true' || process.env['CI'] === '1';

if (!have) {
  const why = `the real conch-voice is not at ${binary}: run make rust-build, or set CONCH_VOICE_BIN`;
  if (ci) throw new Error(`share-audio-helper integration: ${why}`);
  console.warn(`share-audio-helper integration: SKIPPED: ${why}`);
}

// ------------------------------------------------------------- LiveKit

// The image e2e/internal/lkit pins, by digest; the dev server's key pair is
// made up for this test and guards nothing.
const IMAGE = 'livekit/livekit-server:v1.13.7@sha256:6fd3b7088874c4d119160dd688798dfec852bc014786d392caad15f6f63912a3';
const CONTAINER = 'conch-desktop-share-audio-it';
const KEY = 'devkey';
const SECRET = 'conch-desktop-share-audio-it-secret-0001';
const LK_PORT = 7880;
const LK_URL = `ws://127.0.0.1:${String(LK_PORT)}`;
const LK_API = `http://127.0.0.1:${String(LK_PORT)}/twirp/livekit.RoomService`;
const ROOM = 'desktop-share-it';
const IDENTITY = 'p1';

const b64 = (value: unknown): string => Buffer.from(JSON.stringify(value)).toString('base64url');

// A LiveKit token, HS256 with the dev server's secret.
function mint(identity: string, video: Record<string, unknown>): string {
  const now = Math.floor(Date.now() / 1000);
  const body = `${b64({ alg: 'HS256', typ: 'JWT' })}.${b64({ iss: KEY, sub: identity, nbf: now - 10, exp: now + 600, video })}`;
  return `${body}.${createHmac('sha256', SECRET).update(body).digest('base64url')}`;
}

// What the participant may publish, as conchd's token carries it.
const MIC_ONLY = ['microphone'];
const WITH_SHARE = ['microphone', 'screen_share_audio'];

const docker = (...args: string[]): { ok: boolean; out: string } => {
  const run = spawnSync('docker', args, { encoding: 'utf8', timeout: 120_000 });
  return { ok: run.status === 0, out: `${run.stdout}${run.stderr}` };
};

const portFree = (port: number): Promise<boolean> =>
  new Promise((done) => {
    const probe = listenProbe();
    probe.once('error', () => {
      done(false);
    });
    probe.listen(port, '127.0.0.1', () => {
      probe.close(() => {
        done(true);
      });
    });
  });

// Why LiveKit cannot be had here, or null when it can.
async function livekitUnavailable(): Promise<string | null> {
  if (!docker('info').ok) return 'Docker is not available';
  if (!docker('image', 'inspect', IMAGE).ok && !docker('pull', IMAGE).ok) return `the pinned livekit-server image is not here and could not be pulled (${IMAGE})`;
  if (!(await portFree(LK_PORT))) return `port ${String(LK_PORT)} on loopback is in use`;
  return null;
}

async function roomApi(method: string, body: Record<string, unknown>): Promise<Record<string, unknown>> {
  const admin = mint('admin', { roomAdmin: true, roomList: true, room: ROOM });
  const answer = await fetch(`${LK_API}/${method}`, { method: 'POST', headers: { 'content-type': 'application/json', authorization: `Bearer ${admin}` }, body: JSON.stringify(body) });
  return (await answer.json()) as Record<string, unknown>;
}

// The sources of the tracks the server holds for the participant.
async function serverSources(): Promise<string[]> {
  const listed = (await roomApi('ListParticipants', { room: ROOM })) as { participants?: { identity: string; tracks?: { source?: string }[] }[] };
  return (listed.participants ?? []).filter((p) => p.identity === IDENTITY).flatMap((p) => (p.tracks ?? []).map((t) => t.source ?? ''));
}

// conchd's part of a consent's grant: what the participant may publish, set
// on the server.
async function setPublishSources(sources: string[]): Promise<void> {
  const answer = await roomApi('UpdateParticipant', {
    room: ROOM,
    identity: IDENTITY,
    permission: { can_subscribe: true, can_publish: true, can_publish_data: true, can_publish_sources: sources.map((s) => s.toUpperCase()) },
  });
  if (answer['identity'] !== IDENTITY) throw new Error(`UpdateParticipant: ${JSON.stringify(answer)}`);
}

async function startLiveKit(): Promise<void> {
  docker('rm', '-f', CONTAINER);
  const started = docker('run', '-d', '--rm', '--name', CONTAINER, '--network', 'host', IMAGE, '--dev', '--bind', '127.0.0.1', '--node-ip', '127.0.0.1', '--keys', `${KEY}: ${SECRET}`);
  if (!started.ok) throw new Error(`the LiveKit container did not start: ${started.out}`);
  const deadline = Date.now() + 30_000;
  for (;;) {
    const up = await fetch(`http://127.0.0.1:${String(LK_PORT)}/`).then((r) => r.status === 200, () => false);
    if (up) return;
    if (Date.now() > deadline) throw new Error('LiveKit did not come up');
    await new Promise((r) => setTimeout(r, 200));
  }
}

const stopLiveKit = (): void => {
  docker('stop', CONTAINER);
};

// -------------------------------------------------------------- the rig

const DOC = 5;
const whoami = readFixture<{ id: number }>('whoami-response-v1.json');
const sessionTemplate = readFixture<VoiceSessionResponseV1>('voice-session-response-v1-companion.json');
const companionTemplate = readFixture<VoiceCompanionResponseV1>('voice-companion-response-v1.json');
const consentRequestFixture = readFixture<VoiceCompanionAudioRequestV1>('voice-companion-audio-request-v1-person.json');
const refusal = readFixture('error.json');

// The one list the test reads the order from: what conchd saw and what main
// asked of the helper, in the order they happened.
type Events = string[];

interface Conchd {
  readonly origin: string;
  readonly events: Events;
  // The consent bodies the PUT received.
  readonly consents: unknown[];
  // The helper's share-sound state at the moment each route was reached.
  readonly helperAt: Map<string, string>;
  // Where the stub says LiveKit is; empty refuses every session.
  livekit: string;
  close(): Promise<void>;
}

async function body(req: IncomingMessage): Promise<unknown> {
  const chunks: Buffer[] = [];
  for await (const chunk of req) chunks.push(chunk as Buffer);
  const text = Buffer.concat(chunks).toString('utf8');
  return text === '' ? undefined : (JSON.parse(text) as unknown);
}

async function stubConchd(helperState: () => string): Promise<Conchd> {
  const events: Events = [];
  const consents: unknown[] = [];
  const helperAt = new Map<string, string>();
  const conchd: { livekit: string } & Omit<Conchd, 'origin' | 'close' | 'livekit'> = { livekit: '', events, consents, helperAt };
  const json = (res: import('node:http').ServerResponse, status: number, value: unknown): void => {
    res.writeHead(status, { 'content-type': 'application/json' }).end(JSON.stringify(value));
  };
  const server: Server = createServer((req, res) => {
    void (async () => {
      const route = `${req.method ?? ''} ${req.url ?? ''}`;
      const sent = await body(req);
      const expires = new Date(Date.now() + 10 * 60_000).toISOString();
      switch (route) {
        case 'GET /v1/whoami':
          json(res, 200, whoami);
          return;
        case 'POST /v1/channels/general/voice/session': {
          if (conchd.livekit === '') {
            json(res, 503, refusal);
            return;
          }
          // The token of a participant who may speak; the share's grant is
          // given with the consent.
          const token = mint(IDENTITY, { roomJoin: true, room: ROOM, canPublish: true, canSubscribe: true, canPublishSources: MIC_ONLY });
          const session: VoiceSessionResponseV1 = { ...sessionTemplate, livekit_url: conchd.livekit, identity: IDENTITY, companion_identity: `${IDENTITY}.screen`, rooms: sessionTemplate.rooms.map((r) => ({ ...r, room: ROOM, token, expires_at: expires })) };
          json(res, 200, session);
          return;
        }
        case 'POST /v1/channels/general/voice/companion': {
          events.push('http POST companion');
          const answer: VoiceCompanionResponseV1 = { ...companionTemplate, livekit_url: conchd.livekit, identity: `${IDENTITY}.screen`, room: ROOM, expires_at: expires };
          json(res, 200, answer);
          return;
        }
        case 'PUT /v1/channels/general/voice/companion/audio': {
          events.push('http PUT audio');
          helperAt.set('PUT', helperState());
          consents.push(sent);
          // conchd records the consent, then lets the participant publish.
          await setPublishSources(WITH_SHARE);
          const recorded: VoiceCompanionAudioV1 = { scope: 'system_excluding_call', publisher: 'person', started_at: '2026-11-16T12:00:00Z' };
          events.push('http PUT audio recorded');
          json(res, 200, recorded);
          return;
        }
        case 'DELETE /v1/channels/general/voice/companion/audio':
          events.push('http DELETE audio');
          helperAt.set('DELETE', helperState());
          await setPublishSources(MIC_ONLY);
          res.writeHead(204).end();
          return;
        case 'DELETE /v1/channels/general/voice/companion':
          events.push('http DELETE companion');
          res.writeHead(204).end();
          return;
        default:
          json(res, 503, refusal);
      }
    })().catch(() => {
      res.writeHead(500).end();
    });
  });
  await new Promise<void>((r) => server.listen(0, '127.0.0.1', r));
  return {
    origin: `http://127.0.0.1:${String((server.address() as AddressInfo).port)}`,
    ...conchd,
    get livekit() {
      return conchd.livekit;
    },
    set livekit(url: string) {
      conchd.livekit = url;
    },
    close: () =>
      new Promise<void>((r) => {
        server.closeAllConnections();
        server.close(() => {
          r();
        });
      }),
  };
}

interface Rig {
  readonly sup: Supervisor;
  readonly voice: VoiceService;
  readonly companion: Companion;
  readonly sound: ShareSound | null;
  readonly conchd: Conchd;
  readonly events: Events;
  readonly logs: string[];
  plain(): NativeVoiceView;
  close(): Promise<void>;
}

// rig makes main's side as app.ts does, with the one switch read by the same
// function: the real Supervisor (the versions it offers follow the switch), a
// VoiceService, the real API client on a Session, a Companion, and (switch on)
// a ShareSound. Only the helper's audio options and test source are added, and
// each command main writes to the helper is noted in the order conchd saw
// things.
async function rig(s: Scratch, env: NodeJS.ProcessEnv, livekit: string): Promise<Rig> {
  const logs: string[] = [];
  const switchOn = linuxShareAudioEnabled(env, 'linux');
  let current: () => NativeVoiceView = () => ({ status: 'idle', generation: 0, channel: null, retryable: false });
  const conchd = await stubConchd(() => current().helper?.shareAudio?.state ?? 'none');
  conchd.livekit = livekit;
  const events = conchd.events;
  const sup = new Supervisor({
    resources: s.resources,
    env: { HOME: s.root, XDG_RUNTIME_DIR: s.run, PIPEWIRE_REMOTE: 'conch-voice-test-no-such-socket', RUST_LOG: 'info' },
    log: (line) => logs.push(line),
    spawn: (command, args, options) => defaultSpawn(command, args, options),
    ...(switchOn ? { versions: OWNER_VERSIONS_SHARE_AUDIO } : {}),
  });
  // The real helper's options of this test: no audio device, a tone as the
  // screen's sound. What main writes to its standard input is noted.
  const start = sup.start.bind(sup);
  sup.start = (req) => start({ ...req, extraArgs: [...(req.extraArgs ?? []), '--mic', 'none', '--sink', 'count', '--test-share-source', 'tone:440'] });
  const setShareAudio = sup.setShareAudio.bind(sup);
  sup.setShareAudio = async (want) => {
    events.push(want.capturing ? 'helper capture on' : 'helper capture off');
    const result = await setShareAudio(want);
    events.push(want.capturing ? `helper capture on: ${JSON.stringify(result)}` : `helper capture off: ${JSON.stringify(result)}`);
    return result;
  };
  const setShareAudioMute = sup.setShareAudioMute.bind(sup);
  sup.setShareAudioMute = async (muted) => {
    events.push(`helper share-mute ${String(muted)}`);
    return setShareAudioMute(muted);
  };

  const session = new Session();
  session.setOrigin(conchd.origin);
  session.adopt(FAKE_TOKEN);
  const client = new Client({ fetch: globalThis.fetch, session });
  const identity: VoiceIdentity = {
    profileId: 'it1',
    configDir: s.config,
    principalId: whoami.id,
    credential: () => ({ origin: conchd.origin, token: FAKE_TOKEN }),
    current: () => true,
  };
  const voice = new VoiceService({
    supervisor: sup,
    oneShot: new OneShot({ resources: s.resources, env: { HOME: s.root, XDG_RUNTIME_DIR: s.run }, log: () => undefined }),
    identity: () => Promise.resolve(identity),
    log: (line) => logs.push(line),
    locked: () => false,
  });
  current = () => voice.plain();
  let sound: ShareSound | null = null;
  const companion = new Companion({
    voice,
    locked: () => false,
    client: () => client,
    document: () => DOC,
    tell: () => undefined,
    log: (line) => logs.push(line),
    soundEnded: (held, reason) => sound?.ended(held, reason) ?? Promise.resolve(),
  });
  if (switchOn) {
    const made = new ShareSound({
      voice,
      companion,
      choice: () => 'system_excluding_call',
      setChoice: () => Promise.resolve(true),
      applicationName: registeredApplicationName('Conch'),
      log: (line) => logs.push(line),
    });
    sound = made;
    voice.attachShareSound({ view: () => made.view(), leaving: () => made.leaving() });
  }
  return {
    sup,
    voice,
    companion,
    sound,
    conchd,
    events,
    logs,
    plain: () => voice.plain(),
    close: async () => {
      try {
        await voice.leave();
        await sup.stop();
      } finally {
        sound?.close();
        voice.close();
        await conchd.close();
      }
    },
  };
}

// --------------------------------------------------------------- the tests

describe.runIf(have)('the share sound with the real conch-voice: negotiation', () => {
  let s: Scratch;
  let r: Rig | null;
  beforeEach(() => {
    s = scratch();
    copyFileSync(binary, join(s.resources, HELPER_NAME));
    chmodSync(join(s.resources, HELPER_NAME), 0o755);
    r = null;
  });
  afterEach(async () => {
    try {
      await r?.close();
    } finally {
      s.cleanup();
    }
  });

  beforeAll(() => {
    execFileSync(binary, ['--version'], { stdio: 'ignore' });
  });

  const joined = async (env: NodeJS.ProcessEnv): Promise<NativeVoiceView> => {
    r = await rig(s, env, '');
    const view = await r.voice.join({ channel: 'general', muted: false, deafened: false });
    expect(view.status, JSON.stringify({ view, logs: r.logs })).toBe('running');
    return view;
  };

  it(`with ${SHARE_AUDIO_FLAG}=1 main negotiates v3 with the real helper, and the helper says the sound is off`, async () => {
    const view = await joined({ [SHARE_AUDIO_FLAG]: '1' });
    expect(view.helper?.version).toBe(3);
    await until(() => r?.plain().helper?.shareAudio !== undefined, 15_000, 'the helper to report the share sound');
    expect(r?.plain().helper?.shareAudio).toEqual({ state: 'off', muted: false, sources: 0 });
    expect(r?.voice.state().shareSound).toEqual({ choice: 'system_excluding_call' });
  });

  it('with the switch off main negotiates v2 as before, and the view says nothing of sound', async () => {
    const view = await joined({});
    expect(view.helper?.version).toBe(2);
    expect(r?.plain().helper?.shareAudio).toBeUndefined();
    expect(r?.voice.state().shareSound).toBeUndefined();
    expect(r?.sound).toBeNull();
  });

  it('with the switch set to anything but 1 it is off', async () => {
    const view = await joined({ [SHARE_AUDIO_FLAG]: 'true' });
    expect(view.helper?.version).toBe(2);
  });
});

const lkReason = have ? await livekitUnavailable() : 'the helper binary is not here';
if (lkReason !== null) {
  const why = `share-audio-helper integration (LiveKit part): SKIPPED: ${lkReason}`;
  if (ci && have && !/Docker is not available/.test(lkReason)) throw new Error(why);
  console.warn(why);
}

describe.runIf(have && lkReason === null)('the share sound with the real conch-voice and a real LiveKit', () => {
  let s: Scratch;
  let r: Rig;

  beforeAll(async () => {
    await startLiveKit();
  }, 60_000);
  afterAll(() => {
    stopLiveKit();
  });
  beforeEach(async () => {
    s = scratch();
    copyFileSync(binary, join(s.resources, HELPER_NAME));
    chmodSync(join(s.resources, HELPER_NAME), 0o755);
    r = await rig(s, { [SHARE_AUDIO_FLAG]: '1' }, LK_URL);
  });
  afterEach(async () => {
    try {
      await r.close();
    } finally {
      s.cleanup();
    }
  });

  const helper = (): NativeVoiceView['helper'] => r.plain().helper;
  const audio = (): string => helper()?.shareAudio?.state ?? 'none';

  // Joins voice until the helper is connected, takes the companion and has
  // main grant the capture of a pick: a live share, as display-capture does.
  async function live(): Promise<void> {
    const view = await r.voice.join({ channel: 'general', muted: false, deafened: false });
    expect(view.status, JSON.stringify({ view, logs: r.logs })).toBe('running');
    expect(view.helper?.version).toBe(3);
    await until(() => helper()?.phase === 'connected', 30_000, 'the helper to be connected to LiveKit');
    expect(await r.companion.grant({ channel: 'general' }), JSON.stringify(r.logs)).toMatchObject({ ok: true });
    r.sound?.captureGranted(DOC);
    r.events.length = 0;
  }

  it('the consent comes before the capture command, the sound is on, the track is on the server; the mute is the share sound mute; the end is stop, then DELETE, and the track is gone', async () => {
    expect(r.sound).not.toBeNull();
    await live();
    expect(await serverSources()).not.toContain('SCREEN_SHARE_AUDIO');
    expect(audio()).toBe('off');

    // 2. start order: PUT first, with the one body main builds; the helper was
    // still off when the PUT reached conchd; only then the command.
    const started = await r.sound?.start(DOC);
    expect(started, JSON.stringify({ started, logs: r.logs, helper: helper() })).toMatchObject({ ok: true });
    expect(r.conchd.consents).toEqual([{ scope: 'system_excluding_call', publisher: 'person' }]);
    expect(r.conchd.consents[0]).toEqual({ scope: consentRequestFixture.scope, publisher: 'person' });
    expect(r.conchd.helperAt.get('PUT')).toBe('off');
    const order = r.events.filter((e) => e === 'http PUT audio recorded' || e === 'helper capture on');
    expect(order).toEqual(['http PUT audio recorded', 'helper capture on']);

    // The helper's state is what the desktop reports: on, from the tone, with
    // the source counted, and it is the helper's word, not main's.
    await until(() => audio() === 'on', 15_000, 'the helper to say the share sound is on');
    expect(helper()?.shareAudio).toMatchObject({ state: 'on', muted: false });
    expect(helper()?.shareAudio?.sources ?? 0).toBeGreaterThan(0);
    expect(r.voice.state().shareSound).toEqual({ choice: 'system_excluding_call' });

    // The track is on the real server.
    const deadline = Date.now() + 15_000;
    while (!(await serverSources()).includes('SCREEN_SHARE_AUDIO')) {
      if (Date.now() > deadline) throw new Error(`the server never held the ScreenshareAudio track: ${JSON.stringify({ logs: r.logs, helper: helper() })}`);
      await new Promise((resolve) => setTimeout(resolve, 100));
    }

    // 3. the mute: it reaches the helper and comes back as share_audio_muted,
    // and the microphone's mute is not touched.
    const micBefore = helper()?.muted;
    expect(await r.sound?.mute(true, DOC)).toEqual({ ok: true });
    await until(() => helper()?.shareAudio?.muted === true, 10_000, 'the share sound mute to come back');
    expect(helper()?.shareAudio).toMatchObject({ state: 'on', muted: true });
    expect(helper()?.muted).toBe(micBefore);
    expect(helper()?.deafened).toBe(false);
    expect(r.events).toContain('helper share-mute true');
    expect(r.events.some((e) => e.startsWith('helper mic'))).toBe(false);
    expect(await r.sound?.mute(false, DOC)).toEqual({ ok: true });
    await until(() => helper()?.shareAudio?.muted === false, 10_000, 'the unmute to come back');
    expect(helper()?.muted).toBe(micBefore);

    // 4. the end: the page says its share ended (with the token the start
    // returned): stop, then DELETE; the helper says off; the track is gone.
    const token = started?.ok === true ? started.share : -1;
    r.events.length = 0;
    expect(await r.sound?.end(token)).toBe(true);
    expect(r.events.filter((e) => e.startsWith('helper capture') || e.startsWith('http DELETE audio'))).toEqual(['helper capture off', 'helper capture off: {"ok":true}', 'http DELETE audio']);
    // The DELETE reached conchd after the helper had already said off.
    expect(r.conchd.helperAt.get('DELETE')).toBe('off');
    expect(audio()).toBe('off');
    expect(helper()?.shareAudio).toMatchObject({ state: 'off', sources: 0 });
    const gone = Date.now() + 15_000;
    while ((await serverSources()).includes('SCREEN_SHARE_AUDIO')) {
      if (Date.now() > gone) throw new Error('the server still holds the ScreenshareAudio track after the stop');
      await new Promise((resolve) => setTimeout(resolve, 100));
    }
    expect(r.voice.state().shareSound).toEqual({ choice: 'system_excluding_call' });
    // Nothing of the call's own track or any credential was left in the log.
    expect(JSON.stringify(r.logs)).not.toContain(FAKE_TOKEN);
  }, 90_000);

  it('the share ending (the companion’s end) stops the sound and withdraws, in that order, with no token from the page', async () => {
    await live();
    expect(await r.sound?.start(DOC)).toMatchObject({ ok: true });
    await until(() => audio() === 'on', 15_000, 'on');
    r.events.length = 0;
    expect(await r.sound?.shareEnded(DOC)).toBe(true);
    expect(r.events.filter((e) => e.startsWith('helper capture off') || e === 'http DELETE audio')).toEqual(['helper capture off', 'helper capture off: {"ok":true}', 'http DELETE audio']);
    expect(audio()).toBe('off');
  }, 60_000);

  it('leaving voice with the sound on stops it and withdraws before the helper is told to leave', async () => {
    await live();
    expect(await r.sound?.start(DOC)).toMatchObject({ ok: true });
    await until(() => audio() === 'on', 15_000, 'on');
    r.events.length = 0;
    await r.voice.leave();
    const stop = r.events.indexOf('helper capture off');
    const del = r.events.indexOf('http DELETE audio');
    expect(stop).toBeGreaterThanOrEqual(0);
    expect(del).toBeGreaterThan(stop);
    expect(r.sup.state).toEqual({ status: 'idle' });
  }, 60_000);
});
