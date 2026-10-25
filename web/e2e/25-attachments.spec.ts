import type { Page } from '@playwright/test';
import { createCredentialResponseV1, createPrincipalResponse } from '../src/schema/credential';
import { fileAttachmentPayloadSchema, fileAttachmentV1 } from '../src/schema/file';
import { joinGuestResponseV1, joinSettingsResponseV1 } from '../src/schema/guest';
import type { MessageV2 } from '../src/schema/message';
import { listNetsResponseV1 } from '../src/schema/net';
import { attach, history, makeInvite, post } from './api';
import { channelButton, expect, expectFits, messages, phones, signIn, statusLine, test } from './fixtures';
import { call, ok, type World } from './world';

const buffer = (globalThis as unknown as { Buffer: {
  from(value: string, encoding: 'base64'): Uint8Array<ArrayBuffer>;
  from(value: Uint8Array): Uint8Array<ArrayBuffer>;
} }).Buffer;
const png = buffer.from(
  'iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAYAAAAfFcSJAAAAC0lEQVR4nGNgAAIAAAUAAXpeqz8AAAAASUVORK5CYII=',
  'base64',
);
const pickerFile = { name: 'browser-image.png', mimeType: 'image/png', buffer: png };
const unique = (() => {
  let next = 0;
  return (label: string) => `a762-${label}-${Date.now().toString(36)}-${String(++next)}`;
})();

async function done(world: World, method: 'POST' | 'PUT' | 'DELETE', path: string, body?: unknown): Promise<void> {
  const reply = await call(world, world.tokens.operator, method, path, body);
  if (reply.status < 200 || reply.status > 299) throw new Error(`${method} ${path}: status ${String(reply.status)}`);
}

async function person(world: World, secrets: string[], name: string, channels: readonly string[] = []) {
  const made = await ok(world, world.tokens.operator, 'POST', '/v0/principals', createPrincipalResponse, { kind: 'human', name });
  const issued = await ok(world, world.tokens.operator, 'POST', `/v1/principals/${String(made.principal.id)}/credentials`, createCredentialResponseV1, { label: name });
  secrets.push(issued.token);
  for (const channel of channels) await done(world, 'PUT', `/v1/channels/${encodeURIComponent(channel)}/members/${String(made.principal.id)}`);
  return { id: made.principal.id, name, token: issued.token, credentialId: issued.credential.id };
}

async function invitedGuest(world: World, channel: string) {
  await ok(world, world.tokens.operator, 'PUT', '/v1/join/settings', joinSettingsResponseV1, { guests_enabled: true, open_joining: 'off', open_channels: [] });
  try {
    const invite = await makeInvite(world, world.tokens.operator, { kind: 'open', channels: [channel], voice: false, join_as: 'guest' });
    return await ok(world, '', 'POST', '/v1/invites/redeem-guest', joinGuestResponseV1, { code: invite.code, label: 'Late' });
  } finally {
    await ok(world, world.tokens.operator, 'PUT', '/v1/join/settings', joinSettingsResponseV1, { guests_enabled: false, open_joining: 'off', open_channels: [] });
  }
}

function fileIds(message: MessageV2): number[] {
  if (message.payload?.schema !== fileAttachmentPayloadSchema) throw new Error('message has no file attachment payload');
  const problems = fileAttachmentV1(message.payload.data);
  if (problems.length > 0) throw new Error(problems.join('; '));
  return (message.payload.data as { file_ids: number[] }).file_ids;
}

async function expectDenied(world: World, token: string, id: number, status = 404): Promise<void> {
  expect((await call(world, token, 'GET', `/v2/files/${String(id)}/meta`)).status).toBe(status);
  expect((await call(world, token, 'GET', `/v2/files/${String(id)}`)).status).toBe(status);
}

function fileRequests(page: Page): string[] {
  const seen: string[] = [];
  page.on('request', (request) => {
    if (request.method() === 'GET' && /^\/v2\/files\/\d+(?:\/meta)?$/.test(new URL(request.url()).pathname)) seen.push(request.url());
  });
  return seen;
}

function requestedFileIds(requests: readonly string[]): Set<number> {
  return new Set(requests.map((url) => Number(/^\/v2\/files\/(\d+)(?:\/meta)?$/.exec(new URL(url).pathname)?.[1])));
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
    Object.assign(window, { attachmentObjectURLs: active });
  });
}

function activeObjectURLs(page: Page): Promise<number> {
  return page.evaluate(() => (window as unknown as { attachmentObjectURLs: Set<string> }).attachmentObjectURLs.size);
}

