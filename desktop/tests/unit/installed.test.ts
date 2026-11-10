// The installed-files watcher (issue #1600) over a fake file system and a fake
// clock: nothing here reads a real file or waits for real time.
import { describe, expect, it } from 'vitest';

import type { InstalledView } from '../../../web/src/platform/desktop/bridge.ts';
import { UPDATE_EVENTS } from '../../shared/bridge-names.ts';
import { DocumentGate } from '../../main/document.ts';
import { deliverUpdate, INTERVAL_MS, Installed, parseStamp } from '../../main/installed.ts';

// A stamp file that can be changed, made unreadable or made to say anything.
class FakeFs {
  stamp: string | Error = '0.1.0-alpha.17\n';
  reads = 0;
  read = (): Promise<string> => {
    this.reads++;
    return this.stamp instanceof Error ? Promise.reject(this.stamp) : Promise.resolve(this.stamp);
  };
}

// A clock that runs the callbacks registered with every() when advanced.
class FakeClock {
  #timers = new Map<number, { fn: () => void; ms: number; at: number }>();
  #next = 0;
  now = 0;
  every = (fn: () => void, ms: number): (() => void) => {
    const id = this.#next++;
    this.#timers.set(id, { fn, ms, at: this.now + ms });
    return () => {
      this.#timers.delete(id);
    };
  };
  advance(ms: number): void {
    const end = this.now + ms;
    for (;;) {
      const due = [...this.#timers.values()].filter((t) => t.at <= end).sort((a, b) => a.at - b.at)[0];
      if (due === undefined) break;
      this.now = due.at;
      due.at += due.ms;
      due.fn();
    }
    this.now = end;
  }
  get timers(): number {
    return this.#timers.size;
  }
}

interface Rig {
  readonly fs: FakeFs;
  readonly clock: FakeClock;
  readonly installed: Installed;
  readonly logs: string[];
  readonly views: InstalledView[];
  readonly relaunches: { n: number };
  readonly asks: (string | null)[];
  answer: boolean;
}

async function rig(over: { running?: string } = {}): Promise<Rig> {
  const fs = new FakeFs();
  const clock = new FakeClock();
  const logs: string[] = [];
  const views: InstalledView[] = [];
  const relaunches = { n: 0 };
  const asks: (string | null)[] = [];
  const r: Rig = {
    fs,
    clock,
    logs,
    views,
    relaunches,
    asks,
    answer: false,
    installed: new Installed({
      read: fs.read,
      running: over.running ?? '0.1.0-alpha.17',
      every: clock.every,
      relaunch: () => {
        relaunches.n++;
      },
      ask: (version) => {
        asks.push(version);
        return Promise.resolve(r.answer);
      },
      log: (line) => logs.push(line),
    }),
  };
  r.installed.subscribe((v) => views.push(v));
  await r.installed.start();
  return r;
}

describe('noticing a replaced install', () => {
  it('shows nothing while the stamp is the one read at start, at a focus or an interval', async () => {
    const r = await rig();
    await r.installed.check();
    r.clock.advance(INTERVAL_MS * 3);
    await r.installed.check();
    expect(r.installed.updated()).toBe(false);
    expect(r.installed.view()).toEqual({ updated: false, version: null });
    expect(r.views).toEqual([]);
  });

  it('a changed stamp shows the notice at the next focus', async () => {
    const r = await rig();
    r.fs.stamp = '0.1.0-alpha.18\n';
    await r.installed.check();
    expect(r.installed.updated()).toBe(true);
    expect(r.views).toEqual([{ updated: true, version: '0.1.0-alpha.18' }]);
  });

  it('a changed stamp shows the notice within one interval, and not before it', async () => {
    const r = await rig();
    r.fs.stamp = '0.1.0-alpha.18';
    r.clock.advance(INTERVAL_MS - 1);
    await Promise.resolve();
    expect(r.installed.updated()).toBe(false);
    r.clock.advance(1);
    await r.installed.check();
    expect(r.views).toEqual([{ updated: true, version: '0.1.0-alpha.18' }]);
  });

  it('says it once, however many checks follow, and again for a newer stamp', async () => {
    const r = await rig();
    r.fs.stamp = '0.1.0-alpha.18';
    await r.installed.check();
    await r.installed.check();
    expect(r.views).toHaveLength(1);
    r.fs.stamp = '0.1.0-alpha.19';
    await r.installed.check();
    expect(r.views.map((v) => v.version)).toEqual(['0.1.0-alpha.18', '0.1.0-alpha.19']);
  });

  it('two checks at once share one read', async () => {
    const r = await rig();
    const before = r.fs.reads;
    await Promise.all([r.installed.check(), r.installed.check()]);
    expect(r.fs.reads).toBe(before + 1);
  });

  it('an unreadable stamp is logged once and shows nothing, at start or later', async () => {
    const r = await rig();
    r.fs.stamp = new Error('ENOENT');
    await r.installed.check();
    await r.installed.check();
    r.clock.advance(INTERVAL_MS);
    expect(r.logs.filter((l) => l.includes('stamp'))).toHaveLength(1);
    expect(r.views).toEqual([]);
    expect(r.installed.updated()).toBe(false);
  });

  it('an unreadable stamp after a notice keeps the notice', async () => {
    const r = await rig();
    r.fs.stamp = '0.1.0-alpha.18';
    await r.installed.check();
    r.fs.stamp = new Error('EACCES');
    await r.installed.check();
    expect(r.installed.updated()).toBe(true);
  });

  it('a stamp unreadable at start compares later ones with the running version', async () => {
    const fs = new FakeFs();
    fs.stamp = new Error('ENOENT');
    const logs: string[] = [];
    const installed = new Installed({ read: fs.read, running: '0.1.0-alpha.17', every: new FakeClock().every, relaunch: () => undefined, ask: () => Promise.resolve(false), log: (l) => logs.push(l) });
    await installed.start();
    expect(logs).toHaveLength(1);
    fs.stamp = '0.1.0-alpha.17';
    await installed.check();
    expect(installed.updated()).toBe(false);
    fs.stamp = '0.1.0-alpha.18';
    await installed.check();
    expect(installed.updated()).toBe(true);
  });

  it.each(['', '   ', '1.0\n2.0', '<b>1.0</b>', '../../etc', 'x'.repeat(65), '1.0 beta'])('a stamp of %j is not a version and counts as unreadable', async (text) => {
    const r = await rig();
    r.fs.stamp = text;
    await r.installed.check();
    expect(r.installed.updated()).toBe(false);
    expect(r.views).toEqual([]);
    expect(r.logs).toHaveLength(1);
  });

  it('parses the versions the build writes', () => {
    for (const v of ['0.1.0-alpha.18', '0.0.0-dev+abc1234', '1.2.3']) expect(parseStamp(`${v}\n`)).toBe(v);
  });

  it('stop ends the interval', async () => {
    const r = await rig();
    expect(r.clock.timers).toBe(1);
    r.installed.stop();
    expect(r.clock.timers).toBe(0);
  });
});

describe('restarting', () => {
  it('relaunches only when asked, and only after an update', async () => {
    const r = await rig();
    r.installed.restart();
    expect(r.relaunches.n).toBe(0);
    r.fs.stamp = '0.1.0-alpha.18';
    await r.installed.check();
    r.clock.advance(INTERVAL_MS * 5);
    expect(r.relaunches.n).toBe(0);
    r.installed.restart();
    expect(r.relaunches.n).toBe(1);
  });

  it('a page that fails to start after the update asks once, and relaunches only on Restart', async () => {
    const r = await rig();
    await r.installed.pageFailed();
    expect(r.asks).toEqual([]);
    r.fs.stamp = '0.1.0-alpha.18';
    await r.installed.check();
    await r.installed.pageFailed();
    await r.installed.pageFailed();
    expect(r.asks).toEqual(['0.1.0-alpha.18']);
    expect(r.relaunches.n).toBe(0);
  });

  it('chooses Restart in the dialog and the app relaunches', async () => {
    const r = await rig();
    r.fs.stamp = '0.1.0-alpha.18';
    await r.installed.check();
    r.answer = true;
    await r.installed.pageFailed();
    expect(r.relaunches.n).toBe(1);
  });

  it('measures a page that started after the update', async () => {
    const r = await rig();
    r.installed.pageStarted();
    expect(r.logs).toEqual([]);
    r.fs.stamp = '0.1.0-alpha.18';
    await r.installed.check();
    r.installed.pageStarted();
    expect(r.logs).toEqual(['installed: a page started after the update']);
  });
});

describe('delivery', () => {
  it('sends to the owned page only while its document is committed', () => {
    const gate = new DocumentGate();
    const sent: [string, InstalledView][] = [];
    const target = { contents: () => ({ send: (c: string, v: InstalledView) => sent.push([c, v]) }), gate: () => gate };
    const view = { updated: true, version: '0.1.0-alpha.18' };
    gate.begin();
    deliverUpdate(target, view);
    expect(sent).toEqual([]);
    gate.commit({ frameToken: 'f' });
    deliverUpdate(target, view);
    expect(sent).toEqual([[UPDATE_EVENTS, view]]);
    deliverUpdate({ contents: () => null, gate: () => gate }, view);
    expect(sent).toHaveLength(1);
  });
});
