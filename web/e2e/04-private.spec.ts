// P3-05: what is restricted to test-private stays there, a run in it included.
import { post, roster, run, runsIn } from './api';
import { launchWorker } from './agents';
import { channelButton, conversation, expect, test } from './fixtures';
import { call } from './world';

test('P3-05 browser-test cannot open test-private and nothing about a run there shows in its members panel', async ({ meet, world }) => {
  // worker-test is busy in test-private (a slow fixture, so that the run stays)
  // with something nick-test asked of it there.
  const worker = await launchWorker(world, 'slow.sh', { channel: 'test-private', sleep: 60 });
  try {
    const task = 'p3-05 private work for the worker';
    const asked = await post(world, world.tokens.nick, 'test-private', task);
    let runId = 0;
    await expect
      .poll(
        async () => {
          runId = (await runsIn(world, world.tokens.nick, 'test-private')).find((r) => r.message_id === asked.id)?.id ?? 0;
          return runId;
        },
        { message: 'a run answering the message in test-private' },
      )
      .toBeGreaterThan(0);

    // The page is asked for test-private by name, and does not open it.
    const { page } = await meet();
    await page.goto(`${world.appUrl}#/c/test-private`);
    await page.getByLabel('Credential').fill(world.tokens.browser);
    await page.getByRole('button', { name: 'Sign in' }).click();
    await expect(channelButton(page, 'general')).toBeVisible();
    await expect(channelButton(page, 'test-private')).toHaveCount(0);
    await expect(conversation(page).getByRole('heading', { name: 'general' })).toBeVisible();
    await expect(conversation(page).getByRole('heading', { name: 'test-private' })).toHaveCount(0);

    // The server refuses it too: the channel, and the run by its id.
    expect((await call(world, world.tokens.browser, 'GET', '/v2/channels/test-private/messages?after=0&limit=10')).status).toBeGreaterThanOrEqual(400);
    expect((await call(world, world.tokens.browser, 'GET', `/v1/runs/${String(runId)}`)).status).toBe(404);

    // The panel of general knows that worker-test is busy (its status is on the
    // roster for every member), and that is all: no run, no task, no session
    // detail that leads to either.
    const panel = page.getByRole('region', { name: 'Members list' });
    const agent = panel.getByRole('listitem').filter({ has: page.getByRole('button', { name: /worker-test/ }) });
    await expect(agent).toContainText('running');
    await agent.getByRole('button', { name: /worker-test/ }).click();
    await expect(panel.getByRole('list', { name: 'Sessions of worker-test' })).toBeVisible();
    await expect(panel).not.toContainText('task:');
    await expect(panel).not.toContainText(task);
    await expect(panel.getByRole('button', { name: /Open run|^Run #/ })).toHaveCount(0);
    await expect(panel.getByRole('list', { name: /Recent runs/ })).toHaveCount(0);
    await expect(panel).not.toContainText('test-private');

    // The API's roster and run list say the same for browser-test, while the run
    // is still going on for those who may see it.
    const lines = (await roster(world, world.tokens.browser, 'general')).filter((s) => s.name === 'worker-test');
    expect(lines.length).toBeGreaterThan(0);
    for (const line of lines) {
      expect(line.current_run).toBeUndefined();
      expect(line.task).toBeUndefined();
    }
    expect((await runsIn(world, world.tokens.browser, 'general')).some((r) => r.id === runId)).toBe(false);
    expect((await run(world, world.tokens.nick, runId)).state).toMatch(/running|starting|queued/);
  } finally {
    await worker.stop();
  }
});
