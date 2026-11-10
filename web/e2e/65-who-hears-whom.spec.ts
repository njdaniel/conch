// Who hears whom (issue #1610): alice, bob and carol, three separate principals
// in contexts of their own, each with a microphone that is a tone of its own
// (450, 650 and 950 Hz, no small-integer ratio), in one voice room against the
// voice world's pinned LiveKit. Each page analyses every audio track it receives
// on its own (synthetic-mic.ts), at each of the three tones.
//
// What it asserts, for each of push to talk, the open microphone with mute, and
// deafen: while one holds the key, each of the other two pages has exactly one
// track above its idle floor at that person's tone and none at any other tone,
// and the talker's own page has nothing above its floor; released, nobody hears
// anything. Across the whole test no page ever hears its own tone (no
// self-echo). Muted, alice reaches neither listener; unmuted, both. Deafened,
// every remote audio element on carol's page is muted (that is how the client
// deafens, sdk.ts setDeafened) while the other two still hear each other.
//
// Levels: the floor is measured per page at run time, while nobody transmits
// (the highest level of any track at any of the tones), never below minFloorDb
// (see there), and "heard" is more than marginDb above it. 20 dB is the issue's
// figure.
//
// Attribution: livekit-client's stream ids are the publisher's participant sid
// and the track sid ("PA_...|TR_..."), the same on every listening page. A page
// cannot map a sid to an identity (LiveKit's list, vw.participants(), holds
// identities only), so a track is attributed to its publisher by its tone, and
// the stream ids check that: every tone is carried by one stream id on both
// listening pages, the three differ, and no track on a page has the stream id
// of that page's own tone.
//
// Bounds are 10-voice's (15 s to show or lose "(transmitting)"); issue #1041 is
// open, so nothing is asserted tighter. Without Docker it fails when CI is set
// and is skipped otherwise, as in 10-voice.
//
// What this proves: three separate principals on one machine over loopback,
// routing and gating at the media layer, with synthetic signals. What it does
// not: real microphones, echo cancellation, speakers, other machines, or the
// desktop helper's audio.
import type { Page } from '@playwright/test';
import { chooseMicrophone, expect, test } from './fixtures';
import { enter, expectClean, inbound, joinVoice, leave, type Someone } from './people';
import { floorOf, heardBy, recordHeard, syntheticMicrophone, table, toneLevels, tracksAt, type TrackLevel } from './synthetic-mic';
import { dockerAvailable, emptiesWithin, startVoiceWorld, type VoiceWorld } from './voice-world';
import { loadWorld } from './world';

const tones = [450, 650, 950] as const;
const marginDb = 20;
const readMs = 1200;
// The lowest a floor is taken to be. An idle page is digital silence (-120 dB:
// a gated track sends nothing), which is no floor: Opus decodes a held tone
// with a skirt of noise 70 dB below it (measured -91 to -94 dB against the tone
// at -18), and "20 dB above silence" would call that skirt a speaker. A click
// where a gate opens or shuts, or a glitch in a held tone (a jitter-buffer
// underrun on a starved runner), splatters across the analysis window: -58 dB
// measured, about -52 estimated for a phase jump. "Heard" is therefore above
// -40 dB: 22 dB under the tone and 12 over the worst splatter expected.
const minFloorDb = -60;
// The highest an idle page may measure: anything louder is a blip, or a tone
// that leaked while nobody had pressed a key.
const maxIdleDb = -60;
const voiceRegion = (page: Page) => page.getByRole('region', { name: 'Voice' });
const roster = (page: Page) => voiceRegion(page).getByRole('list', { name: 'In voice' });

