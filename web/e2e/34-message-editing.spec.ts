// Issue #845 (docs/design/message-editing.md §13, §14.2): editing one's own
// recent text in the browser, against the real server. Two people, each a
// browser of their own: the edit reaches the other person live, survives a
// reload, is what search finds (the old words no longer are), and only the
// author can open its earlier versions. A whisper's edit reaches no one
// outside it, and an attachment message keeps its file. At phone sizes the
// ⋯ button is always there and a finger can hit it.
//
// Each test makes its own channel and people, so the edit bucket of one
// person (ten in a burst) is never shared with another spec.
import type { Locator, Page } from '@playwright/test';
import { createCredentialResponseV1, createPrincipalResponse } from '../src/schema/credential';
import { fileAttachmentPayloadSchema, fileAttachmentV1 } from '../src/schema/file';
import { postMessageResponseV2, type MessageV2 } from '../src/schema/message';
import { attach, history } from './api';
import { expect, expectFits, messages, openChannel, phoneSizes, say, signIn, test } from './fixtures';
import { call, ok, type World } from './world';

const stamp = Date.now().toString(36);
let next = 0;
// Words of letters and digits only, so the full-text index takes each whole.
const word = (label: string) => `${label}${stamp}${String(++next)}`;

const buffer = (globalThis as unknown as { Buffer: { from(value: string, encoding: 'base64'): Uint8Array<ArrayBuffer> } }).Buffer;
const png = buffer.from('iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAYAAAAfFcSJAAAAC0lEQVR4nGNgAAIAAAUAAXpeqz8AAAAASUVORK5CYII=', 'base64');

interface Someone {
  readonly id: number;
  readonly name: string;
  readonly token: string;
}

async function done(world: World, method: 'POST' | 'PUT', path: string, body?: unknown): Promise<void> {
  const reply = await call(world, world.tokens.operator, method, path, body);
  if (reply.status < 200 || reply.status > 299) throw new Error(`${method} ${path}: status ${String(reply.status)}`);
}

// A channel of its own, and people in it with a credential each.
async function community(world: World, secrets: string[], label: string, names: readonly string[]): Promise<{ channel: string; people: Someone[] }> {
  const channel = `e845-${label}-${stamp}`;
  await done(world, 'POST', '/v0/channels', { name: channel });
  const people: Someone[] = [];
  for (const base of names) {
    const name = `${base}-${stamp}`;
    const made = await ok(world, world.tokens.operator, 'POST', '/v0/principals', createPrincipalResponse, { kind: 'human', name });
    const issued = await ok(world, world.tokens.operator, 'POST', `/v1/principals/${String(made.principal.id)}/credentials`, createCredentialResponseV1, { label: name });
    secrets.push(issued.token);
    await done(world, 'PUT', `/v1/channels/${encodeURIComponent(channel)}/members/${String(made.principal.id)}`);
    people.push({ id: made.principal.id, name, token: issued.token });
  }
  return { channel, people };
}

const row = (page: Page, id: number) => messages(page).locator(`[data-message-id="${String(id)}"]`);

// edit opens the message's menu, chooses Edit, and saves the new text with
// Enter. On a touch screen it taps.
async function edit(page: Page, id: number, text: string, touch = false): Promise<void> {
  const item = row(page, id);
  const more = item.getByRole('button', { name: `More actions for message ${String(id)}` });
  if (touch) await more.tap();
  else {
    await item.hover();
    await more.click();
  }
  const choice = page.getByRole('menu', { name: `More actions for message ${String(id)}` }).getByRole('menuitem', { name: 'Edit' });
  if (touch) await choice.tap();
  else await choice.click();
  const box = page.getByRole('textbox', { name: `Edit message ${String(id)}` });
  await expect(box).toBeFocused();
  await box.fill(text);
  if (touch) await page.getByRole('button', { name: 'Save', exact: true }).tap();
  else await box.press('Enter');
  await expect(box).toBeHidden();
}

async function postedId(world: World, token: string, channel: string, body: string): Promise<number> {
  const found = (await history(world, token, channel)).find((m) => m.body === body);
  if (found === undefined) throw new Error(`no message "${body}" in ${channel}`);
  return found.id;
}

const marker = (item: Locator) => item.locator('time.edited-marker, button.edited-marker');

