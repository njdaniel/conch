import { describe, expect, it } from 'vitest';
import { dateOf, dateTimeOf, timeOf, timeWithSeconds } from './time';

// A moment built from local parts, so the expected text does not depend on the
// time zone the tests run in.
const at = new Date(2026, 10, 6, 20, 36, 8);
const early = new Date(2026, 10, 6, 9, 5, 2);

describe('the time helper', () => {
  it('writes every time on a 24-hour clock, to the minute or the second', () => {
    expect(timeOf(at)).toBe('20:36');
    expect(timeOf(early)).toBe('09:05');
    expect(timeWithSeconds(at)).toBe('20:36:08');
    expect(timeWithSeconds(early)).toBe('09:05:02');
  });

  it('writes a date and time with the same hour cycle as the time alone', () => {
    expect(dateTimeOf(at)).toContain(timeOf(at));
    expect(dateTimeOf(early)).toContain(timeOf(early));
    expect(dateTimeOf(at)).toContain(dateOf(at).replace(/,? 2026$/, ''));
    expect(dateTimeOf(at)).not.toMatch(/AM|PM/i);
  });

  it('takes a string, a number or a date, and the same moment reads the same', () => {
    expect(timeOf(at.toISOString())).toBe(timeOf(at));
    expect(timeOf(at.getTime())).toBe(timeOf(at));
  });

  it('returns what it was given when that is not a time', () => {
    expect(timeOf('soon')).toBe('soon');
    expect(dateTimeOf('')).toBe('');
  });
});
