// P3-06 and P3-08: a run of a scripted agent as the panel shows it, and the
// instruction a person can give it.
import { activityOf, interventions, post, roster, run, runsIn } from './api';
import { expect, signIn, test } from './fixtures';
import { errorV0, type ErrorV0 } from '../src/schema/error';
import { launchWorker } from './agents';
import { call } from './world';

test('P3-06 and P3-08 a slow run shows with the API ids and states, Cancel is acknowledged, Pause is not offered', async ({ meet, world }) => {
  const worker = await launchWorker(world, 'slow.sh', { sleep: 120 });
  try {
    const { page } = await meet();
    await signIn(page, world, world.tokens.browser, 'browser-test');
    const panel = page.getByRole('region', { name: 'Members list' });
    const agent = panel.getByRole('button', { name: /worker-test/ });
    // Idle first, as the roster says (the API's view of the same session).
    const agentRow = panel.getByRole('listitem').filter({ has: page.getByRole('button', { name: /worker-test/ }) });
    await expect(agentRow).toContainText('idle');
    expect((await roster(world, world.tokens.browser, 'general')).find((s) => s.id === worker.session)?.status).toBe('idle');

    const task = 'p3-06 slow work for the worker';
    const asked = await post(world, world.tokens.nick, 'general', task);
    let seen = 0;
    await expect
      .poll(async () => {
        seen = (await runsIn(world, world.tokens.browser, 'general')).find((r) => r.message_id === asked.id)?.id ?? 0;
        return seen;
      }, { message: 'a run answering the message' })
      .toBeGreaterThan(0);
    // Running, and stays so: the fixture waits two minutes unless it is stopped.
    await expect.poll(async () => (await run(world, world.tokens.browser, seen)).state, { message: 'the run is running' }).toBe('running');
    const started = await run(world, world.tokens.browser, seen);
    const live = (await roster(world, world.tokens.browser, 'general')).find((s) => s.id === worker.session);
    expect(live?.current_run).toBe(started.id);

    // Unfolded, the panel lists the session and the run with the ids and the
    // state the API reports.
    await agent.click();
    const sessionRow = panel.getByRole('list', { name: `Sessions of worker-test` }).getByRole('listitem').filter({ hasText: `#${String(started.session_id)}` });
    await expect(sessionRow).toBeVisible();
    await expect(sessionRow).toContainText(live?.status ?? 'running');
    const runButton = panel.getByRole('button', { name: new RegExp(`^Run #${String(started.id)} · `) });
    await expect(runButton).toHaveText(`Run #${String(started.id)} · ${started.state}`);

    // Into the run.
    await runButton.click();
    const detail = page.getByRole('article', { name: 'Run detail' });
    await expect(detail.getByRole('heading', { name: `Run #${String(started.id)}` })).toBeVisible();
    await expect(detail.getByTestId('run-state')).toHaveText(started.state);
    await expect(detail).toContainText(started.task);

    // Pause is not something a person is given here, and the server refuses it.
    await expect(detail.getByRole('button', { name: 'Pause' })).toHaveCount(0);
    const pause = await call(world, world.tokens.nick, 'POST', `/v1/runs/${String(started.id)}/interventions`, { kind: 'pause' });
    expect(pause.status).toBe(409);
    expect(errorV0(pause.body)).toEqual([]);
    expect((pause.body as ErrorV0).code).toBe('intervention_unsupported');

    // From here every change the page makes to the run's state or to its
    // history of asks is reported to the test as it happens, and the test
    // reads the API at that moment: the page must never be ahead of it.
    const changes: { state: string; asks: string[]; apiState: string; apiAsks: string[] }[] = [];
    await page.exposeFunction('conchObserved', async (state: string, asks: string[]) => {
      const stored = await run(world, world.tokens.browser, started.id);
      const stages = await interventions(world, world.tokens.browser, started.id);
      changes.push({ state, asks, apiState: stored.state, apiAsks: stages.map((i) => i.state) });
    });
    await page.evaluate(() => {
      const report = (window as unknown as { conchObserved: (state: string, asks: string[]) => Promise<void> }).conchObserved;
      let last = '';
      new MutationObserver(() => {
        const state = document.querySelector('[data-testid="run-state"]')?.textContent ?? '';
        const asks = [...document.querySelectorAll('[data-testid^="intervention-"]')].map((e) => /requested…|acknowledged|applied|failed/.exec(e.textContent)?.[0] ?? '');
        const key = JSON.stringify([state, asks]);
        if (key === last) return;
        last = key;
        void report(state, asks);
      }).observe(document.body, { subtree: true, childList: true, characterData: true });
    });

    // Cancel, with the confirmation.
    await detail.getByRole('button', { name: 'Cancel', exact: true }).click();
    await detail.getByRole('button', { name: 'Yes, cancel the run' }).click();

    await expect
      .poll(async () => (await run(world, world.tokens.browser, started.id)).state, { message: 'the API reports the run cancelled', timeout: 45_000 })
      .toBe('cancelled');
    await expect(detail.getByTestId('run-state')).toHaveText('cancelled');
    await expect(detail.getByText('The run has ended')).toBeVisible();

    // The ask is acknowledged, in the API and on the page, in that order.
    const asks = await interventions(world, world.tokens.browser, started.id);
    expect(asks).toHaveLength(1);
    const ask = asks[0];
    expect(ask?.kind).toBe('cancel');
    expect(['acknowledged', 'applied']).toContain(ask?.state);
    await expect(detail.getByTestId(`intervention-${String(ask?.id)}`)).toHaveText(ask?.state === 'applied' ? `↳ #${String(ask.id)} applied` : `↳ #${String(ask?.id)} acknowledged`);

    // Nothing the page showed was ahead of what the API said at that moment.
    await expect.poll(() => changes.some((o) => o.state === 'cancelled'), { message: 'the page was seen to change to cancelled' }).toBe(true);
    const rank: Record<string, number> = { 'requested…': 0, requested: 0, acknowledged: 1, applied: 2 };
    for (const o of changes) {
      if (o.state === 'cancelled') expect(o.apiState, 'the page showed cancelled before the API did').toBe('cancelled');
      o.asks.forEach((word, i) => {
        expect(rank[o.apiAsks[i] ?? ''] ?? -1, `the page showed ${word} before the API did`).toBeGreaterThanOrEqual(rank[word] ?? 99);
      });
    }
    expect(changes.some((o) => o.asks.some((w) => w === 'acknowledged' || w === 'applied'))).toBe(true);
  } finally {
    await worker.stop();
  }
});

