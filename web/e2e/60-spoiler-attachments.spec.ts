// Spoiler attachments (issue #1500): a file whose stored name starts with the
// prefix of pkg/schema is hidden until the viewer presses Reveal, and until
// then only its metadata is read. The mark is presentation: who may fetch the
// file is decided by the file routes and the message's audience, as ever.
import type { Page } from '@playwright/test';
import { createCredentialResponseV1, createPrincipalResponse } from '../src/schema/credential';
import { SPOILER_FILE_PREFIX } from '../src/schema/file';
import { listNetsResponseV1 } from '../src/schema/net';
import { attach, history, post } from './api';
import { channelButton, expect, expectFits, messages, openChannel, phones, signIn, test } from './fixtures';
import { call, ok, type World } from './world';
import { watchTraffic } from './watch';

const buffer = (globalThis as unknown as { Buffer: {
  from(value: string, encoding: 'base64'): Uint8Array<ArrayBuffer>;
  from(value: Uint8Array): Uint8Array<ArrayBuffer>;
} }).Buffer;
const png = buffer.from(
  'iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAYAAAAfFcSJAAAAC0lEQVR4nGNgAAIAAAUAAXpeqz8AAAAASUVORK5CYII=',
  'base64',
);
const unique = (() => {
  let next = 0;
  return (label: string) => `c1500-${label}-${Date.now().toString(36)}-${String(++next)}`;
})();
const spoiler = (name: string) => `${SPOILER_FILE_PREFIX}${name}`;
const seed = (filename: string) => ({ filename, mediaType: 'image/png', bytes: png });

async function done(world: World, method: 'POST' | 'PUT' | 'DELETE', path: string, body?: unknown): Promise<void> {
  const reply = await call(world, world.tokens.operator, method, path, body);
  if (reply.status < 200 || reply.status > 299) throw new Error(`${method} ${path}: status ${String(reply.status)}`);
}

async function person(world: World, secrets: string[], name: string, channels: readonly string[]) {
  const made = await ok(world, world.tokens.operator, 'POST', '/v0/principals', createPrincipalResponse, { kind: 'human', name });
  const issued = await ok(world, world.tokens.operator, 'POST', `/v1/principals/${String(made.principal.id)}/credentials`, createCredentialResponseV1, { label: name });
  secrets.push(issued.token);
  for (const channel of channels) await done(world, 'PUT', `/v1/channels/${encodeURIComponent(channel)}/members/${String(made.principal.id)}`);
  return { id: made.principal.id, name, token: issued.token };
}

async function privateChannel(world: World, label: string): Promise<string> {
  const channel = unique(label);
  await done(world, 'POST', '/v0/channels', { name: channel });
  return channel;
}

// fileReads is every GET of a file's bytes or metadata this page makes.
function fileReads(page: Page): { meta: string[]; bytes: string[] } {
  const seen = { meta: [] as string[], bytes: [] as string[] };
  page.on('request', (request) => {
    if (request.method() !== 'GET') return;
    const path = new URL(request.url()).pathname;
    if (/^\/v2\/files\/\d+\/meta$/.test(path)) seen.meta.push(path);
    else if (/^\/v2\/files\/\d+$/.test(path)) seen.bytes.push(path);
  });
  return seen;
}

async function trackObjectURLs(page: Page): Promise<void> {
  await page.addInitScript(() => {
    const active = new Set<string>();
    const create = URL.createObjectURL.bind(URL);
    const revoke = URL.revokeObjectURL.bind(URL);
    URL.createObjectURL = (blob: Blob) => {
      const url = create(blob);
      active.add(url);
      return url;
    };
    URL.revokeObjectURL = (url: string) => {
      active.delete(url);
      revoke(url);
    };
    Object.assign(window, { spoilerObjectURLs: active });
  });
}

function activeObjectURLs(page: Page): Promise<number> {
  return page.evaluate(() => (window as unknown as { spoilerObjectURLs: Set<string> }).spoilerObjectURLs.size);
}

