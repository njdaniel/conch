// The test every spec uses. Each person is a browser context of their own
// (never a second tab of one), and every page of every context is checked,
// when the test ends, for a credential in the address bar or in
// performance.getEntries(): the one thing that must hold throughout.
import { expect, test as base, type BrowserContext, type BrowserContextOptions, type Page } from '@playwright/test';
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
  // meet opens a fresh browser context with a page in it: one person. The
  // options are the context's (a phone's screen, say).
  meet: (options?: BrowserContextOptions) => Promise<Person>;
  // secrets is the credentials a test came by itself (one an invite gave, say),
  // which the end-of-test scan looks for beside the world's.
  secrets: string[];
  world: World;
}>({
  world: async ({}, use) => {
    await use(loadWorld());
  },
  secrets: async ({}, use) => {
    await use([]);
  },
  meet: async ({ browser, world, secrets }, use) => {
    const people: Person[] = [];
    await use(async (options?: BrowserContextOptions) => {
      const context = await browser.newContext(options);
      const person = { context, page: await context.newPage() };
      people.push(person);
      return person;
    });
    // The credential must not have been anywhere visible, in any test. Every
    // context is closed whatever the check finds.
    const found: string[] = [];
    try {
      for (const { page } of people) {
        for (const where of await leaks(page, [...allTokens(world), ...secrets])) found.push(where);
      }
    } finally {
      // Each page is closed before its context, as a person closes a tab: its
      // pagehide ends the session the page started. Closing the context alone
      // fires no pagehide and leaves the session live until conchd marks it
      // offline, counted against the credential's budget of live sessions,
      // which a spec run again and again soon spends (issue #802).
      for (const { page } of people) if (!page.isClosed()) await page.close();
      for (const { context } of people) await context.close();
    }
    expect(found, 'a credential in the address bar, in performance.getEntries() or in the page').toEqual([]);
  },
});

export { expect };

// ---------------------------------------------------------------- the page

// signIn pastes a credential into the sign-in field and waits for the lobby.
export async function signIn(page: Page, world: Pick<World, 'appUrl'>, token: string, name: string): Promise<void> {
  await page.goto(world.appUrl);
  await page.getByLabel('Credential').fill(token);
  await page.getByRole('button', { name: 'Use credential' }).click();
  await expect(statusLine(page)).toContainText(name);
  await expect(statusLine(page)).toContainText('Connected');
}

// The password the suite's new accounts choose (issue #713: an invite or the
// bare address makes an account with a name and a password together). It
// passes the server's policy for every name the specs use.
export const accountPassword = 'a long enough passphrase for the browser suite';

// fillAccount fills the account form, a name and a password together, and
// sends it with the button the form shows ('Create account', or 'Create my
// account' for a guest's). It waits for nothing: the caller says what comes.
export async function fillAccount(page: Page, name: string, password = accountPassword, button = 'Create account'): Promise<void> {
  await page.getByLabel('Your name').fill(name);
  await page.getByLabel('Choose a password').fill(password);
  await page.getByLabel('Type the password again').fill(password);
  await page.getByRole('button', { name: button, exact: true }).click();
}

// signInWithPassword signs a returning member in on the sign-in page.
export async function signInWithPassword(page: Page, name: string, password = accountPassword): Promise<void> {
  await page.getByLabel('Name', { exact: true }).fill(name);
  await page.getByLabel('Password', { exact: true }).fill(password);
  await page.getByRole('button', { name: 'Sign in', exact: true }).click();
}

export const statusLine = (page: Page) => page.getByRole('status', { name: 'Status' });
export const channelButton = (page: Page, name: string) => page.getByRole('navigation', { name: 'Channels' }).getByRole('button', { name, exact: true });
// The phone layout's bottom navigation (issue #929), present only below 700 px.
export const mainNav = (page: Page) => page.getByRole('navigation', { name: 'Main navigation' });

// showView taps the phone navigation's Chat, Channels or Voice. Where there is
// none (700 px and wider) every view is already on screen and it does nothing.
export async function showView(page: Page, view: 'Chat' | 'Channels' | 'Voice'): Promise<void> {
  const nav = mainNav(page);
  if ((await nav.count()) === 0) return;
  // The members panel is drawn over the page, the navigation's right end
  // included; a person closes it before moving on, as Escape does.
  const members = page.getByRole('region', { name: 'Members list' });
  if (await members.isVisible()) {
    await page.keyboard.press('Escape');
    await expect(members).toBeHidden();
  }
  await nav.getByRole('button', { name: view, exact: true }).click();
  await expect(nav.getByRole('button', { name: view, exact: true })).toHaveAttribute('aria-current', 'page');
}

// openChannel opens a channel from the channel list, which on a phone is the
// Channels view; the phone then shows its chat.
export async function openChannel(page: Page, name: string): Promise<void> {
  await showView(page, 'Channels');
  await channelButton(page, name).click();
  await showView(page, 'Chat');
}

export const messages = (page: Page) => page.getByRole('log', { name: 'Messages' });
export const conversation = (page: Page) => page.getByRole('region', { name: 'Conversation' });

// The phone sizes the lobby must fit (issue #467): mobile Chromium, with touch
// and a phone's pixel density, at the two widths most phones have.
export const phones = [
  ['390×844', { viewport: { width: 390, height: 844 }, deviceScaleFactor: 3, isMobile: true, hasTouch: true }],
  ['360×740', { viewport: { width: 360, height: 740 }, deviceScaleFactor: 3, isMobile: true, hasTouch: true }],
] as const satisfies readonly (readonly [string, BrowserContextOptions])[];

// The four phone sizes of the phone layout (issue #929): the narrowest still
// in use, two common ones and a large one. A portrait phone is under 700 px
// wide; turned on its side it is wider, and has the tablet layout.
export const phoneSizes = [
  ['320×640', { viewport: { width: 320, height: 640 }, deviceScaleFactor: 2, isMobile: true, hasTouch: true }],
  ['360×740', { viewport: { width: 360, height: 740 }, deviceScaleFactor: 3, isMobile: true, hasTouch: true }],
  ['390×844', { viewport: { width: 390, height: 844 }, deviceScaleFactor: 3, isMobile: true, hasTouch: true }],
  ['430×932', { viewport: { width: 430, height: 932 }, deviceScaleFactor: 3, isMobile: true, hasTouch: true }],
] as const satisfies readonly (readonly [string, BrowserContextOptions])[];

// expectFits checks that the page is no wider than the window: nothing scrolls
// sideways, and the window is still the screen's width. A phone browser shown
// a page wider than its screen zooms out to fit it, which is what made taps
// land below where they were aimed (issue #467).
export async function expectFits(page: Page, view: string): Promise<void> {
  const { scroll, inner } = await page.evaluate(() => ({ scroll: document.documentElement.scrollWidth, inner: window.innerWidth }));
  const screen = page.viewportSize()?.width;
  expect(inner, `${view}: the window is ${String(inner)} px wide on a ${String(screen)} px screen`).toBe(screen);
  expect(scroll, `${view}: the page is ${String(scroll)} px wide in a ${String(inner)} px window`).toBeLessThanOrEqual(inner);
}

// say types into the composer and sends with the Send button.
export async function say(page: Page, channel: string, body: string): Promise<void> {
  await page.getByLabel(`Message #${channel}`).fill(body);
  await page.getByRole('button', { name: 'Send' }).click();
}
