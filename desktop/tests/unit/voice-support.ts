// Test support for the voice supervisor: a private scratch directory with a
// runtime directory, a configuration directory and a fake resources
// directory whose conch-voice runs tests/fixtures/fake-helper.ts; the
// fixtures of pkg/schema/testdata/voice-control*/; and the reader of what
// the fake helper reported.
import { chmodSync, existsSync, mkdirSync, mkdtempSync, readFileSync, rmSync, writeFileSync } from 'node:fs';
import { tmpdir } from 'node:os';
import { join, resolve } from 'node:path';

import type { Scenario } from '../fixtures/fake-helper.ts';

export const TESTDATA = resolve(import.meta.dirname, '../../../pkg/schema/testdata');
export const FAKE_HELPER = resolve(import.meta.dirname, '../fixtures/fake-helper.ts');

export function fixture(path: string): string {
  return join(TESTDATA, path);
}

// eslint-disable-next-line @typescript-eslint/no-unnecessary-type-parameters -- the caller names the fixture's shape
export function readFixture<T>(path: string): T {
  return JSON.parse(readFileSync(fixture(path), 'utf8')) as T;
}

// The fake login token of pkg/schema/testdata/voice-control/sensitive.json's
// kind: a value no real server would issue, to be found nowhere it must not be.
export const FAKE_TOKEN = 'conch_FAKE_login_token_do_not_print';

export interface Scratch {
  readonly root: string;
  readonly run: string;
  readonly config: string;
  readonly resources: string;
  readonly report: string;
  // Writes the scenario and a conch-voice wrapper that runs the fake with it.
  helper(scenario: Omit<Scenario, 'report' | 'attached'> & { readonly attached?: string }): void;
  reported(): Record<string, unknown>[];
  cleanup(): void;
}

export function scratch(): Scratch {
  const root = mkdtempSync(join(tmpdir(), 'conch-voice-'));
  const run = join(root, 'run');
  const config = join(root, 'config');
  const resources = join(root, 'resources');
  for (const dir of [run, config, resources]) {
    mkdirSync(dir);
    chmodSync(dir, 0o700);
  }
  chmodSync(resources, 0o755);
  const report = join(root, 'report.jsonl');
  return {
    root,
    run,
    config,
    resources,
    report,
    helper(scenario) {
      const path = join(root, 'scenario.json');
      writeFileSync(path, JSON.stringify({ attached: fixture('voice-control-pipe/attached.json'), ...scenario, report }));
      const quote = (s: string): string => `'${s.replace(/'/g, `'\\''`)}'`;
      const script = join(resources, 'conch-voice');
      writeFileSync(script, `#!/bin/sh\nexec ${quote(process.execPath)} ${quote(FAKE_HELPER)} ${quote(path)} "$@"\n`);
      chmodSync(script, 0o755);
    },
    reported() {
      if (!existsSync(report)) return [];
      return readFileSync(report, 'utf8')
        .split('\n')
        .filter((l) => l !== '')
        .map((l) => JSON.parse(l) as Record<string, unknown>);
    },
    cleanup() {
      rmSync(root, { recursive: true, force: true });
    },
  };
}

// until polls cond every 10 ms until it holds or ms pass.
export async function until(cond: () => boolean, ms = 10_000, what = 'the condition'): Promise<void> {
  const end = Date.now() + ms;
  while (!cond()) {
    if (Date.now() > end) throw new Error(`timed out waiting for ${what}`);
    await new Promise((r) => setTimeout(r, 10));
  }
}

export interface TranscriptStep {
  readonly from: 'helper' | 'owner';
  readonly message?: Record<string, unknown>;
  readonly close?: string;
  readonly signal?: string;
  readonly exit?: number;
}

export function transcript(path: string): TranscriptStep[] {
  return readFixture<{ steps: TranscriptStep[] }>(path).steps;
}

// ownerLines is what a transcript's owner writes, in order.
export function ownerLines(path: string): Record<string, unknown>[] {
  return transcript(path)
    .filter((s) => s.from === 'owner' && s.message !== undefined)
    .map((s) => s.message ?? {});
}
