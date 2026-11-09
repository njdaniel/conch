// The screen companion (issue #1579, D4-02 of #772): main's holder of the
// p<id>.screen grant. A fake client and a fake voice service stand in for
// conchd and the helper, except in the canary test, which runs the real
// request executor with a bearer in its session and a fake fetch.
import { describe, expect, it } from 'vitest';

import { Client, type Exchange, type Send } from '../../main/api/client.ts';
import { invocable } from '../../main/api/service.ts';
import { Session } from '../../main/api/session.ts';
import { COMPANION_PASS_MS, COMPANION_REMOVE_TIMEOUT_MS, Companion, type CompanionClient } from '../../main/voice/companion.ts';
import type { CompanionEndReason, CompanionEvent, NativeVoiceHelper, NativeVoiceView } from '../../../web/src/platform/desktop/bridge.ts';
import { operations } from '../../../web/src/platform/operations.ts';
import type { VoiceCompanionResponseV1 } from '../../../web/src/schema/generated.ts';
import { readFixture } from './voice-support.ts';

const ANSWER = readFixture<VoiceCompanionResponseV1>('voice-companion-response-v1.json');

const helper: NativeVoiceHelper = { version: 2, phase: 'connected', capture: 'on', transmitting: false, muted: false, deafened: false, blocked: [], ptt: 'ready' };
const running = (over: Partial<NativeVoiceView> = {}, helperOver: Partial<NativeVoiceHelper> = {}): NativeVoiceView => ({
  status: 'running',
  generation: 3,
  channel: 'general',
  retryable: false,
  helper: { ...helper, ...helperOver },
  ...over,
});
const idle: NativeVoiceView = { status: 'idle', generation: 3, channel: null, retryable: false };

const ok = (body?: unknown): Exchange => ({ kind: 'answer', status: body === undefined ? 204 : 200, ok: true, ...(body === undefined ? {} : { body }) });
const refusal = (status: number, code?: string): Exchange => ({ kind: 'answer', status, ok: false, ...(code === undefined ? {} : { body: { code, message: 'no' } }) });

type Step = ['send', Send] | ['tell', number, CompanionEvent] | ['sleep', number];

interface Rig {
  readonly companion: Companion;
  readonly steps: Step[];
  readonly logs: string[];
  // What the fake conchd answers next, in order; a DELETE answers 204 unless
  // told otherwise.
  readonly answers: (Exchange | Promise<Exchange>)[];
  readonly removals: Exchange[];
  readonly voice: { view: NativeVoiceView; set(view: NativeVoiceView): void };
  readonly document: { value: number };
  client: CompanionClient | null;
  sends(): Send[];
}

function rig(view: NativeVoiceView = running(), share?: () => Promise<'no-portal' | 'not-wayland' | null>): Rig {
  const steps: Step[] = [];
  const logs: string[] = [];
  const answers: (Exchange | Promise<Exchange>)[] = [];
  const removals: Exchange[] = [];
  const listeners = new Set<(view: NativeVoiceView) => void>();
  const voice = {
    view,
    set(next: NativeVoiceView) {
      voice.view = next;
      for (const l of [...listeners]) l(next);
    },
  };
  const document = { value: 5 };
  const client: CompanionClient = {
    send: (request) => {
      steps.push(['send', request]);
      if (request.op === 'voiceViewerEnd') return Promise.resolve(removals.shift() ?? ok());
      return Promise.resolve(answers.shift() ?? refusal(500));
    },
  };
  const r: Rig = {
    steps,
    logs,
    answers,
    removals,
    voice,
    document,
    client,
    sends: () => steps.flatMap((s) => (s[0] === 'send' ? [s[1]] : [])),
    companion: new Companion({
      voice: {
        state: () => voice.view,
        subscribe: (l) => {
          listeners.add(l);
          return () => {
            listeners.delete(l);
          };
        },
      },
      client: () => r.client,
      document: () => document.value,
      tell: (doc, event) => {
        steps.push(['tell', doc, event]);
      },
      log: (line) => logs.push(line),
      ...(share === undefined ? {} : { share }),
      sleep: (ms) => {
        steps.push(['sleep', ms]);
        return Promise.resolve();
      },
    }),
  };
  return r;
}