const card = (page: Page, shown: string) => page.locator('figure.attachment').filter({ hasText: shown });
const reveal = (page: Page, shown: string) => page.getByRole('button', { name: `Reveal ${shown}` });
const hide = (page: Page, shown: string) => page.getByRole('button', { name: `Hide ${shown}` });
const enlarge = (page: Page, shown: string) => page.getByRole('button', { name: `Enlarge ${shown}` });

test('a marked image is hidden from its recipient until Reveal, which is forgotten on a reload', async ({ meet, secrets, world }) => {
  const channel = await privateChannel(world, 'flow');
  const other = await privateChannel(world, 'flow-other');
  const alice = await person(world, secrets, unique('alice'), [channel]);
  const bob = await person(world, secrets, unique('bob'), [channel, other]);
  const shown = `${unique('pic')}.png`;

  const alicePage = (await meet()).page;
  const bobPage = (await meet()).page;
  await trackObjectURLs(bobPage);
  const reads = fileReads(bobPage);
  await Promise.all([signIn(alicePage, world, alice.token, alice.name), signIn(bobPage, world, bob.token, bob.name)]);
  await channelButton(alicePage, channel).click();
  await channelButton(bobPage, channel).click();

  const uploads: string[] = [];
  alicePage.on('request', (request) => {
    if (request.method() === 'POST' && new URL(request.url()).pathname.endsWith('/files')) uploads.push(new URL(request.url()).searchParams.get('filename') ?? '');
  });
  await alicePage.getByLabel('Choose files').setInputFiles({ name: shown, mimeType: 'image/png', buffer: png });
  const mark = alicePage.getByRole('button', { name: `Mark ${shown} as spoiler` });
  await expect(mark).toHaveAttribute('aria-pressed', 'false');
  await mark.click();
  await expect(mark).toHaveAttribute('aria-pressed', 'true');
  // The sender's own draft preview stays visible.
  await expect(alicePage.locator('li.draft-attachment img')).toHaveCount(1);
  await alicePage.getByRole('button', { name: 'Send', exact: true }).click();

  const hidden = card(bobPage, shown);
  await expect(hidden).toContainText(new RegExp(`Spoiler: ${shown.replace('.', '\\.')}, image/png, \\d+ B`));
  await expect(reveal(bobPage, shown)).toHaveAttribute('aria-expanded', 'false');
  await expect(hidden).not.toContainText(SPOILER_FILE_PREFIX);
  expect(uploads).toEqual([spoiler(shown)]);
  const stored = (await history(world, bob.token, channel)).find((message) => message.body === spoiler(shown));
  expect(stored, 'an image-only message carries the stored name as its body').toBeDefined();

  // Nothing but metadata, no image and no blob URL, before Reveal.
  await expect.poll(() => reads.meta.length).toBeGreaterThan(0);
  expect(reads.bytes).toEqual([]);
  await expect(hidden.locator('img')).toHaveCount(0);
  expect(await activeObjectURLs(bobPage)).toBe(0);
  await expect(messages(bobPage).getByText(spoiler(shown))).toHaveCount(0);

  await reveal(bobPage, shown).click();
  await expect(enlarge(bobPage, shown)).toBeVisible();
  await expect(hidden.locator('img[src^="blob:"]')).toHaveCount(1);
  expect(reads.bytes).toHaveLength(1);
  await expect(hide(bobPage, shown)).toHaveAttribute('aria-expanded', 'true');
  await expect(hide(bobPage, shown)).toBeFocused();

  await hide(bobPage, shown).click();
  await expect(hidden.locator('img')).toHaveCount(0);
  await expect.poll(() => activeObjectURLs(bobPage)).toBe(0);
  await expect(reveal(bobPage, shown)).toHaveAttribute('aria-expanded', 'false');

  // A reveal is forgotten on a fresh render.
  await reveal(bobPage, shown).click();
  await expect(enlarge(bobPage, shown)).toBeVisible();
  expect(reads.bytes).toHaveLength(2);
  const metasBefore = reads.meta.length;
  await bobPage.reload();
  await expect(reveal(bobPage, shown)).toBeVisible();
  await expect(card(bobPage, shown).locator('img')).toHaveCount(0);
  // The remounted hidden card read /meta again and fetched no bytes.
  expect(reads.meta.length).toBeGreaterThan(metasBefore);
  expect(reads.bytes).toHaveLength(2);
  // A channel switch is a fresh render too.
  await reveal(bobPage, shown).click();
  await expect(enlarge(bobPage, shown)).toBeVisible();
  expect(reads.bytes).toHaveLength(3);
  const metasAtSwitch = reads.meta.length;
  await channelButton(bobPage, other).click();
  await channelButton(bobPage, channel).click();
  await expect(reveal(bobPage, shown)).toBeVisible();
  expect(reads.meta.length).toBeGreaterThan(metasAtSwitch);
  expect(reads.bytes).toHaveLength(3);
  await expect.poll(() => activeObjectURLs(bobPage)).toBe(0);
});