function activeDraftObjectURLs(page: Page): Promise<number> {
  return page.evaluate(() => {
    const active = (window as unknown as { attachmentObjectURLs: Set<string> }).attachmentObjectURLs;
    return [...document.querySelectorAll<HTMLImageElement>('li.draft-attachment img')].filter((img) => active.has(img.src)).length;
  });
}

async function pasteFiles(page: Page, files: readonly { name: string; type: string; bytes: Uint8Array<ArrayBuffer> }[]): Promise<void> {
  const incoming = files.map((file) => ({ ...file, bytes: Array.from(file.bytes) }));
  await page.getByLabel('Message #general').evaluate((textarea, files) => {
    const transfer = new DataTransfer();
    for (const file of files) transfer.items.add(new File([new Uint8Array(file.bytes)], file.name, { type: file.type }));
    textarea.dispatchEvent(new ClipboardEvent('paste', { bubbles: true, cancelable: true, clipboardData: transfer }));
  }, incoming);
}

async function dropFiles(page: Page, files: readonly { name: string; type: string; bytes: Uint8Array<ArrayBuffer> }[]): Promise<void> {
  const incoming = files.map((file) => ({ ...file, bytes: Array.from(file.bytes) }));
  await page.locator('form.composer').evaluate((form, files) => {
    const transfer = new DataTransfer();
    for (const file of files) transfer.items.add(new File([new Uint8Array(file.bytes)], file.name, { type: file.type }));
    form.dispatchEvent(new DragEvent('drop', { bubbles: true, cancelable: true, dataTransfer: transfer }));
  }, incoming);
}

function text(value: string): Uint8Array<ArrayBuffer> {
  return buffer.from(new TextEncoder().encode(value));
}

function pngWithDimensions(width: number, height: number): Uint8Array<ArrayBuffer> {
  const bytes = Uint8Array.from(png);
  const view = new DataView(bytes.buffer);
  view.setUint32(16, width);
  view.setUint32(20, height);
  return buffer.from(bytes);
}

async function jpeg(page: Page): Promise<Uint8Array<ArrayBuffer>> {
  const encoded = await page.evaluate(() => {
    const canvas = document.createElement('canvas');
    canvas.width = 3;
    canvas.height = 2;
    const context = canvas.getContext('2d');
    if (context === null) throw new Error('no canvas');
    context.fillStyle = '#2b7';
    context.fillRect(0, 0, 3, 2);
    return canvas.toDataURL('image/jpeg').split(',')[1] ?? '';
  });
  return buffer.from(encoded, 'base64');
}

test('picker previews without uploading, sends a captioned image, and restores it from history', async ({ meet, world }) => {
  const { page } = await meet();
  await signIn(page, world, world.tokens.browser, 'browser-test');
  await expect(page.getByLabel('Message #general')).toBeEnabled();
  const uploads: string[] = [];
  page.on('request', (request) => {
    if (request.method() === 'POST' && new URL(request.url()).pathname.endsWith('/files')) uploads.push(request.url());
  });

  await page.getByLabel('Choose images').setInputFiles(pickerFile);
  await expect(page.getByRole('list', { name: 'Images to send' }).getByText('browser-image.png')).toBeVisible();
  expect(uploads).toEqual([]);
  const caption = unique('browser-caption');
  await page.getByLabel('Message #general').fill(caption);
  await page.getByRole('button', { name: 'Send', exact: true }).click();
  await expect(messages(page).getByText(caption)).toHaveCount(1);
  await expect(page.getByRole('button', { name: 'Enlarge browser-image.png' })).toBeVisible();
  expect(uploads).toHaveLength(1);

  const stored = (await history(world, world.tokens.browser, 'general')).filter((message) => message.body === caption);
  expect(stored).toHaveLength(1);
  expect(stored[0]?.payload).toEqual({ schema: 'conch.file_attachment.v1', data: { file_ids: [expect.any(Number)] } });
  await page.reload();
  await expect(messages(page).getByText(caption)).toHaveCount(1);
  await expect(page.getByRole('button', { name: 'Enlarge browser-image.png' })).toBeVisible();
});

