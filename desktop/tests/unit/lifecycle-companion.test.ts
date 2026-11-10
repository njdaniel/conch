// Lock and suspend end a live share for real (issue #1588, D5-02): the real
// Lifecycle drives the real Companion (issue #1579), with a fake conchd and a
// fake voice service standing in for the network and the helper.
import { describe, expect, it } from 'vitest';

import type { Exchange, Send } from '../../main/api/client.ts';
import { Lifecycle } from '../../main/lifecycle.ts';
import { Companion } from '../../main/voice/companion.ts';
import type { CompanionEvent, NativeVoiceView } from '../../../web/src/platform/desktop/bridge.ts';
import type { VoiceCompanionResponseV1 } from '../../../web/src/schema/generated.ts';
import { readFixture } from './voice-support.ts';

const ANSWER = readFixture<VoiceCompanionResponseV1>('voice-companion-response-v1.json');

const view: NativeVoiceView = {
  status: 'running',
  generation: 3,
  channel: 'general',
  retryable: false,
  helper: { version: 2, phase: 'connected', capture: 'on', transmitting: false, muted: false, deafened: false, blocked: [], ptt: 'ready' },
};

const ok = (body?: unknown): Exchange => ({ kind: 'answer', status: body === undefined ? 204 : 200, ok: true, ...(body === undefined ? {} : { body }) });

async function rig() {
  const sent: Send[] = [];
  const told: CompanionEvent[] = [];
  const calls: string[] = [];
  // One client for the whole activation, as main holds it.
  const client = {
    send: (request: Send) => {
      sent.push(request);
      return Promise.resolve(request.op === 'voiceViewerEnd' ? ok() : ok(ANSWER));
    },
  };
  const companion = new Companion({
    voice: { state: () => view, subscribe: () => () => undefined },
    client: () => client,
    document: () => 5,
    tell: (_document, event) => {
      told.push(event);
    },
    log: () => undefined,
    sleep: () => Promise.resolve(),
  });
  const life = new Lifecycle({
    voice: {
      state: () => ({ status: view.status }),
      setMute: (m) => {
        calls.push(`mute ${String(m)}`);
        return Promise.resolve({ ok: true });
      },
      leave: () => {
        calls.push('leave');
        return Promise.resolve();
      },
      setLockMuted: () => undefined,
    },
    companion,
    notifications: { setLocked: () => undefined },
    log: () => undefined,
  });
  life.observe('available');
  expect((await companion.grant({ channel: 'general' })).ok).toBe(true);
  sent.length = 0;
  told.length = 0;
  return { life, companion, sent, told, calls };
}

describe('lock and suspend end a live share through the real companion', () => {
  it.each([
    ['lock', (r: Awaited<ReturnType<typeof rig>>) => r.life.lockSignal('logind', true), ['mute true']],
    ['suspend', (r: Awaited<ReturnType<typeof rig>>) => r.life.suspend(), ['leave']],
  ] as const)('%s tells the page, deletes the viewer grant and forgets the share', async (reason, act, voiceCalls) => {
    const r = await rig();
    expect(r.companion.liveOrigin()).not.toBeNull();
    await act(r);
    expect(r.told).toEqual([{ type: 'end', reason }]);
    expect(r.sent).toEqual([expect.objectContaining({ op: 'voiceViewerEnd', params: { channel: 'general' }, query: 'publish=screen' })]);
    expect(r.companion.liveOrigin()).toBeNull();
    expect(r.calls).toEqual(voiceCalls);
  });

  it('unlock and resume do not bring the share back, and a second lock sends nothing new', async () => {
    const r = await rig();
    await r.life.lockSignal('logind', true);
    r.sent.length = 0;
    r.told.length = 0;
    await r.life.lockSignal('logind', true);
    await r.life.lockSignal('logind', false);
    r.life.resume();
    expect(r.sent).toEqual([]);
    expect(r.told).toEqual([]);
    expect(r.companion.liveOrigin()).toBeNull();
  });
});