const GRANT = { livekitUrl: ANSWER.livekit_url, token: ANSWER.token, identity: ANSWER.identity, expiresAt: ANSWER.expires_at };
const POST: Send = { op: 'voiceViewer', params: { channel: 'general' }, body: { publish: 'screen', subscribe: true } };
const DELETE: Send = { op: 'voiceViewerEnd', params: { channel: 'general' }, query: 'publish=screen', timeoutMs: COMPANION_REMOVE_TIMEOUT_MS };

async function live(r: Rig): Promise<void> {
  r.answers.push(ok(ANSWER));
  expect(await r.companion.grant({ channel: 'general' })).toEqual({ ok: true, grant: GRANT });
  r.steps.length = 0;
}

describe('the companion’s grant', () => {
  it.each<[string, NativeVoiceView]>([
    ['voice is idle', idle],
    ['voice is starting', { status: 'starting', generation: 3, channel: 'general', retryable: false }],
    ['voice is in another channel (browsing never gets a grant)', running({ channel: 'random' })],
    ['the helper is connecting', running({}, { phase: 'connecting' })],
    ['the helper is reconnecting', running({}, { phase: 'reconnecting' })],
    ['the helper is stopping', running({}, { phase: 'stopping' })],
    ['voice failed', { status: 'failed', generation: 3, channel: 'general', problem: 'exited', retryable: true }],
    ['voice ended', { ...running(), status: 'ended' }],
  ])('is refused, and calls nothing, when %s', async (_name, view) => {
    const r = rig(view);
    expect(await r.companion.grant({ channel: 'general' })).toEqual({ ok: false, error: 'not-in-voice' });
    expect(r.steps).toEqual([]);
    expect(r.companion.liveOrigin()).toBeNull();
  });

  it('is refused as failed, and calls nothing, with no activation in force', async () => {
    const r = rig();
    r.client = null;
    expect(await r.companion.grant({ channel: 'general' })).toEqual({ ok: false, error: 'failed' });
    expect(r.steps).toEqual([]);
  });

  it('asks conchd once, as B2 (publish screen, subscribe), and hands back exactly the grant', async () => {
    const r = rig();
    r.answers.push(ok(ANSWER));
    const result = await r.companion.grant({ channel: 'general' });
    expect(r.sends()).toEqual([POST]);
    expect(result).toEqual({ ok: true, grant: GRANT });
    if (!result.ok) throw new Error('no grant');
    expect(Object.keys(result).sort()).toEqual(['grant', 'ok']);
    expect(Object.keys(result.grant).sort()).toEqual(['expiresAt', 'identity', 'livekitUrl', 'token']);
    // Never the room's name, the publish set or anything else of the answer.
    expect(JSON.stringify(result)).not.toContain(ANSWER.room);
    expect(r.companion.liveOrigin()).toBe('wss://voice.example');
    expect(r.logs).toEqual(['companion: issued']);
  });

  it('asks once more after one pass when conchd has not seen p<id> yet, and then gives the grant', async () => {
    const r = rig();
    r.answers.push(refusal(409, 'voice_no_session'), ok(ANSWER));
    expect(await r.companion.grant({ channel: 'general' })).toEqual({ ok: true, grant: GRANT });
    expect(r.steps).toEqual([['send', POST], ['sleep', COMPANION_PASS_MS], ['send', POST]]);
    expect(COMPANION_PASS_MS).toBe(1_500);
  });

  it('answers not-yet after the second voice_no_session, and asks no third time', async () => {
    const r = rig();
    r.answers.push(refusal(409, 'voice_no_session'), refusal(409, 'voice_no_session'), ok(ANSWER));
    expect(await r.companion.grant({ channel: 'general' })).toEqual({ ok: false, error: 'not-yet' });
    expect(r.steps).toEqual([['send', POST], ['sleep', COMPANION_PASS_MS], ['send', POST]]);
    expect(r.companion.liveOrigin()).toBeNull();
  });

  it('does not ask again when voice ends during the pass', async () => {
    const r = rig();
    r.answers.push(refusal(409, 'voice_no_session'), ok(ANSWER));
    const result = r.companion.grant({ channel: 'general' });
    r.voice.set(idle);
    expect(await result).toEqual({ ok: false, error: 'ended' });
    expect(r.sends()).toEqual([POST]);
  });

  it('answers elsewhere for voice_companion_busy, once', async () => {
    const r = rig();
    r.answers.push(refusal(409, 'voice_companion_busy'));
    expect(await r.companion.grant({ channel: 'general' })).toEqual({ ok: false, error: 'elsewhere' });
    expect(r.sends()).toEqual([POST]);
  });

  it.each<[string, Exchange]>([
    ['403', refusal(403, 'voice_not_allowed')],
    ['404', refusal(404, 'channel_not_found')],
    ['409 archived', refusal(409, 'channel_archived')],
    ['503', refusal(503, 'voice_unavailable')],
    ['500 with no document', refusal(500)],
    ['no answer', { kind: 'none' }],
    ['a stale answer', { kind: 'none', reason: 'stale' }],
    ['a 401', { kind: 'unauthorized' }],
  ])('answers one typed error for %s, and never retries it', async (_name, answer) => {
    const r = rig();
    r.answers.push(answer, ok(ANSWER));
    expect(await r.companion.grant({ channel: 'general' })).toEqual({ ok: false, error: 'failed' });
    expect(r.sends()).toEqual([POST]);
    expect(r.companion.liveOrigin()).toBeNull();
  });

  it.each([
    ['a viewer’s grant', { ...ANSWER, publish: 'none' }],
    ['a publish-only grant', { ...ANSWER, subscribe: false }],
    ['another identity', { ...ANSWER, identity: 'p7' }],
    ['a viewer identity', { ...ANSWER, identity: 'p7.view' }],
    ['an empty token', { ...ANSWER, token: '' }],
    ['a URL that is not LiveKit’s', { ...ANSWER, livekit_url: 'ftp://voice.example' }],
    ['plain ws to a remote host', { ...ANSWER, livekit_url: 'ws://voice.example' }],
    ['a URL with user information', { ...ANSWER, livekit_url: 'wss://user:pw@voice.example' }],
    ['no URL', { ...ANSWER, livekit_url: 'not a url' }],
    ['a missing field', { ...ANSWER, expires_at: undefined }],
  ])('refuses %s as failed and removes the companion conchd issued', async (_name, body) => {
    const r = rig();
    r.answers.push(ok(body));
    expect(await r.companion.grant({ channel: 'general' })).toEqual({ ok: false, error: 'failed' });
    expect(r.sends()).toEqual([POST, DELETE]);
    expect(r.companion.liveOrigin()).toBeNull();
  });

  it('says beside the grant why Share is unavailable (#1580), and still gives the grant', async () => {
    for (const why of ['no-portal', 'not-wayland'] as const) {
      const r = rig(running(), () => Promise.resolve(why));
      r.answers.push(ok(ANSWER));
      expect(await r.companion.grant({ channel: 'general' })).toEqual({ ok: true, grant: GRANT, shareUnavailable: why });
      expect(r.companion.liveOrigin()).toBe('wss://voice.example');
    }
  });

  it('says nothing of Share when the portal answers, or when the probe throws it is "no-portal"', async () => {
    const yes = rig(running(), () => Promise.resolve(null));
    yes.answers.push(ok(ANSWER));
    expect(await yes.companion.grant({ channel: 'general' })).toEqual({ ok: true, grant: GRANT });
    const boom = rig(running(), () => Promise.reject(new Error('boom')));
    boom.answers.push(ok(ANSWER));
    expect(await boom.companion.grant({ channel: 'general' })).toEqual({ ok: true, grant: GRANT, shareUnavailable: 'no-portal' });
  });

  it('accepts plain ws to loopback, for development', async () => {
    const r = rig();
    r.answers.push(ok({ ...ANSWER, livekit_url: 'ws://127.0.0.1:7880' }));
    expect((await r.companion.grant({ channel: 'general' })).ok).toBe(true);
    expect(r.companion.liveOrigin()).toBe('ws://127.0.0.1:7880');
  });

  it('holds at most one: a second request while one is live is refused as conchd would, and calls nothing', async () => {
    const r = rig();
    await live(r);
    expect(await r.companion.grant({ channel: 'general' })).toEqual({ ok: false, error: 'elsewhere' });
    expect(r.steps).toEqual([]);
  });

  it('holds at most one: a second request while the first is in flight is refused, and calls nothing', async () => {
    const r = rig();
    let answer: (exchange: Exchange) => void = () => undefined;
    r.answers.push(new Promise<Exchange>((resolve) => (answer = resolve)));
    const first = r.companion.grant({ channel: 'general' });
    expect(await r.companion.grant({ channel: 'general' })).toEqual({ ok: false, error: 'elsewhere' });
    answer(ok(ANSWER));
    expect(await first).toEqual({ ok: true, grant: GRANT });
    expect(r.sends()).toEqual([POST]);
  });

  it('removes a grant that an end overtook while it was asked for, and hands it to nobody', async () => {
    const r = rig();
    let answer: (exchange: Exchange) => void = () => undefined;
    r.answers.push(new Promise<Exchange>((resolve) => (answer = resolve)));
    const first = r.companion.grant({ channel: 'general' });
    await r.companion.end('lock');
    answer(ok(ANSWER));
    expect(await first).toEqual({ ok: false, error: 'ended' });
    expect(r.sends()).toEqual([POST, DELETE]);
    expect(r.companion.liveOrigin()).toBeNull();
    // Nothing was live when the end came: nobody was told.
    expect(r.steps.filter((s) => s[0] === 'tell')).toEqual([]);
  });

  it('removes a grant whose voice ended while it was asked for', async () => {
    const r = rig();
    let answer: (exchange: Exchange) => void = () => undefined;
    r.answers.push(new Promise<Exchange>((resolve) => (answer = resolve)));
    const first = r.companion.grant({ channel: 'general' });
    r.voice.set(running({ generation: 4 }));
    answer(ok(ANSWER));
    expect(await first).toEqual({ ok: false, error: 'ended' });
    expect(r.sends()).toEqual([POST, DELETE]);
  });

  it('gives a new grant only on a new request: nothing is re-issued after an end', async () => {
    const r = rig();
    await live(r);
    r.voice.set(running({}, { phase: 'reconnecting', reason: 'room_deleted' }));
    r.voice.set(running());
    await Promise.resolve();
    expect(r.sends()).toEqual([DELETE]);
    expect(r.companion.liveOrigin()).toBeNull();
  });

  // The 'share' media hook (app.ts) clears the client before it ends the
  // companion, so a page that asks right after (a switch's or a sign-out's
  // click is still armed) gets nothing under the identity that is ending.
  it('refuses a grant asked for after the teardown hook’s end, with voice still running, and sends nothing', async () => {
    const r = rig();
    await live(r);
    r.client = null;
    expect(await r.companion.end('switch')).toBe(true);
    r.steps.length = 0;
    // The helper has not been stopped yet: the view is the same.
    expect(r.voice.view).toEqual(running());
    expect(await r.companion.grant({ channel: 'general' })).toEqual({ ok: false, error: 'failed' });
    expect(r.steps).toEqual([]);
    expect(r.companion.liveOrigin()).toBeNull();
  });

  it('removes as unused a grant in flight when the teardown hook clears the client, and holds nothing', async () => {
    const r = rig();
    let answer: (exchange: Exchange) => void = () => undefined;
    r.answers.push(new Promise<Exchange>((resolve) => (answer = resolve)));
    const first = r.companion.grant({ channel: 'general' });
    // The client alone has changed (no end yet): that is enough.
    r.client = null;
    answer(ok(ANSWER));
    expect(await first).toEqual({ ok: false, error: 'ended' });
    // The DELETE goes with the client that asked.
    expect(r.sends()).toEqual([POST, DELETE]);
    expect(r.logs).not.toContain('companion: issued');
    expect(r.companion.liveOrigin()).toBeNull();
  });

  it('ends a companion of an activation no longer in force before asking for the next', async () => {
    const r = rig();
    await live(r);
    const old = r.steps;
    const next: Step[] = [];
    r.client = {
      send: (request) => {
        next.push(['send', request]);
        return Promise.resolve(ok(ANSWER));
      },
    };
    expect((await r.companion.grant({ channel: 'general' })).ok).toBe(true);
    expect(old).toEqual([['tell', 5, { type: 'end', reason: 'switch' }], ['send', DELETE]]);
    expect(next).toEqual([['send', POST]]);
  });
});

