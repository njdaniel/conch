import { useId, useState, type ReactNode } from 'react';
import { linkify } from './linkify';

function SafeLink({ text, href }: { readonly text: string; readonly href: string }) {
  return <a href={href} target="_blank" rel="noopener noreferrer">{text}</a>;
}

function Spoiler({ children }: { readonly children: ReactNode }) {
  const [revealed, setRevealed] = useState(false);
  const id = useId();
  return (
    <span className={revealed ? 'spoiler revealed' : 'spoiler'}>
      <button type="button" aria-expanded={revealed} aria-controls={id} onClick={() => { setRevealed((shown) => !shown); }}>
        {revealed ? 'Hide spoiler' : 'Reveal spoiler'}
      </button>
      {revealed ? <span id={id} className="spoiler-content">{children}</span> : null}
    </span>
  );
}

function plain(text: string, prefix: string): ReactNode[] {
  return linkify(text).map((segment, index) => segment.kind === 'text'
    ? segment.text
    : <SafeLink key={`${prefix}-link-${String(index)}`} text={segment.text} href={segment.href} />);
}

type InlineMarker = '`' | '||' | '***' | '**' | '~~' | '*';
interface NextMarkers {
  readonly text: string;
  readonly positions: Record<InlineMarker, Int32Array | null>;
}
const maxInlineDepth = 8;

function nextMarkers(text: string): NextMarkers {
  return {
    text,
    positions: { '`': null, '||': null, '***': null, '**': null, '~~': null, '*': null },
  };
}

function closing(next: NextMarkers, marker: InlineMarker, from: number): number {
  let positions = next.positions[marker];
  if (positions === null) {
    positions = new Int32Array(next.text.length + 1);
    positions.fill(-1);
    for (let at = next.text.length - 1; at >= 0; at--) {
      if (next.text[at] === '\n') continue;
      positions[at] = positions[at + 1] ?? -1;
      if (next.text.startsWith(marker, at)) positions[at] = at;
    }
    next.positions[marker] = positions;
  }
  const end = positions[from] ?? -1;
  return end <= from ? -1 : end;
}

// Inline Markdown becomes React elements, never HTML. Delimiters that do not
// close stay literal, and inline code never parses its contents.
function inline(text: string, prefix = 'inline', depth = 0): ReactNode[] {
  const next = nextMarkers(text);
  const out: ReactNode[] = [];
  let literal = 0;
  let at = 0;
  const flush = (end: number) => {
    if (end > literal) out.push(...plain(text.slice(literal, end), `${prefix}-${String(literal)}`));
  };
  const wrap = (marker: InlineMarker, element: 'strong' | 'em' | 's'): boolean => {
    if (!text.startsWith(marker, at)) return false;
    const from = at + marker.length;
    const end = closing(next, marker, from);
    if (end < 0 || depth >= maxInlineDepth) return false;
    flush(at);
    const content = inline(text.slice(from, end), `${prefix}-${String(at)}`, depth + 1);
    out.push(element === 'strong' ? <strong key={`${prefix}-${String(at)}`}>{content}</strong> : element === 'em' ? <em key={`${prefix}-${String(at)}`}>{content}</em> : <s key={`${prefix}-${String(at)}`}>{content}</s>);
    at = end + marker.length;
    literal = at;
    return true;
  };

  while (at < text.length) {
    if (text[at] === '\\' && at + 1 < text.length && /[*`~|[\]\\]/.test(text[at + 1] ?? '')) {
      flush(at);
      out.push(text[at + 1]);
      at += 2;
      literal = at;
      continue;
    }
    if (text[at] === '`') {
      const end = closing(next, '`', at + 1);
      if (end >= 0) {
        flush(at);
        out.push(<code key={`${prefix}-${String(at)}`}>{text.slice(at + 1, end)}</code>);
        at = end + 1;
        literal = at;
        continue;
      }
    }
    if (text.startsWith('||', at)) {
      const end = closing(next, '||', at + 2);
      if (end >= 0 && depth < maxInlineDepth) {
        flush(at);
        out.push(<Spoiler key={`${prefix}-${String(at)}`}>{inline(text.slice(at + 2, end), `${prefix}-${String(at)}`, depth + 1)}</Spoiler>);
        at = end + 2;
        literal = at;
        continue;
      }
    }
    if (text.startsWith('***', at)) {
      const end = closing(next, '***', at + 3);
      if (end >= 0 && depth < maxInlineDepth) {
        flush(at);
        out.push(<strong key={`${prefix}-${String(at)}`}><em>{inline(text.slice(at + 3, end), `${prefix}-${String(at)}`, depth + 1)}</em></strong>);
        at = end + 3;
        literal = at;
        continue;
      }
    }
    if (wrap('**', 'strong') || wrap('~~', 's') || wrap('*', 'em')) continue;
    at += 1;
  }
  flush(text.length);
  return out;
}

function blocks(text: string): ReactNode[] {
  const lines = text.split('\n');
  const out: ReactNode[] = [];
  let ordinary: string[] = [];
  const flush = () => {
    if (ordinary.length === 0) return;
    const body = ordinary.join('\n');
    out.push(<span key={`text-${String(out.length)}`}>{inline(body, `text-${String(out.length)}`)}</span>);
    ordinary = [];
  };

  for (let at = 0; at < lines.length;) {
    const line = lines[at] ?? '';
    if (line.startsWith('```')) {
      flush();
      at += 1;
      const code: string[] = [];
      while (at < lines.length && !(lines[at] ?? '').startsWith('```')) {
        code.push(lines[at] ?? '');
        at += 1;
      }
      if (at < lines.length) at += 1;
      out.push(<pre key={`code-${String(out.length)}`}><code>{code.join('\n')}</code></pre>);
      continue;
    }
    if (line === '>' || line.startsWith('> ')) {
      flush();
      const quote: string[] = [];
      while (at < lines.length) {
        const quoted = lines[at] ?? '';
        if (quoted !== '>' && !quoted.startsWith('> ')) break;
        quote.push(quoted.slice(1).trimStart());
        at += 1;
      }
      out.push(<blockquote key={`quote-${String(out.length)}`}>{inline(quote.join('\n'), `quote-${String(out.length)}`)}</blockquote>);
      continue;
    }
    ordinary.push(line);
    at += 1;
  }
  flush();
  return out;
}

// Linkified is the shared message-body renderer. The historic name is kept
// because message history, pending sends, search and edit revisions all use it.
export function Linkified({ text }: { readonly text: string }) {
  return <>{blocks(text)}</>;
}