test('an edit reaches the other person live, survives a reload, and is what search finds', async ({ meet, secrets, world }) => {
  const { channel, people } = await community(world, secrets, 'live', ['author', 'reader']);
  const [author, reader] = people as [Someone, Someone];
  const a = await meet();
  const b = await meet();
  await signIn(a.page, world, author.token, author.name);
  await openChannel(a.page, channel);
  await signIn(b.page, world, reader.token, reader.name);
  await openChannel(b.page, channel);

  const oldWord = word('oldword');
  const newWord = word('newword');
  const first = `hello ${oldWord}`;
  await say(a.page, channel, first);
  const id = await postedId(world, author.token, channel, first);
  await expect(row(b.page, id)).toContainText(first);

  const fixed = `hello ${newWord}`;
  await edit(a.page, id, fixed);
  await expect(row(a.page, id)).toContainText(fixed);
  await expect(row(a.page, id).getByRole('button', { name: /^Edited at \d\d:\d\d$/ })).toBeVisible();

  // Live, on the other person's page: the text in place, the marker plain.
  await expect(row(b.page, id)).toContainText(fixed);
  await expect(row(b.page, id)).not.toContainText(oldWord);
  await expect(marker(row(b.page, id))).toHaveText('(edited)');
  await expect(row(b.page, id).getByRole('button', { name: /^Edited at/ })).toHaveCount(0);
  await expect(messages(b.page).getByRole('listitem').filter({ hasText: newWord })).toHaveCount(1);

  // The same after a reload.
  await b.page.reload();
  await openChannel(b.page, channel);
  await expect(row(b.page, id)).toContainText(fixed);
  await expect(marker(row(b.page, id))).toHaveText('(edited)');

  // Search finds the new word and not the old, with the marker.
  await b.page.getByRole('button', { name: 'Search', exact: true }).click();
  const searchbox = b.page.getByRole('searchbox', { name: `Messages in #${channel}` });
  await searchbox.fill(oldWord);
  await b.page.getByRole('button', { name: 'Search', exact: true }).click();
  await expect(b.page.getByText(`No messages match “${oldWord}”.`)).toBeVisible();
  await searchbox.fill(newWord);
  await b.page.getByRole('button', { name: 'Search', exact: true }).click();
  const results = b.page.getByRole('list', { name: 'Search results' });
  await expect(results.getByRole('listitem')).toHaveCount(1);
  await expect(results).toContainText(fixed);
  await expect(results.locator('time.edited-marker')).toHaveText('(edited)');

  // Earlier versions are the author's alone: the reader has no button and the
  // server answers them as for a message that is not there.
  expect((await call(world, reader.token, 'GET', `/v2/channels/${encodeURIComponent(channel)}/messages/${String(id)}/revisions`)).status).toBe(404);
  await row(a.page, id).getByRole('button', { name: /^Edited at/ }).click();
  const versions = a.page.getByRole('dialog', { name: 'Earlier versions' });
  await expect(versions).toContainText('Only you can see earlier versions. They are kept on the server.');
  const list = versions.getByRole('list', { name: 'Versions, newest first' });
  await expect(list.getByRole('listitem')).toHaveCount(2);
  await expect(list.getByRole('listitem').first()).toContainText(fixed);
  await expect(list.getByRole('listitem').first()).toContainText('Current');
  await expect(list.getByRole('listitem').last()).toContainText(first);
  await a.page.keyboard.press('Escape');
  await expect(versions).toBeHidden();
  await expect(row(a.page, id).getByRole('button', { name: /^Edited at/ })).toBeFocused();
});

test('Escape cancels an edit and Shift+Enter adds a line, in a real browser', async ({ meet, secrets, world }) => {
  const { channel, people } = await community(world, secrets, 'keys', ['keys']);
  const [author] = people as [Someone];
  const { page } = await meet();
  await signIn(page, world, author.token, author.name);
  await openChannel(page, channel);
  const body = word('keys');
  await say(page, channel, body);
  const id = await postedId(world, author.token, channel, body);
  const item = row(page, id);
  await item.click({ button: 'right' });
  await page.getByRole('menuitem', { name: 'Edit' }).click();
  const box = page.getByRole('textbox', { name: `Edit message ${String(id)}` });
  await box.press('End');
  await box.pressSequentially(' more');
  await box.press('Shift+Enter');
  await box.pressSequentially('second line');
  await expect(box).toHaveValue(`${body} more\nsecond line`);
  await box.press('Escape');
  await expect(box).toBeHidden();
  await expect(item.getByRole('button', { name: `More actions for message ${String(id)}` })).toBeFocused();
  await expect(item).not.toContainText('second line');
  expect((await history(world, author.token, channel)).find((m) => m.id === id)?.edited_at).toBeUndefined();
});

