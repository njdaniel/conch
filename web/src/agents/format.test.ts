import { describe, expect, it } from 'vitest';
import { dataBody, dataLimit, spanText } from './format';

describe('spanText', () => {
  it.each<[number, boolean, string]>([
    [0, true, '0.0s'],
    [12_450, true, '12.4s'],
    [12_450, false, '12s'],
    [59_999, false, '59s'],
    [60_000, true, '1m00s'],
    [182_000, false, '3m02s'],
    [3_723_000, false, '1h02m03s'],
    [-5000, true, '0.0s'],
  ])('writes %i ms (tenths %s) as %s', (ms, tenths, want) => {
    expect(spanText(ms, tenths)).toBe(want);
  });
});

describe('dataBody', () => {
  it('pretty-prints small data whole', () => {
    expect(dataBody({ a: [1, 2] })).toBe('{\n  "a": [\n    1,\n    2\n  ]\n}');
  });

  it('cuts at the limit in bytes with an ellipsis, never inside a character', () => {
    const text = dataBody({ t: 'é'.repeat(3000) });
    expect(text.endsWith('…')).toBe(true);
    const kept = new TextEncoder().encode(text.slice(0, -1)).length;
    expect(kept).toBeLessThanOrEqual(dataLimit);
    expect(kept).toBeGreaterThan(dataLimit - 2);
    expect(text).not.toContain('\ufffd');
  });
});