test('P3-06 the Runs view lists the run with the API id and state, and its timeline reads the offset the API implies', async ({ meet, world }) => {
  const worker = await launchWorker(world, 'slow.sh', { sleep: 120 });
  try {
    const { page } = await meet();
    await signIn(page, world, world.tokens.browser, 'browser-test');
    const asked = await post(world, world.tokens.nick, 'general', 'p3-06 runs view work for the worker');
    let seen = 0;
    await expect
      .poll(async () => {
        seen = (await runsIn(world, world.tokens.browser, 'general')).find((r) => r.message_id === asked.id)?.id ?? 0;
        return seen;
      }, { message: 'a run answering the message' })
      .toBeGreaterThan(0);
    await expect.poll(async () => (await activityOf(world, world.tokens.browser, seen)).length, { message: 'the run reported activity' }).toBeGreaterThan(0);
    const started = await run(world, world.tokens.browser, seen);

    // The Runs view lists it with the API's id and state; selecting it opens it.
    const panel = page.getByRole('region', { name: 'Members list' });
    await expect(panel.getByRole('heading', { name: 'Runs' })).toBeVisible();
    const row = panel.getByRole('list', { name: 'Channel runs' }).getByRole('button', { name: new RegExp(`^#${String(seen)} · `) });
    await expect(row).toHaveText(`#${String(seen)} · ${started.state}`);
    await row.click();
    const detail = page.getByRole('article', { name: 'Run detail' });
    await expect(detail.getByRole('heading', { name: `Run #${String(seen)}` })).toBeVisible();
    await expect(detail.getByTestId('run-state')).toHaveText(started.state);

    // The first timeline line's offset is the API's first entry minus the run's start, to the second.
    const first = (await activityOf(world, world.tokens.browser, seen))[0];
    const offset = Math.max(0, Date.parse(first?.at ?? '') - Date.parse(started.started_at));
    const want = Math.floor(offset / 1000);
    const line = detail.getByRole('list', { name: 'Timeline' }).getByRole('listitem').first();
    await expect(line).toContainText(/^\s*\+/);
    const shown = (await line.textContent())?.match(/\+(\d+)(?:\.\d)?s/);
    expect(shown, 'an offset in seconds on the first line').not.toBeNull();
    if (want < 60) expect(Number(shown?.[1])).toBe(want);
  } finally {
    await worker.stop();
  }
});