test('an open edit reopens with its text after a reload (issue #1039)', async ({ meet, secrets, world }) => {
  const { channel, people } = await community(world, secrets, 'reload', ['reload']);
  const [author] = people as [Someone];
  const { page } = await meet();
  await signIn(page, world, author.token, author.name);
  await openChannel(page, channel);
  const body = word('reload');
  await say(page, channel, body);
  const id = await postedId(world, author.token, channel, body);
  await row(page, id).click({ button: 'right' });
  await page.getByRole('menuitem', { name: 'Edit' }).click();
  const box = page.getByRole('textbox', { name: `Edit message ${String(id)}` });
  await box.fill('half an edit, not saved');

  await page.reload();
  await openChannel(page, channel);
  await expect(page.getByRole('textbox', { name: `Edit message ${String(id)}` })).toHaveValue('half an edit, not saved');
  // Cancelling forgets it: a reload after shows no editor.
  await page.getByRole('textbox', { name: `Edit message ${String(id)}` }).press('Escape');
  await page.reload();
  await openChannel(page, channel);
  await expect(row(page, id)).toContainText(body);
  await expect(page.getByRole('textbox', { name: `Edit message ${String(id)}` })).toHaveCount(0);
});

test('the menu stops offering Edit after ten known edits, with no reload and no refusal (issue #1031)', async ({ meet, secrets, world }) => {
  const { channel, people } = await community(world, secrets, 'limit', ['limiter']);
  const [author] = people as [Someone];
  const { page } = await meet();
  await signIn(page, world, author.token, author.name);
  await openChannel(page, channel);
  const body = word('limit');
  await say(page, channel, body);
  const id = await postedId(world, author.token, channel, body);

  // Nine edits as another tab of the author's would make them, over the API;
  // the page sees each as a frame.
  for (let n = 1; n <= 9; n++) {
    const reply = await call(world, author.token, 'PUT', `/v2/channels/${encodeURIComponent(channel)}/messages/${String(id)}/body`, { body: `${body} v${String(n)}` });
    expect(reply.status).toBe(200);
  }
  await expect(row(page, id)).toContainText(`${body} v9`);
  // Nine known: Edit is still offered, and the tenth is made in the page.
  await edit(page, id, `${body} v10`);
  await expect(row(page, id)).toContainText(`${body} v10`);

  // Ten known: no Edit, so no ⋯ at all, and the server was not asked.
  await row(page, id).hover();
  await expect(row(page, id).getByRole('button', { name: `More actions for message ${String(id)}` })).toHaveCount(0);
  await row(page, id).click({ button: 'right' });
  await expect(page.getByRole('menuitem', { name: 'Edit' })).toHaveCount(0);
});

test('a whisper’s edit reaches its recipients, and no one else', async ({ meet, secrets, world }) => {
  const { channel, people } = await community(world, secrets, 'whisper', ['whisperer', 'hearer', 'outsider']);
  const [author, hearer, outsider] = people as [Someone, Someone, Someone];
  const a = await meet();
  const b = await meet();
  const c = await meet();
  for (const [person, who] of [[a, author], [b, hearer], [c, outsider]] as const) {
    await signIn(person.page, world, who.token, who.name);
    await openChannel(person.page, channel);
  }
  const body = word('whisper');
  const posted = (await ok(world, author.token, 'POST', `/v2/channels/${encodeURIComponent(channel)}/messages`, postMessageResponseV2, {
    body,
    audience: { kind: 'principals', principal_ids: [hearer.id] },
  })).message;
  await expect(row(a.page, posted.id)).toContainText(body);
  await expect(row(b.page, posted.id)).toContainText(body);
  // A message after it, so the outsider's page is known to be live past it.
  const after = word('after');
  await say(a.page, channel, after);
  await expect(messages(c.page).getByText(after, { exact: true })).toBeVisible();

  const fixed = word('whisperfixed');
  await edit(a.page, posted.id, fixed);
  await expect(row(b.page, posted.id)).toContainText(fixed);
  await expect(marker(row(b.page, posted.id))).toHaveText('(edited)');
  await expect(row(b.page, posted.id)).toContainText('→ whisper to');

  // The outsider: nothing on the page, nothing in what they can read, and the
  // versions are not theirs to ask for.
  const later = word('later');
  await say(a.page, channel, later);
  await expect(messages(c.page).getByText(later, { exact: true })).toBeVisible();
  await expect(row(c.page, posted.id)).toHaveCount(0);
  await expect(messages(c.page)).not.toContainText(body);
  await expect(messages(c.page)).not.toContainText(fixed);
  const seen = await history(world, outsider.token, channel);
  expect(seen.some((m: MessageV2) => m.id === posted.id)).toBe(false);
  const search = await call(world, outsider.token, 'GET', `/v2/channels/${encodeURIComponent(channel)}/messages/search?q=${fixed}`);
  expect(search.status).toBe(200);
  expect(JSON.stringify(search.body)).not.toContain(fixed);
  expect((await call(world, outsider.token, 'GET', `/v2/channels/${encodeURIComponent(channel)}/messages/${String(posted.id)}/revisions`)).status).toBe(404);
  expect((await call(world, outsider.token, 'PUT', `/v2/channels/${encodeURIComponent(channel)}/messages/${String(posted.id)}/body`, { body: 'not mine' })).status).toBe(404);
});