test('image and text drafts survive channel and audience switches without uploading', async ({ meet, world }) => {
  const { page } = await meet();
  await signIn(page, world, world.tokens.browser, 'browser-test');
  await expect(page.getByLabel('Message #general')).toBeEnabled();
  const uploads: string[] = [];
  page.on('request', (request) => {
    if (request.method() === 'POST' && new URL(request.url()).pathname.endsWith('/files')) uploads.push(request.url());
  });
  await page.getByLabel('Choose images').setInputFiles({ ...pickerFile, name: 'switch.png' });
  await expect(page.getByText('switch.png')).toBeVisible();
  await page.getByLabel('Message #general').fill('unsent switch caption');

  await channelButton(page, 'nets-demo').click();
  await expect(page.getByLabel('Message #nets-demo')).toBeVisible();
  await expect(page.getByText('switch.png')).toHaveCount(0);
  await channelButton(page, 'general').click();
  await expect(page.getByText('switch.png')).toBeVisible();
  await expect(page.getByLabel('Message #general')).toHaveValue('unsent switch caption');

  await page.getByLabel('To:').selectOption({ label: 'Whisper to…' });
  await expect(page.getByText('switch.png')).toHaveCount(0);
  await page.getByLabel('To:').selectOption('channel');
  await expect(page.getByText('switch.png')).toBeVisible();
  await expect(page.getByLabel('Message #general')).toHaveValue('unsent switch caption');
  expect(uploads).toEqual([]);
});

test('mobile picker sends an image-only message with a filename fallback', async ({ meet, world }) => {
  const { page } = await meet(phones[0][1]);
  await signIn(page, world, world.tokens.browser, 'browser-test');
  await expect(page.getByLabel('Message #general')).toBeEnabled();
  const name = `${unique('mobile')}.png`;
  await page.getByLabel('Choose images').setInputFiles({ ...pickerFile, name });
  await expect(page.getByText(name)).toBeVisible();
  await expectFits(page, phones[0][0]);
  await page.getByRole('button', { name: 'Send', exact: true }).click();
  await expect(messages(page).getByText(name)).toHaveCount(1);
  await expect(page.getByRole('button', { name: `Enlarge ${name}` })).toBeVisible();
});

test('paste and drop share removable drafts, and a cancelled upload retries without duplicate posts', async ({ meet, world }) => {
  const { page } = await meet();
  await trackObjectURLs(page);
  await signIn(page, world, world.tokens.browser, 'browser-test');
  await expect(page.getByLabel('Message #general')).toBeEnabled();
  const composer = page.getByLabel('Message #general');
  const ordinaryPaste = await composer.evaluate((textarea) => {
    const transfer = new DataTransfer();
    transfer.setData('text/plain', 'ordinary text');
    const event = new ClipboardEvent('paste', { bubbles: true, cancelable: true, clipboardData: transfer });
    textarea.dispatchEvent(event);
    return event.defaultPrevented;
  });
  expect(ordinaryPaste).toBe(false);
  await expect(page.getByRole('list', { name: 'Images to send' })).toHaveCount(0);

  const drafts = page.getByRole('list', { name: 'Images to send' });
  await pasteFiles(page, [{ name: 'pasted.png', type: 'image/png', bytes: png }]);
  await expect(drafts.getByText('pasted.png')).toBeVisible();
  await dropFiles(page, [{ name: 'dropped.png', type: 'image/png', bytes: png }]);
  await expect(drafts.getByText('dropped.png')).toBeVisible();
  await expect(drafts.locator('li.draft-attachment img')).toHaveCount(2);
  await expect.poll(() => activeDraftObjectURLs(page)).toBe(2);
  await page.locator('li.draft-attachment').filter({ hasText: 'pasted.png' }).getByRole('button', { name: /^Remove / }).click();
  await expect(page.getByText('pasted.png')).toHaveCount(0);
  await expect.poll(() => activeDraftObjectURLs(page)).toBe(1);

  let uploads = 0;
  let release = () => {};
  const gate = new Promise<void>((resolve) => { release = resolve; });
  await page.route('**/v2/channels/general/files?*', async (route) => {
    uploads++;
    if (uploads === 1) await gate;
    await route.abort('connectionreset').catch(() => undefined);
  });
  const body = unique('cancel-retry');
  await composer.fill(body);
  await page.getByRole('button', { name: 'Send', exact: true }).click();
  await expect(page.getByLabel('To:')).toBeDisabled();
  await page.getByRole('button', { name: 'Cancel upload of dropped.png' }).click();
  await expect(page.getByText('Upload cancelled.')).toBeVisible();
  await expect(page.getByLabel('To:')).toBeEnabled();
  release();
  await page.unroute('**/v2/channels/general/files?*');
  await page.getByRole('button', { name: 'Retry upload of dropped.png' }).click();
  await expect(page.getByRole('button', { name: 'Send', exact: true })).toBeEnabled();
  await expect(page.getByRole('progressbar')).toHaveCount(0);
  await page.getByRole('button', { name: 'Send', exact: true }).click();
  await expect(messages(page).getByRole('listitem').filter({ hasText: body }).filter({ hasText: 'Sent' })).toHaveCount(1);
  expect((await history(world, world.tokens.browser, 'general')).filter((message) => message.body === body)).toHaveLength(1);
  expect(uploads).toBe(1);
});