test('spoofed content in a spoiler is a file chip after Reveal and runs nothing', async ({ meet, secrets, world }) => {
  const channel = await privateChannel(world, 'spoof');
  const alice = await person(world, secrets, unique('alice'), [channel]);
  const bob = await person(world, secrets, unique('bob'), [channel]);
  const shown = `${unique('spoof')}.png`;
  const html = '<html><script>window.spoofRan = true; alert(1);</script></html>';
  await attach(world, alice.token, channel, spoiler(shown), [{ filename: spoiler(shown), mediaType: 'image/png', bytes: buffer.from(new TextEncoder().encode(html)) }]);

  const { page } = await meet();
  const dialogs: string[] = [];
  page.on('dialog', (dialog) => {
    dialogs.push(dialog.message());
    void dialog.dismiss();
  });
  await signIn(page, world, bob.token, bob.name);
  await channelButton(page, channel).click();
  await expect(reveal(page, shown)).toBeVisible();
  await reveal(page, shown).click();
  const chip = page.locator('figure.attachment-file').filter({ hasText: shown });
  await expect(chip).toBeVisible();
  await expect(chip.locator('.attachment-size')).toBeVisible();
  await expect(chip.getByRole('button', { name: 'Download' })).toBeVisible();
  await expect(chip.locator('img')).toHaveCount(0);
  await expect(hide(page, shown)).toHaveAttribute('aria-expanded', 'true');
  expect(await page.evaluate(() => (window as unknown as { spoofRan?: boolean }).spoofRan)).toBeUndefined();
  expect(dialogs).toEqual([]);
});

