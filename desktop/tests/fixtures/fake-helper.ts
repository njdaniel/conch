// A stand-in for conch-voice managed --events-pipe, run by the unit tests as
// a Node child through a wrapper script in a fake resources directory. It
// replays a transcript from pkg/schema/testdata/voice-control*/ (or the pipe
// directory): it connects to the socket the owner named, writes an attach
// line, then writes the transcript's helper lines and waits for each owner
// step, and reports what it saw to a file the test reads.
//
// It needs no network beyond the owner's Unix socket and imports nothing but
// Node's own modules: every line it writes is a fixture's, so no wire shape
// is made up here.
//
// usage: fake-helper.ts <scenario.json> managed --events-pipe <path> [...]
import { appendFileSync, readFileSync } from 'node:fs';
import { connect, type Socket } from 'node:net';
import { createInterface } from 'node:readline';

interface Step {
  readonly from: 'helper' | 'owner';
  readonly message?: Record<string, unknown>;
  readonly close?: string;
  readonly signal?: string;
  readonly exit?: number;
}

export interface Scenario {
  // The file each observation is appended to, one JSON object per line.
  readonly report: string;
  // A transcript fixture, by path.
  readonly transcript?: string;
  // The attach: the right nonce (default), the fixture's own (a wrong one), a
  // line past 4096 bytes, nothing at all, or the right one from two
  // connections.
  readonly attach?: 'nonce' | 'wrong' | 'long' | 'none' | 'twice';
  // The attached.json fixture, for the attach line.
  readonly attached: string;
  // Replay only the steps before this index, then do `then`.
  readonly stopAt?: number;
  // What to do once replay stops: wait for the owner (default), exit with a
  // code, end the events connection and stay alive, write a line, or hang
  // (ignore the end of standard input, the events connection and SIGTERM).
  readonly then?: 'wait' | 'exit' | 'close-events' | 'line' | 'hang';
  readonly exitCode?: number;
  readonly line?: string;
  // Replace the versions the transcript's hello offers (a helper of a later
  // release than any fixture).
  readonly helloVersions?: readonly string[];
  // Print the token and the nonce to standard error, as a careless helper
  // might, so that the owner's redaction is tested.
  readonly leakToStderr?: boolean;
}

const scenarioPath = process.argv[2] ?? '';
const scenario = JSON.parse(readFileSync(scenarioPath, 'utf8')) as Scenario;
const args = process.argv.slice(3);

function report(entry: Record<string, unknown>): void {
  appendFileSync(scenario.report, `${JSON.stringify({ at: Date.now(), pid: process.pid, ...entry })}\n`);
}

report({ t: 'argv', argv: args });
report({ t: 'env', env: { ...process.env } });

const hang = scenario.then === 'hang';
process.on('SIGTERM', () => {
  report({ t: 'sigterm' });
  if (!hang) process.exit(0);
});

if (scenario.leakToStderr === true) {
  process.stderr.write(`token=${process.env['CONCH_TOKEN'] ?? ''} nonce=${process.env['CONCH_VOICE_ATTACH'] ?? ''}\n`);
}

const at = args.indexOf('--events-pipe');
const path = at >= 0 ? (args[at + 1] ?? '') : '';

// Standard input: the owner's commands, one per line, and its end.
const stdinLines: string[] = [];
let stdinEnded = false;
let wake: (() => void) | null = null;
const rl = createInterface({ input: process.stdin });
rl.on('line', (line) => {
  report({ t: 'stdin', line });
  stdinLines.push(line);
  wake?.();
});
rl.on('close', () => {
  report({ t: 'stdin-end' });
  stdinEnded = true;
  wake?.();
  if (!hang && eventsEnded) process.exit(0);
});

function nextLine(): Promise<string | null> {
  return new Promise((resolve) => {
    const check = (): void => {
      const line = stdinLines.shift();
      if (line !== undefined) {
        wake = null;
        resolve(line);
      } else if (stdinEnded) {
        wake = null;
        resolve(null);
      } else {
        wake = check;
      }
    };
    check();
  });
}

