// The share's sound ends when conchd's presence shows this person's own share
// ended (issue #1876; docs/design/linux-share-audio.md §10). Main reads the
// presence itself, with the companion's client, while a capture or a sound
// record stands: never the page's feed. The fake conchd answers each read
// with the next queued answer; with none queued a read fails and says nothing.
import { describe, expect, it } from 'vitest';

import type { VoiceParticipantV2, VoicePresenceV2 } from '../../../web/src/schema/generated.ts';
import type { Exchange } from '../../main/api/client.ts';
import { DOC, STARTED, consent, deferred, ok, refusal, rig, settle, type Rig } from './share-audio.support.ts';

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

  it('the lag: one "not sharing" before presence has shown the share ends nothing', async () => {
    const r = make();
    await sounding(r);
    await read(r, doc(idle(SELF)));
    expect(r.events.filter((e) => e !== 'http:GET presence')).toEqual([]);
    await read(r, doc(sharing(SELF, STARTED)), doc(idle(SELF)));
    expect(r.events.filter((e) => e !== 'http:GET presence')).toEqual(['helper:capture off', 'http:DELETE audio']);
  });

  it('a share presence never showed ends after two answers in a row of this person sharing nothing', async () => {
    const r = make();
    await sounding(r);
    // An unavailable answer says nothing: it does not break the run, nor count.
    await read(r, doc(idle(SELF)), unavailable);
    expect(r.events.filter((e) => e !== 'http:GET presence')).toEqual([]);
    // Before presence has shown the share, an answer that does not list the
    // person counts too: it may be another channel's (issue #1880).
    await read(r, doc(idle(OTHER)));
    expect(r.events.filter((e) => e !== 'http:GET presence')).toEqual(['helper:capture off', 'http:DELETE audio']);
  });

  it('a pick no claim stood behind keeps no capture: a later share gets no sound without a new pick', async () => {
    const r = make();
    await r.live();
    await read(r, doc(idle(SELF)), doc(idle(SELF)));
    expect(await r.sound?.start(DOC)).toEqual({ ok: false, error: 'no-share' });
    expect(r.events).not.toContain('http:PUT audio');
  });

  it('a share_ended newer than this share’s start ends it', async () => {
    const r = make();
    await sounding(r);
    await read(r, endedOf(LATER));
    expect(r.events.filter((e) => e !== 'http:GET presence')).toEqual(['helper:capture off', 'http:DELETE audio']);
  });

  it('a read sent before a new pick says nothing of it: the new share is not bound to the old one', async () => {
    const r = make();
    await r.live();
    const late = deferred<Exchange>();
    r.presence.push(late.promise);
    for (let i = 0; i < 20 && r.presence.length > 0; i++) await settle();
    // The person stops and shares again: a new pick while the read is out.
    r.sound?.captureGranted(DOC);
    late.resolve(ok(doc(sharing(SELF, STARTED))));
    await settle();
    await read(r, doc(sharing(SELF, LATER)), doc(sharing(SELF, LATER)));
    r.put.push(consent(LATER));
    expect(await r.sound?.start(DOC)).toMatchObject({ ok: true });
    await read(r, doc(sharing(SELF, LATER)));
    expect(r.events).toContain('helper:capture on');
    expect(r.events).not.toContain('helper:capture off');
  });

  it('an answer that arrives after close is not judged', async () => {
    const r = make();
    await sounding(r);
    const late = deferred<Exchange>();
    r.presence.push(late.promise);
    for (let i = 0; i < 20 && r.presence.length > 0; i++) await settle();
    r.sound?.close();
    late.resolve(ok(endedOf(STARTED)));
    await settle();
    expect(r.events.filter((e) => e !== 'http:GET presence')).toEqual([]);
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

// A rename of the channel during a share (issue #1880): conchd answers 404 to
// the old name. Main follows the channel by the id presence gave it, or ends
// the share's sound.
describe('a rename of the share’s channel', () => {
  const channels = (...list: { id: number; name: string }[]): Exchange => ok({ channels: list.map((c) => ({ ...c, created_at: joined })) });
  const names = (r: Rig, op: string): unknown[] => r.sends.filter((s) => s.op === op).map((s) => s.params?.['channel']);
  const quiet = (r: Rig): string[] => r.events.filter((e) => !e.startsWith('http:GET'));
  const renamed = (r: Rig, list: Exchange): void => {
    r.rename.from = 'general';
    r.rename.list = list;
  };

  it('the read follows the rename by id, and every later read uses the new name', async () => {
    const r = make();
    await sounding(r);
    await read(r, doc(sharing(SELF, STARTED)));
    renamed(r, channels({ id: 1, name: 'lounge' }));
    await read(r, doc(sharing(SELF, STARTED)), doc(sharing(SELF, STARTED)));
    expect(r.events).toContain('http:GET channels');
    expect(r.sends.filter((s) => s.op === 'listChannels')).toHaveLength(1);
    expect(names(r, 'voicePresence').slice(0, 2)).toEqual(['general', 'general']);
    expect(names(r, 'voicePresence').slice(-2)).toEqual(['lounge', 'lounge']);
    expect(quiet(r)).toEqual([]);
    expect(r.sup.audio.state).toBe('on');
  });

  it('the withdrawal after a rename reaches the new name', async () => {
    const r = make();
    await sounding(r);
    await read(r, doc(sharing(SELF, STARTED)));
    renamed(r, channels({ id: 1, name: 'lounge' }));
    expect(await r.sound?.shareEnded(DOC)).toBe(true);
    expect(quiet(r)).toEqual(['helper:capture off', 'http:DELETE audio (old name)', 'http:DELETE audio']);
    expect(names(r, 'voiceShareAudioEnd')).toEqual(['general', 'lounge']);
  });

  // The companion's own DELETE (issue #1881) runs alongside the sound's end,
  // which forgets the id once its withdrawal is done: the companion takes the
  // id first, so it still finds the new name, with a record or without one.
  it('the companion’s own DELETE after a rename reaches the new name, while the sound ends alongside it', async () => {
    const r = make();
    await sounding(r);
    await read(r, doc(sharing(SELF, STARTED)));
    renamed(r, channels({ id: 1, name: 'lounge' }));
    await r.companion.stop();
    expect(r.events).toContain('http:DELETE companion (old name)');
    expect(r.events.filter((e) => e === 'http:DELETE companion')).toHaveLength(1);
    expect(names(r, 'voiceViewerEnd')).toEqual(['general', 'lounge']);
    expect(names(r, 'voiceShareAudioEnd').at(-1)).toBe('lounge');
    expect(r.logs.filter((l) => l.includes('not confirmed'))).toEqual([]);
  });

  it('the companion’s own DELETE reaches the new name when the share has no sound record', async () => {
    const r = make();
    await r.live();
    await read(r, doc(sharing(SELF, STARTED)));
    renamed(r, channels({ id: 1, name: 'lounge' }));
    await r.companion.end('lock');
    expect(names(r, 'voiceViewerEnd')).toEqual(['general', 'lounge']);
  });

  it('a watch learned no id: the companion’s DELETE is one by the held name, logged', async () => {
    const r = make();
    await r.watch();
    renamed(r, channels({ id: 1, name: 'lounge' }));
    await r.companion.stop();
    expect(names(r, 'voiceViewerEnd')).toEqual(['general']);
    expect(r.events).not.toContain('http:GET channels');
    expect(r.logs).toContain('companion: the removal after stopped was not confirmed');
  });

  it('no id known yet, then a 404: the sound ends (stop, DELETE attempt, capture cleared)', async () => {
    const r = make();
    await sounding(r);
    renamed(r, channels({ id: 1, name: 'lounge' }));
    for (let i = 0; i < 5; i++) await settle();
    expect(quiet(r)).toEqual(['helper:capture off', 'http:DELETE audio (old name)']);
    expect(r.events).not.toContain('http:GET channels');
    expect(await r.sound?.start(DOC)).toEqual({ ok: false, error: 'no-share' });
    const before = reads(r);
    await settle();
    expect(reads(r)).toBe(before);
  });

  it('no id known and no record: the capture alone is cleared', async () => {
    const r = make();
    await r.live();
    renamed(r, channels({ id: 1, name: 'lounge' }));
    for (let i = 0; i < 5; i++) await settle();
    expect(await r.sound?.start(DOC)).toEqual({ ok: false, error: 'no-share' });
    expect(r.events).not.toContain('http:PUT audio');
  });

  it('the id is not in the list: the sound ends', async () => {
    const r = make();
    await sounding(r);
    await read(r, doc(sharing(SELF, STARTED)));
    renamed(r, channels({ id: 2, name: 'other' }));
    for (let i = 0; i < 5; i++) await settle();
    expect(quiet(r)).toEqual(['helper:capture off', 'http:DELETE audio (old name)']);
    expect(await r.sound?.start(DOC)).toEqual({ ok: false, error: 'no-share' });
  });

  it('a list that cannot be read says nothing, and the next read asks again', async () => {
    const r = make();
    await sounding(r);
    await read(r, doc(sharing(SELF, STARTED)));
    renamed(r, { kind: 'none' });
    for (let i = 0; i < 5; i++) await settle();
    expect(quiet(r)).toEqual([]);
    r.rename.list = channels({ id: 1, name: 'lounge' });
    await read(r, doc(sharing(SELF, STARTED)));
    expect(names(r, 'voicePresence').at(-1)).toBe('lounge');
    expect(quiet(r)).toEqual([]);
  });

  it('the name taken over before main’s first read: answers that never show the share teach no id, and end the pick', async () => {
    const r = make();
    await sounding(r);
    // Another channel (9) now answers to the share's name, and lists nobody.
    await read(r, { ...doc(), channel_id: 9 }, { ...doc(idle(OTHER)), channel_id: 9 });
    expect(quiet(r)).toEqual(['helper:capture off', 'http:DELETE audio']);
    expect(await r.sound?.start(DOC)).toEqual({ ok: false, error: 'no-share' });
  });

  it('the id is learned only from an answer that shows the share', async () => {
    const r = make();
    await sounding(r);
    // One answer of another channel, then the share's own: its id (1) is the
    // one followed after a rename.
    await read(r, { ...doc(idle(OTHER)), channel_id: 9 }, doc(sharing(SELF, STARTED)));
    renamed(r, channels({ id: 9, name: 'trap' }, { id: 1, name: 'lounge' }));
    await read(r, doc(sharing(SELF, STARTED)));
    expect(names(r, 'voicePresence').at(-1)).toBe('lounge');
    expect(quiet(r)).toEqual([]);
  });

  it('a rename to a name main cannot put in a path ends the sound', async () => {
    for (const name of ['50% off', 'dev/ops', 'a\\b']) {
      const r = make();
      await sounding(r);
      await read(r, doc(sharing(SELF, STARTED)));
      renamed(r, channels({ id: 1, name }));
      for (let i = 0; i < 5; i++) await settle();
      expect(quiet(r)).toEqual(['helper:capture off', 'http:DELETE audio (old name)']);
      expect(names(r, 'voicePresence')).not.toContain(name);
    }
  });

  it('a leave’s withdrawal still follows the rename when the companion’s end comes before it finishes', async () => {
    const r = make();
    await sounding(r);
    await read(r, doc(sharing(SELF, STARTED)));
    renamed(r, channels({ id: 1, name: 'lounge' }));
    const share = r.companion.current();
    expect(share).not.toBeNull();
    const leave = r.sound?.leaving();
    if (share !== null) void r.sound?.ended(share, 'stopped');
    await leave;
    await settle();
    expect(names(r, 'voiceShareAudioEnd')).toEqual(['general', 'lounge']);
  });

  it('a 404 that is not conchd’s channel_not_found is not a rename', async () => {
    const r = make();
    await sounding(r);
    r.presence.push(refusal(404), refusal(404, 'not_found'), refusal(404));
    for (let i = 0; i < 10 && r.presence.length > 0; i++) await settle();
    expect(quiet(r)).toEqual([]);
    expect(r.events).not.toContain('http:GET channels');
    expect(r.sup.audio.state).toBe('on');
  });

  it('the old name answering for another channel is not judged: the sound follows its own channel', async () => {
    const r = make();
    await sounding(r);
    await read(r, doc(sharing(SELF, STARTED)));
    r.rename.list = channels({ id: 1, name: 'lounge' });
    // conchd answers the old name with a different channel that now holds it.
    await read(r, { ...doc(idle(SELF)), channel_id: 2 }, doc(sharing(SELF, STARTED)));
    expect(quiet(r)).toEqual([]);
    expect(names(r, 'voicePresence')).toContain('lounge');
    expect(names(r, 'voicePresence').at(-1)).toBe('lounge');
    expect(r.sends.filter((s) => s.op === 'listChannels')).toHaveLength(1);
  });
});