test.describe('@cross @voice who hears whom', () => {
  let vw: VoiceWorld;
  let stopWorld: (() => void) | undefined;

  test.beforeAll(async ({}, info) => {
    if (!dockerAvailable()) {
      if (process.env['CI'] !== undefined) throw new Error('browser voice needs Docker for the pinned LiveKit image, and there is none');
      info.skip(true, 'Docker is not available; the browser voice suite needs it (it fails, rather than skips, when CI is set)');
      return;
    }
    vw = await startVoiceWorld(loadWorld());
    stopWorld = vw.stop;
  });
  test.afterAll(() => {
    stopWorld?.();
  });

  test('three people hear exactly the person who talks, and no one hears themselves', async ({ browser }, info) => {
    test.setTimeout(150_000);
    const members = [vw.alice, vw.bob, vw.carol];
    const people: Someone[] = [];
    // Every reading taken, for the table a failure attaches and the self-echo
    // check at the end.
    const log: { phase: string; page: number; floor: number; tracks: TrackLevel[] }[] = [];
    let phase = 'setup';
    const floors: number[] = [];
    try {
      for (const [i, m] of members.entries()) {
        people.push(
          await enter(browser, vw, m, {
            before: async (page) => {
              await page.addInitScript(syntheticMicrophone, { frequency: tones[i], on: true });
              await page.addInitScript(recordHeard);
            },
          }),
        );
      }
      for (const p of people) await joinVoice(p, 'Join voice');
      // The tone is on throughout; what is sent is up to the key.
      for (const p of people) await expect(voiceRegion(p.page).getByRole('button', { name: 'Hold to talk' })).toBeVisible();
      // Each page receives the other two, and nothing else.
      for (const p of people) await expect.poll(async () => (await toneLevels(p.page, tones, 0)).filter((t) => t.live).length, { timeout: 30_000 }).toBe(2);

      const observe = async (ms: number): Promise<TrackLevel[][]> => {
        const all = await Promise.all(people.map((p) => toneLevels(p.page, tones, ms)));
        all.forEach((tracks, page) => log.push({ phase, page, floor: floors[page] ?? -120, tracks }));
        return all;
      };
      const heard = (all: TrackLevel[][]) => all.map((tracks, i) => heardBy(tracks, floors[i] ?? -120, marginDb));

      // The idle floor of each page: nobody has pressed anything.
      const idle = await Promise.all(people.map((p) => toneLevels(p.page, tones, 1500)));
      for (const [i, t] of idle.entries()) {
        expect(floorOf(t), `${members[i]?.name ?? '?'}'s idle level (dB): something was heard before anyone pressed a key`).toBeLessThanOrEqual(maxIdleDb);
        floors.push(floorOf(t, minFloorDb));
      }
      info.annotations.push({ type: 'idle levels, floors (dB)', description: `${idle.map((t) => floorOf(t).toFixed(0)).join(', ')}; ${floors.map((f) => f.toFixed(0)).join(', ')}` });

      // Stream ids heard at each tone, across the pages that heard it.
      const streams: Set<string>[] = tones.map(() => new Set());
      const expectOnly = (all: TrackLevel[][], talker: number | null, skip: readonly number[] = []) => {
        for (const [i, tracks] of all.entries()) {
          if (skip.includes(i)) continue;
          const floor = floors[i] ?? -120;
          const at = heardBy(tracks, floor, marginDb);
          if (talker === null || i === talker) {
            // The talker's own page, or anyone's when nobody talks: silence.
            expect(at.flat(), `${members[i]?.name ?? '?'} hears a tone`).toEqual([]);
            continue;
          }
          expect(tracksAt(tracks, floor, marginDb, talker), `${members[i]?.name ?? '?'}: tracks carrying ${members[talker]?.name ?? '?'}'s tone`).toBe(1);
          expect(at.flat().filter((f) => f !== talker), `${members[i]?.name ?? '?'} hears a tone that is not ${members[talker]?.name ?? '?'}'s`).toEqual([]);
          for (const t of tracks) if (t.live && t.db[talker] !== undefined && t.db[talker] > floor + marginDb) streams[talker]?.add(t.streamId);
        }
      };
      const expectSilence = async (skip: readonly number[] = []) => {
        // The analyser's window and the jitter buffer drain: poll, within 10-voice's bound.
        await expect
          .poll(async () => heard(await observe(300)).flatMap((at, i) => (skip.includes(i) ? [] : at.flat())), { timeout: 15_000 })
          .toEqual([]);
      };
      const focus = (p: Someone) => p.page.getByRole('heading', { name: 'Conch' }).click();

      try {
        phase = 'idle';
        await expectSilence();
        expectOnly(await observe(readMs), null);

        // Each person talks in turn, by the key.
        for (const [t, p] of people.entries()) {
          phase = `${p.member.name} talks`;
          await focus(p);
          await p.page.keyboard.down('Backquote');
          const others = people.filter((o) => o !== p);
          for (const o of others) await expect(roster(o.page)).toContainText(`${p.member.name} (transmitting)`, { timeout: 15_000 });
          await expect(voiceRegion(p.page)).toContainText('Transmitting');
          expectOnly(await afterOnset(observe), t);
          await p.page.keyboard.up('Backquote');
          for (const o of others) await expect(roster(o.page)).not.toContainText(`${p.member.name} (transmitting)`, { timeout: 15_000 });
          await expectSilence();
        }

        // Attribution: one stream id per tone, three different, and none on its own page.
        const ids = streams.map((s) => [...s]);
        expect(ids.map((s) => s.length), 'stream ids carrying each tone').toEqual([1, 1, 1]);
        expect(new Set(ids.flat()).size, 'three different stream ids').toBe(3);
        expect(ids.flat().every((id) => /^PA_[^|]+\|TR_/.test(id)), 'a stream id is the participant sid and the track sid').toBe(true);
        for (const [i, p] of people.entries()) {
          const own = ids[i]?.[0];
          const here = (await toneLevels(p.page, tones, 0)).map((t) => t.streamId);
          expect(here.includes(own ?? ''), `${p.member.name} receives a track of their own stream`).toBe(false);
        }

        // Mute: alice, muted, in the open microphone reaches neither; unmuted, both.
        const [alice, bob, carol] = people;
        if (alice === undefined || bob === undefined || carol === undefined) throw new Error('three people');
        phase = 'mute';
        const av = voiceRegion(alice.page);
        await av.getByRole('button', { name: 'Mute', exact: true }).click();
        await expect(av).toContainText('Muted.');
        await chooseMicrophone(av, 'open');
        await expectSilence();
        expectOnly(await observe(readMs * 2), null);
        for (const o of [bob, carol]) await expect(roster(o.page)).not.toContainText('alice (transmitting)');
        await av.getByRole('button', { name: 'Unmute', exact: true }).click();
        await expect(av).not.toContainText('Muted.');
        for (const o of [bob, carol]) await expect(roster(o.page)).toContainText('alice (transmitting)', { timeout: 15_000 });
        expectOnly(await afterOnset(observe), 0);
        // Back to push to talk: the open microphone would keep alice's tone going.
        await chooseMicrophone(av, 'push to talk');
        for (const o of [bob, carol]) await expect(roster(o.page)).not.toContainText('alice (transmitting)', { timeout: 15_000 });
        await expectSilence();

        // Deafen: carol's remote audio elements are all muted, and bob and alice still hear each other.
        phase = 'deafen';
        const cv = voiceRegion(carol.page);
        const muteds = (page: Page) => page.evaluate(() => [...document.querySelectorAll('audio')].map((a) => a.muted));
        expect(await muteds(carol.page), 'carol plays two people').toEqual([false, false]);
        await cv.getByRole('button', { name: 'Deafen', exact: true }).click();
        await expect(cv).toContainText('Deafened');
        expect(await muteds(carol.page), "every remote audio element on carol's page is muted").toEqual([true, true]);
        expect(await muteds(alice.page)).toEqual([false, false]);
        for (const [t, p] of [bob, alice].map((p) => [people.indexOf(p), p] as const)) {
          await focus(p);
          await p.page.keyboard.down('Backquote');
          await expect(voiceRegion(p.page)).toContainText('Transmitting');
          const listener = people.find((o) => o !== p && o !== carol);
          if (listener !== undefined) await expect(roster(listener.page)).toContainText(`${p.member.name} (transmitting)`, { timeout: 15_000 });
          // Carol is deaf in her elements, not in her tracks: only the other listener is read.
          expectOnly(await afterOnset(observe), t, [2]);
          await p.page.keyboard.up('Backquote');
          await expectSilence([2]);
        }
        await cv.getByRole('button', { name: 'Undeafen', exact: true }).click();
        expect(await muteds(carol.page)).toEqual([false, false]);

        // No self-echo, over every reading of the test (the polls that wait for
        // a gate to open or shut included): a page's own tone is never above
        // that page's "heard" line.
        for (const r of log) {
          const own = Math.max(-120, ...r.tracks.filter((t) => t.live).map((t) => t.db[r.page] ?? -120));
          expect(own, `${members[r.page]?.name ?? '?'} hears their own tone`).toBeLessThanOrEqual(r.floor + marginDb);
        }
        expect(log.length, 'readings taken').toBeGreaterThan(20);
      } catch (err) {
        // The readings of the phase that failed, and what each page's peer
        // connections have received in audio bytes (they name no token or room).
        info.annotations.push({ type: `levels in '${phase}'`, description: log.filter((r) => r.phase === phase).slice(-30).map((r) => table(members[r.page]?.name ?? '?', r.tracks, tones, r.floor, marginDb)).join('\n') });
        const bytes = await Promise.all(people.map(async (p) => inbound(p.page, { perTrack: true }).then((i) => `${p.member.name}: ${(i.tracks ?? []).filter((t) => t.kind === 'audio').map((t) => String(t.bytes)).join(', ')}`).catch(() => `${p.member.name}: unreadable`)));
        info.annotations.push({ type: 'inbound-rtp audio bytes per stream', description: bytes.join('\n') });
        throw err;
      }

      for (const p of people) await expectClean(vw, p);
    } finally {
      for (const p of people) await leave(p);
    }
    await expect.poll(() => vw.participants(), { timeout: emptiesWithin }).toEqual([]);
  });
});

// afterOnset reads once a talker has been on for a moment: a short wait for
// the first packets to be decoded, then one reading.
async function afterOnset(observe: (ms: number) => Promise<TrackLevel[][]>): Promise<TrackLevel[][]> {
  await new Promise((r) => setTimeout(r, 300));
  return observe(readMs);
}
