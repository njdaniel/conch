// A stand-in for `conch-voice devices --json` and `conch-voice config set`,
// run by the unit tests as a Node child through a wrapper script in a fake
// resources directory (issue #1378). It reports what it was given (argv, the
// whole environment, the working directory, whether standard input is
// /dev/null) to a file the test reads, then behaves as the scenario says.
// Every document it writes is a pkg/schema fixture's or the scenario's own
// bytes, so no wire shape is made up here. Imports only Node's modules.
//
// usage: fake-oneshot.ts <scenario.json> <helper arguments...>
import { appendFileSync, fstatSync, readFileSync, statSync } from 'node:fs';

export interface OneShotScenario {
  readonly report: string;
  // What to write to standard output: a file's bytes, a literal, or a stream
  // of `fill` bytes of 'x' (for an oversize run).
  readonly stdoutFile?: string;
  readonly stdout?: string;
  readonly fill?: number;
  // What to write to standard error.
  readonly stderr?: string;
  // Then: exit with this status (default 0), or hang until killed.
  readonly exitCode?: number;
  readonly hang?: boolean;
  // Start a background process that holds the pipes open after this exits.
  readonly grandchild?: boolean;
}

const scenario = JSON.parse(readFileSync(process.argv[2] ?? '', 'utf8')) as OneShotScenario;
const args = process.argv.slice(3);

let devNull = false;
try {
  devNull = fstatSync(0).isCharacterDevice() && fstatSync(0).rdev === statSync('/dev/null').rdev;
} catch {
  devNull = false;
}
appendFileSync(scenario.report, `${JSON.stringify({ pid: process.pid, argv: args, env: process.env, cwd: process.cwd(), stdinIsDevNull: devNull })}\n`);

if (scenario.stderr !== undefined) process.stderr.write(scenario.stderr);

const done = (): void => {
  if (scenario.hang === true) {
    setInterval(() => undefined, 1000);
    return;
  }
  process.exitCode = scenario.exitCode ?? 0;
};

if (scenario.stdoutFile !== undefined) {
  process.stdout.write(readFileSync(scenario.stdoutFile), done);
} else if (scenario.stdout !== undefined) {
  process.stdout.write(scenario.stdout, done);
} else if (scenario.fill !== undefined) {
  // Written in chunks, so that the reader can stop it part of the way.
  let left = scenario.fill;
  const chunk = Buffer.alloc(16 * 1024, 'x');
  const pump = (): void => {
    while (left > 0) {
      const n = Math.min(left, chunk.length);
      left -= n;
      if (!process.stdout.write(n === chunk.length ? chunk : chunk.subarray(0, n))) {
        process.stdout.once('drain', pump);
        return;
      }
    }
    done();
  };
  pump();
} else {
  done();
}
