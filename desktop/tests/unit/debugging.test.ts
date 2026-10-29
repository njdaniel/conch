import { describe, expect, it } from 'vitest';

import { debuggingVerdict, ON_LINE } from '../../main/debugging.ts';

function run(packaged: boolean, switches: string[], env: Record<string, string> = {}): { verdict: string; logs: string[] } {
  const logs: string[] = [];
  const verdict = debuggingVerdict(packaged, (n) => switches.includes(n), env, (l) => logs.push(l));
  return { verdict, logs };
}

describe('debuggingVerdict', () => {
  it.each(['remote-debugging-port', 'remote-debugging-pipe'])('refuses a packaged app with --%s and prints the receipt', (sw) => {
    const { verdict, logs } = run(true, [sw]);
    expect(verdict).toBe('refused');
    expect(logs[0]).toBe(ON_LINE);
    expect(logs).toHaveLength(2);
  });

  it.each(['remote-debugging-port', 'remote-debugging-pipe'])('allows --%s only with CONCH_DESKTOP_ALLOW_CDP=1, still printing the receipt', (sw) => {
    const { verdict, logs } = run(true, [sw], { CONCH_DESKTOP_ALLOW_CDP: '1' });
    expect(verdict).toBe('allowed');
    expect(logs).toEqual([ON_LINE]);
  });

  it.each(['', '0', 'true', 'yes'])('does not take %j for the opt-in', (value) => {
    expect(run(true, ['remote-debugging-port'], { CONCH_DESKTOP_ALLOW_CDP: value }).verdict).toBe('refused');
  });

  it('says nothing when neither switch is present', () => {
    expect(run(true, ['inspect'])).toEqual({ verdict: 'off', logs: [] });
  });

  it('leaves an unpackaged app alone', () => {
    expect(run(false, ['remote-debugging-port']).verdict).toBe('off');
  });
});