test('content validation rejects active and corrupt formats and dimension bombs, while server refusals keep drafts', async ({ meet, world }) => {
  const { page } = await meet();
  await signIn(page, world, world.tokens.browser, 'browser-test');
  await expect(page.getByLabel('Message #general')).toBeEnabled();
  const picker = page.getByLabel('Choose images');
  const alert = page.locator('form.composer').getByRole('alert');
  const rejected = [
    { name: 'script.svg', mimeType: 'image/png', buffer: text('<svg xmlns="http://www.w3.org/2000/svg"><script>alert(1)</script></svg>'), line: 'script.svg: only PNG and JPEG images can be attached.' },
    { name: 'page.html', mimeType: 'image/jpeg', buffer: text('<html><img src=x onerror=alert(1)></html>'), line: 'page.html: only PNG and JPEG images can be attached.' },
    { name: 'moving.gif', mimeType: 'image/gif', buffer: text('GIF89a'), line: 'moving.gif: only PNG and JPEG images can be attached.' },
    { name: 'modern.webp', mimeType: 'image/webp', buffer: text('RIFF0000WEBPVP8 '), line: 'modern.webp: only PNG and JPEG images can be attached.' },
    { name: 'short.png', mimeType: 'image/png', buffer: png.slice(0, 20), line: 'short.png: the image is damaged or is not really a PNG or JPEG.' },
    { name: 'axis.png', mimeType: 'image/png', buffer: pngWithDimensions(8193, 1), line: 'axis.png: the image dimensions are too large.' },
    { name: 'pixels.png', mimeType: 'image/png', buffer: pngWithDimensions(5000, 5001), line: 'pixels.png: the image dimensions are too large.' },
    { name: 'decoder.png', mimeType: 'image/png', buffer: png.slice(0, 33), line: 'decoder.png: the image is damaged or is not really a PNG or JPEG.' },
  ];
  for (const { line, ...file } of rejected) {
    await picker.setInputFiles(file);
    await expect(alert).toHaveText(line);
    await expect(page.getByRole('list', { name: 'Images to send' })).toHaveCount(0);
  }

  const uploadTypes: (string | null)[] = [];
  page.on('request', (request) => {
    if (request.method() === 'POST' && new URL(request.url()).pathname.endsWith('/files')) uploadTypes.push(request.headers()['content-type'] ?? null);
  });
  const spoofed = `${unique('spoofed')}.svg`;
  await picker.setInputFiles({ name: spoofed, mimeType: 'text/html', buffer: png });
  await expect(page.getByText(spoofed)).toBeVisible();
  await page.getByRole('button', { name: 'Send', exact: true }).click();
  await expect(page.getByRole('button', { name: `Enlarge ${spoofed}` })).toBeVisible();
  expect(uploadTypes).toEqual(['image/png']);

  const oversized = new Uint8Array(10 * 1024 * 1024 + 1);
  oversized.set(png);
  const largeName = `${unique('server-large')}.png`;
  await picker.setInputFiles({ name: largeName, mimeType: 'image/png', buffer: buffer.from(oversized) });
  await expect(page.getByText(largeName)).toBeVisible();
  const largeCaption = unique('large-caption');
  await page.getByLabel('Message #general').fill(largeCaption);
  await page.getByRole('button', { name: 'Send', exact: true }).click();
  await expect(page.getByRole('alert', { name: '' }).filter({ hasText: 'a file may be at most 10485760 bytes' })).toBeVisible();
  await expect(page.getByLabel('Message #general')).toHaveValue(largeCaption);
  await expect(page.getByText(largeName)).toBeVisible();
  await page.getByRole('button', { name: `Remove ${largeName}` }).click();

  await page.route('**/v2/channels/general/files?*', async (route) => {
    await route.fulfill({ status: 403, contentType: 'application/json', body: JSON.stringify({ code: 'uploads_disabled', message: 'file uploads are disabled on this server' }) });
  });
  const disabledName = `${unique('disabled')}.png`;
  await picker.setInputFiles({ name: disabledName, mimeType: 'image/png', buffer: png });
  const disabledCaption = unique('disabled-caption');
  await page.getByLabel('Message #general').fill(disabledCaption);
  await page.getByRole('button', { name: 'Send', exact: true }).click();
  await expect(page.getByText('file uploads are disabled on this server')).toBeVisible();
  await expect(page.getByLabel('Message #general')).toHaveValue(disabledCaption);
  await expect(page.getByText(disabledName)).toBeVisible();
  await expect(page.getByRole('button', { name: `Retry upload of ${disabledName}` })).toBeVisible();
});

