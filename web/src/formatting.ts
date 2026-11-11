export type FormatKind = 'bold' | 'italic' | 'strike' | 'code' | 'codeBlock' | 'quote' | 'spoiler';

export interface FormattedText {
  readonly text: string;
  readonly start: number;
  readonly end: number;
}

const markers: Readonly<Record<'bold' | 'italic' | 'strike' | 'code' | 'spoiler', string>> = {
  bold: '**',
  italic: '*',
  strike: '~~',
  code: '`',
  spoiler: '||',
};

export function formatText(text: string, start: number, end: number, kind: FormatKind): FormattedText {
  const selected = text.slice(start, end);
  if (kind === 'codeBlock') {
    // A fence must start its own line and end its own line: break the line
    // before it, and after it, where the selection sits mid-line.
    const before = start > 0 && text[start - 1] !== '\n' ? '\n' : '';
    const after = end < text.length && text[end] !== '\n' ? '\n' : '';
    const made = `${before}\`\`\`\n${selected}\n\`\`\`${after}`;
    const inner = start + before.length + 4;
    return { text: text.slice(0, start) + made + text.slice(end), start: inner, end: inner + selected.length };
  }
  if (kind === 'quote') {
    const first = text.lastIndexOf('\n', Math.max(0, start - 1)) + 1;
    const next = text.indexOf('\n', end);
    const last = next < 0 ? text.length : next;
    const quoted = text.slice(first, last).split('\n').map((line) => `> ${line}`).join('\n');
    return { text: text.slice(0, first) + quoted + text.slice(last), start: first + 2, end: first + quoted.length };
  }
  const marker = markers[kind];
  if (kind === 'spoiler' && selected.includes('\n')) {
    // Each line gets its own pair, so no spoiler has to span a quote or a code
    // block; empty lines stay empty.
    const made = selected.split('\n').map((line) => (line.trim() === '' ? line : `${marker}${line}${marker}`)).join('\n');
    return { text: text.slice(0, start) + made + text.slice(end), start, end: start + made.length };
  }
  const made = marker + selected + marker;
  return {
    text: text.slice(0, start) + made + text.slice(end),
    start: start + marker.length,
    end: start + marker.length + selected.length,
  };
}
