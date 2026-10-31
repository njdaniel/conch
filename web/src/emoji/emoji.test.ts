import { describe, expect, it } from 'vitest';
import { EMOJI_VERSION, MAX_REACTION_KEY_BYTES, MAX_REACTION_KEY_CODE_POINTS } from '../schema/reaction';
import { buildIndex, completeShortcode, completionAt, insertAt, loadEmoji, searchEmoji, shortcodeOf } from './emoji';

const index = await loadEmoji();

describe('the catalog', () => {
  it('is the version the schema pins, and every key is a bounded sequence', () => {
    expect(index.version).toBe(EMOJI_VERSION);
    expect(index.all.length).toBeGreaterThan(3000);
    for (const e of index.all) {
      expect(new TextEncoder().encode(e.key).length).toBeLessThanOrEqual(MAX_REACTION_KEY_BYTES);
      expect(Array.from(e.key).length).toBeLessThanOrEqual(MAX_REACTION_KEY_CODE_POINTS);
    }
  });

  it('gives every emoji a different shortcode of a-z, 0-9 and _', () => {
    const seen = new Map<string, string>();
    for (const e of index.all) {
      expect(e.shortcode, e.name).toMatch(/^[a-z0-9]+(_[a-z0-9]+)*$/);
      expect(seen.get(e.shortcode), `${e.name} and ${seen.get(e.shortcode) ?? ''}`).toBeUndefined();
      seen.set(e.shortcode, e.name);
    }
  });

  it('does not list skin-tone variants when browsing, and still finds them', () => {
    const people = index.groups.find((g) => g.name === 'People & Body');
    expect(people?.browse.length).toBeLessThan(people?.emoji.length ?? 0);
    expect(people?.browse.some((e) => e.toned)).toBe(false);
    expect(searchEmoji(index, 'thumbs up medium skin tone').map((e) => e.key)).toContain('👍🏽');
  });
});

describe('shortcodeOf', () => {
  it.each([
    ['thumbs up', 'thumbs_up'],
    ['thumbs up: medium skin tone', 'thumbs_up_medium_skin_tone'],
    ['twelve o’clock', 'twelve_oclock'],
    ['piñata', 'pinata'],
    ['keycap: #', 'keycap_hash'],
    ['keycap: *', 'keycap_asterisk'],
    ['flag: Côte d’Ivoire', 'flag_cote_divoire'],
  ])('%s is :%s:', (name, code) => {
    expect(shortcodeOf(name)).toBe(code);
  });
});

describe('searchEmoji', () => {
  const keys = (q: string) => searchEmoji(index, q).map((e) => e.key);

  it('finds by name, by shortcode and by part of either', () => {
    expect(keys('grinning face')[0]).toBe('😀');
    expect(keys('thumbs_up')).toContain('👍');
    expect(keys('THUMBS')).toContain('👍');
    expect(keys('pinata')).toContain('🪅');
  });

  it('wants every word, in any order', () => {
    expect(keys('face grinning')).toContain('😀');
    expect(keys('grinning zzzz')).toEqual([]);
  });

  it('puts a name that starts with the query before one that only contains it', () => {
    const found = searchEmoji(index, 'cat').map((e) => e.name);
    expect(found[0]?.startsWith('cat')).toBe(true);
    expect(found.findIndex((n) => !n.startsWith('cat'))).toBeGreaterThan(0);
  });

  it('answers nothing for an empty query and no more than the limit', () => {
    expect(searchEmoji(index, '   ')).toEqual([]);
    expect(searchEmoji(index, 'e', 10)).toHaveLength(10);
  });
});

describe('completionAt', () => {
  it.each([
    ['hi :sm', 6, { start: 3, query: 'sm' }],
    [':thumbs_up', 10, { start: 0, query: 'thumbs_up' }],
    ['a\n:Fire', 7, { start: 2, query: 'fire' }],
    ['(:fire', 6, { start: 1, query: 'fire' }],
    ['go :fire and more', 8, { start: 3, query: 'fire' }],
  ])('%j at %i', (text, caret, want) => {
    expect(completionAt(text, caret)).toEqual(want);
  });

  it.each([
    ['12:30', 5],
    ['https://example.com', 19],
    ['hi :s', 5],
    ['hi :', 4],
    ['hi : sm', 7],
    ['hi :smile: ', 11],
    ['no colon', 8],
  ])('%j at %i is not a completion', (text, caret) => {
    expect(completionAt(text, caret)).toBeNull();
  });
});

describe('completeShortcode', () => {
  it('lists the shortcodes that start with the query before those that contain it', () => {
    const found = completeShortcode(index, 'face_with');
    expect(found).toHaveLength(8);
    expect(found.every((e) => e.shortcode.startsWith('face_with'))).toBe(true);
    expect(completeShortcode(index, 'thumbs_up', 100)[0]?.key).toBe('👍');
    // An exact shortcode beats the longer ones that start with it.
    expect(completeShortcode(index, 'fire')[0]?.key).toBe('🔥');
  });

  it.each([
    ['family', '👪'],
    ['woman', '👩'],
    ['man', '👨'],
    ['kiss', '💏'],
    ['dog', '🐕'],
  ])('puts the exact :%s: first even when longer ones come earlier in the catalog', (query, key) => {
    expect(completeShortcode(index, query)[0]?.key).toBe(key);
  });

  it('lists prefix matches when there is no exact one', () => {
    const found = completeShortcode(index, 'heart');
    expect(found).toHaveLength(8);
    expect(found.every((e) => [e.shortcode, ...e.aliases].some((n) => n.startsWith('heart')))).toBe(true);
  });

  it('finds nothing for nonsense', () => {
    expect(completeShortcode(index, 'zzzzqq')).toEqual([]);
  });
});

