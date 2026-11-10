// Screen share in the browser, the three things 12-screen-share does not try
// (issue #1611): the share_screen permission, the picture a viewer gets, and a
// share stopped and started again. Separate people (people.ts) against the
// voice world's own conchd and pinned LiveKit (voice-world.ts), on Chromium's
// fake screen. Without Docker the tests fail when CI is set and are skipped
// otherwise, as in 10-voice.
//
// share_screen off: before a share, conchd refuses the claim (403
// voice_not_allowed) and writes no voice_share_started; during one, the share
// ends with reason access_lost, the viewer's tile goes, its frames stop and
// LiveKit lists no screen track of the sharer's. The owner's PUT
// /v1/permissions ends it before it answers, so the bound below is the
// 10-voice roster bound, a stated margin and no more.
//
// Pixels: the viewer's <video> is drawn to a canvas five times over two
// seconds; the picture is not flat (luminance standard deviation above
// flatBelow). The pinned Chromium's fake screen (the @playwright/test
// version in package.json) animates. Measured: the
// deviation is 23 to 31 and consecutive samples half a second apart differ by
// by 103 levels (102.6 to 104.4 over 20 pairs) in some pixel, so the samples must also differ.
//
// Stop then restart: the second share is a new LiveKit publication (a track
// sid LiveKit never listed for the first), the first is gone from LiveKit, and
// the viewer receives frames from the new one after pressing Watch again.
// LiveKit's tracks are asked of LiveKit directly with the world's API secret;
// livekit-admin.ts lists identities only, so the spec asks for tracks itself.
//
// What this proves: separate principals, Chromium's fake screen, one machine,
// LiveKit's own list. What it does not: real displays, GPUs, Wayland portals,
// Firefox or Safari capture, other machines. Those are #474 (Chrome and
// Firefox by hand), #1584 and #1603 (the desktop) and #1513 (the droplet).
import type { Page } from '@playwright/test';
import { expect, test } from './fixtures';
import { adminToken, LiveKitStatusError } from './livekit-admin';
import { enter, expectClean, inbound, joinVoice, leave, type Someone } from './people';
import { dockerAvailable, startVoiceWorld, type VoiceWorld } from './voice-world';
import { loadWorld } from './world';

// A flat picture (one colour, black or a placeholder) has a luminance standard
// deviation near 0 on a 0 to 255 scale; the fake screen's test pattern is far
// above this.
const flatBelow = 5;
// Two samples differ when some pixel's luminance moved by more than this.
const movedBy = 8;
// The longest a share may take to end after share_screen goes off: the 10-voice
// roster bound (2 * (2 s call + 1.5 s gap + 2 s call)), for a loaded machine.
const endsWithin = 2 * (2000 + 1500 + 2000);

const voiceRegion = (page: Page) => page.getByRole('region', { name: 'Voice' });
const shareView = (page: Page) => page.getByRole('region', { name: 'Shared screen' });

// A screen track LiveKit lists for a participant: its sid and source.
interface Published {
  readonly sid: string;
  readonly source: string;
}