test('uncertain sends reconcile across complete history walks and never duplicate a post or upload', async ({ meet, world }) => {
  const { page } = await meet();
  await signIn(page, world, world.tokens.browser, 'browser-test');
  await expect(page.getByLabel('Message #general')).toBeEnabled();
  let uploads = 0;
  await page.route('**/v2/channels/general/files?*', async (route) => {
    uploads++;
    await route.continue();
  });
  const first = unique('lost-before-post');
  await page.route('**/v2/channels/general/messages', async (route) => {
    if (route.request().method() === 'POST' && route.request().postData()?.includes(first) === true) await route.abort('connectionreset');
    else await route.continue();
  });
  await page.getByLabel('Choose images').setInputFiles({ ...pickerFile, name: 'uncertain.png' });
  await page.getByLabel('Message #general').fill(first);
  await page.getByRole('button', { name: 'Send', exact: true }).click();
  const pending = messages(page).getByRole('listitem').filter({ hasText: first });
  await expect(pending).toContainText('Not confirmed: check the conversation before retrying');
  expect((await history(world, world.tokens.browser, 'general')).filter((message) => message.body === first)).toHaveLength(0);
  for (let n = 0; n < 105; n++) await post(world, world.tokens.nick, 'general', `${first}-history-${String(n)}`);
  await page.unroute('**/v2/channels/general/messages');
  await pending.getByRole('button', { name: 'Retry' }).click();
  await expect(messages(page).getByRole('listitem').filter({ hasText: first }).filter({ hasText: 'Sent' })).toHaveCount(1);
  await expect(page.getByRole('button', { name: 'Enlarge uncertain.png' })).toBeVisible();
  expect((await history(world, world.tokens.browser, 'general')).filter((message) => message.body === first)).toHaveLength(1);
  await expect.poll(() => uploads).toBe(1);

  const second = unique('lost-after-post');
  let answered = 0;
  await page.route('**/v2/channels/general/messages', async (route) => {
    if (route.request().method() === 'POST' && route.request().postData()?.includes(second) === true) {
      answered++;
      await route.fetch();
      await route.abort('connectionreset');
    } else await route.continue();
  });
  await page.getByLabel('Choose images').setInputFiles({ ...pickerFile, name: 'uncertain-after.png' });
  await page.getByLabel('Message #general').fill(second);
  await page.getByRole('button', { name: 'Send', exact: true }).click();
  await expect(messages(page).getByRole('listitem').filter({ hasText: second }).filter({ hasText: 'Sent' })).toHaveCount(1);
  await page.unroute('**/v2/channels/general/messages');
  expect(answered).toBe(1);
  expect((await history(world, world.tokens.browser, 'general')).filter((message) => message.body === second)).toHaveLength(1);
  await expect.poll(() => uploads).toBe(2);
});

