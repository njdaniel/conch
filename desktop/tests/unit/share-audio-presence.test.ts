// The share's sound ends when conchd's presence shows this person's own share
// ended (issue #1876; docs/design/linux-share-audio.md §10). Main reads the
// presence itself, with the companion's client, while a capture or a sound
// record stands: never the page's feed. The fake conchd answers each read
// with the next queued answer; with none queued a read fails and says nothing.
import { describe, expect, it } from 'vitest';

import type { VoiceParticipantV2, VoicePresenceV2 } from '../../../web/src/schema/generated.ts';
import { DOC, STARTED, ok, refusal, rig, settle, type Rig } from './share-audio.support.ts';

const SELF = 7; // the principal of p7.screen, the fixture grant's identity
const OTHER = 9;
const EARLIER = '2026-11-16T11:55:00Z';
const LATER = '2026-11-16T12:05:00Z';

const joined = '2026-11-16T11:00:00Z';
const sharing = (principal: number, startedAt?: string): VoiceParticipantV2 => ({
  principal_id: principal,
  can_publish: true,
  transmitting: false,
  joined_at: joined,
  sharing: true,
  ...(startedAt === undefined ? {} : { share_started_at: startedAt }),
});
const idle = (principal: number): VoiceParticipantV2 => ({ principal_id: principal, can_publish: true, transmitting: false, joined_at: joined });

function doc(...participants: VoiceParticipantV2[]): VoicePresenceV2 {
  return { schema: 'conch.voice_presence.v2', channel_id: 1, configured: true, available: true, rooms: [{ participants }] };
}
const unavailable: VoicePresenceV2 = { schema: 'conch.voice_presence.v2', channel_id: 1, configured: true, available: false, rooms: [] };
const endedOf = (startedAt: string, d: VoicePresenceV2 = doc(idle(SELF))): VoicePresenceV2 => ({
  ...d,
  share_ended: { reason: 'operator', started_at: startedAt, ended_at: '2026-11-16T12:10:00Z' },
});

const make = (): Rig => rig({ presencePollMs: 1 });

// read queues presence answers and waits until main has read them all.
async function read(r: Rig, ...answers: VoicePresenceV2[]): Promise<void> {
  r.presence.push(...answers.map((d) => ok(d)));
  for (let i = 0; i < 100 && r.presence.length > 0; i++) await settle();
  await settle();
}

async function sounding(r: Rig): Promise<void> {
  await r.live();
  expect(await r.sound?.start(DOC)).toMatchObject({ ok: true });
  r.events.length = 0;
  r.sends.length = 0;
}

const reads = (r: Rig): number => r.sends.filter((s) => s.op === 'voicePresence').length;