test('an attachment message keeps its file when its caption is edited', async ({ meet, secrets, world }) => {
  const { channel, people } = await community(world, secrets, 'file', ['sharer', 'viewer']);
  const [author, viewer] = people as [Someone, Someone];
  const a = await meet();
  const b = await meet();
  await signIn(a.page, world, author.token, author.name);
  await openChannel(a.page, channel);
  await signIn(b.page, world, viewer.token, viewer.name);
  await openChannel(b.page, channel);
  const caption = word('caption');
  const posted = await attach(world, author.token, channel, caption, [{ filename: 'e845.png', mediaType: 'image/png', bytes: png }]);
  for (const page of [a.page, b.page]) await expect(row(page, posted.id).getByRole('button', { name: 'Enlarge e845.png' })).toBeVisible();

  const fixed = word('captionfixed');
  await edit(a.page, posted.id, fixed);
  for (const page of [a.page, b.page]) {
    await expect(row(page, posted.id)).toContainText(fixed);
    await expect(row(page, posted.id).getByRole('button', { name: 'Enlarge e845.png' })).toBeVisible();
  }
  await b.page.reload();
  await openChannel(b.page, channel);
  await expect(row(b.page, posted.id).getByRole('button', { name: 'Enlarge e845.png' })).toBeVisible();

  const now = (await history(world, viewer.token, channel)).find((m) => m.id === posted.id);
  expect(now?.body).toBe(fixed);
  expect(now?.payload).toEqual(posted.payload);
  if (posted.payload?.schema !== fileAttachmentPayloadSchema || fileAttachmentV1(posted.payload.data).length > 0) throw new Error('not an attachment');
  for (const fileId of (posted.payload.data as { file_ids: number[] }).file_ids) {
    expect((await call(world, viewer.token, 'GET', `/v2/files/${String(fileId)}/meta`)).status).toBe(200);
  }
});

for (const [size, options] of phoneSizes.filter(([name]) => name === '320×640' || name === '390×844')) {
  test(`at ${size} the ⋯ is always there, a finger can hit it, and the page fits`, async ({ meet, secrets, world }) => {
    const { channel, people } = await community(world, secrets, `phone${size.replace('×', 'x')}`, [`phone${size.replace('×', 'x')}`]);
    const [author] = people as [Someone];
    const { page } = await meet(options);
    await signIn(page, world, author.token, author.name);
    await openChannel(page, channel);
    const body = word('phone');
    await say(page, channel, body);
    const id = await postedId(world, author.token, channel, body);
    const more = row(page, id).getByRole('button', { name: `More actions for message ${String(id)}` });
    // Shown with no hover and no focus.
    await expect(more).toBeVisible();
    expect(await more.evaluate((el) => getComputedStyle(el).opacity)).toBe('1');
    const box = await more.boundingBox();
    expect(box?.width ?? 0).toBeGreaterThanOrEqual(44);
    expect(box?.height ?? 0).toBeGreaterThanOrEqual(44);
    await expectFits(page, `${size}: a message with its ⋯`);

    await more.tap();
    const choice = page.getByRole('menuitem', { name: 'Edit' });
    expect((await choice.boundingBox())?.height ?? 0).toBeGreaterThanOrEqual(44);
    await choice.tap();
    const editor = page.getByRole('textbox', { name: `Edit message ${String(id)}` });
    await expect(editor).toBeVisible();
    for (const name of ['Save', 'Cancel']) {
      const button = await page.getByRole('button', { name, exact: true }).boundingBox();
      expect(button?.height ?? 0, name).toBeGreaterThanOrEqual(44);
      expect(button?.width ?? 0, name).toBeGreaterThanOrEqual(44);
    }
    await expectFits(page, `${size}: the editor`);
    await editor.fill(`${body} fixed`);
    await page.getByRole('button', { name: 'Save', exact: true }).tap();
    await expect(editor).toBeHidden();
    await expect(row(page, id)).toContainText(`${body} fixed`);

    await row(page, id).getByRole('button', { name: /^Edited at/ }).tap();
    const versions = page.getByRole('dialog', { name: 'Earlier versions' });
    await expect(versions.getByRole('list', { name: 'Versions, newest first' }).getByRole('listitem')).toHaveCount(2);
    const sheet = await versions.boundingBox();
    expect(sheet?.x ?? -1).toBeGreaterThanOrEqual(0);
    expect(Math.round(sheet?.width ?? 0)).toBe(page.viewportSize()?.width);
    await expectFits(page, `${size}: earlier versions`);
    await versions.getByRole('button', { name: 'Close', exact: true }).tap();
    await expect(versions).toBeHidden();
  });
}