test('two clients deliver only authorized previews to members, nets, whispers and post-watermark guests', async ({ meet, secrets, world }) => {
  const channel = unique('private');
  await done(world, 'POST', '/v0/channels', { name: channel });
  const sender = await person(world, secrets, unique('sender'), [channel]);
  const member = await person(world, secrets, unique('member'), [channel]);
  const wrong = await person(world, secrets, unique('wrong'), [channel]);
  await done(world, 'POST', `/v1/channels/${encodeURIComponent(channel)}/nets`, { name: 'blue' });
  const nets = await ok(world, world.tokens.operator, 'GET', `/v1/channels/${encodeURIComponent(channel)}/nets`, listNetsResponseV1);
  const net = nets.nets.find((candidate) => candidate.name === 'blue');
  if (net === undefined) throw new Error('the private net was not created');
  for (const id of [sender.id, member.id]) await done(world, 'PUT', `/v1/channels/${encodeURIComponent(channel)}/nets/blue/members/${String(id)}`, { role: 'member' });

  const wide = await attach(world, sender.token, channel, unique('wide-before'), [{ filename: 'wide.png', mediaType: 'image/png', bytes: png }]);
  const netMessage = await attach(world, sender.token, channel, unique('net-before'), [{ filename: 'net.png', mediaType: 'image/png', bytes: png }], { kind: 'net', net_id: net.id });
  const whisper = await attach(world, sender.token, channel, unique('whisper-before'), [{ filename: 'whisper.png', mediaType: 'image/png', bytes: png }], { kind: 'principals', principal_ids: [member.id] });
  const guest = await invitedGuest(world, channel);
  secrets.push(guest.token);

  const senderPage = (await meet()).page;
  const memberPage = (await meet()).page;
  const wrongPage = (await meet()).page;
  const guestPage = (await meet()).page;
  const outsiderPage = (await meet()).page;
  const wrongReads = fileRequests(wrongPage);
  const guestReads = fileRequests(guestPage);
  const outsiderReads = fileRequests(outsiderPage);
  await Promise.all([
    signIn(senderPage, world, sender.token, sender.name),
    signIn(memberPage, world, member.token, member.name),
    signIn(wrongPage, world, wrong.token, wrong.name),
    signIn(outsiderPage, world, world.tokens.browser, 'browser-test'),
  ]);
  await guestPage.goto(world.appUrl);
  await guestPage.getByLabel('Credential').fill(guest.token);
  await guestPage.getByRole('button', { name: 'Use credential' }).click();
  await expect(statusLine(guestPage)).toContainText('Connected');
  for (const page of [senderPage, memberPage, wrongPage, guestPage]) await channelButton(page, channel).click();

  for (const body of [wide.body, netMessage.body, whisper.body]) {
    await expect(messages(memberPage).getByText(body)).toBeVisible();
    await expect(messages(wrongPage).getByText(body)).toHaveCount(wide.body === body ? 1 : 0);
    await expect(messages(guestPage).getByText(body)).toHaveCount(0);
  }
  await expect(memberPage.getByRole('button', { name: 'Enlarge wide.png' })).toBeVisible();
  await expect(memberPage.getByRole('button', { name: 'Enlarge net.png' })).toBeVisible();
  await expect(memberPage.getByRole('button', { name: 'Enlarge whisper.png' })).toBeVisible();
  await expect(wrongPage.getByRole('button', { name: 'Enlarge wide.png' })).toBeVisible();
  await expect(wrongPage.getByRole('button', { name: /Enlarge (net|whisper)\.png/ })).toHaveCount(0);
  await expect(guestPage.getByRole('button', { name: /Enlarge/ })).toHaveCount(0);
  await expect(channelButton(outsiderPage, channel)).toHaveCount(0);

  for (const id of fileIds(wide)) await expectDenied(world, world.tokens.browser, id);
  for (const message of [netMessage, whisper]) {
    for (const id of fileIds(message)) await expectDenied(world, wrong.token, id);
  }
  for (const message of [wide, netMessage, whisper]) {
    for (const id of fileIds(message)) await expectDenied(world, guest.token, id);
    for (const id of fileIds(message)) await expectDenied(world, world.tokens.browser, id);
  }

  const after = unique('wide-after');
  await senderPage.getByLabel('Choose images').setInputFiles({ ...pickerFile, name: 'after.png' });
  await senderPage.getByLabel(`Message #${channel}`).fill(after);
  await senderPage.getByRole('button', { name: 'Send', exact: true }).click();
  for (const page of [memberPage, wrongPage, guestPage]) {
    await expect(messages(page).getByText(after)).toBeVisible();
    await expect(page.getByRole('button', { name: 'Enlarge after.png' })).toBeVisible();
  }
  const stored = (await history(world, sender.token, channel)).find((message) => message.body === after);
  if (stored === undefined) throw new Error('the after-watermark message was not stored');
  for (const id of fileIds(stored)) {
    expect((await call(world, guest.token, 'GET', `/v2/files/${String(id)}/meta`)).status).toBe(200);
    expect((await call(world, guest.token, 'GET', `/v2/files/${String(id)}`)).status).toBe(200);
    await expectDenied(world, world.tokens.browser, id);
  }
  const wrongIds = requestedFileIds(wrongReads);
  expect(fileIds(netMessage).concat(fileIds(whisper)).some((id) => wrongIds.has(id))).toBe(false);
  const guestIds = requestedFileIds(guestReads);
  expect([wide, netMessage, whisper].flatMap(fileIds).some((id) => guestIds.has(id))).toBe(false);
  const outsiderIds = requestedFileIds(outsiderReads);
  expect([wide, netMessage, whisper, stored].flatMap(fileIds).some((id) => outsiderIds.has(id))).toBe(false);
});

