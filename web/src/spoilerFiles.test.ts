import { afterEach, beforeEach, describe, expect, it } from 'vitest';
import { MAX_FILE_NAME_BYTES, SPOILER_FILE_PREFIX } from './schema/file';
import { forgetFileNames, rememberFileName, attachmentNames, canSpoilerName, displayBody, isSpoilerName, messagePreview, shownName, spoilerName, storedName } from './spoilerFiles';

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

describe('canSpoilerName', () => {
  const room = MAX_FILE_NAME_BYTES - bytes(SPOILER_FILE_PREFIX);
  it.each([
    ['a short name', 'photo.png', true],
    ['a long base with a short extension', `${'a'.repeat(300)}.png`, true],
    ['an extension that leaves one byte of base', `a.${'b'.repeat(room - 2)}`, true],
    ['an extension of exactly the room', `a.${'b'.repeat(room - 1)}`, false],
    ['an extension over the room', `a.${'b'.repeat(room + 20)}`, false],
    ['a name that already has the prefix', prefixed(`a.${'b'.repeat(room + 20)}`), true],
  ])('%s', (_label, name, want) => {
    expect(canSpoilerName(name)).toBe(want);
  });
});

const attachment = (count: number) => ({ schema: 'conch.file_attachment.v1', data: { file_ids: Array.from({ length: count }, (_, i) => i + 1) } });

describe('attachmentNames and displayBody', () => {
  it('reads the names of an image-only message, one per file', () => {
    expect(attachmentNames({ body: `${prefixed('a.png')}, b.png`, payload: attachment(2) })).toEqual([prefixed('a.png'), 'b.png']);
    expect(displayBody({ body: `${prefixed('a.png')}, ${prefixed('b.png')}`, payload: attachment(2) })).toBe('a.png, b.png');
  });
  it('leaves a caption, a body that does not match the files and a message with no attachment alone', () => {
    expect(attachmentNames({ body: `${prefixed('note')} ||the, secret||`, payload: attachment(1) })).toBeNull();
    expect(attachmentNames({ body: 'a, b', payload: attachment(1) })).toBeNull();
    expect(displayBody({ body: prefixed('note') })).toBe(prefixed('note'));
    // One part for one file is taken for its name, a name with a space included.
    expect(displayBody({ body: `${prefixed('note')} tail`, payload: attachment(1) })).toBe('note tail');
    expect(displayBody({ body: `${prefixed('note')} tail`, payload: attachment(2) })).toBe(`${prefixed('note')} tail`);
  });
});

describe('messagePreview', () => {
  it('reads an image-only spoiler as [spoiler] and its shown name', () => {
    expect(messagePreview({ body: prefixed('x.png'), payload: attachment(1) })).toBe('[spoiler] x.png');
    expect(messagePreview({ body: `${prefixed('a.png')}, b.png, ${prefixed('c.png')}`, payload: attachment(3) })).toBe('[spoiler] a.png, b.png, [spoiler] c.png');
  });
  it('hides a text spoiler in a caption that starts with the prefix, commas inside it included', () => {
    expect(messagePreview({ body: `${prefixed('note')} ||the secret||`, payload: attachment(1) })).toBe(`${prefixed('note')} [spoiler]`);
    expect(messagePreview({ body: `${prefixed('a')}, ${prefixed('b')} ||x, y||`, payload: attachment(2) })).not.toContain('x, y');
    expect(messagePreview({ body: `${prefixed('a')} ||x, y||`, payload: attachment(2) })).not.toContain('x, y');
  });
  it('keeps other names and a body with no attachment', () => {
    expect(messagePreview({ body: 'x.png', payload: attachment(1) })).toBe('x.png');
    expect(messagePreview({ body: prefixed('x.png') })).toBe(prefixed('x.png'));
    expect(messagePreview({ body: 'see ||this||' })).toBe('see [spoiler]');
  });
});

describe('names that hold ", "', () => {
  const comma = prefixed('trip, day 1.png');
  const other = prefixed('other.png');
  const body = `${comma}, ${other}`;
  beforeEach(() => { forgetFileNames(); });
  afterEach(() => { forgetFileNames(); });

  it('falls back to the split while the names are not known', () => {
    expect(attachmentNames({ body, payload: attachment(2) })).toBeNull();
    expect(messagePreview({ body, payload: attachment(2) })).toBe(body);
    rememberFileName(1, comma);
    expect(attachmentNames({ body, payload: attachment(2) })).toBeNull();
  });
  it('matches the body against the known stored names', () => {
    rememberFileName(1, comma);
    rememberFileName(2, other);
    expect(attachmentNames({ body, payload: attachment(2) })).toEqual([comma, other]);
    expect(displayBody({ body, payload: attachment(2) })).toBe('trip, day 1.png, other.png');
    expect(messagePreview({ body, payload: attachment(2) })).toBe('[spoiler] trip, day 1.png, [spoiler] other.png');
  });
  it('does not match a body that is not exactly the known names', () => {
    rememberFileName(1, comma);
    rememberFileName(2, other);
    expect(attachmentNames({ body: `${body} tail`, payload: attachment(2) })).toBeNull();
    expect(attachmentNames({ body: `${other}, ${comma}`, payload: attachment(2) })).toBeNull();
  });
  it('never turns a body with || into names, known names or not', () => {
    rememberFileName(1, prefixed('a, b'));
    expect(attachmentNames({ body: `${prefixed('a, b')} ||x||`, payload: attachment(1) })).toBeNull();
    expect(messagePreview({ body: `${prefixed('a, b')} ||x, y||`, payload: attachment(1) })).not.toContain('x, y');
    rememberFileName(1, prefixed('a||b'));
    expect(attachmentNames({ body: prefixed('a||b'), payload: attachment(1) })).toBeNull();
    expect(messagePreview({ body: prefixed('a||b'), payload: attachment(1) })).toBe(prefixed('a||b'));
  });
});
