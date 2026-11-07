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
    const made = `\`\`\`\n${selected}\n\`\`\``;
    return { text: text.slice(0, start) + made + text.slice(end), start: start + 4, end: start + 4 + selected.length };
  }
  if (kind === 'quote') {
    const first = text.lastIndexOf('\n', Math.max(0, start - 1)) + 1;
    const next = text.indexOf('\n', end);
    const last = next < 0 ? text.length : next;
    const quoted = text.slice(first, last).split('\n').map((line) => `> ${line}`).join('\n');
    return { text: text.slice(0, first) + quoted + text.slice(last), start: first + 2, end: first + quoted.length };
  }
  const marker = markers[kind];
  const made = marker + selected + marker;
  return {
    text: text.slice(0, start) + made + text.slice(end),
    start: start + marker.length,
    end: start + marker.length + selected.length,
  };
}
