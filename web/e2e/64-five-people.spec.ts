// Five separate people together (issue #1608), the heavy scenario: alice, bob
// and carol of the voice world and two more made by members(2), each in a
// context of its own, in voice and in chat at once, with LiveKit asked who is
// connected. Tagged @heavy: five browsers with media are too many for a PR
// shard (e2e-heavy.yml runs them). Without Docker it fails when CI is set and
// is skipped otherwise, as in 10-voice.
//
// What this proves: five separate principals at once on one machine over
// loopback with Chromium's fake devices. What it does not: other machines,
// real devices, other engines.
import { expect, messages, say, test } from './fixtures';
import { enter, expectClean, joinVoice, leave, type Someone } from './people';
import { dockerAvailable, emptiesWithin, startVoiceWorld, type VoiceWorld } from './voice-world';
import { loadWorld } from './world';

test.describe('@heavy five people', () => {
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

  test('@heavy five principals join voice and chat, LiveKit lists five, then three, then none', async ({ browser }) => {
    test.setTimeout(240_000);
    const members = [vw.alice, vw.bob, vw.carol, ...(await vw.members(2))];
    expect(new Set(members.map((m) => m.id)).size, 'five different principals').toBe(5);
    const identities = (ms: readonly { id: number }[]) => ms.map((m) => `p${String(m.id)}`).sort();
    const people: Someone[] = [];
    try {
      for (const m of members) people.push(await enter(browser, vw, m));
      for (const p of people) await joinVoice(p, 'Join voice');
      await expect.poll(() => vw.participants(), { timeout: 20_000 }).toEqual(identities(members));

      // Each posts one message; the other four (and the sender) see it once.
      const bodies = members.map((m) => `1608 hello from ${m.name} ${Date.now().toString(36)}`);
      for (const [i, p] of people.entries()) await say(p.page, vw.channel, bodies[i] ?? '');
      for (const p of people) for (const body of bodies) await expect(messages(p.page).getByText(body)).toHaveCount(1);

      for (const p of people) await expectClean(vw, p);

      // Two leave; LiveKit lists the other three.
      await leave(people[3] as Someone);
      await leave(people[4] as Someone);
      await expect.poll(() => vw.participants(), { timeout: emptiesWithin }).toEqual(identities(members.slice(0, 3)));
    } finally {
      for (const p of people) await leave(p);
    }
    await expect.poll(() => vw.participants(), { timeout: emptiesWithin }).toEqual([]);
    // And the room itself is closed, not merely empty.
    await expect.poll(() => vw.rooms(), { timeout: emptiesWithin, message: 'LiveKit closes the room' }).toBe(0);
  });
});