describe('the companion’s end', () => {
  it('companionEnd sends the DELETE and forgets the record, and tells the page nothing (it asked)', async () => {
    const r = rig();
    await live(r);
    expect(await r.companion.stop()).toBe(true);
    expect(r.steps).toEqual([['send', DELETE]]);
    expect(r.companion.liveOrigin()).toBeNull();
    expect(await r.companion.stop()).toBe(false);
    expect(r.steps).toHaveLength(1);
  });

  const reasons: CompanionEndReason[] = ['leave', 'helper', 'renewal', 'sign-out', 'switch', 'quit', 'crash', 'reload', 'lock', 'suspend'];

  it.each(reasons)('ends on %s: tells the page that was given the grant, then sends the DELETE', async (reason) => {
    const r = rig();
    await live(r);
    // A later document is not the one told: the tell names the grant's.
    r.document.value = 9;
    expect(await r.companion.end(reason)).toBe(true);
    expect(r.steps).toEqual([['tell', 5, { type: 'end', reason }], ['send', DELETE]]);
    expect(r.companion.liveOrigin()).toBeNull();
    // Nothing is live: a second end tells nobody and calls nothing.
    expect(await r.companion.end(reason)).toBe(false);
    expect(r.steps).toHaveLength(2);
  });

  it('still sends the DELETE when the page cannot be told', async () => {
    const steps: Send[] = [];
    const client: CompanionClient = {
      send: (request) => {
        steps.push(request);
        return Promise.resolve(request.op === 'voiceViewer' ? ok(ANSWER) : ok());
      },
    };
    const companion = new Companion({
      voice: { state: () => running(), subscribe: () => () => undefined },
      client: () => client,
      document: () => 1,
      tell: () => {
        throw new Error('window gone');
      },
      log: () => undefined,
    });
    expect((await companion.grant({ channel: 'general' })).ok).toBe(true);
    expect(await companion.end('crash')).toBe(true);
    expect(steps).toEqual([POST, DELETE]);
  });

  it.each<[string, NativeVoiceView, CompanionEndReason]>([
    ['the view leaves running (stopping)', { ...running(), status: 'stopping' }, 'helper'],
    ['the view goes idle', idle, 'helper'],
    ['the helper is lost', { status: 'lost', generation: 3, channel: 'general', problem: 'exited', retryable: true }, 'helper'],
    ['a new helper generation', running({ generation: 4 }), 'helper'],
    ['another channel', running({ channel: 'random' }), 'helper'],
    ['the helper is stopping', running({}, { phase: 'stopping', reason: 'left' }), 'helper'],
    ['the helper lost its connection', running({}, { phase: 'reconnecting', reason: 'connection_lost' }), 'helper'],
    ['the helper connects anew', running({}, { phase: 'connecting' }), 'helper'],
    ['conchd renewed the room', running({}, { phase: 'reconnecting', reason: 'room_deleted' }), 'renewal'],
  ])('ends on its own when %s', async (_name, view, reason) => {
    const r = rig();
    await live(r);
    r.voice.set(view);
    await Promise.resolve();
    expect(r.steps).toEqual([['tell', 5, { type: 'end', reason }], ['send', DELETE]]);
    expect(r.companion.liveOrigin()).toBeNull();
  });

  it('stays through the SDK’s own resume of the helper’s connection, and through mute and deafen', async () => {
    const r = rig();
    await live(r);
    r.voice.set(running({}, { phase: 'reconnecting' }));
    r.voice.set(running({}, { muted: true, deafened: true }));
    r.voice.set(running());
    await Promise.resolve();
    expect(r.steps).toEqual([]);
    expect(r.companion.liveOrigin()).toBe('wss://voice.example');
  });

  it('logs a failed DELETE by its reason alone, and does not send it again', async () => {
    const r = rig();
    await live(r);
    r.logs.length = 0;
    r.removals.push(refusal(503, 'voice_unavailable'));
    await r.companion.end('leave');
    expect(r.sends()).toEqual([DELETE]);
    expect(r.logs).toEqual(['companion: the removal after leave was not confirmed']);
  });

  it('survives a client that throws, and logs it by reason', async () => {
    const r = rig();
    await live(r);
    r.logs.length = 0;
    const held = r.client;
    if (held === null) throw new Error('no client');
    const send = held.send.bind(held);
    held.send = (request) => (request.op === 'voiceViewerEnd' ? Promise.reject(new Error('boom')) : send(request));
    expect(await r.companion.end('quit')).toBe(true);
    expect(r.logs).toEqual(['companion: the removal after quit was not confirmed']);
  });
});

