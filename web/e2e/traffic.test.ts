// The scrubber behind A14's search (issue #1069) on traffic made by hand.
// A unit test: no browser, no network.
import { describe, expect, it } from 'vitest';
import { findLeak, scrubSecrets } from './traffic';

describe('findLeak', () => {
  it.each([
    ['a random ticket', '{"ticket":"conch_wst_xxboby"}'],
    ['a ticket without the prefix', '{"ticket": "z1boblB0g"}'],
    ['a token', '{"token":"abcbobdef","x":1}'],
    ['a livekit token', '{"livekit_token":"eyJ.bob.sig"}'],
    ['a secret', '{"secret":"bob\\"bob"}'],
    ['a bare token form', 'Bearer conch_pat_bobbob'],
  ])('passes %s', (_what, text) => {
    expect(findLeak(text, 'bob')).toBeUndefined();
  });

  it.each([
    ['a name field', '{"name":"bob"}'],
    ['a sentence', '{"body":"hello, bob joined the channel"}'],
    ['a name beside a ticket', '{"ticket":"conch_wst_xx","name":"bob"}'],
  ])('fails %s', (_what, text) => {
    expect(findLeak(text, 'bob')).toBeDefined();
  });

  it('shows about 40 characters around the match', () => {
    const text = `${'a'.repeat(100)}bob${'b'.repeat(100)}`;
    expect(findLeak(text, 'bob')).toBe(`"bob" at ...${'a'.repeat(40)}bob${'b'.repeat(40)}...`);
  });
});

describe('scrubSecrets', () => {
  it('keeps the rest of the document', () => {
    expect(scrubSecrets('{"ticket":"conch_wst_q","id":7}')).toBe('{"ticket":"","id":7}');
  });
});
