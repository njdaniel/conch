// The test every spec uses. Each person is a browser context of their own
// (never a second tab of one), and every page of every context is checked,
// when the test ends, for a credential in the address bar or in
// performance.getEntries(): the one thing that must hold throughout.
import { expect, test as base, type BrowserContext, type Page } from '@playwright/test';
import { allTokens, loadWorld, type World } from './world';

// leaks lists where in a page a credential shows, none when it does not.
async function leaks(page: Page, tokens: readonly string[]): Promise<string[]> {
  if (page.isClosed()) return [];
  const found: string[] = [];
  const url = page.url();
  // performance.getEntries() is the page's own record of every address it asked
  // for, the web socket's included (it carries a ticket, which is not the credential).
  const entries = await page.evaluate(() => JSON.stringify(performance.getEntries().map((e) => e.name)));
  const visible = await page.evaluate(() => document.documentElement.outerHTML);
  for (const token of tokens) {
    if (url.includes(token)) found.push('the address bar');
    if (entries.includes(token)) found.push('performance.getEntries()');
    if (visible.includes(token)) found.push('the page');
  }
  return found;
}

export interface Person {
  readonly page: Page;
  readonly context: BrowserContext;
}

export const test = base.extend<{
  // meet opens a fresh browser context with a page in it: one person.
  meet: () => Promise<Person>;
  world: World;
}>({
  world: async ({}, use) => {
    await use(loadWorld());
  },
  meet: async ({ browser, world }, use) => {
    const people: Person[] = [];
    await use(async () => {
      const context = await browser.newContext();
      const person = { context, page: await context.newPage() };
      people.push(person);
      return person;
    });
    // The credential must not have been anywhere visible, in any test. Every
    // context is closed whatever the check finds.
    const found: string[] = [];
    try {
      for (const { page } of people) {
        for (const where of await leaks(page, allTokens(world))) found.push(where);
      }
    } finally {
      for (const { context } of people) await context.close();
    }
    expect(found, 'a credential in the address bar, in performance.getEntries() or in the page').toEqual([]);
  },
});

export { expect };

// ---------------------------------------------------------------- the page

// signIn pastes a credential into the sign-in field and waits for the lobby.
export async function signIn(page: Page, world: World, token: string, name: string): Promise<void> {
  await page.goto(world.appUrl);
  await page.getByLabel('Credential').fill(token);
  await page.getByRole('button', { name: 'Sign in' }).click();
  await expect(statusLine(page)).toContainText(name);
  await expect(statusLine(page)).toContainText('Connected');
}

export const statusLine = (page: Page) => page.getByRole('status', { name: 'Status' });
export const channelButton = (page: Page, name: string) => page.getByRole('navigation', { name: 'Channels' }).getByRole('button', { name, exact: true });
export const messages = (page: Page) => page.getByRole('log', { name: 'Messages' });
export const conversation = (page: Page) => page.getByRole('region', { name: 'Conversation' });

// say types into the composer and sends with the Send button.
export async function say(page: Page, channel: string, body: string): Promise<void> {
  await page.getByLabel(`Message #${channel}`).fill(body);
  await page.getByRole('button', { name: 'Send' }).click();
}
