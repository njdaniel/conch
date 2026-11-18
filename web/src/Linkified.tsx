import { Component, useState, type ReactNode } from 'react';
import { Spoiler } from './Spoiler';
import { Glyph } from './Icons';
import { linkify } from './linkify';
import { platform } from './platform';

function SafeLink({ text, href }: { readonly text: string; readonly href: string }) {
  const canonical = new URL(href).href;
  // The cue sits after the last character as the reader sees it: a whole code
  // point (an astral one is two UTF-16 units) with any combining marks.
  const tail = /\P{M}\p{M}*$/u.exec(text)?.[0] ?? text.slice(-1);
  return (
    <a className="external-link" href={canonical} target="_blank" rel="noopener noreferrer" title={canonical} aria-label={`${text} (opens in a new tab)`}>
      {text.slice(0, text.length - tail.length)}
      <span className="external-link-tail">
        {tail}
        <span className="external-link-cue" aria-hidden="true"><Glyph name="external" /></span>
      </span>
    </a>
  );
}

// text_ is the string a preview-mode result holds (it holds only strings).
function text_(nodes: ReactNode): string {
  if (typeof nodes === 'string') return nodes;
  return Array.isArray(nodes) ? nodes.map((node: ReactNode) => (typeof node === 'string' ? node : '')).join('') : '';
}

