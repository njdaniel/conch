import { describe, expect, it } from 'vitest';
import { MAX_FILE_NAME_BYTES, SPOILER_FILE_PREFIX } from './schema/file';
import { isSpoilerName, messagePreview, shownName, spoilerName, storedName } from './spoilerFiles';

const bytes = (text: string) => new TextEncoder().encode(text).length;
const prefixed = (rest: string) => `${SPOILER_FILE_PREFIX}${rest}`;

describe('isSpoilerName', () => {
  it.each([
    [prefixed('x.png'), true],
    [SPOILER_FILE_PREFIX, true],
    [prefixed(prefixed('x.png')), true],
    [`${SPOILER_FILE_PREFIX.toLowerCase()}x.png`, false],
    [`${SPOILER_FILE_PREFIX.slice(0, -1)}x.png`, false],
    [`X${SPOILER_FILE_PREFIX}`, false],
    [` ${prefixed('x.png')}`, false],
    ['x.png', false],
    ['', false],
  ])('%j is %s', (name, want) => {
    expect(isSpoilerName(name)).toBe(want);
  });
});

describe('shownName', () => {
  it.each([
    [prefixed('x.png'), 'x.png'],
    [prefixed(prefixed('x.png')), prefixed('x.png')],
    ['x.png', 'x.png'],
    [`${SPOILER_FILE_PREFIX.toLowerCase()}x.png`, `${SPOILER_FILE_PREFIX.toLowerCase()}x.png`],
  ])('%j reads %j', (name, want) => {
    expect(shownName(name)).toBe(want);
  });
});

describe('spoilerName', () => {
  const room = MAX_FILE_NAME_BYTES - bytes(SPOILER_FILE_PREFIX);
  const cases: readonly [string, string][] = [
    ['a short name', 'photo.png'],
    ['no extension', 'photo'],
    ['one letter', 'a'],
    ['a dot file', '.hidden'],
    ['exactly fitting', 'a'.repeat(room)],
    ['one byte over', 'a'.repeat(room + 1)],
    ['255 bytes with an extension', `${'a'.repeat(MAX_FILE_NAME_BYTES - 4)}.png`],
    ['255 two-byte characters', 'é'.repeat(127)],
    ['multi-byte characters before an extension', `${'日'.repeat(100)}.png`],
    ['four-byte characters before an extension', `${'😀'.repeat(70)}.jpeg`],
    ['an extension too long to keep', `a.${'b'.repeat(MAX_FILE_NAME_BYTES - 2)}`],
    ['a name that is only dots and multi-byte', `.${'日'.repeat(84)}`],
  ];

  it.each(cases)('keeps %s valid, prefixed and within the limit', (_label, name) => {
    const made = spoilerName(name);
    expect(made.startsWith(SPOILER_FILE_PREFIX)).toBe(true);
    expect(bytes(made)).toBeLessThanOrEqual(MAX_FILE_NAME_BYTES);
    // Cut at a character boundary: it round-trips through UTF-8.
    expect(new TextDecoder('utf-8', { fatal: true }).decode(new TextEncoder().encode(made))).toBe(made);
    expect(made).not.toContain('�');
    expect(spoilerName(made)).toBe(made);
  });

  it('only adds the prefix when it fits', () => {
    expect(spoilerName('photo.png')).toBe(prefixed('photo.png'));
    const fitting = 'a'.repeat(room);
    expect(spoilerName(fitting)).toBe(prefixed(fitting));
  });

  it('trims the base name and keeps the extension', () => {
    const made = spoilerName(`${'a'.repeat(MAX_FILE_NAME_BYTES)}.png`);
    expect(made.endsWith('.png')).toBe(true);
    expect(bytes(made)).toBe(MAX_FILE_NAME_BYTES);
    const wide = spoilerName(`${'日'.repeat(200)}.png`);
    expect(wide.endsWith('.png')).toBe(true);
    expect(bytes(wide)).toBeLessThanOrEqual(MAX_FILE_NAME_BYTES);
    expect(bytes(wide)).toBeGreaterThan(MAX_FILE_NAME_BYTES - 3);
  });

  it('leaves a name that already has the prefix alone', () => {
    const already = prefixed('x.png');
    expect(spoilerName(already)).toBe(already);
  });
});

describe('storedName', () => {
  it('is the name as chosen unless the draft is marked', () => {
    expect(storedName({ filename: 'x.png', spoiler: false })).toBe('x.png');
    expect(storedName({ filename: 'x.png', spoiler: true })).toBe(prefixed('x.png'));
  });
});

describe('messagePreview', () => {
  const attachment = { schema: 'conch.file_attachment.v1', data: { file_ids: [1] } };
  it('reads an image-only spoiler as [spoiler] and its shown name', () => {
    expect(messagePreview({ body: prefixed('x.png'), payload: attachment })).toBe('[spoiler] x.png');
  });
  it('keeps other names and a body with no attachment', () => {
    expect(messagePreview({ body: 'x.png', payload: attachment })).toBe('x.png');
    expect(messagePreview({ body: prefixed('x.png') })).toBe(prefixed('x.png'));
    expect(messagePreview({ body: 'see ||this||' })).toBe('see [spoiler]');
  });
});