describe('presence ends the share’s sound', () => {
  it('main reads the share’s channel’s v2 presence itself, with the companion’s client', async () => {
    const r = make();
    await sounding(r);
    await read(r, doc(sharing(SELF, STARTED)));
    const sent = r.sends.find((s) => s.op === 'voicePresence');
    expect(sent).toMatchObject({ op: 'voicePresence', params: { channel: 'general' }, query: 'schema=conch.voice_presence.v2' });
    expect(r.events).toEqual(['http:GET presence']);
  });

  it('the person’s own share ends in presence while the page says nothing: stop, then DELETE, capture cleared', async () => {
    const r = make();
    await sounding(r);
    await read(r, doc(sharing(SELF, STARTED), idle(OTHER)));
    await read(r, doc(idle(SELF), idle(OTHER)));
    expect(r.events.filter((e) => e !== 'http:GET presence')).toEqual(['helper:capture off', 'http:DELETE audio']);
    expect(await r.sound?.start(DOC)).toEqual({ ok: false, error: 'no-share' });
    // Nothing stands, so main stops reading.
    const before = reads(r);
    await settle();
    await settle();
    expect(reads(r)).toBe(before);
  });

  it('conchd’s share_ended naming this share’s start ends it, even when presence never showed it', async () => {
    const r = make();
    await sounding(r);
    await read(r, endedOf(STARTED));
    expect(r.events.filter((e) => e !== 'http:GET presence')).toEqual(['helper:capture off', 'http:DELETE audio']);
  });

  it('a share_ended of an earlier share ends nothing', async () => {
    const r = make();
    await sounding(r);
    await read(r, endedOf(EARLIER, doc(sharing(SELF, STARTED))));
    expect(r.events.filter((e) => e !== 'http:GET presence')).toEqual([]);
  });

  it('a newer share of the same person ends this one’s sound', async () => {
    const r = make();
    await sounding(r);
    await read(r, doc(sharing(SELF, LATER)));
    expect(r.events.filter((e) => e !== 'http:GET presence')).toEqual(['helper:capture off', 'http:DELETE audio']);
  });

  it('LiveKit unreachable (available false) and a hidden room say nothing: the live share keeps its sound', async () => {
    const r = make();
    await sounding(r);
    await read(r, doc(sharing(SELF, STARTED)), unavailable, doc(idle(OTHER)), { ...doc(), rooms: [] }, doc(sharing(SELF, STARTED)));
    expect(r.events.filter((e) => e !== 'http:GET presence')).toEqual([]);
    expect(r.sup.audio.state).toBe('on');
  });

  it('a failed read says nothing', async () => {
    const r = make();
    await sounding(r);
    r.presence.push(refusal(500), { kind: 'none' }, ok({ schema: 'conch.voice_presence.v2' }));
    await read(r, doc(sharing(SELF, STARTED)));
    expect(r.events.filter((e) => e !== 'http:GET presence')).toEqual([]);
  });

  it('another participant’s share ending sends nothing', async () => {
    const r = make();
    await sounding(r);
    await read(r, doc(sharing(SELF, STARTED), sharing(OTHER, EARLIER)), doc(sharing(SELF, STARTED), idle(OTHER)), endedOf(EARLIER, doc(sharing(SELF, STARTED))));
    expect(r.events.filter((e) => e !== 'http:GET presence')).toEqual([]);
  });

  it('the lag: "not sharing" before presence has shown the share ends nothing', async () => {
    const r = make();
    await sounding(r);
    await read(r, doc(idle(SELF)), doc(idle(SELF)));
    expect(r.events.filter((e) => e !== 'http:GET presence')).toEqual([]);
    await read(r, doc(sharing(SELF, STARTED)), doc(idle(SELF)));
    expect(r.events.filter((e) => e !== 'http:GET presence')).toEqual(['helper:capture off', 'http:DELETE audio']);
  });

  it('a share listed with no start ties to nothing and ends nothing', async () => {
    const r = make();
    await sounding(r);
    await read(r, doc(sharing(SELF, STARTED)), doc(sharing(SELF)));
    expect(r.events.filter((e) => e !== 'http:GET presence')).toEqual([]);
  });

  it('with no sound record, the share’s end clears the capture: a later choice cannot add sound to it', async () => {
    const r = make();
    await r.live();
    await read(r, doc(sharing(SELF, STARTED)), doc(idle(SELF)));
    expect(await r.sound?.start(DOC)).toEqual({ ok: false, error: 'no-share' });
    expect(r.events).not.toContain('http:PUT audio');
    expect(r.events).not.toContain('helper:capture on');
  });

  it('a new share on the same companion after the pick’s share ended gets no sound without a new pick', async () => {
    const r = make();
    await r.live();
    await read(r, doc(sharing(SELF, STARTED)), doc(sharing(SELF, LATER)));
    expect(await r.sound?.start(DOC)).toEqual({ ok: false, error: 'no-share' });
    expect(r.events).not.toContain('helper:capture on');
  });

  it('a watch (no capture) is never read for', async () => {
    const r = make();
    await r.watch();
    r.presence.push(ok(doc(idle(SELF))));
    await settle();
    await settle();
    expect(reads(r)).toBe(0);
  });
});
