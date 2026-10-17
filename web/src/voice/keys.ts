// The talk key. It acts only while the tab has focus, and only when focus is
// outside the places text is typed, so the default key never types into the
// composer and a held key never fights a form.
export const defaultTalkKey = 'Backquote';

const typingTags = new Set(['INPUT', 'TEXTAREA', 'SELECT']);

export function isTyping(target: EventTarget | null): boolean {
  if (!(target instanceof HTMLElement)) return false;
  return typingTags.has(target.tagName) || target.isContentEditable;
}

// keyLabel names a KeyboardEvent.code for people.
export function keyLabel(code: string): string {
  if (code === 'Backquote') return '` (backtick)';
  if (code.startsWith('Key')) return code.slice(3);
  if (code.startsWith('Digit')) return code.slice(5);
  return code;
}
