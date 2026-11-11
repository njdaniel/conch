import { describe, expect, it } from 'vitest';
import { keyboardKeys, keyForCode, keyForMouseButton } from './evdev';

describe('evdev table', () => {
  it.each([
    ['KeyQ', 16], ['KeyA', 30], ['KeyZ', 44], ['KeyM', 50], ['KeyP', 25], ['KeyL', 38], ['KeyB', 48],
    ['Digit1', 2], ['Digit9', 10], ['Digit0', 11],
    ['F1', 59], ['F10', 68], ['F11', 87], ['F12', 88], ['F13', 183], ['F24', 194],
    ['ControlLeft', 29], ['ShiftLeft', 42], ['AltLeft', 56], ['ShiftRight', 54], ['ControlRight', 97], ['AltRight', 100], ['MetaLeft', 125],
    ['CapsLock', 58], ['Backquote', 41], ['Space', 57],
  ])('%s is evdev %i', (dom, code) => {
    expect(keyForCode(dom)?.code).toBe(code);
  });

  it('refuses codes with no row', () => {
    for (const dom of ['', 'Unidentified', 'MediaPlayPause', 'F25', 'KeyAA', 'Lang1']) expect(keyForCode(dom)).toBeNull();
  });

  it('has every letter exactly once and unique codes', () => {
    const codes = keyboardKeys().map(([, k]) => k.code);
    expect(new Set(codes).size).toBe(codes.length);
    expect(keyboardKeys().filter(([d]) => /^Key[A-Z]$/.test(d))).toHaveLength(26);
  });

  it('maps the fourth and fifth mouse buttons only', () => {
    expect(keyForMouseButton(3)).toEqual({ code: 275, label: 'Mouse 4', kind: 'mouse' });
    expect(keyForMouseButton(4)).toEqual({ code: 276, label: 'Mouse 5', kind: 'mouse' });
    for (const b of [0, 1, 2, 5, -1]) expect(keyForMouseButton(b)).toBeNull();
  });
});