test('a spoiler in a whisper, a net and a private channel reaches only its audience', async ({ meet, secrets, world }) => {
  const channel = await privateChannel(world, 'audience');
  const alice = await person(world, secrets, unique('alice'), [channel]);
  const bob = await person(world, secrets, unique('bob'), [channel]);
  const carol = await person(world, secrets, unique('carol'), [channel]);
  await done(world, 'POST', `/v1/channels/${encodeURIComponent(channel)}/nets`, { name: 'blue' });
  const nets = await ok(world, world.tokens.operator, 'GET', `/v1/channels/${encodeURIComponent(channel)}/nets`, listNetsResponseV1);
  const net = nets.nets.find((candidate) => candidate.name === 'blue');
  if (net === undefined) throw new Error('the net was not created');
  for (const id of [alice.id, bob.id]) await done(world, 'PUT', `/v1/channels/${encodeURIComponent(channel)}/nets/blue/members/${String(id)}`, { role: 'member' });

  const whisperName = `${unique('whisper')}.png`;
  const netName = `${unique('net')}.png`;
  const whisper = await attach(world, alice.token, channel, spoiler(whisperName), [seed(spoiler(whisperName))], { kind: 'principals', principal_ids: [bob.id] });
  const inNet = await attach(world, alice.token, channel, spoiler(netName), [seed(spoiler(netName))], { kind: 'net', net_id: net.id });
  const ids = [whisper, inNet].flatMap((message) => (message.payload?.data as { file_ids: number[] }).file_ids);

  const bobPage = (await meet()).page;
  const carolPage = (await meet()).page;
  const outsiderPage = (await meet()).page;
  await trackObjectURLs(bobPage);
  const carolTraffic = watchTraffic(carolPage);
  const outsiderTraffic = watchTraffic(outsiderPage);
  const carolReads = fileReads(carolPage);
  const outsiderReads = fileReads(outsiderPage);
  await Promise.all([
    signIn(bobPage, world, bob.token, bob.name),
    signIn(carolPage, world, carol.token, carol.name),
    signIn(outsiderPage, world, world.tokens.browser, 'browser-test'),
  ]);
  await channelButton(bobPage, channel).click();
  await channelButton(carolPage, channel).click();
  await expect(channelButton(outsiderPage, channel)).toHaveCount(0);

  await expect(reveal(bobPage, whisperName)).toBeVisible();
  await expect(reveal(bobPage, netName)).toBeVisible();
  const marker = unique('marker');
  await post(world, alice.token, channel, marker);
  await expect(messages(carolPage).getByText(marker)).toBeVisible();
  await expect(messages(bobPage).getByText(marker)).toBeVisible();

  for (const text of [await carolTraffic.text(), await outsiderTraffic.text()]) {
    for (const name of [whisperName, netName, spoiler(whisperName), spoiler(netName)]) expect(text).not.toContain(name);
  }
  await expect(carolPage.locator('figure.attachment')).toHaveCount(0);
  expect([...carolReads.meta, ...carolReads.bytes, ...outsiderReads.meta, ...outsiderReads.bytes]).toEqual([]);
  for (const id of ids) {
    for (const token of [carol.token, world.tokens.browser]) {
      expect((await call(world, token, 'GET', `/v2/files/${String(id)}/meta`)).status).toBe(404);
      expect((await call(world, token, 'GET', `/v2/files/${String(id)}`)).status).toBe(404);
    }
  }

  // Bob reveals the whisper; losing the channel clears the revealed preview.
  await reveal(bobPage, whisperName).click();
  await expect(enlarge(bobPage, whisperName)).toBeVisible();
  await expect.poll(() => activeObjectURLs(bobPage)).toBe(1);
  await done(world, 'DELETE', `/v1/channels/${encodeURIComponent(channel)}/members/${String(bob.id)}`);
  await expect(channelButton(bobPage, channel)).toHaveCount(0);
  await expect(bobPage.locator('figure.attachment')).toHaveCount(0);
  await expect.poll(() => activeObjectURLs(bobPage)).toBe(0);
  expect((await call(world, bob.token, 'GET', `/v2/files/${String(ids[0])}`)).status).toBe(404);
});

test('on a phone, Reveal, Hide and Download work by tap with 44 px targets', async ({ meet, secrets, world }) => {
  const channel = await privateChannel(world, 'phone');
  const alice = await person(world, secrets, unique('alice'), [channel]);
  const bob = await person(world, secrets, unique('bob'), [channel]);
  const shown = `${unique('phone')}.png`;
  await attach(world, alice.token, channel, spoiler(shown), [seed(spoiler(shown))]);

  const { page } = await meet(phones[0][1]);
  const reads = fileReads(page);
  await signIn(page, world, bob.token, bob.name);
  await openChannel(page, channel);
  await expect(reveal(page, shown)).toBeVisible();
  await expectFits(page, phones[0][0]);
  const big = async (locator: ReturnType<typeof reveal>) => {
    const box = await locator.boundingBox();
    expect(box?.height ?? 0).toBeGreaterThanOrEqual(44);
    expect(box?.width ?? 0).toBeGreaterThanOrEqual(44);
  };
  await big(reveal(page, shown));
  await big(card(page, shown).getByRole('button', { name: 'Download' }));
  expect(reads.bytes).toEqual([]);

  await reveal(page, shown).tap();
  await expect(enlarge(page, shown)).toBeVisible();
  await expectFits(page, phones[0][0]);
  await big(hide(page, shown));
  await hide(page, shown).tap();
  await expect(card(page, shown).locator('img')).toHaveCount(0);

  const downloaded = page.waitForEvent('download');
  await card(page, shown).getByRole('button', { name: 'Download' }).tap();
  expect((await downloaded).suggestedFilename()).toBe(spoiler(shown));
  // Download never reveals the preview.
  await expect(card(page, shown).locator('img')).toHaveCount(0);
  await expect(reveal(page, shown)).toHaveAttribute('aria-expanded', 'false');
});