function stdinEnd(): Promise<void> {
  return new Promise((resolve) => {
    const check = (): void => {
      if (stdinEnded) {
        wake = null;
        resolve();
      } else {
        stdinLines.length = 0;
        wake = check;
      }
    };
    check();
  });
}

let eventsEnded = false;
let received = 0;

function open(): Promise<Socket> {
  return new Promise((resolve, reject) => {
    const s = connect(path, () => {
      resolve(s);
    });
    s.on('error', reject);
  });
}

function attachLine(): string {
  const doc = JSON.parse(readFileSync(scenario.attached, 'utf8')) as Record<string, unknown>;
  if (scenario.attach !== 'wrong') doc['nonce'] = process.env['CONCH_VOICE_ATTACH'] ?? '';
  return JSON.stringify(doc);
}

async function main(): Promise<void> {
  if (scenario.attach === 'none') {
    // Never connects; waits to be stopped.
    await stdinEnd();
    process.exit(0);
  }
  const events = await open();
  events.on('error', () => undefined);
  events.on('data', (chunk: Buffer) => {
    received += chunk.length;
    report({ t: 'events-bytes', n: received });
  });
  events.on('end', () => {
    eventsEnded = true;
    report({ t: 'events-end' });
    // Owner loss, as the helper sees it: once standard input has ended too,
    // exit.
    if (!hang && stdinEnded) process.exit(0);
  });
  if (scenario.attach === 'twice') {
    const second = await open().catch(() => null);
    report({ t: 'second', connected: second !== null });
  }
  if (scenario.attach === 'long') {
    events.write(`${'x'.repeat(5000)}\n`);
    await stdinEnd();
    process.exit(0);
  }
  events.write(`${attachLine()}\n`);
  const write = (doc: Record<string, unknown>): void => {
    if (!events.destroyed) events.write(`${JSON.stringify(doc)}\n`);
  };

  const steps: Step[] = scenario.transcript === undefined ? [] : (JSON.parse(readFileSync(scenario.transcript, 'utf8')) as { steps: Step[] }).steps;
  const end = scenario.stopAt ?? steps.length;
  for (let i = 0; i < end; i++) {
    const step = steps[i];
    if (step === undefined) break;
    if (step.from === 'helper' && step.message !== undefined) {
      // The transcript's own attach line is replaced by the one above.
      if (step.message['kind'] === 'attached') continue;
      if (step.message['kind'] === 'hello' && scenario.helloVersions !== undefined) write({ ...step.message, versions: scenario.helloVersions });
      else write(step.message);
    } else if (step.from === 'owner' && step.message !== undefined) {
      const line = await nextLine();
      if (line === null) {
        // The owner went away early: what the helper does on owner loss.
        if (!hang) process.exit(0);
        return;
      }
    } else if (step.from === 'owner' && step.close !== undefined) {
      await stdinEnd();
    } else if (step.from === 'owner' && step.signal !== undefined) {
      await new Promise<void>((resolve) => process.once('SIGTERM', () => {
        resolve();
      }));
    } else if (step.from === 'helper' && step.exit !== undefined) {
      events.end();
      process.exit(step.exit);
    }
  }
  switch (scenario.then ?? 'wait') {
    case 'exit':
      process.exit(scenario.exitCode ?? 3);
      break;
    case 'close-events':
      events.end();
      await stdinEnd();
      process.exit(0);
      break;
    case 'line':
      events.write(`${scenario.line ?? ''}\n`);
      await stdinEnd();
      process.exit(0);
      break;
    case 'hang':
      // Ignore everything; only SIGKILL ends this.
      setInterval(() => undefined, 1000);
      break;
    case 'wait':
      await stdinEnd();
      process.exit(0);
  }
}

main().catch((err: unknown) => {
  report({ t: 'error', message: err instanceof Error ? err.message : 'unknown' });
  process.exit(9);
});
