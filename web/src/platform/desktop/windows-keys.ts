// Windows Set 1 physical bindings, matching winit's to_scancode convention
// (windows-desktop.md §4.5). No event, key text or device data is retained.
// Set 1's ordinary rows coincide with evdev 1..83 except NumLock. Extended
// rows are explicit; unsupported DOM codes are refused, never guessed.
import { keyboardKeys } from './evdev';

export interface WindowsKey {
  readonly code: string;
  readonly label: string;
  readonly value: string;
}
const hex = (n: number) => `0x${n.toString(16).toUpperCase().padStart(2, '0')}`;
const extended: Readonly<Record<string, number>> = {
  NumpadEnter: 0x1c, ControlRight: 0x1d, NumpadDivide: 0x35,
  PrintScreen: 0x37, AltRight: 0x38, NumLock: 0x45,
  Home: 0x47, ArrowUp: 0x48, PageUp: 0x49, ArrowLeft: 0x4b,
  ArrowRight: 0x4d, End: 0x4f, ArrowDown: 0x50, PageDown: 0x51,
  Insert: 0x52, Delete: 0x53, MetaLeft: 0x5b, MetaRight: 0x5c, ContextMenu: 0x5d,
};
const ordinary: Readonly<Record<string, number>> = { IntlBackslash: 0x56, F11: 0x57, F12: 0x58, Pause: 0x45 };
const rows: WindowsKey[] = [];
for (const [code, key] of keyboardKeys()) {
  const ext = extended[code];
  const sc = ordinary[code] ?? (key.code <= 83 ? key.code : undefined);
  if (ext !== undefined || sc !== undefined) {
    rows.push({ code, label: key.label, value: ext !== undefined ? `sc:0xE0,${hex(ext)}` : `sc:${hex(sc ?? 0)}` });
  }
}
rows.push({ code: 'Mouse4', label: 'Mouse 4', value: 'mouse:4' }, { code: 'Mouse5', label: 'Mouse 5', value: 'mouse:5' });
export const windowsKeys: readonly WindowsKey[] = rows;
export function windowsKey(code: string): WindowsKey | null {
  return windowsKeys.find((row) => row.code === code) ?? null;
}
// Match the helper's closed grammar including its three unobservable codes.
export function isWindowsBinding(value: string): boolean {
  if (value === 'mouse:4' || value === 'mouse:5') return true;
  if (!/^sc:(?:0xE0,)?0x[0-9A-F]{2}$/iu.test(value)) return false;
  const n = Number.parseInt(value.slice(-2), 16);
  return n >= 1 && n <= 127 && !['sc:0x54', 'sc:0xe0,0x46', 'sc:0xe0,0x2a'].includes(value.toLowerCase());
}