function plain(text: string, prefix: string): ReactNode[] {
  if (!text.includes('://')) return [text];
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
// Past this many spoilers in one body the rest are not components: each shows
// as the hidden text "[spoiler]", so a body of 200 000 of them stays cheap.
const maxSpoilers = 500;
interface Budget {
  spoilers: number;
  // A preview is the body's own text with each spoiler replaced by
  // "[spoiler]": the same tokenizer, strings out instead of elements.
  readonly preview: boolean;
}

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
      // Only a spoiler may span lines; the other markers close on their own line.
      if (next.text[at] === '\n' && marker !== '||') continue;
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
//
// A closed spoiler may span lines within one block, but not a quote or a code
// block, list or heading, so the toolbar wraps each line of a selection in
// its own. An unclosed || stays literal, as in Discord.
function inline(text: string, prefix: string, depth: number, budget: Budget, plainText = false): ReactNode[] {
  const next = nextMarkers(text);
  const out: ReactNode[] = [];
  let literal = 0;
  let at = 0;
  // Neighbouring strings join, so a body of many capped spoilers is a few
  // text nodes rather than one per spoiler.
  const add = (node: ReactNode) => {
    const last = out[out.length - 1];
    if (typeof node === 'string' && typeof last === 'string') out[out.length - 1] = last + node;
    else out.push(node);
  };
  const flush = (end: number) => {
    // A loop, not a spread: a body can hold tens of thousands of segments,
    // more than a call may take as arguments.
    if (end > literal && budget.preview) add(text.slice(literal, end));
    else if (end > literal) for (const node of plain(text.slice(literal, end), `${prefix}-${String(literal)}`)) add(node);
  };
  const wrap = (marker: InlineMarker, element: 'strong' | 'em' | 's'): boolean => {
    if (!text.startsWith(marker, at)) return false;
    const from = at + marker.length;
    const end = closing(next, marker, from);
    if (end < 0 || depth >= maxInlineDepth) return false;
    flush(at);
    const content = inline(text.slice(from, end), `${prefix}-${String(at)}`, depth + 1, budget);
    if (budget.preview) add(marker + text_(content) + marker);
    else out.push(element === 'strong' ? <strong key={`${prefix}-${String(at)}`}>{content}</strong> : element === 'em' ? <em key={`${prefix}-${String(at)}`}>{content}</em> : <s key={`${prefix}-${String(at)}`}>{content}</s>);
    at = end + marker.length;
    literal = at;
    return true;
  };

  while (at < text.length) {
    if (text[at] === '\\' && at + 1 < text.length && /[*`~|[\]\\]/.test(text[at + 1] ?? '')) {
      flush(at);
      add(budget.preview || plainText ? text.slice(at, at + 2) : text[at + 1]);
      at += 2;
      literal = at;
      continue;
    }
    if (!plainText && text[at] === '`') {
      const end = closing(next, '`', at + 1);
      if (end >= 0) {
        flush(at);
        if (budget.preview) add(text.slice(at, end + 1));
        else out.push(<code key={`${prefix}-${String(at)}`}>{text.slice(at + 1, end)}</code>);
        at = end + 1;
        literal = at;
        continue;
      }
    }
    if (text.startsWith('||', at)) {
      const end = closing(next, '||', at + 2);
      if (end >= 0) {
        flush(at);
        const inner = text.slice(at + 2, end);
        if (budget.preview || budget.spoilers >= maxSpoilers) {
          add('[spoiler]');
        } else {
          budget.spoilers += 1;
          const content = depth < maxInlineDepth ? inline(inner, `${prefix}-${String(at)}`, depth + 1, budget, plainText) : inner;
          out.push(<Spoiler key={`${prefix}-${String(at)}`}>{content}</Spoiler>);
        }
        at = end + 2;
        literal = at;
        continue;
      }
    }
    if (!plainText && text.startsWith('***', at)) {
      const end = closing(next, '***', at + 3);
      if (end >= 0 && depth < maxInlineDepth) {
        flush(at);
        const inner = inline(text.slice(at + 3, end), `${prefix}-${String(at)}`, depth + 1, budget);
        if (budget.preview) add(`***${text_(inner)}***`);
        else out.push(<strong key={`${prefix}-${String(at)}`}><em>{inner}</em></strong>);
        at = end + 3;
        literal = at;
        continue;
      }
    }
    if (!plainText && (wrap('**', 'strong') || wrap('~~', 's') || wrap('*', 'em'))) continue;
    at += 1;
  }
  flush(text.length);
  return out;
}

// Native buttons participate in the message log's existing roving Tab order.
// No key handler is needed: Enter and Space activate the same copy operation.
function CodeBlock({ text }: { readonly text: string }) {
  const [status, setStatus] = useState('');
  async function copy() {
    try {
      await platform.services.copyText(text);
      setStatus('Copied');
    } catch {
      setStatus('Could not copy. Try again.');
    }
  }
  return (
    <div className="markdown-code-block">
      <div className="markdown-code-actions">
        <button type="button" aria-label="Copy code" onClick={() => { void copy(); }}>Copy</button>
        <span role="status">{status}</span>
      </div>
      <pre><code>{text}</code></pre>
    </div>
  );
}

const maxListDepth = 3;
// Share one per-message budget across block kinds: near-1 MiB bodies can
// otherwise create hundreds of thousands of elements and stateful Copy controls.
const maxStructuralElements = 500;
interface ListLine {
  readonly depth: number;
  readonly ordered: boolean;
  readonly body: string;
}

function listLine(line: string): ListLine | null {
  const match = /^( *)([-*]|1\.) (.*)$/.exec(line);
  if (match === null) return null;
  return {
    depth: Math.min(maxListDepth - 1, Math.floor((match[1]?.length ?? 0) / 2)),
    ordered: match[2] === '1.',
    body: match[3] ?? '',
  };
}

interface MarkdownList {
  readonly ordered: boolean;
  readonly items: { readonly body: string; readonly children: MarkdownList[] }[];
}

// Build iteratively, flattening indentation jumps and levels beyond three.
// Rendering only recurses through that fixed depth; the shared structural cap
// leaves excess lines as source text instead of making unbounded DOM nodes.
function lists(lines: readonly ListLine[], prefix: string, budget: Budget): ReactNode {
  if (budget.preview) return lines.map((line) => text_(inline(line.body, prefix, 0, budget))).join('\n');
  const roots: MarkdownList[] = [];
  const stack: MarkdownList[] = [];
  for (const line of lines) {
    const depth = Math.min(line.depth, stack.length);
    stack.length = Math.min(stack.length, depth + 1);
    const parent = stack[depth - 1]?.items.at(-1);
    const siblings = parent?.children ?? roots;
    let group = stack[depth];
    if (group === undefined || group.ordered !== line.ordered) {
      group = { ordered: line.ordered, items: [] };
      siblings.push(group);
      stack[depth] = group;
    }
    group.items.push({ body: line.body, children: [] });
  }
  function renderGroups(groups: readonly MarkdownList[], key: string): ReactNode[] {
    return groups.map((group, index) => {
      const id = `${key}-${String(index)}`;
      const Tag = group.ordered ? 'ol' : 'ul';
      return <Tag className="markdown-list" key={id}>{group.items.map((item, n) => (
        <li key={n}>{inline(item.body, `${id}-${String(n)}`, 0, budget)}{renderGroups(item.children, `${id}-${String(n)}`)}</li>
      ))}</Tag>;
    });
  }
  return renderGroups(roots, prefix);
}

function blocks(text: string, preview = false): ReactNode[] {
  const budget: Budget = { spoilers: 0, preview };
  const lines = text.split('\n');
  const out: ReactNode[] = [];
  let ordinary: string[] = [];
  let structuralElements = 0;
  const flush = () => {
    if (ordinary.length === 0) return;
    const body = ordinary.join('\n');
    const nodes = inline(body, `text-${String(out.length)}`, 0, budget);
    out.push(preview ? text_(nodes) : <span key={`text-${String(out.length)}`}>{nodes}</span>);
    ordinary = [];
  };

  for (let at = 0; at < lines.length;) {
    if (structuralElements >= maxStructuralElements) {
      flush();
      // Preserve all remaining source in one block. Only links and bounded
      // spoilers become components; Markdown markers stay readable as text.
      const nodes = inline(lines.slice(at).join('\n'), 'overflow', 0, budget, true);
      out.push(preview ? text_(nodes) : <span key="overflow">{nodes}</span>);
      break;
    }
    const line = lines[at] ?? '';
    if (line.startsWith('```')) {
      structuralElements += 1;
      flush();
      const fence = at;
      at += 1;
      const code: string[] = [];
      while (at < lines.length && !(lines[at] ?? '').startsWith('```')) {
        code.push(lines[at] ?? '');
        at += 1;
      }
      if (at < lines.length) at += 1;
      if (preview) out.push(lines.slice(fence, at).join('\n'));
      else out.push(<CodeBlock key={`code-${String(out.length)}`} text={code.join('\n')} />);
      continue;
    }
    if (line === '>' || line.startsWith('> ')) {
      structuralElements += 1;
      flush();
      const quote: string[] = [];
      while (at < lines.length) {
        const quoted = lines[at] ?? '';
        if (quoted !== '>' && !quoted.startsWith('> ')) break;
        quote.push(quoted.slice(1).trimStart());
        at += 1;
      }
      const nodes = inline(quote.join('\n'), `quote-${String(out.length)}`, 0, budget);
      if (preview) out.push(text_(nodes).split('\n').map((part) => `> ${part}`).join('\n'));
      else out.push(<blockquote key={`quote-${String(out.length)}`}>{nodes}</blockquote>);
      continue;
    }
    const heading = /^(#{1,2}) (.*)$/.exec(line);
    if (heading !== null) {
      structuralElements += 1;
      flush();
      const nodes = inline(heading[2] ?? '', `heading-${String(out.length)}`, 0, budget);
      // Message text must not enter the page's screen-reader heading outline.
      out.push(preview ? text_(nodes) : <div className={`markdown-heading markdown-heading-${String(heading[1]?.length)}`} key={`heading-${String(out.length)}`}>{nodes}</div>);
      at += 1;
      continue;
    }
    if (listLine(line) !== null) {
      flush();
      const items: ListLine[] = [];
      while (at < lines.length && structuralElements < maxStructuralElements) {
        const item = listLine(lines[at] ?? '');
        if (item === null) break;
        items.push(item);
        structuralElements += 1;
        at += 1;
      }
      const nodes = lists(items, `list-${String(out.length)}`, budget);
      out.push(preview ? nodes : <div className="markdown-lists" key={`list-${String(out.length)}`}>{nodes}</div>);
      continue;
    }
    ordinary.push(line);
    at += 1;
  }
  flush();
  return out;
}

// BodyBoundary keeps one body that fails to render from taking the page down
// with it: that body is shown as its plain text instead, and the rest of the
// view renders as usual.
export class BodyBoundary extends Component<{ readonly text: string; readonly children: ReactNode }, { readonly failed: boolean }> {
  override state = { failed: false };

  static getDerivedStateFromError(): { readonly failed: boolean } {
    return { failed: true };
  }

  override render(): ReactNode {
    return this.state.failed ? this.props.text : this.props.children;
  }
}

function Rendered({ text }: { readonly text: string }) {
  return <>{blocks(text)}</>;
}

// Linkified is the shared message-body renderer. The historic name is kept
// because message history, pending sends, search and edit revisions all use it.
export function Linkified({ text }: { readonly text: string }) {
  return (
    <BodyBoundary key={text} text={text}>
      <Rendered text={text} />
    </BodyBoundary>
  );
}

// previewText is a body as a preview outside the message view (a system
// notification, the notification list, a reply excerpt): the body's own text
// with every spoiler the message view would hide replaced by "[spoiler]". It
// runs the renderer's tokenizer, so escapes, inline code, code blocks and
// nesting decide what is a spoiler exactly as they do there; an unclosed ||
// stays literal. Recognized list and heading markers are removed; other
// markup (including markers inside code) is kept as typed.
// Conversation.excerptOf calls this directly; notification previews use it
// through messagePreview. SearchPanel uses Linkified and the same blocks parser.
export function previewText(body: string): string {
  return blocks(body, true).map(text_).join('\n');
}
