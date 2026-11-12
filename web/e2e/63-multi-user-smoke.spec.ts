// Three separate people together (issue #1608), the PR lane's smoke of the
// multi-user helpers (people.ts, voice-world.ts).
//
// Messaging runs on the shared p3-local server: three principals, each in a
// browser context of its own, each send one message to #general. Voice runs on
// the voice world's own conchd and pinned LiveKit: alice, bob and carol join,
// and LiveKit itself is asked who is connected (voice-world.ts participants()).
// Without Docker the voice test fails when CI is set and is skipped otherwise,
// as in 10-voice.
//
// What this proves: separate principals, each in a context of its own, on one
// machine over loopback with Chromium's fake devices, and LiveKit's own list.
// What it does not: other machines, real devices, other engines.
import type { Page } from '@playwright/test';
import { history, post } from './api';
import { expect, messages, say, signIn, statusLine, test } from './fixtures';
import { enter, expectClean, joinVoice, leave, leaveVoice, leaveWithin, type Someone } from './people';
import { answered } from './livekit-admin';
import { dockerAvailable, emptiesWithin, startVoiceWorld, type VoiceWorld } from './voice-world';
import { loadWorld, principalId } from './world';

test('@smoke @cross three principals each send one message to #general and every page shows each once', async ({ meet, world }) => {
  const people = [
    { name: 'browser-test', token: world.tokens.browser, body: `1608 hello from browser-test ${Date.now().toString(36)}` },
    { name: 'jamie-test', token: world.tokens.guest, body: `1608 hello from jamie-test ${Date.now().toString(36)}` },
    { name: 'nick-test', token: world.tokens.nick, body: `1608 hello from nick-test ${Date.now().toString(36)}` },
  ];
  const pages: Page[] = [];
  for (const p of people) {
    const { page } = await meet();
    await signIn(page, world, p.token, p.name);
    pages.push(page);
  }
  // Three contexts, three principals: none shares another's.
  const ids = await Promise.all(people.map((p) => principalId(world, p.token)));
  expect(new Set(ids).size, 'three different principals').toBe(3);

  for (const [i, page] of pages.entries()) {
    await expect(statusLine(page)).toContainText(people[i]?.name ?? '');
    await say(page, 'general', people[i]?.body ?? '');
  }
  const everyoneSeesEachOnce = async () => {
    for (const page of pages) for (const p of people) await expect(messages(page).getByText(p.body)).toHaveCount(1);
  };
  await everyoneSeesEachOnce();
  for (const p of people) {
    expect((await history(world, world.tokens.nick, 'general')).filter((m) => m.body === p.body), `${p.name}'s message in the history`).toHaveLength(1);
  }

  // Once more after one page reloads. A message posted now and seen on the
  // reloaded page means the replay that follows a reload has landed, so a
  // duplicate between it and the history would show.
  const first = pages[0];
  if (first === undefined) throw new Error('no page');
  await first.reload();
  const sentinel = `1608 sentinel after the reload ${Date.now().toString(36)}`;
  await post(world, world.tokens.nick, 'general', sentinel);
  for (const page of pages) await expect(messages(page).getByText(sentinel)).toHaveCount(1);
  await everyoneSeesEachOnce();
});

test.describe('@smoke @cross @voice with LiveKit', () => {
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

  test('@smoke three principals join voice, see each other, and LiveKit lists exactly them until they leave', async ({ browser }) => {
    const members = [vw.alice, vw.bob, vw.carol];
    expect(new Set(members.map((m) => m.id)).size, 'three different principals').toBe(3);
    const people: Someone[] = [];
    try {
      for (const m of members) people.push(await enter(browser, vw, m));
      for (const p of people) await joinVoice(p, 'Join voice');

      // Each page's roster names the other two.
      for (const p of people) {
        const roster = p.page.getByRole('region', { name: 'Voice' }).getByRole('list', { name: 'In voice' });
        for (const other of members.filter((m) => m !== p.member)) await expect(roster).toContainText(other.name, { timeout: 15_000 });
      }

      // LiveKit's own view: exactly these three, none twice.
      await expect.poll(() => answered(vw.participants()), { timeout: 15_000 }).toEqual(members.map((m) => `p${String(m.id)}`).sort());

      for (const p of people) await expectClean(vw, p);

      // Each person leaves by the page's own button, and LiveKit's own list
      // loses exactly that person within leaveWithin, while the pages are
      // still open (issue #1764). A page closed instead is left to
      // livekit-client's pagehide handler, which races the page's teardown;
      // the tab-close and vanished-page paths are 67-voice-recovery's. The
      // bound is people.ts's leaveWithin, far under the 20 s a leave that never
      // arrived takes to time out.
      const remaining = [...people];
      for (const p of people) {
        await leaveVoice(p);
        remaining.splice(remaining.indexOf(p), 1);
        await expect
          .poll(() => answered(vw.participants()), { timeout: leaveWithin, message: `${p.member.name} leaves LiveKit at once` })
          .toEqual(remaining.map((r) => `p${String(r.member.id)}`).sort());
      }
    } finally {
      for (const p of people) await leave(p);
    }
    await expect.poll(() => answered(vw.participants()), { timeout: emptiesWithin }).toEqual([]);
    // And the room itself is closed, not merely empty.
    await expect.poll(() => answered(vw.rooms()), { timeout: emptiesWithin, message: 'LiveKit closes the room' }).toBe(0);
  });
});
