// linkify splits a message body into text and http(s) links. The body is other
// people's text: the result is data for React elements (Linkified.tsx), never
// HTML, and only a URL that new URL() accepts with an http: or https: scheme
// becomes a link.
//
// Every step is linear in the body: one forward scan for a scheme, one pass
// over the non-space run that follows, and a trim that counts brackets once.
// A run longer than maxLink stays text rather than becoming a link.

export type Segment =
  | { readonly kind: 'text'; readonly text: string }
  | { readonly kind: 'link'; readonly text: string; readonly href: string };

export const maxLink = 2048;

const scheme = /https?:\/\//gi;
const word = /[^\s<>"]*/y;
const trailing = '.,;:!?\'*';
const pairs: Readonly<Record<string, string>> = { ')': '(', ']': '[', '}': '{' };

// A letter or digit before the scheme means it is the end of some other word.
function wordChar(ch: string | undefined): boolean {
  return ch !== undefined && /[\p{L}\p{N}_]/u.test(ch);
}

// trim drops trailing punctuation, and a closing bracket with no opener in
// the run, so a link in parentheses leaves the closing one outside.
function trim(run: string): string {
  const count: Record<string, number> = {};
  for (const ch of run) {
    if ('([{)]}'.includes(ch)) count[ch] = (count[ch] ?? 0) + 1;
  }
  let end = run.length;
  while (end > 0) {
    const ch = run.charAt(end - 1);
    const opener = pairs[ch];
    if (trailing.includes(ch)) {
      end -= 1;
    } else if (opener !== undefined && (count[ch] ?? 0) > (count[opener] ?? 0)) {
      count[ch] = (count[ch] ?? 0) - 1;
      end -= 1;
    } else {
      break;
    }
  }
  return run.slice(0, end);
}

function href(text: string): string | null {
  if (text.length > maxLink) return null;
  let url: URL;
  try {
    url = new URL(text);
  } catch {
    return null;
  }
  if ((url.protocol !== 'http:' && url.protocol !== 'https:') || url.hostname === '') return null;
  return url.href;
}

export function linkify(body: string): Segment[] {
  const out: Segment[] = [];
  let at = 0;
  scheme.lastIndex = 0;
  for (;;) {
    const found = scheme.exec(body);
    if (found === null) break;
    const start = found.index;
    if (wordChar(body[start - 1])) continue;
    word.lastIndex = start;
    const run = word.exec(body)?.[0] ?? '';
    const text = trim(run);
    const target = href(text);
    if (target === null) {
      // Resume after the run, so a hostile run is read once.
      scheme.lastIndex = start + run.length;
      continue;
    }
    if (start > at) out.push({ kind: 'text', text: body.slice(at, start) });
    out.push({ kind: 'link', text, href: target });
    at = start + text.length;
    scheme.lastIndex = at;
  }
  if (at < body.length) out.push({ kind: 'text', text: body.slice(at) });
  return out;
}