test('revocation clears cached previews and a second credential restores the complete interrupted draft', async ({ meet, secrets, world }) => {
  const channel = unique('revoked');
  await done(world, 'POST', '/v0/channels', { name: channel });
  const first = await person(world, secrets, unique('revoked-person'), [channel]);
  const second = await ok(world, world.tokens.operator, 'POST', `/v1/principals/${String(first.id)}/credentials`, createCredentialResponseV1, { label: 'second' });
  secrets.push(second.token);
  await attach(world, first.token, channel, unique('existing'), [{ filename: 'existing.png', mediaType: 'image/png', bytes: png }]);

  const { page } = await meet();
  await trackObjectURLs(page);
  await signIn(page, world, first.token, first.name);
  await channelButton(page, channel).click();
  await expect(page.getByRole('button', { name: 'Enlarge existing.png' })).toBeVisible();
  await expect.poll(() => activeObjectURLs(page)).toBe(1);
  const caption = unique('restore-caption');
  await page.getByLabel('Choose images').setInputFiles({ ...pickerFile, name: 'restored.png' });
  await page.getByLabel(`Message #${channel}`).fill(caption);
  let release = () => {};
  const gate = new Promise<void>((resolve) => { release = resolve; });
  await page.route(`**/v2/channels/${channel}/files?*`, async (route) => {
    await gate;
    await route.abort('connectionreset').catch(() => undefined);
  });
  await page.getByRole('button', { name: 'Send', exact: true }).click();
  await expect(page.getByRole('button', { name: 'Cancel upload of restored.png' })).toBeVisible();

  await page.getByRole('button', { name: 'Enlarge existing.png' }).click();
  await done(world, 'DELETE', `/v1/credentials/${String(first.credentialId)}`);
  await expect(page.getByLabel('Credential')).toBeVisible({ timeout: 45_000 });
  await expect.poll(() => activeObjectURLs(page)).toBe(0);
  const historyMessage = (await history(world, second.token, channel)).find((message) => message.payload !== undefined);
  if (historyMessage === undefined) throw new Error('the history attachment was not stored');
  for (const id of fileIds(historyMessage)) await expectDenied(world, first.token, id, 401);
  release();
  await page.unroute(`**/v2/channels/${channel}/files?*`);

  await page.getByLabel('Credential').fill(second.token);
  await page.getByRole('button', { name: 'Use credential' }).click();
  await expect(statusLine(page)).toContainText(first.name);
  await expect(statusLine(page)).toContainText('Connected');
  await channelButton(page, channel).click();
  await expect(page.getByLabel(`Message #${channel}`)).toHaveValue(caption);
  await expect(page.getByText('restored.png')).toBeVisible();
  await expect(page.getByRole('button', { name: 'Cancel upload of restored.png' })).toHaveCount(0);
  await page.getByRole('button', { name: 'Send', exact: true }).click();
  await expect(messages(page).getByRole('listitem').filter({ hasText: caption }).filter({ hasText: 'Sent' })).toHaveCount(1);
  expect((await history(world, second.token, channel)).filter((message) => message.body === caption)).toHaveLength(1);
});

test('changing server origin invalidates an in-flight upload without retargeting or leaking the draft', async ({ meet, secrets, world }) => {
  const channel = unique('origin');
  await done(world, 'POST', '/v0/channels', { name: channel });
  const author = await person(world, secrets, unique('origin-person'), [channel]);
  const { page } = await meet();
  await signIn(page, world, author.token, author.name);
  await channelButton(page, channel).click();
  const caption = unique('origin-caption');
  await page.getByLabel('Choose images').setInputFiles({ ...pickerFile, name: 'origin.png' });
  await page.getByLabel(`Message #${channel}`).fill(caption);
  let release = () => {};
  const gate = new Promise<void>((resolve) => { release = resolve; });
  await page.route(`**/v2/channels/${channel}/files?*`, async (route) => {
    await gate;
    await route.abort('connectionreset').catch(() => undefined);
  });
  await page.getByRole('button', { name: 'Send', exact: true }).click();
  await expect(page.getByRole('button', { name: 'Cancel upload of origin.png' })).toBeVisible();
  const elsewhere = world.appUrl.replace('127.0.0.1', 'localhost');
  expect(elsewhere).not.toBe(world.appUrl);
  await page.goto(elsewhere);
  await expect(page.getByLabel('Credential')).toBeVisible();
  release();
  await page.unroute(`**/v2/channels/${channel}/files?*`);
  await signIn(page, { appUrl: elsewhere }, author.token, author.name);
  await channelButton(page, channel).click();
  await expect(page.getByLabel(`Message #${channel}`)).toHaveValue('');
  await expect(page.getByText('origin.png')).toHaveCount(0);
  expect((await history(world, author.token, channel)).filter((message) => message.body === caption)).toHaveLength(0);
});

