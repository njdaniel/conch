import { describe, expect, it } from 'vitest';
import { completeMention, mentionAt } from './mention';

describe('mentionAt', () => {
  it.each([
    ['@me', 3, { start: 0, query: 'me' }],
    ['hi @me', 6, { start: 3, query: 'me' }],
    ['hi (@me', 7, { start: 4, query: 'me' }],
    ['hi @', 4, { start: 3, query: '' }],
    ['hi @me there', 6, { start: 3, query: 'me' }],
    ['mail a@b.c', 10, null],
    ['hi @me there', 12, null],
    ['no mention', 10, null],
    ['(@ada', 5, { start: 1, query: 'ada' }],
    ['see [@ada', 9, { start: 5, query: 'ada' }],
    ['see {@ada', 9, { start: 5, query: 'ada' }],
    ['http://example.com/?q=(@ada', 28, null],
    ['https://example.com/?q=(@ada', 29, null],
    ['https://example.com/@ada', 24, null],
    ['https://example.com/?q=@ada', 28, null],
    ['see https://example.com/?q=(@ada', 33, null],
    ['(https://example.com/@ada', 25, null],
    ['https://example.com/ (@ada', 26, { start: 22, query: 'ada' }],
  ])('%s at %i', (text, caret, want) => {
    expect(mentionAt(text, caret)).toEqual(want);
  });
});

describe('completeMention', () => {
  const names = ['member-1', 'member-2', 'owner', 'homer', 'Mel'];

  it('offers both members for a shared prefix', () => {
    expect(completeMention(names, 'me')).toEqual(['Mel', 'member-1', 'member-2', 'homer']);
  });
  it('puts a prefix match before a substring match', () => {
    expect(completeMention(['homer', 'member-2'], 'me')).toEqual(['member-2', 'homer']);
  });
  it('ranks member-2 first for its full name', () => {
    expect(completeMention(names, 'member-2')).toEqual(['member-2']);
  });
  it('never offers a name that is not in the list', () => {
    expect(completeMention(names, 'stranger')).toEqual([]);
  });
  it('is case-insensitive, deduplicates and caps at 8', () => {
    expect(completeMention(names, 'MEMBER-2')).toEqual(['member-2']);
    const many = Array.from({ length: 12 }, (_, i) => `user-${String(i).padStart(2, '0')}`);
    expect(completeMention([...many, 'user-00'], 'u')).toHaveLength(8);
  });
});