describe('aliases and ranking (issue #1036)', () => {
  it.each([
    ['fire', '🔥'],
    ['smile', '😄'],
    ['heart', '❤️'],
    ['joy', '😂'],
    ['tada', '🎉'],
    ['+1', '👍'],
    ['thumb', '👍'],
  ])('the first suggestion for :%s is %s', (query, key) => {
    expect(completeShortcode(index, query)[0]?.key).toBe(key);
  });

  it('offers no skin-tone variant unless the query names a tone', () => {
    for (const query of ['fir', 'thumb', 'ok', 'thumbs_up', 'wave']) {
      const found = completeShortcode(index, query, 50);
      expect(found.length, query).toBeGreaterThan(0);
      expect(found.some((e) => e.toned), query).toBe(false);
    }
    expect(completeShortcode(index, 'thumbs_up_medium_skin').every((e) => e.toned)).toBe(true);
    expect(completeShortcode(index, 'thumbs_up_medium_skin').length).toBeGreaterThan(0);
    expect(searchEmoji(index, 'thumb').some((e) => e.toned)).toBe(false);
  });

  it('keeps the key canonical: an alias finds the emoji, whose key is its sequence', () => {
    const tada = completeShortcode(index, 'tada')[0];
    expect(tada?.key).toBe('🎉');
    expect(tada?.aliases).toContain('tada');
    expect(completeShortcode(index, 'heart')[0]?.key).toBe('\u2764\uFE0F');
  });

  const fake = (key: string, shortcode: string, aliases: string[] = []) => ({ key, name: shortcode.replace(/_/g, ' '), shortcode, aliases, group: 'g', subgroup: 's', toned: false });

  it('orders the tiers: exact, then starts with, then a later word, then contains', () => {
    const small = { version: index.version, groups: [], all: [fake('1', 'xxfoo'), fake('2', 'a_foo_b'), fake('3', 'foo_bar'), fake('4', 'foo')] };
    expect(completeShortcode(small, 'foo').map((e) => e.key)).toEqual(['4', '3', '2', '1']);
  });

  it('lets a multiword query match at a word boundary, above a mid-word hit', () => {
    const small = { version: index.version, groups: [], all: [fake('1', 'xxfoo_bar'), fake('2', 'a_foo_bar'), fake('3', 'foo_bar_z')] };
    expect(completeShortcode(small, 'foo_bar').map((e) => e.key)).toEqual(['3', '2', '1']);
    expect(searchEmoji(small, 'foo bar').map((e) => e.key)).toEqual(['3', '2', '1']);
  });

  it('puts a name with the words "with smiling" at a boundary first in the picker', () => {
    const first = searchEmoji(index, 'with smiling').slice(0, 5);
    expect(first.every((e) => e.shortcode.includes('_with_smiling') || e.shortcode.startsWith('with_smiling'))).toBe(true);
  });

  it('ranks an alias before a CLDR name of the same tier, then the shorter name', () => {
    const small = { version: index.version, groups: [], all: [fake('a', 'bar_long'), fake('b', 'bar_x'), fake('c', 'qq', ['barz']), fake('d', 'qq2', ['bar_alias_long'])] };
    expect(completeShortcode(small, 'bar').map((e) => e.key)).toEqual(['c', 'd', 'b', 'a']);
  });

  it('finds an alias in the picker search too, first', () => {
    expect(searchEmoji(index, 'tada')[0]?.key).toBe('🎉');
    expect(searchEmoji(index, '+1')[0]?.key).toBe('👍');
  });

  it('opens a completion for :+1', () => {
    expect(completionAt('go :+1', 6)).toEqual({ start: 3, query: '+1' });
  });
});

describe('insertAt', () => {
  it.each([
    ['', 0, 0, '👍', '👍', 2],
    ['hello', 5, 5, '👍', 'hello👍', 7],
    ['hello', 2, 2, '👍', 'he👍llo', 4],
    ['hello', 1, 4, '👍', 'h👍o', 3],
    ['hi :thumbs', 3, 10, '👍', 'hi 👍', 5],
    ['abc', 9, 9, '👍', 'abc👍', 5],
    ['abc', 2, 1, '👍', 'a👍c', 3],
  ])('%j [%i,%i] gets %s', (text, from, to, insert, want, caret) => {
    expect(insertAt(text, from, to, insert)).toEqual({ text: want, caret });
  });
});

describe('buildIndex', () => {
  it('keeps the catalog order and tags groups', () => {
    const small = buildIndex({ version: '1.0', groups: [{ name: 'G', subgroups: [{ name: 's', emoji: [{ key: '😀', name: 'grinning face' }, { key: '👍🏽', name: 'thumbs up: medium skin tone' }] }] }] });
    expect(small.all.map((e) => [e.key, e.group, e.toned])).toEqual([['😀', 'G', false], ['👍🏽', 'G', true]]);
    expect(small.groups[0]?.browse).toHaveLength(1);
  });
});
