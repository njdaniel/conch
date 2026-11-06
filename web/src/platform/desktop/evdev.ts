// The DOM-to-evdev table of the talk-key setup (issue #1382, D3-11 of #771):
// the evdev code the voice helper binds for a KeyboardEvent.code or a
// MouseEvent.button the person pressed. Pure data; nothing here reads an event.
//
// PROVENANCE: the codes below were written by hand, then every row (119:
// all keyboard rows and both mouse buttons) was checked in review against
// linux/input-event-codes.h (KEY_*, BTN_SIDE, BTN_EXTRA) on 2026-11-06, with
// no mismatch. A DOM code with no row is "can't be used as a talk key";
// nothing is guessed.

export interface EvdevKey {
  // The evdev code the helper's keys.talk takes.
  readonly code: number;
  // What the person reads: "hold <label> to talk".
  readonly label: string;
  readonly kind: 'keyboard' | 'mouse';
}

const keys = new Map<string, EvdevKey>();
const add = (dom: string, code: number, label: string): void => {
  keys.set(dom, { code, label, kind: 'keyboard' });
};

for (let i = 0; i < 26; i++) {
  const letter = String.fromCharCode(65 + i);
  // KEY_Q..: the rows of the keyboard, not alphabetical.
  const evdev = 'QWERTYUIOP'.indexOf(letter) >= 0 ? 16 + 'QWERTYUIOP'.indexOf(letter) : 'ASDFGHJKL'.indexOf(letter) >= 0 ? 30 + 'ASDFGHJKL'.indexOf(letter) : 44 + 'ZXCVBNM'.indexOf(letter);
  add(`Key${letter}`, evdev, letter);
}
for (let i = 1; i <= 9; i++) add(`Digit${String(i)}`, 1 + i, String(i));
add('Digit0', 11, '0');
for (let i = 1; i <= 10; i++) add(`F${String(i)}`, 58 + i, `F${String(i)}`);
add('F11', 87, 'F11');
add('F12', 88, 'F12');
for (let i = 13; i <= 24; i++) add(`F${String(i)}`, 170 + i, `F${String(i)}`); // KEY_F13 = 183

add('Escape', 1, 'Escape');
add('Minus', 12, '-');
add('Equal', 13, '=');
add('Backspace', 14, 'Backspace');
add('Tab', 15, 'Tab');
add('BracketLeft', 26, '[');
add('BracketRight', 27, ']');
add('Enter', 28, 'Enter');
add('ControlLeft', 29, 'Left Ctrl');
add('Semicolon', 39, ';');
add('Quote', 40, "'");
add('Backquote', 41, '`');
add('ShiftLeft', 42, 'Left Shift');
add('Backslash', 43, '\\');
add('Comma', 51, ',');
add('Period', 52, '.');
add('Slash', 53, '/');
add('ShiftRight', 54, 'Right Shift');
add('AltLeft', 56, 'Left Alt');
add('Space', 57, 'Space');
add('CapsLock', 58, 'Caps Lock');
add('NumLock', 69, 'Num Lock');
add('ScrollLock', 70, 'Scroll Lock');
add('IntlBackslash', 86, '<');
add('ControlRight', 97, 'Right Ctrl');
add('PrintScreen', 99, 'Print Screen');
add('AltRight', 100, 'Right Alt');
add('Home', 102, 'Home');
add('ArrowUp', 103, 'Up');
add('PageUp', 104, 'Page Up');
add('ArrowLeft', 105, 'Left');
add('ArrowRight', 106, 'Right');
add('End', 107, 'End');
add('ArrowDown', 108, 'Down');
add('PageDown', 109, 'Page Down');
add('Insert', 110, 'Insert');
add('Delete', 111, 'Delete');
add('Pause', 119, 'Pause');
add('MetaLeft', 125, 'Left Super');
add('MetaRight', 126, 'Right Super');
add('ContextMenu', 127, 'Menu');
// The keypad.
add('NumpadMultiply', 55, 'Keypad *');
add('Numpad7', 71, 'Keypad 7');
add('Numpad8', 72, 'Keypad 8');
add('Numpad9', 73, 'Keypad 9');
add('NumpadSubtract', 74, 'Keypad -');
add('Numpad4', 75, 'Keypad 4');
add('Numpad5', 76, 'Keypad 5');
add('Numpad6', 77, 'Keypad 6');
add('NumpadAdd', 78, 'Keypad +');
add('Numpad1', 79, 'Keypad 1');
add('Numpad2', 80, 'Keypad 2');
add('Numpad3', 81, 'Keypad 3');
add('Numpad0', 82, 'Keypad 0');
add('NumpadDecimal', 83, 'Keypad .');
add('NumpadEnter', 96, 'Keypad Enter');
add('NumpadDivide', 98, 'Keypad /');

// keyForCode is the row for a KeyboardEvent.code, or null: not usable.
export function keyForCode(code: string): EvdevKey | null {
  return keys.get(code) ?? null;
}

// Every keyboard row, by DOM code, for the picker.
export function keyboardKeys(): readonly (readonly [string, EvdevKey])[] {
  return [...keys.entries()];
}

// BTN_SIDE and BTN_EXTRA. Only the fourth and fifth buttons (MouseEvent.button
// 3 and 4) are offered: the first three are the click itself.
const mouse: Readonly<Record<number, EvdevKey>> = {
  3: { code: 275, label: 'Mouse 4', kind: 'mouse' },
  4: { code: 276, label: 'Mouse 5', kind: 'mouse' },
};

// keyForMouseButton is the row for a MouseEvent.button, or null.
export function keyForMouseButton(button: number): EvdevKey | null {
  return mouse[button] ?? null;
}

export function mouseKeys(): readonly EvdevKey[] {
  return [mouse[3], mouse[4]].filter((k): k is EvdevKey => k !== undefined);
}
