// The talk key. It acts only while the tab has focus, and only when focus is
// outside the places text is typed, so the default key never types into the
// composer and a held key never fights a form.
export const defaultTalkKey = 'Backquote';

const typingTags = new Set(['INPUT', 'TEXTAREA', 'SELECT']);

export function isTyping(target: EventTarget | null): boolean {
  if (!(target instanceof HTMLElement)) return false;
  return typingTags.has(target.tagName) || target.isContentEditable;
}

const mouseTalkKeys: Readonly<Record<number, string>> = {
  3: 'Mouse4',
  4: 'Mouse5',
};

export function talkKeyForMouseButton(button: number): string | null {
  return mouseTalkKeys[button] ?? null;
}

export function mouseButtonForTalkKey(code: string): number | null {
  if (code === 'Mouse4') return 3;
  if (code === 'Mouse5') return 4;
  return null;
}

// keyLabel names a KeyboardEvent.code or mouse talk key for people.
export function keyLabel(code: string): string {
  if (code === 'Mouse4') return 'Mouse 4';
  if (code === 'Mouse5') return 'Mouse 5';
  if (code === 'Backquote') return '` (backtick)';
  if (code.startsWith('Key')) return code.slice(3);
  if (code.startsWith('Digit')) return code.slice(5);
  return code;
}