describe('the bearer never leaves main (canary)', () => {
  const BEARER = 'conch_BEARER_canary_companion_do_not_print';

  it('hands back the grant alone, and no return value or log line carries the bearer or the room', async () => {
    const session = new Session();
    session.setOrigin('https://conch.example');
    session.adopt(BEARER);
    const seen: { url: string; method: string; authorization: string | null; body: string | null }[] = [];
    const fetch: typeof globalThis.fetch = (input, init) => {
      const headers = new Headers(init?.headers);
      seen.push({ url: typeof input === 'string' ? input : input instanceof URL ? input.href : input.url, method: init?.method ?? 'GET', authorization: headers.get('authorization'), body: typeof init?.body === 'string' ? init.body : null });
      return Promise.resolve(
        init?.method === 'DELETE' ? new Response(null, { status: 204 }) : new Response(JSON.stringify(ANSWER), { status: 200, headers: { 'Content-Type': 'application/json' } }),
      );
    };
    const client = new Client({ fetch, session });
    const logs: string[] = [];
    const told: unknown[] = [];
    const companion = new Companion({
      voice: { state: () => running(), subscribe: () => () => undefined },
      client: () => client,
      document: () => 1,
      tell: (_doc, event) => told.push(event),
      log: (line) => logs.push(line),
    });
    const result = await companion.grant({ channel: 'general' });
    const ended = await companion.end('leave');
    const stopped = await companion.stop();

    // The bearer was there, and went to conchd alone.
    expect(seen.map((s) => [s.method, s.url, s.authorization])).toEqual([
      ['POST', 'https://conch.example/v1/channels/general/voice/companion', `Bearer ${BEARER}`],
      ['DELETE', 'https://conch.example/v1/channels/general/voice/companion?publish=screen', `Bearer ${BEARER}`],
    ]);
    expect(JSON.parse(seen[0]?.body ?? 'null')).toEqual({ publish: 'screen', subscribe: true });

    expect(result).toEqual({ ok: true, grant: GRANT });
    if (!result.ok) throw new Error('no grant');
    expect(Object.keys(result.grant).sort()).toEqual(['expiresAt', 'identity', 'livekitUrl', 'token']);
    const everything = JSON.stringify([result, ended, stopped, told, companion.liveOrigin(), logs]);
    expect(everything).not.toContain(BEARER);
    expect(everything).not.toContain(ANSWER.room);
    for (const line of logs) {
      expect(line).not.toContain(BEARER);
      expect(line).not.toContain(ANSWER.token);
      expect(line).not.toContain('general');
      expect(line).not.toContain('voice.example');
    }
  });
});

describe('the route’s entry in the operation table', () => {
  it('stays renderer: false, so the page can never ask for a grant itself, nor end main’s companion behind its back', () => {
    expect(operations.voiceViewer.renderer).toBe(false);
    expect(invocable('voiceViewer')).toBe(false);
    // A DELETE from the page would leave main's record (and liveOrigin) in
    // place for a companion conchd had removed (security review of #1604).
    expect(operations.voiceViewerEnd.renderer).toBe(false);
    expect(invocable('voiceViewerEnd')).toBe(false);
    expect(operations.voiceViewer.method).toBe('POST');
    expect(operations.voiceViewer.path).toBe('/v1/channels/{channel}/voice/companion');
    expect(operations.voiceViewerEnd.method).toBe('DELETE');
    expect(operations.voiceViewerEnd.path).toBe('/v1/channels/{channel}/voice/companion');
  });
});
