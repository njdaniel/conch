// passwordProblem is what the page says of a chosen password before sending
// it. There is no minimum length beyond non-empty (issue #977).
import { describe, expect, it } from 'vitest';
import { maxPassword, passwordProblem } from './CreateAccount';

describe('passwordProblem', () => {
  it.each([
    { name: 'four letters', chosen: 'tide', repeated: 'tide', want: null },
    { name: 'one character', chosen: 'x', repeated: 'x', want: null },
    { name: 'a long passphrase', chosen: 'a few words with spaces between them', repeated: 'a few words with spaces between them', want: null },
    { name: 'exactly the maximum', chosen: 'a'.repeat(maxPassword), repeated: 'a'.repeat(maxPassword), want: null },
    { name: 'empty', chosen: '', repeated: '', want: 'Choose a password.' },
    { name: 'over the maximum', chosen: 'a'.repeat(maxPassword + 1), repeated: 'a'.repeat(maxPassword + 1), want: `Choose a password of at most ${String(maxPassword)} characters.` },
    { name: 'mismatched', chosen: 'tide', repeated: 'tidal', want: 'The two passwords are not the same. Type them again.' },
    // 128 composed letters written decomposed are 256 code points typed, 128 after NFC.
    { name: 'decomposed text that composes to the maximum', chosen: 'e\u0301'.repeat(maxPassword), repeated: 'e\u0301'.repeat(maxPassword), want: null },
    { name: 'composed text one over the maximum', chosen: '\u00e9'.repeat(maxPassword + 1), repeated: '\u00e9'.repeat(maxPassword + 1), want: `Choose a password of at most ${String(maxPassword)} characters.` },
  ])('$name', ({ chosen, repeated, want }) => {
    expect(passwordProblem(chosen, repeated)).toBe(want);
  });
});