test('history decodes JPEG, rejects spoofed active content, revokes URLs and supports keyboard dialog operation', async ({ meet, secrets, world }) => {
  const channel = unique('decode');
  const other = unique('decode-other');
  for (const name of [channel, other]) await done(world, 'POST', '/v0/channels', { name });
  const author = await person(world, secrets, unique('decode-author'), [channel]);
  const reader = await person(world, secrets, unique('decode-reader'), [channel, other]);
  const { page } = await meet();
  await trackObjectURLs(page);
  await signIn(page, world, reader.token, reader.name);
  const jpegBytes = await jpeg(page);
  const jpgName = `${unique('decoded')}.jpg`;
  const svgName = `${unique('active-svg')}.png`;
  const htmlName = `${unique('active-html')}.jpg`;
  const hugeName = `${unique('huge-history')}.png`;
  await attach(world, author.token, channel, unique('jpeg-history'), [{ filename: jpgName, mediaType: 'image/png', bytes: jpegBytes }]);
  await attach(world, author.token, channel, unique('svg-history'), [{ filename: svgName, mediaType: 'image/png', bytes: text('<svg xmlns="http://www.w3.org/2000/svg"><script>alert(1)</script></svg>') }]);
  await attach(world, author.token, channel, unique('html-history'), [{ filename: htmlName, mediaType: 'image/jpeg', bytes: text('<html><script>alert(1)</script></html>') }]);
  await attach(world, author.token, channel, unique('dimensions-history'), [{ filename: hugeName, mediaType: 'image/png', bytes: pngWithDimensions(5000, 5001) }]);
  await channelButton(page, channel).click();
  const trigger = page.getByRole('button', { name: `Enlarge ${jpgName}` });
  await expect(trigger).toBeVisible();
  const decoded = page.locator('figure.attachment').filter({ hasText: jpgName }).locator('img[src^="blob:"]');
  await expect(decoded).toHaveJSProperty('naturalWidth', 3);
  await expect(decoded).toHaveJSProperty('naturalHeight', 2);
  for (const [name, note] of [
    [svgName, 'This file format is not supported for image preview.'],
    [htmlName, 'This file format is not supported for image preview.'],
    [hugeName, 'This image is too large to preview.'],
  ] as const) {
    const figure = page.locator('figure.attachment').filter({ hasText: name });
    await expect(figure).toContainText(note);
    await expect(figure.getByRole('button', { name: 'Download' })).toBeVisible();
    await expect(figure.getByRole('button', { name: /Enlarge/ })).toHaveCount(0);
  }
  await expect.poll(() => activeObjectURLs(page)).toBe(1);

  await trigger.focus();
  await page.keyboard.press('Enter');
  const dialog = page.getByRole('dialog', { name: jpgName });
  const close = dialog.getByRole('button', { name: 'Close' });
  const download = dialog.getByRole('button', { name: 'Download' });
  await expect(close).toBeFocused();
  await page.keyboard.press('Tab');
  await expect(download).toBeFocused();
  await page.keyboard.press('Tab');
  await expect(close).toBeFocused();
  await page.keyboard.press('Shift+Tab');
  await expect(download).toBeFocused();
  const downloaded = page.waitForEvent('download');
  await page.keyboard.press('Enter');
  expect((await downloaded).suggestedFilename()).toBe(jpgName);
  await download.focus();
  await page.keyboard.press('Escape');
  await expect(dialog).toHaveCount(0);
  await expect(trigger).toBeFocused();
  await page.keyboard.press('Enter');
  await expect(close).toBeFocused();
  await page.keyboard.press('Enter');
  await expect(dialog).toHaveCount(0);
  await expect(trigger).toBeFocused();
  await expect.poll(() => activeObjectURLs(page)).toBe(1);

  await channelButton(page, other).click();
  await expect.poll(() => activeObjectURLs(page)).toBe(0);
  await channelButton(page, channel).click();
  await expect(page.getByRole('button', { name: `Enlarge ${jpgName}` })).toBeVisible();
  await expect.poll(() => activeObjectURLs(page)).toBe(1);
  await page.reload();
  await expect(page.getByRole('button', { name: `Enlarge ${jpgName}` })).toBeVisible();
  await expect.poll(() => activeObjectURLs(page)).toBe(1);
});
