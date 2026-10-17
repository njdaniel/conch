import { describe, expect, it } from 'vitest';
import { afterDisconnect, afterRefusal, retryDelay, type Next } from './disconnect';

describe('after a disconnect', () => {
  it.each<[Parameters<typeof afterDisconnect>[0], Next['kind']]>([
    ['room_deleted', 'new_session'],
    ['participant_removed', 'new_session'],
    ['duplicate_identity', 'stop'],
    ['server_shutdown', 'retry'],
    ['lost', 'retry'],
  ])('%s -> %s', (cause, kind) => {
    expect(afterDisconnect(cause).kind).toBe(kind);
  });

  it('a duplicate identity says another device took over', () => {
    const next = afterDisconnect('duplicate_identity');
    expect(next.kind === 'stop' && next.why).toMatch(/Another device/);
  });
});

describe('after a refused session', () => {
  it.each<[string, Parameters<typeof afterRefusal>[0], Next['kind']]>([
    ['401', { ok: false, failure: 'unauthorized' }, 'stop'],
    ['404', { ok: false, failure: 'invalid', status: 404 }, 'stop'],
    ['403', { ok: false, failure: 'invalid', status: 403 }, 'stop'],
    ['400', { ok: false, failure: 'invalid', status: 400 }, 'stop'],
    ['not configured', { ok: false, failure: 'invalid', status: 503, code: 'voice_not_configured' }, 'stop'],
    ['unavailable', { ok: false, failure: 'invalid', status: 503, code: 'voice_unavailable' }, 'retry'],
    ['no answer', { ok: false, failure: 'network' }, 'retry'],
    ['500', { ok: false, failure: 'invalid', status: 500 }, 'retry'],
  ])('%s -> %s', (_name, refusal, kind) => {
    expect(afterRefusal(refusal).kind).toBe(kind);
  });
});

describe('retry delay', () => {
  it('doubles from 1 s to 30 s with jitter between half and all of it', () => {
    const ceilings = [1000, 2000, 4000, 8000, 16000, 30000, 30000];
    [0, 1, 2, 3, 4, 5, 9].forEach((n, i) => {
      const ceiling = ceilings[i] ?? 0;
      expect(retryDelay(n, () => 0)).toBe(ceiling / 2);
      expect(retryDelay(n, () => 0.999999)).toBeGreaterThanOrEqual(ceiling - 1);
      expect(retryDelay(n, () => 0.999999)).toBeLessThanOrEqual(ceiling);
    });
    expect(retryDelay(0, () => 0)).toBe(500);
  });
});