test.describe('@smoke screen share permission and pixels with LiveKit', () => {
  let vw: VoiceWorld;
  let stopWorld: (() => void) | undefined;

  test.beforeAll(async ({}, info) => {
    if (!dockerAvailable()) {
      if (process.env['CI'] !== undefined) throw new Error('screen share needs Docker for the pinned LiveKit image, and there is none');
      info.skip(true, 'Docker is not available; the screen share suite needs it (it fails, rather than skips, when CI is set)');
      return;
    }
    vw = await startVoiceWorld(loadWorld());
    stopWorld = vw.stop;
  });
  test.afterAll(() => {
    stopWorld?.();
  });

  // The member cell of share_screen as the owner sets it, and as it was found.
  async function memberShareCell(): Promise<boolean> {
    const read = await vw.api(vw.operator, 'GET', '/v1/permissions');
    expect(read.status).toBe(200);
    const { rows } = JSON.parse(read.text) as { rows: { permission: string; member: boolean }[] };
    const row = rows.find((r) => r.permission === 'share_screen');
    if (row === undefined) throw new Error('no share_screen row in the permissions');
    return row.member;
  }
  async function setMemberShareCell(granted: boolean): Promise<void> {
    const read = await vw.api(vw.operator, 'GET', '/v1/permissions');
    expect(read.status).toBe(200);
    const { revision } = JSON.parse(read.text) as { revision: number };
    const put = await vw.api(vw.operator, 'PUT', '/v1/permissions', { revision, changes: [{ permission: 'share_screen', role: 'member', granted }] });
    expect(put.status, put.text).toBe(200);
  }
  // withShareOff turns the member cell off, runs body, and puts the cell back as
  // it was found, checking that it is.
  async function withShareOff(body: () => Promise<void>): Promise<void> {
    const found = await memberShareCell();
    await setMemberShareCell(false);
    try {
      await body();
    } finally {
      await setMemberShareCell(found);
    }
    // After the finally, so a failed body is not masked by this check.
    expect(await memberShareCell(), 'share_screen restored for members').toBe(found);
  }

  // LiveKit's screen share tracks of member, asked of LiveKit directly.
  async function screenTracks(member: { id: number }): Promise<Published[]> {
    const base = `http://127.0.0.1:${new URL(vw.livekitOrigin.replace(/^ws/, 'http')).port}`;
    // The key and secret the voice world chose; the secret is the one it adds
    // to secrets with this prefix.
    const secret = vw.secrets.find((s) => s.startsWith('web-voice-NOT-FOR-PRODUCTION-'));
    if (secret === undefined) throw new Error('no LiveKit secret in the world');
    const call = async (method: string, grant: object, body: object): Promise<unknown> => {
      let res: Response;
      try {
        res = await fetch(`${base}/twirp/livekit.RoomService/${method}`, {
          method: 'POST',
          headers: { 'Content-Type': 'application/json', Authorization: `Bearer ${adminToken('webvoicekey', secret, grant)}` },
          body: JSON.stringify(body),
          signal: AbortSignal.timeout(5000),
        });
      } catch {
        // A transport error names the address.
        throw new Error(`livekit ${method}: no answer`);
      }
      // Status only: a body can name a room.
      if (res.status !== 200) throw new LiveKitStatusError(method, res.status);
      try {
        return (await res.json()) as unknown;
      } catch {
        // A parse error quotes part of the body.
        throw new Error(`livekit ${method}: unreadable answer`);
      }
    };
    const rooms = (await call('ListRooms', { roomList: true }, {})) as { rooms?: { name?: string }[] };
    const out: Published[] = [];
    for (const room of rooms.rooms ?? []) {
      if (typeof room.name !== 'string') continue;
      type Found = { participants?: { identity?: string; tracks?: { sid?: string; source?: string }[] }[] };
      let found: Found;
      try {
        found = (await call('ListParticipants', { roomAdmin: true, room: room.name }, { room: room.name })) as Found;
      } catch (err) {
        // A room that ends between the two calls has no one, as in livekit-admin.ts.
        if (err instanceof LiveKitStatusError && err.status === 404) continue;
        throw err;
      }
      for (const p of found.participants ?? []) {
        if (p.identity !== `p${String(member.id)}`) continue;
        for (const t of p.tracks ?? []) {
          if (typeof t.sid === 'string' && typeof t.source === 'string' && t.source.startsWith('SCREEN_SHARE')) out.push({ sid: t.sid, source: t.source });
        }
      }
    }
    return out;
  }
  const videoSids = async (member: { id: number }) => (await screenTracks(member)).filter((t) => t.source === 'SCREEN_SHARE').map((t) => t.sid);

  async function shareRows(): Promise<{ actor: string; action: string; detail: string }[]> {
    return (await vw.audit()).filter((e) => e.action.startsWith('voice_share_')).map(({ actor, action, detail }) => ({ actor, action, detail }));
  }
  // The row's actor, action and reason.
  const rowName = (r: { actor: string; action: string; detail: string }) => `${r.actor} ${r.action} ${/reason=(\w+)/.exec(r.detail)?.[1] ?? ''}`.trim();

  async function share(p: Someone): Promise<void> {
    const voice = voiceRegion(p.page);
    await expect(voice).toContainText(/Sound: .*tab shares its (own )?sound/);
    await voice.getByRole('button', { name: 'Share screen' }).click();
    await expect(voice).toContainText('You are sharing your screen with sound.', { timeout: 30_000 });
  }

  // watch presses Watch on the sharer's tile and waits for it to show.
  async function watch(p: Someone): Promise<void> {
    await p.page.getByRole('button', { name: 'Watch' }).click({ timeout: 30_000 });
    await expect(shareView(p.page)).toContainText('alice · LIVE');
  }

  // The received video streams of the page, by stats id, with frames decoded.
  async function videoFrames(p: Someone): Promise<Map<string, number>> {
    const got = await inbound(p.page, { perTrack: true });
    return new Map((got.tracks ?? []).filter((t) => t.kind === 'video').map((t) => [t.id, t.frames]));
  }

  // framesRise waits for a received video stream whose id is not in known to
  // decode frames and then keep decoding them, and returns its id.
  async function framesRise(p: Someone, known: ReadonlySet<string>): Promise<string> {
    let id = '';
    await expect
      .poll(
        async () => {
          for (const [k, frames] of await videoFrames(p)) if (!known.has(k) && frames > 10) id = k;
          return id;
        },
        { timeout: 30_000, message: 'a new video stream decodes frames' },
      )
      .not.toBe('');
    const at = (await videoFrames(p)).get(id) ?? 0;
    await expect.poll(async () => (await videoFrames(p)).get(id) ?? 0, { timeout: 15_000, message: 'its frames keep rising' }).toBeGreaterThan(at);
    return id;
  }

  // Five luminance samples of the watched <video> over two seconds, each a
  // 64x36 grid, with the standard deviation of each.
  async function samples(page: Page): Promise<{ stds: number[]; grids: number[][] }> {
    const grids: number[][] = [];
    for (let i = 0; i < 5; i++) {
      if (i > 0) await page.waitForTimeout(500);
      grids.push(
        await page.evaluate(() => {
          const video = document.querySelector<HTMLVideoElement>('[aria-label="alice\'s screen"]');
          if (video === null || video.videoWidth === 0) throw new Error('no picture to sample');
          const canvas = document.createElement('canvas');
          canvas.width = 64;
          canvas.height = 36;
          const ctx = canvas.getContext('2d');
          if (ctx === null) throw new Error('no canvas');
          ctx.drawImage(video, 0, 0, 64, 36);
          const px = ctx.getImageData(0, 0, 64, 36).data;
          const lum: number[] = [];
          for (let j = 0; j < px.length; j += 4) lum.push(0.2126 * (px[j] ?? 0) + 0.7152 * (px[j + 1] ?? 0) + 0.0722 * (px[j + 2] ?? 0));
          return lum;
        }),
      );
    }
    const stds = grids.map((g) => {
      const mean = g.reduce((a, b) => a + b, 0) / g.length;
      return Math.sqrt(g.reduce((a, b) => a + (b - mean) ** 2, 0) / g.length);
    });
    return { stds, grids };
  }
  const moved = (a: number[], b: number[]) => a.some((v, i) => Math.abs(v - (b[i] ?? 0)) > movedBy);

  test('the picture a viewer receives is not flat, and a stopped share started again is a new publication', async ({ browser }) => {
    test.setTimeout(150_000);
    const before = (await shareRows()).length;
    const a = await enter(browser, vw, vw.alice);
    const c = await enter(browser, vw, vw.carol);
    try {
      await joinVoice(a, 'Join voice');
      await joinVoice(c, 'Watch only');

      await share(a);
      await expect.poll(() => videoSids(vw.alice), { timeout: 15_000 }).toHaveLength(1);
      const first = (await videoSids(vw.alice))[0] ?? '';
      await watch(c);
      const firstStream = await framesRise(c, new Set());

      const { stds, grids } = await samples(c.page);
      for (const [i, s] of stds.entries()) expect(s, `luminance deviation of sample ${String(i)}`).toBeGreaterThan(flatBelow);
      const differing = grids.filter((g, i) => i > 0 && moved(grids[i - 1] ?? [], g)).length;
      expect(differing, 'samples that differ from the one before').toBeGreaterThanOrEqual(2);

      // Stop: the first publication leaves LiveKit and the viewer's tile.
      await voiceRegion(a.page).getByRole('button', { name: 'Stop sharing' }).click();
      await expect(voiceRegion(a.page)).toContainText('You stopped sharing.');
      await expect(shareView(c.page)).toHaveCount(0, { timeout: 15_000 });
      await expect.poll(() => videoSids(vw.alice), { timeout: 15_000 }).toEqual([]);

      // Again: a new publication, which the viewer watches afresh.
      await share(a);
      await expect.poll(() => videoSids(vw.alice), { timeout: 15_000 }).toHaveLength(1);
      const second = (await videoSids(vw.alice))[0] ?? '';
      expect(second, 'the second share is a publication of its own').not.toBe(first);
      await watch(c);
      // A stream of its own, not the first share's, decoding frames.
      const secondStream = await framesRise(c, new Set([firstStream]));
      expect(secondStream).not.toBe(firstStream);
      expect(await videoSids(vw.alice), 'only the new screen publication is listed').toEqual([second]);

      await voiceRegion(a.page).getByRole('button', { name: 'Stop sharing' }).click();
      await expect(shareView(c.page)).toHaveCount(0, { timeout: 15_000 });
      const me = `principal:${String(vw.alice.id)}`;
      await expect
        .poll(async () => (await shareRows()).slice(before).map(rowName), { timeout: 15_000 })
        .toEqual([`${me} voice_share_started`, `${me} voice_share_stopped self`, `${me} voice_share_started`, `${me} voice_share_stopped self`]);
      for (const p of [a, c]) await expectClean(vw, p);
    } finally {
      for (const p of [a, c]) await leave(p);
    }
  });

  test('share_screen off for members before a share: bob is refused, his page says so and nothing is audited as started', async ({ browser }) => {
    const before = (await shareRows()).length;
    const b = await enter(browser, vw, vw.bob);
    try {
      await joinVoice(b, 'Join voice');
      await withShareOff(async () => {
        const claim = await vw.api(vw.bob.token, 'POST', `/v1/channels/${vw.channel}/voice/share`);
        expect(claim.status, claim.text).toBe(403);
        expect((JSON.parse(claim.text) as { code?: string }).code).toBe('voice_not_allowed');

        // The page offers Share screen whatever the permission (it knows only
        // what the browser can do) and explains after the refusal.
        const voice = voiceRegion(b.page);
        await voice.getByRole('button', { name: 'Share screen' }).click();
        await expect(voice).toContainText('Your share could not start.', { timeout: 15_000 });
        await expect(voice.getByRole('button', { name: 'Stop sharing' })).toHaveCount(0);
        await expect(voice.getByRole('button', { name: 'Share screen' })).toBeVisible();
        expect(await videoSids(vw.bob)).toEqual([]);
      });
      expect((await shareRows()).slice(before).map(rowName), 'no share was audited').toEqual([]);
      await expectClean(vw, b);
    } finally {
      await leave(b);
    }
  });

  test('share_screen turned off during a share ends it: the viewer loses the tile and the frames, LiveKit the track, and the audit says access_lost', async ({ browser }) => {
    test.setTimeout(150_000);
    const before = (await shareRows()).length;
    const a = await enter(browser, vw, vw.alice);
    const c = await enter(browser, vw, vw.carol);
    try {
      await joinVoice(a, 'Join voice');
      await joinVoice(c, 'Watch only');
      await share(a);
      await watch(c);
      const stream = await framesRise(c, new Set());
      await expect.poll(() => videoSids(vw.alice), { timeout: 15_000 }).toHaveLength(1);

      let framesAtEnd = 0;
      await withShareOff(async () => {
        // Today's words for access_lost, which say voice was lost when only
        // share_screen was (a UX follow-up, not changed here).
        await expect(voiceRegion(a.page)).toContainText('Your share ended: you no longer have voice in this channel.', { timeout: endsWithin });
        await expect(voiceRegion(a.page).getByRole('button', { name: 'Stop sharing' })).toHaveCount(0);
        await expect(shareView(c.page)).toHaveCount(0, { timeout: endsWithin });
        await expect.poll(() => screenTracks(vw.alice), { timeout: endsWithin }).toEqual([]);
        framesAtEnd = (await videoFrames(c)).get(stream) ?? 0;
      });
      const me = `principal:${String(vw.alice.id)}`;
      const rows = (await shareRows()).slice(before).map(rowName);
      expect(rows).toEqual([`${me} voice_share_started`, `${me} voice_share_stopped access_lost`]);
      // The ended stream decoded nothing more while the rows were read.
      expect((await videoFrames(c)).get(stream) ?? 0, 'frames after the share ended').toBeLessThanOrEqual(framesAtEnd);
      // Back as found: alice may share again.
      await expect(voiceRegion(a.page).getByRole('button', { name: 'Share screen' })).toBeEnabled();
      for (const p of [a, c]) await expectClean(vw, p);
    } finally {
      for (const p of [a, c]) await leave(p);
    }
  });
});
