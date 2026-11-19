import { describe, expect, it } from 'vitest';
import { isWindowsBinding, windowsKey, windowsKeys } from './windows-keys';

describe('Windows physical binding translation', () => {
  it.each([
    ['KeyA', 'sc:0x1E'], ['Backquote', 'sc:0x29'], ['ControlLeft', 'sc:0x1D'],
    ['ControlRight', 'sc:0xE0,0x1D'], ['AltRight', 'sc:0xE0,0x38'],
    ['ShiftRight', 'sc:0x36'], ['NumLock', 'sc:0xE0,0x45'],
    ['Pause', 'sc:0x45'], ['PrintScreen', 'sc:0xE0,0x37'],
    ['NumpadEnter', 'sc:0xE0,0x1C'], ['Mouse4', 'mouse:4'], ['Mouse5', 'mouse:5'],
  ])('%s matches the helper grammar', (code, value) => {
    expect(windowsKey(code)?.value).toBe(value);
  });
  it('refuses unknown codes and the three unobservable scan codes', () => {
    for (const code of ['typed text', 'Unidentified', 'HangulMode', 'Mouse1']) expect(windowsKey(code)).toBeNull();
    for (const value of ['sc:0x54', 'sc:0xE0,0x46', 'sc:0xe0,0x2a', 'sc:0x00', 'sc:0x80', 'mouse:3', '/dev/input/event0', 'sc:0xE1,0x45']) expect(isWindowsBinding(value)).toBe(false);
    expect(windowsKeys.every((key) => isWindowsBinding(key.value))).toBe(true);
    expect(new Set(windowsKeys.map((key) => key.value)).size).toBe(windowsKeys.length);
  });
});