test('a file uploaded under the prefix through the API is a spoiler; a lower-case prefix is not', async ({ meet, secrets, world }) => {
  const channel = await privateChannel(world, 'api');
  const alice = await person(world, secrets, unique('alice'), [channel]);
  const bob = await person(world, secrets, unique('bob'), [channel]);
  const marked = `${unique('marked')}.png`;
  const plain = `${unique('plain')}.png`;
  await attach(world, alice.token, channel, unique('marked-caption'), [seed(spoiler(marked))]);
  await attach(world, alice.token, channel, unique('plain-caption'), [seed(`${SPOILER_FILE_PREFIX.toLowerCase()}${plain}`)]);

  const { page } = await meet();
  await signIn(page, world, bob.token, bob.name);
  await channelButton(page, channel).click();
  await expect(reveal(page, marked)).toBeVisible();
  await expect(enlarge(page, `${SPOILER_FILE_PREFIX.toLowerCase()}${plain}`)).toBeVisible();
  await expect(page.getByRole('button', { name: /^Reveal / })).toHaveCount(1);
});

test('an edit, a reaction and a search result never fetch or reveal a spoiler', async ({ meet, secrets, world }) => {
  const channel = await privateChannel(world, 'live');
  const alice = await person(world, secrets, unique('alice'), [channel]);
  const bob = await person(world, secrets, unique('bob'), [channel]);
  const shown = `${unique('live')}.png`;
  const tag = unique('tag').replace(/-/g, '');
  const posted = await attach(world, alice.token, channel, `look ${tag}`, [seed(spoiler(shown))]);

  const { page } = await meet();
  const reads = fileReads(page);
  await signIn(page, world, bob.token, bob.name);
  await channelButton(page, channel).click();
  await expect(reveal(page, shown)).toBeVisible();
  await expect.poll(() => reads.meta.length).toBeGreaterThan(0);

  const base = `/v2/channels/${encodeURIComponent(channel)}/messages/${String(posted.id)}`;
  expect((await call(world, alice.token, 'PUT', `${base}/body`, { body: `look again ${tag}` })).status).toBeLessThan(300);
  await expect(messages(page).getByText(`look again ${tag}`)).toBeVisible();
  expect((await call(world, alice.token, 'PUT', `${base}/reactions/${encodeURIComponent('👍')}`)).status).toBeLessThan(300);
  await expect(page.locator('.reactions')).toBeVisible();
  await expect(reveal(page, shown)).toHaveAttribute('aria-expanded', 'false');
  await expect(card(page, shown).locator('img')).toHaveCount(0);
  expect(reads.bytes).toEqual([]);

  // A search result is a fresh render of the message: hidden, metadata only.
  await page.getByRole('button', { name: 'Search', exact: true }).click();
  await page.getByRole('searchbox', { name: `Messages in #${channel}` }).fill(tag);
  await page.getByRole('button', { name: 'Search', exact: true }).click();
  const results = page.getByRole('list', { name: 'Search results' });
  await expect(results.getByText(`look again ${tag}`)).toBeVisible();
  await expect(results.getByRole('button', { name: `Reveal ${shown}` })).toBeVisible();
  await expect(results.locator('img')).toHaveCount(0);
  expect(reads.bytes).toEqual([]);
});
